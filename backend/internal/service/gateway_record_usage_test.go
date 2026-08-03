//go:build unit

package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

func newGatewayRecordUsageServiceForTest(usageRepo UsageLogRepository, userRepo UserRepository, subRepo UserSubscriptionRepository) *GatewayService {
	cfg := &config.Config{}
	cfg.Default.RateMultiplier = 1.1
	return NewGatewayService(
		nil,
		nil,
		usageRepo,
		nil,
		userRepo,
		subRepo,
		nil,
		nil,
		cfg,
		nil,
		nil,
		NewBillingService(cfg, nil),
		nil,
		&BillingCacheService{},
		nil,
		nil,
		&DeferredService{},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil, // userPlatformQuotaRepo
		nil, // usageRecordWorkerPool
	)
}

func newGatewayRecordUsageServiceWithBillingRepoForTest(usageRepo UsageLogRepository, billingRepo UsageBillingRepository, userRepo UserRepository, subRepo UserSubscriptionRepository) *GatewayService {
	svc := newGatewayRecordUsageServiceForTest(usageRepo, userRepo, subRepo)
	svc.usageBillingRepo = billingRepo
	return svc
}

type gatewayRecordUsageBillingOutboxStub struct {
	command *BillingOutboxCommand
	calls   int
	err     error
}

func (s *gatewayRecordUsageBillingOutboxStub) Enqueue(_ context.Context, command *BillingOutboxCommand) (*BillingOutboxRecord, error) {
	s.calls++
	s.command = command
	if s.err != nil {
		return nil, s.err
	}
	return &BillingOutboxRecord{Command: *command}, nil
}

func (s *gatewayRecordUsageBillingOutboxStub) Claim(context.Context, string, int, time.Duration) ([]BillingOutboxRecord, error) {
	return nil, nil
}
func (s *gatewayRecordUsageBillingOutboxStub) Retry(context.Context, int64, string, time.Time, string, bool) error {
	return nil
}
func (s *gatewayRecordUsageBillingOutboxStub) Ack(context.Context, int64, string) error { return nil }
func (s *gatewayRecordUsageBillingOutboxStub) Stats(context.Context) (BillingOutboxStats, error) {
	return BillingOutboxStats{}, nil
}
func (s *gatewayRecordUsageBillingOutboxStub) CleanupTerminal(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

type openAIRecordUsageBestEffortLogRepoStub struct {
	UsageLogRepository

	createErr          error
	createCalls        int
	lastLog            *UsageLog
	lastCtxErr         error
	createDeadline     time.Time
	contextValueKey    any
	createContextValue any
}

func (s *openAIRecordUsageBestEffortLogRepoStub) Create(ctx context.Context, log *UsageLog) (bool, error) {
	s.createCalls++
	s.lastLog = log
	s.lastCtxErr = ctx.Err()
	s.createDeadline, _ = ctx.Deadline()
	if s.contextValueKey != nil {
		s.createContextValue = ctx.Value(s.contextValueKey)
	}
	return false, s.createErr
}

func TestGatewayServiceRecordUsage_EnqueuesDurableCommandBeforeApply(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{}
	billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
	outbox := &gatewayRecordUsageBillingOutboxStub{}
	svc := newGatewayRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})
	svc.billingOutboxRepo = outbox

	err := svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{RequestID: "upstream", AttemptID: "physical-a", Usage: ClaudeUsage{InputTokens: 10, OutputTokens: 6}, Model: "claude-sonnet-4"},
		APIKey: &APIKey{ID: 501, Quota: 100}, User: &User{ID: 601}, Account: &Account{ID: 701},
	})
	require.NoError(t, err)
	require.Equal(t, 1, outbox.calls)
	require.Equal(t, 0, billingRepo.calls)
	require.Equal(t, "physical-a", outbox.command.AttemptID)
	require.Equal(t, "attempt:physical-a", outbox.command.Billing.RequestID)
}

