package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/google/uuid"
)

const (
	billingOutboxBatchSize    = 100
	billingOutboxPollInterval = 500 * time.Millisecond
	// Apply is bounded within the lease. Finalization may block indefinitely, so
	// its claim is renewed until the synchronous Finalize call returns.
	billingOutboxLease                     = 90 * time.Second
	billingOutboxConcurrency               = 16
	billingOutboxApplyTimeout              = 30 * time.Second
	billingOutboxAckRetryTimeout           = 2 * time.Second
	billingOutboxFinalizationRenewInterval = 20 * time.Second
	billingOutboxFinalizationDBTimeout     = 2 * time.Second
	billingOutboxMaxAttempts               = 10
	// 同一用户的扣费事务按分片互斥串行化，避免多个 apply worker 同时
	// UPDATE 同一 users 余额行形成行锁链（热点用户下可阻塞整轮 worker）。
	billingApplyUserShardCount = 256
	// 分片组并行：不同用户分片的批量事务并发执行的上限。同一用户必在同一
	// 分片（userID % 256），组内单个事务天然保持用户级串行；组间无共享锁、
	// 无嵌套获取，并发获取不同分片锁不可能死锁。测试可注入 1 退化为串行。
	billingOutboxApplyGroupParallelism = 8
)

// BillingOutboxHealth reports durable backlog and in-process replay state.
type BillingOutboxHealth struct {
	Running     bool          `json:"running"`
	Processed   uint64        `json:"processed"`
	Failures    uint64        `json:"failures"`
	Pending     int64         `json:"pending"`
	Processing  int64         `json:"processing"`
	Terminal    int64         `json:"terminal"`
	OldestLag   time.Duration `json:"oldest_lag"`
	LastError   string        `json:"last_error,omitempty"`
	StatsError  string        `json:"stats_error,omitempty"`
	MaxAttempts int           `json:"max_attempts"`
}

// BillingOutboxPostProcessor replays non-transactional enforcement updates
// only after a newly applied billing command commits successfully.
type BillingOutboxPostProcessor interface {
	Finalize(ctx context.Context, command *BillingOutboxCommand, result *UsageBillingApplyResult) error
}

type BillingOutboxWorker struct {
	repo                           BillingOutboxRepository
	billing                        UsageBillingRepository
	postProcessor                  BillingOutboxPostProcessor
	workerID                       string
	finalizationLease              time.Duration
	finalizationLeaseRenewInterval time.Duration
	finalizationDBTimeout          time.Duration
	ctx                            context.Context
	cancel                         context.CancelFunc
	wg                             sync.WaitGroup
	start                          sync.Once
	stop                           sync.Once
	running                        atomic.Bool
	processed                      atomic.Uint64
	failures                       atomic.Uint64
	lastError                      atomic.Value
	applyUserLocks                 [billingApplyUserShardCount]sync.Mutex
	applyGroupParallelism          int
}

func NewBillingOutboxWorker(repo BillingOutboxRepository, billing UsageBillingRepository, postProcessor ...BillingOutboxPostProcessor) *BillingOutboxWorker {
	ctx, cancel := context.WithCancel(context.Background())
	var processor BillingOutboxPostProcessor
	if len(postProcessor) > 0 {
		processor = postProcessor[0]
	}
	worker := &BillingOutboxWorker{
		repo: repo, billing: billing, postProcessor: processor, workerID: uuid.NewString(),
		finalizationLease: billingOutboxLease, finalizationLeaseRenewInterval: billingOutboxFinalizationRenewInterval,
		finalizationDBTimeout: billingOutboxFinalizationDBTimeout, ctx: ctx, cancel: cancel,
		applyGroupParallelism: billingOutboxApplyGroupParallelism,
	}
	worker.lastError.Store("")
	return worker
}

func (w *BillingOutboxWorker) Start() {
	if w == nil || w.repo == nil || w.billing == nil {
		return
	}
	w.start.Do(func() {
		w.running.Store(true)
		w.wg.Add(1)
		go w.run()
	})
}

func (w *BillingOutboxWorker) Stop() {
	if w == nil {
		return
	}
	w.stop.Do(func() {
		w.cancel()
		w.wg.Wait()
		w.running.Store(false)
	})
}

