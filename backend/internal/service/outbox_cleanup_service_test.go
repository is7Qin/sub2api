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
	cleanupCalls atomic.Int32
	deleted      int64
	lastCutoff   time.Time
}

func (s *outboxCleanupBillingRepoStub) CleanupTerminal(_ context.Context, cutoff time.Time, _ int) (int64, error) {
	s.cleanupCalls.Add(1)
	s.lastCutoff = cutoff
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
	billing := &outboxCleanupBillingRepoStub{deleted: 5000}
	svc := NewOutboxCleanupService(billing, nil, nil)
	svc.SetLeaderLock(&fakeLeaderLockCache{}, nil)

	before := time.Now()
	require.NoError(t, svc.Run(context.Background()))

	require.Equal(t, int32(outboxCleanupMaxBatches), billing.cleanupCalls.Load(),
		"full batches must continue until fewer than batchSize rows remain")
	require.WithinDuration(t, before.Add(-outboxTerminalRetention), billing.lastCutoff, time.Second,
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
	svc := NewOutboxCleanupService(nil, scheduler, cache)
	svc.SetLeaderLock(&fakeLeaderLockCache{}, nil)

	require.NoError(t, svc.Run(context.Background()))

	require.Equal(t, int32(1), scheduler.cleanupCalls.Load())
	require.Equal(t, int64(123456), scheduler.lastWm)
}

func TestOutboxCleanupService_SkipsSchedulerWhenWatermarkMissing(t *testing.T) {
	scheduler := &outboxCleanupSchedulerRepoStub{deleted: 3}
	cache := &outboxCleanupCacheStub{watermark: 0}
	svc := NewOutboxCleanupService(nil, scheduler, cache)
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

	peer := NewOutboxCleanupService(billing, nil, nil)
	peer.SetLeaderLock(lock, nil)
	require.NoError(t, peer.Run(context.Background()))
	require.Equal(t, int32(0), billing.cleanupCalls.Load(), "non-leader must not run cleanup")
}

func TestOutboxCleanupService_IntervalAndAdapter(t *testing.T) {
	svc := NewOutboxCleanupService(nil, nil, nil)
	require.Equal(t, outboxCleanupInterval, svc.Interval())

	job, err := NewOutboxCleanupWorker(svc)
	require.NoError(t, err)
	require.Equal(t, "outbox-cleanup", job.Descriptor().Name)
	require.Equal(t, workerruntime.CoordinationSingletonRun, job.Descriptor().CoordinationMode)
}
