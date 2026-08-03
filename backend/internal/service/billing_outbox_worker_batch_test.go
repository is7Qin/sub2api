package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// batchUsageBillingRepoStub 同时实现单条 staged 路径与批量路径，
// 用于断言 worker 优先走批量事务、失败隔离与重试语义。
type batchUsageBillingRepoStub struct {
	stagedUsageBillingRepoStub
	mu         sync.Mutex
	batchCalls int
	batchItems [][]UsageBillingBatchItem
	batchFn    func(context.Context, []UsageBillingBatchItem) ([]UsageBillingBatchOutcome, error)
}

func (r *batchUsageBillingRepoStub) ApplyBatchAndStageOutboxFinalizations(ctx context.Context, items []UsageBillingBatchItem) ([]UsageBillingBatchOutcome, error) {
	r.mu.Lock()
	r.batchCalls++
	r.batchItems = append(r.batchItems, append([]UsageBillingBatchItem(nil), items...))
	batchFn := r.batchFn
	r.mu.Unlock()
	if batchFn != nil {
		return batchFn(ctx, items)
	}
	outcomes := make([]UsageBillingBatchOutcome, len(items))
	for i := range items {
		outcomes[i].Result = &UsageBillingApplyResult{Applied: true}
	}
	return outcomes, nil
}

func (r *batchUsageBillingRepoStub) batchSnapshot() (int, [][]UsageBillingBatchItem) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.batchCalls, r.batchItems
}

func batchValidRecord(id int64, userID int64) BillingOutboxRecord {
	record := validBillingOutboxRecord(id)
	record.Command.Billing.UserID = userID
	return record
}

func TestBillingOutboxWorker_BatchAppliesAllRecordsInOneBatchCall(t *testing.T) {
	records := make([]BillingOutboxRecord, 16)
	for i := range records {
		records[i] = batchValidRecord(int64(i+1), int64(i+100))
	}
	repo := &billingOutboxRepoStub{records: records}
	billing := &batchUsageBillingRepoStub{}
	worker := NewBillingOutboxWorker(repo, billing)

	require.NoError(t, worker.processBatch(context.Background()))

	calls, items := billing.batchSnapshot()
	require.Equal(t, 1, calls, "the whole round must be applied in a single batch transaction")
	require.Len(t, items, 1)
	require.Len(t, items[0], 16)
	require.Equal(t, int64(1), items[0][0].Binding.OutboxID)
	require.Equal(t, int64(16), items[0][15].Binding.OutboxID)
	// staged 结果由 finalization 阶段收尾：本阶段不应 Ack，也不应 Retry。
	require.Empty(t, repo.acked)
	require.Empty(t, repo.retried)
	require.Equal(t, 0, billing.stageCalls, "per-record staged path must not be used when batching")
}

func TestBillingOutboxWorker_BatchAcksDeduplicatedRecords(t *testing.T) {
	records := make([]BillingOutboxRecord, 4)
	for i := range records {
		records[i] = batchValidRecord(int64(i+1), int64(i+100))
	}
	repo := &billingOutboxRepoStub{records: records}
	billing := &batchUsageBillingRepoStub{batchFn: func(context.Context, []UsageBillingBatchItem) ([]UsageBillingBatchOutcome, error) {
		return []UsageBillingBatchOutcome{
			{Result: &UsageBillingApplyResult{Applied: false}},
			{Result: &UsageBillingApplyResult{Applied: true}},
			{Result: &UsageBillingApplyResult{Applied: false}},
			{Result: &UsageBillingApplyResult{Applied: true}},
		}, nil
	}}
	worker := NewBillingOutboxWorker(repo, billing)

	require.NoError(t, worker.processBatch(context.Background()))
	require.ElementsMatch(t, []int64{1, 3}, repo.acked)
	require.Empty(t, repo.retried)
}

func TestBillingOutboxWorker_BatchIsolatesRecordFailureAndRetries(t *testing.T) {
	records := make([]BillingOutboxRecord, 3)
	for i := range records {
		records[i] = batchValidRecord(int64(i+1), int64(i+100))
	}
	repo := &billingOutboxRepoStub{records: records}
	billing := &batchUsageBillingRepoStub{batchFn: func(_ context.Context, items []UsageBillingBatchItem) ([]UsageBillingBatchOutcome, error) {
		outcomes := make([]UsageBillingBatchOutcome, len(items))
		for i := range items {
			if items[i].Binding.OutboxID == 2 {
				outcomes[i].Err = errors.New("transient infra failure")
				continue
			}
			outcomes[i].Result = &UsageBillingApplyResult{Applied: false}
		}
		return outcomes, nil
	}}
	worker := NewBillingOutboxWorker(repo, billing)

	require.NoError(t, worker.processBatch(context.Background()))

	// 只有失败的记录被重试；其余记录正常 Ack。
	require.Len(t, repo.retried, 1)
	require.Equal(t, int64(2), repo.retried[0].id)
	require.False(t, repo.retried[0].terminal)
	require.False(t, repo.retried[0].availableAt.IsZero(), "transient failure must be retried with backoff")
	require.ElementsMatch(t, []int64{1, 3}, repo.acked)
}

