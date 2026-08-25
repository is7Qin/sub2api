package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestBillingOutboxRepository_CleanupTerminal(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	repo := &billingOutboxRepository{db: db}
	cutoff := time.Now().UTC().Add(-24 * time.Hour)

	mock.ExpectExec(`(?s)DELETE FROM billing_attempt_outbox.*status IN \('succeeded', 'terminal'\).*updated_at < \$1.*LIMIT \$2`).
		WithArgs(cutoff, 5000).
		WillReturnResult(sqlmock.NewResult(0, 42))

	deleted, err := repo.CleanupTerminal(context.Background(), cutoff, 5000)
	require.NoError(t, err)
	require.Equal(t, int64(42), deleted)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSchedulerOutboxRepository_CleanupConsumed(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	repo := &schedulerOutboxRepository{db: db}

	mock.ExpectExec(`(?s)DELETE FROM scheduler_outbox.*id <= \$1.*LIMIT \$2`).
		WithArgs(int64(1000), 5000).
		WillReturnResult(sqlmock.NewResult(0, 250))

	deleted, err := repo.CleanupConsumed(context.Background(), 1000, 5000)
	require.NoError(t, err)
	require.Equal(t, int64(250), deleted)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSchedulerOutboxRepository_CleanupConsumedSkipsNonPositiveWatermark(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	repo := &schedulerOutboxRepository{db: db}

	deleted, err := repo.CleanupConsumed(context.Background(), 0, 5000)
	require.NoError(t, err)
	require.Equal(t, int64(0), deleted)
	require.NoError(t, mock.ExpectationsWereMet())
}
