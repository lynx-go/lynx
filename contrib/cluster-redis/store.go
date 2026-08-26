// Package clusterredis 用 Redis 实现 cluster.Store（SET NX + 续约）。
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

type store struct {
	rdb  redis.Cmdable
	opts []cluster.Option
}

// NewStore 用 Redis 客户端构造 cluster.Store。rdb 通常是 *redis.Client。
func NewStore(rdb redis.Cmdable, opts ...cluster.Option) cluster.Store {
	return &store{rdb: rdb, opts: opts}
}

func (s *store) Claim(ctx context.Context, name string, ttl time.Duration) (bool, error) {
	if name == "" {
		return false, cluster.ErrEmptyName
	}
	if ttl <= 0 {
		return false, cluster.ErrInvalidTTL
	}
	key := cluster.FormatKey(name, s.opts...)
	owner := cluster.Owner(s.opts...)
	ok, err := s.rdb.SetNX(ctx, key, owner, ttl).Result()
	if err != nil {
		return false, err
	}
	return ok, nil
}

func (s *store) Acquire(ctx context.Context, name string, ttl time.Duration) (cluster.Lease, bool, error) {
	if name == "" {
		return nil, false, cluster.ErrEmptyName
	}
	if ttl <= 0 {
		return nil, false, cluster.ErrInvalidTTL
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
	go l.renewLoop()
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

func (l *redisLease) renewLoop() {
	interval := max(l.ttl/3, time.Millisecond)
	t := time.NewTicker(interval)
	defer t.Stop()
	ms := l.ttl.Milliseconds()
	for {
		select {
		case <-l.ctx.Done():
			return
		case <-t.C:
			n, err := l.rdb.Eval(context.Background(), renewScript, []string{l.key}, l.token, ms).Int()
			if err != nil || n == 0 {
				l.cancel()
				return
			}
		}
	}
}

func newToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

var (
	_ cluster.Store = (*store)(nil)
	_ cluster.Lease = (*redisLease)(nil)
)
