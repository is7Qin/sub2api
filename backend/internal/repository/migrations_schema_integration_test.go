//go:build integration

package repository

import (
	"context"
	"database/sql"
	"testing"

	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestMigrationsRunner_IsIdempotent_AndSchemaIsUpToDate(t *testing.T) {
	tx := testTx(t)

	// Re-apply migrations to verify idempotency (no errors, no duplicate rows).
	require.NoError(t, ApplyMigrations(context.Background(), integrationDB))

	// schema_migrations should have at least the current migration set.
	var applied int
	require.NoError(t, tx.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM schema_migrations").Scan(&applied))
	require.GreaterOrEqual(t, applied, 7, "expected schema_migrations to contain applied migrations")

	// users: columns required by repository queries
	requireColumn(t, tx, "users", "username", "character varying", 100, false)
	requireColumn(t, tx, "users", "notes", "text", 0, false)

	// accounts: schedulable and rate-limit fields
	requireColumn(t, tx, "accounts", "notes", "text", 0, true)
	requireColumn(t, tx, "accounts", "schedulable", "boolean", 0, false)
	requireColumn(t, tx, "accounts", "rate_limited_at", "timestamp with time zone", 0, true)
	requireColumn(t, tx, "accounts", "rate_limit_reset_at", "timestamp with time zone", 0, true)
	requireColumn(t, tx, "accounts", "overload_until", "timestamp with time zone", 0, true)
	requireColumn(t, tx, "accounts", "session_window_status", "character varying", 20, true)
	requireIndex(t, tx, "accounts", "idx_accounts_autopause_expiry_due")

	// api_keys: key length should be 128
	requireColumn(t, tx, "api_keys", "key", "character varying", 128, false)

	// redeem_codes: subscription fields
	requireColumn(t, tx, "redeem_codes", "group_id", "bigint", 0, true)
	requireColumn(t, tx, "redeem_codes", "validity_days", "integer", 0, false)

	// usage_logs: billing_type used by filters/stats
	requireColumn(t, tx, "usage_logs", "billing_type", "smallint", 0, false)
	requireColumn(t, tx, "usage_logs", "request_type", "smallint", 0, false)
	requireColumn(t, tx, "usage_logs", "openai_ws_mode", "boolean", 0, false)
	requireColumn(t, tx, "usage_logs", "image_input_size", "character varying", 32, true)
	requireColumn(t, tx, "usage_logs", "image_output_size", "character varying", 32, true)
	requireColumn(t, tx, "usage_logs", "image_size_source", "character varying", 16, true)
	requireColumn(t, tx, "usage_logs", "image_size_breakdown", "jsonb", 0, true)
	requireConstraintDefinitionContains(
		t,
		tx,
		"usage_logs",
		"usage_logs_image_size_source_check",
		"image_size_source",
		"'output'",
		"'input'",
		"'default'",
		"'legacy'",
	)
	requireConstraintDefinitionContains(
		t,
		tx,
		"usage_logs",
		"usage_logs_image_billing_size_check",
		"image_count",
		"image_size IS NOT NULL",
		"'1K'",
		"'2K'",
		"'4K'",
		"'mixed'",
	)

	// usage_billing_dedup: billing idempotency narrow table
	var usageBillingDedupRegclass sql.NullString
	require.NoError(t, tx.QueryRowContext(context.Background(), "SELECT to_regclass('public.usage_billing_dedup')").Scan(&usageBillingDedupRegclass))
	require.True(t, usageBillingDedupRegclass.Valid, "expected usage_billing_dedup table to exist")
	requireColumn(t, tx, "usage_billing_dedup", "request_fingerprint", "character varying", 64, false)
	requireIndex(t, tx, "usage_billing_dedup", "idx_usage_billing_dedup_request_api_key")
	requireIndex(t, tx, "usage_billing_dedup", "idx_usage_billing_dedup_created_at_brin")

	var usageBillingDedupArchiveRegclass sql.NullString
	require.NoError(t, tx.QueryRowContext(context.Background(), "SELECT to_regclass('public.usage_billing_dedup_archive')").Scan(&usageBillingDedupArchiveRegclass))
	require.True(t, usageBillingDedupArchiveRegclass.Valid, "expected usage_billing_dedup_archive table to exist")
	requireColumn(t, tx, "usage_billing_dedup_archive", "request_fingerprint", "character varying", 64, false)
	requireIndex(t, tx, "usage_billing_dedup_archive", "usage_billing_dedup_archive_pkey")

	// settings table should exist
	var settingsRegclass sql.NullString
	require.NoError(t, tx.QueryRowContext(context.Background(), "SELECT to_regclass('public.settings')").Scan(&settingsRegclass))
	require.True(t, settingsRegclass.Valid, "expected settings table to exist")

	// security_secrets table should exist
	var securitySecretsRegclass sql.NullString
	require.NoError(t, tx.QueryRowContext(context.Background(), "SELECT to_regclass('public.security_secrets')").Scan(&securitySecretsRegclass))
	require.True(t, securitySecretsRegclass.Valid, "expected security_secrets table to exist")

	// scheduler_outbox pending dedup support
	requireColumn(t, tx, "scheduler_outbox", "dedup_key", "text", 0, true)
	requireIndex(t, tx, "scheduler_outbox", "idx_scheduler_outbox_pending_dedup_key")

	// user_allowed_groups table should exist
	var uagRegclass sql.NullString
	require.NoError(t, tx.QueryRowContext(context.Background(), "SELECT to_regclass('public.user_allowed_groups')").Scan(&uagRegclass))
	require.True(t, uagRegclass.Valid, "expected user_allowed_groups table to exist")

	// user_subscriptions: deleted_at for soft delete support (migration 012)
	requireColumn(t, tx, "user_subscriptions", "deleted_at", "timestamp with time zone", 0, true)

	// orphan_allowed_groups_audit table should exist (migration 013)
	var orphanAuditRegclass sql.NullString
	require.NoError(t, tx.QueryRowContext(context.Background(), "SELECT to_regclass('public.orphan_allowed_groups_audit')").Scan(&orphanAuditRegclass))
	require.True(t, orphanAuditRegclass.Valid, "expected orphan_allowed_groups_audit table to exist")

	// account_groups: created_at should be timestamptz
	requireColumn(t, tx, "account_groups", "created_at", "timestamp with time zone", 0, false)

	// user_allowed_groups: created_at should be timestamptz
	requireColumn(t, tx, "user_allowed_groups", "created_at", "timestamp with time zone", 0, false)
}

func TestMigration172_AdminQuotaLoweringEnqueuesGenericAuthInvalidation(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	content, err := migrations.FS.ReadFile("172_billing_quota_auth_invalidation.sql")
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, string(content))
	require.NoError(t, err)

	var userID int64
	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO users (email, password_hash, role, status, balance, concurrency)
VALUES ($1, 'hash', 'user', 'active', 0, 1)
RETURNING id
`, "migration-172-quota-lowering@example.com").Scan(&userID))

	var apiKeyID int64
	const apiKey = "sk-migration-172-quota-lowering"
	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO api_keys (user_id, key, name, quota, quota_used)
VALUES ($1, $2, 'migration-172-quota-lowering', 100, 75)
RETURNING id
`, userID, apiKey).Scan(&apiKeyID))

	_, err = tx.ExecContext(ctx, "UPDATE api_keys SET quota = 50 WHERE id = $1", apiKeyID)
	require.NoError(t, err)

	var count int
	require.NoError(t, tx.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM auth_cache_invalidation_outbox
WHERE cache_key = encode(sha256(convert_to($1, 'UTF8')), 'hex')
  AND source_key IS NULL
`, apiKey).Scan(&count))
	require.Equal(t, 1, count)
}

func TestMigrationsRunner_UsageLogAccountRelationship(t *testing.T) {
	tx := testTx(t)
	content, err := migrations.FS.ReadFile("169_preserve_usage_logs_on_account_delete.sql")
	require.NoError(t, err)
	_, err = tx.ExecContext(context.Background(), string(content))
	require.NoError(t, err)

	requireColumn(t, tx, "usage_logs", "account_id", "bigint", 0, true)
	requireColumn(t, tx, "usage_logs", "user_id", "bigint", 0, false)
	requireColumn(t, tx, "usage_logs", "api_key_id", "bigint", 0, false)
	requireForeignKeyOnDelete(t, tx, "usage_logs", "account_id", "accounts", "SET NULL")
	requireForeignKeyOnDelete(t, tx, "usage_logs", "user_id", "users", "CASCADE")
	requireForeignKeyOnDelete(t, tx, "usage_logs", "api_key_id", "api_keys", "CASCADE")
	requireSingleUsageLogAccountForeignKey(t, tx, "usage_logs_account_id_fkey", true)
}

func TestMigrationUsageLogAccount_NoncanonicalNameAndRerun(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	content, err := migrations.FS.ReadFile("169_preserve_usage_logs_on_account_delete.sql")
	require.NoError(t, err)

	_, err = tx.ExecContext(ctx, `
ALTER TABLE public.usage_logs DROP CONSTRAINT usage_logs_account_id_fkey;
ALTER TABLE public.usage_logs
    ADD CONSTRAINT historical_custom_account_fk
    FOREIGN KEY (account_id) REFERENCES public.accounts(id) ON DELETE CASCADE
`)
	require.NoError(t, err)

	_, err = tx.ExecContext(ctx, string(content))
	require.NoError(t, err, "replace noncanonical account FK")
	_, err = tx.ExecContext(ctx, string(content))
	require.NoError(t, err, "migration SQL must be rerunnable")

	requireSingleUsageLogAccountForeignKey(t, tx, "usage_logs_account_id_fkey", true)
	requireForeignKeyOnDelete(t, tx, "usage_logs", "user_id", "users", "CASCADE")
	requireForeignKeyOnDelete(t, tx, "usage_logs", "api_key_id", "api_keys", "CASCADE")
}

func TestMigrationUsageLogAccount_OrdinaryInheritance(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	content, err := migrations.FS.ReadFile("169_preserve_usage_logs_on_account_delete.sql")
	require.NoError(t, err)

	_, err = tx.ExecContext(ctx, `
ALTER TABLE public.usage_logs RENAME TO usage_logs_before_inheritance_test;
CREATE TABLE public.usage_logs (
    id BIGINT NOT NULL,
    account_id BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);
CREATE TABLE public.usage_logs_inheritance_test () INHERITS (public.usage_logs);
ALTER TABLE public.usage_logs
    ADD CONSTRAINT root_custom_account_fk
    FOREIGN KEY (account_id) REFERENCES public.accounts(id) ON DELETE CASCADE;
ALTER TABLE public.usage_logs_inheritance_test
    ADD CONSTRAINT child_custom_account_fk
    FOREIGN KEY (account_id) REFERENCES public.accounts(id) ON DELETE CASCADE;
`)
	require.NoError(t, err)

	_, err = tx.ExecContext(ctx, string(content))
	require.NoError(t, err, "apply migration to ordinary INHERITS layout")
	assertSingleForeignKeyOnDelete(t, tx, "usage_logs", "account_id", "accounts", "usage_logs_account_id_fkey", "SET NULL", true)
	assertSingleForeignKeyOnDelete(t, tx, "usage_logs_inheritance_test", "account_id", "accounts", "usage_logs_account_id_fkey", "SET NULL", true)
	assertStandaloneForeignKey(t, tx, "usage_logs_inheritance_test", "usage_logs_account_id_fkey")

	_, err = tx.ExecContext(ctx, string(content))
	require.NoError(t, err, "ordinary INHERITS migration must be rerunnable")
	assertSingleForeignKeyOnDelete(t, tx, "usage_logs", "account_id", "accounts", "usage_logs_account_id_fkey", "SET NULL", true)
	assertSingleForeignKeyOnDelete(t, tx, "usage_logs_inheritance_test", "account_id", "accounts", "usage_logs_account_id_fkey", "SET NULL", true)
	assertStandaloneForeignKey(t, tx, "usage_logs_inheritance_test", "usage_logs_account_id_fkey")

	var accountID int64
	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO public.accounts (name, platform, type, credentials, extra, concurrency, priority, status, schedulable)
VALUES ('inheritance-retention', 'anthropic', 'oauth', '{}', '{}', 1, 1, 'active', TRUE)
RETURNING id
`).Scan(&accountID))
	_, err = tx.ExecContext(ctx, `
INSERT INTO public.usage_logs_inheritance_test (id, account_id, created_at)
VALUES (1, $1, '2026-07-26')
`, accountID)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, "DELETE FROM public.accounts WHERE id = $1", accountID)
	require.NoError(t, err)

	var retainedAccountID sql.NullInt64
	require.NoError(t, tx.QueryRowContext(ctx, "SELECT account_id FROM ONLY public.usage_logs_inheritance_test WHERE id = 1").Scan(&retainedAccountID))
	require.False(t, retainedAccountID.Valid, "ordinary inherited child row must survive with NULL account_id")
}

