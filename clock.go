package breaker

import (
	"sync"
	"time"
)

// Clock 返回当前时间。注入时钟使状态机判定可确定性复现，
// 尤其是"冷却时间边界时刻"的判定。
type Clock func() time.Time

// RealClock 使用系统时间。
func RealClock() time.Time { return time.Now() }

// ManualClock 是手动推进的时钟，用于测试与回放模拟。并发安全。
type ManualClock struct {
	mu sync.Mutex
	t  time.Time
}

func NewManualClock(start time.Time) *ManualClock { return &ManualClock{t: start} }

func (c *ManualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *ManualClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func (c *ManualClock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	return c.t
}
