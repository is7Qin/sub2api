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

func TestCandidateRenderersPreserveOpsUpstreamStatusProvenance(t *testing.T) {
	renderers := []struct {
		name   string
		render func(*gin.Context, *service.UpstreamErrorCandidate)
	}{
		{
			name: "gateway_generic",
			render: func(c *gin.Context, candidate *service.UpstreamErrorCandidate) {
				(&GatewayHandler{}).handleUpstreamCandidate(c, candidate, false)
			},
		},
		{
			name: "gateway_chat_completions",
			render: func(c *gin.Context, candidate *service.UpstreamErrorCandidate) {
				(&GatewayHandler{}).handleCCCandidate(c, candidate, false)
			},
		},
		{
			name: "gateway_responses",
			render: func(c *gin.Context, candidate *service.UpstreamErrorCandidate) {
				(&GatewayHandler{}).handleResponsesCandidate(c, candidate, false)
			},
		},
		{
			name: "gateway_gemini",
			render: func(c *gin.Context, candidate *service.UpstreamErrorCandidate) {
				(&GatewayHandler{}).handleGeminiCandidate(c, candidate)
			},
		},
		{
			name: "openai_gateway_responses",
			render: func(c *gin.Context, candidate *service.UpstreamErrorCandidate) {
				(&OpenAIGatewayHandler{}).handleUpstreamCandidate(c, candidate, false)
			},
		},
		{
			name: "openai_gateway_anthropic",
			render: func(c *gin.Context, candidate *service.UpstreamErrorCandidate) {
				(&OpenAIGatewayHandler{}).handleAnthropicCandidate(c, candidate, false)
			},
		},
	}

	facts := []struct {
		name              string
		fact              service.UpstreamErrorFact
		wantUpstream      int
		wantUpstreamKnown bool
	}{
		{
			name: "known",
			fact: service.UpstreamErrorFact{
				Provider:        service.PlatformOpenAI,
				Source:          service.UpstreamErrorSourceHTTP,
				HTTPStatusKnown: true,
				HTTPStatus:      http.StatusServiceUnavailable,
				ProviderCode:    "vendor_failure",
			},
			wantUpstream:      http.StatusServiceUnavailable,
			wantUpstreamKnown: true,
		},
		{
			name: "unknown",
			fact: service.UpstreamErrorFact{
				Provider:        service.PlatformOpenAI,
				Source:          service.UpstreamErrorSourceTransport,
				HTTPStatusKnown: false,
			},
			wantUpstreamKnown: false,
		},
	}

	for _, renderer := range renderers {
		for _, fact := range facts {
			t.Run(renderer.name+"_"+fact.name, func(t *testing.T) {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				if !fact.wantUpstreamKnown {
					c.Set(service.OpsUpstreamStatusCodeKey, http.StatusTeapot)
				}
				candidate := service.NewUpstreamErrorCandidate(fact.fact, service.UpstreamCandidateStatusOnly)

				renderer.render(c, candidate)

				require.Equal(t, http.StatusBadGateway, rec.Code, "client presentation status must not change")
				got, ok := c.Get(service.OpsUpstreamStatusCodeKey)
				if fact.wantUpstreamKnown {
					require.True(t, ok)
					require.Equal(t, fact.wantUpstream, got)
				} else if ok {
					require.Zero(t, got, "unknown final fact must clear stale upstream status")
				}
			})
		}
	}
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
