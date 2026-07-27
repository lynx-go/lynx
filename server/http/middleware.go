package http

import "net/http"

// Middleware wraps an http.Handler, typically to add cross-cutting behavior
// such as logging, authentication, or custom metrics.
type Middleware func(http.Handler) http.Handler

// WithMiddleware registers middlewares applied to the server's handler in
// declaration order: the first declared middleware is the outermost. The
// final chain is: otel instrumentation -> request log -> middlewares -> handler.
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
