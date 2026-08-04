package pubsub

import (
	"github.com/lynx-go/lynx"
)

// Binder 定义事件名与主题名之间的绑定关系，并提供消费者构建器。
type Binder interface {
	lynx.ServerLike
	SetBroker(broker Broker)
	ConsumerBuilders() []lynx.ComponentBuilder
	CanPublish(eventName string) (string, bool)
	CanSubscribe(eventName string) (string, bool)
}
