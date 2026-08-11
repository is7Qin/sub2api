//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/workerruntime"
	"github.com/stretchr/testify/require"
)

func newOpsSystemLogSinkWorkerForTest(t *testing.T, repo OpsRepository) (*OpsSystemLogSinkWorker, *OpsSystemLogSink) {
	t.Helper()
	sink := NewOpsSystemLogSink(repo)
	sink.batchSize = 200
	sink.flushInterval = time.Hour
	worker, err := NewOpsSystemLogSinkWorker(sink)
	require.NoError(t, err)
	return worker, sink
}

func opsSystemLogEvent(message string) *logger.LogEvent {
	return &logger.LogEvent{Time: time.Now().UTC(), Level: "warn", Component: "app", Message: message, Fields: map[string]any{}}
}

func TestOpsSystemLogSinkWorkerDescriptorAndInitialStatusAreDetached(t *testing.T) {
	worker, _ := newOpsSystemLogSinkWorkerForTest(t, &opsRepoMock{})
	want := workerruntime.Descriptor{Name: "ops-system-log-sink", Kind: workerruntime.KindPool, Group: "ops", CoordinationMode: workerruntime.CoordinationPerInstance, Description: "Persists buffered operational log events", Tags: []string{"ops", "system-logs", "ingestion"}}
	require.Equal(t, want, worker.Descriptor())
	snapshot := worker.Snapshot()
	require.Equal(t, want, snapshot.Descriptor)
	require.Equal(t, workerruntime.LifecycleStopped, snapshot.Lifecycle.State)
	status, ok := snapshot.Status.(workerruntime.PoolStatus)
	require.True(t, ok)
	require.False(t, status.Accepting)
	require.False(t, status.StillRunning)
	require.Equal(t, 1, status.MaxConcurrency)
	require.Zero(t, status.RunningWorkers)

	detached := worker.Descriptor()
	detached.Tags[0] = "mutated"
	require.Equal(t, want.Tags, worker.Descriptor().Tags)
}

func TestOpsSystemLogSinkWorkerRejectsMissingSinkOrRepository(t *testing.T) {
	for _, sink := range []*OpsSystemLogSink{nil, NewOpsSystemLogSink(nil)} {
		worker, err := NewOpsSystemLogSinkWorker(sink)
		require.Nil(t, worker)
		require.EqualError(t, err, "Ops system log sink is required")
	}
}

func TestOpsSystemLogSinkWorkerRejectsTypedNilRepository(t *testing.T) {
	var repo *opsRepoMock
	worker, err := NewOpsSystemLogSinkWorker(NewOpsSystemLogSink(repo))
	require.Nil(t, worker)
	require.EqualError(t, err, "Ops system log sink is required")
}

func TestOpsSystemLogSinkWorkerPreCanceledStartDoesNotLaunchConsumer(t *testing.T) {
	var calls atomic.Int64
	worker, sink := newOpsSystemLogSinkWorkerForTest(t, &opsRepoMock{BatchInsertSystemLogsFn: func(context.Context, []*OpsInsertSystemLogInput) (int64, error) {
		calls.Add(1)
		return 1, nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, worker.Start(ctx), context.Canceled)
	sink.WriteLogEvent(opsSystemLogEvent("buffered-before-valid-start"))
	require.Equal(t, 1, len(sink.queue))
	require.Zero(t, calls.Load())
	require.Equal(t, workerruntime.LifecycleStopped, worker.Snapshot().Lifecycle.State)

	require.NoError(t, worker.Start(context.Background()))
	require.NoError(t, worker.Stop(context.Background()))
	require.Equal(t, int64(1), calls.Load())
}

func TestOpsSystemLogSinkWorkerStartIsIdempotentAndPersistsEligibleEvent(t *testing.T) {
	persisted := make(chan []*OpsInsertSystemLogInput, 2)
	worker, sink := newOpsSystemLogSinkWorkerForTest(t, &opsRepoMock{BatchInsertSystemLogsFn: func(_ context.Context, inputs []*OpsInsertSystemLogInput) (int64, error) {
		persisted <- inputs
		return int64(len(inputs)), nil
	}})
	sink.batchSize = 1
	require.NoError(t, worker.Start(context.Background()))
	require.NoError(t, worker.Start(context.Background()))
	snapshot := worker.Snapshot()
	require.Equal(t, workerruntime.LifecycleRunning, snapshot.Lifecycle.State)
	status := snapshot.Status.(workerruntime.PoolStatus)
	require.True(t, status.Accepting)
	require.True(t, status.StillRunning)
	require.Equal(t, int64(1), status.RunningWorkers)

	sink.WriteLogEvent(opsSystemLogEvent("persist-me"))
	select {
	case inputs := <-persisted:
		require.Len(t, inputs, 1)
	case <-time.After(time.Second):
		t.Fatal("eligible event was not persisted")
	}
	require.NoError(t, worker.Stop(context.Background()))
	select {
	case <-persisted:
		t.Fatal("idempotent Start launched a second consumer")
	default:
	}
}

func TestOpsSystemLogSinkWorkerConcurrentStartCallsNativeStartExactlyOnce(t *testing.T) {
	worker, _ := newOpsSystemLogSinkWorkerForTest(t, &opsRepoMock{})
	var calls atomic.Int64
	worker.testHooks = &opsSystemLogSinkWorkerTestHooks{beforeNativeStart: func() { calls.Add(1) }}

	const callers = 32
	start := make(chan struct{})
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- worker.Start(context.Background())
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, int64(1), calls.Load())
	require.NoError(t, worker.Stop(context.Background()))
}

func TestOpsSystemLogSinkWorkerMapsTruthfulCountersWithoutDiagnosticContent(t *testing.T) {
	worker, sink := newOpsSystemLogSinkWorkerForTest(t, &opsRepoMock{})
	sink.queue = make(chan *logger.LogEvent, 1)
	sink.lastError.Store("repository-secret-error")
	atomic.StoreUint64(&sink.writtenCount, 4)
	atomic.StoreUint64(&sink.writeFailed, 3)
	sink.WriteLogEvent(opsSystemLogEvent("private-event-content"))
	sink.WriteLogEvent(opsSystemLogEvent("queue-full-private-content"))

	snapshot := worker.Snapshot()
	status := snapshot.Status.(workerruntime.PoolStatus)
	require.Equal(t, uint64(1), status.WaitingTasks)
	require.Equal(t, uint64(4), status.SuccessfulTasks)
	require.Equal(t, uint64(3), status.FailedTasks)
	require.Equal(t, uint64(7), status.CompletedTasks)
	require.Equal(t, uint64(1), status.DroppedTasks)
	require.Equal(t, uint64(1), status.DroppedQueueFull)
	require.Zero(t, status.DroppedPoolStopped)
	require.Zero(t, status.SubmittedTasks)
	require.Zero(t, status.SyncFallbackTasks)
	projection := fmt.Sprintf("%+v", snapshot)
	require.NotContains(t, projection, "repository-secret-error")
	require.NotContains(t, projection, "private-event-content")
}

func TestOpsSystemLogSinkWorkerNormalStopDrainsPrivateBatchAndQueue(t *testing.T) {
	firstReceived := make(chan struct{})
	releaseFirst := make(chan struct{})
	persisted := make(chan int, 2)
	var calls atomic.Int64
	worker, sink := newOpsSystemLogSinkWorkerForTest(t, &opsRepoMock{BatchInsertSystemLogsFn: func(_ context.Context, inputs []*OpsInsertSystemLogInput) (int64, error) {
		if calls.Add(1) == 1 {
			close(firstReceived)
			<-releaseFirst
		}
		persisted <- len(inputs)
		return int64(len(inputs)), nil
	}})
	sink.batchSize = 2
	require.NoError(t, worker.Start(context.Background()))
	sink.WriteLogEvent(opsSystemLogEvent("private-batch"))
	sink.WriteLogEvent(opsSystemLogEvent("triggers-flush"))
	<-firstReceived
	sink.WriteLogEvent(opsSystemLogEvent("queued-during-flush"))
	close(releaseFirst)
	require.NoError(t, worker.Stop(context.Background()))
	require.Equal(t, 2, <-persisted)
	require.Equal(t, 1, <-persisted)
	snapshot := worker.Snapshot()
	require.Equal(t, workerruntime.LifecycleStopped, snapshot.Lifecycle.State)
	status := snapshot.Status.(workerruntime.PoolStatus)
	require.False(t, status.StillRunning)
	require.False(t, status.Accepting)
	require.Zero(t, status.RunningWorkers)
	require.Equal(t, uint64(3), status.SuccessfulTasks)
}

func TestOpsSystemLogSinkWorkerConcurrentStopSharesNativeCompletion(t *testing.T) {
	flushEntered := make(chan struct{})
	releaseFlush := make(chan struct{})
	worker, sink := newOpsSystemLogSinkWorkerForTest(t, &opsRepoMock{BatchInsertSystemLogsFn: func(_ context.Context, inputs []*OpsInsertSystemLogInput) (int64, error) {
		close(flushEntered)
		<-releaseFlush
		return int64(len(inputs)), nil
	}})
	var calls atomic.Int64
	worker.testHooks = &opsSystemLogSinkWorkerTestHooks{beforeNativeStop: func() { calls.Add(1) }}
	require.NoError(t, worker.Start(context.Background()))
	sink.WriteLogEvent(opsSystemLogEvent("drain-once"))

	const callers = 32
	start := make(chan struct{})
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- worker.Stop(context.Background())
		}()
	}
	close(start)
	<-flushEntered
	close(releaseFlush)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.NoError(t, worker.Stop(context.Background()))
	require.Equal(t, int64(1), calls.Load())
}

