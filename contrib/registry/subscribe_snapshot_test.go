package registry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lynx-go/lynx/internal/clock"
)

// TestSubscribeSnapshotIsDeepCopy：订阅推送是深拷贝副本（与 Get 的
// 「快照副本，调用方可原地改」契约一致）：订阅者原地修改不影响共享缓存
// 与后续读取。
func TestSubscribeSnapshotIsDeepCopy(t *testing.T) {
	fd := &fakeDiscovery{silent: true}
	r := NewResolver(fd, WithPollInterval(time.Hour))
	defer func() { _ = r.Close() }()

	w, err := r.Subscribe("svc", Filter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = w.Stop() }()

	e, ok := r.entryFor("svc")
	if !ok {
		t.Fatal("entryFor failed")
	}
	e.store([]Instance{{
		ID: "a", Name: "svc", Status: StatusPassing,
		Endpoints: []Endpoint{{Protocol: ProtocolGRPC, Address: "1.1.1.1:9090"}},
		Tags:      []string{"api"},
		Meta:      map[string]string{"region": "cn"},
	}})

	snap, err := nextWithTimeout(t, w)
	if err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if len(snap) != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}
	// 原地篡改订阅快照：共享缓存不得受影响。
	snap[0].Endpoints[0].Address = "mutated"
	snap[0].Tags[0] = "mutated"
	snap[0].Meta["region"] = "mutated"

	got, err := r.Get(context.Background(), "svc", Filter{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Endpoints[0].Address != "1.1.1.1:9090" || got.Tags[0] != "api" || got.Meta["region"] != "cn" {
		t.Fatalf("subscription snapshot must be a deep copy, cache mutated: %+v", got)
	}
}

// TestSubscribeStaleNotNotifiedThenRecovers：stale 丢弃不通知订阅者
// （last-known 语义）；后端恢复后的下一次 store 自动推送，订阅不丢。
func TestSubscribeStaleNotNotifiedThenRecovers(t *testing.T) {
	fc := clock.NewFake(time.Unix(0, 0))
	fd := &fakeDiscovery{silent: true}
	r := NewResolver(fd,
		WithResolverClock(fc),
		WithStaleMaxAge(10*time.Second),
		WithPollInterval(time.Hour))
	defer func() { _ = r.Close() }()

	w, err := r.Subscribe("svc", Filter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = w.Stop() }()

	e, ok := r.entryFor("svc")
	if !ok {
		t.Fatal("entryFor failed")
	}
	e.store([]Instance{inst("a")})
	if _, err := nextWithTimeout(t, w); err != nil {
		t.Fatalf("first Next: %v", err)
	}

	// 越过 stale：读路径丢弃缓存。
	fc.Advance(11 * time.Second)
	if _, err := r.Get(context.Background(), "svc", Filter{}); !errors.Is(err, ErrNoInstance) {
		t.Fatalf("past stale boundary Get = %v, want ErrNoInstance", err)
	}

	// 订阅不通知：短窗口内 Next 不得返回。
	nextCh := make(chan []Instance, 1)
	go func() {
		s, err := w.Next()
		if err == nil {
			nextCh <- s
		}
	}()
	select {
	case s := <-nextCh:
		t.Fatalf("stale drop must not notify subscribers, got %+v", ids(s))
	case <-time.After(100 * time.Millisecond):
	}

	// 恢复：下一次 store 自动推送。
	e.store([]Instance{inst("b")})
	select {
	case s := <-nextCh:
		if len(s) != 1 || s[0].ID != "b" {
			t.Fatalf("recovery push = %+v", ids(s))
		}
	case <-time.After(nextTimeout):
		t.Fatal("subscription must recover after stale")
	}
}