func TestGatewayServiceRecordUsage_PersistsUsageLogWhenOutboxEnqueueFails(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{}
	outbox := &gatewayRecordUsageBillingOutboxStub{err: errors.New("outbox unavailable")}
	svc := newGatewayRecordUsageServiceForTest(usageRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})
	svc.billingOutboxRepo = outbox

	err := svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{RequestID: "upstream", AttemptID: "physical-failure", Usage: ClaudeUsage{InputTokens: 10, OutputTokens: 6}, Model: "claude-sonnet-4"},
		APIKey: &APIKey{ID: 501, Quota: 100}, User: &User{ID: 601}, Account: &Account{ID: 701},
	})
	require.EqualError(t, err, "outbox unavailable")
	require.Equal(t, 1, usageRepo.calls)
}

func TestGatewayServiceRecordUsage_BillingUsesDetachedContext(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: false, err: context.DeadlineExceeded}
	userRepo := &openAIRecordUsageUserRepoStub{}
	subRepo := &openAIRecordUsageSubRepoStub{}
	quotaSvc := &openAIRecordUsageAPIKeyQuotaStub{}
	svc := newGatewayRecordUsageServiceForTest(usageRepo, userRepo, subRepo)

	reqCtx, cancel := context.WithCancel(context.Background())
	cancel()

	err := svc.RecordUsage(reqCtx, &RecordUsageInput{
		Result: &ForwardResult{
			RequestID: "gateway_detached_ctx",
			Usage: ClaudeUsage{
				InputTokens:  10,
				OutputTokens: 6,
			},
			Model:    "claude-sonnet-4",
			Duration: time.Second,
		},
		APIKey: &APIKey{
			ID:    501,
			Quota: 100,
		},
		User:          &User{ID: 601},
		Account:       &Account{ID: 701},
		APIKeyService: quotaSvc,
	})

	require.NoError(t, err)
	require.Equal(t, 1, usageRepo.calls)
	require.Equal(t, 1, userRepo.deductCalls)
	require.NoError(t, userRepo.lastCtxErr)
	require.Equal(t, 1, quotaSvc.quotaCalls)
	require.NoError(t, quotaSvc.lastQuotaCtxErr)
}

func TestGatewayServiceRecordUsage_BillingFingerprintIncludesRequestPayloadHash(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{}
	billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
	svc := newGatewayRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})

	payloadHash := HashUsageRequestPayload([]byte(`{"messages":[{"role":"user","content":"hello"}]}`))
	err := svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{
			RequestID: "gateway_payload_hash",
			Usage: ClaudeUsage{
				InputTokens:  10,
				OutputTokens: 6,
			},
			Model:    "claude-sonnet-4",
			Duration: time.Second,
		},
		APIKey:             &APIKey{ID: 501, Quota: 100},
		User:               &User{ID: 601},
		Account:            &Account{ID: 701},
		RequestPayloadHash: payloadHash,
	})
	require.NoError(t, err)
	require.NotNil(t, billingRepo.lastCmd)
	require.Equal(t, payloadHash, billingRepo.lastCmd.RequestPayloadHash)
}

func TestGatewayServiceRecordUsage_BillingFingerprintFallsBackToContextRequestID(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{}
	billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
	svc := newGatewayRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})

	ctx := context.WithValue(context.Background(), ctxkey.RequestID, "req-local-123")
	err := svc.RecordUsage(ctx, &RecordUsageInput{
		Result: &ForwardResult{
			RequestID: "gateway_payload_fallback",
			Usage: ClaudeUsage{
				InputTokens:  10,
				OutputTokens: 6,
			},
			Model:    "claude-sonnet-4",
			Duration: time.Second,
		},
		APIKey:  &APIKey{ID: 501, Quota: 100},
		User:    &User{ID: 601},
		Account: &Account{ID: 701},
	})
	require.NoError(t, err)
	require.NotNil(t, billingRepo.lastCmd)
	require.Equal(t, "local:req-local-123", billingRepo.lastCmd.RequestPayloadHash)
}

