package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

type openaiOAuthClientStateStub struct {
	exchangeCalled int32
	lastClientID   string
	lastUAProfile  OpenAICodexUAProfile
}

func (s *openaiOAuthClientStateStub) ExchangeCode(ctx context.Context, code, codeVerifier, redirectURI, proxyURL string, opts OpenAIOAuthTokenOptions) (*openai.TokenResponse, error) {
	atomic.AddInt32(&s.exchangeCalled, 1)
	s.lastClientID = opts.ClientID
	s.lastUAProfile = opts.UAProfile
	return &openai.TokenResponse{
		AccessToken:  "at",
		RefreshToken: "rt",
		ExpiresIn:    3600,
	}, nil
}

func (s *openaiOAuthClientStateStub) RefreshToken(ctx context.Context, refreshToken, proxyURL string) (*openai.TokenResponse, error) {
	return nil, errors.New("not implemented")
}

func (s *openaiOAuthClientStateStub) RefreshTokenWithClientID(ctx context.Context, refreshToken, proxyURL string, clientID string) (*openai.TokenResponse, error) {
	return s.RefreshToken(ctx, refreshToken, proxyURL)
}

func (s *openaiOAuthClientStateStub) RefreshTokenWithOptions(ctx context.Context, refreshToken, proxyURL string, opts OpenAIOAuthTokenOptions) (*openai.TokenResponse, error) {
	return s.RefreshToken(ctx, refreshToken, proxyURL)
}

func TestOpenAIOAuthService_ExchangeCode_StateRequired(t *testing.T) {
	client := &openaiOAuthClientStateStub{}
	svc := NewOpenAIOAuthService(nil, client)

	svc.sessionStore.Set(context.Background(), "sid", &openAIOAuthPendingSession{OAuthSession: openai.OAuthSession{
		State:        "expected-state",
		CodeVerifier: "verifier",
		RedirectURI:  openai.DefaultRedirectURI,
		CreatedAt:    time.Now(),
	}})

	_, err := svc.ExchangeCode(context.Background(), &OpenAIExchangeCodeInput{
		SessionID: "sid",
		Code:      "auth-code",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "oauth state is required")
	require.Equal(t, int32(0), atomic.LoadInt32(&client.exchangeCalled))
}

func TestOpenAIOAuthService_ExchangeCode_StateMismatch(t *testing.T) {
	client := &openaiOAuthClientStateStub{}
	svc := NewOpenAIOAuthService(nil, client)

	svc.sessionStore.Set(context.Background(), "sid", &openAIOAuthPendingSession{OAuthSession: openai.OAuthSession{
		State:        "expected-state",
		CodeVerifier: "verifier",
		RedirectURI:  openai.DefaultRedirectURI,
		CreatedAt:    time.Now(),
	}})

	_, err := svc.ExchangeCode(context.Background(), &OpenAIExchangeCodeInput{
		SessionID: "sid",
		Code:      "auth-code",
		State:     "wrong-state",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid oauth state")
	require.Equal(t, int32(0), atomic.LoadInt32(&client.exchangeCalled))
}

func TestOpenAIOAuthService_ExchangeCode_StateMatch(t *testing.T) {
	client := &openaiOAuthClientStateStub{}
	svc := NewOpenAIOAuthService(nil, client)
	pendingFingerprint, _ := NormalizeOpenAICodexFingerprint(OpenAICodexFingerprint{
		SchemaVersion:  openAICodexFingerprintSchemaV1,
		InstallationID: "550e8400-e29b-41d4-a716-446655440000",
		UAProfile: OpenAICodexUAProfile{
			Originator:    "pending-originator",
			CodexVersion:  "1.2.3",
			OSFingerprint: "Pending OS; arm64",
			TerminalToken: "Pending_Terminal/1.0",
		},
		CreatedAt: "2026-06-16T00:00:00Z",
		UpdatedAt: "2026-06-16T00:00:00Z",
	}, ParseOpenAICodexUAProfile(DefaultOpenAICodexUserAgent), time.Now())

	svc.sessionStore.Set(context.Background(), "sid", &openAIOAuthPendingSession{
		OAuthSession: openai.OAuthSession{
			State:        "expected-state",
			CodeVerifier: "verifier",
			RedirectURI:  openai.DefaultRedirectURI,
			CreatedAt:    time.Now(),
		},
		CodexFingerprint: pendingFingerprint,
	})

	info, err := svc.ExchangeCode(context.Background(), &OpenAIExchangeCodeInput{
		SessionID: "sid",
		Code:      "auth-code",
		State:     "expected-state",
	})
	require.NoError(t, err)
	require.NotNil(t, info)
	require.Equal(t, "at", info.AccessToken)
	require.Equal(t, openai.ClientID, info.ClientID)
	require.Equal(t, openai.ClientID, client.lastClientID)
	require.Equal(t, pendingFingerprint.UAProfile, client.lastUAProfile)
	require.Equal(t, pendingFingerprint, *info.CodexFingerprint)
	require.Equal(t, int32(1), atomic.LoadInt32(&client.exchangeCalled))

	_, ok := svc.sessionStore.Get(context.Background(), "sid")
	require.False(t, ok)
}
