package service

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type openAI403CounterResetStub struct {
	resetCalls []int64
}

func newOpenAIRecordUsageServiceWith403CounterForTest(counter *openAI403CounterResetStub, billingRepo *openAIRecordUsageBillingRepoStub) *OpenAIGatewayService {
	rateLimitSvc := NewRateLimitService(nil, nil, nil, nil, nil)
	rateLimitSvc.SetOpenAI403CounterCache(counter)
	svc := newOpenAIRecordUsageServiceWithBillingRepoForTest(&openAIRecordUsageLogRepoStub{inserted: true}, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
	svc.rateLimitService = rateLimitSvc
	return svc
}

func (s *openAI403CounterResetStub) IncrementOpenAI403Count(context.Context, int64, int) (int64, error) {
	return 0, nil
}

func (s *openAI403CounterResetStub) ResetOpenAI403Count(_ context.Context, accountID int64) error {
	s.resetCalls = append(s.resetCalls, accountID)
	return nil
}

func TestOpenAIGatewayServiceRecordUsage_ResetsOpenAI403CounterForZeroUsage(t *testing.T) {
	counter := &openAI403CounterResetStub{}
	rateLimitSvc := NewRateLimitService(nil, nil, nil, nil, nil)
	rateLimitSvc.SetOpenAI403CounterCache(counter)

	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
	userRepo := &openAIRecordUsageUserRepoStub{}
	subRepo := &openAIRecordUsageSubRepoStub{}
	svc := newOpenAIRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, userRepo, subRepo, nil)
	svc.rateLimitService = rateLimitSvc

	err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
		Result: &OpenAIForwardResult{
			RequestID: "resp_zero_usage_reset_403",
			Model:     "gpt-5.1",
		},
		APIKey:  &APIKey{ID: 1001, Group: &Group{RateMultiplier: 1}},
		User:    &User{ID: 2001},
		Account: &Account{ID: 777, Platform: PlatformOpenAI},
	})

	require.NoError(t, err)
	require.Equal(t, []int64{777}, counter.resetCalls)
	require.Equal(t, 1, usageRepo.calls)
}

func TestOpenAIGatewayServiceRecordUsage_SimpleModePreserveAccountHealthSkipsResetAndLastUsed(t *testing.T) {
	counter := &openAI403CounterResetStub{}
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	svc := newOpenAIRecordUsageServiceForTest(usageRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
	svc.cfg.RunMode = config.RunModeSimple
	svc.rateLimitService = NewRateLimitService(nil, nil, nil, nil, nil)
	svc.rateLimitService.SetOpenAI403CounterCache(counter)
	deferred := &DeferredService{}
	svc.deferredService = deferred

	err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
		Result:                &OpenAIForwardResult{RequestID: "partial_failure_health", Model: "gpt-5.1", Usage: OpenAIUsage{InputTokens: 5, OutputTokens: 1}},
		APIKey:                &APIKey{ID: 1004},
		User:                  &User{ID: 2004},
		Account:               &Account{ID: 780, Platform: PlatformOpenAI},
		PreserveAccountHealth: true,
	})

	require.NoError(t, err)
	require.Equal(t, 1, usageRepo.calls)
	require.Empty(t, counter.resetCalls)
	_, scheduled := deferred.lastUsedUpdates.Load(int64(780))
	require.False(t, scheduled)
}

func TestOpenAIGatewayServiceRecordUsage_SimpleModeSuccessResetsHealthAndSchedulesLastUsed(t *testing.T) {
	counter := &openAI403CounterResetStub{}
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	svc := newOpenAIRecordUsageServiceForTest(usageRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
	svc.cfg.RunMode = config.RunModeSimple
	svc.rateLimitService = NewRateLimitService(nil, nil, nil, nil, nil)
	svc.rateLimitService.SetOpenAI403CounterCache(counter)
	deferred := &DeferredService{}
	svc.deferredService = deferred

	err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
		Result:  &OpenAIForwardResult{RequestID: "successful_usage_health", Model: "gpt-5.1", Usage: OpenAIUsage{InputTokens: 5, OutputTokens: 1}},
		APIKey:  &APIKey{ID: 1005},
		User:    &User{ID: 2005},
		Account: &Account{ID: 781, Platform: PlatformOpenAI},
	})

	require.NoError(t, err)
	require.Equal(t, 1, usageRepo.calls)
	require.Equal(t, []int64{781}, counter.resetCalls)
	_, scheduled := deferred.lastUsedUpdates.Load(int64(781))
	require.True(t, scheduled)
}

func TestOpenAIGatewayServiceRecordUsage_BilledModePreserveAccountHealthSkipsHealthMutation(t *testing.T) {
	counter := &openAI403CounterResetStub{}
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
	svc := newOpenAIRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
	svc.rateLimitService = NewRateLimitService(nil, nil, nil, nil, nil)
	svc.rateLimitService.SetOpenAI403CounterCache(counter)
	deferred := &DeferredService{}
	svc.deferredService = deferred

	err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
		Result:                &OpenAIForwardResult{RequestID: "billed_partial_failure_health", Model: "gpt-5.1", Usage: OpenAIUsage{InputTokens: 5, OutputTokens: 1}},
		APIKey:                &APIKey{ID: 1006},
		User:                  &User{ID: 2006},
		Account:               &Account{ID: 782, Platform: PlatformOpenAI},
		PreserveAccountHealth: true,
	})

	require.NoError(t, err)
	require.Equal(t, 1, billingRepo.calls)
	require.Equal(t, 1, usageRepo.calls)
	require.Empty(t, counter.resetCalls)
	_, scheduled := deferred.lastUsedUpdates.Load(int64(782))
	require.False(t, scheduled)
}

func TestOpenAIGatewayServiceRecordUsage_InvalidPricingDoesNotResetOpenAI403Counter(t *testing.T) {
	counter := &openAI403CounterResetStub{}
	svc := newOpenAIRecordUsageServiceWith403CounterForTest(counter, &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}})
	svc.cfg.Default.RateMultiplier = math.NaN()

	err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
		Result: &OpenAIForwardResult{RequestID: "invalid_pricing_no_reset", Model: "gpt-5.1", Usage: OpenAIUsage{InputTokens: 1}},
		APIKey: &APIKey{ID: 1002}, User: &User{ID: 2002}, Account: &Account{ID: 778, Platform: PlatformOpenAI},
	})

	require.ErrorContains(t, err, "invalid billing cost")
	require.Empty(t, counter.resetCalls)
}

func TestOpenAIGatewayServiceRecordUsage_PersistenceFailureDoesNotResetOpenAI403Counter(t *testing.T) {
	counter := &openAI403CounterResetStub{}
	svc := newOpenAIRecordUsageServiceWith403CounterForTest(counter, &openAIRecordUsageBillingRepoStub{err: errors.New("billing tx failed")})

	err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
		Result: &OpenAIForwardResult{RequestID: "persistence_failure_no_reset", Model: "gpt-5.1", Usage: OpenAIUsage{InputTokens: 1}},
		APIKey: &APIKey{ID: 1003}, User: &User{ID: 2003}, Account: &Account{ID: 779, Platform: PlatformOpenAI},
	})

	require.ErrorContains(t, err, "billing tx failed")
	require.Empty(t, counter.resetCalls)
}
