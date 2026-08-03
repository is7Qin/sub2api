//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type schedulerSupportRepo struct {
	AccountRepository

	candidates []Account
	support    []Account
}

func (r schedulerSupportRepo) ListSchedulableByGroupIDAndPlatform(context.Context, int64, string) ([]Account, error) {
	return append([]Account(nil), r.candidates...), nil
}

func (r schedulerSupportRepo) ListModelAvailabilityCandidates(context.Context, *int64, []string, bool) ([]Account, error) {
	return append([]Account(nil), r.support...), nil
}

type staticStateCache struct {
	snapshotHydrationCache

	candidates map[SchedulerBucket][]Account
	support    map[SchedulerBucket][]Account
	buckets    []SchedulerBucket
}

func (c *staticStateCache) SetStaticState(_ context.Context, bucket SchedulerBucket, candidates, support []Account) error {
	if c.candidates == nil {
		c.candidates = make(map[SchedulerBucket][]Account)
		c.support = make(map[SchedulerBucket][]Account)
	}
	c.candidates[bucket] = append([]Account(nil), candidates...)
	c.support[bucket] = append([]Account(nil), support...)
	return nil
}

func (c *staticStateCache) GetPersistentSupport(_ context.Context, bucket SchedulerBucket) ([]*Account, bool, error) {
	accounts, ok := c.support[bucket]
	if !ok {
		return nil, false, nil
	}
	out := make([]*Account, 0, len(accounts))
	for i := range accounts {
		account := accounts[i]
		out = append(out, &account)
	}
	return out, true, nil
}

func (c *staticStateCache) ListBuckets(context.Context) ([]SchedulerBucket, error) {
	return append([]SchedulerBucket(nil), c.buckets...), nil
}

type staticStateGroupRepo struct {
	GroupRepository
	groups []Group
	err    error
}

func (r staticStateGroupRepo) ListActive(context.Context) ([]Group, error) {
	return append([]Group(nil), r.groups...), r.err
}

func staticStateAccountIDs(accounts []Account) []int64 {
	ids := make([]int64, 0, len(accounts))
	for _, account := range accounts {
		ids = append(ids, account.ID)
	}
	return ids
}

func TestSchedulerSnapshotService_RebuildPublishesPersistentSupport(t *testing.T) {
	bucket := SchedulerBucket{GroupID: 7, Platform: PlatformOpenAI, Mode: SchedulerModeSingle}
	cache := &staticStateCache{}
	repo := schedulerSupportRepo{
		candidates: []Account{{ID: 1, Platform: PlatformOpenAI}},
		support:    []Account{{ID: 1, Platform: PlatformOpenAI}, {ID: 2, Platform: PlatformOpenAI}},
	}
	svc := newSchedulerSnapshotService(cache, nil, nil, nil, repo, nil, &config.Config{})

	require.NoError(t, svc.rebuildBucketWithQueryCache(context.Background(), bucket, "test", nil))
	require.Equal(t, []int64{1}, staticStateAccountIDs(cache.candidates[bucket]))
	require.Equal(t, []int64{1, 2}, staticStateAccountIDs(cache.support[bucket]))
	support, hit, err := svc.ListPersistentSupport(context.Background(), ptrInt64(bucket.GroupID), PlatformOpenAI, false)
	require.NoError(t, err)
	require.True(t, hit)
	require.Equal(t, []int64{1, 2}, staticStateAccountIDs(support))
}

func TestSchedulerSnapshotService_DefaultBucketsMergeRegistryAndActiveGroups(t *testing.T) {
	registered := SchedulerBucket{GroupID: 99, Platform: PlatformOpenAI, Mode: SchedulerModeSingle}
	cache := &staticStateCache{buckets: []SchedulerBucket{registered}}
	groups := staticStateGroupRepo{groups: []Group{{ID: 7, Platform: PlatformOpenAI}}}
	svc := newSchedulerSnapshotService(cache, nil, nil, nil, nil, groups, &config.Config{})

	buckets, err := svc.rebuildBucketsForStartup(context.Background())
	require.NoError(t, err)
	require.Contains(t, buckets, registered)
	require.Contains(t, buckets, SchedulerBucket{GroupID: 7, Platform: PlatformOpenAI, Mode: SchedulerModeSingle})
	require.Contains(t, buckets, SchedulerBucket{GroupID: 0, Platform: PlatformOpenAI, Mode: SchedulerModeSingle})
}

func TestSchedulerSnapshotService_DefaultBucketsReturnsGroupDiscoveryError(t *testing.T) {
	wantErr := errors.New("group discovery failed")
	svc := newSchedulerSnapshotService(&staticStateCache{}, nil, nil, nil, nil, staticStateGroupRepo{err: wantErr}, &config.Config{})

	_, err := svc.defaultBuckets(context.Background())
	require.ErrorIs(t, err, wantErr)
}
