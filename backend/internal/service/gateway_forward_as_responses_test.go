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

func TestForwardAsResponses_FailoverRetainsStructuredHTTPFact(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	body := []byte(`{"model":"claude-sonnet-4-5","input":"hello"}`)
	upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"X-Request-Id": []string{"req_conversion_rate_limit"}},
		Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"quota exceeded"}}`)),
	}}
	svc := &GatewayService{httpUpstream: upstream, cfg: &config.Config{}}

	_, err := svc.ForwardAsResponses(context.Background(), c, newAnthropicAPIKeyAccountForTest(), body, nil)

	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr), err)
	fact, ok := failoverErr.UpstreamFact()
	require.True(t, ok)
	require.Equal(t, PlatformAnthropic, fact.Provider)
	require.Equal(t, http.StatusTooManyRequests, fact.HTTPStatus)
	require.Equal(t, "rate_limit_exceeded", fact.ProviderCode)
	require.Equal(t, "rate_limit_error", fact.ProviderType)
	require.Equal(t, "req_conversion_rate_limit", fact.RequestID)
}

func TestForwardAsResponses_FailoverEventAttachesFactForSkipMonitoring(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
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

	_, err := svc.ForwardAsResponses(context.Background(), c, newAnthropicAPIKeyAccountForTest(), []byte(`{"model":"claude-sonnet-4-5","input":"hello"}`), nil)

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

func TestForwardAsResponses_UnknownHTTPErrorUsesSafeFinalPresentation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	body := []byte(`{"model":"claude-sonnet-4-5","input":"hello"}`)
	upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"X-Request-Id": []string{"req_conversion_unknown"}},
		Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"vendor_failure","message":"private detail https://internal.example.test/secret"}}`)),
	}}
	svc := &GatewayService{httpUpstream: upstream, cfg: &config.Config{}}

	_, err := svc.ForwardAsResponses(context.Background(), c, newAnthropicAPIKeyAccountForTest(), body, nil)

	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "upstream_error", gjson.GetBytes(rec.Body.Bytes(), "error.code").String())
	require.Equal(t, "Upstream request failed", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
	require.NotContains(t, rec.Body.String(), "private detail")
}

func TestForwardAsResponses_UnknownRuleAppliesSkipMonitoring(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
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

	_, err := svc.ForwardAsResponses(context.Background(), c, newAnthropicAPIKeyAccountForTest(), []byte(`{"model":"claude-sonnet-4-5","input":"hello"}`), nil)

	require.Error(t, err)
	require.Equal(t, http.StatusTeapot, rec.Code)
	require.Equal(t, "safe custom message", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
	skip, ok := c.Get(OpsSkipPassthroughKey)
	require.True(t, ok)
	require.Equal(t, true, skip)
}

func TestForwardAsResponses_RecognizedHTTPErrorBypassesFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	body := []byte(`{"model":"claude-sonnet-4-5","input":"hello"}`)
	upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"X-Request-Id": []string{"req_conversion_context"}},
		Body:       io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"context too long"}}`)),
	}}
	svc := &GatewayService{httpUpstream: upstream, cfg: &config.Config{}}

	_, err := svc.ForwardAsResponses(context.Background(), c, newAnthropicAPIKeyAccountForTest(), body, nil)

	var recognizedErr *RecognizedUpstreamError
	require.True(t, errors.As(err, &recognizedErr), err)
	require.NotErrorAs(t, err, new(*UpstreamFailoverError))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "context_length_exceeded", gjson.GetBytes(rec.Body.Bytes(), "error.code").String())
	require.Equal(t, "context too long", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
}

func TestExtractResponsesReasoningEffortFromBody(t *testing.T) {
	t.Parallel()

	got := ExtractResponsesReasoningEffortFromBody([]byte(`{"model":"claude-sonnet-4.5","reasoning":{"effort":"HIGH"}}`))
	require.NotNil(t, got)
	require.Equal(t, "high", *got)

	got = ExtractResponsesReasoningEffortFromBody([]byte(`{"model":"deepseek-reasoner","reasoning":{"effort":"max"}}`))
	require.NotNil(t, got)
	require.Equal(t, "xhigh", *got)

	require.Nil(t, ExtractResponsesReasoningEffortFromBody([]byte(`{"model":"claude-sonnet-4.5"}`)))
}

func TestHandleResponsesBufferedStreamingResponse_PreservesMessageStartCacheUsage(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	resp := &http.Response{
		Header: http.Header{"x-request-id": []string{"rid_buffered"}},
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
	result, err := svc.handleResponsesBufferedStreamingResponse(resp, c, "claude-sonnet-4.5", "claude-sonnet-4.5", nil, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 12, result.Usage.InputTokens)
	require.Equal(t, 7, result.Usage.OutputTokens)
	require.Equal(t, 9, result.Usage.CacheReadInputTokens)
	require.Equal(t, 3, result.Usage.CacheCreationInputTokens)
	require.Contains(t, rec.Body.String(), `"cached_tokens":9`)
}

func TestHandleResponsesStreamingResponse_PreservesMessageStartCacheUsage(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	resp := &http.Response{
		Header: http.Header{"x-request-id": []string{"rid_stream"}},
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
	result, err := svc.handleResponsesStreamingResponse(resp, c, "claude-sonnet-4.5", "claude-sonnet-4.5", nil, time.Now())
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 20, result.Usage.InputTokens)
	require.Equal(t, 8, result.Usage.OutputTokens)
	require.Equal(t, 11, result.Usage.CacheReadInputTokens)
	require.Equal(t, 4, result.Usage.CacheCreationInputTokens)
	require.Contains(t, rec.Body.String(), `response.completed`)
}
