package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/workerruntime"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestProvideWorkerRuntimeRegistersAndStartsPilots(t *testing.T) {
	usagePool := service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{
		WorkerCount: 1,
		QueueSize:   1,
	})
	runtime, err := provideWorkerRuntime(
		service.NewAccountExpiryService(nil, time.Hour),
		service.NewIdempotencyCleanupService(nil, &config.Config{}),
		usagePool,
		service.NewSubscriptionExpiryService(nil, time.Hour),
		service.NewPaymentOrderExpiryService(nil, time.Hour),
		service.NewPricingService(&config.Config{}, nil),
		service.NewOutboxCleanupService(nil, nil, nil, 30*24*time.Hour),
		service.NewTokenRefreshService(nil, nil, nil, nil, nil, nil, nil, &config.Config{}, nil),
		service.NewOAuthService(nil, nil),
		service.NewGeminiOAuthService(nil, nil, nil, nil, &config.Config{}),
		service.NewAntigravityOAuthService(nil),
		service.NewOpenAIOAuthService(nil, nil),
		service.NewUserMessageQueueService(nil, nil, &config.UserMessageQueueConfig{}),
		service.NewConcurrencyService(nil),
		service.NewEmailQueueService(nil, 1),
		service.NewOpsSystemLogSink(&serverOpsRepositoryStub{}),
		service.NewChannelMonitorV2Aggregator(nil, nil, nil, nil),
		nil,
		nil,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = runtime.StopAll(context.Background()) })

	snapshots := runtime.Snapshot()
	require.Equal(t, []string{"account-expiry", "antigravity-oauth-session-cleanup", "channel-monitor-v2-aggregation", "claude-oauth-session-cleanup", "email-queue", "gemini-oauth-session-cleanup", "idempotency-cleanup", "openai-oauth-session-cleanup", "ops-system-log-sink", "outbox-cleanup", "payment-order-expiry", "subscription-expiry", "usage-record-pool"}, snapshotNames(snapshots))
	var sinkSnapshots int
	for _, snapshot := range snapshots {
		if snapshot.Descriptor.Name == "ops-system-log-sink" {
			sinkSnapshots++
			require.Equal(t, workerruntime.KindPool, snapshot.Descriptor.Kind)
			require.Equal(t, "ops", snapshot.Descriptor.Group)
			require.Equal(t, workerruntime.CoordinationPerInstance, snapshot.Descriptor.CoordinationMode)
			require.Equal(t, workerruntime.LifecycleRunning, snapshot.Lifecycle.State)
			require.IsType(t, workerruntime.PoolStatus{}, snapshot.Status)
		}
	}
	require.Equal(t, 1, sinkSnapshots)
	for _, snapshot := range snapshots {
		require.Equal(t, workerruntime.LifecycleRunning, snapshot.Lifecycle.State)
	}
	// Status is the public kind-specific runtime API; do not depend on obsolete snapshot fields.
	for _, snapshot := range snapshots {
		if snapshot.Descriptor.Kind == workerruntime.KindPool {
			require.IsType(t, workerruntime.PoolStatus{}, snapshot.Status)
			continue
		}
		require.IsType(t, workerruntime.PeriodicStatus{}, snapshot.Status)
	}
	require.True(t, usagePool.Accepting())
	antigravitySnapshot := snapshots[1]
	require.Equal(t, "antigravity-oauth-session-cleanup", antigravitySnapshot.Descriptor.Name)
	require.Equal(t, workerruntime.LifecycleRunning, antigravitySnapshot.Lifecycle.State)
	require.IsType(t, workerruntime.PeriodicStatus{}, antigravitySnapshot.Status)
}

