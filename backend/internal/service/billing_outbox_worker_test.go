package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
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

func TestBillingOutboxWorker_FinalizationBatchDeadlineBoundsBlockedFinalize(t *testing.T) {
	// 整轮 finalization 总时限：Finalize 阻塞（实现尊重 ctx，直到取消才返回）
	// 时，batch deadline 到期即结束本轮——在场记录经派生 ctx 取消后走 retry
	// 释放回 finalization_pending，轮次绝不无限挂起。per-record 时限注入为大值，
	// 保证唯一能结束本轮的是 batch deadline（无 batch 时限时本测试在 2s 超时
	// 处失败——正是修复前的无限阻塞行为）。
	records := make([]BillingOutboxRecord, billingOutboxConcurrency)
	for i := range records {
		records[i] = validBillingOutboxRecord(int64(i + 1))
		records[i].Status = "finalizing"
		records[i].ApplyResult = &UsageBillingApplyResult{Applied: true}
	}
	repo := &billingOutboxRepoStub{finalizationRecords: records}
	postProcessor := billingOutboxPostProcessorFunc(func(ctx context.Context, _ *BillingOutboxCommand, _ *UsageBillingApplyResult) error {
		<-ctx.Done()
		return ctx.Err()
	})
	worker := NewBillingOutboxWorker(repo, &usageBillingRepoStub{}, postProcessor)
	worker.finalizeRecordTimeout = 10 * time.Second
	worker.finalizationBatchTimeout = 100 * time.Millisecond

	started := time.Now()
	finished := make(chan error, 1)
	go func() { _, err := worker.processBatch(context.Background()); finished <- err }()
	select {
	case err := <-finished:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("finalization batch did not return within the batch deadline")
	}
	require.Less(t, time.Since(started), time.Second,
		"batch deadline must bound the round, not the per-record timeout")
	require.Empty(t, repo.finalizationAcked)
	require.Len(t, repo.finalizationRetried, len(records),
		"records in flight at the deadline must be released to the retry pool")
	for _, retry := range repo.finalizationRetried {
		require.False(t, retry.terminal, "deadline interruption must stay retryable")
	}
}

func TestBillingOutboxWorker_FinalizeRecordTimeoutReleasesRecordToRetryPool(t *testing.T) {
	// 单条记录独立时限：Finalize 阻塞时 record deadline 到期即取消该记录，
	// 错误走 persistFinalizationFailure 的 retry 路径（terminal=false、退避后移
	// available_at），记录回到 finalization_pending 补领池，下一轮可再领重试。
	record := validBillingOutboxRecord(77)
	record.Status = "finalizing"
	record.ApplyResult = &UsageBillingApplyResult{Applied: true}
	repo := &billingOutboxRepoStub{finalizationRecords: []BillingOutboxRecord{record}}
	postProcessor := billingOutboxPostProcessorFunc(func(ctx context.Context, _ *BillingOutboxCommand, _ *UsageBillingApplyResult) error {
		<-ctx.Done()
		return ctx.Err()
	})
	worker := NewBillingOutboxWorker(repo, &usageBillingRepoStub{}, postProcessor)
	worker.finalizeRecordTimeout = 50 * time.Millisecond
	worker.finalizationBatchTimeout = 5 * time.Second

	before := time.Now().UTC()
	finished := make(chan error, 1)
	go func() { _, err := worker.processBatch(context.Background()); finished <- err }()
	select {
	case err := <-finished:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("per-record deadline did not bound the blocked finalizer")
	}
	require.Empty(t, repo.finalizationAcked)
	require.Len(t, repo.finalizationRetried, 1)
	require.False(t, repo.finalizationRetried[0].terminal, "timed-out finalization must not terminalize")
	require.Greater(t, repo.finalizationRetried[0].availableAt, before,
		"retry must carry backoff so the record returns to the claim pool later")
	require.Contains(t, repo.finalizationRetried[0].lastError, "finalize billing outbox command")

	// 记录已释放回补领池：下一轮再次 Claim 并重试 Finalize。
	_, err := worker.processBatch(context.Background())
	require.NoError(t, err)
	require.Len(t, repo.finalizationRetried, 2, "timed-out record must be re-claimable and re-processed")
}