func TestOpsSystemLogSinkWorkerStopDeadlineRemainsTruthfullyStopping(t *testing.T) {
	flushEntered := make(chan struct{})
	releaseFlush := make(chan struct{})
	worker, sink := newOpsSystemLogSinkWorkerForTest(t, &opsRepoMock{BatchInsertSystemLogsFn: func(_ context.Context, inputs []*OpsInsertSystemLogInput) (int64, error) {
		close(flushEntered)
		<-releaseFlush
		return int64(len(inputs)), nil
	}})
	require.NoError(t, worker.Start(context.Background()))
	sink.WriteLogEvent(opsSystemLogEvent("blocked-final-flush-content"))
	ctx, cancel := context.WithCancel(context.Background())
	stopErr := make(chan error, 1)
	go func() { stopErr <- worker.Stop(ctx) }()
	<-flushEntered
	cancel()
	require.ErrorIs(t, <-stopErr, context.Canceled)
	snapshot := worker.Snapshot()
	require.Equal(t, workerruntime.LifecycleStopping, snapshot.Lifecycle.State)
	status := snapshot.Status.(workerruntime.PoolStatus)
	require.True(t, status.StillRunning)
	require.False(t, status.Accepting)
	require.Equal(t, int64(1), status.RunningWorkers)

	close(releaseFlush)
	require.NoError(t, worker.Stop(context.Background()))
	snapshot = worker.Snapshot()
	require.Equal(t, workerruntime.LifecycleStopped, snapshot.Lifecycle.State)
	status = snapshot.Status.(workerruntime.PoolStatus)
	require.False(t, status.StillRunning)
	require.Zero(t, status.RunningWorkers)
	require.NotContains(t, fmt.Sprintf("%+v", snapshot), "blocked-final-flush-content")
}

func TestOpsSystemLogSinkWorkerRejectsStartAfterStoppingBegins(t *testing.T) {
	worker, _ := newOpsSystemLogSinkWorkerForTest(t, &opsRepoMock{})
	stopEntered := make(chan struct{})
	releaseStop := make(chan struct{})
	worker.testHooks = &opsSystemLogSinkWorkerTestHooks{
		beforeNativeStop: func() {
			close(stopEntered)
			<-releaseStop
		},
	}

	require.NoError(t, worker.Start(context.Background()))
	stopErr := make(chan error, 1)
	go func() { stopErr <- worker.Stop(context.Background()) }()
	<-stopEntered
	require.EqualError(t, worker.Start(context.Background()), "Ops system log sink is stopping")
	close(releaseStop)
	require.NoError(t, <-stopErr)
}

func TestOpsSystemLogSinkWorkerRejectsRestartAfterNativeStop(t *testing.T) {
	worker, _ := newOpsSystemLogSinkWorkerForTest(t, &opsRepoMock{})
	var starts atomic.Int64
	worker.testHooks = &opsSystemLogSinkWorkerTestHooks{
		beforeNativeStart: func() { starts.Add(1) },
	}

	require.NoError(t, worker.Start(context.Background()))
	require.NoError(t, worker.Stop(context.Background()))
	require.EqualError(t, worker.Start(context.Background()), "Ops system log sink is non-restartable after Stop")
	require.Equal(t, int64(1), starts.Load())
	snapshot := worker.Snapshot()
	require.Equal(t, workerruntime.LifecycleStopped, snapshot.Lifecycle.State)
	status := snapshot.Status.(workerruntime.PoolStatus)
	require.False(t, status.Accepting)
	require.False(t, status.StillRunning)
	require.Zero(t, status.RunningWorkers)
}

func TestOpsSystemLogSinkWorkerStopBeforeStartPreservesFutureStart(t *testing.T) {
	persisted := make(chan struct{}, 1)
	worker, sink := newOpsSystemLogSinkWorkerForTest(t, &opsRepoMock{BatchInsertSystemLogsFn: func(_ context.Context, inputs []*OpsInsertSystemLogInput) (int64, error) {
		persisted <- struct{}{}
		return int64(len(inputs)), nil
	}})
	var starts, stops atomic.Int64
	worker.testHooks = &opsSystemLogSinkWorkerTestHooks{
		beforeNativeStart: func() { starts.Add(1) },
		beforeNativeStop:  func() { stops.Add(1) },
	}
	require.NoError(t, worker.Stop(context.Background()))
	require.Zero(t, stops.Load())
	require.Equal(t, workerruntime.LifecycleStopped, worker.Snapshot().Lifecycle.State)
	staleNotifier := worker.StopInitiated()
	select {
	case <-staleNotifier:
	default:
		t.Fatal("never-started Stop did not close its notifier generation")
	}

	sink.WriteLogEvent(opsSystemLogEvent("buffered-after-noop-stop"))
	require.Equal(t, 1, len(sink.queue))
	require.NoError(t, worker.Start(context.Background()))
	require.Equal(t, int64(1), starts.Load())
	activeNotifier := worker.StopInitiated()
	require.NotEqual(t, staleNotifier, activeNotifier)
	select {
	case <-activeNotifier:
		t.Fatal("valid Start retained the closed notifier from never-started Stop")
	default:
	}

	stopErr := make(chan error, 1)
	go func() { stopErr <- worker.Stop(context.Background()) }()
	select {
	case <-activeNotifier:
	case <-time.After(time.Second):
		t.Fatal("real Stop did not close the active notifier generation")
	}
	require.NoError(t, <-stopErr)
	require.Equal(t, int64(1), stops.Load())
	select {
	case <-persisted:
	case <-time.After(time.Second):
		t.Fatal("future valid start was canceled by never-started Stop")
	}
}

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

func newAuthCacheInvalidationOutboxAdapterForTest(t *testing.T, repo AuthCacheInvalidationOutboxRepository, cache APIKeyCache) (*AuthCacheInvalidationOutboxWorker, *AuthCacheInvalidationWorker) {
	t.Helper()
	native := NewAuthCacheInvalidationWorker(repo, cache)
	adapter, err := NewAuthCacheInvalidationOutboxWorker(native)
	require.NoError(t, err)
	return adapter, native
}

func TestAuthCacheInvalidationOutboxWorkerDescriptorAndInitialStatusAreDetached(t *testing.T) {
	adapter, _ := newAuthCacheInvalidationOutboxAdapterForTest(t, &authInvalidationRepoStub{}, &authInvalidationCacheStub{})
	want := workerruntime.Descriptor{
		Name:             "auth-cache-invalidation-outbox",
		Kind:             workerruntime.KindPool,
		Group:            "auth",
		CoordinationMode: workerruntime.CoordinationDurableClaim,
		Description:      "Processes durable API key auth cache invalidation events",
		Tags:             []string{"auth", "cache", "invalidation", "outbox"},
	}
	require.Equal(t, want, adapter.Descriptor())
	snapshot := adapter.Snapshot()
	require.Equal(t, want, snapshot.Descriptor)
	require.Equal(t, workerruntime.LifecycleStopped, snapshot.Lifecycle.State)
	status, ok := snapshot.Status.(workerruntime.PoolStatus)
	require.True(t, ok)
	require.False(t, status.Accepting)
	require.False(t, status.StillRunning)
	require.Equal(t, authInvalidationConcurrency, status.MaxConcurrency)
	require.Zero(t, status.RunningWorkers)

	detached := adapter.Descriptor()
	detached.Tags[0] = "mutated"
	require.Equal(t, want.Tags, adapter.Descriptor().Tags)
}

