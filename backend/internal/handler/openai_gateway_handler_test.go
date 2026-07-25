package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/model"
	pkghttputil "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type fixedErrorPassthroughRepo struct {
	rules []*model.ErrorPassthroughRule
}

type fixedOpenAIHTTPUpstream struct {
	response *http.Response
}

func (u *fixedOpenAIHTTPUpstream) Do(*http.Request, string, int64, int) (*http.Response, error) {
	return u.response, nil
}
func (u *fixedOpenAIHTTPUpstream) DoWithTLS(*http.Request, string, int64, int, *tlsfingerprint.Profile) (*http.Response, error) {
	return u.response, nil
}

func (r *fixedErrorPassthroughRepo) List(context.Context) ([]*model.ErrorPassthroughRule, error) {
	return r.rules, nil
}
func (r *fixedErrorPassthroughRepo) GetByID(context.Context, int64) (*model.ErrorPassthroughRule, error) {
	return nil, errors.New("not implemented")
}
func (r *fixedErrorPassthroughRepo) Create(context.Context, *model.ErrorPassthroughRule) (*model.ErrorPassthroughRule, error) {
	return nil, errors.New("not implemented")
}
func (r *fixedErrorPassthroughRepo) Update(context.Context, *model.ErrorPassthroughRule) (*model.ErrorPassthroughRule, error) {
	return nil, errors.New("not implemented")
}
func (r *fixedErrorPassthroughRepo) Delete(context.Context, int64) error {
	return errors.New("not implemented")
}

func TestOpenAIHandleFailoverExhaustedSanitizedResponseBypassesMatchingRule(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstreamBody := `{"error":{"message":"private upstream temporarily unavailable"},"debug":"sk-upstream-secret"}`
	upstream := &fixedOpenAIHTTPUpstream{response: &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Header: http.Header{
			"Content-Type": {"text/html"},
			"Retry-After":  {"17"},
			"Set-Cookie":   {"private=1"},
			"X-Request-Id": {"private-id"},
		},
		Body: io.NopCloser(strings.NewReader(upstreamBody)),
	}}
	forwardContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	forwardContext.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	cfg := &config.Config{Gateway: config.GatewayConfig{ForceCodexCLI: false}}
	forwarder := service.NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil)
	account := &service.Account{
		ID: 91, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test", "base_url": "https://api.example.test"},
		Extra:       map[string]any{"openai_passthrough": true}, Status: service.StatusActive, Schedulable: true,
	}
	_, err := forwarder.Forward(context.Background(), forwardContext, account, []byte(`{"model":"gpt-5.2","input":"hello"}`))
	var failoverErr *service.UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)

	customStatus := http.StatusTeapot
	customMessage := "rule leaked private upstream body"
	ruleService := service.NewErrorPassthroughService(&fixedErrorPassthroughRepo{rules: []*model.ErrorPassthroughRule{{
		Name: "adversarial", Enabled: true, Priority: 1, ErrorCodes: []int{http.StatusServiceUnavailable},
		MatchMode: model.MatchModeAny, Platforms: []string{model.PlatformOpenAI}, ResponseCode: &customStatus,
		CustomMessage: &customMessage,
	}}}, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Header("Set-Cookie", "handler-secret=1")
	c.Header("X-Request-Id", "handler-private-id")

	(&OpenAIGatewayHandler{errorPassthroughService: ruleService}).handleFailoverExhausted(c, failoverErr, false)

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.Equal(t, "Upstream service temporarily unavailable", gjson.Get(recorder.Body.String(), "error.message").String())
	require.NotContains(t, recorder.Body.String(), "private upstream")
	require.NotContains(t, recorder.Body.String(), "sk-upstream-secret")
	require.NotContains(t, recorder.Body.String(), customMessage)
	require.Equal(t, "application/json; charset=utf-8", recorder.Header().Get("Content-Type"))
	require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
	require.Equal(t, "17", recorder.Header().Get("Retry-After"))
	require.Empty(t, recorder.Header().Get("Set-Cookie"))
	require.Empty(t, recorder.Header().Get("X-Request-Id"))

	streamRecorder := httptest.NewRecorder()
	streamContext, _ := gin.CreateTestContext(streamRecorder)
	streamContext.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	streamContext.Header("Set-Cookie", "committed-header=preserved")
	_, _ = streamContext.Writer.WriteString(":\n\n")

	(&OpenAIGatewayHandler{errorPassthroughService: ruleService}).handleFailoverExhausted(streamContext, failoverErr, true)

	require.Equal(t, http.StatusOK, streamRecorder.Code, "committed status cannot be replaced")
	require.Equal(t, "committed-header=preserved", streamRecorder.Header().Get("Set-Cookie"), "committed headers cannot be cleared")
	require.Contains(t, streamRecorder.Body.String(), "event: response.failed\n")
	require.Contains(t, streamRecorder.Body.String(), "Upstream service temporarily unavailable")
	require.NotContains(t, streamRecorder.Body.String(), "private upstream")
	require.NotContains(t, streamRecorder.Body.String(), "sk-upstream-secret")
	require.NotContains(t, streamRecorder.Body.String(), customMessage)
}

func TestOpenAIHandleStreamingAwareError_JSONEscaping(t *testing.T) {
	tests := []struct {
		name    string
		errType string
		message string
	}{
		{
			name:    "包含双引号的消息",
			errType: "server_error",
			message: `upstream returned "invalid" response`,
		},
		{
			name:    "包含反斜杠的消息",
			errType: "server_error",
			message: `path C:\Users\test\file.txt not found`,
		},
		{
			name:    "包含双引号和反斜杠的消息",
			errType: "upstream_error",
			message: `error parsing "key\value": unexpected token`,
		},
		{
			name:    "包含换行符的消息",
			errType: "server_error",
			message: "line1\nline2\ttab",
		},
		{
			name:    "普通消息",
			errType: "upstream_error",
			message: "Upstream service temporarily unavailable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

			h := &OpenAIGatewayHandler{}
			h.handleStreamingAwareError(c, http.StatusBadGateway, tt.errType, tt.message, true)

			body := w.Body.String()

			// 验证 SSE 格式：event: error\ndata: {JSON}\n\n
			assert.True(t, strings.HasPrefix(body, "event: error\n"), "应以 'event: error\\n' 开头")
			assert.True(t, strings.HasSuffix(body, "\n\n"), "应以 '\\n\\n' 结尾")

			// 提取 data 部分
			lines := strings.Split(strings.TrimSuffix(body, "\n\n"), "\n")
			require.Len(t, lines, 2, "应有 event 行和 data 行")
			dataLine := lines[1]
			require.True(t, strings.HasPrefix(dataLine, "data: "), "第二行应以 'data: ' 开头")
			jsonStr := strings.TrimPrefix(dataLine, "data: ")

			// 验证 JSON 合法性
			var parsed map[string]any
			err := json.Unmarshal([]byte(jsonStr), &parsed)
			require.NoError(t, err, "JSON 应能被成功解析，原始 JSON: %s", jsonStr)

			// 验证结构
			errorObj, ok := parsed["error"].(map[string]any)
			require.True(t, ok, "应包含 error 对象")
			assert.Equal(t, tt.errType, errorObj["type"])
			assert.Equal(t, tt.message, errorObj["message"])
		})
	}
}

func TestResolveOpenAIMessagesMetadataSession_DoesNotDerivePromptCacheKey(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","metadata":{"user_id":"claude-code-session"},"messages":[{"role":"user","content":"hello"}]}`)

	sessionHash, promptCacheKey := resolveOpenAIMessagesMetadataSession("", "", "claude-sonnet-4-5", body)

	require.NotEmpty(t, sessionHash)
	require.Empty(t, promptCacheKey)
}

func TestResolveOpenAIMessagesMetadataSession_PreservesExplicitPromptCacheKey(t *testing.T) {
	body := []byte(`{"metadata":{"user_id":"claude-code-session"}}`)

	sessionHash, promptCacheKey := resolveOpenAIMessagesMetadataSession("", "explicit-cache", "claude-sonnet-4-5", body)

	require.NotEmpty(t, sessionHash)
	require.Equal(t, "explicit-cache", promptCacheKey)
}

