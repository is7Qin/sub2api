package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type billingOutboxRepoStub struct {
	mu sync.Mutex

	records []BillingOutboxRecord
	// claimSeq 非空时，每次 Claim 依次返回对应记录集（测试不同轮次的记录组成）。
	claimSeq                   [][]BillingOutboxRecord
	claimLimit                 int
	expiredLeaseRecords        []BillingOutboxRecord
	expiredLeaseErr            error
	claimErr                   error
	ackErr                     error
	retryErr                   error
	expiredLeaseClaimLimit     int
	finalizationRecords        []BillingOutboxRecord
	finalizationExpiredRecords []BillingOutboxRecord
	finalizationExpiredErr     error
	finalizationClaimLimit     int
	finalizationClaimed        int
	workerID                   string
	lease                      time.Duration
	acked                      []int64
	finalizationAcked          []int64
	retried                    []billingOutboxRetry
	finalizationRetried        []billingOutboxRetry
	stats                      BillingOutboxStats
	statsErr                   error
	// claims 非 nil 时每次 Claim 发送一个时间戳（run 循环轮次节奏断言）。
	claims chan time.Time
	// claim 调用计数（熔断跳过 apply Claim 的断言）。
	applyClaimCalls             int
	expiredLeaseClaimCalls      int
	finalizationClaimCalls      int
	finalizationExpiredClaimCnt int
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
	r.applyClaimCalls++
	r.claimLimit = limit
	r.workerID = workerID
	r.lease = lease
	if r.claimErr != nil {
		return nil, r.claimErr
	}
	if len(r.claimSeq) > 0 {
		batch := r.claimSeq[0]
		r.claimSeq = r.claimSeq[1:]
		if r.claims != nil {
			r.claims <- time.Now()
		}
		return append([]BillingOutboxRecord(nil), batch...), nil
	}
	if r.claims != nil {
		r.claims <- time.Now()
	}
	return append([]BillingOutboxRecord(nil), r.records...), nil
}

func (r *billingOutboxRepoStub) ClaimExpiredLeased(_ context.Context, workerID string, limit int, _ time.Duration) ([]BillingOutboxRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expiredLeaseClaimCalls++
	r.expiredLeaseClaimLimit = limit
	r.workerID = workerID
	if r.expiredLeaseErr != nil {
		return nil, r.expiredLeaseErr
	}
	return append([]BillingOutboxRecord(nil), r.expiredLeaseRecords...), nil
}

func (r *billingOutboxRepoStub) ClaimFinalizationExpiredLeased(_ context.Context, workerID string, limit int, _ time.Duration) ([]BillingOutboxRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finalizationExpiredClaimCnt++
	r.workerID = workerID
	if r.finalizationExpiredErr != nil {
		return nil, r.finalizationExpiredErr
	}
	return append([]BillingOutboxRecord(nil), r.finalizationExpiredRecords...), nil
}

func (r *billingOutboxRepoStub) Ack(_ context.Context, id int64, workerID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.acked = append(r.acked, id)
	r.workerID = workerID
	return r.ackErr
}

func (r *billingOutboxRepoStub) Retry(_ context.Context, id int64, workerID string, availableAt time.Time, lastError string, terminal bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.retried = append(r.retried, billingOutboxRetry{id: id, workerID: workerID, availableAt: availableAt, lastError: lastError, terminal: terminal})
	r.workerID = workerID
	return r.retryErr
}

func (r *billingOutboxRepoStub) ClaimFinalization(_ context.Context, workerID string, limit int, _ time.Duration) ([]BillingOutboxRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finalizationClaimCalls++
	r.workerID = workerID
	r.finalizationClaimLimit = limit
	claimed := r.finalizationRecords
	if len(claimed) > limit {
		claimed = claimed[:limit]
	}
	r.finalizationClaimed = len(claimed)
	return append([]BillingOutboxRecord(nil), claimed...), nil
}

