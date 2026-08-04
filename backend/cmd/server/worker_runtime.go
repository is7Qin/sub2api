package main

import (
	"context"
	"log"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/workerruntime"
)

// provideWorkerRuntime is the composition root for pilot worker lifecycles.
func provideWorkerRuntime(
	accountExpiry *service.AccountExpiryService,
	idempotencyCleanup *service.IdempotencyCleanupService,
	usagePool *service.UsageRecordWorkerPool,
	subscriptionExpiry *service.SubscriptionExpiryService,
	paymentOrderExpiry *service.PaymentOrderExpiryService,
	pricing *service.PricingService,
	outboxCleanup *service.OutboxCleanupService,
	tokenRefresh *service.TokenRefreshService,
	oauth *service.OAuthService,
	userMessageQueue *service.UserMessageQueueService,
	concurrency *service.ConcurrencyService,
	emailQueue *service.EmailQueueService,
) (*workerruntime.Runtime, error) {
	accountExpiryWorker, err := service.NewAccountExpiryWorker(accountExpiry)
	if err != nil {
		return nil, err
	}
	idempotencyCleanupWorker, err := service.NewIdempotencyCleanupWorker(idempotencyCleanup)
	if err != nil {
		return nil, err
	}
	subscriptionExpiryWorker, err := service.NewSubscriptionExpiryWorker(subscriptionExpiry)
	if err != nil {
		return nil, err
	}
	paymentOrderExpiryWorker, err := service.NewPaymentOrderExpiryWorker(paymentOrderExpiry)
	if err != nil {
		return nil, err
	}
	pricingRemoteSyncWorker, err := service.NewPricingRemoteSyncWorker(pricing)
	if err != nil {
		return nil, err
	}
	outboxCleanupWorker, err := service.NewOutboxCleanupWorker(outboxCleanup)
	if err != nil {
		return nil, err
	}
	tokenRefreshWorker, err := service.NewTokenRefreshWorker(tokenRefresh)
	if err != nil {
		return nil, err
	}
	claudeOAuthSessionCleanupWorker, err := service.NewClaudeOAuthSessionCleanupWorker(oauth)
	if err != nil {
		return nil, err
	}
	userMessageQueueCleanupWorker, err := service.NewUserMessageQueueCleanupWorker(userMessageQueue)
	if err != nil {
		return nil, err
	}
	concurrencySlotCleanupWorker, err := service.NewConcurrencySlotCleanupWorker(concurrency)
	if err != nil {
		return nil, err
	}

	components := []workerruntime.Component{
		accountExpiryWorker,
		idempotencyCleanupWorker,
		subscriptionExpiryWorker,
		paymentOrderExpiryWorker,
		outboxCleanupWorker,
		claudeOAuthSessionCleanupWorker,
		service.NewUsageRecordWorkerPoolWorker(usagePool),
		service.NewEmailQueueWorker(emailQueue),
	}
	if pricingRemoteSyncWorker != nil {
		components = append(components, pricingRemoteSyncWorker)
	}
	if tokenRefreshWorker != nil {
		components = append(components, tokenRefreshWorker)
	}
	if userMessageQueueCleanupWorker != nil {
		components = append(components, userMessageQueueCleanupWorker)
	}
	if concurrencySlotCleanupWorker != nil {
		components = append(components, concurrencySlotCleanupWorker)
	}

	runtime := workerruntime.NewRuntime(workerruntime.NewRegistry())
	for _, component := range components {
		if err := runtime.Register(component); err != nil {
			return nil, err
		}
	}
	if err := runtime.StartAll(context.Background()); err != nil {
		return nil, err
	}
	return runtime, nil
}

type cleanupStep struct {
	name string
	fn   func() error
}

type runtimeStopper interface {
	StopAll(context.Context) ([]workerruntime.StopResult, error)
}

// runCleanup stops runtime-owned workers before the remaining application and infrastructure cleanup.
func runCleanup(ctx context.Context, runtime runtimeStopper, parallelSteps, infraSteps []cleanupStep) {
	if runtime != nil && !isNilRuntimeStopper(runtime) {
		results, err := runtime.StopAll(ctx)
		for _, result := range results {
			if result.Err != nil {
				log.Printf("[Cleanup] runtime worker %s %s: %v", result.Name, result.Outcome, result.Err)
				continue
			}
			log.Printf("[Cleanup] runtime worker %s %s", result.Name, result.Outcome)
		}
		if err != nil {
			log.Printf("[Cleanup] runtime stop failed: %v", err)
		}
	}

	runParallelCleanup(parallelSteps)
	runSequentialCleanup(infraSteps)
}

func runParallelCleanup(steps []cleanupStep) {
	var wg sync.WaitGroup
	for i := range steps {
		step := steps[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := step.fn(); err != nil {
				log.Printf("[Cleanup] %s failed: %v", step.name, err)
				return
			}
			log.Printf("[Cleanup] %s succeeded", step.name)
		}()
	}
	wg.Wait()
}

func runSequentialCleanup(steps []cleanupStep) {
	for i := range steps {
		step := steps[i]
		if err := step.fn(); err != nil {
			log.Printf("[Cleanup] %s failed: %v", step.name, err)
			continue
		}
		log.Printf("[Cleanup] %s succeeded", step.name)
	}
}

func isNilRuntimeStopper(runtime runtimeStopper) bool {
	typed, ok := runtime.(*workerruntime.Runtime)
	return ok && typed == nil
}
