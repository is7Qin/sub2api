package repository

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newSupportDecisionRedisTestStore(t *testing.T) (*supportDecisionRedis, *redis.Client, *miniredis.Miniredis) {
	t.Helper()
	server := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return newSupportDecisionRedis(rdb), rdb, server
}

func TestSupportDecisionRedisUsesNormativeKeys(t *testing.T) {
	store, rdb, _ := newSupportDecisionRedisTestStore(t)
	ctx := context.Background()

	require.Equal(t, "sched:support-decision:active", supportDecisionActiveKey)
	require.Equal(t, "sched:support-decision:document:", supportDecisionDocumentKeyPrefix)
	require.Equal(t, "sched:support-decision:wakeup", supportDecisionWakeupChannel)
	require.Equal(t, "sched:support-decision:document:42", supportDecisionDocumentKey(42))

	require.NoError(t, store.PutDocument(ctx, 42, []byte("document"), time.Minute))
	require.Equal(t, "document", rdb.Get(ctx, "sched:support-decision:document:42").Val())

	activated, err := store.Activate(ctx, 42)
	require.NoError(t, err)
	require.True(t, activated)
	require.Equal(t, "42", rdb.Get(ctx, "sched:support-decision:active").Val())

	pubsub := rdb.Subscribe(ctx, "sched:support-decision:wakeup")
	t.Cleanup(func() { _ = pubsub.Close() })
	_, err = pubsub.Receive(ctx)
	require.NoError(t, err)
	require.NoError(t, store.PublishWakeup(ctx, 42))
	message, err := pubsub.ReceiveMessage(ctx)
	require.NoError(t, err)
	require.Equal(t, "42", message.Payload)
}

func TestSupportDecisionRedisWritesDocumentBeforeActivation(t *testing.T) {
	store, rdb, _ := newSupportDecisionRedisTestStore(t)
	ctx := context.Background()

	activated, err := store.Activate(ctx, 7)
	require.ErrorIs(t, err, service.ErrSupportDecisionDocumentNotFound)
	require.False(t, activated)
	require.Equal(t, int64(0), rdb.Exists(ctx, supportDecisionActiveKey).Val())

	require.NoError(t, store.PutDocument(ctx, 7, []byte("complete"), time.Minute))
	_, err = store.ActiveGeneration(ctx)
	require.ErrorIs(t, err, service.ErrSupportDecisionActiveGenerationNotFound)
	require.Equal(t, "complete", rdb.Get(ctx, supportDecisionDocumentKey(7)).Val())

	activated, err = store.Activate(ctx, 7)
	require.NoError(t, err)
	require.True(t, activated)
}

func TestSupportDecisionRedisActivationRejectsLowerGeneration(t *testing.T) {
	store, _, _ := newSupportDecisionRedisTestStore(t)
	ctx := context.Background()
	putAndActivateSupportDecision(t, ctx, store, 20)
	require.NoError(t, store.PutDocument(ctx, 19, []byte("lower"), time.Minute))

	activated, err := store.Activate(ctx, 19)
	require.NoError(t, err)
	require.False(t, activated)
	generation, err := store.ActiveGeneration(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(20), generation)
}

func TestSupportDecisionRedisActivationRejectsEqualGeneration(t *testing.T) {
	store, _, _ := newSupportDecisionRedisTestStore(t)
	ctx := context.Background()
	putAndActivateSupportDecision(t, ctx, store, 20)

	activated, err := store.Activate(ctx, 20)
	require.NoError(t, err)
	require.False(t, activated)
	generation, err := store.ActiveGeneration(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(20), generation)
}

func TestSupportDecisionRedisDelayedPublisherCannotRollbackActiveState(t *testing.T) {
	store, rdb, _ := newSupportDecisionRedisTestStore(t)
	ctx := context.Background()
	require.NoError(t, store.PutDocument(ctx, 101, []byte("new"), time.Minute))
	require.NoError(t, store.PutDocument(ctx, 100, []byte("delayed"), time.Minute))

	activated, err := store.Activate(ctx, 101)
	require.NoError(t, err)
	require.True(t, activated)
	activated, err = store.Activate(ctx, 100)
	require.NoError(t, err)
	require.False(t, activated)
	require.Equal(t, "101", rdb.Get(ctx, supportDecisionActiveKey).Val())
}

func TestSupportDecisionRedisCASHandlesGenerationAboveLuaExactIntegerRange(t *testing.T) {
	store, _, _ := newSupportDecisionRedisTestStore(t)
	ctx := context.Background()
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
	generation, err := store.ActiveGeneration(ctx)
	require.NoError(t, err)
	require.Equal(t, higher, generation)
}

type ambiguousActivationHook struct {
	mu       sync.Mutex
	injected bool
}

func (h *ambiguousActivationHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *ambiguousActivationHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		if err != nil || !strings.EqualFold(cmd.Name(), "eval") {
			return err
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.injected {
			return nil
		}
		h.injected = true
		return errors.New("injected ambiguous activation result")
	}
}

func (h *ambiguousActivationHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func TestSupportDecisionRedisAmbiguousActivationPreservesActiveState(t *testing.T) {
	server := miniredis.RunT(t)
	observer := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = observer.Close() })
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := newSupportDecisionRedis(client)
	ctx := context.Background()
	require.NoError(t, store.PutDocument(ctx, 55, []byte("document"), time.Minute))
	client.AddHook(&ambiguousActivationHook{})

	activated, err := store.Activate(ctx, 55)
	require.ErrorContains(t, err, "ambiguous")
	require.False(t, activated)
	// The script may have committed before the client observed the error. Never clean it up.
	require.Equal(t, "55", observer.Get(ctx, supportDecisionActiveKey).Val())
}

