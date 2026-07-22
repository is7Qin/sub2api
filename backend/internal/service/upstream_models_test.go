package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/stretchr/testify/require"
)

func upstreamModelSyncTestConfig() *config.Config {
	return &config.Config{
		Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{Enabled: false},
		},
	}
}

func TestBuildV1ModelsURL(t *testing.T) {
	t.Parallel()

	require.Equal(t, "https://api.anthropic.com/v1/models", buildV1ModelsURL("https://api.anthropic.com"))
	require.Equal(t, "https://api.anthropic.com/v1/models", buildV1ModelsURL("https://api.anthropic.com/v1"))
	require.Equal(t, "https://api.anthropic.com/v1/models", buildV1ModelsURL("https://api.anthropic.com/v1/models"))
	require.Equal(t, "https://gateway.example.com/antigravity/v1/models", buildV1ModelsURL("https://gateway.example.com/antigravity/"))
}

func TestBuildOpenAIModelsURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		base string
		want string
	}{
		{
			name: "host fallback uses v1",
			base: "https://api.openai.com",
			want: "https://api.openai.com/v1/models",
		},
		{
			name: "openai v1 base url",
			base: "https://api.openai.com/v1",
			want: "https://api.openai.com/v1/models",
		},
		{
			name: "models url unchanged",
			base: "https://api.openai.com/v1/models",
			want: "https://api.openai.com/v1/models",
		},
		{
			name: "third party v4 base url",
			base: "https://open.bigmodel.cn/api/coding/paas/v4",
			want: "https://open.bigmodel.cn/api/coding/paas/v4/models",
		},
		{
			name: "third party v4 models url unchanged",
			base: "https://open.bigmodel.cn/api/coding/paas/v4/models",
			want: "https://open.bigmodel.cn/api/coding/paas/v4/models",
		},
		{
			name: "trailing slash on versioned base",
			base: "https://open.bigmodel.cn/api/coding/paas/v4/",
			want: "https://open.bigmodel.cn/api/coding/paas/v4/models",
		},
		{
			name: "non versioned path appends v1",
			base: "https://gateway.example.com/openai",
			want: "https://gateway.example.com/openai/v1/models",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tt.want, buildOpenAIModelsURL(tt.base))
		})
	}
}

func TestBuildGeminiModelsURL(t *testing.T) {
	t.Parallel()

	require.Equal(t, "https://generativelanguage.googleapis.com/v1beta/models", buildGeminiModelsURL("https://generativelanguage.googleapis.com"))
	require.Equal(t, "https://generativelanguage.googleapis.com/v1beta/models", buildGeminiModelsURL("https://generativelanguage.googleapis.com/v1beta"))
	require.Equal(t, "https://generativelanguage.googleapis.com/v1beta/models", buildGeminiModelsURL("https://generativelanguage.googleapis.com/v1beta/models"))
}