func (w *BillingOutboxWorker) run() {
	defer w.wg.Done()
	defer w.running.Store(false)
	ticker := time.NewTicker(billingOutboxPollInterval)
	defer ticker.Stop()
	for {
		if err := w.processBatch(w.ctx); err != nil && w.ctx.Err() == nil {
			w.recordFailure(err)
		}
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *BillingOutboxWorker) processBatch(ctx context.Context) error {
	if finalRepo, ok := w.repo.(BillingOutboxFinalizationRepository); ok {
		finalRecords, err := finalRepo.ClaimFinalization(ctx, w.workerID, billingOutboxConcurrency, w.finalizationLease)
		if err != nil {
			return fmt.Errorf("claim billing outbox finalizations: %w", err)
		}
		if err := w.processFinalizationBatch(ctx, finalRecords, finalRepo); err != nil {
			return err
		}
	}
	records, err := w.repo.Claim(ctx, w.workerID, billingOutboxConcurrency, billingOutboxLease)
	if err != nil {
		return fmt.Errorf("claim billing outbox commands: %w", err)
	}
	if err := w.processApplyBatch(ctx, records); err != nil {
		return err
	}
	return nil
}

func (w *BillingOutboxWorker) processApplyBatch(ctx context.Context, records []BillingOutboxRecord) error {
	if batchRepo, ok := w.billing.(UsageBillingBatchFinalizationRepository); ok {
		return w.processApplyBatchBatched(ctx, records, batchRepo)
	}
	// 旧仓库回退路径：逐条独立事务，16 并发，语义不变。
	semaphore := make(chan struct{}, billingOutboxConcurrency)
	var wg sync.WaitGroup
	for i := range records {
		select {
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		case semaphore <- struct{}{}:
		}
		wg.Add(1)
		go func(record BillingOutboxRecord) {
			defer wg.Done()
			defer func() { <-semaphore }()
			w.processRecord(ctx, record)
		}(records[i])
	}
	wg.Wait()
	return nil
}

// processApplyBatchBatched 把整轮记录按用户分片拆成多个批量事务（每分片
// 一个事务）：同一分片的多条记录共享一个事务，行锁与分片锁的作用域都只
// 覆盖本分片事务，不再横跨整轮——热点用户的行锁不会拖住整轮、其他分片的
// 轮次也不会被整轮持锁阻塞。不同分片的批量事务并发执行（组并行上限
// billingOutboxApplyGroupParallelism），逐条失败仍通过 savepoint 隔离（仓库内），
// 重试与去重 Ack 语义不变。整轮 applyCtx（30s 总时限）在所有组之间共享。
func (w *BillingOutboxWorker) processApplyBatchBatched(ctx context.Context, records []BillingOutboxRecord, batchRepo UsageBillingBatchFinalizationRepository) error {
	items := make([]UsageBillingBatchItem, 0, len(records))
	// 校验失败的记录不进批量事务，直接按 terminal 落库，与逐条路径一致。
	var invalid []struct {
		record BillingOutboxRecord
		err    error
	}
	for i := range records {
		command := records[i].Command
		command.Normalize()
		if err := command.Validate(); err != nil {
			err = fmt.Errorf("validate billing outbox command %d: %w", records[i].ID, err)
			w.recordFailure(err)
			invalid = append(invalid, struct {
				record BillingOutboxRecord
				err    error
			}{records[i], err})
			continue
		}
		items = append(items, UsageBillingBatchItem{
			Command: command.Billing,
			Binding: UsageBillingOutboxBinding{OutboxID: records[i].ID, WorkerID: w.workerID},
		})
	}
	for _, f := range invalid {
		w.persistFailure(f.record, f.err, true)
	}
	if len(items) == 0 {
		return nil
	}

	applyCtx, applyCancel := context.WithTimeout(ctx, billingOutboxApplyTimeout)
	defer applyCancel()
	// 分片组并行：不同分片的批量事务并发执行。同一用户必在同一分片，组内
	// 单个事务天然保持用户级串行；组间无共享锁、无嵌套获取，并发获取不同
	// 分片锁不可能死锁。错误聚合保留第一错误语义（并发下由互斥保护）。
	var firstErr error
	var errMu sync.Mutex
	parallelism := w.applyGroupParallelism
	if parallelism < 1 {
		parallelism = billingOutboxApplyGroupParallelism
	}
	sem := make(chan struct{}, parallelism)
	var wg sync.WaitGroup
	for _, group := range groupBatchItemsByShard(items) {
		sem <- struct{}{}
		wg.Add(1)
		go func(group billingShardGroup) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := w.applyBatchGroup(applyCtx, group, items, records, batchRepo); err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
			}
		}(group)
	}
	wg.Wait()
	return firstErr
}

