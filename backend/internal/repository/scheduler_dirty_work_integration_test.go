//go:build integration

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestSchedulerDirtySourceTriggersAreLocalAtomicAndCoalescing(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)

	var accountID, oldGroupID, newGroupID int64
	suffix := time.Now().UnixNano()
	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO groups (name) VALUES ($1) RETURNING id
`, fmt.Sprintf("dirty-old-%d", suffix)).Scan(&oldGroupID))
	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO groups (name) VALUES ($1) RETURNING id
`, fmt.Sprintf("dirty-new-%d", suffix)).Scan(&newGroupID))
	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO accounts (name, platform, type) VALUES ($1, 'openai', 'oauth') RETURNING id
`, fmt.Sprintf("dirty-account-%d", suffix)).Scan(&accountID))
	_, err := tx.ExecContext(ctx, `
INSERT INTO account_groups (account_id, group_id) VALUES ($1, $2)
`, accountID, oldGroupID)
	require.NoError(t, err)
	truncateSchedulerDirtyTables(t, tx)

	// Bucket-affecting writes accumulate a sticky bit on the account identity.
	_, err = tx.ExecContext(ctx, "UPDATE accounts SET priority = priority + 1 WHERE id = $1", accountID)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, "UPDATE accounts SET concurrency = concurrency + 1 WHERE id = $1", accountID)
	require.NoError(t, err)
	requireAccountSource(t, tx, accountID, 2, true)
	requireNoCanonicalDirty(t, tx)

	// Moving a membership preserves both OLD and NEW pair identities.
	_, err = tx.ExecContext(ctx, `
