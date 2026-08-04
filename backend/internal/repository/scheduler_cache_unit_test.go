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
		if (cmd.Name() == "zadd" || cmd.Name() == "eval" || cmd.Name() == "evalsha") && h.zaddCalls.Add(1) == 2 {
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

func TestSchedulerCache_EmptyPublishedSnapshotIsAHit(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	bucket := service.SchedulerBucket{GroupID: 7, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}

	require.NoError(t, cache.SetStaticState(ctx, bucket, nil, nil))
	candidates, hit, err := cache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	require.True(t, hit)
	require.Empty(t, candidates)
}

func TestSchedulerCache_StaticStateReadersShareActiveVersion(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	bucket := service.SchedulerBucket{GroupID: 7, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
	candidate := service.Account{ID: 10, Platform: service.PlatformOpenAI, Status: service.StatusActive, Schedulable: true}
	supportOnly := service.Account{ID: 11, Platform: service.PlatformOpenAI, Status: service.StatusActive, Schedulable: true}

	require.NoError(t, cache.SetStaticState(ctx, bucket, []service.Account{candidate}, []service.Account{candidate, supportOnly}))
	candidates, candidateHit, err := cache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	support, supportHit, err := cache.GetPersistentSupport(ctx, bucket)
	require.NoError(t, err)
	require.True(t, candidateHit)
	require.True(t, supportHit)
	require.Equal(t, []int64{10}, schedulerCacheTestIDs(candidates))
	require.Equal(t, []int64{10, 11}, schedulerCacheTestIDs(support))
}

func TestSchedulerCache_StaticStateSameIDReadersUseIndependentPayloads(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	bucket := service.SchedulerBucket{GroupID: 7, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
	resetAt := time.Now().Add(time.Hour).UTC()
	overloadUntil := time.Now().Add(30 * time.Minute).UTC()
	candidate := service.Account{
		ID:               12,
		Platform:         service.PlatformOpenAI,
		Status:           service.StatusActive,
		Schedulable:      true,
		RateLimitResetAt: &resetAt,
		OverloadUntil:    &overloadUntil,
		Credentials:      map[string]any{"api_key": "candidate-secret"},
	}
	persistentSupport := service.Account{
		ID:          candidate.ID,
		Platform:    service.PlatformOpenAI,
		Status:      service.StatusActive,
		Schedulable: true,
	}

	require.NoError(t, cache.SetStaticState(ctx, bucket, []service.Account{candidate}, []service.Account{persistentSupport}))
	candidates, candidateHit, err := cache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	support, supportHit, err := cache.GetPersistentSupport(ctx, bucket)
	require.NoError(t, err)
	hydrated, err := cache.GetStaticCandidateAccount(ctx, bucket, candidate.ID)
	require.NoError(t, err)
	refreshed, err := cache.GetStaticCandidateAccountsByIDs(ctx, bucket, []int64{candidate.ID, 99, candidate.ID})
	require.NoError(t, err)
	require.True(t, candidateHit)
	require.True(t, supportHit)
	require.Len(t, candidates, 1)
	require.Len(t, support, 1)
	require.Equal(t, resetAt, *candidates[0].RateLimitResetAt)
	require.Nil(t, support[0].RateLimitResetAt)
	require.NotNil(t, hydrated)
	require.Equal(t, resetAt, *hydrated.RateLimitResetAt)
	require.Equal(t, overloadUntil, *hydrated.OverloadUntil)
	require.Equal(t, "candidate-secret", hydrated.GetCredential("api_key"))
	require.Len(t, refreshed, 1)
	require.Equal(t, resetAt, *refreshed[candidate.ID].RateLimitResetAt)
	require.Equal(t, overloadUntil, *refreshed[candidate.ID].OverloadUntil)
}

func TestSchedulerCache_IncompleteStaticStateDoesNotHitPersistentSupport(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	bucket := service.SchedulerBucket{GroupID: 8, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
	version := "1"
	support := service.Account{ID: 12, Platform: service.PlatformOpenAI, Status: service.StatusActive, Schedulable: true}

	_, err := cache.writeAccountIDs(ctx, []service.Account{support})
	require.NoError(t, err)
	require.NoError(t, cache.writeSnapshotAccountIDs(ctx, bucket, version, []int64{support.ID}))
	require.NoError(t, cache.rdb.Set(ctx, schedulerBucketKey(schedulerActivePrefix, bucket), version, 0).Err())
	require.NoError(t, cache.rdb.Set(ctx, schedulerBucketKey(schedulerReadyPrefix, bucket), "1", 0).Err())

	accounts, hit, err := cache.GetPersistentSupport(ctx, bucket)
	require.NoError(t, err)
	require.False(t, hit)
	require.Nil(t, accounts)
}

func TestSchedulerCache_StaticStatePartialWriteKeepsPreviousVersion(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	cache.writeChunkSize = 1
	bucket := service.SchedulerBucket{GroupID: 9, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
	oldCandidate := service.Account{ID: 20, Name: "old", Platform: service.PlatformOpenAI, Credentials: map[string]any{"model_mapping": map[string]any{"old-model": "old-upstream"}}}
	oldSupport := service.Account{ID: 21, Name: "old-support", Platform: service.PlatformOpenAI}
	require.NoError(t, cache.SetStaticState(ctx, bucket, []service.Account{oldCandidate}, []service.Account{oldCandidate, oldSupport}))

	hook := &schedulerSnapshotWriteFailureHook{}
	cache.rdb.AddHook(hook)
	err := cache.SetStaticState(ctx, bucket,
		[]service.Account{{ID: 20, Name: "replacement", Platform: service.PlatformOpenAI, Credentials: map[string]any{"model_mapping": map[string]any{"new-model": "new-upstream"}}}},
		[]service.Account{{ID: 20, Name: "replacement", Platform: service.PlatformOpenAI, Credentials: map[string]any{"model_mapping": map[string]any{"new-model": "new-upstream"}}}, {ID: 23, Platform: service.PlatformOpenAI}},
	)
	require.ErrorContains(t, err, "injected snapshot zadd failure")

	candidates, candidateHit, err := cache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	support, supportHit, err := cache.GetPersistentSupport(ctx, bucket)
	require.NoError(t, err)
	require.True(t, candidateHit)
	require.True(t, supportHit)
	require.Equal(t, []int64{20}, schedulerCacheTestIDs(candidates))
	require.Equal(t, []int64{20, 21}, schedulerCacheTestIDs(support))
	require.Equal(t, "old", candidates[0].Name)
	require.Equal(t, map[string]any{"old-model": "old-upstream"}, candidates[0].Credentials["model_mapping"])
	require.Equal(t, "old", support[0].Name)
	require.Equal(t, map[string]any{"old-model": "old-upstream"}, support[0].Credentials["model_mapping"])
}

func TestSchedulerCache_GetStaticCandidateAccountRetriesStaticToLegacyTransition(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	bucket := service.SchedulerBucket{GroupID: 18, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
	v1 := service.Account{ID: 85, Name: "static-v1", Platform: service.PlatformOpenAI, Credentials: map[string]any{"api_key": "v1"}}
	v2 := service.Account{ID: 86, Name: "legacy-v2", Platform: service.PlatformOpenAI, Credentials: map[string]any{"api_key": "v2"}}
	replacementCache, ok := newSchedulerCacheWithChunkSizes(redis.NewClient(&redis.Options{Addr: cache.rdb.Options().Addr}), defaultSchedulerSnapshotMGetChunkSize, defaultSchedulerSnapshotWriteChunkSize).(*schedulerCache)
	require.True(t, ok)
	t.Cleanup(func() { _ = replacementCache.rdb.Close() })

	require.NoError(t, cache.SetStaticState(ctx, bucket, []service.Account{v1}, []service.Account{v1}))
	hook := &schedulerStaticStateTransitionReadHook{
		bucket: bucket,
		publishNext: func(ctx context.Context) error {
			return replacementCache.SetSnapshot(ctx, bucket, []service.Account{v2})
		},
	}
	cache.rdb.AddHook(hook)

	got, err := cache.GetStaticCandidateAccount(ctx, bucket, v1.ID)
	require.NoError(t, err)
	require.True(t, hook.fired.Load())
	require.Nil(t, got, "a static candidate absent from the new legacy snapshot must not use an old payload")
	got, err = cache.GetStaticCandidateAccount(ctx, bucket, v2.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, v2.Name, got.Name)
	require.Equal(t, "v2", got.GetCredential("api_key"))
}

func TestSchedulerCache_GetStaticCandidateAccountsByIDs_LegacySnapshotUsesGlobalPayload(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	bucket := service.SchedulerBucket{GroupID: 17, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
	account := service.Account{ID: 84, Name: "legacy", Platform: service.PlatformOpenAI, Credentials: map[string]any{"api_key": "legacy-secret"}}

	require.NoError(t, cache.SetSnapshot(ctx, bucket, []service.Account{account}))
	got, err := cache.GetStaticCandidateAccountsByIDs(ctx, bucket, []int64{account.ID, 999, account.ID})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, account.Name, got[account.ID].Name)
	require.Empty(t, got[account.ID].GetCredential("api_key"), "legacy batch payload is metadata")
	full, err := cache.GetStaticCandidateAccount(ctx, bucket, account.ID)
	require.NoError(t, err)
	require.NotNil(t, full)
	require.Equal(t, "legacy-secret", full.GetCredential("api_key"))
}

func TestSchedulerCache_StaticStateActivationUsesNewPayloadAndExpiresOldPayload(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	bucket := service.SchedulerBucket{GroupID: 10, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
	accountID := int64(30)

	require.NoError(t, cache.SetStaticState(ctx, bucket, []service.Account{{ID: accountID, Name: "old", Platform: service.PlatformOpenAI}}, []service.Account{{ID: accountID, Name: "old", Platform: service.PlatformOpenAI}}))
	oldVersion := cache.rdb.Get(ctx, schedulerBucketKey(schedulerActivePrefix, bucket)).Val()
	require.NoError(t, cache.SetStaticState(ctx, bucket, []service.Account{{ID: accountID, Name: "new", Platform: service.PlatformOpenAI}}, []service.Account{{ID: accountID, Name: "new", Platform: service.PlatformOpenAI}}))

	candidates, candidateHit, err := cache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	support, supportHit, err := cache.GetPersistentSupport(ctx, bucket)
	require.NoError(t, err)
	require.True(t, candidateHit)
	require.True(t, supportHit)
	require.Equal(t, "new", candidates[0].Name)
	require.Equal(t, "new", support[0].Name)
	id := strconv.FormatInt(accountID, 10)
	require.Equal(t, time.Duration(snapshotGraceTTLSeconds)*time.Second, cache.rdb.TTL(ctx, schedulerVersionedAccountMetaKey(bucket, oldVersion, id)).Val())
	require.Equal(t, time.Duration(snapshotGraceTTLSeconds)*time.Second, cache.rdb.TTL(ctx, schedulerVersionedSupportAccountMetaKey(bucket, oldVersion, id)).Val())
}

func TestSchedulerCache_SetSnapshotTransitionsStaticStateToLegacyPayloads(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	bucket := service.SchedulerBucket{GroupID: 15, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
	staticCandidate := service.Account{ID: 80, Name: "static candidate", Platform: service.PlatformOpenAI}
	staticSupport := service.Account{ID: 82, Name: "static support", Platform: service.PlatformOpenAI}
	legacy := service.Account{ID: 81, Name: "legacy", Platform: service.PlatformOpenAI}

	require.NoError(t, cache.SetStaticState(ctx, bucket, []service.Account{staticCandidate}, []service.Account{staticCandidate, staticSupport}))
	staticVersion := cache.rdb.Get(ctx, schedulerBucketKey(schedulerActivePrefix, bucket)).Val()
	require.NoError(t, cache.SetSnapshot(ctx, bucket, []service.Account{legacy}))

	candidates, hit, err := cache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	require.True(t, hit)
	require.Equal(t, []int64{legacy.ID}, schedulerCacheTestIDs(candidates))
	require.Equal(t, legacy.Name, candidates[0].Name)
	require.Zero(t, cache.rdb.Exists(ctx,
		schedulerBucketKey(schedulerSupportStatePrefix, bucket),
		schedulerBucketKey(schedulerSupportReadyPrefix, bucket),
	).Val())

	for _, key := range []string{
		schedulerSnapshotKey(bucket, staticVersion),
		schedulerSupportKey(bucket, staticVersion),
		schedulerVersionedAccountKey(bucket, staticVersion, strconv.FormatInt(staticCandidate.ID, 10)),
		schedulerVersionedAccountMetaKey(bucket, staticVersion, strconv.FormatInt(staticCandidate.ID, 10)),
		schedulerVersionedSupportAccountKey(bucket, staticVersion, strconv.FormatInt(staticCandidate.ID, 10)),
		schedulerVersionedSupportAccountMetaKey(bucket, staticVersion, strconv.FormatInt(staticCandidate.ID, 10)),
		schedulerVersionedSupportAccountKey(bucket, staticVersion, strconv.FormatInt(staticSupport.ID, 10)),
		schedulerVersionedSupportAccountMetaKey(bucket, staticVersion, strconv.FormatInt(staticSupport.ID, 10)),
	} {
		ttl := cache.rdb.TTL(ctx, key).Val()
		require.Greater(t, ttl, time.Duration(0), key)
		require.LessOrEqual(t, ttl, time.Duration(snapshotGraceTTLSeconds)*time.Second, key)
	}
}

type schedulerStaticStateTransitionReadHook struct {
	bucket      service.SchedulerBucket
	publishNext func(context.Context) error
	fired       atomic.Bool
}

func (h *schedulerStaticStateTransitionReadHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (h *schedulerStaticStateTransitionReadHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "get" && len(cmd.Args()) > 1 && cmd.Args()[1] == schedulerBucketKey(schedulerSupportStatePrefix, h.bucket) && h.fired.CompareAndSwap(false, true) {
			if err := h.publishNext(ctx); err != nil {
				return err
			}
		}
		return next(ctx, cmd)
	}
}

func (h *schedulerStaticStateTransitionReadHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func TestSchedulerCache_GetPersistentSupportRetriesStaticStateTransition(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	bucket := service.SchedulerBucket{GroupID: 19, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
	v1 := service.Account{ID: 87, Name: "support-v1", Platform: service.PlatformOpenAI}
	v2 := service.Account{ID: 88, Name: "support-v2", Platform: service.PlatformOpenAI}
	replacementCache, ok := newSchedulerCacheWithChunkSizes(
		redis.NewClient(&redis.Options{Addr: cache.rdb.Options().Addr}),
		defaultSchedulerSnapshotMGetChunkSize,
		defaultSchedulerSnapshotWriteChunkSize,
	).(*schedulerCache)
	require.True(t, ok)
	t.Cleanup(func() { _ = replacementCache.rdb.Close() })

	require.NoError(t, cache.SetStaticState(ctx, bucket, []service.Account{v1}, []service.Account{v1}))
	hook := &schedulerPersistentSupportTransitionReadHook{
		bucket: bucket,
		publishNext: func(ctx context.Context) error {
			return replacementCache.SetStaticState(ctx, bucket, []service.Account{v2}, []service.Account{v2})
		},
	}
	cache.rdb.AddHook(hook)

	support, hit, err := cache.GetPersistentSupport(ctx, bucket)
	require.NoError(t, err)
	require.True(t, hook.fired.Load())
	if hit {
		require.Equal(t, []int64{v2.ID}, schedulerCacheTestIDs(support), "a completed V2 activation must never return V1 support")
		require.Equal(t, v2.Name, support[0].Name)
	}
}

type schedulerPersistentSupportTransitionReadHook struct {
	bucket      service.SchedulerBucket
	publishNext func(context.Context) error
	fired       atomic.Bool
}

func (h *schedulerPersistentSupportTransitionReadHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (h *schedulerPersistentSupportTransitionReadHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		if err == nil && cmd.Name() == "zrange" && len(cmd.Args()) > 1 && cmd.Args()[1] == schedulerSupportKey(h.bucket, "1") && h.fired.CompareAndSwap(false, true) {
			return h.publishNext(ctx)
		}
		return err
	}
}

func (h *schedulerPersistentSupportTransitionReadHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func TestSchedulerCache_GetSnapshotRetriesStaticStateTransitionBeforeChoosingPayload(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	bucket := service.SchedulerBucket{GroupID: 14, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
	v1 := service.Account{ID: 70, Name: "v1 versioned", Platform: service.PlatformOpenAI}
	global := service.Account{ID: v1.ID, Name: "stale global", Platform: service.PlatformOpenAI}
	v2 := service.Account{ID: 71, Name: "v2 versioned", Platform: service.PlatformOpenAI}
	replacementCache, ok := newSchedulerCacheWithChunkSizes(
		redis.NewClient(&redis.Options{Addr: cache.rdb.Options().Addr}),
		defaultSchedulerSnapshotMGetChunkSize,
		defaultSchedulerSnapshotWriteChunkSize,
	).(*schedulerCache)
	require.True(t, ok)
	t.Cleanup(func() { _ = replacementCache.rdb.Close() })

	require.NoError(t, cache.SetStaticState(ctx, bucket, []service.Account{v1}, []service.Account{v1}))
	require.NoError(t, cache.SetAccount(ctx, &global))
	hook := &schedulerStaticStateTransitionReadHook{
		bucket: bucket,
		publishNext: func(ctx context.Context) error {
			return replacementCache.SetStaticState(ctx, bucket, []service.Account{v2}, []service.Account{v2})
		},
	}
	cache.rdb.AddHook(hook)

	candidates, hit, err := cache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	require.True(t, hook.fired.Load())
	require.True(t, hit)
	require.Equal(t, []int64{v2.ID}, schedulerCacheTestIDs(candidates))
	require.Equal(t, v2.Name, candidates[0].Name)
}

func TestSchedulerCache_GetSnapshotRetriesStaticToLegacyTransitionBeforeChoosingPayload(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	bucket := service.SchedulerBucket{GroupID: 16, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
	v1 := service.Account{ID: 82, Name: "v1 versioned", Platform: service.PlatformOpenAI}
	staleGlobal := service.Account{ID: v1.ID, Name: "stale global", Platform: service.PlatformOpenAI}
	v2 := service.Account{ID: 83, Name: "v2 legacy", Platform: service.PlatformOpenAI}
	replacementCache, ok := newSchedulerCacheWithChunkSizes(
		redis.NewClient(&redis.Options{Addr: cache.rdb.Options().Addr}),
		defaultSchedulerSnapshotMGetChunkSize,
		defaultSchedulerSnapshotWriteChunkSize,
	).(*schedulerCache)
	require.True(t, ok)
	t.Cleanup(func() { _ = replacementCache.rdb.Close() })

	require.NoError(t, cache.SetStaticState(ctx, bucket, []service.Account{v1}, []service.Account{v1}))
	require.NoError(t, cache.SetAccount(ctx, &staleGlobal))
	hook := &schedulerStaticStateTransitionReadHook{
		bucket: bucket,
		publishNext: func(ctx context.Context) error {
			return replacementCache.SetSnapshot(ctx, bucket, []service.Account{v2})
		},
	}
	cache.rdb.AddHook(hook)

	candidates, hit, err := cache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	require.True(t, hook.fired.Load())
	if hit {
		require.NotEqual(t, []int64{v1.ID}, schedulerCacheTestIDs(candidates))
		require.Equal(t, []int64{v2.ID}, schedulerCacheTestIDs(candidates))
		require.Equal(t, v2.Name, candidates[0].Name)
	}
}

type schedulerStaticPayloadPipelineFailureHook struct {
	pipelineCalls atomic.Int32
}

func (h *schedulerStaticPayloadPipelineFailureHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (h *schedulerStaticPayloadPipelineFailureHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return next
}

func (h *schedulerStaticPayloadPipelineFailureHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		err := next(ctx, cmds)
		if h.pipelineCalls.Add(1) == 1 && err == nil {
			// Report an ambiguous outcome only after Redis accepted the pipeline writes.
			return errors.New("injected ambiguous static payload pipeline failure")
		}
		return err
	}
}

type schedulerStaticActivationRaceHook struct {
	bucket        service.SchedulerBucket
	publishWinner func(context.Context) error
	fired         atomic.Bool
}

func (h *schedulerStaticActivationRaceHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *schedulerStaticActivationRaceHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		if err == nil && (cmd.Name() == "eval" || cmd.Name() == "evalsha") && redisCommandContainsKey(cmd, schedulerSnapshotKey(h.bucket, "1")) && h.fired.CompareAndSwap(false, true) {
			// Publish a complete newer state after this version is materialized but before activation.
			return h.publishWinner(ctx)
		}
		return err
	}
}

func (h *schedulerStaticActivationRaceHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

type schedulerStaticActivationResponseLostHook struct {
	bucket      service.SchedulerBucket
	publishNext func(context.Context) error
	fired       atomic.Bool
}

func (h *schedulerStaticActivationResponseLostHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (h *schedulerStaticActivationResponseLostHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		if err == nil && (cmd.Name() == "eval" || cmd.Name() == "evalsha") && redisCommandContainsKey(cmd, schedulerBucketKey(schedulerActivePrefix, h.bucket)) && h.fired.CompareAndSwap(false, true) {
			if publishErr := h.publishNext(ctx); publishErr != nil {
				return publishErr
			}
			// Redis committed activation, but the caller lost its response.
			return errors.New("injected lost static-state activation response")
		}
		return err
	}
}

func (h *schedulerStaticActivationResponseLostHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func TestSchedulerCache_StaticStateLostActivationResponsePreservesReaderGrace(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	bucket := service.SchedulerBucket{GroupID: 13, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
	v1Candidate := service.Account{ID: 60, Name: "v1 candidate", Platform: service.PlatformOpenAI}
	v1Support := service.Account{ID: 61, Name: "v1 support", Platform: service.PlatformOpenAI}
	v2 := service.Account{ID: 62, Name: "v2", Platform: service.PlatformOpenAI}
	v2Cache, ok := newSchedulerCacheWithChunkSizes(
		redis.NewClient(&redis.Options{Addr: cache.rdb.Options().Addr}),
		defaultSchedulerSnapshotMGetChunkSize,
		defaultSchedulerSnapshotWriteChunkSize,
	).(*schedulerCache)
	require.True(t, ok)
	t.Cleanup(func() { _ = v2Cache.rdb.Close() })

	hook := &schedulerStaticActivationResponseLostHook{bucket: bucket, publishNext: func(ctx context.Context) error {
		return v2Cache.SetStaticState(ctx, bucket, []service.Account{v2}, []service.Account{v2})
	}}
	cache.rdb.AddHook(hook)
	err := cache.SetStaticState(ctx, bucket, []service.Account{v1Candidate}, []service.Account{v1Candidate, v1Support})
	require.ErrorContains(t, err, "injected lost static-state activation response")
	require.True(t, hook.fired.Load())
	require.Equal(t, "2", cache.rdb.Get(ctx, schedulerBucketKey(schedulerActivePrefix, bucket)).Val())

	candidates, candidateHit, err := cache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	support, supportHit, err := cache.GetPersistentSupport(ctx, bucket)
	require.NoError(t, err)
	require.True(t, candidateHit)
	require.True(t, supportHit)
	require.Equal(t, []int64{v2.ID}, schedulerCacheTestIDs(candidates))
	require.Equal(t, []int64{v2.ID}, schedulerCacheTestIDs(support))

	for _, key := range []string{
		schedulerSnapshotKey(bucket, "1"),
		schedulerSupportKey(bucket, "1"),
		schedulerVersionedAccountKey(bucket, "1", "60"),
		schedulerVersionedAccountMetaKey(bucket, "1", "60"),
		schedulerVersionedSupportAccountKey(bucket, "1", "60"),
		schedulerVersionedSupportAccountMetaKey(bucket, "1", "60"),
		schedulerVersionedSupportAccountKey(bucket, "1", "61"),
		schedulerVersionedSupportAccountMetaKey(bucket, "1", "61"),
	} {
		ttl := cache.rdb.TTL(ctx, key).Val()
		require.GreaterOrEqual(t, ttl, time.Duration(snapshotGraceTTLSeconds)*time.Second, key)
		require.LessOrEqual(t, ttl, time.Duration(staticStateUnpublishedPayloadTTLSeconds)*time.Second, key)
	}
}

func TestSchedulerCache_StaticStateAmbiguousPayloadPipelineFailureCleansUnpublishedPayloads(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	bucket := service.SchedulerBucket{GroupID: 11, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
	old := service.Account{ID: 40, Name: "old", Platform: service.PlatformOpenAI}
	replacement := service.Account{ID: 41, Name: "replacement", Platform: service.PlatformOpenAI}
	supportOnly := service.Account{ID: 42, Name: "support", Platform: service.PlatformOpenAI}
	require.NoError(t, cache.SetStaticState(ctx, bucket, []service.Account{old}, []service.Account{old}))
	oldVersion := cache.rdb.Get(ctx, schedulerBucketKey(schedulerActivePrefix, bucket)).Val()

	hook := &schedulerStaticPayloadPipelineFailureHook{}
	cache.rdb.AddHook(hook)
	err := cache.SetStaticState(ctx, bucket, []service.Account{replacement}, []service.Account{replacement, supportOnly})
	require.ErrorContains(t, err, "injected ambiguous static payload pipeline failure")
	require.Equal(t, int32(1), hook.pipelineCalls.Load())

	candidates, hit, err := cache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	require.True(t, hit)
	require.Equal(t, []int64{old.ID}, schedulerCacheTestIDs(candidates))
	require.Equal(t, oldVersion, cache.rdb.Get(ctx, schedulerBucketKey(schedulerActivePrefix, bucket)).Val())
	for _, account := range []service.Account{replacement, supportOnly} {
		id := strconv.FormatInt(account.ID, 10)
		require.Zero(t, cache.rdb.Exists(ctx,
			schedulerVersionedAccountKey(bucket, "2", id),
			schedulerVersionedAccountMetaKey(bucket, "2", id),
			schedulerVersionedSupportAccountKey(bucket, "2", id),
			schedulerVersionedSupportAccountMetaKey(bucket, "2", id),
		).Val())
	}
}

type schedulerStaticMembershipAmbiguousWriteHook struct {
	targetKey string
	failed    atomic.Bool
}

func (h *schedulerStaticMembershipAmbiguousWriteHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (h *schedulerStaticMembershipAmbiguousWriteHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "del" && redisCommandContainsKey(cmd, h.targetKey) {
			// Keep the server-accepted membership key around to exercise the TTL fallback.
			return errors.New("injected static membership cleanup failure")
		}
		err := next(ctx, cmd)
		if err == nil && (cmd.Name() == "eval" || cmd.Name() == "evalsha") && redisCommandContainsKey(cmd, h.targetKey) && h.failed.CompareAndSwap(false, true) {
			// Redis accepted the ZADD, but the client lost the response.
			return errors.New("injected ambiguous static membership zadd failure")
		}
		return err
	}
}

func (h *schedulerStaticMembershipAmbiguousWriteHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		err := next(ctx, cmds)
		if err != nil {
			return err
		}
		for _, cmd := range cmds {
			if (cmd.Name() == "eval" || cmd.Name() == "evalsha") && redisCommandContainsKey(cmd, h.targetKey) && h.failed.CompareAndSwap(false, true) {
				// Both ZADD and its paired expiry reached Redis before the response was lost.
				return errors.New("injected ambiguous static membership zadd failure")
			}
		}
		return nil
	}
}

func redisCommandContainsKey(cmd redis.Cmder, key string) bool {
	for _, arg := range cmd.Args()[1:] {
		if arg == key {
			return true
		}
	}
	return false
}

func TestSchedulerCache_StaticStateAmbiguousMembershipWriteBoundsUnpublishedZSets(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name          string
		membershipKey func(service.SchedulerBucket) string
		wantKeys      func(service.SchedulerBucket) []string
	}{
		{
			name: "candidate",
			membershipKey: func(bucket service.SchedulerBucket) string {
				return schedulerSnapshotKey(bucket, "1")
			},
			wantKeys: func(bucket service.SchedulerBucket) []string {
				return []string{schedulerSnapshotKey(bucket, "1")}
			},
		},
		{
			name: "support",
			membershipKey: func(bucket service.SchedulerBucket) string {
				return schedulerSupportKey(bucket, "1")
			},
			wantKeys: func(bucket service.SchedulerBucket) []string {
				return []string{schedulerSnapshotKey(bucket, "1"), schedulerSupportKey(bucket, "1")}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := newSchedulerCacheUnit(t)
			bucket := service.SchedulerBucket{GroupID: 24, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
			hook := &schedulerStaticMembershipAmbiguousWriteHook{targetKey: tc.membershipKey(bucket)}
			cache.rdb.AddHook(hook)

			err := cache.SetStaticState(ctx, bucket,
				[]service.Account{{ID: 90, Platform: service.PlatformOpenAI}},
				[]service.Account{{ID: 90, Platform: service.PlatformOpenAI}, {ID: 91, Platform: service.PlatformOpenAI}},
			)
			require.ErrorContains(t, err, "injected ambiguous static membership zadd failure")
			require.True(t, hook.failed.Load())
			_, err = cache.rdb.Get(ctx, schedulerBucketKey(schedulerActivePrefix, bucket)).Result()
			require.ErrorIs(t, err, redis.Nil)

			for _, key := range tc.wantKeys(bucket) {
				ttl := cache.rdb.TTL(ctx, key).Val()
				require.Greater(t, ttl, time.Duration(0), key)
				require.LessOrEqual(t, ttl, time.Duration(staticStateUnpublishedPayloadTTLSeconds)*time.Second, key)
			}
		})
	}
}

func TestSchedulerCache_StaticStatePublicationPersistsActiveMembershipZSets(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	bucket := service.SchedulerBucket{GroupID: 25, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
	candidate := service.Account{ID: 92, Platform: service.PlatformOpenAI}
	support := service.Account{ID: 93, Platform: service.PlatformOpenAI}

	require.NoError(t, cache.SetStaticState(ctx, bucket, []service.Account{candidate}, []service.Account{candidate, support}))
	version := cache.rdb.Get(ctx, schedulerBucketKey(schedulerActivePrefix, bucket)).Val()
	for _, key := range []string{schedulerSnapshotKey(bucket, version), schedulerSupportKey(bucket, version)} {
		require.Equal(t, time.Duration(-1), cache.rdb.TTL(ctx, key).Val(), key)
	}
}

func TestSchedulerCache_StaticStateStaleActivationCleansUnpublishedPayloads(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	bucket := service.SchedulerBucket{GroupID: 12, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
	candidate := service.Account{ID: 50, Platform: service.PlatformOpenAI}
	support := service.Account{ID: 51, Platform: service.PlatformOpenAI}
	winner := service.Account{ID: 52, Platform: service.PlatformOpenAI}
	winnerCache, ok := newSchedulerCacheWithChunkSizes(
		redis.NewClient(&redis.Options{Addr: cache.rdb.Options().Addr}),
		defaultSchedulerSnapshotMGetChunkSize,
		defaultSchedulerSnapshotWriteChunkSize,
	).(*schedulerCache)
	require.True(t, ok)
	t.Cleanup(func() { _ = winnerCache.rdb.Close() })

	hook := &schedulerStaticActivationRaceHook{bucket: bucket, publishWinner: func(ctx context.Context) error {
		return winnerCache.SetStaticState(ctx, bucket, []service.Account{winner}, []service.Account{winner})
	}}
	cache.rdb.AddHook(hook)
	err := cache.SetStaticState(ctx, bucket, []service.Account{candidate}, []service.Account{candidate, support})
	require.ErrorContains(t, err, "static state version was superseded")
	require.True(t, hook.fired.Load())
	require.Equal(t, "2", cache.rdb.Get(ctx, schedulerBucketKey(schedulerActivePrefix, bucket)).Val())
	require.Zero(t, cache.rdb.Exists(ctx,
		schedulerSnapshotKey(bucket, "1"),
		schedulerSupportKey(bucket, "1"),
		schedulerVersionedAccountKey(bucket, "1", "50"),
		schedulerVersionedAccountMetaKey(bucket, "1", "50"),
		schedulerVersionedSupportAccountKey(bucket, "1", "50"),
		schedulerVersionedSupportAccountMetaKey(bucket, "1", "50"),
		schedulerVersionedSupportAccountKey(bucket, "1", "51"),
		schedulerVersionedSupportAccountMetaKey(bucket, "1", "51"),
	).Val())
	candidates, hit, err := cache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	require.True(t, hit)
	require.Equal(t, []int64{winner.ID}, schedulerCacheTestIDs(candidates))
}

func schedulerCacheTestIDs(accounts []*service.Account) []int64 {
	ids := make([]int64, 0, len(accounts))
	for _, account := range accounts {
		if account != nil {
			ids = append(ids, account.ID)
		}
	}
	return ids
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

func TestSchedulerCacheSetAccountsWritesAllAccountsInOneCall(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)

	accounts := []service.Account{
		{ID: 201, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey},
		{ID: 202, Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey},
		{ID: 203, Platform: service.PlatformGemini, Type: service.AccountTypeAPIKey},
	}
	require.NoError(t, cache.SetAccounts(ctx, accounts))

	for _, id := range []int64{201, 202, 203} {
		got, err := cache.GetAccount(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, got)
		require.Equal(t, id, got.ID)
	}
	// 空输入是幂等空操作。
	require.NoError(t, cache.SetAccounts(ctx, nil))
}

func TestSchedulerCacheSetAccountsClearsUnencodablePayload(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	invalidTime := time.Date(10000, time.January, 1, 0, 0, 0, 0, time.UTC)

	require.NoError(t, cache.SetAccounts(ctx, []service.Account{
		{ID: 204, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey},
		{ID: 205, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, ExpiresAt: &invalidTime},
	}))

	// 不可编码账号被删除（连同 meta/last_used 侧键），其余账号正常写入。
	got, err := cache.GetAccount(ctx, 204)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, int64(204), got.ID)
	require.Zero(t, cache.rdb.Exists(ctx, schedulerAccountKey("205"), schedulerAccountMetaKey("205"), schedulerLastUsedKey("205")).Val())
}