func assertStandaloneForeignKey(t *testing.T, tx *sql.Tx, table, constraint string) {
	t.Helper()

	var parentOID int64
	err := tx.QueryRowContext(context.Background(), `
SELECT c.conparentid
FROM pg_constraint c
JOIN pg_class tbl ON tbl.oid = c.conrelid
JOIN pg_namespace ns ON ns.oid = tbl.relnamespace
WHERE ns.nspname = 'public' AND tbl.relname = $1 AND c.conname = $2
`, table, constraint).Scan(&parentOID)
	require.NoError(t, err)
	require.Zero(t, parentOID, "ordinary inheritance descendants need their own FK, not a partition clone")
}

func TestMigrationUsageLogAccount_PartitionedLayout(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	content, err := migrations.FS.ReadFile("169_preserve_usage_logs_on_account_delete.sql")
	require.NoError(t, err)

	_, err = tx.ExecContext(ctx, `
ALTER TABLE public.usage_logs RENAME TO usage_logs_before_partition_test;
CREATE TABLE public.usage_logs (
    id BIGINT NOT NULL,
    account_id BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
) PARTITION BY RANGE (created_at);
CREATE TABLE public.usage_logs_partition_test
    PARTITION OF public.usage_logs
    FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
ALTER TABLE public.usage_logs
    ADD CONSTRAINT partition_custom_account_fk
    FOREIGN KEY (account_id) REFERENCES public.accounts(id) ON DELETE CASCADE;
`)
	require.NoError(t, err)

	_, err = tx.ExecContext(ctx, string(content))
	require.NoError(t, err, "apply migration to a partitioned usage_logs parent")

	for _, table := range []string{"usage_logs", "usage_logs_partition_test"} {
		var nullable string
		require.NoError(t, tx.QueryRowContext(ctx, `
SELECT is_nullable
FROM information_schema.columns
WHERE table_schema = 'public' AND table_name = $1 AND column_name = 'account_id'
`, table).Scan(&nullable))
		require.Equal(t, "YES", nullable, "%s.account_id must be nullable", table)
		requireForeignKeyOnDelete(t, tx, table, "account_id", "accounts", "SET NULL")
	}
	requireSingleUsageLogAccountForeignKey(t, tx, "usage_logs_account_id_fkey", true)

	var accountID int64
	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO public.accounts (name, platform, type, credentials, extra, concurrency, priority, status, schedulable)
VALUES ('partition-retention', 'anthropic', 'oauth', '{}', '{}', 1, 1, 'active', TRUE)
RETURNING id
`).Scan(&accountID))
	_, err = tx.ExecContext(ctx, `
INSERT INTO public.usage_logs (id, account_id, created_at)
VALUES (1, $1, '2026-07-26')
`, accountID)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, "DELETE FROM public.accounts WHERE id = $1", accountID)
	require.NoError(t, err)

	var retainedAccountID sql.NullInt64
	require.NoError(t, tx.QueryRowContext(ctx, "SELECT account_id FROM public.usage_logs WHERE id = 1").Scan(&retainedAccountID))
	require.False(t, retainedAccountID.Valid, "partition row must survive account deletion with NULL account_id")
}

func TestMigrationUsageLogAccount_PartitionStandaloneForeignKey(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	content, err := migrations.FS.ReadFile("169_preserve_usage_logs_on_account_delete.sql")
	require.NoError(t, err)

	_, err = tx.ExecContext(ctx, `
ALTER TABLE public.usage_logs RENAME TO usage_logs_before_partition_standalone_test;
CREATE TABLE public.usage_logs (
    id BIGINT NOT NULL,
    account_id BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
) PARTITION BY RANGE (created_at);
CREATE TABLE public.usage_logs_partition_standalone_test
    PARTITION OF public.usage_logs
    FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
ALTER TABLE public.usage_logs
    ADD CONSTRAINT usage_logs_account_id_fkey
    FOREIGN KEY (account_id) REFERENCES public.accounts(id) ON DELETE SET NULL;
ALTER TABLE public.usage_logs_partition_standalone_test
    ADD CONSTRAINT legacy_partition_account_fk
    FOREIGN KEY (account_id) REFERENCES public.accounts(id) ON DELETE CASCADE;
`)
	require.NoError(t, err)

	_, err = tx.ExecContext(ctx, string(content))
	require.NoError(t, err, "remove standalone exact account FK from partition")
	assertPartitionHasOnlyInheritedAccountForeignKey(t, tx, "usage_logs_partition_standalone_test")

	_, err = tx.ExecContext(ctx, string(content))
	require.NoError(t, err, "partition standalone cleanup must be rerunnable")
	assertPartitionHasOnlyInheritedAccountForeignKey(t, tx, "usage_logs_partition_standalone_test")

	var accountID int64
	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO public.accounts (name, platform, type, credentials, extra, concurrency, priority, status, schedulable)
VALUES ('partition-standalone-retention', 'anthropic', 'oauth', '{}', '{}', 1, 1, 'active', TRUE)
RETURNING id
`).Scan(&accountID))
	_, err = tx.ExecContext(ctx, `
INSERT INTO public.usage_logs (id, account_id, created_at)
VALUES (1, $1, '2026-07-26')
`, accountID)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, "DELETE FROM public.accounts WHERE id = $1", accountID)
	require.NoError(t, err)

	var retainedAccountID sql.NullInt64
	require.NoError(t, tx.QueryRowContext(ctx, "SELECT account_id FROM public.usage_logs WHERE id = 1").Scan(&retainedAccountID))
	require.False(t, retainedAccountID.Valid, "partition row must survive after standalone CASCADE FK cleanup")
}

