// Package breaker 实现服务网格场景下的熔断器状态机：
// 关闭（Closed）→ 打开（Open）→ 半开（HalfOpen）→ 关闭 的三态流转，
// 基于滑动时间窗口的错误率统计与半开态探测限流。
//
// 状态转换规则：
//
//	Closed   → Open     窗口内请求数 >= MinRequests 且错误率 >= ErrorThreshold
//	Open     → HalfOpen 当前时间 >= openedAt + Cooldown（冷却期满，含边界时刻）
//	HalfOpen → Open     任一探测请求失败（冷却计时从此时刻重新起算）
//	HalfOpen → Closed   连续 SuccessThreshold 个探测成功
//
// 半开态最多同时放行 MaxProbes 个探测请求，超出配额的请求直接拒绝，
// 避免积压请求同时涌入下游。
package breaker

import (
	"sync"
	"time"
)

// State 是熔断器状态。
type State int

const (
	Closed State = iota
	Open
	HalfOpen
)

func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case Open:
		return "open"
	case HalfOpen:
		return "half-open"
	}
	return "unknown"
}

// Config 是熔断器配置。
type Config struct {
	// Window 是滑动时间窗口长度。
	Window time.Duration
	// Buckets 是窗口的分桶数，越大统计越平滑。
	Buckets int
	// MinRequests 是触发熔断所需的最小样本数：
	// 窗口内请求总数低于该值时，即使错误率超阈值也不打开，
	// 避免低流量下单次失败误触发。
	MinRequests int
	// ErrorThreshold 是触发打开的错误率阈值，取值 (0,1]。
	ErrorThreshold float64
	// Cooldown 是打开态的冷却时间，期满后进入半开态。
	Cooldown time.Duration
	// MaxProbes 是半开态允许同时在飞的探测请求数上限。
	MaxProbes int
	// SuccessThreshold 是半开态关闭熔断器所需的连续探测成功次数。
	SuccessThreshold int
}

// DefaultConfig 返回一组合理默认配置。
func DefaultConfig() Config {
	return Config{
		Window:           10 * time.Second,
		Buckets:          20,
		MinRequests:      10,
		ErrorThreshold:   0.5,
		Cooldown:         5 * time.Second,
		MaxProbes:        3,
		SuccessThreshold: 3,
	}
}

// Permit 是一次放行许可。Report 凭 Permit 上报结果，
// 其中携带的代际号用于丢弃状态切换后迟到的过期上报。
type Permit struct {
	gen uint64
}

// CircuitBreaker 是单个熔断器实例。实例间不共享任何状态，
// 多实例各自独立统计、独立流转。并发安全。
type CircuitBreaker struct {
	cfg Config
	now Clock

	mu            sync.Mutex
	state         State
	openedAt      time.Time
	win           *slidingWindow
	gen           uint64 // 状态代际，每次状态切换递增
	inFlight      int    // 半开态当前在飞探测数
	consecSuccess int    // 半开态连续探测成功数
}

func New(cfg Config, clock Clock) *CircuitBreaker {
	if clock == nil {
		clock = RealClock
	}
	return &CircuitBreaker{
		cfg:   cfg,
		now:   clock,
		state: Closed,
		win:   newSlidingWindow(cfg.Window, cfg.Buckets),
	}
}

// effectiveStateLocked 返回 now 时刻的有效状态。
// 打开态冷却期满（含边界：now >= openedAt+Cooldown）即视为半开，
// 判定是 (now, openedAt, Cooldown) 的纯函数，同一时刻重复判定结果一致。
func (cb *CircuitBreaker) effectiveStateLocked(now time.Time) State {
	if cb.state == Open && !now.Before(cb.openedAt.Add(cb.cfg.Cooldown)) {
		return HalfOpen
	}
	return cb.state
}

// Allow 请求一次调用许可。放行时返回 (Permit, true)，
// 调用方必须在调用完成后用 Report 上报结果；
// 拒绝时返回 (Permit{}, false)，调用方不应发起下游调用。
func (cb *CircuitBreaker) Allow() (Permit, bool) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	now := cb.now()
	switch cb.effectiveStateLocked(now) {
	case Closed:
		return Permit{gen: cb.gen}, true
	case Open:
		return Permit{}, false
	default: // HalfOpen：持久化 Open→HalfOpen 跃迁，并占用一个探测配额
		if cb.state == Open {
			cb.enterHalfOpenLocked()
		}
		if cb.inFlight >= cb.cfg.MaxProbes {
			return Permit{}, false
		}
		cb.inFlight++
		return Permit{gen: cb.gen}, true
	}
}

// Report 上报一次被放行调用的结果。
func (cb *CircuitBreaker) Report(p Permit, success bool) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if p.gen != cb.gen {
		return // 过期许可：状态已切换，结果不再计入，避免跨代际污染统计
	}
	now := cb.now()
	switch cb.state {
	case Closed:
		cb.win.add(now, !success)
		total, failures := cb.win.counts(now)
		if total >= cb.cfg.MinRequests &&
			float64(failures)/float64(total) >= cb.cfg.ErrorThreshold {
			cb.enterOpenLocked(now)
		}
	case HalfOpen:
		cb.inFlight--
		if !success {
			// 任一探测失败立即回到打开态，冷却计时从此刻重新起算
			cb.enterOpenLocked(now)
			return
		}
		cb.consecSuccess++
		if cb.consecSuccess >= cb.cfg.SuccessThreshold {
			cb.enterClosedLocked()
		}
	}
}

// Do 是 Allow + Report 的便捷封装：放行则执行 fn 并按其 error 上报，
// 拒绝则返回 ErrOpen。
func (cb *CircuitBreaker) Do(fn func() error) error {
	p, ok := cb.Allow()
	if !ok {
		return ErrOpen
	}
	err := fn()
	cb.Report(p, err == nil)
	return err
}

// ErrOpen 表示请求被熔断器拒绝。
var ErrOpen = errOpen{}

type errOpen struct{}

func (errOpen) Error() string { return "circuit breaker is open" }

func (cb *CircuitBreaker) enterOpenLocked(now time.Time) {
	cb.state = Open
	cb.openedAt = now
	cb.inFlight = 0
	cb.consecSuccess = 0
	cb.gen++
}

func (cb *CircuitBreaker) enterHalfOpenLocked() {
	cb.state = HalfOpen
	cb.inFlight = 0
	cb.consecSuccess = 0
	cb.gen++
}

func (cb *CircuitBreaker) enterClosedLocked() {
	cb.state = Closed
	cb.win.reset()
	cb.inFlight = 0
	cb.consecSuccess = 0
	cb.gen++
}

// Snapshot 是熔断器某一时刻的可观测状态，用于测试与回放。
type Snapshot struct {
	State         State
	WindowTotal   int
	WindowFailure int
	ErrorRate     float64
	InFlight      int
	ConsecSuccess int
	OpenedAt      time.Time
}

// Snapshot 返回当前时刻的可观测状态。State 为有效状态
// （冷却期满的打开态在此即呈现为半开，与 Allow 的判定一致）。
func (cb *CircuitBreaker) Snapshot() Snapshot {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	now := cb.now()
	total, failures := cb.win.counts(now)
	var rate float64
	if total > 0 {
		rate = float64(failures) / float64(total)
	}
	return Snapshot{
		State:         cb.effectiveStateLocked(now),
		WindowTotal:   total,
		WindowFailure: failures,
		ErrorRate:     rate,
		InFlight:      cb.inFlight,
		ConsecSuccess: cb.consecSuccess,
		OpenedAt:      cb.openedAt,
	}
}
