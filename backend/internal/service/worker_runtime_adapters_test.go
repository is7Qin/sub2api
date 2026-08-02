//go:build unit

package service

import (
	"context"
	"os"
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
