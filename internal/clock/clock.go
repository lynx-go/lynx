// Package clock 提供 lynx.Clock 的生产实现与测试用可控时钟。
package clock

import (
	"sort"
	"sync"
	"time"

	"github.com/lynx-go/lynx"
)

// Real 返回真实时间源（lynx.Clock 的生产默认）。
func Real() lynx.Clock { return realClock{} }

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Fake 是可控时钟：Now 返回虚拟时间；After 注册定时器；Advance 推进虚拟
// 时间并按 deadline 升序触发到期定时器。并发安全。
type Fake struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	deadline time.Time
	ch       chan time.Time
}

// NewFake 创建从 start 开始的虚拟时钟。
func NewFake(start time.Time) *Fake { return &Fake{now: start} }

// Now 返回当前虚拟时间。
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// After 注册一个 d 后到期的定时器；d <= 0 立即就绪。
func (f *Fake) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if d <= 0 {
		ch <- f.now
		return ch
	}
	f.timers = append(f.timers, &fakeTimer{deadline: f.now.Add(d), ch: ch})
	return ch
}

// Advance 推进虚拟时间 d，触发所有 deadline <= 新时间的定时器（按 deadline
// 升序；同一时刻到期的全部触发），返回推进后的时间。未到期的定时器保留。
func (f *Fake) Advance(d time.Duration) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	sort.Slice(f.timers, func(i, j int) bool { return f.timers[i].deadline.Before(f.timers[j].deadline) })
	var remaining []*fakeTimer
	for _, t := range f.timers {
		if !t.deadline.After(f.now) {
			t.ch <- f.now
			continue
		}
		remaining = append(remaining, t)
	}
	f.timers = remaining
	return f.now
}

// TimerCount 返回尚未触发（未到期）的定时器数：测试用它等待 loop 注册
// 定时器后再推进虚拟时间（消除 goroutine 调度竞态）。
func (f *Fake) TimerCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.timers)
}
