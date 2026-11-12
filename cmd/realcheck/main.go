// 真实上游连通性检查:用 CPA 里已配置的 Command Code key 查一次余额。
// 只读,不打印 key 明文。用法: go run ./cmd/realcheck /tmp/keys.json
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/yanjie233/cpa-plugin-commandcode-quota/internal/quota"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("用法: realcheck <keys.json>")
		os.Exit(1)
	}
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Println("读取失败:", err)
		os.Exit(1)
	}
	var cfg struct {
		Accounts []struct {
			APIKey string `json:"api_key"`
			Label  string `json:"label"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		fmt.Println("解析失败:", err)
		os.Exit(1)
	}

	f := quota.NewFetcher(quota.DefaultBaseURL, 60*time.Second, 10*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	rows := make([]string, 0, len(cfg.Accounts))
	for _, a := range cfg.Accounts {
		if a.APIKey == "" {
			continue
		}
		snap := f.Get(ctx, a.APIKey)
		line := fmt.Sprintf("%-10s ok=%-5v 耗尽=%-5v 瓶颈=%6.1f%%",
			a.Label, snap.OK, snap.Exhausted(0.1), snap.Bottleneck())
		if snap.FiveHour != nil {
			line += fmt.Sprintf("  5h=$%.2f/%.0f", snap.FiveHour.Remaining, snap.FiveHour.Cap)
		}
		if snap.Weekly != nil {
			line += fmt.Sprintf("  周=$%.2f/%.0f", snap.Weekly.Remaining, snap.Weekly.Cap)
		}
		if snap.Monthly != nil {
			line += fmt.Sprintf("  月=$%.2f/%.0f", snap.Monthly.Remaining, snap.Monthly.Cap)
		}
		if snap.PlanName != "" {
			line += "  plan=" + snap.PlanName
		}
		if snap.Err != "" {
			line += "  err=" + snap.Err
		}
		rows = append(rows, line)
	}
	sort.Strings(rows)
	for _, r := range rows {
		fmt.Println(r)
	}
}
