//go:build unit

package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newSchedulerCacheUnit(t *testing.T) *schedulerCache {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	cache, ok := newSchedulerCacheWithChunkSizes(rdb, defaultSchedulerSnapshotMGetChunkSize, defaultSchedulerSnapshotWriteChunkSize).(*schedulerCache)
	require.True(t, ok)
	return cache
}

func TestSchedulerCacheSetSnapshotSkipsOnlyUnencodableAccounts(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	bucket := service.SchedulerBucket{GroupID: 7, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
	invalidTime := time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC)
	stale := service.Account{ID: 112, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey}
	require.NoError(t, cache.SetAccount(ctx, &stale))
	require.NoError(t, cache.rdb.Set(ctx, schedulerLastUsedKey("112"), time.Now().UnixMilli(), 0).Err())

	err := cache.SetSnapshot(ctx, bucket, []service.Account{
		{ID: 111, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey},
		{ID: 112, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, ExpiresAt: &invalidTime},
	})
	require.NoError(t, err)

	snapshot, hit, err := cache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	require.True(t, hit)
	require.Len(t, snapshot, 1)
	require.Equal(t, int64(111), snapshot[0].ID)
	cachedInvalid, err := cache.GetAccount(ctx, stale.ID)
	require.NoError(t, err)
	require.Nil(t, cachedInvalid)
	require.Zero(t, cache.rdb.Exists(ctx, schedulerAccountKey("112"), schedulerAccountMetaKey("112"), schedulerLastUsedKey("112")).Val())
}

func TestSchedulerCacheSetAccountClearsUnencodablePayload(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	account := service.Account{ID: 113, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey}
	require.NoError(t, cache.SetAccount(ctx, &account))

	invalidTime := time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC)
	account.ExpiresAt = &invalidTime
	require.NoError(t, cache.SetAccount(ctx, &account))

	cached, err := cache.GetAccount(ctx, account.ID)
	require.NoError(t, err)
	require.Nil(t, cached)
	exists, err := cache.rdb.Exists(ctx, schedulerAccountMetaKey("113")).Result()
	require.NoError(t, err)
	require.Zero(t, exists)
}

func TestSchedulerCacheUpdateLastUsedClearsOnlyUnencodableAccount(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	invalid := service.Account{ID: 114, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey}
	valid := service.Account{ID: 115, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey}
	require.NoError(t, cache.SetAccount(ctx, &invalid))
	require.NoError(t, cache.SetAccount(ctx, &valid))
	require.NoError(t, cache.rdb.Set(ctx, schedulerLastUsedKey("114"), time.Now().UnixMilli(), 0).Err())

	invalidTime := time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC)
	validTime := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, cache.UpdateLastUsed(ctx, map[int64]time.Time{
		invalid.ID: invalidTime,
		valid.ID:   validTime,
	}))

	cachedInvalid, err := cache.GetAccount(ctx, invalid.ID)
	require.NoError(t, err)
	require.Nil(t, cachedInvalid)
	exists, err := cache.rdb.Exists(ctx, schedulerAccountMetaKey("114")).Result()
	require.NoError(t, err)
	require.Zero(t, exists)
	exists, err = cache.rdb.Exists(ctx, schedulerLastUsedKey("114")).Result()
	require.NoError(t, err)
	require.Zero(t, exists)
	cachedValid, err := cache.GetAccount(ctx, valid.ID)
	require.NoError(t, err)
	require.NotNil(t, cachedValid)
	require.Equal(t, validTime, *cachedValid.LastUsedAt)
}

