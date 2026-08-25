//go:build integration

package repository

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSchedulerSupportPublicationGenerationSequenceIsIdempotent(t *testing.T) {
	ctx := context.Background()
	const statement = `CREATE SEQUENCE IF NOT EXISTS scheduler_support_publication_generation_seq AS BIGINT INCREMENT BY 1 MINVALUE 1 START WITH 1 NO CYCLE`
	_, err := integrationDB.ExecContext(ctx, statement)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, statement)
	require.NoError(t, err)
}

func TestSchedulerSupportPublicationGenerationIsMonotonic(t *testing.T) {
	repo := NewSupportDecisionGenerationRepository(integrationDB)
	first, err := repo.NextSupportDecisionGeneration(context.Background())
	require.NoError(t, err)
	second, err := repo.NextSupportDecisionGeneration(context.Background())
	require.NoError(t, err)
	require.NotZero(t, first)
	require.Greater(t, second, first)
}

func TestSchedulerSupportPublicationGenerationAllowsGaps(t *testing.T) {
	ctx := context.Background()
	repo := NewSupportDecisionGenerationRepository(integrationDB)
	first, err := repo.NextSupportDecisionGeneration(ctx)
	require.NoError(t, err)

	tx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	var discarded int64
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT nextval('scheduler_support_publication_generation_seq')`).Scan(&discarded))
	require.NoError(t, tx.Rollback())

	second, err := repo.NextSupportDecisionGeneration(ctx)
	require.NoError(t, err)
	require.Greater(t, second, first+1)
}
