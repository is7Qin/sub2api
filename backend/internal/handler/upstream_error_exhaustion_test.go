package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIResponsesNilSelectionPrefersPriorRecoveryCandidate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	recovery := NewUpstreamRecoveryState()
	recovery.RetainCandidate(service.NewUpstreamErrorCandidate(service.UpstreamErrorFact{
		Provider:        service.PlatformOpenAI,
		Source:          service.UpstreamErrorSourceHTTP,
		HTTPStatusKnown: true,
		HTTPStatus:      http.StatusTooManyRequests,
		ProviderCode:    "rate_limit_exceeded",
		ProviderType:    "rate_limit_error",
		SafeMessage:     "quota exceeded",
	}, service.UpstreamCandidateStructured))

	(&OpenAIGatewayHandler{}).handleResponsesAccountSelectionExhausted(c, recovery, false)

	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, "rate_limit_exceeded", gjson.GetBytes(rec.Body.Bytes(), "error.code").String())
	require.Equal(t, "quota exceeded", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
}

func TestOpenAIMessagesNilSelectionPrefersPriorRecoveryCandidate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	recovery := NewUpstreamRecoveryState()
	recovery.RetainCandidate(service.NewUpstreamErrorCandidate(service.UpstreamErrorFact{
		Provider:        service.PlatformOpenAI,
		Source:          service.UpstreamErrorSourceHTTP,
		HTTPStatusKnown: true,
		HTTPStatus:      http.StatusTooManyRequests,
		ProviderCode:    "rate_limit_exceeded",
		ProviderType:    "rate_limit_error",
		SafeMessage:     "quota exceeded",
	}, service.UpstreamCandidateStructured))

	(&OpenAIGatewayHandler{}).handleMessagesAccountSelectionExhausted(c, recovery, false)

	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, "rate_limit_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
	require.Equal(t, "quota exceeded", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
}

func TestOpenAINilSelectionWithoutPriorAttemptKeepsNoAvailableAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name    string
		handle  func(*OpenAIGatewayHandler, *gin.Context, *UpstreamRecoveryState)
		message string
	}{
		{
			name: "responses",
			handle: func(h *OpenAIGatewayHandler, c *gin.Context, recovery *UpstreamRecoveryState) {
				h.handleResponsesAccountSelectionExhausted(c, recovery, false)
			},
			message: "No available accounts",
		},
		{
			name: "messages",
			handle: func(h *OpenAIGatewayHandler, c *gin.Context, recovery *UpstreamRecoveryState) {
				h.handleMessagesAccountSelectionExhausted(c, recovery, false)
			},
			message: "No available accounts",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			tt.handle(&OpenAIGatewayHandler{}, c, NewUpstreamRecoveryState())

			require.Equal(t, http.StatusServiceUnavailable, rec.Code)
			require.Equal(t, tt.message, gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
		})
	}
}

func TestOpenAIHandleUpstreamCandidate_Unknown503UsesGeneric502(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	candidate := service.NewUpstreamErrorCandidate(service.UpstreamErrorFact{
		Provider:        service.PlatformOpenAI,
		Source:          service.UpstreamErrorSourceHTTP,
		HTTPStatusKnown: true,
		HTTPStatus:      http.StatusServiceUnavailable,
		ProviderCode:    "vendor_failure",
		SafeMessage:     "private vendor detail",
	}, service.UpstreamCandidateStatusOnly)

	(&OpenAIGatewayHandler{}).handleUpstreamCandidate(c, candidate, false)

	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Equal(t, "Upstream request failed", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
	require.NotContains(t, rec.Body.String(), "private vendor detail")
}

func TestGatewayResponsesCandidatePreservesStructuredRateLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, EndpointResponses, nil)
	candidate := service.NewUpstreamErrorCandidate(service.UpstreamErrorFact{
		Provider:        service.PlatformOpenAI,
		Source:          service.UpstreamErrorSourceHTTP,
		HTTPStatusKnown: true,
		HTTPStatus:      http.StatusTooManyRequests,
		ProviderCode:    "rate_limit_exceeded",
		ProviderType:    "rate_limit_error",
		SafeMessage:     "quota exceeded",
	}, service.UpstreamCandidateStructured)

	(&GatewayHandler{}).handleResponsesCandidate(c, candidate, false)

	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, "rate_limit_exceeded", gjson.GetBytes(rec.Body.Bytes(), "error.code").String())
	require.Equal(t, "quota exceeded", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
}

