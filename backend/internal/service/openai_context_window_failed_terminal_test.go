package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const openAIContextFailedTerminalPayload = `{"type":"response.failed","response":{"id":"resp_context_failed","status":"failed","error":{"code":"context_length_exceeded","type":"invalid_request_error","message":"Your input exceeds the context window of this model. api_key=sk-private"},"usage":{"input_tokens":37,"output_tokens":0}}}`

func requireOpenAIContextRequestError(t *testing.T, err error) *OpenAIUpstreamRequestError {
	t.Helper()
	if err == nil {
		t.Error("expected OpenAI upstream request error")
		return nil
	}
	var requestErr *OpenAIUpstreamRequestError
	if !errors.As(err, &requestErr) || requestErr == nil {
		t.Errorf("expected OpenAIUpstreamRequestError, got %T", err)
		return nil
	}
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	require.Equal(t, http.StatusBadRequest, requestErr.StatusCode)
	require.Equal(t, "context_length_exceeded", requestErr.Code)
	require.Equal(t, "invalid_request_error", requestErr.Type)
	require.Contains(t, requestErr.Message, "context window")
	require.NotContains(t, requestErr.Message, "sk-private")
	return requestErr
}

func TestOpenAIContextFailedTerminalClassifierUsesTrustedBoundedFieldsAcrossAccounts(t *testing.T) {
	for _, accountType := range []string{AccountTypeAPIKey, AccountTypeOAuth, AccountTypeSetupToken} {
		t.Run(accountType, func(t *testing.T) {
			err := newOpenAIUpstreamRequestError([]byte(openAIContextFailedTerminalPayload), "rid-context")
			requestErr := requireOpenAIContextRequestError(t, err)
			require.Equal(t, "rid-context", requestErr.RequestID)
		})
	}

	for _, payload := range []string{
		`{"error":{"code":"context_length_exceeded","message":"context window"}}`,
		`{"response":{"error":{"code":"context_length_exceeded","message":"context window"}}}`,
	} {
		requireOpenAIContextRequestError(t, newOpenAIUpstreamRequestError([]byte(payload), ""))
	}

	for _, payload := range []string{
		`{"error":{"message":"capacity"},"debug":"context_length_exceeded"}`,
		`[{"error":{"code":"context_length_exceeded"}}]`,
		`{"error":{"code":"other"},"error":{"code":"context_length_exceeded"}}`,
		`{"error":{"code":"context_length_exceeded"}`,
		`{"error":{"code":"server_error","message":"temporary outage"}}`,
		`{"code":"context_length_exceeded","message":"context window"}`,
		`{"error":{"code":"server_error","message":"temporary outage"},"code":"context_length_exceeded","message":"context window"}`,
		`{"error":{"code":"context_length_exceeded","message":"` + strings.Repeat("x", openAIErrorClassificationMaxBytes) + `"}}`,
	} {
		require.Nil(t, newOpenAIUpstreamRequestError([]byte(payload), ""), payload)
	}
}

func TestOpenAIContextFailedTerminalClassifierRejectsConflictingTrustedEnvelopes(t *testing.T) {
	for _, payload := range []string{
		`{"error":{"code":"context_length_exceeded","message":"context window"},"response":{"error":{"code":"server_error","message":"temporary outage"}}}`,
		`{"error":{"code":"server_error","message":"temporary outage"},"response":{"error":{"code":"context_length_exceeded","message":"context window"}}}`,
		`{"error":{"code":"context_length_exceeded","message":"context window"},"response":{"error":{"code":"context_length_exceeded","message":"different context window"}}}`,
	} {
		require.Nil(t, newOpenAIUpstreamRequestError([]byte(payload), ""), payload)
	}
}

