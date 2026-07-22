package handler

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGatewayCompatibilityConcurrencyErrorResponses(t *testing.T) {
	h := &GatewayHandler{}

	t.Run("chat completions keeps OpenAI envelope", func(t *testing.T) {
		c, rec := newHelperTestContext(http.MethodPost, "/v1/chat/completions")

		h.chatCompletionsConcurrencyErrorResponse(c, &WaitQueueFullError{SlotType: "user"}, "user", false)

		require.Equal(t, http.StatusTooManyRequests, rec.Code)
		require.JSONEq(t, `{"error":{"type":"rate_limit_error","message":"Too many pending requests, please retry later"}}`, rec.Body.String())
	})

	t.Run("responses keeps Responses envelope", func(t *testing.T) {
		c, rec := newHelperTestContext(http.MethodPost, "/v1/responses")

		h.responsesConcurrencyErrorResponse(c, &WaitQueueFullError{SlotType: "user"}, "user", false)

		require.Equal(t, http.StatusTooManyRequests, rec.Code)
		require.JSONEq(t, `{"error":{"code":"rate_limit_error","message":"Too many pending requests, please retry later"}}`, rec.Body.String())
	})
}

func TestAPIKeyFullProtocolFamiliesReturnUncommitted429(t *testing.T) {
	gateway := &GatewayHandler{}
	openAI := &OpenAIGatewayHandler{}

	// Every listed handler enters through AcquireClientSlotsWithWait, then uses
	// the family-specific mapper below. Exercising both shared seams proves key
	// rejection precedes user acquisition and remains an ordinary HTTP response.
	tests := []struct {
		name string
		path string
		call func(*gin.Context, error)
	}{
		{"anthropic messages", "/v1/messages", func(c *gin.Context, err error) { gateway.handleConcurrencyError(c, err, "client", false) }},
		{"generic responses", "/v1/responses", func(c *gin.Context, err error) { gateway.responsesConcurrencyErrorResponse(c, err, "client", false) }},
		{"generic chat", "/v1/chat/completions", func(c *gin.Context, err error) {
			gateway.chatCompletionsConcurrencyErrorResponse(c, err, "client", false)
		}},
		{"openai responses", "/v1/responses", func(c *gin.Context, err error) { openAI.handleConcurrencyError(c, err, "client", false) }},
		{"openai messages", "/v1/messages", func(c *gin.Context, err error) { openAI.handleConcurrencyError(c, err, "client", false) }},
		{"openai chat", "/v1/chat/completions", func(c *gin.Context, err error) { openAI.handleConcurrencyError(c, err, "client", false) }},
		{"openai embeddings", "/v1/embeddings", func(c *gin.Context, err error) { openAI.handleConcurrencyError(c, err, "client", false) }},
		{"openai images", "/v1/images/generations", func(c *gin.Context, err error) { openAI.handleConcurrencyError(c, err, "client", false) }},
		{"gemini native", "/v1beta/models/gemini:streamGenerateContent", func(c *gin.Context, err error) { googleError(c, http.StatusTooManyRequests, err.Error()) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var userAcquires int
			cache := &concurrencyCacheMock{
				acquireAPIKeySlotFn: func(context.Context, int64, int, string) (bool, error) { return false, nil },
				acquireUserSlotFn: func(context.Context, int64, int, string) (bool, error) {
					userAcquires++
					return true, nil
				},
			}
			helper := NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatComment, time.Millisecond)
			c, rec := newHelperTestContext(http.MethodPost, tt.path)
			streamStarted := false
			release, err := helper.AcquireClientSlotsWithWait(c, 77, 1, 101, 1, true, &streamStarted)
			require.Nil(t, release)
			require.Zero(t, userAcquires)
			require.False(t, streamStarted)

			tt.call(c, err)
			require.Equal(t, http.StatusTooManyRequests, rec.Code)
			require.NotEqual(t, "text/event-stream", rec.Header().Get("Content-Type"))
			require.Contains(t, rec.Body.String(), "api_key")
		})
	}
}

func TestGeminiConcurrencyErrorResponseSanitizesAdmissionFailures(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantBody   string
		forbidden  string
	}{
		{name: "api key full", err: &ConcurrencyError{SlotType: "api_key"}, wantStatus: http.StatusTooManyRequests, wantBody: "Concurrency limit exceeded for api_key"},
		{name: "redis failure", err: errors.New("redis tcp 10.0.0.7:6379 auth secret"), wantStatus: http.StatusServiceUnavailable, wantBody: "Service temporarily unavailable", forbidden: "10.0.0.7"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, rec := newHelperTestContext(http.MethodPost, "/v1beta/models/gemini:generateContent")
			geminiConcurrencyErrorResponse(c, tt.err, "client")
			require.Equal(t, tt.wantStatus, rec.Code)
			require.Contains(t, rec.Body.String(), tt.wantBody)
			if tt.forbidden != "" {
				require.NotContains(t, rec.Body.String(), tt.forbidden)
			}
		})
	}
}

func TestConcurrencyErrorResponse(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		slotType    string
		wantStatus  int
		wantType    string
		wantMessage string
	}{
		{
			name:        "true concurrency timeout remains rate limit",
			err:         &ConcurrencyError{SlotType: "account", IsTimeout: true},
			slotType:    "user",
			wantStatus:  http.StatusTooManyRequests,
			wantType:    "rate_limit_error",
			wantMessage: "Concurrency limit exceeded for account, please retry later",
		},
		{
			name:        "full wait queue is rate limited",
			err:         &WaitQueueFullError{SlotType: "user"},
			slotType:    "user",
			wantStatus:  http.StatusTooManyRequests,
			wantType:    "rate_limit_error",
			wantMessage: "Too many pending requests, please retry later",
		},
		{
			name:        "client cancellation is not classified as concurrency limit",
			err:         context.Canceled,
			slotType:    "user",
			wantStatus:  statusClientClosedRequest,
			wantType:    "api_error",
			wantMessage: "context canceled",
		},
		{
			name:        "deadline exceeded is service unavailable",
			err:         context.DeadlineExceeded,
			slotType:    "user",
			wantStatus:  http.StatusServiceUnavailable,
			wantType:    "api_error",
			wantMessage: "Service temporarily unavailable, please retry later",
		},
		{
			name:        "redis acquire error is service unavailable",
			err:         errors.New("redis unavailable"),
			slotType:    "user",
			wantStatus:  http.StatusServiceUnavailable,
			wantType:    "api_error",
			wantMessage: "Service temporarily unavailable, please retry later",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, errType, message := concurrencyErrorResponse(tt.err, tt.slotType)
			require.Equal(t, tt.wantStatus, status)
			require.Equal(t, tt.wantType, errType)
			require.Equal(t, tt.wantMessage, message)
		})
	}
}