func TestOpenAIHandleStreamingAwareError_NonStreaming(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	h := &OpenAIGatewayHandler{}
	h.handleStreamingAwareError(c, http.StatusBadGateway, "upstream_error", "test error", false)

	// 非流式应返回 JSON 响应
	assert.Equal(t, http.StatusBadGateway, w.Code)

	var parsed map[string]any
	err := json.Unmarshal(w.Body.Bytes(), &parsed)
	require.NoError(t, err)
	errorObj, ok := parsed["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "upstream_error", errorObj["type"])
	assert.Equal(t, "test error", errorObj["message"])
}

func TestReadRequestBodyWithPrealloc(t *testing.T) {
	payload := `{"model":"gpt-5","input":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(payload))
	req.ContentLength = int64(len(payload))

	body, err := pkghttputil.ReadRequestBodyWithPrealloc(req)
	require.NoError(t, err)
	require.Equal(t, payload, string(body))
}

func TestReadRequestBodyWithPrealloc_MaxBytesError(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(strings.Repeat("x", 8)))
	req.Body = http.MaxBytesReader(rec, req.Body, 4)

	_, err := pkghttputil.ReadRequestBodyWithPrealloc(req)
	require.Error(t, err)
	var maxErr *http.MaxBytesError
	require.ErrorAs(t, err, &maxErr)
}

func TestOpenAIEnsureForwardErrorResponse_WritesFallbackWhenNotWritten(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	h := &OpenAIGatewayHandler{}
	wrote := h.ensureForwardErrorResponse(c, false)

	require.True(t, wrote)
	require.Equal(t, http.StatusBadGateway, w.Code)

	var parsed map[string]any
	err := json.Unmarshal(w.Body.Bytes(), &parsed)
	require.NoError(t, err)
	errorObj, ok := parsed["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "upstream_error", errorObj["type"])
	assert.Equal(t, "Upstream request failed", errorObj["message"])
}

// Writer 已写后 ensureForwardErrorResponse 必须仍然把错误信息以 SSE
// 形式追加给客户端（streamStarted 强制 true）。
// 这是 case B 修复：旧实现遇到 Writer.Written 直接 return false，
// 客户端只能拿到 silent EOF；Codex CLI 报 "stream closed before response.completed"。
func TestOpenAIEnsureForwardErrorResponse_AppendsSSEAfterWritten(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.String(http.StatusTeapot, "already written")

	h := &OpenAIGatewayHandler{}
	wrote := h.ensureForwardErrorResponse(c, false)

	require.True(t, wrote, "must attempt to communicate the failure to the client via SSE")
	// 状态码改不了（headers 已 flush），但 body 应该追加 SSE 错误事件。
	require.Equal(t, http.StatusTeapot, w.Code)
	assert.Contains(t, w.Body.String(), "already written")
	// 非 /responses 路径走 legacy event: error 分支。
	assert.Contains(t, w.Body.String(), "event: error\n")
}

// case B 回归测试：/responses 路径，Writer 已被写过（模拟 ping flushed），
// ensureForwardErrorResponse 必须发 response.failed，让 Codex 收到合规终止事件。
func TestOpenAIEnsureForwardErrorResponse_ResponsesRouteAfterWrittenEmitsResponseFailed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, EndpointResponses, nil)
	// 模拟 ping 已 flush 的状态：Writer 已写过 1 个字节
	_, _ = c.Writer.WriteString(":\n\n")

	h := &OpenAIGatewayHandler{}
	wrote := h.ensureForwardErrorResponse(c, false)

	require.True(t, wrote)
	body := w.Body.String()
	assert.Contains(t, body, ":\n\n", "earlier ping bytes preserved")
	assert.Contains(t, body, "event: response.failed\n", "appended a Responses terminal event")
	assert.Contains(t, body, `"type":"response.failed"`)
	assert.Contains(t, body, `"code":"upstream_error"`)
	assert.Contains(t, body, "Upstream request failed")
}

func TestShouldLogOpenAIForwardFailureAsWarn(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("fallback_written_should_not_downgrade", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		require.False(t, shouldLogOpenAIForwardFailureAsWarn(c, true))
	})

	t.Run("context_nil_should_not_downgrade", func(t *testing.T) {
		require.False(t, shouldLogOpenAIForwardFailureAsWarn(nil, false))
	})

	t.Run("response_not_written_should_not_downgrade", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		require.False(t, shouldLogOpenAIForwardFailureAsWarn(c, false))
	})

	t.Run("response_already_written_should_downgrade", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		c.String(http.StatusForbidden, "already written")
		require.True(t, shouldLogOpenAIForwardFailureAsWarn(c, false))
	})
}

func TestOpenAIRecoverResponsesPanic_WritesFallbackResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	h := &OpenAIGatewayHandler{}
	streamStarted := false
	require.NotPanics(t, func() {
		func() {
			defer h.recoverResponsesPanic(c, &streamStarted)
			panic("test panic")
		}()
	})

	require.Equal(t, http.StatusBadGateway, w.Code)

	var parsed map[string]any
	err := json.Unmarshal(w.Body.Bytes(), &parsed)
	require.NoError(t, err)

	errorObj, ok := parsed["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "upstream_error", errorObj["type"])
	assert.Equal(t, "Upstream request failed", errorObj["message"])
}

func TestOpenAIRecoverResponsesPanic_NoPanicNoWrite(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	h := &OpenAIGatewayHandler{}
	streamStarted := false
	require.NotPanics(t, func() {
		func() {
			defer h.recoverResponsesPanic(c, &streamStarted)
		}()
	})

	require.False(t, c.Writer.Written())
	assert.Equal(t, "", w.Body.String())
}

// Panic 在已 flush 的 /v1/responses 流中：状态码无法改（已 written），
// 但 body 应追加 response.failed 让客户端识别为合规截断而不是 silent EOF。
func TestOpenAIRecoverResponsesPanic_AppendsResponseFailedAfterWritten(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.String(http.StatusTeapot, "already written")

	h := &OpenAIGatewayHandler{}
	streamStarted := false
	require.NotPanics(t, func() {
		func() {
			defer h.recoverResponsesPanic(c, &streamStarted)
			panic("test panic")
		}()
	})

	require.Equal(t, http.StatusTeapot, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "already written")
	assert.Contains(t, body, "event: response.failed\n")
}

func TestOpenAIMissingResponsesDependencies(t *testing.T) {
	t.Run("nil_handler", func(t *testing.T) {
		var h *OpenAIGatewayHandler
		require.Equal(t, []string{"handler"}, h.missingResponsesDependencies())
	})

	t.Run("all_dependencies_missing", func(t *testing.T) {
		h := &OpenAIGatewayHandler{}
		require.Equal(t,
			[]string{"gatewayService", "billingCacheService", "apiKeyService", "concurrencyHelper"},
			h.missingResponsesDependencies(),
		)
	})

	t.Run("all_dependencies_present", func(t *testing.T) {
		h := &OpenAIGatewayHandler{
			gatewayService:      &service.OpenAIGatewayService{},
			billingCacheService: &service.BillingCacheService{},
			apiKeyService:       &service.APIKeyService{},
			concurrencyHelper: &ConcurrencyHelper{
				concurrencyService: &service.ConcurrencyService{},
			},
		}
		require.Empty(t, h.missingResponsesDependencies())
	})
}

func TestOpenAIEnsureResponsesDependencies(t *testing.T) {
	t.Run("missing_dependencies_returns_503", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

		h := &OpenAIGatewayHandler{}
		ok := h.ensureResponsesDependencies(c, nil)

		require.False(t, ok)
		require.Equal(t, http.StatusServiceUnavailable, w.Code)
		var parsed map[string]any
		err := json.Unmarshal(w.Body.Bytes(), &parsed)
		require.NoError(t, err)
		errorObj, exists := parsed["error"].(map[string]any)
		require.True(t, exists)
		assert.Equal(t, "api_error", errorObj["type"])
		assert.Equal(t, "Service temporarily unavailable", errorObj["message"])
	})

	t.Run("already_written_response_not_overridden", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		c.String(http.StatusTeapot, "already written")

		h := &OpenAIGatewayHandler{}
		ok := h.ensureResponsesDependencies(c, nil)

		require.False(t, ok)
		require.Equal(t, http.StatusTeapot, w.Code)
		assert.Equal(t, "already written", w.Body.String())
	})

	t.Run("dependencies_ready_returns_true_and_no_write", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

		h := &OpenAIGatewayHandler{
			gatewayService:      &service.OpenAIGatewayService{},
			billingCacheService: &service.BillingCacheService{},
			apiKeyService:       &service.APIKeyService{},
			concurrencyHelper: &ConcurrencyHelper{
				concurrencyService: &service.ConcurrencyService{},
			},
		}
		ok := h.ensureResponsesDependencies(c, nil)

		require.True(t, ok)
		require.False(t, c.Writer.Written())
		assert.Equal(t, "", w.Body.String())
	})
}

func TestResolveOpenAIMessagesDispatchMappedModel(t *testing.T) {
	t.Run("exact_claude_model_override_wins", func(t *testing.T) {
		apiKey := &service.APIKey{
			Group: &service.Group{
				MessagesDispatchModelConfig: service.OpenAIMessagesDispatchModelConfig{
					SonnetMappedModel: "gpt-5.2",
					ExactModelMappings: map[string]string{
						"claude-sonnet-4-5-20250929": "gpt-5.4-mini-high",
					},
				},
			},
		}
		require.Equal(t, "gpt-5.4-mini", resolveOpenAIMessagesDispatchMappedModel(apiKey, "claude-sonnet-4-5-20250929"))
	})

	t.Run("uses_family_default_when_no_override", func(t *testing.T) {
		apiKey := &service.APIKey{Group: &service.Group{}}
		require.Equal(t, "gpt-5.4", resolveOpenAIMessagesDispatchMappedModel(apiKey, "claude-opus-4-6"))
		require.Equal(t, "gpt-5.3-codex", resolveOpenAIMessagesDispatchMappedModel(apiKey, "claude-sonnet-4-5-20250929"))
		require.Equal(t, "gpt-5.4-mini", resolveOpenAIMessagesDispatchMappedModel(apiKey, "claude-haiku-4-5-20251001"))
	})

	t.Run("returns_empty_for_non_claude_or_missing_group", func(t *testing.T) {
		require.Empty(t, resolveOpenAIMessagesDispatchMappedModel(nil, "claude-sonnet-4-5-20250929"))
		require.Empty(t, resolveOpenAIMessagesDispatchMappedModel(&service.APIKey{}, "claude-sonnet-4-5-20250929"))
		require.Empty(t, resolveOpenAIMessagesDispatchMappedModel(&service.APIKey{Group: &service.Group{}}, "gpt-5.4"))
	})

	t.Run("does_not_fall_back_to_group_default_mapped_model", func(t *testing.T) {
		apiKey := &service.APIKey{
			Group: &service.Group{
				DefaultMappedModel: "gpt-5.4",
			},
		}
		require.Empty(t, resolveOpenAIMessagesDispatchMappedModel(apiKey, "gpt-5.4"))
		require.Equal(t, "gpt-5.3-codex", resolveOpenAIMessagesDispatchMappedModel(apiKey, "claude-sonnet-4-5-20250929"))
	})
}

func TestOpenAIModelMappedBody(t *testing.T) {
	body := []byte(`{"model":"alias","input":"hello"}`)
	calls := 0

	forwardBody := openAIModelMappedBody(body, true, "gpt-5.4", func(body []byte, newModel string) []byte {
		calls++
		return service.ReplaceModelInBody(body, newModel)
	})

	require.Equal(t, 1, calls)
	require.Equal(t, "gpt-5.4", gjson.GetBytes(forwardBody, "model").String())
	require.Equal(t, "alias", gjson.GetBytes(body, "model").String())
}

func TestOpenAIModelMappedBodyCache(t *testing.T) {
	body := []byte(`{"model":"alias","input":"hello"}`)
	calls := 0
	mappedBody := newOpenAIModelMappedBodyCache(body, func(body []byte, newModel string) []byte {
		calls++
		return service.ReplaceModelInBody(body, newModel)
	})

	first := mappedBody(true, "gpt-5.4")
	second := mappedBody(true, "gpt-5.4")
	third := mappedBody(true, "gpt-5.3-codex")
	unmapped := mappedBody(false, "ignored")

	require.Equal(t, 2, calls)
	require.Equal(t, "gpt-5.4", gjson.GetBytes(first, "model").String())
	require.Equal(t, "gpt-5.4", gjson.GetBytes(second, "model").String())
	require.Equal(t, "gpt-5.3-codex", gjson.GetBytes(third, "model").String())
	require.Equal(t, body, unmapped)
	require.Same(t, &first[0], &second[0])
}

func TestOpenAIResponses_MissingDependencies_ReturnsServiceUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","stream":false}`))
	c.Request.Header.Set("Content-Type", "application/json")

	groupID := int64(2)
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
		ID:      10,
		GroupID: &groupID,
	})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{
		UserID:      1,
		Concurrency: 1,
	})

	// 故意使用未初始化依赖，验证快速失败而不是崩溃。
	h := &OpenAIGatewayHandler{}
	require.NotPanics(t, func() {
		h.Responses(c)
	})

	require.Equal(t, http.StatusServiceUnavailable, w.Code)

	var parsed map[string]any
	err := json.Unmarshal(w.Body.Bytes(), &parsed)
	require.NoError(t, err)

	errorObj, ok := parsed["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "api_error", errorObj["type"])
	assert.Equal(t, "Service temporarily unavailable", errorObj["message"])
}

