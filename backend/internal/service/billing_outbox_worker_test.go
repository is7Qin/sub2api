package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type billingOutboxRepoStub struct {
	mu sync.Mutex

	records             []BillingOutboxRecord
	finalizationRecords []BillingOutboxRecord
	claimLimit          int
	workerID            string
	lease               time.Duration
	acked               []int64
	finalizationAcked   []int64
	retried             []billingOutboxRetry
	finalizationRetried []billingOutboxRetry
	stats               BillingOutboxStats
	statsErr            error
}

type billingOutboxRetry struct {
	id          int64
	workerID    string
	availableAt time.Time
	lastError   string
	terminal    bool
}

func (r *billingOutboxRepoStub) Enqueue(context.Context, *BillingOutboxCommand) (*BillingOutboxRecord, error) {
	return nil, errors.New("not implemented")
}

func (r *billingOutboxRepoStub) Claim(_ context.Context, workerID string, limit int, lease time.Duration) ([]BillingOutboxRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.claimLimit = limit
	r.workerID = workerID
	r.lease = lease
	return append([]BillingOutboxRecord(nil), r.records...), nil
}

func (r *billingOutboxRepoStub) Ack(_ context.Context, id int64, workerID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.acked = append(r.acked, id)
	r.workerID = workerID
	return nil
}

func (r *billingOutboxRepoStub) Retry(_ context.Context, id int64, workerID string, availableAt time.Time, lastError string, terminal bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.retried = append(r.retried, billingOutboxRetry{id: id, workerID: workerID, availableAt: availableAt, lastError: lastError, terminal: terminal})
	r.workerID = workerID
	return nil
}

func (r *billingOutboxRepoStub) ClaimFinalization(_ context.Context, workerID string, _ int, _ time.Duration) ([]BillingOutboxRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.workerID = workerID
	return append([]BillingOutboxRecord(nil), r.finalizationRecords...), nil
}

func (r *billingOutboxRepoStub) RetryFinalization(_ context.Context, id int64, workerID string, availableAt time.Time, lastError string, terminal bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finalizationRetried = append(r.finalizationRetried, billingOutboxRetry{id: id, workerID: workerID, availableAt: availableAt, lastError: lastError, terminal: terminal})
	return nil
}

func (r *billingOutboxRepoStub) AckFinalization(_ context.Context, id int64, workerID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finalizationAcked = append(r.finalizationAcked, id)
	return nil
}

func (r *billingOutboxRepoStub) Stats(context.Context) (BillingOutboxStats, error) {
	return r.stats, r.statsErr
}

type billingOutboxPostProcessorStub struct {
	calls   int
	command *BillingOutboxCommand
	result  *UsageBillingApplyResult
	err     error
}

func (s *billingOutboxPostProcessorStub) Finalize(_ context.Context, command *BillingOutboxCommand, result *UsageBillingApplyResult) error {
	s.calls++
	s.command = command
	s.result = result
	return s.err
}

type usageBillingRepoStub struct {
	mu       sync.Mutex
	applyFn  func(context.Context, *UsageBillingCommand) (*UsageBillingApplyResult, error)
	commands []UsageBillingCommand
}

type stagedUsageBillingRepoStub struct {
	usageBillingRepoStub
	stageCalls int
	binding    UsageBillingOutboxBinding
	stageFn    func(context.Context, *UsageBillingCommand, UsageBillingOutboxBinding) (*UsageBillingApplyResult, error)
}

func (r *stagedUsageBillingRepoStub) ApplyAndStageOutboxFinalization(ctx context.Context, cmd *UsageBillingCommand, binding UsageBillingOutboxBinding) (*UsageBillingApplyResult, error) {
	r.stageCalls++
	r.binding = binding
	if r.stageFn != nil {
		return r.stageFn(ctx, cmd, binding)
	}
	return &UsageBillingApplyResult{Applied: true}, nil
}

func (r *usageBillingRepoStub) Apply(ctx context.Context, cmd *UsageBillingCommand) (*UsageBillingApplyResult, error) {
	r.mu.Lock()
	if cmd != nil {
		r.commands = append(r.commands, *cmd)
	}
	r.mu.Unlock()
	if r.applyFn != nil {
		return r.applyFn(ctx, cmd)
	}
	return &UsageBillingApplyResult{Applied: true}, nil
}

