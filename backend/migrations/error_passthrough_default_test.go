package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigration170DefinesOpenAICyberPolicyFallbackRule(t *testing.T) {
	content, err := FS.ReadFile("170_seed_openai_cyber_policy_passthrough.sql")
	require.NoError(t, err)

	sql := string(content)
	require.Contains(t, sql, "INSERT INTO error_passthrough_rules")
	require.Contains(t, sql, "'OpenAI cyber_policy fallback'")
	require.Contains(t, sql, "TRUE, 1000")
	require.Contains(t, sql, `'[]'::jsonb, '["cyber_policy"]'::jsonb, 'all', '["openai"]'::jsonb`)
	require.Contains(t, sql, "FALSE, 400, TRUE")
	require.Contains(t, sql, "WHERE NOT EXISTS")
}
