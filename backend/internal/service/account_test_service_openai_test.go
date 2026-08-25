//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// --- shared test helpers ---

type queuedHTTPUpstream struct {
	responses []*http.Response
	requests  []*http.Request
	tlsFlags  []bool
}

func (u *queuedHTTPUpstream) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return nil, fmt.Errorf("unexpected Do call")
}

func (u *queuedHTTPUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	u.requests = append(u.requests, req)
	u.tlsFlags = append(u.tlsFlags, profile != nil)
	if len(u.responses) == 0 {
		return nil, fmt.Errorf("no mocked response")
	}
	resp := u.responses[0]
	u.responses = u.responses[1:]
	return resp, nil
}

func newJSONResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// --- test functions ---

func newTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/1/test", nil)
	return c, rec
}

func TestAccountTestService_OpenAIProbeModelNormalizationBoundaries(t *testing.T) {
	tests := []struct {
		name           string
		accountType    string
		model          string
		modelMapping   map[string]any
		wantUpstream   string
		wantEventModel string
	}{
		{
			name:           "oauth alias",
			accountType:    AccountTypeOAuth,
			model:          "gpt-5.6",
			wantUpstream:   "gpt-5.6-sol",
			wantEventModel: "gpt-5.6",
		},
		{
			name:           "setup token alias",
			accountType:    AccountTypeSetupToken,
			model:          "gpt-5.6",
			wantUpstream:   "gpt-5.6-sol",
			wantEventModel: "gpt-5.6",
		},
		{
			name:           "oauth account mapping before normalization",
			accountType:    AccountTypeOAuth,
			model:          "client-gpt",
			modelMapping:   map[string]any{"client-gpt": "gpt-5.6"},
			wantUpstream:   "gpt-5.6-sol",
			wantEventModel: "gpt-5.6",
		},
		{
			name:           "oauth already upstream",
			accountType:    AccountTypeOAuth,
			model:          "gpt-5.6-sol",
			wantUpstream:   "gpt-5.6-sol",
			wantEventModel: "gpt-5.6-sol",
		},
		{
			name:           "api key alias remains unchanged",
			accountType:    AccountTypeAPIKey,
			model:          "gpt-5.6",
			wantUpstream:   "gpt-5.6",
			wantEventModel: "gpt-5.6",
		},
		{
			name:           "oauth unknown model remains unchanged",
			accountType:    AccountTypeOAuth,
			model:          "future-model",
			wantUpstream:   "future-model",
			wantEventModel: "future-model",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, recorder := newTestContext()
			resp := newJSONResponse(http.StatusOK, "")
			resp.Body = io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\"}\n\n"))
			upstream := &queuedHTTPUpstream{responses: []*http.Response{resp}}
			svc := &AccountTestService{
				httpUpstream: upstream,
				cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
			}
			account := &Account{
				ID:          90,
				Platform:    PlatformOpenAI,
				Type:        tt.accountType,
				Concurrency: 1,
				Credentials: map[string]any{
					"access_token":  "test-token",
					"api_key":       "sk-test",
					"base_url":      "https://upstream.example",
					"model_mapping": tt.modelMapping,
				},
			}

			require.NoError(t, svc.testOpenAIAccountConnection(ctx, account, tt.model, "", ""))
			require.Len(t, upstream.requests, 1)
			body := readTestRequestBody(t, upstream.requests[0])
			require.Equal(t, tt.wantUpstream, gjson.GetBytes(body, "model").String())
			require.Contains(t, recorder.Body.String(), `"type":"test_start"`)
			require.Contains(t, recorder.Body.String(), `"model":"`+tt.wantEventModel+`"`)
		})
	}
}

type openAIAccountNestedBoolUpdate struct {
	updates   map[string]any
	mapKey    string
	nestedKey string
	value     bool
}

type openAIAccountTestRepo struct {
	mockAccountRepoForGemini
	updatedExtra       map[string]any
	observedAtKey      string
	observedAt         time.Time
	sessionWindowEnd   *time.Time
	nestedBoolUpdates  []openAIAccountNestedBoolUpdate
	updateExtraErr     error
	updateExtraErrFor  func(map[string]any) error
	bulkUpdatedIDs     []int64
	bulkUpdatedPayload AccountBulkUpdate
	rateLimitedID      int64
	rateLimitedAt      *time.Time
	clearedErrorID     int64
	setErrorID         int64
	setErrorMsg        string
}

func (r *openAIAccountTestRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	r.updatedExtra = updates
	if r.updateExtraErrFor != nil {
		if err := r.updateExtraErrFor(updates); err != nil {
			return err
		}
	}
	if r.updateExtraErr != nil {
		return r.updateExtraErr
	}
	return nil
}

