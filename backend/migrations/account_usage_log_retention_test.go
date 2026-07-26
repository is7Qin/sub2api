package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigration169PreservesUsageLogsOnAccountDelete(t *testing.T) {
	raw, err := FS.ReadFile("169_preserve_usage_logs_on_account_delete.sql")
	require.NoError(t, err)
	sql := strings.ToLower(string(raw))
	require.Contains(t, sql, "on delete set null")
	require.Contains(t, sql, "usage_logs_account_id_fkey")
	require.Contains(t, sql, "c.conrelid = usage_logs_oid")
	require.Contains(t, sql, "c.confrelid = accounts_oid")
	require.Contains(t, sql, "c.conkey = array[src.attnum]::smallint[]")
	require.Contains(t, sql, "c.confkey = array[ref.attnum]::smallint[]")
	require.Contains(t, sql, "with recursive usage_relations")
	require.Contains(t, sql, "not valid")
	require.Contains(t, sql, "validate constraint usage_logs_account_id_fkey")
	require.Contains(t, sql, "and not rel.relispartition")
	require.Contains(t, sql, "and rel.relispartition")
	require.Contains(t, sql, "and c.conparentid = 0")
	require.Contains(t, sql, "order by tree.depth desc")
	require.Contains(t, sql, "alter table %i.%i add constraint %i")
	require.NotContains(t, sql, "update usage_logs")
	require.NotContains(t, sql, "user_id drop not null")
	require.NotContains(t, sql, "api_key_id drop not null")
}