func TestGatewayServiceRecordUsage_PreservesRequestedAndUpstreamModels(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	svc := newGatewayRecordUsageServiceForTest(usageRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})
	mappedModel := "claude-sonnet-4-20250514"

	err := svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{
			RequestID:     "gateway_models_split",
			Usage:         ClaudeUsage{InputTokens: 10, OutputTokens: 6},
			Model:         "claude-sonnet-4",
			UpstreamModel: mappedModel,
			Duration:      time.Second,
		},
		APIKey:  &APIKey{ID: 501, Quota: 100},
		User:    &User{ID: 601},
		Account: &Account{ID: 701},
	})

	require.NoError(t, err)
	require.NotNil(t, usageRepo.lastLog)
	require.Equal(t, "claude-sonnet-4", usageRepo.lastLog.Model)
	require.Equal(t, "claude-sonnet-4", usageRepo.lastLog.RequestedModel)
	require.NotNil(t, usageRepo.lastLog.UpstreamModel)
	require.Equal(t, mappedModel, *usageRepo.lastLog.UpstreamModel)
}

func TestGatewayServiceRecordUsage_EmptyImageSizeDefaultsBeforeBillingAndPersistence(t *testing.T) {
	imagePrice2K := 0.19
	groupID := int64(901)
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	svc := newGatewayRecordUsageServiceForTest(usageRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})

	err := svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{
			RequestID:      "gateway_image_default_size",
			Model:          "gemini-image",
			ImageCount:     1,
			ImageInputSize: "auto",
			Duration:       time.Second,
		},
		APIKey: &APIKey{
			ID:      801,
			GroupID: i64p(groupID),
			Group: &Group{
				ID:             groupID,
				RateMultiplier: 1.0,
				ImagePrice2K:   &imagePrice2K,
			},
		},
		User:    &User{ID: 601},
		Account: &Account{ID: 701},
	})

	require.NoError(t, err)
	require.NotNil(t, usageRepo.lastLog)
	require.Equal(t, 1, usageRepo.lastLog.ImageCount)
	require.NotNil(t, usageRepo.lastLog.ImageSize)
	require.Equal(t, ImageBillingSize2K, *usageRepo.lastLog.ImageSize)
	require.NotNil(t, usageRepo.lastLog.ImageInputSize)
	require.Equal(t, "auto", *usageRepo.lastLog.ImageInputSize)
	require.NotNil(t, usageRepo.lastLog.ImageSizeSource)
	require.Equal(t, ImageSizeSourceDefault, *usageRepo.lastLog.ImageSizeSource)
	require.InDelta(t, 0.19, usageRepo.lastLog.TotalCost, 1e-12)
	require.InDelta(t, 0.19, usageRepo.lastLog.ActualCost, 1e-12)
}

func TestGatewayServiceRecordUsage_UsageLogWriteErrorDoesNotSkipBilling(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: false, err: MarkUsageLogCreateNotPersisted(context.Canceled)}
	userRepo := &openAIRecordUsageUserRepoStub{}
	subRepo := &openAIRecordUsageSubRepoStub{}
	quotaSvc := &openAIRecordUsageAPIKeyQuotaStub{}
	svc := newGatewayRecordUsageServiceForTest(usageRepo, userRepo, subRepo)

	err := svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{
			RequestID: "gateway_not_persisted",
			Usage: ClaudeUsage{
				InputTokens:  10,
				OutputTokens: 6,
			},
			Model:    "claude-sonnet-4",
			Duration: time.Second,
		},
		APIKey: &APIKey{
			ID:    503,
			Quota: 100,
		},
		User:          &User{ID: 603},
		Account:       &Account{ID: 703},
		APIKeyService: quotaSvc,
	})

	require.NoError(t, err)
	require.Equal(t, 1, usageRepo.calls)
	require.Equal(t, 1, userRepo.deductCalls)
	require.Equal(t, 1, quotaSvc.quotaCalls)
}

