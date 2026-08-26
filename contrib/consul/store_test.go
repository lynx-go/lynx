package consul

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lynx-go/lynx/contrib/cluster"
)

type fakeLock struct {
	mu       sync.Mutex
	seq      atomic.Uint64
	sessions map[string]struct{}
	held     map[string]string // key -> session
}

func newFakeLock(t *testing.T) (*fakeLock, *httptest.Server) {
	t.Helper()
	f := &fakeLock{
		sessions: make(map[string]struct{}),
		held:     make(map[string]string),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/session/create", f.handleCreate)
	mux.HandleFunc("PUT /v1/session/renew/{id}", f.handleRenew)
	mux.HandleFunc("PUT /v1/session/destroy/{id}", f.handleDestroy)
	mux.HandleFunc("PUT /v1/kv/{key...}", f.handleKV)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeLock) handleCreate(w http.ResponseWriter, r *http.Request) {
	id := strconv.FormatUint(f.seq.Add(1), 10)
	f.mu.Lock()
	f.sessions[id] = struct{}{}
	f.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]string{"ID": id})
}

func (f *fakeLock) handleRenew(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f.mu.Lock()
	_, ok := f.sessions[id]
	f.mu.Unlock()
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode([]map[string]string{{"ID": id}})
}

func (f *fakeLock) handleDestroy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f.mu.Lock()
	delete(f.sessions, id)
	for k, sess := range f.held {
		if sess == id {
			delete(f.held, k)
		}
	}
	f.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (f *fakeLock) handleKV(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	acquire := r.URL.Query().Get("acquire")
	_, _ = io.Copy(io.Discard, r.Body)
	if acquire == "" {
		http.Error(w, "missing acquire", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sessions[acquire]; !ok {
		_ = json.NewEncoder(w).Encode(false)
		return
	}
	if holder, ok := f.held[key]; ok && holder != acquire {
		_ = json.NewEncoder(w).Encode(false)
		return
	}
	f.held[key] = acquire
	_ = json.NewEncoder(w).Encode(true)
}

func TestStoreClaimExclusive(t *testing.T) {
	_, srv := newFakeLock(t)
	c := newTestClient(t, srv)
	s := c.Store(cluster.WithNamespace("app"))
	won, err := s.Claim(context.Background(), "job", MinSessionTTL)
	if err != nil || !won {
		t.Fatalf("first: won=%v err=%v", won, err)
	}
	won, err = s.Claim(context.Background(), "job", MinSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	if won {
		t.Fatal("second claim should lose")
	}
}

func TestStoreClaimRejectsShortTTL(t *testing.T) {
	_, srv := newFakeLock(t)
	c := newTestClient(t, srv)
	_, err := c.Store().Claim(context.Background(), "job", time.Second)
	if err == nil || !strings.Contains(err.Error(), "10s") {
		t.Fatalf("got %v", err)
	}
}

func TestStoreAcquireRelease(t *testing.T) {
	_, srv := newFakeLock(t)
	c := newTestClient(t, srv)
	s := NewStore(c, cluster.WithNamespace("app"), cluster.WithInstance("n1"))
	lease, ok, err := s.Acquire(context.Background(), "leader", MinSessionTTL)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	_, ok, err = s.Acquire(context.Background(), "leader", MinSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("second acquire should lose")
	}
	if err := lease.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, ok, err = s.Acquire(context.Background(), "leader", MinSessionTTL)
	if err != nil || !ok {
		t.Fatalf("after release: ok=%v err=%v", ok, err)
	}
}

func TestStoreEmptyName(t *testing.T) {
	_, srv := newFakeLock(t)
	c := newTestClient(t, srv)
	_, err := c.Store().Claim(context.Background(), "", MinSessionTTL)
	if !errors.Is(err, cluster.ErrEmptyName) {
		t.Fatalf("got %v", err)
	}
}