func (r *billingOutboxRepoStub) RenewFinalizationLease(context.Context, int64, string, time.Duration) error {
	return nil
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

func (r *billingOutboxRepoStub) CleanupTerminal(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

type finalizationLeaseRepoStub struct {
	billingOutboxRepoStub
	finalizationRecord BillingOutboxRecord
	leaseUntil         time.Time
	leaseOwner         string
	renewals           int
	failRenewals       int
}

func newFinalizationLeaseRepoStub(record BillingOutboxRecord) *finalizationLeaseRepoStub {
	return &finalizationLeaseRepoStub{
		finalizationRecord: record,
		leaseUntil:         time.Now().Add(time.Hour),
		leaseOwner:         "",
	}
}

func (r *finalizationLeaseRepoStub) ClaimFinalization(_ context.Context, workerID string, limit int, lease time.Duration) ([]BillingOutboxRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if limit <= 0 || (r.leaseOwner != "" && r.leaseUntil.After(time.Now())) {
		return nil, nil
	}
	r.leaseOwner = workerID
	r.leaseUntil = time.Now().Add(lease)
	r.finalizationRecord.LeasedBy = workerID
	return []BillingOutboxRecord{r.finalizationRecord}, nil
}

func (r *finalizationLeaseRepoStub) RenewFinalizationLease(_ context.Context, id int64, workerID string, lease time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id != r.finalizationRecord.ID || r.leaseOwner != workerID || !r.leaseUntil.After(time.Now()) {
		return fmt.Errorf("%w: %d", ErrBillingOutboxClaimLost, id)
	}
	if r.failRenewals > 0 {
		r.failRenewals--
		return errors.New("temporary renewal failure")
	}
	r.leaseUntil = time.Now().Add(lease)
	r.renewals++
	return nil
}

func (r *finalizationLeaseRepoStub) AckFinalization(ctx context.Context, id int64, workerID string) error {
	if err := r.RenewFinalizationLease(ctx, id, workerID, time.Millisecond); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finalizationAcked = append(r.finalizationAcked, id)
	r.leaseOwner = ""
	return nil
}

func (r *finalizationLeaseRepoStub) renewalCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.renewals
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
	r.mu.Lock()
	r.stageCalls++
	r.binding = binding
	stageFn := r.stageFn
	r.mu.Unlock()
	if stageFn != nil {
		return stageFn(ctx, cmd, binding)
	}
	return &UsageBillingApplyResult{Applied: true}, nil
}

func (r *stagedUsageBillingRepoStub) stagingSnapshot() (int, UsageBillingOutboxBinding) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stageCalls, r.binding
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

	_, err := worker.processBatch(context.Background())
	require.NoError(t, err)

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

	stageCalls, binding := billing.stagingSnapshot()
	require.Equal(t, 1, stageCalls)
	require.Equal(t, int64(72), binding.OutboxID)
	require.Equal(t, worker.workerID, binding.WorkerID)
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

	_, err := worker.processBatch(context.Background())
	require.NoError(t, err)

	require.Empty(t, billing.commands)
	require.Equal(t, 1, postProcessor.calls)
	require.Equal(t, []int64{73}, repo.finalizationAcked)
}

func TestBillingOutboxWorkerProcessesFinalizationBatchConcurrently(t *testing.T) {
	records := make([]BillingOutboxRecord, 2*billingOutboxConcurrency)
	for i := range records {
		records[i] = validBillingOutboxRecord(int64(i + 1))
		records[i].Status = "finalizing"
		records[i].ApplyResult = &UsageBillingApplyResult{Applied: true}
	}
	repo := &billingOutboxRepoStub{finalizationRecords: records}
	started := make(chan struct{}, len(records))
	release := make(chan struct{})
	postProcessor := &blockingBillingOutboxPostProcessor{started: started, release: release}
	worker := NewBillingOutboxWorker(repo, &usageBillingRepoStub{}, postProcessor)

	finished := make(chan error, 1)
	go func() { _, err := worker.processBatch(context.Background()); finished <- err }()
	for range billingOutboxConcurrency {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("finalization did not start up to the concurrency limit")
		}
	}
	select {
	case <-started:
		t.Fatal("finalization exceeded the concurrency limit before release")
	case <-time.After(100 * time.Millisecond):
	}
	repo.mu.Lock()
	claimed := repo.finalizationClaimed
	claimLimit := repo.finalizationClaimLimit
	repo.mu.Unlock()
	require.Equal(t, billingOutboxConcurrency, claimLimit)
	require.Equal(t, billingOutboxConcurrency, claimed)

	close(release)
	require.NoError(t, <-finished)
	require.Len(t, repo.finalizationAcked, billingOutboxConcurrency)
}

type blockingBillingOutboxPostProcessor struct {
	started chan<- struct{}
	release <-chan struct{}
}

