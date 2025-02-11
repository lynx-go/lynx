package options

import "context"

type optionCtx struct {
}

var optionKey = optionCtx{}

func Context[O any](ctx context.Context, o O) context.Context {
	return context.WithValue(ctx, optionKey, o)
}

func FromContext[O any](ctx context.Context) O {
	return ctx.Value(optionKey).(O)
}