UPDATE account_groups SET group_id = $1 WHERE account_id = $2 AND group_id = $3
`, newGroupID, accountID, oldGroupID)
	require.NoError(t, err)
	requireMembershipSource(t, tx, accountID, oldGroupID, 1)
	requireMembershipSource(t, tx, accountID, newGroupID, 1)
	requireNoCanonicalDirty(t, tx)

	// Observational last-used writes stay off the source hot path; runtime
	// last-used publication is monotonic and handled separately.
	truncateSchedulerDirtyTables(t, tx)
	usedAt := time.Now().UTC().Add(time.Hour)
	_, err = tx.ExecContext(ctx, "UPDATE accounts SET last_used_at = $1 WHERE id = $2", usedAt, accountID)
	require.NoError(t, err)
	var sourceCount int
	require.NoError(t, tx.QueryRowContext(ctx, "SELECT count(*) FROM scheduler_dirty_account_sources WHERE account_id=$1", accountID).Scan(&sourceCount))
	require.Zero(t, sourceCount)

	// Runtime overlay columns are scheduling inputs consumed by the cache payload
	// (rate-limited / overloaded / session-window state), so their changes refresh
	// the account snapshot — without forcing a bucket rebuild.
	for _, update := range []string{
		"rate_limited_at = NOW()",
		"rate_limit_reset_at = NOW() + INTERVAL '1 minute'",
		"overload_until = NOW() + INTERVAL '2 minutes'",
		"temp_unschedulable_until = NOW() + INTERVAL '3 minutes'",
		"temp_unschedulable_reason = 'runtime penalty'",
		"session_window_start = NOW()",
		"session_window_end = NOW() + INTERVAL '5 hours'",
		"session_window_status = 'active'",
	} {
		truncateSchedulerDirtyTables(t, tx)
		_, err = tx.ExecContext(ctx, "UPDATE accounts SET "+update+" WHERE id = $1", accountID)
		require.NoError(t, err)
		requireAccountSource(t, tx, accountID, 1, false)
	}

	for key, value := range map[string]string{
		"codex_usage_updated_at":               `"2026-07-17T00:00:00Z"`,
		"model_rate_limits":                    `{"gpt-5":{"rate_limit_reset_at":"2026-07-17T01:00:00Z"}}`,
		"openai_codex_fingerprint":             `{"schema_version":1}`,
		"session_window_utilization":           `0.5`,
		"codex_primary_used_percent":           `11.5`,
		"codex_primary_reset_after_seconds":    `120`,
		"codex_primary_window_minutes":         `300`,
		"codex_primary_over_secondary_percent": `9.5`,
		"codex_secondary_used_percent":         `12.5`,
		"codex_secondary_reset_after_seconds":  `240`,
		"codex_secondary_window_minutes":       `10080`,
		"codex_5h_used_percent":                `22.5`,
		"codex_5h_reset_after_seconds":         `3600`,
		"codex_5h_window_minutes":              `300`,
		"codex_5h_reset_at":                    `"2026-07-17T05:00:00Z"`,
		"codex_7d_used_percent":                `33.5`,
		"codex_7d_reset_after_seconds":         `7200`,
		"codex_7d_window_minutes":              `10080`,
		"codex_7d_reset_at":                    `"2026-07-18T00:00:00Z"`,
		"passive_usage_7d_utilization":         `0.42`,
		"passive_usage_7d_reset":               `1784332800`,
		"passive_usage_7d_oi_utilization":      `0.87`,
		"passive_usage_7d_oi_reset":            `1784332800`,
		"passive_usage_sampled_at":             `"2026-07-17T00:00:00Z"`,
	} {
		truncateSchedulerDirtyTables(t, tx)
		_, err = tx.ExecContext(ctx, `
			UPDATE accounts
			SET extra = jsonb_set(COALESCE(extra, '{}'::jsonb), ARRAY[$1], $2::jsonb, true)
			WHERE id = $3
		`, key, value, accountID)
		require.NoError(t, err)
		requireNoAccountSource(t, tx, accountID)
	}

	// Unknown keys outside the fixed-point whitelist no longer dirty: the 177
	// trigger compares exactly the scheduling-relevant keys, nothing more.
	for _, key := range []string{
		"codex_primary_future_policy",
		"codex_5h_future_policy",
		"passive_usage_future_policy",
	} {
		truncateSchedulerDirtyTables(t, tx)
		_, err = tx.ExecContext(ctx, `
			UPDATE accounts
			SET extra = jsonb_set(COALESCE(extra, '{}'::jsonb), ARRAY[$1], 'true'::jsonb, true)
			WHERE id = $2
		`, key, accountID)
		require.NoError(t, err)
		requireNoAccountSource(t, tx, accountID)
	}

	// Whitelisted static policy keys still refresh the snapshot (no bucket
	// rebuild); only mixed_scheduling affects bucket membership.
	truncateSchedulerDirtyTables(t, tx)
	_, err = tx.ExecContext(ctx, `UPDATE accounts SET extra = jsonb_set(COALESCE(extra, '{}'::jsonb), '{privacy_mode}', '"training_off"'::jsonb, true) WHERE id = $1`, accountID)
	require.NoError(t, err)
	requireAccountSource(t, tx, accountID, 1, false)

	truncateSchedulerDirtyTables(t, tx)
	_, err = tx.ExecContext(ctx, `UPDATE accounts SET extra = jsonb_set(COALESCE(extra, '{}'::jsonb), '{future_scheduler_key}', 'true'::jsonb, true) WHERE id = $1`, accountID)
	require.NoError(t, err)
	requireNoAccountSource(t, tx, accountID)

	truncateSchedulerDirtyTables(t, tx)
	_, err = tx.ExecContext(ctx, `UPDATE accounts SET extra = jsonb_set(COALESCE(extra, '{}'::jsonb), '{mixed_scheduling}', 'true'::jsonb, true) WHERE id = $1`, accountID)
	require.NoError(t, err)
	requireAccountSource(t, tx, accountID, 1, true)

	// Scheduler metadata changes still request account refresh without forcing a
	// bucket rebuild. Explicit administrative last-used clear remains valid.
	truncateSchedulerDirtyTables(t, tx)
	_, err = tx.ExecContext(ctx, "UPDATE accounts SET concurrency = concurrency + 1 WHERE id = $1", accountID)
	require.NoError(t, err)
	requireAccountSource(t, tx, accountID, 1, false)
	_, err = tx.ExecContext(ctx, "UPDATE accounts SET last_used_at = NULL WHERE id = $1", accountID)
	require.NoError(t, err)
	var stored *time.Time
	require.NoError(t, tx.QueryRowContext(ctx, "SELECT last_used_at FROM accounts WHERE id = $1", accountID).Scan(&stored))
	require.Nil(t, stored)

	// Business rollback also rolls back source-local evidence.
	truncateSchedulerDirtyTables(t, tx)
	_, err = tx.ExecContext(ctx, "SAVEPOINT scheduler_dirty_rollback")
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, "UPDATE accounts SET priority = priority + 1 WHERE id = $1", accountID)
	require.NoError(t, err)
	requireAccountSource(t, tx, accountID, 1, true)
	_, err = tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT scheduler_dirty_rollback")
	require.NoError(t, err)
	sourceCount = 0
	require.NoError(t, tx.QueryRowContext(ctx, "SELECT count(*) FROM scheduler_dirty_account_sources WHERE account_id=$1", accountID).Scan(&sourceCount))
	require.Zero(t, sourceCount)
}

func TestAutoPauseExpiredAccountsUsesTransactionalDirtySources(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	suffix := time.Now().UnixNano()
	now := time.Now().UTC()

	var groupID int64
	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO groups(name) VALUES($1) RETURNING id
`, fmt.Sprintf("auto-pause-group-%d", suffix)).Scan(&groupID))

	accountIDs := make([]int64, 3)
	for i := range accountIDs {
		require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO accounts(
    name, platform, type, schedulable, auto_pause_on_expired, expires_at
) VALUES ($1, 'openai', 'oauth', true, $2, $3) RETURNING id
`, fmt.Sprintf("auto-pause-%d-%d", suffix, i), i < 2, now.Add(time.Duration(i-1)*time.Hour)).Scan(&accountIDs[i]))
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO account_groups(account_id, group_id) VALUES($1, $2)
`, accountIDs[0], groupID)
	require.NoError(t, err)
	truncateSchedulerDirtyTables(t, tx)
	_, err = tx.ExecContext(ctx, `TRUNCATE scheduler_outbox RESTART IDENTITY`)
	require.NoError(t, err)

	repo := &accountRepository{sql: tx}
	paused, err := repo.AutoPauseExpiredAccounts(ctx, now)
	require.NoError(t, err)
	require.Equal(t, int64(2), paused)
	requireAccountSource(t, tx, accountIDs[0], 1, true)
	requireAccountSource(t, tx, accountIDs[1], 1, true)
	requireNoAccountSource(t, tx, accountIDs[2])

	var outboxCount int
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT count(*) FROM scheduler_outbox`).Scan(&outboxCount))
	require.Zero(t, outboxCount)

	// The same business transaction owns both the pause and its invalidation
	// evidence, so rolling it back cannot leave either side committed alone.
	truncateSchedulerDirtyTables(t, tx)
	_, err = tx.ExecContext(ctx, `UPDATE accounts SET schedulable=true WHERE id=$1`, accountIDs[0])
	require.NoError(t, err)
	truncateSchedulerDirtyTables(t, tx)
	_, err = tx.ExecContext(ctx, `SAVEPOINT auto_pause_rollback`)
	require.NoError(t, err)
	paused, err = repo.AutoPauseExpiredAccounts(ctx, now)
	require.NoError(t, err)
	require.Equal(t, int64(1), paused)
	requireAccountSource(t, tx, accountIDs[0], 1, true)
	_, err = tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT auto_pause_rollback`)
	require.NoError(t, err)
	requireNoAccountSource(t, tx, accountIDs[0])
	var schedulable bool
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT schedulable FROM accounts WHERE id=$1`, accountIDs[0]).Scan(&schedulable))
	require.True(t, schedulable)
}

func TestAutoPauseExpiredAccountsHonorsCallerTransaction(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	now := time.Now().UTC()
	account := mustCreateAccount(t, client, &service.Account{
		Name:        fmt.Sprintf("auto-pause-caller-tx-%d", time.Now().UnixNano()),
		Schedulable: true,
	})
	t.Cleanup(func() { _, _ = client.Account.Delete().Where(dbaccount.IDEQ(account.ID)).Exec(context.Background()) })
	_, err := client.Account.UpdateOneID(account.ID).
		SetAutoPauseOnExpired(true).
		SetExpiresAt(now.Add(-time.Hour)).
		Save(ctx)
	require.NoError(t, err)

	repo := newAccountRepositoryWithSQL(client, integrationDB, nil)
	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	txCtx := dbent.NewTxContext(ctx, tx)

	paused, err := repo.AutoPauseExpiredAccounts(txCtx, now)
	require.NoError(t, err)
	require.Equal(t, int64(1), paused)
	require.NoError(t, tx.Rollback())

	got, err := repo.GetByID(ctx, account.ID)
	require.NoError(t, err)
	require.True(t, got.Schedulable)
}

func TestSchedulerGroupPrimaryKeyUpdatePreservesOldAndNewSources(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	suffix := time.Now().UnixNano()
	var oldID int64
	require.NoError(t, tx.QueryRowContext(ctx, `INSERT INTO groups(name) VALUES($1) RETURNING id`, fmt.Sprintf("group-pk-%d", suffix)).Scan(&oldID))
	truncateSchedulerDirtyTables(t, tx)
	newID := oldID + 1000000
	_, err := tx.ExecContext(ctx, `UPDATE groups SET id=$1 WHERE id=$2`, newID, oldID)
	require.NoError(t, err)
	for _, id := range []int64{oldID, newID} {
		var generation int64
		require.NoError(t, tx.QueryRowContext(ctx, `SELECT generation FROM scheduler_dirty_group_sources WHERE group_id=$1`, id).Scan(&generation))
		require.Equal(t, int64(1), generation)
	}
}

func TestSchedulerAccountPrimaryKeyUpdatePreservesOldAndNewSources(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	suffix := time.Now().UnixNano()
	var oldID int64
	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO accounts(name, platform, type) VALUES($1, 'openai', 'oauth') RETURNING id
`, fmt.Sprintf("account-pk-%d", suffix)).Scan(&oldID))
	truncateSchedulerDirtyTables(t, tx)
	newID := oldID + 1000000
	_, err := tx.ExecContext(ctx, `UPDATE accounts SET id=$1 WHERE id=$2`, newID, oldID)
	require.NoError(t, err)
	for _, id := range []int64{oldID, newID} {
		var generation int64
		var bucketDirty bool
		require.NoError(t, tx.QueryRowContext(ctx, `
SELECT generation, bucket_dirty
FROM scheduler_dirty_account_sources
WHERE account_id=$1
`, id).Scan(&generation, &bucketDirty))
		require.Equal(t, int64(1), generation)
		require.True(t, bucketDirty)
	}
}

