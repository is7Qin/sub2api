//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/workerruntime"
	"github.com/stretchr/testify/require"
)

func TestUsageRecordWorkerPoolWorkerStartDoesNotOverwriteStopping(t *testing.T) {
	pool := NewUsageRecordWorkerPoolWithOptions(UsageRecordWorkerPoolOptions{
		WorkerCount:      1,
		QueueSize:        1,
		AutoScaleEnabled: false,
	})
	worker := NewUsageRecordWorkerPoolWorker(pool).(*UsageRecordWorkerPoolWorker)
	poolStarted := make(chan struct{})
	releaseStart := make(chan struct{})
	worker.testHooks = &usageRecordWorkerPoolWorkerTestHooks{
		afterPoolStart: func() {
			close(poolStarted)
			<-releaseStart
		},
	}

	startErr := make(chan error, 1)
	go func() { startErr <- worker.Start(context.Background()) }()
	<-poolStarted

	block := make(chan struct{})
	started := make(chan struct{})
	require.Equal(t, UsageRecordSubmitModeEnqueued, pool.Submit(func(context.Context) {
		close(started)
		<-block
	}))
	<-started
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, worker.Stop(stopCtx), context.DeadlineExceeded)
	require.Equal(t, workerruntime.LifecycleStopping, worker.Snapshot().Lifecycle.State)

	close(releaseStart)
	require.Error(t, <-startErr)
	require.Equal(t, workerruntime.LifecycleStopping, worker.Snapshot().Lifecycle.State)
	close(block)
	require.NoError(t, worker.Stop(context.Background()))
}

func TestUsageRecordWorkerPoolWorkerStartDoesNotOverwriteStoppedAfterConcurrentStop(t *testing.T) {
	pool := NewUsageRecordWorkerPoolWithOptions(UsageRecordWorkerPoolOptions{
		WorkerCount:      1,
		QueueSize:        1,
		AutoScaleEnabled: false,
	})
	worker := NewUsageRecordWorkerPoolWorker(pool).(*UsageRecordWorkerPoolWorker)
	poolStarted := make(chan struct{})
	releaseStart := make(chan struct{})
	worker.testHooks = &usageRecordWorkerPoolWorkerTestHooks{
		afterPoolStart: func() {
			close(poolStarted)
			<-releaseStart
		},
	}

	startErr := make(chan error, 1)
	go func() { startErr <- worker.Start(context.Background()) }()
	<-poolStarted
	require.NoError(t, worker.Stop(context.Background()))
	require.Equal(t, workerruntime.LifecycleStopped, worker.Snapshot().Lifecycle.State)

	close(releaseStart)
	require.Error(t, <-startErr)
	require.Equal(t, workerruntime.LifecycleStopped, worker.Snapshot().Lifecycle.State)
}

func TestUsageRecordWorkerPoolWorkerStopBeforeStartStopsNativePool(t *testing.T) {
	pool := NewUsageRecordWorkerPoolWithOptions(UsageRecordWorkerPoolOptions{
		WorkerCount:      1,
		QueueSize:        1,
		AutoScaleEnabled: false,
	})
	worker := NewUsageRecordWorkerPoolWorker(pool)

	require.NoError(t, worker.Stop(context.Background()))
	require.True(t, pool.pool.Stopped())
	require.False(t, pool.Accepting())
	require.Error(t, worker.Start(context.Background()))
}

func TestUsageRecordWorkerPoolWorkerStopDeadlineKeepsStoppingSnapshot(t *testing.T) {
	pool := NewUsageRecordWorkerPoolWithOptions(UsageRecordWorkerPoolOptions{
		WorkerCount:      1,
		QueueSize:        1,
		AutoScaleEnabled: false,
	})
	worker := NewUsageRecordWorkerPoolWorker(pool)
	require.NoError(t, worker.Start(context.Background()))

	block := make(chan struct{})
	started := make(chan struct{})
	require.Equal(t, UsageRecordSubmitModeEnqueued, pool.Submit(func(context.Context) {
		close(started)
		<-block
	}))
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, worker.Stop(ctx), context.DeadlineExceeded)

	snapshot := worker.Snapshot()
	require.Equal(t, workerruntime.LifecycleStopping, snapshot.Lifecycle.State)
	status, ok := snapshot.Status.(workerruntime.PoolStatus)
	require.True(t, ok)
	require.True(t, status.StillRunning)

	close(block)
	require.NoError(t, worker.Stop(context.Background()))
}

