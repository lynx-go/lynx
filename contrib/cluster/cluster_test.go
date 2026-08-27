package cluster

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lynx-go/lynx"
)

func TestFormatKeyAndOwner(t *testing.T) {
	if got := FormatKey("job"); got != "lynx/job" {
		t.Fatalf("default key = %q", got)
	}
	if got := FormatKey("job", WithNamespace("app")); got != "app/job" {
		t.Fatalf("ns key = %q", got)
	}
	if got := Owner(WithInstance("n1")); got != "n1" {
		t.Fatalf("owner = %q", got)
	}
}

func TestMemoryClaimExclusive(t *testing.T) {
	s := NewMemory(WithNamespace("t"))
	won, err := s.Claim(context.Background(), "job", 200*time.Millisecond)
	if err != nil || !won {
		t.Fatalf("first claim: won=%v err=%v", won, err)
	}
	won, err = s.Claim(context.Background(), "job", 200*time.Millisecond)
	if err != nil {
		t.Fatalf("second claim err: %v", err)
	}
	if won {
		t.Fatal("second claim should not win")
	}
}

func TestMemoryClaimExpires(t *testing.T) {
	s := NewMemory()
	if _, err := s.Claim(context.Background(), "job", 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	won, err := s.Claim(context.Background(), "job", 50*time.Millisecond)
	if err != nil || !won {
		t.Fatalf("after expiry: won=%v err=%v", won, err)
	}
}

func TestMemoryClaimRejectsBadInput(t *testing.T) {
	s := NewMemory()
	if _, err := s.Claim(context.Background(), "", time.Second); !errors.Is(err, ErrEmptyName) {
		t.Fatalf("empty name: %v", err)
	}
	if _, err := s.Claim(context.Background(), "x", 0); !errors.Is(err, ErrInvalidTTL) {
		t.Fatalf("zero ttl: %v", err)
	}
}

func TestMemoryAcquireRelease(t *testing.T) {
	s := NewMemory()
	lease, ok, err := s.Acquire(context.Background(), "leader", 200*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	_, ok, err = s.Acquire(context.Background(), "leader", 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("second acquire should fail")
	}
	if err := lease.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lease.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("release did not cancel lease context")
	}
	_, ok, err = s.Acquire(context.Background(), "leader", 200*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("after release: ok=%v err=%v", ok, err)
	}
}

func TestMemoryAcquireRenews(t *testing.T) {
	s := NewMemory()
	lease, ok, err := s.Acquire(context.Background(), "leader", 150*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	defer func() { _ = lease.Release(context.Background()) }()
	time.Sleep(250 * time.Millisecond)
	_, ok, err = s.Acquire(context.Background(), "leader", 150*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("lease should still be held after ttl due to renew")
	}
}

func TestTryOnceSkipAndRun(t *testing.T) {
	s := NewMemory()
	var n atomic.Int32
	fn := func(ctx context.Context) error {
		n.Add(1)
		return nil
	}
	skipped, err := TryOnce(context.Background(), s, "once", 200*time.Millisecond, fn)
	if err != nil || skipped {
		t.Fatalf("first: skipped=%v err=%v", skipped, err)
	}
	skipped, err = TryOnce(context.Background(), s, "once", 200*time.Millisecond, fn)
	if err != nil {
		t.Fatal(err)
	}
	if !skipped {
		t.Fatal("second TryOnce should skip")
	}
	if n.Load() != 1 {
		t.Fatalf("fn calls = %d, want 1", n.Load())
	}
}

func TestTryOnceNilCoordinator(t *testing.T) {
	_, err := TryOnce(context.Background(), nil, "x", time.Second, func(ctx context.Context) error { return nil })
	if !errors.Is(err, ErrNilCoordinator) {
		t.Fatalf("got %v", err)
	}
}

func TestCampaignResign(t *testing.T) {
	s := NewMemory()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	lead, err := CampaignTTL(ctx, s, "role", 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan struct{})
	go func() {
		_, err := CampaignTTL(ctx, s, "role", 200*time.Millisecond)
		if err != nil {
			return
		}
		close(got)
	}()
	select {
	case <-got:
		t.Fatal("second campaign won before resign")
	case <-time.After(80 * time.Millisecond):
	}
	if err := lead.Resign(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("second campaign did not win after resign")
	}
}

type recService struct {
	starts atomic.Int32
	started chan struct{}
}

func (r *recService) Name() string { return "inner" }
func (r *recService) Init(lynx.AppContext) error { return nil }
func (r *recService) Start(ctx context.Context) error {
	if r.starts.Add(1) == 1 {
		close(r.started)
	}
	<-ctx.Done()
	return nil
}
func (r *recService) Stop(context.Context) error { return nil }

func TestSingletonOnlyOneStarts(t *testing.T) {
	coord := NewMemory()
	a := &recService{started: make(chan struct{})}
	b := &recService{started: make(chan struct{})}
	sa := Singleton("worker", a, coord)
	sb := Singleton("worker", b, coord)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sa.Start(ctx) }()
	go func() { _ = sb.Start(ctx) }()

	var first *recService
	select {
	case <-a.started:
		first = a
	case <-b.started:
		first = b
	case <-time.After(2 * time.Second):
		t.Fatal("no leader started")
	}
	time.Sleep(80 * time.Millisecond)
	other := b
	if first == b {
		other = a
	}
	if other.starts.Load() != 0 {
		t.Fatalf("follower inner.Start called %d times", other.starts.Load())
	}
	if err := sa.(lynx.Checker).CheckHealth(); err != nil {
		t.Fatalf("leader/follower CheckHealth: %v", err)
	}
	if err := sb.(lynx.Checker).CheckHealth(); err != nil {
		t.Fatalf("CheckHealth: %v", err)
	}

	_ = sa.Stop(context.Background())
	_ = sb.Stop(context.Background())
	cancel()
}

func TestSingletonInitRequiresCoordinator(t *testing.T) {
	s := Singleton("x", &recService{started: make(chan struct{})}, nil)
	if err := s.Init(nil); !errors.Is(err, ErrNilCoordinator) {
		t.Fatalf("got %v", err)
	}
}
