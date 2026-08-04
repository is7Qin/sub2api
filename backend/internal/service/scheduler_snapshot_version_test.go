//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// versionedSnapshotCache 实现 SchedulerCache + snapshotVersionReader（可选接口），
// 用于验证解码缓存按 Redis active version 失效。
type versionedSnapshotCache struct {
	SchedulerCache
	snapshot         []*Account
	version          string
	versionErr       error
	getSnapshotCalls int
	versionCalls     int
	batchReadCalls   int
	batchAccounts    map[int64]*Account
	activeAccounts   map[int64]*Account
}

func (c *versionedSnapshotCache) GetSnapshot(context.Context, SchedulerBucket) ([]*Account, bool, error) {
	c.getSnapshotCalls++
	c.activeAccounts = make(map[int64]*Account, len(c.snapshot))
	for _, candidate := range c.snapshot {
		if candidate != nil {
			c.activeAccounts[candidate.ID] = candidate
		}
	}
	return c.snapshot, true, nil
}

func (c *versionedSnapshotCache) GetSnapshotVersion(context.Context, SchedulerBucket) (string, error) {
	c.versionCalls++
	return c.version, c.versionErr
}

func (c *versionedSnapshotCache) GetStaticCandidateAccount(context.Context, SchedulerBucket, int64) (*Account, error) {
	return nil, nil
}

func (c *versionedSnapshotCache) GetStaticCandidateAccountsByIDs(_ context.Context, _ SchedulerBucket, ids []int64) (map[int64]*Account, error) {
	c.batchReadCalls++
	if c.batchAccounts != nil {
		return c.batchAccounts, nil
	}
	staticAccounts := c.activeAccounts
	if staticAccounts == nil {
		staticAccounts = make(map[int64]*Account, len(c.snapshot))
		for _, candidate := range c.snapshot {
			if candidate != nil {
				staticAccounts[candidate.ID] = candidate
			}
		}
	}
	accounts := make(map[int64]*Account, len(ids))
	for _, id := range ids {
		if candidate := staticAccounts[id]; candidate != nil {
			accounts[id] = candidate
		}
	}
	return accounts, nil
}

// ttlOnlySnapshotCache 不实现 snapshotVersionReader：验证退化路径与现状一致（TTL 兜底）。
type ttlOnlySnapshotCache struct {
	SchedulerCache
	snapshot         []*Account
	getSnapshotCalls int
}

func (c *ttlOnlySnapshotCache) GetSnapshot(context.Context, SchedulerBucket) ([]*Account, bool, error) {
	c.getSnapshotCalls++
	return c.snapshot, true, nil
}

// TestSchedulerSnapshotDecodeCache_InvalidatedOnVersionChange 验证 rebuild 后
// active version 变化会立即失效本地解码缓存，不再返回旧账号集合。
func TestSchedulerSnapshotDecodeCache_InvalidatedOnVersionChange(t *testing.T) {
	cache := &versionedSnapshotCache{
		snapshot: []*Account{{ID: 1, Platform: PlatformOpenAI}},
		version:  "v1",
	}
	svc := &SchedulerSnapshotService{cache: cache}
	ctx := context.Background()

	accounts, _, err := svc.ListSchedulableAccounts(ctx, nil, PlatformOpenAI, false)
	require.NoError(t, err)
	require.Len(t, accounts, 1)
	require.Equal(t, int64(1), accounts[0].ID)
	require.Equal(t, 1, cache.getSnapshotCalls)

	// 调度器重建快照：active version 递增，解码缓存必须失效。
	cache.snapshot = []*Account{{ID: 2, Platform: PlatformOpenAI}}
	cache.version = "v2"

	accounts, _, err = svc.ListSchedulableAccounts(ctx, nil, PlatformOpenAI, false)
	require.NoError(t, err)
	require.Len(t, accounts, 1)
	require.Equal(t, int64(2), accounts[0].ID) // 返回新快照而非旧账号集合
	require.Equal(t, 2, cache.getSnapshotCalls)
}