func (p *blockingBillingOutboxPostProcessor) Finalize(context.Context, *BillingOutboxCommand, *UsageBillingApplyResult) error {
	p.started <- struct{}{}
	<-p.release
	return nil
}

func TestBillingOutboxWorker_RenewsFinalizationLeaseUntilBlockedFinalizeReturns(t *testing.T) {
	record := validBillingOutboxRecord(74)
	record.Status = "finalizing"
	record.ApplyResult = &UsageBillingApplyResult{Applied: true}
	repo := newFinalizationLeaseRepoStub(record)
	started := make(chan struct{})
	release := make(chan struct{})
	postProcessor := &blockingBillingOutboxPostProcessor{started: started, release: release}
	worker := NewBillingOutboxWorker(repo, &usageBillingRepoStub{}, postProcessor)
	worker.finalizationLease = 40 * time.Millisecond
	worker.finalizationLeaseRenewInterval = 10 * time.Millisecond
	worker.finalizationDBTimeout = 20 * time.Millisecond

	finished := make(chan error, 1)
	go func() { _, err := worker.processBatch(context.Background()); finished <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("finalizer did not start")
	}

	// This exceeds the original lease. The live heartbeat must retain the claim,
	// so another worker cannot start a concurrent finalizer.
	time.Sleep(3 * worker.finalizationLease)
	secondPostProcessor := &billingOutboxPostProcessorStub{}
	secondWorker := NewBillingOutboxWorker(repo, &usageBillingRepoStub{}, secondPostProcessor)
	secondWorker.workerID = "worker-b"
	secondWorker.finalizationLease = worker.finalizationLease
	_, err := secondWorker.processBatch(context.Background())
	require.NoError(t, err)
	require.Zero(t, secondPostProcessor.calls)
	require.GreaterOrEqual(t, repo.renewalCount(), 2)

	close(release)
	require.NoError(t, <-finished)
	require.Equal(t, []int64{record.ID}, repo.finalizationAcked)
}

func TestBillingOutboxWorker_KeepsRenewingAfterFailureUntilUninterruptibleFinalizeReturns(t *testing.T) {
	record := validBillingOutboxRecord(75)
	record.Status = "finalizing"
	record.ApplyResult = &UsageBillingApplyResult{Applied: true}
	repo := newFinalizationLeaseRepoStub(record)
	repo.failRenewals = 1
	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	postProcessor := billingOutboxPostProcessorFunc(func(ctx context.Context, _ *BillingOutboxCommand, _ *UsageBillingApplyResult) error {
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release
		return ctx.Err()
	})
	worker := NewBillingOutboxWorker(repo, &usageBillingRepoStub{}, postProcessor)
	worker.finalizationLease = 80 * time.Millisecond
	worker.finalizationLeaseRenewInterval = 10 * time.Millisecond
	worker.finalizationDBTimeout = 20 * time.Millisecond

	finished := make(chan error, 1)
	go func() { _, err := worker.processBatch(context.Background()); finished <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("finalizer did not start")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("finalizer context was not canceled after renewal failed")
	}
	time.Sleep(2 * worker.finalizationLease)
	secondPostProcessor := &billingOutboxPostProcessorStub{}
	secondWorker := NewBillingOutboxWorker(repo, &usageBillingRepoStub{}, secondPostProcessor)
	secondWorker.workerID = "worker-b"
	secondWorker.finalizationLease = worker.finalizationLease
	_, err := secondWorker.processBatch(context.Background())
	require.NoError(t, err)
	require.Zero(t, secondPostProcessor.calls)

	close(release)
	require.NoError(t, <-finished)
	require.Empty(t, repo.finalizationAcked)
	require.Empty(t, repo.finalizationRetried)
}

func TestBillingOutboxWorker_CancelsFinalizerAndDoesNotTransitionAfterRenewalFailure(t *testing.T) {
	record := validBillingOutboxRecord(75)
	record.Status = "finalizing"
	record.ApplyResult = &UsageBillingApplyResult{Applied: true}
	repo := &failingFinalizationRenewalRepoStub{billingOutboxRepoStub: billingOutboxRepoStub{finalizationRecords: []BillingOutboxRecord{record}}}
	canceled := make(chan struct{})
	postProcessor := billingOutboxPostProcessorFunc(func(ctx context.Context, _ *BillingOutboxCommand, _ *UsageBillingApplyResult) error {
		<-ctx.Done()
		close(canceled)
		return ctx.Err()
	})
	worker := NewBillingOutboxWorker(repo, &usageBillingRepoStub{}, postProcessor)
	worker.finalizationLease = 40 * time.Millisecond
	worker.finalizationLeaseRenewInterval = 10 * time.Millisecond
	worker.finalizationDBTimeout = 20 * time.Millisecond

	_, err := worker.processBatch(context.Background())
	require.NoError(t, err)
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("finalizer context was not canceled after lease renewal failed")
	}
	require.Empty(t, repo.finalizationAcked)
	require.Empty(t, repo.finalizationRetried)
}