func TestOpenAIResponses_SetsClientTransportHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(`{"model":"gpt-5"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h := &OpenAIGatewayHandler{}
	h.Responses(c)

	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Equal(t, service.OpenAIClientTransportHTTP, service.GetOpenAIClientTransport(c))
}

func TestOpenAIResponses_RejectsMessageIDAsPreviousResponseID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(
		`{"model":"gpt-5.1","stream":false,"previous_response_id":"msg_123456","input":[{"type":"input_text","text":"hello"}]}`,
	))
	c.Request.Header.Set("Content-Type", "application/json")

	groupID := int64(2)
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
		ID:      101,
		GroupID: &groupID,
		User:    &service.User{ID: 1},
	})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{
		UserID:      1,
		Concurrency: 1,
	})

	h := newOpenAIHandlerForPreviousResponseIDValidation(t, nil)
	h.Responses(c)

	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "previous_response_id must be a response.id")
}

func TestOpenAIResponses_RejectsHTTPContinuationPreviousResponseID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(
		`{"model":"gpt-5.1","stream":false,"previous_response_id":"resp_123456","input":[{"type":"input_text","text":"hello"}]}`,
	))
	c.Request.Header.Set("Content-Type", "application/json")

	groupID := int64(2)
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
		ID:      101,
		GroupID: &groupID,
		User:    &service.User{ID: 1},
	})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{
		UserID:      1,
		Concurrency: 1,
	})

	h := newOpenAIHandlerForPreviousResponseIDValidation(t, nil)
	h.Responses(c)

	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "previous_response_id is not supported for HTTP responses requests")
}

func TestOpenAIResponses_FunctionCallOutputHTTPGuidanceDoesNotSuggestPreviousResponseReuse(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(
		`{"model":"gpt-5.1","stream":false,"input":[{"type":"function_call_output","output":"{}"}]}`,
	))
	c.Request.Header.Set("Content-Type", "application/json")

	groupID := int64(2)
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
		ID:      101,
		GroupID: &groupID,
		User:    &service.User{ID: 1},
	})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{
		UserID:      1,
		Concurrency: 1,
	})

	h := newOpenAIHandlerForPreviousResponseIDValidation(t, nil)
	h.Responses(c)

	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "Responses WebSocket v2")
	require.NotContains(t, w.Body.String(), "reuse previous_response_id")
}

func TestOpenAIResponsesWebSocket_SetsClientTransportWSWhenUpgradeValid(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/openai/v1/responses", nil)
	c.Request.Header.Set("Upgrade", "websocket")
	c.Request.Header.Set("Connection", "Upgrade")

	h := &OpenAIGatewayHandler{}
	h.ResponsesWebSocket(c)

	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Equal(t, service.OpenAIClientTransportWS, service.GetOpenAIClientTransport(c))
}

func TestOpenAIResponsesWebSocket_InvalidUpgradeDoesNotSetTransport(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/openai/v1/responses", nil)

	h := &OpenAIGatewayHandler{}
	h.ResponsesWebSocket(c)

	require.Equal(t, http.StatusUpgradeRequired, w.Code)
	require.Equal(t, service.OpenAIClientTransportUnknown, service.GetOpenAIClientTransport(c))
}

func TestOpenAIResponsesWebSocket_RejectsMessageIDAsPreviousResponseID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := newOpenAIHandlerForPreviousResponseIDValidation(t, nil)
	wsServer := newOpenAIWSHandlerTestServer(t, h, middleware.AuthSubject{UserID: 1, Concurrency: 1})
	defer wsServer.Close()

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
	clientConn, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(wsServer.URL, "http")+"/openai/v1/responses", nil)
	cancelDial()
	require.NoError(t, err)
	defer func() {
		_ = clientConn.CloseNow()
	}()

	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 3*time.Second)
	err = clientConn.Write(writeCtx, coderws.MessageText, []byte(
		`{"type":"response.create","model":"gpt-5.1","stream":false,"previous_response_id":"msg_abc123"}`,
	))
	cancelWrite()
	require.NoError(t, err)

	readCtx, cancelRead := context.WithTimeout(context.Background(), 3*time.Second)
	_, _, err = clientConn.Read(readCtx)
	cancelRead()
	require.Error(t, err)
	var closeErr coderws.CloseError
	require.ErrorAs(t, err, &closeErr)
	require.Equal(t, coderws.StatusPolicyViolation, closeErr.Code)
	require.Contains(t, strings.ToLower(closeErr.Reason), "previous_response_id")
}

func TestOpenAIResponsesWebSocket_PreviousResponseIDKindLoggedBeforeAcquireFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cache := &concurrencyCacheMock{
		acquireUserSlotFn: func(ctx context.Context, userID int64, maxConcurrency int, requestID string) (bool, error) {
			return false, errors.New("user slot unavailable")
		},
	}
	h := newOpenAIHandlerForPreviousResponseIDValidation(t, cache)
	wsServer := newOpenAIWSHandlerTestServer(t, h, middleware.AuthSubject{UserID: 1, Concurrency: 1})
	defer wsServer.Close()

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
	clientConn, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(wsServer.URL, "http")+"/openai/v1/responses", nil)
	cancelDial()
	require.NoError(t, err)
	defer func() {
		_ = clientConn.CloseNow()
	}()

	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 3*time.Second)
	err = clientConn.Write(writeCtx, coderws.MessageText, []byte(
		`{"type":"response.create","model":"gpt-5.1","stream":false,"previous_response_id":"resp_prev_123"}`,
	))
	cancelWrite()
	require.NoError(t, err)

	readCtx, cancelRead := context.WithTimeout(context.Background(), 3*time.Second)
	_, _, err = clientConn.Read(readCtx)
	cancelRead()
	require.Error(t, err)
	var closeErr coderws.CloseError
	require.ErrorAs(t, err, &closeErr)
	require.Equal(t, coderws.StatusInternalError, closeErr.Code)
	require.Contains(t, strings.ToLower(closeErr.Reason), "failed to acquire user concurrency slot")
}

type contentModerationHandlerSettingRepo struct {
	values map[string]string
}

func (r *contentModerationHandlerSettingRepo) Get(ctx context.Context, key string) (*service.Setting, error) {
	if value, ok := r.values[key]; ok {
		return &service.Setting{Key: key, Value: value}, nil
	}
	return nil, service.ErrSettingNotFound
}

func (r *contentModerationHandlerSettingRepo) GetValue(ctx context.Context, key string) (string, error) {
	if value, ok := r.values[key]; ok {
		return value, nil
	}
	return "", service.ErrSettingNotFound
}

func (r *contentModerationHandlerSettingRepo) Set(ctx context.Context, key, value string) error {
	if r.values == nil {
		r.values = map[string]string{}
	}
	r.values[key] = value
	return nil
}

func (r *contentModerationHandlerSettingRepo) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	out := map[string]string{}
	for _, key := range keys {
		if value, ok := r.values[key]; ok {
			out[key] = value
		}
	}
	return out, nil
}

func (r *contentModerationHandlerSettingRepo) SetMultiple(ctx context.Context, settings map[string]string) error {
	if r.values == nil {
		r.values = map[string]string{}
	}
	for key, value := range settings {
		r.values[key] = value
	}
	return nil
}

func (r *contentModerationHandlerSettingRepo) GetAll(ctx context.Context) (map[string]string, error) {
	out := make(map[string]string, len(r.values))
	for key, value := range r.values {
		out[key] = value
	}
	return out, nil
}

func (r *contentModerationHandlerSettingRepo) Delete(ctx context.Context, key string) error {
	delete(r.values, key)
	return nil
}

type contentModerationHandlerTestRepo struct {
	mu   sync.Mutex
	logs []service.ContentModerationLog
}

func (r *contentModerationHandlerTestRepo) CreateLog(ctx context.Context, log *service.ContentModerationLog) error {
	if log != nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.logs = append(r.logs, *log)
	}
	return nil
}

func (r *contentModerationHandlerTestRepo) resetLogs() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = nil
}

func (r *contentModerationHandlerTestRepo) logSnapshot() []service.ContentModerationLog {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]service.ContentModerationLog(nil), r.logs...)
}

func (r *contentModerationHandlerTestRepo) ListLogs(ctx context.Context, filter service.ContentModerationLogFilter) ([]service.ContentModerationLog, *pagination.PaginationResult, error) {
	return nil, nil, nil
}

func (r *contentModerationHandlerTestRepo) CountFlaggedByUserSince(ctx context.Context, userID int64, since time.Time) (int, error) {
	return 0, nil
}

func (r *contentModerationHandlerTestRepo) CleanupExpiredLogs(ctx context.Context, hitBefore time.Time, nonHitBefore time.Time) (*service.ContentModerationCleanupResult, error) {
	return &service.ContentModerationCleanupResult{}, nil
}

func TestOpenAIResponsesWebSocket_ContentModerationBlocksFirstFrame(t *testing.T) {
	gin.SetMode(gin.TestMode)

	moderationServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/moderations", r.URL.Path)
		_, _ = w.Write([]byte(`{"results":[{"category_scores":{"sexual":0.9}}]}`))
	}))
	defer moderationServer.Close()

	cfg := &service.ContentModerationConfig{
		Enabled:      true,
		Mode:         service.ContentModerationModePreBlock,
		BaseURL:      moderationServer.URL,
		Model:        "omni-moderation-latest",
		APIKeys:      []string{"sk-test"},
		SampleRate:   100,
		AllGroups:    true,
		BlockMessage: "内容审计测试阻断",
	}
	rawCfg, err := json.Marshal(cfg)
	require.NoError(t, err)

	repo := &contentModerationHandlerTestRepo{}
	settingRepo := &contentModerationHandlerSettingRepo{values: map[string]string{
		service.SettingKeyRiskControlEnabled:      "true",
		service.SettingKeyContentModerationConfig: string(rawCfg),
	}}
	moderationSvc := service.NewContentModerationService(
		settingRepo,
		repo,
		nil,
		nil,
		nil,
		nil,
		nil,
	)
	decision, err := moderationSvc.Check(context.Background(), service.ContentModerationCheckInput{
		UserID:   1,
		Endpoint: "/v1/responses",
		Provider: "openai",
		Model:    "gpt-5.5",
		Protocol: service.ContentModerationProtocolOpenAIResponses,
		Body:     []byte(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"bad prompt"}]}]}`),
	})
	require.NoError(t, err)
	require.True(t, decision.Blocked)
	require.Eventually(t, func() bool {
		return len(repo.logSnapshot()) == 1
	}, time.Second, 10*time.Millisecond)
	repo.resetLogs()
	h := &OpenAIGatewayHandler{
		gatewayService:           &service.OpenAIGatewayService{},
		billingCacheService:      &service.BillingCacheService{},
		apiKeyService:            &service.APIKeyService{},
		contentModerationService: moderationSvc,
		concurrencyHelper:        NewConcurrencyHelper(service.NewConcurrencyService(&concurrencyCacheMock{}), SSEPingFormatNone, time.Second),
	}
	wsServer := newOpenAIWSHandlerTestServer(t, h, middleware.AuthSubject{UserID: 1, Concurrency: 1})
	defer wsServer.Close()

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
	clientConn, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(wsServer.URL, "http")+"/openai/v1/responses", nil)
	cancelDial()
	require.NoError(t, err)
	defer func() {
		_ = clientConn.CloseNow()
	}()

	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 3*time.Second)
	err = clientConn.Write(writeCtx, coderws.MessageText, []byte(`{
		"type":"response.create",
		"model":"gpt-5.5",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"bad prompt"}]}]
	}`))
	cancelWrite()
	require.NoError(t, err)

	readCtx, cancelRead := context.WithTimeout(context.Background(), 3*time.Second)
	_, payload, readErr := clientConn.Read(readCtx)
	cancelRead()
	if readErr == nil {
		require.Contains(t, string(payload), "content_policy_violation")
		require.Contains(t, string(payload), "内容审计测试阻断")
	} else {
		var closeErr coderws.CloseError
		require.ErrorAs(t, readErr, &closeErr)
		require.Equal(t, coderws.StatusPolicyViolation, closeErr.Code)
		require.Contains(t, closeErr.Reason, "内容审计测试阻断")
	}
	var logs []service.ContentModerationLog
	require.Eventually(t, func() bool {
		logs = repo.logSnapshot()
		return len(logs) == 1
	}, time.Second, 10*time.Millisecond)
	require.True(t, logs[0].Flagged)
	require.Equal(t, service.ContentModerationActionBlock, logs[0].Action)
	require.Equal(t, "bad prompt", logs[0].InputExcerpt)
}