func TestProvideWorkerRuntimeRegistersOpenAIOAuthMarkerCleanupOnlyForRedisStore(t *testing.T) {
	newRuntime := func(openAIOAuth *service.OpenAIOAuthService) *workerruntime.Runtime {
		runtime, err := provideWorkerRuntime(
			service.NewAccountExpiryService(nil, time.Hour),
			service.NewIdempotencyCleanupService(nil, &config.Config{}),
			service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{WorkerCount: 1, QueueSize: 1}),
			service.NewSubscriptionExpiryService(nil, time.Hour),
			service.NewPaymentOrderExpiryService(nil, time.Hour),
			service.NewPricingService(&config.Config{}, nil),
			service.NewOutboxCleanupService(nil, nil, nil, 30*24*time.Hour),
			service.NewTokenRefreshService(nil, nil, nil, nil, nil, nil, nil, &config.Config{}, nil),
			service.NewOAuthService(nil, nil),
			service.NewGeminiOAuthService(nil, nil, nil, nil, &config.Config{}),
			service.NewAntigravityOAuthService(nil),
			openAIOAuth,
			service.NewUserMessageQueueService(nil, nil, &config.UserMessageQueueConfig{}),
			service.NewConcurrencyService(nil),
			service.NewEmailQueueService(nil, 1),
			service.NewOpsSystemLogSink(&serverOpsRepositoryStub{}),
			service.NewChannelMonitorV2Aggregator(nil, nil, nil, nil),
			nil,
			nil,
		)
		require.NoError(t, err)
		t.Cleanup(func() { _, _ = runtime.StopAll(context.Background()) })
		return runtime
	}

	memoryNames := snapshotNames(newRuntime(service.NewOpenAIOAuthService(nil, nil)).Snapshot())
	require.Contains(t, memoryNames, "openai-oauth-session-cleanup")
	require.NotContains(t, memoryNames, "openai-oauth-redis-set-failure-cleanup")

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	redisNames := snapshotNames(newRuntime(service.NewOpenAIOAuthServiceWithRedis(nil, nil, rdb)).Snapshot())
	require.Contains(t, redisNames, "openai-oauth-session-cleanup")
	require.Contains(t, redisNames, "openai-oauth-redis-set-failure-cleanup")
}

func TestProvideWorkerRuntimeRegistersTokenRefreshOnlyWhenEnabled(t *testing.T) {
	usagePool := service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{WorkerCount: 1, QueueSize: 1})
	disabledCfg := &config.Config{}
	disabled := service.NewTokenRefreshService(nil, nil, nil, nil, nil, nil, nil, disabledCfg, nil)

	runtime, err := provideWorkerRuntime(
		service.NewAccountExpiryService(nil, time.Hour),
		service.NewIdempotencyCleanupService(nil, &config.Config{}),
		usagePool,
		service.NewSubscriptionExpiryService(nil, time.Hour),
		service.NewPaymentOrderExpiryService(nil, time.Hour),
		service.NewPricingService(&config.Config{}, nil),
		service.NewOutboxCleanupService(nil, nil, nil, 30*24*time.Hour),
		disabled,
		service.NewOAuthService(nil, nil),
		service.NewGeminiOAuthService(nil, nil, nil, nil, &config.Config{}),
		service.NewAntigravityOAuthService(nil),
		service.NewOpenAIOAuthService(nil, nil),
		service.NewUserMessageQueueService(nil, nil, &config.UserMessageQueueConfig{}),
		service.NewConcurrencyService(nil),
		service.NewEmailQueueService(nil, 1),
		service.NewOpsSystemLogSink(&serverOpsRepositoryStub{}),
		service.NewChannelMonitorV2Aggregator(nil, nil, nil, nil),
		nil,
		nil,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = runtime.StopAll(context.Background()) })
	require.NotContains(t, snapshotNames(runtime.Snapshot()), "token-refresh")

	enabledCfg := &config.Config{}
	enabledCfg.TokenRefresh.Enabled = true
	enabledCfg.TokenRefresh.CheckIntervalMinutes = 60
	enabled := service.NewTokenRefreshService(nil, nil, nil, nil, nil, nil, nil, enabledCfg, nil)
	enabledPool := service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{WorkerCount: 1, QueueSize: 1})
	enabledRuntime, err := provideWorkerRuntime(
		service.NewAccountExpiryService(nil, time.Hour),
		service.NewIdempotencyCleanupService(nil, &config.Config{}),
		enabledPool,
		service.NewSubscriptionExpiryService(nil, time.Hour),
		service.NewPaymentOrderExpiryService(nil, time.Hour),
		service.NewPricingService(&config.Config{}, nil),
		service.NewOutboxCleanupService(nil, nil, nil, 30*24*time.Hour),
		enabled,
		service.NewOAuthService(nil, nil),
		service.NewGeminiOAuthService(nil, nil, nil, nil, &config.Config{}),
		service.NewAntigravityOAuthService(nil),
		service.NewOpenAIOAuthService(nil, nil),
		service.NewUserMessageQueueService(nil, nil, &config.UserMessageQueueConfig{}),
		service.NewConcurrencyService(nil),
		service.NewEmailQueueService(nil, 1),
		service.NewOpsSystemLogSink(&serverOpsRepositoryStub{}),
		service.NewChannelMonitorV2Aggregator(nil, nil, nil, nil),
		nil,
		nil,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = enabledRuntime.StopAll(context.Background()) })
	require.Contains(t, snapshotNames(enabledRuntime.Snapshot()), "token-refresh")
}