func TestOpenAIContextFailedTerminalErrorTypeIsEndpointSafeAndBounded(t *testing.T) {
	for _, payload := range []string{
		`{"error":{"code":"context_length_exceeded","type":"server_error","message":"context window"}}`,
		`{"error":{"code":"context_length_exceeded","type":"` + strings.Repeat("x", openAIUpstreamRequestErrorMessageMaxBytes) + `","message":"context window"}}`,
	} {
		requestErr := requireOpenAIContextRequestError(t, newOpenAIUpstreamRequestError([]byte(payload), ""))
		require.Equal(t, "invalid_request_error", requestErr.Type)
		require.Equal(t, "invalid_request_error", requestErr.AnthropicErrorType())
	}
}

func TestOpenAIContextFailedTerminalDualEnvelopesMustAgree(t *testing.T) {
	for _, payload := range []string{
		`{"error":{"code":"context_length_exceeded","type":"invalid_request_error","message":"context window exceeded"},"response":{"error":{"code":"context_length_exceeded","type":"invalid_request_error","message":"context window exceeded"}}}`,
		`{"error":{"code":"other","message":"maximum context window exceeded"},"response":{"error":{"code":"other","message":"maximum context window exceeded"}}}`,
	} {
		requestErr := newOpenAIUpstreamRequestError([]byte(payload), "")
		require.NotNil(t, requestErr, payload)
		if requestErr != nil {
			requireOpenAIContextRequestError(t, requestErr)
		}
	}

	for _, payload := range []string{
		`{"error":{"code":"context_length_exceeded","message":"context window exceeded"},"response":{"error":{"code":"server_error","message":"temporary outage"}}}`,
		`{"error":{"code":"context_length_exceeded","message":"context window exceeded"},"response":{"error":{"code":"context_too_large","message":"context window exceeded"}}}`,
		`{"error":{"code":"other","message":"maximum context length exceeded"},"response":{"error":{"code":"other","message":"temporary outage"}}}`,
	} {
		require.Nil(t, newOpenAIUpstreamRequestError([]byte(payload), ""), payload)
	}
}

func TestOpenAIContextFailedTerminalNormalizesUntrustedType(t *testing.T) {
	payloads := [][]byte{
		[]byte(`{"error":{"code":"context_length_exceeded","type":"api_key=sk-private","message":"context window exceeded"}}`),
		[]byte(`{"error":{"code":"context_length_exceeded","type":"` + strings.Repeat("x", openAIUpstreamRequestErrorMessageMaxBytes+1) + `","message":"context window exceeded"}}`),
		append([]byte(`{"error":{"code":"context_length_exceeded","type":"`), append([]byte{0xff}, []byte(`","message":"context window exceeded"}}`)...)...),
	}
	for _, payload := range payloads {
		requestErr := newOpenAIUpstreamRequestError(payload, "")
		if utf8.Valid(payload) {
			requireOpenAIContextRequestError(t, requestErr)
			require.Equal(t, "invalid_request_error", requestErr.Type)
			require.NotContains(t, string(requestErr.ChatErrorBody()), "sk-private")
			continue
		}
		require.Nil(t, requestErr)
	}
}

func TestOpenAIUpstreamRequestErrorOwnsTerminalUsageCopy(t *testing.T) {
	requestErr := requireOpenAIContextRequestError(t, newOpenAIUpstreamRequestError([]byte(openAIContextFailedTerminalPayload), ""))
	usage := OpenAIUsage{InputTokens: 37, OutputTokens: 2}

	requestErr.attachUsage(usage)
	usage.InputTokens = 99

	require.Equal(t, 37, requestErr.Usage.InputTokens)
	require.Equal(t, 2, requestErr.Usage.OutputTokens)
}

