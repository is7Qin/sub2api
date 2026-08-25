//go:build unit

package repository

import (
	"context"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestSchedulerCacheUpdateLastUsedUsesSideKeyWithoutRewritingPayloads(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	bucket := service.SchedulerBucket{GroupID: 9, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
	initial := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Hour)
	account := service.Account{
		ID: 9201, Name: "grok-large-oauth", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Status: service.StatusActive, Schedulable: true, LastUsedAt: &initial,
		Credentials: map[string]any{"access_token": strings.Repeat("a", 4096)},
		Extra:       map[string]any{"large": strings.Repeat("x", 4096)},
	}
	require.NoError(t, cache.SetSnapshot(ctx, bucket, []service.Account{account}))

	id := strconv.FormatInt(account.ID, 10)
	fullBefore, err := cache.rdb.Get(ctx, schedulerAccountKey(id)).Bytes()
	require.NoError(t, err)
	metaBefore, err := cache.rdb.Get(ctx, schedulerAccountMetaKey(id)).Bytes()
	require.NoError(t, err)

	latest := initial.Add(37 * time.Second)
	require.NoError(t, cache.UpdateLastUsed(ctx, map[int64]time.Time{account.ID: latest}))

	fullAfter, err := cache.rdb.Get(ctx, schedulerAccountKey(id)).Bytes()
	require.NoError(t, err)
	metaAfter, err := cache.rdb.Get(ctx, schedulerAccountMetaKey(id)).Bytes()
	require.NoError(t, err)
	require.Equal(t, fullBefore, fullAfter)
	require.Equal(t, metaBefore, metaAfter)
	require.Equal(t, strconv.FormatInt(latest.UnixMilli(), 10), cache.rdb.Get(ctx, schedulerLastUsedKey(id)).Val())

	cached, err := cache.GetAccount(ctx, account.ID)
	require.NoError(t, err)
	require.Equal(t, latest, *cached.LastUsedAt)
	snapshot, hit, err := cache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	require.True(t, hit)
	require.Equal(t, latest, *snapshot[0].LastUsedAt)
}

func TestSchedulerCacheLastUsedSideKeyIsMonotonicAndRequiresAccount(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	account := service.Account{ID: 9202, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth}
	require.NoError(t, cache.SetAccount(ctx, &account))

	newer := time.Now().UTC().Truncate(time.Millisecond)
	older := newer.Add(-time.Minute)
	require.NoError(t, cache.UpdateLastUsed(ctx, map[int64]time.Time{account.ID: newer}))
	require.NoError(t, cache.UpdateLastUsed(ctx, map[int64]time.Time{account.ID: older}))

	id := strconv.FormatInt(account.ID, 10)
	require.Equal(t, strconv.FormatInt(newer.UnixMilli(), 10), cache.rdb.Get(ctx, schedulerLastUsedKey(id)).Val())
	ttl := cache.rdb.TTL(ctx, schedulerLastUsedKey(id)).Val()
	require.Greater(t, ttl, 0*time.Second)
	require.LessOrEqual(t, ttl, time.Duration(schedulerLastUsedTTLSeconds)*time.Second)
	cached, err := cache.GetAccount(ctx, account.ID)
	require.NoError(t, err)
	require.Equal(t, newer, *cached.LastUsedAt)

	const missingID int64 = 9299
	require.NoError(t, cache.UpdateLastUsed(ctx, map[int64]time.Time{missingID: newer}))
	_, err = cache.rdb.Get(ctx, schedulerLastUsedKey(strconv.FormatInt(missingID, 10))).Result()
	require.ErrorIs(t, err, redis.Nil)

	require.NoError(t, cache.DeleteAccount(ctx, account.ID))
	_, err = cache.rdb.Get(ctx, schedulerLastUsedKey(id)).Result()
	require.ErrorIs(t, err, redis.Nil)
}

func TestSchedulerCacheLastUsedSideKeyHasBoundedLegacyOrphanLifetime(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	account := service.Account{ID: 9206, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth}
	require.NoError(t, cache.SetAccount(ctx, &account))
	require.NoError(t, cache.UpdateLastUsed(ctx, map[int64]time.Time{account.ID: time.Now().UTC()}))

	id := strconv.FormatInt(account.ID, 10)
	key := schedulerLastUsedKey(id)
	ttl := cache.rdb.TTL(ctx, key).Val()
	require.Greater(t, ttl, 0*time.Second)
	require.LessOrEqual(t, ttl, time.Duration(schedulerLastUsedTTLSeconds)*time.Second)

	// A pre-side-key binary deletes only full/meta. The additive side key can remain,
	// but its TTL bounds the orphan lifetime after a mixed-version rollout.
	require.NoError(t, cache.rdb.Del(ctx, schedulerAccountKey(id), schedulerAccountMetaKey(id)).Err())
	require.Equal(t, int64(1), cache.rdb.Exists(ctx, key).Val())
	require.Greater(t, cache.rdb.TTL(ctx, key).Val(), 0*time.Second)
}

func TestSchedulerCacheLastUsedSideKeyFallsBackToNewerEmbeddedValue(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	bucket := service.SchedulerBucket{GroupID: 11, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
	embedded := time.Now().UTC().Truncate(time.Millisecond)
	account := service.Account{ID: 9203, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, LastUsedAt: &embedded}
	require.NoError(t, cache.SetSnapshot(ctx, bucket, []service.Account{account}))

	id := strconv.FormatInt(account.ID, 10)
	require.NoError(t, cache.rdb.Set(ctx, schedulerLastUsedKey(id), embedded.Add(-time.Hour).UnixMilli(), 0).Err())
	cached, err := cache.GetAccount(ctx, account.ID)
	require.NoError(t, err)
	require.Equal(t, embedded, *cached.LastUsedAt)
	snapshot, hit, err := cache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	require.True(t, hit)
	require.Equal(t, embedded, *snapshot[0].LastUsedAt)
}

func TestSchedulerCacheMalformedLastUsedSideKeyReturnsTypedReadError(t *testing.T) {
	cases := map[string]string{
		"invalid syntax": "not-millis",
		"maximum int64":  strconv.FormatInt(math.MaxInt64, 10),
		"minimum int64":  strconv.FormatInt(math.MinInt64, 10),
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			cache := newSchedulerCacheUnit(t)
			account := service.Account{ID: 9205, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth}
			bucket := service.SchedulerBucket{GroupID: 12, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
			require.NoError(t, cache.SetSnapshot(ctx, bucket, []service.Account{account}))
			require.NoError(t, cache.rdb.Set(ctx, schedulerLastUsedKey(strconv.FormatInt(account.ID, 10)), value, 0).Err())

			_, err := cache.GetAccount(ctx, account.ID)
			require.ErrorIs(t, err, errSchedulerLastUsedCacheMalformed)
			_, _, err = cache.GetSnapshot(ctx, bucket)
			require.ErrorIs(t, err, errSchedulerLastUsedCacheMalformed)
		})
	}
}

func TestSchedulerCacheLastUsedSideKeySurvivesStaleAccountAndSnapshotWrites(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	bucket := service.SchedulerBucket{GroupID: 10, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
	embedded := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Minute)
	latest := embedded.Add(30 * time.Second)
	account := service.Account{ID: 9204, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Schedulable: true, LastUsedAt: &embedded}
	require.NoError(t, cache.SetAccount(ctx, &account))
	require.NoError(t, cache.UpdateLastUsed(ctx, map[int64]time.Time{account.ID: latest}))
	require.NoError(t, cache.SetAccount(ctx, &account))
	require.NoError(t, cache.SetSnapshot(ctx, bucket, []service.Account{account}))

	id := strconv.FormatInt(account.ID, 10)
	require.Equal(t, strconv.FormatInt(latest.UnixMilli(), 10), cache.rdb.Get(ctx, schedulerLastUsedKey(id)).Val())
	cached, err := cache.GetAccount(ctx, account.ID)
	require.NoError(t, err)
	require.Equal(t, latest, *cached.LastUsedAt)
}

func TestSchedulerCacheUpdateLastUsedChunksLargeBatches(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	total := schedulerLastUsedUpdateChunkSize + 1
	accounts := make([]service.Account, 0, total)
	updates := make(map[int64]time.Time, total)
	base := time.Now().UTC().Truncate(time.Millisecond)
	for i := 0; i < total; i++ {
		id := int64(9300 + i)
		accounts = append(accounts, service.Account{ID: id, Platform: service.PlatformOpenAI})
		updates[id] = base.Add(time.Duration(i) * time.Millisecond)
	}

	written, err := cache.writeAccountIDs(ctx, accounts)
	require.NoError(t, err)
	require.Len(t, written, total)
	require.NoError(t, cache.UpdateLastUsed(ctx, updates))
	for id, usedAt := range updates {
		key := schedulerLastUsedKey(strconv.FormatInt(id, 10))
		require.Equal(t, strconv.FormatInt(usedAt.UnixMilli(), 10), cache.rdb.Get(ctx, key).Val())
	}
}

type schedulerLastUsedPipelineHook struct {
	pipelineCalls       atomic.Int32
	maxPipelineCommands atomic.Int32
}

func (h *schedulerLastUsedPipelineHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *schedulerLastUsedPipelineHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return next
}

func (h *schedulerLastUsedPipelineHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		h.pipelineCalls.Add(1)
		for {
			current := h.maxPipelineCommands.Load()
			if int32(len(cmds)) <= current || h.maxPipelineCommands.CompareAndSwap(current, int32(len(cmds))) {
				break
			}
		}
		return next(ctx, cmds)
	}
}

