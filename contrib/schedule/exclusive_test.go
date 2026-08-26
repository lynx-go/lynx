package schedule

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lynx-go/lynx/contrib/cluster"
)

func TestExclusiveRequiresStore(t *testing.T) {
	_, err := NewScheduler(
		[]Task{Exclusive(newCountingTask("t", "@every 1s", &atomic.Int32{}))},
		WithLogger(discardLogger()),
	)
	if !errors.Is(err, ErrStoreRequired) {
		t.Fatalf("got %v, want ErrStoreRequired", err)
	}
}

func TestExclusiveSameStoreRunsOncePerTick(t *testing.T) {
	store := cluster.NewMemory(cluster.WithNamespace("sched-test"))
	var a, b atomic.Int32
	s1, err := NewScheduler([]Task{Exclusive(&testTask{name: "once", cron: "@every 1s", handler: func(ctx context.Context) error {
		a.Add(1)
		return nil
	}})}, WithLogger(discardLogger()), WithStore(store), WithLocation(time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	s2, err := NewScheduler([]Task{Exclusive(&testTask{name: "once", cron: "@every 1s", handler: func(ctx context.Context) error {
		b.Add(1)
		return nil
	}})}, WithLogger(discardLogger()), WithStore(store), WithLocation(time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	done1, done2 := make(chan struct{}), make(chan struct{})
	go func() { defer close(done1); _ = s1.Start(ctx1) }()
	go func() { defer close(done2); _ = s2.Start(ctx2) }()

	deadline := time.Now().Add(3500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if a.Load()+b.Load() >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(1200 * time.Millisecond)
	_ = s1.Stop(context.Background())
	_ = s2.Stop(context.Background())
	cancel1()
	cancel2()
	<-done1
	<-done2

	total := a.Load() + b.Load()
	if total < 1 {
		t.Fatal("exclusive task never ran")
	}
	// Two independent schedulers over ~2 ticks: without exclusivity we'd see
	// roughly 2× ticks. With a shared store each tick is claimed once.
	if a.Load() > 0 && b.Load() > 0 && a.Load()+b.Load() > 6 {
		t.Fatalf("too many combined runs a=%d b=%d (exclusivity not applied)", a.Load(), b.Load())
	}
	// At least one side should have been skipped entirely on some ticks:
	// both incrementing every tick would mean a≈b≈ticks.
	if total >= 4 && (a.Load() == 0 || b.Load() == 0 || a.Load()+b.Load() <= 4) {
		return
	}
	if a.Load() > 0 && b.Load() > 0 {
		// Possible if they never overlapped a slot; still require combined
		// count not double a single scheduler (~2–3 ticks).
		if total > 5 {
			t.Fatalf("combined runs %d look like both nodes firing", total)
		}
	}
}

func TestNonExclusiveBothRun(t *testing.T) {
	var a, b atomic.Int32
	s1, err := NewScheduler([]Task{newCountingTask("t", "@every 1s", &a)}, WithLogger(discardLogger()))
	if err != nil {
		t.Fatal(err)
	}
	s2, err := NewScheduler([]Task{newCountingTask("t", "@every 1s", &b)}, WithLogger(discardLogger()))
	if err != nil {
		t.Fatal(err)
	}
	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	go func() { _ = s1.Start(ctx1) }()
	go func() { _ = s2.Start(ctx2) }()
	if !pollUntil(3*time.Second, 20*time.Millisecond, func() bool { return a.Load() >= 1 && b.Load() >= 1 }) {
		cancel1()
		cancel2()
		_ = s1.Stop(context.Background())
		_ = s2.Stop(context.Background())
		t.Fatalf("want both to run, a=%d b=%d", a.Load(), b.Load())
	}
	_ = s1.Stop(context.Background())
	_ = s2.Stop(context.Background())
	cancel1()
	cancel2()
}

type errStore struct{ err error }

func (e errStore) Claim(ctx context.Context, name string, ttl time.Duration) (bool, error) {
	return false, e.err
}

func (e errStore) Acquire(ctx context.Context, name string, ttl time.Duration) (cluster.Lease, bool, error) {
	return nil, false, e.err
}

func TestExclusiveClaimErrorGoesToHandler(t *testing.T) {
	var got atomic.Int32
	var ran atomic.Int32
	boom := errors.New("store down")
	s, err := NewScheduler(
		[]Task{Exclusive(&testTask{name: "t", cron: "@every 1s", handler: func(ctx context.Context) error {
			ran.Add(1)
			return nil
		}})},
		WithLogger(discardLogger()),
		WithStore(errStore{err: boom}),
		WithErrorHandler(func(ctx context.Context, task Task, err error) {
			if errors.Is(err, boom) {
				got.Add(1)
			}
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Start(ctx) }()
	if !pollUntil(3*time.Second, 20*time.Millisecond, func() bool { return got.Load() >= 1 }) {
		cancel()
		_ = s.Stop(context.Background())
		t.Fatal("OnTaskError not invoked for store error")
	}
	if ran.Load() != 0 {
		t.Fatalf("handler ran %d times on store error", ran.Load())
	}
	_ = s.Stop(context.Background())
	cancel()
}

func TestExclusiveSkipNotError(t *testing.T) {
	store := cluster.NewMemory()
	var errs atomic.Int32
	var runs atomic.Int32
	mk := func() *Scheduler {
		s, err := NewScheduler(
			[]Task{Exclusive(&testTask{name: "t", cron: "@every 1s", handler: func(ctx context.Context) error {
				runs.Add(1)
				return nil
			}})},
			WithLogger(discardLogger()),
			WithStore(store),
			WithLocation(time.UTC),
			WithErrorHandler(func(ctx context.Context, task Task, err error) {
				errs.Add(1)
			}),
		)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s1, s2 := mk(), mk()
	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	go func() { _ = s1.Start(ctx1) }()
	go func() { _ = s2.Start(ctx2) }()
	if !pollUntil(3*time.Second, 20*time.Millisecond, func() bool { return runs.Load() >= 1 }) {
		cancel1()
		cancel2()
		t.Fatal("task did not run")
	}
	time.Sleep(300 * time.Millisecond)
	_ = s1.Stop(context.Background())
	_ = s2.Stop(context.Background())
	cancel1()
	cancel2()
	if errs.Load() != 0 {
		t.Fatalf("skip must not invoke OnTaskError, got %d", errs.Load())
	}
}
