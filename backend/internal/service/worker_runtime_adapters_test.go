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

func TestAccountExpiryWorkerPreservesImmediateRuntimeSpec(t *testing.T) {
	worker, err := NewAccountExpiryWorker(NewAccountExpiryService(&accountExpiryRepoStub{autoPauseFn: func(context.Context, time.Time) (int64, error) {
		return 0, nil
	}}, time.Minute))

	require.NoError(t, err)
	snapshot := worker.Snapshot()
	require.Equal(t, "account-expiry", snapshot.Descriptor.Name)
	require.Equal(t, workerruntime.KindPeriodic, snapshot.Descriptor.Kind)
	require.Equal(t, workerruntime.CoordinationPerInstance, snapshot.Descriptor.CoordinationMode)
}

func TestIdempotencyCleanupWorkerPreservesImmediateRuntimeSpec(t *testing.T) {
	worker, err := NewIdempotencyCleanupWorker(NewIdempotencyCleanupService(&idempotencyCleanupRepoStub{}, &config.Config{}))

	require.NoError(t, err)
	snapshot := worker.Snapshot()
	require.Equal(t, "idempotency-cleanup", snapshot.Descriptor.Name)
	require.Equal(t, workerruntime.KindPeriodic, snapshot.Descriptor.Kind)
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