func TestAuthCacheInvalidationOutboxWorkerRejectsMissingDependencies(t *testing.T) {
	var typedNilRepo *authInvalidationRepoStub
	var typedNilCache *authInvalidationCacheStub
	validRepo := &authInvalidationRepoStub{}
	validCache := &authInvalidationCacheStub{}
	for _, native := range []*AuthCacheInvalidationWorker{
		nil,
		{},
		NewAuthCacheInvalidationWorker(typedNilRepo, validCache),
		NewAuthCacheInvalidationWorker(validRepo, typedNilCache),
	} {
		adapter, err := NewAuthCacheInvalidationOutboxWorker(native)
		require.Nil(t, adapter)
		require.EqualError(t, err, "Auth cache invalidation worker is required")
	}
}

func TestAuthCacheInvalidationOutboxWorkerPreCanceledStartDoesNotInvokeNativeStart(t *testing.T) {
	adapter, _ := newAuthCacheInvalidationOutboxAdapterForTest(t, &authInvalidationRepoStub{}, &authInvalidationCacheStub{})
	var starts atomic.Int64
	adapter.testHooks = &authCacheInvalidationOutboxWorkerTestHooks{beforeNativeStart: func() { starts.Add(1) }}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, adapter.Start(ctx), context.Canceled)
	require.Zero(t, starts.Load())
	require.Equal(t, workerruntime.LifecycleStopped, adapter.Snapshot().Lifecycle.State)
	require.NoError(t, adapter.Start(context.Background()))
	require.NoError(t, adapter.Stop(context.Background()))
}

func TestAuthCacheInvalidationOutboxWorkerStartIsRunningAndIdempotent(t *testing.T) {
	adapter, _ := newAuthCacheInvalidationOutboxAdapterForTest(t, &authInvalidationRepoStub{}, &authInvalidationCacheStub{})
	var starts atomic.Int64
	adapter.testHooks = &authCacheInvalidationOutboxWorkerTestHooks{beforeNativeStart: func() { starts.Add(1) }}
	require.NoError(t, adapter.Start(context.Background()))
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, adapter.Start(canceledCtx))
	require.Equal(t, int64(1), starts.Load())
	snapshot := adapter.Snapshot()
	require.Equal(t, workerruntime.LifecycleRunning, snapshot.Lifecycle.State)
	status := snapshot.Status.(workerruntime.PoolStatus)
	require.True(t, status.Accepting)
	require.True(t, status.StillRunning)
	require.Equal(t, int64(1), status.RunningWorkers)
	require.NoError(t, adapter.Stop(context.Background()))
}

func TestAuthCacheInvalidationOutboxWorkerConcurrentStartCallsNativeStartExactlyOnce(t *testing.T) {
	adapter, _ := newAuthCacheInvalidationOutboxAdapterForTest(t, &authInvalidationRepoStub{}, &authInvalidationCacheStub{})
	var starts atomic.Int64
	adapter.testHooks = &authCacheInvalidationOutboxWorkerTestHooks{beforeNativeStart: func() { starts.Add(1) }}
	const callers = 32
	start := make(chan struct{})
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- adapter.Start(context.Background())
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, int64(1), starts.Load())
	require.NoError(t, adapter.Stop(context.Background()))
}

func TestAuthCacheInvalidationOutboxWorkerMapsOnlyProcessLocalCounters(t *testing.T) {
	repo := &authInvalidationRepoStub{stats: AuthCacheInvalidationOutboxStats{Pending: 99, LastError: "repository-secret-error"}}
	adapter, native := newAuthCacheInvalidationOutboxAdapterForTest(t, repo, &authInvalidationCacheStub{})
	native.processed.Store(7)
	native.failures.Store(3)
	native.lastError.Store("redis-secret-error")
	native.workerID = "private-worker-uuid"
	snapshot := adapter.Snapshot()
	status := snapshot.Status.(workerruntime.PoolStatus)
	require.Equal(t, uint64(7), status.SuccessfulTasks)
	require.Equal(t, uint64(3), status.FailedTasks)
	require.Zero(t, status.CompletedTasks)
	require.Zero(t, status.WaitingTasks)
	require.Zero(t, status.SubmittedTasks)
	require.Zero(t, status.DroppedTasks)
	require.Zero(t, status.DroppedQueueFull)
	require.Zero(t, status.DroppedPoolStopped)
	require.Zero(t, status.SyncFallbackTasks)
	require.Zero(t, repo.statsCalls, "generic runtime status must not query durable stats")
	projection := fmt.Sprintf("%+v", snapshot)
	for _, private := range []string{"repository-secret-error", "redis-secret-error", "private-worker-uuid", "private-cache-key", "private-event-id"} {
		require.NotContains(t, projection, private)
	}
}

func TestAuthCacheInvalidationOutboxWorkerNormalStopReachesStopped(t *testing.T) {
	adapter, _ := newAuthCacheInvalidationOutboxAdapterForTest(t, &authInvalidationRepoStub{}, &authInvalidationCacheStub{})
	require.NoError(t, adapter.Start(context.Background()))
	require.NoError(t, adapter.Stop(context.Background()))
	require.NoError(t, adapter.Stop(context.Background()))
	snapshot := adapter.Snapshot()
	require.Equal(t, workerruntime.LifecycleStopped, snapshot.Lifecycle.State)
	status := snapshot.Status.(workerruntime.PoolStatus)
	require.False(t, status.Accepting)
	require.False(t, status.StillRunning)
	require.Zero(t, status.RunningWorkers)
}

func TestAuthCacheInvalidationOutboxWorkerConcurrentStopSharesNativeCompletion(t *testing.T) {
	adapter, _ := newAuthCacheInvalidationOutboxAdapterForTest(t, &authInvalidationRepoStub{}, &authInvalidationCacheStub{})
	stopEntered := make(chan struct{})
	releaseStop := make(chan struct{})
	var stops atomic.Int64
	adapter.testHooks = &authCacheInvalidationOutboxWorkerTestHooks{beforeNativeStop: func() {
		stops.Add(1)
		close(stopEntered)
		<-releaseStop
	}}
	require.NoError(t, adapter.Start(context.Background()))
	const callers = 32
	start := make(chan struct{})
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- adapter.Stop(context.Background())
		}()
	}
	close(start)
	<-stopEntered
	close(releaseStop)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, int64(1), stops.Load())
	require.NoError(t, adapter.Stop(context.Background()))
}

func TestAuthCacheInvalidationOutboxWorkerStopDeadlineRemainsTruthfullyStopping(t *testing.T) {
	claimEntered := make(chan struct{})
	releaseClaim := make(chan struct{})
	repo := &authInvalidationRepoStub{claimFn: func(context.Context, string, int, time.Duration) ([]AuthCacheInvalidationEvent, error) {
		close(claimEntered)
		<-releaseClaim
		return nil, nil
	}}
	adapter, _ := newAuthCacheInvalidationOutboxAdapterForTest(t, repo, &authInvalidationCacheStub{})
	require.NoError(t, adapter.Start(context.Background()))
	<-claimEntered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, adapter.Stop(ctx), context.Canceled)
	snapshot := adapter.Snapshot()
	require.Equal(t, workerruntime.LifecycleStopping, snapshot.Lifecycle.State)
	status := snapshot.Status.(workerruntime.PoolStatus)
	require.False(t, status.Accepting)
	require.True(t, status.StillRunning)
	require.Equal(t, int64(1), status.RunningWorkers)
	close(releaseClaim)
	require.NoError(t, adapter.Stop(context.Background()))
	require.Equal(t, workerruntime.LifecycleStopped, adapter.Snapshot().Lifecycle.State)
}

