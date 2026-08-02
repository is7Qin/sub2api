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
		service.NewTokenRefreshService(nil, nil, nil, nil, nil, nil, nil, &config.Config{}, nil),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = runtime.StopAll(context.Background()) })

	snapshots := runtime.Snapshot()
	require.Equal(t, []string{"account-expiry", "idempotency-cleanup", "payment-order-expiry", "subscription-expiry", "usage-record-pool"}, snapshotNames(snapshots))
	for _, snapshot := range snapshots {
		require.Equal(t, workerruntime.LifecycleRunning, snapshot.Lifecycle.State)
	}
	// Status is the public kind-specific runtime API; do not depend on obsolete snapshot fields.
	require.IsType(t, workerruntime.PeriodicStatus{}, snapshots[0].Status)
	require.IsType(t, workerruntime.PeriodicStatus{}, snapshots[1].Status)
	require.IsType(t, workerruntime.PeriodicStatus{}, snapshots[2].Status)
	require.IsType(t, workerruntime.PeriodicStatus{}, snapshots[3].Status)
	require.IsType(t, workerruntime.PoolStatus{}, snapshots[4].Status)
	require.True(t, usagePool.Accepting())
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
		disabled,
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
		enabled,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = enabledRuntime.StopAll(context.Background()) })
	require.Contains(t, snapshotNames(enabledRuntime.Snapshot()), "token-refresh")
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