func TestGatewayChatCandidateAfterCommitEmitsSafeSSEError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	_, _ = c.Writer.WriteString(":\n\n")
	candidate := service.NewUpstreamErrorCandidate(service.UpstreamErrorFact{
		Provider:        service.PlatformOpenAI,
		Source:          service.UpstreamErrorSourceHTTP,
		HTTPStatusKnown: true,
		HTTPStatus:      http.StatusServiceUnavailable,
		ProviderCode:    "vendor_failure",
		SafeMessage:     "private vendor detail",
	}, service.UpstreamCandidateStatusOnly)

	(&GatewayHandler{}).handleCCCandidate(c, candidate, true)

	require.Equal(t, 1, strings.Count(rec.Body.String(), `data: {"type":"error"`))
	require.Contains(t, rec.Body.String(), "Upstream request failed")
	require.NotContains(t, rec.Body.String(), "private vendor detail")
}

func TestGatewayGeminiCandidateUnknown503UsesGeneric502(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	candidate := service.NewUpstreamErrorCandidate(service.UpstreamErrorFact{
		Provider:        service.PlatformGemini,
		Source:          service.UpstreamErrorSourceHTTP,
		HTTPStatusKnown: true,
		HTTPStatus:      http.StatusServiceUnavailable,
		ProviderCode:    "vendor_failure",
		SafeMessage:     "private vendor detail",
	}, service.UpstreamCandidateStatusOnly)

	(&GatewayHandler{}).handleGeminiCandidate(c, candidate)

	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Equal(t, "Upstream request failed", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
	require.NotContains(t, rec.Body.String(), "private vendor detail")
}

func TestOpenAIAnthropicFailoverExhaustedUnknown503UsesSafeFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	failoverErr := &service.UpstreamFailoverError{
		StatusCode:   http.StatusServiceUnavailable,
		ResponseBody: []byte(`{"error":{"type":"vendor_failure","message":"private vendor detail"}}`),
	}

	(&OpenAIGatewayHandler{}).handleAnthropicFailoverExhausted(c, failoverErr, false)

	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Equal(t, "upstream_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
	require.Equal(t, "Upstream request failed", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
	require.NotContains(t, rec.Body.String(), "private vendor detail")
}

func TestOpenAIAnthropicFailoverExhaustedAfterCommitEmitsOneSafeError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	_, _ = c.Writer.WriteString(":\n\n")
	failoverErr := &service.UpstreamFailoverError{
		StatusCode:   http.StatusServiceUnavailable,
		ResponseBody: []byte(`{"error":{"type":"vendor_failure","message":"private vendor detail"}}`),
	}

	(&OpenAIGatewayHandler{}).handleAnthropicFailoverExhausted(c, failoverErr, true)

	require.Equal(t, 1, strings.Count(rec.Body.String(), "event: error\n"))
	require.Equal(t, 1, strings.Count(rec.Body.String(), `"type":"error"`))
	require.Contains(t, rec.Body.String(), "Upstream request failed")
	require.NotContains(t, rec.Body.String(), "private vendor detail")
}

func TestGatewayResponsesCandidateAfterCommitEmitsOneResponseFailed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, EndpointResponses, nil)
	_, _ = c.Writer.WriteString(":\n\n")
	candidate := service.NewUpstreamErrorCandidate(service.UpstreamErrorFact{
		Provider:        service.PlatformOpenAI,
		Source:          service.UpstreamErrorSourceHTTP,
		HTTPStatusKnown: true,
		HTTPStatus:      http.StatusTooManyRequests,
		ProviderCode:    "rate_limit_exceeded",
		ProviderType:    "rate_limit_error",
		SafeMessage:     "quota exceeded",
	}, service.UpstreamCandidateStructured)

	(&GatewayHandler{}).handleResponsesCandidate(c, candidate, true)

	require.Equal(t, 1, strings.Count(rec.Body.String(), "event: response.failed\n"))
	require.Equal(t, 1, strings.Count(rec.Body.String(), `"type":"response.failed"`))
	require.NotContains(t, rec.Body.String(), `data: {"type":"error"`)
}
