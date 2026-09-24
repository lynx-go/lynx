package main

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/lynx-go/lynx"
	"github.com/lynx-go/lynx/eventbus"
)

// OrderCreatedHandler 是业务 handler：结构体持有依赖（name 构造期注入），
// 经 lynx.NewHandlerService 适配为可注册的 Service。两个实例共享同一
// 类型，靠注入的 name 与注册点的 group 区分消费身份。
type OrderCreatedHandler struct {
	name string
}

func (h *OrderCreatedHandler) Topic() eventbus.Topic[OrderCreated] {
	return OrderCreatedTopic
}

func (h *OrderCreatedHandler) HandlerName() string { return h.name }

// Init 是依赖注入点：适配器保证它在订阅之前执行。本例依赖全部在构造期
// 注入，故直接返回 nil；需要 ctx.Config()/ctx.Logger() 时在此获取。
func (h *OrderCreatedHandler) Init(lynx.AppContext) error { return nil }

func (h *OrderCreatedHandler) Handle(ctx context.Context, e *eventbus.Event[OrderCreated]) error {
	logOrder(ctx, e, h.name)
	return nil
}

func logOrder(ctx context.Context, e *eventbus.Event[OrderCreated], name string) {
	s, _ := json.Marshal(e)
	slog.InfoContext(ctx, "recv order created", "handler", name, "event", string(s))
}
