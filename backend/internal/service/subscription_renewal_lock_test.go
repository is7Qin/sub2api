package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type lockingRenewalRepo struct {
	userSubRepoNoop
	mu        sync.Mutex
	stale     UserSubscription
	current   UserSubscription
	lockReads int
}

func (r *lockingRenewalRepo) GetByUserIDAndGroupID(context.Context, int64, int64) (*UserSubscription, error) {
	copy := r.stale
	return &copy, nil
}

func (r *lockingRenewalRepo) GetByID(_ context.Context, _ int64) (*UserSubscription, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	copy := r.current
	return &copy, nil
}

func (r *lockingRenewalRepo) GetByIDForUpdate(_ context.Context, _ int64) (*UserSubscription, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lockReads++
	copy := r.current
	return &copy, nil
}

func (r *lockingRenewalRepo) ExtendExpiry(_ context.Context, _ int64, expiresAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.current.ExpiresAt = expiresAt
	return nil
}

func (r *lockingRenewalRepo) UpdateStatus(_ context.Context, _ int64, status string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.current.Status = status
	return nil
}

func (r *lockingRenewalRepo) UpdateNotes(_ context.Context, _ int64, notes string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.current.Notes = notes
	return nil
}

func (r *lockingRenewalRepo) Update(_ context.Context, sub *UserSubscription) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.current = *sub
	return nil
}

func TestAssignOrExtendSubscriptionUsesLockedCurrentRow(t *testing.T) {
	now := time.Now()
	lockedExpiry := now.AddDate(0, 0, 20)
	windowStart := now.Add(-24 * time.Hour)
	repo := &lockingRenewalRepo{
		stale: UserSubscription{
			ID:        7,
			UserID:    11,
			GroupID:   13,
			ExpiresAt: now.Add(-time.Hour),
			Status:    SubscriptionStatusExpired,
			Notes:     "stale",
		},
		current: UserSubscription{
			ID:               7,
			UserID:           11,
			GroupID:          13,
			StartsAt:         now.AddDate(0, 0, -10),
			ExpiresAt:        lockedExpiry,
			Status:           SubscriptionStatusSuspended,
			Notes:            "current",
			DailyWindowStart: &windowStart,
			DailyUsageUSD:    4,
		},
	}
	svc := NewSubscriptionService(
		&subscriptionGroupRepoStub{group: &Group{ID: 13, SubscriptionType: SubscriptionTypeSubscription}},
		repo,
		nil,
		nil,
		nil,
	)

	sub, extended, err := svc.AssignOrExtendSubscription(context.Background(), &AssignSubscriptionInput{
		UserID:       11,
		GroupID:      13,
		ValidityDays: 5,
		Notes:        "renewed",
	})

	require.NoError(t, err)
	require.True(t, extended)
	require.Equal(t, 1, repo.lockReads)
	require.Equal(t, lockedExpiry.AddDate(0, 0, 5), sub.ExpiresAt)
	require.Equal(t, SubscriptionStatusActive, sub.Status)
	require.Equal(t, "current\nrenewed", sub.Notes)
	require.Equal(t, windowStart, *sub.DailyWindowStart)
	require.Equal(t, float64(4), sub.DailyUsageUSD)
}

func TestAssignOrExtendSubscriptionSerializedRenewalsAccumulateDays(t *testing.T) {
	now := time.Now()
	initialExpiry := now.AddDate(0, 0, 10)
	stale := UserSubscription{
		ID:        17,
		UserID:    21,
		GroupID:   23,
		StartsAt:  now,
		ExpiresAt: initialExpiry,
		Status:    SubscriptionStatusActive,
	}
	repo := &lockingRenewalRepo{stale: stale, current: stale}
	svc := NewSubscriptionService(
		&subscriptionGroupRepoStub{group: &Group{ID: 23, SubscriptionType: SubscriptionTypeSubscription}},
		repo,
		nil,
		nil,
		nil,
	)
	input := &AssignSubscriptionInput{UserID: 21, GroupID: 23, ValidityDays: 7}

	_, _, err := svc.AssignOrExtendSubscription(context.Background(), input)
	require.NoError(t, err)
	second, _, err := svc.AssignOrExtendSubscription(context.Background(), input)
	require.NoError(t, err)

	require.Equal(t, 2, repo.lockReads)
	require.Equal(t, initialExpiry.AddDate(0, 0, 14), second.ExpiresAt)
}