func TestForwardAsChatCompletionsBufferedContextFailedIsNativeRequestError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"large"}],"stream":false}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}, "X-Request-Id": {"rid-chat-context"}},
		Body:       io.NopCloser(strings.NewReader("event: response.failed\ndata: " + openAIContextFailedTerminalPayload + "\n\n")),
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1, Credentials: map[string]any{"access_token": "token", "chatgpt_account_id": "acc"}}

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "")

	requestErr := requireOpenAIContextRequestError(t, err)
	require.Equal(t, "rid-chat-context", requestErr.RequestID)
	require.NotNil(t, result)
	require.Equal(t, 37, result.Usage.InputTokens)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "context_length_exceeded", gjson.Get(rec.Body.String(), "error.code").String())
	require.NotContains(t, rec.Body.String(), "sk-private")
	require.NotContains(t, rec.Body.String(), `"finish_reason":"stop"`)
}

func TestForwardAsChatCompletionsStreamingContextFailedIsNativeSSERequestError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"large"}],"stream":true}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}, "X-Request-Id": {"rid-chat-context-stream"}},
		Body:       io.NopCloser(strings.NewReader("event: response.failed\ndata: " + openAIContextFailedTerminalPayload + "\n\n")),
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeSetupToken, Concurrency: 1, Credentials: map[string]any{"access_token": "token", "chatgpt_account_id": "acc"}}

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "")

	requireOpenAIContextRequestError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 37, result.Usage.InputTokens)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"code":"context_length_exceeded"`)
	require.NotContains(t, rec.Body.String(), "sk-private")
	require.NotContains(t, rec.Body.String(), `"finish_reason":"stop"`)
}

func TestForwardAsChatCompletionsStreamingContextFailedAfterOutputCarriesOutputDisposition(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"large"}],"stream":true}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	upstreamBody := strings.Join([]string{
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","response_id":"resp_chat_context_output","delta":"partial"}`,
		"",
		"event: response.failed",
		"data: " + openAIContextFailedTerminalPayload,
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}, "X-Request-Id": {"rid-chat-context-output"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1, Credentials: map[string]any{"access_token": "token", "chatgpt_account_id": "acc"}}

	result, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "")

	requestErr := requireOpenAIContextRequestError(t, err)
	require.True(t, requestErr.OutputStarted)
	require.NotNil(t, requestErr.Usage)
	require.NotNil(t, result)
	require.Equal(t, 37, result.Usage.InputTokens)
	require.Contains(t, rec.Body.String(), "partial")
}

func TestForwardResponsesContextFailedBeforeOutputRelaysSanitizedNativeFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","stream":true,"input":"large"}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}, "X-Request-Id": {"rid-responses-context"}},
		Body:       io.NopCloser(strings.NewReader("event: response.failed\ndata: " + openAIContextFailedTerminalPayload + "\n\n")),
	}}
	svc := &OpenAIGatewayService{cfg: passthroughPrivacyTestConfig(), httpUpstream: upstream, toolCorrector: NewCodexToolCorrector()}

	result, err := svc.Forward(context.Background(), c, passthroughPrivacyNativeAccount(), body)

	requestErr := requireOpenAIContextRequestError(t, err)
	require.False(t, requestErr.OutputStarted)
	require.NotNil(t, result)
	require.Equal(t, 37, result.Usage.InputTokens)
	require.Contains(t, rec.Body.String(), "event: response.failed")
	require.Contains(t, rec.Body.String(), "context_length_exceeded")
	require.NotContains(t, rec.Body.String(), "sk-private")
}

func TestForwardResponsesContextFailedAfterOutputCarriesOutputDisposition(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","stream":true,"input":"large"}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	upstreamBody := strings.Join([]string{
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","delta":"partial"}`,
		"",
		"event: response.failed",
		"data: " + openAIContextFailedTerminalPayload,
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}, "X-Request-Id": {"rid-responses-context-output"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}
	svc := &OpenAIGatewayService{cfg: passthroughPrivacyTestConfig(), httpUpstream: upstream, toolCorrector: NewCodexToolCorrector()}

	result, err := svc.Forward(context.Background(), c, passthroughPrivacyNativeAccount(), body)

	requestErr := requireOpenAIContextRequestError(t, err)
	require.True(t, requestErr.OutputStarted)
	require.NotNil(t, requestErr.Usage)
	require.NotNil(t, result)
	require.Equal(t, 37, result.Usage.InputTokens)
}

