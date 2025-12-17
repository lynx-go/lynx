package pubsub

import (
	"github.com/lynx-go/lynx"
)

type Binder interface {
	lynx.ServerLike
	SetBroker(broker Broker)
	ConsumerBuilders() []lynx.ComponentBuilder
	CanPublish(eventName string) (string, bool)
	CanSubscribe(eventName string) (string, bool)
}
