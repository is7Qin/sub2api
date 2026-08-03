//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/workerruntime"
	"github.com/stretchr/testify/require"
)

func TestNewEmailQueueServiceDoesNotStartWorkers(t *testing.T) {
	queue := NewEmailQueueService(nil, 2)
	stats := queue.Stats()
	require.Equal(t, 2, stats.MaxConcurrency)
	require.Zero(t, stats.RunningWorkers)
	require.False(t, stats.Accepting)
	require.NoError(t, queue.Stop(context.Background()))
}

func TestEmailQueueWorkerDescriptorAndSnapshot(t *testing.T) {
	component := NewEmailQueueWorker(NewEmailQueueService(nil, 2))

	snapshot := component.Snapshot()
	require.Equal(t, workerruntime.Descriptor{
		Name:             "email-queue",
		Kind:             workerruntime.KindPool,
		Group:            "notifications",
		CoordinationMode: workerruntime.CoordinationPerInstance,
		Description:      "Delivers queued email messages asynchronously",
		Tags:             []string{"email", "notifications", "asynchronous-delivery"},
	}, snapshot.Descriptor)
	require.IsType(t, workerruntime.PoolStatus{}, snapshot.Status)
}

func TestEmailQueueWorkerStartsConfiguredWorkersExactlyOnce(t *testing.T) {
	queue := NewEmailQueueService(nil, 2)
	component := NewEmailQueueWorker(queue)
	t.Cleanup(func() { require.NoError(t, component.Stop(context.Background())) })

	require.NoError(t, component.Start(context.Background()))
	require.NoError(t, component.Start(context.Background()))

	snapshot := component.Snapshot()
	status := snapshot.Status.(workerruntime.PoolStatus)
	require.Equal(t, workerruntime.LifecycleRunning, snapshot.Lifecycle.State)
	require.True(t, status.Accepting)
	require.True(t, status.StillRunning)
	require.Equal(t, 2, status.MaxConcurrency)
	require.EqualValues(t, 2, status.RunningWorkers)
}

func TestEmailQueueWorkerStopIsTruthfulAndRetryable(t *testing.T) {
	queue := NewEmailQueueService(nil, 1)
	component := NewEmailQueueWorker(queue)
	require.NoError(t, component.Start(context.Background()))

	require.NoError(t, component.Stop(context.Background()))
	snapshot := component.Snapshot()
	status := snapshot.Status.(workerruntime.PoolStatus)
	require.Equal(t, workerruntime.LifecycleStopped, snapshot.Lifecycle.State)
	require.False(t, status.Accepting)
	require.False(t, status.StillRunning)
	require.Zero(t, status.RunningWorkers)
	require.NoError(t, component.Stop(context.Background()))
}

func TestEmailQueueWorkerStopDeadlineKeepsStillRunningUntilRetry(t *testing.T) {
	queue := NewEmailQueueService(nil, 1)
	component := NewEmailQueueWorker(queue)
	require.NoError(t, component.Start(context.Background()))

	// Hold the native wait group after its worker exits to exercise deadline truthfulness.
	queue.wg.Add(1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, component.Stop(ctx), context.DeadlineExceeded)

	snapshot := component.Snapshot()
	status := snapshot.Status.(workerruntime.PoolStatus)
	require.Equal(t, workerruntime.LifecycleStopping, snapshot.Lifecycle.State)
	require.True(t, status.StillRunning)
	require.False(t, status.Accepting)

	queue.wg.Done()
	require.NoError(t, component.Stop(context.Background()))
	require.Equal(t, workerruntime.LifecycleStopped, component.Snapshot().Lifecycle.State)
}

func TestEmailQueueEnqueueAfterShutdownBeginsDoesNotBlockOrPanic(t *testing.T) {
	queue := NewEmailQueueService(nil, 1)
	component := NewEmailQueueWorker(queue)
	require.NoError(t, component.Start(context.Background()))
	queue.wg.Add(1)

	stopDone := make(chan error, 1)
	go func() { stopDone <- component.Stop(context.Background()) }()
	require.Eventually(t, func() bool {
		return component.Snapshot().Lifecycle.State == workerruntime.LifecycleStopping
	}, time.Second, time.Millisecond)

	enqueueDone := make(chan error, 1)
	require.NotPanics(t, func() {
		go func() { enqueueDone <- queue.EnqueueVerifyCode("user@example.test", "site") }()
	})
	select {
	case err := <-enqueueDone:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("enqueue blocked while shutdown was in progress")
	}

	queue.wg.Done()
	require.NoError(t, <-stopDone)
}
