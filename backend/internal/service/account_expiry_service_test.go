//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type accountExpiryRepoStub struct {
	AccountRepository
	autoPauseFn func(context.Context, time.Time) (int64, error)
}

func (r *accountExpiryRepoStub) AutoPauseExpiredAccounts(ctx context.Context, now time.Time) (int64, error) {
	return r.autoPauseFn(ctx, now)
}

func TestAccountExpiryRunUsesCallerDeadline(t *testing.T) {
	repo := &accountExpiryRepoStub{autoPauseFn: func(ctx context.Context, _ time.Time) (int64, error) {
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		require.WithinDuration(t, time.Now().Add(5*time.Second), deadline, 200*time.Millisecond)
		return 2, nil
	}}
	svc := NewAccountExpiryService(repo, time.Minute)

	require.NoError(t, svc.Run(context.Background()))
}

func TestAccountExpiryRunReturnsRepositoryError(t *testing.T) {
	repoErr := errors.New("auto pause failed")
	svc := NewAccountExpiryService(&accountExpiryRepoStub{autoPauseFn: func(context.Context, time.Time) (int64, error) {
		return 0, repoErr
	}}, time.Minute)

	require.ErrorIs(t, svc.Run(context.Background()), repoErr)
}

func TestNewAccountExpiryServiceDoesNotStartWork(t *testing.T) {
	called := make(chan struct{}, 1)
	NewAccountExpiryService(&accountExpiryRepoStub{autoPauseFn: func(context.Context, time.Time) (int64, error) {
		called <- struct{}{}
		return 0, nil
	}}, time.Millisecond)

	select {
	case <-called:
		t.Fatal("constructor started account expiry work")
	case <-time.After(25 * time.Millisecond):
	}
}
