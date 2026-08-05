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

	_, err := worker.processBatch(context.Background())
	require.NoError(t, err)

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

	_, err := worker.processBatch(context.Background())
	require.NoError(t, err)

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

	_, err := worker.processBatch(context.Background())
	require.NoError(t, err)
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

	_, err := worker.processBatch(context.Background())
	require.NoError(t, err)

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

	_, err := worker.processBatch(context.Background())
	require.NoError(t, err)
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

	_, err := worker.processBatch(context.Background())
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

	_, err := worker.processBatch(context.Background())
	require.Error(t, err)
	calls, _ := billing.batchSnapshot()
	require.Equal(t, 2, calls)
	require.Len(t, repo.retried, 1)
	require.Equal(t, int64(3), repo.retried[0].id)
	require.False(t, repo.retried[0].terminal)
	require.False(t, repo.retried[0].availableAt.IsZero())
	require.Empty(t, repo.acked)
}

func TestBillingOutboxWorker_BatchAdvisoryLockScopeIsPerShardTransaction(t *testing.T) {
	// 进程内分片锁已移除，同一用户跨轮并发改由 DB 层 advisory xact lock 串行
	// （锁在事务内获取、随事务提交/回滚自动释放）。轮 1 在分片 5 的事务内阻塞
	// 时，只涉及分片 200 的轮 2 必须能直接完成：advisory 锁只在各自分片事务
	// 期间持有，而非整轮批量持有（修复前进程内分片锁持有到整轮结束）。
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
			// 轮 1 的分片 5 事务：模拟持锁期间阻塞，验证该锁不拖住只涉及分片 200 的轮 2。
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
	go func() { _, err := worker.processBatch(context.Background()); round1Done <- err }()
	<-started // 轮 1 已阻塞在分片 5 的事务中

	round2Done := make(chan error, 1)
	go func() { _, err := worker.processBatch(context.Background()); round2Done <- err }()
	select {
	case err := <-round2Done:
		require.NoError(t, err, "round 2 (different shard) must not wait for round 1's in-flight transaction")
	case <-time.After(2 * time.Second):
		releaseOnce.Do(func() { close(release) })
		t.Fatal("round 2 blocked behind round 1: advisory lock held across the whole batch")
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

	_, err := worker.processBatch(context.Background())
	require.NoError(t, err)

	calls, items := billing.batchSnapshot()
	require.Equal(t, 1, calls)
	require.Len(t, items[0], 1, "invalid record must not enter the batch transaction")
	require.Equal(t, int64(1), items[0][0].Binding.OutboxID)
	require.Len(t, repo.retried, 1)
	require.Equal(t, int64(2), repo.retried[0].id)
	require.True(t, repo.retried[0].terminal)
}

func TestBillingOutboxWorker_BatchAllowsConcurrentSameUserRounds(t *testing.T) {
	// 进程内分片锁已移除：同一用户的跨轮并发改由 DB 层 advisory xact lock
	// 串行（跨实例生效），worker 自身不再限制并发——两轮同用户批量事务
	// 可以同时进入仓库调用。串行性断言移到 repository 集成测试。
	records := make([]BillingOutboxRecord, 4)
	for i := range records {
		records[i] = batchValidRecord(int64(i+1), 42)
	}
	repo := &billingOutboxRepoStub{records: records}

	var mu sync.Mutex
	active, maxActive := 0, 0
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	billing := &batchUsageBillingRepoStub{batchFn: func(context.Context, []UsageBillingBatchItem) ([]UsageBillingBatchOutcome, error) {
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
		outcomes := make([]UsageBillingBatchOutcome, 4)
		for i := range outcomes {
			outcomes[i].Result = &UsageBillingApplyResult{Applied: true}
		}
		return outcomes, nil
	}}
	worker := NewBillingOutboxWorker(repo, billing)

	errs := make(chan error, 2)
	go func() { _, err := worker.processBatch(context.Background()); errs <- err }()
	select {
	case <-started: // 第一轮已进入批量事务
	case <-time.After(2 * time.Second):
		t.Fatal("round 1 did not reach the batch transaction")
	}
	go func() { _, err := worker.processBatch(context.Background()); errs <- err }()
	select {
	case <-started: // 第二轮也已进入：worker 不再串行同用户
	case <-time.After(2 * time.Second):
		t.Fatal("round 2 blocked behind round 1: worker still serializes same-user rounds in-process")
	}
	close(release)

	for i := 0; i < 2; i++ {
		require.NoError(t, <-errs)
	}
	mu.Lock()
	peak := maxActive
	mu.Unlock()
	require.Equal(t, 2, peak, "worker must not serialize same-user rounds in-process; DB advisory lock is the serializer")
	require.Len(t, repo.acked, 0)
	require.Len(t, repo.retried, 0)
}

func TestBillingOutboxApplyGroupsRunInParallel(t *testing.T) {
	// 不同用户分片的批量事务必须并发执行：100 条记录分布在 32 个分片，
	// ApplyBatch 的并发峰值必须超过旧默认并行度 8（Task C 把默认提到 32；
	// 若回退到旧默认 8，本断言失败）。
	records := make([]BillingOutboxRecord, 100)
	for i := range records {
		records[i] = batchValidRecord(int64(i+1), int64(1+i%32))
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
	go func() { _, err := worker.processBatch(context.Background()); finished <- err }()

	// 等满默认并行度个分片事务同时进入批量调用：32 个分片组在默认并行度下
	// 全部并发在途（若并行度退回旧默认 8，最多 8 个并发，此处超时暴露）。
	for i := 0; i < billingOutboxApplyGroupParallelism; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			releaseOnce.Do(func() { close(release) })
			t.Fatalf("only %d/%d concurrent batch transactions observed: default group parallelism below the expected value?", i, billingOutboxApplyGroupParallelism)
		}
	}
	// 先释放再断言：断言失败（如并行度退回旧默认 8）时事务 goroutine 能
	// 正常收尾，不会泄漏阻塞 goroutine 拖住测试二进制。
	close(release)
	require.NoError(t, <-finished)

	mu.Lock()
	peak := maxActive
	mu.Unlock()
	require.Greater(t, peak, 8, "default group parallelism must exceed the old default of 8")
	require.LessOrEqual(t, peak, billingOutboxApplyGroupParallelism, "batch concurrency must be capped by the group parallelism limit")
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

	_, err := worker.processBatch(context.Background())
	require.NoError(t, err)

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

func TestBillingOutboxApplyGroupParallelismInjectionClamped(t *testing.T) {
	// 注入的并行度超过上限时必须 clamp：200 个不同分片、注入 200，并发峰值
	// 必须恰好是上限（未 clamp 时 200 个事务全部并发、峰值 200）。上限防测试/
	// 调参误注入超大值导致 goroutine 与 DB 事务同时爆炸。
	records := make([]BillingOutboxRecord, 200)
	for i := range records {
		records[i] = batchValidRecord(int64(i+1), int64(i+1001)) // 200 个不同分片
	}
	repo := &billingOutboxRepoStub{records: records}

	var mu sync.Mutex
	active, maxActive := 0, 0
	// 缓冲区必须容纳全部 200 个分片组的 started 信号：release 后所有事务
	// 一次性涌出，缓冲不足会阻塞事务 goroutine、导致 wg.Wait 永不返回。
	started := make(chan struct{}, len(records))
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
	worker.applyGroupParallelism = 200

	finished := make(chan error, 1)
	go func() { _, err := worker.processBatch(context.Background()); finished <- err }()

	// clamp 生效时最多同时阻塞上限个事务：等满上限个即可确认未超限
	// （若上限被误调低，此处超时暴露）。
	for i := 0; i < billingOutboxApplyGroupParallelismMax; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			releaseOnce.Do(func() { close(release) })
			t.Fatalf("only %d/%d concurrent batch transactions observed: parallelism clamped below max?", i, billingOutboxApplyGroupParallelismMax)
		}
	}
	releaseOnce.Do(func() { close(release) })
	require.NoError(t, <-finished)

	mu.Lock()
	peak := maxActive
	mu.Unlock()
	require.Equal(t, billingOutboxApplyGroupParallelismMax, peak, "parallelism injected above max must be clamped to the cap")
	require.Empty(t, repo.acked)
	require.Empty(t, repo.retried)
}

