// Package quota 负责与 Command Code 的计费接口通信，并把结果缓存到内存。
//
// 设计原则:
//   - 任何上游失败都不能阻断请求:查不到余额时返回 stale 快照或"未知",调用方据此放行。
//   - 所有网络调用都有超时,且不使用 context.Background() 无限等待。
//   - 状态按 API key 的指纹索引,key 本身不落盘、不进日志。
package quota

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL 是 Command Code 的计费接口根地址(与 CLI 的 /usage 同源)。
const DefaultBaseURL = "https://api.commandcode.ai"

// planCatalog 把订阅 planId 映射成月额度池上限(美元)。
// 仅在上游未返回 monthlyCreditsGranted 时兜底。
var planCatalog = map[string]float64{
	"individual-go":     10,
	"individual-go-v1":  10,
	"individual-goat":   70,
	"individual-pro":    30,
	"individual-pro-v1": 80,
	"individual-max":    150,
	"individual-ultra":  300,
	"teams-pro":         40,
}

// Window 是一档额度窗口(5 小时 / 周 / 月)的余额。
type Window struct {
	Cap       float64 `json:"cap"`
	Used      float64 `json:"used"`
	Remaining float64 `json:"remaining"`
	PctLeft   float64 `json:"pct_remaining"`
	ResetAt   int64   `json:"reset_at,omitempty"`
}

// Snapshot 是某个 key 在某一时刻的完整余额快照。
type Snapshot struct {
	OK          bool    `json:"ok"`
	Err         string  `json:"error,omitempty"`
	FiveHour    *Window `json:"five_hour,omitempty"`
	Weekly      *Window `json:"weekly,omitempty"`
	Monthly     *Window `json:"monthly,omitempty"`
	PlanID      string  `json:"plan_id,omitempty"`
	PlanName    string  `json:"plan_name,omitempty"`
	Email       string  `json:"email,omitempty"`
	FetchedAt   int64   `json:"fetched_at"`
	Stale       bool    `json:"stale,omitempty"`
	UpstreamErr string  `json:"upstream_error,omitempty"`
}

// Bottleneck 返回三档中剩余比例的最小值(无数据时返回 -1)。
// 用比例而不是金额比较,避免不同套餐上限差异。
func (s *Snapshot) Bottleneck() float64 {
	if s == nil {
		return -1
	}
	min := -1.0
	for _, w := range []*Window{s.FiveHour, s.Weekly, s.Monthly} {
		if w == nil {
			continue
		}
		if min < 0 || w.PctLeft < min {
			min = w.PctLeft
		}
	}
	return min
}

// Exhausted 判断是否应该被排除在选号之外。
// 任一档剩余金额 <= floor 即视为耗尽;无数据时返回 false(放行,宁可多用不可断路)。
func (s *Snapshot) Exhausted(floorUSD float64) bool {
	if s == nil || !s.OK {
		return false
	}
	for _, w := range []*Window{s.FiveHour, s.Weekly, s.Monthly} {
		if w == nil {
			continue
		}
		if w.Remaining <= floorUSD {
			return true
		}
	}
	return false
}

// Fetcher 拉取并缓存余额。
type Fetcher struct {
	baseURL   string
	ttl       time.Duration
	timeout   time.Duration
	client    *http.Client
	mu        sync.RWMutex
	snapshots map[string]*Snapshot // fingerprint -> snapshot
	inflight  map[string]*sync.Mutex
}

// NewFetcher 构造一个带缓存的取数器。
func NewFetcher(baseURL string, ttl, timeout time.Duration) *Fetcher {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultBaseURL
	}
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Fetcher{
		baseURL:   strings.TrimRight(baseURL, "/"),
		ttl:       ttl,
		timeout:   timeout,
		client:    &http.Client{Timeout: timeout},
		snapshots: map[string]*Snapshot{},
		inflight:  map[string]*sync.Mutex{},
	}
}

// Fingerprint 返回 key 的稳定短指纹,用于做 map 键和日志,不泄露明文。
func Fingerprint(key string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(key)))
	return hex.EncodeToString(sum[:8])
}

// Cached 只读缓存,不触发网络。请求路径专用。
func (f *Fetcher) Cached(key string) (*Snapshot, bool) {
	if f == nil || strings.TrimSpace(key) == "" {
		return nil, false
	}
	f.mu.RLock()
	snap := f.snapshots[Fingerprint(key)]
	f.mu.RUnlock()
	if snap == nil {
		return nil, false
	}
	age := time.Since(time.UnixMilli(snap.FetchedAt))
	out := *snap
	out.Stale = age > f.ttl
	return &out, true
}

