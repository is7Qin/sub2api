package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestApplyErrorPassthroughRule_PassthroughBodyUsesFactSafeMessage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	ruleSvc := &ErrorPassthroughService{}
	ruleSvc.setLocalCache([]*model.ErrorPassthroughRule{{
		Enabled: true, Priority: 1, Platforms: []string{PlatformOpenAI},
		Keywords: []string{"vendor_failure"}, MatchMode: model.MatchModeAny,
		PassthroughCode: true, PassthroughBody: true,
	}})
	BindErrorPassthroughService(c, ruleSvc)
	body := []byte(`{"error":{"code":"vendor_failure","message":"Authorization: Bearer sk-private https://internal.example/path"}}`)

	status, errType, message, matched := applyErrorPassthroughRule(
		c, PlatformOpenAI, http.StatusBadRequest, body,
		http.StatusBadGateway, "upstream_error", "Upstream request failed",
	)

	require.True(t, matched)
	require.Equal(t, http.StatusBadRequest, status)
	require.Equal(t, "upstream_error", errType)
	require.NotContains(t, message, "sk-private")
	require.NotContains(t, message, "internal.example")
	require.LessOrEqual(t, len(message), upstreamErrorFactMaxScalarBytes)
}

func TestErrorPassthroughService_MatchUnknownRuleUsesBoundedFactText(t *testing.T) {
	responseCode := http.StatusTeapot
	customMessage := "safe unknown message"
	svc := &ErrorPassthroughService{}
	svc.setLocalCache([]*model.ErrorPassthroughRule{{
		Enabled: true, Priority: 1, Platforms: []string{PlatformOpenAI},
		Keywords: []string{"vendor_failure"}, MatchMode: model.MatchModeAny,
		ResponseCode: &responseCode, CustomMessage: &customMessage,
	}})

	matched := svc.MatchUnknownRule(UpstreamErrorFact{
		Provider: PlatformOpenAI, Source: UpstreamErrorSourceHTTP,
		HTTPStatusKnown: true, HTTPStatus: http.StatusBadGateway,
		ProviderCode: "vendor_failure", InternalMatchText: "openai vendor_failure",
	})

	require.NotNil(t, matched)
	require.Equal(t, responseCode, *matched.ResponseCode)
}

func TestErrorPassthroughService_MatchUnknownRuleRejectsRecognizedFact(t *testing.T) {
	responseCode := http.StatusTeapot
	svc := &ErrorPassthroughService{}
	svc.setLocalCache([]*model.ErrorPassthroughRule{{
		Enabled: true, Priority: 1, Platforms: []string{PlatformOpenAI},
		Keywords: []string{"server_is_overloaded"}, MatchMode: model.MatchModeAny,
		ResponseCode: &responseCode,
	}})

	matched := svc.MatchUnknownRule(UpstreamErrorFact{
		Provider: PlatformOpenAI, Source: UpstreamErrorSourceSSE,
		ProviderCode: "server_is_overloaded", InternalMatchText: "openai server_is_overloaded",
	})

	require.Nil(t, matched)
}

func TestApplyErrorPassthroughRule_NoBoundService(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	status, errType, errMsg, matched := applyErrorPassthroughRule(
		c,
		PlatformAnthropic,
		http.StatusUnprocessableEntity,
		[]byte(`{"error":{"message":"invalid schema"}}`),
		http.StatusBadGateway,
		"upstream_error",
		"Upstream request failed",
	)

	assert.False(t, matched)
	assert.Equal(t, http.StatusBadGateway, status)
	assert.Equal(t, "upstream_error", errType)
	assert.Equal(t, "Upstream request failed", errMsg)
}

func TestGatewayHandleErrorResponse_NoRuleKeepsDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	svc := &GatewayService{}
	respBody := []byte(`{"error":{"message":"Invalid schema for field messages"}}`)
	resp := &http.Response{
		StatusCode: http.StatusUnprocessableEntity,
		Body:       io.NopCloser(bytes.NewReader(respBody)),
		Header:     http.Header{},
	}
	account := &Account{ID: 11, Platform: PlatformAnthropic, Type: AccountTypeAPIKey}

	_, err := svc.handleErrorResponse(context.Background(), resp, c, account)
	require.Error(t, err)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	errField, ok := payload["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "upstream_error", errField["type"])
	assert.Equal(t, "Upstream request failed", errField["message"])
}