func TestExtractUpstreamModelIDs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "openai and anthropic data array",
			body: `{"data":[{"id":"claude-sonnet-4-5"},{"id":"gpt-5"},{"id":"gpt-5"},{"id":""}]}`,
			want: []string{"claude-sonnet-4-5", "gpt-5"},
		},
		{
			name: "gemini models array strips prefix",
			body: `{"models":[{"name":"models/gemini-2.5-pro"},{"name":"gemini-2.5-flash"}]}`,
			want: []string{"gemini-2.5-flash", "gemini-2.5-pro"},
		},
		{
			name: "top level array",
			body: `[{"id":"z-model"},{"name":"models/a-model"}]`,
			want: []string{"a-model", "z-model"},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := extractUpstreamModelIDs([]byte(tt.body))
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestBuildUpstreamModelsRequestsForAPIKeyAccounts(t *testing.T) {
	t.Parallel()

	svc := &AccountTestService{cfg: upstreamModelSyncTestConfig()}
	ctx := context.Background()

	anthropicReq, err := svc.buildAnthropicUpstreamModelsRequest(ctx, &Account{
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "anthropic-key",
			"base_url": "https://anthropic.example.com/v1",
		},
	})
	require.NoError(t, err)
	require.Equal(t, "https://anthropic.example.com/v1/models", anthropicReq.URL.String())
	require.Equal(t, "anthropic-key", getHeaderRaw(anthropicReq.Header, "x-api-key"))
	require.Equal(t, "anthropic-key", anthropicReq.Header["x-api-key"][0])
	require.Equal(t, "2023-06-01", anthropicReq.Header.Get("anthropic-version"))

	anthropicBearerReq, err := svc.buildAnthropicUpstreamModelsRequest(ctx, &Account{
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "ollama-key",
			"base_url": "https://ollama.com",
		},
		Extra: map[string]any{
			"anthropic_apikey_auth_scheme": AnthropicAPIKeyAuthSchemeAuthorizationBearer,
		},
	})
	require.NoError(t, err)
	require.Equal(t, "https://ollama.com/v1/models", anthropicBearerReq.URL.String())
	require.Equal(t, "Bearer ollama-key", getHeaderRaw(anthropicBearerReq.Header, "authorization"))
	require.Equal(t, "Bearer ollama-key", anthropicBearerReq.Header["authorization"][0])
	require.Empty(t, getHeaderRaw(anthropicBearerReq.Header, "x-api-key"))
	require.Equal(t, "2023-06-01", anthropicBearerReq.Header.Get("anthropic-version"))

	openAIReq, err := svc.buildOpenAIUpstreamModelsRequest(ctx, &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "openai-key",
			"base_url": "https://openai.example.com",
		},
	})
	require.NoError(t, err)
	require.Equal(t, "https://openai.example.com/v1/models", openAIReq.URL.String())
	require.Equal(t, "Bearer openai-key", openAIReq.Header.Get("Authorization"))

	geminiReq, err := svc.buildGeminiUpstreamModelsRequest(ctx, &Account{
		Platform: PlatformGemini,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "gemini-key",
			"base_url": "https://generativelanguage.googleapis.com/v1beta",
		},
	})
	require.NoError(t, err)
	require.Equal(t, "https://generativelanguage.googleapis.com/v1beta/models", geminiReq.URL.String())
	require.Equal(t, "gemini-key", geminiReq.Header.Get("x-goog-api-key"))

	antigravityReq, err := svc.buildAntigravityAPIKeyModelsRequest(ctx, &Account{
		Platform: PlatformAntigravity,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "antigravity-key",
			"base_url": "https://gateway.example.com/antigravity",
		},
	})
	require.NoError(t, err)
	require.Equal(t, "https://gateway.example.com/antigravity/v1/models", antigravityReq.URL.String())
	require.Equal(t, "antigravity-key", antigravityReq.Header.Get("x-api-key"))
}

func TestBuildAntigravityAPIKeyModelsRequestRejectsOfficialCloudCodeBase(t *testing.T) {
	t.Parallel()

	svc := &AccountTestService{cfg: upstreamModelSyncTestConfig()}
	_, err := svc.buildAntigravityAPIKeyModelsRequest(context.Background(), &Account{
		Platform: PlatformAntigravity,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "antigravity-key",
			"base_url": "https://cloudcode-pa.googleapis.com",
		},
	})
	require.Error(t, err)

	var syncErr *UpstreamModelSyncError
	require.True(t, errors.As(err, &syncErr))
	require.Equal(t, UpstreamModelSyncErrorUnsupported, syncErr.Kind)
	require.Contains(t, syncErr.SafeMessage(), "compatible gateway")
}

