package service

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

// pgError42P01 构造永久性 PG 错误（42P01 undefined_table）。
func pgError42P01() error {
	return &pgconn.PgError{Code: "42P01", Message: `relation "billing_attempt_outbox" does not exist`, Severity: "ERROR"}
}

// fivePermanentApplyRecords 构造 5 条同分片 apply 记录。
func fivePermanentApplyRecords() []BillingOutboxRecord {
	records := make([]BillingOutboxRecord, 5)
	for i := range records {
		records[i] = batchValidRecord(int64(i+1), 42)
	}
	return records
}

// TestBillingOutboxPgErrorIsPermanent 是 42xxx/22xxx 逐码审计的分类断言：
// 全部匹配码必须 permanent，显式暂时性码（40001/40P01/55P03/57014/57P01-03/
// 08xxx/全部 40 类）绝不可 permanent。前缀匹配设计保证未来 PG 新增同类码自动纳入。
func TestBillingOutboxPgErrorIsPermanent(t *testing.T) {
	permanentCodes := []string{
		// class 42 — Syntax Error or Access Rule Violation
		"42601", "42501", "42846", "42803", "42P20", "42P19", "42830", "42602", "42622",
		"42939", "42804", "42P18", "42P21", "42P22", "42809", "428C9", "42703", "42883",
		"42P01", "42P02", "42704", "42701", "42P03", "42P04", "42723", "42P05", "42P06",
		"42P07", "42712", "42710", "42702", "42725", "42P08", "42P09", "42P10", "42611",
		"42P11", "42P12", "42P13", "42P14", "42P15", "42P16", "42P17",
		// class 22 — Data Exception
		"2202E", "22021", "22008", "22012", "22005", "2200B", "22022", "22015", "2201E",
		"22014", "22016", "2201F", "2201G", "22018", "22007", "22019", "2200D", "22025",
		"22P06", "22010", "22023", "2201B", "2201W", "2201X", "2202H", "2202G", "22009",
		"2200C", "2200G", "22004", "22002", "22003", "22026", "22001", "22011", "22027",
		"22024", "2200F", "22P01", "22P02", "22P03", "22P04", "22P05", "2200L", "2200M",
		"2200N", "2200S", "2200T", "22030", "22031", "22032", "22033", "22034", "22035",
		"22036", "22037", "22038", "22039", "2203A", "2203B", "2203C", "2203D", "2203E",
		"2203F", "2203G",
	}
	for _, code := range permanentCodes {
		require.Truef(t, billingOutboxIsPermanentError(&pgconn.PgError{Code: code, Message: "boom"}),
			"PG code %s must be permanent", code)
	}
	transientCodes := []string{
		// class 40 — Transaction Rollback（全部）
		"40000", "40001", "40002", "40003", "40P01",
		// class 08 — Connection Exception（全部）
		"08000", "08001", "08003", "08004", "08006", "08007", "08P01",
		// class 55 / 57 — 显式暂时性码
		"55P03", "57014", "57P01", "57P02", "57P03",
		// 其他非永久类（完整性约束等）
		"23000", "23503", "23505", "25006", "2BP01",
	}
	for _, code := range transientCodes {
		require.Falsef(t, billingOutboxIsPermanentError(&pgconn.PgError{Code: code, Message: "boom"}),
			"PG code %s must be transient", code)
	}
	require.False(t, billingOutboxIsPermanentError(nil))
	require.False(t, billingOutboxIsPermanentError(context.DeadlineExceeded))
	require.False(t, billingOutboxIsPermanentError(context.Canceled))
	require.False(t, billingOutboxIsPermanentError(errors.New("plain infra failure")))
	require.True(t, billingOutboxIsPermanentError(fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: "42P01", Message: "x"})))
	// 既有 terminal 列表保留
	require.True(t, billingOutboxIsPermanentError(ErrSubscriptionNotFound))
	require.True(t, billingOutboxIsPermanentError(ErrAccountNotFound))
	require.True(t, billingOutboxIsPermanentError(ErrBillingOutboxAttemptIDRequired))
	require.False(t, billingOutboxIsPermanentError(ErrBillingOutboxFinalizationUnsupported))
	require.False(t, billingOutboxIsPermanentError(ErrBillingOutboxClaimLost))
}