func (r *openAIAccountTestRepo) UpdateRuntimeExtra(_ context.Context, _ int64, updates map[string]any, observedAtKey string, observedAt time.Time) (bool, error) {
	r.updatedExtra = updates
	r.observedAtKey = observedAtKey
	r.observedAt = observedAt
	if r.updateExtraErrFor != nil {
		if err := r.updateExtraErrFor(updates); err != nil {
			return false, err
		}
	}
	if r.updateExtraErr != nil {
		return false, r.updateExtraErr
	}
	return true, nil
}

func (r *openAIAccountTestRepo) UpdateSessionWindowEnd(_ context.Context, _ int64, end time.Time) error {
	r.sessionWindowEnd = &end
	return nil
}

func (r *openAIAccountTestRepo) UpdateExtraNestedBool(_ context.Context, _ int64, updates map[string]any, mapKey string, nestedKey string, value bool) error {
	r.updatedExtra = updates
	r.nestedBoolUpdates = append(r.nestedBoolUpdates, openAIAccountNestedBoolUpdate{
		updates:   updates,
		mapKey:    mapKey,
		nestedKey: nestedKey,
		value:     value,
	})
	if r.updateExtraErrFor != nil {
		if err := r.updateExtraErrFor(updates); err != nil {
			return err
		}
	}
	if r.updateExtraErr != nil {
		return r.updateExtraErr
	}
	return nil
}

func (r *openAIAccountTestRepo) BulkUpdate(_ context.Context, ids []int64, updates AccountBulkUpdate) (int64, error) {
	r.bulkUpdatedIDs = append([]int64(nil), ids...)
	r.bulkUpdatedPayload = updates
	return int64(len(ids)), nil
}

func (r *openAIAccountTestRepo) SetRateLimited(_ context.Context, id int64, resetAt time.Time) error {
	r.rateLimitedID = id
	r.rateLimitedAt = &resetAt
	return nil
}

func (r *openAIAccountTestRepo) ClearError(_ context.Context, id int64) error {
	r.clearedErrorID = id
	return nil
}

func (r *openAIAccountTestRepo) SetError(_ context.Context, id int64, errorMsg string) error {
	r.setErrorID = id
	r.setErrorMsg = errorMsg
	return nil
}

func TestProbeOpenAIAPIKeyResponsesSupportDefaultProbeWritesAccountMarkerOnly(t *testing.T) {
	repo := &openAIAccountTestRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{
		7: {
			ID:          7,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Concurrency: 1,
			Credentials: map[string]any{"api_key": "sk-test", "base_url": "https://upstream.example"},
		},
	}}}
	upstream := &queuedHTTPUpstream{responses: []*http.Response{newJSONResponse(http.StatusNotFound, `{"error":"not found"}`)}}
	svc := &AccountTestService{
		accountRepo:  repo,
		httpUpstream: upstream,
		cfg:          &config.Config{},
	}

	svc.ProbeOpenAIAPIKeyResponsesSupport(context.Background(), 7)

	require.Empty(t, repo.nestedBoolUpdates)
	require.Equal(t, map[string]any{openai_compat.ExtraKeyResponsesSupported: false}, repo.updatedExtra)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, "https://upstream.example/v1/responses", upstream.requests[0].URL.String())
	require.Empty(t, upstream.requests[0].Header.Get("x-openai-fedramp"))
	require.Equal(t, openai.DefaultTestModel, gjson.GetBytes(readTestRequestBody(t, upstream.requests[0]), "model").String())
}

func TestProbeOpenAIAPIKeyResponsesSupportDefaultProbeClearsStaleModelMap(t *testing.T) {
	repo := &openAIAccountTestRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{
		8: {
			ID:          8,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Concurrency: 1,
			Credentials: map[string]any{"api_key": "sk-test", "base_url": "https://upstream.example"},
			Extra: map[string]any{
				openai_compat.ExtraKeyResponsesSupportedByModel: map[string]any{"gpt-5.4": false},
			},
		},
	}}}
	upstream := &queuedHTTPUpstream{responses: []*http.Response{newJSONResponse(http.StatusNotFound, `{"error":"not found"}`)}}
	svc := &AccountTestService{
		accountRepo:  repo,
		httpUpstream: upstream,
		cfg:          &config.Config{},
	}

	svc.ProbeOpenAIAPIKeyResponsesSupport(context.Background(), 8)

	require.Empty(t, repo.nestedBoolUpdates)
	require.Equal(t, false, repo.updatedExtra[openai_compat.ExtraKeyResponsesSupported])
	require.Contains(t, repo.updatedExtra, openai_compat.ExtraKeyResponsesSupportedByModel)
	require.Nil(t, repo.updatedExtra[openai_compat.ExtraKeyResponsesSupportedByModel])
}

