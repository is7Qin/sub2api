package migrations

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// schedulerTriggerWhitelistedExtraKeys 是 177 触发函数比较的 extra 定点白名单
// （调度判定实际读取，宁可多列不可漏）。与 runtime 热路径 key 互斥：
// 白名单外的 key（codex 用量快照、model_rate_limits、指纹、会话利用率、
// passive usage、compact 探测观测等）变更不产生脏标记。
var schedulerTriggerWhitelistedExtraKeys = []string{
	// account.go 路由/资格判定读取
	"privacy_mode",         // account.go:165,167 IsPrivacyModeTrainingOff / AntigravityPrivacySet
	"claude_user_id",       // account.go:870 GetClaudeUserID
	"mixed_scheduling",     // account.go:1394 IsMixedSchedulingEnabled（分桶归属）
	"allow_overages",       // account.go:1410 IsOveragesEnabled
	"openai_passthrough",   // account.go:1424 IsOpenAIAPIKeyPassthroughEnabled
	"openai_oauth_ws_mode", // account.go:1442,1561 WS 入口模式
	"openai_apikey_responses_websockets_v2_enabled", // account.go:1453
	"openai_apikey_responses_websockets_v2_mode",    // account.go:1574
	"responses_websockets_v2_enabled",               // account.go:1457
	"openai_ws_enabled",                             // account.go:1460
	"openai_ws_force_http",                          // account.go:1600
	"openai_ws_allow_store_recovery",                // account.go:1610
	"anthropic_passthrough",                         // account.go:1621
	"web_search_emulation",                          // account.go:1639 gateway_websearch_emulation.go:35
	"codex_cli_only",                                // account.go:1667
	"codex_cli_only_allowed_clients",                // account.go:1678
	"enable_tls_fingerprint",                        // account.go:1732
	"tls_fingerprint_profile_id",                    // account.go:1746
	"user_msg_queue_mode",                           // account.go:1772
	"user_msg_queue_enabled",                        // account.go:1779
	"session_id_masking_enabled",                    // account.go:1796
	"custom_base_url_enabled",                       // account.go:1813
	"cache_ttl_override_enabled",                    // account.go:1836
	"cache_ttl_override_target",                     // account.go:1850
	// compact / responses 能力路由
	"openai_compact_mode",                 // account.go:763 GetOpenAICompactMode
	"openai_compact_supported",            // account.go:784 OpenAICompactSupportKnown
	"openai_responses_mode",               // pkg/openai_compat/upstream_capability.go:57,99
	"openai_responses_supported",          // upstream_capability.go:61,114
	"openai_responses_supported_by_model", // upstream_capability.go:66（对象值，必须用 -> 比较）
	// 旧版 WS 开关（迁移兼容，随 oauth_ws_mode 一起处理）
	"openai_oauth_responses_websockets_v2_enabled", // scheduler_cache.go:1009 meta 白名单
	"openai_oauth_responses_websockets_v2_mode",    // scheduler_cache.go:1010
	// quota 调度（限额/用量/重置窗口）
	"quota_limit",             // account.go:1861
	"quota_used",              // account.go:1866
	"quota_daily_limit",       // account.go:1871
	"quota_daily_used",        // account.go:1876
	"quota_daily_start",       // account.go:2298
	"quota_daily_reset_mode",  // account.go:1965
	"quota_daily_reset_hour",  // account.go:1973
	"quota_daily_reset_at",    // account.go:2157（与 used/start 同批写入）
	"quota_weekly_limit",      // account.go:1881
	"quota_weekly_used",       // account.go:1886
	"quota_weekly_start",      // account.go:2307
	"quota_weekly_reset_mode", // account.go:1978
	"quota_weekly_reset_day",  // account.go:1989
	"quota_weekly_reset_hour", // account.go:1997
	"quota_weekly_reset_at",   // account.go:2176
	"quota_reset_timezone",    // account.go:2002
	// 费用窗口 / 会话 / RPM
	"window_cost_limit",            // account.go:2355
	"window_cost_sticky_reserve",   // account.go:2367
	"max_sessions",                 // account.go:2382
	"session_idle_timeout_minutes", // account.go:2394
	"base_rpm",                     // account.go:2409
	"rpm_strategy",                 // account.go:2424
	"rpm_sticky_buffer",            // account.go:2441
	// Gemini drive tier 缓存（路由判定读取）
	"drive_tier_updated_at", // gemini_oauth_service.go:836
	"drive_storage_limit",   // gemini_oauth_service.go:862（同批写入）
	"drive_storage_usage",   // gemini_oauth_service.go:863
	// 自动暂停阈值
	"auto_pause_5h_threshold", // openai_gateway_service.go:2189
	"auto_pause_7d_threshold", // openai_gateway_service.go:2190
	"auto_pause_5h_disabled",  // openai_gateway_service.go:2138
	"auto_pause_7d_disabled",  // openai_gateway_service.go:2139
	// quota 通知配置（account.go:2013-2015，宁可多列）
	"quota_notify_total_enabled",
	"quota_notify_total_threshold",
	"quota_notify_total_threshold_type",
	"quota_notify_daily_enabled",
	"quota_notify_daily_threshold",
	"quota_notify_daily_threshold_type",
	"quota_notify_weekly_enabled",
	"quota_notify_weekly_threshold",
	"quota_notify_weekly_threshold_type",
}