// TestBillingOutboxWorker_CircuitIgnoresImmediatelyTerminalFailures：立即 terminal
// 的失败（4xx/哨兵，如 ErrUserNotFound——正常的 enqueue→apply 竞态换行）永不再
// 重试，喂给熔断计数保护不了任何东西，反而会被连打 5 条误开熔断、拖垮全部健康
// apply。熔断计数只统计"本轮确实会重试"的永久错误。
func TestBillingOutboxWorker_CircuitIgnoresImmediatelyTerminalFailures(t *testing.T) {
	repo := &billingOutboxRepoStub{records: fivePermanentApplyRecords()}
	billing := &batchUsageBillingRepoStub{batchFn: func(_ context.Context, items []UsageBillingBatchItem) ([]UsageBillingBatchOutcome, error) {
		outcomes := make([]UsageBillingBatchOutcome, len(items))
		for i := range outcomes {
			outcomes[i].Err = ErrUserNotFound
		}
		return outcomes, nil
	}}
	worker := NewBillingOutboxWorker(repo, billing)
	worker.circuitCooldown = time.Hour

	_, err := worker.processBatch(context.Background())
	require.NoError(t, err)
	require.Len(t, repo.retried, 5)
	for _, retry := range repo.retried {
		require.True(t, retry.terminal, "4xx/sentinel failures must stay immediately terminal")
	}
	health := worker.Health(context.Background())
	require.False(t, health.CircuitOpen, "immediately-terminal churn must not open the circuit")
	require.Zero(t, health.PermanentFailures, "immediately-terminal failures must not feed the permanent counter")
}

// TestBillingOutboxWorker_AckFailureDoesNotFeedCircuit：确认路径（去重 Ack）失败
// 不喂熔断计数——即使错误本身带永久分类（如 PG 42P01 落在 Ack UPDATE 上），
// Ack 失败由 billingOutboxAckFailureBreakThreshold 与租约补领兜底，永久分类在此
// 只会产生假信号，拖垮与确认路径无关的健康 apply。
func TestBillingOutboxWorker_AckFailureDoesNotFeedCircuit(t *testing.T) {
	repo := &billingOutboxRepoStub{records: fivePermanentApplyRecords(), ackErr: pgError42P01()}
	billing := &batchUsageBillingRepoStub{batchFn: func(_ context.Context, items []UsageBillingBatchItem) ([]UsageBillingBatchOutcome, error) {
		outcomes := make([]UsageBillingBatchOutcome, len(items))
		for i := range outcomes {
			outcomes[i].Result = &UsageBillingApplyResult{Applied: false} // 全部走去重 Ack 路径
		}
		return outcomes, nil
	}}
	worker := NewBillingOutboxWorker(repo, billing)
	worker.circuitCooldown = time.Hour

	_, err := worker.processBatch(context.Background())
	require.NoError(t, err)
	require.Len(t, repo.acked, billingOutboxAckFailureBreakThreshold, "ack failures must break after the consecutive threshold")
	require.Empty(t, repo.retried)
	health := worker.Health(context.Background())
	require.False(t, health.CircuitOpen, "ack failures must not feed the circuit")
	require.Zero(t, health.PermanentFailures)
}