func TestSupportDecisionRedisUnactivatedDocumentExpires(t *testing.T) {
	store, rdb, server := newSupportDecisionRedisTestStore(t)
	ctx := context.Background()
	require.NoError(t, store.PutDocument(ctx, 8, []byte("temporary"), time.Second))
	require.Equal(t, time.Second, rdb.TTL(ctx, supportDecisionDocumentKey(8)).Val())

	server.FastForward(time.Second)
	_, err := store.GetDocument(ctx, 8)
	require.ErrorIs(t, err, service.ErrSupportDecisionDocumentNotFound)
	_, err = store.ActiveGeneration(ctx)
	require.ErrorIs(t, err, service.ErrSupportDecisionActiveGenerationNotFound)
}

func TestSupportDecisionRedisWakeupFailureDoesNotUndoActivation(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	store := newSupportDecisionRedis(client)
	observer := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = observer.Close() })
	ctx := context.Background()
	putAndActivateSupportDecision(t, ctx, store, 99)
	require.NoError(t, client.Close())

	require.Error(t, store.PublishWakeup(ctx, 99))
	require.Equal(t, "99", observer.Get(ctx, supportDecisionActiveKey).Val())
}

func TestSupportDecisionRedisRejectsMalformedActiveGeneration(t *testing.T) {
	store, rdb, _ := newSupportDecisionRedisTestStore(t)
	ctx := context.Background()
	malformed := []string{"", "0", "01", "+1", "-1", " 1", "1 ", "1.0", "abc", "18446744073709551616"}
	for _, value := range malformed {
		t.Run(fmt.Sprintf("value_%q", value), func(t *testing.T) {
			require.NoError(t, rdb.Set(ctx, supportDecisionActiveKey, value, 0).Err())
			_, err := store.ActiveGeneration(ctx)
			require.Error(t, err)

			require.NoError(t, store.PutDocument(ctx, 2, []byte("candidate"), time.Minute))
			activated, err := store.Activate(ctx, 2)
			require.Error(t, err)
			require.False(t, activated)
			require.Equal(t, value, rdb.Get(ctx, supportDecisionActiveKey).Val())
		})
	}
}

func TestSupportDecisionRedisRejectsInvalidInputs(t *testing.T) {
	store, _, _ := newSupportDecisionRedisTestStore(t)
	ctx := context.Background()

	require.Error(t, store.PutDocument(ctx, 0, []byte("document"), time.Minute))
	require.Error(t, store.PutDocument(ctx, 1, nil, time.Minute))
	require.Error(t, store.PutDocument(ctx, 1, []byte("document"), 0))
	require.Error(t, store.PutDocument(ctx, 1, []byte("document"), -time.Second))
	require.Error(t, store.PutDocument(ctx, 1, []byte("document"), maxSupportDecisionDocumentTTL+time.Millisecond))
	_, err := store.Activate(ctx, 0)
	require.Error(t, err)
	_, err = store.GetDocument(ctx, 0)
	require.Error(t, err)
	require.Error(t, store.PublishWakeup(ctx, 0))
}

func TestSupportDecisionRedisMissingState(t *testing.T) {
	store, _, _ := newSupportDecisionRedisTestStore(t)
	ctx := context.Background()

	_, err := store.ActiveGeneration(ctx)
	require.ErrorIs(t, err, service.ErrSupportDecisionActiveGenerationNotFound)
	_, err = store.GetDocument(ctx, 1)
	require.ErrorIs(t, err, service.ErrSupportDecisionDocumentNotFound)
}

func TestSupportDecisionRedisExactPayloadRoundTrip(t *testing.T) {
	store, _, _ := newSupportDecisionRedisTestStore(t)
	ctx := context.Background()
	payload := []byte{0, 1, 2, '\n', 0xff, 0, 'x'}
	require.NoError(t, store.PutDocument(ctx, ^uint64(0), payload, time.Minute))

	got, err := store.GetDocument(ctx, ^uint64(0))
	require.NoError(t, err)
	require.Equal(t, payload, got)
}

func TestSupportDecisionRedisSubscriptionParsingAndLifecycle(t *testing.T) {
	store, rdb, _ := newSupportDecisionRedisTestStore(t)
	ctx := context.Background()
	subscription, err := store.SubscribeWakeups(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = subscription.Close() })

	require.NoError(t, rdb.Publish(ctx, supportDecisionWakeupChannel, strconv.FormatUint(^uint64(0), 10)).Err())
	generation, err := subscription.Receive(ctx)
	require.NoError(t, err)
	require.Equal(t, ^uint64(0), generation)

	require.NoError(t, rdb.Publish(ctx, supportDecisionWakeupChannel, "01").Err())
	generation, err = subscription.Receive(ctx)
	require.Error(t, err)
	require.Zero(t, generation)

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	generation, err = subscription.Receive(cancelled)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, generation)
	require.NoError(t, subscription.Close())
	require.NoError(t, subscription.Close())
}

func putAndActivateSupportDecision(t *testing.T, ctx context.Context, store *supportDecisionRedis, generation uint64) {
	t.Helper()
	require.NoError(t, store.PutDocument(ctx, generation, []byte(strconv.FormatUint(generation, 10)), time.Minute))
	activated, err := store.Activate(ctx, generation)
	require.NoError(t, err)
	require.True(t, activated)
}