func assertPartitionHasOnlyInheritedAccountForeignKey(t *testing.T, tx *sql.Tx, table string) {
	t.Helper()

	var count, standaloneCount int
	var names, actions string
	var validated, inherited bool
	err := tx.QueryRowContext(context.Background(), `
SELECT
    COUNT(*),
    COUNT(*) FILTER (WHERE c.conparentid = 0),
    COALESCE(array_to_string(array_agg(c.conname ORDER BY c.conname), ','), ''),
    COALESCE(array_to_string(array_agg(DISTINCT CASE c.confdeltype
        WHEN 'a' THEN 'NO ACTION'
        WHEN 'r' THEN 'RESTRICT'
        WHEN 'c' THEN 'CASCADE'
        WHEN 'n' THEN 'SET NULL'
        WHEN 'd' THEN 'SET DEFAULT'
    END), ','), ''),
    COALESCE(BOOL_AND(c.convalidated), FALSE),
    COALESCE(BOOL_AND(c.conparentid <> 0), FALSE)
FROM pg_constraint c
JOIN pg_class tbl ON tbl.oid = c.conrelid
JOIN pg_namespace ns ON ns.oid = tbl.relnamespace
JOIN pg_class ref_tbl ON ref_tbl.oid = c.confrelid
JOIN pg_attribute src ON src.attrelid = tbl.oid AND src.attname = 'account_id'
JOIN pg_attribute ref ON ref.attrelid = ref_tbl.oid AND ref.attname = 'id'
WHERE ns.nspname = 'public'
  AND tbl.relname = $1
  AND c.contype = 'f'
  AND c.conkey = ARRAY[src.attnum]::smallint[]
  AND c.confkey = ARRAY[ref.attnum]::smallint[]
  AND ref_tbl.relname = 'accounts'
`, table).Scan(&count, &standaloneCount, &names, &actions, &validated, &inherited)
	require.NoError(t, err)
	require.Equal(t, 1, count, "expected exactly one inherited account FK; names=%q actions=%q", names, actions)
	require.Zero(t, standaloneCount, "partition must not retain an exact standalone account FK")
	require.Equal(t, "usage_logs_account_id_fkey", names)
	require.Equal(t, "SET NULL", actions)
	require.True(t, validated)
	require.True(t, inherited)
}