func TestBuildSchedulerMetadataAccount_KeepsOpenAIWSFlags(t *testing.T) {
	account := service.Account{
		ID:       42,
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeOAuth,
		Extra: map[string]any{
			"openai_oauth_ws_mode":                         service.OpenAIOAuthWSModeManagedSession,
			"openai_oauth_responses_websockets_v2_enabled": true,
			"openai_oauth_responses_websockets_v2_mode":    service.OpenAIWSIngressModePassthrough,
			"openai_ws_force_http":                         true,
			"openai_responses_mode":                        "force_chat_completions",
			"openai_responses_supported":                   false,
			"mixed_scheduling":                             true,
			"unused_large_field":                           "drop-me",
		},
	}

	got := buildSchedulerMetadataAccount(account)

	require.Equal(t, service.OpenAIOAuthWSModeManagedSession, got.Extra["openai_oauth_ws_mode"])
	require.Equal(t, true, got.Extra["openai_oauth_responses_websockets_v2_enabled"])
	require.Equal(t, service.OpenAIWSIngressModePassthrough, got.Extra["openai_oauth_responses_websockets_v2_mode"])
	require.Equal(t, true, got.Extra["openai_ws_force_http"])
	require.Equal(t, "force_chat_completions", got.Extra["openai_responses_mode"])
	require.Equal(t, false, got.Extra["openai_responses_supported"])
	require.Equal(t, true, got.Extra["mixed_scheduling"])
	require.Nil(t, got.Extra["unused_large_field"])
}

func TestBuildSchedulerMetadataAccount_KeepsSlimGroupMembership(t *testing.T) {
	account := service.Account{
		ID:       42,
		Platform: service.PlatformAnthropic,
		GroupIDs: []int64{7, 9, 7, 0},
		AccountGroups: []service.AccountGroup{
			{
				AccountID: 42,
				GroupID:   7,
				Priority:  2,
				Account:   &service.Account{ID: 42, Name: "drop-from-metadata"},
				Group:     &service.Group{ID: 7, Name: "drop-from-metadata"},
			},
			{
				AccountID: 42,
				GroupID:   11,
				Priority:  3,
				Group:     &service.Group{ID: 11, Name: "drop-from-metadata"},
			},
			{
				AccountID: 42,
				GroupID:   0,
				Priority:  4,
			},
		},
	}

	got := buildSchedulerMetadataAccount(account)

	require.Equal(t, []int64{7, 9, 11}, got.GroupIDs)
	require.Len(t, got.AccountGroups, 2)
	require.Equal(t, int64(42), got.AccountGroups[0].AccountID)
	require.Equal(t, int64(7), got.AccountGroups[0].GroupID)
	require.Equal(t, 2, got.AccountGroups[0].Priority)
	require.Nil(t, got.AccountGroups[0].Account)
	require.Nil(t, got.AccountGroups[0].Group)
	require.Equal(t, int64(11), got.AccountGroups[1].GroupID)
	require.Nil(t, got.Groups)
}

func TestBuildSchedulerMetadataAccount_KeepsQuotaAutoPauseFields(t *testing.T) {
	account := service.Account{
		ID: 88,
		Extra: map[string]any{
			"privacy_mode":                 service.PrivacyModeTrainingOff,
			"codex_5h_used_percent":        12.34,
			"codex_7d_used_percent":        56.78,
			"codex_5h_reset_at":            "2026-05-29T10:00:00Z",
			"codex_7d_reset_at":            "2026-06-01T10:00:00Z",
			"codex_5h_reset_after_seconds": 300,
			"codex_7d_reset_after_seconds": 600,
			"codex_usage_updated_at":       "2026-05-29T09:00:00Z",
			"auto_pause_5h_threshold":      0.95,
			"auto_pause_7d_threshold":      0.96,
			"auto_pause_5h_disabled":       true,
			"auto_pause_7d_disabled":       false,
		},
	}

	got := buildSchedulerMetadataAccount(account)

	require.Equal(t, service.PrivacyModeTrainingOff, got.Extra["privacy_mode"])
	require.Equal(t, 12.34, got.Extra["codex_5h_used_percent"])
	require.Equal(t, 56.78, got.Extra["codex_7d_used_percent"])
	require.Equal(t, "2026-05-29T10:00:00Z", got.Extra["codex_5h_reset_at"])
	require.Equal(t, "2026-06-01T10:00:00Z", got.Extra["codex_7d_reset_at"])
	require.Equal(t, 300, got.Extra["codex_5h_reset_after_seconds"])
	require.Equal(t, 600, got.Extra["codex_7d_reset_after_seconds"])
	require.Equal(t, "2026-05-29T09:00:00Z", got.Extra["codex_usage_updated_at"])
	require.Equal(t, 0.95, got.Extra["auto_pause_5h_threshold"])
	require.Equal(t, 0.96, got.Extra["auto_pause_7d_threshold"])
	require.Equal(t, true, got.Extra["auto_pause_5h_disabled"])
	require.Equal(t, false, got.Extra["auto_pause_7d_disabled"])
}

