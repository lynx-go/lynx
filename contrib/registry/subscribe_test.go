package registry

import (
	"context"
	"errors"
	"testing"
	"time"
)

// nextTimeout 是单次订阅 Next 的测试预算。
const nextTimeout = 2 * time.Second

// nextWithTimeout 以超时保护执行一次 Next（Next 阻塞语义的测试助手）。
func nextWithTimeout(t *testing.T, w Watcher) ([]Instance, error) {
	t.Helper()
	type result struct {
		insts []Instance
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		insts, err := w.Next()
		ch <- result{insts, err}
	}()
	select {
	case r := <-ch:
		return r.insts, r.err
	case <-time.After(nextTimeout):
		t.Fatal("Next() did not return within timeout")
		return nil, nil
	}
}

func inst(id string) Instance {
	return Instance{ID: id, Name: "svc", Status: StatusPassing, Endpoints: []Endpoint{{Protocol: "grpc", Address: id}}}
}

// TestSubscribeFirstNextImmediate：缓存已填充时首个 Next 立即返回当前
// 快照（预发信号）。
func TestSubscribeFirstNextImmediate(t *testing.T) {
	fd := &fakeDiscovery{snap: []Instance{inst("a")}}
	r := NewResolver(fd, WithPollInterval(50*time.Millisecond))
	defer func() { _ = r.Close() }()
	if _, err := r.Get(context.Background(), "svc", Filter{}); err != nil {
		t.Fatalf("Get: %v", err)
	}
	w, err := r.Subscribe("svc")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = w.Stop() }()
	insts, err := nextWithTimeout(t, w)
	if err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if len(insts) != 1 || insts[0].ID != "a" {
		t.Errorf("first Next = %v, want [a]", insts)
	}
}

// TestSubscribeReceivesUpdates：后端推送经缓存触发订阅通知；空快照
// （服务下线）同样推送。
func TestSubscribeReceivesUpdates(t *testing.T) {
	fd := &fakeDiscovery{snap: []Instance{inst("a")}}
	r := NewResolver(fd, WithPollInterval(50*time.Millisecond))
	defer func() { _ = r.Close() }()
	w, err := r.Subscribe("svc")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = w.Stop() }()
	if _, err := nextWithTimeout(t, w); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	fd.push([]Instance{inst("a"), inst("b")})
	insts, err := nextWithTimeout(t, w)
	if err != nil {
		t.Fatalf("second Next: %v", err)
	}
	if len(insts) != 2 {
		t.Errorf("Next after push = %d instances, want 2", len(insts))
	}
	fd.push(nil)
	insts, err = nextWithTimeout(t, w)
	if err != nil {
		t.Fatalf("Next after empty push: %v", err)
	}
	if len(insts) != 0 {
		t.Errorf("Next after empty push = %d instances, want 0 (offline)", len(insts))
	}
}

// TestSubscribeSignalCoalescing：连续两次变更只消费一次时，Next 拿到
// 最新快照（信号合并，不排队陈旧快照）。
func TestSubscribeSignalCoalescing(t *testing.T) {
	fd := &fakeDiscovery{snap: []Instance{inst("a")}}
	r := NewResolver(fd, WithPollInterval(50*time.Millisecond))
	defer func() { _ = r.Close() }()
	w, err := r.Subscribe("svc")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = w.Stop() }()
	if _, err := nextWithTimeout(t, w); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	fd.push([]Instance{inst("a"), inst("b")})
	fd.push([]Instance{inst("a"), inst("b"), inst("c")})
	insts, err := nextWithTimeout(t, w)
	if err != nil {
		t.Fatalf("Next after two pushes: %v", err)
	}
	if len(insts) != 3 {
		t.Errorf("coalesced Next = %d instances, want 3 (latest only)", len(insts))
	}
}

// TestSubscribeStop：Stop 幂等且之后 Next 返回 ErrWatcherStopped；
// Stop 后的变更不再产生通知（Next 保持错误语义）。
func TestSubscribeStop(t *testing.T) {
	fd := &fakeDiscovery{snap: []Instance{inst("a")}}
	r := NewResolver(fd, WithPollInterval(50*time.Millisecond))
	defer func() { _ = r.Close() }()
	w, err := r.Subscribe("svc")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if _, err := nextWithTimeout(t, w); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if err := w.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := w.Stop(); err != nil {
		t.Fatalf("second Stop: %v (want idempotent)", err)
	}
	fd.push([]Instance{inst("b")})
	if _, err := nextWithTimeout(t, w); !errors.Is(err, ErrWatcherStopped) {
		t.Fatalf("Next after Stop = %v, want ErrWatcherStopped", err)
	}
}

// TestSubscribeAfterResolverClose：Resolver 关闭后 Next 返回
// ErrResolverClosed。
func TestSubscribeAfterResolverClose(t *testing.T) {
	fd := &fakeDiscovery{snap: []Instance{inst("a")}}
	r := NewResolver(fd, WithPollInterval(50*time.Millisecond))
	w, err := r.Subscribe("svc")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := nextWithTimeout(t, w); !errors.Is(err, ErrResolverClosed) {
		t.Fatalf("Next after Close = %v, want ErrResolverClosed", err)
	}
}

// TestSubscribePollingBackend：后端 Watch 不可用（DNS 式）时缓存由
// 轮询维护，订阅同样收到通知——订阅者对后端形态无感。
func TestSubscribePollingBackend(t *testing.T) {
	fd := &fakeDiscovery{snap: []Instance{inst("a")}, watchErr: errWatchBroken}
	r := NewResolver(fd, WithPollInterval(50*time.Millisecond))
	defer func() { _ = r.Close() }()
	w, err := r.Subscribe("svc")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = w.Stop() }()
	if _, err := nextWithTimeout(t, w); err != nil {
		t.Fatalf("first Next (polling backend): %v", err)
	}
	fd.mu.Lock()
	fd.snap = []Instance{inst("a"), inst("b")}
	fd.mu.Unlock()
	insts, err := nextWithTimeout(t, w)
	if err != nil {
		t.Fatalf("Next after polling refresh: %v", err)
	}
	if len(insts) != 2 {
		t.Errorf("polled Next = %d instances, want 2", len(insts))
	}
}

func TestSubscribeBadNameAndClosed(t *testing.T) {
	r := NewResolver(&fakeDiscovery{})
	if _, err := r.Subscribe(""); !errors.Is(err, ErrBadName) {
		t.Errorf("Subscribe(\"\") = %v, want ErrBadName", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := r.Subscribe("svc"); !errors.Is(err, ErrResolverClosed) {
		t.Errorf("Subscribe after Close = %v, want ErrResolverClosed", err)
	}
}