func TestGatewayServiceRecordUsageWithLongContext_BillingUsesDetachedContext(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: false, err: context.DeadlineExceeded}
	userRepo := &openAIRecordUsageUserRepoStub{}
	subRepo := &openAIRecordUsageSubRepoStub{}
	quotaSvc := &openAIRecordUsageAPIKeyQuotaStub{}
	svc := newGatewayRecordUsageServiceForTest(usageRepo, userRepo, subRepo)

	reqCtx, cancel := context.WithCancel(context.Background())
	cancel()

	err := svc.RecordUsageWithLongContext(reqCtx, &RecordUsageLongContextInput{
		Result: &ForwardResult{
			RequestID: "gateway_long_context_detached_ctx",
			Usage: ClaudeUsage{
				InputTokens:  12,
				OutputTokens: 8,
			},
			Model:    "claude-sonnet-4",
			Duration: time.Second,
		},
		APIKey: &APIKey{
			ID:    502,
			Quota: 100,
		},
		User:                  &User{ID: 602},
		Account:               &Account{ID: 702},
		LongContextThreshold:  200000,
		LongContextMultiplier: 2,
		APIKeyService:         quotaSvc,
	})

	require.NoError(t, err)
	require.Equal(t, 1, usageRepo.calls)
	require.Equal(t, 1, userRepo.deductCalls)
	require.NoError(t, userRepo.lastCtxErr)
	require.Equal(t, 1, quotaSvc.quotaCalls)
	require.NoError(t, quotaSvc.lastQuotaCtxErr)
}

func TestGatewayServiceRecordUsage_UsesFallbackRequestIDForUsageLog(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{}
	userRepo := &openAIRecordUsageUserRepoStub{}
	subRepo := &openAIRecordUsageSubRepoStub{}
	svc := newGatewayRecordUsageServiceForTest(usageRepo, userRepo, subRepo)

	ctx := context.WithValue(context.Background(), ctxkey.RequestID, "gateway-local-fallback")
	err := svc.RecordUsage(ctx, &RecordUsageInput{
		Result: &ForwardResult{
			RequestID: "",
			Usage: ClaudeUsage{
				InputTokens:  10,
				OutputTokens: 6,
			},
			Model:    "claude-sonnet-4",
			Duration: time.Second,
		},
		APIKey:  &APIKey{ID: 504},
		User:    &User{ID: 604},
		Account: &Account{ID: 704},
	})

	require.NoError(t, err)
	require.NotNil(t, usageRepo.lastLog)
	require.Equal(t, "local:gateway-local-fallback", usageRepo.lastLog.RequestID)
}

func TestGatewayServiceRecordUsage_AttemptIdentityStableAcrossReplayContexts(t *testing.T) {
	result := &ForwardResult{
		RequestID: "upstream-request",
		AttemptID: "physical-a",
		Usage:     ClaudeUsage{InputTokens: 10, OutputTokens: 6},
		Model:     "claude-sonnet-4",
	}
	record := func(ctx context.Context) (*UsageBillingCommand, *UsageLog) {
		usageRepo := &openAIRecordUsageLogRepoStub{}
		billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
		svc := newGatewayRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})
		err := svc.RecordUsage(ctx, &RecordUsageInput{
			Result:  result,
			APIKey:  &APIKey{ID: 506},
			User:    &User{ID: 606},
			Account: &Account{ID: 706},
		})
		require.NoError(t, err)
		require.Equal(t, 1, billingRepo.calls)
		return billingRepo.lastCmd, usageRepo.lastLog
	}

	clientCtx := context.WithValue(context.Background(), ctxkey.ClientRequestID, "logical-1")
	differentCtx := context.WithValue(context.Background(), ctxkey.ClientRequestID, "logical-2")
	first, firstLog := record(clientCtx)
	emptyReplay, emptyReplayLog := record(context.Background())
	differentReplay, differentReplayLog := record(differentCtx)

	require.Equal(t, "attempt:physical-a", first.RequestID)
	require.Equal(t, first.RequestID, emptyReplay.RequestID)
	require.Equal(t, first.RequestID, differentReplay.RequestID)
	require.Equal(t, "client:logical-1", firstLog.RequestID)
	require.Equal(t, "upstream-request", emptyReplayLog.RequestID)
	require.Equal(t, "client:logical-2", differentReplayLog.RequestID)
}

