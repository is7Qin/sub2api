//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestSchedulerDirtyTriggerScopeIgnoresRuntimeOnlyUpdates 验证 177 迁移后的触发
// 函数（定点白名单契约）：只动 extra 运行时 key / last_used_at / 白名单外 key
// 的 UPDATE 不产生脏标记；status/schedulable 等调度相关列、运行时叠加列
// （限流/会话窗口）与白名单 extra key 必须产生脏标记。
func TestSchedulerDirtyTriggerScopeIgnoresRuntimeOnlyUpdates(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	suffix := time.Now().UnixNano()

	var accountID int64
	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO accounts(name, platform, type, status, schedulable)
VALUES($1, 'openai', 'oauth', 'active', true) RETURNING id
`, fmt.Sprintf("dirty-scope-%d", suffix)).Scan(&accountID))
	truncateSchedulerDirtyTables(t, tx)

	// 只写 extra 运行时 key（codex 用量快照等）：不产生脏标记。
	_, err := tx.ExecContext(ctx, `
UPDATE accounts
SET extra = jsonb_set(COALESCE(extra, '{}'::jsonb), '{codex_usage_updated_at}', '"2026-08-03T00:00:00Z"'::jsonb, true)
WHERE id = $1`, accountID)
	require.NoError(t, err)
	requireNoAccountSource(t, tx, accountID)

	// 只写 last_used_at：不产生脏标记（观测性写入走独立通道）。
	_, err = tx.ExecContext(ctx, "UPDATE accounts SET last_used_at = NOW() WHERE id = $1", accountID)
	require.NoError(t, err)
	requireNoAccountSource(t, tx, accountID)

	// 调度相关列变更：必须产生脏标记（status/schedulable 影响分桶 → bucket_dirty）。
	_, err = tx.ExecContext(ctx, "UPDATE accounts SET status = 'error' WHERE id = $1", accountID)
	require.NoError(t, err)
	requireAccountSource(t, tx, accountID, 1, true)

	truncateSchedulerDirtyTables(t, tx)
	_, err = tx.ExecContext(ctx, "UPDATE accounts SET schedulable = false WHERE id = $1", accountID)
	require.NoError(t, err)
	requireAccountSource(t, tx, accountID, 1, true)

	// 生命周期 extra key（mixed_scheduling）：必须产生脏标记并带 bucket_dirty。
	truncateSchedulerDirtyTables(t, tx)
	_, err = tx.ExecContext(ctx, `
UPDATE accounts SET extra = jsonb_set(COALESCE(extra, '{}'::jsonb), '{mixed_scheduling}', 'true'::jsonb, true) WHERE id = $1`, accountID)
	require.NoError(t, err)
	requireAccountSource(t, tx, accountID, 1, true)

	// 运行时叠加列（限流/会话窗口）：调度消费，产生脏标记（不强制分桶重建）。
	for _, update := range []string{
		"rate_limited_at = NOW()",
		"overload_until = NOW() + INTERVAL '2 minutes'",
		"session_window_end = NOW() + INTERVAL '5 hours'",
	} {
		truncateSchedulerDirtyTables(t, tx)
		_, err = tx.ExecContext(ctx, "UPDATE accounts SET "+update+" WHERE id = $1", accountID)
		require.NoError(t, err)
		requireAccountSource(t, tx, accountID, 1, false)
	}

	// 白名单 extra key（privacy_mode 调度判定读取）：产生脏标记。
	truncateSchedulerDirtyTables(t, tx)
	_, err = tx.ExecContext(ctx, `
UPDATE accounts SET extra = jsonb_set(COALESCE(extra, '{}'::jsonb), '{privacy_mode}', '"training_off"'::jsonb, true) WHERE id = $1`, accountID)
	require.NoError(t, err)
	requireAccountSource(t, tx, accountID, 1, false)

	// 白名单外 extra key：不产生脏标记（定点白名单契约，不再保守脏化）。
	truncateSchedulerDirtyTables(t, tx)
	_, err = tx.ExecContext(ctx, `
UPDATE accounts SET extra = jsonb_set(COALESCE(extra, '{}'::jsonb), '{future_scheduler_key}', 'true'::jsonb, true) WHERE id = $1`, accountID)
	require.NoError(t, err)
	requireNoAccountSource(t, tx, accountID)
}