func TestOpenAIResponsesWebSocket_PassthroughUsageLogPersistsUserAgentAndReasoningEffort(t *testing.T) {
	got := runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
		firstPayload: `{"type":"response.create","model":"gpt-5.4","stream":false,"reasoning":{"effort":"HIGH"}}`,
		userAgent:    testStringPtr("codex_cli_rs/0.125.0 test"),
	})

	require.NotNil(t, got.log.UserAgent)
	require.Equal(t, "codex_cli_rs/0.125.0 test", *got.log.UserAgent)
	require.NotNil(t, got.log.ReasoningEffort)
	require.Equal(t, "high", *got.log.ReasoningEffort)
	require.True(t, got.log.OpenAIWSMode)
}

func TestOpenAIResponsesWebSocket_PassthroughUsageLogInfersReasoningFromInitialRequestModel(t *testing.T) {
	got := runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
		firstPayload: `{"type":"response.create","model":"gpt-5.4-xhigh","stream":false}`,
		userAgent:    testStringPtr("codex_cli_rs/0.125.0 mapped"),
		channelMapping: map[string]string{
			"gpt-5.4-xhigh": "gpt-5.4",
		},
	})

	require.Equal(t, "gpt-5.4", gjson.GetBytes(got.upstreamFirstPayload, "model").String(),
		"上游首帧应使用渠道映射后的模型")
	require.NotNil(t, got.log.ReasoningEffort)
	require.Equal(t, "xhigh", *got.log.ReasoningEffort,
		"usage log reasoning effort 必须使用渠道映射前首帧模型后缀推导")
}