func TestOpenAIForward_RecognizedOverloadBypassesFailoverAndConflictingRule(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	conflictingMessage := "database rule must not win"
	responseCode := http.StatusTeapot
	ruleSvc := &ErrorPassthroughService{}
	ruleSvc.setLocalCache([]*model.ErrorPassthroughRule{{
		Enabled:         true,
		Priority:        1,
		Platforms:       []string{PlatformOpenAI},
		Keywords:        []string{"server_is_overloaded"},
		MatchMode:       model.MatchModeAll,
		PassthroughCode: false,
		ResponseCode:    &responseCode,
		CustomMessage:   &conflictingMessage,
	}})
	BindErrorPassthroughService(c, ruleSvc)

	respBody := []byte(`{"error":{"code":"server_is_overloaded","type":"response.failed","message":"Bearer sk-private"}}`)
	svc := &OpenAIGatewayService{
		httpUpstream: &httpUpstreamRecorder{resp: &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(bytes.NewReader(respBody)),
			Header:     http.Header{},
		}},
		cfg: &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          120,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "sk-test"},
	}

	_, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-5.4","input":"hello"}`))

	require.Error(t, err)
	require.NotErrorAs(t, err, new(*UpstreamFailoverError))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "server_is_overloaded", gjson.GetBytes(rec.Body.Bytes(), "error.code").String())
	assert.Equal(t, "service_unavailable_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
	assert.NotContains(t, rec.Body.String(), "sk-private")
	assert.NotContains(t, rec.Body.String(), conflictingMessage)
}

