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
}

func (c *versionedSnapshotCache) GetSnapshot(context.Context, SchedulerBucket) ([]*Account, bool, error) {
	c.getSnapshotCalls++
	return c.snapshot, true, nil
}

func (c *versionedSnapshotCache) GetSnapshotVersion(context.Context, SchedulerBucket) (string, error) {
	c.versionCalls++
	return c.version, c.versionErr
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
