package breaker

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testConfig() Config {
	return Config{
		WindowSize:               10 * time.Second,
		WindowBuckets:            10,
		ErrorThreshold:           0.5,
		MinSamples:               10,
		Cooldown:                 5 * time.Second,
		HalfOpenMaxProbes:        3,
		HalfOpenSuccessThreshold: 3,
	}
}

// clock 是可手动推进的假时钟。
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) set(t time.Time)         { c.t = t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestBreaker(c *clock) *Breaker {
	return NewBreaker(testConfig(), c.now)
}

// drive 在时刻 t 上报 n 次结果。
func drive(b *Breaker, n int, success bool) {
	for i := 0; i < n; i++ {
		p, ok := b.Allow()
		if !ok {
			continue
		}
		b.Report(p, success)
	}
}

func TestMinSamplesGating(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	b := newTestBreaker(c)

	// 5 次失败、0 次成功：错误率 100%，但样本数 5 < MinSamples(10)，不得打开。
	drive(b, 5, false)
	if got := b.State(); got != Closed {
		t.Fatalf("低样本数下不应打开: got %v", got)
	}
}

func TestOpenOnErrorRate(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	b := newTestBreaker(c)

	drive(b, 5, true)  // 5 成功
	drive(b, 5, false) // 5 失败：样本 10，错误率 50%，不大于阈值 → 仍关闭
	if got := b.State(); got != Closed {
		t.Fatalf("错误率等于阈值不应打开: got %v", got)
	}
	drive(b, 1, false) // 错误率 6/11 ≈ 54.5% > 50% → 打开
	if got := b.State(); got != Open {
		t.Fatalf("错误率超阈值应打开: got %v", got)
	}

	// 打开态下请求被快速拒绝
	if _, ok := b.Allow(); ok {
		t.Fatal("打开态应拒绝请求")
	}
}

func TestHalfOpenProbeQuotaConcurrency(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	b := newTestBreaker(c)

	drive(b, 11, false)        // 触发打开
	c.advance(5 * time.Second) // 冷却结束

	// 100 个 goroutine 并发竞争探测配额，放行数必须恰好等于 3。
	var granted int64
	permits := make(chan Permit, 100)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p, ok := b.Allow(); ok {
				atomic.AddInt64(&granted, 1)
				permits <- p
			}
		}()
	}
	wg.Wait()
	close(permits)

	if granted != int64(testConfig().HalfOpenMaxProbes) {
		t.Fatalf("探测配额被超发: 放行 %d, 上限 %d", granted, testConfig().HalfOpenMaxProbes)
	}
	if n := b.InFlight(); n != testConfig().HalfOpenMaxProbes {
		t.Fatalf("在途探测数错误: got %d", n)
	}

	// 释放一个配额（上报一次成功）后，应能再放行一个，但不能更多。
	p := <-permits
	b.Report(p, true)
	if _, ok := b.Allow(); !ok {
		t.Fatal("释放配额后应能再放行一个探测")
	}
	if _, ok := b.Allow(); ok {
		t.Fatal("配额已满，不应继续放行")
	}
}

func TestHalfOpenFailureResetsCooldown(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	b := newTestBreaker(c)

	drive(b, 11, false)        // t=0 打开
	c.advance(5 * time.Second) // t=5s 冷却结束 → 半开

	p, ok := b.Allow()
	if !ok {
		t.Fatal("冷却后应进入半开并放行探测")
	}
	c.advance(1 * time.Second) // t=6s 探测在途
	b.Report(p, false)         // 探测失败 → 立即回到打开态，冷却从 t=6s 重计

	if got := b.State(); got != Open {
		t.Fatalf("探测失败后应回到打开态: got %v", got)
	}

	// 旧的冷却边界（t=10s）不应生效：t=10.5s 时距重新打开仅 4.5s，仍应拒绝。
	c.set(time.Unix(0, 0).Add(10500 * time.Millisecond))
	if _, ok := b.Allow(); ok {
		t.Fatal("冷却计时已重置，旧边界不应放行")
	}

	// 新边界 t=11s 应放行。
	c.set(time.Unix(0, 0).Add(11 * time.Second))
	if _, ok := b.Allow(); !ok {
		t.Fatal("重置后的冷却期满应进入半开并放行")
	}
}

