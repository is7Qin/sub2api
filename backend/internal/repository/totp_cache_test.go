package repository

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestTotpCacheUseBackupS3TotpStepIsAtomicAndEndpointScoped(t *testing.T) {
	server := miniredis.RunT(t)
	cache := &TotpCache{rdb: redis.NewClient(&redis.Options{Addr: server.Addr()})}
	ctx := context.Background()

	const callers = 8
	start := make(chan struct{})
	results := make(chan bool, callers)
	errs := make(chan error, callers)
	var ready sync.WaitGroup
	ready.Add(callers)
	for range callers {
		go func() {
			ready.Done()
			<-start
			used, err := cache.UseBackupS3TotpStep(ctx, 7, 123, 90*time.Second)
			results <- used
			errs <- err
		}()
	}
	ready.Wait()
	close(start)

	firstUses := 0
	for range callers {
		require.NoError(t, <-errs)
		if !<-results {
			firstUses++
		}
	}
	require.Equal(t, 1, firstUses)

	used, err := cache.UseBackupS3TotpStep(ctx, 7, 124, 90*time.Second)
	require.NoError(t, err)
	require.False(t, used)
}

func TestTotpCacheUseBackupS3TotpStepIsIsolatedByUser(t *testing.T) {
	server := miniredis.RunT(t)
	cache := &TotpCache{rdb: redis.NewClient(&redis.Options{Addr: server.Addr()})}
	ctx := context.Background()

	used, err := cache.UseBackupS3TotpStep(ctx, 7, 123, 90*time.Second)
	require.NoError(t, err)
	require.False(t, used)

	used, err = cache.UseBackupS3TotpStep(ctx, 8, 123, 90*time.Second)
	require.NoError(t, err)
	require.False(t, used)

	used, err = cache.UseBackupS3TotpStep(ctx, 7, 123, 90*time.Second)
	require.NoError(t, err)
	require.True(t, used)
}
