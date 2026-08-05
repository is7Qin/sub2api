//go:build unit

package repository

import (
	"context"
	"errors"
	"regexp"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

const nextSupportDecisionGenerationSQL = `SELECT nextval('scheduler_support_publication_generation_seq')`

func newSupportDecisionGenerationTestRepository(t *testing.T) (*supportDecisionSource, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return &supportDecisionSource{db: db}, mock
}

func TestSchedulerSupportPublicationGenerationIsMonotonic(t *testing.T) {
	repo, mock := newSupportDecisionGenerationTestRepository(t)
	mock.ExpectQuery(regexp.QuoteMeta(nextSupportDecisionGenerationSQL)).WillReturnRows(sqlmock.NewRows([]string{"nextval"}).AddRow(int64(41)))
	mock.ExpectQuery(regexp.QuoteMeta(nextSupportDecisionGenerationSQL)).WillReturnRows(sqlmock.NewRows([]string{"nextval"}).AddRow(int64(42)))

	first, err := repo.NextSupportDecisionGeneration(context.Background())
	require.NoError(t, err)
	second, err := repo.NextSupportDecisionGeneration(context.Background())
	require.NoError(t, err)
	require.Equal(t, uint64(41), first)
	require.Equal(t, uint64(42), second)
	require.Greater(t, second, first)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSchedulerSupportPublicationGenerationAllowsGaps(t *testing.T) {
	repo, mock := newSupportDecisionGenerationTestRepository(t)
	mock.ExpectQuery(regexp.QuoteMeta(nextSupportDecisionGenerationSQL)).WillReturnRows(sqlmock.NewRows([]string{"nextval"}).AddRow(int64(7)))
	mock.ExpectQuery(regexp.QuoteMeta(nextSupportDecisionGenerationSQL)).WillReturnRows(sqlmock.NewRows([]string{"nextval"}).AddRow(int64(11)))

	first, err := repo.NextSupportDecisionGeneration(context.Background())
	require.NoError(t, err)
	second, err := repo.NextSupportDecisionGeneration(context.Background())
	require.NoError(t, err)
	require.Equal(t, uint64(7), first)
	require.Equal(t, uint64(11), second)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSupportDecisionGenerationReportsQueryScanAndInvalidValues(t *testing.T) {
	t.Run("query", func(t *testing.T) {
		repo, mock := newSupportDecisionGenerationTestRepository(t)
		mock.ExpectQuery(regexp.QuoteMeta(nextSupportDecisionGenerationSQL)).WillReturnError(errors.New("sequence unavailable"))
		generation, err := repo.NextSupportDecisionGeneration(context.Background())
		require.Zero(t, generation)
		require.ErrorContains(t, err, "allocate support decision generation")
		require.NoError(t, mock.ExpectationsWereMet())
	})

	for name, value := range map[string]any{
		"zero":     int64(0),
		"negative": int64(-1),
		"scan":     "not-an-integer",
	} {
		t.Run(name, func(t *testing.T) {
			repo, mock := newSupportDecisionGenerationTestRepository(t)
			mock.ExpectQuery(regexp.QuoteMeta(nextSupportDecisionGenerationSQL)).WillReturnRows(sqlmock.NewRows([]string{"nextval"}).AddRow(value))
			generation, err := repo.NextSupportDecisionGeneration(context.Background())
			require.Zero(t, generation)
			require.Error(t, err)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestSupportDecisionGenerationRepositoryContract(t *testing.T) {
	var _ service.SupportDecisionGenerationRepository = (*supportDecisionSource)(nil)
}
