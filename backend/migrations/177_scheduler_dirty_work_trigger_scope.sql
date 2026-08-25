-- Fixed-point scoping of the accounts dirty-source comparison: the trigger now
-- fires only when a scheduling-relevant field changes. 164/165 previously
-- compared the whole extra payload (projection over every row) and kept unknown
-- keys conservatively dirty; that still paid O(extra) on every accounts UPDATE
-- and could not distinguish scheduling inputs from payload cargo.
--
-- The comparison below is an explicit whitelist:
--   * top-level columns consumed by the scheduler cache payload (SetAccount /
--     buildSchedulerMetadataAccount) — including the runtime overlay columns
--     (rate_limited_at, overload_until, session_window_*, ...) which the hot
--     path publishes separately but which the scheduler also consumes;
--   * extra keys read by scheduling decisions (account.go getExtra*/Extra[..]
--     call sites, gateway eligibility/routing, upstream capability resolution).
--   * last_used_at and the runtime extra keys (codex usage snapshots, rate-limit
--     models, fingerprint, session utilization, passive usage) are excluded by
--     design: they are published via hot paths / the outbox worker.
--
-- Unknown extra keys no longer dirty (whitelist-only contract). Per the
-- over-inclusion rule, keys with any payload-consumer ambiguity are listed.
--
-- Keys are compared with `->` (jsonb), not `->>` (text): `->>` returns NULL for
-- object-valued keys (e.g. openai_responses_supported_by_model) which would
-- silently never dirty.
CREATE OR REPLACE FUNCTION scheduler_accounts_source_from_update()
RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public
AS $$
BEGIN
    INSERT INTO public.scheduler_dirty_account_sources (account_id, bucket_dirty)
    SELECT
        COALESCE(n.id, o.id),
        CASE
            WHEN n.id IS NULL OR o.id IS NULL THEN true
            ELSE
                o.platform IS DISTINCT FROM n.platform OR
                o.priority IS DISTINCT FROM n.priority OR
                o.status IS DISTINCT FROM n.status OR
                o.expires_at IS DISTINCT FROM n.expires_at OR
                o.schedulable IS DISTINCT FROM n.schedulable OR
                o.deleted_at IS DISTINCT FROM n.deleted_at OR
                o.extra -> 'mixed_scheduling' IS DISTINCT FROM n.extra -> 'mixed_scheduling'
        END
    FROM new_rows AS n
    FULL JOIN old_rows AS o USING (id)
    WHERE
        n.id IS NULL OR
        o.id IS NULL OR
        -- 调度缓存消费的顶层列（SetAccount 全量 + meta 子集）。
        o.name IS DISTINCT FROM n.name OR
        o.platform IS DISTINCT FROM n.platform OR
        o.type IS DISTINCT FROM n.type OR
        o.credentials IS DISTINCT FROM n.credentials OR
        o.proxy_id IS DISTINCT FROM n.proxy_id OR
        o.concurrency IS DISTINCT FROM n.concurrency OR
        o.load_factor IS DISTINCT FROM n.load_factor OR
        o.priority IS DISTINCT FROM n.priority OR
        o.rate_multiplier IS DISTINCT FROM n.rate_multiplier OR
        o.status IS DISTINCT FROM n.status OR
        o.expires_at IS DISTINCT FROM n.expires_at OR
        o.auto_pause_on_expired IS DISTINCT FROM n.auto_pause_on_expired OR
        o.schedulable IS DISTINCT FROM n.schedulable OR
        o.rate_limited_at IS DISTINCT FROM n.rate_limited_at OR
        o.rate_limit_reset_at IS DISTINCT FROM n.rate_limit_reset_at OR
        o.overload_until IS DISTINCT FROM n.overload_until OR
        o.temp_unschedulable_until IS DISTINCT FROM n.temp_unschedulable_until OR
        o.temp_unschedulable_reason IS DISTINCT FROM n.temp_unschedulable_reason OR
        o.session_window_start IS DISTINCT FROM n.session_window_start OR
        o.session_window_end IS DISTINCT FROM n.session_window_end OR
        o.session_window_status IS DISTINCT FROM n.session_window_status OR
        o.deleted_at IS DISTINCT FROM n.deleted_at OR
        -- extra 定点比较：仅白名单 key（调度判定实际读取；宁可多列不可漏）。
        o.extra -> 'privacy_mode' IS DISTINCT FROM n.extra -> 'privacy_mode' OR
        o.extra -> 'claude_user_id' IS DISTINCT FROM n.extra -> 'claude_user_id' OR
        o.extra -> 'mixed_scheduling' IS DISTINCT FROM n.extra -> 'mixed_scheduling' OR
        o.extra -> 'allow_overages' IS DISTINCT FROM n.extra -> 'allow_overages' OR
        o.extra -> 'openai_passthrough' IS DISTINCT FROM n.extra -> 'openai_passthrough' OR
        o.extra -> 'openai_oauth_ws_mode' IS DISTINCT FROM n.extra -> 'openai_oauth_ws_mode' OR
        o.extra -> 'openai_apikey_responses_websockets_v2_enabled' IS DISTINCT FROM n.extra -> 'openai_apikey_responses_websockets_v2_enabled' OR
        o.extra -> 'openai_apikey_responses_websockets_v2_mode' IS DISTINCT FROM n.extra -> 'openai_apikey_responses_websockets_v2_mode' OR
        o.extra -> 'responses_websockets_v2_enabled' IS DISTINCT FROM n.extra -> 'responses_websockets_v2_enabled' OR
        o.extra -> 'openai_ws_enabled' IS DISTINCT FROM n.extra -> 'openai_ws_enabled' OR
        o.extra -> 'openai_ws_force_http' IS DISTINCT FROM n.extra -> 'openai_ws_force_http' OR
        o.extra -> 'openai_ws_allow_store_recovery' IS DISTINCT FROM n.extra -> 'openai_ws_allow_store_recovery' OR
        o.extra -> 'anthropic_passthrough' IS DISTINCT FROM n.extra -> 'anthropic_passthrough' OR
        o.extra -> 'web_search_emulation' IS DISTINCT FROM n.extra -> 'web_search_emulation' OR
        o.extra -> 'codex_cli_only' IS DISTINCT FROM n.extra -> 'codex_cli_only' OR
        o.extra -> 'codex_cli_only_allowed_clients' IS DISTINCT FROM n.extra -> 'codex_cli_only_allowed_clients' OR
        o.extra -> 'enable_tls_fingerprint' IS DISTINCT FROM n.extra -> 'enable_tls_fingerprint' OR
        o.extra -> 'tls_fingerprint_profile_id' IS DISTINCT FROM n.extra -> 'tls_fingerprint_profile_id' OR
        o.extra -> 'user_msg_queue_mode' IS DISTINCT FROM n.extra -> 'user_msg_queue_mode' OR
        o.extra -> 'user_msg_queue_enabled' IS DISTINCT FROM n.extra -> 'user_msg_queue_enabled' OR
        o.extra -> 'session_id_masking_enabled' IS DISTINCT FROM n.extra -> 'session_id_masking_enabled' OR
        o.extra -> 'custom_base_url_enabled' IS DISTINCT FROM n.extra -> 'custom_base_url_enabled' OR
        o.extra -> 'cache_ttl_override_enabled' IS DISTINCT FROM n.extra -> 'cache_ttl_override_enabled' OR
        o.extra -> 'cache_ttl_override_target' IS DISTINCT FROM n.extra -> 'cache_ttl_override_target' OR
        o.extra -> 'openai_compact_mode' IS DISTINCT FROM n.extra -> 'openai_compact_mode' OR
        o.extra -> 'openai_compact_supported' IS DISTINCT FROM n.extra -> 'openai_compact_supported' OR
        o.extra -> 'openai_responses_mode' IS DISTINCT FROM n.extra -> 'openai_responses_mode' OR
        o.extra -> 'openai_responses_supported' IS DISTINCT FROM n.extra -> 'openai_responses_supported' OR
        o.extra -> 'openai_responses_supported_by_model' IS DISTINCT FROM n.extra -> 'openai_responses_supported_by_model' OR
        o.extra -> 'openai_oauth_responses_websockets_v2_enabled' IS DISTINCT FROM n.extra -> 'openai_oauth_responses_websockets_v2_enabled' OR
        o.extra -> 'openai_oauth_responses_websockets_v2_mode' IS DISTINCT FROM n.extra -> 'openai_oauth_responses_websockets_v2_mode' OR
        o.extra -> 'quota_limit' IS DISTINCT FROM n.extra -> 'quota_limit' OR
        o.extra -> 'quota_used' IS DISTINCT FROM n.extra -> 'quota_used' OR
        o.extra -> 'quota_daily_limit' IS DISTINCT FROM n.extra -> 'quota_daily_limit' OR
        o.extra -> 'quota_daily_used' IS DISTINCT FROM n.extra -> 'quota_daily_used' OR
        o.extra -> 'quota_daily_start' IS DISTINCT FROM n.extra -> 'quota_daily_start' OR
        o.extra -> 'quota_daily_reset_mode' IS DISTINCT FROM n.extra -> 'quota_daily_reset_mode' OR
        o.extra -> 'quota_daily_reset_hour' IS DISTINCT FROM n.extra -> 'quota_daily_reset_hour' OR
        o.extra -> 'quota_daily_reset_at' IS DISTINCT FROM n.extra -> 'quota_daily_reset_at' OR
        o.extra -> 'quota_weekly_limit' IS DISTINCT FROM n.extra -> 'quota_weekly_limit' OR
        o.extra -> 'quota_weekly_used' IS DISTINCT FROM n.extra -> 'quota_weekly_used' OR
        o.extra -> 'quota_weekly_start' IS DISTINCT FROM n.extra -> 'quota_weekly_start' OR
        o.extra -> 'quota_weekly_reset_mode' IS DISTINCT FROM n.extra -> 'quota_weekly_reset_mode' OR
        o.extra -> 'quota_weekly_reset_day' IS DISTINCT FROM n.extra -> 'quota_weekly_reset_day' OR
        o.extra -> 'quota_weekly_reset_hour' IS DISTINCT FROM n.extra -> 'quota_weekly_reset_hour' OR
        o.extra -> 'quota_weekly_reset_at' IS DISTINCT FROM n.extra -> 'quota_weekly_reset_at' OR
        o.extra -> 'quota_reset_timezone' IS DISTINCT FROM n.extra -> 'quota_reset_timezone' OR
        o.extra -> 'window_cost_limit' IS DISTINCT FROM n.extra -> 'window_cost_limit' OR
        o.extra -> 'window_cost_sticky_reserve' IS DISTINCT FROM n.extra -> 'window_cost_sticky_reserve' OR
        o.extra -> 'max_sessions' IS DISTINCT FROM n.extra -> 'max_sessions' OR
        o.extra -> 'session_idle_timeout_minutes' IS DISTINCT FROM n.extra -> 'session_idle_timeout_minutes' OR
        o.extra -> 'base_rpm' IS DISTINCT FROM n.extra -> 'base_rpm' OR
        o.extra -> 'rpm_strategy' IS DISTINCT FROM n.extra -> 'rpm_strategy' OR
        o.extra -> 'rpm_sticky_buffer' IS DISTINCT FROM n.extra -> 'rpm_sticky_buffer' OR
        o.extra -> 'drive_tier_updated_at' IS DISTINCT FROM n.extra -> 'drive_tier_updated_at' OR
        o.extra -> 'drive_storage_limit' IS DISTINCT FROM n.extra -> 'drive_storage_limit' OR
        o.extra -> 'drive_storage_usage' IS DISTINCT FROM n.extra -> 'drive_storage_usage' OR
        o.extra -> 'auto_pause_5h_threshold' IS DISTINCT FROM n.extra -> 'auto_pause_5h_threshold' OR
        o.extra -> 'auto_pause_7d_threshold' IS DISTINCT FROM n.extra -> 'auto_pause_7d_threshold' OR
        o.extra -> 'auto_pause_5h_disabled' IS DISTINCT FROM n.extra -> 'auto_pause_5h_disabled' OR
        o.extra -> 'auto_pause_7d_disabled' IS DISTINCT FROM n.extra -> 'auto_pause_7d_disabled' OR
        o.extra -> 'quota_notify_total_enabled' IS DISTINCT FROM n.extra -> 'quota_notify_total_enabled' OR
        o.extra -> 'quota_notify_total_threshold' IS DISTINCT FROM n.extra -> 'quota_notify_total_threshold' OR
        o.extra -> 'quota_notify_total_threshold_type' IS DISTINCT FROM n.extra -> 'quota_notify_total_threshold_type' OR
        o.extra -> 'quota_notify_daily_enabled' IS DISTINCT FROM n.extra -> 'quota_notify_daily_enabled' OR
        o.extra -> 'quota_notify_daily_threshold' IS DISTINCT FROM n.extra -> 'quota_notify_daily_threshold' OR
        o.extra -> 'quota_notify_daily_threshold_type' IS DISTINCT FROM n.extra -> 'quota_notify_daily_threshold_type' OR
        o.extra -> 'quota_notify_weekly_enabled' IS DISTINCT FROM n.extra -> 'quota_notify_weekly_enabled' OR
        o.extra -> 'quota_notify_weekly_threshold' IS DISTINCT FROM n.extra -> 'quota_notify_weekly_threshold' OR
        o.extra -> 'quota_notify_weekly_threshold_type' IS DISTINCT FROM n.extra -> 'quota_notify_weekly_threshold_type'
    ORDER BY COALESCE(n.id, o.id)
    ON CONFLICT (account_id) DO UPDATE
    SET generation = public.scheduler_dirty_account_sources.generation + 1,
        bucket_dirty = public.scheduler_dirty_account_sources.bucket_dirty OR EXCLUDED.bucket_dirty,
        group_cursor = 0,
        updated_at = statement_timestamp();
    RETURN NULL;
END
$$;

REVOKE ALL ON FUNCTION scheduler_accounts_source_from_update() FROM PUBLIC;

-- Recreate the trigger so the bound function is the scoped version; the stable
-- name keeps re-runs idempotent.
DROP TRIGGER IF EXISTS scheduler_accounts_update_source ON accounts;
CREATE TRIGGER scheduler_accounts_update_source
AFTER UPDATE ON accounts
REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows
FOR EACH STATEMENT EXECUTE FUNCTION scheduler_accounts_source_from_update();