func TestOpenAIResponsesWebSocket_PassthroughUsageLogLeavesUserAgentNilWhenMissing(t *testing.T) {
	got := runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
		firstPayload: `{"type":"response.create","model":"gpt-5.4","stream":false,"reasoning":{"effort":"medium"}}`,
		userAgent:    testStringPtr(""),
	})

	require.Nil(t, got.log.UserAgent, "空入站 User-Agent 不应由上游握手 UA 或默认 UA 兜底")
	require.NotNil(t, got.log.ReasoningEffort)
	require.Equal(t, "medium", *got.log.ReasoningEffort)
}

func TestSetOpenAIClientTransportHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	setOpenAIClientTransportHTTP(c)
	require.Equal(t, service.OpenAIClientTransportHTTP, service.GetOpenAIClientTransport(c))
}

func TestSetOpenAIClientTransportWS(t *testing.T) {
	gin.SetMode(gin.TestMode)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	setOpenAIClientTransportWS(c)
	require.Equal(t, service.OpenAIClientTransportWS, service.GetOpenAIClientTransport(c))
}

// TestOpenAIHandler_GjsonExtraction 验证 gjson 从请求体中提取 model/stream 的正确性
func TestOpenAIHandler_GjsonExtraction(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantModel  string
		wantStream bool
	}{
		{"正常提取", `{"model":"gpt-4","stream":true,"input":"hello"}`, "gpt-4", true},
		{"stream false", `{"model":"gpt-4","stream":false}`, "gpt-4", false},
		{"无 stream 字段", `{"model":"gpt-4"}`, "gpt-4", false},
		{"model 缺失", `{"stream":true}`, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(tt.body)
			modelResult := gjson.GetBytes(body, "model")
			model := ""
			if modelResult.Type == gjson.String {
				model = modelResult.String()
			}
			stream := gjson.GetBytes(body, "stream").Bool()
			require.Equal(t, tt.wantModel, model)
			require.Equal(t, tt.wantStream, stream)
		})
	}
}

// TestOpenAIHandler_GjsonValidation 验证修复后的 JSON 合法性和类型校验
func TestOpenAIHandler_GjsonValidation(t *testing.T) {
	// 非法 JSON 被 gjson.ValidBytes 拦截
	require.False(t, gjson.ValidBytes([]byte(`{invalid json`)))

	// model 为数字 → 类型不是 gjson.String，应被拒绝
	body := []byte(`{"model":123}`)
	modelResult := gjson.GetBytes(body, "model")
	require.True(t, modelResult.Exists())
	require.NotEqual(t, gjson.String, modelResult.Type)

	// model 为 null → 类型不是 gjson.String，应被拒绝
	body2 := []byte(`{"model":null}`)
	modelResult2 := gjson.GetBytes(body2, "model")
	require.True(t, modelResult2.Exists())
	require.NotEqual(t, gjson.String, modelResult2.Type)

	// stream 为 string → 类型既不是 True 也不是 False，应被拒绝
	body3 := []byte(`{"model":"gpt-4","stream":"true"}`)
	streamResult := gjson.GetBytes(body3, "stream")
	require.True(t, streamResult.Exists())
	require.NotEqual(t, gjson.True, streamResult.Type)
	require.NotEqual(t, gjson.False, streamResult.Type)

	// stream 为 int → 同上
	body4 := []byte(`{"model":"gpt-4","stream":1}`)
	streamResult2 := gjson.GetBytes(body4, "stream")
	require.True(t, streamResult2.Exists())
	require.NotEqual(t, gjson.True, streamResult2.Type)
	require.NotEqual(t, gjson.False, streamResult2.Type)
}

// TestOpenAIHandler_InstructionsInjection 验证 instructions 的 gjson/sjson 注入逻辑
func TestOpenAIHandler_InstructionsInjection(t *testing.T) {
	// 测试 1：无 instructions → 注入
	body := []byte(`{"model":"gpt-4"}`)
	existing := gjson.GetBytes(body, "instructions").String()
	require.Empty(t, existing)
	newBody, err := sjson.SetBytes(body, "instructions", "test instruction")
	require.NoError(t, err)
	require.Equal(t, "test instruction", gjson.GetBytes(newBody, "instructions").String())

	// 测试 2：已有 instructions → 不覆盖
	body2 := []byte(`{"model":"gpt-4","instructions":"existing"}`)
	existing2 := gjson.GetBytes(body2, "instructions").String()
	require.Equal(t, "existing", existing2)

	// 测试 3：空白 instructions → 注入
	body3 := []byte(`{"model":"gpt-4","instructions":"   "}`)
	existing3 := strings.TrimSpace(gjson.GetBytes(body3, "instructions").String())
	require.Empty(t, existing3)

	// 测试 4：sjson.SetBytes 返回错误时不应 panic
	// 正常 JSON 不会产生 sjson 错误，验证返回值被正确处理
	validBody := []byte(`{"model":"gpt-4"}`)
	result, setErr := sjson.SetBytes(validBody, "instructions", "hello")
	require.NoError(t, setErr)
	require.True(t, gjson.ValidBytes(result))
}

func newOpenAIHandlerForPreviousResponseIDValidation(t *testing.T, cache *concurrencyCacheMock) *OpenAIGatewayHandler {
	t.Helper()
	if cache == nil {
		cache = &concurrencyCacheMock{
			acquireUserSlotFn: func(ctx context.Context, userID int64, maxConcurrency int, requestID string) (bool, error) {
				return true, nil
			},
			acquireAccountSlotFn: func(ctx context.Context, accountID int64, maxConcurrency int, requestID string) (bool, error) {
				return true, nil
			},
		}
	}
	return &OpenAIGatewayHandler{
		gatewayService:      &service.OpenAIGatewayService{},
		billingCacheService: &service.BillingCacheService{},
		apiKeyService:       &service.APIKeyService{},
		concurrencyHelper:   NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, time.Second),
	}
}

func newOpenAIWSHandlerTestServer(t *testing.T, h *OpenAIGatewayHandler, subject middleware.AuthSubject) *httptest.Server {
	t.Helper()
	groupID := int64(2)
	apiKey := &service.APIKey{
		ID:      101,
		GroupID: &groupID,
		User:    &service.User{ID: subject.UserID},
	}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), apiKey)
		c.Set(string(middleware.ContextKeyUser), subject)
		c.Next()
	})
	router.GET("/openai/v1/responses", h.ResponsesWebSocket)
	return httptest.NewServer(router)
}

type openAIResponsesWSUsageLogCase struct {
	firstPayload   string
	userAgent      *string
	channelMapping map[string]string
}

type openAIResponsesWSUsageLogResult struct {
	log                  *service.UsageLog
	upstreamFirstPayload []byte
}

type openAIWSUsageHandlerAccountRepoStub struct {
	service.AccountRepository
	account service.Account
}

func (s *openAIWSUsageHandlerAccountRepoStub) ListSchedulableByPlatform(ctx context.Context, platform string) ([]service.Account, error) {
	if s.account.Platform != platform {
		return nil, nil
	}
	return []service.Account{s.account}, nil
}

func (s *openAIWSUsageHandlerAccountRepoStub) ListSchedulableByGroupIDAndPlatform(ctx context.Context, groupID int64, platform string) ([]service.Account, error) {
	return s.ListSchedulableByPlatform(ctx, platform)
}

func (s *openAIWSUsageHandlerAccountRepoStub) GetByID(ctx context.Context, id int64) (*service.Account, error) {
	if s.account.ID != id {
		return nil, nil
	}
	account := s.account
	return &account, nil
}

type openAIWSFailoverHandlerAccountRepoStub struct {
	service.AccountRepository
	accounts       []service.Account
	rateLimitedIDs []int64
}

func (s *openAIWSFailoverHandlerAccountRepoStub) ListSchedulableByPlatform(ctx context.Context, platform string) ([]service.Account, error) {
	out := make([]service.Account, 0, len(s.accounts))
	for _, account := range s.accounts {
		if account.Platform == platform && account.IsSchedulable() {
			out = append(out, account)
		}
	}
	return out, nil
}

func (s *openAIWSFailoverHandlerAccountRepoStub) ListSchedulableByGroupIDAndPlatform(ctx context.Context, groupID int64, platform string) ([]service.Account, error) {
	return s.ListSchedulableByPlatform(ctx, platform)
}

func (s *openAIWSFailoverHandlerAccountRepoStub) ListSchedulableUngroupedByPlatform(ctx context.Context, platform string) ([]service.Account, error) {
	return s.ListSchedulableByPlatform(ctx, platform)
}

func (s *openAIWSFailoverHandlerAccountRepoStub) GetByID(ctx context.Context, id int64) (*service.Account, error) {
	for _, account := range s.accounts {
		if account.ID == id {
			acc := account
			return &acc, nil
		}
	}
	return nil, nil
}