func TestAuthCacheInvalidationOutboxWorkerRejectsStartDuringAndAfterStop(t *testing.T) {
	adapter, _ := newAuthCacheInvalidationOutboxAdapterForTest(t, &authInvalidationRepoStub{}, &authInvalidationCacheStub{})
	stopEntered := make(chan struct{})
	releaseStop := make(chan struct{})
	var starts atomic.Int64
	adapter.testHooks = &authCacheInvalidationOutboxWorkerTestHooks{
		beforeNativeStart: func() { starts.Add(1) },
		beforeNativeStop: func() {
			close(stopEntered)
			<-releaseStop
		},
	}
	require.NoError(t, adapter.Start(context.Background()))
	stopErr := make(chan error, 1)
	go func() { stopErr <- adapter.Stop(context.Background()) }()
	<-stopEntered
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	require.EqualError(t, adapter.Start(canceledCtx), "Auth cache invalidation worker is stopping")
	close(releaseStop)
	require.NoError(t, <-stopErr)
	require.EqualError(t, adapter.Start(canceledCtx), "Auth cache invalidation worker is non-restartable after Stop")
	require.Equal(t, int64(1), starts.Load())
}

func TestAuthCacheInvalidationOutboxWorkerStopBeforeStartPreservesFutureStart(t *testing.T) {
	adapter, _ := newAuthCacheInvalidationOutboxAdapterForTest(t, &authInvalidationRepoStub{}, &authInvalidationCacheStub{})
	var starts, stops atomic.Int64
	adapter.testHooks = &authCacheInvalidationOutboxWorkerTestHooks{
		beforeNativeStart: func() { starts.Add(1) },
		beforeNativeStop:  func() { stops.Add(1) },
	}
	require.NoError(t, adapter.Stop(context.Background()))
	require.Zero(t, stops.Load())
	staleNotifier := adapter.StopInitiated()
	select {
	case <-staleNotifier:
	default:
		t.Fatal("never-started Stop did not close its notifier generation")
	}
	require.NoError(t, adapter.Start(context.Background()))
	require.Equal(t, int64(1), starts.Load())
	activeNotifier := adapter.StopInitiated()
	require.NotEqual(t, staleNotifier, activeNotifier)
	select {
	case <-activeNotifier:
		t.Fatal("valid Start retained the closed notifier from never-started Stop")
	default:
	}
	stopErr := make(chan error, 1)
	go func() { stopErr <- adapter.Stop(context.Background()) }()
	select {
	case <-activeNotifier:
	case <-time.After(time.Second):
		t.Fatal("real Stop did not close the active notifier generation")
	}
	require.NoError(t, <-stopErr)
	require.Equal(t, int64(1), stops.Load())
}

// periodicJobDuration reads a concrete PeriodicJob duration only in tests. The runtime
// intentionally exposes no public spec accessor, so bounded reflection verifies the
// constructed adapter rather than merely checking the shared source constant.
func periodicJobDuration(t *testing.T, worker *workerruntime.PeriodicJob, fieldName string) time.Duration {
	t.Helper()
	field := reflect.ValueOf(worker).Elem().FieldByName(fieldName)
	require.True(t, field.IsValid(), "PeriodicJob field %q not found", fieldName)
	require.True(t, field.CanAddr(), "PeriodicJob field %q is not addressable", fieldName)
	return time.Duration(reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Int())
}

func TestOpenAIOAuthSessionCleanupWorkerUsesRuntimePeriodicSpec(t *testing.T) {
	worker, err := NewOpenAIOAuthSessionCleanupWorker(NewOpenAIOAuthService(nil, nil))
	require.NoError(t, err)
	require.NotNil(t, worker)
	snapshot := worker.Snapshot()
	require.Equal(t, "openai-oauth-session-cleanup", snapshot.Descriptor.Name)
	require.Equal(t, workerruntime.KindPeriodic, snapshot.Descriptor.Kind)
	require.Equal(t, "auth", snapshot.Descriptor.Group)
	require.Equal(t, workerruntime.CoordinationPerInstance, snapshot.Descriptor.CoordinationMode)
	require.Equal(t, "Removes expired OpenAI OAuth authorization sessions", snapshot.Descriptor.Description)
	require.Equal(t, []string{"oauth", "openai", "session-cleanup"}, snapshot.Descriptor.Tags)
	require.Equal(t, 5*time.Minute, periodicJobDuration(t, worker, "interval"))
	require.Equal(t, 5*time.Second, periodicJobDuration(t, worker, "timeout"))
	require.IsType(t, workerruntime.PeriodicStatus{}, snapshot.Status)
}

func TestOpenAIOAuthRedisSetFailureCleanupWorkerUsesRuntimePeriodicSpec(t *testing.T) {
	svc := newOpenAIOAuthServiceWithSessionStore(nil, nil, &openAIOAuthRedisSessionStore{memory: newOpenAIOAuthMemorySessionStore()})
	worker, err := NewOpenAIOAuthRedisSetFailureCleanupWorker(svc)
	require.NoError(t, err)
	require.NotNil(t, worker)
	snapshot := worker.Snapshot()
	require.Equal(t, "openai-oauth-redis-set-failure-cleanup", snapshot.Descriptor.Name)
	require.Equal(t, workerruntime.KindPeriodic, snapshot.Descriptor.Kind)
	require.Equal(t, "auth", snapshot.Descriptor.Group)
	require.Equal(t, workerruntime.CoordinationPerInstance, snapshot.Descriptor.CoordinationMode)
	require.Equal(t, "Removes expired OpenAI OAuth Redis write-failure fallback markers", snapshot.Descriptor.Description)
	require.Equal(t, []string{"oauth", "openai", "redis-fallback-cleanup"}, snapshot.Descriptor.Tags)
	require.Equal(t, 5*time.Minute, periodicJobDuration(t, worker, "interval"))
	require.Equal(t, 5*time.Second, periodicJobDuration(t, worker, "timeout"))
	require.IsType(t, workerruntime.PeriodicStatus{}, snapshot.Status)
}

func TestOpenAIOAuthCleanupWorkersRejectMissingServiceOrStore(t *testing.T) {
	for _, factory := range []func(*OpenAIOAuthService) (*workerruntime.PeriodicJob, error){
		NewOpenAIOAuthSessionCleanupWorker,
		NewOpenAIOAuthRedisSetFailureCleanupWorker,
	} {
		worker, err := factory(nil)
		require.Nil(t, worker)
		require.EqualError(t, err, "OpenAI OAuth service is required")
		worker, err = factory(&OpenAIOAuthService{})
		require.Nil(t, worker)
		require.EqualError(t, err, "OpenAI OAuth service is required")
	}
}

func TestOpenAIOAuthRedisSetFailureCleanupWorkerIsOmittedForMemoryStore(t *testing.T) {
	worker, err := NewOpenAIOAuthRedisSetFailureCleanupWorker(NewOpenAIOAuthService(nil, nil))
	require.NoError(t, err)
	require.Nil(t, worker)
}

func TestOpenAIOAuthCleanupWorkersDeferFirstRunAndStop(t *testing.T) {
	redisService := newOpenAIOAuthServiceWithSessionStore(nil, nil, &openAIOAuthRedisSessionStore{memory: newOpenAIOAuthMemorySessionStore()})
	workers := []*workerruntime.PeriodicJob{}
	sessionWorker, err := NewOpenAIOAuthSessionCleanupWorker(redisService)
	require.NoError(t, err)
	workers = append(workers, sessionWorker)
	markerWorker, err := NewOpenAIOAuthRedisSetFailureCleanupWorker(redisService)
	require.NoError(t, err)
	workers = append(workers, markerWorker)

	for _, worker := range workers {
		beforeStart := time.Now()
		require.NoError(t, worker.Start(context.Background()))
		var status workerruntime.PeriodicStatus
		require.Eventually(t, func() bool {
			status = worker.Snapshot().Status.(workerruntime.PeriodicStatus)
			return !status.NextRunAt.IsZero()
		}, time.Second, time.Millisecond)
		observedAt := time.Now()
		require.False(t, status.NextRunAt.Before(beforeStart.Add(5*time.Minute)))
		require.False(t, status.NextRunAt.After(observedAt.Add(5*time.Minute)))
		require.Zero(t, status.RunCount)
		require.NoError(t, worker.Stop(context.Background()))
		require.Equal(t, workerruntime.LifecycleStopped, worker.Snapshot().Lifecycle.State)
	}
}

