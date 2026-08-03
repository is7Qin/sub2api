//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSchedulerSnapshotCacheMissUsesCallerContext(t *testing.T) {
	cache := &cacheOnlySnapshotCache{}
	svc := newSchedulerSnapshotService(cache, nil, nil, nil, panicSchedulerDBRepo{}, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	accounts, _, err := svc.ListSchedulableAccounts(ctx, nil, PlatformOpenAI, false)

	require.ErrorIs(t, err, ErrSchedulerCacheNotReady)
	require.Nil(t, accounts)
	require.Equal(t, 1, cache.getSnapshotCalls)
}
