package http

import (
	"net/http"

	"github.com/lynx-go/lynx/eventbus"
)

// Middleware wraps an http.Handler, typically to add cross-cutting behavior
// such as logging, authentication, or custom metrics.
type Middleware func(http.Handler) http.Handler

// WithMiddleware registers middlewares applied to the server's handler in
// declaration order: the first declared middleware is the outermost. The
// final chain is: otel instrumentation -> request log -> bus inject -> middlewares -> handler.
func WithMiddleware(middlewares ...Middleware) Option {
	return func(o *Options) {
		o.Middlewares = append(o.Middlewares, middlewares...)
	}
}

// chain wraps h with middlewares, first declared being outermost.
func chain(h http.Handler, middlewares []Middleware) http.Handler {
	for i := len(middlewares) - 1; i >= 0; i-- {
		h = middlewares[i](h)
	}
	return h
}

// injectBus 将 Bus 写入请求 Context，供 Topic.Publish/Subscribe 经 BusFromContext 解析。
func injectBus(b eventbus.Bus) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if b != nil {
				r = r.WithContext(eventbus.ContextWithBus(r.Context(), b))
			}
			next.ServeHTTP(w, r)
		})
	}
}
