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

var errTTLTooShort = errors.New("consul: store ttl must be at least 10s (session minimum)")

type kvStore struct {
	c    *Client
	opts []cluster.Option
}

// Store 返回基于本 Client 的 cluster.Store（KV + Session）。
// 与 Registry 共用同一 Consul 连接与 token。Session TTL 最短 10s，
// 短间隔任务请用 contrib/cluster-redis。
func (c *Client) Store(opts ...cluster.Option) cluster.Store {
	return &kvStore{c: c, opts: opts}
}

// NewStore 等价于 c.Store(opts...)。
func NewStore(c *Client, opts ...cluster.Option) cluster.Store {
	return c.Store(opts...)
}

func (s *kvStore) Claim(ctx context.Context, name string, ttl time.Duration) (bool, error) {
	_, err := s.createLock(ctx, name, ttl)
	if err != nil {
		if errors.Is(err, errBusy) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *kvStore) Acquire(ctx context.Context, name string, ttl time.Duration) (cluster.Lease, bool, error) {
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
	go l.renewLoop(sessionTTL(ttl))
	return l, true, nil
}

var errBusy = errors.New("consul: lock held")

func (s *kvStore) createLock(ctx context.Context, name string, ttl time.Duration) (string, error) {
	if err := s.c.checkOpen(); err != nil {
		return "", err
	}
	if name == "" {
		return "", cluster.ErrEmptyName
	}
	if ttl <= 0 {
		return "", cluster.ErrInvalidTTL
	}
	if ttl < MinSessionTTL {
		return "", fmt.Errorf("%w: got %s", errTTLTooShort, ttl)
	}
	if err := ctx.Err(); err != nil {
		return "", err
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

func (l *sessionLease) renewLoop(ttl time.Duration) {
	interval := ttl / 3
	if interval < time.Second {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-l.ctx.Done():
			return
		case <-t.C:
			_, _, err := l.c.api.Session().Renew(l.session, nil)
			if err != nil {
				l.cancel()
				return
			}
		}
	}
}

var (
	_ cluster.Store = (*kvStore)(nil)
	_ cluster.Lease = (*sessionLease)(nil)
)
