// Package cluster 提供进程间协调：具名占位（Claim）与长租约（Acquire）。
// 本包不含 Redis/Consul 依赖；生产适配器在 contrib/consul 与 contrib/cluster-redis。
package cluster

import (
	"context"
	"errors"
	"time"
)

const (
	// DefaultNamespace 是未指定 WithNamespace 时的键前缀。
	DefaultNamespace = "lynx"
	// DefaultLeaseTTL 是 Singleton / Campaign 的默认租约 TTL。
	DefaultLeaseTTL = 15 * time.Second
)

var (
	// ErrInvalidTTL 表示 ttl <= 0。
	ErrInvalidTTL = errors.New("cluster: ttl must be positive")
	// ErrEmptyName 表示占位名为空。
	ErrEmptyName = errors.New("cluster: empty name")
	// ErrNilCoordinator 表示 Coordinator 为 nil。
	ErrNilCoordinator = errors.New("cluster: nil coordinator")
	// ErrNilService 表示 Singleton 的 inner 为 nil。
	ErrNilService = errors.New("cluster: nil service")
)

// Coordinator 是进程间协调端口：Claim 一次性占位、Acquire 长租约。
// 适配器必须并发安全。
type Coordinator interface {
	// Claim 一次性占位，ttl 后自动过期，无续约、不释放。
	// won=false 且 err=nil 表示已被占用（跳过，不是错误）。
	Claim(ctx context.Context, name string, ttl time.Duration) (won bool, err error)
	// Acquire 长租约：持有期间按 ttl 续约；崩溃后约在 ttl 内过期。
	// ok=false 且 err=nil 表示当前被别人持有。
	Acquire(ctx context.Context, name string, ttl time.Duration) (Lease, bool, error)
}

// Lease 是 Acquire 拿到的租约。Release 幂等。
type Lease interface {
	// Context 在续约失败、租约丢失或 Release 后取消。
	Context() context.Context
	Release(ctx context.Context) error
}

// Option 配置 Coordinator 的命名空间与实例标识。
type Option func(*coordinatorOptions)

type coordinatorOptions struct {
	namespace string
	instance  string
}

func defaultCoordinatorOptions() coordinatorOptions {
	return coordinatorOptions{
		namespace: DefaultNamespace,
	}
}

func applyCoordinatorOptions(opts []Option) coordinatorOptions {
	o := defaultCoordinatorOptions()
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	if o.namespace == "" {
		o.namespace = DefaultNamespace
	}
	return o
}

// WithNamespace 设置键前缀（默认 "lynx"）。实际键为 "{ns}/{name}"。
func WithNamespace(ns string) Option {
	return func(o *coordinatorOptions) {
		if ns != "" {
			o.namespace = ns
		}
	}
}

// WithInstance 写入占位 value，仅供排障（默认空）。
func WithInstance(id string) Option {
	return func(o *coordinatorOptions) {
		o.instance = id
	}
}

func (o coordinatorOptions) key(name string) string {
	return o.namespace + "/" + name
}

// FormatKey 返回适配器写入后端的键：{namespace}/{name}。
func FormatKey(name string, opts ...Option) string {
	return applyCoordinatorOptions(opts).key(name)
}

// Owner 返回 WithInstance 设置的排障标识，未设置时为空。
func Owner(opts ...Option) string {
	return applyCoordinatorOptions(opts).instance
}