func TestBillingOutboxApplyGroupParallelismBelowOneFallsBackToDefault(t *testing.T) {
	// 注入值 <1（0/负值）必须回退到默认并行度，而不是退化为 1 或死锁：
	// 40 个不同分片、注入 0，并发峰值必须等于当前默认常量（Task C 后为 32）。
	// 断言相对常量进行，随默认值自适应；回退失效（退化为串行/死锁）时超时
	// 暴露。上限/下限两条防御语义共同防止误注入搞垮资源预算。
	records := make([]BillingOutboxRecord, 40)
	for i := range records {
		records[i] = batchValidRecord(int64(i+1), int64(i+1001)) // 40 个不同分片
	}
	repo := &billingOutboxRepoStub{records: records}

	var mu sync.Mutex
	active, maxActive := 0, 0
	started := make(chan struct{}, 40)
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
	worker.applyGroupParallelism = 0

	finished := make(chan error, 1)
	go func() { _, err := worker.processBatch(context.Background()); finished <- err }()

	// 回退默认生效时最多同时阻塞默认并行度个事务：等满默认并行度即可确认
	// 未退化为串行（若回退逻辑失效，此处超时暴露）。
	for i := 0; i < billingOutboxApplyGroupParallelism; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			releaseOnce.Do(func() { close(release) })
			t.Fatalf("only %d/%d concurrent batch transactions observed: injection below 1 did not fall back to the default?", i, billingOutboxApplyGroupParallelism)
		}
	}
	releaseOnce.Do(func() { close(release) })
	require.NoError(t, <-finished)

	mu.Lock()
	peak := maxActive
	mu.Unlock()
	require.Equal(t, billingOutboxApplyGroupParallelism, peak, "parallelism injected below 1 must fall back to the default")
	require.Empty(t, repo.acked)
	require.Empty(t, repo.retried)
}

