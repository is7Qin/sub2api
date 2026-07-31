//go:build unit

package repository

import (
	"context"
	"database/sql"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func newUsageBillingSQLMock(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db, mock
}

func expectUsageBillingClaimInserted(mock sqlmock.Sqlmock, cmd *service.UsageBillingCommand) {
	cmd.Normalize()
	mock.ExpectQuery("INSERT INTO usage_billing_dedup").
		WithArgs(cmd.RequestID, cmd.APIKeyID, cmd.RequestFingerprint).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(1)))
	mock.ExpectQuery("FROM usage_billing_dedup_archive").
		WithArgs(cmd.RequestID, cmd.APIKeyID).
		WillReturnRows(sqlmock.NewRows([]string{"request_fingerprint"}))
}

func expectUsageBillingClaimDuplicate(mock sqlmock.Sqlmock, cmd *service.UsageBillingCommand) {
	cmd.Normalize()
	mock.ExpectQuery("INSERT INTO usage_billing_dedup").
		WithArgs(cmd.RequestID, cmd.APIKeyID, cmd.RequestFingerprint).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery("FROM usage_billing_dedup").
		WithArgs(cmd.RequestID, cmd.APIKeyID).
		WillReturnRows(sqlmock.NewRows([]string{"request_fingerprint"}).AddRow(cmd.RequestFingerprint))
}

func expectGuardedBalanceDeduction(mock sqlmock.Sqlmock, userID int64, amount float64, newBalance float64) {
	mock.ExpectQuery(`(?s)UPDATE users.*AND balance >= \$1.*RETURNING balance`).
		WithArgs(amount, userID).
		WillReturnRows(sqlmock.NewRows([]string{"balance"}).AddRow(newBalance))
}

func expectGuardedBalanceMiss(mock sqlmock.Sqlmock, userID int64, amount float64) {
	mock.ExpectQuery(`(?s)UPDATE users.*AND balance >= \$1.*RETURNING balance`).
		WithArgs(amount, userID).
		WillReturnRows(sqlmock.NewRows([]string{"balance"}))
}

func expectFallbackBalanceDeduction(mock sqlmock.Sqlmock, userID int64, amount float64, newBalance float64) {
	mock.ExpectQuery(`(?s)UPDATE users.*WHERE id = \$2 AND deleted_at IS NULL[[:space:]]+RETURNING balance`).
		WithArgs(amount, userID).
		WillReturnRows(sqlmock.NewRows([]string{"balance"}).AddRow(newBalance))
}

func expectFallbackBalanceMiss(mock sqlmock.Sqlmock, userID int64, amount float64) {
	mock.ExpectQuery(`(?s)UPDATE users.*WHERE id = \$2 AND deleted_at IS NULL[[:space:]]+RETURNING balance`).
		WithArgs(amount, userID).
		WillReturnRows(sqlmock.NewRows([]string{"balance"}))
}

func expectAPIKeyQuotaIncrement(mock sqlmock.Sqlmock, apiKeyID int64, amount float64, exhausted bool) {
	mock.ExpectQuery(`(?s)UPDATE api_keys.*quota_used = quota_used \+ \$1.*RETURNING quota > 0`).
		WithArgs(amount, apiKeyID, service.StatusAPIKeyActive, service.StatusAPIKeyQuotaExhausted).
		WillReturnRows(sqlmock.NewRows([]string{"exhausted"}).AddRow(exhausted))
}

