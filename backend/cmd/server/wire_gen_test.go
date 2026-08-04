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

	openAIOAuthSvc := service.NewOpenAIOAuthService(nil, nil)
	antigravityOAuthSvc := service.NewAntigravityOAuthService(nil)

	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	schedulerSnapshotSvc := service.NewSchedulerSnapshotService(nil, nil, nil, nil, cfg)
	opsSystemLogSinkSvc := service.NewOpsSystemLogSink(nil)

	cleanup := provideCleanup(
		nil, // entClient
		nil, // redis
		&service.OpsMetricsCollector{},
		&service.OpsAggregationService{},
		&service.OpsAlertEvaluatorService{},
		&service.OpsCleanupService{},
		&service.OpsScheduledReportService{},
		opsSystemLogSinkSvc,
		nil, // authCacheInvalidationWorker
		nil, // apiKeyService
		schedulerSnapshotSvc,
		&service.UsageCleanupService{},
		billingCacheSvc,
		&service.SubscriptionService{},
		&service.OpenAIOAuthStartupConfigValidation{},
		openAIOAuthSvc,
		antigravityOAuthSvc,
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
	require.Contains(t, string(inventory), "This inventory records all process-local background activity through Issue #9 Phase 8.")
	require.Contains(t, string(inventory), "| Gemini OAuth session cleanup | Yes | `cmd/server/provideWorkerRuntime` | `workerruntime.PeriodicJob` (delayed fixed-delay 5-minute interval) | Server runtime | `Runtime.StopAll` waits to deadline and reports timeout | per-instance in-memory session map protected by an RW mutex | Ops worker status | Phase 8 |")

	wireGen, err := os.ReadFile("wire_gen.go")
	require.NoError(t, err)
	generated := string(wireGen)
	initialize := functionSource(generated, "initializeApplication")
	require.Contains(t, initialize, "provideWorkerRuntime(accountExpiryService, idempotencyCleanupService, usageRecordWorkerPool, subscriptionExpiryService, paymentOrderExpiryService, pricingService, outboxCleanupService, tokenRefreshService, oAuthService, geminiOAuthService,")
	require.NotContains(t, functionSource(generated, "provideCleanup"), "geminiOAuth *service.GeminiOAuthService")
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
