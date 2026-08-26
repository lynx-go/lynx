package cluster

import (
	"context"
	"time"
)

const campaignBackoff = 50 * time.Millisecond

// Leadership 是竞选成功后的长角色。Resign 立即让出。
type Leadership interface {
	Context() context.Context
	Resign(ctx context.Context) error
}

type leadership struct {
	Lease
}

func (l *leadership) Resign(ctx context.Context) error {
	return l.Release(ctx)
}

// Campaign 循环 Acquire 直到成为 leader 或 ctx 结束。
func Campaign(ctx context.Context, s Store, name string) (Leadership, error) {
	return CampaignTTL(ctx, s, name, DefaultLeaseTTL)
}

// CampaignTTL 与 Campaign 相同，但使用指定 ttl。
func CampaignTTL(ctx context.Context, s Store, name string, ttl time.Duration) (Leadership, error) {
	if s == nil {
		return nil, ErrNilStore
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lease, ok, err := s.Acquire(ctx, name, ttl)
		if err != nil {
			return nil, err
		}
		if ok {
			return &leadership{Lease: lease}, nil
		}
		timer := time.NewTimer(campaignBackoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

var _ Leadership = (*leadership)(nil)
