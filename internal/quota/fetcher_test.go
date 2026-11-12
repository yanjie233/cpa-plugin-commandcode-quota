package quota

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const testKey = "user_test_1234567890"

func creditsBody() string {
	return `{
	  "credits": {
	    "monthlyCredits": 7.25,
	    "purchasedCredits": 2,
	    "premiumMonthlyCredits": 0,
	    "opensourceMonthlyCredits": 0,
	    "windowLimits": {
	      "fiveHour": {"cap": "4", "used": "1", "resetAt": "1780200000"},
	      "weekly":   {"cap": 20, "used": 4, "resetAt": 1780300000000}
	    }
	  }
	}`
}

func subBody() string {
	return `{"success":true,"data":{"planId":"individual-goat","status":"active",
	         "currentPeriodEnd":"2026-12-12T00:00:00.000Z"}}`
}

func server(t *testing.T, credits, sub string, creditsStatus int) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/alpha/billing/credits":
			if creditsStatus != 0 {
				w.WriteHeader(creditsStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(credits))
		case "/alpha/billing/subscriptions":
			_, _ = w.Write([]byte(sub))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestParseCreditsBothShapes(t *testing.T) {
	// windowLimits 内嵌在 credits 内
	nested := parseCredits(map[string]any{
		"credits": map[string]any{
			"monthlyCredits": 7.25,
			"purchasedCredits": 2.0,
			"windowLimits": map[string]any{
				"fiveHour": map[string]any{"cap": "4", "used": "1", "resetAt": "1780200000"},
				"weekly":   map[string]any{"cap": 20.0, "used": 4.0, "resetAt": 1780300000000.0},
			},
		},
	})
	if nested.FiveHour == nil || nested.FiveHour.Cap != 4 {
		t.Fatalf("5h 解析失败: %+v", nested.FiveHour)
	}
	if nested.FiveHour.Remaining != 3 || nested.FiveHour.PctLeft != 75 {
		t.Fatalf("5h 剩余算错: %+v", nested.FiveHour)
	}
	// 秒级时间戳要转毫秒
	if nested.FiveHour.ResetAt != 1780200000000 {
		t.Fatalf("秒->毫秒转换失败: %d", nested.FiveHour.ResetAt)
	}
	if nested.Weekly.ResetAt != 1780300000000 {
		t.Fatalf("毫秒时间戳被改坏了: %d", nested.Weekly.ResetAt)
	}
	if nested.Monthly.Remaining != 9.25 {
		t.Fatalf("月剩额应含额外额度 7.25+2: %+v", nested.Monthly)
	}

	// windowLimits 在根级 + data 包裹
	root := parseCredits(map[string]any{
		"data": map[string]any{
			"credits": map[string]any{"monthlyCredits": 8.5},
			"windowLimits": map[string]any{
				"fiveHour": map[string]any{"cap": 3.0, "used": 0.75},
			},
		},
	})
	if root.FiveHour == nil || root.FiveHour.Cap != 3 || root.FiveHour.Remaining != 2.25 {
		t.Fatalf("根级 windowLimits 解析失败: %+v", root.FiveHour)
	}
}

func TestCapZeroIsSkipped(t *testing.T) {
	snap := parseCredits(map[string]any{
		"credits": map[string]any{"monthlyCredits": 0.0},
		"windowLimits": map[string]any{
			"fiveHour": map[string]any{"cap": 0.0, "used": 0},
		},
	})
	if snap.FiveHour != nil {
		t.Fatalf("cap<=0 应视为无该窗口")
	}
	if snap.Monthly == nil || snap.Monthly.Remaining != 0 {
		t.Fatalf("月额度 0 应保留")
	}
}

func TestExhaustedFloor(t *testing.T) {
	mk := func(rem float64) *Snapshot {
		return &Snapshot{OK: true, Monthly: &Window{Remaining: rem}}
	}
	if !mk(0).Exhausted(0.1) {
		t.Fatal("0 应判为耗尽")
	}
	if !mk(0.1).Exhausted(0.1) {
		t.Fatal("恰好等于阈值应判为耗尽")
	}
	if mk(0.11).Exhausted(0.1) {
		t.Fatal("高于阈值不应判为耗尽")
	}
	// 无数据必须放行,绝不能因为查不到就断路
	if (&Snapshot{OK: false}).Exhausted(0.1) {
		t.Fatal("无数据不应判为耗尽")
	}
	if (&Snapshot{OK: true}).Exhausted(0.1) {
		t.Fatal("没有任何窗口时不应判为耗尽")
	}
	// 任一档见底即耗尽
	snap := &Snapshot{OK: true,
		FiveHour: &Window{Remaining: 0.05},
		Monthly:  &Window{Remaining: 50}}
	if !snap.Exhausted(0.1) {
		t.Fatal("任一档见底应判为耗尽")
	}
}

func TestFetchAndCache(t *testing.T) {
	srv, hits := server(t, creditsBody(), subBody(), 0)
	f := NewFetcher(srv.URL, 60*time.Second, 3*time.Second)

	snap := f.Get(context.Background(), testKey)
	if !snap.OK {
		t.Fatalf("取数失败: %s", snap.Err)
	}
	if snap.PlanID != "individual-goat" {
		t.Fatalf("套餐未填充: %q", snap.PlanID)
	}
	// 上游没报 grant,应按套餐目录兜底成 70
	if snap.Monthly.Cap != 70 {
		t.Fatalf("月上限应按 GOAT 兜底为 70,得到 %v", snap.Monthly.Cap)
	}
	if snap.Monthly.ResetAt == 0 {
		t.Fatal("月重置日应来自 currentPeriodEnd")
	}

	first := atomic.LoadInt32(hits)
	// 再次读取必须命中缓存,不打上游
	for i := 0; i < 5; i++ {
		f.Get(context.Background(), testKey)
	}
	if got := atomic.LoadInt32(hits); got != first {
		t.Fatalf("缓存未生效: 命中前后请求数 %d -> %d", first, got)
	}
}

func TestCachedNeverBlocks(t *testing.T) {
	f := NewFetcher("http://127.0.0.1:1", 60*time.Second, 500*time.Millisecond)
	// 从未取过数: Cached 必须立即返回 false,不发起网络
	if _, ok := f.Cached(testKey); ok {
		t.Fatal("空缓存应返回 false")
	}
	start := time.Now()
	if snap, ok := f.Cached(testKey); ok || snap != nil {
		t.Fatal("空缓存不应返回快照")
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("Cached 不该有网络等待")
	}
}

func TestUpstreamErrorsKeepOldValue(t *testing.T) {
	var fail int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&fail) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if r.URL.Path == "/alpha/billing/credits" {
			_, _ = w.Write([]byte(creditsBody()))
			return
		}
		_, _ = w.Write([]byte(subBody()))
	}))
	t.Cleanup(srv.Close)

	f := NewFetcher(srv.URL, 1*time.Millisecond, 2*time.Second)
	good := f.Get(context.Background(), testKey)
	if !good.OK {
		t.Fatalf("首次取数应成功: %s", good.Err)
	}
	// 让缓存过期并让上游失败
	time.Sleep(5 * time.Millisecond)
	atomic.StoreInt32(&fail, 1)
	snap := f.Get(context.Background(), testKey)
	if !snap.OK {
		t.Fatal("上游失败时应退回上次成功的快照,而不是断路")
	}
	if !snap.Stale {
		t.Fatal("退回的快照应标记 stale")
	}
	if snap.UpstreamErr == "" {
		t.Fatal("应记录上游错误原因")
	}
}

