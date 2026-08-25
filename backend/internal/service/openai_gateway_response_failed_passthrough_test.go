package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const cyberPolicyMessage = "This content was flagged for possible cybersecurity risk. If this seems wrong, try rephrasing your request. To get authorized for security work, join the Trusted Access for Cyber program: https://chatgpt.com/cyber"

func cyberPolicyFailedSSE() string {
	return `event: response.failed
` + `data: {"response":{"background":false,"completed_at":null,"created_at":1785157337,"error":{"code":"cyber_policy","message":` + strconv.Quote(cyberPolicyMessage) + `},"frequency_penalty":0,"id":"resp_06b0fd5c70ab3164016a6756d905dc819488a71be15c81d61e","model":"gpt-5.5","moderation":null,"object":"response","presence_penalty":0,"prompt_cache_retention":"24h","safety_identifier":"user-PvdVDgDtjCh8CwEu3PdfWkLh","service_tier":"default","status":"failed","store":false,"temperature":1,"tool_usage":{"image_gen":{"input_tokens":0,"input_tokens_details":"[REDACTED]","output_tokens":0,"output_tokens_details":"[REDACTED]","total_tokens":0},"web_search":{"num_requests":0}},"top_logprobs":0,"top_p":0.98,"user":null},"sequence_number":808,"type":"response.failed"}` + "\n\n"
}

func newResponseFailedPassthroughTestService(sse, requestID string) (*OpenAIGatewayService, *httpUpstreamRecorder) {
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{requestID}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}}
	return &OpenAIGatewayService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}, upstream
}

func requireSingleResponseFailedUpstreamRequest(t *testing.T, upstream *httpUpstreamRecorder) {
	t.Helper()
	require.Len(t, upstream.requests, 1)
	require.Equal(t, http.MethodPost, upstream.requests[0].Method)
	require.NotEmpty(t, upstream.bodies[0])
}

func bindCyberPolicyPassthroughRule(c *gin.Context) {
	bindCyberPolicyPassthroughRuleWithMessage(c, nil)
}

func bindCyberPolicyPassthroughRuleWithMessage(c *gin.Context, customMessage *string) {
	bindOpenAIErrorPassthroughRule(c, "cyber_policy", customMessage)
}

func bindOpenAIErrorPassthroughRule(c *gin.Context, keyword string, customMessage *string) {
	responseCode := http.StatusBadRequest
	rule := &model.ErrorPassthroughRule{
		ID:              1,
		Name:            "openai-error-policy",
		Enabled:         true,
		Priority:        1,
		Platforms:       []string{PlatformOpenAI},
		Keywords:        []string{keyword},
		MatchMode:       model.MatchModeAll,
		PassthroughCode: false,
		ResponseCode:    &responseCode,
		PassthroughBody: customMessage == nil,
		CustomMessage:   customMessage,
	}
	ruleSvc := &ErrorPassthroughService{}
	ruleSvc.setLocalCache([]*model.ErrorPassthroughRule{rule})
	BindErrorPassthroughService(c, ruleSvc)
}

func TestForwardAsChatCompletions_RecognizedCyberPolicyDoesNotRequireRule(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}],"stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	svc, upstream := newResponseFailedPassthroughTestService(cyberPolicyFailedSSE(), "rid_cyber_chat")

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, newOpenAICompatMessagesTestAccount(), body, "")

	requireSingleResponseFailedUpstreamRequest(t, upstream)
	require.Error(t, err)
	require.NotErrorAs(t, err, new(*UpstreamFailoverError))
	require.True(t, IsResponseCommitted(c))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "cyber_policy", gjson.GetBytes(rec.Body.Bytes(), "error.code").String())
	require.Equal(t, "This content was flagged for possible cybersecurity risk. If this seems wrong, try rephrasing your request. To get authorized for security work, join the Trusted Access for Cyber program: [url-redacted]", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
	require.NotContains(t, rec.Body.String(), "safety_identifier")
	require.NotContains(t, gjson.GetBytes(rec.Body.Bytes(), "error.type").String(), "response.failed")
}