// schedulerTriggerWhitelistedColumns 是触发函数比较的顶层列白名单
// （SetAccount 全量 + meta 子集消费；不含 last_used_at/updated_at/created_at/
// error_message——last_used 走 outbox 观测通道，error_message 变更必伴随 status）。
var schedulerTriggerWhitelistedColumns = []string{
	"name", "platform", "type", "credentials", "proxy_id",
	"concurrency", "load_factor", "priority", "rate_multiplier",
	"status", "expires_at", "auto_pause_on_expired", "schedulable",
	"rate_limited_at", "rate_limit_reset_at", "overload_until",
	"temp_unschedulable_until", "temp_unschedulable_reason",
	"session_window_start", "session_window_end", "session_window_status",
	"deleted_at",
}

// schedulerRuntimeOverlayKeys 是白名单外、热路径单独发布的运行时 extra key
// （与 164/165 投影 allowlist 完全一致），177 触发函数不得比较它们。
var schedulerRuntimeOverlayKeys = []string{
	"codex_usage_updated_at",
	"model_rate_limits",
	"openai_codex_fingerprint",
	"session_window_utilization",
	"codex_primary_used_percent",
	"codex_primary_reset_after_seconds",
	"codex_primary_window_minutes",
	"codex_primary_over_secondary_percent",
	"codex_secondary_used_percent",
	"codex_secondary_reset_after_seconds",
	"codex_secondary_window_minutes",
	"codex_5h_used_percent",
	"codex_5h_reset_after_seconds",
	"codex_5h_window_minutes",
	"codex_5h_reset_at",
	"codex_7d_used_percent",
	"codex_7d_reset_after_seconds",
	"codex_7d_window_minutes",
	"codex_7d_reset_at",
	"passive_usage_7d_utilization",
	"passive_usage_7d_reset",
	"passive_usage_7d_oi_utilization",
	"passive_usage_7d_oi_reset",
	"passive_usage_sampled_at",
}

func TestSchedulerSupportDecisionChannelUpdateDirtiesAffectedGroups(t *testing.T) {
	sql := readSchedulerSupportDecisionMigration(t)
	require.Contains(t, sql, "create or replace function scheduler_support_channels_dirty()")
	require.Contains(t, sql, "from old_rows as o full join new_rows as n using (id)")
	for _, column := range []string{"status", "restrict_models", "billing_model_source"} {
		require.Contains(t, sql, "o."+column+" is distinct from n."+column)
	}
	for _, column := range []string{"name", "description", "model_mapping", "features", "features_config", "apply_pricing_to_account_stats", "updated_at"} {
		require.NotContains(t, sql, "o."+column+" is distinct from n."+column)
	}
	require.Contains(t, sql, "from public.channel_groups cg")
	require.Contains(t, sql, "where cg.channel_id = any(channel_ids)")
	require.Contains(t, sql, "insert into public.scheduler_dirty_group_sources (group_id)")
	require.Contains(t, sql, "create trigger scheduler_support_channels_update_dirty")
	require.Contains(t, sql, "referencing old table as old_rows new table as new_rows")
}

func TestSchedulerSupportDecisionChannelMembershipDeleteDirtiesOldGroup(t *testing.T) {
	sql := readSchedulerSupportDecisionMigration(t)
	require.Contains(t, sql, "create or replace function scheduler_support_channel_groups_dirty()")
	require.Contains(t, sql, "from old_rows as o join new_rows as n using (id)")
	require.Contains(t, sql, "o.channel_id is distinct from n.channel_id")
	require.Contains(t, sql, "o.group_id is distinct from n.group_id")
	require.Contains(t, sql, "select group_id from old_rows order by group_id")
	require.Contains(t, sql, "create trigger scheduler_support_channel_groups_delete_dirty")
	require.Contains(t, sql, "referencing old table as old_rows")
}

func TestSchedulerSupportDecisionPricingModelChangeDirtiesAffectedGroups(t *testing.T) {
	sql := readSchedulerSupportDecisionMigration(t)
	require.Contains(t, sql, "create or replace function scheduler_support_channel_pricing_dirty()")
	for _, column := range []string{"channel_id", "models", "platform"} {
		require.Contains(t, sql, "o."+column+" is distinct from n."+column)
	}
	require.Contains(t, sql, "o.channel_id as old_channel_id")
	require.Contains(t, sql, "n.channel_id as new_channel_id")
	require.Contains(t, sql, "select old_channel_id as channel_id from changed")
	require.Contains(t, sql, "select new_channel_id as channel_id from changed")
	for _, column := range []string{"input_price", "output_price", "cache_write_price", "cache_read_price", "image_output_price", "per_request_price", "billing_mode", "updated_at"} {
		require.NotContains(t, sql, "o."+column+" is distinct from n."+column)
	}
	require.Contains(t, sql, "create trigger scheduler_support_channel_pricing_update_dirty")
	require.Contains(t, sql, "referencing old table as old_rows new table as new_rows")
	require.Contains(t, sql, "c.status = 'active'")
	require.Contains(t, sql, "c.restrict_models")
	require.Contains(t, sql, "billing_model_source = 'upstream'")
	// Interval columns are all price/tier data; the support builder consumes only
	// the parent pricing row's platform and models.
	require.NotContains(t, sql, "create trigger scheduler_support_pricing_intervals_")
}