func validBillingOutboxRecord(id int64) BillingOutboxRecord {
	return BillingOutboxRecord{
		ID: id,
		Command: BillingOutboxCommand{
			AttemptID:          "attempt-1",
			RequestID:          "request-1",
			APIKeyID:           11,
			RequestFingerprint: "fingerprint-1",
			Billing: UsageBillingCommand{
				RequestID:          "request-1",
				APIKeyID:           11,
				RequestFingerprint: "fingerprint-1",
				AccountID:          22,
			},
		},
	}
}

func TestBillingOutboxWorker_RejectsRepositoryWithoutDurableFinalization(t *testing.T) {
	repo := &billingOutboxRepoStub{}
	billing := &usageBillingRepoStub{}
	worker := NewBillingOutboxWorker(repo, billing)

	worker.processRecord(context.Background(), validBillingOutboxRecord(7))

	require.Empty(t, billing.commands)
	require.Empty(t, repo.acked)
	require.Len(t, repo.retried, 1)
	require.False(t, repo.retried[0].terminal)
	require.Equal(t, ErrBillingOutboxFinalizationUnsupported.Error(), repo.retried[0].lastError)
	require.Equal(t, uint64(1), worker.Health(context.Background()).Failures)
}

func TestBillingOutboxWorker_FinalizesOnlyDurablyClaimedEffects(t *testing.T) {
	record := validBillingOutboxRecord(70)
	record.Status = "finalizing"
	record.ApplyResult = &UsageBillingApplyResult{Applied: true}
	repo := &billingOutboxRepoStub{finalizationRecords: []BillingOutboxRecord{record}}
	billing := &stagedUsageBillingRepoStub{}
	postProcessor := &billingOutboxPostProcessorStub{}
	worker := NewBillingOutboxWorker(repo, billing, postProcessor)

	require.NoError(t, worker.processBatch(context.Background()))

	require.Equal(t, 1, postProcessor.calls)
	require.Equal(t, "attempt-1", postProcessor.command.AttemptID)
	require.True(t, postProcessor.result.Applied)
	require.Equal(t, []int64{70}, repo.finalizationAcked)
	require.Empty(t, repo.acked)
}

func TestBillingOutboxWorker_AcknowledgesDeduplicatedStagedReplay(t *testing.T) {
	repo := &billingOutboxRepoStub{}
	billing := &stagedUsageBillingRepoStub{stageFn: func(context.Context, *UsageBillingCommand, UsageBillingOutboxBinding) (*UsageBillingApplyResult, error) {
		return &UsageBillingApplyResult{Applied: false}, nil
	}}
	worker := NewBillingOutboxWorker(repo, billing)

	worker.processRecord(context.Background(), validBillingOutboxRecord(71))

	require.Equal(t, []int64{71}, repo.acked)
}

func TestBillingOutboxWorker_StagesNewBillingBeforeFinalization(t *testing.T) {
	repo := &billingOutboxRepoStub{}
	billing := &stagedUsageBillingRepoStub{}
	worker := NewBillingOutboxWorker(repo, billing)

	worker.processRecord(context.Background(), validBillingOutboxRecord(72))

	require.Equal(t, 1, billing.stageCalls)
	require.Equal(t, int64(72), billing.binding.OutboxID)
	require.Equal(t, worker.workerID, billing.binding.WorkerID)
	require.Empty(t, repo.acked)
}

func TestBillingOutboxWorker_AcknowledgesDeduplicatedStagedCommand(t *testing.T) {
	repo := &billingOutboxRepoStub{}
	billing := &stagedUsageBillingRepoStub{stageFn: func(context.Context, *UsageBillingCommand, UsageBillingOutboxBinding) (*UsageBillingApplyResult, error) {
		return &UsageBillingApplyResult{Applied: false}, nil
	}}
	worker := NewBillingOutboxWorker(repo, billing)

	worker.processRecord(context.Background(), validBillingOutboxRecord(73))

	require.Equal(t, []int64{73}, repo.acked)
}