func TestBuildAnthropicUpstreamModelsRequestRejectsBedrock(t *testing.T) {
	t.Parallel()

	svc := &AccountTestService{cfg: upstreamModelSyncTestConfig()}
	_, err := svc.buildAnthropicUpstreamModelsRequest(context.Background(), &Account{
		Platform: PlatformAnthropic,
		Type:     AccountTypeBedrock,
	})
	require.Error(t, err)

	var syncErr *UpstreamModelSyncError
	require.True(t, errors.As(err, &syncErr))
	require.Equal(t, UpstreamModelSyncErrorUnsupported, syncErr.Kind)
}

func TestFetchUpstreamSupportedModelsParsesOpenAIResponse(t *testing.T) {
	t.Parallel()

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"data":[{"id":"gpt-5"},{"id":"gpt-5"},{"name":"o3"}]}`)),
	}}
	svc := &AccountTestService{
		httpUpstream: upstream,
		cfg:          upstreamModelSyncTestConfig(),
	}

	models, err := svc.FetchUpstreamSupportedModels(context.Background(), &Account{
		ID:       7,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "openai-key",
			"base_url": "https://openai.example.com/v1",
		},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"gpt-5", "o3"}, models)
	require.Equal(t, "https://openai.example.com/v1/models", upstream.lastReq.URL.String())
	require.Equal(t, "Bearer openai-key", upstream.lastReq.Header.Get("Authorization"))
}

func TestFetchOpenAIAgentIdentityUpstreamModelsRecoversTask(t *testing.T) {
	key, privateKey := newTestAgentIdentityKey(t)
	account := &Account{ID: 71, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1, Credentials: map[string]any{
		"auth_mode":          OpenAIAuthModeAgentIdentity,
		"agent_runtime_id":   key.runtimeID,
		"agent_private_key":  privateKey,
		"task_id":            "task-old",
		"chatgpt_account_id": "chatgpt-acc",
	}}
	repo := &agentIdentityCredentialsRepo{account: account}
	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"task_id":"task-new"}`)
	}))
	defer authServer.Close()
	oldAuthBase := openAIAgentIdentityAuthAPIBaseURL
	openAIAgentIdentityAuthAPIBaseURL = authServer.URL
	t.Cleanup(func() { openAIAgentIdentityAuthAPIBaseURL = oldAuthBase })

	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"invalid_task_id"}}`))},
		{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"models":[{"slug":"gpt-5.4"},{"slug":"o3"}]}`))},
	}}
	invalidator := &recordingAgentIdentityWSInvalidator{}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream, cfg: upstreamModelSyncTestConfig(), agentIdentityWSInvalidator: invalidator}
	models, err := svc.FetchUpstreamSupportedModels(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, []string{"gpt-5.4", "o3"}, models)
	require.Len(t, upstream.requests, 2)
	require.Equal(t, []int64{account.ID}, invalidator.snapshot())
	require.Equal(t, "task-old", decodeAgentIdentityAssertionTaskID(t, upstream.requests[0].Header.Get("Authorization")))
	require.Equal(t, "task-new", decodeAgentIdentityAssertionTaskID(t, upstream.requests[1].Header.Get("Authorization")))
	require.Equal(t, openAICodexUpstreamModelsURL, upstream.lastReq.URL.String())
	require.Equal(t, codexCLIUserAgent, upstream.lastReq.Header.Get("User-Agent"))
	require.Equal(t, "chatgpt-acc", upstream.lastReq.Header.Get("chatgpt-account-id"))
}