// applyBatchGroup 在单个分片事务内应用一组批量记录：分片锁只覆盖本事务的
// 持锁区间（lockUserShard → ApplyBatchAndStageOutboxFinalizations → unlock），
// 逐条失败经 savepoint 隔离（仓库内），outcome 处理与串行路径一致。返回
// 组级错误（批量事务失败时非 nil；组内逐条失败只落库不返回）。
func (w *BillingOutboxWorker) applyBatchGroup(ctx context.Context, group billingShardGroup, items []UsageBillingBatchItem, records []BillingOutboxRecord, batchRepo UsageBillingBatchFinalizationRepository) error {
	groupItems := make([]UsageBillingBatchItem, len(group.indexes))
	for gi, idx := range group.indexes {
		groupItems[gi] = items[idx]
	}
	var unlock func()
	if group.shard >= 0 {
		unlock = w.lockUserShard(uint64(group.shard))
	}
	outcomes, err := batchRepo.ApplyBatchAndStageOutboxFinalizations(ctx, groupItems)
	if unlock != nil {
		unlock()
	}
	if err == nil && len(outcomes) != len(groupItems) {
		err = fmt.Errorf("batch billing apply returned %d outcomes for %d items", len(outcomes), len(groupItems))
	}
	if err != nil {
		// 本分片事务失败：只重试本分片记录；其余分片独立事务照常处理。
		for gi := range groupItems {
			idx := group.indexes[gi]
			record := records[itemRecordIndex(records, items[idx].Binding.OutboxID, idx)]
			w.recordFailure(err)
			w.persistFailure(record, err, false)
		}
		return err
	}
	for gi, outcome := range outcomes {
		idx := group.indexes[gi]
		record := records[itemRecordIndex(records, items[idx].Binding.OutboxID, idx)]
		if outcome.Err != nil {
			w.recordFailure(outcome.Err)
			w.persistFailure(record, outcome.Err, billingOutboxIsTerminalError(outcome.Err))
			continue
		}
		if outcome.Result == nil || outcome.Result.Applied {
			// 已转入 finalization 阶段：后续 Claim 负责 post-effects。
			continue
		}
		// 已存在的去重键：本行无需 post-effects，直接确认完成。
		ackCtx, ackCancel := context.WithTimeout(context.Background(), billingOutboxAckRetryTimeout)
		ackErr := w.repo.Ack(ackCtx, record.ID, w.workerID)
		ackCancel()
		if ackErr != nil {
			w.recordFailure(fmt.Errorf("ack billing outbox command %d: %w", record.ID, ackErr))
			continue
		}
		w.processed.Add(1)
		w.lastError.Store("")
	}
	return nil
}

// itemRecordIndex 在整轮记录里按 outbox ID 找回 items 对应的原始记录。
// items 与 records 顺序一一对应（不含校验失败项），按 ID 匹配更稳妥。
func itemRecordIndex(records []BillingOutboxRecord, outboxID int64, fallback int) int {
	for i := range records {
		if records[i].ID == outboxID {
			return i
		}
	}
	return fallback
}

// billingShardGroup 一轮批量记录按用户分片的子集：同一分片的记录共享
// 一个批量事务（savepoint 逐条失败隔离），分片间互不阻塞。
type billingShardGroup struct {
	shard   int   // -1 表示 userID<=0 的免锁记录组（不触碰 users 行）
	indexes []int // 整轮 items 里的下标
}