func TestGatewayServiceRecordUsage_AttemptIdentitySeparatesFailoverBilling(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxkey.ClientRequestID, "client-stable-123")
	record := func(attemptID string) (*UsageBillingCommand, *UsageLog) {
		usageRepo := &openAIRecordUsageLogRepoStub{}
		billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
		svc := newGatewayRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})
		err := svc.RecordUsage(ctx, &RecordUsageInput{
			Result: &ForwardResult{
				RequestID: "upstream-request",
				AttemptID: attemptID,
				Usage:     ClaudeUsage{InputTokens: 10, OutputTokens: 6},
				Model:     "claude-sonnet-4",
			},
			APIKey:  &APIKey{ID: 506},
			User:    &User{ID: 606},
			Account: &Account{ID: 706},
		})
		require.NoError(t, err)
		require.Equal(t, 1, billingRepo.calls)
		return billingRepo.lastCmd, usageRepo.lastLog
	}

	first, firstLog := record("attempt-a")
	replay, replayLog := record("attempt-a")
	second, secondLog := record("attempt-b")

	require.Equal(t, "attempt:attempt-a", first.RequestID)
	require.Equal(t, first.RequestID, replay.RequestID)
	require.NotEqual(t, first.RequestID, second.RequestID)
	require.Equal(t, "client:client-stable-123", firstLog.RequestID)
	require.Equal(t, firstLog.RequestID, replayLog.RequestID)
	require.Equal(t, firstLog.RequestID, secondLog.RequestID)
}

func TestGatewayServiceRecordUsage_PrefersClientRequestIDOverUpstreamRequestID(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{}
	billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
	svc := newGatewayRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})

	ctx := context.WithValue(context.Background(), ctxkey.ClientRequestID, "client-stable-123")
	ctx = context.WithValue(ctx, ctxkey.RequestID, "req-local-ignored")
	err := svc.RecordUsage(ctx, &RecordUsageInput{
		Result: &ForwardResult{
			RequestID: "upstream-volatile-456",
			Usage: ClaudeUsage{
				InputTokens:  10,
				OutputTokens: 6,
			},
			Model:    "claude-sonnet-4",
			Duration: time.Second,
		},
		APIKey:  &APIKey{ID: 506},
		User:    &User{ID: 606},
		Account: &Account{ID: 706},
	})

	require.NoError(t, err)
	require.NotNil(t, billingRepo.lastCmd)
	require.Equal(t, "client:client-stable-123", billingRepo.lastCmd.RequestID)
	require.NotNil(t, usageRepo.lastLog)
	require.Equal(t, "client:client-stable-123", usageRepo.lastLog.RequestID)
}

func TestGatewayServiceRecordUsage_GeneratesRequestIDWhenAllSourcesMissing(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{}
	billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
	svc := newGatewayRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})

	err := svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{
			RequestID: "",
			Usage: ClaudeUsage{
				InputTokens:  10,
				OutputTokens: 6,
			},
			Model:    "claude-sonnet-4",
			Duration: time.Second,
		},
		APIKey:  &APIKey{ID: 507},
		User:    &User{ID: 607},
		Account: &Account{ID: 707},
	})

	require.NoError(t, err)
	require.NotNil(t, billingRepo.lastCmd)
	require.True(t, strings.HasPrefix(billingRepo.lastCmd.RequestID, "generated:"))
	require.NotNil(t, usageRepo.lastLog)
	require.Equal(t, billingRepo.lastCmd.RequestID, usageRepo.lastLog.RequestID)
}