func TestProbeOpenAIAPIKeyResponsesSupportMappedProbeUsesNestedModelUpdate(t *testing.T) {
	repo := &openAIAccountTestRepo{mockAccountRepoForGemini: mockAccountRepoForGemini{accountsByID: map[int64]*Account{
		9: {
			ID:          9,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Concurrency: 1,
			Credentials: map[string]any{
				"api_key":  "sk-test",
				"base_url": "https://upstream.example",
				"model_mapping": map[string]any{
					"client-b": "zeta-model",
					"client-a": "alpha-model",
				},
			},
		},
	}}}
	upstream := &queuedHTTPUpstream{responses: []*http.Response{newJSONResponse(http.StatusOK, `{"output":[{"type":"function_call","name":"probe_ping"}]}`)}}
	svc := &AccountTestService{
		accountRepo:  repo,
		httpUpstream: upstream,
		cfg:          &config.Config{},
	}

	svc.ProbeOpenAIAPIKeyResponsesSupport(context.Background(), 9)

	require.Equal(t, map[string]any{openai_compat.ExtraKeyResponsesSupported: true}, repo.updatedExtra)
	require.Len(t, repo.nestedBoolUpdates, 1)
	require.Equal(t, openai_compat.ExtraKeyResponsesSupportedByModel, repo.nestedBoolUpdates[0].mapKey)
	require.Equal(t, "alpha-model", repo.nestedBoolUpdates[0].nestedKey)
	require.True(t, repo.nestedBoolUpdates[0].value)
	require.Equal(t, "alpha-model", gjson.GetBytes(readTestRequestBody(t, upstream.requests[0]), "model").String())
}

func readTestRequestBody(t *testing.T, req *http.Request) []byte {
	t.Helper()
	body, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	return body
}

func TestAccountTestService_OpenAISuccessPersistsSnapshotFromHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, recorder := newTestContext()

	resp := newJSONResponse(http.StatusOK, "")
	resp.Body = io.NopCloser(strings.NewReader(`data: {"type":"response.completed"}

`))
	resp.Header.Set("x-codex-primary-used-percent", "88")
	resp.Header.Set("x-codex-primary-reset-after-seconds", "604800")
	resp.Header.Set("x-codex-primary-window-minutes", "10080")
	resp.Header.Set("x-codex-secondary-used-percent", "42")
	resp.Header.Set("x-codex-secondary-reset-after-seconds", "18000")
	resp.Header.Set("x-codex-secondary-window-minutes", "300")

	repo := &openAIAccountTestRepo{}
	upstream := &queuedHTTPUpstream{responses: []*http.Response{resp}}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream}
	account := &Account{
		ID:          89,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": "test-token"},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "", "")
	require.NoError(t, err)
	require.Len(t, upstream.requests, 1)
	require.Equal(t, HTTPUpstreamProfileOpenAI, HTTPUpstreamProfileFromContext(upstream.requests[0].Context()))
	require.NotEmpty(t, repo.updatedExtra)
	require.Equal(t, 42.0, repo.updatedExtra["codex_5h_used_percent"])
	require.Equal(t, 88.0, repo.updatedExtra["codex_7d_used_percent"])
	require.Equal(t, "codex_usage_updated_at", repo.observedAtKey)
	parsed, err := runtimeExtraObservedAt(repo.updatedExtra, repo.observedAtKey)
	require.NoError(t, err)
	require.Equal(t, parsed, repo.observedAt)
	require.NotNil(t, repo.sessionWindowEnd)
	resetAt, err := parseTime(repo.updatedExtra["codex_5h_reset_at"].(string))
	require.NoError(t, err)
	require.Equal(t, resetAt, *repo.sessionWindowEnd)
	require.Contains(t, recorder.Body.String(), "test_complete")
}

