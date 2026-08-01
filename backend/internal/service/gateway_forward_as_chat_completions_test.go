//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestForwardAsChatCompletions_FailoverRetainsGeneric5xxHTTPFact(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	body := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello"}]}`)
	upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Header:     http.Header{"X-Request-Id": []string{"req_conversion_503"}},
		Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"vendor_failure","message":"private vendor detail"}}`)),
	}}
	svc := &GatewayService{httpUpstream: upstream, cfg: &config.Config{}}

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, newAnthropicAPIKeyAccountForTest(), body, nil)

	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr), err)
	fact, ok := failoverErr.UpstreamFact()
	require.True(t, ok)
	require.Equal(t, PlatformAnthropic, fact.Provider)
	require.Equal(t, http.StatusServiceUnavailable, fact.HTTPStatus)
	require.Equal(t, "vendor_failure", fact.ProviderType)
	require.Equal(t, "req_conversion_503", fact.RequestID)
}

func TestForwardAsChatCompletions_FailoverEventAttachesFactForSkipMonitoring(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rule := newNonFailoverPassthroughRule(http.StatusServiceUnavailable, "vendor marker", http.StatusTeapot, "safe custom message")
	rule.SkipMonitoring = true
	ruleSvc := &ErrorPassthroughService{}
	ruleSvc.setLocalCache([]*model.ErrorPassthroughRule{rule})
	BindErrorPassthroughService(c, ruleSvc)
	upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"vendor_failure","message":"vendor marker private detail"}}`)),
	}}
	svc := &GatewayService{httpUpstream: upstream, cfg: &config.Config{}}

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, newAnthropicAPIKeyAccountForTest(), []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello"}]}`), nil)

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	skip, ok := c.Get(OpsSkipPassthroughKey)
	require.True(t, ok)
	require.Equal(t, true, skip)
	events := c.MustGet(OpsUpstreamErrorsKey).([]*OpsUpstreamErrorEvent)
	require.Len(t, events, 1)
	require.NotNil(t, events[0].UpstreamFact)
	require.Equal(t, "vendor_failure", events[0].UpstreamFact.ProviderType)
}

func TestForwardAsChatCompletions_UnknownHTTPErrorUsesSafeFinalPresentation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	body := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello"}]}`)
	upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"X-Request-Id": []string{"req_conversion_unknown"}},
		Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"vendor_failure","message":"private detail https://internal.example.test/secret"}}`)),
	}}
	svc := &GatewayService{httpUpstream: upstream, cfg: &config.Config{}}

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, newAnthropicAPIKeyAccountForTest(), body, nil)

	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "upstream_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
	require.Equal(t, "Upstream request failed", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
	require.NotContains(t, rec.Body.String(), "private detail")
}

func TestForwardAsChatCompletions_UnknownRuleAppliesSkipMonitoring(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rule := newNonFailoverPassthroughRule(http.StatusBadRequest, "vendor marker", http.StatusTeapot, "safe custom message")
	rule.SkipMonitoring = true
	ruleSvc := &ErrorPassthroughService{}
	ruleSvc.setLocalCache([]*model.ErrorPassthroughRule{rule})
	BindErrorPassthroughService(c, ruleSvc)
	upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"vendor_failure","message":"vendor marker private detail"}}`)),
	}}
	svc := &GatewayService{httpUpstream: upstream, cfg: &config.Config{}}

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, newAnthropicAPIKeyAccountForTest(), []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello"}]}`), nil)

	require.Error(t, err)
	require.Equal(t, http.StatusTeapot, rec.Code)
	require.Equal(t, "safe custom message", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
	skip, ok := c.Get(OpsSkipPassthroughKey)
	require.True(t, ok)
	require.Equal(t, true, skip)
}