func TestBillingOutboxWorker_ReplaysDurablyStagedFinalizationWithoutApplyingAgain(t *testing.T) {
	record := validBillingOutboxRecord(73)
	record.Status = "finalizing"
	record.ApplyResult = &UsageBillingApplyResult{Applied: true}
	repo := &billingOutboxRepoStub{finalizationRecords: []BillingOutboxRecord{record}}
	billing := &usageBillingRepoStub{}
	postProcessor := &billingOutboxPostProcessorStub{}
	worker := NewBillingOutboxWorker(repo, billing, postProcessor)

	require.NoError(t, worker.processBatch(context.Background()))

	require.Empty(t, billing.commands)
	require.Equal(t, 1, postProcessor.calls)
	require.Equal(t, []int64{73}, repo.finalizationAcked)
}

func TestBillingOutboxWorker_RetriesFinalizationFailureWithoutAcknowledging(t *testing.T) {
	record := validBillingOutboxRecord(74)
	record.Status = "finalizing"
	record.ApplyResult = &UsageBillingApplyResult{Applied: true}
	repo := &billingOutboxRepoStub{finalizationRecords: []BillingOutboxRecord{record}}
	billing := &usageBillingRepoStub{}
	postProcessor := &billingOutboxPostProcessorStub{err: errors.New("cache unavailable")}
	worker := NewBillingOutboxWorker(repo, billing, postProcessor)

	require.NoError(t, worker.processBatch(context.Background()))

	require.Empty(t, repo.finalizationAcked)
	require.Len(t, repo.finalizationRetried, 1)
	require.False(t, repo.finalizationRetried[0].terminal)
}

func TestBillingOutboxWorker_KeepsAppliedFinalizationRetryablePastAttemptLimit(t *testing.T) {
	record := validBillingOutboxRecord(75)
	record.Status = "finalizing"
	record.Attempts = billingOutboxMaxAttempts
	record.ApplyResult = &UsageBillingApplyResult{Applied: true}
	repo := &billingOutboxRepoStub{finalizationRecords: []BillingOutboxRecord{record}}
	postProcessor := &billingOutboxPostProcessorStub{err: errors.New("notification provider unavailable")}
	worker := NewBillingOutboxWorker(repo, &usageBillingRepoStub{}, postProcessor)

	require.NoError(t, worker.processBatch(context.Background()))

	require.Empty(t, repo.finalizationAcked)
	require.Len(t, repo.finalizationRetried, 1)
	require.False(t, repo.finalizationRetried[0].terminal)
	require.False(t, repo.finalizationRetried[0].availableAt.IsZero())
}

func TestBillingOutboxWorker_QuarantinesTransientFailureAtAttemptLimit(t *testing.T) {
	repo := &billingOutboxRepoStub{}
	billing := &usageBillingRepoStub{applyFn: func(context.Context, *UsageBillingCommand) (*UsageBillingApplyResult, error) {
		return nil, errors.New("database temporarily unavailable")
	}}
	worker := NewBillingOutboxWorker(repo, billing)
	record := validBillingOutboxRecord(81)
	record.Attempts = billingOutboxMaxAttempts

	worker.processRecord(context.Background(), record)

	require.Len(t, repo.retried, 1)
	require.True(t, repo.retried[0].terminal)
	require.True(t, repo.retried[0].availableAt.IsZero())
}

func TestBillingOutboxWorker_RetriesTransientStagedApplyFailureWithBoundedError(t *testing.T) {
	repo := &billingOutboxRepoStub{}
	billing := &stagedUsageBillingRepoStub{stageFn: func(context.Context, *UsageBillingCommand, UsageBillingOutboxBinding) (*UsageBillingApplyResult, error) {
		return nil, errors.New(strings.Repeat("database temporarily unavailable ", 200))
	}}
	worker := NewBillingOutboxWorker(repo, billing)
	before := time.Now().UTC()

	worker.processRecord(context.Background(), validBillingOutboxRecord(8))

	require.Empty(t, repo.acked)
	require.Len(t, repo.retried, 1)
	require.False(t, repo.retried[0].terminal)
	require.Equal(t, int64(8), repo.retried[0].id)
	require.Greater(t, repo.retried[0].availableAt, before)
	require.Len(t, repo.retried[0].lastError, BillingOutboxLastErrorLimit)
	require.Equal(t, uint64(1), worker.Health(context.Background()).Failures)
}