func TestOpenAIHandleErrorResponse_RecognizedOverloadBypassesConflictingRule(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	conflictingMessage := "database rule must not win"
	responseCode := http.StatusTeapot
	ruleSvc := &ErrorPassthroughService{}
	ruleSvc.setLocalCache([]*model.ErrorPassthroughRule{{
		Enabled:         true,
		Priority:        1,
		Platforms:       []string{PlatformOpenAI},
		Keywords:        []string{"server_is_overloaded"},
		MatchMode:       model.MatchModeAll,
		PassthroughCode: false,
		ResponseCode:    &responseCode,
		CustomMessage:   &conflictingMessage,
	}})
	BindErrorPassthroughService(c, ruleSvc)

	svc := &OpenAIGatewayService{}
	respBody := []byte(`{"error":{"code":"server_is_overloaded","type":"service_unavailable_error","message":"Please retry later"}}`)
	resp := &http.Response{
		StatusCode: http.StatusBadGateway,
		Body:       io.NopCloser(bytes.NewReader(respBody)),
		Header:     http.Header{},
	}
	account := &Account{ID: 120, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	_, err := svc.handleErrorResponse(context.Background(), resp, c, account, nil)

	require.Error(t, err)
	require.NotErrorAs(t, err, new(*UpstreamFailoverError))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "server_is_overloaded", gjson.GetBytes(rec.Body.Bytes(), "error.code").String())
	assert.Equal(t, "service_unavailable_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
	assert.Equal(t, "Please retry later", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
	_, skipMonitoring := c.Get(OpsSkipPassthroughKey)
	assert.False(t, skipMonitoring)
}

func TestOpenAIHandleErrorResponse_NoRuleKeepsDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	svc := &OpenAIGatewayService{}
	respBody := []byte(`{"error":{"message":"Invalid schema for field messages"}}`)
	resp := &http.Response{
		StatusCode: http.StatusUnprocessableEntity,
		Body:       io.NopCloser(bytes.NewReader(respBody)),
		Header:     http.Header{},
	}
	account := &Account{ID: 12, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	_, err := svc.handleErrorResponse(context.Background(), resp, c, account, nil)
	require.Error(t, err)
	assert.Equal(t, http.StatusBadGateway, rec.Code)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	errField, ok := payload["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "upstream_error", errField["type"])
	assert.Equal(t, "Upstream request failed", errField["message"])
}

func TestGeminiWriteGeminiMappedError_NoRuleKeepsDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	svc := &GeminiMessagesCompatService{}
	respBody := []byte(`{"error":{"code":422,"message":"Invalid schema for field messages","status":"INVALID_ARGUMENT"}}`)
	account := &Account{ID: 13, Platform: PlatformGemini, Type: AccountTypeAPIKey}

	err := svc.writeGeminiMappedError(c, account, http.StatusUnprocessableEntity, "req-2", respBody)
	require.Error(t, err)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	errField, ok := payload["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "invalid_request_error", errField["type"])
	assert.Equal(t, "Upstream request failed", errField["message"])
}

func TestGatewayHandleErrorResponse_AppliesRuleFor422(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	ruleSvc := &ErrorPassthroughService{}
	ruleSvc.setLocalCache([]*model.ErrorPassthroughRule{newNonFailoverPassthroughRule(http.StatusUnprocessableEntity, "invalid schema", http.StatusTeapot, "上游请求失败")})
	BindErrorPassthroughService(c, ruleSvc)

	svc := &GatewayService{}
	respBody := []byte(`{"error":{"message":"Invalid schema for field messages"}}`)
	resp := &http.Response{
		StatusCode: http.StatusUnprocessableEntity,
		Body:       io.NopCloser(bytes.NewReader(respBody)),
		Header:     http.Header{},
	}
	account := &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeAPIKey}

	_, err := svc.handleErrorResponse(context.Background(), resp, c, account)
	require.Error(t, err)
	assert.Equal(t, http.StatusTeapot, rec.Code)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	errField, ok := payload["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "upstream_error", errField["type"])
	assert.Equal(t, "上游请求失败", errField["message"])
}

func TestOpenAIHandleErrorResponse_AppliesRuleFor422(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	ruleSvc := &ErrorPassthroughService{}
	ruleSvc.setLocalCache([]*model.ErrorPassthroughRule{newNonFailoverPassthroughRule(http.StatusUnprocessableEntity, "invalid schema", http.StatusTeapot, "OpenAI上游失败")})
	BindErrorPassthroughService(c, ruleSvc)

	svc := &OpenAIGatewayService{}
	respBody := []byte(`{"error":{"message":"Invalid schema for field messages"}}`)
	resp := &http.Response{
		StatusCode: http.StatusUnprocessableEntity,
		Body:       io.NopCloser(bytes.NewReader(respBody)),
		Header:     http.Header{},
	}
	account := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	_, err := svc.handleErrorResponse(context.Background(), resp, c, account, nil)
	require.Error(t, err)
	assert.Equal(t, http.StatusTeapot, rec.Code)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	errField, ok := payload["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "upstream_error", errField["type"])
	assert.Equal(t, "OpenAI上游失败", errField["message"])
}

func TestOpenAIHandleErrorResponse_RedactsOpsDiagnostics(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	installationID := "550e8400-e29b-41d4-a716-446655440000"
	threadID := "018fed75-1b7e-7000-8000-000000000123"
	accessToken := "setup-secret-access-token"
	rawUA := "codex-tui/0.136.0 (Mac OS 26.5.0; arm64) Apple_Terminal/470.2 (codex-tui; 0.136.0)"
	respBody := []byte(`{"error":{"message":"bad x-codex-installation-id=` + installationID + ` thread-id=` + threadID + ` Authorization=Bearer ` + accessToken + ` ua ` + rawUA + `"},"raw_user_agent":"` + rawUA + `","prompt_cache_key":"probe_openai_usage:` + threadID + `"}`)
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       io.NopCloser(bytes.NewReader(respBody)),
		Header:     http.Header{},
	}
	svc := &OpenAIGatewayService{
		cfg: &config.Config{Gateway: config.GatewayConfig{LogUpstreamErrorBody: true, LogUpstreamErrorBodyMaxBytes: 4096}},
	}
	account := &Account{ID: 312, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	_, err := svc.handleErrorResponse(context.Background(), resp, c, account, nil)

	require.Error(t, err)
	messageRaw, ok := c.Get(OpsUpstreamErrorMessageKey)
	require.True(t, ok)
	detailRaw, ok := c.Get(OpsUpstreamErrorDetailKey)
	require.True(t, ok)
	combined := err.Error() + "\n" + messageRaw.(string) + "\n" + detailRaw.(string)
	for _, leaked := range []string{installationID, threadID, accessToken, rawUA, "setup-secret"} {
		require.NotContains(t, combined, leaked)
	}
	require.Contains(t, combined, "x-codex-installation-id=[redacted]")
	require.Contains(t, combined, "thread-id=[redacted]")
	require.Contains(t, combined, "[redacted]")
	require.Contains(t, combined, "[codex-user-agent-redacted]")
}

func TestGeminiWriteGeminiMappedError_RecognizedInvalidArgumentPrecedesRule(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	ruleSvc := &ErrorPassthroughService{}
	ruleSvc.setLocalCache([]*model.ErrorPassthroughRule{newNonFailoverPassthroughRule(http.StatusUnprocessableEntity, "invalid schema", http.StatusTeapot, "Gemini上游失败")})
	BindErrorPassthroughService(c, ruleSvc)

	svc := &GeminiMessagesCompatService{}
	respBody := []byte(`{"error":{"code":422,"message":"Invalid schema for field messages","status":"INVALID_ARGUMENT"}}`)
	account := &Account{ID: 3, Platform: PlatformGemini, Type: AccountTypeAPIKey}

	err := svc.writeGeminiMappedError(c, account, http.StatusUnprocessableEntity, "req-1", respBody)
	require.Error(t, err)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	errField, ok := payload["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "invalid_request_error", errField["type"])
	assert.Equal(t, "Invalid schema for field messages", errField["message"])
}

func TestApplyErrorPassthroughRule_SkipMonitoringSetsContextKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	rule := newNonFailoverPassthroughRule(http.StatusBadRequest, "prompt is too long", http.StatusBadRequest, "上下文超限")
	rule.SkipMonitoring = true

	ruleSvc := &ErrorPassthroughService{}
	ruleSvc.setLocalCache([]*model.ErrorPassthroughRule{rule})
	BindErrorPassthroughService(c, ruleSvc)

	_, _, _, matched := applyErrorPassthroughRule(
		c,
		PlatformAnthropic,
		http.StatusBadRequest,
		[]byte(`{"error":{"message":"prompt is too long"}}`),
		http.StatusBadGateway,
		"upstream_error",
		"Upstream request failed",
	)

	assert.True(t, matched)
	v, exists := c.Get(OpsSkipPassthroughKey)
	assert.True(t, exists, "OpsSkipPassthroughKey should be set when skip_monitoring=true")
	boolVal, ok := v.(bool)
	assert.True(t, ok, "value should be bool")
	assert.True(t, boolVal)
}

func TestApplyErrorPassthroughRule_NoSkipMonitoringDoesNotSetContextKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	rule := newNonFailoverPassthroughRule(http.StatusBadRequest, "prompt is too long", http.StatusBadRequest, "上下文超限")
	rule.SkipMonitoring = false

	ruleSvc := &ErrorPassthroughService{}
	ruleSvc.setLocalCache([]*model.ErrorPassthroughRule{rule})
	BindErrorPassthroughService(c, ruleSvc)

	_, _, _, matched := applyErrorPassthroughRule(
		c,
		PlatformAnthropic,
		http.StatusBadRequest,
		[]byte(`{"error":{"message":"prompt is too long"}}`),
		http.StatusBadGateway,
		"upstream_error",
		"Upstream request failed",
	)

	assert.True(t, matched)
	_, exists := c.Get(OpsSkipPassthroughKey)
	assert.False(t, exists, "OpsSkipPassthroughKey should NOT be set when skip_monitoring=false")
}

func TestGatewayHandleErrorResponse_Unknown400UsesSafeEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"error":{"message":"bad request"},"access_token":"secret-token","debug":"internal"}`)
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}

	_, err := (&GatewayService{}).handleErrorResponse(
		context.Background(), resp, c,
		&Account{ID: 902, Platform: PlatformAnthropic, Type: AccountTypeAPIKey},
	)

	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "Upstream request failed", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
	require.NotContains(t, rec.Body.String(), "secret-token")
	require.NotContains(t, rec.Body.String(), "debug")
	require.True(t, IsResponseCommitted(c))
}

func TestGatewayHandleErrorResponse_Unknown503UsesGeneric502(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"error":{"code":"vendor_failure","message":"private vendor detail"}}`)
	resp := &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Header:     http.Header{},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}

	_, err := (&GatewayService{}).handleErrorResponse(
		context.Background(), resp, c,
		&Account{ID: 903, Platform: PlatformAnthropic, Type: AccountTypeAPIKey},
	)

	require.Error(t, err)
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Equal(t, "Upstream request failed", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
	require.NotContains(t, rec.Body.String(), "private vendor detail")
	require.True(t, IsResponseCommitted(c))
}

