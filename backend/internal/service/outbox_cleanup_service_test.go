package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/workerruntime"
	"github.com/stretchr/testify/require"
)

type outboxCleanupBillingRepoStub struct {
	BillingOutboxRepository
	cleanupCalls  atomic.Int32
	deleted       int64
	lastCutoff    time.Time
	lastBatchSize int
}

func (s *outboxCleanupBillingRepoStub) CleanupTerminal(_ context.Context, cutoff time.Time, batchSize int) (int64, error) {
	s.cleanupCalls.Add(1)
	s.lastCutoff = cutoff
	s.lastBatchSize = batchSize
	return s.deleted, nil
}

type outboxCleanupSchedulerRepoStub struct {
	SchedulerOutboxRepository
	cleanupCalls atomic.Int32
	deleted      int64
	lastWm       int64
}

func (s *outboxCleanupSchedulerRepoStub) CleanupConsumed(_ context.Context, watermark int64, _ int) (int64, error) {
	s.cleanupCalls.Add(1)
	s.lastWm = watermark
	return s.deleted, nil
}

type outboxCleanupCacheStub struct {
	SchedulerCache
	watermark int64
}

func (s *outboxCleanupCacheStub) GetOutboxWatermark(ctx context.Context) (int64, error) {
	return s.watermark, nil
}

func TestOutboxCleanupService_LeaderCleansBillingInBatches(t *testing.T) {
	billing := &outboxCleanupBillingRepoStub{deleted: outboxCleanupBatchSize}
	svc := NewOutboxCleanupService(billing, nil, nil, 30*24*time.Hour)
	svc.SetLeaderLock(&fakeLeaderLockCache{}, nil)

	before := time.Now()
	require.NoError(t, svc.Run(context.Background()))

	require.Equal(t, int32(outboxCleanupMaxBatches), billing.cleanupCalls.Load(),
		"full batches must continue until fewer than batchSize rows remain")
	require.WithinDuration(t, before.Add(-30*24*time.Hour), billing.lastCutoff, time.Second,
		"cutoff must be now minus the retention window")

	// 剩余不足一批时提前结束
	billing.cleanupCalls.Store(0)
	billing.deleted = 3
	require.NoError(t, svc.Run(context.Background()))
	require.Equal(t, int32(1), billing.cleanupCalls.Load())
}

func TestOutboxCleanupService_LeaderCleansSchedulerConsumedRows(t *testing.T) {
	scheduler := &outboxCleanupSchedulerRepoStub{deleted: 3}
	cache := &outboxCleanupCacheStub{watermark: 123456}
	svc := NewOutboxCleanupService(nil, scheduler, cache, 30*24*time.Hour)
	svc.SetLeaderLock(&fakeLeaderLockCache{}, nil)

	require.NoError(t, svc.Run(context.Background()))

	require.Equal(t, int32(1), scheduler.cleanupCalls.Load())
	require.Equal(t, int64(123456), scheduler.lastWm)
}

func TestOutboxCleanupService_SkipsSchedulerWhenWatermarkMissing(t *testing.T) {
	scheduler := &outboxCleanupSchedulerRepoStub{deleted: 3}
	cache := &outboxCleanupCacheStub{watermark: 0}
	svc := NewOutboxCleanupService(nil, scheduler, cache, 30*24*time.Hour)
	svc.SetLeaderLock(&fakeLeaderLockCache{}, nil)

	require.NoError(t, svc.Run(context.Background()))

	require.Equal(t, int32(0), scheduler.cleanupCalls.Load(), "watermark <= 0 must skip cleanup")
}

func TestOutboxCleanupService_NonLeaderSkipsCleanup(t *testing.T) {
	billing := &outboxCleanupBillingRepoStub{deleted: 3}
	lock := &fakeLeaderLockCache{}
	// 手动让另一副本持有 leader 锁，模拟多副本场景
	held, err := lock.TryAcquireLeaderLock(context.Background(), outboxCleanupLeaderLockKey, "other-replica", time.Minute)
	require.NoError(t, err)
	require.True(t, held)

	peer := NewOutboxCleanupService(billing, nil, nil, 30*24*time.Hour)
	peer.SetLeaderLock(lock, nil)
	require.NoError(t, peer.Run(context.Background()))
	require.Equal(t, int32(0), billing.cleanupCalls.Load(), "non-leader must not run cleanup")
}

func TestOutboxCleanupService_IntervalAndAdapter(t *testing.T) {
	svc := NewOutboxCleanupService(nil, nil, nil, 30*24*time.Hour)
	require.Equal(t, outboxCleanupInterval, svc.Interval())

	job, err := NewOutboxCleanupWorker(svc)
	require.NoError(t, err)
	require.Equal(t, "outbox-cleanup", job.Descriptor().Name)
	require.Equal(t, workerruntime.CoordinationSingletonRun, job.Descriptor().CoordinationMode)
}

// TestOutboxCleanupService_TuningSustainsDesignPointIngest 是 push 型参数锁：
// 终态清理的稳态删除率必须 ≥ 10K/s 摄入设计点（Task C 吞吐模型：3 实例 × ~3.1K/s），
// 否则表在 30 天保留期下无界增长。锁定三个不变量：
//  1. 每轮删除容量 ≥ 一个周期内摄入的行数（名义速率含裕量）；
//  2. 最坏情况（每轮都吃满周期超时）下周期容量 / (周期 + 超时) 仍 ≥ 摄入率；
//  3. 周期超时 < leader 锁 TTL（避免锁到期后仍有清理在跑）。
//
// 保留期默认 30 天（outbox_cleanup.terminal_retention_days，Task D 恢复窗口）
// 不得随本任务缩短，由 config.go 默认值维持，这里不做断言。
func TestOutboxCleanupService_TuningSustainsDesignPointIngest(t *testing.T) {
	// Task C 定义的 usage 摄入设计点（行/s）。
	const ingestDesignPointRowsPerSecond = 10_000

	capacityPerCycle := outboxCleanupBatchSize * outboxCleanupMaxBatches
	require.GreaterOrEqual(t, capacityPerCycle,
		ingestDesignPointRowsPerSecond*int(outboxCleanupInterval/time.Second),
		"每轮删除容量必须 ≥ 一个周期内的摄入行数")

	// 最坏情况：run 每轮都吃满周期超时（fixed-delay runtime 下周期 = interval + run）。
	worstCasePeriod := outboxCleanupInterval + outboxCleanupCycleTimeout
	require.GreaterOrEqual(t, float64(capacityPerCycle)/worstCasePeriod.Seconds(),
		float64(ingestDesignPointRowsPerSecond),
		"最坏情况稳态删除率必须 ≥ 摄入率")

	require.Less(t, outboxCleanupCycleTimeout, outboxCleanupLockTTL,
		"周期超时必须小于 leader 锁 TTL")
}

// TestOutboxCleanupService_BatchSizeFlowsToRepo 锁定清理批大小透传到 repo 调用。
func TestOutboxCleanupService_BatchSizeFlowsToRepo(t *testing.T) {
	billing := &outboxCleanupBillingRepoStub{deleted: 3}
	svc := NewOutboxCleanupService(billing, nil, nil, 30*24*time.Hour)
	svc.SetLeaderLock(&fakeLeaderLockCache{}, nil)

	require.NoError(t, svc.Run(context.Background()))

	require.Equal(t, int32(1), billing.cleanupCalls.Load())
	require.Equal(t, outboxCleanupBatchSize, billing.lastBatchSize,
		"CleanupTerminal 必须收到配置的批大小")
}