func TestProvideWorkerRuntimeRegistersUserMessageQueueCleanupOnlyWhenEnabled(t *testing.T) {
	newRuntime := func(cache service.UserMsgQueueCache, interval int) *workerruntime.Runtime {
		runtime, err := provideWorkerRuntime(
			service.NewAccountExpiryService(nil, time.Hour),
			service.NewIdempotencyCleanupService(nil, &config.Config{}),
			service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{WorkerCount: 1, QueueSize: 1}),
			service.NewSubscriptionExpiryService(nil, time.Hour),
			service.NewPaymentOrderExpiryService(nil, time.Hour),
			service.NewPricingService(&config.Config{}, nil),
			service.NewOutboxCleanupService(nil, nil, nil, 30*24*time.Hour),
			service.NewTokenRefreshService(nil, nil, nil, nil, nil, nil, nil, &config.Config{}, nil),
			service.NewOAuthService(nil, nil),
			service.NewGeminiOAuthService(nil, nil, nil, nil, &config.Config{}),
			service.NewAntigravityOAuthService(nil),
			service.NewOpenAIOAuthService(nil, nil),
			service.NewUserMessageQueueService(cache, nil, &config.UserMessageQueueConfig{CleanupIntervalSeconds: interval}),
			service.NewConcurrencyService(nil),
			service.NewEmailQueueService(nil, 1),
			service.NewOpsSystemLogSink(&serverOpsRepositoryStub{}),
			service.NewChannelMonitorV2Aggregator(nil, nil, nil, nil),
			nil,
			nil,
		)
		require.NoError(t, err)
		t.Cleanup(func() { _, _ = runtime.StopAll(context.Background()) })
		return runtime
	}

	require.NotContains(t, snapshotNames(newRuntime(nil, 60).Snapshot()), "user-message-queue-cleanup")
	require.NotContains(t, snapshotNames(newRuntime(nil, 0).Snapshot()), "user-message-queue-cleanup")

	enabled := newRuntime(&serverUserMessageQueueCacheStub{}, 60)
	require.Contains(t, snapshotNames(enabled.Snapshot()), "user-message-queue-cleanup")
}