func TestFetchOpenAIAgentIdentityUpstreamModelsSecondFailureIsRedacted(t *testing.T) {
	key, privateKey := newTestAgentIdentityKey(t)
	account := &Account{ID: 72, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{
		"auth_mode": "agentIdentity", "agent_runtime_id": key.runtimeID, "agent_private_key": privateKey, "task_id": "task-old",
	}}
	repo := &agentIdentityCredentialsRepo{account: account}
	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"task_id":"task-new"}`)
	}))
	defer authServer.Close()
	oldAuthBase := openAIAgentIdentityAuthAPIBaseURL
	openAIAgentIdentityAuthAPIBaseURL = authServer.URL
	t.Cleanup(func() { openAIAgentIdentityAuthAPIBaseURL = oldAuthBase })
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"task_expired"}}`))},
		{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"task_expired","message":"` + privateKey + ` ` + key.runtimeID + ` task-new AgentAssertion secret"}}`))},
	}}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream, cfg: upstreamModelSyncTestConfig()}
	_, err := svc.FetchUpstreamSupportedModels(context.Background(), account)
	require.Error(t, err)
	require.Len(t, upstream.requests, 2)
	for _, secret := range []string{privateKey, key.runtimeID, "task-new", "AgentAssertion secret"} {
		require.NotContains(t, err.Error(), secret)
	}
	require.Contains(t, err.Error(), "[redacted]")
}

func TestFetchOpenAIAPIKeyUpstreamModelsDoesNotUseCodexManifest(t *testing.T) {
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader(`{"error":"denied"}`))}}
	svc := &AccountTestService{httpUpstream: upstream, cfg: upstreamModelSyncTestConfig()}
	_, err := svc.FetchUpstreamSupportedModels(context.Background(), &Account{ID: 73, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"api_key": "key", "base_url": "https://openai.example.com",
	}})
	require.Error(t, err)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, "https://openai.example.com/v1/models", upstream.lastReq.URL.String())
}

func TestFetchUpstreamSupportedModelsDoesNotExposeUpstreamBody(t *testing.T) {
	t.Parallel()

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusBadGateway,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":"SECRET_TOKEN should not be exposed"}`)),
	}}
	svc := &AccountTestService{
		httpUpstream: upstream,
		cfg:          upstreamModelSyncTestConfig(),
	}

	_, err := svc.FetchUpstreamSupportedModels(context.Background(), &Account{
		ID:       8,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "openai-key",
			"base_url": "https://openai.example.com/v1",
		},
	})
	require.Error(t, err)
	require.NotContains(t, err.Error(), "SECRET_TOKEN")

	var syncErr *UpstreamModelSyncError
	require.True(t, errors.As(err, &syncErr))
	require.Equal(t, UpstreamModelSyncErrorUpstream, syncErr.Kind)
	require.NotContains(t, syncErr.SafeMessage(), "SECRET_TOKEN")
	require.Contains(t, syncErr.SafeMessage(), "HTTP 502")
}

func TestAntigravityUpstreamModelsUsesConfiguredProjectFallback(t *testing.T) {
	var gotProject string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/v1internal:fetchAvailableModels", r.URL.Path)

		var req antigravity.FetchAvailableModelsRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		gotProject = req.Project

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":{"gemini-2.5-flash":{},"claude-sonnet-4-5":{}}}`))
	}))
	defer server.Close()
	withAntigravityModelSyncBaseURLs(t, []string{server.URL})

	svc := &AccountTestService{
		antigravityGatewayService: &AntigravityGatewayService{
			tokenProvider: &AntigravityTokenProvider{},
		},
	}
	account := &Account{
		ID:       77,
		Platform: PlatformAntigravity,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":                          "token",
			antigravityProjectFallbackCredentialKey: " configured-project ",
		},
	}

	models, err := svc.FetchUpstreamSupportedModels(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, []string{"claude-sonnet-4-5", "gemini-2.5-flash"}, models)
	require.Equal(t, "configured-project", gotProject)
	require.Empty(t, account.GetCredential("project_id"), "configured fallback must not backfill project_id")
}