func requireSingleUsageLogAccountForeignKey(t *testing.T, tx *sql.Tx, expectedName string, expectedValidated bool) {
	t.Helper()
	assertSingleForeignKeyOnDelete(t, tx, "usage_logs", "account_id", "accounts", expectedName, "SET NULL", expectedValidated)
}

func assertSingleForeignKeyOnDelete(t *testing.T, tx *sql.Tx, table, column, refTable, expectedName, expectedAction string, expectedValidated bool) {
	t.Helper()

	var name, actions string
	var validated bool
	var count int
	err := tx.QueryRowContext(context.Background(), `
SELECT
    COALESCE(MIN(c.conname), ''),
    COALESCE(array_to_string(array_agg(DISTINCT CASE c.confdeltype
        WHEN 'a' THEN 'NO ACTION'
        WHEN 'r' THEN 'RESTRICT'
        WHEN 'c' THEN 'CASCADE'
        WHEN 'n' THEN 'SET NULL'
        WHEN 'd' THEN 'SET DEFAULT'
    END), ','), ''),
    COALESCE(BOOL_AND(c.convalidated), FALSE),
    COUNT(*)
FROM pg_constraint c
JOIN pg_class tbl ON tbl.oid = c.conrelid
JOIN pg_namespace ns ON ns.oid = tbl.relnamespace
JOIN pg_class ref_tbl ON ref_tbl.oid = c.confrelid
JOIN pg_attribute src ON src.attrelid = tbl.oid AND src.attname = $2
JOIN pg_attribute ref ON ref.attrelid = ref_tbl.oid AND ref.attname = 'id'
WHERE ns.nspname = 'public'
  AND tbl.relname = $1
  AND c.contype = 'f'
  AND c.conkey = ARRAY[src.attnum]::smallint[]
  AND c.confkey = ARRAY[ref.attnum]::smallint[]
  AND ref_tbl.relname = $3
`, table, column, refTable).Scan(&name, &actions, &validated, &count)
	require.NoError(t, err)
	require.Equal(t, 1, count, "expected exactly one FK for %s.%s -> %s.id; complete actions=%q", table, column, refTable, actions)
	require.Equal(t, expectedName, name)
	require.Equal(t, expectedAction, actions)
	require.Equal(t, expectedValidated, validated)
}

