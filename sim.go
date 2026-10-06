package breaker

import (
	"fmt"
	"sort"
	"time"
)

// Call 是仿真序列中的一次下游调用。
type Call struct {
	At   time.Duration // 相对仿真起点的时刻
	Fail bool          // 调用结果是否为失败
}

// Frame 是回放数据中的一帧（某一时刻的熔断器快照）。
type Frame struct {
	T        int64   `json:"t"`              // 相对起点的毫秒数
	State    string  `json:"state"`          // CLOSED / OPEN / HALF-OPEN
	Total    int     `json:"total"`          // 窗口内请求总数
	Fail     int     `json:"fail"`           // 窗口内失败数
	Rate     float64 `json:"rate"`           // 增量统计错误率
	FullRate float64 `json:"fullRate"`       // 全量重算错误率（校验用，应与 rate 重合）
	InFlight int     `json:"inFlight"`       // 半开态在途探测数
	Kind     string  `json:"kind,omitempty"` // ok / fail / reject / ""（纯采样帧）
	Note     string  `json:"note,omitempty"` // 状态切换等需要高亮的说明
}

// InstanceReplay 是单个熔断器实例的完整回放。
type InstanceReplay struct {
	Name   string  `json:"name"`
	Frames []Frame `json:"frames"`
}

// Replay 是内嵌进 HTML 的完整回放数据。
type Replay struct {
	Threshold  float64          `json:"threshold"`
	MinSamples int              `json:"minSamples"`
	CooldownMs int64            `json:"cooldownMs"`
	Instances  []InstanceReplay `json:"instances"`
}

// Simulate 用假时钟回放一段调用序列，逐帧记录熔断器状态。
// calls 无需有序，函数内部会按时间排序。tick 为采样间隔，
// 除每个调用事件外，每隔 tick 也会采一帧，以便展示窗口滑出
// 引起的错误率自然衰减。
func Simulate(name string, cfg Config, calls []Call, end, tick time.Duration) InstanceReplay {
	sorted := make([]Call, len(calls))
	copy(sorted, calls)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].At < sorted[j].At })

	start := time.Unix(0, 0)
	var cur time.Duration
	clock := func() time.Time { return start.Add(cur) }
	b := NewBreaker(cfg, clock)

	rep := InstanceReplay{Name: name}
	prevState := Closed.String()

	snapshot := func(kind string) {
		st := b.StateAt(clock())
		total, fail, rate, fullRate := b.Stats(clock())
		f := Frame{
			T:        cur.Milliseconds(),
			State:    st.String(),
			Total:    total,
			Fail:     fail,
			Rate:     rate,
			FullRate: fullRate,
			InFlight: b.InFlight(),
			Kind:     kind,
		}
		if st.String() != prevState {
			f.Note = fmt.Sprintf("%s → %s", prevState, st.String())
			prevState = st.String()
		}
		rep.Frames = append(rep.Frames, f)
	}

	callIdx := 0
	for t := time.Duration(0); t <= end; t += tick {
		cur = t
		for callIdx < len(sorted) && sorted[callIdx].At <= t {
			cur = sorted[callIdx].At
			c := sorted[callIdx]
			p, ok := b.Allow()
			if !ok {
				snapshot("reject")
			} else {
				b.Report(p, !c.Fail)
				if c.Fail {
					snapshot("fail")
				} else {
					snapshot("ok")
				}
			}
			callIdx++
		}
		cur = t
		snapshot("")
	}
	return rep
}
