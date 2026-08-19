package registry

import (
	"math/rand/v2"
	"sync/atomic"
)

// Picker 从一组实例中挑选一个。空切片返回 ErrNoInstance。
// v1 内置实现均忽略 Instance.Weight（字段仍进入目录，供以后或外部消费者）。
type Picker interface {
	Pick(instances []Instance) (Instance, error)
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