func TestAccountTestService_OpenAIPersistsSnapshotOnlyAfterRepositorySuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, recorder := newTestContext()

	resp := newJSONResponse(http.StatusOK, "")
	resp.Body = io.NopCloser(strings.NewReader(`data: {"type":"response.completed"}

`))
	resp.Header.Set("x-codex-primary-used-percent", "88")
	resp.Header.Set("x-codex-primary-reset-after-seconds", "604800")
	resp.Header.Set("x-codex-primary-window-minutes", "10080")

	repo := &openAIAccountTestRepo{
		updateExtraErrFor: func(updates map[string]any) error {
			if _, ok := updates["codex_usage_updated_at"]; ok {
				return errors.New("snapshot persist failed")
			}
			return nil
		},
	}
	upstream := &queuedHTTPUpstream{responses: []*http.Response{resp}}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream}
	account := &Account{
		ID:          891,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": "test-token"},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "", "")
	require.Error(t, err)
	require.Contains(t, recorder.Body.String(), "Failed to persist Codex probe snapshot")
	require.Contains(t, repo.updatedExtra, "codex_usage_updated_at")
	require.NotContains(t, account.Extra, "codex_usage_updated_at")
	require.NotContains(t, account.Extra, "codex_7d_used_percent")
}

func TestAccountTestService_OpenAIErrorSanitizesOAuthUpstreamBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, recorder := newTestContext()

	installationID := "550e8400-e29b-41d4-a716-446655440000"
	threadID := "018fed75-1b7e-7000-8000-000000000123"
	accessToken := "setup-secret-access-token"
	resp := newJSONResponse(http.StatusUnauthorized, fmt.Sprintf(`{"error":{"message":"bad auth x-codex-installation-id=%s thread-id=%s Authorization=Bearer %s x-openai-fedramp=true {\"x-openai-fedramp\":true}"},"raw":{"x-codex-installation-id":"%s","thread-id":"%s","authorization":"Bearer %s"}}`, installationID, threadID, accessToken, installationID, threadID, accessToken))

	repo := &openAIAccountTestRepo{}
	upstream := &queuedHTTPUpstream{responses: []*http.Response{resp}}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream}
	account := &Account{
		ID:          892,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": accessToken},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "", "")
	require.Error(t, err)
	output := recorder.Body.String()
	require.Contains(t, output, "API returned 401")
	require.Contains(t, repo.setErrorMsg, "Authentication failed (401)")
	for _, leaked := range []string{installationID, threadID, accessToken, "setup-secret", "x-openai-fedramp=true", `"x-openai-fedramp":true`, `\"x-openai-fedramp\":true`} {
		require.NotContains(t, output, leaked)
		require.NotContains(t, repo.setErrorMsg, leaked)
	}
	require.Contains(t, output, "x-codex-installation-id=[redacted]")
	require.Contains(t, output, "thread-id=[redacted]")
	require.Contains(t, output, "x-openai-fedramp=[redacted]")
	require.Contains(t, output, `\"x-openai-fedramp\":\"[redacted]\"`)
	require.Contains(t, repo.setErrorMsg, "Authorization=[redacted]")
	require.Contains(t, repo.setErrorMsg, "x-openai-fedramp=[redacted]")
	require.Contains(t, repo.setErrorMsg, `"x-openai-fedramp":"[redacted]"`)
}

