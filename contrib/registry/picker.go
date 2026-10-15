package registry

import (
	"hash/fnv"
	"math/rand/v2"
	"sync/atomic"
)

// Picker 从一组实例中挑选一个。空切片返回 ErrNoInstance。
// v1 内置实现均忽略 Instance.Weight（字段仍进入目录，供以后或外部消费者）。
type Picker interface {
	Pick(instances []Instance) (Instance, error)
}

// KeyedPicker 由调用方提供路由 key，从一组实例中挑选一个。空列表返回
// ErrNoInstance。与 Picker 不同，它面向「同一 key 稳定落到同一实例」的
// 定向路由场景（如按房间/场景把请求路由到内存态服务实例）；同一
// (key, instances) 的映射必须确定——各调用方（进程/节点）独立计算也要
// 得到同一实例，才能免协调地收敛到同一属主。
type KeyedPicker interface {
	Pick(key string, instances []Instance) (Instance, error)
}

// RandomPicker 返回均匀随机的 Picker；忽略 Instance.Weight。
func RandomPicker() Picker { return randomPicker{} }

type randomPicker struct{}

func (randomPicker) Pick(instances []Instance) (Instance, error) {
	if len(instances) == 0 {
		return Instance{}, ErrNoInstance
	}
	return instances[rand.IntN(len(instances))], nil
}

// RoundRobinPicker 返回原子计数取模的 Picker；忽略 Instance.Weight。
func RoundRobinPicker() Picker { return &roundRobinPicker{} }

type roundRobinPicker struct {
	next atomic.Uint64
}

func (p *roundRobinPicker) Pick(instances []Instance) (Instance, error) {
	if len(instances) == 0 {
		return Instance{}, ErrNoInstance
	}
	n := p.next.Add(1) - 1
	return instances[n%uint64(len(instances))], nil
}

// RendezvousPicker 返回 Rendezvous（HRW，最高随机权重）一致性 hash 实现：
// score = FNV-1a 64(key + "\x00" + instance.ID)，取 score 最大者；score
// 并列时取 instance.ID 字典序较小者（冻结 tie-break）。映射已冻结——它
// 是跨服务、跨语言的分布契约（消费方可能用其他语言对同一目录重算映射
// 做预分片），任何变更都会打断既有部署的属主关系，禁止改动；测试向量
// 同步钉死（picker_test.go frozenKeyedVector）。忽略 Instance.Weight
// （v1，与内置 Picker 一致）。实例摘除时只有其名下 key 迁移（Rendezvous
// 最小扰动）。空 key 合法（按空串参与 hash）。
func RendezvousPicker() KeyedPicker { return rendezvousPicker{} }

type rendezvousPicker struct{}

func (rendezvousPicker) Pick(key string, instances []Instance) (Instance, error) {
	if len(instances) == 0 {
		return Instance{}, ErrNoInstance
	}
	h := fnv.New64a()
	var (
		best  Instance
		score uint64
	)
	for i, inst := range instances {
		h.Reset()
		_, _ = h.Write([]byte(key + "\x00" + inst.ID))
		s := h.Sum64()
		if i == 0 || s > score || (s == score && inst.ID < best.ID) {
			best, score = inst, s
		}
	}
	return best, nil
}