func TestOpenAIOAuthCleanupLifecycleIsRuntimeOwned(t *testing.T) {
	serviceSource, err := os.ReadFile("openai_oauth_service.go")
	require.NoError(t, err)
	require.NotContains(t, string(serviceSource), "func (s *OpenAIOAuthService) Stop")
	storeSource, err := os.ReadFile("openai_oauth_session_store.go")
	require.NoError(t, err)
	require.NotContains(t, string(storeSource), "time.NewTicker")
	require.NotContains(t, string(storeSource), "stopCh")
}

func TestClaudeOAuthSessionCleanupWorkerUsesRuntimePeriodicSpec(t *testing.T) {
	svc := NewOAuthService(nil, nil)
	worker, err := NewClaudeOAuthSessionCleanupWorker(svc)

	require.NoError(t, err)
	require.NotNil(t, worker)
	snapshot := worker.Snapshot()
	require.Equal(t, "claude-oauth-session-cleanup", snapshot.Descriptor.Name)
	require.Equal(t, workerruntime.KindPeriodic, snapshot.Descriptor.Kind)
	require.Equal(t, "auth", snapshot.Descriptor.Group)
	require.Equal(t, workerruntime.CoordinationPerInstance, snapshot.Descriptor.CoordinationMode)
	require.IsType(t, workerruntime.PeriodicStatus{}, snapshot.Status)
}

func TestClaudeOAuthSessionCleanupWorkerDefersFirstRunAndStops(t *testing.T) {
	svc := NewOAuthService(nil, nil)
	worker, err := NewClaudeOAuthSessionCleanupWorker(svc)
	require.NoError(t, err)
	require.NoError(t, worker.Start(context.Background()))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, worker.Stop(ctx))
	require.Equal(t, workerruntime.LifecycleStopped, worker.Snapshot().Lifecycle.State)
}

func TestClaudeOAuthSessionCleanupLifecycleIsRuntimeOwned(t *testing.T) {
	content, err := os.ReadFile("oauth_service.go")
	require.NoError(t, err)
	require.NotContains(t, string(content), "func (s *OAuthService) Stop")
}

func TestGeminiOAuthSessionCleanupWorkerUsesRuntimePeriodicSpec(t *testing.T) {
	svc := NewGeminiOAuthService(nil, nil, nil, nil, &config.Config{})
	worker, err := NewGeminiOAuthSessionCleanupWorker(svc)

	require.NoError(t, err)
	require.NotNil(t, worker)
	snapshot := worker.Snapshot()
	require.Equal(t, "gemini-oauth-session-cleanup", snapshot.Descriptor.Name)
	require.Equal(t, workerruntime.KindPeriodic, snapshot.Descriptor.Kind)
	require.Equal(t, "auth", snapshot.Descriptor.Group)
	require.Equal(t, workerruntime.CoordinationPerInstance, snapshot.Descriptor.CoordinationMode)
	require.Equal(t, "Removes expired Gemini OAuth authorization sessions", snapshot.Descriptor.Description)
	require.Equal(t, []string{"oauth", "gemini", "session-cleanup"}, snapshot.Descriptor.Tags)
	require.Equal(t, 5*time.Minute, geminiOAuthSessionCleanupInterval)
	require.Equal(t, 5*time.Second, geminiOAuthSessionCleanupTimeout)
	require.IsType(t, workerruntime.PeriodicStatus{}, snapshot.Status)
}

func TestGeminiOAuthSessionCleanupWorkerDefersFirstRunAndStops(t *testing.T) {
	svc := NewGeminiOAuthService(nil, nil, nil, nil, &config.Config{})
	worker, err := NewGeminiOAuthSessionCleanupWorker(svc)
	require.NoError(t, err)
	beforeStart := time.Now()
	require.NoError(t, worker.Start(context.Background()))
	var status workerruntime.PeriodicStatus
	require.Eventually(t, func() bool {
		status = worker.Snapshot().Status.(workerruntime.PeriodicStatus)
		return !status.NextRunAt.IsZero()
	}, time.Second, time.Millisecond)
	observedAt := time.Now()
	require.False(t, status.NextRunAt.Before(beforeStart.Add(5*time.Minute)))
	require.False(t, status.NextRunAt.After(observedAt.Add(5*time.Minute)))
	require.Zero(t, status.RunCount)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, worker.Stop(ctx))
	require.Equal(t, workerruntime.LifecycleStopped, worker.Snapshot().Lifecycle.State)
}

func TestGeminiOAuthSessionCleanupWorkerRejectsMissingService(t *testing.T) {
	worker, err := NewGeminiOAuthSessionCleanupWorker(nil)
	require.Nil(t, worker)
	require.EqualError(t, err, "Gemini OAuth service is required")

	worker, err = NewGeminiOAuthSessionCleanupWorker(&GeminiOAuthService{})
	require.Nil(t, worker)
	require.EqualError(t, err, "Gemini OAuth service is required")
}

func TestGeminiOAuthSessionCleanupLifecycleIsRuntimeOwned(t *testing.T) {
	content, err := os.ReadFile("gemini_oauth_service.go")
	require.NoError(t, err)
	require.NotContains(t, string(content), "func (s *GeminiOAuthService) Stop")
}

func TestAntigravityOAuthSessionCleanupWorkerUsesRuntimePeriodicSpec(t *testing.T) {
	svc := NewAntigravityOAuthService(nil)
	worker, err := NewAntigravityOAuthSessionCleanupWorker(svc)

	require.NoError(t, err)
	require.NotNil(t, worker)
	snapshot := worker.Snapshot()
	require.Equal(t, "antigravity-oauth-session-cleanup", snapshot.Descriptor.Name)
	require.Equal(t, workerruntime.KindPeriodic, snapshot.Descriptor.Kind)
	require.Equal(t, "auth", snapshot.Descriptor.Group)
	require.Equal(t, workerruntime.CoordinationPerInstance, snapshot.Descriptor.CoordinationMode)
	require.Equal(t, "Removes expired Antigravity OAuth authorization sessions", snapshot.Descriptor.Description)
	require.Equal(t, []string{"oauth", "antigravity", "session-cleanup"}, snapshot.Descriptor.Tags)
	require.Equal(t, 5*time.Minute, antigravityOAuthSessionCleanupInterval)
	require.Equal(t, 5*time.Second, antigravityOAuthSessionCleanupTimeout)
	require.IsType(t, workerruntime.PeriodicStatus{}, snapshot.Status)
}

func TestAntigravityOAuthSessionCleanupWorkerDefersFirstRunAndStops(t *testing.T) {
	svc := NewAntigravityOAuthService(nil)
	worker, err := NewAntigravityOAuthSessionCleanupWorker(svc)
	require.NoError(t, err)
	beforeStart := time.Now()
	require.NoError(t, worker.Start(context.Background()))
	var status workerruntime.PeriodicStatus
	require.Eventually(t, func() bool {
		status = worker.Snapshot().Status.(workerruntime.PeriodicStatus)
		return !status.NextRunAt.IsZero()
	}, time.Second, time.Millisecond)
	observedAt := time.Now()
	require.False(t, status.NextRunAt.Before(beforeStart.Add(5*time.Minute)))
	require.False(t, status.NextRunAt.After(observedAt.Add(5*time.Minute)))
	require.Zero(t, status.RunCount)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, worker.Stop(ctx))
	require.Equal(t, workerruntime.LifecycleStopped, worker.Snapshot().Lifecycle.State)
}

func TestAntigravityOAuthSessionCleanupWorkerRejectsMissingService(t *testing.T) {
	worker, err := NewAntigravityOAuthSessionCleanupWorker(nil)
	require.Nil(t, worker)
	require.EqualError(t, err, "Antigravity OAuth service is required")

	worker, err = NewAntigravityOAuthSessionCleanupWorker(&AntigravityOAuthService{})
	require.Nil(t, worker)
	require.EqualError(t, err, "Antigravity OAuth service is required")
}

func TestAntigravityOAuthSessionCleanupLifecycleIsRuntimeOwned(t *testing.T) {
	content, err := os.ReadFile("antigravity_oauth_service.go")
	require.NoError(t, err)
	require.NotContains(t, string(content), "func (s *AntigravityOAuthService) Stop")
}

