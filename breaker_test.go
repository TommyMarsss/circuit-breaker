package breaker

import (
	"sync"
	"testing"
	"time"
)

func testConfig() Config {
	return Config{
		Window:           10 * time.Second,
		Buckets:          10,
		MinRequests:      10,
		ErrorThreshold:   0.5,
		Cooldown:         5 * time.Second,
		MaxProbes:        3,
		SuccessThreshold: 3,
	}
}

var epoch = time.Unix(1_700_000_000, 0)

// call 在时钟指向 now 时执行一次完整调用（Allow + Report）。
func call(cb *CircuitBreaker, c *ManualClock, now time.Time, fail bool) (allowed bool) {
	c.Set(now)
	p, ok := cb.Allow()
	if !ok {
		return false
	}
	cb.Report(p, !fail)
	return true
}

// 打开熔断器：在窗口内制造 MinRequests 个请求、80% 失败。
func forceOpen(cb *CircuitBreaker, c *ManualClock, start time.Time) time.Time {
	t := start
	for i := 0; i < 10; i++ {
		call(cb, c, t, i != 0 && i != 5) // 8/10 失败
		t = t.Add(100 * time.Millisecond)
	}
	return t
}

// 最小样本数门控：低流量下即使全部失败也不打开。
func TestMinRequestsGating(t *testing.T) {
	c := NewManualClock(epoch)
	cb := New(testConfig(), c.Now)

	for i := 0; i < 9; i++ { // 9 < MinRequests(10)，全部失败
		if !call(cb, c, epoch.Add(time.Duration(i)*100*time.Millisecond), true) {
			t.Fatalf("call %d should be allowed", i)
		}
	}
	if s := cb.Snapshot().State; s != Closed {
		t.Fatalf("below MinRequests with 100%% failure: state=%v, want closed", s)
	}
	// 第 10 个请求失败，样本数达标且错误率 100% → 打开
	call(cb, c, epoch.Add(900*time.Millisecond), true)
	if s := cb.Snapshot().State; s != Open {
		t.Fatalf("at MinRequests with 100%% failure: state=%v, want open", s)
	}
}

// 错误率超阈值且样本达标才打开；低于阈值不打开。
func TestErrorRateThreshold(t *testing.T) {
	c := NewManualClock(epoch)
	cb := New(testConfig(), c.Now)

	// 10 个请求 4 个失败（40% < 50%）→ 保持关闭
	tm := epoch
	for i := 0; i < 10; i++ {
		call(cb, c, tm, i < 4)
		tm = tm.Add(100 * time.Millisecond)
	}
	if s := cb.Snapshot().State; s != Closed {
		t.Fatalf("40%% error rate: state=%v, want closed", s)
	}
	// 再来 10 个请求 8 个失败 → 窗口内 20 个请求 12 个失败（60%）→ 打开
	for i := 0; i < 10; i++ {
		call(cb, c, tm, i < 8)
		tm = tm.Add(100 * time.Millisecond)
	}
	if s := cb.Snapshot().State; s != Open {
		t.Fatalf("60%% error rate: state=%v, want open", s)
	}
	// 打开态拒绝请求
	if call(cb, c, tm, false) {
		t.Fatal("open breaker should reject")
	}
}

// 半开态探测配额在并发竞争下不超发。
func TestHalfOpenProbeQuotaConcurrent(t *testing.T) {
	c := NewManualClock(epoch)
	cfg := testConfig()
	cb := New(cfg, c.Now)

	forceOpen(cb, c, epoch)
	openedAt := cb.Snapshot().OpenedAt

	// 冷却期满进入半开
	c.Set(openedAt.Add(cfg.Cooldown))

	const goroutines = 64
	permits := make(chan Permit, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p, ok := cb.Allow(); ok {
				permits <- p
			}
		}()
	}
	wg.Wait()
	close(permits)

	granted := 0
	for range permits {
		granted++
	}
	if granted != cfg.MaxProbes {
		t.Fatalf("granted %d probes, want exactly %d (quota must not be oversubscribed)", granted, cfg.MaxProbes)
	}
	if got := cb.Snapshot().InFlight; got != cfg.MaxProbes {
		t.Fatalf("in-flight=%d, want %d", got, cfg.MaxProbes)
	}
	// 配额占满后继续拒绝
	if _, ok := cb.Allow(); ok {
		t.Fatal("quota exhausted: Allow should reject")
	}
}

