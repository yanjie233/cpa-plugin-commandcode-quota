package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

// 宿主函数表保存在 C 侧,供 hostInvoke 反向调用。
static cliproxy_host_api g_host;
static int g_host_ready = 0;

static void save_host_api(cliproxy_host_api* api) {
	if (api == NULL) { return; }
	g_host = *api;
	g_host_ready = 1;
}

static int host_call(const char* method, const uint8_t* req, size_t req_len, cliproxy_buffer* resp) {
	if (!g_host_ready || g_host.call == NULL) { return -1; }
	return g_host.call(g_host.host_ctx, method, req, req_len, resp);
}

static void host_free(void* ptr, size_t len) {
	if (g_host_ready && g_host.free_buffer != NULL && ptr != NULL) {
		g_host.free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/yanjie233/cpa-plugin-commandcode-quota/internal/panel"
	"github.com/yanjie233/cpa-plugin-commandcode-quota/internal/quota"
)

const (
	pluginID      = "commandcode-quota"
	pluginName    = "CommandCode Quota"
	pluginVersion = "1.0.0"
	repoURL       = "https://github.com/yanjie233/cpa-plugin-commandcode-quota"
)

// envelope 是 C ABI 的 JSON 信封。
type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// lifecycleRequest 兼容宿主可能使用的几种键名。
// C ABI 用 JSON 信封,[]byte 字段会自动 base64;也可能直接给字符串。
type lifecycleRequest struct {
	ConfigYAML json.RawMessage `json:"config_yaml"`
	AltCamel   json.RawMessage `json:"configYaml"`
	AltYAML    json.RawMessage `json:"yaml"`
}

// decodeConfigYAML 从生命周期请求里取出 YAML 原文。
func decodeConfigYAML(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return nil
	}
	// 直接就是对象/字符串?(少数实现会把配置本身放在顶层)
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []byte(s)
	}
	// base64 编码的字节数组([]byte 的默认 JSON 形式)
	var b []byte
	if err := json.Unmarshal(raw, &b); err == nil {
		return b
	}
	// 已经是对象:原样返回,交给 YAML 解析器(JSON 是 YAML 子集)
	return raw
}

func (r lifecycleRequest) yaml() []byte {
	for _, c := range []json.RawMessage{r.ConfigYAML, r.AltCamel, r.AltYAML} {
		if b := decodeConfigYAML(c); len(b) > 0 {
			return b
		}
	}
	return nil
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	Scheduler                 bool `json:"scheduler"`
	SchedulerAcrossPriorities bool `json:"scheduler_across_priorities"`
	QuotaProvider             bool `json:"quota_provider"`
	ManagementAPI             bool `json:"management_api"`
}

// config 对应 plugins.configs.<id> 的插件自有 YAML。
type config struct {
	Enabled         bool     `yaml:"enabled"`
	Priority        int      `yaml:"priority"`
	APIBase         string   `yaml:"api_base"`
	FloorUSD        float64  `yaml:"floor_usd"`
	QuotaTTLSeconds int      `yaml:"quota_ttl_seconds"`
	TimeoutSeconds  int      `yaml:"timeout_seconds"`
	Providers       []string `yaml:"providers"`
	DryRun          bool     `yaml:"dry_run"`
	ExcludeExhausted *bool   `yaml:"exclude_exhausted"`
	DebugLog        bool     `yaml:"debug_log"`
}

func (c config) excludeExhausted() bool {
	if c.ExcludeExhausted == nil {
		return true
	}
	return *c.ExcludeExhausted
}

// account 是面板展示用的账号条目。
type account struct {
	ID    string
	Label string
	Key   string
}

type state struct {
	cfg     atomic.Value // config
	fetcher atomic.Value // *quota.Fetcher
	mu      sync.Mutex
	log     []string
	picks   map[string]int64
	seen    map[string]account // authID -> account
}

var pluginState = &state{picks: map[string]int64{}, seen: map[string]account{}}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.save_host_api(host)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	cfg := defaultConfig()
	pluginState.cfg.Store(cfg)
	pluginState.fetcher.Store(newFetcher(cfg))
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, err := safeHandle(C.GoString(method), requestBytes)
	if err != nil {
		writeResponse(response, errorEnvelope("plugin_error", err.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

// safeHandle 兜住所有 panic:插件与宿主同进程,不能被拖垮。
func safeHandle(method string, request []byte) (out []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			out = errorEnvelope("panic", fmt.Sprintf("recovered: %v", r))
			err = nil
		}
	}()
	return handleMethod(method, request)
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		applyConfig(request)
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodSchedulerPick:
		return pickAuth(request)
	case pluginabi.MethodQuotaIdentifier:
		return okEnvelope(map[string]string{"identifier": pluginID})
	case pluginabi.MethodQuotaDescribe:
		return okEnvelope(pluginapi.QuotaDescribeResponse{
			SupportedProviders: []string{"openai-compatibility", "commandcode"},
			DisplayName:        "CommandCode 额度",
		})
	case pluginabi.MethodQuotaFetch:
		return fetchQuota(request)
	case pluginabi.MethodQuotaReset:
		return okEnvelope(pluginapi.QuotaResetResponse{Success: false, Message: "不支持重置"})
	case pluginabi.MethodManagementRegister:
		return okEnvelope(managementRegistration())
	case pluginabi.MethodManagementHandle:
		return handleManagement(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "yanjie233",
			GitHubRepository: repoURL,
			ConfigFields: []pluginapi.ConfigField{
				{Name: "api_base", Type: pluginapi.ConfigFieldTypeString,
					Description: "Command Code 计费接口根地址。默认 https://api.commandcode.ai。"},
				{Name: "floor_usd", Type: pluginapi.ConfigFieldTypeNumber,
					Description: "额度已用光阈值(美元)。任一档剩余 ≤ 该值即跳过该账号。默认 0.1。"},
				{Name: "quota_ttl_seconds", Type: pluginapi.ConfigFieldTypeInteger,
					Description: "余额快照有效秒数,过期后下次选号时刷新。默认 60。"},
				{Name: "timeout_seconds", Type: pluginapi.ConfigFieldTypeInteger,
					Description: "单次余额查询超时秒数。默认 5。"},
				{Name: "providers", Type: pluginapi.ConfigFieldTypeArray,
					Description: "限定接管的 provider 键,留空表示全部。"},
				{Name: "dry_run", Type: pluginapi.ConfigFieldTypeBoolean,
					Description: "只记录决策、不真正影响选号。默认关闭。"},
				{Name: "exclude_exhausted", Type: pluginapi.ConfigFieldTypeBoolean,
					Description: "是否跳过额度耗尽的账号。默认开启。"},
				{Name: "debug_log", Type: pluginapi.ConfigFieldTypeBoolean,
					Description: "把选号摘要记入内存供面板查看。默认关闭。"},
			},
		},
		Capabilities: registrationCapability{
			Scheduler:                 true,
			SchedulerAcrossPriorities: true,
			QuotaProvider:             true,
			ManagementAPI:             true,
		},
	}
}

