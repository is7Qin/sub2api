//go:build unit

package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// missingSnapshotFallbackCache 快照恒 miss，记录 SetSnapshot 写回次数。
type missingSnapshotFallbackCache struct {
	SchedulerCache
	snapshotWrites int
}

func (c *missingSnapshotFallbackCache) GetSnapshot(context.Context, SchedulerBucket) ([]*Account, bool, error) {
	return nil, false, nil
}

func (c *missingSnapshotFallbackCache) SetSnapshot(context.Context, SchedulerBucket, []Account) error {
	c.snapshotWrites++
	return nil
}

// staticStateFallbackCache models the production static-cache capability: legacy
// SetSnapshot changes only candidate state, leaving the support version untouched.
type staticStateFallbackCache struct {
	snapshotHydrationCache

	candidateVersion string
	supportVersion   string
	snapshotWrites   int
}

func (c *staticStateFallbackCache) GetSnapshot(context.Context, SchedulerBucket) ([]*Account, bool, error) {
	return nil, false, nil
}

func (c *staticStateFallbackCache) SetSnapshot(context.Context, SchedulerBucket, []Account) error {
	c.snapshotWrites++
	c.candidateVersion = "legacy"
	return nil
}

func (c *staticStateFallbackCache) SetStaticState(context.Context, SchedulerBucket, []Account, []Account) error {
	c.candidateVersion = "static"
	c.supportVersion = "static"
	return nil
}

func (c *staticStateFallbackCache) GetPersistentSupport(context.Context, SchedulerBucket) ([]*Account, bool, error) {
	return nil, c.candidateVersion == c.supportVersion, nil
}

// slowGatedFallbackAccountRepo 回源查询先停留 fallbackQueryHold 再返回：让同一
// 瞬间放行的所有并发调用都能加入 leader 的单飞组（确定性验证合并，而不是依赖
// 时序碰运气）。前 failures 次查询报错。
type slowGatedFallbackAccountRepo struct {
	AccountRepository
	queryCalls        int
	failures          int
	fallbackQueryHold time.Duration
}

func (r *slowGatedFallbackAccountRepo) ListSchedulableUngroupedByPlatform(context.Context, string) ([]Account, error) {
	r.queryCalls++
	time.Sleep(r.fallbackQueryHold)
	if r.failures > 0 {
		r.failures--
		return nil, errors.New("db down")
	}
	return []Account{{ID: 1, Platform: PlatformOpenAI, Schedulable: true}}, nil
}

func newSlowGatedFallbackTestService() (*SchedulerSnapshotService, *missingSnapshotFallbackCache, *slowGatedFallbackAccountRepo) {
	cache := &missingSnapshotFallbackCache{}
	repo := &slowGatedFallbackAccountRepo{fallbackQueryHold: 200 * time.Millisecond}
	return &SchedulerSnapshotService{cache: cache, accountRepo: repo}, cache, repo
}

// TestSchedulerSnapshotFallback_SingleflightMergesConcurrentMisses 验证快照 miss
// 时并发请求按分桶合并为一次 DB 回源 + 一次 SetSnapshot 写回，其余请求等待
// 同一结果——快照 miss 风暴下不再每个请求各自回源 DB。
func TestSchedulerSnapshotFallback_StaticCachePreservesCoherentState(t *testing.T) {
	cache := &staticStateFallbackCache{candidateVersion: "static", supportVersion: "static"}
	repo := &slowGatedFallbackAccountRepo{}
	svc := &SchedulerSnapshotService{cache: cache, accountRepo: repo}

	accounts, _, err := svc.ListSchedulableAccounts(context.Background(), nil, PlatformOpenAI, false)

	require.NoError(t, err)
	require.Equal(t, []Account{{ID: 1, Platform: PlatformOpenAI, Schedulable: true}}, accounts)
	require.Equal(t, "static", cache.candidateVersion)
	require.Equal(t, "static", cache.supportVersion)
	require.Zero(t, cache.snapshotWrites)
}

func TestSchedulerSnapshotFallback_SingleflightMergesConcurrentMisses(t *testing.T) {
	svc, cache, repo := newSlowGatedFallbackTestService()
	ctx := context.Background()

	const concurrency = 8
	var wg sync.WaitGroup
	results := make([]error, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := svc.ListSchedulableAccounts(ctx, nil, PlatformOpenAI, false)
			results[i] = err
		}(i)
	}
	wg.Wait()

	for i := range results {
		require.NoError(t, results[i], "并发等待者必须共享 leader 的回源结果")
	}
	require.Equal(t, 1, repo.queryCalls, "并发 miss 必须合并为一次 DB 回源")
	require.Equal(t, 1, cache.snapshotWrites, "并发 miss 必须只写回一次快照")
}

// TestSchedulerSnapshotFallback_LeaderErrorSharedAndRecovered 验证回源失败时
// 同一单飞组的所有等待者共享 leader 的错误；失败不残留，下一次调用正常回源。
func TestSchedulerSnapshotFallback_LeaderErrorSharedAndRecovered(t *testing.T) {
	svc, _, repo := newSlowGatedFallbackTestService()
	repo.failures = 8
	ctx := context.Background()

	const concurrency = 8
	var wg sync.WaitGroup
	results := make([]error, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := svc.ListSchedulableAccounts(ctx, nil, PlatformOpenAI, false)
			results[i] = err
		}(i)
	}
	wg.Wait()

	for i := range results {
		require.Error(t, results[i], "回源失败时同一单飞组的等待者应共享错误")
	}
	require.Equal(t, 1, repo.queryCalls, "同一单飞组只执行一次回源查询")

	// 失败不残留：下一次调用正常回源。
	repo.failures = 0
	_, _, err := svc.ListSchedulableAccounts(ctx, nil, PlatformOpenAI, false)
	require.NoError(t, err)
}