// groupBatchItemsByShard 按用户分片分组，返回按分片升序的组序列；
// userID<=0 的记录单独成组、无需分片锁。升序保证并发轮次间的加锁
// 顺序一致（与单条路径的 billingApplyUserShardCount 语义一致）。
func groupBatchItemsByShard(items []UsageBillingBatchItem) []billingShardGroup {
	if len(items) == 0 {
		return nil
	}
	byShard := make(map[int][]int, 8)
	for i := range items {
		shard := -1
		if userID := items[i].Command.UserID; userID > 0 {
			shard = int(uint64(userID) % billingApplyUserShardCount)
		}
		byShard[shard] = append(byShard[shard], i)
	}
	shards := make([]int, 0, len(byShard))
	for shard := range byShard {
		shards = append(shards, shard)
	}
	sort.Ints(shards)
	groups := make([]billingShardGroup, 0, len(shards))
	for _, shard := range shards {
		groups = append(groups, billingShardGroup{shard: shard, indexes: byShard[shard]})
	}
	return groups
}

// lockUserShard 锁定单个用户分片，返回解锁函数。逐分片加锁，同一时刻
// 只持有一把锁，无嵌套获取。
func (w *BillingOutboxWorker) lockUserShard(shard uint64) func() {
	if w == nil || shard >= billingApplyUserShardCount {
		return func() {}
	}
	w.applyUserLocks[shard].Lock()
	return w.applyUserLocks[shard].Unlock
}

func (w *BillingOutboxWorker) processFinalizationBatch(ctx context.Context, records []BillingOutboxRecord, repo BillingOutboxFinalizationRepository) error {
	semaphore := make(chan struct{}, billingOutboxConcurrency)
	var wg sync.WaitGroup
	for i := range records {
		select {
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		case semaphore <- struct{}{}:
		}
		wg.Add(1)
		go func(record BillingOutboxRecord) {
			defer wg.Done()
			defer func() { <-semaphore }()
			w.processFinalization(ctx, record, repo)
		}(records[i])
	}
	wg.Wait()
	return nil
}

func (w *BillingOutboxWorker) processRecord(parent context.Context, record BillingOutboxRecord) {
	command := record.Command
	command.Normalize()
	if err := command.Validate(); err != nil {
		w.recordFailure(err)
		w.persistFailure(record, err, true)
		return
	}

	applyCtx, applyCancel := context.WithTimeout(parent, billingOutboxApplyTimeout)
	var result *UsageBillingApplyResult
	var err error
	if staged, ok := w.billing.(UsageBillingFinalizationRepository); ok {
		unlock := w.lockUserApply(command.Billing.UserID)
		defer unlock()
		result, err = staged.ApplyAndStageOutboxFinalization(applyCtx, &command.Billing, UsageBillingOutboxBinding{OutboxID: record.ID, WorkerID: w.workerID})
		applyCancel()
		if err != nil {
			terminal := billingOutboxIsTerminalError(err)
			w.recordFailure(err)
			w.persistFailure(record, err, terminal)
			return
		}
		if result == nil || result.Applied {
			// Staging atomically records the Apply outcome and transfers ownership
			// to the finalization phase. A later claim performs post-effects.
			return
		}
		// A pre-existing deduplication key means this row did not own unfinished
		// finalization, so it can complete without post-effects.
		ackCtx, ackCancel := context.WithTimeout(context.Background(), billingOutboxAckRetryTimeout)
		ackErr := w.repo.Ack(ackCtx, record.ID, w.workerID)
		ackCancel()
		if ackErr != nil {
			w.recordFailure(fmt.Errorf("ack billing outbox command %d: %w", record.ID, ackErr))
			return
		}
		w.processed.Add(1)
		w.lastError.Store("")
		return
	}
	applyCancel()
	// Monetary Apply and the durable finalization handoff must be one repository
	// operation. A legacy repository cannot safely acknowledge this command: it
	// could commit money and lose the retryable post-effect route.
	err = ErrBillingOutboxFinalizationUnsupported
	w.recordFailure(err)
	w.persistFailure(record, err, false)
}