type billingOutboxPostProcessorFunc func(context.Context, *BillingOutboxCommand, *UsageBillingApplyResult) error

func (f billingOutboxPostProcessorFunc) Finalize(ctx context.Context, command *BillingOutboxCommand, result *UsageBillingApplyResult) error {
	return f(ctx, command, result)
}

type failingFinalizationRenewalRepoStub struct {
	billingOutboxRepoStub
}

func (r *failingFinalizationRenewalRepoStub) RenewFinalizationLease(context.Context, int64, string, time.Duration) error {
	return ErrBillingOutboxClaimLost
}

func TestBillingOutboxWorker_RetriesFinalizationFailureWithoutAcknowledging(t *testing.T) {
	record := validBillingOutboxRecord(74)
	record.Status = "finalizing"
	record.ApplyResult = &UsageBillingApplyResult{Applied: true}
	repo := &billingOutboxRepoStub{finalizationRecords: []BillingOutboxRecord{record}}
	billing := &usageBillingRepoStub{}
	postProcessor := &billingOutboxPostProcessorStub{err: errors.New("cache unavailable")}
	worker := NewBillingOutboxWorker(repo, billing, postProcessor)

	_, err := worker.processBatch(context.Background())
	require.NoError(t, err)

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

	_, err := worker.processBatch(context.Background())
	require.NoError(t, err)

	require.Empty(t, repo.finalizationAcked)
	require.Len(t, repo.finalizationRetried, 1)
	require.False(t, repo.finalizationRetried[0].terminal)
	require.False(t, repo.finalizationRetried[0].availableAt.IsZero())
}

