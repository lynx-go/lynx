package lynx

import (
	"context"
	"time"
)

type Context struct {
	ctx   context.Context
	hvals map[string]interface{}
}

func (c Context) Deadline() (deadline time.Time, ok bool) {
	return c.ctx.Deadline()
}

func (c Context) Done() <-chan struct{} {
	return c.ctx.Done()
}

func (c Context) Err() error {
	return c.ctx.Err()
}

func (c Context) Value(key any) any {
	//TODO implement me
	panic("implement me")
}

var _ context.Context = new(Context)