func TestForwardAsAnthropicStreamingContextFailedIsNativeRequestError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"large"}],"max_tokens":8,"stream":true}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}, "X-Request-Id": {"rid-messages-context-stream"}},
		Body:       io.NopCloser(strings.NewReader("event: response.failed\ndata: " + openAIContextFailedTerminalPayload + "\n\n")),
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1, Credentials: map[string]any{"access_token": "token", "chatgpt_account_id": "acc"}}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")

	requestErr := requireOpenAIContextRequestError(t, err)
	require.False(t, requestErr.OutputStarted)
	require.NotNil(t, result)
	require.Equal(t, 37, result.Usage.InputTokens)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "event: error")
	require.Contains(t, rec.Body.String(), `"type":"invalid_request_error"`)
	require.NotContains(t, rec.Body.String(), "sk-private")
}

func TestForwardAsAnthropicStreamingContextFailedAfterOutputCarriesOutputDisposition(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"large"}],"max_tokens":8,"stream":true}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	upstreamBody := strings.Join([]string{
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","delta":"partial"}`,
		"",
		"event: response.failed",
		"data: " + openAIContextFailedTerminalPayload,
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}, "X-Request-Id": {"rid-messages-context-output"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1, Credentials: map[string]any{"access_token": "token", "chatgpt_account_id": "acc"}}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")

	requestErr := requireOpenAIContextRequestError(t, err)
	require.True(t, requestErr.OutputStarted)
	require.NotNil(t, requestErr.Usage)
	require.NotNil(t, result)
	require.Equal(t, 37, result.Usage.InputTokens)
}

func TestForwardAsAnthropicBufferedContextFailedIsNativeRequestError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"large"}],"max_tokens":8,"stream":false}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}, "X-Request-Id": {"rid-messages-context"}},
		Body:       io.NopCloser(strings.NewReader("event: response.failed\ndata: " + openAIContextFailedTerminalPayload + "\n\n")),
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1, Credentials: map[string]any{"access_token": "token", "chatgpt_account_id": "acc"}}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "")

	requireOpenAIContextRequestError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 37, result.Usage.InputTokens)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "invalid_request_error", gjson.Get(rec.Body.String(), "error.type").String())
	require.NotContains(t, rec.Body.String(), "sk-private")
}

func TestHandleSSEToJSONContextFailedIsNativeRequestError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{false, true} {
		t.Run(map[bool]string{false: "oauth_adapter", true: "api_key_passthrough"}[passthrough], func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"text/event-stream"}, "X-Request-Id": {"rid-non-stream-context"}},
			}
			body := []byte("event: response.failed\ndata: " + openAIContextFailedTerminalPayload + "\n\n")
			svc := &OpenAIGatewayService{cfg: &config.Config{}}

			if passthrough {
				result, err := svc.handlePassthroughSSEToJSON(resp, c, body, "gpt-5.4", "gpt-5.4")
				requestErr := requireOpenAIContextRequestError(t, err)
				require.NotNil(t, result)
				require.Equal(t, 37, result.InputTokens)
				require.Equal(t, 37, requestErr.Usage.InputTokens)
			} else {
				result, err := svc.handleSSEToJSON(resp, c, body, "gpt-5.4", "gpt-5.4")
				requestErr := requireOpenAIContextRequestError(t, err)
				require.NotNil(t, result)
				require.Equal(t, 37, result.InputTokens)
				require.Equal(t, 37, requestErr.Usage.InputTokens)
			}
			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Equal(t, "response.failed", gjson.Get(rec.Body.String(), "type").String())
			require.Equal(t, "context_length_exceeded", gjson.Get(rec.Body.String(), "response.error.code").String())
			require.NotContains(t, rec.Body.String(), "sk-private")
		})
	}
}