func TestProvideWorkerRuntimeRegistersConcurrencySlotCleanupOnlyWhenEnabled(t *testing.T) {
	newRuntime := func(cache service.ConcurrencyCache, interval time.Duration) *workerruntime.Runtime {
		concurrency := service.NewConcurrencyService(cache)
		concurrency.ConfigureSlotCleanup(interval)
		runtime, err := provideWorkerRuntime(
			service.NewAccountExpiryService(nil, time.Hour),
			service.NewIdempotencyCleanupService(nil, &config.Config{}),
			service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{WorkerCount: 1, QueueSize: 1}),
			service.NewSubscriptionExpiryService(nil, time.Hour),
			service.NewPaymentOrderExpiryService(nil, time.Hour),
			service.NewPricingService(&config.Config{}, nil),
			service.NewOutboxCleanupService(nil, nil, nil, 30*24*time.Hour),
			service.NewTokenRefreshService(nil, nil, nil, nil, nil, nil, nil, &config.Config{}, nil),
			service.NewOAuthService(nil, nil),
			service.NewGeminiOAuthService(nil, nil, nil, nil, &config.Config{}),
			service.NewAntigravityOAuthService(nil),
			service.NewOpenAIOAuthService(nil, nil),
			service.NewUserMessageQueueService(nil, nil, &config.UserMessageQueueConfig{}),
			concurrency,
			service.NewEmailQueueService(nil, 1),
			service.NewOpsSystemLogSink(&serverOpsRepositoryStub{}),
			service.NewChannelMonitorV2Aggregator(nil, nil, nil, nil),
			nil,
			nil,
		)
		require.NoError(t, err)
		t.Cleanup(func() { _, _ = runtime.StopAll(context.Background()) })
		return runtime
	}

	require.NotContains(t, snapshotNames(newRuntime(nil, time.Minute).Snapshot()), "concurrency-slot-cleanup")
	require.NotContains(t, snapshotNames(newRuntime(&serverConcurrencyCacheStub{}, 0).Snapshot()), "concurrency-slot-cleanup")
	require.Contains(t, snapshotNames(newRuntime(&serverConcurrencyCacheStub{}, time.Minute).Snapshot()), "concurrency-slot-cleanup")
}

func TestProvideWorkerRuntimeStartsWithMissingSupportGenerationAndForeignOwnership(t *testing.T) {
	store := &serverSupportDecisionStore{documents: make(map[uint64][]byte)}
	publisher := service.NewSchedulerSupportPublisherWorker(
		&serverSchedulerDirtyRepo{},
		&serverSchedulerOwnershipRepo{},
		serverSchedulerDirtyProcessor{},
		service.NewSupportDecisionPublisher(serverSupportDecisionGeneration{}, serverSupportDecisionSource{}, store, &config.Config{}),
		store,
		&config.Config{},
	)
	reader := service.NewSupportDecisionAtomicReader(time.Hour)
	replica := service.NewSupportDecisionReplicaWorker(service.NewSupportDecisionReplica(store, reader))

	started := make(chan struct{})
	var runtime *workerruntime.Runtime
	var err error
	go func() {
		runtime, err = provideWorkerRuntime(
			service.NewAccountExpiryService(nil, time.Hour),
			service.NewIdempotencyCleanupService(nil, &config.Config{}),
			service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{WorkerCount: 1, QueueSize: 1}),
			service.NewSubscriptionExpiryService(nil, time.Hour),
			service.NewPaymentOrderExpiryService(nil, time.Hour),
			service.NewPricingService(&config.Config{}, nil),
			service.NewOutboxCleanupService(nil, nil, nil, 30*24*time.Hour),
			service.NewTokenRefreshService(nil, nil, nil, nil, nil, nil, nil, &config.Config{}, nil),
			service.NewOAuthService(nil, nil),
			service.NewGeminiOAuthService(nil, nil, nil, nil, &config.Config{}),
			service.NewAntigravityOAuthService(nil),
			service.NewOpenAIOAuthService(nil, nil),
			service.NewUserMessageQueueService(nil, nil, &config.UserMessageQueueConfig{}),
			service.NewConcurrencyService(nil),
			service.NewEmailQueueService(nil, 1),
			service.NewOpsSystemLogSink(&serverOpsRepositoryStub{}),
			publisher,
			replica,
		)
		close(started)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker runtime startup waited for support publication")
	}
	require.NoError(t, err)
	require.NotNil(t, runtime)
	require.Equal(t, workerruntime.LifecycleRunning, publisher.Snapshot().Lifecycle.State)
	require.Equal(t, workerruntime.LifecycleRunning, replica.Snapshot().Lifecycle.State)
	require.Equal(t, service.SupportDecisionUnknown, reader.Lookup(service.SupportDecisionQuery{}))

	table, buildErr := service.BuildSupportDecisionTable(&service.SupportDecisionConstructionSnapshot{}, service.SupportDecisionBuildOptions{Generation: 1})
	require.NoError(t, buildErr)
	payload, encodeErr := service.EncodeSupportDecisionDocument(table)
	require.NoError(t, encodeErr)
	store.publish(1, payload)
	require.Eventually(t, func() bool { return reader.Snapshot().Generation == 1 }, time.Second, time.Millisecond)

	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = runtime.StopAll(stopCtx)
	require.NoError(t, err)
}

