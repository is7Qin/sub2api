//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func responseWithAttemptID(t *testing.T, attemptID, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "https://upstream.example/v1/messages", nil)
	req = req.WithContext(context.WithValue(req.Context(), httpAttemptIDKey{}, attemptID))
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"x-request-id": []string{"logical-request"}},
		Request:    req,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func anthropicTerminalSSE() string {
	return strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4.5","stop_reason":"","usage":{"input_tokens":1}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"ok"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
		``,
	}, "\n")
}

func newGatewayCompatTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	return c, rec
}

func TestForwardAsResponsesPreservesAttemptIDForBufferedResult(t *testing.T) {
	c, _ := newGatewayCompatTestContext()
	attemptID := "attempt-responses-buffered"
	resp := responseWithAttemptID(t, attemptID, anthropicTerminalSSE())

	result, err := (&GatewayService{}).handleResponsesBufferedStreamingResponse(
		resp, c, "gpt-5", "claude-sonnet-4.5", nil, time.Now(),
	)

	require.NoError(t, err)
	require.Equal(t, attemptID, result.AttemptID)
}

func TestForwardAsResponsesPreservesAttemptIDForStreamingResult(t *testing.T) {
	c, _ := newGatewayCompatTestContext()
	attemptID := "attempt-responses-streaming"
	resp := responseWithAttemptID(t, attemptID, anthropicTerminalSSE())

	result, err := (&GatewayService{}).handleResponsesStreamingResponse(
		resp, c, "gpt-5", "claude-sonnet-4.5", nil, time.Now(),
	)

	require.NoError(t, err)
	require.Equal(t, attemptID, result.AttemptID)
}

func TestForwardAsChatCompletionsPreservesAttemptIDForBufferedResult(t *testing.T) {
	c, _ := newGatewayCompatTestContext()
	attemptID := "attempt-chat-buffered"
	resp := responseWithAttemptID(t, attemptID, anthropicTerminalSSE())

	result, err := (&GatewayService{}).handleCCBufferedFromAnthropic(
		resp, c, "gpt-5", "claude-sonnet-4.5", nil, time.Now(),
	)

	require.NoError(t, err)
	require.Equal(t, attemptID, result.AttemptID)
}

func TestForwardAsChatCompletionsPreservesAttemptIDForStreamingResult(t *testing.T) {
	c, _ := newGatewayCompatTestContext()
	attemptID := "attempt-chat-streaming"
	resp := responseWithAttemptID(t, attemptID, anthropicTerminalSSE())

	result, err := (&GatewayService{}).handleCCStreamingFromAnthropic(
		resp, c, "gpt-5", "claude-sonnet-4.5", nil, time.Now(), true,
	)

	require.NoError(t, err)
	require.Equal(t, attemptID, result.AttemptID)
}

func TestGeminiChatCompletionsCompatPreservesFinalAttemptIDAfterRetry(t *testing.T) {
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		{
			StatusCode: http.StatusServiceUnavailable,
			Header:     http.Header{"x-request-id": []string{"retry-request"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":"temporarily unavailable"}`)),
		},
		{
			StatusCode: http.StatusOK,
			Header:     http.Header{"x-request-id": []string{"success-request"}},
			Body:       io.NopCloser(strings.NewReader(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1}}`)),
		},
	}}
	svc := &GeminiMessagesCompatService{
		httpUpstream: upstream,
		cfg:          &config.Config{},
	}
	account := &Account{
		ID:          7,
		Type:        AccountTypeAPIKey,
		Platform:    PlatformGemini,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "test-key", "base_url": "https://generativelanguage.googleapis.com"},
	}
	c, _ := newGatewayCompatTestContext()

	result, err := svc.ForwardAsChatCompletions(
		context.Background(), c, account,
		[]byte(`{"model":"gemini-2.0-flash","messages":[{"role":"user","content":"hello"}]}`),
	)

	require.NoError(t, err)
	require.Len(t, upstream.requests, 2)
	require.NotEmpty(t, HTTPAttemptID(upstream.requests[0].Context()))
	require.NotEmpty(t, HTTPAttemptID(upstream.requests[1].Context()))
	require.NotEqual(t, HTTPAttemptID(upstream.requests[0].Context()), HTTPAttemptID(upstream.requests[1].Context()))
	require.Equal(t, HTTPAttemptID(upstream.requests[1].Context()), result.AttemptID)
}
