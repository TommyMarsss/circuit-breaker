package breaker

import "time"

// bucket 保存一个时间分桶内的请求计数。
type bucket struct {
	total    int
	failures int
}

// slidingWindow 是基于固定分桶的滑动时间窗口统计。
//
// 窗口被划分为 n 个等长分桶，每个分桶记录落在其中的请求总数与失败数。
// 窗口整体维护 total / failures 两个滚动计数：新请求到来时增量累加，
// 时间前进时把滑出窗口的分桶计数从滚动值中增量扣除，而不是每次全量重算。
//
// 淘汰以分桶为粒度：任意时刻窗口覆盖的分桶下标区间为
// [currentIdx-n+1, currentIdx]，其中 currentIdx = now / bucketDur。
// 因此 Counts 的语义是"最近 n 个分桶（含当前分桶）的累计"，
// 与全量重算（对原始事件按同一分桶下标过滤后求和）严格一致。
//
// slidingWindow 本身不加锁，并发安全由持有方（CircuitBreaker）的互斥锁保证。
type slidingWindow struct {
	bucketDur int64 // 分桶宽度，纳秒
	n         int   // 分桶个数
	buckets   []bucket
	lastIdx   int64 // 最近一次 advance 对齐到的分桶下标
	started   bool
	total     int
	failures  int
}

func newSlidingWindow(window time.Duration, n int) *slidingWindow {
	if n <= 0 {
		n = 1
	}
	d := window.Nanoseconds() / int64(n)
	if d <= 0 {
		d = 1
	}
	return &slidingWindow{
		bucketDur: d,
		n:         n,
		buckets:   make([]bucket, n),
	}
}

// bucketIndex 返回 t 所属的分桶下标。
func (w *slidingWindow) bucketIndex(t time.Time) int64 {
	return t.UnixNano() / w.bucketDur
}

// advance 把窗口对齐到 now 所在分桶，增量淘汰其间滑出的分桶。
// 时间回退（now 落在 lastIdx 之前）时不做任何事，统计保持单调。
func (w *slidingWindow) advance(now time.Time) {
	idx := w.bucketIndex(now)
	if !w.started {
		w.started = true
		w.lastIdx = idx
		return
	}
	if idx <= w.lastIdx {
		return
	}
	steps := idx - w.lastIdx
	if steps > int64(w.n) {
		steps = int64(w.n) // 整窗过期，清空全部即可
	}
	for i := int64(1); i <= steps; i++ {
		b := &w.buckets[(w.lastIdx+i)%int64(w.n)]
		w.total -= b.total
		w.failures -= b.failures
		*b = bucket{}
	}
	w.lastIdx = idx
}

// add 记录一次请求结果。failure 为 true 表示失败。
func (w *slidingWindow) add(now time.Time, failure bool) {
	w.advance(now)
	b := &w.buckets[w.lastIdx%int64(w.n)]
	b.total++
	w.total++
	if failure {
		b.failures++
		w.failures++
	}
}

// counts 返回 now 时刻窗口内的请求总数与失败数。
// 注意：它会先把窗口对齐到 now（增量淘汰过期分桶），属于变更操作。
func (w *slidingWindow) counts(now time.Time) (total, failures int) {
	w.advance(now)
	return w.total, w.failures
}

// reset 清空窗口（熔断器关闭/打开时重置统计）。
func (w *slidingWindow) reset() {
	for i := range w.buckets {
		w.buckets[i] = bucket{}
	}
	w.total = 0
	w.failures = 0
	w.started = false
	w.lastIdx = 0
}