func TestAntigravityUpstreamModelsBackfillsMissingProjectBeforeResolve(t *testing.T) {
	cache := &recordingAntigravityTokenCache{}
	probe := newAntigravityV1InternalProbe(t)

	svc := &AccountTestService{
		antigravityGatewayService: &AntigravityGatewayService{
			tokenProvider: newRecordingAntigravityTokenProvider(cache),
		},
	}
	account := &Account{
		ID:       78,
		Platform: PlatformAntigravity,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token": "token",
		},
	}

	models, err := svc.FetchUpstreamSupportedModels(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, []string{"gemini-2.5-flash"}, models)
	require.Equal(t, []string{"ag:account:78"}, cache.getCalls)
	require.Equal(t, []string{"ag:backfilled-project"}, cache.setCalls)
	require.Contains(t, probe.paths, "/v1internal:loadCodeAssist")
	require.Contains(t, probe.paths, "/v1internal:fetchAvailableModels")
	require.Equal(t, "backfilled-project", account.GetCredential("project_id"))
}

func TestAntigravityUpstreamModelsAPIKeyIgnoresConfiguredProjectFallback(t *testing.T) {
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"data":[{"id":"claude-sonnet-4-5"}]}`)),
	}}
	probe := newAntigravityV1InternalProbe(t)
	svc := &AccountTestService{
		httpUpstream: upstream,
		cfg:          upstreamModelSyncTestConfig(),
		antigravityGatewayService: &AntigravityGatewayService{
			tokenProvider: newRecordingAntigravityTokenProvider(&recordingAntigravityTokenCache{}),
		},
	}

	models, err := svc.FetchUpstreamSupportedModels(context.Background(), &Account{
		ID:       79,
		Platform: PlatformAntigravity,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":                               "antigravity-key",
			"base_url":                              "https://gateway.example.com/antigravity",
			antigravityProjectFallbackCredentialKey: " configured-project ",
		},
	})

	require.NoError(t, err)
	require.Equal(t, []string{"claude-sonnet-4-5"}, models)
	require.Equal(t, "https://gateway.example.com/antigravity/v1/models", upstream.lastReq.URL.String())
	require.Equal(t, "antigravity-key", upstream.lastReq.Header.Get("x-api-key"))
	require.Empty(t, probe.paths, "API-key model sync must not call Antigravity OAuth v1internal APIs")
}

func TestAntigravityUpstreamModelsUpstreamDoesNotUseOAuthFallback(t *testing.T) {
	cache := &recordingAntigravityTokenCache{}
	probe := newAntigravityV1InternalProbe(t)
	upstream := &httpUpstreamRecorder{}
	svc := &AccountTestService{
		httpUpstream: upstream,
		cfg:          upstreamModelSyncTestConfig(),
		antigravityGatewayService: &AntigravityGatewayService{
			tokenProvider: newRecordingAntigravityTokenProvider(cache),
		},
	}

	_, err := svc.FetchUpstreamSupportedModels(context.Background(), &Account{
		ID:       80,
		Platform: PlatformAntigravity,
		Type:     AccountTypeUpstream,
		Credentials: map[string]any{
			"api_key":                               "upstream-key",
			"base_url":                              "https://gateway.example.com/antigravity",
			antigravityProjectFallbackCredentialKey: " configured-project ",
		},
	})

	require.Error(t, err)
	cache.requireNoTokenWork(t)
	require.Empty(t, probe.paths)
	require.Nil(t, upstream.lastReq)

	var syncErr *UpstreamModelSyncError
	require.True(t, errors.As(err, &syncErr))
	require.Equal(t, UpstreamModelSyncErrorUnsupported, syncErr.Kind)
	require.Contains(t, syncErr.SafeMessage(), "Unsupported Antigravity account type")
}

func withAntigravityModelSyncBaseURLs(t *testing.T, urls []string) {
	t.Helper()
	origBaseURLs := antigravity.BaseURLs
	origBaseURL := antigravity.BaseURL
	antigravity.BaseURLs = urls
	if len(urls) > 0 {
		antigravity.BaseURL = urls[0]
	}
	t.Cleanup(func() {
		antigravity.BaseURLs = origBaseURLs
		antigravity.BaseURL = origBaseURL
	})
}