func TestBillingOutboxGroupShardingUsesAdvisoryShardFormula(t *testing.T) {
	// 分片公式必须与 repository 侧 advisory 锁推导一致：uint64(userID) % 1024。
	// 常量被改小或公式漂移时，userID 1024+7 会映射到错误分片，跨实例串行失效。
	items := []UsageBillingBatchItem{
		{Command: UsageBillingCommand{UserID: 7}},
		{Command: UsageBillingCommand{UserID: 1024 + 7}},
		{Command: UsageBillingCommand{UserID: 0}},
		{Command: UsageBillingCommand{UserID: -3}},
	}
	groups := groupBatchItemsByShard(items)
	require.Len(t, groups, 2)
	require.Equal(t, -1, groups[0].shard, "userID<=0 记录归入免锁组")
	require.Equal(t, 7, groups[1].shard, "uint64(userID) %% 1024 与 advisory 锁分片必须一致")
	require.ElementsMatch(t, []int{2, 3}, groups[0].indexes)
	require.ElementsMatch(t, []int{0, 1}, groups[1].indexes)
	require.Equal(t, 1024, BillingApplyUserShardCount, "分片常量必须保持 1024（service 导出唯一来源，repository 同源引用）")
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

	_, err := worker.processBatch(context.Background())
	require.NoError(t, err)
	require.Equal(t, 8, billing.stageCalls)
}

