//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type schedulerCancellationCache struct {
	SchedulerCache
	cancel         context.CancelFunc
	snapshotWrites int
}

func (c *schedulerCancellationCache) GetSnapshot(ctx context.Context, _ SchedulerBucket) ([]*Account, bool, error) {
	c.cancel()
	return nil, false, ctx.Err()
}

func (c *schedulerCancellationCache) SetSnapshot(context.Context, SchedulerBucket, []Account) error {
	c.snapshotWrites++
	return nil
}

func (c *schedulerCancellationCache) GetAccount(ctx context.Context, _ int64) (*Account, error) {
	c.cancel()
	return nil, ctx.Err()
}

type schedulerCancellationAccountRepo struct {
	AccountRepository
	listCalls    int
	getByIDCalls int
}

func (r *schedulerCancellationAccountRepo) ListSchedulableUngroupedByPlatform(context.Context, string) ([]Account, error) {
	r.listCalls++
	return nil, nil
}

func (r *schedulerCancellationAccountRepo) GetByID(context.Context, int64) (*Account, error) {
	r.getByIDCalls++
	return nil, nil
}

func TestSchedulerSnapshotListStopsAfterRequestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cache := &schedulerCancellationCache{cancel: cancel}
	repo := &schedulerCancellationAccountRepo{}
	svc := NewSchedulerSnapshotService(cache, nil, repo, nil, nil)

	accounts, useMixed, err := svc.ListSchedulableAccounts(ctx, nil, PlatformOpenAI, false)

	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, accounts)
	require.False(t, useMixed)
	require.Zero(t, cache.snapshotWrites, "canceled requests must not publish a fallback snapshot")
	require.Zero(t, repo.listCalls, "canceled requests must not fall back to the database")
}

func TestSchedulerSnapshotGetAccountStopsAfterRequestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cache := &schedulerCancellationCache{cancel: cancel}
	repo := &schedulerCancellationAccountRepo{}
	svc := NewSchedulerSnapshotService(cache, nil, repo, nil, nil)

	account, err := svc.GetAccount(ctx, 42)

	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, account)
	require.Zero(t, repo.getByIDCalls, "canceled requests must not fall back to the database")
}