func (s *openAIWSFailoverHandlerAccountRepoStub) SetRateLimited(ctx context.Context, id int64, resetAt time.Time) error {
	s.rateLimitedIDs = append(s.rateLimitedIDs, id)
	for i := range s.accounts {
		if s.accounts[i].ID == id {
			reset := resetAt
			s.accounts[i].RateLimitResetAt = &reset
			break
		}
	}
	return nil
}

type openAIWSUsageHandlerUsageLogRepoStub struct {
	service.UsageLogRepository
	created chan *service.UsageLog
}

func (s *openAIWSUsageHandlerUsageLogRepoStub) Create(ctx context.Context, log *service.UsageLog) (bool, error) {
	if s.created != nil {
		s.created <- log
	}
	return true, nil
}

type openAIWSUsageHandlerChannelRepoStub struct {
	service.ChannelRepository
	channels       []service.Channel
	groupPlatforms map[int64]string
}

func (s *openAIWSUsageHandlerChannelRepoStub) ListAll(ctx context.Context) ([]service.Channel, error) {
	return s.channels, nil
}

func (s *openAIWSUsageHandlerChannelRepoStub) GetGroupPlatforms(ctx context.Context, groupIDs []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(groupIDs))
	for _, groupID := range groupIDs {
		if platform := strings.TrimSpace(s.groupPlatforms[groupID]); platform != "" {
			out[groupID] = platform
		}
	}
	return out, nil
}

type openAIMessagesUsageHTTPUpstream struct {
	service.HTTPUpstream
	body string
}

func (u openAIMessagesUsageHTTPUpstream) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			"X-Request-Id": []string{"req_messages_policy_usage"},
		},
		Body: io.NopCloser(strings.NewReader(u.body)),
	}, nil
}

func TestOpenAIMessages_PreOutputPolicyFailureRecordsUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(4242)
	accountRepo := &openAIWSFailoverHandlerAccountRepoStub{accounts: []service.Account{
		{
			ID:          9904,
			Name:        "openai-messages-policy",
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeAPIKey,
			Status:      service.StatusActive,
			Schedulable: true,
			Concurrency: 0,
			Credentials: map[string]any{"api_key": "sk-test"},
		},
	}}
	usageRepo := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 1)}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCacheSvc.Stop)
	concurrencySvc := service.NewConcurrencyService(nil)
	upstreamBody := strings.Join([]string{
		`data: {"type":"response.failed","response":{"id":"resp_policy_usage","object":"response","model":"gpt-5.4","status":"failed","error":{"type":"invalid_request_error","code":"content_policy_violation","message":"request violates safety policy"},"output":[],"usage":{"input_tokens":5,"output_tokens":0,"total_tokens":5}}}`,
		"",
	}, "\n")
	gatewaySvc := service.NewOpenAIGatewayService(
		accountRepo,
		usageRepo,
		nil,
		nil,
		nil,
		nil,
		nil,
		cfg,
		nil,
		concurrencySvc,
		service.NewBillingService(cfg, nil),
		nil,
		billingCacheSvc,
		openAIMessagesUsageHTTPUpstream{body: upstreamBody},
		&service.DeferredService{},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	)
	h := NewOpenAIGatewayHandler(gatewaySvc, concurrencySvc, billingCacheSvc, &service.APIKeyService{}, nil, nil, nil, cfg)

	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
		ID:      1803,
		GroupID: &groupID,
		User:    &service.User{ID: 1703, Status: service.StatusActive},
		Group: &service.Group{
			ID:                    groupID,
			Platform:              service.PlatformOpenAI,
			Status:                service.StatusActive,
			AllowMessagesDispatch: true,
		},
	})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1703, Concurrency: 0})

	h.Messages(c)

	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Contains(t, rec.Body.String(), "request violates safety policy")
	select {
	case log := <-usageRepo.created:
		require.Equal(t, int64(1703), log.UserID)
		require.Equal(t, int64(9904), log.AccountID)
		require.Equal(t, 5, log.InputTokens)
		require.Equal(t, 0, log.OutputTokens)
		require.True(t, log.Stream)
	case <-time.After(time.Second):
		t.Fatal("expected usage log for pre-output policy failure")
	}
}

