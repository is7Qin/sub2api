package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/workerruntime"
)

// NewAccountExpiryWorker adapts account expiry maintenance to the worker runtime.
func NewAccountExpiryWorker(svc *AccountExpiryService) (*workerruntime.PeriodicJob, error) {
	if svc == nil {
		return nil, fmt.Errorf("account expiry service is required")
	}
	return workerruntime.NewPeriodicJob(workerruntime.PeriodicJobSpec{
		Descriptor: workerruntime.Descriptor{
			Name:             "account-expiry",
			Kind:             workerruntime.KindPeriodic,
			Group:            "maintenance",
			CoordinationMode: workerruntime.CoordinationPerInstance,
		},
		Interval:       svc.Interval(),
		Timeout:        5 * time.Second,
		RunImmediately: true,
		Run:            svc.Run,
	})
}

// NewIdempotencyCleanupWorker adapts idempotency cleanup maintenance to the worker runtime.
func NewIdempotencyCleanupWorker(svc *IdempotencyCleanupService) (*workerruntime.PeriodicJob, error) {
	if svc == nil {
		return nil, fmt.Errorf("idempotency cleanup service is required")
	}
	return workerruntime.NewPeriodicJob(workerruntime.PeriodicJobSpec{
		Descriptor: workerruntime.Descriptor{
			Name:             "idempotency-cleanup",
			Kind:             workerruntime.KindPeriodic,
			Group:            "maintenance",
			CoordinationMode: workerruntime.CoordinationPerInstance,
		},
		Interval:       svc.Interval(),
		Timeout:        10 * time.Second,
		RunImmediately: true,
		Run:            svc.Run,
	})
}

type usageRecordWorkerPoolWorkerTestHooks struct {
	afterPoolStart func()
	afterPoolStop  func()
}

// UsageRecordWorkerPoolWorker adapts the usage record pool to the worker runtime.
type UsageRecordWorkerPoolWorker struct {
	pool       *UsageRecordWorkerPool
	descriptor workerruntime.Descriptor

	mu             sync.RWMutex
	lifecycle      workerruntime.LifecycleSnapshot
	stopping       bool
	stopDone       chan struct{}
	nativeStopDone chan struct{}
	testHooks      *usageRecordWorkerPoolWorkerTestHooks
}

// NewUsageRecordWorkerPoolWorker returns the runtime component for pool.
func NewUsageRecordWorkerPoolWorker(pool *UsageRecordWorkerPool) workerruntime.Component {
	return &UsageRecordWorkerPoolWorker{
		pool: pool,
		descriptor: workerruntime.Descriptor{
			Name:             "usage-record-pool",
			Kind:             workerruntime.KindPool,
			Group:            "usage",
			CoordinationMode: workerruntime.CoordinationPerInstance,
		},
		lifecycle: workerruntime.LifecycleSnapshot{
			State:     workerruntime.LifecycleStopped,
			UpdatedAt: time.Now(),
		},
	}
}

