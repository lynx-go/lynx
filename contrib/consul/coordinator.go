package consul

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hashicorp/consul/api"
	"github.com/lynx-go/lynx/contrib/cluster"
)

// MinSessionTTL 是 Consul Session TTL 的下限（官方 10s）。
const MinSessionTTL = 10 * time.Second

var errTTLTooShort = errors.New("consul: coordinator ttl must be at least 10s (session minimum)")

type kvCoordinator struct {
	c    *Client
	opts []cluster.Option
}

// Coordinator 返回基于本 Client 的 cluster.Coordinator（KV + Session）。
// 与 Registry 共用同一 Consul 连接与 token。Session TTL 最短 10s（经
// cluster.TTLAware / cluster.MinTTL 对调用方可见；schedule 的 Exclusive
// 触发会自动把短 TTL 钳制到下限）；无 TTL 下限需求的短间隔任务仍推荐
// contrib/cluster-redis。
func (c *Client) Coordinator(opts ...cluster.Option) cluster.Coordinator {
	return &kvCoordinator{c: c, opts: opts}
}

// NewCoordinator 等价于 c.Coordinator(opts...)。
func NewCoordinator(c *Client, opts ...cluster.Option) cluster.Coordinator {
	return c.Coordinator(opts...)
}

// MinTTL 实现 cluster.TTLAware：Consul Session 的官方下限。
func (s *kvCoordinator) MinTTL() time.Duration { return MinSessionTTL }

func (s *kvCoordinator) Claim(ctx context.Context, name string, ttl time.Duration) (bool, error) {
	_, err := s.createLock(ctx, name, ttl)
	if err != nil {
		if errors.Is(err, errBusy) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *kvCoordinator) Acquire(ctx context.Context, name string, ttl time.Duration) (cluster.Lease, bool, error) {
	sess, err := s.createLock(ctx, name, ttl)
	if err != nil {
		if errors.Is(err, errBusy) {
			return nil, false, nil
		}
		return nil, false, err
	}
	leaseCtx, cancel := context.WithCancel(context.Background())
	l := &sessionLease{
		c:       s.c,
		session: sess,
		ctx:     leaseCtx,
		cancel:  cancel,
	}
	go cluster.RunRenewLoop(l.ctx, l.cancel, cluster.RenewInterval(sessionTTL(ttl)), cluster.ClockFrom(s.opts...), l.renew)
	return l, true, nil
}

var errBusy = errors.New("consul: lock held")

func (s *kvCoordinator) createLock(ctx context.Context, name string, ttl time.Duration) (string, error) {
	if err := s.c.checkOpen(); err != nil {
		return "", err
	}
	if err := cluster.ValidateCall(ctx, name, ttl); err != nil {
		return "", err
	}
	if ttl < MinSessionTTL {
		return "", fmt.Errorf("%w: got %s", errTTLTooShort, ttl)
	}
	sttl := sessionTTL(ttl)
	entry := &api.SessionEntry{
		TTL:      sttl.String(),
		Behavior: api.SessionBehaviorDelete,
	}
	sess, _, err := s.c.api.Session().Create(entry, nil)
	if err != nil {
		return "", err
	}
	key := cluster.FormatKey(name, s.opts...)
	owner := cluster.Owner(s.opts...)
	ok, _, err := s.c.api.KV().Acquire(&api.KVPair{
		Key:     key,
		Value:   []byte(owner),
		Session: sess,
	}, nil)
	if err != nil {
		_, _ = s.c.api.Session().Destroy(sess, nil)
		return "", err
	}
	if !ok {
		_, _ = s.c.api.Session().Destroy(sess, nil)
		return "", errBusy
	}
	return sess, nil
}

func sessionTTL(d time.Duration) time.Duration {
	if d < MinSessionTTL {
		return MinSessionTTL
	}
	if d%time.Second != 0 {
		return d.Truncate(time.Second) + time.Second
	}
	return d
}

type sessionLease struct {
	c       *Client
	session string
	ctx     context.Context
	cancel  context.CancelFunc
}

func (l *sessionLease) Context() context.Context { return l.ctx }

func (l *sessionLease) Release(ctx context.Context) error {
	l.cancel()
	if err := l.c.checkOpen(); err != nil {
		return err
	}
	_, err := l.c.api.Session().Destroy(l.session, nil)
	return err
}

// renew 续期 Session；Session 失效（过期/销毁）时 Renew 返回错误。
func (l *sessionLease) renew() error {
	_, _, err := l.c.api.Session().Renew(l.session, nil)
	return err
}

var (
	_ cluster.Coordinator = (*kvCoordinator)(nil)
	_ cluster.Lease       = (*sessionLease)(nil)
)
