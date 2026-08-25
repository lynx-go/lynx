package lynx

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type orderLog struct {
	mu   sync.Mutex
	evts []string
}

func (l *orderLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.evts = append(l.evts, e)
}

func (l *orderLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.evts))
	copy(out, l.evts)
	return out
}

// readyProbe 阻塞式服务：Start 进入后延迟关闭 Ready，再等 ctx。
type readyProbe struct {
	name       string
	log        *orderLog
	ready      chan struct{}
	readyDelay time.Duration
	startErr   error
}

func newReadyProbe(name string, log *orderLog, delay time.Duration) *readyProbe {
	return &readyProbe{name: name, log: log, ready: make(chan struct{}), readyDelay: delay}
}

func (p *readyProbe) Name() string                   { return p.name }
func (p *readyProbe) Init(ctx AppContext) error      { return nil }
func (p *readyProbe) Ready() <-chan struct{}         { return p.ready }
func (p *readyProbe) Stop(ctx context.Context) error { return nil }
func (p *readyProbe) Start(ctx context.Context) error {
	p.log.add("start:" + p.name)
	if p.startErr != nil {
		return p.startErr
	}
	if p.readyDelay > 0 {
		time.Sleep(p.readyDelay)
	}
	close(p.ready)
	p.log.add("ready:" + p.name)
	<-ctx.Done()
	return nil
}

// healthProbe 无 Ready，靠 CheckHealth。Start 后延迟置健康。
type healthProbe struct {
	name        string
	log         *orderLog
	healthy     atomic.Bool
	healthDelay time.Duration
}

