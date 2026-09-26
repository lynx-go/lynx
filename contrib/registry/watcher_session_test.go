package registry

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// sessionInstance 构造带完整字段的实例（相等判定的字段矩阵用）。
func sessionInstance(id string) Instance {
	return Instance{
		Name:    "svc",
		ID:      id,
		Version: "v1",
		Status:  StatusPassing,
		Weight:  100,
		Endpoints: []Endpoint{
			{Protocol: ProtocolHTTP, Address: id + ":8080"},
			{Protocol: ProtocolGRPC, Address: id + ":9090"},
		},
		Tags: []string{"z", "a"},
		Meta: map[string]string{"region": "cn"},
	}
}

// TestCanonicalSnapshotEquality 钉住规范相等的全字段、顺序无关语义：
// 实例/Endpoints/Tags 顺序打乱视为相等；任一字段变化必须被发现。
func TestCanonicalSnapshotEquality(t *testing.T) {
	base := []Instance{sessionInstance("b"), sessionInstance("a")}
	shuffled := []Instance{sessionInstance("a"), sessionInstance("b")}
	// 打乱端点与标签顺序（规范化应消除差异）。
	slices.Reverse(shuffled[0].Endpoints)
	slices.Reverse(shuffled[1].Tags)
	shuffled[0].Meta = map[string]string{"region": "cn"} // map 无序，等值

	if !snapshotsEqual(canonicalSnapshot(base), canonicalSnapshot(shuffled)) {
		t.Fatal("order-only differences must compare equal after canonicalization")
	}

	mutations := []struct {
		name   string
		mutate func([]Instance)
	}{
		{"status", func(s []Instance) { s[0].Status = StatusWarning }},
		{"version", func(s []Instance) { s[0].Version = "v2" }},
		{"weight", func(s []Instance) { s[0].Weight = 50 }},
		{"endpoints", func(s []Instance) { s[0].Endpoints = s[0].Endpoints[:1] }},
		{"tags", func(s []Instance) { s[0].Tags = []string{"other"} }},
		{"meta", func(s []Instance) { s[0].Meta = map[string]string{"region": "us"} }},
		{"name", func(s []Instance) { s[0].Name = "other" }},
	}
	for _, m := range mutations {
		got := canonicalSnapshot(base)
		m.mutate(got)
		if snapshotsEqual(canonicalSnapshot(base), got) {
			t.Errorf("%s change must be detected", m.name)
		}
	}

	// 深拷贝：规范化输出与输入不共享内部切片/映射。
	raw := []Instance{sessionInstance("a")}
	canon := canonicalSnapshot(raw)
	canon[0].Endpoints[0].Address = "mutated"
	canon[0].Tags[0] = "mutated"
	canon[0].Meta["region"] = "mutated"
	if raw[0].Endpoints[0].Address != "a:8080" || raw[0].Tags[0] != "z" || raw[0].Meta["region"] != "cn" {
		t.Fatal("canonicalSnapshot must deep-copy instance internals")
	}
}

