package service

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

type openaiOAuthClientAuthURLStub struct{}

func (s *openaiOAuthClientAuthURLStub) ExchangeCode(ctx context.Context, code, codeVerifier, redirectURI, proxyURL string, opts OpenAIOAuthTokenOptions) (*openai.TokenResponse, error) {
	return nil, errors.New("not implemented")
}

func (s *openaiOAuthClientAuthURLStub) RefreshToken(ctx context.Context, refreshToken, proxyURL string) (*openai.TokenResponse, error) {
	return nil, errors.New("not implemented")
}

func (s *openaiOAuthClientAuthURLStub) RefreshTokenWithClientID(ctx context.Context, refreshToken, proxyURL string, clientID string) (*openai.TokenResponse, error) {
	return nil, errors.New("not implemented")
}

func (s *openaiOAuthClientAuthURLStub) RefreshTokenWithOptions(ctx context.Context, refreshToken, proxyURL string, opts OpenAIOAuthTokenOptions) (*openai.TokenResponse, error) {
	return nil, errors.New("not implemented")
}

func TestOpenAIOAuthService_GenerateAuthURL_OpenAIKeepsCodexFlow(t *testing.T) {
	svc := NewOpenAIOAuthService(nil, &openaiOAuthClientAuthURLStub{})

	result, err := svc.GenerateAuthURL(context.Background(), nil, "", PlatformOpenAI)
	require.NoError(t, err)
	require.NotEmpty(t, result.AuthURL)
	require.NotEmpty(t, result.SessionID)

	parsed, err := url.Parse(result.AuthURL)
	require.NoError(t, err)
	q := parsed.Query()
	require.Equal(t, openai.ClientID, q.Get("client_id"))
	require.Equal(t, openai.DefaultScopes, q.Get("scope"))
	require.Equal(t, "true", q.Get("codex_cli_simplified_flow"))
	require.Equal(t, codexOfficialOriginator, q.Get("originator"))

	session, ok := svc.sessionStore.Get(context.Background(), result.SessionID)
	require.True(t, ok)
	require.Equal(t, openai.ClientID, session.ClientID)
	require.Equal(t, session.CodexFingerprint.UAProfile.Originator, q.Get("originator"))
	require.NotEmpty(t, session.CodexFingerprint.InstallationID)
}

func TestOpenAIOAuthService_GenerateAuthURL_ReauthUsesAccountFingerprint(t *testing.T) {
	svc := NewOpenAIOAuthService(nil, &openaiOAuthClientAuthURLStub{})

	accountProfile := OpenAICodexUAProfile{
		Originator:    "account-originator",
		CodexVersion:  "9.8.7",
		OSFingerprint: "Test OS; amd64",
		TerminalToken: "Test_Terminal/1.0",
	}
	accountFingerprint, _ := NormalizeOpenAICodexFingerprint(OpenAICodexFingerprint{
		SchemaVersion:  openAICodexFingerprintSchemaV1,
		InstallationID: "550e8400-e29b-41d4-a716-446655440000",
		UAProfile:      accountProfile,
		CreatedAt:      "2026-06-16T00:00:00Z",
		UpdatedAt:      "2026-06-16T00:00:00Z",
	}, ParseOpenAICodexUAProfile(DefaultOpenAICodexUserAgent), time.Now())
	account := &Account{
		ID:       11,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			OpenAICodexFingerprintExtraKey: accountFingerprint,
		},
	}

	result, err := svc.GenerateAuthURLWithInput(context.Background(), OpenAIAuthURLInput{Platform: PlatformOpenAI, Account: account})
	require.NoError(t, err)

	parsed, err := url.Parse(result.AuthURL)
	require.NoError(t, err)
	q := parsed.Query()
	require.Equal(t, accountFingerprint.UAProfile.Originator, q.Get("originator"))

	session, ok := svc.sessionStore.Get(context.Background(), result.SessionID)
	require.True(t, ok)
	require.Equal(t, accountFingerprint, session.CodexFingerprint)
}