func TestMigrationsRunner_AuthIdentityAndPaymentSchemaStayAligned(t *testing.T) {
	tx := testTx(t)

	requireColumn(t, tx, "auth_identity_migration_reports", "report_type", "character varying", 80, false)
	requireColumn(t, tx, "users", "signup_source", "character varying", 20, false)
	requireColumnDefaultContains(t, tx, "users", "signup_source", "email")
	requireConstraintDefinitionContains(
		t,
		tx,
		"users",
		"users_signup_source_check",
		"signup_source",
		"'email'",
		"'linuxdo'",
		"'wechat'",
		"'oidc'",
	)

	requireForeignKeyOnDelete(t, tx, "auth_identities", "user_id", "users", "CASCADE")
	requireForeignKeyOnDelete(t, tx, "auth_identity_channels", "identity_id", "auth_identities", "CASCADE")
	requireForeignKeyOnDelete(t, tx, "pending_auth_sessions", "target_user_id", "users", "SET NULL")
	requireForeignKeyOnDelete(t, tx, "identity_adoption_decisions", "pending_auth_session_id", "pending_auth_sessions", "CASCADE")
	requireForeignKeyOnDelete(t, tx, "identity_adoption_decisions", "identity_id", "auth_identities", "SET NULL")

	requireIndex(t, tx, "payment_orders", "paymentorder_out_trade_no")
	requirePartialUniqueIndexDefinition(t, tx, "payment_orders", "paymentorder_out_trade_no", "out_trade_no", "WHERE")
	requireIndexAbsent(t, tx, "payment_orders", "paymentorder_out_trade_no_unique")
}

