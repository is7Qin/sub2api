package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type rebuildBackoffCache struct {
	outboxPollCache
	listBuckets []SchedulerBucket
	setErr      error
	setAttempts int
	lockCalls   int
}

func (c *rebuildBackoffCache) ListBuckets(context.Context) ([]SchedulerBucket, error) {
	return c.listBuckets, nil
}

func (c *rebuildBackoffCache) TryLockBucket(context.Context, SchedulerBucket, time.Duration) (string, bool, error) {
	c.lockCalls++
	return "test-lock", true, nil
}

func (c *rebuildBackoffCache) UnlockBucket(context.Context, SchedulerBucket, string) error {
	return nil
}

func (c *rebuildBackoffCache) SetSnapshot(context.Context, SchedulerBucket, []Account) error {
	c.setAttempts++
	return c.setErr
}

type rebuildBackoffAccountRepo struct {
	AccountRepository
	accounts []Account
}

func (r *rebuildBackoffAccountRepo) ListSchedulableUngroupedByPlatform(context.Context, string) ([]Account, error) {
	return r.accounts, nil
}

func (r *rebuildBackoffAccountRepo) ListSchedulableByGroupIDAndPlatform(context.Context, int64, string) ([]Account, error) {
	return r.accounts, nil
}

func (r *rebuildBackoffAccountRepo) ListSchedulableByGroupIDAndPlatforms(context.Context, int64, []string) ([]Account, error) {
	return r.accounts, nil
}

func TestSchedulerRebuildRetryDelayExponential(t *testing.T) {
	for _, tc := range []struct {
		failures int
		want     time.Duration
	}{
		{failures: 1, want: 5 * time.Second},
		{failures: 2, want: 10 * time.Second},
		{failures: 3, want: 20 * time.Second},
		{failures: 4, want: 40 * time.Second},
		{failures: 5, want: 80 * time.Second},
		{failures: 6, want: 160 * time.Second},
		{failures: 7, want: 300 * time.Second},
		{failures: 12, want: 300 * time.Second},
	} {
		require.Equal(t, tc.want, outboxRebuildRetryDelay(tc.failures))
	}
}

func TestSchedulerCheckOutboxLagBacksOffAfterFailedRebuild(t *testing.T) {
	bucket := SchedulerBucket{GroupID: 0, Platform: PlatformOpenAI, Mode: SchedulerModeSingle}
	cache := &rebuildBackoffCache{
		listBuckets: []SchedulerBucket{bucket},
		setErr:      errors.New("rebuild failed"),
	}
	svc := &SchedulerSnapshotService{
		cache:       cache,
		accountRepo: &rebuildBackoffAccountRepo{},
		cfg: &config.Config{Gateway: config.GatewayConfig{Scheduling: config.GatewaySchedulingConfig{
			OutboxLagRebuildSeconds:  5,
			OutboxLagRebuildFailures: 2,
		}}},
	}
	oldest := SchedulerOutboxEvent{CreatedAt: time.Now().Add(-time.Minute)}

	svc.checkOutboxLag(context.Background(), oldest, 0)
	require.Zero(t, cache.lockCalls, "未达到失败阈值不得触发重建")

	svc.checkOutboxLag(context.Background(), oldest, 0)
	require.Equal(t, 1, cache.lockCalls)
	require.Equal(t, 1, cache.setAttempts)
	require.Equal(t, 1, svc.outboxRebuildFailures)
	require.False(t, svc.outboxRebuildRetryAt.IsZero())
	require.True(t, svc.outboxRebuildRetryAt.After(time.Now().Add(4*time.Second)), "首次失败后的重试必须至少推迟 5s")
	require.Equal(t, "outbox_lag", svc.outboxRebuildRetryReason)

	// 退避窗口内（下一轮 poll）不得再次触发重建
	svc.checkOutboxLag(context.Background(), oldest, 0)
	require.Equal(t, 1, cache.lockCalls)
	require.Equal(t, 1, cache.setAttempts)

	// 退避到期后按原原因重试，失败后继续翻倍
	svc.outboxRebuildRetryAt = time.Now().Add(-time.Second)
	svc.checkOutboxLag(context.Background(), oldest, 0)
	require.Equal(t, 2, cache.lockCalls)
	require.Equal(t, 2, cache.setAttempts)
	require.Equal(t, 2, svc.outboxRebuildFailures)
}