// TestBillingOutboxWorker_CircuitOpensAfterConsecutivePermanentErrorsAndSkipsApplyClaims：
// 连续 5 次永久错误 → CircuitOpen；熔断期间 apply Claim（pending 与过期租约分支）
// 一律跳过，finalization Claim 照常。
func TestBillingOutboxWorker_CircuitOpensAfterConsecutivePermanentErrorsAndSkipsApplyClaims(t *testing.T) {
	repo := &billingOutboxRepoStub{records: fivePermanentApplyRecords()}
	billing := &batchUsageBillingRepoStub{batchFn: func(context.Context, []UsageBillingBatchItem) ([]UsageBillingBatchOutcome, error) {
		return nil, pgError42P01()
	}}
	worker := NewBillingOutboxWorker(repo, billing)
	worker.circuitCooldown = time.Hour

	_, err := worker.processBatch(context.Background())
	require.Error(t, err, "batch transaction failure must be propagated as before")
	health := worker.Health(context.Background())
	require.True(t, health.CircuitOpen, "5 consecutive permanent errors must open the circuit")
	require.Equal(t, uint64(5), health.PermanentFailures)
	require.Contains(t, health.CircuitError, "42P01")
	require.NotNil(t, health.CircuitOpenedAt)
	require.Equal(t, 1, repo.applyClaimCalls)

	// 熔断打开后的下一轮：apply Claim 全部跳过，finalization Claim 不受影响。
	_, err = worker.processBatch(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, repo.applyClaimCalls, "apply Claim must be skipped while the circuit is open")
	require.Equal(t, 1, repo.expiredLeaseClaimCalls, "expired-lease apply Claim must be skipped while the circuit is open")
	require.Equal(t, 2, repo.finalizationClaimCalls, "finalization Claim must keep running while the circuit is open")
	require.Equal(t, 2, repo.finalizationExpiredClaimCnt)
}

// TestBillingOutboxWorker_CircuitProbeRecoversAfterCooldown：冷却窗口结束后自动
// 试探恢复——试探轮成功（无非永久错误）→ 电路关闭、恢复 apply Claim。
func TestBillingOutboxWorker_CircuitProbeRecoversAfterCooldown(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	repo := &billingOutboxRepoStub{records: fivePermanentApplyRecords()}
	billing := &batchUsageBillingRepoStub{batchFn: func(_ context.Context, items []UsageBillingBatchItem) ([]UsageBillingBatchOutcome, error) {
		if fail.Load() {
			return nil, pgError42P01()
		}
		outcomes := make([]UsageBillingBatchOutcome, len(items))
		for i := range outcomes {
			outcomes[i].Result = &UsageBillingApplyResult{Applied: true}
		}
		return outcomes, nil
	}}
	worker := NewBillingOutboxWorker(repo, billing)
	worker.circuitCooldown = 25 * time.Millisecond

	_, err := worker.processBatch(context.Background())
	require.Error(t, err, "batch transaction failure must be propagated as before")
	require.True(t, worker.Health(context.Background()).CircuitOpen)

	fail.Store(false)
	time.Sleep(2 * worker.circuitCooldown)
	_, err = worker.processBatch(context.Background())
	require.NoError(t, err)
	health := worker.Health(context.Background())
	require.False(t, health.CircuitOpen, "probe round without permanent errors must close the circuit")
	require.Nil(t, health.CircuitOpenedAt)
	require.Empty(t, health.CircuitError)
	require.Equal(t, uint64(5), health.PermanentFailures)

	_, err = worker.processBatch(context.Background())
	require.NoError(t, err)
	require.Equal(t, 3, repo.applyClaimCalls, "apply Claim must resume after the circuit closes")
}

