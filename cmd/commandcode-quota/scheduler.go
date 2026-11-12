package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/yanjie233/cpa-plugin-commandcode-quota/internal/quota"
)

// candidateKey 从候选记录里取发往上游的 API key。
// 宿主的 Attributes/Metadata 是弱类型,key 可能出现的位置都试一遍。
func candidateKey(c pluginapi.SchedulerAuthCandidate) string {
	for _, k := range []string{"api_key", "apiKey", "key"} {
		if v := strings.TrimSpace(c.Attributes[k]); v != "" {
			return v
		}
	}
	for _, k := range []string{"api_key", "apiKey", "key"} {
		if v, ok := c.Metadata[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// pickAuth 是 scheduler.pick 的实现。
//
// 这是整个插件的关键:余额耗尽的 key 在**选号阶段**就被剔除,
// 不会先发出请求再靠上游报错。
//
// 任何算不出来的情况都退回 Handled=false 交还宿主,插件绝不卡死请求。
func pickAuth(raw []byte) ([]byte, error) {
	var req pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}

	cfg := currentConfig()
	// 无条件记录调用,用于确认宿主是否真的把选号权交给我们。
	pluginState.record(fmt.Sprintf("pick调用 provider=%q 候选=%d 模型=%s",
		req.Provider, len(req.Candidates), req.Model), true)
	if len(cfg.Providers) > 0 && !providerAllowed(cfg, req.Provider) {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}
	if len(req.Candidates) == 0 {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	fetcher := currentFetcher()
	ctx, cancel := context.WithTimeout(context.Background(), secs(cfg.TimeoutSeconds))
	defer cancel()

	type scored struct {
		id    string
		bneck float64
		rem   float64
		known bool
	}

	kept := make([]scored, 0, len(req.Candidates))
	dropped := 0

	for _, c := range req.Candidates {
		key := candidateKey(c)
		if key == "" {
			// 拿不到 key 就无法判断,保守放行。
			kept = append(kept, scored{id: c.ID})
			continue
		}
		pluginState.remember(account{ID: c.ID, Label: labelOf(c), Key: key})

		snap, hit := fetcher.Cached(key)
		if !hit {
			snap = fetcher.Get(ctx, key)
		}
		if snap != nil && snap.Exhausted(cfg.FloorUSD) && cfg.excludeExhausted() {
			dropped++
			pluginState.record(fmt.Sprintf("skip %s: 额度已用光 最低剩余 $%.4f ≤ $%.2f",
				shortID(c.ID), lowestRemaining(snap), cfg.FloorUSD), cfg.DebugLog)
			continue
		}
		kept = append(kept, scored{
			id:    c.ID,
			bneck: snap.Bottleneck(),
			rem:   lowestRemaining(snap),
			known: snap != nil && snap.OK,
		})
	}

	if len(kept) == 0 {
		pluginState.record(fmt.Sprintf("reject: %d 个候选全部额度已用光", dropped), cfg.DebugLog)
		return okEnvelope(pluginapi.SchedulerPickResponse{
			Handled:      true,
			Reject:       true,
			RejectReason: "CommandCode 全部账号额度已用光",
			RejectCode:   "auth_unavailable",
		})
	}

	// 有数据的优先,其次按瓶颈比例降序,再次按剩余金额降序。
	sort.SliceStable(kept, func(i, j int) bool {
		a, b := kept[i], kept[j]
		if a.known != b.known {
			return a.known
		}
		if a.bneck != b.bneck {
			return a.bneck > b.bneck
		}
		return a.rem > b.rem
	})

	winner := kept[0]
	pluginState.countPick(winner.id)

	if cfg.DryRun {
		pluginState.record(fmt.Sprintf("dry_run: 本应选 %s(会跳过 %d 个)", shortID(winner.id), dropped), cfg.DebugLog)
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	pluginState.record(fmt.Sprintf("pick %s 瓶颈=%.1f%% 最低剩余=$%.2f 跳过=%d",
		shortID(winner.id), winner.bneck, winner.rem, dropped), cfg.DebugLog)

	return okEnvelope(pluginapi.SchedulerPickResponse{AuthID: winner.id, Handled: true})
}

func providerAllowed(cfg config, provider string) bool {
	if provider == "" {
		return true
	}
	for _, p := range cfg.Providers {
		if strings.EqualFold(strings.TrimSpace(p), strings.TrimSpace(provider)) {
			return true
		}
	}
	return false
}

func labelOf(c pluginapi.SchedulerAuthCandidate) string {
	for _, k := range []string{"label", "email", "name", "account"} {
		if v := c.Attributes[k]; strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	for _, k := range []string{"label", "email", "name", "account"} {
		if v, ok := c.Metadata[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return shortID(c.ID)
}

// lowestRemaining 返回三档里最小的剩余金额。
func lowestRemaining(s *quota.Snapshot) float64 {
	if s == nil {
		return -1
	}
	min := -1.0
	for _, w := range []*quota.Window{s.FiveHour, s.Weekly, s.Monthly} {
		if w == nil {
			continue
		}
		if min < 0 || w.Remaining < min {
			min = w.Remaining
		}
	}
	return min
}

func shortID(id string) string {
	if len(id) <= 20 {
		return id
	}
	return "…" + id[len(id)-10:]
}

func (s *state) record(line string, enabled bool) {
	if !enabled {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log = append(s.log, nowClock()+" "+line)
	if len(s.log) > 200 {
		s.log = s.log[len(s.log)-200:]
	}
}

func (s *state) countPick(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.picks[id]++
}

func (s *state) snapshotLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.log))
	copy(out, s.log)
	return out
}

func (s *state) snapshotPicks() map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int64, len(s.picks))
	for k, v := range s.picks {
		out[k] = v
	}
	return out
}

// fetchQuota 是 quota.fetch:面板按单个凭据查余额。
func fetchQuota(raw []byte) ([]byte, error) {
	var req pluginapi.QuotaFetchRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}

	key := ""
	for _, k := range []string{"api_key", "apiKey", "key"} {
		if v := strings.TrimSpace(req.Attributes[k]); v != "" {
			key = v
			break
		}
	}
	if key == "" {
		for _, k := range []string{"api_key", "apiKey", "key"} {
			if v, ok := req.Metadata[k].(string); ok && strings.TrimSpace(v) != "" {
				key = strings.TrimSpace(v)
				break
			}
		}
	}
	if key == "" {
		return okEnvelope(pluginapi.QuotaFetchResponse{})
	}

	cfg := currentConfig()
	ctx, cancel := context.WithTimeout(context.Background(), secs(cfg.TimeoutSeconds))
	defer cancel()
	snap := currentFetcher().Get(ctx, key)
	return okEnvelope(snapshotToQuota(snap))
}

// snapshotToQuota 把内部快照转成宿主标准配额结构。
func snapshotToQuota(s *quota.Snapshot) pluginapi.QuotaFetchResponse {
	if s == nil {
		return pluginapi.QuotaFetchResponse{}
	}
	resp := pluginapi.QuotaFetchResponse{}
	if s.PlanName != "" {
		resp.Subscription = &pluginapi.QuotaSubscription{Plan: s.PlanName}
	}
	buckets := make([]pluginapi.QuotaBucket, 0, 3)
	add := func(name string, w *quota.Window) {
		if w == nil {
			return
		}
		b := pluginapi.QuotaBucket{
			Window:            name,
			RemainingFraction: clamp01(w.PctLeft / 100),
			Description:       fmt.Sprintf("剩余 $%.2f / 上限 $%.2f", w.Remaining, w.Cap),
		}
		if w.ResetAt > 0 {
			b.ResetTime = epochISO(w.ResetAt)
		}
		buckets = append(buckets, b)
	}
	add("5h", s.FiveHour)
	add("weekly", s.Weekly)
	add("monthly", s.Monthly)
	if len(buckets) > 0 {
		resp.Groups = []pluginapi.QuotaGroup{{DisplayName: "Command Code", Buckets: buckets}}
	}
	return resp
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