// TestWatcherSessionSubmitFiltersAndSuppressesEqual 钉住会话核心的提交
// 语义：后置过滤、规范相等抑制（顺序打乱的无变化提交不唤醒）、变化推送。
func TestWatcherSessionSubmitFiltersAndSuppressesEqual(t *testing.T) {
	s := NewWatcherSession(context.Background(), "svc", Filter{Protocol: ProtocolHTTP}, nil)
	defer func() { _ = s.Stop() }()

	raw := []Instance{
		{Name: "svc", ID: "pass", Status: StatusPassing, Endpoints: []Endpoint{{Protocol: ProtocolHTTP, Address: "1:1"}}},
		{Name: "svc", ID: "critical", Status: StatusCritical, Endpoints: []Endpoint{{Protocol: ProtocolHTTP, Address: "2:2"}}},
		{Name: "svc", ID: "grpc-only", Status: StatusPassing, Endpoints: []Endpoint{{Protocol: ProtocolGRPC, Address: "3:3"}}},
	}
	snap, err := s.Next(func(context.Context) ([]Instance, error) { return raw, nil })
	if err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if len(snap) != 1 || snap[0].ID != "pass" {
		t.Fatalf("filter must keep only the http/passing instance, got %+v", snap)
	}

	// 顺序打乱、内容相同的提交：抑制推送（短窗口内不唤醒）。
	nextCh := make(chan []Instance, 1)
	go func() {
		got, err := s.Next(func(context.Context) ([]Instance, error) {
			return nil, errors.New("first closure must not be called twice")
		})
		if err == nil {
			nextCh <- got
		}
	}()
	s.Submit([]Instance{raw[0], raw[2], raw[1]})
	select {
	case got := <-nextCh:
		t.Fatalf("equal submit must not wake Next, got %+v", got)
	case <-time.After(100 * time.Millisecond):
	}

	// 内容变化：推送。
	changed := append(slices.Clone(raw), Instance{
		Name: "svc", ID: "new", Status: StatusPassing,
		Endpoints: []Endpoint{{Protocol: ProtocolHTTP, Address: "4:4"}},
	})
	s.Submit(changed)
	select {
	case got := <-nextCh:
		if len(got) != 2 {
			t.Fatalf("changed submit must push, got %+v", ids(got))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("changed submit did not wake Next")
	}
}

// TestWatcherSessionSubmitBeforeBaselineDoesNotPush 钉住基线前提交语义：
// 首个 Next 前到达的提交只更新相等基线、不投递（首个 Next 的 first 查询
// 返回当前状态，先推会在查询/推送竞态中投递陈旧快照）；基线后的变化
// 正常推送。
func TestWatcherSessionSubmitBeforeBaselineDoesNotPush(t *testing.T) {
	s := NewWatcherSession(context.Background(), "svc", Filter{}, nil)
	defer func() { _ = s.Stop() }()

	s.Submit([]Instance{sessionInstance("stale")}) // 基线前：记录基线，不推送

	snap, err := s.Next(func(context.Context) ([]Instance, error) {
		return []Instance{sessionInstance("current")}, nil
	})
	if err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if len(snap) != 1 || snap[0].ID != "current" {
		t.Fatalf("first Next must return the queried current state, got %+v", ids(snap))
	}

	nextCh := make(chan []Instance, 1)
	go func() {
		got, err := s.Next(nil)
		if err == nil {
			nextCh <- got
		}
	}()
	select {
	case got := <-nextCh:
		t.Fatalf("pre-baseline submit must not be delivered, got %+v", ids(got))
	case <-time.After(100 * time.Millisecond):
	}

	s.Submit([]Instance{sessionInstance("next")})
	select {
	case got := <-nextCh:
		if len(got) != 1 || got[0].ID != "next" {
			t.Fatalf("post-baseline change = %+v, want [next]", ids(got))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("post-baseline change must be pushed")
	}
}

// TestPullPolicyNextDelay 钉住拉模式退避节奏：DNS 的固定钳制值用
// MinDelay == MaxDelay 表达；Consul 从 MinDelay 起倍增封顶 MaxDelay。
func TestPullPolicyNextDelay(t *testing.T) {
	dns := PullPolicy{Interval: 15 * time.Second, MinDelay: 15 * time.Second, MaxDelay: 15 * time.Second}
	if got := dns.nextDelay(0); got != 15*time.Second {
		t.Fatalf("dns first failure = %s, want 15s", got)
	}
	if got := dns.nextDelay(15 * time.Second); got != 15*time.Second {
		t.Fatalf("dns repeated failure = %s, want clamped 15s", got)
	}

	consul := PullPolicy{MinDelay: time.Second, MaxDelay: 30 * time.Second}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	prev := time.Duration(0)
	for i, w := range want {
		prev = consul.nextDelay(prev)
		if prev != w {
			t.Fatalf("consul step %d = %s, want %s", i, prev, w)
		}
	}
}

// TestWatcherSessionRunPullPoll 钉住轮询模式：失败按退避重试、成功后
// Submit 推送；ctx 取消/Stop 退出。
func TestWatcherSessionRunPullPoll(t *testing.T) {
	s := NewWatcherSession(context.Background(), "svc", Filter{}, nil)
	defer func() { _ = s.Stop() }()

	var calls atomic.Int32
	query := func(context.Context) ([]Instance, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("transient boom")
		}
		return []Instance{sessionInstance("a")}, nil
	}
	// 首快照（空基线）。
	if _, err := s.Next(func(context.Context) ([]Instance, error) { return []Instance{}, nil }); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	go s.RunPull(query, PullPolicy{Interval: 5 * time.Millisecond, MinDelay: 5 * time.Millisecond, MaxDelay: 20 * time.Millisecond})

	nextCh := make(chan []Instance, 1)
	go func() {
		got, err := s.Next(nil)
		if err == nil {
			nextCh <- got
		}
	}()
	select {
	case got := <-nextCh:
		if len(got) != 1 || got[0].ID != "a" {
			t.Fatalf("push after retry = %+v", ids(got))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunPull did not recover after transient error")
	}
}

// TestWatcherSessionRunPullLongPoll 钉住长轮询模式：首次立即查询（不等
// Interval）、成功后立即续查；Stop 唤醒阻塞中的查询并退出。
func TestWatcherSessionRunPullLongPoll(t *testing.T) {
	s := NewWatcherSession(context.Background(), "svc", Filter{}, nil)

	if _, err := s.Next(func(context.Context) ([]Instance, error) { return []Instance{}, nil }); err != nil {
		t.Fatalf("first Next: %v", err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	var once atomic.Bool
	query := func(ctx context.Context) ([]Instance, error) {
		if once.CompareAndSwap(false, true) {
			close(started)
		}
		select {
		case <-release:
			return []Instance{sessionInstance("a")}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	go s.RunPull(query, PullPolicy{MinDelay: time.Second, MaxDelay: time.Second})

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("long-poll must query immediately without waiting Interval")
	}
	close(release)
	nextCh := make(chan []Instance, 1)
	go func() {
		got, err := s.Next(nil)
		if err == nil {
			nextCh <- got
		}
	}()
	select {
	case got := <-nextCh:
		if len(got) != 1 || got[0].ID != "a" {
			t.Fatalf("long-poll push = %+v", ids(got))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("long-poll result not pushed")
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestWatcherSessionEmptyDelaySlowsPoll 钉住空快照的负缓存节奏：空结果
// 仍 Submit（服务下线立即生效），但下一轮改用 EmptyDelay。
func TestWatcherSessionEmptyDelaySlowsPoll(t *testing.T) {
	s := NewWatcherSession(context.Background(), "svc", Filter{}, nil)
	defer func() { _ = s.Stop() }()

	var calls atomic.Int32
	query := func(context.Context) ([]Instance, error) {
		calls.Add(1)
		return nil, nil
	}
	if _, err := s.Next(func(context.Context) ([]Instance, error) { return []Instance{}, nil }); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	go s.RunPull(query, PullPolicy{Interval: 5 * time.Millisecond, EmptyDelay: time.Hour})
	time.Sleep(80 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("empty result must switch to EmptyDelay after one poll, calls = %d", got)
	}
}