func TestAccountTestService_OpenAIOAuthDeactivatedWorkspace402MarksError(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "top-level code", body: `{"code":"deactivated_workspace"}`},
		{name: "detail code", body: `{"detail":{"code":"deactivated_workspace"}}`},
		{name: "error code", body: `{"error":{"code":"deactivated_workspace"}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, recorder := newTestContext()
			repo := &openAIAccountTestRepo{}
			upstream := &queuedHTTPUpstream{responses: []*http.Response{newJSONResponse(http.StatusPaymentRequired, tt.body)}}
			svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream}
			account := &Account{
				ID:          895,
				Platform:    PlatformOpenAI,
				Type:        AccountTypeOAuth,
				Concurrency: 1,
				Credentials: map[string]any{"access_token": "oauth-test"},
			}

			err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "", "")

			require.Error(t, err)
			require.Contains(t, recorder.Body.String(), "API returned 402")
			require.Equal(t, account.ID, repo.setErrorID)
			require.Contains(t, repo.setErrorMsg, "Workspace deactivated (402)")
		})
	}
}

func TestAccountTestService_OpenAIOAuthOther402DoesNotMarkError(t *testing.T) {
	ctx, _ := newTestContext()
	repo := &openAIAccountTestRepo{}
	upstream := &queuedHTTPUpstream{responses: []*http.Response{newJSONResponse(http.StatusPaymentRequired, `{"code":"billing_required"}`)}}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream}
	account := &Account{
		ID:          896,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": "oauth-test"},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "", "")

	require.Error(t, err)
	require.Zero(t, repo.setErrorID)
	require.Empty(t, repo.setErrorMsg)
}

func TestAccountTestService_OpenAIPATWorkspace403MarksError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, recorder := newTestContext()

	body := `{"error":{"message":"Personal access token owner is not an active member of the selected workspace."}}`
	repo := &openAIAccountTestRepo{}
	upstream := &queuedHTTPUpstream{responses: []*http.Response{newJSONResponse(http.StatusForbidden, body)}}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream}
	account := &Account{
		ID:          893,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"personal_access_token": "pat-test",
			"chatgpt_account_id":    "chatgpt-acc",
		},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "", "")

	require.Error(t, err)
	require.Contains(t, recorder.Body.String(), "API returned 403")
	require.Equal(t, account.ID, repo.setErrorID)
	require.Contains(t, repo.setErrorMsg, "Access forbidden (403)")
	require.Contains(t, repo.setErrorMsg, "Personal access token owner is not an active member of the selected workspace")
}

func TestAccountTestService_OpenAIPATOwnerInactive403MarksError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, recorder := newTestContext()

	body := `{"error":{"message":"Personal access token owner is inactive."}}`
	repo := &openAIAccountTestRepo{}
	upstream := &queuedHTTPUpstream{responses: []*http.Response{newJSONResponse(http.StatusForbidden, body)}}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream}
	account := &Account{
		ID:          894,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"personal_access_token": "pat-test",
			"chatgpt_account_id":    "chatgpt-acc",
		},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "", "")

	require.Error(t, err)
	require.Contains(t, recorder.Body.String(), "API returned 403")
	require.Equal(t, account.ID, repo.setErrorID)
	require.Contains(t, repo.setErrorMsg, "Access forbidden (403)")
	require.Contains(t, repo.setErrorMsg, "Personal access token owner is inactive")
}

func TestAccountTestService_OpenAIOAuthProbeSendsCodexFingerprint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := newTestContext()

	resp := newJSONResponse(http.StatusOK, "")
	resp.Body = io.NopCloser(strings.NewReader(`data: {"type":"response.completed"}

`))

	upstream := &httpUpstreamRecorder{resp: resp}
	svc := &AccountTestService{httpUpstream: upstream}
	account := &Account{
		ID:          92,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":               "test-token",
			"chatgpt_account_id":         "chatgpt-acc",
			"chatgpt_account_is_fedramp": true,
		},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "", "")
	require.NoError(t, err)
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, "chatgpt.com", upstream.lastReq.Host)
	require.Empty(t, upstream.lastReq.Header.Get("OpenAI-Beta"))
	require.Equal(t, codexOfficialOriginator, upstream.lastReq.Header.Get("originator"))
	require.Equal(t, codexCLIUserAgent, upstream.lastReq.Header.Get("User-Agent"))
	require.Equal(t, codexCLIVersion, upstream.lastReq.Header.Get("Version"))
	require.Equal(t, "chatgpt-acc", upstream.lastReq.Header.Get("chatgpt-account-id"))
	require.Equal(t, "true", upstream.lastReq.Header.Get("x-openai-fedramp"))
	require.NotEmpty(t, upstream.lastReq.Header.Get(openAICodexSessionIDHeader))
	require.NotEmpty(t, upstream.lastReq.Header.Get(openAICodexThreadIDHeader))
	require.NotEmpty(t, upstream.lastReq.Header.Get(openAICodexClientRequestIDHeader))
	require.NotEmpty(t, upstream.lastReq.Header.Get(openAICodexInstallationIDHeader))
	require.NotEmpty(t, upstream.lastReq.Header.Get(openAICodexWindowIDHeader))
	promptCacheKey := gjson.GetBytes(upstream.lastBody, "prompt_cache_key").String()
	require.Equal(t, upstream.lastReq.Header.Get(openAICodexThreadIDHeader), promptCacheKey)
	require.NotEqual(t, "probe_openai", promptCacheKey)
	require.Contains(t, promptCacheKey, "-")
	require.Equal(t, upstream.lastReq.Header.Get(openAICodexInstallationIDHeader), gjson.GetBytes(upstream.lastBody, "client_metadata.x-codex-installation-id").String())
	// Codex HTTP 不在 client_metadata 中放 x-codex-window-id，仅放在 HTTP header。
	require.False(t, gjson.GetBytes(upstream.lastBody, "client_metadata.x-codex-window-id").Exists())
}

func TestAccountTestService_OpenAIOAuthProbeOmitsFedRAMPHeaderWhenAccountMetadataFalse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := newTestContext()

	resp := newJSONResponse(http.StatusOK, "")
	resp.Body = io.NopCloser(strings.NewReader(`data: {"type":"response.completed"}

`))

	upstream := &httpUpstreamRecorder{resp: resp}
	svc := &AccountTestService{httpUpstream: upstream}
	account := &Account{
		ID:          925,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":               "test-token",
			"chatgpt_account_id":         "chatgpt-acc",
			"chatgpt_account_is_fedramp": false,
		},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "", "")
	require.NoError(t, err)
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, "chatgpt-acc", upstream.lastReq.Header.Get("chatgpt-account-id"))
	require.Empty(t, upstream.lastReq.Header.Get("x-openai-fedramp"))
}

func TestAccountTestService_OpenAIOAuthProbeIDsAreAccountScoped(t *testing.T) {
	gin.SetMode(gin.TestMode)

	probe := func(accountID int64) (threadID string, windowID string, promptCacheKey string) {
		ctx, _ := newTestContext()
		resp := newJSONResponse(http.StatusOK, "")
		resp.Body = io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\"}\n\n"))
		upstream := &httpUpstreamRecorder{resp: resp}
		repo := &snapshotUpdateAccountRepo{stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{{
			ID:          accountID,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeOAuth,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Credentials: map[string]any{"access_token": "test-token"},
		}}}}
		svc := &AccountTestService{
			accountRepo:             repo,
			httpUpstream:            upstream,
			codexFingerprintService: NewOpenAICodexFingerprintService(repo, nil),
		}
		account, err := repo.GetByID(context.Background(), accountID)
		require.NoError(t, err)
		require.NoError(t, svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "", ""))
		return upstream.lastReq.Header.Get(openAICodexThreadIDHeader),
			upstream.lastReq.Header.Get(openAICodexWindowIDHeader),
			gjson.GetBytes(upstream.lastBody, "prompt_cache_key").String()
	}

	threadA, windowA, promptA := probe(931)
	threadB, windowB, promptB := probe(932)
	require.NotEmpty(t, promptA)
	require.NotEmpty(t, promptB)
	require.NotEqual(t, "probe_openai", promptA)
	require.NotEqual(t, "probe_openai", promptB)
	require.NotEqual(t, promptA, promptB)
	require.NotEqual(t, threadA, threadB)
	require.NotEqual(t, windowA, windowB)
}

func TestAccountTestService_OpenAIStreamEOFBeforeCompletedFails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, recorder := newTestContext()

	resp := newJSONResponse(http.StatusOK, "")
	resp.Body = io.NopCloser(strings.NewReader(`data: {"type":"response.output_text.delta","delta":"hi"}

`))

	upstream := &queuedHTTPUpstream{responses: []*http.Response{resp}}
	svc := &AccountTestService{httpUpstream: upstream}
	account := &Account{
		ID:          90,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": "test-token"},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "", "")
	require.Error(t, err)
	require.Contains(t, recorder.Body.String(), "response.completed")
	require.NotContains(t, recorder.Body.String(), `"success":true`)
}

func TestAccountTestService_OpenAI429OAuthPersistsSnapshotWithoutRateLimitState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := newTestContext()

	resp := newJSONResponse(http.StatusTooManyRequests, `{"error":{"type":"usage_limit_reached","message":"limit reached","resets_at":1777283883}}`)
	resp.Header.Set("x-codex-primary-used-percent", "100")
	resp.Header.Set("x-codex-primary-reset-after-seconds", "604800")
	resp.Header.Set("x-codex-primary-window-minutes", "10080")
	resp.Header.Set("x-codex-secondary-used-percent", "100")
	resp.Header.Set("x-codex-secondary-reset-after-seconds", "18000")
	resp.Header.Set("x-codex-secondary-window-minutes", "300")

	repo := &openAIAccountTestRepo{}
	upstream := &queuedHTTPUpstream{responses: []*http.Response{resp}}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream}
	account := &Account{
		ID:          88,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusError,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": "test-token"},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "", "")
	require.Error(t, err)
	require.NotEmpty(t, repo.updatedExtra)
	require.Equal(t, 100.0, repo.updatedExtra["codex_5h_used_percent"])
	require.Zero(t, repo.rateLimitedID)
	require.Nil(t, repo.rateLimitedAt)
	require.Zero(t, repo.clearedErrorID)
	require.Equal(t, StatusError, account.Status)
	require.Empty(t, account.ErrorMessage)
	require.Nil(t, account.RateLimitResetAt)
}

func TestAccountTestService_OpenAI429OAuthBodyOnlyDoesNotRateLimitOrClearStaleError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := newTestContext()

	resp := newJSONResponse(http.StatusTooManyRequests, `{"error":{"type":"usage_limit_reached","message":"limit reached","resets_at":"1777283883"}}`)

	repo := &openAIAccountTestRepo{}
	upstream := &queuedHTTPUpstream{responses: []*http.Response{resp}}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream}
	account := &Account{
		ID:           77,
		Platform:     PlatformOpenAI,
		Type:         AccountTypeOAuth,
		Status:       StatusError,
		ErrorMessage: "Access forbidden (403): account may be suspended or lack permissions",
		Concurrency:  1,
		Credentials:  map[string]any{"access_token": "test-token"},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "", "")
	require.Error(t, err)
	require.Zero(t, repo.rateLimitedID)
	require.Nil(t, repo.rateLimitedAt)
	require.Zero(t, repo.clearedErrorID)
	require.Equal(t, StatusError, account.Status)
	require.Equal(t, "Access forbidden (403): account may be suspended or lack permissions", account.ErrorMessage)
	require.Nil(t, account.RateLimitResetAt)
	require.Contains(t, repo.updatedExtra, OpenAICodexFingerprintExtraKey)
	require.NotContains(t, repo.updatedExtra, "codex_5h_used_percent")
	require.NotContains(t, repo.updatedExtra, "codex_7d_used_percent")
}

func TestAccountTestService_OpenAI429SyncsObservedPlanType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := newTestContext()

	resp := newJSONResponse(http.StatusTooManyRequests, `{"error":{"type":"usage_limit_reached","message":"limit reached","plan_type":"free","resets_at":1777283883}}`)

	repo := &openAIAccountTestRepo{}
	upstream := &queuedHTTPUpstream{responses: []*http.Response{resp}}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream}
	account := &Account{
		ID:          81,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": "test-token", "plan_type": "plus"},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "", "")
	require.Error(t, err)
	require.Equal(t, []int64{account.ID}, repo.bulkUpdatedIDs)
	require.Equal(t, "free", repo.bulkUpdatedPayload.Credentials["plan_type"])
	require.Equal(t, "free", account.Credentials["plan_type"])
	require.Zero(t, repo.rateLimitedID)
	require.Nil(t, account.RateLimitResetAt)
}

func TestAccountTestService_OpenAI429OAuthActiveAccountDoesNotRateLimitOrClearError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := newTestContext()

	resp := newJSONResponse(http.StatusTooManyRequests, `{"error":{"type":"usage_limit_reached","message":"limit reached","resets_in_seconds":3600}}`)

	repo := &openAIAccountTestRepo{}
	upstream := &queuedHTTPUpstream{responses: []*http.Response{resp}}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream}
	account := &Account{
		ID:          78,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": "test-token"},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "", "")
	require.Error(t, err)
	require.Zero(t, repo.rateLimitedID)
	require.Nil(t, repo.rateLimitedAt)
	require.Zero(t, repo.clearedErrorID)
	require.Equal(t, StatusActive, account.Status)
	require.Nil(t, account.RateLimitResetAt)
}

func TestAccountTestService_OpenAI429WithoutResetSignalDoesNotMutateRuntimeState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := newTestContext()

	resp := newJSONResponse(http.StatusTooManyRequests, `{"error":{"type":"usage_limit_reached","message":"limit reached"}}`)

	repo := &openAIAccountTestRepo{}
	upstream := &queuedHTTPUpstream{responses: []*http.Response{resp}}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream}
	account := &Account{
		ID:           79,
		Platform:     PlatformOpenAI,
		Type:         AccountTypeOAuth,
		Status:       StatusError,
		ErrorMessage: "stale 403",
		Concurrency:  1,
		Credentials:  map[string]any{"access_token": "test-token"},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "", "")
	require.Error(t, err)
	require.Zero(t, repo.rateLimitedID)
	require.Nil(t, repo.rateLimitedAt)
	require.Zero(t, repo.clearedErrorID)
	require.Equal(t, StatusError, account.Status)
	require.Equal(t, "stale 403", account.ErrorMessage)
	require.Nil(t, account.RateLimitResetAt)
}

func TestAccountTestService_OpenAI401SetsPermanentErrorOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := newTestContext()

	resp := newJSONResponse(http.StatusUnauthorized, `{"error":"bad token"}`)

	repo := &openAIAccountTestRepo{}
	upstream := &queuedHTTPUpstream{responses: []*http.Response{resp}}
	svc := &AccountTestService{accountRepo: repo, httpUpstream: upstream}
	account := &Account{
		ID:          80,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Concurrency: 1,
		Credentials: map[string]any{"access_token": "test-token"},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "", "")
	require.Error(t, err)
	require.Equal(t, account.ID, repo.setErrorID)
	require.Contains(t, repo.setErrorMsg, "Authentication failed (401)")
	require.Zero(t, repo.rateLimitedID)
	require.Zero(t, repo.clearedErrorID)
	require.Nil(t, account.RateLimitResetAt)
}

func TestAccountTestService_OpenAIAPIKeyResponsesUnsupportedUsesChatCompletionsPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, recorder := newTestContext()

	upstreamBody := strings.Join([]string{
		`data: {"id":"chatcmpl_test","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"pong"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_test","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}
	svc := &AccountTestService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          91,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://compat-upstream.example/v1",
		},
		Extra: map[string]any{openai_compat.ExtraKeyResponsesSupported: false},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "hello", "")
	require.NoError(t, err)
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, HTTPUpstreamProfileOpenAI, HTTPUpstreamProfileFromContext(upstream.lastReq.Context()))
	require.Equal(t, "https://compat-upstream.example/v1/chat/completions", upstream.lastReq.URL.String())
	require.Equal(t, "Bearer sk-test", upstream.lastReq.Header.Get("Authorization"))
	require.Equal(t, "text/event-stream", upstream.lastReq.Header.Get("Accept"))
	require.Empty(t, upstream.lastReq.Header.Get("x-openai-fedramp"))
	require.Equal(t, "gpt-5.4", gjson.GetBytes(upstream.lastBody, "model").String())
	require.True(t, gjson.GetBytes(upstream.lastBody, "stream").Bool())
	require.Equal(t, "hello", gjson.GetBytes(upstream.lastBody, "messages.0.content").String())
	require.False(t, gjson.GetBytes(upstream.lastBody, "input").Exists())
	body := recorder.Body.String()
	require.Contains(t, body, "pong")
	require.Contains(t, body, "已通过 /v1/chat/completions 验证")
	require.Contains(t, body, `"success":true`)
	require.NotContains(t, body, "当前测试接口仅支持 Responses API 路径")
}

