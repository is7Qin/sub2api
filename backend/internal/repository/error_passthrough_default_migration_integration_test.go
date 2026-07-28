//go:build integration

package repository

import (
	"context"
	"database/sql"
	"testing"

	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestMigration170SeedsOpenAICyberPolicyFallbackRuleIdempotently(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	content, err := migrations.FS.ReadFile("170_seed_openai_cyber_policy_passthrough.sql")
	require.NoError(t, err)

	const ruleName = "OpenAI cyber_policy fallback"
	_, err = tx.ExecContext(ctx, "DELETE FROM error_passthrough_rules WHERE name = $1", ruleName)
	require.NoError(t, err)

	for range 2 {
		_, err = tx.ExecContext(ctx, string(content))
		require.NoError(t, err, "migration SQL must be rerunnable")
	}

	var rule struct {
		Enabled         bool
		Priority        int
		ErrorCodes      string
		Keywords        string
		MatchMode       string
		Platforms       string
		PassthroughCode bool
		ResponseCode    sql.NullInt64
		PassthroughBody bool
		CustomMessage   sql.NullString
		SkipMonitoring  bool
	}
	err = tx.QueryRowContext(ctx, `
SELECT
    enabled,
    priority,
    error_codes::text,
    keywords::text,
    match_mode,
    platforms::text,
    passthrough_code,
    response_code,
    passthrough_body,
    custom_message,
    skip_monitoring
FROM error_passthrough_rules
WHERE name = $1
`, ruleName).Scan(
		&rule.Enabled,
		&rule.Priority,
		&rule.ErrorCodes,
		&rule.Keywords,
		&rule.MatchMode,
		&rule.Platforms,
		&rule.PassthroughCode,
		&rule.ResponseCode,
		&rule.PassthroughBody,
		&rule.CustomMessage,
		&rule.SkipMonitoring,
	)
	require.NoError(t, err)
	require.True(t, rule.Enabled)
	require.Equal(t, 1000, rule.Priority)
	require.Equal(t, "[]", rule.ErrorCodes)
	require.JSONEq(t, `["cyber_policy"]`, rule.Keywords)
	require.Equal(t, "all", rule.MatchMode)
	require.JSONEq(t, `["openai"]`, rule.Platforms)
	require.False(t, rule.PassthroughCode)
	require.Equal(t, int64(400), rule.ResponseCode.Int64)
	require.True(t, rule.ResponseCode.Valid)
	require.True(t, rule.PassthroughBody)
	require.False(t, rule.CustomMessage.Valid)
	require.False(t, rule.SkipMonitoring)

	var count int
	require.NoError(t, tx.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM error_passthrough_rules
WHERE name = $1
`, ruleName).Scan(&count))
	require.Equal(t, 1, count)
}
