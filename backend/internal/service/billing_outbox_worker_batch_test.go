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

func outboxIDsOf(items []UsageBillingBatchItem) []int64 {
	ids := make([]int64, len(items))
	for i := range items {
		ids[i] = items[i].Binding.OutboxID
	}
	return ids
}

func TestBillingOutboxWorker_BatchAppliesSameShardRecordsInOneBatchCall(t *testing.T) {
	// 同一用户（同一分片）的多条记录合并进一个批量事务：每轮每分片一个事务。
	records := make([]BillingOutboxRecord, 16)
	for i := range records {
		records[i] = batchValidRecord(int64(i+1), 42)
	}
	repo := &billingOutboxRepoStub{records: records}
	billing := &batchUsageBillingRepoStub{}
	worker := NewBillingOutboxWorker(repo, billing)

	require.NoError(t, worker.processBatch(context.Background()))

	calls, items := billing.batchSnapshot()
	require.Equal(t, 1, calls, "same-shard records must share one per-shard batch transaction")
	require.Len(t, items, 1)
	require.Len(t, items[0], 16)
	require.Equal(t, int64(1), items[0][0].Binding.OutboxID)
	require.Equal(t, int64(16), items[0][15].Binding.OutboxID)
	// staged 结果由 finalization 阶段收尾：本阶段不应 Ack，也不应 Retry。
	require.Empty(t, repo.acked)
	require.Empty(t, repo.retried)
	require.Equal(t, 0, billing.stageCalls, "per-record staged path must not be used when batching")
}

func TestBillingOutboxWorker_BatchSplitsCrossShardRecordsIntoPerShardCalls(t *testing.T) {
	// 不同用户（不同分片）的记录拆分到各自分片事务；userID<=0 的记录不触碰
	// users 行，作为免锁组。组并行后跨分片调用顺序不再确定，只断言分片组成
	// （组内记录序仍与整轮顺序一致）。
	records := []BillingOutboxRecord{
		batchValidRecord(1, 42),
		batchValidRecord(2, 100),
		batchValidRecord(3, 42),
		batchValidRecord(4, 100),
		batchValidRecord(5, 7),
		validBillingOutboxRecord(6), // UserID 0
	}
	repo := &billingOutboxRepoStub{records: records}
	billing := &batchUsageBillingRepoStub{}
	worker := NewBillingOutboxWorker(repo, billing)

	require.NoError(t, worker.processBatch(context.Background()))

	calls, items := billing.batchSnapshot()
	require.Equal(t, 4, calls, "one per-shard batch transaction per shard")
	var groups [][]int64
	for _, group := range items {
		groups = append(groups, outboxIDsOf(group))
	}
	require.ElementsMatch(t, [][]int64{{6}, {5}, {1, 3}, {2, 4}}, groups, "same-shard records stay in one batch")
	require.Empty(t, repo.acked)
	require.Empty(t, repo.retried)
}

func TestBillingOutboxWorker_BatchAcksDeduplicatedRecords(t *testing.T) {
	records := make([]BillingOutboxRecord, 4)
	for i := range records {
		records[i] = batchValidRecord(int64(i+1), 42)
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
		records[i] = batchValidRecord(int64(i+1), 42)
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
		records[i] = batchValidRecord(int64(i+1), 42)
	}
	repo := &billingOutboxRepoStub{records: records}
	billing := &batchUsageBillingRepoStub{batchFn: func(context.Context, []UsageBillingBatchItem) ([]UsageBillingBatchOutcome, error) {
		return nil, errors.New("connection reset")
	}}
	worker := NewBillingOutboxWorker(repo, billing)

	err := worker.processBatch(context.Background())
	require.Error(t, err)
	calls, _ := billing.batchSnapshot()
	require.Equal(t, 1, calls, "one same-shard transaction must fail atomically")
	require.Len(t, repo.retried, 8, "a failed batch transaction must retry every record in the round")
	require.Empty(t, repo.acked)
	for _, retry := range repo.retried {
		require.False(t, retry.terminal)
		require.False(t, retry.availableAt.IsZero())
	}
}