func TestProvideWorkerRuntimeRegistersSupportPublisherAndReplica(t *testing.T) {
	store := newServerSupportDecisionStore(t)
	publisher := service.NewSchedulerSupportPublisherWorker(
		&serverSchedulerDirtyRepo{},
		&serverSchedulerOwnershipRepo{},
		serverSchedulerDirtyProcessor{},
		service.NewSupportDecisionPublisher(serverSupportDecisionGeneration{}, serverSupportDecisionSource{}, store, &config.Config{}),
		store,
		&config.Config{},
	)
	replica := service.NewSupportDecisionReplicaWorker(service.NewSupportDecisionReplica(store, service.NewSupportDecisionAtomicReader(time.Hour)))
	usagePool := service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{WorkerCount: 1, QueueSize: 1})
	runtime, err := provideWorkerRuntime(
		service.NewAccountExpiryService(nil, time.Hour),
		service.NewIdempotencyCleanupService(nil, &config.Config{}),
		usagePool,
		service.NewSubscriptionExpiryService(nil, time.Hour),
		service.NewPaymentOrderExpiryService(nil, time.Hour),
		service.NewPricingService(&config.Config{}, nil),
		service.NewOutboxCleanupService(nil, nil, nil, 30*24*time.Hour),
		service.NewTokenRefreshService(nil, nil, nil, nil, nil, nil, nil, &config.Config{}, nil),
		service.NewOAuthService(nil, nil),
		service.NewGeminiOAuthService(nil, nil, nil, nil, &config.Config{}),
		service.NewAntigravityOAuthService(nil),
		service.NewOpenAIOAuthService(nil, nil),
		service.NewUserMessageQueueService(nil, nil, &config.UserMessageQueueConfig{}),
		service.NewConcurrencyService(nil),
		service.NewEmailQueueService(nil, 1),
		service.NewOpsSystemLogSink(&serverOpsRepositoryStub{}),
		service.NewChannelMonitorV2Aggregator(nil, nil, nil, nil),
		publisher,
		replica,
	)
	require.NoError(t, err)
	require.Contains(t, snapshotNames(runtime.Snapshot()), "scheduler-support-publisher")
	require.Contains(t, snapshotNames(runtime.Snapshot()), "scheduler-support-replica")
	require.Equal(t, workerruntime.LifecycleRunning, publisher.Snapshot().Lifecycle.State)
	require.Equal(t, workerruntime.LifecycleRunning, replica.Snapshot().Lifecycle.State)

	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	results, err := runtime.StopAll(stopCtx)
	require.NoError(t, err)
	require.Len(t, results, len(runtime.Snapshot()))
	require.Equal(t, workerruntime.LifecycleStopped, publisher.Snapshot().Lifecycle.State)
	require.Equal(t, workerruntime.LifecycleStopped, replica.Snapshot().Lifecycle.State)
}

