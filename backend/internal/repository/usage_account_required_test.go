//go:build unit

package repository

import (
	"context"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestUsageLogRepositoryCreateUsageAccountRequired(t *testing.T) {
	for _, accountID := range []*int64{nil, usageLogAccountIDPointer(0), usageLogAccountIDPointer(-1)} {
		t.Run(usageAccountIDPointerTestName(accountID), func(t *testing.T) {
			db, mock := newSQLMock(t)
			repo := &usageLogRepository{sql: db}

			inserted, err := repo.Create(context.Background(), &service.UsageLog{AccountID: accountID})

			require.False(t, inserted)
			require.ErrorIs(t, err, service.ErrUsageLogAccountRequired)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestUsageLogRepositoryCreateBestEffortInvalidAccountNeverQueues(t *testing.T) {
	for _, accountID := range []*int64{nil, usageLogAccountIDPointer(0), usageLogAccountIDPointer(-1)} {
		t.Run(usageAccountIDPointerTestName(accountID), func(t *testing.T) {
			repo := &usageLogRepository{
				bestEffortBatchCh: make(chan usageLogBestEffortRequest, 1),
			}

			err := repo.CreateBestEffort(context.Background(), &service.UsageLog{AccountID: accountID})

			require.ErrorIs(t, err, service.ErrUsageLogAccountRequired)
			require.Empty(t, repo.bestEffortBatchCh)
		})
	}
}

func TestPrepareUsageLogInsertUsageAccountRequired(t *testing.T) {
	for _, accountID := range []*int64{nil, usageLogAccountIDPointer(0), usageLogAccountIDPointer(-1)} {
		t.Run(usageAccountIDPointerTestName(accountID), func(t *testing.T) {
			prepared, err := prepareUsageLogInsert(&service.UsageLog{AccountID: accountID})

			require.ErrorIs(t, err, service.ErrUsageLogAccountRequired)
			require.Empty(t, prepared.args)
		})
	}

	prepared, err := prepareUsageLogInsert(&service.UsageLog{AccountID: usageLogAccountIDPointer(42)})
	require.NoError(t, err)
	require.Equal(t, int64(42), prepared.args[2])
}

func TestExecUsageLogInsertNoResultUsageAccountRequired(t *testing.T) {
	db, mock := newSQLMock(t)

	err := execUsageLogInsertNoResult(context.Background(), db, usageLogInsertPrepared{})

	require.ErrorIs(t, err, service.ErrUsageLogAccountRequired)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBuildUsageLogBatchInsertQueryUsageAccountRequired(t *testing.T) {
	prepared, err := prepareUsageLogInsert(&service.UsageLog{
		AccountID: usageLogAccountIDPointer(42),
		CreatedAt: time.Now(),
	})
	require.NoError(t, err)
	prepared.args[2] = int64(0)

	query, args, err := buildUsageLogBatchInsertQuery([]string{"key"}, map[string]usageLogInsertPrepared{"key": prepared})

	require.ErrorIs(t, err, service.ErrUsageLogAccountRequired)
	require.Empty(t, query)
	require.Nil(t, args)
}

func TestBuildUsageLogBestEffortInsertQueryUsageAccountRequired(t *testing.T) {
	prepared, err := prepareUsageLogInsert(&service.UsageLog{
		AccountID: usageLogAccountIDPointer(42),
		CreatedAt: time.Now(),
	})
	require.NoError(t, err)
	prepared.args[2] = int64(-1)

	query, args, err := buildUsageLogBestEffortInsertQuery([]usageLogInsertPrepared{prepared})

	require.ErrorIs(t, err, service.ErrUsageLogAccountRequired)
	require.Empty(t, query)
	require.Nil(t, args)
}

func TestUsageLogRepositoryFlushCreateBatchInvalidAccountNeverExecutes(t *testing.T) {
	db, mock := newUsageBillingSQLMock(t)
	prepared := usageLogInsertPrepared{accountID: -1, args: []any{int64(1), int64(2), int64(-1)}, requestID: "req-invalid-batch"}
	resultCh := make(chan usageLogCreateResult, 1)

	(&usageLogRepository{}).flushCreateBatch(db, []usageLogCreateRequest{{
		log:      &service.UsageLog{APIKeyID: 2, AccountID: usageLogAccountIDPointer(-1), RequestID: "req-invalid-batch"},
		prepared: prepared,
		resultCh: resultCh,
	}})

	result := <-resultCh
	require.ErrorIs(t, result.err, service.ErrUsageLogAccountRequired)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUsageLogRepositoryFlushBestEffortBatchInvalidAccountNeverExecutes(t *testing.T) {
	db, mock := newUsageBillingSQLMock(t)
	resultCh := make(chan error, 1)

	(&usageLogRepository{}).flushBestEffortBatch(db, []usageLogBestEffortRequest{{
		prepared: usageLogInsertPrepared{accountID: 0, args: []any{int64(1), int64(2), int64(0)}},
		resultCh: resultCh,
	}})

	require.ErrorIs(t, <-resultCh, service.ErrUsageLogAccountRequired)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUsageBillingRepositoryApplyUsageInvalidAccountBeforeTransaction(t *testing.T) {
	for _, accountID := range []int64{0, -1} {
		t.Run(usageAccountIDTestName(accountID), func(t *testing.T) {
			db, mock := newUsageBillingSQLMock(t)
			repo := &usageBillingRepository{db: db}
			cmd := &service.UsageBillingCommand{
				RequestID: "req-invalid-account",
				AccountID: accountID,
				UsageLog:  &service.UsageLog{AccountID: usageLogAccountIDPointer(42)},
			}

			result, err := repo.Apply(context.Background(), cmd)

			require.Nil(t, result)
			require.ErrorIs(t, err, service.ErrUsageLogAccountRequired)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestUsageBillingRepositoryApplyUsageLogInvalidAccountBeforeTransaction(t *testing.T) {
	db, mock := newUsageBillingSQLMock(t)
	repo := &usageBillingRepository{db: db}
	cmd := &service.UsageBillingCommand{
		RequestID: "req-invalid-log-account",
		AccountID: 42,
		UsageLog:  &service.UsageLog{AccountID: nil},
	}

	result, err := repo.Apply(context.Background(), cmd)

	require.Nil(t, result)
	require.ErrorIs(t, err, service.ErrUsageLogAccountRequired)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUsageBillingRepositoryApplyPersistsPositiveScalarAccountID(t *testing.T) {
	db, mock := newUsageBillingSQLMock(t)
	repo := &usageBillingRepository{db: db}
	accountID := int64(42)
	cmd := &service.UsageBillingCommand{
		RequestID: "req-positive-account",
		APIKeyID:  10,
		AccountID: accountID,
		UsageLog: &service.UsageLog{
			APIKeyID:  10,
			AccountID: &accountID,
		},
	}
	cmd.Normalize()

	mock.ExpectBegin()
	expectUsageBillingClaimInserted(mock, cmd)
	mock.ExpectExec("INSERT INTO usage_logs").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), accountID, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	result, err := repo.Apply(context.Background(), cmd)

	require.NoError(t, err)
	require.True(t, result.UsageLogPersisted)
	require.NoError(t, mock.ExpectationsWereMet())
}

func usageAccountIDPointerTestName(accountID *int64) string {
	if accountID == nil {
		return "nil"
	}
	return usageAccountIDTestName(*accountID)
}

func usageAccountIDTestName(accountID int64) string {
	if accountID == 0 {
		return "zero"
	}
	return "negative"
}
