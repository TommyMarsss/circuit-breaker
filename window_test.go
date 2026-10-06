package breaker

import (
	"math/rand"
	"testing"
	"time"
)

func TestWindowIncrementalMatchesFullRecompute(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	w := NewWindow(10*time.Second, 10)
	base := time.Unix(0, 0)

	// 随机生成 2000 个事件，每加一个都校验增量统计与全量重算一致。
	for i := 0; i < 2000; i++ {
		at := base.Add(time.Duration(rng.Intn(50)) * time.Millisecond).Add(time.Duration(i) * 25 * time.Millisecond)
		w.Add(at, rng.Intn(2) == 0)
		total, fail := w.Counts(at)
		ftotal, ffail := w.FullCounts(at)
		if total != ftotal || fail != ffail {
			t.Fatalf("第 %d 个事件后不一致: 增量=(%d,%d) 全量=(%d,%d)",
				i, total, fail, ftotal, ffail)
		}
	}
}

func TestWindowExpiry(t *testing.T) {
	w := NewWindow(10*time.Second, 10) // 每桶 1s
	base := time.Unix(0, 0)

	w.Add(base.Add(500*time.Millisecond), true)  // 槽 0
	w.Add(base.Add(1500*time.Millisecond), true) // 槽 1

	// 10.5s 时槽 0 已滑出窗口（窗口覆盖槽 1..10）
	total, fail := w.Counts(base.Add(10500 * time.Millisecond))
	if total != 1 || fail != 1 {
		t.Fatalf("滑出后计数错误: got (%d,%d), want (1,1)", total, fail)
	}

	// 11.5s 时槽 1 也滑出
	total, fail = w.Counts(base.Add(11500 * time.Millisecond))
	if total != 0 || fail != 0 {
		t.Fatalf("全部滑出后计数应为 0: got (%d,%d)", total, fail)
	}
	if r := w.Rate(base.Add(11500 * time.Millisecond)); r != 0 {
		t.Fatalf("空窗口错误率应为 0: got %v", r)
	}
}

func TestWindowReset(t *testing.T) {
	w := NewWindow(10*time.Second, 10)
	base := time.Unix(0, 0)
	w.Add(base, true)
	w.Add(base.Add(time.Second), false)
	w.Reset()
	total, fail := w.Counts(base)
	if total != 0 || fail != 0 {
		t.Fatalf("Reset 后计数应为 0: got (%d,%d)", total, fail)
	}
	ftotal, ffail := w.FullCounts(base)
	if ftotal != 0 || ffail != 0 {
		t.Fatalf("Reset 后全量重算应为 0: got (%d,%d)", ftotal, ffail)
	}
}
