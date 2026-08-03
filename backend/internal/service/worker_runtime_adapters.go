package service

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/workerruntime"
)

const concurrencySlotCleanupWorkerTimeout = 6 * time.Second

// NewConcurrencySlotCleanupWorker adapts expired account-slot cleanup to the worker runtime.
func NewConcurrencySlotCleanupWorker(svc *ConcurrencyService) (*workerruntime.PeriodicJob, error) {
	if svc == nil {
		return nil, fmt.Errorf("concurrency service is required")
	}
	if !svc.CleanupEnabled() {
		return nil, nil
	}
	return workerruntime.NewPeriodicJob(workerruntime.PeriodicJobSpec{
		Descriptor: workerruntime.Descriptor{
			Name:             "concurrency-slot-cleanup",
			Kind:             workerruntime.KindPeriodic,
			Group:            "maintenance",
			CoordinationMode: workerruntime.CoordinationPerInstance,
			Description:      "Removes expired account concurrency slots",
			Tags:             []string{"concurrency", "account-slots", "cleanup"},
		},
		Interval:       svc.CleanupInterval(),
		Timeout:        concurrencySlotCleanupWorkerTimeout,
		RunImmediately: true,
		Run:            svc.RunSlotCleanup,
	})
}

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

// NewSubscriptionExpiryWorker adapts subscription expiry maintenance to the worker runtime.
func NewSubscriptionExpiryWorker(svc *SubscriptionExpiryService) (*workerruntime.PeriodicJob, error) {
	if svc == nil {
		return nil, fmt.Errorf("subscription expiry service is required")
	}
	return workerruntime.NewPeriodicJob(workerruntime.PeriodicJobSpec{
		Descriptor: workerruntime.Descriptor{
			Name:             "subscription-expiry",
			Kind:             workerruntime.KindPeriodic,
			Group:            "maintenance",
			CoordinationMode: workerruntime.CoordinationPerInstance,
			Description:      "Periodically expires subscriptions and sends configured reminders",
			Tags:             []string{"subscriptions", "expiry", "reminders"},
		},
		Interval:       svc.Interval(),
		Timeout:        10 * time.Second,
		RunImmediately: true,
		Run:            svc.Run,
	})
}

const paymentOrderExpiryWorkerTimeout = paymentOrderExpiryLockAcquireTimeout + 2*expiryCheckTimeout + time.Second

// NewPaymentOrderExpiryWorker adapts payment-order expiry maintenance to the worker runtime.
func NewPaymentOrderExpiryWorker(svc *PaymentOrderExpiryService) (*workerruntime.PeriodicJob, error) {
	if svc == nil {
		return nil, fmt.Errorf("payment order expiry service is required")
	}
	return workerruntime.NewPeriodicJob(workerruntime.PeriodicJobSpec{
		Descriptor: workerruntime.Descriptor{
			Name:             "payment-order-expiry",
			Kind:             workerruntime.KindPeriodic,
			Group:            "maintenance",
			CoordinationMode: workerruntime.CoordinationSingletonRun,
			Description:      "Reconciles and expires timed-out payment orders",
			Tags:             []string{"payments", "expiry", "reconciliation"},
		},
		Interval:       svc.Interval(),
		Timeout:        paymentOrderExpiryWorkerTimeout,
		RunImmediately: true,
		Run:            svc.Run,
	})
}

// NewPricingRemoteSyncWorker adapts optional pricing remote synchronization.
func NewPricingRemoteSyncWorker(svc *PricingService) (*workerruntime.PeriodicJob, error) {
	if svc == nil {
		return nil, fmt.Errorf("pricing service is required")
	}
	interval := svc.RemoteSyncInterval()
	if interval <= 0 {
		return nil, nil
	}
	return workerruntime.NewPeriodicJob(workerruntime.PeriodicJobSpec{
		Descriptor: workerruntime.Descriptor{
			Name:             "pricing-remote-sync",
			Kind:             workerruntime.KindPeriodic,
			Group:            "pricing",
			CoordinationMode: workerruntime.CoordinationPerInstance,
			Description:      "Synchronizes configured remote pricing data",
			Tags:             []string{"pricing", "remote-sync"},
		},
		Interval:       interval,
		Timeout:        30 * time.Second,
		RunImmediately: false,
		Run:            svc.RunRemoteSync,
	})
}

