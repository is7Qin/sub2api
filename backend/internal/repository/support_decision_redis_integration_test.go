//go:build integration

package repository

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestSupportDecisionRedisRealRedisLua(t *testing.T) {
	ctx := context.Background()
	rdb := testRedis(t)
	store := newSupportDecisionRedis(rdb)
	const lower = uint64(9007199254740993)
	const higher = uint64(9007199254740994)

	require.NoError(t, store.PutDocument(ctx, lower, []byte("lower"), time.Minute))
	require.NoError(t, store.PutDocument(ctx, higher, []byte("higher"), time.Minute))

	activated, err := store.Activate(ctx, lower)
	require.NoError(t, err)
	require.True(t, activated)
	activated, err = store.Activate(ctx, higher)
	require.NoError(t, err)
	require.True(t, activated)
	activated, err = store.Activate(ctx, lower)
	require.NoError(t, err)
	require.False(t, activated)
	require.Equal(t, strconv.FormatUint(higher, 10), rdb.Get(ctx, supportDecisionActiveKey).Val())

	require.NoError(t, rdb.Set(ctx, supportDecisionActiveKey, "01", 0).Err())
	activated, err = store.Activate(ctx, higher)
	require.Error(t, err)
	require.False(t, activated)
	require.Equal(t, "01", rdb.Get(ctx, supportDecisionActiveKey).Val())

	require.NoError(t, rdb.Set(ctx, supportDecisionActiveKey, strconv.FormatUint(higher, 10), 0).Err())
	require.NoError(t, rdb.Del(ctx, supportDecisionDocumentKey(lower)).Err())
	require.NoError(t, rdb.PExpire(ctx, supportDecisionDocumentKey(higher), time.Millisecond).Err())
	require.Eventually(t, func() bool {
		return rdb.Exists(ctx, supportDecisionDocumentKey(higher)).Val() == 0
	}, time.Second, 10*time.Millisecond)
	activated, err = store.Activate(ctx, lower)
	require.NoError(t, err)
	require.False(t, activated)
	activated, err = store.Activate(ctx, higher)
	require.NoError(t, err)
	require.False(t, activated)
	require.Equal(t, strconv.FormatUint(higher, 10), rdb.Get(ctx, supportDecisionActiveKey).Val())

	require.NoError(t, rdb.Del(ctx, supportDecisionActiveKey).Err())
	activated, err = store.Activate(ctx, higher+1)
	require.ErrorIs(t, err, service.ErrSupportDecisionDocumentNotFound)
	require.False(t, activated)
	_, err = rdb.Get(ctx, supportDecisionActiveKey).Result()
	require.ErrorIs(t, err, redis.Nil)
}

func TestSupportDecisionRedisRealRedisDocumentTTLIsExactMilliseconds(t *testing.T) {
	ctx := context.Background()
	rdb := testRedis(t)
	store := newSupportDecisionRedis(rdb)
	const ttl = 10 * time.Second

	require.NoError(t, store.PutDocument(ctx, 1, []byte("document"), ttl))
	pttl, err := rdb.PTTL(ctx, supportDecisionDocumentKey(1)).Result()
	require.NoError(t, err)
	require.Greater(t, pttl, ttl-time.Second)
	require.LessOrEqual(t, pttl, ttl)
}

func TestSupportDecisionRedisRealRedisSubscriptionLifecycle(t *testing.T) {
	ctx := context.Background()
	store := newSupportDecisionRedis(testRedis(t))
	subscription, err := store.SubscribeWakeups(ctx)
	require.NoError(t, err)

	require.NoError(t, store.PublishWakeup(ctx, ^uint64(0)))
	received, err := subscription.Receive(ctx)
	require.NoError(t, err)
	require.Equal(t, ^uint64(0), received)

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = subscription.Receive(cancelled)
	require.True(t, errors.Is(err, context.Canceled))
	require.NoError(t, subscription.Close())
	require.NoError(t, subscription.Close())
}
