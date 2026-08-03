//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// cacheOnlySnapshotCache returns only worker-published scheduler state. It has
// no request-path mechanism to rebuild a missing snapshot.
type cacheOnlySnapshotCache struct {
	SchedulerCache
	snapshot         []*Account
	hit              bool
	getSnapshotCalls int
}

func (c *cacheOnlySnapshotCache) GetSnapshot(context.Context, SchedulerBucket) ([]*Account, bool, error) {
	c.getSnapshotCalls++
	return c.snapshot, c.hit, nil
}

func TestSchedulerSnapshotCacheMissReturnsUnavailableWithoutDB(t *testing.T) {
	cache := &cacheOnlySnapshotCache{}
	svc := newSchedulerSnapshotService(cache, nil, nil, nil, panicSchedulerDBRepo{}, nil, nil)

	accounts, _, err := svc.ListSchedulableAccounts(context.Background(), nil, PlatformOpenAI, false)

	require.ErrorIs(t, err, ErrSchedulerCacheNotReady)
	require.Nil(t, accounts)
	require.Equal(t, 1, cache.getSnapshotCalls)
}

func TestSchedulerSnapshotReturnsPublishedAccountsWithoutDB(t *testing.T) {
	cache := &cacheOnlySnapshotCache{
		hit: true,
		snapshot: []*Account{{
			ID:          1,
			Platform:    PlatformOpenAI,
			Schedulable: true,
		}},
	}
	svc := newSchedulerSnapshotService(cache, nil, nil, nil, panicSchedulerDBRepo{}, nil, nil)

	accounts, _, err := svc.ListSchedulableAccounts(context.Background(), nil, PlatformOpenAI, false)

	require.NoError(t, err)
	require.Equal(t, []Account{{ID: 1, Platform: PlatformOpenAI, Schedulable: true}}, accounts)
	require.Equal(t, 1, cache.getSnapshotCalls)
}