func TestConcurrencySlotCleanupWorkerUsesRuntimePeriodicSpec(t *testing.T) {
	cache := &slotCleanupCache{}
	svc := NewConcurrencyService(cache)
	svc.ConfigureSlotCleanup(time.Minute)

	worker, err := NewConcurrencySlotCleanupWorker(svc)

	require.NoError(t, err)
	require.NotNil(t, worker)
	snapshot := worker.Snapshot()
	require.Equal(t, "concurrency-slot-cleanup", snapshot.Descriptor.Name)
	require.Equal(t, workerruntime.KindPeriodic, snapshot.Descriptor.Kind)
	require.Equal(t, "maintenance", snapshot.Descriptor.Group)
	require.Equal(t, workerruntime.CoordinationPerInstance, snapshot.Descriptor.CoordinationMode)
	require.Equal(t, "Removes expired account concurrency slots", snapshot.Descriptor.Description)
	require.Equal(t, []string{"concurrency", "account-slots", "cleanup"}, snapshot.Descriptor.Tags)
	require.IsType(t, workerruntime.PeriodicStatus{}, snapshot.Status)
}

func TestConcurrencySlotCleanupWorkerIsOmittedWhenDisabled(t *testing.T) {
	for _, svc := range []*ConcurrencyService{
		NewConcurrencyService(nil),
		NewConcurrencyService(&slotCleanupCache{}),
	} {
		if svc.cache != nil {
			svc.ConfigureSlotCleanup(0)
		}
		worker, err := NewConcurrencySlotCleanupWorker(svc)
		require.NoError(t, err)
		require.Nil(t, worker)
	}
}

func TestConcurrencySlotCleanupWorkerRunsImmediatelyAndUsesFixedDelay(t *testing.T) {
	cache := &slotCleanupCache{}
	svc := NewConcurrencyService(cache)
	svc.ConfigureSlotCleanup(time.Hour)
	worker, err := NewConcurrencySlotCleanupWorker(svc)
	require.NoError(t, err)
	require.NoError(t, worker.Start(context.Background()))
	t.Cleanup(func() { require.NoError(t, worker.Stop(context.Background())) })

	require.Eventually(t, func() bool { return cache.calls.Load() == 1 }, time.Second, time.Millisecond)
	require.Eventually(t, func() bool {
		status, ok := worker.Snapshot().Status.(workerruntime.PeriodicStatus)
		return ok && status.RunCount == 1
	}, time.Second, time.Millisecond)
	status := worker.Snapshot().Status.(workerruntime.PeriodicStatus)
	require.Equal(t, workerruntime.OutcomeSuccess, status.LastOutcome)
	require.False(t, status.NextRunAt.IsZero())
}

func TestConcurrencySlotCleanupCapsContextAtFiveSecondsAndPreservesEarlierDeadline(t *testing.T) {
	cache := &slotCleanupCache{contexts: make(chan context.Context, 2)}
	svc := NewConcurrencyService(cache)

	require.NoError(t, svc.RunSlotCleanup(context.Background()))
	cleanupCtx := <-cache.contexts
	deadline, ok := cleanupCtx.Deadline()
	require.True(t, ok)
	require.LessOrEqual(t, deadline.Sub(time.Now()), 5*time.Second)
	require.Greater(t, deadline.Sub(time.Now()), 4*time.Second)

	parent, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	parentDeadline, ok := parent.Deadline()
	require.True(t, ok)
	require.NoError(t, svc.RunSlotCleanup(parent))
	cleanupCtx = <-cache.contexts
	deadline, ok = cleanupCtx.Deadline()
	require.True(t, ok)
	require.False(t, deadline.After(parentDeadline))
}