func TestBuildSchedulerMetadataAccount_KeepsQuotaStateForCachedAccounts(t *testing.T) {
	now := time.Now().UTC()
	activeStart := now.Add(-time.Hour).Format(time.RFC3339)
	expiredDailyStart := now.Add(-25 * time.Hour).Format(time.RFC3339)
	expiredWeeklyStart := now.Add(-8 * 24 * time.Hour).Format(time.RFC3339)
	weeklyResetDay := float64(now.AddDate(0, 0, 1).Weekday())

	cases := []struct {
		name          string
		platform      string
		typ           string
		extra         map[string]any
		quotaExceeded bool
	}{
		{name: "anthropic api key total quota exhausted", platform: service.PlatformAnthropic, typ: service.AccountTypeAPIKey,
			extra: map[string]any{"quota_limit": 10.0, "quota_used": 10.0}, quotaExceeded: true},
		{name: "gemini api key rolling daily quota exhausted", platform: service.PlatformGemini, typ: service.AccountTypeAPIKey,
			extra: map[string]any{"quota_daily_limit": 20.0, "quota_daily_used": 20.0, "quota_daily_start": activeStart, "quota_daily_reset_mode": "rolling"}, quotaExceeded: true},
		{name: "gemini api key expired rolling daily window", platform: service.PlatformGemini, typ: service.AccountTypeAPIKey,
			extra: map[string]any{"quota_daily_limit": 20.0, "quota_daily_used": 20.0, "quota_daily_start": expiredDailyStart, "quota_daily_reset_mode": "rolling"}},
		{name: "bedrock fixed weekly quota exhausted", platform: service.PlatformAnthropic, typ: service.AccountTypeBedrock,
			extra: map[string]any{"quota_weekly_limit": 30.0, "quota_weekly_used": 30.0, "quota_weekly_start": activeStart, "quota_weekly_reset_mode": "fixed", "quota_weekly_reset_day": weeklyResetDay, "quota_weekly_reset_hour": 0.0, "quota_reset_timezone": "UTC"}, quotaExceeded: true},
		{name: "bedrock expired fixed weekly window", platform: service.PlatformAnthropic, typ: service.AccountTypeBedrock,
			extra: map[string]any{"quota_weekly_limit": 30.0, "quota_weekly_used": 30.0, "quota_weekly_start": expiredWeeklyStart, "quota_weekly_reset_mode": "fixed", "quota_weekly_reset_day": weeklyResetDay, "quota_weekly_reset_hour": 0.0, "quota_reset_timezone": "UTC"}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			extra := make(map[string]any, len(tc.extra)+1)
			for key, value := range tc.extra {
				extra[key] = value
			}
			extra["unrelated"] = "drop me"
			account := service.Account{ID: int64(46690 + i), Platform: tc.platform, Type: tc.typ, Extra: extra, Status: service.StatusActive, Schedulable: true}
			cache := newSchedulerCacheUnit(t)
			ctx := context.Background()
			bucket := service.SchedulerBucket{GroupID: int64(46690 + i), Platform: tc.platform, Mode: service.SchedulerModeSingle}
			require.NoError(t, cache.SetSnapshot(ctx, bucket, []service.Account{account}))

			snapshot, hit, err := cache.GetSnapshot(ctx, bucket)
			require.NoError(t, err)
			require.True(t, hit)
			require.Len(t, snapshot, 1)
			cached := snapshot[0]
			require.Equal(t, tc.extra, cached.Extra)
			require.NotContains(t, cached.Extra, "unrelated")
			require.Equal(t, tc.quotaExceeded, cached.IsQuotaExceeded())
			require.Equal(t, !tc.quotaExceeded, cached.IsSchedulable())
		})
	}
}