// TestSchedulerSnapshotDecodeCache_ReusedWhenVersionUnchanged 验证版本未变时
// TTL 内的条目命中解码缓存，不重复调用 GetSnapshot。
func TestSchedulerSnapshotDecodeCache_FirstFillAppliesCurrentStaticCandidateState(t *testing.T) {
	cache := &versionedSnapshotCache{
		snapshot: []*Account{{ID: 1, Platform: PlatformOpenAI, Schedulable: true}},
		version:  "v1",
		batchAccounts: map[int64]*Account{
			1: {ID: 1, Platform: PlatformOpenAI, Schedulable: false},
		},
	}
	svc := &SchedulerSnapshotService{cache: cache}

	accounts, _, err := svc.ListSchedulableAccounts(context.Background(), nil, PlatformOpenAI, false)
	require.NoError(t, err)
	require.Len(t, accounts, 1)
	require.False(t, accounts[0].Schedulable)
	require.Equal(t, 1, cache.batchReadCalls)
}

func TestSchedulerSnapshotDecodeCache_ReusedWhenVersionUnchanged(t *testing.T) {
	cache := &versionedSnapshotCache{
		snapshot: []*Account{{ID: 1, Platform: PlatformOpenAI}},
		version:  "v1",
	}
	svc := &SchedulerSnapshotService{cache: cache}
	ctx := context.Background()

	_, _, err := svc.ListSchedulableAccounts(ctx, nil, PlatformOpenAI, false)
	require.NoError(t, err)
	_, _, err = svc.ListSchedulableAccounts(ctx, nil, PlatformOpenAI, false)
	require.NoError(t, err)
	require.Equal(t, 1, cache.getSnapshotCalls)
}

// TestSchedulerSnapshotDecodeCache_VersionedEntryLongerTTL 验证版本可用时条目
// 使用更长的保留 TTL（版本失效为主，TTL 只是兜底）。
func TestSchedulerSnapshotDecodeCache_VersionedEntryLongerTTL(t *testing.T) {
	cache := &versionedSnapshotCache{
		snapshot: []*Account{{ID: 1, Platform: PlatformOpenAI}},
		version:  "v1",
	}
	svc := &SchedulerSnapshotService{cache: cache}
	ctx := context.Background()

	_, _, err := svc.ListSchedulableAccounts(ctx, nil, PlatformOpenAI, false)
	require.NoError(t, err)

	raw, ok := svc.decodeCache.Load("0:openai:single")
	require.True(t, ok)
	entry, ok := raw.(*snapshotDecodeCacheEntry)
	require.True(t, ok)
	require.Equal(t, "v1", entry.version)
	require.WithinDuration(t, time.Now().Add(snapshotDecodeVersionedTTL), entry.exp, time.Second)
}

// TestSchedulerSnapshotDecodeCache_FallsBackToTTLWithoutVersionReader 验证
// cache 未实现可选版本接口时行为与现状一致：TTL 兜底复用。
func TestSchedulerSnapshotDecodeCache_FallsBackToTTLWithoutVersionReader(t *testing.T) {
	cache := &ttlOnlySnapshotCache{
		snapshot: []*Account{{ID: 1, Platform: PlatformOpenAI}},
	}
	svc := &SchedulerSnapshotService{cache: cache}
	ctx := context.Background()

	_, _, err := svc.ListSchedulableAccounts(ctx, nil, PlatformOpenAI, false)
	require.NoError(t, err)
	_, _, err = svc.ListSchedulableAccounts(ctx, nil, PlatformOpenAI, false)
	require.NoError(t, err)
	require.Equal(t, 1, cache.getSnapshotCalls)

	raw, ok := svc.decodeCache.Load("0:openai:single")
	require.True(t, ok)
	entry, ok := raw.(*snapshotDecodeCacheEntry)
	require.True(t, ok)
	require.Equal(t, "", entry.version)
	require.WithinDuration(t, time.Now().Add(snapshotDecodeCacheTTL), entry.exp, time.Second)
}