func expectAPIKeyRateLimitIncrement(mock sqlmock.Sqlmock, apiKeyID int64, amount float64) {
	mock.ExpectExec("UPDATE api_keys SET").
		WithArgs(amount, apiKeyID).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func TestUsageBillingRepositoryApply_GuardedBalanceDeductionSucceeds(t *testing.T) {
	db, mock := newUsageBillingSQLMock(t)
	repo := &usageBillingRepository{db: db}
	cmd := &service.UsageBillingCommand{
		RequestID:           "req-sufficient-balance",
		AccountID:           1,
		APIKeyID:            10,
		UserID:              20,
		BalanceCost:         1.25,
		APIKeyQuotaCost:     1.25,
		APIKeyRateLimitCost: 1.25,
	}
	cmd.Normalize()

	mock.ExpectBegin()
	expectUsageBillingClaimInserted(mock, cmd)
	expectGuardedBalanceDeduction(mock, cmd.UserID, cmd.BalanceCost, 8.75)
	expectAPIKeyQuotaIncrement(mock, cmd.APIKeyID, cmd.APIKeyQuotaCost, false)
	expectAPIKeyRateLimitIncrement(mock, cmd.APIKeyID, cmd.APIKeyRateLimitCost)
	mock.ExpectCommit()

	result, err := repo.Apply(context.Background(), cmd)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Applied)
	require.NotNil(t, result.NewBalance)
	require.InDelta(t, 8.75, *result.NewBalance, 1e-9)
	require.False(t, result.BalanceOverdrafted)
	require.False(t, result.APIKeyQuotaExhausted)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUsageBillingRepositoryApply_GuardedBalanceMissFallbackRecordsDebt(t *testing.T) {
	db, mock := newUsageBillingSQLMock(t)
	repo := &usageBillingRepository{db: db}
	cmd := &service.UsageBillingCommand{
		RequestID:   "req-overdraft-balance",
		AccountID:   1,
		APIKeyID:    10,
		UserID:      20,
		BalanceCost: 1.25,
	}
	cmd.Normalize()

	mock.ExpectBegin()
	expectUsageBillingClaimInserted(mock, cmd)
	expectGuardedBalanceMiss(mock, cmd.UserID, cmd.BalanceCost)
	expectFallbackBalanceDeduction(mock, cmd.UserID, cmd.BalanceCost, -0.25)
	mock.ExpectCommit()

	result, err := repo.Apply(context.Background(), cmd)
	require.NoError(t, err)
	require.NotErrorIs(t, err, service.ErrInsufficientBalance)
	require.NotNil(t, result)
	require.True(t, result.Applied)
	require.NotNil(t, result.NewBalance)
	require.InDelta(t, -0.25, *result.NewBalance, 1e-9)
	require.True(t, result.BalanceOverdrafted)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUsageBillingRepositoryApply_MissingUserRollsBack(t *testing.T) {
	db, mock := newUsageBillingSQLMock(t)
	repo := &usageBillingRepository{db: db}
	cmd := &service.UsageBillingCommand{
		RequestID:        "req-missing-user",
		AccountID:        1,
		APIKeyID:         10,
		UserID:           404,
		SubscriptionID:   ptrInt64(30),
		SubscriptionCost: 0.50,
		BalanceCost:      1.25,
	}
	cmd.Normalize()

	mock.ExpectBegin()
	expectUsageBillingClaimInserted(mock, cmd)
	mock.ExpectExec("UPDATE user_subscriptions").
		WithArgs(cmd.SubscriptionCost, *cmd.SubscriptionID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectGuardedBalanceMiss(mock, cmd.UserID, cmd.BalanceCost)
	expectFallbackBalanceMiss(mock, cmd.UserID, cmd.BalanceCost)
	mock.ExpectRollback()

	result, err := repo.Apply(context.Background(), cmd)
	require.ErrorIs(t, err, service.ErrUserNotFound)
	require.Nil(t, result)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUsageBillingRepositoryApplyAndStageOutboxFinalization_StagesWithinBillingTransaction(t *testing.T) {
	db, mock := newUsageBillingSQLMock(t)
	repo := &usageBillingRepository{db: db}
	cmd := &service.UsageBillingCommand{
		RequestID: "req-stage-finalization", AccountID: 1, APIKeyID: 10, UserID: 20, BalanceCost: 1.25,
	}
	cmd.Normalize()

	mock.ExpectBegin()
	expectUsageBillingClaimInserted(mock, cmd)
	expectGuardedBalanceDeduction(mock, cmd.UserID, cmd.BalanceCost, 8.75)
	mock.ExpectExec(`(?s)UPDATE billing_attempt_outbox.*status = 'finalization_pending'.*leased_by = \$2.*status = 'processing'`).
		WithArgs(int64(7), "worker-1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	result, err := repo.ApplyAndStageOutboxFinalization(context.Background(), cmd, service.UsageBillingOutboxBinding{OutboxID: 7, WorkerID: "worker-1"})
	require.NoError(t, err)
	require.True(t, result.Applied)
	require.NotNil(t, result.NewBalance)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUsageBillingRepositoryApplyAndStageOutboxFinalization_RollsBackWhenStageLosesLease(t *testing.T) {
	db, mock := newUsageBillingSQLMock(t)
	repo := &usageBillingRepository{db: db}
	cmd := &service.UsageBillingCommand{
		RequestID: "req-stage-lost-lease", AccountID: 1, APIKeyID: 10, UserID: 20, BalanceCost: 1.25,
	}
	cmd.Normalize()

	mock.ExpectBegin()
	expectUsageBillingClaimInserted(mock, cmd)
	expectGuardedBalanceDeduction(mock, cmd.UserID, cmd.BalanceCost, 8.75)
	mock.ExpectExec(`(?s)UPDATE billing_attempt_outbox.*status = 'finalization_pending'.*leased_by = \$2.*status = 'processing'`).
		WithArgs(int64(8), "worker-1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	result, err := repo.ApplyAndStageOutboxFinalization(context.Background(), cmd, service.UsageBillingOutboxBinding{OutboxID: 8, WorkerID: "worker-1"})
	require.ErrorIs(t, err, service.ErrBillingOutboxClaimLost)
	require.Nil(t, result)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUsageBillingRepositoryApply_DuplicateRequestIDSkipsEffects(t *testing.T) {
	db, mock := newUsageBillingSQLMock(t)
	repo := &usageBillingRepository{db: db}
	cmd := &service.UsageBillingCommand{
		RequestID:           "req-duplicate",
		AccountID:           1,
		APIKeyID:            10,
		UserID:              20,
		BalanceCost:         1.25,
		APIKeyQuotaCost:     1.25,
		APIKeyRateLimitCost: 1.25,
	}
	cmd.Normalize()

	mock.ExpectBegin()
	expectUsageBillingClaimDuplicate(mock, cmd)
	mock.ExpectCommit()

	result, err := repo.Apply(context.Background(), cmd)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.Applied)
	require.Nil(t, result.NewBalance)
	require.False(t, result.BalanceOverdrafted)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUsageBillingRepositoryApply_FailureAfterDeductionRollsBack(t *testing.T) {
	db, mock := newUsageBillingSQLMock(t)
	repo := &usageBillingRepository{db: db}
	cmd := &service.UsageBillingCommand{
		RequestID:       "req-deduct-then-fail",
		AccountID:       1,
		APIKeyID:        10,
		UserID:          20,
		BalanceCost:     1.25,
		APIKeyQuotaCost: 1.25,
	}
	cmd.Normalize()

	mock.ExpectBegin()
	expectUsageBillingClaimInserted(mock, cmd)
	expectGuardedBalanceDeduction(mock, cmd.UserID, cmd.BalanceCost, 8.75)
	mock.ExpectQuery(`(?s)UPDATE api_keys.*quota_used = quota_used \+ \$1.*RETURNING quota > 0`).
		WithArgs(cmd.APIKeyQuotaCost, cmd.APIKeyID, service.StatusAPIKeyActive, service.StatusAPIKeyQuotaExhausted).
		WillReturnError(sql.ErrConnDone)
	mock.ExpectRollback()

	result, err := repo.Apply(context.Background(), cmd)
	require.ErrorIs(t, err, sql.ErrConnDone)
	require.Nil(t, result)
	require.NoError(t, mock.ExpectationsWereMet())
}

func ptrInt64(v int64) *int64 {
	return &v
}