func TestSchedulerAccountDirtyProjectionClassifiesEachUpdatedRow(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	suffix := time.Now().UnixNano()

	var runtimeID, lifecycleID int64
	require.NoError(t, tx.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type) VALUES($1,'openai','oauth') RETURNING id`, fmt.Sprintf("projection-runtime-%d", suffix)).Scan(&runtimeID))
	require.NoError(t, tx.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type) VALUES($1,'openai','oauth') RETURNING id`, fmt.Sprintf("projection-lifecycle-%d", suffix)).Scan(&lifecycleID))
	truncateSchedulerDirtyTables(t, tx)

	_, err := tx.ExecContext(ctx, `
UPDATE accounts
SET extra = CASE id
	WHEN $1 THEN jsonb_set(COALESCE(extra, '{}'::jsonb), '{codex_5h_used_percent}', '50'::jsonb, true)
	WHEN $2 THEN jsonb_set(COALESCE(extra, '{}'::jsonb), '{future_scheduler_key}', 'true'::jsonb, true)
	ELSE extra
END
WHERE id IN ($1, $2)`, runtimeID, lifecycleID)
	require.NoError(t, err)

	// 运行时 key 与白名单外 key 都不产生脏标记（定点白名单契约）。
	requireNoAccountSource(t, tx, runtimeID)
	requireNoAccountSource(t, tx, lifecycleID)
}

