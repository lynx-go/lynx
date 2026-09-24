package clock

import (
	"testing"
	"time"
)

// TestFakeAfterAndAdvance：未到期不触发；推进到恰好 deadline 触发一次；
// 多个定时器按 deadline 升序触发。
func TestFakeAfterAndAdvance(t *testing.T) {
	start := time.Unix(1000, 0)
	f := NewFake(start)

	early := f.After(10 * time.Second)
	late := f.After(20 * time.Second)
	if got := f.TimerCount(); got != 2 {
		t.Fatalf("TimerCount = %d, want 2", got)
	}

	f.Advance(10*time.Second - time.Nanosecond)
	select {
	case <-early:
		t.Fatal("timer fired before deadline")
	default:
	}

	now := f.Advance(time.Nanosecond)
	select {
	case got := <-early:
		if !got.Equal(now) {
			t.Fatalf("fired at %v, want %v", got, now)
		}
	default:
		t.Fatal("timer did not fire at exactly its deadline")
	}
	select {
	case <-late:
		t.Fatal("later timer fired too early")
	default:
	}

	f.Advance(10 * time.Second)
	select {
	case <-late:
	default:
		t.Fatal("later timer did not fire")
	}
	if got := f.TimerCount(); got != 0 {
		t.Fatalf("TimerCount = %d, want 0 after all fired", got)
	}
}

// TestFakeAfterNonPositive：d <= 0 立即就绪。
func TestFakeAfterNonPositive(t *testing.T) {
	f := NewFake(time.Unix(0, 0))
	select {
	case <-f.After(0):
	default:
		t.Fatal("After(0) must be immediately ready")
	}
	select {
	case <-f.After(-time.Second):
	default:
		t.Fatal("After(negative) must be immediately ready")
	}
}

// TestFakeNow：Now 随 Advance 推进。
func TestFakeNow(t *testing.T) {
	start := time.Unix(1000, 0)
	f := NewFake(start)
	if got := f.Now(); !got.Equal(start) {
		t.Fatalf("Now = %v, want %v", got, start)
	}
	f.Advance(5 * time.Second)
	if got := f.Now(); !got.Equal(start.Add(5 * time.Second)) {
		t.Fatalf("Now = %v, want %v", got, start.Add(5*time.Second))
	}
}

// TestReal：生产实现回退真实时间（粗粒度断言）。
func TestReal(t *testing.T) {
	r := Real()
	before := time.Now()
	now := r.Now()
	after := time.Now()
	if now.Before(before) || now.After(after) {
		t.Fatalf("Real().Now() = %v, want between %v and %v", now, before, after)
	}
	select {
	case <-r.After(time.Millisecond):
	case <-time.After(time.Second):
		t.Fatal("Real().After did not fire")
	}
}
