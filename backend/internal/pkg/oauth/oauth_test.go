package oauth

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAuthorizeURLMatchesClaudeCodeCLI(t *testing.T) {
	const want = "https://claude.com/cai/oauth/authorize"
	require.Equal(t, want, AuthorizeURL)

	authURL := BuildAuthorizationURL("state-value", "challenge-value", ScopeOAuth)
	require.True(t, strings.HasPrefix(authURL, want+"?"), authURL)
	for _, part := range []string{
		"code=true",
		"client_id=" + ClientID,
		"response_type=code",
		"code_challenge=challenge-value",
		"code_challenge_method=S256",
		"state=state-value",
	} {
		require.Contains(t, authURL, part)
	}
}

func TestSessionStoreCleanupExpiredPreservesTTLBoundary(t *testing.T) {
	now := time.Date(2026, time.August, 4, 8, 0, 0, 0, time.UTC)
	store := NewSessionStore()
	store.Set("expired", &OAuthSession{CreatedAt: now.Add(-SessionTTL - time.Nanosecond)})
	store.Set("boundary", &OAuthSession{CreatedAt: now.Add(-SessionTTL)})
	store.Set("fresh", &OAuthSession{CreatedAt: now.Add(-SessionTTL + time.Nanosecond)})

	require.NoError(t, store.cleanupExpiredAt(context.Background(), now))
	store.mu.RLock()
	defer store.mu.RUnlock()
	_, expiredExists := store.sessions["expired"]
	_, boundaryExists := store.sessions["boundary"]
	_, freshExists := store.sessions["fresh"]
	require.False(t, expiredExists)
	require.True(t, boundaryExists)
	require.True(t, freshExists)
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

func TestSessionStoreConstructionDoesNotOwnCleanupLifecycle(t *testing.T) {
	source, err := os.ReadFile("oauth.go")
	require.NoError(t, err)
	constructor := functionSource(string(source), "NewSessionStore")
	require.NotContains(t, constructor, "go ")
	require.NotContains(t, constructor, "time.NewTicker")
}

func functionSource(source, name string) string {
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
