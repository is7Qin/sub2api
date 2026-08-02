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

	oauthSvc := service.NewOAuthService(nil, nil)
	openAIOAuthSvc := service.NewOpenAIOAuthService(nil, nil)
	geminiOAuthSvc := service.NewGeminiOAuthService(nil, nil, nil, nil, cfg)
	antigravityOAuthSvc := service.NewAntigravityOAuthService(nil)

	tokenRefreshSvc := service.NewTokenRefreshService(
		nil,
		oauthSvc,
		openAIOAuthSvc,
		geminiOAuthSvc,
		antigravityOAuthSvc,
		nil,
		nil,
		cfg,
		nil,
	)
	emailQueueSvc := service.NewEmailQueueService(nil, 1)
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
		tokenRefreshSvc,
		&service.UsageCleanupService{},
		emailQueueSvc,
		billingCacheSvc,
		&service.SubscriptionService{},
		&service.OpenAIOAuthStartupConfigValidation{},
		oauthSvc,
		openAIOAuthSvc,
		geminiOAuthSvc,
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