func (w *BillingOutboxWorker) processFinalization(parent context.Context, record BillingOutboxRecord, repo BillingOutboxFinalizationRepository) {
	finalizeCtx, cancelFinalize := context.WithCancel(parent)
	defer cancelFinalize()
	ownershipLost := func() bool { return false }

	command := record.Command
	command.Normalize()
	if err := command.Validate(); err != nil {
		w.recordFailure(err)
		w.persistFinalizationFailure(repo, record, err, true, ownershipLost)
		return
	}
	if record.ApplyResult == nil {
		err := errors.New("billing outbox finalization result is missing")
		w.recordFailure(err)
		w.persistFinalizationFailure(repo, record, err, true, ownershipLost)
		return
	}
	if w.postProcessor != nil {
		stopHeartbeat, heartbeatLost := w.renewFinalizationLease(cancelFinalize, record, repo)
		err := w.postProcessor.Finalize(finalizeCtx, &command, record.ApplyResult)
		stopHeartbeat()
		ownershipLost = heartbeatLost
		if err != nil {
			w.recordFailure(fmt.Errorf("finalize billing outbox command %d: %w", record.ID, err))
			w.persistFinalizationFailure(repo, record, err, billingOutboxIsTerminalError(err), ownershipLost)
			return
		}
	}
	if ownershipLost() || !w.renewFinalizationLeaseOnce(repo, record.ID) {
		w.recordFailure(fmt.Errorf("ack billing outbox finalization %d: %w", record.ID, ErrBillingOutboxClaimLost))
		return
	}
	ackCtx, cancel := context.WithTimeout(context.Background(), billingOutboxAckRetryTimeout)
	err := repo.AckFinalization(ackCtx, record.ID, w.workerID)
	cancel()
	if err != nil {
		w.recordFailure(fmt.Errorf("ack billing outbox finalization %d: %w", record.ID, err))
		return
	}
	w.processed.Add(1)
	w.lastError.Store("")
}

// lockUserApply 按用户分片串行化同一用户的扣费事务。固定 256 个分片互斥，
// 无动态 map 增长；分片冲突只会让不同用户的事务偶发排队，不影响正确性。
// userID <= 0 的指令不触碰 users 余额行，无需加锁。
// 注意：分片互斥为进程内机制，多副本下同一用户跨进程仍可能并发触碰
// users 行，残余竞争由行锁串行化（最坏并发数 = 副本数，改造前为
// 副本数 × worker 并发数）。
func (w *BillingOutboxWorker) lockUserApply(userID int64) func() {
	if w == nil || userID <= 0 {
		return func() {}
	}
	shard := &w.applyUserLocks[uint64(userID)%billingApplyUserShardCount]
	shard.Lock()
	return shard.Unlock
}

// renewFinalizationLease fences finalization state transitions while Finalize is
// blocked. It intentionally uses background-bounded contexts: a stopped parent
// must not interrupt the final ownership check needed to prevent a stale ACK.
func (w *BillingOutboxWorker) renewFinalizationLease(cancelFinalize context.CancelFunc, record BillingOutboxRecord, repo BillingOutboxFinalizationRepository) (func(), func() bool) {
	var lost atomic.Bool
	stop := make(chan struct{})
	done := make(chan struct{})
	interval := w.finalizationLeaseRenewInterval
	if interval <= 0 || interval >= w.finalizationLease {
		interval = w.finalizationLease / 3
	}
	if interval <= 0 {
		interval = time.Second
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if !w.renewFinalizationLeaseOnce(repo, record.ID) {
					lost.Store(true)
					cancelFinalize()
				}
			}
		}
	}()
	return func() { close(stop); <-done }, lost.Load
}