func TestGatewayHandleErrorResponse_SetsResponseCommitted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	svc := &GatewayService{}
	respBody := []byte(`{"error":{"message":"Invalid schema for field messages"}}`)
	resp := &http.Response{
		StatusCode: http.StatusUnprocessableEntity,
		Body:       io.NopCloser(bytes.NewReader(respBody)),
		Header:     http.Header{},
	}
	account := &Account{ID: 21, Platform: PlatformAnthropic, Type: AccountTypeAPIKey}

	_, err := svc.handleErrorResponse(context.Background(), resp, c, account)
	require.Error(t, err)
	assert.True(t, IsResponseCommitted(c))
}

func TestGatewayHandleErrorResponse_PassthroughRuleSetsCommitted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	ruleSvc := &ErrorPassthroughService{}
	ruleSvc.setLocalCache([]*model.ErrorPassthroughRule{newNonFailoverPassthroughRule(http.StatusUnprocessableEntity, "invalid schema", http.StatusTeapot, "上游请求失败")})
	BindErrorPassthroughService(c, ruleSvc)

	svc := &GatewayService{}
	respBody := []byte(`{"error":{"message":"Invalid schema for field messages"}}`)
	resp := &http.Response{
		StatusCode: http.StatusUnprocessableEntity,
		Body:       io.NopCloser(bytes.NewReader(respBody)),
		Header:     http.Header{},
	}
	account := &Account{ID: 22, Platform: PlatformAnthropic, Type: AccountTypeAPIKey}

	_, err := svc.handleErrorResponse(context.Background(), resp, c, account)
	require.Error(t, err)
	assert.True(t, IsResponseCommitted(c))
}