func TestUsageRecordWorkerPoolWorkerStopReturnsSuccessWhenNativeStopCompletesAtDeadline(t *testing.T) {
	pool := NewUsageRecordWorkerPoolWithOptions(UsageRecordWorkerPoolOptions{
		WorkerCount:      1,
		QueueSize:        1,
		AutoScaleEnabled: false,
	})
	worker := NewUsageRecordWorkerPoolWorker(pool).(*UsageRecordWorkerPoolWorker)
	require.NoError(t, worker.Start(context.Background()))

	block := make(chan struct{})
	started := make(chan struct{})
	require.Equal(t, UsageRecordSubmitModeEnqueued, pool.Submit(func(context.Context) {
		close(started)
		<-block
	}))
	<-started

	nativeStopComplete := make(chan struct{})
	releaseCompletion := make(chan struct{})
	worker.testHooks = &usageRecordWorkerPoolWorkerTestHooks{
		afterPoolStop: func() {
			close(nativeStopComplete)
			<-releaseCompletion
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopErr := make(chan error, 1)
	go func() { stopErr <- worker.Stop(ctx) }()
	require.Eventually(t, func() bool {
		return worker.Snapshot().Lifecycle.State == workerruntime.LifecycleStopping
	}, time.Second, time.Millisecond)

	close(block)
	<-nativeStopComplete
	cancel()
	require.NoError(t, <-stopErr)
	close(releaseCompletion)
}

func TestUsageRecordWorkerPoolWorkerStopReturnsSuccessAfterCompletionWithExpiredContext(t *testing.T) {
	pool := NewUsageRecordWorkerPoolWithOptions(UsageRecordWorkerPoolOptions{
		WorkerCount:      1,
		QueueSize:        1,
		AutoScaleEnabled: false,
	})
	worker := NewUsageRecordWorkerPoolWorker(pool)
	require.NoError(t, worker.Start(context.Background()))
	require.NoError(t, worker.Stop(context.Background()))

	ctx, cancel := context.WithDeadline(context.Background(), time.Now())
	defer cancel()
	require.NoError(t, worker.Stop(ctx))
	require.Equal(t, workerruntime.LifecycleStopped, worker.Snapshot().Lifecycle.State)
}

func TestAccountExpiryWorkerPreservesImmediateRuntimeSpec(t *testing.T) {
	worker, err := NewAccountExpiryWorker(NewAccountExpiryService(&accountExpiryRepoStub{autoPauseFn: func(context.Context, time.Time) (int64, error) {
		return 0, nil
	}}, time.Minute))

	require.NoError(t, err)
	snapshot := worker.Snapshot()
	require.Equal(t, "account-expiry", snapshot.Descriptor.Name)
	require.Equal(t, workerruntime.KindPeriodic, snapshot.Descriptor.Kind)
	require.Equal(t, "maintenance", snapshot.Descriptor.Group)
	require.Equal(t, workerruntime.CoordinationPerInstance, snapshot.Descriptor.CoordinationMode)
}

func TestIdempotencyCleanupWorkerPreservesImmediateRuntimeSpec(t *testing.T) {
	worker, err := NewIdempotencyCleanupWorker(NewIdempotencyCleanupService(&idempotencyCleanupRepoStub{}, &config.Config{}))

	require.NoError(t, err)
	snapshot := worker.Snapshot()
	require.Equal(t, "idempotency-cleanup", snapshot.Descriptor.Name)
	require.Equal(t, workerruntime.KindPeriodic, snapshot.Descriptor.Kind)
	require.Equal(t, "maintenance", snapshot.Descriptor.Group)
	require.Equal(t, workerruntime.CoordinationPerInstance, snapshot.Descriptor.CoordinationMode)
}

func TestAccountExpiryWorkerRunsImmediately(t *testing.T) {
	called := make(chan struct{}, 1)
	worker, err := NewAccountExpiryWorker(NewAccountExpiryService(&accountExpiryRepoStub{autoPauseFn: func(context.Context, time.Time) (int64, error) {
		called <- struct{}{}
		return 0, nil
	}}, time.Hour))
	require.NoError(t, err)
	require.NoError(t, worker.Start(context.Background()))
	t.Cleanup(func() { require.NoError(t, worker.Stop(context.Background())) })

	require.Eventually(t, func() bool { return len(called) == 1 }, time.Second, time.Millisecond)
}

func TestIdempotencyCleanupWorkerRunsImmediately(t *testing.T) {
	called := make(chan struct{}, 1)
	cfg := &config.Config{}
	cfg.Idempotency.CleanupIntervalSeconds = int(time.Hour / time.Second)
	worker, err := NewIdempotencyCleanupWorker(NewIdempotencyCleanupService(&idempotencyCleanupRepoStub{deleteFn: func(context.Context, time.Time, int) (int64, error) {
		called <- struct{}{}
		return 0, nil
	}}, cfg))
	require.NoError(t, err)
	require.NoError(t, worker.Start(context.Background()))
	t.Cleanup(func() { require.NoError(t, worker.Stop(context.Background())) })

	require.Eventually(t, func() bool { return len(called) == 1 }, time.Second, time.Millisecond)
}

func TestAccountExpiryWorkerCreationDoesNotStartWork(t *testing.T) {
	called := make(chan struct{}, 1)
	svc := NewAccountExpiryService(&accountExpiryRepoStub{autoPauseFn: func(context.Context, time.Time) (int64, error) {
		called <- struct{}{}
		return 0, nil
	}}, time.Millisecond)
	_, err := NewAccountExpiryWorker(svc)
	require.NoError(t, err)

	select {
	case <-called:
		t.Fatal("adapter creation started account expiry work")
	case <-time.After(25 * time.Millisecond):
	}
}

func TestIdempotencyCleanupWorkerCreationDoesNotStartWork(t *testing.T) {
	called := make(chan struct{}, 1)
	svc := NewIdempotencyCleanupService(&idempotencyCleanupRepoStub{deleteFn: func(context.Context, time.Time, int) (int64, error) {
		called <- struct{}{}
		return 0, nil
	}}, nil)
	_, err := NewIdempotencyCleanupWorker(svc)
	require.NoError(t, err)

	select {
	case <-called:
		t.Fatal("adapter creation started idempotency cleanup work")
	case <-time.After(25 * time.Millisecond):
	}
}