func TestSchedulerCacheGetSchedulableAccountsByIDs_MixedPresentMissing(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	full := &service.Account{
		ID: 701, Name: "full", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true,
		Credentials: map[string]any{"api_key": "secret-701", "project_id": "proj-701"},
		Extra:       map[string]any{"quota_limit": 100.0, "unused": "drop"},
	}
	require.NoError(t, cache.SetAccount(ctx, full))
	require.NoError(t, cache.SetAccount(ctx, &service.Account{ID: 703, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey}))

	got, err := cache.GetSchedulableAccountsByIDs(ctx, []int64{701, 999, 703, 0, 701})
	require.NoError(t, err)
	// 缺失 ID 跳过、重复 ID 去重：只有快照中存在的账号进入返回 map。
	require.Len(t, got, 2)
	require.Equal(t, "full", got[701].Name)
	// 批量刷新读取 meta payload：调度字段保留、凭据脱敏、extra 走白名单过滤。
	require.Equal(t, map[string]any{"project_id": "proj-701", "has_api_key": true}, got[701].Credentials)
	require.Equal(t, map[string]any{"quota_limit": 100.0}, got[701].Extra)
	require.Equal(t, int64(703), got[703].ID)
	require.Nil(t, got[999])

	empty, err := cache.GetSchedulableAccountsByIDs(ctx, nil)
	require.NoError(t, err)
	require.Empty(t, empty)
}

// 只写入 meta payload（sched:meta:{id}）时批量刷新即可正常返回调度字段。
func TestSchedulerCacheGetSchedulableAccountsByIDs_MetaKeysOnly(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	account := service.Account{
		ID: 801, Name: "meta", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true,
		Credentials: map[string]any{"api_key": "secret-801", "project_id": "proj-801"},
		Extra:       map[string]any{"quota_limit": 100.0, "unused": "drop"},
	}
	metaPayload, err := json.Marshal(buildSchedulerMetadataAccount(account))
	require.NoError(t, err)
	require.NoError(t, cache.rdb.Set(ctx, schedulerAccountMetaKey("801"), metaPayload, 0).Err())

	got, err := cache.GetSchedulableAccountsByIDs(ctx, []int64{801, 999})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "meta", got[801].Name)
	require.Equal(t, map[string]any{"project_id": "proj-801", "has_api_key": true}, got[801].Credentials)
	require.Equal(t, map[string]any{"quota_limit": 100.0}, got[801].Extra)
	require.Nil(t, got[999])
}

// 只写入全量 payload（sched:acc:{id}）时批量刷新视为不可调度（空 map）：
// 读取 meta 键后旧版本残留的全量键不得让已删除账号重新进入候选。
func TestSchedulerCacheGetSchedulableAccountsByIDs_FullKeysOnly(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	fullPayload, err := json.Marshal(service.Account{ID: 802, Name: "full-only", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey})
	require.NoError(t, err)
	require.NoError(t, cache.rdb.Set(ctx, schedulerAccountKey("802"), fullPayload, 0).Err())

	got, err := cache.GetSchedulableAccountsByIDs(ctx, []int64{802})
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestSchedulerCacheGetSchedulableAccountsByIDs_ChunkBoundary(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	cache.mgetChunkSize = 2
	var ids []int64
	for i := int64(1); i <= 5; i++ {
		ids = append(ids, i)
		require.NoError(t, cache.SetAccount(ctx, &service.Account{ID: i, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey}))
	}

	// 跨分块混入缺失 ID：分块内和分块之间都要跳过而非报错。
	got, err := cache.GetSchedulableAccountsByIDs(ctx, []int64{1, 999, 2, 3, 4, 5, 1000})
	require.NoError(t, err)
	require.Len(t, got, 5)
	for _, id := range ids {
		require.Equal(t, id, got[id].ID)
	}
}

type schedulerSnapshotWriteFailureHook struct {
	zaddCalls atomic.Int32
}

func (h *schedulerSnapshotWriteFailureHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *schedulerSnapshotWriteFailureHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "zadd" && h.zaddCalls.Add(1) == 2 {
			return errors.New("injected snapshot zadd failure")
		}
		return next(ctx, cmd)
	}
}

func (h *schedulerSnapshotWriteFailureHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func TestSchedulerCacheSetSnapshotPartialFailureDoesNotPublishNewVersion(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	cache.writeChunkSize = 1
	bucket := service.SchedulerBucket{GroupID: 23, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
	oldAccount := service.Account{ID: 731, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey}
	require.NoError(t, cache.SetSnapshot(ctx, bucket, []service.Account{oldAccount}))
	oldVersion := cache.rdb.Get(ctx, schedulerBucketKey(schedulerActivePrefix, bucket)).Val()

	hook := &schedulerSnapshotWriteFailureHook{}
	cache.rdb.AddHook(hook)
	err := cache.SetSnapshot(ctx, bucket, []service.Account{
		{ID: 732, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey},
		{ID: 733, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey},
	})
	require.ErrorContains(t, err, "injected snapshot zadd failure")
	require.Equal(t, oldVersion, cache.rdb.Get(ctx, schedulerBucketKey(schedulerActivePrefix, bucket)).Val())
	newVersionNumber, err := cache.rdb.Get(ctx, schedulerBucketKey(schedulerVersionPrefix, bucket)).Int64()
	require.NoError(t, err)
	newVersion := strconv.FormatInt(newVersionNumber, 10)
	require.NotEqual(t, oldVersion, newVersion)
	require.Zero(t, cache.rdb.Exists(ctx, schedulerSnapshotKey(bucket, newVersion)).Val())
	snapshot, hit, err := cache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	require.True(t, hit)
	require.Len(t, snapshot, 1)
	require.Equal(t, oldAccount.ID, snapshot[0].ID)
}

func TestSchedulerCacheSetSnapshotPreservesIDMemberSemanticsAndPayloadBytes(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	invalidTime := time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC)
	validOne := service.Account{ID: 721, Name: "first", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Extra: map[string]any{"mixed_scheduling": true}}
	validTwo := service.Account{ID: 722, Name: "second", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey}
	invalid := service.Account{ID: 799, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, ExpiresAt: &invalidTime}
	bucket := service.SchedulerBucket{GroupID: 21, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}

	fullExpected, metaExpected, err := marshalSchedulerCacheAccount(validOne)
	require.NoError(t, err)
	require.NoError(t, cache.SetSnapshot(ctx, bucket, []service.Account{validOne, invalid, validTwo, validOne}))
	fullActual, err := cache.rdb.Get(ctx, schedulerAccountKey("721")).Bytes()
	require.NoError(t, err)
	metaActual, err := cache.rdb.Get(ctx, schedulerAccountMetaKey("721")).Bytes()
	require.NoError(t, err)
	require.Equal(t, fullExpected, fullActual)
	require.Equal(t, metaExpected, metaActual)
	version := cache.rdb.Get(ctx, schedulerBucketKey(schedulerActivePrefix, bucket)).Val()
	require.Equal(t, []string{"722", "721"}, cache.rdb.ZRange(ctx, schedulerSnapshotKey(bucket, version), 0, -1).Val())
	require.Zero(t, cache.rdb.Exists(ctx, schedulerAccountKey("799"), schedulerAccountMetaKey("799"), schedulerLastUsedKey("799")).Val())

	empty := service.SchedulerBucket{GroupID: 22, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
	require.NoError(t, cache.SetSnapshot(ctx, empty, nil))
	snapshot, hit, err := cache.GetSnapshot(ctx, empty)
	require.NoError(t, err)
	require.False(t, hit)
	require.Nil(t, snapshot)
}

func TestSchedulerCacheGetSnapshotVersion(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	bucket := service.SchedulerBucket{GroupID: 5, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}

	// active key 尚未写入：空版本且无错误（此时 GetSnapshot 同样未命中，
	// 不存在任何本地解码缓存条目可复用）。
	version, err := cache.GetSnapshotVersion(ctx, bucket)
	require.NoError(t, err)
	require.Empty(t, version)

	require.NoError(t, cache.SetSnapshot(ctx, bucket, []service.Account{
		{ID: 501, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey},
	}))
	version, err = cache.GetSnapshotVersion(ctx, bucket)
	require.NoError(t, err)
	require.Equal(t, "1", version)

	// 二次写入后版本递增，解码缓存据此失效。
	require.NoError(t, cache.SetSnapshot(ctx, bucket, []service.Account{
		{ID: 502, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey},
	}))
	version, err = cache.GetSnapshotVersion(ctx, bucket)
	require.NoError(t, err)
	require.Equal(t, "2", version)
}

func BenchmarkSchedulerSnapshotAccountMemberMaterialization(b *testing.B) {
	for _, size := range []int{128, 1024, 10000} {
		accounts := make([]service.Account, size)
		ids := make([]int64, size)
		for i := range accounts {
			accounts[i].ID = int64(i + 1)
			ids[i] = accounts[i].ID
		}
		b.Run(fmt.Sprintf("ids/%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				members := make([]redis.Z, 0, len(ids))
				for idx, id := range ids {
					members = append(members, redis.Z{Score: float64(idx), Member: strconv.FormatInt(id, 10)})
				}
			}
		})
		b.Run(fmt.Sprintf("old_accounts/%d", size), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				temporary := append([]service.Account(nil), accounts...)
				members := make([]redis.Z, 0, len(temporary))
				for idx, account := range temporary {
					members = append(members, redis.Z{Score: float64(idx), Member: strconv.FormatInt(account.ID, 10)})
				}
			}
		})
	}
}

