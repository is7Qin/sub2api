package service

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestOpenAIOAuthMemorySessionCleanupBoundaries(t *testing.T) {
	now := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	store := newOpenAIOAuthMemorySessionStore()
	store.sessions["expired"] = &openAIOAuthPendingSession{OAuthSession: openai.OAuthSession{CreatedAt: now.Add(-openai.SessionTTL - time.Nanosecond)}}
	store.sessions["equal"] = &openAIOAuthPendingSession{OAuthSession: openai.OAuthSession{CreatedAt: now.Add(-openai.SessionTTL)}}
	store.sessions["valid"] = &openAIOAuthPendingSession{OAuthSession: openai.OAuthSession{CreatedAt: now.Add(-openai.SessionTTL + time.Nanosecond)}}

	require.NoError(t, store.cleanupExpiredSessionsAt(context.Background(), now))
	require.NotContains(t, store.sessions, "expired")
	require.Contains(t, store.sessions, "equal")
	require.Contains(t, store.sessions, "valid")
}

func TestOpenAIOAuthMemorySessionCleanupCancellationPreservesPhysicalEntry(t *testing.T) {
	now := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	store := newOpenAIOAuthMemorySessionStore()
	store.sessions["expired"] = &openAIOAuthPendingSession{OAuthSession: openai.OAuthSession{CreatedAt: now.Add(-openai.SessionTTL - time.Nanosecond)}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.ErrorIs(t, store.cleanupExpiredSessionsAt(ctx, now), context.Canceled)
	require.Contains(t, store.sessions, "expired")
}

func TestOpenAIOAuthRedisSetFailureCleanupBoundaries(t *testing.T) {
	now := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	store := &openAIOAuthRedisSessionStore{}
	store.redisSetFailures.Store("expired", openAIOAuthRedisSetFailure{expiresAt: now.Add(-time.Nanosecond)})
	store.redisSetFailures.Store("equal", openAIOAuthRedisSetFailure{expiresAt: now})
	store.redisSetFailures.Store("valid", openAIOAuthRedisSetFailure{expiresAt: now.Add(time.Nanosecond)})

	require.NoError(t, store.cleanupExpiredRedisSetFailuresAt(context.Background(), now))
	_, expiredPresent := store.redisSetFailures.Load("expired")
	_, equalPresent := store.redisSetFailures.Load("equal")
	_, validPresent := store.redisSetFailures.Load("valid")
	require.False(t, expiredPresent)
	require.True(t, equalPresent)
	require.True(t, validPresent)
}

func TestOpenAIOAuthRedisSetFailureCleanupCancellationPreservesPhysicalEntry(t *testing.T) {
	now := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	store := &openAIOAuthRedisSessionStore{}
	store.redisSetFailures.Store("expired", openAIOAuthRedisSetFailure{expiresAt: now.Add(-time.Nanosecond)})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.ErrorIs(t, store.cleanupExpiredRedisSetFailuresAt(ctx, now), context.Canceled)
	_, present := store.redisSetFailures.Load("expired")
	require.True(t, present)
}

func TestOpenAIOAuthSessionStoreConstructorsArePassive(t *testing.T) {
	content, err := os.ReadFile("openai_oauth_session_store.go")
	require.NoError(t, err)
	source := string(content)
	for _, constructor := range []string{"newOpenAIOAuthMemorySessionStore", "newOpenAIOAuthSessionStore"} {
		body := openAIOAuthFunctionSource(source, constructor)
		require.NotContains(t, body, "go ")
		require.NotContains(t, body, "time.NewTicker")
		require.NotContains(t, body, "stopCh")
	}
}

func TestOpenAIOAuthServiceCleanupDelegatesToRequestServingStore(t *testing.T) {
	now := time.Now()
	store := newOpenAIOAuthMemorySessionStore()
	store.sessions["expired"] = &openAIOAuthPendingSession{OAuthSession: openai.OAuthSession{CreatedAt: now.Add(-openai.SessionTTL - time.Second)}}
	svc := newOpenAIOAuthServiceWithSessionStore(nil, nil, store)

	require.NoError(t, svc.CleanupSessions(context.Background()))
	require.NotContains(t, store.sessions, "expired")
}

func TestOpenAIOAuthServiceCleanupNilSafetyAndCancellation(t *testing.T) {
	var nilService *OpenAIOAuthService
	require.NoError(t, nilService.CleanupSessions(context.Background()))
	require.NoError(t, (&OpenAIOAuthService{}).CleanupSessions(context.Background()))
	require.NoError(t, nilService.CleanupRedisSetFailures(context.Background()))
	require.NoError(t, (&OpenAIOAuthService{}).CleanupRedisSetFailures(context.Background()))
	require.NoError(t, NewOpenAIOAuthService(nil, nil).CleanupRedisSetFailures(context.Background()))

	store := newOpenAIOAuthMemorySessionStore()
	store.sessions["expired"] = &openAIOAuthPendingSession{OAuthSession: openai.OAuthSession{CreatedAt: time.Now().Add(-openai.SessionTTL - time.Second)}}
	svc := newOpenAIOAuthServiceWithSessionStore(nil, nil, store)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, svc.CleanupSessions(ctx), context.Canceled)
	require.Contains(t, store.sessions, "expired")
}

