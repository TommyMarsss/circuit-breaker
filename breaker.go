// Package breaker 实现服务网格场景下的熔断器状态机：
// 关闭（Closed）/ 打开（Open）/ 半开（HalfOpen）三态，
// 基于滑动时间窗口的错误率统计与半开态探测限流。
package breaker

import (
	"sync"
	"time"
)

// State 熔断器状态。
type State int

const (
	// Closed 关闭态：请求正常放行，按滑动窗口统计错误率。
	Closed State = iota
	// Open 打开态：请求直接拒绝（快速失败），冷却期结束后进入半开态。
	Open
	// HalfOpen 半开态：只放行有限数量的探测请求。
	HalfOpen
)

func (s State) String() string {
	switch s {
	case Closed:
		return "CLOSED"
	case Open:
		return "OPEN"
	case HalfOpen:
		return "HALF-OPEN"
	}
	return "UNKNOWN"
}

// Config 熔断器配置。
type Config struct {
	// WindowSize 滑动窗口总时长。
	WindowSize time.Duration
	// WindowBuckets 窗口分桶数（分桶越细，淘汰粒度越细）。
	WindowBuckets int
	// ErrorThreshold 错误率阈值，窗口错误率严格大于该值才触发打开。
	ErrorThreshold float64
	// MinSamples 最小样本数：窗口内请求数低于该值时不触发打开，
	// 避免低流量下单次失败误触发。
	MinSamples int
	// Cooldown 打开态冷却时长，冷却结束后进入半开态。
	Cooldown time.Duration
	// HalfOpenMaxProbes 半开态允许同时在途的最大探测请求数。
	HalfOpenMaxProbes int
	// HalfOpenSuccessThreshold 半开态连续成功达到该值后关闭熔断器。
	HalfOpenSuccessThreshold int
}

// Permit 是一次放行许可。调用方必须在调用结束后用同一个 Permit
// 调用 Report 汇报结果。Permit 携带半开态代际（generation），
// 用于识别并丢弃已过期半开周期里迟到的探测结果。
type Permit struct {
	probe bool // 是否为半开态探测请求
	gen   int  // 放行时所处的半开态代际
}

// Breaker 熔断器。所有方法并发安全。
type Breaker struct {
	mu  sync.Mutex
	cfg Config
	now func() time.Time

	state    State
	win      *Window
	openedAt time.Time // 最近一次进入打开态的时刻

	gen       int // 半开态代际，每次进入半开态递增
	inFlight  int // 半开态当前在途探测数
	successes int // 半开态当前连续成功数
}

// NewBreaker 创建熔断器。now 为时钟函数（测试可注入假时钟），
// 传 nil 则使用真实时钟。
func NewBreaker(cfg Config, now func() time.Time) *Breaker {
	if now == nil {
		now = time.Now
	}
	return &Breaker{
		cfg:   cfg,
		now:   now,
		state: Closed,
		win:   NewWindow(cfg.WindowSize, cfg.WindowBuckets),
	}
}

// Allow 请求一次放行许可。返回 ok=false 表示被熔断拒绝（快速失败）。
//
// 边界规则：打开态在 now >= openedAt+Cooldown 时进入半开态，
// 即恰好处于冷却边界的请求按半开态处理。该判定只依赖一次
// 时间比较，同一时刻无论查询多少次结果都一致。
func (b *Breaker) Allow() (p Permit, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case Closed:
		return Permit{gen: b.gen}, true
	case Open:
		if b.now().Before(b.openedAt.Add(b.cfg.Cooldown)) {
			return Permit{}, false
		}
		b.enterHalfOpenLocked()
	}
	// 半开态：受探测配额限制，超额的请求拒绝，
	// 防止积压请求同时涌入下游。
	if b.inFlight < b.cfg.HalfOpenMaxProbes {
		b.inFlight++
		return Permit{probe: true, gen: b.gen}, true
	}
	return Permit{}, false
}

// Report 汇报一次已放行调用的结果。
func (b *Breaker) Report(p Permit, success bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()

	if p.probe {
		// 迟到或跨代际的探测结果直接丢弃：
		// 熔断器可能已因其他探测失败而重新打开。
		if b.state != HalfOpen || p.gen != b.gen {
			return
		}
		b.inFlight--
		if success {
			b.successes++
			if b.successes >= b.cfg.HalfOpenSuccessThreshold {
				b.state = Closed
				b.win.Reset() // 关闭后重新统计，避免历史失败立刻再次触发
			}
		} else {
			// 任一探测失败立即回到打开态，并重置冷却计时。
			b.enterOpenLocked(now)
		}
		return
	}

	// 关闭态请求的结果：计入滑动窗口并评估是否打开。
	b.win.Add(now, !success)
	if b.state == Closed {
		total, fail := b.win.Counts(now)
		if total >= b.cfg.MinSamples &&
			float64(fail)/float64(total) > b.cfg.ErrorThreshold {
			b.enterOpenLocked(now)
		}
	}
}

// enterOpenLocked 进入打开态（调用方须持锁）。
func (b *Breaker) enterOpenLocked(now time.Time) {
	b.state = Open
	b.openedAt = now
	b.inFlight = 0
	b.successes = 0
}

// enterHalfOpenLocked 进入半开态（调用方须持锁）。
func (b *Breaker) enterHalfOpenLocked() {
	b.state = HalfOpen
	b.gen++
	b.inFlight = 0
	b.successes = 0
}

// State 返回当前状态（不改变状态机，打开态到期的迁移在 Allow 时发生）。
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// StateAt 返回 t 时刻的对外可见状态，是纯判定函数：
// 打开态且冷却已到期（含恰好在边界上）时对外表现为半开态。
// 同一 t 多次调用结果必然一致，边界前后各一侧判定也一致。
func (b *Breaker) StateAt(t time.Time) State {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == Open && !t.Before(b.openedAt.Add(b.cfg.Cooldown)) {
		return HalfOpen
	}
	return b.state
}

// Stats 返回 now 时刻的窗口统计快照，供监控与仿真使用。
// fullRate 为全量重算的错误率，用于校验增量统计的正确性。
func (b *Breaker) Stats(now time.Time) (total, fail int, rate, fullRate float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	total, fail = b.win.Counts(now)
	ftotal, ffail := b.win.FullCounts(now)
	rate = rateOf(total, fail)
	fullRate = rateOf(ftotal, ffail)
	return
}

// InFlight 返回半开态当前在途探测数（监控用）。
func (b *Breaker) InFlight() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inFlight
}

func rateOf(total, fail int) float64 {
	if total == 0 {
		return 0
	}
	return float64(fail) / float64(total)
}
