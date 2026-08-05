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
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	// 单轮 apply Claim 批量：与并发数（billingOutboxConcurrency）解耦，
	// pending 与过期租约分支共用。Claim 拉满即认为队列可能仍有积压，
	// run 循环据此跳过 poll 间隔连续拉（背压感知）；空拉/部分拉取回落
	// 固定 poll 间隔，防止空转忙循环。
	billingOutboxClaimBatchSize = 500
	billingOutboxPollInterval   = 500 * time.Millisecond
	// Apply is bounded within the lease. Finalization may block indefinitely, so
	// its claim is renewed until the synchronous Finalize call returns.
	billingOutboxLease                     = 90 * time.Second
	billingOutboxConcurrency               = 16
	billingOutboxApplyTimeout              = 30 * time.Second
	billingOutboxAckRetryTimeout           = 2 * time.Second
	billingOutboxFinalizationRenewInterval = 20 * time.Second
	billingOutboxFinalizationDBTimeout     = 2 * time.Second
	billingOutboxMaxAttempts               = 10
	// 熔断阈值：连续永久错误达到该值打开熔断（成功或暂时性错误复位计数）。
	// 阈值取 5 是"系统性故障"与"零星坏记录"的分界：单轮 500 条里零星几条
	// 永久错误不足以开闸，连续 5 条（同一轮或跨轮）基本可判定系统级故障。
	billingOutboxCircuitBreakThreshold = 5
	// 熔断冷却：打开后至少等待该时长才放行一轮试探恢复（apply Claim），
	// 试探仍永久错误则重新冷却。测试注入更小值加速恢复断言。
	billingOutboxCircuitCoolDown = 5 * time.Minute
	// 确认路径（去重 Ack / 失败落库 Retry）连续失败的 break 阈值：Ack/Retry 被
	// 行锁、连接池等系统性阻塞（逐条 2s 超时）时，outcome 循环在第 5 条连续失败
	// 后中断，剩余记录仍被租约持有、未离开队列，租约过期后由 ClaimExpiredLeased
	// 补领重放（去重键幂等，重放安全）。无界时最坏尾巴 = 单轮 1000 条 × 2s ≈
	// 33 分钟，远超 90s 租约并阻塞 Stop 退出；5 条 × 2s = 10s，远小于租约。
	billingOutboxAckFailureBreakThreshold = 5
	// BillingApplyUserShardCount 是 billing apply 用户分片数（advisory 锁分片
	// 推导的模数）：同一用户的扣费事务按分片在 DB 层跨实例串行，apply 事务在
	// repository 侧通过 pg_advisory_xact_lock(类 ID, uint64(userID) %
	// BillingApplyUserShardCount) 取锁（同分片 = 同用户）。repository 包引用本
	// 导出常量作为唯一来源，两处分片推导编译期同源，漂移不可能静默发生。
	BillingApplyUserShardCount = 1024
	// 分片组并行：不同用户分片的批量事务并发执行的上限。同一用户必在同一
	// 分片（userID % 1024），组内单个事务天然保持用户级串行；组间无共享锁、
	// 无嵌套获取，并发获取不同分片锁不可能死锁。测试可注入 1 退化为串行。
	// 32 与 Claim 批量 500 的组合：单轮 ≈16 波、事务 ≤10ms 时约 3.1K/s/实例。
	billingOutboxApplyGroupParallelism = 32
	// 注入并行度的上限：测试/调参误注入超大值会同时开爆 goroutine 与 DB
	// 事务（每个分片组一个事务），clamp 到 128 保证最坏情况资源可控。
	billingOutboxApplyGroupParallelismMax = 128
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
	// 熔断状态（worker 实例内内存态）：连续永久错误 ≥ billingOutboxCircuitBreakThreshold
	// 时 CircuitOpen，apply Claim/处理被跳过直至冷却结束后的试探恢复成功。
	CircuitOpen       bool       `json:"circuit_open"`
	CircuitError      string     `json:"circuit_error,omitempty"`
	CircuitOpenedAt   *time.Time `json:"circuit_opened_at,omitempty"`
	PermanentFailures uint64     `json:"permanent_failures"`
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
	applyGroupParallelism          int
	// pollInterval 是 run 循环非积压轮之间的等待间隔（测试注入更小值
	// 加速节奏断言；<=0 时回退 billingOutboxPollInterval）。
	pollInterval time.Duration
	// 永久错误熔断状态机（worker 实例内内存态）与可注入冷却时长（测试注入
	// 更小值加速恢复断言；<=0 时回退 billingOutboxCircuitCoolDown）。
	circuit         billingOutboxCircuit
	circuitCooldown time.Duration
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
		pollInterval:          billingOutboxPollInterval,
		circuitCooldown:       billingOutboxCircuitCoolDown,
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
	pollInterval := w.pollInterval
	if pollInterval <= 0 {
		pollInterval = billingOutboxPollInterval
	}
	for {
		backlogged, err := w.processBatch(w.ctx)
		if err != nil && w.ctx.Err() == nil {
			w.recordFailure(err)
		}
		if w.ctx.Err() != nil {
			// Stop 取消在每轮 processBatch 返回后立即生效：连续拉循环不
			// 会因跳过 select 而无法退出。
			return
		}
		if backlogged {
			// 背压：本轮 Claim 拉满且实际消化了记录，跳过 poll 间隔立即
			// 下一轮，直到队列见底；空拉/部分拉取/失败自然回落下方等待。
			continue
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-w.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// processBatch 处理一轮 Claim（finalization + apply）。返回 backlogged
// 表示队列可能仍有积压、run 循环应跳过 poll 间隔连续拉取；语义见 run 注释。
func (w *BillingOutboxWorker) processBatch(ctx context.Context) (backlogged bool, err error) {
	if finalRepo, ok := w.repo.(BillingOutboxFinalizationRepository); ok {
		finalRecords, err := finalRepo.ClaimFinalization(ctx, w.workerID, billingOutboxConcurrency, w.finalizationLease)
		if err != nil {
			return false, fmt.Errorf("claim billing outbox finalizations: %w", err)
		}
		expiredFinalRecords, err := finalRepo.ClaimFinalizationExpiredLeased(ctx, w.workerID, billingOutboxConcurrency, w.finalizationLease)
		if err != nil {
			return false, fmt.Errorf("claim expired billing outbox finalizations: %w", err)
		}
		finalRecords = append(finalRecords, expiredFinalRecords...)
		if err := w.processFinalizationBatch(ctx, finalRecords, finalRepo); err != nil {
			return false, err
		}
	}
	// 熔断门：熔断打开且处于冷却窗口内时，apply Claim（pending 与过期租约
	// 分支）整体跳过——finalization 已在上方照常处理，已扣费记录的后置效应
	// 不停摆。跳过时返回不积压，run 循环回落 poll 间隔；冷却结束后下一轮
	// 自动成为试探恢复轮。
	if !w.beginApplyRound() {
		return false, nil
	}
	// pending 分支与过期租约分支分开 Claim（各自命中部分索引），合并后
	// 一次 apply：两批记录共享同一 processApplyBatch，apply 语义不变。
	// Claim 批量 = billingOutboxClaimBatchSize（500），与并发数解耦：积压
	// 时单轮最多消化 500 条，配合 run 循环的背压连续拉消除固定 ticker 上限。
	pendingRecords, err := w.repo.Claim(ctx, w.workerID, billingOutboxClaimBatchSize, billingOutboxLease)
	if err != nil {
		// Claim 自身的永久 PG 错误（如 42P01 表缺失）同样喂给熔断计数，
		// 否则缺失表的场景会以 poll 间隔无限空转锤击。
		w.observeApplyError(err)
		w.endApplyRound()
		return false, fmt.Errorf("claim billing outbox commands: %w", err)
	}
	expiredRecords, err := w.repo.ClaimExpiredLeased(ctx, w.workerID, billingOutboxClaimBatchSize, billingOutboxLease)
	if err != nil {
		w.observeApplyError(err)
		w.endApplyRound()
		return false, fmt.Errorf("claim expired billing outbox commands: %w", err)
	}
	records := append(pendingRecords, expiredRecords...)
	handled, err := w.processApplyBatch(ctx, records)
	w.endApplyRound()
	if err != nil {
		return false, err
	}
	// 背压语义：仅当 apply Claim 任一分支拉满且本轮实际消化了记录时报告
	// 积压（run 循环据此跳过 poll 间隔连续拉）。空拉、部分拉取、失败或
	// "拉满但 0 消化"（竞态/空转）一律返回 false，回落到固定 poll 等待，
	// 从设计上杜绝空转忙循环。
	full := len(pendingRecords) == billingOutboxClaimBatchSize || len(expiredRecords) == billingOutboxClaimBatchSize
	return full && handled > 0, nil
}

// processApplyBatch 应用一轮 Claim 的记录，返回实际被消化（转入
// finalization、Ack 成功、或失败落库）的记录数。ACK 失败（去重键已存在但
// 确认失败）的记录仍被租约持有、未离开队列，不计入——防止 run 循环仅凭
// "拉满"误判积压而空转。旧仓库回退路径逐条独立事务，并发保持 16，语义不变。
func (w *BillingOutboxWorker) processApplyBatch(ctx context.Context, records []BillingOutboxRecord) (int, error) {
	if batchRepo, ok := w.billing.(UsageBillingBatchFinalizationRepository); ok {
		return w.processApplyBatchBatched(ctx, records, batchRepo)
	}
	semaphore := make(chan struct{}, billingOutboxConcurrency)
	var wg sync.WaitGroup
	var handled int
	var handledMu sync.Mutex
	for i := range records {
		select {
		case <-ctx.Done():
			wg.Wait()
			return handled, ctx.Err()
		case semaphore <- struct{}{}:
		}
		wg.Add(1)
		go func(record BillingOutboxRecord) {
			defer wg.Done()
			defer func() { <-semaphore }()
			if w.processRecord(ctx, record) {
				handledMu.Lock()
				handled++
				handledMu.Unlock()
			}
		}(records[i])
	}
	wg.Wait()
	return handled, nil
}

// processApplyBatchBatched 把整轮记录按用户分片拆成多个批量事务（每分片
// 一个事务）：同一分片的多条记录共享一个事务，行锁与 advisory 锁的作用域
// 都只覆盖本分片事务，不再横跨整轮——热点用户的行锁不会拖住整轮、其他
// 分片的轮次也不会被整轮持锁阻塞。不同分片的批量事务并发执行（组并行上限
// billingOutboxApplyGroupParallelism），逐条失败仍通过 savepoint 隔离（仓库内），
// 重试与去重 Ack 语义不变。整轮 applyCtx（30s 总时限）在所有组之间共享。
func (w *BillingOutboxWorker) processApplyBatchBatched(ctx context.Context, records []BillingOutboxRecord, batchRepo UsageBillingBatchFinalizationRepository) (int, error) {
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
	// 校验失败记录逐个转 terminal 落库：Retry 落库连续失败时按阈值中断，剩余
	// 记录保留租约、过期后补领（校验确定性，补领重放仍落同一 terminal 错误）。
	failStreak := 0
	for _, f := range invalid {
		if ctx.Err() != nil {
			break
		}
		if !w.persistFailure(f.record, f.err, true) {
			failStreak++
			if failStreak >= billingOutboxAckFailureBreakThreshold {
				break
			}
		} else {
			failStreak = 0
		}
	}
	handled := len(invalid)
	if len(items) == 0 {
		return handled, nil
	}

	applyCtx, applyCancel := context.WithTimeout(ctx, billingOutboxApplyTimeout)
	defer applyCancel()
	// 分片组并行：不同分片的批量事务并发执行。同一用户必在同一分片，组内
	// 单个事务天然保持用户级串行；每个事务只取一把 advisory 锁（按组分片），
	// 组间无共享锁、无嵌套获取，并发取不同分片锁不可能死锁。注入值经
	// clampApplyGroupParallelism 收敛（<1 回退默认 32，超上限截断 128），防误
	// 注入导致 goroutine/DB 事务爆炸。错误聚合保留第一错误语义（并发下由
	// 互斥保护）。
	var firstErr error
	var errMu sync.Mutex
	handledMu := sync.Mutex{}
	parallelism := clampApplyGroupParallelism(w.applyGroupParallelism)
	sem := make(chan struct{}, parallelism)
	var wg sync.WaitGroup
	for _, group := range groupBatchItemsByShard(items) {
		sem <- struct{}{}
		wg.Add(1)
		go func(group billingShardGroup) {
			defer wg.Done()
			defer func() { <-sem }()
			groupHandled, err := w.applyBatchGroup(applyCtx, group, items, records, batchRepo)
			handledMu.Lock()
			handled += groupHandled
			handledMu.Unlock()
			if err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
			}
		}(group)
	}
	wg.Wait()
	return handled, firstErr
}

// clampApplyGroupParallelism 将注入的组并行度收敛到 [1, 上限]：<1 回退
// 默认 32，超过上限截断到 128。上限防测试/调参误注入超大值导致 goroutine
// 与 DB 事务同时爆炸；128 已远超默认 32 的吞吐需求。
func clampApplyGroupParallelism(v int) int {
	if v < 1 {
		v = billingOutboxApplyGroupParallelism
	}
	if v > billingOutboxApplyGroupParallelismMax {
		v = billingOutboxApplyGroupParallelismMax
	}
	return v
}

// applyBatchGroup 在单个分片事务内应用一组批量记录：同用户串行由 repository
// 侧事务级 pg_advisory_xact_lock 保证（跨实例生效，随事务提交/回滚自动
// 释放），worker 不再持有任何进程内锁。outcome 处理无嵌套加锁死锁风险；
// 逐条失败经 savepoint 隔离（仓库内），outcome 处理与串行路径一致。返回
// 组级错误（批量事务失败时非 nil；组内逐条失败只落库不返回）与本组实际
// 消化（转入 finalization / Ack 成功 / 失败落库）的记录数；Ack 失败的不
// 计入，防止背压语义把"拉满但零进展"误判为积压。
func (w *BillingOutboxWorker) applyBatchGroup(ctx context.Context, group billingShardGroup, items []UsageBillingBatchItem, records []BillingOutboxRecord, batchRepo UsageBillingBatchFinalizationRepository) (int, error) {
	groupItems := make([]UsageBillingBatchItem, len(group.indexes))
	for gi, idx := range group.indexes {
		groupItems[gi] = items[idx]
	}
	outcomes, err := batchRepo.ApplyBatchAndStageOutboxFinalizations(ctx, groupItems)
	if err == nil && len(outcomes) != len(groupItems) {
		err = fmt.Errorf("batch billing apply returned %d outcomes for %d items", len(outcomes), len(groupItems))
	}
	if err != nil {
		// 本分片事务失败：只重试本分片记录；其余分片独立事务照常处理。
		// Retry 落库连续失败按阈值中断，剩余记录保留租约、过期后补领。
		failStreak := 0
		for gi := range groupItems {
			if ctx.Err() != nil {
				break
			}
			idx := group.indexes[gi]
			record := records[itemRecordIndex(records, items[idx].Binding.OutboxID, idx)]
			w.recordFailure(err)
			if !w.persistFailure(record, err, billingOutboxFailureTerminal(record, err)) {
				failStreak++
				if failStreak >= billingOutboxAckFailureBreakThreshold {
					break
				}
			} else {
				failStreak = 0
			}
		}
		return len(groupItems), err
	}
	handled := 0
	// failStreak 统计确认路径连续失败（去重 Ack、逐条失败 Retry 落库），任一
	// 成功重置。连续达到 billingOutboxAckFailureBreakThreshold 说明确认路径
	// 系统性降级：break 出循环，剩余记录仍被租约持有、未离开队列，租约过期后
	// 由 ClaimExpiredLeased 补领重放（去重键幂等），避免整轮尾巴逐条 2s 超时
	// （最坏 1000 条 ≈ 33 分钟）拖垮轮时长、阻塞 Stop；ctx（applyCtx）到期或
	// Stop 取消时同样 break。
	failStreak := 0
	for gi, outcome := range outcomes {
		if ctx.Err() != nil {
			break
		}
		idx := group.indexes[gi]
		record := records[itemRecordIndex(records, items[idx].Binding.OutboxID, idx)]
		if outcome.Err != nil {
			w.recordFailure(outcome.Err)
			if !w.persistFailure(record, outcome.Err, billingOutboxFailureTerminal(record, outcome.Err)) {
				failStreak++
				if failStreak >= billingOutboxAckFailureBreakThreshold {
					break
				}
			} else {
				failStreak = 0
			}
			handled++
			continue
		}
		if outcome.Result == nil || outcome.Result.Applied {
			// 已转入 finalization 阶段：后续 Claim 负责 post-effects。
			failStreak = 0
			w.observeApplySuccess()
			handled++
			continue
		}
		// 已存在的去重键：本行无需 post-effects，直接确认完成。
		ackCtx, ackCancel := context.WithTimeout(context.Background(), billingOutboxAckRetryTimeout)
		ackErr := w.repo.Ack(ackCtx, record.ID, w.workerID)
		ackCancel()
		if ackErr != nil {
			w.recordFailure(fmt.Errorf("ack billing outbox command %d: %w", record.ID, ackErr))
			w.observeApplyError(ackErr)
			failStreak++
			if failStreak >= billingOutboxAckFailureBreakThreshold {
				break
			}
			continue
		}
		failStreak = 0
		w.processed.Add(1)
		w.lastError.Store("")
		w.observeApplySuccess()
		handled++
	}
	return handled, nil
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
// userID<=0 的记录单独成组、免 advisory 锁。分片公式与 repository 侧
// advisory 锁推导同源（uint64(userID) % BillingApplyUserShardCount），
// 每组的批量事务在仓库内按组取一次锁。
func groupBatchItemsByShard(items []UsageBillingBatchItem) []billingShardGroup {
	if len(items) == 0 {
		return nil
	}
	byShard := make(map[int][]int, 8)
	for i := range items {
		shard := -1
		if userID := items[i].Command.UserID; userID > 0 {
			shard = int(uint64(userID) % BillingApplyUserShardCount)
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

// processRecord 处理单条记录（旧仓库回退路径）。返回该记录是否被实际消化
// （转入 finalization / Ack 成功 / 失败落库）；仅 Ack 失败时返回 false。
func (w *BillingOutboxWorker) processRecord(parent context.Context, record BillingOutboxRecord) bool {
	command := record.Command
	command.Normalize()
	if err := command.Validate(); err != nil {
		w.recordFailure(err)
		w.persistFailure(record, err, true)
		return true
	}

	applyCtx, applyCancel := context.WithTimeout(parent, billingOutboxApplyTimeout)
	var result *UsageBillingApplyResult
	var err error
	if staged, ok := w.billing.(UsageBillingFinalizationRepository); ok {
		result, err = staged.ApplyAndStageOutboxFinalization(applyCtx, &command.Billing, UsageBillingOutboxBinding{OutboxID: record.ID, WorkerID: w.workerID})
		applyCancel()
		if err != nil {
			terminal := billingOutboxFailureTerminal(record, err)
			w.recordFailure(err)
			w.persistFailure(record, err, terminal)
			return true
		}
		if result == nil || result.Applied {
			// Staging atomically records the Apply outcome and transfers ownership
			// to the finalization phase. A later claim performs post-effects.
			w.observeApplySuccess()
			return true
		}
		// A pre-existing deduplication key means this row did not own unfinished
		// finalization, so it can complete without post-effects.
		ackCtx, ackCancel := context.WithTimeout(context.Background(), billingOutboxAckRetryTimeout)
		ackErr := w.repo.Ack(ackCtx, record.ID, w.workerID)
		ackCancel()
		if ackErr != nil {
			w.recordFailure(fmt.Errorf("ack billing outbox command %d: %w", record.ID, ackErr))
			w.observeApplyError(ackErr)
			return false
		}
		w.processed.Add(1)
		w.lastError.Store("")
		w.observeApplySuccess()
		return true
	}
	applyCancel()
	// Monetary Apply and the durable finalization handoff must be one repository
	// operation. A legacy repository cannot safely acknowledge this command: it
	// could commit money and lose the retryable post-effect route.
	err = ErrBillingOutboxFinalizationUnsupported
	w.recordFailure(err)
	w.persistFailure(record, err, false)
	return true
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
			// finalization 分类与 apply 一致（billingOutboxIsPermanentError），但
			// 不受 maxAttempts 约束：已扣费记录的后置效应必须保持可重放，暂时性
			// 失败永远 pending 重试，永久错误立即 terminal。
			w.persistFinalizationFailure(repo, record, err, billingOutboxIsPermanentError(err), ownershipLost)
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
	message := boundedBillingOutboxWorkerError(err)
	if terminal {
		message = billingOutboxErrorMessageWithSQLState(err, message)
	}
	ctx, cancel := context.WithTimeout(context.Background(), billingOutboxAckRetryTimeout)
	retryErr := repo.RetryFinalization(ctx, record.ID, w.workerID, retryAt, message, terminal)
	cancel()
	if retryErr != nil {
		w.recordFailure(fmt.Errorf("release billing outbox finalization %d: %w", record.ID, retryErr))
	}
}

// persistFailure 把失败记录落库到 Retry 队列，返回 Retry 是否落库成功。批量
// 路径的调用方按连续失败阈值（billingOutboxAckFailureBreakThreshold）中断，
// 剩余记录保留租约、租约过期后补领重放，避免逐条 2s 超时拖垮整轮；逐条
// （并发）路径忽略返回值，行为不变。
func (w *BillingOutboxWorker) persistFailure(record BillingOutboxRecord, err error, terminal bool) bool {
	// 所有 apply 路径的失败都经 persistFailure 落库：这里是熔断计数的唯一
	// 汇聚点（永久错误 +1 计数，暂时性错误复位连续计数）。
	w.observeApplyError(err)
	// Retryable infrastructure failures remain recoverable indefinitely; the
	// capped backoff limits poll delay without discarding durable work.
	retryAt := time.Now().UTC().Add(billingOutboxRetryDelay(record.Attempts + 1))
	if terminal {
		retryAt = time.Time{}
	}
	message := boundedBillingOutboxWorkerError(err)
	if terminal {
		message = billingOutboxErrorMessageWithSQLState(err, message)
	}
	retryCtx, retryCancel := context.WithTimeout(context.Background(), billingOutboxAckRetryTimeout)
	retryErr := w.repo.Retry(retryCtx, record.ID, w.workerID, retryAt, message, terminal)
	retryCancel()
	if retryErr != nil {
		w.recordFailure(fmt.Errorf("release billing outbox command %d: %w", record.ID, retryErr))
		return false
	}
	return true
}

// billingOutboxCircuit 是 worker 实例内的永久错误熔断状态机（内存态，
// 不跨实例共享）：连续永久错误 ≥ billingOutboxCircuitBreakThreshold 时
// CircuitOpen，apply Claim/处理被跳过，冷却 billingOutboxCircuitCoolDown
// 后放行一轮试探恢复（成功或无非永久错误 → 关闭；仍永久错误 → 重新冷却）。
// 状态机只作用于 apply Claim 与 apply 处理；finalization Claim/处理不受影响。
type billingOutboxCircuit struct {
	mu             sync.Mutex
	open           bool
	openedAt       time.Time
	errMessage     string
	total          uint64 // 累计永久错误（Health.PermanentFailures，单调不减）
	streak         uint64 // 连续永久错误（成功/暂时性错误复位为 0）
	probing        bool  // 冷却结束后的试探恢复轮进行中
	probePermanent bool  // 试探轮内已观察到永久错误（首个即重启冷却窗口）
}

// circuitCooldownDuration 返回可注入冷却时长；<=0（测试注入 0/负值或未注入）
// 回退默认 billingOutboxCircuitCoolDown。
func (w *BillingOutboxWorker) circuitCooldownDuration() time.Duration {
	if w.circuitCooldown <= 0 {
		return billingOutboxCircuitCoolDown
	}
	return w.circuitCooldown
}

// observeApplyError 在 apply 处理失败时调用：永久错误累加计数并可能打开/
// 重启熔断；暂时性错误复位连续计数（"连续永久错误"语义：成功或暂时性
// 错误都会打断连续性）。打开瞬间与试探轮首个永久错误都记 Error 级日志并
// 刷新 openedAt/errMessage。幂等：重复观察同一错误只是多计数一次，无状态
// 破坏；finalization 路径不调用本方法（熔断不作用于 finalization）。
func (w *BillingOutboxWorker) observeApplyError(err error) {
	if !billingOutboxIsPermanentError(err) {
		w.circuit.resetStreak()
		return
	}
	c := &w.circuit
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total++
	c.streak++
	if c.probing && !c.probePermanent {
		// 试探轮首个永久错误：重启冷却窗口（打开状态不变），试探轮的
		// probePermanent 标记使 endApplyRound 不会关闭电路。
		c.probePermanent = true
		c.openedAt = time.Now()
		c.errMessage = boundedBillingOutboxWorkerError(err)
		slog.Error("billing outbox apply circuit probe failed, cooldown restarted",
			"error", c.errMessage, "permanent_failures", c.total)
	}
	if !c.open && c.streak >= billingOutboxCircuitBreakThreshold {
		c.open = true
		c.openedAt = time.Now()
		c.errMessage = boundedBillingOutboxWorkerError(err)
		slog.Error("billing outbox apply circuit opened",
			"consecutive_permanent_errors", c.streak, "error", c.errMessage)
	}
}

// observeApplySuccess 在 apply 处理成功时调用：复位连续永久错误计数。
func (w *BillingOutboxWorker) observeApplySuccess() {
	w.circuit.resetStreak()
}

func (c *billingOutboxCircuit) resetStreak() {
	c.mu.Lock()
	c.streak = 0
	c.mu.Unlock()
}

// beginApplyRound 在 apply Claim 前调用：返回 false 表示熔断生效（打开且
// 处于冷却窗口内），调用方必须跳过整个 apply Claim；返回 true 时若熔断
// 已打开但冷却已结束，本轮即为试探恢复轮（probing 标记由 endApplyRound
// 消费）。finalization 阶段不经过本方法，始终照常。
func (w *BillingOutboxWorker) beginApplyRound() bool {
	c := &w.circuit
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.open {
		c.probing = false
		return true
	}
	if time.Since(c.openedAt) < w.circuitCooldownDuration() {
		return false
	}
	c.probing = true
	c.probePermanent = false
	return true
}

// endApplyRound 在 apply 阶段结束（含 Claim 失败路径）后调用：试探轮
// 未观察到任何永久错误 → 关闭熔断并复位连续计数；试探轮有永久错误 →
// 保持打开（冷却已在 observeApplyError 中重启）。非试探轮为 no-op。
func (w *BillingOutboxWorker) endApplyRound() {
	c := &w.circuit
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.probing {
		return
	}
	c.probing = false
	if c.probePermanent {
		return
	}
	c.open = false
	c.openedAt = time.Time{}
	c.errMessage = ""
	c.streak = 0
	slog.Info("billing outbox apply circuit closed after probe round")
}

// billingOutboxFailureTerminal 是 apply 路径的 terminal 决策（finalization
// 路径不经过本函数，不受 maxAttempts 限制）：仅永久错误可能 terminal——
// 本次新增的 PG 42xxx/22xxx 永久分类受 maxAttempts 约束（attempts ≥
// billingOutboxMaxAttempts 才 terminal，落库带 SQLSTATE 前缀）；既有
// terminal 列表（infra 4xx + 哨兵/校验错误）保持立即 terminal（确定性
// 毒药命令重试无意义）。暂时性错误（含 40001/40P01/55P03/57014/57P01-03/
// 08xxx/context 超时）即使 attempts 已达上限也绝不 terminal。
func billingOutboxFailureTerminal(record BillingOutboxRecord, err error) bool {
	if !billingOutboxIsPermanentError(err) {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr != nil {
		return record.Attempts >= billingOutboxMaxAttempts
	}
	return true
}

// billingOutboxErrorMessageWithSQLState 为 terminal 落库的 PG 错误附加
// SQLSTATE 前缀（[SQLSTATE 42P01] ...），对账与恢复定位可直接看到根因码；
// 结果仍受 last_error 上限（BillingOutboxLastErrorLimit）约束。
func billingOutboxErrorMessageWithSQLState(err error, message string) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr != nil && pgErr.Code != "" {
		message = fmt.Sprintf("[SQLSTATE %s] %s", pgErr.Code, message)
		if len(message) > BillingOutboxLastErrorLimit {
			message = message[:BillingOutboxLastErrorLimit]
		}
	}
	return message
}

// billingOutboxIsPermanentError 是 apply 与 finalization 两处共用的永久错误
// 分类（幂等：同一错误在两处分类一致）。永久 = PG 42xxx/22xxx（前缀匹配
// 全类）∪ 既有 terminal 列表（infra 4xx + 哨兵错误）；其余一律非永久。
// 显式暂时性 PG 码——40001/40P01（及全部 40 类）、55P03、57014、57P01-03、
// 08xxx（及全部 08 类）——与 42/22 前缀不相交，天然落入非永久，绝不 terminal。
func billingOutboxIsPermanentError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr != nil && len(pgErr.Code) >= 2 {
		return strings.HasPrefix(pgErr.Code, "42") || strings.HasPrefix(pgErr.Code, "22")
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
	w.circuit.mu.Lock()
	health.PermanentFailures = w.circuit.total
	health.CircuitError = w.circuit.errMessage
	if w.circuit.open {
		health.CircuitOpen = true
		openedAt := w.circuit.openedAt
		health.CircuitOpenedAt = &openedAt
	}
	w.circuit.mu.Unlock()
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