func TestAccountTestService_OpenAIChatCompletionsPathReturns4xx(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, recorder := newTestContext()

	upstream := &httpUpstreamRecorder{resp: newJSONResponse(http.StatusBadRequest, `{"error":{"message":"bad request"}}`)}
	svc := &AccountTestService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          92,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://compat-upstream.example",
		},
		Extra: map[string]any{openai_compat.ExtraKeyResponsesSupported: false},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "", "")
	require.Error(t, err)
	require.Equal(t, "https://compat-upstream.example/v1/chat/completions", upstream.lastReq.URL.String())
	require.Contains(t, err.Error(), "Chat Completions API (/v1/chat/completions) returned 400")
	require.Contains(t, recorder.Body.String(), "/v1/chat/completions")
	require.NotContains(t, recorder.Body.String(), `"success":true`)
}

func TestAccountTestService_OpenAIChatCompletionsPathTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, recorder := newTestContext()

	upstream := &httpUpstreamRecorder{err: context.DeadlineExceeded}
	svc := &AccountTestService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          93,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://compat-upstream.example",
		},
		Extra: map[string]any{openai_compat.ExtraKeyResponsesSupported: false},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "", "")
	require.Error(t, err)
	require.Equal(t, "https://compat-upstream.example/v1/chat/completions", upstream.lastReq.URL.String())
	require.Contains(t, err.Error(), "Chat Completions API (/v1/chat/completions) request failed")
	require.Contains(t, err.Error(), context.DeadlineExceeded.Error())
	require.Contains(t, recorder.Body.String(), "/v1/chat/completions")
	require.NotContains(t, recorder.Body.String(), `"success":true`)
}

func TestAccountTestService_OpenAIChatCompletionsPathRejectsNonJSONStream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, recorder := newTestContext()

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader("data: not-json\n\n")),
	}}
	svc := &AccountTestService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          94,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://compat-upstream.example",
		},
		Extra: map[string]any{openai_compat.ExtraKeyResponsesSupported: false},
	}

	err := svc.testOpenAIAccountConnection(ctx, account, "gpt-5.4", "", "")
	require.Error(t, err)
	require.Equal(t, "https://compat-upstream.example/v1/chat/completions", upstream.lastReq.URL.String())
	require.Contains(t, err.Error(), "Invalid Chat Completions response from /v1/chat/completions")
	require.Contains(t, recorder.Body.String(), "/v1/chat/completions")
	require.NotContains(t, recorder.Body.String(), `"success":true`)
}
