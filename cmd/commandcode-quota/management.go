package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/yanjie233/cpa-plugin-commandcode-quota/internal/quota"
)

type managementRegistrationResponse struct {
	Routes []pluginapi.ManagementRoute `json:"routes,omitempty"`
}

// managementRegistration 注册插件自有页面与数据接口。
//
// 关键约束:C ABI 插件无法提供 Go 接口 Handler,而宿主在 registerResourceRoute
// 里要求 item.Handler != nil。唯一可行路径是用 ManagementRoute 的 legacy 写法:
// GET + Menu 非空 → 宿主把它转成 /v0/resource/plugins/<id>/ 下的资源页。
// 因此所有路由都声明为带 Menu 的 GET 管理路由。
func managementRegistration() managementRegistrationResponse {
	return managementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{
				Method:      http.MethodGet,
				Path:        "/plugins/" + pluginID + "/panel",
				Menu:        "CommandCode 额度",
				Description: "5 小时 / 周 / 月额度、跳过记录与阈值。",
			},
			{
				Method:      http.MethodGet,
				Path:        "/plugins/" + pluginID + "/status",
				Menu:        "CommandCode 状态",
				Description: "插件运行状态与最近选号记录。",
			},
			{
				Method:      http.MethodGet,
				Path:        "/plugins/" + pluginID + "/quota",
				Menu:        "CommandCode 余额",
				Description: "各账号最新余额快照。",
			},
		},
	}
}

// handleManagement 处理资源页与自有数据接口。
func handleManagement(raw []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}

	switch {
	// 宿主把资源页请求转进来时,path 是 /v0/resource/plugins/<id>/panel。
	// 也接受相对形式,避免不同版本差异。
	case strings.HasSuffix(req.Path, "/panel"):
		return okEnvelope(htmlResponse(panelHTML()))

	case strings.HasSuffix(req.Path, "/status"):
		payload := statusPayload()
		body, _ := json.Marshal(payload)
		return okEnvelope(jsonResponse(body))

	case strings.HasSuffix(req.Path, "/quota"):
		body, _ := json.Marshal(map[string]any{"accounts": collectQuota()})
		return okEnvelope(jsonResponse(body))
	}

	return okEnvelope(jsonResponse([]byte(`{"error":"not_found"}`)))
}

// statusPayload 汇总插件运行状态,供面板显示。
func statusPayload() map[string]any {
	cfg := currentConfig()
	return map[string]any{
		"plugin":     pluginID,
		"version":    pluginVersion,
		"dry_run":    cfg.DryRun,
		"floor_usd":  cfg.FloorUSD,
		"quota_ttl":  cfg.QuotaTTLSeconds,
		"timeout":    cfg.TimeoutSeconds,
		"providers":  cfg.Providers,
		"exclude":    cfg.excludeExhausted(),
		"debug_log":  cfg.DebugLog,
		"picks":      pluginState.snapshotPicks(),
		"recent_log": pluginState.snapshotLog(),
		"generated":  nowMs(),
	}
}

type accountRow struct {
	ID        string        `json:"id"`
	Label     string        `json:"label"`
	OK        bool          `json:"ok"`
	Exhausted bool          `json:"exhausted"`
	Error     string        `json:"error,omitempty"`
	Plan      string        `json:"plan,omitempty"`
	FiveHour  *quota.Window `json:"five_hour,omitempty"`
	Weekly    *quota.Window `json:"weekly,omitempty"`
	Monthly   *quota.Window `json:"monthly,omitempty"`
	Picks     int64         `json:"picks"`
	FetchedAt int64         `json:"fetched_at,omitempty"`
}

