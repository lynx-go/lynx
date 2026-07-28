package http

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestChainAppliesMiddlewareInDeclarationOrder(t *testing.T) {
	var order []string
	mw := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name+":before")
				next.ServeHTTP(w, r)
				order = append(order, name+":after")
			})
		}
	}
	handler := chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "handler")
	}), []Middleware{mw("mw1"), mw("mw2")})

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	want := []string{"mw1:before", "mw2:before", "handler", "mw2:after", "mw1:after"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
}

func TestWithMiddlewareAccumulates(t *testing.T) {
	o := Options{}
	noop := func(next http.Handler) http.Handler { return next }
	WithMiddleware(noop)(&o)
	WithMiddleware(noop, noop)(&o)
	if len(o.Middlewares) != 3 {
		t.Errorf("len(Middlewares) = %d, want 3", len(o.Middlewares))
	}
}

func TestServerAppliesMiddleware(t *testing.T) {
	mw := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Middleware", "applied")
			next.ServeHTTP(w, r)
		})
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	srv := NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), WithAddr(addr), WithMiddleware(mw))

	startErr := make(chan error, 1)
	go func() { startErr <- srv.Start(context.Background()) }()
	waitForDial(t, addr)
	defer srv.Stop(context.Background())

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("GET error = %v", err)
	}
	_ = resp.Body.Close()

	if got := resp.Header.Get("X-Middleware"); got != "applied" {
		t.Errorf("X-Middleware header = %q, want %q", got, "applied")
	}

	srv.Stop(context.Background())
	select {
	case err := <-startErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Start() error = %v, want nil or http.ErrServerClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start() did not return after Stop()")
	}
}