// NewTokenRefreshWorker adapts configured OAuth token refresh checks to the worker runtime.
func NewTokenRefreshWorker(svc *TokenRefreshService) (*workerruntime.PeriodicJob, error) {
	if svc == nil {
		return nil, fmt.Errorf("token refresh service is required")
	}
	if !svc.Enabled() {
		slog.Info("token_refresh.service_disabled")
		return nil, nil
	}
	return workerruntime.NewPeriodicJob(workerruntime.PeriodicJobSpec{
		Descriptor: workerruntime.Descriptor{
			Name:             "token-refresh",
			Kind:             workerruntime.KindPeriodic,
			Group:            "auth",
			CoordinationMode: workerruntime.CoordinationPerInstance,
			Description:      "Refreshes eligible OAuth tokens before expiry",
			Tags:             []string{"oauth", "token-refresh"},
		},
		Interval: svc.Interval(),
		// Legacy refresh checks had no per-run deadline; retain that behavior while
		// letting the runtime root context drive truthful shutdown.
		Timeout:        100 * 365 * 24 * time.Hour,
		RunImmediately: true,
		Run:            svc.Run,
		OnStart: func() {
			slog.Info("token_refresh.service_started",
				"check_interval_minutes", svc.cfg.CheckIntervalMinutes,
				"refresh_before_expiry_hours", svc.cfg.RefreshBeforeExpiryHours,
			)
		},
		OnStop: func() {
			slog.Info("token_refresh.service_stopped")
		},
	})
}

// The legacy cycle allowed a ten-second scan plus up to 1000 sequential two-second
// releases. Keep the runtime deadline above that maximum so it only governs shutdown.
const userMessageQueueCleanupWorkerTimeout = 10*time.Second + 1000*2*time.Second + time.Second

// NewUserMessageQueueCleanupWorker adapts orphan-lock cleanup to the worker runtime.
func NewUserMessageQueueCleanupWorker(svc *UserMessageQueueService) (*workerruntime.PeriodicJob, error) {
	if svc == nil {
		return nil, fmt.Errorf("user message queue service is required")
	}
	if !svc.CleanupEnabled() {
		return nil, nil
	}
	return workerruntime.NewPeriodicJob(workerruntime.PeriodicJobSpec{
		Descriptor: workerruntime.Descriptor{
			Name:             "user-message-queue-cleanup",
			Kind:             workerruntime.KindPeriodic,
			Group:            "maintenance",
			CoordinationMode: workerruntime.CoordinationPerInstance,
			Description:      "Releases orphaned user-message queue locks",
			Tags:             []string{"user-message-queue", "orphan-lock-cleanup"},
		},
		Interval:       svc.CleanupInterval(),
		Timeout:        userMessageQueueCleanupWorkerTimeout,
		RunImmediately: false,
		Run:            svc.RunCleanup,
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

// NewOutboxCleanupWorker adapts outbox retention cleanup to the worker runtime.
// 该任务为 singleton（每周期仅 leader 副本执行），协调逻辑见 OutboxCleanupService.Run。
func NewOutboxCleanupWorker(svc *OutboxCleanupService) (*workerruntime.PeriodicJob, error) {
	if svc == nil {
		return nil, fmt.Errorf("outbox cleanup service is required")
	}
	return workerruntime.NewPeriodicJob(workerruntime.PeriodicJobSpec{
		Descriptor: workerruntime.Descriptor{
			Name:             "outbox-cleanup",
			Kind:             workerruntime.KindPeriodic,
			Group:            "maintenance",
			CoordinationMode: workerruntime.CoordinationSingletonRun,
		},
		Interval:       svc.Interval(),
		Timeout:        outboxCleanupCycleTimeout,
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
	stopStarted    chan struct{}
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

// StopInitiated returns a channel closed when Stop has entered.
func (w *UsageRecordWorkerPoolWorker) StopInitiated() <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopStarted == nil {
		w.stopStarted = make(chan struct{})
	}
	return w.stopStarted
}

func (w *UsageRecordWorkerPoolWorker) Stop(ctx context.Context) error {
	if w == nil || w.pool == nil {
		return nil
	}
	w.mu.Lock()
	if w.stopStarted == nil {
		w.stopStarted = make(chan struct{})
	}
	close(w.stopStarted)
	w.stopStarted = nil
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
