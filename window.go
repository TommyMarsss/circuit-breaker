package breaker

import "time"

// windowEvent 是窗口保留的原始事件日志，仅用于全量重算校验。
type windowEvent struct {
	t    time.Time
	fail bool
}

// bucket 是窗口内一个固定宽度的时间分桶。
// slot < 0 表示该桶为空（尚未使用或已被淘汰）。
type bucket struct {
	slot  int64
	total int
	fail  int
}

// Window 是滑动时间窗口统计器。
//
// 窗口被切分为 n 个等宽分桶，每个事件按时间落入对应分桶。
// 窗口维护运行中的 total/fail 计数：新事件到来时增量累加，
// 分桶随时间滑出窗口时增量扣减，不做全量重算。
//
// 窗口的包含规则以分桶为单位定义：设当前桶序号为 cur，
// 则窗口覆盖槽位 [cur-n+1, cur]。全量重算使用完全相同的规则，
// 因此增量统计与全量重算结果必然一致（由测试保证）。
type Window struct {
	width   time.Duration // 单个分桶宽度
	buckets []bucket
	total   int
	fail    int
	events  []windowEvent // 原始日志，仅供 FullCounts 校验
}

// NewWindow 创建窗口：size 为窗口总时长，bucketCount 为分桶数。
func NewWindow(size time.Duration, bucketCount int) *Window {
	if bucketCount < 1 {
		panic("breaker: bucketCount must be >= 1")
	}
	if size <= 0 || size%time.Duration(bucketCount) != 0 {
		panic("breaker: window size must be positive and divisible by bucketCount")
	}
	w := &Window{
		width:   size / time.Duration(bucketCount),
		buckets: make([]bucket, bucketCount),
	}
	w.Reset()
	return w
}

// slotOf 返回时间 t 所属的分桶序号。
func (w *Window) slotOf(t time.Time) int64 {
	return t.UnixNano() / int64(w.width)
}

// advance 将窗口推进到 now：淘汰滑出窗口的分桶并增量扣减计数。
func (w *Window) advance(now time.Time) {
	cur := w.slotOf(now)
	minSlot := cur - int64(len(w.buckets)) + 1
	for i := range w.buckets {
		b := &w.buckets[i]
		if b.slot >= 0 && b.slot < minSlot {
			w.total -= b.total
			w.fail -= b.fail
			*b = bucket{slot: -1}
		}
	}
	// 同步裁剪原始事件日志
	cut := 0
	for cut < len(w.events) && w.slotOf(w.events[cut].t) < minSlot {
		cut++
	}
	w.events = w.events[cut:]
}

// Add 记录一次调用结果（fail 表示失败），并增量更新窗口计数。
func (w *Window) Add(t time.Time, fail bool) {
	w.advance(t)
	s := w.slotOf(t)
	b := &w.buckets[s%int64(len(w.buckets))]
	if b.slot != s {
		// 该位置上是过期桶：先扣减再覆盖
		if b.slot >= 0 {
			w.total -= b.total
			w.fail -= b.fail
		}
		*b = bucket{slot: s}
	}
	b.total++
	w.total++
	if fail {
		b.fail++
		w.fail++
	}
	w.events = append(w.events, windowEvent{t: t, fail: fail})
}

// Counts 返回 now 时刻窗口内的 (总数, 失败数)，来自增量维护的运行计数。
func (w *Window) Counts(now time.Time) (total, fail int) {
	w.advance(now)
	return w.total, w.fail
}

// FullCounts 从原始事件日志全量重算 now 时刻的窗口计数。
// 仅用于测试与仿真校验，生产路径不调用。
func (w *Window) FullCounts(now time.Time) (total, fail int) {
	w.advance(now)
	minSlot := w.slotOf(now) - int64(len(w.buckets)) + 1
	for _, e := range w.events {
		if w.slotOf(e.t) >= minSlot {
			total++
			if e.fail {
				fail++
			}
		}
	}
	return total, fail
}

// Rate 返回窗口内错误率；窗口为空时返回 0。
func (w *Window) Rate(now time.Time) float64 {
	total, fail := w.Counts(now)
	if total == 0 {
		return 0
	}
	return float64(fail) / float64(total)
}

// Reset 清空窗口（熔断器由半开转为关闭时调用，避免历史失败立即再次触发）。
func (w *Window) Reset() {
	for i := range w.buckets {
		w.buckets[i] = bucket{slot: -1}
	}
	w.total, w.fail = 0, 0
	w.events = w.events[:0]
}