func TestUnauthorizedIsReported(t *testing.T) {
	srv, _ := server(t, "", "", http.StatusUnauthorized)
	f := NewFetcher(srv.URL, 60*time.Second, 2*time.Second)
	snap := f.Get(context.Background(), testKey)
	if snap.OK {
		t.Fatal("401 不应成功")
	}
	if snap.Err == "" {
		t.Fatal("应给出可读错误")
	}
	// 失败且无旧值:不得被认为是耗尽(避免误封)
	if snap.Exhausted(0.1) {
		t.Fatal("取数失败绝不能判为额度耗尽")
	}
}

func TestFingerprintStableAndOpaque(t *testing.T) {
	a := Fingerprint(" user_abc ")
	b := Fingerprint("user_abc")
	if a != b {
		t.Fatal("指纹应忽略首尾空格")
	}
	if len(a) != 16 {
		t.Fatalf("指纹长度应为 16,得到 %d", len(a))
	}
	if Fingerprint("user_abc") == Fingerprint("user_abd") {
		t.Fatal("不同 key 不应撞指纹")
	}
	if got := Fingerprint(""); got == "" {
		t.Fatal("空 key 也要有稳定指纹")
	}
}

func TestBottleneck(t *testing.T) {
	snap := &Snapshot{
		FiveHour: &Window{PctLeft: 80},
		Weekly:   &Window{PctLeft: 20},
		Monthly:  &Window{PctLeft: 55},
	}
	if got := snap.Bottleneck(); got != 20 {
		t.Fatalf("瓶颈应取最小比例 20,得到 %v", got)
	}
	if got := (&Snapshot{}).Bottleneck(); got != -1 {
		t.Fatalf("无窗口应返回 -1,得到 %v", got)
	}
	if got := (*Snapshot)(nil).Bottleneck(); got != -1 {
		t.Fatalf("nil 快照应返回 -1,得到 %v", got)
	}
}