func TestBillingOutboxWorkerKeepsRetryableApplyFailurePendingPastAttemptLimit(t *testing.T) {
	repo := &billingOutboxRepoStub{}
	billing := &stagedUsageBillingRepoStub{stageFn: func(context.Context, *UsageBillingCommand, UsageBillingOutboxBinding) (*UsageBillingApplyResult, error) {
		return nil, errors.New("database temporarily unavailable")
	}}
	worker := NewBillingOutboxWorker(repo, billing)
	record := validBillingOutboxRecord(81)
	record.Attempts = billingOutboxMaxAttempts

	worker.processRecord(context.Background(), record)

	require.Len(t, repo.retried, 1)
	require.False(t, repo.retried[0].terminal)
	require.False(t, repo.retried[0].availableAt.IsZero())
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

func TestBillingOutboxWorker_ClaimsOnlyRunnableApplyBatch(t *testing.T) {
	// apply Claim 批量与并发数解耦：pending 与过期租约分支都以
	// billingOutboxClaimBatchSize（500）为限；空拉不报告积压。
	repo := &billingOutboxRepoStub{}
	worker := NewBillingOutboxWorker(repo, &usageBillingRepoStub{})

	backlogged, err := worker.processBatch(context.Background())
	require.NoError(t, err)
	require.False(t, backlogged, "empty claim must not report backlog")
	require.Equal(t, billingOutboxClaimBatchSize, repo.claimLimit)
	require.Equal(t, billingOutboxClaimBatchSize, repo.expiredLeaseClaimLimit)
}

func TestBillingOutboxWorker_MergesPendingAndExpiredLeaseClaimsIntoOneApplyBatch(t *testing.T) {
	// 同一轮内 pending 分支与过期租约分支各 Claim 一批，合并后一次 apply：
	// 两批记录都必须被消化，且共享同一个 processApplyBatch（保持既有语义）。
	pending := []BillingOutboxRecord{batchValidRecord(1, 42), batchValidRecord(2, 42)}
	expired := []BillingOutboxRecord{batchValidRecord(3, 42), batchValidRecord(4, 42)}
	repo := &billingOutboxRepoStub{records: pending, expiredLeaseRecords: expired}
	billing := &batchUsageBillingRepoStub{}
	worker := NewBillingOutboxWorker(repo, billing)

	_, err := worker.processBatch(context.Background())
	require.NoError(t, err)

	calls, items := billing.batchSnapshot()
	require.Equal(t, 1, calls, "merged claims must share one per-shard batch transaction")
	require.Len(t, items, 1)
	require.Equal(t, []int64{1, 2, 3, 4}, outboxIDsOf(items[0]))
	require.Empty(t, repo.acked)
	require.Empty(t, repo.retried)
}

func TestBillingOutboxWorker_MergesFinalizationClaimsFromBothBranches(t *testing.T) {
	first := validBillingOutboxRecord(80)
	first.Status = "finalizing"
	first.ApplyResult = &UsageBillingApplyResult{Applied: true}
	expired := validBillingOutboxRecord(81)
	expired.Status = "finalizing"
	expired.ApplyResult = &UsageBillingApplyResult{Applied: true}
	repo := &billingOutboxRepoStub{
		finalizationRecords:        []BillingOutboxRecord{first},
		finalizationExpiredRecords: []BillingOutboxRecord{expired},
	}
	postProcessor := &billingOutboxPostProcessorStub{}
	worker := NewBillingOutboxWorker(repo, &usageBillingRepoStub{}, postProcessor)

	_, err := worker.processBatch(context.Background())
	require.NoError(t, err)

	require.Equal(t, 2, postProcessor.calls)
	require.ElementsMatch(t, []int64{80, 81}, repo.finalizationAcked)
}

func TestBillingOutboxWorker_ReturnsErrorWhenExpiredLeaseClaimFails(t *testing.T) {
	repo := &billingOutboxRepoStub{expiredLeaseErr: errors.New("database temporarily unavailable")}
	worker := NewBillingOutboxWorker(repo, &usageBillingRepoStub{})

	_, err := worker.processBatch(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "expired")
}

func TestBillingOutboxWorker_ReportsHealth(t *testing.T) {
	oldest := time.Now().Add(-time.Minute)
	repo := &billingOutboxRepoStub{stats: BillingOutboxStats{
		Pending: 12, Processing: 3, Terminal: 2, MaxAttempts: 4, OldestCreatedAt: &oldest, LastError: "prior failure",
	}}
	worker := NewBillingOutboxWorker(repo, &usageBillingRepoStub{})

	_, err := worker.processBatch(context.Background())
	require.NoError(t, err)
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
	_, err := worker.processBatch(context.Background())
	require.NoError(t, err)
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

// fullBillingOutboxClaimBatch 构造一个恰好拉满 apply Claim 批量上限的记录集。
func fullBillingOutboxClaimBatch() []BillingOutboxRecord {
	records := make([]BillingOutboxRecord, billingOutboxClaimBatchSize)
	for i := range records {
		records[i] = batchValidRecord(int64(i+1), int64(i%7+1))
	}
	return records
}

func TestBillingOutboxWorker_DrainsContinuouslyWhileBacklogged(t *testing.T) {
	// 背压感知连续拉：满额 Claim 轮之间不得等待 poll 间隔。固定 ticker 实现
	// 每轮都等 poll（4 次 Claim 最早也要 3*poll 完成），背压实现应远小于一个
	// poll 完成 3 轮满额 + 1 轮空拉（空拉本身不触发连续拉）。
	const poll = 400 * time.Millisecond
	repo := &billingOutboxRepoStub{claims: make(chan time.Time, 16)}
	repo.claimSeq = [][]BillingOutboxRecord{
		fullBillingOutboxClaimBatch(), fullBillingOutboxClaimBatch(), fullBillingOutboxClaimBatch(), nil,
	}
	worker := NewBillingOutboxWorker(repo, &batchUsageBillingRepoStub{})
	worker.pollInterval = poll
	worker.Start()
	defer worker.Stop()

	first := <-repo.claims
	var last time.Time
	for i := 1; i <= 3; i++ {
		select {
		case last = <-repo.claims:
		case <-time.After(2 * time.Second):
			t.Fatalf("claim %d never happened", i+1)
		}
	}
	require.Less(t, last.Sub(first), poll,
		"backlogged rounds must not wait for the poll interval between claims")
}

func TestBillingOutboxWorker_WaitsPollIntervalAfterEmptyRound(t *testing.T) {
	// 背压语义的防忙循环半边：满额轮立即接下一轮（不等 poll），空拉轮必须
	// 回落 poll 间隔（不能空转），且之后仍会继续轮询（不能停摆）。
	const poll = 300 * time.Millisecond
	repo := &billingOutboxRepoStub{claims: make(chan time.Time, 16)}
	repo.claimSeq = [][]BillingOutboxRecord{fullBillingOutboxClaimBatch(), nil}
	worker := NewBillingOutboxWorker(repo, &batchUsageBillingRepoStub{})
	worker.pollInterval = poll
	worker.Start()
	defer worker.Stop()

	fullAt := <-repo.claims  // 轮 1：满额
	emptyAt := <-repo.claims // 轮 2：空拉
	require.Less(t, emptyAt.Sub(fullAt), poll/2,
		"full round must drain immediately into the next round")

	select {
	case next := <-repo.claims: // 轮 3：等待 poll 之后才到
		require.GreaterOrEqual(t, next.Sub(emptyAt), poll/2,
			"empty round must wait for the poll interval before the next claim")
	case <-time.After(2 * poll):
		t.Fatal("worker stopped polling after an empty round")
	}
}

func TestBillingOutboxWorker_WaitsAfterFullClaimWithZeroProgress(t *testing.T) {
	// 防忙循环的最坏情形：Claim 拉满（500）但没有任何记录被消化（全部
	// 走去重 Ack 且 Ack 失败，记录仍被租约持有）。此时必须回落 poll 间隔
	// 等待，不能因"拉满"就连续拉——否则同样 500 条会一直空转。
	const poll = 300 * time.Millisecond
	repo := &billingOutboxRepoStub{claims: make(chan time.Time, 16), ackErr: errors.New("ack unavailable")}
	repo.claimSeq = [][]BillingOutboxRecord{fullBillingOutboxClaimBatch(), nil}
	billing := &batchUsageBillingRepoStub{batchFn: func(_ context.Context, items []UsageBillingBatchItem) ([]UsageBillingBatchOutcome, error) {
		outcomes := make([]UsageBillingBatchOutcome, len(items))
		for i := range outcomes {
			outcomes[i].Result = &UsageBillingApplyResult{Applied: false} // 全部走去重 Ack 路径
		}
		return outcomes, nil
	}}
	worker := NewBillingOutboxWorker(repo, billing)
	worker.pollInterval = poll
	worker.Start()
	defer worker.Stop()

	fullAt := <-repo.claims // 轮 1：拉满 500，零消化
	select {
	case next := <-repo.claims: // 轮 2：必须等到 poll 之后
		require.GreaterOrEqual(t, next.Sub(fullAt), poll/2,
			"full claim with zero progress must not loop immediately")
	case <-time.After(2 * poll):
		t.Fatal("worker never polled again after the zero-progress round")
	}
}

func TestBillingOutboxWorker_StopExitsPromptlyDuringContinuousDrain(t *testing.T) {
	// 连续拉循环中 Stop 必须及时退出：ctx 取消经每轮 processBatch 返回后的
	// 检查生效，不能被无限拉取卡住。poll 保持默认值，证明退出不依赖 poll 命中。
	repo := &billingOutboxRepoStub{claims: make(chan time.Time, 16)}
	repo.claimSeq = [][]BillingOutboxRecord{
		fullBillingOutboxClaimBatch(), fullBillingOutboxClaimBatch(), fullBillingOutboxClaimBatch(),
		fullBillingOutboxClaimBatch(), fullBillingOutboxClaimBatch(),
	}
	worker := NewBillingOutboxWorker(repo, &batchUsageBillingRepoStub{})
	worker.Start()

	select {
	case <-repo.claims: // 至少一轮满额拉取后立即停止
	case <-time.After(2 * time.Second):
		t.Fatal("worker never claimed")
	}
	stopped := make(chan struct{})
	go func() {
		worker.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop blocked during continuous drain")
	}
	require.False(t, worker.Health(context.Background()).Running)
}

func TestBillingOutboxRetryDelayIsBounded(t *testing.T) {
	for attempt := 1; attempt <= 20; attempt++ {
		delay := billingOutboxRetryDelay(attempt)
		require.GreaterOrEqual(t, delay, 800*time.Millisecond)
		require.LessOrEqual(t, delay, 308*time.Second)
	}
}
