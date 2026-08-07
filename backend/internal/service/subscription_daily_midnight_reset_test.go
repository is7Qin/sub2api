package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/stretchr/testify/require"
)

type dailyMidnightResetRepo struct {
	userSubRepoNoop
	resetCalled    bool
	newWindowStart time.Time
}

func (r *dailyMidnightResetRepo) ResetDailyUsage(_ context.Context, _ int64, _ *time.Time, newWindowStart time.Time) error {
	r.resetCalled = true
	r.newWindowStart = newWindowStart
	return nil
}

func midnightTestBase() time.Time {
	return timezone.StartOfDay(time.Date(2026, 8, 6, 12, 0, 0, 0, timezone.Location()))
}

func newMidnightTestSub(dailyWindowStart time.Time, base time.Time) *UserSubscription {
	start := dailyWindowStart
	return &UserSubscription{
		ID: 1, UserID: 10, GroupID: 20,
		StartsAt: base.AddDate(0, 0, -3), ExpiresAt: base.AddDate(0, 0, 30),
		DailyUsageUSD: 43.34, DailyWindowStart: &start,
	}
}

func TestUserSubscriptionNeedsDailyResetAtUsesCalendarDay(t *testing.T) {
	base := midnightTestBase()
	sub := newMidnightTestSub(base.Add(16*time.Hour+49*time.Minute), base)

	require.False(t, sub.NeedsDailyResetAt(base.Add(23*time.Hour+59*time.Minute)))
	require.True(t, sub.NeedsDailyResetAt(base.AddDate(0, 0, 1).Add(time.Minute)))
}

func TestCheckAndResetWindowsDailyUsesMidnightAfterManualReset(t *testing.T) {
	base := midnightTestBase()
	now := base.AddDate(0, 0, 1).Add(5 * time.Minute)
	repo := &dailyMidnightResetRepo{}
	svc := NewSubscriptionService(groupRepoNoop{}, repo, nil, nil, nil)
	svc.now = func() time.Time { return now }
	sub := newMidnightTestSub(base.Add(16*time.Hour+49*time.Minute), base)

	require.NoError(t, svc.CheckAndResetWindows(context.Background(), sub))
	require.True(t, repo.resetCalled)
	require.Equal(t, timezone.StartOfDay(now), repo.newWindowStart)
	require.Zero(t, sub.DailyUsageUSD)
}

func TestCheckAndResetWindowsDailyDoesNotResetWithinCalendarDay(t *testing.T) {
	base := midnightTestBase()
	repo := &dailyMidnightResetRepo{}
	svc := NewSubscriptionService(groupRepoNoop{}, repo, nil, nil, nil)
	svc.now = func() time.Time { return base.Add(23*time.Hour + 59*time.Minute) }
	sub := newMidnightTestSub(base.Add(16*time.Hour+49*time.Minute), base)

	require.NoError(t, svc.CheckAndResetWindows(context.Background(), sub))
	require.False(t, repo.resetCalled)
	require.Equal(t, 43.34, sub.DailyUsageUSD)
}

func TestNormalizeExpiredWindowsDailyClearsAfterMidnight(t *testing.T) {
	base := midnightTestBase()
	subs := []UserSubscription{*newMidnightTestSub(base.Add(16*time.Hour+49*time.Minute), base)}

	normalizeExpiredWindowsAt(subs, base.AddDate(0, 0, 1).Add(time.Minute))

	require.Zero(t, subs[0].DailyUsageUSD)
	require.Nil(t, subs[0].DailyWindowStart)
}

func TestDailyResetTimeUsesNextMidnight(t *testing.T) {
	base := midnightTestBase()
	sub := newMidnightTestSub(base.Add(16*time.Hour+49*time.Minute), base)

	resetAt := sub.DailyResetTime()
	require.NotNil(t, resetAt)
	require.Equal(t, base.AddDate(0, 0, 1), *resetAt)
}

func TestCheckAndResetWindowsOneTimeDailyCardRemainsExempt(t *testing.T) {
	base := midnightTestBase()
	anchor := base
	repo := &dailyMidnightResetRepo{}
	svc := NewSubscriptionService(groupRepoNoop{}, repo, nil, nil, nil)
	svc.now = func() time.Time { return base.AddDate(0, 0, 1).Add(2 * time.Hour) }
	sub := &UserSubscription{
		ID: 1, StartsAt: base.Add(17 * time.Hour), ExpiresAt: base.Add(41 * time.Hour),
		DailyWindowStart: &anchor, DailyUsageUSD: 10,
	}

	require.NoError(t, svc.CheckAndResetWindows(context.Background(), sub))
	require.False(t, repo.resetCalled)
	require.Equal(t, 10.0, sub.DailyUsageUSD)
}
