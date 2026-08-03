package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type slotCleanupCache struct {
	ConcurrencyCache
	calls    atomic.Int64
	contexts chan context.Context
	err      error
}

func (c *slotCleanupCache) CleanupStaleProcessSlots(context.Context, string) error { return nil }

func (c *slotCleanupCache) CleanupExpiredAccountSlotKeys(ctx context.Context) error {
	c.calls.Add(1)
	if c.contexts != nil {
		c.contexts <- ctx
	}
	return c.err
}

func TestProvideConcurrencyServiceConfiguresButDoesNotStartSlotCleanup(t *testing.T) {
	cache := &slotCleanupCache{}
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.SlotCleanupInterval = time.Millisecond

	svc := ProvideConcurrencyService(cache, cfg, nil)

	require.Equal(t, time.Millisecond, svc.CleanupInterval())
	require.Never(t, func() bool { return cache.calls.Load() > 0 }, 50*time.Millisecond, time.Millisecond)
}