func (p *healthProbe) Name() string              { return p.name }
func (p *healthProbe) Init(ctx AppContext) error { return nil }
func (p *healthProbe) CheckHealth() error {
	if !p.healthy.Load() {
		return errors.New("not healthy")
	}
	return nil
}
func (p *healthProbe) Stop(ctx context.Context) error { return nil }
func (p *healthProbe) Start(ctx context.Context) error {
	p.log.add("start:" + p.name)
	if p.healthDelay > 0 {
		select {
		case <-time.After(p.healthDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	p.healthy.Store(true)
	p.log.add("healthy:" + p.name)
	<-ctx.Done()
	return nil
}

type bothProbe struct {
	readyProbe
	healthy atomic.Bool
}

func (p *bothProbe) CheckHealth() error {
	if !p.healthy.Load() {
		return errors.New("not healthy")
	}
	return nil
}

type seqProbe struct {
	name     string
	log      *orderLog
	initErr  error
	startErr error
	stopErr  error
}

func (p *seqProbe) Name() string { return p.name }
func (p *seqProbe) Init(ctx AppContext) error {
	p.log.add("init:" + p.name)
	return p.initErr
}
func (p *seqProbe) Start(ctx context.Context) error {
	p.log.add("start:" + p.name)
	if p.startErr != nil {
		return p.startErr
	}
	<-ctx.Done()
	return nil
}
func (p *seqProbe) Stop(ctx context.Context) error {
	p.log.add("stop:" + p.name)
	return p.stopErr
}

type oneShotProbe struct {
	name  string
	log   *orderLog
	ready chan struct{}
}

func (p *oneShotProbe) Name() string              { return p.name }
func (p *oneShotProbe) Init(ctx AppContext) error { return nil }
func (p *oneShotProbe) Ready() <-chan struct{}    { return p.ready }
func (p *oneShotProbe) Stop(ctx context.Context) error {
	p.log.add("stop:" + p.name)
	return nil
}
func (p *oneShotProbe) Start(ctx context.Context) error {
	p.log.add("start:" + p.name)
	close(p.ready)
	p.log.add("ready:" + p.name)
	return nil
}

func testAppCtx(t *testing.T) AppContext {
	t.Helper()
	app, err := newLynx(NewOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return app
}

func TestOrderedServicesName(t *testing.T) {
	s := OrderedServices("infra", &seqProbe{name: "a", log: &orderLog{}})
	if got := s.Name(); got != "infra" {
		t.Fatalf("Name() = %q, want infra", got)
	}
}

func TestOrderedServicesInitOrder(t *testing.T) {
	log := &orderLog{}
	g := OrderedServices("g",
		&seqProbe{name: "a", log: log},
		&seqProbe{name: "b", log: log},
	)
	if err := g.Init(testAppCtx(t)); err != nil {
		t.Fatal(err)
	}
	if got, want := log.all(), []string{"init:a", "init:b"}; !equalStrs(got, want) {
		t.Fatalf("init order = %v, want %v", got, want)
	}
}

func TestOrderedServicesInitFailureStopsPrevious(t *testing.T) {
	log := &orderLog{}
	want := errors.New("init-b")
	g := OrderedServices("g",
		&seqProbe{name: "a", log: log},
		&seqProbe{name: "b", log: log, initErr: want},
		&seqProbe{name: "c", log: log},
	)
	if err := g.Init(testAppCtx(t)); !errors.Is(err, want) {
		t.Fatalf("Init() = %v, want %v", err, want)
	}
	if got, wantEv := log.all(), []string{"init:a", "init:b", "stop:a"}; !equalStrs(got, wantEv) {
		t.Fatalf("events = %v, want %v", got, wantEv)
	}
}

func TestOrderedServicesRejectsEmptyName(t *testing.T) {
	g := OrderedServices("", &seqProbe{name: "a", log: &orderLog{}})
	if err := g.Init(testAppCtx(t)); err == nil {
		t.Fatal("Init() with empty name = nil, want error")
	}
}

func TestOrderedServicesRejectsEmptyList(t *testing.T) {
	g := OrderedServices("g")
	if err := g.Init(testAppCtx(t)); err == nil {
		t.Fatal("Init() with no services = nil, want error")
	}
}

func TestOrderedServicesRejectsNilService(t *testing.T) {
	g := OrderedServices("g", &seqProbe{name: "a", log: &orderLog{}}, nil)
	if err := g.Init(testAppCtx(t)); err == nil {
		t.Fatal("Init() with nil service = nil, want error")
	}
}

func TestOrderedServicesStartWaitsForReady(t *testing.T) {
	log := &orderLog{}
	a := newReadyProbe("a", log, 40*time.Millisecond)
	b := newReadyProbe("b", log, 0)
	g := OrderedServices("g", a, b)
	if err := g.Init(testAppCtx(t)); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- g.Start(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if containsStr(log.all(), "start:b") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	ev := log.all()
	if idxA, idxB := indexOf(ev, "ready:a"), indexOf(ev, "start:b"); idxA < 0 || idxB < 0 || idxA > idxB {
		t.Fatalf("events = %v, want ready:a before start:b", ev)
	}

	_ = g.Stop(context.Background())
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return")
	}
}

func TestOrderedServicesStartWaitsForChecker(t *testing.T) {
	log := &orderLog{}
	a := &healthProbe{name: "a", log: log, healthDelay: 40 * time.Millisecond}
	b := newReadyProbe("b", log, 0)
	g := OrderedServices("g", a, b)
	if err := g.Init(testAppCtx(t)); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- g.Start(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if containsStr(log.all(), "start:b") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	ev := log.all()
	if idxA, idxB := indexOf(ev, "healthy:a"), indexOf(ev, "start:b"); idxA < 0 || idxB < 0 || idxA > idxB {
		t.Fatalf("events = %v, want healthy:a before start:b", ev)
	}

	_ = g.Stop(context.Background())
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return")
	}
}

func TestOrderedServicesReadyPreferredOverChecker(t *testing.T) {
	log := &orderLog{}
	a := &bothProbe{readyProbe: *newReadyProbe("a", log, 0)}
	// CheckHealth 一直失败：若走 Checker 回退，下一个永远不会启动。
	b := newReadyProbe("b", log, 0)
	g := OrderedServices("g", a, b)
	if err := g.Init(testAppCtx(t)); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- g.Start(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if containsStr(log.all(), "start:b") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !containsStr(log.all(), "start:b") {
		t.Fatalf("events = %v, want start:b (Ready should win over failing Checker)", log.all())
	}

	_ = g.Stop(context.Background())
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return")
	}
}

func TestOrderedServicesStartErrorStopsSequence(t *testing.T) {
	log := &orderLog{}
	want := errors.New("start-a")
	a := newReadyProbe("a", log, 0)
	a.startErr = want
	b := newReadyProbe("b", log, 0)
	g := OrderedServices("g", a, b)
	if err := g.Init(testAppCtx(t)); err != nil {
		t.Fatal(err)
	}

	err := g.Start(context.Background())
	if !errors.Is(err, want) {
		t.Fatalf("Start() = %v, want %v", err, want)
	}
	if containsStr(log.all(), "start:b") {
		t.Fatalf("events = %v, b should not start after a failed", log.all())
	}
}

func TestOrderedServicesStopReverseOrder(t *testing.T) {
	log := &orderLog{}
	g := OrderedServices("g",
		&seqProbe{name: "a", log: log},
		&seqProbe{name: "b", log: log},
	)
	if err := g.Init(testAppCtx(t)); err != nil {
		t.Fatal(err)
	}
	if err := g.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := log.all(), []string{"init:a", "init:b", "stop:b", "stop:a"}; !equalStrs(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestOrderedServicesStopBeforeStart(t *testing.T) {
	g := OrderedServices("g", &seqProbe{name: "a", log: &orderLog{}})
	if err := g.Init(testAppCtx(t)); err != nil {
		t.Fatal(err)
	}
	if err := g.Stop(context.Background()); err != nil {
		t.Fatalf("Stop before Start: %v", err)
	}
}

func TestOrderedServicesCheckHealthAggregates(t *testing.T) {
	log := &orderLog{}
	a := &healthProbe{name: "a", log: log}
	b := &seqProbe{name: "b", log: log}
	g := OrderedServices("g", a, b)
	if err := g.Init(testAppCtx(t)); err != nil {
		t.Fatal(err)
	}
	if err := g.(Checker).CheckHealth(); err == nil {
		t.Fatal("CheckHealth() before children healthy = nil, want error")
	}
	a.healthy.Store(true)
	if err := g.(Checker).CheckHealth(); err != nil {
		t.Fatalf("CheckHealth() after a healthy = %v, want nil", err)
	}
}

func TestOrderedServicesCheckerTimeout(t *testing.T) {
	log := &orderLog{}
	a := &healthProbe{name: "a", log: log, healthDelay: time.Hour}
	g := OrderedServices("g", a).(*orderedServices)
	g.readyTimeout = 30 * time.Millisecond
	if err := g.Init(testAppCtx(t)); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := g.Start(ctx)
	if err == nil {
		t.Fatal("Start() = nil, want health wait timeout")
	}
	cancel()
	_ = g.Stop(context.Background())
}

func TestOrderedServicesOneShotThenNext(t *testing.T) {
	log := &orderLog{}
	a := &oneShotProbe{name: "a", log: log, ready: make(chan struct{})}
	b := newReadyProbe("b", log, 0)
	g := OrderedServices("g", a, b)
	if err := g.Init(testAppCtx(t)); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- g.Start(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if containsStr(log.all(), "start:b") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	ev := log.all()
	if idxA, idxB := indexOf(ev, "ready:a"), indexOf(ev, "start:b"); idxA < 0 || idxB < 0 || idxA > idxB {
		t.Fatalf("events = %v, want ready:a before start:b", ev)
	}

	_ = g.Stop(context.Background())
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return")
	}
}

func TestOrderedServicesNested(t *testing.T) {
	log := &orderLog{}
	inner := OrderedServices("inner",
		newReadyProbe("a", log, 0),
		newReadyProbe("b", log, 0),
	)
	outer := OrderedServices("outer", inner, newReadyProbe("c", log, 0))
	if err := outer.Init(testAppCtx(t)); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- outer.Start(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if containsStr(log.all(), "start:c") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	ev := log.all()
	if idxB, idxC := indexOf(ev, "ready:b"), indexOf(ev, "start:c"); idxB < 0 || idxC < 0 || idxB > idxC {
		t.Fatalf("events = %v, want ready:b before start:c", ev)
	}

	_ = outer.Stop(context.Background())
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return")
	}
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsStr(ss []string, want string) bool {
	return indexOf(ss, want) >= 0
}

func indexOf(ss []string, want string) int {
	for i, s := range ss {
		if s == want {
			return i
		}
	}
	return -1
}