func requireIndex(t *testing.T, tx *sql.Tx, table, index string) {
	t.Helper()

	var exists bool
	err := tx.QueryRowContext(context.Background(), `
SELECT EXISTS (
	SELECT 1
	FROM pg_indexes
	WHERE schemaname = 'public'
	  AND tablename = $1
	  AND indexname = $2
)
`, table, index).Scan(&exists)
	require.NoError(t, err, "query pg_indexes for %s.%s", table, index)
	require.True(t, exists, "expected index %s on %s", index, table)
}

func requireIndexAbsent(t *testing.T, tx *sql.Tx, table, index string) {
	t.Helper()

	var exists bool
	err := tx.QueryRowContext(context.Background(), `
SELECT EXISTS (
	SELECT 1
	FROM pg_indexes
	WHERE schemaname = 'public'
	  AND tablename = $1
	  AND indexname = $2
)
`, table, index).Scan(&exists)
	require.NoError(t, err, "query pg_indexes for %s.%s", table, index)
	require.False(t, exists, "expected index %s on %s to be absent", index, table)
}

func requirePartialUniqueIndexDefinition(t *testing.T, tx *sql.Tx, table, index string, fragments ...string) {
	t.Helper()

	var (
		unique bool
		def    string
	)

	err := tx.QueryRowContext(context.Background(), `
SELECT
	i.indisunique,
	pg_get_indexdef(i.indexrelid)
FROM pg_class idx
JOIN pg_index i ON i.indexrelid = idx.oid
JOIN pg_class tbl ON tbl.oid = i.indrelid
JOIN pg_namespace ns ON ns.oid = tbl.relnamespace
WHERE ns.nspname = 'public'
  AND tbl.relname = $1
  AND idx.relname = $2
`, table, index).Scan(&unique, &def)
	require.NoError(t, err, "query index definition for %s.%s", table, index)
	require.True(t, unique, "expected index %s on %s to be unique", index, table)

	for _, fragment := range fragments {
		require.Contains(t, def, fragment, "expected index definition for %s.%s to contain %q", table, index, fragment)
	}
}

