// genreport 用确定性时钟驱动一段下游调用序列，记录熔断器每一帧的
// 可观测状态，生成单一静态 HTML 文件（默认 report.html），
// 在浏览器中逐帧回放状态流转与滑动窗口错误率曲线。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	breaker "github.com/TommyMarsss/circuit-breaker"
)

var epoch = time.Unix(1_700_000_000, 0)

type frame struct {
	T      int64   `json:"t"` // 相对起始时刻的毫秒偏移
	Desc   string  `json:"desc"`
	State  string  `json:"state"`
	Total  int     `json:"total"`
	Fail   int     `json:"fail"`
	Rate   float64 `json:"rate"`
	Flight int     `json:"flight"`
	Consec int     `json:"consec"`
	Trans  bool    `json:"trans,omitempty"`
}

type sim struct {
	cb     *breaker.CircuitBreaker
	clock  *breaker.ManualClock
	frames []frame
	prev   breaker.State
}

func (s *sim) snap(atMs int64, desc string) {
	ss := s.cb.Snapshot()
	s.frames = append(s.frames, frame{
		T:      atMs,
		Desc:   desc,
		State:  ss.State.String(),
		Total:  ss.WindowTotal,
		Fail:   ss.WindowFailure,
		Rate:   ss.ErrorRate,
		Flight: ss.InFlight,
		Consec: ss.ConsecSuccess,
		Trans:  ss.State != s.prev,
	})
	s.prev = ss.State
}

func (s *sim) at(ms int64) { s.clock.Set(epoch.Add(time.Duration(ms) * time.Millisecond)) }

// call 模拟一次调用：放行则按 fail 上报结果，拒绝则记录一帧。
func (s *sim) call(ms int64, fail bool, desc string) {
	s.at(ms)
	p, ok := s.cb.Allow()
	if !ok {
		s.snap(ms, desc+" → 被熔断器拒绝")
		return
	}
	s.cb.Report(p, !fail)
	result := "成功"
	if fail {
		result = "失败"
	}
	s.snap(ms, fmt.Sprintf("%s → 放行，下游%s", desc, result))
}

// note 插入一帧纯注释（不与熔断器交互）。
func (s *sim) note(ms int64, desc string) {
	s.at(ms)
	s.snap(ms, desc)
}

func main() {
	out := "report.html"
	if len(os.Args) > 1 {
		out = os.Args[1]
	}

	cfg := breaker.Config{
		Window:           4 * time.Second,
		Buckets:          40, // 100ms 分桶
		MinRequests:      8,
		ErrorThreshold:   0.5,
		Cooldown:         2 * time.Second,
		MaxProbes:        3,
		SuccessThreshold: 3,
	}
	clock := breaker.NewManualClock(epoch)
	cb := breaker.New(cfg, clock.Now)
	s := &sim{cb: cb, clock: clock, prev: breaker.Closed}

	// 阶段一：健康流量，偶发失败
	s.note(0, "阶段一：健康流量，错误率远低于阈值")
	for i, ms := range []int64{0, 200, 400, 600, 800, 1000, 1200, 1400, 1600, 1800} {
		s.call(ms, ms == 400 || ms == 1200, fmt.Sprintf("请求 #%d", i+1))
	}

	// 阶段二：下游故障风暴，错误率爬升越过阈值 → 打开
	s.note(2000, "阶段二：下游开始大面积失败")
	for i, ms := range []int64{2000, 2150, 2300, 2450, 2600, 2750, 2900, 3050} {
		s.call(ms, ms != 2000, fmt.Sprintf("故障请求 #%d", i+1))
	}
	s.note(3050, "错误率达到 50% 且样本数达标 → 熔断器打开")

	// 阶段三：打开态，所有请求快速失败；冷却边界精确探测
	for _, ms := range []int64{3400, 3800, 4200, 4600} {
		s.call(ms, false, "积压请求")
	}
	s.call(5040, false, "冷却边界前 10ms 的请求")
	s.note(5050, "冷却期满（openedAt+2s），边界时刻请求进入半开态")

	// 阶段四：半开态并发探测竞争配额
	s.at(5050)
	p1, ok1 := cb.Allow()
	s.snap(5050, "半开：探测 P1 占用配额 1/3")
	p2, ok2 := cb.Allow()
	s.snap(5050, "半开：探测 P2 占用配额 2/3")
	p3, ok3 := cb.Allow()
	s.snap(5050, "半开：探测 P3 占用配额 3/3")
	if _, ok4 := cb.Allow(); ok4 {
		panic("配额超发！")
	}
	s.snap(5050, "半开：第 4 个并发探测被配额拒绝")
	if !ok1 || !ok2 || !ok3 {
		panic("半开应放行 3 个探测")
	}
	s.at(5200)
	cb.Report(p1, true)
	s.snap(5200, "探测 P1 成功（连续成功 1/3）")
	s.at(5300)
	cb.Report(p2, false)
	s.snap(5300, "探测 P2 失败 → 立即回到打开态，冷却重新计时")
	s.at(5400)
	cb.Report(p3, true) // 跨代际的过期许可，必须被忽略
	s.snap(5400, "迟到的 P3 结果到达：许可已过期，结果被丢弃")

	// 阶段五：再次打开，冷却重置为 5300+2000=7300
	for _, ms := range []int64{5800, 6300, 6800, 7290} {
		s.call(ms, false, "积压请求")
	}
	s.note(7300, "新冷却期满，再次进入半开态")

	// 阶段六：三个探测连续成功 → 完全关闭
	s.at(7300)
	q1, _ := cb.Allow()
	s.snap(7300, "半开：探测 Q1 放行")
	q2, _ := cb.Allow()
	s.snap(7300, "半开：探测 Q2 放行")
	q3, _ := cb.Allow()
	s.snap(7300, "半开：探测 Q3 放行")
	s.at(7400)
	cb.Report(q1, true)
	s.snap(7400, "探测 Q1 成功（连续成功 1/3）")
	s.at(7450)
	cb.Report(q2, true)
	s.snap(7450, "探测 Q2 成功（连续成功 2/3）")
	s.at(7500)
	cb.Report(q3, true)
	s.snap(7500, "探测 Q3 成功（连续成功 3/3）→ 熔断器关闭")

	// 阶段七：恢复；末尾低流量单次失败不触发（最小样本数门控）
	s.note(7700, "阶段七：流量恢复")
	for i, ms := range []int64{7700, 7900, 8100, 8300, 8500, 8700, 8900} {
		s.call(ms, false, fmt.Sprintf("恢复请求 #%d", i+1))
	}
	s.call(13100, false, "稀疏请求 #1")
	s.call(13700, false, "稀疏请求 #2")
	s.call(14300, true, "稀疏请求 #3（失败）")
	s.note(14300, "窗口内仅 3 个样本 < 最小样本数 8：单次失败不触发熔断")

	data, err := json.Marshal(s.frames)
	if err != nil {
		panic(err)
	}
	meta, err := json.Marshal(map[string]any{
		"windowMs":         cfg.Window.Milliseconds(),
		"minRequests":      cfg.MinRequests,
		"errorThreshold":   cfg.ErrorThreshold,
		"cooldownMs":       cfg.Cooldown.Milliseconds(),
		"maxProbes":        cfg.MaxProbes,
		"successThreshold": cfg.SuccessThreshold,
	})
	if err != nil {
		panic(err)
	}

	html := strings.NewReplacer("__META__", string(meta), "__FRAMES__", string(data)).
		Replace(reportTemplate)
	if err := os.WriteFile(out, []byte(html), 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("wrote %s (%d frames)\n", out, len(s.frames))
}