// collectQuota 用宿主凭据表枚举账号并查询余额。
// 宿主回调不可用时退回"选号时见过的账号",保证面板不会白屏。
func collectQuota() []accountRow {
	cfg := currentConfig()
	fetcher := currentFetcher()
	picks := pluginState.snapshotPicks()

	ctx, cancel := context.WithTimeout(context.Background(), secs(cfg.TimeoutSeconds))
	defer cancel()

	// 优先用宿主凭据表;但 openai-compatibility 的 api-key-entries 不在
	// 凭据表里(host.auth.list 只覆盖 auths/ 目录),这种情况下退回
	// "选号时见过的候选"。这是本插件能看到全部 Command Code 账号的唯一途径。
	accounts := mergeAccounts(listHostAccounts(), pluginState.knownAccounts())

	out := make([]accountRow, 0, len(accounts))
	for _, a := range accounts {
		row := accountRow{ID: a.ID, Label: a.Label, Picks: picks[a.ID]}
		if a.Key == "" {
			row.Error = "未读到 API Key"
			out = append(out, row)
			continue
		}
		snap := fetcher.Get(ctx, a.Key)
		if snap != nil {
			row.OK = snap.OK
			row.FetchedAt = snap.FetchedAt
			row.FiveHour = snap.FiveHour
			row.Weekly = snap.Weekly
			row.Monthly = snap.Monthly
			row.Plan = snap.PlanName
			row.Exhausted = snap.Exhausted(cfg.FloorUSD)
			if !snap.OK {
				row.Error = snap.Err
			} else if snap.UpstreamErr != "" {
				row.Error = snap.UpstreamErr
			}
		}
		out = append(out, row)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Exhausted != out[j].Exhausted {
			return !out[i].Exhausted
		}
		return out[i].Label < out[j].Label
	})
	return out
}

// mergeAccounts 合并两个来源,按 ID 去重(宿主凭据表优先)。
func mergeAccounts(primary, secondary []account) []account {
	seen := make(map[string]struct{}, len(primary)+len(secondary))
	out := make([]account, 0, len(primary)+len(secondary))
	for _, list := range [][]account{primary, secondary} {
		for _, a := range list {
			key := a.ID
			if key == "" {
				key = a.Label
			}
			if key == "" {
				continue
			}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, a)
		}
	}
	return out
}

// listHostAccounts 通过 host.auth.list 枚举宿主的凭据,并用 host.auth.get 取 key。
// 任何一步失败都返回空切片,由调用方退回内存记录。
func listHostAccounts() (out []account) {
	defer func() { _ = recover() }()

	raw, err := hostInvoke(pluginabi.MethodHostAuthList, map[string]any{})
	if err != nil {
		return nil
	}
	var resp struct {
		Entries []pluginapi.HostAuthFileEntry `json:"entries"`
		Files   []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil
	}
	entries := resp.Entries
	if len(entries) == 0 {
		entries = resp.Files
	}

	for _, e := range entries {
		if e.Disabled {
			continue
		}
		if !looksLikeCommandCode(e) {
			continue
		}
		key := readHostKey(e.AuthIndex)
		label := e.Label
		if label == "" {
			label = e.Name
		}
		out = append(out, account{ID: e.ID + "", Label: label, Key: key})
	}
	return out
}

// looksLikeCommandCode 判断凭据是否属于 Command Code。
func looksLikeCommandCode(e pluginapi.HostAuthFileEntry) bool {
	if e.Provider == "commandcode" {
		return true
	}
	if len(e.Provider) >= 15 && e.Provider[:15] == "openai-compatib" {
		// openai-compatibility 下可能混着别的上游,靠 base_url 区分。
		return contains(e.BaseURL, "commandcode")
	}
	return contains(e.Name, "commandcode") || contains(e.Label, "commandcode")
}

// readHostKey 用 host.auth.get 取凭据里的 api key。
func readHostKey(authIndex string) string {
	if authIndex == "" {
		return ""
	}
	raw, err := hostInvoke(pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: authIndex})
	if err != nil {
		return ""
	}
	var resp pluginapi.HostAuthGetResponse
	if err := json.Unmarshal(raw, &resp); err != nil || len(resp.JSON) == 0 {
		return ""
	}
	var payload map[string]any
	if err := json.Unmarshal(resp.JSON, &payload); err != nil {
		return ""
	}
	for _, k := range []string{"api_key", "apiKey", "key"} {
		if v, ok := payload[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func contains(haystack, needle string) bool {
	if haystack == "" || needle == "" {
		return false
	}
	return len(haystack) >= len(needle) && (func() bool {
		h, n := []byte(haystack), []byte(needle)
		for i := 0; i+len(n) <= len(h); i++ {
			match := true
			for j := range n {
				if lower(h[i+j]) != lower(n[j]) {
					match = false
					break
				}
			}
			if match {
				return true
			}
		}
		return false
	})()
}

func lower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 32
	}
	return c
}

func jsonResponse(body []byte) pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       body,
	}
}

func htmlResponse(body []byte) pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       body,
	}
}

func nowMs() int64   { return time.Now().UnixMilli() }
func nowClock() string { return time.Now().Format("15:04:05") }
func epochISO(ms int64) string {
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}