func TestBillingOutboxWorker_BatchMarksDeterministicFailureTerminal(t *testing.T) {
	records := []BillingOutboxRecord{batchValidRecord(1, 100)}
	repo := &billingOutboxRepoStub{records: records}
	billing := &batchUsageBillingRepoStub{batchFn: func(context.Context, []UsageBillingBatchItem) ([]UsageBillingBatchOutcome, error) {
		return []UsageBillingBatchOutcome{{Err: ErrAccountNotFound}}, nil
	}}
	worker := NewBillingOutboxWorker(repo, billing)

	require.NoError(t, worker.processBatch(context.Background()))
	require.Len(t, repo.retried, 1)
	require.True(t, repo.retried[0].terminal)
	require.True(t, repo.retried[0].availableAt.IsZero(), "terminal failures must be marked without backoff")
}

func TestBillingOutboxWorker_BatchRetriesAllRecordsOnBatchInfraFailure(t *testing.T) {
	records := make([]BillingOutboxRecord, 8)
	for i := range records {
		records[i] = batchValidRecord(int64(i+1), int64(i+100))
	}
	repo := &billingOutboxRepoStub{records: records}
	billing := &batchUsageBillingRepoStub{batchFn: func(context.Context, []UsageBillingBatchItem) ([]UsageBillingBatchOutcome, error) {
		return nil, errors.New("connection reset")
	}}
	worker := NewBillingOutboxWorker(repo, billing)

	err := worker.processBatch(context.Background())
	require.Error(t, err)
	require.Len(t, repo.retried, 8, "a failed batch transaction must retry every record in the round")
	require.Empty(t, repo.acked)
	for _, retry := range repo.retried {
		require.False(t, retry.terminal)
		require.False(t, retry.availableAt.IsZero())
	}
}

func TestBillingOutboxWorker_BatchSkipsInvalidRecordsButRetriesThemTerminally(t *testing.T) {
	valid := batchValidRecord(1, 100)
	invalid := validBillingOutboxRecord(2) // 缺少 attempt_id → Validate 失败
	invalid.Command.AttemptID = ""
	repo := &billingOutboxRepoStub{records: []BillingOutboxRecord{valid, invalid}}
	billing := &batchUsageBillingRepoStub{}
	worker := NewBillingOutboxWorker(repo, billing)

	require.NoError(t, worker.processBatch(context.Background()))

	calls, items := billing.batchSnapshot()
	require.Equal(t, 1, calls)
	require.Len(t, items[0], 1, "invalid record must not enter the batch transaction")
	require.Equal(t, int64(1), items[0][0].Binding.OutboxID)
	require.Len(t, repo.retried, 1)
	require.Equal(t, int64(2), repo.retried[0].id)
	require.True(t, repo.retried[0].terminal)
}

func TestBillingOutboxWorker_BatchSerializesSameUserShardAcrossRounds(t *testing.T) {
	// 同一用户的多条记录进入同一轮批量事务；跨轮并发调用仍按用户分片互斥。
	records := make([]BillingOutboxRecord, 4)
	for i := range records {
		records[i] = batchValidRecord(int64(i+1), 42)
	}
	repo := &billingOutboxRepoStub{records: records}

	var mu sync.Mutex
	active, maxActive := 0, 0
	var startOnce sync.Once
	started := make(chan struct{})
	release := make(chan struct{})
	billing := &batchUsageBillingRepoStub{batchFn: func(context.Context, []UsageBillingBatchItem) ([]UsageBillingBatchOutcome, error) {
		startOnce.Do(func() { close(started) })
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()
		<-release
		mu.Lock()
		active--
		mu.Unlock()
		outcomes := make([]UsageBillingBatchOutcome, 4)
		for i := range outcomes {
			outcomes[i].Result = &UsageBillingApplyResult{Applied: true}
		}
		return outcomes, nil
	}}
	worker := NewBillingOutboxWorker(repo, billing)

	errs := make(chan error, 2)
	go func() { errs <- worker.processBatch(context.Background()) }()
	<-started // 第一轮已进入批量事务
	go func() { errs <- worker.processBatch(context.Background()) }()
	time.Sleep(50 * time.Millisecond)
	close(release)

	for i := 0; i < 2; i++ {
		require.NoError(t, <-errs)
	}
	require.Equal(t, 1, maxActive, "same-user batch transactions must be serialized")
	require.Len(t, repo.acked, 0)
	require.Len(t, repo.retried, 0)
}

func TestBillingOutboxWorker_BatchFallbackPreservesPerRecordPath(t *testing.T) {
	// 仓库不支持批量接口时，回退到逐条事务（原有并发行为不变）。
	records := make([]BillingOutboxRecord, 8)
	for i := range records {
		records[i] = batchValidRecord(int64(i+1), int64(i+100))
	}
	repo := &billingOutboxRepoStub{records: records}
	billing := &stagedUsageBillingRepoStub{} // 未实现批量接口
	worker := NewBillingOutboxWorker(repo, billing)

	require.NoError(t, worker.processBatch(context.Background()))
	require.Equal(t, 8, billing.stageCalls)
}