// TestBillingOutboxWorker_CircuitProbeFailureRestartsCooldown：试探轮仍永久错误
// → 重新进入冷却（新冷却窗口内跳过 apply Claim）；故障消除后下一个试探轮成功
// → 关闭。
func TestBillingOutboxWorker_CircuitProbeFailureRestartsCooldown(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	repo := &billingOutboxRepoStub{records: fivePermanentApplyRecords()}
	billing := &batchUsageBillingRepoStub{batchFn: func(_ context.Context, items []UsageBillingBatchItem) ([]UsageBillingBatchOutcome, error) {
		if fail.Load() {
			return nil, pgError42P01()
		}
		outcomes := make([]UsageBillingBatchOutcome, len(items))
		for i := range outcomes {
			outcomes[i].Result = &UsageBillingApplyResult{Applied: true}
		}
		return outcomes, nil
	}}
	worker := NewBillingOutboxWorker(repo, billing)

	_, err := worker.processBatch(context.Background())
	require.Error(t, err, "batch transaction failure must be propagated as before")
	require.True(t, worker.Health(context.Background()).CircuitOpen)

	// 冷却结束后试探轮仍永久错误 → 冷却重启（openedAt 刷新）。
	worker.circuitCooldown = 25 * time.Millisecond
	time.Sleep(2 * worker.circuitCooldown)
	_, err = worker.processBatch(context.Background())
	require.Error(t, err, "probe batch transaction failure must be propagated as before")
	require.True(t, worker.Health(context.Background()).CircuitOpen, "probe round with permanent errors must keep the circuit open")
	require.Equal(t, 2, repo.applyClaimCalls, "probe round must run the apply Claim")

	// 重启后的冷却窗口内：apply Claim 再次跳过。
	worker.circuitCooldown = time.Hour
	_, err = worker.processBatch(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, repo.applyClaimCalls, "apply Claim must be skipped inside the restarted cooldown window")

	// 故障消除 + 冷却结束 → 试探成功 → 关闭并恢复。
	fail.Store(false)
	worker.circuitCooldown = 25 * time.Millisecond
	time.Sleep(2 * worker.circuitCooldown)
	_, err = worker.processBatch(context.Background())
	require.NoError(t, err)
	require.False(t, worker.Health(context.Background()).CircuitOpen)
	require.Equal(t, 3, repo.applyClaimCalls)
}

// TestBillingOutboxWorker_TransientErrorsNeverOpenCircuitOrTerminate：暂时性 PG
// 错误（40001/40P01/55P03/57014/08P01）与 context 超时不熔断、不 terminal。
func TestBillingOutboxWorker_TransientErrorsNeverOpenCircuitOrTerminate(t *testing.T) {
	transientByID := map[int64]error{
		1: &pgconn.PgError{Code: "40001", Message: "could not serialize access due to concurrent update"},
		2: &pgconn.PgError{Code: "40P01", Message: "deadlock detected"},
		3: &pgconn.PgError{Code: "55P03", Message: "lock_not_available"},
		4: &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"},
		5: &pgconn.PgError{Code: "08P01", Message: "terminating connection due to protocol error"},
		6: context.DeadlineExceeded,
	}
	records := make([]BillingOutboxRecord, 0, len(transientByID))
	for id := range transientByID {
		records = append(records, batchValidRecord(id, 42))
	}
	repo := &billingOutboxRepoStub{records: records}
	billing := &stagedUsageBillingRepoStub{stageFn: func(_ context.Context, _ *UsageBillingCommand, binding UsageBillingOutboxBinding) (*UsageBillingApplyResult, error) {
		return nil, transientByID[binding.OutboxID]
	}}
	worker := NewBillingOutboxWorker(repo, billing)

	_, err := worker.processBatch(context.Background())
	require.NoError(t, err)

	require.Len(t, repo.retried, len(records))
	for _, retry := range repo.retried {
		require.False(t, retry.terminal, "transient PG errors must never be terminal")
		require.False(t, retry.availableAt.IsZero(), "transient errors must be retried with backoff")
	}
	health := worker.Health(context.Background())
	require.False(t, health.CircuitOpen, "transient errors must not open the circuit")
	require.Zero(t, health.PermanentFailures)
}

// TestBillingOutboxWorker_MixedOutcomeResetsPermanentStreak：成功或暂时性错误
// 复位连续永久错误计数——4 永久 + 1 成功/暂时 + 4 永久 不熔断（非 5 连）。
func TestBillingOutboxWorker_MixedOutcomeResetsPermanentStreak(t *testing.T) {
	permanentIDs := map[int64]bool{1: true, 2: true, 3: true, 4: true, 6: true, 7: true, 8: true, 9: true}
	records := make([]BillingOutboxRecord, 0, 9)
	for id := int64(1); id <= 9; id++ {
		records = append(records, batchValidRecord(id, 42))
	}
	repo := &billingOutboxRepoStub{records: records}
	billing := &batchUsageBillingRepoStub{batchFn: func(_ context.Context, items []UsageBillingBatchItem) ([]UsageBillingBatchOutcome, error) {
		outcomes := make([]UsageBillingBatchOutcome, len(items))
		for i := range items {
			if permanentIDs[items[i].Binding.OutboxID] {
				outcomes[i].Err = pgError42P01()
			} else {
				outcomes[i].Result = &UsageBillingApplyResult{Applied: true}
			}
		}
		return outcomes, nil
	}}
	worker := NewBillingOutboxWorker(repo, billing)

	_, err := worker.processBatch(context.Background())
	require.NoError(t, err)
	require.False(t, worker.Health(context.Background()).CircuitOpen,
		"a success between permanent errors must reset the consecutive streak")
	require.Equal(t, uint64(8), worker.Health(context.Background()).PermanentFailures)
}