func TestProvideWorkerRuntimeRollsBackPublisherWhenReplicaStartupFails(t *testing.T) {
	store := newServerSupportDecisionStore(t)
	publisher := service.NewSchedulerSupportPublisherWorker(
		&serverSchedulerDirtyRepo{},
		&serverSchedulerOwnershipRepo{},
		serverSchedulerDirtyProcessor{},
		service.NewSupportDecisionPublisher(serverSupportDecisionGeneration{}, serverSupportDecisionSource{}, store, &config.Config{}),
		store,
		&config.Config{},
	)
	badReplica := service.NewSupportDecisionReplicaWorker(service.NewSupportDecisionReplica(nil, service.NewSupportDecisionAtomicReader(time.Hour)))
	_, err := provideWorkerRuntime(
		service.NewAccountExpiryService(nil, time.Hour),
		service.NewIdempotencyCleanupService(nil, &config.Config{}),
		service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{WorkerCount: 1, QueueSize: 1}),
		service.NewSubscriptionExpiryService(nil, time.Hour),
		service.NewPaymentOrderExpiryService(nil, time.Hour),
		service.NewPricingService(&config.Config{}, nil),
		service.NewOutboxCleanupService(nil, nil, nil, 30*24*time.Hour),
		service.NewTokenRefreshService(nil, nil, nil, nil, nil, nil, nil, &config.Config{}, nil),
		service.NewOAuthService(nil, nil),
		service.NewGeminiOAuthService(nil, nil, nil, nil, &config.Config{}),
		service.NewAntigravityOAuthService(nil),
		service.NewOpenAIOAuthService(nil, nil),
		service.NewUserMessageQueueService(nil, nil, &config.UserMessageQueueConfig{}),
		service.NewConcurrencyService(nil),
		service.NewEmailQueueService(nil, 1),
		service.NewOpsSystemLogSink(&serverOpsRepositoryStub{}),
		service.NewChannelMonitorV2Aggregator(nil, nil, nil, nil),
		publisher,
		badReplica,
	)
	require.Error(t, err)
	require.Equal(t, workerruntime.LifecycleStopped, publisher.Snapshot().Lifecycle.State)
}

func TestWorkerProvidersDoNotStartSupportDecisionGoroutines(t *testing.T) {
	content, err := os.ReadFile("../../internal/service/wire.go")
	require.NoError(t, err)
	serviceWire := string(content)
	require.NotContains(t, functionSource(serviceWire, "ProvideSupportDecisionAtomicReader"), ".Start(")
	require.NotContains(t, functionSource(serviceWire, "ProvideSchedulerSnapshotDirtyProcessor"), ".Start(")
}

func TestChannelMonitorV2AggregationLifecycleIsRuntimeOwned(t *testing.T) {
	workerRuntimeSource, err := os.ReadFile("worker_runtime.go")
	require.NoError(t, err)
	require.Contains(t, string(workerRuntimeSource), "service.NewChannelMonitorV2AggregationWorker")

	serviceWireSource, err := os.ReadFile("../../internal/service/wire.go")
	require.NoError(t, err)
	provider := functionSource(string(serviceWireSource), "ProvideChannelMonitorV2Aggregator")
	require.NotContains(t, provider, ".Start(")
}

func TestConcurrencySlotCleanupLifecycleIsRuntimeOwned(t *testing.T) {
	workerRuntimeSource, err := os.ReadFile("worker_runtime.go")
	require.NoError(t, err)
	require.Contains(t, string(workerRuntimeSource), "service.NewConcurrencySlotCleanupWorker")

	serviceWireSource, err := os.ReadFile("../../internal/service/wire.go")
	require.NoError(t, err)
	provideConcurrency := functionSource(string(serviceWireSource), "ProvideConcurrencyService")
	require.NotContains(t, provideConcurrency, "StartSlotCleanupWorker")
}

func TestCleanupStopsRuntimeBeforeInfrastructure(t *testing.T) {
	order := make([]string, 0, 3)
	runtime := &cleanupRuntimeSpy{order: &order}

	runCleanup(context.Background(), runtime, nil, []cleanupStep{
		{name: "Redis", fn: func() error {
			order = append(order, "redis")
			return nil
		}},
		{name: "Ent", fn: func() error {
			order = append(order, "ent")
			return nil
		}},
	})

	require.Equal(t, []string{"runtime", "redis", "ent"}, order)
}

