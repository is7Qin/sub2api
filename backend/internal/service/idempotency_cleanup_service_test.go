package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type idempotencyCleanupRepoStub struct {
	deleteCalls int
	lastLimit   int
	deleteErr   error
	deleteFn    func(context.Context, time.Time, int) (int64, error)
}

func (r *idempotencyCleanupRepoStub) CreateProcessing(context.Context, *IdempotencyRecord) (bool, error) {
	return false, nil
}
func (r *idempotencyCleanupRepoStub) GetByScopeAndKeyHash(context.Context, string, string) (*IdempotencyRecord, error) {
	return nil, nil
}
func (r *idempotencyCleanupRepoStub) TryReclaim(context.Context, int64, string, time.Time, time.Time, time.Time) (bool, error) {
	return false, nil
}
func (r *idempotencyCleanupRepoStub) ExtendProcessingLock(context.Context, int64, string, time.Time, time.Time) (bool, error) {
	return false, nil
}
func (r *idempotencyCleanupRepoStub) MarkSucceeded(context.Context, int64, int, string, time.Time) error {
	return nil
}
func (r *idempotencyCleanupRepoStub) MarkFailedRetryable(context.Context, int64, string, time.Time, time.Time) error {
	return nil
}
func (r *idempotencyCleanupRepoStub) DeleteExpired(ctx context.Context, now time.Time, limit int) (int64, error) {
	r.deleteCalls++
	r.lastLimit = limit
	if r.deleteFn != nil {
		return r.deleteFn(ctx, now, limit)
	}
	if r.deleteErr != nil {
		return 0, r.deleteErr
	}
	return 1, nil
}

func TestNewIdempotencyCleanupService_UsesConfig(t *testing.T) {
	repo := &idempotencyCleanupRepoStub{}
	cfg := &config.Config{
		Idempotency: config.IdempotencyConfig{
			CleanupIntervalSeconds: 7,
			CleanupBatchSize:       321,
		},
	}
	svc := NewIdempotencyCleanupService(repo, cfg)
	require.Equal(t, 7*time.Second, svc.interval)
	require.Equal(t, 321, svc.batch)
}

func TestIdempotencyCleanupRunPreservesConfiguredBatchAndDeadline(t *testing.T) {
	cfg := &config.Config{}
	cfg.Idempotency.CleanupBatchSize = 37
	svc := NewIdempotencyCleanupService(&idempotencyCleanupRepoStub{deleteFn: func(ctx context.Context, _ time.Time, batch int) (int64, error) {
		require.Equal(t, 37, batch)
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		require.WithinDuration(t, time.Now().Add(10*time.Second), deadline, 200*time.Millisecond)
		return 1, nil
	}}, cfg)

	require.NoError(t, svc.Run(context.Background()))
}

func TestIdempotencyCleanupRunReturnsRepositoryError(t *testing.T) {
	repoErr := errors.New("cleanup failed")
	svc := NewIdempotencyCleanupService(&idempotencyCleanupRepoStub{deleteErr: repoErr}, nil)

	require.ErrorIs(t, svc.Run(context.Background()), repoErr)
}

func TestNewIdempotencyCleanupServiceUsesDefaults(t *testing.T) {
	svc := NewIdempotencyCleanupService(&idempotencyCleanupRepoStub{}, nil)

	require.Equal(t, 60*time.Second, svc.Interval())
	require.Equal(t, 500, svc.BatchSize())
}

func TestNewIdempotencyCleanupServiceDoesNotStartWork(t *testing.T) {
	called := make(chan struct{}, 1)
	NewIdempotencyCleanupService(&idempotencyCleanupRepoStub{deleteFn: func(context.Context, time.Time, int) (int64, error) {
		called <- struct{}{}
		return 0, nil
	}}, nil)

	select {
	case <-called:
		t.Fatal("constructor started idempotency cleanup work")
	case <-time.After(25 * time.Millisecond):
	}
}
