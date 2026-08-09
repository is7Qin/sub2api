package handler

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func compactBodySignalContext(path string, body []byte) *gin.Context {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	return c
}

func TestOpenAIRemoteCompactionTriggerMarksOperationWithoutChangingResponsesPath(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","stream":true,"store":true,"prompt_cache_key":"session-1","input":[{"type":"compaction_trigger"}]}`)
	c := compactBodySignalContext("/v1/responses", body)
	c.Request.Header.Set("User-Agent", "codex_cli_rs/0.136.0")

	require.True(t, service.PromoteOpenAICompactBodySignal(c, body, false))
	require.Equal(t, "/v1/responses", c.Request.URL.Path)
	require.True(t, service.IsOpenAICompactBodySignalRequest(c))
	require.True(t, gjson.GetBytes(body, "stream").Bool())
	require.True(t, gjson.GetBytes(body, "store").Bool())
	require.Equal(t, "session-1", gjson.GetBytes(body, "prompt_cache_key").String())
	require.Equal(t, "compaction_trigger", gjson.GetBytes(body, "input.0.type").String())
}

func TestOpenAICompactBodySignalRequiresOfficialCodexIdentity(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","input":[{"type":"compaction_trigger"}]}`)
	c := compactBodySignalContext("/v1/responses", body)

	require.False(t, service.PromoteOpenAICompactBodySignal(c, body, false))
	require.Equal(t, "/v1/responses", c.Request.URL.Path)
	require.False(t, service.IsOpenAICompactBodySignalRequest(c))
}

func TestOpenAICompactBodySignalOnlyPromotesBareResponsesEndpoints(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","input":[{"type":"compaction_trigger"}]}`)
	tests := []struct {
		name string
		path string
		want bool
	}{
		{name: "canonical v1 endpoint", path: "/v1/responses", want: true},
		{name: "root alias", path: "/responses", want: true},
		{name: "Codex backend alias", path: "/backend-api/codex/responses", want: true},
		{name: "response subpath ending in responses", path: "/v1/responses/resp_123/responses", want: false},
		{name: "other response subpath", path: "/v1/responses/resp_123/cancel", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := compactBodySignalContext(tt.path, body)
			c.Request.Header.Set("User-Agent", "codex_cli_rs/0.136.0")

			require.Equal(t, tt.want, service.PromoteOpenAICompactBodySignal(c, body, false))
			require.Equal(t, tt.path, c.Request.URL.Path)
		})
	}
}

func TestOpenAICompactBodySignalForceCodexCLIIsExplicitOverride(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","input":[{"type":"compaction_trigger"}]}`)
	c := compactBodySignalContext("/v1/responses", body)
	h := &OpenAIGatewayHandler{cfg: &config.Config{Gateway: config.GatewayConfig{ForceCodexCLI: true}}}

	require.True(t, service.PromoteOpenAICompactBodySignal(c, body, h.cfg.Gateway.ForceCodexCLI))
	require.Equal(t, "/v1/responses", c.Request.URL.Path)
}

type remoteCompactionHandlerAccountRepo struct {
	service.AccountRepository
	accounts []service.Account
}

func (r *remoteCompactionHandlerAccountRepo) ListSchedulableByPlatform(context.Context, string) ([]service.Account, error) {
	return append([]service.Account(nil), r.accounts...), nil
}

func (r *remoteCompactionHandlerAccountRepo) ListSchedulableByGroupIDAndPlatform(context.Context, int64, string) ([]service.Account, error) {
	return append([]service.Account(nil), r.accounts...), nil
}

func (r *remoteCompactionHandlerAccountRepo) ListSchedulableUngroupedByPlatform(context.Context, string) ([]service.Account, error) {
	return append([]service.Account(nil), r.accounts...), nil
}

func (r *remoteCompactionHandlerAccountRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	for i := range r.accounts {
		if r.accounts[i].ID == id {
			account := r.accounts[i]
			return &account, nil
		}
	}
	return nil, nil
}

type remoteCompactionHandlerUpstream struct {
	request  *http.Request
	body     []byte
	response *http.Response
}

func (u *remoteCompactionHandlerUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.request = req
	u.body, _ = io.ReadAll(req.Body)
	if u.response != nil {
		return u.response, nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "X-Request-Id": []string{"remote-compaction-apikey"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"resp_remote_compaction","model":"gpt-5.5","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)),
	}, nil
}

func (u *remoteCompactionHandlerUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, timeoutSeconds int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, timeoutSeconds)
}