func TestBillingOutboxWorker_BatchInfraFailureRetriesOnlyFailingShard(t *testing.T) {
	// 分片 5 的两条记录与分片 9 的一条记录：分片 9 的事务失败只重试该分片，
	// 分片 5 的独立事务照常提交（行锁与失败隔离都以分片为边界）。
	records := []BillingOutboxRecord{
		batchValidRecord(1, 5),
		batchValidRecord(2, 5),
		batchValidRecord(3, 9),
	}
	repo := &billingOutboxRepoStub{records: records}
	billing := &batchUsageBillingRepoStub{batchFn: func(_ context.Context, items []UsageBillingBatchItem) ([]UsageBillingBatchOutcome, error) {
		if len(items) > 0 && items[0].Binding.OutboxID == 3 {
			return nil, errors.New("connection reset")
		}
		outcomes := make([]UsageBillingBatchOutcome, len(items))
		for i := range outcomes {
			outcomes[i].Result = &UsageBillingApplyResult{Applied: true}
		}
		return outcomes, nil
	}}
	worker := NewBillingOutboxWorker(repo, billing)

	err := worker.processBatch(context.Background())
	require.Error(t, err)
	calls, _ := billing.batchSnapshot()
	require.Equal(t, 2, calls)
	require.Len(t, repo.retried, 1)
	require.Equal(t, int64(3), repo.retried[0].id)
	require.False(t, repo.retried[0].terminal)
	require.False(t, repo.retried[0].availableAt.IsZero())
	require.Empty(t, repo.acked)
}

