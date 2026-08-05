//go:build unit

package repository

import (
	"context"
	"database/sql"
	"strconv"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func expectSavepoint(mock sqlmock.Sqlmock, index int) {
	mock.ExpectExec("SAVEPOINT billing_apply_" + strconv.Itoa(index)).
		WillReturnResult(sqlmock.NewResult(0, 0))
}

func expectReleaseSavepoint(mock sqlmock.Sqlmock, index int) {
	mock.ExpectExec("RELEASE SAVEPOINT billing_apply_" + strconv.Itoa(index)).
		WillReturnResult(sqlmock.NewResult(0, 0))
}

func expectRollbackToSavepoint(mock sqlmock.Sqlmock, index int) {
	mock.ExpectExec("ROLLBACK TO SAVEPOINT billing_apply_" + strconv.Itoa(index)).
		WillReturnResult(sqlmock.NewResult(0, 0))
}

func newBatchItem(id int64, cmd *service.UsageBillingCommand) service.UsageBillingBatchItem {
	return service.UsageBillingBatchItem{
		Command: *cmd,
		Binding: service.UsageBillingOutboxBinding{OutboxID: id, WorkerID: "worker-1"},
	}
}

func expectOutboxFinalizationStage(mock sqlmock.Sqlmock, outboxID int64) {
	mock.ExpectExec(`(?s)UPDATE billing_attempt_outbox.*status = 'finalization_pending'.*leased_by = \$2.*status = 'processing'`).
		WithArgs(outboxID, "worker-1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func TestUsageBillingRepositoryApplyBatch_CommitsAllItemsInOneTransaction(t *testing.T) {
	db, mock := newUsageBillingSQLMock(t)
	repo := &usageBillingRepository{db: db}

	cmd1 := &service.UsageBillingCommand{RequestID: "batch-req-1", AccountID: 1, APIKeyID: 10, UserID: 20, BalanceCost: 1.25}
	cmd2 := &service.UsageBillingCommand{RequestID: "batch-req-2", AccountID: 1, APIKeyID: 10, UserID: 21, BalanceCost: 0.5}
	items := []service.UsageBillingBatchItem{newBatchItem(7, cmd1), newBatchItem(8, cmd2)}

	mock.ExpectBegin()
	expectBillingApplyLock(mock, cmd1.UserID)
	expectSavepoint(mock, 1)
	expectUsageBillingClaimInserted(mock, cmd1)
	expectGuardedBalanceDeduction(mock, cmd1.UserID, cmd1.BalanceCost, 8.75)
	expectOutboxFinalizationStage(mock, 7)
	expectReleaseSavepoint(mock, 1)
	expectSavepoint(mock, 2)
	expectUsageBillingClaimInserted(mock, cmd2)
	expectGuardedBalanceDeduction(mock, cmd2.UserID, cmd2.BalanceCost, 19.5)
	expectOutboxFinalizationStage(mock, 8)
	expectReleaseSavepoint(mock, 2)
	mock.ExpectCommit()

	outcomes, err := repo.ApplyBatchAndStageOutboxFinalizations(context.Background(), items)
	require.NoError(t, err)
	require.Len(t, outcomes, 2)
	require.Nil(t, outcomes[0].Err)
	require.True(t, outcomes[0].Result.Applied)
	require.NotNil(t, outcomes[0].Result.NewBalance)
	require.Nil(t, outcomes[1].Err)
	require.True(t, outcomes[1].Result.Applied)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUsageBillingRepositoryApplyBatch_IsolatesItemFailureWithSavepoint(t *testing.T) {
	db, mock := newUsageBillingSQLMock(t)
	repo := &usageBillingRepository{db: db}

	// 记录 1：余额不足且用户不存在 → ErrUserNotFound（回滚到 savepoint，不影响记录 2）
	cmd1 := &service.UsageBillingCommand{RequestID: "batch-fail-1", AccountID: 1, APIKeyID: 10, UserID: 20, BalanceCost: 1.25}
	// 记录 2：正常扣费
	cmd2 := &service.UsageBillingCommand{RequestID: "batch-ok-2", AccountID: 1, APIKeyID: 10, UserID: 21, BalanceCost: 0.5}
	items := []service.UsageBillingBatchItem{newBatchItem(7, cmd1), newBatchItem(8, cmd2)}

	mock.ExpectBegin()
	expectBillingApplyLock(mock, cmd1.UserID)
	expectSavepoint(mock, 1)
	expectUsageBillingClaimInserted(mock, cmd1)
	expectGuardedBalanceMiss(mock, cmd1.UserID, cmd1.BalanceCost)
	expectFallbackBalanceMiss(mock, cmd1.UserID, cmd1.BalanceCost)
	expectRollbackToSavepoint(mock, 1)
	expectReleaseSavepoint(mock, 1)
	expectSavepoint(mock, 2)
	expectUsageBillingClaimInserted(mock, cmd2)
	expectGuardedBalanceDeduction(mock, cmd2.UserID, cmd2.BalanceCost, 19.5)
	expectOutboxFinalizationStage(mock, 8)
	expectReleaseSavepoint(mock, 2)
	mock.ExpectCommit()

	outcomes, err := repo.ApplyBatchAndStageOutboxFinalizations(context.Background(), items)
	require.NoError(t, err)
	require.Len(t, outcomes, 2)
	require.ErrorIs(t, outcomes[0].Err, service.ErrUserNotFound)
	require.Nil(t, outcomes[0].Result)
	require.Nil(t, outcomes[1].Err)
	require.True(t, outcomes[1].Result.Applied)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUsageBillingRepositoryApplyBatch_SavepointIndexesBeyondNine(t *testing.T) {
	// 回归：savepoint 名必须用十进制索引（10+ 条记录时不能用 rune 算术）。
	db, mock := newUsageBillingSQLMock(t)
	repo := &usageBillingRepository{db: db}

	var items []service.UsageBillingBatchItem
	for i := 1; i <= 12; i++ {
		cmd := &service.UsageBillingCommand{
			RequestID:          "batch-many-" + strconv.Itoa(i),
			AccountID:          1,
			APIKeyID:           int64(i),
			RequestFingerprint: "fp-" + strconv.Itoa(i),
		}
		items = append(items, newBatchItem(int64(i), cmd))
	}

	mock.ExpectBegin()
	expectBillingApplyLock(mock, 0)
	for i := 1; i <= len(items); i++ {
		expectSavepoint(mock, i)
		expectUsageBillingClaimInserted(mock, &items[i-1].Command)
		expectOutboxFinalizationStage(mock, int64(i))
		expectReleaseSavepoint(mock, i)
	}
	mock.ExpectCommit()

	outcomes, err := repo.ApplyBatchAndStageOutboxFinalizations(context.Background(), items)
	require.NoError(t, err)
	require.Len(t, outcomes, len(items))
	for i := range outcomes {
		require.Nil(t, outcomes[i].Err)
		require.True(t, outcomes[i].Result.Applied)
	}
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUsageBillingRepositoryApplyBatch_InfraFailureRollsBackWholeBatch(t *testing.T) {
	db, mock := newUsageBillingSQLMock(t)
	repo := &usageBillingRepository{db: db}

	cmd1 := &service.UsageBillingCommand{RequestID: "batch-infra-1", AccountID: 1, APIKeyID: 10, UserID: 20, BalanceCost: 1.25}
	cmd2 := &service.UsageBillingCommand{RequestID: "batch-infra-2", AccountID: 1, APIKeyID: 10, UserID: 21, BalanceCost: 0.5}
	items := []service.UsageBillingBatchItem{newBatchItem(7, cmd1), newBatchItem(8, cmd2)}

	mock.ExpectBegin()
	expectBillingApplyLock(mock, cmd1.UserID)
	expectSavepoint(mock, 1)
	expectUsageBillingClaimInserted(mock, cmd1)
	expectGuardedBalanceDeduction(mock, cmd1.UserID, cmd1.BalanceCost, 8.75)
	expectOutboxFinalizationStage(mock, 7)
	// 连接断开：事务级失败，整个批次回滚，逐条 savepoint 无法挽救。
	mock.ExpectExec("RELEASE SAVEPOINT billing_apply_1").
		WillReturnError(sql.ErrConnDone)
	mock.ExpectRollback()

	outcomes, err := repo.ApplyBatchAndStageOutboxFinalizations(context.Background(), items)
	require.ErrorIs(t, err, sql.ErrConnDone)
	require.Nil(t, outcomes)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUsageBillingRepositoryApplyBatch_AdvisoryLockHeldAcrossSavepointRollback(t *testing.T) {
	// 事务级 advisory 锁语义：锁在批量事务开头取一次；失败记录经
	// ROLLBACK TO SAVEPOINT 回滚后锁仍然持有（不随 savepoint 释放），
	// 后续记录继续在同一事务内执行，锁随事务提交一起释放。sqlmock 顺序
	// 期望断言锁恰好出现一次且在首个 SAVEPOINT 之前、无任何 unlock。
	db, mock := newUsageBillingSQLMock(t)
	repo := &usageBillingRepository{db: db}

	cmd1 := &service.UsageBillingCommand{RequestID: "batch-lock-fail-1", AccountID: 1, APIKeyID: 10, UserID: 20, BalanceCost: 1.25}
	cmd2 := &service.UsageBillingCommand{RequestID: "batch-lock-ok-2", AccountID: 1, APIKeyID: 10, UserID: 20, BalanceCost: 0.5}
	items := []service.UsageBillingBatchItem{newBatchItem(7, cmd1), newBatchItem(8, cmd2)}

	mock.ExpectBegin()
	expectBillingApplyLock(mock, cmd1.UserID)
	expectSavepoint(mock, 1)
	expectUsageBillingClaimInserted(mock, cmd1)
	expectGuardedBalanceDeduction(mock, cmd1.UserID, cmd1.BalanceCost, 8.75)
	// 记录 1 在 staging 阶段失去租约：savepoint 回滚，锁不受影响。
	mock.ExpectExec(`(?s)UPDATE billing_attempt_outbox.*status = 'finalization_pending'.*leased_by = \$2.*status = 'processing'`).
		WithArgs(int64(7), "worker-1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	expectRollbackToSavepoint(mock, 1)
	expectReleaseSavepoint(mock, 1)
	expectSavepoint(mock, 2)
	expectUsageBillingClaimInserted(mock, cmd2)
	expectGuardedBalanceDeduction(mock, cmd2.UserID, cmd2.BalanceCost, 9.5)
	expectOutboxFinalizationStage(mock, 8)
	expectReleaseSavepoint(mock, 2)
	mock.ExpectCommit()

	outcomes, err := repo.ApplyBatchAndStageOutboxFinalizations(context.Background(), items)
	require.NoError(t, err)
	require.Len(t, outcomes, 2)
	require.ErrorIs(t, outcomes[0].Err, service.ErrBillingOutboxClaimLost)
	require.Nil(t, outcomes[0].Result)
	require.Nil(t, outcomes[1].Err)
	require.True(t, outcomes[1].Result.Applied)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUsageBillingRepositoryApplyBatch_LockShardFromFirstPositiveUserID(t *testing.T) {
	// worker 保证组内所有记录映射同一分片；仓库按组内第一条正 userID 推导
	// advisory 锁分片，即使组内混入 userID<=0 的记录也正确取锁。
	db, mock := newUsageBillingSQLMock(t)
	repo := &usageBillingRepository{db: db}

	cmd1 := &service.UsageBillingCommand{RequestID: "batch-mixed-1", AccountID: 1, APIKeyID: 10} // UserID 0
	cmd2 := &service.UsageBillingCommand{RequestID: "batch-mixed-2", AccountID: 1, APIKeyID: 10, UserID: 21, BalanceCost: 0.5}
	items := []service.UsageBillingBatchItem{newBatchItem(7, cmd1), newBatchItem(8, cmd2)}

	mock.ExpectBegin()
	expectBillingApplyLock(mock, cmd2.UserID)
	expectSavepoint(mock, 1)
	expectUsageBillingClaimInserted(mock, cmd1)
	expectOutboxFinalizationStage(mock, 7)
	expectReleaseSavepoint(mock, 1)
	expectSavepoint(mock, 2)
	expectUsageBillingClaimInserted(mock, cmd2)
	expectGuardedBalanceDeduction(mock, cmd2.UserID, cmd2.BalanceCost, 9.5)
	expectOutboxFinalizationStage(mock, 8)
	expectReleaseSavepoint(mock, 2)
	mock.ExpectCommit()

	outcomes, err := repo.ApplyBatchAndStageOutboxFinalizations(context.Background(), items)
	require.NoError(t, err)
	require.Len(t, outcomes, 2)
	require.Nil(t, outcomes[0].Err)
	require.Nil(t, outcomes[1].Err)
	require.True(t, outcomes[1].Result.Applied)
	require.NoError(t, mock.ExpectationsWereMet())
}
