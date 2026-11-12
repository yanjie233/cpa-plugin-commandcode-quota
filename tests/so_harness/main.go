// 用 cgo 直接加载插件 .so 并驱动 C ABI,验证真实行为。
// 这不是单元测试的替代,而是对"交付物"本身的验收。
package main

/*
#include <stdint.h>
#include <stdlib.h>
#include <dlfcn.h>
#include <string.h>

typedef struct { void* ptr; size_t len; } cliproxy_buffer;

typedef int (*host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*host_free_fn)(void*, size_t);
typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	host_call_fn call;
	host_free_fn free_buffer;
} host_api;

typedef int (*plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*plugin_free_fn)(void*, size_t);
typedef void (*plugin_shutdown_fn)(void);
typedef struct {
	uint32_t abi_version;
	plugin_call_fn call;
	plugin_free_fn free_buffer;
	plugin_shutdown_fn shutdown;
} plugin_api;

typedef int (*init_fn)(host_api*, plugin_api*);

static int do_init(void* fn, host_api* h, plugin_api* p) {
	return ((init_fn)fn)(h, p);
}
static int do_plugin_call(plugin_call_fn fn, char* m, uint8_t* req, size_t n, cliproxy_buffer* resp) {
	return fn(m, req, n, resp);
}
static void do_plugin_free(plugin_free_fn fn, void* ptr, size_t n) {
	fn(ptr, n);
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"os"
	"unsafe"
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

var failures int

func check(name string, cond bool, detail string) {
	if cond {
		fmt.Printf("PASS  %s\n", name)
		return
	}
	failures++
	fmt.Printf("FAIL  %s  → %s\n", name, detail)
}

func main() {
	path := "/root/cpa-ccq/dist/commandcode-quota.so"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))

	handle := C.dlopen(cPath, C.RTLD_NOW)
	if handle == nil {
		fmt.Println("FAIL  dlopen:", C.GoString(C.dlerror()))
		os.Exit(1)
	}
	fmt.Println("PASS  dlopen 成功")

	symInit := C.CString("cliproxy_plugin_init")
	defer C.free(unsafe.Pointer(symInit))
	initPtr := C.dlsym(handle, symInit)
	if initPtr == nil {
		fmt.Println("FAIL  找不到 cliproxy_plugin_init")
		os.Exit(1)
	}
	fmt.Println("PASS  找到 cliproxy_plugin_init")

	var pluginAPI C.plugin_api
	// 传 nil host:插件必须容忍宿主回调不可用
	rc := C.do_init(initPtr, nil, &pluginAPI)
	check("init 返回 0", rc == 0, fmt.Sprintf("rc=%d", rc))
	check("init 填充了 call 指针", pluginAPI.call != nil, "call 为空")
	check("init 填充了 free_buffer", pluginAPI.free_buffer != nil, "free_buffer 为空")

	call := func(method string, payload any) (envelope, string) {
		var env envelope
		body, _ := json.Marshal(payload)
		cMethod := C.CString(method)
		defer C.free(unsafe.Pointer(cMethod))

		var reqPtr *C.uint8_t
		if len(body) > 0 {
			buf := C.CBytes(body)
			defer C.free(buf)
			reqPtr = (*C.uint8_t)(buf)
		}
		var resp C.cliproxy_buffer
		C.do_plugin_call(pluginAPI.call, cMethod, reqPtr, C.size_t(len(body)), &resp)

		var raw []byte
		if resp.ptr != nil && resp.len > 0 {
			raw = C.GoBytes(resp.ptr, C.int(resp.len))
		}
		if resp.ptr != nil {
			C.do_plugin_free(pluginAPI.free_buffer, resp.ptr, resp.len)
		}
		if len(raw) == 0 {
			return env, ""
		}
		_ = json.Unmarshal(raw, &env)
		return env, string(raw)
	}

	// 注册
	env, raw := call("plugin.register", map[string]any{
		"config_yaml": "floor_usd: 0.25\nquota_ttl_seconds: 30\ndry_run: true\ndebug_log: true\n",
	})
	check("plugin.register 返回 ok", env.OK, raw)
	var reg struct {
		SchemaVersion uint32 `json:"schema_version"`
		Metadata      struct {
			Name    string `json:"Name"`
			Version string `json:"Version"`
		} `json:"metadata"`
		Capabilities struct {
			Scheduler     bool `json:"scheduler"`
			QuotaProvider bool `json:"quota_provider"`
			ManagementAPI bool `json:"management_api"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		fmt.Println("FAIL  注册响应解析失败:", err)
	} else {
		check("声明 scheduler 能力", reg.Capabilities.Scheduler, raw)
		check("声明 quota_provider 能力", reg.Capabilities.QuotaProvider, raw)
		check("声明 management_api 能力", reg.Capabilities.ManagementAPI, raw)
		check("元数据有名称", reg.Metadata.Name != "", raw)
	}

	// 选号:两个候选,一个 key 明显无效(会查不到余额),应放行而不是拒绝
	env, raw = call("scheduler.pick", map[string]any{
		"Provider": "openai-compatibility",
		"Model":    "deepseek/deepseek-v4.1-flash",
		"Stream":   false,
		"Candidates": []map[string]any{
			{"ID": "auth-a", "Provider": "openai-compatibility",
				"Attributes": map[string]string{"api_key": "user_fake_key_aaaaaaaaaaaa"}},
			{"ID": "auth-b", "Provider": "openai-compatibility",
				"Attributes": map[string]string{"api_key": "user_fake_key_bbbbbbbbbbbb"}},
		},
	})
	check("scheduler.pick 不崩", env.OK, raw)
	var pick struct {
		AuthID  string `json:"AuthID"`
		Handled bool   `json:"Handled"`
		Reject  bool   `json:"Reject"`
	}
	_ = json.Unmarshal(env.Result, &pick)
	// dry_run=true 时必须 Handled=false,把决定权交还宿主
	check("dry_run 时不抢决定权", !pick.Handled, raw)
	check("dry_run 时不误拒", !pick.Reject, raw)

	// 空候选:必须交还宿主,不能拒绝
	env, raw = call("scheduler.pick", map[string]any{"Candidates": []map[string]any{}})
	check("空候选交还宿主", env.OK, raw)
	_ = json.Unmarshal(env.Result, &pick)
	check("空候选 Handled=false", !pick.Handled, raw)

	// 能力:quota.describe
	env, raw = call("quota.describe", map[string]any{})
	check("quota.describe 正常", env.OK, raw)

	// 能力:management.register
	env, raw = call("management.register", map[string]any{})
	check("management.register 正常", env.OK, raw)
	var mgmt struct {
		Resources []struct {
			Path string `json:"Path"`
			Menu string `json:"Menu"`
		} `json:"resources"`
		Routes []struct {
			Method string `json:"Method"`
			Path   string `json:"Path"`
		} `json:"routes"`
	}
	_ = json.Unmarshal(env.Result, &mgmt)
	check("注册了面板资源", len(mgmt.Resources) > 0 && mgmt.Resources[0].Path == "/panel", raw)

	// 管理路由:资源页必须返回 HTML
	env, raw = call("management.handle", map[string]any{"Method": "GET", "Path": "/panel"})
	check("资源页返回 ok", env.OK, raw)
	var page struct {
		StatusCode int    `json:"StatusCode"`
		Body       []byte `json:"Body"`
	}
	_ = json.Unmarshal(env.Result, &page)
	html := string(page.Body)
	check("资源页是 HTML", page.StatusCode == 200 && len(html) > 1000, fmt.Sprintf("code=%d len=%d", page.StatusCode, len(html)))
	check("页面含 5 小时额度", contains(html, "5 小时额度"), "缺少 5h 文案")
	check("页面含 额度已用光", contains(html, "额度已用光"), "缺少耗尽文案")
	check("页面无密码输入框", !contains(html, "type=\"password\""), "不该有密码框")

	// 状态接口
	env, raw = call("management.handle", map[string]any{"Method": "GET", "Path": "/plugins/commandcode-quota/status"})
	check("status 接口正常", env.OK, raw)

	// 未知路径不能崩
	env, raw = call("management.handle", map[string]any{"Method": "GET", "Path": "/nope"})
	check("未知路径不崩", env.OK, raw)

	// 未知方法
	env, raw = call("no.such.method", map[string]any{})
	check("未知方法返回错误信封", !env.OK, raw)

	// 畸形 JSON 不能崩(插件与宿主同进程)
	env, raw = call("scheduler.pick", "this is not json")
	check("畸形 JSON 不崩", raw != "", raw)

	// reconfigure 生效:改成 dry_run 关闭 + 更低阈值
	env, raw = call("plugin.reconfigure", map[string]any{
		"config_yaml": "floor_usd: 0.01\ndry_run: false\nexclude_exhausted: true\n",
	})
	check("reconfigure 返回 ok", env.OK, raw)

	if failures > 0 {
		fmt.Printf("\n%d 项失败\n", failures)
		os.Exit(1)
	}
	fmt.Println("\n全部通过")
}

func contains(h, n string) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}