func TestBuildSchedulerMetadataAccount_KeepsModelRateLimits(t *testing.T) {
	account := service.Account{
		ID:       90,
		Platform: service.PlatformAntigravity,
		Extra: map[string]any{
			"model_rate_limits": map[string]any{
				"gemini-3-flash": map[string]any{
					"rate_limit_reset_at": "2026-05-30T10:10:00Z",
				},
				"antigravity:gemini": map[string]any{
					"rate_limit_reset_at": "2026-05-30T10:10:00Z",
				},
			},
			"unused_large_field": "drop-me",
		},
	}

	got := buildSchedulerMetadataAccount(account)

	limits, ok := got.Extra["model_rate_limits"].(map[string]any)
	require.True(t, ok)
	require.Contains(t, limits, "gemini-3-flash")
	require.Contains(t, limits, "antigravity:gemini")
	require.Nil(t, got.Extra["unused_large_field"])
}

func TestBuildSchedulerMetadataAccount_DropsSensitiveCredentials(t *testing.T) {
	account := service.Account{
		ID:       91,
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"model_mapping":       map[string]any{"gpt-5.4": "upstream-model"},
			"project_id":          "project-visible-for-routing",
			"oauth_type":          "chatgpt",
			"openai_capabilities": []any{"chat_completions"},
			"api_key":             "present-sensitive-value",
			"access_token":        "present-sensitive-value",
			"refresh_token":       "present-sensitive-value",
		},
	}

	got := buildSchedulerMetadataAccount(account)

	require.Equal(t, map[string]any{"gpt-5.4": "upstream-model"}, got.Credentials["model_mapping"])
	require.Equal(t, "project-visible-for-routing", got.Credentials["project_id"])
	require.Equal(t, "chatgpt", got.Credentials["oauth_type"])
	require.Equal(t, []any{"chat_completions"}, got.Credentials["openai_capabilities"])
	require.True(t, got.HasCredential("api_key"))
	require.NotContains(t, got.Credentials, "api_key")
	require.NotContains(t, got.Credentials, "access_token")
	require.NotContains(t, got.Credentials, "refresh_token")
}

func TestBuildSchedulerMetadataAccount_HasAPIKeyRequiresNonEmptyString(t *testing.T) {
	cases := []struct {
		name  string
		value any
	}{
		{name: "empty", value: ""},
		{name: "whitespace", value: "   "},
		{name: "non string", value: true},
		{name: "object", value: map[string]any{"present": true}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildSchedulerMetadataAccount(service.Account{
				ID:       92,
				Platform: service.PlatformOpenAI,
				Type:     service.AccountTypeAPIKey,
				Credentials: map[string]any{
					"api_key": tc.value,
				},
			})

			require.False(t, got.HasCredential("api_key"))
			require.NotContains(t, got.Credentials, "api_key")
			require.NotContains(t, got.Credentials, "has_api_key")
		})
	}
}