// TestSchedulerSnapshotDecodeCache_FallsBackToTTLOnVersionReadError 验证版本
// 读取失败时退化为 TTL 兜底（5 秒），不 panic。
func TestSchedulerSnapshotDecodeCache_FallsBackToTTLOnVersionReadError(t *testing.T) {
	cache := &versionedSnapshotCache{
		snapshot:   []*Account{{ID: 1, Platform: PlatformOpenAI}},
		version:    "v1",
		versionErr: errors.New("redis down"),
	}
	svc := &SchedulerSnapshotService{cache: cache}
	ctx := context.Background()

	_, _, err := svc.ListSchedulableAccounts(ctx, nil, PlatformOpenAI, false)
	require.NoError(t, err)
	_, _, err = svc.ListSchedulableAccounts(ctx, nil, PlatformOpenAI, false)
	require.NoError(t, err)
	require.Equal(t, 1, cache.getSnapshotCalls)

	raw, ok := svc.decodeCache.Load("0:openai:single")
	require.True(t, ok)
	entry, ok := raw.(*snapshotDecodeCacheEntry)
	require.True(t, ok)
	require.Equal(t, "", entry.version)
	require.WithinDuration(t, time.Now().Add(snapshotDecodeCacheTTL), entry.exp, time.Second)
}

// TestSchedulerSnapshotDecodeCache_VersionedEntryNotServedOnVersionReadError
// 验证版本读取失败时，带版本号的解码缓存条目不得被命中：否则重建后的新
// 快照会被 30s TTL 内的旧条目顶掉（旧账号继续派发、新账号不可见）。
func TestSchedulerSnapshotDecodeCache_VersionedEntryNotServedOnVersionReadError(t *testing.T) {
	cache := &versionedSnapshotCache{
		snapshot: []*Account{{ID: 1, Platform: PlatformOpenAI}},
		version:  "v1",
	}
	svc := &SchedulerSnapshotService{cache: cache}
	ctx := context.Background()

	// 第一轮：版本可用，存入带版本号 "v1" 的条目（30s TTL）。
	accounts, _, err := svc.ListSchedulableAccounts(ctx, nil, PlatformOpenAI, false)
	require.NoError(t, err)
	require.Len(t, accounts, 1)
	require.Equal(t, int64(1), accounts[0].ID)
	require.Equal(t, 1, cache.getSnapshotCalls)

	// 快照重建后版本读取瞬时失败（version == ""）：带版本号的旧条目
	// 不得命中，必须重新 GetSnapshot 并返回新账号集合。
	cache.snapshot = []*Account{{ID: 2, Platform: PlatformOpenAI}}
	cache.versionErr = errors.New("redis down")

	accounts, _, err = svc.ListSchedulableAccounts(ctx, nil, PlatformOpenAI, false)
	require.NoError(t, err)
	require.Equal(t, 2, cache.getSnapshotCalls)
	require.Len(t, accounts, 1)
	require.Equal(t, int64(2), accounts[0].ID)
}

// TestSchedulerSnapshotDecodeCache_ExpiredEntryRefetches 验证即使版本一致，
// 超过保留 TTL 的条目也必须重新 GetSnapshot。
func TestSchedulerSnapshotDecodeCache_ExpiredEntryRefetches(t *testing.T) {
	cache := &versionedSnapshotCache{
		snapshot: []*Account{{ID: 1, Platform: PlatformOpenAI}},
		version:  "v1",
	}
	svc := &SchedulerSnapshotService{cache: cache}
	ctx := context.Background()

	// 手工塞入已过期条目：版本一致也不能命中。
	svc.decodeCache.Store("0:openai:single", &snapshotDecodeCacheEntry{
		accounts: []*Account{{ID: 99}},
		version:  "v1",
		exp:      time.Now().Add(-time.Second),
	})

	accounts, _, err := svc.ListSchedulableAccounts(ctx, nil, PlatformOpenAI, false)
	require.NoError(t, err)
	require.Equal(t, 1, cache.getSnapshotCalls)
	require.Len(t, accounts, 1)
	require.Equal(t, int64(1), accounts[0].ID)
}

