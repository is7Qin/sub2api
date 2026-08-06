package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type openAIOAuthHandlerOAuthClientStub struct {
	lastExchangeOpts service.OpenAIOAuthTokenOptions
	lastRefreshToken string
	lastRefreshProxy string
	lastRefreshOpts  service.OpenAIOAuthTokenOptions
}

func (s *openAIOAuthHandlerOAuthClientStub) ExchangeCode(ctx context.Context, code, codeVerifier, redirectURI, proxyURL string, opts service.OpenAIOAuthTokenOptions) (*openai.TokenResponse, error) {
	s.lastExchangeOpts = opts
	return &openai.TokenResponse{
		AccessToken:  "handler-access-token",
		RefreshToken: "handler-refresh-token",
		ExpiresIn:    3600,
	}, nil
}

func (s *openAIOAuthHandlerOAuthClientStub) RefreshToken(ctx context.Context, refreshToken, proxyURL string) (*openai.TokenResponse, error) {
	return s.RefreshTokenWithOptions(ctx, refreshToken, proxyURL, service.OpenAIOAuthTokenOptions{})
}

func (s *openAIOAuthHandlerOAuthClientStub) RefreshTokenWithClientID(ctx context.Context, refreshToken, proxyURL string, clientID string) (*openai.TokenResponse, error) {
	return s.RefreshTokenWithOptions(ctx, refreshToken, proxyURL, service.OpenAIOAuthTokenOptions{ClientID: clientID})
}

func (s *openAIOAuthHandlerOAuthClientStub) RefreshTokenWithOptions(ctx context.Context, refreshToken, proxyURL string, opts service.OpenAIOAuthTokenOptions) (*openai.TokenResponse, error) {
	s.lastRefreshToken = refreshToken
	s.lastRefreshProxy = proxyURL
	s.lastRefreshOpts = opts
	return &openai.TokenResponse{
		AccessToken:  "refreshed-handler-access-token",
		RefreshToken: "refreshed-handler-refresh-token",
		ExpiresIn:    3600,
	}, nil
}

func setupOpenAIOAuthHandlerRouter(oauthSvc *service.OpenAIOAuthService, adminSvc service.AdminService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := NewOpenAIOAuthHandler(oauthSvc, adminSvc, nil, nil)
	router.POST("/api/v1/admin/openai/generate-auth-url", handler.GenerateAuthURL)
	router.POST("/api/v1/admin/openai/exchange-code", handler.ExchangeCode)
	router.POST("/api/v1/admin/openai/refresh-token", handler.RefreshToken)
	router.POST("/api/v1/admin/openai/create-from-refresh-token", handler.CreateAccountFromRefreshToken)
	return router
}

func performOpenAIOAuthJSONRequest(router *gin.Engine, method string, path string, payload any) *httptest.ResponseRecorder {
	body, _ := json.Marshal(payload)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	return rec
}