func TestSchedulerRuntimeProjectionDoesNotDisturbPendingLifecycleSource(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	suffix := time.Now().UnixNano()

	var accountID int64
	require.NoError(t, tx.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type) VALUES($1,'openai','oauth') RETURNING id`, fmt.Sprintf("projection-pending-%d", suffix)).Scan(&accountID))
	truncateSchedulerDirtyTables(t, tx)

	_, err := tx.ExecContext(ctx, `UPDATE accounts SET priority=priority+1 WHERE id=$1`, accountID)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `UPDATE scheduler_dirty_account_sources SET group_cursor=42 WHERE account_id=$1`, accountID)
	require.NoError(t, err)

	// Runtime-only observations must not restart pending lifecycle fanout or
	// advance its generation. A later whitelisted lifecycle change must do both
	// while keeping the earlier bucket-rebuild intent sticky.
	_, err = tx.ExecContext(ctx, `UPDATE accounts SET extra=jsonb_set(COALESCE(extra, '{}'::jsonb), '{codex_5h_used_percent}', '50'::jsonb, true) WHERE id=$1`, accountID)
	require.NoError(t, err)
	requireAccountSourceState(t, tx, accountID, 1, true, 42)
	_, err = tx.ExecContext(ctx, `UPDATE accounts SET extra=COALESCE(extra, '{}'::jsonb)-'codex_5h_used_percent' WHERE id=$1`, accountID)
	require.NoError(t, err)
	requireAccountSourceState(t, tx, accountID, 1, true, 42)

	// 白名单外 key（future_scheduler_key）不推进挂起的生命周期扇出。
	_, err = tx.ExecContext(ctx, `UPDATE accounts SET extra=jsonb_set(COALESCE(extra, '{}'::jsonb), '{future_scheduler_key}', 'true'::jsonb, true) WHERE id=$1`, accountID)
	require.NoError(t, err)
	requireAccountSourceState(t, tx, accountID, 1, true, 42)
}

func TestSchedulerMixedSchedulingChangePromotesPendingSourceToBucketDirty(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	suffix := time.Now().UnixNano()

	var accountID int64
	require.NoError(t, tx.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type) VALUES($1,'openai','oauth') RETURNING id`, fmt.Sprintf("projection-mixed-%d", suffix)).Scan(&accountID))
	truncateSchedulerDirtyTables(t, tx)

	_, err := tx.ExecContext(ctx, `UPDATE accounts SET concurrency=concurrency+1 WHERE id=$1`, accountID)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `UPDATE scheduler_dirty_account_sources SET group_cursor=42 WHERE account_id=$1`, accountID)
	require.NoError(t, err)
	requireAccountSourceState(t, tx, accountID, 1, false, 42)

	// A mixed runtime/lifecycle statement remains lifecycle-relevant because
	// mixed_scheduling changes bucket membership. It must restart pending fanout
	// and promote the previously clean source to sticky bucket rebuild work.
	_, err = tx.ExecContext(ctx, `
UPDATE accounts
SET extra=jsonb_set(
	jsonb_set(
		COALESCE(extra, '{}'::jsonb),
		'{codex_5h_used_percent}',
		'50'::jsonb,
		true
	),
	'{mixed_scheduling}',
	'true'::jsonb,
	true
)
WHERE id=$1`, accountID)
	require.NoError(t, err)
	requireAccountSourceState(t, tx, accountID, 2, true, 0)

	_, err = tx.ExecContext(ctx, `UPDATE accounts SET extra=COALESCE(extra, '{}'::jsonb)-'mixed_scheduling' WHERE id=$1`, accountID)
	require.NoError(t, err)
	requireAccountSourceState(t, tx, accountID, 3, true, 0)
}

