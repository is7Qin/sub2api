//go:build integration

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSchedulerSupportDecisionChannelUpdateDirtiesAffectedGroups(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	channelID, groupID := createSupportDecisionChannelScope(t, tx)
	clearSupportDecisionDirty(t, tx)

	_, err := tx.ExecContext(ctx, `UPDATE channels SET restrict_models = NOT restrict_models WHERE id = $1`, channelID)
	require.NoError(t, err)
	requireSupportDecisionGroupDirty(t, tx, groupID)

	clearSupportDecisionDirty(t, tx)
	_, err = tx.ExecContext(ctx, `UPDATE channels SET description = description || ' metadata' WHERE id = $1`, channelID)
	require.NoError(t, err)
	requireNoSupportDecisionDirty(t, tx)
}

func TestSchedulerSupportDecisionChannelInsertDirtiesNewGroup(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	var groupID int64
	require.NoError(t, tx.QueryRowContext(ctx, `INSERT INTO groups(name) VALUES($1) RETURNING id`, fmt.Sprintf("support-new-group-%d", time.Now().UnixNano())).Scan(&groupID))
	clearSupportDecisionDirty(t, tx)

	var channelID int64
	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO channels(name, status, restrict_models, billing_model_source)
VALUES($1, 'active', false, 'upstream') RETURNING id`, fmt.Sprintf("support-new-channel-%d", time.Now().UnixNano())).Scan(&channelID))
	var count int
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT count(*) FROM scheduler_dirty_work WHERE kind = 3 AND entity_id = 0`).Scan(&count))
	require.Equal(t, 1, count)

	clearSupportDecisionDirty(t, tx)
	_, err := tx.ExecContext(ctx, `INSERT INTO channel_groups(channel_id, group_id) VALUES($1, $2)`, channelID, groupID)
	require.NoError(t, err)
	requireSupportDecisionGroupDirty(t, tx, groupID)
}

func TestSchedulerSupportDecisionChannelMembershipDeleteDirtiesOldGroup(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	channelID, groupID := createSupportDecisionChannelScope(t, tx)
	clearSupportDecisionDirty(t, tx)

	_, err := tx.ExecContext(ctx, `DELETE FROM channel_groups WHERE channel_id = $1 AND group_id = $2`, channelID, groupID)
	require.NoError(t, err)
	requireSupportDecisionGroupDirty(t, tx, groupID)
}

func TestSchedulerSupportDecisionPricingModelChangeDirtiesAffectedGroups(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	channelID, groupID := createSupportDecisionChannelScope(t, tx)
	_, err := tx.ExecContext(ctx, `UPDATE channels SET restrict_models = true WHERE id = $1`, channelID)
	require.NoError(t, err)
	var pricingID int64
	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO channel_model_pricing(channel_id, platform, models, input_price)
VALUES($1, 'openai', '["gpt-old"]'::jsonb, 1) RETURNING id`, channelID).Scan(&pricingID))
	clearSupportDecisionDirty(t, tx)

	_, err = tx.ExecContext(ctx, `UPDATE channel_model_pricing SET models = '["gpt-new"]'::jsonb WHERE id = $1`, pricingID)
	require.NoError(t, err)
	requireSupportDecisionGroupDirty(t, tx, groupID)

	clearSupportDecisionDirty(t, tx)
	_, err = tx.ExecContext(ctx, `UPDATE channel_model_pricing SET input_price = input_price + 1 WHERE id = $1`, pricingID)
	require.NoError(t, err)
	requireNoSupportDecisionDirty(t, tx)

	var intervalID int64
	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO channel_pricing_intervals(pricing_id, input_price) VALUES($1, 1) RETURNING id`, pricingID).Scan(&intervalID))
	requireNoSupportDecisionDirty(t, tx)
	_, err = tx.ExecContext(ctx, `UPDATE channel_pricing_intervals SET input_price = 2 WHERE id = $1`, intervalID)
	require.NoError(t, err)
	requireNoSupportDecisionDirty(t, tx)
}

func TestSchedulerSupportDecisionUnscopedChannelChangeRequestsGlobalRebuild(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	var channelID int64
	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO channels(name, status, restrict_models, billing_model_source)
VALUES($1, 'active', false, 'upstream') RETURNING id`, fmt.Sprintf("support-unscoped-%d", time.Now().UnixNano())).Scan(&channelID))
	clearSupportDecisionDirty(t, tx)

	_, err := tx.ExecContext(ctx, `UPDATE channels SET restrict_models = true WHERE id = $1`, channelID)
	require.NoError(t, err)
	var count int
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT count(*) FROM scheduler_dirty_work WHERE kind = 3 AND entity_id = 0`).Scan(&count))
	require.Equal(t, 1, count)
}

func TestSchedulerSupportDecisionDirtyWorkIsTransactional(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	channelID, groupID := createSupportDecisionChannelScope(t, tx)
	clearSupportDecisionDirty(t, tx)

	_, err := tx.ExecContext(ctx, `SAVEPOINT support_dirty_rollback`)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `UPDATE channels SET restrict_models = NOT restrict_models WHERE id = $1`, channelID)
	require.NoError(t, err)
	requireSupportDecisionGroupDirty(t, tx, groupID)
	_, err = tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT support_dirty_rollback`)
	require.NoError(t, err)
	requireNoSupportDecisionDirty(t, tx)
}

func createSupportDecisionChannelScope(t *testing.T, tx *sql.Tx) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	var groupID, channelID int64
	require.NoError(t, tx.QueryRowContext(ctx, `INSERT INTO groups(name) VALUES($1) RETURNING id`, fmt.Sprintf("support-group-%d", suffix)).Scan(&groupID))
	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO channels(name, status, restrict_models, billing_model_source)
VALUES($1, 'active', false, 'upstream') RETURNING id`, fmt.Sprintf("support-channel-%d", suffix)).Scan(&channelID))
	_, err := tx.ExecContext(ctx, `INSERT INTO channel_groups(channel_id, group_id) VALUES($1, $2)`, channelID, groupID)
	require.NoError(t, err)
	return channelID, groupID
}

func clearSupportDecisionDirty(t *testing.T, tx *sql.Tx) {
	t.Helper()
	_, err := tx.ExecContext(context.Background(), `TRUNCATE scheduler_dirty_group_sources, scheduler_dirty_work`)
	require.NoError(t, err)
}

func requireSupportDecisionGroupDirty(t *testing.T, tx *sql.Tx, groupID int64) {
	t.Helper()
	var count int
	require.NoError(t, tx.QueryRowContext(context.Background(), `SELECT count(*) FROM scheduler_dirty_group_sources WHERE group_id = $1`, groupID).Scan(&count))
	require.Equal(t, 1, count)
}

func requireNoSupportDecisionDirty(t *testing.T, tx *sql.Tx) {
	t.Helper()
	var count int
	require.NoError(t, tx.QueryRowContext(context.Background(), `
SELECT (SELECT count(*) FROM scheduler_dirty_group_sources) +
       (SELECT count(*) FROM scheduler_dirty_work WHERE kind = 3 AND entity_id = 0)`).Scan(&count))
	require.Zero(t, count)
}
