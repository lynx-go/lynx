package kafka

import (
	"hash"

	"github.com/IBM/sarama"
	"github.com/xdg-go/scram"
)

// xdgSCRAMClient 是基于 github.com/xdg-go/scram 的 sarama.SCRAMClient 实现，
// 用于 SCRAM-SHA-256/512 机制（sarama 不内置 SCRAM 客户端，config.Validate()
// 强制要求 SCRAMClientGeneratorFunc 非 nil）。
type xdgSCRAMClient struct {
	*scram.Client
	*scram.ClientConversation
	scram.HashGeneratorFcn
}

// Begin 开始 SCRAM 认证会话。
func (x *xdgSCRAMClient) Begin(userName, password, authzID string) error {
	client, err := x.NewClient(userName, password, authzID)
	if err != nil {
		return err
	}
	x.Client = client
	x.ClientConversation = x.NewConversation()
	return nil
}

// Step 推进一次挑战-应答。
func (x *xdgSCRAMClient) Step(challenge string) (string, error) {
	return x.ClientConversation.Step(challenge)
}

// Done 返回会话是否完成。
func (x *xdgSCRAMClient) Done() bool {
	return x.ClientConversation.Done()
}

var _ sarama.SCRAMClient = (*xdgSCRAMClient)(nil)

// newSCRAMClientGenerator 返回指定哈希族的 SCRAM 客户端生成器。
func newSCRAMClientGenerator(hashGen func() hash.Hash) func() sarama.SCRAMClient {
	return func() sarama.SCRAMClient {
		return &xdgSCRAMClient{HashGeneratorFcn: hashGen}
	}
}
