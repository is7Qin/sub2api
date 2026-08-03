//go:build unit

package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestSchedulerStaticCandidateStateSupportsListRefreshAndHydration(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	cache := repository.NewSchedulerCache(rdb)

	groupID := int64(10441)
	resetAt := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	overloadUntil := time.Now().Add(2 * time.Minute).UTC().Truncate(time.Second)
	candidate := service.Account{
		ID:               37441,
		Platform:         service.PlatformOpenAI,
		Type:             service.AccountTypeAPIKey,
		Status:           service.StatusActive,
		Schedulable:      true,
		Concurrency:      1,
		GroupIDs:         []int64{groupID},
		RateLimitResetAt: &resetAt,
		OverloadUntil:    &overloadUntil,
		Credentials: map[string]any{
			"api_key":             "static-candidate-secret",
			"openai_capabilities": []any{"chat_completions"},
		},
	}
	support := service.Account{ID: candidate.ID, Platform: service.PlatformOpenAI, Status: service.StatusActive, Schedulable: true}
	bucket := service.SchedulerBucket{GroupID: groupID, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
	require.NoError(t, cache.(service.SchedulerStaticStateCache).SetStaticState(ctx, bucket, []service.Account{candidate}, []service.Account{support}))

	snapshot := service.NewSchedulerSnapshotService(cache, nil, nil, nil, nil)
	listed, _, err := snapshot.ListSchedulableAccounts(ctx, &groupID, service.PlatformOpenAI, false)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Empty(t, listed[0].GetCredential("api_key"), "candidate list must retain the slim metadata projection")

	refreshed, err := snapshot.GetStaticCandidateAccountsByIDs(ctx, &groupID, service.PlatformOpenAI, false, []int64{candidate.ID, 999, candidate.ID})
	require.NoError(t, err)
	require.Len(t, refreshed, 1)
	require.Equal(t, resetAt, *refreshed[candidate.ID].RateLimitResetAt)
	require.Equal(t, overloadUntil, *refreshed[candidate.ID].OverloadUntil)

	hydrated, err := snapshot.GetStaticCandidateAccount(ctx, &groupID, service.PlatformOpenAI, false, candidate.ID)
	require.NoError(t, err)
	require.NotNil(t, hydrated)
	require.Equal(t, "static-candidate-secret", hydrated.GetCredential("api_key"))
	require.Equal(t, resetAt, *hydrated.RateLimitResetAt)
	require.Equal(t, overloadUntil, *hydrated.OverloadUntil)
}
