package quota

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// 模拟两个 Command Code 账号:一个还有额度,一个已用光。
// 验证 Fetcher 给出正确的耗尽判定,这是选号剔除的依据。
func TestExhaustedAccountIsDetected(t *testing.T) {
	const richKey = "user_rich_000000000000"
	const poorKey = "user_poor_000000000000"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/alpha/billing/subscriptions" {
			_, _ = w.Write([]byte(`{"success":true,"data":{"planId":"individual-go","status":"active"}}`))
			return
		}
		if r.URL.Path != "/alpha/billing/credits" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		auth := r.Header.Get("Authorization")
		if auth == "Bearer "+poorKey {
			// 月额度只剩 5 分钱,5h/周也都见底
			_, _ = w.Write([]byte(`{"credits":{"monthlyCredits":0.05,"monthlyCreditsGranted":10},
			  "windowLimits":{"fiveHour":{"cap":3,"used":3},"weekly":{"cap":6,"used":6}}}`))
			return
		}
		if auth == "Bearer "+richKey {
			_, _ = w.Write([]byte(`{"credits":{"monthlyCredits":9.5,"monthlyCreditsGranted":10},
			  "windowLimits":{"fiveHour":{"cap":3,"used":0.2},"weekly":{"cap":6,"used":1}}}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	f := NewFetcher(srv.URL, time.Minute, 2*time.Second)
	poor := f.Get(nil, poorKey)
	rich := f.Get(nil, richKey)

	if !poor.OK || !rich.OK {
		t.Fatalf("两个账号都应取数成功: poor=%v rich=%v", poor.Err, rich.Err)
	}
	if !poor.Exhausted(0.1) {
		t.Fatalf("剩 0.05 的账号应判为耗尽: %+v", poor.Monthly)
	}
	if rich.Exhausted(0.1) {
		t.Fatalf("剩 9.5 的账号不应判为耗尽")
	}
	// 瓶颈比例:穷号应为 0%
	if got := poor.Bottleneck(); got > 0.01 {
		t.Fatalf("穷号瓶颈比例应为 0,得到 %v", got)
	}
	// 富号瓶颈应取三档最小值: 5h 剩 2.8/3=93.3%, 周剩 5/6=83.3% → 83.3
	if got := rich.Bottleneck(); got < 83 || got > 84 {
		t.Fatalf("富号瓶颈应约为 83.3,得到 %v", got)
	}
}

// 5h 用满但月额度充足:仍应判为耗尽(任一档见底即不可用)。
func TestFiveHourExhaustedBlocks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/alpha/billing/credits" {
			_, _ = w.Write([]byte(`{"credits":{"monthlyCredits":9.9,"monthlyCreditsGranted":10},
			  "windowLimits":{"fiveHour":{"cap":3,"used":3.0}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":true,"data":{"planId":"individual-go"}}`))
	}))
	t.Cleanup(srv.Close)

	f := NewFetcher(srv.URL, time.Minute, 2*time.Second)
	snap := f.Get(nil, "user_any")
	if !snap.OK {
		t.Fatalf("取数应成功: %s", snap.Err)
	}
	if !snap.Exhausted(0.1) {
		t.Fatal("5h 用满应触发耗尽判定,即使月额度还有")
	}
}

// 阈值语义:$0.10 边界必须精确。
func TestFloorBoundary(t *testing.T) {
	cases := []struct {
		rem  float64
		want bool
	}{
		{0, true}, {0.05, true}, {0.1, true},
		{0.100001, false}, {0.11, false}, {5, false},
	}
	for _, c := range cases {
		s := &Snapshot{OK: true, Monthly: &Window{Remaining: c.rem}}
		if got := s.Exhausted(0.1); got != c.want {
			t.Fatalf("rem=%.6f 期望 %v 得到 %v", c.rem, c.want, got)
		}
	}
}

// 并发选号时,同一个 key 只应触发一次上游查询(避免打爆计费接口)。
func TestSingleflightPerKey(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/alpha/billing/credits" {
			atomic.AddInt32(&hits, 1)
			time.Sleep(30 * time.Millisecond)
			_, _ = w.Write([]byte(`{"credits":{"monthlyCredits":5,"monthlyCreditsGranted":10}}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":true,"data":{"planId":"individual-go"}}`))
	}))
	t.Cleanup(srv.Close)

	f := NewFetcher(srv.URL, time.Minute, 3*time.Second)

	done := make(chan struct{})
	for i := 0; i < 12; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			f.Get(nil, "user_same_key")
		}()
	}
	for i := 0; i < 12; i++ {
		<-done
	}

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("12 个并发应只查 1 次上游,实际 %d 次", got)
	}
}

// panic 隔离:上游返回畸形数据时不能崩。
func TestMalformedUpstreamNoPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/alpha/billing/credits" {
			_, _ = w.Write([]byte(`{"credits":"this should be an object"} `))
			return
		}
		_, _ = w.Write([]byte(`not json at all`))
	}))
	t.Cleanup(srv.Close)

	f := NewFetcher(srv.URL, time.Minute, 2*time.Second)
	snap := f.Get(nil, "user_x")
	if snap == nil {
		t.Fatal("不能返回 nil")
	}
	// 解析不出月额度是允许的,但不能崩、不能误判耗尽
	if snap.Exhausted(0.1) {
		t.Fatal("畸形数据不能判为耗尽")
	}
}