func TestGatewayServiceRecordUsage_FallbackUsesSingleCreateWithDetachedContext(t *testing.T) {
	type usageContextKey struct{}
	key := usageContextKey{}
	usageRepo := &openAIRecordUsageBestEffortLogRepoStub{contextValueKey: key}
	billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
	svc := newGatewayRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})
	base := context.WithValue(context.Background(), key, "trace-value")
	ctx, cancel := context.WithTimeout(base, 50*time.Millisecond)
	defer cancel()
	parentDeadline, ok := ctx.Deadline()
	require.True(t, ok)

	err := svc.RecordUsage(ctx, &RecordUsageInput{
		Result: &ForwardResult{
			RequestID: "gateway_fallback_usage_log",
			Usage: ClaudeUsage{
				InputTokens:  10,
				OutputTokens: 6,
			},
			Model:    "claude-sonnet-4",
			Duration: time.Second,
		},
		APIKey:  &APIKey{ID: 508},
		User:    &User{ID: 608},
		Account: &Account{ID: 708},
	})

	require.NoError(t, err)
	require.Equal(t, 1, usageRepo.createCalls)
	require.NoError(t, usageRepo.lastCtxErr)
	require.Equal(t, "trace-value", usageRepo.createContextValue)
	require.WithinDuration(t, parentDeadline, usageRepo.createDeadline, time.Millisecond)
}

func TestGatewayServiceRecordUsage_AtomicBillingSkipsSeparateUsageLogWrite(t *testing.T) {
	usageRepo := &openAIRecordUsageBestEffortLogRepoStub{}
	billingRepo := &openAIRecordUsageBillingRepoStub{
		result: &UsageBillingApplyResult{Applied: true, UsageLogPersisted: true},
	}
	svc := newGatewayRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})

	err := svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{
			RequestID: "gateway_atomic_usage_log",
			Usage:     ClaudeUsage{InputTokens: 10, OutputTokens: 6},
			Model:     "claude-sonnet-4",
			Duration:  time.Second,
		},
		APIKey:  &APIKey{ID: 509},
		User:    &User{ID: 609},
		Account: &Account{ID: 709},
	})

	require.NoError(t, err)
	require.NotNil(t, billingRepo.lastCmd)
	require.NotNil(t, billingRepo.lastCmd.UsageLog)
	require.Equal(t, "gateway_atomic_usage_log", billingRepo.lastCmd.UsageLog.RequestID)
	require.Zero(t, usageRepo.createCalls)
}

func TestDetachedBillingContextPreservesShorterDeadline(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	detached, detachedCancel := detachedBillingContext(parent)
	defer detachedCancel()

	parentDeadline, ok := parent.Deadline()
	require.True(t, ok)
	detachedDeadline, ok := detached.Deadline()
	require.True(t, ok)
	require.WithinDuration(t, parentDeadline, detachedDeadline, time.Millisecond)
}

func TestGatewayServiceRecordUsage_BillingErrorSkipsUsageLogWrite(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{}
	billingRepo := &openAIRecordUsageBillingRepoStub{err: context.DeadlineExceeded}
	userRepo := &openAIRecordUsageUserRepoStub{}
	subRepo := &openAIRecordUsageSubRepoStub{}
	svc := newGatewayRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, userRepo, subRepo)

	err := svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{
			RequestID: "gateway_billing_fail",
			Usage: ClaudeUsage{
				InputTokens:  10,
				OutputTokens: 6,
			},
			Model:    "claude-sonnet-4",
			Duration: time.Second,
		},
		APIKey:  &APIKey{ID: 505},
		User:    &User{ID: 605},
		Account: &Account{ID: 705},
	})

	require.Error(t, err)
	require.Equal(t, 1, billingRepo.calls)
	require.Equal(t, 0, usageRepo.calls)
}