func TestOpenAIResponses_PostOutputResponseFailedRecordsUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(4243)
	account := service.Account{
		ID:          9905,
		Name:        "openai-responses-failed-usage",
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "http://upstream.example",
		},
		Extra: map[string]any{
			openai_compat.ExtraKeyResponsesMode:      string(openai_compat.ResponsesSupportModeAuto),
			openai_compat.ExtraKeyResponsesSupported: true,
		},
	}
	accountRepo := &openAIWSUsageHandlerAccountRepoStub{account: account}
	usageRepo := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 1)}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Default.RateMultiplier = 1
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.MaxLineSize = 1024 * 1024
	cfg.Gateway.StreamKeepaliveInterval = 3600
	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCacheSvc.Stop)
	concurrencySvc := service.NewConcurrencyService(&concurrencyCacheMock{
		acquireUserSlotFn: func(ctx context.Context, userID int64, maxConcurrency int, requestID string) (bool, error) {
			return true, nil
		},
		acquireAccountSlotFn: func(ctx context.Context, accountID int64, maxConcurrency int, requestID string) (bool, error) {
			return true, nil
		},
	})
	upstreamBody := strings.Join([]string{
		"event: response.created",
		`data: {"type":"response.created","response":{"id":"resp_failed_usage"}}`,
		"",
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","delta":"partial"}`,
		"",
		"event: response.failed",
		`data: {"type":"response.failed","response":{"id":"resp_failed_usage","status":"failed","instructions":"sensitive instructions","output":[{"type":"message","content":[{"type":"output_text","text":"large"}]}],"usage":{"input_tokens":7,"output_tokens":0,"total_tokens":7,"input_tokens_details":{"cached_tokens":3}},"tools":[{"type":"function","name":"secret_tool"}],"metadata":{"tenant":"secret"},"error":{"code":"content_policy_violation","message":"request violates safety policy"}}}`,
		"",
		": queued after failed",
		": queued after failed",
		": queued after failed",
		": queued after failed",
		": queued after failed",
		": queued after failed",
		": queued after failed",
		": queued after failed",
		": queued after failed",
		": queued after failed",
		": queued after failed",
		": queued after failed",
		": queued after failed",
		": queued after failed",
		": queued after failed",
		": queued after failed",
	}, "\n")
	gatewaySvc := service.NewOpenAIGatewayService(
		accountRepo,
		usageRepo,
		nil,
		nil,
		nil,
		nil,
		nil,
		cfg,
		nil,
		concurrencySvc,
		service.NewBillingService(cfg, nil),
		nil,
		billingCacheSvc,
		openAIMessagesUsageHTTPUpstream{body: upstreamBody},
		&service.DeferredService{},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	)
	h := NewOpenAIGatewayHandler(gatewaySvc, concurrencySvc, billingCacheSvc, &service.APIKeyService{}, nil, nil, nil, cfg)

	body := []byte(`{"model":"gpt-5.4","input":"hello","stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
		ID:      1804,
		GroupID: &groupID,
		User:    &service.User{ID: 1704, Status: service.StatusActive},
		Group: &service.Group{
			ID:       groupID,
			Platform: service.PlatformOpenAI,
			Status:   service.StatusActive,
		},
	})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1704, Concurrency: 1})

	h.Responses(c)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "event: response.failed")
	require.Contains(t, rec.Body.String(), "\n\n: queued after failed")
	require.Contains(t, rec.Body.String(), "request violates safety policy")
	require.NotContains(t, rec.Body.String(), "sensitive instructions")
	require.NotContains(t, rec.Body.String(), "secret_tool")
	require.NotContains(t, rec.Body.String(), `"usage"`)
	select {
	case log := <-usageRepo.created:
		require.Equal(t, int64(1704), log.UserID)
		require.Equal(t, int64(9905), log.AccountID)
		require.Equal(t, 4, log.InputTokens)
		require.Equal(t, 0, log.OutputTokens)
		require.Equal(t, 3, log.CacheReadTokens)
		require.True(t, log.Stream)
	case <-time.After(time.Second):
		t.Fatal("expected usage log for post-output response.failed")
	}
	select {
	case log := <-usageRepo.created:
		t.Fatalf("unexpected duplicate usage log for post-output response.failed: %+v", log)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestOpenAIResponses_PreOutputResponseFailedDoesNotRecordUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(4244)
	account := service.Account{
		ID:          9906,
		Name:        "openai-responses-pre-output-failed-usage",
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "http://upstream.example",
		},
		Extra: map[string]any{
			openai_compat.ExtraKeyResponsesMode:      string(openai_compat.ResponsesSupportModeAuto),
			openai_compat.ExtraKeyResponsesSupported: true,
		},
	}
	accountRepo := &openAIWSUsageHandlerAccountRepoStub{account: account}
	usageRepo := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 1)}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Default.RateMultiplier = 1
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.MaxLineSize = 1024 * 1024
	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCacheSvc.Stop)
	concurrencySvc := service.NewConcurrencyService(&concurrencyCacheMock{
		acquireUserSlotFn: func(ctx context.Context, userID int64, maxConcurrency int, requestID string) (bool, error) {
			return true, nil
		},
		acquireAccountSlotFn: func(ctx context.Context, accountID int64, maxConcurrency int, requestID string) (bool, error) {
			return true, nil
		},
	})
	upstreamBody := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_failed_pre_usage"}}`,
		"",
		"event: response.output_text.delta",
		"event: response.failed",
		`data: {"type":"response.failed","response":{"id":"resp_failed_pre_usage","status":"failed","instructions":"sensitive instructions","output":[{"type":"message","content":[{"type":"output_text","text":"large"}]}],"usage":{"input_tokens":7,"output_tokens":0,"total_tokens":7,"input_tokens_details":{"cached_tokens":3}},"tools":[{"type":"function","name":"secret_tool"}],"metadata":{"tenant":"secret"},"error":{"type":"invalid_request_error","code":"content_policy_violation","message":"request violates safety policy"}}}`,
		"",
	}, "\n")
	gatewaySvc := service.NewOpenAIGatewayService(
		accountRepo,
		usageRepo,
		nil,
		nil,
		nil,
		nil,
		nil,
		cfg,
		nil,
		concurrencySvc,
		service.NewBillingService(cfg, nil),
		nil,
		billingCacheSvc,
		openAIMessagesUsageHTTPUpstream{body: upstreamBody},
		&service.DeferredService{},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	)
	h := NewOpenAIGatewayHandler(gatewaySvc, concurrencySvc, billingCacheSvc, &service.APIKeyService{}, nil, nil, nil, cfg)

	body := []byte(`{"model":"gpt-5.4","input":"hello","stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
		ID:      1805,
		GroupID: &groupID,
		User:    &service.User{ID: 1705, Status: service.StatusActive},
		Group: &service.Group{
			ID:       groupID,
			Platform: service.PlatformOpenAI,
			Status:   service.StatusActive,
		},
	})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1705, Concurrency: 1})

	h.Responses(c)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "event: response.failed")
	require.Contains(t, rec.Body.String(), "request violates safety policy")
	require.NotContains(t, rec.Body.String(), "sensitive instructions")
	require.NotContains(t, rec.Body.String(), "secret_tool")
	require.NotContains(t, rec.Body.String(), `"usage"`)
	select {
	case log := <-usageRepo.created:
		t.Fatalf("unexpected usage log for pre-output response.failed: %+v", log)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestOpenAIResponsesWebSocket_FailoverOnUpstreamUsageLimitEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	firstHitCh := make(chan []byte, 1)
	secondHitCh := make(chan []byte, 1)

	firstUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, &coderws.AcceptOptions{CompressionMode: coderws.CompressionContextTakeover})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()

		readCtx, cancelRead := context.WithTimeout(r.Context(), 3*time.Second)
		_, payload, readErr := conn.Read(readCtx)
		cancelRead()
		if readErr == nil {
			firstHitCh <- payload
		}

		writeCtx, cancelWrite := context.WithTimeout(r.Context(), 3*time.Second)
		_ = conn.Write(writeCtx, coderws.MessageText, []byte(`{"type":"error","error":{"code":"rate_limit_exceeded","type":"usage_limit_reached","message":"The usage limit has been reached"}}`))
		cancelWrite()
	}))
	defer firstUpstream.Close()

	secondUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, &coderws.AcceptOptions{CompressionMode: coderws.CompressionContextTakeover})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()

		readCtx, cancelRead := context.WithTimeout(r.Context(), 3*time.Second)
		_, payload, readErr := conn.Read(readCtx)
		cancelRead()
		if readErr == nil {
			secondHitCh <- payload
		}

		writeCtx, cancelWrite := context.WithTimeout(r.Context(), 3*time.Second)
		_ = conn.Write(writeCtx, coderws.MessageText, []byte(`{"type":"response.completed","response":{"id":"resp_ws_failover_ok","model":"gpt-5.1","usage":{"input_tokens":1,"output_tokens":1}}}`))
		cancelWrite()
		_ = conn.Close(coderws.StatusNormalClosure, "done")
	}))
	defer secondUpstream.Close()

	groupID := int64(4202)
	accounts := []service.Account{
		{
			ID:          9902,
			Name:        "openai-ws-rate-limited",
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeAPIKey,
			Status:      service.StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    1,
			Credentials: map[string]any{
				"api_key":  "sk-first",
				"base_url": firstUpstream.URL,
			},
			Extra: map[string]any{
				"openai_apikey_responses_websockets_v2_enabled": true,
				"openai_apikey_responses_websockets_v2_mode":    service.OpenAIWSIngressModePassthrough,
			},
		},
		{
			ID:          9903,
			Name:        "openai-ws-healthy",
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeAPIKey,
			Status:      service.StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    2,
			Credentials: map[string]any{
				"api_key":  "sk-second",
				"base_url": secondUpstream.URL,
			},
			Extra: map[string]any{
				"openai_apikey_responses_websockets_v2_enabled": true,
				"openai_apikey_responses_websockets_v2_mode":    service.OpenAIWSIngressModePassthrough,
			},
		},
	}

	cfg := &config.Config{}
	cfg.RunMode = config.RunModeSimple
	cfg.Default.RateMultiplier = 1
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
	cfg.Gateway.MaxAccountSwitches = 3

	accountRepo := &openAIWSFailoverHandlerAccountRepoStub{accounts: accounts}
	rateLimitSvc := service.NewRateLimitService(accountRepo, nil, cfg, nil, nil)
	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	var keyAcquires atomic.Int32
	var userAcquires atomic.Int32
	cache := &concurrencyCacheMock{
		acquireAPIKeySlotFn: func(ctx context.Context, apiKeyID int64, maxConcurrency int, requestID string) (bool, error) {
			keyAcquires.Add(1)
			return true, nil
		},
		acquireUserSlotFn: func(ctx context.Context, userID int64, maxConcurrency int, requestID string) (bool, error) {
			userAcquires.Add(1)
			return true, nil
		},
		acquireAccountSlotFn: func(ctx context.Context, accountID int64, maxConcurrency int, requestID string) (bool, error) {
			return true, nil
		},
	}
	concurrencySvc := service.NewConcurrencyService(cache)
	gatewaySvc := service.NewOpenAIGatewayService(
		accountRepo,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		cfg,
		nil,
		concurrencySvc,
		service.NewBillingService(cfg, nil),
		rateLimitSvc,
		billingCacheSvc,
		nil,
		&service.DeferredService{},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	)

	h := &OpenAIGatewayHandler{
		gatewayService:      gatewaySvc,
		billingCacheService: billingCacheSvc,
		apiKeyService:       &service.APIKeyService{},
		concurrencyHelper:   NewConcurrencyHelper(concurrencySvc, SSEPingFormatNone, time.Second),
		maxAccountSwitches:  3,
	}

	apiKey := &service.APIKey{
		ID:          1802,
		GroupID:     &groupID,
		Concurrency: 1,
		User:        &service.User{ID: 1702, Status: service.StatusActive},
		Group:       &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive},
	}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), apiKey)
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.User.ID, Concurrency: 1})
		c.Next()
	})
	router.GET("/openai/v1/responses", h.ResponsesWebSocket)
	handlerServer := httptest.NewServer(router)
	defer handlerServer.Close()

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
	clientConn, _, err := coderws.Dial(
		dialCtx,
		"ws"+strings.TrimPrefix(handlerServer.URL, "http")+"/openai/v1/responses",
		&coderws.DialOptions{CompressionMode: coderws.CompressionContextTakeover},
	)
	cancelDial()
	require.NoError(t, err)
	defer func() { _ = clientConn.CloseNow() }()

	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 3*time.Second)
	err = clientConn.Write(writeCtx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","stream":false}`))
	cancelWrite()
	require.NoError(t, err)

	readCtx, cancelRead := context.WithTimeout(context.Background(), 5*time.Second)
	_, event, err := clientConn.Read(readCtx)
	cancelRead()
	require.NoError(t, err)
	require.Equal(t, "response.completed", gjson.GetBytes(event, "type").String())
	require.Equal(t, "resp_ws_failover_ok", gjson.GetBytes(event, "response.id").String())

	select {
	case <-firstHitCh:
	case <-time.After(3 * time.Second):
		t.Fatal("等待第一个上游收到首帧超时")
	}
	select {
	case <-secondHitCh:
	case <-time.After(3 * time.Second):
		t.Fatal("等待第二个上游收到重放首帧超时")
	}
	require.Equal(t, []int64{int64(9902)}, accountRepo.rateLimitedIDs)
	require.Equal(t, int32(1), keyAcquires.Load(), "failover must retain the logical turn's key slot")
	require.Equal(t, int32(1), userAcquires.Load(), "failover must retain the logical turn's user slot")
	require.Equal(t, int32(1), atomic.LoadInt32(&cache.releaseAPIKeyCalled))
	require.Equal(t, int32(1), atomic.LoadInt32(&cache.releaseUserCalled))
	require.Equal(t, int32(2), atomic.LoadInt32(&cache.releaseAccountCalled), "each scheduler-acquired account attempt releases once")
}