func TestOpenAIOAuthHandler_PublicTokenResponsesOmitCodexFingerprint(t *testing.T) {
	client := &openAIOAuthHandlerOAuthClientStub{}
	oauthSvc := service.NewOpenAIOAuthService(nil, client)
	defer oauthSvc.Stop()
	router := setupOpenAIOAuthHandlerRouter(oauthSvc, newStubAdminService())

	generateRec := performOpenAIOAuthJSONRequest(router, http.MethodPost, "/api/v1/admin/openai/generate-auth-url", map[string]any{})
	require.Equal(t, http.StatusOK, generateRec.Code)

	var generatePayload struct {
		Data struct {
			AuthURL   string `json:"auth_url"`
			SessionID string `json:"session_id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(generateRec.Body.Bytes(), &generatePayload))
	authURL, err := url.Parse(generatePayload.Data.AuthURL)
	require.NoError(t, err)
	state := authURL.Query().Get("state")
	require.NotEmpty(t, state)

	exchangeRec := performOpenAIOAuthJSONRequest(router, http.MethodPost, "/api/v1/admin/openai/exchange-code", map[string]any{
		"session_id": generatePayload.Data.SessionID,
		"code":       "auth-code",
		"state":      state,
	})
	require.Equal(t, http.StatusOK, exchangeRec.Code)
	require.Contains(t, exchangeRec.Body.String(), "handler-access-token")
	require.NotEmpty(t, client.lastExchangeOpts.UAProfile.Originator)
	require.NotContains(t, exchangeRec.Body.String(), service.OpenAICodexFingerprintExtraKey)
	require.NotContains(t, exchangeRec.Body.String(), "installation_id")
	require.NotContains(t, exchangeRec.Body.String(), "ua_profile")
	require.NotContains(t, exchangeRec.Body.String(), service.DefaultOpenAICodexUserAgent)

	refreshRec := performOpenAIOAuthJSONRequest(router, http.MethodPost, "/api/v1/admin/openai/refresh-token", map[string]any{
		"refresh_token": "manual-refresh-token",
		"client_id":     "manual-client-id",
	})
	require.Equal(t, http.StatusOK, refreshRec.Code)
	require.Contains(t, refreshRec.Body.String(), "refreshed-handler-access-token")
	require.Equal(t, "manual-refresh-token", client.lastRefreshToken)
	require.NotEmpty(t, client.lastRefreshOpts.UAProfile.Originator)
	require.NotContains(t, refreshRec.Body.String(), service.OpenAICodexFingerprintExtraKey)
	require.NotContains(t, refreshRec.Body.String(), "installation_id")
	require.NotContains(t, refreshRec.Body.String(), "ua_profile")
	require.NotContains(t, refreshRec.Body.String(), service.DefaultOpenAICodexUserAgent)
}

func TestOpenAIOAuthHandler_CreateAccountFromRefreshTokenPersistsServerOwnedFingerprint(t *testing.T) {
	client := &openAIOAuthHandlerOAuthClientStub{}
	oauthSvc := service.NewOpenAIOAuthService(nil, client)
	defer oauthSvc.Stop()
	adminSvc := newStubAdminService()
	router := setupOpenAIOAuthHandlerRouter(oauthSvc, adminSvc)

	rec := performOpenAIOAuthJSONRequest(router, http.MethodPost, "/api/v1/admin/openai/create-from-refresh-token", map[string]any{
		"refresh_token": "manual-refresh-token",
		"client_id":     "manual-client-id",
		"name":          "Manual OpenAI OAuth",
		"credentials": map[string]any{
			"client_id":     "client-supplied-client-id",
			"api_key":       "client-supplied-secret",
			"model_mapping": map[string]any{"gpt-test": "gpt-test-upstream"},
		},
		"extra": map[string]any{
			service.OpenAICodexFingerprintExtraKey: map[string]any{"present": true},
			"openai_oauth_ws_mode":                 "managed_session",
		},
	})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "manual-refresh-token", client.lastRefreshToken)
	require.Equal(t, "manual-client-id", client.lastRefreshOpts.ClientID)
	require.NotEmpty(t, client.lastRefreshOpts.UAProfile.Originator)

	require.Len(t, adminSvc.createdAccounts, 1)
	created := adminSvc.createdAccounts[0]
	require.Equal(t, "Manual OpenAI OAuth", created.Name)
	require.Equal(t, service.PlatformOpenAI, created.Platform)
	require.Equal(t, service.AccountTypeOAuth, created.Type)
	require.NotNil(t, created.OpenAICodexFingerprint)
	require.NotEmpty(t, created.OpenAICodexFingerprint.InstallationID)
	require.Equal(t, client.lastRefreshOpts.UAProfile, created.OpenAICodexFingerprint.UAProfile)
	require.Equal(t, "manual-client-id", created.Credentials["client_id"])
	require.Equal(t, "refreshed-handler-access-token", created.Credentials["access_token"])
	require.Equal(t, "refreshed-handler-refresh-token", created.Credentials["refresh_token"])
	require.Contains(t, created.Credentials, "model_mapping")
	require.NotContains(t, created.Credentials, "api_key")
}