func TestBillingOutboxWorker_StopExitsPromptlyWhileFinalizationBlocked(t *testing.T) {
	// Stop 时 Finalize 阻塞中必须及时退出：worker ctx 取消沿 batch/record 派生
	// ctx 传播，阻塞中的 Finalize 返回后本轮结束、run 循环退出，Stop 不被卡死。
	record := validBillingOutboxRecord(78)
	record.Status = "finalizing"
	record.ApplyResult = &UsageBillingApplyResult{Applied: true}
	repo := &billingOutboxRepoStub{finalizationRecords: []BillingOutboxRecord{record}}
	started := make(chan struct{})
	postProcessor := billingOutboxPostProcessorFunc(func(ctx context.Context, _ *BillingOutboxCommand, _ *UsageBillingApplyResult) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	worker := NewBillingOutboxWorker(repo, &usageBillingRepoStub{}, postProcessor)
	worker.finalizeRecordTimeout = 10 * time.Second // 仅 Stop 能取消阻塞中的 Finalize
	worker.finalizationBatchTimeout = 20 * time.Second
	worker.finalizationLease = 40 * time.Millisecond
	worker.finalizationLeaseRenewInterval = 10 * time.Millisecond
	worker.finalizationDBTimeout = 20 * time.Millisecond
	worker.Start()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("finalizer did not start")
	}
	stopped := make(chan struct{})
	go func() { worker.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop blocked while a finalizer was blocked")
	}
	require.False(t, worker.Health(context.Background()).Running)
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

// TestBillingOutboxWorker_FinalizationPgErrorRetryableBelowMaxAttempts：finalization
// 路径的 PG 42xxx/22xxx（部署可修复）受 maxAttempts 约束——attempts 未到
// billingOutboxMaxAttempts 时保持 pending 重试（已扣费记录的后置效应必须保持
// 可重放），不得在首次失败就 terminal。
func TestBillingOutboxWorker_FinalizationPgErrorRetryableBelowMaxAttempts(t *testing.T) {
	record := validBillingOutboxRecord(94)
	record.Status = "finalizing"
	record.Attempts = billingOutboxMaxAttempts - 1
	record.ApplyResult = &UsageBillingApplyResult{Applied: true}
	repo := &billingOutboxRepoStub{finalizationRecords: []BillingOutboxRecord{record}}
	postProcessor := &billingOutboxPostProcessorStub{err: pgError42P01()}
	worker := NewBillingOutboxWorker(repo, &usageBillingRepoStub{}, postProcessor)

	_, err := worker.processBatch(context.Background())
	require.NoError(t, err)

	require.Empty(t, repo.finalizationAcked)
	require.Len(t, repo.finalizationRetried, 1)
	require.False(t, repo.finalizationRetried[0].terminal, "finalization PG error below max attempts must stay retryable")
	require.False(t, repo.finalizationRetried[0].availableAt.IsZero(), "retryable finalization failure must be retried with backoff")
}

// TestBillingOutboxWorker_FinalizationPgErrorTerminalAtMaxAttempts：finalization 的
// PG 42xxx/22xxx 在 attempts ≥ billingOutboxMaxAttempts 时 terminal，且 last_error
// 带 [SQLSTATE 42P01] 前缀（对账与恢复定位可见根因码）。
func TestBillingOutboxWorker_FinalizationPgErrorTerminalAtMaxAttempts(t *testing.T) {
	record := validBillingOutboxRecord(95)
	record.Status = "finalizing"
	record.Attempts = billingOutboxMaxAttempts
	record.ApplyResult = &UsageBillingApplyResult{Applied: true}
	repo := &billingOutboxRepoStub{finalizationRecords: []BillingOutboxRecord{record}}
	postProcessor := &billingOutboxPostProcessorStub{err: pgError42P01()}
	worker := NewBillingOutboxWorker(repo, &usageBillingRepoStub{}, postProcessor)

	_, err := worker.processBatch(context.Background())
	require.NoError(t, err)

	require.Empty(t, repo.finalizationAcked)
	require.Len(t, repo.finalizationRetried, 1)
	require.True(t, repo.finalizationRetried[0].terminal, "finalization PG error at max attempts must be terminal")
	require.True(t, repo.finalizationRetried[0].availableAt.IsZero())
	require.Contains(t, repo.finalizationRetried[0].lastError, "[SQLSTATE 42P01]", "terminal last_error must carry the SQLSTATE prefix")
}

// TestBillingOutboxWorker_FinalizationLegacyTerminalImmediateAtAnyAttempts：legacy
// 立即 terminal 分类（infra 4xx + 哨兵，确定性客户端数据毒药）在 finalization
// 保持立即 terminal（pre-existing 行为），不受 maxAttempts 约束。
func TestBillingOutboxWorker_FinalizationLegacyTerminalImmediateAtAnyAttempts(t *testing.T) {
	record := validBillingOutboxRecord(96)
	record.Status = "finalizing"
	record.Attempts = 0
	record.ApplyResult = &UsageBillingApplyResult{Applied: true}
	repo := &billingOutboxRepoStub{finalizationRecords: []BillingOutboxRecord{record}}
	postProcessor := &billingOutboxPostProcessorStub{err: ErrSubscriptionNotFound}
	worker := NewBillingOutboxWorker(repo, &usageBillingRepoStub{}, postProcessor)

	_, err := worker.processBatch(context.Background())
	require.NoError(t, err)

	require.Empty(t, repo.finalizationAcked)
	require.Len(t, repo.finalizationRetried, 1)
	require.True(t, repo.finalizationRetried[0].terminal, "legacy immediate-terminal failures must stay terminal in finalization")
	require.True(t, repo.finalizationRetried[0].availableAt.IsZero())
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
	// finalization 整轮最坏持租时长 = batch 总时限（per-record 时限派生自 batch
	// ctx、被其截断，60+30=90 的朴素相加不会发生）：90s 租约必须大于 60s。
	require.Greater(t, billingOutboxLease, billingOutboxFinalizationBatchTimeout)
	require.Greater(t, billingOutboxFinalizationBatchTimeout, billingOutboxFinalizeRecordTimeout)
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

func TestBillingOutboxWorker_CountsBackloggedRounds(t *testing.T) {
	// 积压轮计数：processBatch 判定积压（Claim 拉满且实际消化 > 0）的轮次累计
	// 为 BackloggedRounds（单调不减，消费方取相邻采样 delta）；空拉轮不计数。
	empty := NewBillingOutboxWorker(&billingOutboxRepoStub{}, &batchUsageBillingRepoStub{})
	backlogged, err := empty.processBatch(context.Background())
	require.NoError(t, err)
	require.False(t, backlogged)
	require.Zero(t, empty.Health(context.Background()).BackloggedRounds)

	repo := &billingOutboxRepoStub{records: fullBillingOutboxClaimBatch()}
	worker := NewBillingOutboxWorker(repo, &batchUsageBillingRepoStub{})

	backlogged, err = worker.processBatch(context.Background())
	require.NoError(t, err)
	require.True(t, backlogged)
	require.Equal(t, uint64(1), worker.Health(context.Background()).BackloggedRounds)

	backlogged, err = worker.processBatch(context.Background())
	require.NoError(t, err)
	require.True(t, backlogged)
	require.Equal(t, uint64(2), worker.Health(context.Background()).BackloggedRounds)
}

func TestBillingOutboxWorker_CountsFinalizationBatchRoundTimeouts(t *testing.T) {
	// RoundTimeouts 语义：finalization 整轮 deadline（batch 时限）触发而提前结束
	// 的累计轮次。Finalize 阻塞至 ctx 取消时，batch deadline 到期结束本轮并计数。
	records := make([]BillingOutboxRecord, billingOutboxConcurrency)
	for i := range records {
		records[i] = validBillingOutboxRecord(int64(i + 1))
		records[i].Status = "finalizing"
		records[i].ApplyResult = &UsageBillingApplyResult{Applied: true}
	}
	repo := &billingOutboxRepoStub{finalizationRecords: records}
	postProcessor := billingOutboxPostProcessorFunc(func(ctx context.Context, _ *BillingOutboxCommand, _ *UsageBillingApplyResult) error {
		<-ctx.Done()
		return ctx.Err()
	})
	worker := NewBillingOutboxWorker(repo, &usageBillingRepoStub{}, postProcessor)
	worker.finalizeRecordTimeout = 10 * time.Second
	worker.finalizationBatchTimeout = 100 * time.Millisecond

	finished := make(chan error, 1)
	go func() { _, err := worker.processBatch(context.Background()); finished <- err }()
	select {
	case err := <-finished:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("finalization batch did not return within the batch deadline")
	}
	require.Equal(t, uint64(1), worker.Health(context.Background()).RoundTimeouts)
}

func TestBillingOutboxWorker_PerRecordTimeoutDoesNotCountAsRoundTimeout(t *testing.T) {
	// per-record 时限到期是单条记录的退避释放（回到 finalization_pending 补领池），
	// 不是整轮被 deadline 截断：不得计入 RoundTimeouts。
	record := validBillingOutboxRecord(79)
	record.Status = "finalizing"
	record.ApplyResult = &UsageBillingApplyResult{Applied: true}
	repo := &billingOutboxRepoStub{finalizationRecords: []BillingOutboxRecord{record}}
	postProcessor := billingOutboxPostProcessorFunc(func(ctx context.Context, _ *BillingOutboxCommand, _ *UsageBillingApplyResult) error {
		<-ctx.Done()
		return ctx.Err()
	})
	worker := NewBillingOutboxWorker(repo, &usageBillingRepoStub{}, postProcessor)
	worker.finalizeRecordTimeout = 50 * time.Millisecond
	worker.finalizationBatchTimeout = 5 * time.Second

	_, err := worker.processBatch(context.Background())
	require.NoError(t, err)
	require.Zero(t, worker.Health(context.Background()).RoundTimeouts,
		"per-record deadline release must not count as a round timeout")
	require.Len(t, repo.finalizationRetried, 1)
}

func TestBillingOutboxWorker_TerminalGrowthAlertTriggersOnThresholdAndDedups(t *testing.T) {
	// 告警触发与去重：terminal 绝对量超阈 → slog.Error + Health.TerminalAlert；
	// 冷却窗口内持续条件不重复刷屏；条件回落清除告警状态；窗口内回落后再次越界
	// 不重新告警（重新告警的最小间隔 = 冷却时长，与中间状态无关），窗口结束后
	// 新一次越界重新触发。
	var logBuf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(previous)

	repo := &billingOutboxRepoStub{}
	worker := NewBillingOutboxWorker(repo, &usageBillingRepoStub{})
	worker.terminalAlertThreshold = 5
	worker.terminalAlertCooldown = 30 * time.Minute
	worker.terminalSampleInterval = time.Nanosecond

	ctx := context.Background()
	now := time.Now()
	repo.stats = BillingOutboxStats{Terminal: 10}
	worker.sampleTerminalGrowth(ctx, now)
	require.Contains(t, logBuf.String(), "billing outbox terminal growth")
	require.NotEmpty(t, worker.Health(ctx).TerminalAlert)
	alertLogs := strings.Count(logBuf.String(), "billing outbox terminal growth")

	repo.stats = BillingOutboxStats{Terminal: 12}
	worker.sampleTerminalGrowth(ctx, now.Add(time.Minute))
	require.Equal(t, alertLogs, strings.Count(logBuf.String(), "billing outbox terminal growth"),
		"persistent condition within the cooldown must not re-alert")

	repo.stats = BillingOutboxStats{Terminal: 3}
	worker.sampleTerminalGrowth(ctx, now.Add(2*time.Minute))
	require.Empty(t, worker.Health(ctx).TerminalAlert, "condition recovery must clear the alert flag")

	repo.stats = BillingOutboxStats{Terminal: 9}
	worker.sampleTerminalGrowth(ctx, now.Add(3*time.Minute))
	require.Equal(t, alertLogs, strings.Count(logBuf.String(), "billing outbox terminal growth"),
		"re-crossing within the cooldown window after recovery must not re-alert (oscillation must not reset the window)")
	require.Empty(t, worker.Health(ctx).TerminalAlert,
		"alert state stays dormant until the cooldown window elapses")

	repo.stats = BillingOutboxStats{Terminal: 9}
	worker.sampleTerminalGrowth(ctx, now.Add(31*time.Minute))
	require.Greater(t, strings.Count(logBuf.String(), "billing outbox terminal growth"), alertLogs,
		"a new spike after the cooldown window elapses must re-alert")
	require.NotEmpty(t, worker.Health(ctx).TerminalAlert)
}

func TestBillingOutboxWorker_TerminalGrowthAlertBoundedPerCooldownWindowOnOscillation(t *testing.T) {
	// M1 回归：terminal 计数在阈值附近振荡（清理删除把堆叠拉回阈值下、churn 又
	// 推回阈值上，如 499K↔501K）时，冷却窗口内无论中间是否回落都不重新告警——
	// 每冷却窗口恰好 1 条日志（硬性 ≤1 条/窗口），而不是每次振荡（≈1 条/采样）
	// 刷屏。注入 1 分钟冷却 + 纳秒采样间隔，3 个窗口 × 每窗口 10 轮振荡断言有界。
	var logBuf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(previous)

	repo := &billingOutboxRepoStub{}
	worker := NewBillingOutboxWorker(repo, &usageBillingRepoStub{})
	worker.terminalAlertThreshold = 500_000
	worker.terminalAlertCooldown = time.Minute
	worker.terminalSampleInterval = time.Nanosecond

	ctx := context.Background()
	now := time.Now()
	over, under := int64(501_000), int64(499_000)
	const cycles, windows = 10, 3
	for window := 0; window < windows; window++ {
		// 每个冷却窗口（60s）只取首个越界采样做一次告警；随后 cycles 轮
		// 越界→回落→越界振荡（每秒一轮）全部落在窗口内，不得新增日志。
		base := now.Add(time.Duration(window)*61*time.Second + time.Second)
		repo.stats = BillingOutboxStats{Terminal: over}
		worker.sampleTerminalGrowth(ctx, base)
		require.Equal(t, window+1, strings.Count(logBuf.String(), "billing outbox terminal growth"),
			"the first over-threshold sample of each cooldown window must alert exactly once")
		for i := 0; i < cycles; i++ {
			repo.stats = BillingOutboxStats{Terminal: under}
			worker.sampleTerminalGrowth(ctx, base.Add(time.Duration(i)*time.Second+500*time.Millisecond))
			repo.stats = BillingOutboxStats{Terminal: over}
			worker.sampleTerminalGrowth(ctx, base.Add(time.Duration(i)*time.Second+time.Second))
		}
		require.Equal(t, window+1, strings.Count(logBuf.String(), "billing outbox terminal growth"),
			"oscillation across the threshold within a cooldown window must not add logs, even after recovery")
	}
	require.Equal(t, windows, strings.Count(logBuf.String(), "billing outbox terminal growth"),
		"total alert logs must be bounded to exactly one per cooldown window")
	require.Empty(t, worker.Health(ctx).TerminalAlert,
		"alert state stays dormant within the window after recovery; it re-arms at the next window boundary")
}

func TestBillingOutboxWorker_TerminalGrowthAlertTriggersOnRate(t *testing.T) {
	// 速率维度：首次采样只建立基线（无增长速率可言），第二次采样 delta/实际间隔
	// 超阈即告警——60s 内新增 300 行 = 5 行/s，超过注入的 4 行/s 阈值。
	var logBuf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(previous)

	repo := &billingOutboxRepoStub{}
	worker := NewBillingOutboxWorker(repo, &usageBillingRepoStub{})
	worker.terminalAlertThreshold = 1_000_000 // 抬高绝对量阈值：只验证速率维度
	worker.terminalGrowthRate = 4.0           // 注入 4 行/s 速率阈值
	worker.terminalSampleInterval = time.Nanosecond

	ctx := context.Background()
	now := time.Now()
	repo.stats = BillingOutboxStats{Terminal: 1000}
	worker.sampleTerminalGrowth(ctx, now)
	require.Empty(t, worker.Health(ctx).TerminalAlert, "first sample only establishes the baseline")

	repo.stats = BillingOutboxStats{Terminal: 1300}
	worker.sampleTerminalGrowth(ctx, now.Add(60*time.Second))
	require.Contains(t, logBuf.String(), "billing outbox terminal growth")
	require.NotEmpty(t, worker.Health(ctx).TerminalAlert)
}

func TestBillingOutboxWorker_TerminalGrowthNoAlertWithinBudget(t *testing.T) {
	// 健康稳态（绝对量低于阈值、增长速率在预算内）不告警、不置位；terminal 回落
	// （清理删除）产生负 delta，同样不告警。
	repo := &billingOutboxRepoStub{}
	worker := NewBillingOutboxWorker(repo, &usageBillingRepoStub{})
	worker.terminalAlertThreshold = 500_000
	worker.terminalGrowthRate = 5.0
	worker.terminalSampleInterval = time.Nanosecond

	ctx := context.Background()
	now := time.Now()
	repo.stats = BillingOutboxStats{Terminal: 1000}
	worker.sampleTerminalGrowth(ctx, now)
	repo.stats = BillingOutboxStats{Terminal: 1050}
	worker.sampleTerminalGrowth(ctx, now.Add(60*time.Second)) // ~0.83 行/s，预算内
	repo.stats = BillingOutboxStats{Terminal: 500}
	worker.sampleTerminalGrowth(ctx, now.Add(2*time.Minute)) // 清理删除：负 delta
	require.Empty(t, worker.Health(ctx).TerminalAlert)
}

func TestBillingOutboxWorker_TerminalGrowthSampleErrorLogsWarning(t *testing.T) {
	// M3 回归：采样 repo.Stats 失败不再静默——记 slog.Warn 暴露告警传感器故障
	// （Warn 由独立冷却默认 60s 限频 ≤1 条/分钟，持续失败的有界性见
	// TerminalGrowthWarnBoundedUnderPersistentFailure）；失败不更新基线
	// （lastAt/lastCount），恢复后首次成功采样仍按"首次采样"处理，不会用跨
	// 失败窗口的 delta 误报速率。
	var logBuf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(previous)

	repo := &billingOutboxRepoStub{statsErr: errors.New("stats down")}
	worker := NewBillingOutboxWorker(repo, &usageBillingRepoStub{})
	worker.terminalAlertThreshold = 1_000_000 // 抬高绝对量阈：只验证速率维度
	worker.terminalGrowthRate = 5.0
	worker.terminalSampleInterval = time.Nanosecond
	worker.terminalStatsWarnCooldown = 60 * time.Second

	ctx := context.Background()
	now := time.Now()
	worker.sampleTerminalGrowth(ctx, now)
	require.Contains(t, logBuf.String(), "billing outbox terminal growth stats sampling failed")
	require.Empty(t, worker.Health(ctx).TerminalAlert)

	// 恢复采样恰落在失败 Warn 冷却边界上（60s < 60s 不成立）：放行查询、走
	// 成功路径。若失败误更新了基线（lastAt=now、lastCount=0），本样本会按
	// 5000 行/60s ≈ 83 行/s 误报；不更新基线则只建立基线、不告警。
	repo.statsErr = nil
	repo.stats = BillingOutboxStats{Terminal: 5000}
	worker.sampleTerminalGrowth(ctx, now.Add(time.Minute))
	require.Empty(t, worker.Health(ctx).TerminalAlert,
		"failed samples must not update the baseline (no rate misjudgment across the failure window)")
}

// statsCountingRepoStub 在共享 stub 之上记录 Stats 调用次数（断言失败冷却
// 窗口内确实跳过查询，而非只跳过日志）。
type statsCountingRepoStub struct {
	billingOutboxRepoStub
	statsCalls int
}

func (r *statsCountingRepoStub) Stats(ctx context.Context) (BillingOutboxStats, error) {
	r.statsCalls++
	return r.stats, r.statsErr
}

func TestBillingOutboxWorker_TerminalGrowthWarnBoundedUnderPersistentFailure(t *testing.T) {
	// R1 回归：repo.Stats 持续失败时，失败 Warn 必须按独立冷却（默认 60s）
	// 硬性 ≤1 条/窗口。旧实现把 Warn 限频锚定在 lastAt（仅成功时更新）：
	// 持续失败下 lastAt 冻结、采样门每轮放行，run 轮次节奏（积压热循环 ~100
	// 轮/秒）下 Warn 随轮刷屏。本测试注入纳秒采样间隔模拟每轮都到采样检查点、
	// 60s 失败冷却，3 个窗口 × 每窗口 10 轮失败采样断言：每窗口恰 1 条 Warn
	// 且恰 1 次 Stats 查询（skip-call 形态：冷却窗口内连查询也跳过）；恢复后
	// Warn 停止、首轮成功采样仍按"首次采样"建立基线（不误报速率）。
	var logBuf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(previous)

	repo := &statsCountingRepoStub{}
	repo.statsErr = errors.New("stats down")
	worker := NewBillingOutboxWorker(repo, &usageBillingRepoStub{})
	worker.terminalAlertThreshold = 1_000_000 // 抬高绝对量阈：只验证速率维度
	worker.terminalGrowthRate = 5.0
	worker.terminalSampleInterval = time.Nanosecond // 采样门每轮放行（模拟热循环）
	worker.terminalStatsWarnCooldown = 60 * time.Second

	ctx := context.Background()
	now := time.Now()
	const windows, roundsPerWindow = 3, 10
	for window := 0; window < windows; window++ {
		// 每窗口首轮落在上一窗口 +61s（恰好越过 60s 冷却），窗口内其余轮全部
		// 命中冷却。
		base := now.Add(time.Duration(window)*61*time.Second + time.Second)
		for i := 0; i < roundsPerWindow; i++ {
			worker.sampleTerminalGrowth(ctx, base.Add(time.Duration(i)*10*time.Millisecond))
		}
		require.Equal(t, window+1, strings.Count(logBuf.String(), "billing outbox terminal growth stats sampling failed"),
			"each failure cooldown window must warn exactly once (first round of the window)")
		require.Equal(t, window+1, repo.statsCalls,
			"failed Stats queries must also be skipped inside the cooldown window (skip-call throttle)")
	}
	require.Equal(t, windows, strings.Count(logBuf.String(), "billing outbox terminal growth stats sampling failed"),
		"persistent Stats failure must bound the warn to exactly one per cooldown window")
	require.Empty(t, worker.Health(ctx).TerminalAlert)

	// 恢复（越过冷却边界）：首轮成功采样按"首次采样"建立基线——5000 行/60s
	// （≈83 行/s > 5）不得误报速率；Warn 停止。
	repo.statsErr = nil
	repo.stats = BillingOutboxStats{Terminal: 5000}
	statsCallsBeforeRecovery := repo.statsCalls
	worker.sampleTerminalGrowth(ctx, now.Add(time.Duration(windows)*61*time.Second+2*time.Second))
	require.Equal(t, statsCallsBeforeRecovery+1, repo.statsCalls,
		"the recovered sample must actually query Stats (Health() also queries Stats; assert the delta)")
	require.Empty(t, worker.Health(ctx).TerminalAlert,
		"failed samples must not update the baseline (no rate misjudgment across the failure window)")
	require.Equal(t, windows, strings.Count(logBuf.String(), "billing outbox terminal growth stats sampling failed"),
		"the warn must stop once Stats recovers")
}

func TestBillingOutboxWorker_TerminalGrowthRecoveryAfterLongStatsFailureResetsBaseline(t *testing.T) {
	// R2 回归（基线老化）：evaluateTerminalGrowth 原先只对"无历史基线"（首次
	// 采样）跳过速率评估——已有基线时，长时间 repo.Stats 失败后的恢复采样会拿
	// 整段失败窗口的平均速率评估（600s 间隔 +5000 行 = 8.33 行/s > 5.0）误报
	// 速率告警，"跨失败窗口的 delta 不误报速率"的保证只覆盖首次采样场景。修复：
	// 距上次成功采样超过 billingOutboxTerminalGrowthMaxSampleGap（默认 120s =
	// 2 个采样节奏）的成功采样按"首次采样"重建基线、不评估速率。本测试先建立
	// 正常基线（1000 行）→ Stats 持续失败 10 分钟（10 个 60s 冷却窗口各恰 1 条
	// Warn/1 次查询）→ 恢复采样 +5000 行：不得按陈旧平均误报；基线已重置 →
	// 下一采样（60s 后）再 +5000 行 = 83 行/s > 5.0 → 速率检查重新武装并告警。
	var logBuf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(previous)

	repo := &statsCountingRepoStub{}
	worker := NewBillingOutboxWorker(repo, &usageBillingRepoStub{})
	worker.terminalAlertThreshold = 1_000_000 // 抬高绝对量阈：只验证速率维度
	worker.terminalGrowthRate = 5.0
	worker.terminalSampleInterval = time.Nanosecond // 采样门每轮放行
	worker.terminalStatsWarnCooldown = 60 * time.Second

	ctx := context.Background()
	now := time.Now()

	// 建立正常基线（存在历史基线，与"首次采样"场景区分开）。
	repo.stats = BillingOutboxStats{Terminal: 1000}
	worker.sampleTerminalGrowth(ctx, now)
	require.Empty(t, worker.Health(ctx).TerminalAlert)

	// 持续失败 10 分钟：每 61s 一探（越过 60s Warn 冷却边界），10 条 Warn + 10
	// 次 Stats 查询；基线（lastAt/lastCount）保持不动。
	repo.statsErr = errors.New("stats down")
	for i := 0; i < 10; i++ {
		worker.sampleTerminalGrowth(ctx, now.Add(time.Duration(i)*61*time.Second+time.Second))
	}
	require.Equal(t, 10, strings.Count(logBuf.String(), "billing outbox terminal growth stats sampling failed"),
		"persistent Stats failure must warn exactly once per 60s cooldown window")
	require.Empty(t, worker.Health(ctx).TerminalAlert)

	// 恢复采样（now + 612s：距上次成功采样 612s > 120s 上限，距上次 Warn 62s
	// ≥ 60s 冷却边界）：+5000 行 / 612s ≈ 8.2 行/s > 5.0，若按整段失败窗口的
	// 平均速率评估必误报；现按"首次采样"重建基线（terminal=6000、lastAt=恢复
	// 时刻）、不评估速率 → 无告警。
	repo.statsErr = nil
	repo.stats = BillingOutboxStats{Terminal: 6000}
	statsCallsBeforeRecovery := repo.statsCalls
	recoveryAt := now.Add(10*61*time.Second + 2*time.Second)
	worker.sampleTerminalGrowth(ctx, recoveryAt)
	require.Equal(t, statsCallsBeforeRecovery+1, repo.statsCalls,
		"the recovered sample must actually query Stats (Health() also queries Stats; assert the delta)")
	require.Empty(t, worker.Health(ctx).TerminalAlert,
		"recovery after a long Stats failure must not compute a stale-window average rate (8.2/s > 5.0 would falsely alert)")

	// 基线已重置：下一采样（正常 60s 节奏）再 +5000 行 = 83 行/s > 5.0 → 速率
	// 检查重新武装并触发告警（"billing outbox terminal growth:" 是告警专属前缀，
	// Warn 文案是 "…stats sampling failed"，不匹配）。
	repo.stats = BillingOutboxStats{Terminal: 11000}
	worker.sampleTerminalGrowth(ctx, recoveryAt.Add(60*time.Second))
	require.Contains(t, logBuf.String(), "billing outbox terminal growth:")
	require.NotEmpty(t, worker.Health(ctx).TerminalAlert,
		"the next sample after the reset baseline must re-arm the rate check and alert")
}

func TestBillingOutboxWorker_RunLoopSamplesTerminalGrowthAtCadence(t *testing.T) {
	// run 循环按注入采样间隔调用 repo.Stats 并评估告警（与轮次节奏解耦）：
	// 首次循环即采样，terminal 超阈后 Health.TerminalAlert 置位、slog.Error 落日志。
	var logBuf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(previous)

	repo := &billingOutboxRepoStub{stats: BillingOutboxStats{Terminal: 1000}}
	worker := NewBillingOutboxWorker(repo, &usageBillingRepoStub{})
	worker.terminalAlertThreshold = 5
	worker.terminalSampleInterval = 20 * time.Millisecond
	worker.Start()
	require.Eventually(t, func() bool {
		return worker.Health(context.Background()).TerminalAlert != ""
	}, time.Second, 10*time.Millisecond)
	worker.Stop()
	require.Contains(t, logBuf.String(), "billing outbox terminal growth")
}
