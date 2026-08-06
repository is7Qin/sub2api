//go:build unit

package service

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"
	"unsafe"

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