func requireForeignKeyOnDelete(t *testing.T, tx *sql.Tx, table, column, refTable, expected string) {
	t.Helper()

	var actions string
	var count int
	err := tx.QueryRowContext(context.Background(), `
SELECT
    COALESCE(array_to_string(array_agg(DISTINCT CASE c.confdeltype
        WHEN 'a' THEN 'NO ACTION'
        WHEN 'r' THEN 'RESTRICT'
        WHEN 'c' THEN 'CASCADE'
        WHEN 'n' THEN 'SET NULL'
        WHEN 'd' THEN 'SET DEFAULT'
    END), ','), ''),
    COUNT(*)
FROM pg_constraint c
JOIN pg_class tbl ON tbl.oid = c.conrelid
JOIN pg_namespace ns ON ns.oid = tbl.relnamespace
JOIN pg_class ref_tbl ON ref_tbl.oid = c.confrelid
JOIN pg_attribute src ON src.attrelid = tbl.oid AND src.attname = $2
JOIN pg_attribute ref ON ref.attrelid = ref_tbl.oid AND ref.attname = 'id'
WHERE ns.nspname = 'public'
  AND c.contype = 'f'
  AND tbl.relname = $1
  AND c.conkey = ARRAY[src.attnum]::smallint[]
  AND c.confkey = ARRAY[ref.attnum]::smallint[]
  AND ref_tbl.relname = $3
`, table, column, refTable).Scan(&actions, &count)
	require.NoError(t, err, "query foreign key actions for %s.%s -> %s", table, column, refTable)
	require.Equal(t, 1, count, "expected exactly one FK for %s.%s -> %s.id; complete actions=%q", table, column, refTable, actions)
	require.Equal(t, expected, actions, "unexpected complete ON DELETE action set for %s.%s -> %s", table, column, refTable)
}