// 半开态任一探测失败立即回到打开态，且冷却计时重置。
func TestProbeFailureReopensAndResetsCooldown(t *testing.T) {
	c := NewManualClock(epoch)
	cfg := testConfig()
	cb := New(cfg, c.Now)

	forceOpen(cb, c, epoch)
	firstOpen := cb.Snapshot().OpenedAt
	c.Set(firstOpen.Add(cfg.Cooldown))

	// 两个探测：第一个成功，第二个失败 → 立即回打开
	p1, ok1 := cb.Allow()
	p2, ok2 := cb.Allow()
	if !ok1 || !ok2 {
		t.Fatal("half-open should grant probes")
	}
	failAt := firstOpen.Add(cfg.Cooldown).Add(200 * time.Millisecond)
	c.Set(failAt)
	cb.Report(p1, true)
	if s := cb.Snapshot().State; s != HalfOpen {
		t.Fatalf("one success should keep half-open, got %v", s)
	}
	cb.Report(p2, false)
	if s := cb.Snapshot().State; s != Open {
		t.Fatalf("probe failure should reopen, got %v", s)
	}

	// 冷却必须重置：旧冷却期满时刻仍应拒绝
	reopened := cb.Snapshot().OpenedAt
	if !reopened.Equal(failAt) {
		t.Fatalf("openedAt=%v, want reset to failure time %v", reopened, failAt)
	}
	c.Set(firstOpen.Add(2 * cfg.Cooldown)) // 旧冷却早已过期
	if _, ok := cb.Allow(); ok {
		t.Fatal("cooldown not reset: request allowed at stale boundary")
	}
	// 新冷却期满 → 半开
	c.Set(failAt.Add(cfg.Cooldown))
	if _, ok := cb.Allow(); !ok {
		t.Fatal("new cooldown elapsed: should allow probe")
	}
}

// 连续 SuccessThreshold 个探测成功后完全关闭。
func TestHalfOpenSuccessCloses(t *testing.T) {
	c := NewManualClock(epoch)
	cfg := testConfig()
	cb := New(cfg, c.Now)

	forceOpen(cb, c, epoch)
	c.Set(cb.Snapshot().OpenedAt.Add(cfg.Cooldown))

	for i := 0; i < cfg.SuccessThreshold; i++ {
		p, ok := cb.Allow()
		if !ok {
			t.Fatalf("probe %d should be allowed", i)
		}
		cb.Report(p, true)
		if i < cfg.SuccessThreshold-1 && cb.Snapshot().State != HalfOpen {
			t.Fatalf("after %d successes: state=%v, want half-open", i+1, cb.Snapshot().State)
		}
	}
	if s := cb.Snapshot().State; s != Closed {
		t.Fatalf("after %d consecutive successes: state=%v, want closed", cfg.SuccessThreshold, s)
	}
	// 关闭后窗口统计已重置
	if snap := cb.Snapshot(); snap.WindowTotal != 0 {
		t.Fatalf("window should reset on close, got total=%d", snap.WindowTotal)
	}
}

// 冷却边界时刻的状态判定一致性：
// now < openedAt+Cooldown → 打开（拒绝）；now >= openedAt+Cooldown → 半开（放行探测）。
// 边界含于半开侧，同一时刻重复判定结果一致。
func TestCooldownBoundaryConsistency(t *testing.T) {
	c := NewManualClock(epoch)
	cfg := testConfig()
	cb := New(cfg, c.Now)

	forceOpen(cb, c, epoch)
	openedAt := cb.Snapshot().OpenedAt
	boundary := openedAt.Add(cfg.Cooldown)

	// 边界前 1ns：打开，拒绝；且重复判定一致
	c.Set(boundary.Add(-time.Nanosecond))
	for i := 0; i < 3; i++ {
		if _, ok := cb.Allow(); ok {
			t.Fatal("1ns before boundary: should reject")
		}
		if s := cb.Snapshot().State; s != Open {
			t.Fatalf("1ns before boundary: state=%v, want open", s)
		}
	}

	// 恰在边界：半开，放行探测；同一时刻重复判定一致
	c.Set(boundary)
	p, ok := cb.Allow()
	if !ok {
		t.Fatal("at boundary: should allow probe (cooldown elapsed, boundary inclusive)")
	}
	for i := 0; i < 3; i++ {
		if s := cb.Snapshot().State; s != HalfOpen {
			t.Fatalf("at boundary repeated check: state=%v, want half-open", s)
		}
	}
	// 同一时刻第二个 Allow：仍是半开判定（配额内放行）
	if _, ok := cb.Allow(); !ok {
		t.Fatal("at boundary: second probe within quota should be allowed")
	}
	cb.Report(p, false) // 清理：回到打开
}