func TestSchedulerDirtySourceStatementUpdatesAreSortedAndCoalesced(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	suffix := time.Now().UnixNano()

	var firstID, secondID int64
	require.NoError(t, tx.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type) VALUES($1,'openai','oauth') RETURNING id`, fmt.Sprintf("statement-a-%d", suffix)).Scan(&firstID))
	require.NoError(t, tx.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type) VALUES($1,'openai','oauth') RETURNING id`, fmt.Sprintf("statement-b-%d", suffix)).Scan(&secondID))
	truncateSchedulerDirtyTables(t, tx)

	_, err := tx.ExecContext(ctx, `UPDATE accounts SET priority=priority+1 WHERE id IN ($1,$2)`, secondID, firstID)
	require.NoError(t, err)
	requireAccountSource(t, tx, firstID, 1, true)
	requireAccountSource(t, tx, secondID, 1, true)
}

func truncateSchedulerDirtyTables(t *testing.T, exec interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}) {
	t.Helper()
	_, err := exec.ExecContext(context.Background(), `TRUNCATE scheduler_dirty_account_sources, scheduler_dirty_group_sources, scheduler_dirty_membership_sources, scheduler_dirty_work`)
	require.NoError(t, err)
}

func requireAccountSource(t *testing.T, tx queryRower, accountID, generation int64, bucketDirty bool) {
	t.Helper()
	var gotGeneration int64
	var gotBucketDirty bool
	require.NoError(t, tx.QueryRowContext(context.Background(), `
SELECT generation,bucket_dirty FROM scheduler_dirty_account_sources WHERE account_id=$1
`, accountID).Scan(&gotGeneration, &gotBucketDirty))
	require.Equal(t, generation, gotGeneration)
	require.Equal(t, bucketDirty, gotBucketDirty)
}

