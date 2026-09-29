package smarthome

import "time"

// Duration 用毫秒表示，既方便真实时钟，也方便测试用的虚拟时钟。
type Duration int64

const (
	Millisecond Duration = 1
	Second      Duration = 1000
	Minute      Duration = 60 * Second
)

// Clock 抽象时间来源。测试用可控的 FakeClock，生产用 WallClock。
type Clock interface {
	Now() Duration
}

// WallClock 从引擎启动时刻起计时（毫秒）。
type WallClock struct {
	start time.Time
}

func NewWallClock() *WallClock {
	return &WallClock{start: time.Now()}
}

func (c *WallClock) Now() Duration {
	return Duration(time.Since(c.start) / time.Millisecond)
}

// FakeClock 由测试手动推进。
type FakeClock struct {
	t Duration
}

func NewFakeClock() *FakeClock { return &FakeClock{} }

func (c *FakeClock) Now() Duration { return c.t }

// Advance 推进虚拟时间，返回推进后的时刻。
func (c *FakeClock) Advance(d Duration) Duration {
	c.t += d
	return c.t
}
