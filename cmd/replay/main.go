// 命令 replay 运行一段预置的调用序列仿真，生成单一的静态 HTML 回放文件。
// 用法：go run ./cmd/replay [输出路径]（默认 replay.html）
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	breaker "github.com/TommyMarsss/circuit-breaker"
)

//go:embed template.html
var template string

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

// 在 [from, to) 内每隔 step 生成一次调用。
func burst(calls *[]breaker.Call, from, step, count int, fail bool) {
	for i := 0; i < count; i++ {
		*calls = append(*calls, breaker.Call{At: ms(from + i*step), Fail: fail})
	}
}

func main() {
	out := "replay.html"
	if len(os.Args) > 1 {
		out = os.Args[1]
	}

	cfg := breaker.Config{
		WindowSize:               10 * time.Second,
		WindowBuckets:            10,
		ErrorThreshold:           0.5,
		MinSamples:               10,
		Cooldown:                 5 * time.Second,
		HalfOpenMaxProbes:        3,
		HalfOpenSuccessThreshold: 3,
	}

	// 实例 A：完整走完 关闭→打开→半开→(探测失败)再打开→半开→关闭 的全流程。
	var a []breaker.Call
	burst(&a, 0, 100, 10, false)                          // 0-1s：10 次成功，健康流量
	burst(&a, 2000, 80, 12, true)                         // 2-3s：连续 12 次失败 → 错误率 12/22≈55% > 50% → 打开
	burst(&a, 3500, 500, 9, false)                        // 3.5-7.5s：打开期间，全部被快速拒绝
	burst(&a, 7800, 50, 6, false)                         // 7.8s：冷却结束进入半开，3 个探测放行、3 个被配额拒绝
	a = append(a, breaker.Call{At: ms(7850), Fail: true}) // 一个探测失败 → 立即回到打开态
	burst(&a, 8500, 500, 9, false)                        // 8.5-12.5s：再次打开，全部拒绝
	burst(&a, 13100, 100, 3, false)                       // 13.1s：冷却结束，3 个探测全部成功 → 关闭
	burst(&a, 14000, 200, 25, false)                      // 14-19s：恢复流量，偶发失败
	a = append(a, breaker.Call{At: ms(15500), Fail: true})
	a = append(a, breaker.Call{At: ms(17200), Fail: true})

	// 实例 B：低流量场景。少量失败因未达最小样本数不会误触发，
	// 与实例 A 完全独立统计、互不影响。
	var b []breaker.Call
	burst(&b, 1000, 1000, 3, false)
	burst(&b, 5000, 300, 4, true) // 5s 左右 4 次失败：错误率 4/7≈57% 但样本数 7 < 10 → 保持关闭
	burst(&b, 8000, 800, 10, false)
	b = append(b, breaker.Call{At: ms(12000), Fail: true})
	burst(&b, 14000, 500, 8, false)

	replay := breaker.Replay{
		Threshold:  cfg.ErrorThreshold,
		MinSamples: cfg.MinSamples,
		CooldownMs: cfg.Cooldown.Milliseconds(),
		Instances: []breaker.InstanceReplay{
			breaker.Simulate("实例 A（完整熔断周期）", cfg, a, 20*time.Second, 100*time.Millisecond),
			breaker.Simulate("实例 B（低流量最小样本门控）", cfg, b, 20*time.Second, 100*time.Millisecond),
		},
	}

	data, err := json.Marshal(replay)
	if err != nil {
		fmt.Fprintln(os.Stderr, "marshal:", err)
		os.Exit(1)
	}
	html := strings.Replace(template, "__DATA__", string(data), 1)
	if err := os.WriteFile(out, []byte(html), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Printf("已生成 %s（%d 帧 × %d 实例），用浏览器打开即可回放。\n",
		out, len(replay.Instances[0].Frames), len(replay.Instances))
}