func TestBillingOutboxWorker_BatchShardLockScopeIsPerShard(t *testing.T) {
	// 轮 1 在分片 5 的事务内阻塞时，只涉及分片 200 的轮 2 必须能直接完成：
	// 分片锁只在各自分片事务期间持有，而非整轮批量持有（修复前锁到整轮结束）。
	// 组并行后轮内分片处理顺序不再确定，按分片内容（outbox ID 1）定位轮 1
	// 的阻塞事务，使断言与调度顺序无关。
	round1 := []BillingOutboxRecord{batchValidRecord(1, 5), batchValidRecord(2, 200)}
	round2 := []BillingOutboxRecord{batchValidRecord(3, 200)}
	repo := &billingOutboxRepoStub{claimSeq: [][]BillingOutboxRecord{round1, round2}}

	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce, releaseOnce sync.Once
	billing := &batchUsageBillingRepoStub{batchFn: func(_ context.Context, items []UsageBillingBatchItem) ([]UsageBillingBatchOutcome, error) {
		if len(items) > 0 && items[0].Binding.OutboxID == 1 {
			// 轮 1 的分片 5 事务：持锁期间阻塞，验证该锁不拖住只涉及分片 200 的轮 2。
			startedOnce.Do(func() { close(started) })
			<-release
		}
		outcomes := make([]UsageBillingBatchOutcome, len(items))
		for i := range outcomes {
			outcomes[i].Result = &UsageBillingApplyResult{Applied: true}
		}
		return outcomes, nil
	}}
	worker := NewBillingOutboxWorker(repo, billing)

	round1Done := make(chan error, 1)
	go func() { round1Done <- worker.processBatch(context.Background()) }()
	<-started // 轮 1 已阻塞在分片 5 的事务中

	round2Done := make(chan error, 1)
	go func() { round2Done <- worker.processBatch(context.Background()) }()
	select {
	case err := <-round2Done:
		require.NoError(t, err, "round 2 (different shard) must not wait for round 1's in-flight transaction")
	case <-time.After(2 * time.Second):
		releaseOnce.Do(func() { close(release) })
		t.Fatal("round 2 blocked behind round 1: shard lock held across the whole batch")
	}
	releaseOnce.Do(func() { close(release) })
	require.NoError(t, <-round1Done)

	require.Empty(t, repo.acked)
	require.Empty(t, repo.retried)
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

func TestBillingOutboxApplyGroupsRunInParallel(t *testing.T) {
	// 不同用户分片的批量事务必须并发执行：100 条记录分布在 8 个分片，
	// ApplyBatch 的并发峰值至少为 2（修复前逐组串行，峰值恒为 1）。
	records := make([]BillingOutboxRecord, 100)
	for i := range records {
		records[i] = batchValidRecord(int64(i+1), int64(1+i%8))
	}
	repo := &billingOutboxRepoStub{records: records}

	var mu sync.Mutex
	active, maxActive := 0, 0
	started := make(chan struct{}, 100)
	release := make(chan struct{})
	var releaseOnce sync.Once
	billing := &batchUsageBillingRepoStub{batchFn: func(_ context.Context, items []UsageBillingBatchItem) ([]UsageBillingBatchOutcome, error) {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()
		started <- struct{}{}
		<-release
		mu.Lock()
		active--
		mu.Unlock()
		outcomes := make([]UsageBillingBatchOutcome, len(items))
		for i := range outcomes {
			outcomes[i].Result = &UsageBillingApplyResult{Applied: true}
		}
		return outcomes, nil
	}}
	worker := NewBillingOutboxWorker(repo, billing)

	finished := make(chan error, 1)
	go func() { finished <- worker.processBatch(context.Background()) }()

	// 等待至少 2 个分片事务同时进入批量调用；串行实现下第二个永远不会出现。
	startedCount := 0
	for startedCount < 2 {
		select {
		case <-started:
			startedCount++
		case <-time.After(2 * time.Second):
			releaseOnce.Do(func() { close(release) })
			t.Fatalf("distinct-shard batch transactions did not run in parallel: only %d of 2 concurrent calls observed", startedCount)
		}
	}
	mu.Lock()
	peak := maxActive
	mu.Unlock()
	require.GreaterOrEqual(t, peak, 2, "distinct-shard batch transactions must run concurrently")
	require.LessOrEqual(t, peak, billingOutboxApplyGroupParallelism, "batch concurrency must be capped by the group parallelism limit")

	close(release)
	require.NoError(t, <-finished)
	require.Empty(t, repo.acked)
	require.Empty(t, repo.retried)
}

func TestBillingOutboxApplyGroupsParallelismOneIsSerial(t *testing.T) {
	// 注入 parallelism=1 必须退化为逐组串行：并发峰值恒为 1，且组按分片
	// 升序依次处理（顺序回归断言，若注入失效则顺序随机、该断言不稳定）。
	records := make([]BillingOutboxRecord, 40)
	for i := range records {
		records[i] = batchValidRecord(int64(i+1), int64(1+i%4))
	}
	repo := &billingOutboxRepoStub{records: records}

	var mu sync.Mutex
	active, maxActive := 0, 0
	billing := &batchUsageBillingRepoStub{batchFn: func(_ context.Context, items []UsageBillingBatchItem) ([]UsageBillingBatchOutcome, error) {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()
		outcomes := make([]UsageBillingBatchOutcome, len(items))
		for i := range outcomes {
			outcomes[i].Result = &UsageBillingApplyResult{Applied: true}
		}
		mu.Lock()
		active--
		mu.Unlock()
		return outcomes, nil
	}}
	worker := NewBillingOutboxWorker(repo, billing)
	worker.applyGroupParallelism = 1

	require.NoError(t, worker.processBatch(context.Background()))

	mu.Lock()
	peak := maxActive
	mu.Unlock()
	require.Equal(t, 1, peak, "parallelism=1 must serialize batch transactions")

	// userID 1..4 → 分片 1..4：串行退化时按分片升序，每组 10 条、组内序保持。
	calls, items := billing.batchSnapshot()
	require.Equal(t, 4, calls)
	require.Equal(t, []int64{1, 5, 9, 13, 17, 21, 25, 29, 33, 37}, outboxIDsOf(items[0]))
	require.Equal(t, []int64{2, 6, 10, 14, 18, 22, 26, 30, 34, 38}, outboxIDsOf(items[1]))
	require.Equal(t, []int64{3, 7, 11, 15, 19, 23, 27, 31, 35, 39}, outboxIDsOf(items[2]))
	require.Equal(t, []int64{4, 8, 12, 16, 20, 24, 28, 32, 36, 40}, outboxIDsOf(items[3]))
	require.Empty(t, repo.acked)
	require.Empty(t, repo.retried)
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