func TestBillingOutboxWorker_RetainsPoisonCommandsAsTerminal(t *testing.T) {
	repo := &billingOutboxRepoStub{}
	billing := &usageBillingRepoStub{}
	worker := NewBillingOutboxWorker(repo, billing)
	record := validBillingOutboxRecord(9)
	record.Command.APIKeyID = 0
	record.Command.Billing.APIKeyID = 0

	worker.processRecord(context.Background(), record)

	require.Empty(t, billing.commands)
	require.Len(t, repo.retried, 1)
	require.True(t, repo.retried[0].terminal)
	require.NotEmpty(t, repo.retried[0].lastError)
}

func TestBillingOutboxWorker_RetainsDeterministicStagedBillingFailureAsTerminal(t *testing.T) {
	repo := &billingOutboxRepoStub{}
	billing := &stagedUsageBillingRepoStub{stageFn: func(context.Context, *UsageBillingCommand, UsageBillingOutboxBinding) (*UsageBillingApplyResult, error) {
		return nil, ErrSubscriptionNotFound
	}}
	worker := NewBillingOutboxWorker(repo, billing)

	worker.processRecord(context.Background(), validBillingOutboxRecord(10))

	require.Len(t, repo.retried, 1)
	require.True(t, repo.retried[0].terminal)
	require.Empty(t, repo.acked)
}

func TestBillingOutboxLeaseOutlivesBoundedApplyAndFinalization(t *testing.T) {
	require.Greater(t, billingOutboxLease, 2*billingOutboxApplyTimeout)
}

func TestBillingOutboxWorker_ClaimsBoundedBatchAndReportsHealth(t *testing.T) {
	oldest := time.Now().Add(-time.Minute)
	repo := &billingOutboxRepoStub{stats: BillingOutboxStats{
		Pending: 12, Processing: 3, Terminal: 2, MaxAttempts: 4, OldestCreatedAt: &oldest, LastError: "prior failure",
	}}
	worker := NewBillingOutboxWorker(repo, &usageBillingRepoStub{})

	require.NoError(t, worker.processBatch(context.Background()))
	require.Equal(t, billingOutboxBatchSize, repo.claimLimit)
	require.NotEmpty(t, repo.workerID)
	require.Equal(t, billingOutboxLease, repo.lease)

	health := worker.Health(context.Background())
	require.Equal(t, int64(12), health.Pending)
	require.Equal(t, int64(3), health.Processing)
	require.Equal(t, int64(2), health.Terminal)
	require.Equal(t, 4, health.MaxAttempts)
	require.Equal(t, "prior failure", health.LastError)
	require.GreaterOrEqual(t, health.OldestLag, time.Minute)
}

func TestBillingOutboxWorker_ProcessesBatchConcurrently(t *testing.T) {
	records := make([]BillingOutboxRecord, 32)
	for i := range records {
		records[i] = validBillingOutboxRecord(int64(i + 1))
	}
	repo := &billingOutboxRepoStub{records: records}
	billing := &stagedUsageBillingRepoStub{stageFn: func(context.Context, *UsageBillingCommand, UsageBillingOutboxBinding) (*UsageBillingApplyResult, error) {
		time.Sleep(100 * time.Millisecond)
		return &UsageBillingApplyResult{Applied: false}, nil
	}}
	worker := NewBillingOutboxWorker(repo, billing)

	started := time.Now()
	require.NoError(t, worker.processBatch(context.Background()))
	require.Less(t, time.Since(started), time.Second)
	require.Len(t, repo.acked, 32)
}

func TestBillingOutboxWorker_LifecycleIsManagedAndIdempotent(t *testing.T) {
	worker := NewBillingOutboxWorker(&billingOutboxRepoStub{}, &usageBillingRepoStub{})

	worker.Start()
	require.Eventually(t, func() bool { return worker.Health(context.Background()).Running }, time.Second, 10*time.Millisecond)
	require.NotPanics(t, func() { worker.Stop(); worker.Stop() })
	require.False(t, worker.Health(context.Background()).Running)
}

func TestBillingOutboxRetryDelayIsBounded(t *testing.T) {
	for attempt := 1; attempt <= 20; attempt++ {
		delay := billingOutboxRetryDelay(attempt)
		require.GreaterOrEqual(t, delay, 800*time.Millisecond)
		require.LessOrEqual(t, delay, 308*time.Second)
	}
}