func (w *BillingOutboxWorker) renewFinalizationLeaseOnce(repo BillingOutboxFinalizationRepository, id int64) bool {
	timeout := w.finalizationDBTimeout
	if timeout <= 0 {
		timeout = billingOutboxFinalizationDBTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	err := repo.RenewFinalizationLease(ctx, id, w.workerID, w.finalizationLease)
	cancel()
	return err == nil
}

func (w *BillingOutboxWorker) persistFinalizationFailure(repo BillingOutboxFinalizationRepository, record BillingOutboxRecord, err error, terminal bool, ownershipLost func() bool) {
	if ownershipLost() || !w.renewFinalizationLeaseOnce(repo, record.ID) {
		w.recordFailure(fmt.Errorf("release billing outbox finalization %d: %w", record.ID, ErrBillingOutboxClaimLost))
		return
	}
	// Monetary effects already committed before this phase. A transient finalizer
	// failure must remain replayable regardless of the ordinary Apply retry limit.
	retryAt := time.Now().UTC().Add(billingOutboxRetryDelay(record.Attempts + 1))
	if terminal {
		retryAt = time.Time{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), billingOutboxAckRetryTimeout)
	retryErr := repo.RetryFinalization(ctx, record.ID, w.workerID, retryAt, boundedBillingOutboxWorkerError(err), terminal)
	cancel()
	if retryErr != nil {
		w.recordFailure(fmt.Errorf("release billing outbox finalization %d: %w", record.ID, retryErr))
	}
}

func (w *BillingOutboxWorker) persistFailure(record BillingOutboxRecord, err error, terminal bool) {
	// Retryable infrastructure failures remain recoverable indefinitely; the
	// capped backoff limits poll delay without discarding durable work.
	retryAt := time.Now().UTC().Add(billingOutboxRetryDelay(record.Attempts + 1))
	if terminal {
		retryAt = time.Time{}
	}
	retryCtx, retryCancel := context.WithTimeout(context.Background(), billingOutboxAckRetryTimeout)
	retryErr := w.repo.Retry(retryCtx, record.ID, w.workerID, retryAt, boundedBillingOutboxWorkerError(err), terminal)
	retryCancel()
	if retryErr != nil {
		w.recordFailure(fmt.Errorf("release billing outbox command %d: %w", record.ID, retryErr))
	}
}

func billingOutboxIsTerminalError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// Application errors with client-side status codes are deterministic poison
	// commands; retry only infrastructure/server failures.
	if code := infraerrors.Code(err); code >= 400 && code < 500 {
		return true
	}
	for _, terminalErr := range []error{
		ErrBillingOutboxAttemptIDRequired,
		ErrBillingOutboxAPIKeyRequired,
		ErrBillingOutboxFingerprintConflict,
		ErrUsageBillingRequestIDRequired,
		ErrUsageBillingRequestConflict,
		ErrUsageLogAccountRequired,
		ErrAccountNotFound,
		ErrUserNotFound,
		ErrAPIKeyNotFound,
		ErrSubscriptionNotFound,
	} {
		if errors.Is(err, terminalErr) {
			return true
		}
	}
	return false
}

func billingOutboxRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 9 {
		attempt = 9
	}
	base := time.Second * time.Duration(1<<(attempt-1))
	return time.Duration(float64(base) * (0.8 + rand.Float64()*0.4))
}

func boundedBillingOutboxWorkerError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Error())
	message = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 {
			return -1
		}
		return r
	}, message)
	if len(message) > BillingOutboxLastErrorLimit {
		return message[:BillingOutboxLastErrorLimit]
	}
	return message
}

func (w *BillingOutboxWorker) recordFailure(err error) {
	if err == nil {
		return
	}
	message := boundedBillingOutboxWorkerError(err)
	w.failures.Add(1)
	w.lastError.Store(message)
	slog.Warn("billing outbox processing failed", "error", message)
}

func (w *BillingOutboxWorker) Health(ctx context.Context) BillingOutboxHealth {
	health := BillingOutboxHealth{}
	if w == nil {
		return health
	}
	health.Running = w.running.Load()
	health.Processed = w.processed.Load()
	health.Failures = w.failures.Load()
	if value := w.lastError.Load(); value != nil {
		health.LastError, _ = value.(string)
	}
	if w.repo == nil {
		return health
	}
	stats, err := w.repo.Stats(ctx)
	if err != nil {
		health.StatsError = boundedBillingOutboxWorkerError(err)
		return health
	}
	health.Pending = stats.Pending
	health.Processing = stats.Processing
	health.Terminal = stats.Terminal
	health.MaxAttempts = stats.MaxAttempts
	if health.LastError == "" && stats.LastError != "" {
		health.LastError = boundedBillingOutboxWorkerError(errors.New(stats.LastError))
	}
	if stats.OldestCreatedAt != nil {
		health.OldestLag = time.Since(*stats.OldestCreatedAt)
		if health.OldestLag < 0 {
			health.OldestLag = 0
		}
	}
	return health
}