func TestConcurrencySlotCleanupReturnsCacheFailure(t *testing.T) {
	cache := &slotCleanupCache{err: errors.New("cleanup failed")}
	svc := NewConcurrencyService(cache)

	require.ErrorIs(t, svc.RunSlotCleanup(context.Background()), cache.err)
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

func TestSubscriptionExpiryWorkerPreservesPeriodicRuntimeSpec(t *testing.T) {
	worker, err := NewSubscriptionExpiryWorker(NewSubscriptionExpiryService(nil, time.Minute))

	require.NoError(t, err)
	snapshot := worker.Snapshot()
	require.Equal(t, "subscription-expiry", snapshot.Descriptor.Name)
	require.Equal(t, workerruntime.KindPeriodic, snapshot.Descriptor.Kind)
	require.Equal(t, "maintenance", snapshot.Descriptor.Group)
	require.Equal(t, workerruntime.CoordinationPerInstance, snapshot.Descriptor.CoordinationMode)
}

func TestPaymentOrderExpiryWorkerPreservesPeriodicRuntimeSpec(t *testing.T) {
	worker, err := NewPaymentOrderExpiryWorker(NewPaymentOrderExpiryService(nil, time.Minute))

	require.NoError(t, err)
	snapshot := worker.Snapshot()
	require.Equal(t, "payment-order-expiry", snapshot.Descriptor.Name)
	require.Equal(t, workerruntime.KindPeriodic, snapshot.Descriptor.Kind)
	require.Equal(t, "maintenance", snapshot.Descriptor.Group)
	require.Equal(t, workerruntime.CoordinationSingletonRun, snapshot.Descriptor.CoordinationMode)
}

func TestPricingRemoteSyncWorkerIsNotRegisteredWithoutRemoteURL(t *testing.T) {
	worker, err := NewPricingRemoteSyncWorker(NewPricingService(&config.Config{}, nil))

	require.NoError(t, err)
	require.Nil(t, worker)
}

func TestPricingRemoteSyncWorkerWaitsForItsFirstInterval(t *testing.T) {
	cfg := &config.Config{}
	cfg.Pricing.RemoteURL = "https://pricing.example/models.json"
	cfg.Pricing.HashCheckIntervalMinutes = 60
	worker, err := NewPricingRemoteSyncWorker(NewPricingService(cfg, nil))

	require.NoError(t, err)
	require.NotNil(t, worker)
	require.NoError(t, worker.Start(context.Background()))
	t.Cleanup(func() { require.NoError(t, worker.Stop(context.Background())) })
	require.Never(t, func() bool {
		return worker.Snapshot().Status.(workerruntime.PeriodicStatus).RunCount > 0
	}, 50*time.Millisecond, time.Millisecond)
}

func TestPricingRemoteSyncCapsEachRemoteOperationAtThirtySeconds(t *testing.T) {
	cfg := &config.Config{}
	cfg.Pricing.RemoteURL = "https://pricing.example/models.json"
	cfg.Pricing.HashURL = "https://pricing.example/models.sha256"
	cfg.Pricing.DataDir = t.TempDir()
	pricingFile := cfg.Pricing.DataDir + "/model_pricing.json"
	require.NoError(t, os.WriteFile(pricingFile, []byte(`{"test":{"input_cost_per_token":1}}`), 0600))
	client := &pricingRemoteClientContextSpy{}
	svc := NewPricingService(cfg, client)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	require.NoError(t, svc.RunRemoteSync(ctx))
	assertDeadlineDurationNear(t, client.hashObservedAt, client.hashDeadline, 30*time.Second)
	assertDeadlineDurationNear(t, client.downloadObservedAt, client.downloadDeadline, 30*time.Second)
}

func TestPricingRemoteSyncRetainsEarlierParentDeadlineForEachOperation(t *testing.T) {
	cfg := &config.Config{}
	cfg.Pricing.RemoteURL = "https://pricing.example/models.json"
	cfg.Pricing.HashURL = "https://pricing.example/models.sha256"
	cfg.Pricing.DataDir = t.TempDir()
	pricingFile := cfg.Pricing.DataDir + "/model_pricing.json"
	require.NoError(t, os.WriteFile(pricingFile, []byte(`{"test":{"input_cost_per_token":1}}`), 0600))
	client := &pricingRemoteClientContextSpy{}
	svc := NewPricingService(cfg, client)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	parentDeadline, ok := ctx.Deadline()
	require.True(t, ok)

	require.NoError(t, svc.RunRemoteSync(ctx))
	require.False(t, client.hashDeadline.After(parentDeadline))
	require.False(t, client.downloadDeadline.After(parentDeadline))
}

func assertDeadlineDurationNear(t *testing.T, observedAt, deadline time.Time, expected time.Duration) {
	t.Helper()
	require.False(t, deadline.IsZero())
	remaining := deadline.Sub(observedAt)
	require.GreaterOrEqual(t, remaining, expected-time.Second)
	require.LessOrEqual(t, remaining, expected)
}

func TestPricingRemoteSyncPropagatesWorkerDeadlineToHashAndDownload(t *testing.T) {
	cfg := &config.Config{}
	cfg.Pricing.RemoteURL = "https://pricing.example/models.json"
	cfg.Pricing.HashURL = "https://pricing.example/models.sha256"
	cfg.Pricing.DataDir = t.TempDir()
	pricingFile := cfg.Pricing.DataDir + "/model_pricing.json"
	require.NoError(t, os.WriteFile(pricingFile, []byte(`{"test":{"input_cost_per_token":1}}`), 0600))
	client := &pricingRemoteClientContextSpy{}
	svc := NewPricingService(cfg, client)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	workerDeadline, ok := ctx.Deadline()
	require.True(t, ok)

	require.NoError(t, svc.RunRemoteSync(ctx))
	require.False(t, client.hashDeadline.IsZero())
	require.False(t, client.downloadDeadline.IsZero())
	require.False(t, client.hashDeadline.After(workerDeadline))
	require.False(t, client.downloadDeadline.After(workerDeadline))
}

func TestPaymentOrderExpiryWorkerTimeoutExceedsLockAndOperationBudgets(t *testing.T) {
	require.Greater(t, paymentOrderExpiryWorkerTimeout,
		paymentOrderExpiryLockAcquireTimeout+2*expiryCheckTimeout)
}

func TestTokenRefreshWorkerUsesEnabledServiceRuntimeSpec(t *testing.T) {
	cfg := &config.Config{}
	cfg.TokenRefresh.Enabled = true
	cfg.TokenRefresh.CheckIntervalMinutes = 30
	svc := NewTokenRefreshService(&tokenRefreshRuntimeRepo{}, nil, nil, nil, nil, nil, nil, cfg, nil)

	worker, err := NewTokenRefreshWorker(svc)

	require.NoError(t, err)
	require.NotNil(t, worker)
	snapshot := worker.Snapshot()
	require.Equal(t, "token-refresh", snapshot.Descriptor.Name)
	require.Equal(t, workerruntime.KindPeriodic, snapshot.Descriptor.Kind)
	require.Equal(t, "auth", snapshot.Descriptor.Group)
	require.Equal(t, workerruntime.CoordinationPerInstance, snapshot.Descriptor.CoordinationMode)
	require.Equal(t, "Refreshes eligible OAuth tokens before expiry", snapshot.Descriptor.Description)
	require.Equal(t, []string{"oauth", "token-refresh"}, snapshot.Descriptor.Tags)
	require.IsType(t, workerruntime.PeriodicStatus{}, snapshot.Status)
	require.Equal(t, 30*time.Minute, svc.Interval())
}

func TestTokenRefreshWorkerUsesFiveMinuteMinimumInterval(t *testing.T) {
	cfg := &config.Config{}
	cfg.TokenRefresh.Enabled = true
	cfg.TokenRefresh.CheckIntervalMinutes = 0
	svc := NewTokenRefreshService(&tokenRefreshRuntimeRepo{}, nil, nil, nil, nil, nil, nil, cfg, nil)

	worker, err := NewTokenRefreshWorker(svc)

	require.NoError(t, err)
	require.Equal(t, 5*time.Minute, svc.Interval())
	require.NotNil(t, worker)
}

func TestTokenRefreshWorkerDoesNotStartDisabledService(t *testing.T) {
	cfg := &config.Config{}
	svc := NewTokenRefreshService(&tokenRefreshRuntimeRepo{}, nil, nil, nil, nil, nil, nil, cfg, nil)

	worker, err := NewTokenRefreshWorker(svc)

	require.NoError(t, err)
	require.Nil(t, worker)
}

func TestTokenRefreshWorkerRunsEnabledServiceImmediately(t *testing.T) {
	cfg := &config.Config{}
	cfg.TokenRefresh.Enabled = true
	cfg.TokenRefresh.CheckIntervalMinutes = 60
	repo := &tokenRefreshRuntimeRepo{runs: make(chan context.Context, 1)}
	svc := NewTokenRefreshService(repo, nil, nil, nil, nil, nil, nil, cfg, nil)
	worker, err := NewTokenRefreshWorker(svc)
	require.NoError(t, err)

	require.NoError(t, worker.Start(context.Background()))
	t.Cleanup(func() { require.NoError(t, worker.Stop(context.Background())) })

	select {
	case ctx := <-repo.runs:
		require.NotNil(t, ctx)
	case <-time.After(time.Second):
		t.Fatal("token refresh worker did not run immediately")
	}
}

func TestProvideUserMessageQueueServiceConstructionDoesNotStartCleanup(t *testing.T) {
	cache := &userMessageQueueCacheSpy{scanCalls: make(chan context.Context, 1)}
	cfg := &config.Config{}
	cfg.Gateway.UserMessageQueue.CleanupIntervalSeconds = 1

	_ = ProvideUserMessageQueueService(cache, nil, cfg)

	require.Never(t, func() bool { return len(cache.scanCalls) > 0 }, 50*time.Millisecond, time.Millisecond)
}

func TestUserMessageQueueCleanupWorkerUsesDelayedPeriodicRuntimeSpec(t *testing.T) {
	cfg := &config.UserMessageQueueConfig{CleanupIntervalSeconds: 60}
	worker, err := NewUserMessageQueueCleanupWorker(NewUserMessageQueueService(&userMessageQueueCacheSpy{}, nil, cfg))

	require.NoError(t, err)
	require.NotNil(t, worker)
	snapshot := worker.Snapshot()
	require.Equal(t, "user-message-queue-cleanup", snapshot.Descriptor.Name)
	require.Equal(t, workerruntime.KindPeriodic, snapshot.Descriptor.Kind)
	require.Equal(t, "maintenance", snapshot.Descriptor.Group)
	require.Equal(t, workerruntime.CoordinationPerInstance, snapshot.Descriptor.CoordinationMode)
	require.Equal(t, "Releases orphaned user-message queue locks", snapshot.Descriptor.Description)
	require.Equal(t, []string{"user-message-queue", "orphan-lock-cleanup"}, snapshot.Descriptor.Tags)
	require.IsType(t, workerruntime.PeriodicStatus{}, snapshot.Status)
}

func TestUserMessageQueueCleanupWorkerIsOmittedWhenDisabled(t *testing.T) {
	for _, svc := range []*UserMessageQueueService{
		NewUserMessageQueueService(nil, nil, &config.UserMessageQueueConfig{CleanupIntervalSeconds: 60}),
		NewUserMessageQueueService(&userMessageQueueCacheSpy{}, nil, &config.UserMessageQueueConfig{}),
	} {
		worker, err := NewUserMessageQueueCleanupWorker(svc)
		require.NoError(t, err)
		require.Nil(t, worker)
	}
}

func TestUserMessageQueueCleanupWorkerWaitsForFirstInterval(t *testing.T) {
	cache := &userMessageQueueCacheSpy{scanCalls: make(chan context.Context, 1)}
	worker, err := NewUserMessageQueueCleanupWorker(NewUserMessageQueueService(cache, nil, &config.UserMessageQueueConfig{CleanupIntervalSeconds: 1}))
	require.NoError(t, err)
	require.NoError(t, worker.Start(context.Background()))
	t.Cleanup(func() { require.NoError(t, worker.Stop(context.Background())) })

	require.Never(t, func() bool { return len(cache.scanCalls) > 0 }, 100*time.Millisecond, time.Millisecond)
	require.Eventually(t, func() bool { return len(cache.scanCalls) == 1 }, 3*time.Second, 10*time.Millisecond)
}

func TestUserMessageQueueCleanupPropagatesCancellationToScan(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cache := &userMessageQueueCacheSpy{scanFn: func(scanCtx context.Context) ([]int64, error) {
		cancel()
		<-scanCtx.Done()
		return nil, scanCtx.Err()
	}}
	svc := NewUserMessageQueueService(cache, nil, &config.UserMessageQueueConfig{CleanupIntervalSeconds: 60})

	require.ErrorIs(t, svc.RunCleanup(ctx), context.Canceled)
}

func TestUserMessageQueueCleanupPropagatesCallbackContextToScanAndRelease(t *testing.T) {
	cache := &userMessageQueueCacheSpy{lockIDs: []int64{1}, scanCalls: make(chan context.Context, 1), releaseCalls: make(chan context.Context, 1)}
	svc := NewUserMessageQueueService(cache, nil, &config.UserMessageQueueConfig{CleanupIntervalSeconds: 60})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	parentDeadline, ok := ctx.Deadline()
	require.True(t, ok)

	require.NoError(t, svc.RunCleanup(ctx))
	scanCtx := <-cache.scanCalls
	scanDeadline, ok := scanCtx.Deadline()
	require.True(t, ok)
	require.False(t, scanDeadline.After(parentDeadline))
	releaseCtx := <-cache.releaseCalls
	releaseDeadline, ok := releaseCtx.Deadline()
	require.True(t, ok)
	require.False(t, releaseDeadline.After(parentDeadline))
}

func TestUserMessageQueueCleanupCapsReleaseContextAtTwoSeconds(t *testing.T) {
	cache := &userMessageQueueCacheSpy{lockIDs: []int64{1}, releaseCalls: make(chan context.Context, 1)}
	svc := NewUserMessageQueueService(cache, nil, &config.UserMessageQueueConfig{CleanupIntervalSeconds: 60})

	require.NoError(t, svc.RunCleanup(context.Background()))
	releaseCtx := <-cache.releaseCalls
	deadline, ok := releaseCtx.Deadline()
	require.True(t, ok)
	require.LessOrEqual(t, deadline.Sub(time.Now()), 2*time.Second)
	require.Greater(t, deadline.Sub(time.Now()), time.Second)
}

func TestUserMessageQueueCleanupFinalReleaseCancellationIsNotReportedAsSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cache := &userMessageQueueCacheSpy{lockIDs: []int64{1}, releaseFn: func(releaseCtx context.Context, _ int64) error {
		cancel()
		<-releaseCtx.Done()
		return releaseCtx.Err()
	}}
	svc := NewUserMessageQueueService(cache, nil, &config.UserMessageQueueConfig{CleanupIntervalSeconds: 60})

	require.ErrorIs(t, svc.RunCleanup(ctx), context.Canceled)
}