func TestOpenAIHandleErrorResponse_SetsResponseCommitted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	svc := &OpenAIGatewayService{}
	respBody := []byte(`{"error":{"message":"Invalid schema for field messages"}}`)
	resp := &http.Response{
		StatusCode: http.StatusUnprocessableEntity,
		Body:       io.NopCloser(bytes.NewReader(respBody)),
		Header:     http.Header{},
	}
	account := &Account{ID: 23, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	_, err := svc.handleErrorResponse(context.Background(), resp, c, account, nil)
	require.Error(t, err)
	assert.True(t, IsResponseCommitted(c))
}

func TestGeminiWriteGeminiMappedError_SetsResponseCommitted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	svc := &GeminiMessagesCompatService{}
	account := &Account{ID: 24, Platform: PlatformGemini, Type: AccountTypeAPIKey}
	err := svc.writeGeminiMappedError(c, account, http.StatusUnprocessableEntity, "req-committed", []byte(`{"error":{"message":"Invalid schema"}}`))

	require.Error(t, err)
	assert.True(t, IsResponseCommitted(c))
}

func newNonFailoverPassthroughRule(statusCode int, keyword string, respCode int, customMessage string) *model.ErrorPassthroughRule {
	return &model.ErrorPassthroughRule{
		ID:              1,
		Name:            "non-failover-rule",
		Enabled:         true,
		Priority:        1,
		ErrorCodes:      []int{statusCode},
		Keywords:        []string{keyword},
		MatchMode:       model.MatchModeAll,
		PassthroughCode: false,
		ResponseCode:    &respCode,
		PassthroughBody: false,
		CustomMessage:   &customMessage,
	}
}
