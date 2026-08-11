package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestProvideServiceBuildInfo(t *testing.T) {
	in := handler.BuildInfo{
		Version:   "v-test",
		BuildType: "release",
	}
	out := provideServiceBuildInfo(in)
	require.Equal(t, in.Version, out.Version)
	require.Equal(t, in.BuildType, out.BuildType)
}

type failingShutdownServer struct {
	called bool
}

func (s *failingShutdownServer) Shutdown(context.Context) error {
	s.called = true
	return errors.New("shutdown failed")
}

func TestShutdownErrorReturnsSoDeferredCleanupRuns(t *testing.T) {
	server := &failingShutdownServer{}
	cleaned := false

	func() {
		defer func() { cleaned = true }()
		shutdownServer(context.Background(), server)
	}()

	require.True(t, server.called)
	require.True(t, cleaned, "shutdown errors must return normally so application cleanup can run")
}

func TestProvideCleanup_WithMinimalDependencies_NoPanic(t *testing.T) {
	cfg := &config.Config{}

	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	schedulerSnapshotSvc := service.NewSchedulerSnapshotService(nil, nil, nil, nil, cfg)

	cleanup := provideCleanup(
		nil, // entClient
		nil, // redis
		&service.OpsMetricsCollector{},
		&service.OpsAggregationService{},
		&service.OpsAlertEvaluatorService{},
		&service.OpsCleanupService{},
		&service.OpsScheduledReportService{},
		nil, // apiKeyService
		schedulerSnapshotSvc,
		&service.UsageCleanupService{},
		billingCacheSvc,
		&service.SubscriptionService{},
		&service.OpenAIOAuthStartupConfigValidation{},
		nil, // openAIGateway
		nil, // scheduledTestRunner
		nil, // backupSvc
		nil, // channelMonitorRunner
		nil, // quotaFlusher
		nil, // billingOutboxWorker
		nil, // runtime
	)

	require.NotPanics(t, func() {
		cleanup()
	})
}

func TestEmailQueueLifecycleIsRuntimeOwned(t *testing.T) {
	workerRuntimeSource, err := os.ReadFile("worker_runtime.go")
	require.NoError(t, err)
	require.Contains(t, string(workerRuntimeSource), "emailQueue *service.EmailQueueService")
	require.Contains(t, string(workerRuntimeSource), "service.NewEmailQueueWorker(emailQueue)")

	wireSource, err := os.ReadFile("wire.go")
	require.NoError(t, err)
	require.NotContains(t, string(wireSource), "emailQueue *service.EmailQueueService")
	require.NotContains(t, string(wireSource), `{"EmailQueueService"`)
}

func TestEmailQueueInventoryIsRuntimeManaged(t *testing.T) {
	content, err := os.ReadFile("../../../docs/worker-runtime-inventory.md")
	require.NoError(t, err)
	require.Contains(t, string(content), "| EmailQueueService | Yes | `cmd/server/provideWorkerRuntime` | `workerruntime` pool adapter | Server runtime | `Runtime.StopAll` waits to deadline and reports still-running truthfully | per-instance | Ops worker status | Phase 6 |")
}

func TestGeminiOAuthInventoryAndWireGraphAreRuntimeManaged(t *testing.T) {
	inventory, err := os.ReadFile("../../../docs/worker-runtime-inventory.md")
	require.NoError(t, err)
	require.Contains(t, string(inventory), "| Gemini OAuth session cleanup | Yes | `cmd/server/provideWorkerRuntime` | `workerruntime.PeriodicJob` (delayed fixed-delay 5-minute interval) | Server runtime | `Runtime.StopAll` waits to deadline and reports timeout | per-instance in-memory session map protected by an RW mutex | Ops worker status | Phase 8 |")

	wireGen, err := os.ReadFile("wire_gen.go")
	require.NoError(t, err)
	generated := string(wireGen)
	initialize := functionSource(generated, "initializeApplication")
	require.Contains(t, initialize, "provideWorkerRuntime(accountExpiryService, idempotencyCleanupService, usageRecordWorkerPool, subscriptionExpiryService, paymentOrderExpiryService, pricingService, outboxCleanupService, tokenRefreshService, oAuthService, geminiOAuthService,")
	require.NotContains(t, functionSource(generated, "provideCleanup"), "geminiOAuth *service.GeminiOAuthService")
}