func TestOpenAIOAuthRedisMarkerCleanupCapabilityMatchesStore(t *testing.T) {
	memoryService := NewOpenAIOAuthService(nil, nil)
	require.False(t, memoryService.hasRedisSetFailureCleanup())
	require.NoError(t, memoryService.CleanupRedisSetFailures(context.Background()))

	redisStore := &openAIOAuthRedisSessionStore{memory: newOpenAIOAuthMemorySessionStore()}
	redisService := newOpenAIOAuthServiceWithSessionStore(nil, nil, redisStore)
	require.True(t, redisService.hasRedisSetFailureCleanup())
	redisStore.redisSetFailures.Store("expired", openAIOAuthRedisSetFailure{expiresAt: time.Now().Add(-time.Second)})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, redisService.CleanupRedisSetFailures(ctx), context.Canceled)
	_, present := redisStore.redisSetFailures.Load("expired")
	require.True(t, present, "service callback must propagate cancellation without physically deleting the marker")

	require.NoError(t, redisService.CleanupRedisSetFailures(context.Background()))
	_, present = redisStore.redisSetFailures.Load("expired")
	require.False(t, present, "service callback must delegate successful cleanup to the request-serving store")
}

func openAIOAuthFunctionSource(source, name string) string {
	start := strings.Index(source, "func "+name)
	if start < 0 {
		return ""
	}
	end := strings.Index(source[start:], "\nfunc ")
	if end < 0 {
		return source[start:]
	}
	return source[start : start+end]
}

func TestOpenAIOAuthRedisSessionStore_SharesSessionsAcrossInstances(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	ctx := context.Background()
	storeA := newOpenAIOAuthSessionStore(rdb)
	storeB := newOpenAIOAuthSessionStore(rdb)

	session := &openAIOAuthPendingSession{OAuthSession: openai.OAuthSession{
		State:        "state-1",
		CodeVerifier: "verifier-1",
		ClientID:     openai.ClientID,
		RedirectURI:  openai.DefaultRedirectURI,
		ProxyURL:     "http://proxy.example",
		CreatedAt:    time.Now(),
	}}
	require.NoError(t, storeA.Set(ctx, "session-1", session))

	loaded, ok := storeB.Get(ctx, "session-1")
	require.True(t, ok)
	require.Equal(t, session.State, loaded.State)
	require.Equal(t, session.CodeVerifier, loaded.CodeVerifier)
	require.Equal(t, session.ProxyURL, loaded.ProxyURL)

	storeB.Delete(ctx, "session-1")
	_, ok = storeA.Get(ctx, "session-1")
	require.False(t, ok, "delete in Redis must not be resurrected by another instance's memory fallback")
}

func TestOpenAIOAuthRedisSessionStore_DoesNotFallbackToStaleMemoryAfterRedisDelete(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	ctx := context.Background()
	storeA := newOpenAIOAuthSessionStore(rdb)
	storeB := newOpenAIOAuthSessionStore(rdb)

	session := &openAIOAuthPendingSession{OAuthSession: openai.OAuthSession{
		State:        "state-stale",
		CodeVerifier: "verifier-stale",
		ClientID:     openai.ClientID,
		RedirectURI:  openai.DefaultRedirectURI,
		CreatedAt:    time.Now(),
	}}
	require.NoError(t, storeA.Set(ctx, "session-stale", session))
	loaded, ok := storeA.Get(ctx, "session-stale")
	require.True(t, ok)
	require.Equal(t, session.State, loaded.State)

	storeB.Delete(ctx, "session-stale")
	mr.Close()

	_, ok = storeA.Get(ctx, "session-stale")
	require.False(t, ok, "a session that reached Redis must not fall back to stale memory after another instance deletes it")
}

func TestOpenAIOAuthRedisSessionStore_FallsBackToMemoryWhenRedisWriteFails(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{
		Addr:            "127.0.0.1:0",
		DialTimeout:     50 * time.Millisecond,
		ReadTimeout:     50 * time.Millisecond,
		WriteTimeout:    50 * time.Millisecond,
		MaxRetries:      0,
		MinRetryBackoff: -1,
		MaxRetryBackoff: -1,
	})
	t.Cleanup(func() { _ = rdb.Close() })

	ctx := context.Background()
	store := newOpenAIOAuthSessionStore(rdb)

	session := &openAIOAuthPendingSession{OAuthSession: openai.OAuthSession{
		State:        "state-2",
		CodeVerifier: "verifier-2",
		ClientID:     openai.ClientID,
		RedirectURI:  openai.DefaultRedirectURI,
		CreatedAt:    time.Now(),
	}}
	require.NoError(t, store.Set(ctx, "session-2", session))

	loaded, ok := store.Get(ctx, "session-2")
	require.True(t, ok)
	require.Equal(t, session.State, loaded.State)
	require.Equal(t, session.CodeVerifier, loaded.CodeVerifier)
}

func TestOpenAIOAuthRedisSessionStore_RejectsExpiredSession(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	ctx := context.Background()
	store := newOpenAIOAuthSessionStore(rdb)

	session := &openAIOAuthPendingSession{OAuthSession: openai.OAuthSession{
		State:        "state-expired",
		CodeVerifier: "verifier-expired",
		RedirectURI:  openai.DefaultRedirectURI,
		CreatedAt:    time.Now().Add(-openai.SessionTTL - time.Minute),
	}}
	require.NoError(t, store.Set(ctx, "session-expired", session))

	_, ok := store.Get(ctx, "session-expired")
	require.False(t, ok)
	require.False(t, mr.Exists(openAIOAuthSessionKey("session-expired")))
}