func TestGatewayServiceRecordUsage_InvalidUsageSkipsBillingAndUsageLog(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{}
	billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
	userRepo := &openAIRecordUsageUserRepoStub{}
	subRepo := &openAIRecordUsageSubRepoStub{}
	svc := newGatewayRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, userRepo, subRepo)

	err := svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{
			RequestID: "gateway_invalid_usage",
			Usage: ClaudeUsage{
				InputTokens:       10,
				OutputTokens:      6,
				ImageOutputTokens: -1,
			},
			Model:    "claude-sonnet-4",
			Duration: time.Second,
		},
		APIKey:  &APIKey{ID: 506},
		User:    &User{ID: 606},
		Account: &Account{ID: 706},
	})

	require.ErrorContains(t, err, "image_output_tokens is negative")
	require.Equal(t, 0, billingRepo.calls)
	require.Equal(t, 0, usageRepo.calls)
}

func TestGatewayServiceRecordUsage_NegativeImageCountSkipsBillingAndUsageLog(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{}
	billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
	userRepo := &openAIRecordUsageUserRepoStub{}
	subRepo := &openAIRecordUsageSubRepoStub{}
	svc := newGatewayRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, userRepo, subRepo)

	err := svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{
			RequestID: "gateway_negative_image_count",
			Usage:     ClaudeUsage{InputTokens: 10, OutputTokens: 6},
			Model:     "claude-sonnet-4", ImageCount: -1, Duration: time.Second,
		},
		APIKey: &APIKey{ID: 507}, User: &User{ID: 607}, Account: &Account{ID: 707},
	})

	require.ErrorContains(t, err, "image_count is negative")
	require.Equal(t, 0, billingRepo.calls)
	require.Equal(t, 0, usageRepo.calls)
}

func TestGatewayServiceRecordUsage_DoesNotMutateInputDuringNormalization(t *testing.T) {
	usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
	billingRepo := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
	svc := newGatewayRecordUsageServiceWithBillingRepoForTest(usageRepo, billingRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})
	result := &ForwardResult{
		RequestID: "gateway_unchanged_input", Model: "claude-sonnet-4", ImageCount: 1,
		Usage: ClaudeUsage{InputTokens: 10, CacheReadInputTokens: 2},
	}
	original := *result

	err := svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: result, APIKey: &APIKey{ID: 508}, User: &User{ID: 608}, Account: &Account{ID: 708},
		ForceCacheBilling: true,
	})

	require.NoError(t, err)
	require.Equal(t, original, *result)
}

func TestGatewayServiceRecordUsage_ReasoningEffortPersisted(t *testing.T) {
	usageRepo := &openAIRecordUsageBestEffortLogRepoStub{}
	svc := newGatewayRecordUsageServiceForTest(usageRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})

	effort := "max"
	err := svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{
			RequestID: "effort_test",
			Usage: ClaudeUsage{
				InputTokens:  10,
				OutputTokens: 5,
			},
			Model:           "claude-opus-4-6",
			Duration:        time.Second,
			ReasoningEffort: &effort,
		},
		APIKey:  &APIKey{ID: 1},
		User:    &User{ID: 1},
		Account: &Account{ID: 1},
	})

	require.NoError(t, err)
	require.NotNil(t, usageRepo.lastLog)
	require.NotNil(t, usageRepo.lastLog.ReasoningEffort)
	require.Equal(t, "max", *usageRepo.lastLog.ReasoningEffort)
}

func TestGatewayServiceRecordUsage_ReasoningEffortNil(t *testing.T) {
	usageRepo := &openAIRecordUsageBestEffortLogRepoStub{}
	svc := newGatewayRecordUsageServiceForTest(usageRepo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})

	err := svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{
			RequestID: "no_effort_test",
			Usage: ClaudeUsage{
				InputTokens:  10,
				OutputTokens: 5,
			},
			Model:    "claude-sonnet-4",
			Duration: time.Second,
		},
		APIKey:  &APIKey{ID: 1},
		User:    &User{ID: 1},
		Account: &Account{ID: 1},
	})

	require.NoError(t, err)
	require.NotNil(t, usageRepo.lastLog)
	require.Nil(t, usageRepo.lastLog.ReasoningEffort)
}