func TestSchedulerSupportDecisionUnscopedChannelChangeRequestsGlobalRebuild(t *testing.T) {
	sql := readSchedulerSupportDecisionMigration(t)
	require.Contains(t, sql, "insert into public.scheduler_dirty_work (kind, entity_id, rebuild_buckets)")
	require.Contains(t, sql, "select 3, 0, true")
	require.Contains(t, sql, "where not exists")
	require.Contains(t, sql, "on conflict (kind, entity_id) do update")
	require.Contains(t, sql, "generation = public.scheduler_dirty_work.generation + 1")
}

func TestSchedulerSupportDecisionTransientAccountChangesRemainIgnored(t *testing.T) {
	raw, err := FS.ReadFile("177_scheduler_dirty_work_trigger_scope.sql")
	require.NoError(t, err)
	sql := strings.ToLower(string(raw))
	for _, column := range []string{"last_used_at", "updated_at", "created_at", "error_message"} {
		require.NotContains(t, sql, "o."+column+" is distinct from")
	}
	for _, key := range schedulerRuntimeOverlayKeys {
		require.NotContains(t, sql, "o.extra -> '"+key+"'")
	}
}

func TestSchedulerDirtyWorkTriggerScopeMigrationPinsFixedPointWhitelist(t *testing.T) {
	raw, err := FS.ReadFile("177_scheduler_dirty_work_trigger_scope.sql")
	require.NoError(t, err)
	// 先剥离行注释，避免注释里提及旧实现干扰断言。
	var builder strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "--") {
			continue
		}
		builder.WriteString(line)
		builder.WriteString("\n")
	}
	sql := strings.ToLower(builder.String())

	// 触发函数必须重建：迁移要替换 164 版比较逻辑。
	require.Contains(t, sql, "create or replace function scheduler_accounts_source_from_update()")
	require.Contains(t, sql, "drop trigger if exists scheduler_accounts_update_source on accounts")
	require.Contains(t, sql, "create trigger scheduler_accounts_update_source")
	require.Contains(t, sql, "referencing old table as old_rows new table as new_rows")

	// 定点比较：不得再出现整份 extra 的批量剥离/投影。
	require.NotContains(t, sql, "- array[")
	require.NotContains(t, sql, "jsonb_each")
	require.NotContains(t, sql, "jsonb_object_agg")
	require.NotContains(t, sql, "scheduler_account_lifecycle_extra(")

	// 顶层列白名单逐一在 WHERE 中比较；运行时列与 last_used_at 不参与。
	for _, column := range schedulerTriggerWhitelistedColumns {
		require.Contains(t, sql, "o."+column+" is distinct from n."+column)
	}
	for _, column := range []string{"last_used_at", "updated_at", "created_at", "error_message"} {
		require.NotContains(t, sql, "o."+column+" is distinct from")
	}

	// extra 定点 key 白名单：o.extra -> 'key' 与 n.extra -> 'key' 成对出现。
	// 只统计 WHERE 段（using (id) 之后），排除 bucket_dirty CASE 里的 mixed_scheduling。
	whereSection := sql[strings.Index(sql, "using (id)"):]
	keyPattern := regexp.MustCompile(`o\.extra -> '([^']+)' is distinct from n\.extra -> '([^']+)'`)
	matches := keyPattern.FindAllStringSubmatch(whereSection, -1)
	require.Len(t, matches, len(schedulerTriggerWhitelistedExtraKeys))
	gotKeys := make([]string, 0, len(matches))
	for _, match := range matches {
		require.Equal(t, match[1], match[2], "o/n 两侧 key 必须一致")
		gotKeys = append(gotKeys, match[1])
	}
	require.ElementsMatch(t, schedulerTriggerWhitelistedExtraKeys, gotKeys)

	// 运行时热路径 key 不得进入定点比较。
	for _, key := range schedulerRuntimeOverlayKeys {
		require.NotContains(t, sql, "o.extra -> '"+key+"'")
	}
	// compact 探测观测字段（只写不读）也不得进入比较。
	for _, key := range []string{"openai_compact_checked_at", "openai_compact_last_status", "openai_compact_last_error"} {
		require.NotContains(t, sql, "o.extra -> '"+key+"'")
	}

	// bucket_dirty 判定保持不变：mixed_scheduling 变更需触发分桶重建。
	require.Contains(t, sql, "o.extra -> 'mixed_scheduling' is distinct from n.extra -> 'mixed_scheduling'")
}