func TestUserMessageQueueCleanupCancellationPreventsLaterReleases(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cache := &userMessageQueueCacheSpy{lockIDs: []int64{1, 2}, releaseFn: func(_ context.Context, accountID int64) error {
		if accountID == 1 {
			cancel()
		}
		return nil
	}}
	svc := NewUserMessageQueueService(cache, nil, &config.UserMessageQueueConfig{CleanupIntervalSeconds: 60})

	require.ErrorIs(t, svc.RunCleanup(ctx), context.Canceled)
	require.Equal(t, []int64{1}, cache.releasedIDs)
}

func TestUserMessageQueueCleanupContinuesAfterPerKeyFailure(t *testing.T) {
	cache := &userMessageQueueCacheSpy{lockIDs: []int64{1, 2}, releaseFn: func(_ context.Context, accountID int64) error {
		if accountID == 1 {
			return errors.New("release failed")
		}
		return nil
	}}
	svc := NewUserMessageQueueService(cache, nil, &config.UserMessageQueueConfig{CleanupIntervalSeconds: 60})

	require.NoError(t, svc.RunCleanup(context.Background()))
	require.Equal(t, []int64{1, 2}, cache.releasedIDs)
}

type userMessageQueueCacheSpy struct {
	lockIDs      []int64
	scanCalls    chan context.Context
	releaseCalls chan context.Context
	releasedIDs  []int64
	releaseFn    func(context.Context, int64) error
	scanFn       func(context.Context) ([]int64, error)
}

func (s *userMessageQueueCacheSpy) AcquireLock(context.Context, int64, string, int) (bool, error) {
	return false, nil
}
func (s *userMessageQueueCacheSpy) ReleaseLock(context.Context, int64, string) (bool, error) {
	return false, nil
}
func (s *userMessageQueueCacheSpy) GetLastCompletedMs(context.Context, int64) (int64, error) {
	return 0, nil
}
func (s *userMessageQueueCacheSpy) GetCurrentTimeMs(context.Context) (int64, error) { return 0, nil }
func (s *userMessageQueueCacheSpy) ScanLockKeys(ctx context.Context, _ int) ([]int64, error) {
	if s.scanCalls != nil {
		s.scanCalls <- ctx
	}
	if s.scanFn != nil {
		return s.scanFn(ctx)
	}
	return s.lockIDs, nil
}
func (s *userMessageQueueCacheSpy) ForceReleaseLock(ctx context.Context, accountID int64) error {
	if s.releaseCalls != nil {
		s.releaseCalls <- ctx
	}
	s.releasedIDs = append(s.releasedIDs, accountID)
	if s.releaseFn != nil {
		return s.releaseFn(ctx, accountID)
	}
	return nil
}

type tokenRefreshRuntimeRepo struct {
	mockAccountRepoForGemini
	runs chan context.Context
}

func (r *tokenRefreshRuntimeRepo) ListOAuthRefreshCandidates(ctx context.Context) ([]Account, error) {
	if r.runs != nil {
		r.runs <- ctx
	}
	return nil, nil
}

type pricingRemoteClientContextSpy struct {
	hashDeadline       time.Time
	downloadDeadline   time.Time
	hashObservedAt     time.Time
	downloadObservedAt time.Time
}

func (s *pricingRemoteClientContextSpy) FetchPricingJSON(ctx context.Context, _ string) ([]byte, error) {
	s.downloadObservedAt = time.Now()
	s.downloadDeadline, _ = ctx.Deadline()
	return []byte(`{"test":{"input_cost_per_token":1}}`), nil
}

func (s *pricingRemoteClientContextSpy) FetchHashText(ctx context.Context, _ string) (string, error) {
	s.hashObservedAt = time.Now()
	s.hashDeadline, _ = ctx.Deadline()
	return "different", nil
}

func TestRuntimeStopAllWaitsForPeriodicEntryBeforeRealUsagePoolStop(t *testing.T) {
	periodicRunning := make(chan struct{})
	releasePeriodic := make(chan struct{})
	periodic, err := workerruntime.NewPeriodicJob(workerruntime.PeriodicJobSpec{
		Descriptor: workerruntime.Descriptor{
			Name:             "blocking-periodic",
			Kind:             workerruntime.KindPeriodic,
			CoordinationMode: workerruntime.CoordinationPerInstance,
		},
		Interval:       time.Hour,
		Timeout:        time.Hour,
		RunImmediately: true,
		Run: func(context.Context) error {
			close(periodicRunning)
			<-releasePeriodic
			return nil
		},
	})
	require.NoError(t, err)

	pool := NewUsageRecordWorkerPoolWithOptions(UsageRecordWorkerPoolOptions{
		WorkerCount:      1,
		QueueSize:        1,
		AutoScaleEnabled: false,
	})
	worker := NewUsageRecordWorkerPoolWorker(pool)
	runtime := workerruntime.NewRuntime(workerruntime.NewRegistry())
	require.NoError(t, runtime.Register(periodic))
	require.NoError(t, runtime.Register(worker))
	require.NoError(t, runtime.StartAll(context.Background()))
	<-periodicRunning

	poolTaskStarted := make(chan struct{})
	releasePoolTask := make(chan struct{})
	require.Equal(t, UsageRecordSubmitModeEnqueued, pool.Submit(func(context.Context) {
		close(poolTaskStarted)
		<-releasePoolTask
	}))
	<-poolTaskStarted

	stopDone := make(chan struct{})
	go func() {
		_, _ = runtime.StopAll(context.Background())
		close(stopDone)
	}()
	t.Cleanup(func() {
		close(releasePeriodic)
		close(releasePoolTask)
		select {
		case <-stopDone:
		case <-time.After(time.Second):
			t.Error("Runtime.StopAll did not finish after releasing work")
		}
	})

	select {
	case <-periodic.StopInitiated():
	case <-time.After(time.Second):
		t.Fatal("periodic Stop did not enter")
	}
	require.Eventually(t, func() bool { return !pool.Accepting() }, 100*time.Millisecond, time.Millisecond,
		"real usage pool Stop was not promptly initiated after periodic Stop entry")
}