func requireConstraintDefinitionContains(t *testing.T, tx *sql.Tx, table, constraint string, fragments ...string) {
	t.Helper()

	var def string
	err := tx.QueryRowContext(context.Background(), `
SELECT pg_get_constraintdef(c.oid)
FROM pg_constraint c
JOIN pg_class tbl ON tbl.oid = c.conrelid
JOIN pg_namespace ns ON ns.oid = tbl.relnamespace
WHERE ns.nspname = 'public'
  AND tbl.relname = $1
  AND c.conname = $2
`, table, constraint).Scan(&def)
	require.NoError(t, err, "query constraint definition for %s.%s", table, constraint)

	for _, fragment := range fragments {
		require.Contains(t, def, fragment, "expected constraint definition for %s.%s to contain %q", table, constraint, fragment)
	}
}

func requireColumnDefaultContains(t *testing.T, tx *sql.Tx, table, column string, fragments ...string) {
	t.Helper()

	var columnDefault sql.NullString
	err := tx.QueryRowContext(context.Background(), `
SELECT column_default
FROM information_schema.columns
WHERE table_schema = 'public'
  AND table_name = $1
  AND column_name = $2
`, table, column).Scan(&columnDefault)
	require.NoError(t, err, "query column_default for %s.%s", table, column)
	require.True(t, columnDefault.Valid, "expected column_default for %s.%s", table, column)

	for _, fragment := range fragments {
		require.Contains(t, columnDefault.String, fragment, "expected default for %s.%s to contain %q", table, column, fragment)
	}
}

func requireColumn(t *testing.T, tx *sql.Tx, table, column, dataType string, maxLen int, nullable bool) {
	t.Helper()

	var row struct {
		DataType string
		MaxLen   sql.NullInt64
		Nullable string
	}

	err := tx.QueryRowContext(context.Background(), `
SELECT
  data_type,
  character_maximum_length,
  is_nullable
FROM information_schema.columns
WHERE table_schema = 'public'
  AND table_name = $1
  AND column_name = $2
`, table, column).Scan(&row.DataType, &row.MaxLen, &row.Nullable)
	require.NoError(t, err, "query information_schema.columns for %s.%s", table, column)
	require.Equal(t, dataType, row.DataType, "data_type mismatch for %s.%s", table, column)

	if maxLen > 0 {
		require.True(t, row.MaxLen.Valid, "expected maxLen for %s.%s", table, column)
		require.Equal(t, int64(maxLen), row.MaxLen.Int64, "maxLen mismatch for %s.%s", table, column)
	}

	if nullable {
		require.Equal(t, "YES", row.Nullable, "nullable mismatch for %s.%s", table, column)
	} else {
		require.Equal(t, "NO", row.Nullable, "nullable mismatch for %s.%s", table, column)
	}
}