func TestAntigravityOAuthInventoryAndWireGraphAreRuntimeManaged(t *testing.T) {
	inventory, err := os.ReadFile("../../../docs/worker-runtime-inventory.md")
	require.NoError(t, err)
	require.Contains(t, string(inventory), "| Antigravity OAuth session cleanup | Yes | `cmd/server/provideWorkerRuntime` | `workerruntime.PeriodicJob` (delayed fixed-delay 5-minute interval) | Server runtime | `Runtime.StopAll` waits to deadline and reports timeout | per-instance in-memory session map protected by an RW mutex | Ops worker status | Phase 9 |")
	require.Contains(t, string(inventory), "| OpenAI OAuth pending-session cleanup | Yes |")

	wireGen, err := os.ReadFile("wire_gen.go")
	require.NoError(t, err)
	generated := string(wireGen)
	initialize := functionSource(generated, "initializeApplication")
	require.Contains(t, initialize, "provideWorkerRuntime(accountExpiryService, idempotencyCleanupService, usageRecordWorkerPool, subscriptionExpiryService, paymentOrderExpiryService, pricingService, outboxCleanupService, tokenRefreshService, oAuthService, geminiOAuthService, antigravityOAuthService,")
	require.Contains(t, initialize, "admin.NewAntigravityOAuthHandler(antigravityOAuthService)")
	require.NotContains(t, functionSource(generated, "provideCleanup"), "antigravityOAuth *service.AntigravityOAuthService")
	require.NotContains(t, initialize, "provideCleanup(client, redisClient, opsMetricsCollector, opsAggregationService, opsAlertEvaluatorService, opsCleanupService, opsScheduledReportService, opsSystemLogSink, authCacheInvalidationWorker, apiKeyService, schedulerSnapshotService, usageCleanupService, billingCacheService, subscriptionService, openAIOAuthStartupConfigValidation, openAIOAuthService, antigravityOAuthService,")
}

func TestOpenAIOAuthInventoryAndWireGraphAreRuntimeManaged(t *testing.T) {
	inventory, err := os.ReadFile("../../../docs/worker-runtime-inventory.md")
	require.NoError(t, err)
	require.Contains(t, string(inventory), "| OpenAI OAuth pending-session cleanup | Yes |")
	require.Contains(t, string(inventory), "| OpenAI OAuth Redis write-failure fallback-marker cleanup | Yes, when Redis-backed pending session storage is configured |")

	wireGen, err := os.ReadFile("wire_gen.go")
	require.NoError(t, err)
	initialize := functionSource(string(wireGen), "initializeApplication")
	require.Contains(t, initialize, "provideWorkerRuntime(accountExpiryService, idempotencyCleanupService, usageRecordWorkerPool, subscriptionExpiryService, paymentOrderExpiryService, pricingService, outboxCleanupService, tokenRefreshService, oAuthService, geminiOAuthService, antigravityOAuthService, openAIOAuthService,")
	require.Contains(t, initialize, "admin.NewOpenAIOAuthHandler(openAIOAuthService")
	require.NotContains(t, functionSource(string(wireGen), "provideCleanup"), "openaiOAuth *service.OpenAIOAuthService")
	require.Equal(t, 1, strings.Count(initialize, "service.ProvideOpenAIOAuthService("))
}

func TestOpsSystemLogSinkWireGraphAndInventoryAreRuntimeManaged(t *testing.T) {
	inventory, err := os.ReadFile("../../../docs/worker-runtime-inventory.md")
	require.NoError(t, err)
	require.Contains(t, string(inventory), "| OpsSystemLogSink | Yes | `cmd/server/provideWorkerRuntime` | `workerruntime` pool adapter | Server runtime | `Runtime.StopAll` joins native drain and final flush to caller deadline, reporting still-running truthfully | per-instance | Ops worker status plus unchanged dedicated system-log health endpoint | Phase 11 |")

	wireGen, err := os.ReadFile("wire_gen.go")
	require.NoError(t, err)
	generated := string(wireGen)
	initialize := functionSource(generated, "initializeApplication")
	require.Equal(t, 1, strings.Count(initialize, "service.ProvideOpsSystemLogSink("))
	require.Contains(t, initialize, "provideWorkerRuntime(accountExpiryService, idempotencyCleanupService, usageRecordWorkerPool, subscriptionExpiryService, paymentOrderExpiryService, pricingService, outboxCleanupService, tokenRefreshService, oAuthService, geminiOAuthService, antigravityOAuthService, openAIOAuthService, userMessageQueueService, concurrencyService, emailQueueService, opsSystemLogSink, authCacheInvalidationWorker, schedulerSupportPublisherWorker, supportDecisionReplicaWorker)")
	require.NotContains(t, functionSource(generated, "provideCleanup"), "opsSystemLogSink *service.OpsSystemLogSink")
	require.NotContains(t, initialize, "provideCleanup(client, redisClient, opsMetricsCollector, opsAggregationService, opsAlertEvaluatorService, opsCleanupService, opsScheduledReportService, opsSystemLogSink,")
}