// TestSchedulerSnapshotDecodeCache_VersionGetSkippedOnHit 验证解码缓存命中时
// 不再向 Redis 读取激活版本：本地版本缓存窗口内命中为零 Redis 往返（版本 GET
// 只在首次调用发生），这是选择路径每个请求省掉一次版本 GET 的核心。
func TestSchedulerSnapshotDecodeCache_VersionGetSkippedOnHit(t *testing.T) {
	cache := &versionedSnapshotCache{
		snapshot: []*Account{{ID: 1, Platform: PlatformOpenAI}},
		version:  "v1",
	}
	// 打开版本本地缓存窗口（生产构造经由 NewSchedulerSnapshotService 默认 1s）。
	svc := &SchedulerSnapshotService{
		cache:                   cache,
		snapshotVersionCacheTTL: snapshotVersionCacheWindow,
	}
	ctx := context.Background()

	_, _, err := svc.ListSchedulableAccounts(ctx, nil, PlatformOpenAI, false)
	require.NoError(t, err)
	_, _, err = svc.ListSchedulableAccounts(ctx, nil, PlatformOpenAI, false)
	require.NoError(t, err)

	// 第二次调用命中解码缓存 + 本地版本缓存：不读 Redis 版本，也不再 GetSnapshot。
	require.Equal(t, 1, cache.versionCalls, "解码缓存命中时不得再读 Redis 激活版本")
	require.Equal(t, 1, cache.getSnapshotCalls)
}

// TestSchedulerSnapshotDecodeCache_VersionChangeDetectedAfterWindow 验证本地
// 版本缓存窗口过期后能感知重建导致的版本变化并失效解码缓存（失效延迟 ≤ 窗口；
// 窗口内命中旧条目由 snapshotDecodeVersionedTTL 兜底，符合版本缓存设计的
// 有界陈旧性）。
func TestSchedulerSnapshotDecodeCache_VersionChangeDetectedAfterWindow(t *testing.T) {
	cache := &versionedSnapshotCache{
		snapshot: []*Account{{ID: 1, Platform: PlatformOpenAI}},
		version:  "v1",
	}
	svc := &SchedulerSnapshotService{
		cache:                   cache,
		snapshotVersionCacheTTL: 10 * time.Millisecond,
	}
	ctx := context.Background()

	_, _, err := svc.ListSchedulableAccounts(ctx, nil, PlatformOpenAI, false)
	require.NoError(t, err)
	require.Equal(t, 1, cache.getSnapshotCalls)

	// 窗口内版本变化：命中旧条目（有界陈旧性由窗口兜底，可接受）。
	cache.snapshot = []*Account{{ID: 2, Platform: PlatformOpenAI}}
	cache.version = "v2"
	accounts, _, err := svc.ListSchedulableAccounts(ctx, nil, PlatformOpenAI, false)
	require.NoError(t, err)
	require.Equal(t, int64(1), accounts[0].ID)
	require.Equal(t, 1, cache.getSnapshotCalls)

	// 窗口过期：重新读版本发现变化，解码缓存失效并返回新快照。
	time.Sleep(50 * time.Millisecond)
	accounts, _, err = svc.ListSchedulableAccounts(ctx, nil, PlatformOpenAI, false)
	require.NoError(t, err)
	require.Equal(t, int64(2), accounts[0].ID)
	require.Equal(t, 2, cache.getSnapshotCalls)
}