func requireAccountSourceState(t *testing.T, tx queryRower, accountID, generation int64, bucketDirty bool, groupCursor int64) {
	t.Helper()
	var gotGeneration, gotGroupCursor int64
	var gotBucketDirty bool
	require.NoError(t, tx.QueryRowContext(context.Background(), `
SELECT generation,bucket_dirty,group_cursor FROM scheduler_dirty_account_sources WHERE account_id=$1
`, accountID).Scan(&gotGeneration, &gotBucketDirty, &gotGroupCursor))
	require.Equal(t, generation, gotGeneration)
	require.Equal(t, bucketDirty, gotBucketDirty)
	require.Equal(t, groupCursor, gotGroupCursor)
}

func requireNoAccountSource(t *testing.T, tx queryRower, accountID int64) {
	t.Helper()
	var count int
	require.NoError(t, tx.QueryRowContext(context.Background(), `
SELECT count(*) FROM scheduler_dirty_account_sources WHERE account_id=$1
`, accountID).Scan(&count))
	require.Zero(t, count)
}

func requireMembershipSource(t *testing.T, tx queryRower, accountID, groupID, generation int64) {
	t.Helper()
	var got int64
	require.NoError(t, tx.QueryRowContext(context.Background(), `
SELECT generation FROM scheduler_dirty_membership_sources WHERE account_id=$1 AND group_id=$2
`, accountID, groupID).Scan(&got))
	require.Equal(t, generation, got)
}

func requireNoCanonicalDirty(t *testing.T, tx queryRower) {
	t.Helper()
	var count int
	require.NoError(t, tx.QueryRowContext(context.Background(), `SELECT count(*) FROM scheduler_dirty_work`).Scan(&count))
	require.Zero(t, count)
}

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}