func TestSchedulerCheckOutboxLagClearsRetryStateAfterSuccessfulRebuild(t *testing.T) {
	bucket := SchedulerBucket{GroupID: 0, Platform: PlatformOpenAI, Mode: SchedulerModeSingle}
	cache := &rebuildBackoffCache{
		listBuckets: []SchedulerBucket{bucket},
		setErr:      errors.New("rebuild failed"),
	}
	svc := &SchedulerSnapshotService{
		cache:       cache,
		accountRepo: &rebuildBackoffAccountRepo{},
		cfg: &config.Config{Gateway: config.GatewayConfig{Scheduling: config.GatewaySchedulingConfig{
			OutboxLagRebuildSeconds:  5,
			OutboxLagRebuildFailures: 1,
		}}},
	}
	oldest := SchedulerOutboxEvent{CreatedAt: time.Now().Add(-time.Minute)}

	svc.checkOutboxLag(context.Background(), oldest, 0)
	require.Equal(t, 1, svc.outboxRebuildFailures)
	require.False(t, svc.outboxRebuildRetryAt.IsZero())

	// 退避到期后的重试成功：失败计数与重试时间必须清除
	cache.setErr = nil
	svc.outboxRebuildRetryAt = time.Now().Add(-time.Second)
	svc.checkOutboxLag(context.Background(), oldest, 0)
	require.Zero(t, svc.outboxRebuildFailures)
	require.True(t, svc.outboxRebuildRetryAt.IsZero())
	require.Empty(t, svc.outboxRebuildRetryReason)
}

func TestSchedulerCheckOutboxLagClearsRetryStateWhenConditionRecovers(t *testing.T) {
	bucket := SchedulerBucket{GroupID: 0, Platform: PlatformOpenAI, Mode: SchedulerModeSingle}
	cache := &rebuildBackoffCache{
		listBuckets: []SchedulerBucket{bucket},
		setErr:      errors.New("rebuild failed"),
	}
	svc := &SchedulerSnapshotService{
		cache:       cache,
		accountRepo: &rebuildBackoffAccountRepo{},
		cfg: &config.Config{Gateway: config.GatewayConfig{Scheduling: config.GatewaySchedulingConfig{
			OutboxLagRebuildSeconds:  5,
			OutboxLagRebuildFailures: 1,
		}}},
	}
	oldest := SchedulerOutboxEvent{CreatedAt: time.Now().Add(-time.Minute)}
	svc.checkOutboxLag(context.Background(), oldest, 0)
	require.False(t, svc.outboxRebuildRetryAt.IsZero())

	// 条件恢复（无 lag、无 backlog）后必须清除退避状态，避免旧失败污染下一轮退化
	fresh := SchedulerOutboxEvent{CreatedAt: time.Now()}
	svc.checkOutboxLag(context.Background(), fresh, 0)
	require.Zero(t, svc.outboxRebuildFailures)
	require.True(t, svc.outboxRebuildRetryAt.IsZero())
	require.Empty(t, svc.outboxRebuildRetryReason)
}