func defaultConfig() config {
	return config{
		APIBase:         quota.DefaultBaseURL,
		FloorUSD:        0.1,
		QuotaTTLSeconds: 60,
		TimeoutSeconds:  5,
	}
}

func newFetcher(c config) *quota.Fetcher {
	return quota.NewFetcher(c.APIBase, secs(c.QuotaTTLSeconds), secs(c.TimeoutSeconds))
}

func applyConfig(request []byte) {
	cfg := defaultConfig()
	var req lifecycleRequest
	if len(request) > 0 && json.Unmarshal(request, &req) == nil {
		if yml := req.yaml(); len(yml) > 0 {
			var parsed config
			if err := yamlUnmarshal(yml, &parsed); err == nil {
				cfg = mergeConfig(cfg, parsed)
			}
		}
	}
	pluginState.cfg.Store(cfg)
	pluginState.fetcher.Store(newFetcher(cfg))
}

func mergeConfig(base, next config) config {
	if s := strings.TrimSpace(next.APIBase); s != "" {
		base.APIBase = s
	}
	if next.FloorUSD > 0 {
		base.FloorUSD = next.FloorUSD
	}
	if next.QuotaTTLSeconds > 0 {
		base.QuotaTTLSeconds = next.QuotaTTLSeconds
	}
	if next.TimeoutSeconds > 0 {
		base.TimeoutSeconds = next.TimeoutSeconds
	}
	if len(next.Providers) > 0 {
		base.Providers = next.Providers
	}
	base.DryRun = next.DryRun
	base.DebugLog = next.DebugLog
	base.Enabled = next.Enabled
	base.Priority = next.Priority
	if next.ExcludeExhausted != nil {
		base.ExcludeExhausted = next.ExcludeExhausted
	}
	return base
}

func currentConfig() config {
	if v, ok := pluginState.cfg.Load().(config); ok {
		return v
	}
	return defaultConfig()
}

func currentFetcher() *quota.Fetcher {
	if v, ok := pluginState.fetcher.Load().(*quota.Fetcher); ok && v != nil {
		return v
	}
	return newFetcher(defaultConfig())
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func panelHTML() []byte { return panel.HTML() }

func secs(n int) time.Duration {
	if n <= 0 {
		n = 5
	}
	return time.Duration(n) * time.Second
}

// 供 scheduler.go 复用的小工具。
func (s *state) remember(a account) {
	if a.ID == "" || a.Key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen[a.ID] = a
}

func (s *state) knownAccounts() []account {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]account, 0, len(s.seen))
	for _, a := range s.seen {
		out = append(out, a)
	}
	return out
}

var _ = strconv.Itoa

// hostInvoke 通过宿主函数表调用一个宿主回调,返回 result 字段的原始 JSON。
func hostInvoke(method string, payload any) (json.RawMessage, error) {
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal host payload %s: %w", method, err)
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var reqPtr *C.uint8_t
	if len(rawPayload) > 0 {
		buf := C.CBytes(rawPayload)
		if buf == nil {
			return nil, fmt.Errorf("allocate host payload %s", method)
		}
		defer C.free(buf)
		reqPtr = (*C.uint8_t)(buf)
	}

	var resp C.cliproxy_buffer
	code := C.host_call(cMethod, reqPtr, C.size_t(len(rawPayload)), &resp)

	var rawResponse []byte
	if resp.ptr != nil && resp.len > 0 {
		rawResponse = C.GoBytes(resp.ptr, C.int(resp.len))
	}
	if resp.ptr != nil {
		C.host_free(resp.ptr, resp.len)
	}
	if len(rawResponse) == 0 {
		return nil, fmt.Errorf("host callback %s returned nothing (code=%d)", method, int(code))
	}

	var env envelope
	if err := json.Unmarshal(rawResponse, &env); err != nil {
		return nil, fmt.Errorf("decode host envelope %s: %w", method, err)
	}
	if !env.OK {
		msg := "unknown error"
		if env.Error != nil && env.Error.Message != "" {
			msg = env.Error.Message
		}
		return nil, fmt.Errorf("host callback %s failed: %s", method, msg)
	}
	return env.Result, nil
}
