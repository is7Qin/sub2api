//go:build unit

package repository

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// schedulerSnapshotGetFailureHook 注入第一次 GET 命令失败，模拟 Redis 读错误。
type schedulerSnapshotGetFailureHook struct {
	calls atomic.Int32
}

func (h *schedulerSnapshotGetFailureHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *schedulerSnapshotGetFailureHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "get" && h.calls.Add(1) == 1 {
			return errors.New("injected snapshot get failure")
		}
		return next(ctx, cmd)
	}
}

func (h *schedulerSnapshotGetFailureHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// capturingSlogHandler 捕获 slog 记录，用于断言汇总日志内容。
type capturingSlogHandler struct {
	records []slog.Record
}

func (h *capturingSlogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *capturingSlogHandler) Handle(_ context.Context, record slog.Record) error {
	h.records = append(h.records, record.Clone())
	return nil
}

func (h *capturingSlogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingSlogHandler) WithGroup(string) slog.Handler      { return h }

func recordAttrs(record slog.Record) map[string]any {
	out := make(map[string]any)
	record.Attrs(func(attr slog.Attr) bool {
		out[attr.Key] = attr.Value.Any()
		return true
	})
	return out
}

func TestSchedulerCacheGetSnapshotStats_MissReasonCounters(t *testing.T) {
	ctx := context.Background()
	readyKey := func(bucket service.SchedulerBucket) string { return schedulerBucketKey(schedulerReadyPrefix, bucket) }
	activeKey := func(bucket service.SchedulerBucket) string { return schedulerBucketKey(schedulerActivePrefix, bucket) }
	snapshotKey := func(bucket service.SchedulerBucket) string { return schedulerSnapshotKey(bucket, "1") }

	cases := []struct {
		name    string
		prepare func(t *testing.T, cache *schedulerCache, bucket service.SchedulerBucket)
		reason  schedulerSnapshotMissReason
	}{
		{
			name: "ready key missing",
			prepare: func(t *testing.T, cache *schedulerCache, bucket service.SchedulerBucket) {
			},
			reason: schedulerMissNotReady,
		},
		{
			name: "ready value not 1",
			prepare: func(t *testing.T, cache *schedulerCache, bucket service.SchedulerBucket) {
				require.NoError(t, cache.rdb.Set(ctx, readyKey(bucket), "0", 0).Err())
			},
			reason: schedulerMissNotReady,
		},
		{
			name: "active key missing",
			prepare: func(t *testing.T, cache *schedulerCache, bucket service.SchedulerBucket) {
				require.NoError(t, cache.rdb.Set(ctx, readyKey(bucket), "1", 0).Err())
			},
			reason: schedulerMissActiveMissing,
		},
		{
			name: "snapshot zset missing or empty",
			prepare: func(t *testing.T, cache *schedulerCache, bucket service.SchedulerBucket) {
				require.NoError(t, cache.rdb.Set(ctx, readyKey(bucket), "1", 0).Err())
				require.NoError(t, cache.rdb.Set(ctx, activeKey(bucket), "1", 0).Err())
			},
			reason: schedulerMissSnapshotEmpty,
		},
		{
			name: "account meta value nil",
			prepare: func(t *testing.T, cache *schedulerCache, bucket service.SchedulerBucket) {
				require.NoError(t, cache.rdb.Set(ctx, readyKey(bucket), "1", 0).Err())
				require.NoError(t, cache.rdb.Set(ctx, activeKey(bucket), "1", 0).Err())
				require.NoError(t, cache.rdb.ZAdd(ctx, snapshotKey(bucket), redis.Z{Score: 0, Member: "901"}).Err())
			},
			reason: schedulerMissMetaMissing,
		},
		{
			name: "account decode error",
			prepare: func(t *testing.T, cache *schedulerCache, bucket service.SchedulerBucket) {
				require.NoError(t, cache.rdb.Set(ctx, readyKey(bucket), "1", 0).Err())
				require.NoError(t, cache.rdb.Set(ctx, activeKey(bucket), "1", 0).Err())
				require.NoError(t, cache.rdb.ZAdd(ctx, snapshotKey(bucket), redis.Z{Score: 0, Member: "901"}).Err())
				require.NoError(t, cache.rdb.Set(ctx, schedulerAccountMetaKey("901"), "{not-json", 0).Err())
			},
			reason: schedulerMissDecodeError,
		},
		{
			name: "last used malformed",
			prepare: func(t *testing.T, cache *schedulerCache, bucket service.SchedulerBucket) {
				require.NoError(t, cache.rdb.Set(ctx, readyKey(bucket), "1", 0).Err())
				require.NoError(t, cache.rdb.Set(ctx, activeKey(bucket), "1", 0).Err())
				require.NoError(t, cache.rdb.ZAdd(ctx, snapshotKey(bucket), redis.Z{Score: 0, Member: "901"}).Err())
				require.NoError(t, cache.rdb.Set(ctx, schedulerAccountMetaKey("901"), `{"ID":901}`, 0).Err())
				require.NoError(t, cache.rdb.Set(ctx, schedulerLastUsedKey("901"), "not-a-millis", 0).Err())
			},
			reason: schedulerMissLastUsedError,
		},
		{
			name: "redis error on ready read",
			prepare: func(t *testing.T, cache *schedulerCache, bucket service.SchedulerBucket) {
				cache.rdb.AddHook(&schedulerSnapshotGetFailureHook{})
			},
			reason: schedulerMissRedisError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cache := newSchedulerCacheUnit(t)
			bucket := service.SchedulerBucket{GroupID: 31, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
			tc.prepare(t, cache, bucket)

			snapshot, hit, err := cache.GetSnapshot(ctx, bucket)
			require.False(t, hit)
			require.Nil(t, snapshot)
			// decode_error/last_used_error/redis_error 分支按原语义返回错误，其余分支静默未命中。
			if tc.reason == schedulerMissDecodeError || tc.reason == schedulerMissLastUsedError || tc.reason == schedulerMissRedisError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			summary := cache.stats.summarize()
			require.Equal(t, uint64(1), summary.TotalMisses, "rolling total misses")
			require.Zero(t, summary.TotalHits, "rolling total hits")
			require.Equal(t, uint64(1), summary.WindowMisses, "window misses")
			require.Zero(t, summary.WindowHits, "window hits")
			require.Len(t, summary.Buckets, 1)
			require.Equal(t, bucket.String(), summary.Buckets[0].Bucket)
			require.Equal(t, uint64(1), summary.Buckets[0].Misses)
			require.Equal(t, map[schedulerSnapshotMissReason]uint64{tc.reason: 1}, summary.Buckets[0].Reasons)
		})
	}
}

func TestSchedulerCacheGetSnapshotStats_HitCounts(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	bucket := service.SchedulerBucket{GroupID: 32, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
	require.NoError(t, cache.SetSnapshot(ctx, bucket, []service.Account{
		{ID: 902, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey},
	}))

	snapshot, hit, err := cache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	require.True(t, hit)
	require.Len(t, snapshot, 1)

	summary := cache.stats.summarize()
	require.Equal(t, uint64(1), summary.TotalHits)
	require.Zero(t, summary.TotalMisses)
	require.Equal(t, uint64(1), summary.WindowHits)
	require.Len(t, summary.Buckets, 1)
	require.Equal(t, uint64(1), summary.Buckets[0].Hits)
	require.Zero(t, summary.Buckets[0].Misses)
	require.Empty(t, summary.Buckets[0].Reasons)
}

func TestSchedulerCacheSnapshotStats_SummarizeResetsWindowKeepsTotals(t *testing.T) {
	stats := newSchedulerSnapshotStats()
	stats.recordMiss("1:openai:single", schedulerMissNotReady)
	stats.recordMiss("1:openai:single", schedulerMissActiveMissing)
	stats.recordHit("1:openai:single")

	first := stats.summarize()
	require.Equal(t, uint64(2), first.TotalMisses)
	require.Equal(t, uint64(1), first.TotalHits)
	require.Equal(t, uint64(2), first.WindowMisses)
	require.Equal(t, uint64(1), first.WindowHits)
	require.Len(t, first.Buckets, 1)
	require.Equal(t, uint64(2), first.Buckets[0].Misses)
	require.Equal(t, map[schedulerSnapshotMissReason]uint64{
		schedulerMissNotReady:      1,
		schedulerMissActiveMissing: 1,
	}, first.Buckets[0].Reasons)

	// 汇总后窗口计数归零，滚动总量保留；无活动分桶不再出现在输出中。
	second := stats.summarize()
	require.Zero(t, second.WindowMisses)
	require.Zero(t, second.WindowHits)
	require.Equal(t, uint64(2), second.TotalMisses)
	require.Equal(t, uint64(1), second.TotalHits)
	require.Empty(t, second.Buckets)
}

func TestSchedulerCacheSnapshotStats_SummarizeCapsBucketsByMisses(t *testing.T) {
	stats := newSchedulerSnapshotStats()
	for i := 0; i < 25; i++ {
		key := fmt.Sprintf("%d:openai:single", i)
		for j := 0; j <= i; j++ {
			stats.recordMiss(key, schedulerMissNotReady)
		}
	}

	summary := stats.summarize()
	require.Len(t, summary.Buckets, schedulerSnapshotStatsLogBucketLimit)
	// 第 i 个桶有 i+1 次未命中（i=0..24），按未命中次数降序截断到前 20。
	require.Equal(t, "24:openai:single", summary.Buckets[0].Bucket)
	require.Equal(t, uint64(25), summary.Buckets[0].Misses)
	require.Equal(t, "5:openai:single", summary.Buckets[len(summary.Buckets)-1].Bucket)
	require.Equal(t, uint64(6), summary.Buckets[len(summary.Buckets)-1].Misses)
}

func TestSchedulerCacheLogSnapshotStats_EmitsSummaryWithCounts(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	bucket := service.SchedulerBucket{GroupID: 33, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}

	// 一次未命中（ready 缺失）+ 一次命中。
	_, hit, err := cache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	require.False(t, hit)
	require.NoError(t, cache.SetSnapshot(ctx, bucket, []service.Account{
		{ID: 903, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey},
	}))
	_, hit, err = cache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	require.True(t, hit)

	handler := &capturingSlogHandler{}
	previous := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(previous)

	cache.LogSnapshotStats()

	require.Len(t, handler.records, 1)
	attrs := recordAttrs(handler.records[0])
	require.Equal(t, "scheduler.cache", attrs["component"])
	require.Equal(t, uint64(1), attrs["window_misses"])
	require.Equal(t, uint64(1), attrs["window_hits"])
	require.Equal(t, uint64(1), attrs["total_misses"])
	require.Equal(t, uint64(1), attrs["total_hits"])
	buckets, ok := attrs["buckets"].([]schedulerSnapshotBucketSummary)
	require.True(t, ok)
	require.Len(t, buckets, 1)
	require.Equal(t, bucket.String(), buckets[0].Bucket)
	require.Equal(t, uint64(1), buckets[0].Misses)
	require.Equal(t, map[schedulerSnapshotMissReason]uint64{schedulerMissNotReady: 1}, buckets[0].Reasons)
}

func TestSchedulerCacheLogSnapshotStats_SkipsEmptyWindow(t *testing.T) {
	cache := newSchedulerCacheUnit(t)

	handler := &capturingSlogHandler{}
	previous := slog.Default()
	slog.SetDefault(slog.New(handler))
	defer slog.SetDefault(previous)

	cache.LogSnapshotStats()

	require.Empty(t, handler.records, "空窗口不应输出日志")
}
