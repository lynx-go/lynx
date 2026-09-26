// Package clusterredis 用 Redis 实现 cluster.Coordinator（SET NX + 续约）。
// 这是协调后端，不是给业务用的 Redis 客户端。
package clusterredis

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/lynx-go/lynx/contrib/cluster"
	"github.com/redis/go-redis/v9"
)

const (
	renewScript = `if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("PEXPIRE", KEYS[1], ARGV[2]) else return 0 end`
	delScript   = `if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) else return 0 end`
)

type coordinator struct {
	rdb  redis.Cmdable
	opts []cluster.Option
}

// NewCoordinator 用 Redis 客户端构造 cluster.Coordinator。rdb 通常是 *redis.Client。
func NewCoordinator(rdb redis.Cmdable, opts ...cluster.Option) cluster.Coordinator {
	return &coordinator{rdb: rdb, opts: opts}
}

func (s *coordinator) Claim(ctx context.Context, name string, ttl time.Duration) (bool, error) {
	if err := cluster.ValidateCall(ctx, name, ttl); err != nil {
		return false, err
	}
	key := cluster.FormatKey(name, s.opts...)
	owner := cluster.Owner(s.opts...)
	ok, err := s.rdb.SetNX(ctx, key, owner, ttl).Result()
	if err != nil {
		return false, err
	}
	return ok, nil
}

func (s *coordinator) Acquire(ctx context.Context, name string, ttl time.Duration) (cluster.Lease, bool, error) {
	if err := cluster.ValidateCall(ctx, name, ttl); err != nil {
		return nil, false, err
	}
	key := cluster.FormatKey(name, s.opts...)
	token := newToken()
	ok, err := s.rdb.SetNX(ctx, key, token, ttl).Result()
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	leaseCtx, cancel := context.WithCancel(context.Background())
	l := &redisLease{
		rdb:    s.rdb,
		key:    key,
		token:  token,
		ttl:    ttl,
		ctx:    leaseCtx,
		cancel: cancel,
	}
	go cluster.RunRenewLoop(l.ctx, l.cancel, cluster.RenewInterval(l.ttl), cluster.ClockFrom(s.opts...), l.renew)
	return l, true, nil
}

type redisLease struct {
	rdb    redis.Cmdable
	key    string
	token  string
	ttl    time.Duration
	ctx    context.Context
	cancel context.CancelFunc
}

func (l *redisLease) Context() context.Context { return l.ctx }

func (l *redisLease) Release(ctx context.Context) error {
	l.cancel()
	return l.rdb.Eval(ctx, delScript, []string{l.key}, l.token).Err()
}

// renew 经 Lua 脚本条件续约：token 匹配才延长，否则视为丢失（返回
// cluster.ErrLeaseLost 触发引擎退出）。刻意用 Background：续约不受
// 调用侧取消影响，仅由丢失退出。
func (l *redisLease) renew() error {
	n, err := l.rdb.Eval(context.Background(), renewScript, []string{l.key}, l.token, l.ttl.Milliseconds()).Int()
	if err != nil {
		return err
	}
	if n == 0 {
		return cluster.ErrLeaseLost
	}
	return nil
}

func newToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

var (
	_ cluster.Coordinator = (*coordinator)(nil)
	_ cluster.Lease       = (*redisLease)(nil)
)