func TestAuthCacheInvalidationWireGraphAndInventoryAreRuntimeManaged(t *testing.T) {
	inventory, err := os.ReadFile("../../../docs/worker-runtime-inventory.md")
	require.NoError(t, err)
	require.Contains(t, string(inventory), "This inventory records all process-local background activity through Issue #9 Phase 12.")
	require.Contains(t, string(inventory), "| Auth-cache outbox | Yes | `cmd/server/provideWorkerRuntime` | `workerruntime` pool adapter over native durable outbox poller | Server runtime | `Runtime.StopAll` initiates and joins native stop to caller deadline, reporting still-running truthfully | durable claim | Generic process-local Ops worker status; unchanged native auth-cache invalidation `Health` method remains authoritative for durable backlog | Phase 12 |")
	require.Contains(t, string(inventory), "| API-key auth-cache Pub/Sub subscriber | No |")

	wireGen, err := os.ReadFile("wire_gen.go")
	require.NoError(t, err)
	generated := string(wireGen)
	initialize := functionSource(generated, "initializeApplication")
	require.Equal(t, 1, strings.Count(initialize, "service.ProvideAuthCacheInvalidationWorker("))
	require.Contains(t, initialize, "provideWorkerRuntime(accountExpiryService, idempotencyCleanupService, usageRecordWorkerPool, subscriptionExpiryService, paymentOrderExpiryService, pricingService, outboxCleanupService, tokenRefreshService, oAuthService, geminiOAuthService, antigravityOAuthService, openAIOAuthService, userMessageQueueService, concurrencyService, emailQueueService, opsSystemLogSink, authCacheInvalidationWorker, schedulerSupportPublisherWorker, supportDecisionReplicaWorker)")
	require.NotContains(t, functionSource(generated, "provideCleanup"), "authCacheInvalidationWorker *service.AuthCacheInvalidationWorker")
	require.NotContains(t, initialize, "provideCleanup(client, redisClient, opsMetricsCollector, opsAggregationService, opsAlertEvaluatorService, opsCleanupService, opsScheduledReportService, authCacheInvalidationWorker,")
}

func TestWireGeneratedStartupValidationRunsBeforeSideEffectingProviders(t *testing.T) {
	content, err := os.ReadFile("wire_gen.go")
	require.NoError(t, err)
	wireGen := string(content)

	validationIndex := strings.Index(wireGen, "service.ProvideOpenAIOAuthStartupConfigValidation")
	require.NotEqual(t, -1, validationIndex, "startup validation must be wired into initializeApplication")

	for _, provider := range []string{
		"service.ProvideEmailQueueService",
		"service.ProvideBillingCacheService",
		"service.ProvideAPIKeyAuthCacheInvalidator",
		"service.ProvideConcurrencyService",
		"service.ProvideTimingWheelService",
		"service.ProvideDeferredService",
		"service.ProvideSchedulerSnapshotService",
		"service.ProvideDashboardAggregationService",
		"service.ProvideUsageCleanupService",
		"service.ProvideTokenRefreshService",
		"service.ProvideAccountExpiryService",
		"service.ProvideSubscriptionExpiryService",
		"service.ProvideBillingOutboxWorker",
	} {
		providerIndex := strings.Index(wireGen, provider)
		require.NotEqual(t, -1, providerIndex, "%s must be present in generated wiring", provider)
		require.Less(t, validationIndex, providerIndex, "startup validation must run before side-effecting provider %s", provider)
	}
}