// TestBillingOutboxWorker_PermanentPGErrorTerminalOnlyAtMaxAttempts：永久 PG
// 错误受 maxAttempts 约束——attempts ≥ billingOutboxMaxAttempts 才 terminal，
// 且 last_error 带 [SQLSTATE 42P01] 前缀；attempts 未到上限保持 pending 重试。
func TestBillingOutboxWorker_PermanentPGErrorTerminalOnlyAtMaxAttempts(t *testing.T) {
	repo := &billingOutboxRepoStub{}
	billing := &stagedUsageBillingRepoStub{stageFn: func(context.Context, *UsageBillingCommand, UsageBillingOutboxBinding) (*UsageBillingApplyResult, error) {
		return nil, pgError42P01()
	}}
	worker := NewBillingOutboxWorker(repo, billing)

	atLimit := validBillingOutboxRecord(90)
	atLimit.Attempts = billingOutboxMaxAttempts
	worker.processRecord(context.Background(), atLimit)
	require.Len(t, repo.retried, 1)
	require.True(t, repo.retried[0].terminal, "permanent PG error at max attempts must be terminal")
	require.True(t, repo.retried[0].availableAt.IsZero())
	require.Contains(t, repo.retried[0].lastError, "[SQLSTATE 42P01]", "terminal last_error must carry the SQLSTATE prefix")

	belowLimit := validBillingOutboxRecord(91)
	belowLimit.Attempts = billingOutboxMaxAttempts - 1
	worker.processRecord(context.Background(), belowLimit)
	require.Len(t, repo.retried, 2)
	require.False(t, repo.retried[1].terminal, "permanent PG error below max attempts must stay pending")
	require.False(t, repo.retried[1].availableAt.IsZero())
}

// TestBillingOutboxWorker_BatchPermanentPGErrorTerminalAtMaxAttempts：批量 outcome
// 路径同样受 maxAttempts 约束（终端落库带 SQLSTATE 前缀）。
func TestBillingOutboxWorker_BatchPermanentPGErrorTerminalAtMaxAttempts(t *testing.T) {
	record := batchValidRecord(92, 42)
	record.Attempts = billingOutboxMaxAttempts
	repo := &billingOutboxRepoStub{records: []BillingOutboxRecord{record}}
	billing := &batchUsageBillingRepoStub{batchFn: func(context.Context, []UsageBillingBatchItem) ([]UsageBillingBatchOutcome, error) {
		return []UsageBillingBatchOutcome{{Err: pgError42P01()}}, nil
	}}
	worker := NewBillingOutboxWorker(repo, billing)

	_, err := worker.processBatch(context.Background())
	require.NoError(t, err)
	require.Len(t, repo.retried, 1)
	require.True(t, repo.retried[0].terminal)
	require.Contains(t, repo.retried[0].lastError, "[SQLSTATE 42P01]")
}

// TestBillingOutboxWorker_TransientPgErrorAtMaxAttemptsStaysPending：暂时性 PG
// 错误在 attempts ≥ maxAttempts 时绝不 terminal（maxAttempts 只作用于永久路径）。
func TestBillingOutboxWorker_TransientPgErrorAtMaxAttemptsStaysPending(t *testing.T) {
	repo := &billingOutboxRepoStub{}
	billing := &stagedUsageBillingRepoStub{stageFn: func(context.Context, *UsageBillingCommand, UsageBillingOutboxBinding) (*UsageBillingApplyResult, error) {
		return nil, &pgconn.PgError{Code: "40001", Message: "serialization failure"}
	}}
	worker := NewBillingOutboxWorker(repo, billing)

	record := validBillingOutboxRecord(93)
	record.Attempts = billingOutboxMaxAttempts
	worker.processRecord(context.Background(), record)

	require.Len(t, repo.retried, 1)
	require.False(t, repo.retried[0].terminal, "transient error at max attempts must never be terminal")
	require.False(t, repo.retried[0].availableAt.IsZero(), "transient error must be retried with backoff")
	require.False(t, worker.Health(context.Background()).CircuitOpen)
}