func TestForwardAsChatCompletions_BufferedResponseFailedUsesBuiltInPolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}],"stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	svc, upstream := newResponseFailedPassthroughTestService(cyberPolicyFailedSSE(), "rid_cyber_chat")

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, newOpenAICompatMessagesTestAccount(), body, "")

	requireSingleResponseFailedUpstreamRequest(t, upstream)
	require.Error(t, err)
	require.NotErrorAs(t, err, new(*UpstreamFailoverError))
	require.True(t, IsResponseCommitted(c))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "invalid_request_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
	require.Equal(t, "cyber_policy", gjson.GetBytes(rec.Body.Bytes(), "error.code").String())
	require.Equal(t, "This content was flagged for possible cybersecurity risk. If this seems wrong, try rephrasing your request. To get authorized for security work, join the Trusted Access for Cyber program: [url-redacted]", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
	require.NotContains(t, rec.Body.String(), "response.failed")
}

func TestForwardAsChatCompletions_StreamingResponseFailedBeforeOutputUsesBuiltInPolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	svc, upstream := newResponseFailedPassthroughTestService(cyberPolicyFailedSSE(), "rid_cyber_chat")

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, newOpenAICompatMessagesTestAccount(), body, "")

	requireSingleResponseFailedUpstreamRequest(t, upstream)
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	require.True(t, IsResponseCommitted(c))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "cyber_policy", gjson.GetBytes(rec.Body.Bytes(), "error.code").String())
	require.Equal(t, "invalid_request_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
	require.NotContains(t, rec.Body.String(), "safety_identifier")
	require.NotContains(t, rec.Body.String(), "[DONE]")
}

func TestForwardAsChatCompletions_StreamingRecognizedCyberPolicyAfterOutputEmitsOneSafeTerminal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamSSE := `event: response.created
` +
		`data: {"type":"response.created","response":{"id":"resp_cyber"}}` + "\n\n" +
		`event: response.output_text.delta
` +
		`data: {"type":"response.output_text.delta","delta":"partial"}` + "\n\n" +
		cyberPolicyFailedSSE()
	svc, upstream := newResponseFailedPassthroughTestService(upstreamSSE, "rid_cyber_chat")

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, newOpenAICompatMessagesTestAccount(), body, "")

	requireSingleResponseFailedUpstreamRequest(t, upstream)
	require.Error(t, err)
	var requestErr *OpenAIUpstreamRequestError
	require.ErrorAs(t, err, &requestErr)
	require.True(t, requestErr.OutputStarted)
	require.Equal(t, 1, strings.Count(rec.Body.String(), "data: [DONE]"))
	require.Contains(t, rec.Body.String(), "partial")
	require.Contains(t, rec.Body.String(), "cyber_policy")
	require.NotContains(t, rec.Body.String(), "safety_identifier")
}