func newRemoteCompactionHandler(t *testing.T, upstream *remoteCompactionHandlerUpstream) (*OpenAIGatewayHandler, int64) {
	t.Helper()
	groupID := int64(8801)
	accountRepo := &remoteCompactionHandlerAccountRepo{accounts: []service.Account{{
		ID: 9901, Name: "apikey-remote-compaction", Platform: service.PlatformOpenAI,
		Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 0,
		Credentials: map[string]any{"api_key": "sk-test", "base_url": "https://api.openai.com"},
		Extra: map[string]any{
			openai_compat.ExtraKeyResponsesMode:      string(openai_compat.ResponsesSupportModeAuto),
			openai_compat.ExtraKeyResponsesSupported: true,
		},
	}}}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Gateway.StreamKeepaliveInterval = 3600
	billingCache := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCache.Stop)
	concurrencyService := service.NewConcurrencyService(nil)
	gatewayService := service.NewOpenAIGatewayService(
		accountRepo, nil, nil, nil, nil, nil, nil, cfg, nil, concurrencyService,
		service.NewBillingService(cfg, nil), nil, billingCache, upstream,
		&service.DeferredService{}, nil, nil, nil, nil, nil, nil,
		nil, nil, // usageRecordWorkerPool
	)
	return NewOpenAIGatewayHandler(gatewayService, concurrencyService, billingCache, &service.APIKeyService{}, nil, nil, nil, cfg), groupID
}

func invokeRemoteCompactionHandler(t *testing.T, h *OpenAIGatewayHandler, groupID int64, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("User-Agent", "codex_cli_rs/0.136.0")
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
		ID: 7701, GroupID: &groupID, User: &service.User{ID: 6601, Status: service.StatusActive},
		Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive},
	})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 6601, Concurrency: 0})
	h.Responses(c)
	return rec
}

func TestOpenAIResponses_RemoteCompactionTriggerSchedulesAPIKeyThroughRealHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logSink, restore := captureHandlerStructuredLog(t)
	defer restore()
	upstream := &remoteCompactionHandlerUpstream{}
	h, groupID := newRemoteCompactionHandler(t, upstream)
	body := `{"model":"gpt-5.5","stream":false,"input":[{"type":"compaction_trigger"}]}`

	rec := invokeRemoteCompactionHandler(t, h, groupID, "/v1/responses", body)

	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, upstream.request)
	require.Equal(t, "https://api.openai.com/v1/responses", upstream.request.URL.String())
	require.Equal(t, "compaction_trigger", gjson.GetBytes(upstream.body, "input.0.type").String())
	event := logSink.EventWithMessage("codex.remote_compact.succeeded")
	require.NotNil(t, event)
	requireLogField(t, event, "compact_outcome", "succeeded")
	requireLogField(t, event, "status_code", int64(http.StatusOK))
}

func TestOpenAIResponses_RemoteCompactOutcomeTracksForwardedSSETerminal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name        string
		upstreamSSE string
		wantOutcome string
	}{
		{
			name: "response failed",
			upstreamSSE: "event: response.failed\n" +
				`data: {"type":"response.failed","response":{"id":"resp_remote_failed","status":"failed","error":{"type":"invalid_request_error","code":"content_policy_violation","message":"request violates safety policy"}}}` + "\n\n",
			wantOutcome: "failed",
		},
		{
			name: "missing terminal",
			upstreamSSE: "event: response.output_text.delta\n" +
				`data: {"type":"response.output_text.delta","delta":"partial"}` + "\n\n",
			wantOutcome: "failed",
		},
		{
			name: "response completed",
			upstreamSSE: "event: response.completed\n" +
				`data: {"type":"response.completed","response":{"id":"resp_remote_ok","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n",
			wantOutcome: "succeeded",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logSink, restore := captureHandlerStructuredLog(t)
			defer restore()
			upstream := &remoteCompactionHandlerUpstream{response: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "X-Request-Id": []string{"remote-compact-terminal"}},
				Body:       io.NopCloser(strings.NewReader(tt.upstreamSSE)),
			}}
			h, groupID := newRemoteCompactionHandler(t, upstream)
			body := `{"model":"gpt-5.5","stream":true,"input":[{"type":"compaction_trigger"}]}`

			rec := invokeRemoteCompactionHandler(t, h, groupID, "/v1/responses", body)

			require.Equal(t, http.StatusOK, rec.Code)
			event := logSink.EventWithMessage("codex.remote_compact." + tt.wantOutcome)
			require.NotNil(t, event)
			requireLogField(t, event, "compact_outcome", tt.wantOutcome)
			requireLogField(t, event, "status_code", int64(http.StatusOK))
		})
	}
}

func TestOpenAIResponses_RemoteCompactOutcomeLegacyCompactStillUsesHTTPStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logSink, restore := captureHandlerStructuredLog(t)
	defer restore()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(`{"model":"gpt-5.5"}`))
	c.Status(http.StatusOK)

	(&OpenAIGatewayHandler{}).logOpenAIRemoteCompactOutcome(c, time.Now())

	event := logSink.EventWithMessage("codex.remote_compact.succeeded")
	require.NotNil(t, event)
	requireLogField(t, event, "compact_protocol", "legacy")
}
