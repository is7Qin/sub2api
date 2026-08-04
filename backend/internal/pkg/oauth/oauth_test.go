package oauth

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSessionStoreCleanupExpiredRemovesExpiredSessions(t *testing.T) {
	store := NewSessionStore()
	store.Set("expired", &OAuthSession{CreatedAt: time.Now().Add(-SessionTTL - time.Second)})
	store.Set("fresh", &OAuthSession{CreatedAt: time.Now()})

	require.NoError(t, store.CleanupExpired(context.Background()))
	_, ok := store.Get("expired")
	require.False(t, ok)
	_, ok = store.Get("fresh")
	require.True(t, ok)
}

func TestSessionStoreCleanupExpiredHonorsCancellation(t *testing.T) {
	store := NewSessionStore()
	store.Set("expired", &OAuthSession{CreatedAt: time.Now().Add(-SessionTTL - time.Second)})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.ErrorIs(t, store.CleanupExpired(ctx), context.Canceled)
	store.mu.RLock()
	_, ok := store.sessions["expired"]
	store.mu.RUnlock()
	require.True(t, ok)
}

func TestSessionStoreConstructionDoesNotStartCleanup(t *testing.T) {
	store := NewSessionStore()
	store.Set("expired", &OAuthSession{CreatedAt: time.Now().Add(-SessionTTL - time.Second)})

	// Cleanup is runtime-owned; construction must not remove sessions asynchronously.
	time.Sleep(10 * time.Millisecond)
	store.mu.RLock()
	_, ok := store.sessions["expired"]
	store.mu.RUnlock()
	require.True(t, ok)
}