func TestBillingOutboxWorker_BatchAckFailureTailBreaksAndLeavesRecordsForReclaim(t *testing.T) {
	// Ack 路径系统性失败（如连接池/行锁阻塞逐条 2s 超时）时，outcome 循环必须
	// 在连续 billingOutboxAckFailureBreakThreshold 条失败后 break，而不是把整轮
	// 剩余记录逐条打完（批量 500+500 时最坏 1000 条 × 2s ≈ 33 分钟尾巴，远超
	// 90s 租约并阻塞 Stop）。break 后剩余记录仍被租约持有：未转入 finalization、
	// 未 Ack、未 Retry；租约过期后由 ClaimExpiredLeased 补领，重放 apply 再次
	// 命中去重键后正常 Ack 收尾——usage_billing_dedup 去重键幂等，重放不重复计费。
	records := make([]BillingOutboxRecord, 50)
	for i := range records {
		records[i] = batchValidRecord(int64(i+1), 42)
	}
	repo := &billingOutboxRepoStub{
		records:  records,
		claimSeq: [][]BillingOutboxRecord{records, nil}, // 轮 1 拉满，轮 2 pending 空拉
		ackErr:   errors.New("ack unavailable"),
	}
	billing := &batchUsageBillingRepoStub{batchFn: func(_ context.Context, items []UsageBillingBatchItem) ([]UsageBillingBatchOutcome, error) {
		outcomes := make([]UsageBillingBatchOutcome, len(items))
		for i := range outcomes {
			outcomes[i].Result = &UsageBillingApplyResult{Applied: false} // 全部走去重 Ack 路径
		}
		return outcomes, nil
	}}
	worker := NewBillingOutboxWorker(repo, billing)

	_, err := worker.processBatch(context.Background())
	require.NoError(t, err)
	// Ack 调用次数有界：连续失败 break，而非 50 条全部逐条打完。
	require.Len(t, repo.acked, billingOutboxAckFailureBreakThreshold,
		"ack failures must break the outcome loop after the consecutive-failure threshold")
	require.Empty(t, repo.retried)

	// 租约补领语义：本轮未 Ack 的记录留在租约内（processing），等租约过期后
	// 经 ClaimExpiredLeased 分支补领（pending 分支空拉不重复处理），重放再次
	// 命中去重键后正常 Ack——去重幂等，重放不重复计费。
	repo.mu.Lock()
	repo.ackErr = nil
	repo.expiredLeaseRecords = records[billingOutboxAckFailureBreakThreshold:]
	repo.mu.Unlock()
	_, err = worker.processBatch(context.Background())
	require.NoError(t, err)
	require.Len(t, repo.acked, len(records),
		"records left by the ack-failure break must drain via the expired-lease reclaim path")
	require.Empty(t, repo.retried)
}

func TestBillingOutboxWorker_BatchRetryFailureTailBreaks(t *testing.T) {
	// 批量事务整体失败后逐个落库重试：Retry 落库系统性失败时同样按连续阈值
	// break（旧行为把整组 50 条逐条 2s 超时打完），剩余记录保留租约、过期补领。
	records := make([]BillingOutboxRecord, 50)
	for i := range records {
		records[i] = batchValidRecord(int64(i+1), 42)
	}
	repo := &billingOutboxRepoStub{records: records, retryErr: errors.New("retry unavailable")}
	billing := &batchUsageBillingRepoStub{batchFn: func(context.Context, []UsageBillingBatchItem) ([]UsageBillingBatchOutcome, error) {
		return nil, errors.New("connection reset")
	}}
	worker := NewBillingOutboxWorker(repo, billing)

	_, err := worker.processBatch(context.Background())
	require.Error(t, err)
	require.Len(t, repo.retried, billingOutboxAckFailureBreakThreshold,
		"retry failures must break the per-shard retry loop after the consecutive-failure threshold")
	require.Empty(t, repo.acked)
}

func TestBillingOutboxWorker_BatchInvalidRecordRetryTailBreaks(t *testing.T) {
	// 校验失败记录逐个转 terminal 落库：Retry 连续失败按阈值中断，剩余校验
	// 失败记录保留租约、过期后补领（校验确定性，补领重放仍落同一 terminal
	// 错误），避免整轮 50 条逐条 2s 超时拖垮轮时长。
	records := make([]BillingOutboxRecord, 50)
	for i := range records {
		records[i] = validBillingOutboxRecord(int64(i + 1))
		records[i].Command.AttemptID = "" // 缺少 attempt_id → Validate 失败
	}
	repo := &billingOutboxRepoStub{records: records, retryErr: errors.New("retry unavailable")}
	billing := &batchUsageBillingRepoStub{}
	worker := NewBillingOutboxWorker(repo, billing)

	_, err := worker.processBatch(context.Background())
	require.NoError(t, err)
	require.Len(t, repo.retried, billingOutboxAckFailureBreakThreshold,
		"terminal retries must break after the consecutive-failure threshold")
	for _, retry := range repo.retried {
		require.True(t, retry.terminal)
	}
	require.Empty(t, repo.acked)
}