func TestWorkerProvidersAndLegacyCleanupDoNotOwnPilotLifecycle(t *testing.T) {
	content, err := os.ReadFile("../../internal/service/wire.go")
	require.NoError(t, err)
	serviceWire := string(content)
	require.NotContains(t, functionSource(serviceWire, "ProvideAccountExpiryService"), ".Start()")
	require.NotContains(t, functionSource(serviceWire, "ProvideIdempotencyCleanupService"), ".Start()")
	require.NotContains(t, functionSource(serviceWire, "ProvideSubscriptionExpiryService"), ".Start()")
	require.NotContains(t, functionSource(serviceWire, "ProvidePaymentOrderExpiryService"), ".Start()")
	require.NotContains(t, functionSource(serviceWire, "ProvideTokenRefreshService"), ".Start()")
	require.NotContains(t, functionSource(serviceWire, "ProvideUserMessageQueueService"), "StartCleanupWorker")

	serverWire, err := os.ReadFile("wire.go")
	require.NoError(t, err)
	legacyCleanup := string(serverWire)
	require.NotContains(t, legacyCleanup, "accountExpiry.Stop()")
	require.NotContains(t, legacyCleanup, "idempotencyCleanup.Stop()")
	require.NotContains(t, legacyCleanup, "usageRecordWorkerPool.Stop()")
	require.NotContains(t, legacyCleanup, "subscriptionExpiry.Stop()")
	require.NotContains(t, legacyCleanup, "paymentOrderExpiry.Stop()")
	require.NotContains(t, legacyCleanup, "pricing.Stop()")
	require.NotContains(t, legacyCleanup, "tokenRefresh.Stop()")
	require.NotContains(t, legacyCleanup, "geminiOAuth.Stop()")
	require.NotContains(t, legacyCleanup, "antigravityOAuth.Stop()")
	require.NotContains(t, functionSource(legacyCleanup, "provideCleanup"), "geminiOAuth *service.GeminiOAuthService")
	require.NotContains(t, functionSource(legacyCleanup, "provideCleanup"), "antigravityOAuth *service.AntigravityOAuthService")
	require.NotContains(t, legacyCleanup, "openaiOAuth.Stop()")
	require.NotContains(t, functionSource(legacyCleanup, "provideCleanup"), "openaiOAuth *service.OpenAIOAuthService")
	require.NotContains(t, functionSource(legacyCleanup, "provideCleanup"), "opsSystemLogSink *service.OpsSystemLogSink")
	require.NotContains(t, legacyCleanup, `{"OpsSystemLogSink"`)

	provider := functionSource(serviceWire, "ProvideOpsSystemLogSink")
	require.NotContains(t, provider, "sink.Start()")
	require.Contains(t, provider, "logger.SetSink(sink)")
}

type serverOpsRepositoryStub struct {
	service.OpsRepository
}

func (serverOpsRepositoryStub) BatchInsertSystemLogs(context.Context, []*service.OpsInsertSystemLogInput) (int64, error) {
	return 0, nil
}

type serverConcurrencyCacheStub struct {
	service.ConcurrencyCache
}

func (serverConcurrencyCacheStub) CleanupExpiredAccountSlotKeys(context.Context) error { return nil }

type serverUserMessageQueueCacheStub struct{}

func (serverUserMessageQueueCacheStub) AcquireLock(context.Context, int64, string, int) (bool, error) {
	return false, nil
}
func (serverUserMessageQueueCacheStub) ReleaseLock(context.Context, int64, string) (bool, error) {
	return false, nil
}
func (serverUserMessageQueueCacheStub) GetLastCompletedMs(context.Context, int64) (int64, error) {
	return 0, nil
}
func (serverUserMessageQueueCacheStub) GetCurrentTimeMs(context.Context) (int64, error) {
	return 0, nil
}
func (serverUserMessageQueueCacheStub) ForceReleaseLock(context.Context, int64) error { return nil }
func (serverUserMessageQueueCacheStub) ScanLockKeys(context.Context, int) ([]int64, error) {
	return nil, nil
}

type cleanupRuntimeSpy struct {
	order *[]string
}

func (s *cleanupRuntimeSpy) StopAll(context.Context) ([]workerruntime.StopResult, error) {
	*s.order = append(*s.order, "runtime")
	return []workerruntime.StopResult{{Name: "usage-record-pool", Outcome: workerruntime.StopCompleted}}, nil
}

func snapshotNames(snapshots []workerruntime.Snapshot) []string {
	names := make([]string, 0, len(snapshots))
	for _, snapshot := range snapshots {
		names = append(names, snapshot.Descriptor.Name)
	}
	return names
}

func functionSource(source, name string) string {
	start := strings.Index(source, "func "+name)
	if start < 0 {
		return ""
	}
	end := strings.Index(source[start:], "\nfunc ")
	if end < 0 {
		return source[start:]
	}
	return source[start : start+end]
}