func TestForwardAsChatCompletions_RecognizedHTTPErrorBypassesFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	body := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello"}]}`)
	upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"X-Request-Id": []string{"req_conversion_policy"}},
		Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"invalid_request_error","code":"cyber_policy","message":"policy rejected"}}`)),
	}}
	svc := &GatewayService{httpUpstream: upstream, cfg: &config.Config{}}

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, newAnthropicAPIKeyAccountForTest(), body, nil)

	var recognizedErr *RecognizedUpstreamError
	require.True(t, errors.As(err, &recognizedErr), err)
	require.NotErrorAs(t, err, new(*UpstreamFailoverError))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "cyber_policy", gjson.GetBytes(rec.Body.Bytes(), "error.code").String())
	require.Equal(t, "policy rejected", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
}

func TestExtractCCReasoningEffortFromBody(t *testing.T) {
	t.Parallel()

	t.Run("nested reasoning.effort", func(t *testing.T) {
		got := extractCCReasoningEffortFromBody([]byte(`{"reasoning":{"effort":"HIGH"}}`))
		require.NotNil(t, got)
		require.Equal(t, "high", *got)
	})

	t.Run("flat reasoning_effort", func(t *testing.T) {
		got := extractCCReasoningEffortFromBody([]byte(`{"reasoning_effort":"x-high"}`))
		require.NotNil(t, got)
		require.Equal(t, "xhigh", *got)
	})

	t.Run("max normalizes to xhigh", func(t *testing.T) {
		got := extractCCReasoningEffortFromBody([]byte(`{"reasoning_effort":"max"}`))
		require.NotNil(t, got)
		require.Equal(t, "xhigh", *got)
	})

	t.Run("missing effort", func(t *testing.T) {
		require.Nil(t, extractCCReasoningEffortFromBody([]byte(`{"model":"gpt-5"}`)))
	})
}

func TestHandleCCBufferedFromAnthropic_PreservesMessageStartCacheUsageAndReasoning(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	reasoningEffort := "high"
	resp := &http.Response{
		Header: http.Header{"x-request-id": []string{"rid_cc_buffered"}},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`event: message_start`,
			`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4.5","stop_reason":"","usage":{"input_tokens":12,"cache_read_input_tokens":9,"cache_creation_input_tokens":3}}}`,
			``,
			`event: content_block_start`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"hello"}}`,
			``,
			`event: message_delta`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`,
			``,
		}, "\n"))),
	}

	svc := &GatewayService{}
	result, err := svc.handleCCBufferedFromAnthropic(resp, c, "gpt-5", "claude-sonnet-4.5", &reasoningEffort, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 12, result.Usage.InputTokens)
	require.Equal(t, 7, result.Usage.OutputTokens)
	require.Equal(t, 9, result.Usage.CacheReadInputTokens)
	require.Equal(t, 3, result.Usage.CacheCreationInputTokens)
	require.NotNil(t, result.ReasoningEffort)
	require.Equal(t, "high", *result.ReasoningEffort)
}

func TestHandleCCStreamingFromAnthropic_PreservesMessageStartCacheUsageAndReasoning(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	reasoningEffort := "medium"
	resp := &http.Response{
		Header: http.Header{"x-request-id": []string{"rid_cc_stream"}},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`event: message_start`,
			`data: {"type":"message_start","message":{"id":"msg_2","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4.5","stop_reason":"","usage":{"input_tokens":20,"cache_read_input_tokens":11,"cache_creation_input_tokens":4}}}`,
			``,
			`event: content_block_start`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"hello"}}`,
			``,
			`event: message_delta`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":8}}`,
			``,
			`event: message_stop`,
			`data: {"type":"message_stop"}`,
			``,
		}, "\n"))),
	}

	svc := &GatewayService{}
	result, err := svc.handleCCStreamingFromAnthropic(resp, c, "gpt-5", "claude-sonnet-4.5", &reasoningEffort, time.Now(), true)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 20, result.Usage.InputTokens)
	require.Equal(t, 8, result.Usage.OutputTokens)
	require.Equal(t, 11, result.Usage.CacheReadInputTokens)
	require.Equal(t, 4, result.Usage.CacheCreationInputTokens)
	require.NotNil(t, result.ReasoningEffort)
	require.Equal(t, "medium", *result.ReasoningEffort)
	require.Contains(t, rec.Body.String(), `[DONE]`)
}
