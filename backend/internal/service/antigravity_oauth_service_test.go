package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/stretchr/testify/require"
)

func TestAntigravityOAuthService_CleanupSessionsDelegatesAndPropagatesCancellation(t *testing.T) {
	svc := NewAntigravityOAuthService(nil)
	session := &antigravity.OAuthSession{CreatedAt: time.Now().Add(-antigravity.SessionTTL - time.Second)}
	svc.sessionStore.Set("expired", session)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.ErrorIs(t, svc.CleanupSessions(ctx), context.Canceled)
	require.NoError(t, svc.CleanupSessions(context.Background()))
	// Get hides expired entries, so refresh the retained pointer to prove cleanup
	// physically removed it from the same store used by request handling.
	session.CreatedAt = time.Now()
	_, ok := svc.sessionStore.Get("expired")
	require.False(t, ok)
}

func TestAntigravityOAuthService_CleanupSessionsIsNilSafe(t *testing.T) {
	var svc *AntigravityOAuthService
	require.NoError(t, svc.CleanupSessions(context.Background()))
	require.NoError(t, (&AntigravityOAuthService{}).CleanupSessions(context.Background()))
}

func TestResolveDefaultTierID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		loadRaw map[string]any
		want    string
	}{
		{
			name:    "nil loadRaw",
			loadRaw: nil,
			want:    "",
		},
		{
			name: "missing allowedTiers",
			loadRaw: map[string]any{
				"paidTier": map[string]any{"id": "g1-pro-tier"},
			},
			want: "",
		},
		{
			name:    "empty allowedTiers",
			loadRaw: map[string]any{"allowedTiers": []any{}},
			want:    "",
		},
		{
			name: "tier missing id field",
			loadRaw: map[string]any{
				"allowedTiers": []any{
					map[string]any{"isDefault": true},
				},
			},
			want: "",
		},
		{
			name: "allowedTiers but no default",
			loadRaw: map[string]any{
				"allowedTiers": []any{
					map[string]any{"id": "free-tier", "isDefault": false},
					map[string]any{"id": "standard-tier", "isDefault": false},
				},
			},
			want: "",
		},
		{
			name: "default tier found",
			loadRaw: map[string]any{
				"allowedTiers": []any{
					map[string]any{"id": "free-tier", "isDefault": true},
					map[string]any{"id": "standard-tier", "isDefault": false},
				},
			},
			want: "free-tier",
		},
		{
			name: "default tier id with spaces",
			loadRaw: map[string]any{
				"allowedTiers": []any{
					map[string]any{"id": "  standard-tier  ", "isDefault": true},
				},
			},
			want: "standard-tier",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := resolveDefaultTierID(tc.loadRaw)
			if got != tc.want {
				t.Fatalf("resolveDefaultTierID() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAntigravityOAuthService_EnrichRefreshAccountTokenInfo_FallbackOnlySkipsProjectProbe(t *testing.T) {
	probe := newAntigravityV1InternalProbe(t)
	svc := NewAntigravityOAuthService(nil)
	account := &Account{
		ID:       701,
		Platform: PlatformAntigravity,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"email":                                 "user@example.com",
			antigravityProjectFallbackCredentialKey: " configured-project ",
		},
	}
	tokenInfo := &AntigravityTokenInfo{AccessToken: "refreshed-token"}

	svc.enrichRefreshAccountTokenInfo(context.Background(), account, tokenInfo, "")

	require.Equal(t, "user@example.com", tokenInfo.Email)
	require.Empty(t, tokenInfo.ProjectID)
	require.False(t, tokenInfo.ProjectIDMissing)
	require.Empty(t, tokenInfo.PlanType)
	require.Empty(t, probe.paths, "fallback-only refresh must not call LoadCodeAssist or OnboardUser")
}

func TestAntigravityOAuthService_EnrichRefreshAccountTokenInfo_PrimaryProjectStillProbes(t *testing.T) {
	probe := newAntigravityV1InternalProbe(t)
	svc := NewAntigravityOAuthService(nil)
	account := &Account{
		ID:       702,
		Platform: PlatformAntigravity,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"project_id":                            "existing-project",
			antigravityProjectFallbackCredentialKey: " configured-project ",
		},
	}
	tokenInfo := &AntigravityTokenInfo{AccessToken: "refreshed-token"}

	svc.enrichRefreshAccountTokenInfo(context.Background(), account, tokenInfo, "")

	require.Contains(t, probe.paths, "/v1internal:loadCodeAssist")
	require.Equal(t, "backfilled-project", tokenInfo.ProjectID)
}

func TestAntigravityOAuthService_EnrichRefreshAccountTokenInfo_NoFallbackStillBackfills(t *testing.T) {
	probe := newAntigravityV1InternalProbe(t)
	svc := NewAntigravityOAuthService(nil)
	account := &Account{
		ID:          703,
		Platform:    PlatformAntigravity,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{},
	}
	tokenInfo := &AntigravityTokenInfo{AccessToken: "refreshed-token"}

	svc.enrichRefreshAccountTokenInfo(context.Background(), account, tokenInfo, "")

	require.Contains(t, probe.paths, "/v1internal:loadCodeAssist")
	require.Equal(t, "backfilled-project", tokenInfo.ProjectID)
}

func TestAntigravityOAuthService_BuildRefreshAccountCredentials_FallbackOnlyDropsBlankProjectID(t *testing.T) {
	svc := NewAntigravityOAuthService(nil)
	account := &Account{
		ID:       704,
		Platform: PlatformAntigravity,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":                          "old-token",
			"refresh_token":                         "old-refresh",
			"project_id":                            "  ",
			"plan_type":                             "Pro",
			antigravityProjectFallbackCredentialKey: " configured-project ",
		},
	}
	tokenInfo := &AntigravityTokenInfo{
		AccessToken:  "new-token",
		RefreshToken: "new-refresh",
		ExpiresAt:    1234567890,
		TokenType:    "Bearer",
	}

	creds := svc.BuildRefreshAccountCredentials(account, tokenInfo)

	require.Equal(t, "new-token", creds["access_token"])
	require.Equal(t, "new-refresh", creds["refresh_token"])
	require.Equal(t, "Pro", creds["plan_type"])
	require.Equal(t, " configured-project ", creds[antigravityProjectFallbackCredentialKey])
	require.NotContains(t, creds, "project_id")
}

func TestAntigravityOAuthService_BuildRefreshAccountCredentials_PrimaryProjectIDWins(t *testing.T) {
	svc := NewAntigravityOAuthService(nil)
	account := &Account{
		ID:       705,
		Platform: PlatformAntigravity,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"project_id":                            "primary-project",
			antigravityProjectFallbackCredentialKey: " configured-project ",
		},
	}
	tokenInfo := &AntigravityTokenInfo{
		AccessToken: "new-token",
		ExpiresAt:   1234567890,
	}

	creds := svc.BuildRefreshAccountCredentials(account, tokenInfo)

	require.Equal(t, "primary-project", creds["project_id"])
	require.Equal(t, " configured-project ", creds[antigravityProjectFallbackCredentialKey])
}