func (w *UsageRecordWorkerPoolWorker) Descriptor() workerruntime.Descriptor {
	if w == nil {
		return workerruntime.Descriptor{}
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.descriptor
}

func (w *UsageRecordWorkerPoolWorker) Start(context.Context) error {
	if w == nil || w.pool == nil {
		return fmt.Errorf("usage record worker pool is required")
	}
	w.mu.Lock()
	if w.lifecycle.State == workerruntime.LifecycleRunning {
		w.mu.Unlock()
		return nil
	}
	if w.stopping {
		w.mu.Unlock()
		return fmt.Errorf("usage record worker pool is stopping")
	}
	w.lifecycle = workerruntime.LifecycleSnapshot{State: workerruntime.LifecycleStarting, UpdatedAt: time.Now()}
	w.mu.Unlock()

	err := w.pool.Start()
	if w.testHooks != nil && w.testHooks.afterPoolStart != nil {
		w.testHooks.afterPoolStart()
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	// Stop owns lifecycle state from the moment shutdown begins; a concurrent
	// Start must not resurrect Running or Failed while native shutdown drains.
	if w.stopping || w.lifecycle.State != workerruntime.LifecycleStarting {
		if err != nil {
			return err
		}
		return fmt.Errorf("usage record worker pool is stopping")
	}
	if err != nil {
		w.lifecycle = workerruntime.LifecycleSnapshot{State: workerruntime.LifecycleFailed, UpdatedAt: time.Now(), LastError: err.Error()}
		return err
	}
	w.lifecycle = workerruntime.LifecycleSnapshot{State: workerruntime.LifecycleRunning, UpdatedAt: time.Now()}
	return nil
}

func (w *UsageRecordWorkerPoolWorker) Stop(ctx context.Context) error {
	if w == nil || w.pool == nil {
		return nil
	}
	w.mu.Lock()
	if w.stopDone != nil && w.lifecycle.State == workerruntime.LifecycleStopped {
		w.mu.Unlock()
		return nil
	}
	if !w.stopping {
		w.stopping = true
		w.lifecycle = workerruntime.LifecycleSnapshot{State: workerruntime.LifecycleStopping, UpdatedAt: time.Now()}
		w.stopDone = make(chan struct{})
		w.nativeStopDone = make(chan struct{})
		done := w.stopDone
		nativeDone := w.nativeStopDone
		go func() {
			w.pool.Stop()
			close(nativeDone)
			if w.testHooks != nil && w.testHooks.afterPoolStop != nil {
				w.testHooks.afterPoolStop()
			}
			w.mu.Lock()
			w.stopping = false
			w.lifecycle = workerruntime.LifecycleSnapshot{State: workerruntime.LifecycleStopped, UpdatedAt: time.Now()}
			w.mu.Unlock()
			close(done)
		}()
	}
	done := w.stopDone
	nativeDone := w.nativeStopDone
	w.mu.Unlock()

	// Prefer completed shutdown when a caller deadline expires at the same time.
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		select {
		case <-done:
			return nil
		default:
		}

		select {
		case <-nativeDone:
			w.mu.Lock()
			w.stopping = false
			w.lifecycle = workerruntime.LifecycleSnapshot{State: workerruntime.LifecycleStopped, UpdatedAt: time.Now()}
			w.mu.Unlock()
			return nil
		default:
			return ctx.Err()
		}
	}
}

func (w *UsageRecordWorkerPoolWorker) Snapshot() workerruntime.Snapshot {
	if w == nil {
		return workerruntime.Snapshot{}
	}
	w.mu.RLock()
	descriptor := w.descriptor
	lifecycle := w.lifecycle
	stopping := w.stopping
	w.mu.RUnlock()

	stats := UsageRecordWorkerPoolStats{}
	if w.pool != nil {
		stats = w.pool.Stats()
	}
	return workerruntime.Snapshot{
		Descriptor: descriptor,
		Lifecycle:  lifecycle,
		Status: workerruntime.PoolStatus{
			Accepting:          stats.Accepting,
			StillRunning:       stopping || lifecycle.State == workerruntime.LifecycleStopping,
			MaxConcurrency:     stats.MaxConcurrency,
			RunningWorkers:     stats.RunningWorkers,
			WaitingTasks:       stats.WaitingTasks,
			SubmittedTasks:     stats.SubmittedTasks,
			CompletedTasks:     stats.CompletedTasks,
			SuccessfulTasks:    stats.SuccessfulTasks,
			FailedTasks:        stats.FailedTasks,
			DroppedTasks:       stats.DroppedTasks,
			DroppedQueueFull:   stats.DroppedQueueFull,
			DroppedPoolStopped: stats.DroppedPoolStopped,
			SyncFallbackTasks:  stats.SyncFallbackTasks,
		},
	}
}