func TestSchedulerCacheUpdateLastUsedDoesNotAccumulatePipelineChunks(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	hook := &schedulerLastUsedPipelineHook{}
	cache.rdb.AddHook(hook)

	total := schedulerLastUsedUpdateChunkSize*2 + 1
	accounts := make([]service.Account, 0, total)
	updates := make(map[int64]time.Time, total)
	usedAt := time.Now().UTC().Truncate(time.Millisecond)
	for i := 0; i < total; i++ {
		id := int64(9400 + i)
		accounts = append(accounts, service.Account{ID: id, Platform: service.PlatformOpenAI})
		updates[id] = usedAt
	}
	_, err := cache.writeAccountIDs(ctx, accounts)
	require.NoError(t, err)
	hook.pipelineCalls.Store(0)
	hook.maxPipelineCommands.Store(0)

	require.NoError(t, cache.UpdateLastUsed(ctx, updates))
	require.Equal(t, int32(3), hook.pipelineCalls.Load())
	require.Equal(t, int32(1), hook.maxPipelineCommands.Load())
}

func TestSchedulerCacheUpdateLastUsedReturnsPipelineError(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	account := service.Account{ID: 9501, Platform: service.PlatformOpenAI}
	require.NoError(t, cache.SetAccount(ctx, &account))
	require.NoError(t, cache.rdb.Close())

	err := cache.UpdateLastUsed(ctx, map[int64]time.Time{account.ID: time.Now().UTC()})
	require.Error(t, err)
}

func TestSchedulerCacheLastUsedDeleteUpdateRacesNeverLeaveOrphan(t *testing.T) {
	ctx := context.Background()
	for _, deleteFirst := range []bool{false, true} {
		t.Run(strconv.FormatBool(deleteFirst), func(t *testing.T) {
			cache := newSchedulerCacheUnit(t)
			account := service.Account{ID: 9601, Platform: service.PlatformOpenAI}
			require.NoError(t, cache.SetAccount(ctx, &account))
			usedAt := time.Now().UTC()
			if deleteFirst {
				require.NoError(t, cache.DeleteAccount(ctx, account.ID))
				require.NoError(t, cache.UpdateLastUsed(ctx, map[int64]time.Time{account.ID: usedAt}))
			} else {
				require.NoError(t, cache.UpdateLastUsed(ctx, map[int64]time.Time{account.ID: usedAt}))
				require.NoError(t, cache.DeleteAccount(ctx, account.ID))
			}
			id := strconv.FormatInt(account.ID, 10)
			require.Zero(t, cache.rdb.Exists(ctx, schedulerAccountKey(id), schedulerAccountMetaKey(id), schedulerLastUsedKey(id)).Val())
		})
	}
}
