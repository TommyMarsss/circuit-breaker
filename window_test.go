package breaker

import (
	"math/rand"
	"testing"
	"time"
)

type rawEvent struct {
	at      time.Time
	failure bool
}

// 与 slidingWindow 相同的分桶语义，对原始事件日志做全量重算：
// 仅统计分桶下标落在 [currentIdx-n+1, currentIdx] 内的事件。
func fullRecompute(events []rawEvent, w *slidingWindow, now time.Time) (total, failures int) {
	cur := w.bucketIndex(now)
	cutoff := cur - int64(w.n) + 1
	for _, e := range events {
		idx := w.bucketIndex(e.at)
		if idx >= cutoff && idx <= cur {
			total++
			if e.failure {
				failures++
			}
		}
	}
	return total, failures
}

// 增量统计与全量重算在随机事件流下逐步一致。
func TestWindowIncrementalMatchesFullRecompute(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	w := newSlidingWindow(10*time.Second, 10) // 1s 分桶
	start := time.Unix(1_700_000_000, 0)

	var events []rawEvent
	now := start
	compare := func(at time.Time, step int) {
		gotT, gotF := w.counts(at)
		wantT, wantF := fullRecompute(events, w, at)
		if gotT != wantT || gotF != wantF {
			t.Fatalf("step %d: incremental=(%d,%d) full=(%d,%d)", step, gotT, gotF, wantT, wantF)
		}
	}
	for i := 0; i < 2000; i++ {
		// 随机前进 0~1500ms，制造分桶跨越与整窗滑出
		now = now.Add(time.Duration(rng.Intn(1500)) * time.Millisecond)
		failure := rng.Float64() < 0.4
		w.add(now, failure)
		events = append(events, rawEvent{now, failure})
		compare(now, i) // 窗口假设时间单调，查询时刻不得越过未来写入
	}
	// 末尾再单调推进查询若干次，覆盖纯淘汰路径
	for i := 0; i < 30; i++ {
		now = now.Add(time.Duration(rng.Intn(2000)) * time.Millisecond)
		compare(now, 2000+i)
	}
}

// 滑出窗口的分桶必须被增量淘汰：整窗过期后计数归零。
func TestWindowEvictsExpiredBuckets(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)

	w := newSlidingWindow(4*time.Second, 4) // 1s 分桶
	w.add(start, true)
	w.add(start.Add(time.Second), true)
	if total, failures := w.counts(start.Add(2 * time.Second)); total != 2 || failures != 2 {
		t.Fatalf("before eviction: got (%d,%d), want (2,2)", total, failures)
	}
	// 前进超过整个窗口
	if total, failures := w.counts(start.Add(10 * time.Second)); total != 0 || failures != 0 {
		t.Fatalf("after full eviction: got (%d,%d), want (0,0)", total, failures)
	}

	// 部分滑出：t=0 的分桶已淘汰，t=3s 的仍在窗口内
	w2 := newSlidingWindow(4*time.Second, 4)
	w2.add(start, true)
	w2.add(start.Add(3*time.Second), false)
	if total, failures := w2.counts(start.Add(4500 * time.Millisecond)); total != 1 || failures != 0 {
		t.Fatalf("partial eviction: got (%d,%d), want (1,0)", total, failures)
	}
}

// 时间回退不应破坏统计（计数单调，不倒退）。
func TestWindowClockRegression(t *testing.T) {
	w := newSlidingWindow(4*time.Second, 4)
	start := time.Unix(1_700_000_000, 0)
	w.add(start.Add(2*time.Second), true)
	w.add(start.Add(3*time.Second), false)
	// 回退到更早时刻查询：统计保持不变
	if total, failures := w.counts(start); total != 2 || failures != 1 {
		t.Fatalf("clock regression: got (%d,%d), want (2,1)", total, failures)
	}
}