// TestBillingOutboxWorker_CircuitOpenKeepsFinalizationRunning：熔断只跳过 apply
// Claim 与 apply 处理——finalization Claim、处理与 Ack 在熔断期间照常。
func TestBillingOutboxWorker_CircuitOpenKeepsFinalizationRunning(t *testing.T) {
	finalRecord := validBillingOutboxRecord(80)
	finalRecord.Status = "finalizing"
	finalRecord.ApplyResult = &UsageBillingApplyResult{Applied: true}
	repo := &billingOutboxRepoStub{
		records:             fivePermanentApplyRecords(),
		finalizationRecords: []BillingOutboxRecord{finalRecord},
	}
	billing := &batchUsageBillingRepoStub{batchFn: func(context.Context, []UsageBillingBatchItem) ([]UsageBillingBatchOutcome, error) {
		return nil, pgError42P01()
	}}
	postProcessor := &billingOutboxPostProcessorStub{}
	worker := NewBillingOutboxWorker(repo, billing, postProcessor)
	worker.circuitCooldown = time.Hour

	_, err := worker.processBatch(context.Background())
	require.Error(t, err, "batch transaction failure must be propagated as before")
	require.True(t, worker.Health(context.Background()).CircuitOpen)
	require.Equal(t, 1, postProcessor.calls, "finalization must run in the round that opens the circuit")
	require.Equal(t, []int64{80}, repo.finalizationAcked)

	_, err = worker.processBatch(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, postProcessor.calls, "finalization must keep running while the circuit is open")
	require.ElementsMatch(t, []int64{80, 80}, repo.finalizationAcked)
	require.Equal(t, 1, repo.applyClaimCalls, "apply Claim must stay skipped while the circuit is open")
}

// TestBillingOutboxWorker_ClaimPermanentErrorFeedsCircuit：apply Claim 自身返回
// 永久 PG 错误（如 42P01 表缺失）时同样计数熔断，冷却窗口内不再反复 Claim 空转。
func TestBillingOutboxWorker_ClaimPermanentErrorFeedsCircuit(t *testing.T) {
	repo := &billingOutboxRepoStub{}
	worker := NewBillingOutboxWorker(repo, &usageBillingRepoStub{})

	repo.mu.Lock()
	repo.claimErr = pgError42P01()
	repo.mu.Unlock()
	for range 5 {
		_, err := worker.processBatch(context.Background())
		require.Error(t, err)
	}
	require.True(t, worker.Health(context.Background()).CircuitOpen, "consecutive permanent Claim errors must open the circuit")

	// 冷却窗口内不再 Claim（不再对缺失的表空转锤击）。
	worker.circuitCooldown = time.Hour
	_, err := worker.processBatch(context.Background())
	require.NoError(t, err)
	require.Equal(t, 5, repo.applyClaimCalls, "apply Claim must be skipped while the circuit is open")

	// 故障消除 + 冷却结束 → 试探轮 Claim 成功 → 关闭并恢复。
	repo.mu.Lock()
	repo.claimErr = nil
	repo.mu.Unlock()
	worker.circuitCooldown = 25 * time.Millisecond
	time.Sleep(2 * worker.circuitCooldown)
	_, err = worker.processBatch(context.Background())
	require.NoError(t, err)
	require.False(t, worker.Health(context.Background()).CircuitOpen, "probe round with a healthy Claim must close the circuit")
	require.Equal(t, 6, repo.applyClaimCalls, "apply Claim must resume after the circuit closes")
}