func TestForwardAsAnthropic_BufferedResponseFailedBypassesPassthroughRule(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.5","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c, rec := newOpenAICompatMessagesTestContext(body)
	bindCyberPolicyPassthroughRule(c)
	svc, upstream := newResponseFailedPassthroughTestService(cyberPolicyFailedSSE(), "rid_cyber_buffered")

	_, err := svc.ForwardAsAnthropic(context.Background(), c, newOpenAICompatMessagesTestAccount(), body, "", "gpt-5.5")

	requireSingleResponseFailedUpstreamRequest(t, upstream)
	require.Error(t, err)
	require.NotErrorAs(t, err, new(*UpstreamFailoverError))
	require.True(t, IsResponseCommitted(c))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "invalid_request_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
	require.Equal(t, sanitizeUpstreamErrorFactScalar(cyberPolicyMessage, upstreamErrorFactMaxScalarBytes), gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
	require.NotContains(t, rec.Body.String(), "response.failed")
}

func TestForwardAsAnthropic_StreamingResponseFailedBeforeOutputBypassesPassthroughRule(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.5","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	c, rec := newOpenAICompatMessagesTestContext(body)
	bindCyberPolicyPassthroughRule(c)
	svc, upstream := newResponseFailedPassthroughTestService(cyberPolicyFailedSSE(), "rid_cyber_streaming")

	_, err := svc.ForwardAsAnthropic(context.Background(), c, newOpenAICompatMessagesTestAccount(), body, "", "gpt-5.5")

	requireSingleResponseFailedUpstreamRequest(t, upstream)
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	require.True(t, IsResponseCommitted(c))
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "invalid_request_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
	require.Equal(t, sanitizeUpstreamErrorFactScalar(cyberPolicyMessage, upstreamErrorFactMaxScalarBytes), gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
	require.NotContains(t, rec.Body.String(), "response.failed")
}

func TestResponseFailedPassthroughMappedMessagesAreSanitizedAndBounded(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "response-failed-passthrough-secret"
	upstreamMessage := "cyber policy access_token=" + secret + " " + strings.Repeat("oversized ", openAIMessagesErrorMessageMaxBytes)
	failedSSE := `event: response.failed` + "\n" +
		`data: {"id":"resp_private","object":"response","model":"gpt-5.5","status":"failed","error":{"code":"unrecognized_policy","message":` +
		strconv.Quote(upstreamMessage) + `},"output":[],"usage":{"input_tokens":5,"output_tokens":0,"total_tokens":5}}` + "\n\n"

	tests := []struct {
		name      string
		anthropic bool
		stream    bool
	}{
		{name: "chat buffered"},
		{name: "chat streaming", stream: true},
		{name: "anthropic buffered", anthropic: true},
		{name: "anthropic streaming", anthropic: true, stream: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			customMessage := "custom policy access_token=" + secret + " " + strings.Repeat("custom ", openAIMessagesErrorMessageMaxBytes)
			for _, ruleCase := range []struct {
				name          string
				customMessage *string
				wantPrefix    string
			}{
				{name: "passthrough body", wantPrefix: "cyber policy"},
				{name: "custom message", customMessage: &customMessage, wantPrefix: "custom policy"},
			} {
				t.Run(ruleCase.name, func(t *testing.T) {
					var body []byte
					var c *gin.Context
					var rec *httptest.ResponseRecorder
					if tt.anthropic {
						body = []byte(fmt.Sprintf(`{"model":"gpt-5.5","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"stream":%t}`, tt.stream))
						c, rec = newOpenAICompatMessagesTestContext(body)
					} else {
						body = []byte(fmt.Sprintf(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}],"stream":%t}`, tt.stream))
						rec = httptest.NewRecorder()
						c, _ = gin.CreateTestContext(rec)
						c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
						c.Request.Header.Set("Content-Type", "application/json")
					}
					bindOpenAIErrorPassthroughRule(c, "unrecognized_policy", ruleCase.customMessage)
					svc := newOpenAICompatMessagesSSEService(io.NopCloser(strings.NewReader(failedSSE)), "rid_private", nil)

					var err error
					if tt.anthropic {
						_, err = svc.ForwardAsAnthropic(context.Background(), c, newOpenAICompatMessagesTestAccount(), body, "", "gpt-5.5")
					} else {
						_, err = svc.ForwardAsChatCompletions(context.Background(), c, newOpenAICompatMessagesTestAccount(), body, "")
					}

					require.Error(t, err)
					require.NotContains(t, err.Error(), secret)
					require.NotContains(t, rec.Body.String(), secret)
					mappedMessage := gjson.GetBytes(rec.Body.Bytes(), "error.message").String()
					require.Contains(t, mappedMessage, ruleCase.wantPrefix)
					require.Contains(t, mappedMessage, "access_token=[redacted]")
					require.LessOrEqual(t, len(mappedMessage), openAIMessagesErrorMessageMaxBytes)
				})
			}
		})
	}
}