func TestSchedulerCheckOutboxLagBacklogBacksOffAfterFailedRebuild(t *testing.T) {
	bucket := SchedulerBucket{GroupID: 0, Platform: PlatformOpenAI, Mode: SchedulerModeSingle}
	cache := &rebuildBackoffCache{
		listBuckets: []SchedulerBucket{bucket},
		setErr:      errors.New("rebuild failed"),
	}
	repo := &outboxPollRepo{rows: []int64{5, 100}}
	svc := &SchedulerSnapshotService{
		cache:       cache,
		outboxRepo:  repo,
		accountRepo: &rebuildBackoffAccountRepo{},
		cfg: &config.Config{Gateway: config.GatewayConfig{Scheduling: config.GatewaySchedulingConfig{
			OutboxBacklogRebuildRows: 10,
		}}},
	}
	oldest := SchedulerOutboxEvent{CreatedAt: time.Now().Add(-time.Minute)}

	svc.checkOutboxLag(context.Background(), oldest, 0)
	require.Equal(t, 1, cache.lockCalls)
	require.Equal(t, 1, cache.setAttempts)
	require.Equal(t, "outbox_backlog", svc.outboxRebuildRetryReason)

	// backlog 仍超标，但退避窗口内不得再次触发
	svc.checkOutboxLag(context.Background(), oldest, 0)
	require.Equal(t, 1, cache.lockCalls)
	require.Equal(t, 1, cache.setAttempts)

	// 退避到期后按原原因重试
	svc.outboxRebuildRetryAt = time.Now().Add(-time.Second)
	svc.checkOutboxLag(context.Background(), oldest, 0)
	require.Equal(t, 2, cache.lockCalls)
	require.Equal(t, 2, cache.setAttempts)
}

func TestSchedulerHandleDirtyWorkGlobalBacksOffAfterFailedRebuild(t *testing.T) {
	bucket := SchedulerBucket{GroupID: 0, Platform: PlatformOpenAI, Mode: SchedulerModeSingle}
	cache := &rebuildBackoffCache{
		listBuckets: []SchedulerBucket{bucket},
		setErr:      errors.New("rebuild failed"),
	}
	svc := &SchedulerSnapshotService{
		cache:       cache,
		accountRepo: &rebuildBackoffAccountRepo{},
	}
	ctx := context.Background()

	err := svc.handleDirtyWork(ctx, SchedulerDirtyWork{Kind: SchedulerDirtyWorkGlobal, EntityID: 0})
	require.Error(t, err)
	require.Equal(t, 1, cache.lockCalls)
	require.Equal(t, 1, svc.outboxRebuildFailures)
	require.False(t, svc.outboxRebuildRetryAt.IsZero())

	// 退避窗口内：同一轮 poll 再次处理 Global 项时不得执行重建（脏项保持挂起）
	err = svc.handleDirtyWork(ctx, SchedulerDirtyWork{Kind: SchedulerDirtyWorkGlobal, EntityID: 0})
	require.ErrorIs(t, err, errSchedulerRebuildRetryPending)
	require.Equal(t, 1, cache.lockCalls)

	// 退避到期后重试成功则清除状态
	cache.setErr = nil
	svc.outboxRebuildRetryAt = time.Now().Add(-time.Second)
	require.NoError(t, svc.handleDirtyWork(ctx, SchedulerDirtyWork{Kind: SchedulerDirtyWorkGlobal, EntityID: 0}))
	require.Zero(t, svc.outboxRebuildFailures)
	require.True(t, svc.outboxRebuildRetryAt.IsZero())
	require.Empty(t, svc.outboxRebuildRetryReason)
}

func TestSchedulerHandleDirtyWorkGroupNotBlockedByFullRebuildBackoff(t *testing.T) {
	bucket := SchedulerBucket{GroupID: 1, Platform: PlatformOpenAI, Mode: SchedulerModeSingle}
	cache := &rebuildBackoffCache{
		listBuckets: []SchedulerBucket{bucket},
		setErr:      errors.New("rebuild failed"),
	}
	svc := &SchedulerSnapshotService{
		cache:       cache,
		accountRepo: &rebuildBackoffAccountRepo{},
	}
	ctx := context.Background()

	require.Error(t, svc.handleDirtyWork(ctx, SchedulerDirtyWork{Kind: SchedulerDirtyWorkGlobal, EntityID: 0}))
	before := cache.lockCalls
	require.Equal(t, 1, before)

	// 全量重建处于退避窗口内时，分组重建不得被阻塞
	require.Error(t, svc.handleDirtyWork(ctx, SchedulerDirtyWork{Kind: SchedulerDirtyWorkGroup, EntityID: 1}))
	require.Greater(t, cache.lockCalls, before)
}
