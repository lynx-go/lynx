package watermill

import (
	"context"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/lynx-go/lynx/eventbus"
)

// PumpMessages 将 watermill 消息 channel 泵为 Delivery channel——watermill
// 生态 Transport 实现的共享投递泵（设计文档 §5.1 单一映射点的投递侧，
// MemoryTransport 与 watermill-kafka 共用）：
//   - 消息经 FromMessage 还原；Topic 为空时回填订阅的逻辑 topic（经 lynx
//     发布的消息携带 x-logical-topic，优先生效）；
//   - Ack/Nack 转达原 *message.Message（Kafka offset 提交依赖此路径）。
//
// WK-05 语义：收发两侧均带 ctx 退出分支——下游停读（返回 channel 无人
// 消费）时裸发送会永久阻塞 goroutine 且 in-flight 消息的确认丢失；ctx
// 取消时对手中消息 Nack 交还 Transport，随后关闭输出释放下游。
func PumpMessages(ctx context.Context, in <-chan *message.Message, logicalTopic string) <-chan eventbus.Delivery {
	out := make(chan eventbus.Delivery)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-in:
				if !ok {
					return
				}
				raw := FromMessage(msg)
				if raw.Topic == "" {
					raw.Topic = logicalTopic
				}
				wm := msg
				select {
				case out <- eventbus.Delivery{
					Event: raw,
					Ack:   func() { _ = wm.Ack() },
					Nack:  func() { wm.Nack() },
				}:
				case <-ctx.Done():
					wm.Nack()
					return
				}
			}
		}
	}()
	return out
}