// Get 返回缓存,过期则同步刷新。刷新失败时退回旧快照并标 stale。
func (f *Fetcher) Get(ctx context.Context, key string) *Snapshot {
	key = strings.TrimSpace(key)
	if f == nil || key == "" {
		return &Snapshot{OK: false, Err: "empty api key"}
	}
	if snap, fresh := f.cachedFresh(key); fresh {
		return snap
	}

	fp := Fingerprint(key)
	f.mu.Lock()
	lock := f.inflight[fp]
	if lock == nil {
		lock = &sync.Mutex{}
		f.inflight[fp] = lock
	}
	f.mu.Unlock()

	lock.Lock()
	defer lock.Unlock()

	// 双检:等锁期间可能已被别的 goroutine 刷新。
	if snap, fresh := f.cachedFresh(key); fresh {
		return snap
	}

	snap := f.fetch(ctx, key)

	f.mu.Lock()
	old := f.snapshots[fp]
	if snap.OK {
		f.snapshots[fp] = snap
	} else if old != nil && old.OK {
		// 上游失败但手里有旧值:继续用旧值,只记错误。
		merged := *old
		merged.Stale = true
		merged.UpstreamErr = snap.Err
		snap = &merged
	}
	f.mu.Unlock()
	return snap
}

func (f *Fetcher) cachedFresh(key string) (*Snapshot, bool) {
	f.mu.RLock()
	snap := f.snapshots[Fingerprint(key)]
	f.mu.RUnlock()
	if snap == nil || !snap.OK {
		return nil, false
	}
	if time.Since(time.UnixMilli(snap.FetchedAt)) > f.ttl {
		return nil, false
	}
	out := *snap
	return &out, true
}

func (f *Fetcher) fetch(ctx context.Context, key string) (out *Snapshot) {
	nowMs := time.Now().UnixMilli()
	defer func() {
		if r := recover(); r != nil {
			out = &Snapshot{OK: false, Err: fmt.Sprintf("panic: %v", r), FetchedAt: nowMs}
		}
	}()

	// 调用方可能传 nil(例如面板轮询),必须兜住,否则 panic 会打穿到宿主进程。
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()

	var credits map[string]any
	status, err := f.getJSON(ctx, "/alpha/billing/credits", key, &credits)
	if err != nil {
		return &Snapshot{OK: false, Err: err.Error(), FetchedAt: nowMs}
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return &Snapshot{OK: false, Err: "API Key 无效或已被撤销", FetchedAt: nowMs}
	case status == http.StatusTooManyRequests:
		return &Snapshot{OK: false, Err: "上游限流 (429)", FetchedAt: nowMs}
	case status < 200 || status >= 300:
		return &Snapshot{OK: false, Err: fmt.Sprintf("上游 HTTP %d", status), FetchedAt: nowMs}
	}

	snap := parseCredits(credits)
	snap.FetchedAt = nowMs
	snap.OK = true

	// 订阅信息用于补月额度上限和重置日;失败不影响主数据。
	var sub map[string]any
	if s2, err2 := f.getJSON(ctx, "/alpha/billing/subscriptions", key, &sub); err2 == nil && s2 == http.StatusOK {
		applySubscription(snap, sub)
	}
	return snap
}

func (f *Fetcher) getJSON(ctx context.Context, path, key string, out *map[string]any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.baseURL+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(key))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "cpa-commandcode-quota/1.0")

	resp, err := f.client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return 0, errors.New("查询余额超时")
		}
		return 0, fmt.Errorf("无法连接计费接口: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("读取响应失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return resp.StatusCode, fmt.Errorf("响应不是合法 JSON")
	}
	return resp.StatusCode, nil
}

