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

func TestSchedulerStaticCandidatesOverlayImmediateModelCooldownWithoutChangingPersistentSupport(t *testing.T) {
	ctx := context.Background()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	cache := repository.NewSchedulerCache(rdb)

	groupID := int64(10442)
	candidate := service.Account{
		ID:          37442,
		Platform:    service.PlatformAntigravity,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 1,
		GroupIDs:    []int64{groupID},
		Credentials: map[string]any{
			"model_mapping": map[string]any{"claude-test": "claude-test"},
		},
	}
	support := service.Account{
		ID:          candidate.ID,
		Platform:    candidate.Platform,
		Status:      service.StatusActive,
		Schedulable: true,
	}
	bucket := service.SchedulerBucket{GroupID: groupID, Platform: service.PlatformAntigravity, Mode: service.SchedulerModeSingle}
	require.NoError(t, cache.(service.SchedulerStaticStateCache).SetStaticState(ctx, bucket, []service.Account{candidate}, []service.Account{support}))

	snapshot := service.NewSchedulerSnapshotService(cache, nil, nil, nil, nil)
	listed, _, err := snapshot.ListSchedulableAccounts(ctx, &groupID, service.PlatformAntigravity, false)
	require.NoError(t, err)
	require.Len(t, listed, 1, "prime the static candidate decode cache before the runtime update")

	cooldown := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	updated := candidate
	updated.Extra = map[string]any{
		"model_rate_limits": map[string]any{
			"claude-test": map[string]any{"rate_limit_reset_at": cooldown.Format(time.RFC3339)},
		},
	}
	require.NoError(t, snapshot.UpdateAccountInCache(ctx, &updated))

	listed, _, err = snapshot.ListSchedulableAccounts(ctx, &groupID, service.PlatformAntigravity, false)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.False(t, listed[0].IsSchedulableForModelWithContext(ctx, "claude-test"), "request selection must observe the immediate cache-only cooldown")

	persistent, hit, err := snapshot.ListPersistentSupport(ctx, &groupID, service.PlatformAntigravity, false)
	require.NoError(t, err)
	require.True(t, hit)
	require.Len(t, persistent, 1)
	require.Nil(t, persistent[0].Extra, "persistent model-support diagnostics must retain their static projection")
}