func runOpenAIResponsesWebSocketUsageLogCase(t *testing.T, tc openAIResponsesWSUsageLogCase) openAIResponsesWSUsageLogResult {
	t.Helper()
	gin.SetMode(gin.TestMode)

	upstreamPayloadCh := make(chan []byte, 1)
	upstreamErrCh := make(chan error, 1)
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, &coderws.AcceptOptions{
			CompressionMode: coderws.CompressionContextTakeover,
		})
		if err != nil {
			upstreamErrCh <- err
			return
		}
		defer func() {
			_ = conn.CloseNow()
		}()

		readCtx, cancelRead := context.WithTimeout(r.Context(), 3*time.Second)
		msgType, payload, readErr := conn.Read(readCtx)
		cancelRead()
		if readErr != nil {
			upstreamErrCh <- readErr
			return
		}
		if msgType != coderws.MessageText && msgType != coderws.MessageBinary {
			upstreamErrCh <- errors.New("unexpected upstream websocket message type")
			return
		}
		upstreamPayloadCh <- payload

		writeCtx, cancelWrite := context.WithTimeout(r.Context(), 3*time.Second)
		writeErr := conn.Write(writeCtx, coderws.MessageText, []byte(
			`{"type":"response.completed","response":{"id":"resp_usage_e2e","model":"gpt-5.4","usage":{"input_tokens":2,"output_tokens":1}}}`,
		))
		cancelWrite()
		if writeErr != nil {
			upstreamErrCh <- writeErr
			return
		}
		_ = conn.Close(coderws.StatusNormalClosure, "done")
		upstreamErrCh <- nil
	}))
	defer upstreamServer.Close()

	groupID := int64(4201)
	account := service.Account{
		ID:          9901,
		Name:        "openai-ws-passthrough-usage-e2e",
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": upstreamServer.URL,
		},
		Extra: map[string]any{
			"openai_apikey_responses_websockets_v2_enabled": true,
			"openai_apikey_responses_websockets_v2_mode":    service.OpenAIWSIngressModePassthrough,
		},
	}

	cfg := &config.Config{}
	cfg.RunMode = config.RunModeSimple
	cfg.Default.RateMultiplier = 1
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3

	accountRepo := &openAIWSUsageHandlerAccountRepoStub{account: account}
	usageRepo := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 1)}

	var channelSvc *service.ChannelService
	if len(tc.channelMapping) > 0 {
		channelSvc = service.NewChannelService(&openAIWSUsageHandlerChannelRepoStub{
			channels: []service.Channel{{
				ID:           7701,
				Name:         "openai-ws-e2e-channel",
				Status:       service.StatusActive,
				GroupIDs:     []int64{groupID},
				ModelMapping: map[string]map[string]string{service.PlatformOpenAI: tc.channelMapping},
			}},
			groupPlatforms: map[int64]string{groupID: service.PlatformOpenAI},
		}, nil, nil, nil)
	}

	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	gatewaySvc := service.NewOpenAIGatewayService(
		accountRepo,
		usageRepo,
		nil,
		nil,
		nil,
		nil,
		nil,
		cfg,
		nil,
		nil,
		service.NewBillingService(cfg, nil),
		nil,
		billingCacheSvc,
		nil,
		&service.DeferredService{},
		nil,
		nil,
		channelSvc,
		nil,
		nil,
		nil, // userPlatformQuotaRepo
	)

	cache := &concurrencyCacheMock{
		acquireAPIKeySlotFn: func(ctx context.Context, apiKeyID int64, maxConcurrency int, requestID string) (bool, error) {
			return true, nil
		},
		acquireUserSlotFn: func(ctx context.Context, userID int64, maxConcurrency int, requestID string) (bool, error) {
			return true, nil
		},
		acquireAccountSlotFn: func(ctx context.Context, accountID int64, maxConcurrency int, requestID string) (bool, error) {
			return true, nil
		},
	}
	h := &OpenAIGatewayHandler{
		gatewayService:      gatewaySvc,
		billingCacheService: billingCacheSvc,
		apiKeyService:       &service.APIKeyService{},
		concurrencyHelper:   NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, time.Second),
	}

	apiKey := &service.APIKey{
		ID:      1801,
		GroupID: &groupID,
		User:    &service.User{ID: 1701, Status: service.StatusActive},
	}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), apiKey)
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.User.ID, Concurrency: 1})
		c.Next()
	})
	router.GET("/openai/v1/responses", h.ResponsesWebSocket)
	handlerServer := httptest.NewServer(router)
	defer handlerServer.Close()

	headers := http.Header{}
	if tc.userAgent != nil {
		headers.Set("User-Agent", *tc.userAgent)
	}
	dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
	clientConn, _, err := coderws.Dial(
		dialCtx,
		"ws"+strings.TrimPrefix(handlerServer.URL, "http")+"/openai/v1/responses",
		&coderws.DialOptions{HTTPHeader: headers, CompressionMode: coderws.CompressionContextTakeover},
	)
	cancelDial()
	require.NoError(t, err)
	defer func() {
		_ = clientConn.CloseNow()
	}()

	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 3*time.Second)
	err = clientConn.Write(writeCtx, coderws.MessageText, []byte(tc.firstPayload))
	cancelWrite()
	require.NoError(t, err)

	readCtx, cancelRead := context.WithTimeout(context.Background(), 3*time.Second)
	_, event, err := clientConn.Read(readCtx)
	cancelRead()
	require.NoError(t, err)
	require.Equal(t, "response.completed", gjson.GetBytes(event, "type").String())
	_ = clientConn.Close(coderws.StatusNormalClosure, "done")

	var usageLog *service.UsageLog
	select {
	case usageLog = <-usageRepo.created:
		require.NotNil(t, usageLog)
	case <-time.After(3 * time.Second):
		t.Fatal("等待 WebSocket usage log 写入超时")
	}

	var upstreamFirstPayload []byte
	select {
	case upstreamFirstPayload = <-upstreamPayloadCh:
	case <-time.After(3 * time.Second):
		t.Fatal("等待上游 WebSocket 首帧超时")
	}

	select {
	case upstreamErr := <-upstreamErrCh:
		require.NoError(t, upstreamErr)
	case <-time.After(3 * time.Second):
		t.Fatal("等待上游 WebSocket 结束超时")
	}

	return openAIResponsesWSUsageLogResult{
		log:                  usageLog,
		upstreamFirstPayload: upstreamFirstPayload,
	}
}

func testStringPtr(v string) *string {
	return &v
}

func TestOpenAIHandleStreamingAwareErrorWithCode_EmitsStableClassification(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	(&OpenAIGatewayHandler{}).handleStreamingAwareErrorWithCode(
		c,
		http.StatusBadGateway,
		"upstream_error",
		service.OpenAIUpstreamHTTP2StreamErrorCode,
		"Upstream HTTP/2 stream failed",
		true,
	)

	body := w.Body.String()
	require.Contains(t, body, "event: error\n")
	require.Contains(t, body, `"type":"upstream_error"`)
	require.Contains(t, body, `"code":"upstream_http2_stream_error"`)
	require.NotContains(t, body, "stream ID")
}

func TestEnsureOpenAIStreamReadErrorResponse_IgnoresGenericErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	wrote := (&OpenAIGatewayHandler{}).ensureOpenAIStreamReadErrorResponse(c, errors.New("generic failure"), false)

	require.False(t, wrote)
	require.Empty(t, w.Body.String())
}

func TestOpenAIForwardErrorAlreadyCommunicated(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("upstream response failed after write", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, EndpointResponses, nil)
		before := c.Writer.Size()
		_, _ = c.Writer.WriteString(`event: response.failed
data: {"type":"response.failed","error":{"message":"This content was flagged"}}

`)

		reported := openAIForwardErrorAlreadyCommunicated(c, before, errors.New("upstream response failed: This content was flagged"))

		require.True(t, reported)
	})

	t.Run("no write still needs fallback", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, EndpointResponses, nil)

		reported := openAIForwardErrorAlreadyCommunicated(c, c.Writer.Size(), errors.New("upstream response failed: This content was flagged"))

		require.False(t, reported)
	})

	t.Run("generic error after write still needs fallback", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, EndpointResponses, nil)
		before := c.Writer.Size()
		_, _ = c.Writer.WriteString(":\n\n")

		reported := openAIForwardErrorAlreadyCommunicated(c, before, errors.New("stream read error: unexpected EOF"))

		require.False(t, reported)
	})
}