// parseCredits 把 /alpha/billing/credits 的响应归一化成 Snapshot。
// 兼容: 数字或字符串、秒或毫秒时间戳、windowLimits 在根级或 credits 内。
func parseCredits(root map[string]any) *Snapshot {
	inner := root
	if u := unwrap(root); u != nil {
		inner = u
	}

	credits, _ := inner["credits"].(map[string]any)
	if credits == nil {
		credits = map[string]any{}
	}

	limits, _ := inner["windowLimits"].(map[string]any)
	if limits == nil {
		limits, _ = credits["windowLimits"].(map[string]any)
	}

	snap := &Snapshot{}
	if limits != nil {
		snap.FiveHour = parseWindow(limits["fiveHour"])
		snap.Weekly = parseWindow(limits["weekly"])
	}

	monthlyLeft := toFloat(credits["monthlyCredits"])
	granted := toFloat(credits["monthlyCreditsGranted"])
	extras := val(toFloat(credits["purchasedCredits"])) +
		val(toFloat(credits["premiumMonthlyCredits"])) +
		val(toFloat(credits["opensourceMonthlyCredits"]))

	if monthlyLeft != nil {
		cap := granted
		used := 0.0
		if cap != nil && *cap > 0 {
			used = *cap - *monthlyLeft
			if used < 0 {
				used = 0
			}
		}
		rem := *monthlyLeft + extras
		m := &Window{Remaining: round(rem, 4), Used: round(used, 4)}
		if cap != nil && *cap > 0 {
			m.Cap = round(*cap, 4)
			m.PctLeft = round(clamp(*monthlyLeft/(*cap)*100, 0, 100), 2)
		}
		snap.Monthly = m
	}
	return snap
}

// applySubscription 用订阅信息补全月额度上限与重置日。
func applySubscription(snap *Snapshot, payload map[string]any) {
	if snap == nil || payload == nil {
		return
	}
	if ok, _ := payload["success"].(bool); !ok {
		return
	}
	data, _ := payload["data"].(map[string]any)
	if data == nil {
		return
	}
	if planID, _ := data["planId"].(string); planID != "" {
		snap.PlanID = planID
		snap.PlanName = planID
		if cap, found := planCatalog[strings.ToLower(planID)]; found {
			if snap.Monthly == nil {
				snap.Monthly = &Window{}
			}
			if snap.Monthly.Cap == 0 {
				snap.Monthly.Cap = cap
				if snap.Monthly.Remaining > 0 {
					snap.Monthly.PctLeft = round(clamp(snap.Monthly.Remaining/cap*100, 0, 100), 2)
				}
			}
		}
	}
	if end := toEpochMs(data["currentPeriodEnd"]); end > 0 {
		if snap.Monthly != nil {
			snap.Monthly.ResetAt = end
		}
	}
}

func parseWindow(v any) *Window {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	cap, capOK := toFloat(m["cap"]), m["cap"] != nil
	used := toFloat(m["used"])
	if !capOK || cap == nil || *cap <= 0 {
		return nil
	}
	u := 0.0
	if used != nil {
		u = *used
	}
	rem := *cap - u
	if rem < 0 {
		rem = 0
	}
	return &Window{
		Cap:       round(*cap, 4),
		Used:      round(u, 4),
		Remaining: round(rem, 4),
		PctLeft:   round(clamp(rem/(*cap)*100, 0, 100), 2),
		ResetAt:   toEpochMs(m["resetAt"]),
	}
}

func unwrap(obj map[string]any) map[string]any {
	for _, k := range []string{"data", "result"} {
		if m, ok := obj[k].(map[string]any); ok {
			return m
		}
	}
	return nil
}

func toFloat(v any) *float64 {
	switch t := v.(type) {
	case float64:
		return &t
	case float32:
		f := float64(t)
		return &f
	case int:
		f := float64(t)
		return &f
	case int64:
		f := float64(t)
		return &f
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return &f
		}
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return nil
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return &f
		}
	}
	return nil
}

// toEpochMs 把秒 / 毫秒 / ISO8601 统一成毫秒。
func toEpochMs(v any) int64 {
	if f := toFloat(v); f != nil && *f > 0 {
		if *f < 1e10 {
			return int64(*f * 1000)
		}
		return int64(*f)
	}
	if s, ok := v.(string); ok {
		s = strings.TrimSpace(s)
		if s == "" {
			return 0
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z"} {
			if t, err := time.Parse(layout, s); err == nil {
				return t.UnixMilli()
			}
		}
	}
	return 0
}

// val 把可选浮点解引用成 0。上游字段缺失时按 0 处理。
func val(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}

func round(v float64, dp int) float64 {
	p := 1.0
	for i := 0; i < dp; i++ {
		p *= 10
	}
	return float64(int64(v*p+0.5)) / p
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