func TestHalfOpenSuccessCloses(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	b := newTestBreaker(c)

	drive(b, 11, false)
	c.advance(5 * time.Second)

	// 逐批放行 3 个探测并全部成功 → 达到成功阈值 → 关闭。
	for i := 0; i < 3; i++ {
		p, ok := b.Allow()
		if !ok {
			t.Fatalf("第 %d 个探测应被放行", i)
		}
		b.Report(p, true)
	}
	if got := b.State(); got != Closed {
		t.Fatalf("连续成功达到阈值后应关闭: got %v", got)
	}

	// 关闭后窗口已重置：立刻来 5 个失败（样本不足）不应再次打开。
	drive(b, 5, false)
	if got := b.State(); got != Closed {
		t.Fatalf("关闭后窗口应重新统计，低样本不应打开: got %v", got)
	}
}

func TestBoundaryConsistency(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	b := newTestBreaker(c)
	drive(b, 11, false) // t=0 打开

	boundary := time.Unix(0, 0).Add(5 * time.Second)

	// 边界前 1ns：打开；边界上：半开；边界后 1ns：半开。
	if got := b.StateAt(boundary.Add(-time.Nanosecond)); got != Open {
		t.Fatalf("边界前 1ns 应为打开: got %v", got)
	}
	if got := b.StateAt(boundary); got != HalfOpen {
		t.Fatalf("边界上应为半开: got %v", got)
	}
	if got := b.StateAt(boundary.Add(time.Nanosecond)); got != HalfOpen {
		t.Fatalf("边界后 1ns 应为半开: got %v", got)
	}

	// 同一时刻重复判定必须一致（判定是幂等的纯函数）。
	for i := 0; i < 100; i++ {
		if got := b.StateAt(boundary); got != HalfOpen {
			t.Fatalf("第 %d 次边界判定不一致: got %v", i, got)
		}
	}

	// Allow 在边界上的行为与 StateAt 一致。
	c.set(boundary.Add(-time.Nanosecond))
	if _, ok := b.Allow(); ok {
		t.Fatal("边界前 1ns 应拒绝")
	}
	c.set(boundary)
	if _, ok := b.Allow(); !ok {
		t.Fatal("边界上应按半开态放行探测")
	}
}

func TestStaleProbePermitIgnored(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	b := newTestBreaker(c)

	drive(b, 11, false)
	c.advance(5 * time.Second)

	// 第一个半开周期：拿到两个探测许可，一个失败 → 重新打开。
	stale, ok1 := b.Allow()
	p2, ok2 := b.Allow()
	if !ok1 || !ok2 {
		t.Fatal("半开态应放行探测")
	}
	b.Report(p2, false) // → 打开

	c.advance(5 * time.Second) // 第二个半开周期
	if _, ok := b.Allow(); !ok {
		t.Fatal("第二个半开周期应放行探测")
	}

	// 上一周期迟到的成功结果不得计入本周期。
	b.Report(stale, true)
	if got := b.State(); got != HalfOpen {
		t.Fatalf("过期许可的结果应被忽略: got %v", got)
	}
}

func TestMultiInstanceIndependence(t *testing.T) {
	c := &clock{t: time.Unix(0, 0)}
	a := newTestBreaker(c)
	b := newTestBreaker(c)

	// 把 A 打到打开态，B 只接收健康流量。
	drive(a, 11, false)
	drive(b, 11, true)

	if got := a.State(); got != Open {
		t.Fatalf("实例 A 应打开: got %v", got)
	}
	if got := b.State(); got != Closed {
		t.Fatalf("实例 B 不应受 A 影响: got %v", got)
	}

	// A 打开期间拒绝请求，B 正常放行。
	if _, ok := a.Allow(); ok {
		t.Fatal("实例 A 打开态应拒绝")
	}
	if _, ok := b.Allow(); ok != true {
		t.Fatal("实例 B 应正常放行")
	}
}