// 过期许可（跨状态代际）的上报必须被忽略。
func TestStalePermitIgnored(t *testing.T) {
	c := NewManualClock(epoch)
	cfg := testConfig()
	cb := New(cfg, c.Now)

	// 关闭态放行一个请求但不立即上报
	c.Set(epoch)
	stale, ok := cb.Allow()
	if !ok {
		t.Fatal("should be allowed")
	}
	// 其他请求把熔断器打开
	forceOpen(cb, c, epoch.Add(100*time.Millisecond))
	if cb.Snapshot().State != Open {
		t.Fatal("should be open")
	}
	// 迟到的关闭态许可上报失败：不得影响打开态（不得重复计数/改变状态）
	c.Set(epoch.Add(200 * time.Millisecond))
	cb.Report(stale, false)
	if s := cb.Snapshot().State; s != Open {
		t.Fatalf("stale report changed state to %v", s)
	}

	// 半开代际的过期许可同样被忽略
	c.Set(cb.Snapshot().OpenedAt.Add(cfg.Cooldown))
	p1, _ := cb.Allow() // 半开探测 1
	p2, _ := cb.Allow() // 半开探测 2
	cb.Report(p1, false)
	if cb.Snapshot().State != Open {
		t.Fatal("probe failure should reopen")
	}
	// 再次进入半开后，上一代际的 p2 上报成功不得计入关闭进度
	c.Set(cb.Snapshot().OpenedAt.Add(cfg.Cooldown))
	if _, ok := cb.Allow(); !ok {
		t.Fatal("should be half-open again")
	}
	cb.Report(p2, true) // 过期许可
	if got := cb.Snapshot().ConsecSuccess; got != 0 {
		t.Fatalf("stale probe success counted: consecSuccess=%d, want 0", got)
	}
}

// 多实例各自独立统计、独立流转，互不影响。
func TestMultiInstanceIndependence(t *testing.T) {
	c := NewManualClock(epoch)
	cfg := testConfig()
	a := New(cfg, c.Now)
	b := New(cfg, c.Now)

	// 实例 A 打满失败 → 打开
	forceOpen(a, c, epoch)
	// 同一时刻实例 B 全部成功
	tm := epoch
	for i := 0; i < 10; i++ {
		call(b, c, tm, false)
		tm = tm.Add(100 * time.Millisecond)
	}

	if s := a.Snapshot().State; s != Open {
		t.Fatalf("instance A: state=%v, want open", s)
	}
	if s := b.Snapshot().State; s != Closed {
		t.Fatalf("instance B: state=%v, want closed (must not be affected by A)", s)
	}
	// A 拒绝时 B 仍放行
	if _, ok := a.Allow(); ok {
		t.Fatal("A should reject while open")
	}
	if _, ok := b.Allow(); !ok {
		t.Fatal("B should allow while A is open")
	}
}

// 半开态配额在多次打开-半开循环中都不超发（配合 -race 运行）。
func TestProbeQuotaAcrossCycles(t *testing.T) {
	c := NewManualClock(epoch)
	cfg := testConfig()
	cb := New(cfg, c.Now)

	forceOpen(cb, c, epoch)
	if cb.Snapshot().State != Open {
		t.Fatal("setup: breaker should be open")
	}

	for cycle := 0; cycle < 5; cycle++ {
		// 推进到冷却期满
		c.Set(cb.Snapshot().OpenedAt.Add(cfg.Cooldown))

		// 并发竞争探测配额
		permits := make(chan Permit, 32)
		var wg sync.WaitGroup
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if p, ok := cb.Allow(); ok {
					permits <- p
				}
			}()
		}
		wg.Wait()
		close(permits)

		var granted []Permit
		for p := range permits {
			granted = append(granted, p)
		}
		if len(granted) != cfg.MaxProbes {
			t.Fatalf("cycle %d: granted %d probes, want %d", cycle, len(granted), cfg.MaxProbes)
		}
		// 任取一个探测上报失败 → 立即重开，进入下一轮
		c.Set(c.Now().Add(50 * time.Millisecond))
		cb.Report(granted[0], false)
		if s := cb.Snapshot().State; s != Open {
			t.Fatalf("cycle %d: state=%v, want open after probe failure", cycle, s)
		}
	}
}
