package kafka

import (
	"github.com/IBM/sarama"
	watermillkafka "github.com/ThreeDotsLabs/watermill-kafka/v3/pkg/kafka"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/lynx-go/lynx/eventbus"
)

// wireMarshaler 将 RawEvent 的 MessageKey（x-message-key）写入 Kafka record Key
//（分区键）。DefaultMarshaler 仅把 metadata 放进 headers，不会设置 Key。
// 消费侧：若 headers 缺 x-message-key 但 record Key 非空，则回填到 metadata，
// 以便 DecodeWireMetadata 还原 Event.Key。
type wireMarshaler struct{}

func (wireMarshaler) Marshal(topic string, msg *message.Message) (*sarama.ProducerMessage, error) {
	kafkaMsg, err := watermillkafka.DefaultMarshaler{}.Marshal(topic, msg)
	if err != nil {
		return nil, err
	}
	if key := msg.Metadata.Get(eventbus.MetaMessageKey); key != "" {
		kafkaMsg.Key = sarama.ByteEncoder(key)
	}
	return kafkaMsg, nil
}

func (wireMarshaler) Unmarshal(kafkaMsg *sarama.ConsumerMessage) (*message.Message, error) {
	msg, err := watermillkafka.DefaultMarshaler{}.Unmarshal(kafkaMsg)
	if err != nil {
		return nil, err
	}
	if msg.Metadata.Get(eventbus.MetaMessageKey) == "" && len(kafkaMsg.Key) > 0 {
		msg.Metadata.Set(eventbus.MetaMessageKey, string(kafkaMsg.Key))
	}
	return msg, nil
}

var _ watermillkafka.MarshalerUnmarshaler = wireMarshaler{}
