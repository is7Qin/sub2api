package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestRecognizeUpstreamErrorFact_CyberPolicyReturnsDirect400(t *testing.T) {
	fact := UpstreamErrorFact{
		Provider:     PlatformOpenAI,
		Source:       UpstreamErrorSourceSSE,
		ProviderCode: "cyber_policy",
		ProviderType: "invalid_request_error",
		SafeMessage:  "This request was rejected by policy",
	}

	policy, ok := RecognizeUpstreamErrorFact(fact)

	require.True(t, ok)
	require.Equal(t, UpstreamAttemptDirectReturn, policy.Disposition)
	require.Equal(t, http.StatusBadRequest, policy.Presentation.HTTPStatus)
	require.Equal(t, "cyber_policy", policy.Presentation.ErrorCode)
	require.Equal(t, "invalid_request_error", policy.Presentation.ErrorType)
	require.Equal(t, "This request was rejected by policy", policy.Presentation.Message)
}

func TestRecognizeUpstreamErrorFact_ContextLengthReturnsExistingDirect400(t *testing.T) {
	fact := UpstreamErrorFact{
		Provider:     PlatformOpenAI,
		Source:       UpstreamErrorSourceHTTP,
		ProviderCode: "context_length_exceeded",
		ProviderType: "invalid_request_error",
		SafeMessage:  "Maximum context length exceeded",
	}

	policy, ok := RecognizeUpstreamErrorFact(fact)

	require.True(t, ok)
	require.Equal(t, UpstreamAttemptDirectReturn, policy.Disposition)
	require.Equal(t, http.StatusBadRequest, policy.Presentation.HTTPStatus)
	require.Equal(t, "context_length_exceeded", policy.Presentation.ErrorCode)
	require.Equal(t, "invalid_request_error", policy.Presentation.ErrorType)
	require.Equal(t, "Maximum context length exceeded", policy.Presentation.Message)
}

func TestRecognizeUpstreamErrorFact_RecognizedPresentationIsBoundedAndRedacted(t *testing.T) {
	fact := UpstreamErrorFact{
		ProviderCode: "server_is_overloaded",
		ProviderType: "service_unavailable_error",
		SafeMessage:  "Bearer sk-secret-should-not-be-shown https://internal.example/path " + string(make([]byte, upstreamErrorFactMaxScalarBytes)),
	}

	policy, ok := RecognizeUpstreamErrorFact(fact)

	require.True(t, ok)
	require.LessOrEqual(t, len(policy.Presentation.Message), upstreamErrorFactMaxScalarBytes)
	require.NotContains(t, policy.Presentation.Message, "sk-secret-should-not-be-shown")
	require.NotContains(t, policy.Presentation.Message, "internal.example")
}

func TestWriteRecognizedOpenAIHTTPErrorUsesSafePresentation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	presentation := UpstreamClientPresentation{
		HTTPStatus: http.StatusServiceUnavailable,
		ErrorCode:  "server_is_overloaded",
		ErrorType:  "service_unavailable_error",
		Message:    "Please retry later",
	}

	writeRecognizedOpenAIHTTPError(c, presentation)

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.JSONEq(t, `{"error":{"code":"server_is_overloaded","type":"service_unavailable_error","message":"Please retry later"}}`, recorder.Body.String())
}

func TestRecognizeUpstreamErrorFact_UsesFixedMessageWhenRecognizedFactHasNoSafeMessage(t *testing.T) {
	policy, ok := RecognizeUpstreamErrorFact(UpstreamErrorFact{ProviderCode: "server_is_overloaded"})

	require.True(t, ok)
	require.Equal(t, "The upstream service is temporarily overloaded", policy.Presentation.Message)
}

func TestRecognizeUpstreamErrorFact_UnknownIsNotRecognized(t *testing.T) {
	policy, ok := RecognizeUpstreamErrorFact(UpstreamErrorFact{ProviderCode: "unrecognized_failure"})

	require.False(t, ok)
	require.Equal(t, RecognizedUpstreamErrorPolicy{}, policy)
}

func TestRecognizeUpstreamErrorFact_AnthropicOverloadedSSEReturnsDirect503(t *testing.T) {
	fact := UpstreamErrorFact{
		Provider:     PlatformAnthropic,
		Source:       UpstreamErrorSourceSSE,
		ProviderType: "overloaded_error",
		SafeMessage:  "The upstream service is temporarily overloaded",
	}

	policy, ok := RecognizeUpstreamErrorFact(fact)

	require.True(t, ok)
	require.Equal(t, UpstreamAttemptDirectReturn, policy.Disposition)
	require.Equal(t, http.StatusServiceUnavailable, policy.Presentation.HTTPStatus)
	require.Equal(t, "overloaded_error", policy.Presentation.ErrorType)
	require.Equal(t, "The upstream service is temporarily overloaded", policy.Presentation.Message)
}

func TestRecognizeUpstreamErrorFact_AnthropicInvalidRequestReturnsDirect400(t *testing.T) {
	fact := UpstreamErrorFact{
		Provider:     PlatformAnthropic,
		Source:       UpstreamErrorSourceSSE,
		ProviderType: "invalid_request_error",
		SafeMessage:  "max_tokens must be positive",
	}

	policy, ok := RecognizeUpstreamErrorFact(fact)

	require.True(t, ok)
	require.Equal(t, UpstreamAttemptDirectReturn, policy.Disposition)
	require.Equal(t, http.StatusBadRequest, policy.Presentation.HTTPStatus)
	require.Equal(t, "invalid_request_error", policy.Presentation.ErrorType)
	require.Equal(t, "max_tokens must be positive", policy.Presentation.Message)
}

func TestRecognizeUpstreamErrorFact_ServerIsOverloadedReturnsDirect503(t *testing.T) {
	fact := UpstreamErrorFact{
		Provider:     PlatformOpenAI,
		Source:       UpstreamErrorSourceSSE,
		ProviderCode: "server_is_overloaded",
		ProviderType: "service_unavailable_error",
		SafeMessage:  "Please retry later",
		ScopeHint:    UpstreamErrorScopeUnknown,
	}

	policy, ok := RecognizeUpstreamErrorFact(fact)

	require.True(t, ok)
	require.Equal(t, UpstreamAttemptDirectReturn, policy.Disposition)
	require.Equal(t, http.StatusServiceUnavailable, policy.Presentation.HTTPStatus)
	require.Equal(t, "server_is_overloaded", policy.Presentation.ErrorCode)
	require.Equal(t, "service_unavailable_error", policy.Presentation.ErrorType)
	require.Equal(t, "Please retry later", policy.Presentation.Message)
}

func TestHandleChatCompletionsErrorResponse_RecognizedErrorIncludesCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	resp := &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Header:     http.Header{"x-request-id": []string{"rid-overload"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"server_is_overloaded","type":"response.failed","message":"Please retry later"}}`)),
	}

	_, err := (&OpenAIGatewayService{}).handleChatCompletionsErrorResponse(resp, c, &Account{Platform: PlatformOpenAI})

	require.Error(t, err)
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.Equal(t, "server_is_overloaded", gjson.GetBytes(recorder.Body.Bytes(), "error.code").String())
	require.Equal(t, "service_unavailable_error", gjson.GetBytes(recorder.Body.Bytes(), "error.type").String())
}

func TestRecognizedCompatErrorCarriesCodeAndRequestID(t *testing.T) {
	policy := RecognizedUpstreamErrorPolicy{
		Disposition: UpstreamAttemptDirectReturn,
		Presentation: UpstreamClientPresentation{
			HTTPStatus: http.StatusServiceUnavailable,
			ErrorCode:  "server_is_overloaded",
			ErrorType:  "service_unavailable_error",
			Message:    "Please retry later",
		},
	}

	requestErr := newRecognizedOpenAIUpstreamRequestErrorForPolicy(policy, "rid-overload")

	require.Equal(t, http.StatusServiceUnavailable, requestErr.StatusCode)
	require.Equal(t, "server_is_overloaded", requestErr.Code)
	require.Equal(t, "service_unavailable_error", requestErr.Type)
	require.Equal(t, "Please retry later", requestErr.Message)
	require.Equal(t, "rid-overload", requestErr.RequestID)
}

func TestRecognizeUpstreamErrorFact_ServerOverloadNormalizesTerminalEventType(t *testing.T) {
	fact := UpstreamErrorFact{
		Provider:     PlatformOpenAI,
		Source:       UpstreamErrorSourceSSE,
		ProviderCode: "server_is_overloaded",
		ProviderType: "response.failed",
	}

	policy, ok := RecognizeUpstreamErrorFact(fact)

	require.True(t, ok)
	require.Equal(t, "service_unavailable_error", policy.Presentation.ErrorType)
}

func TestResolveUpstreamRecoveryPolicy_ServerOverloadIsDirectAndNonRecoverable(t *testing.T) {
	policy, ok := ResolveUpstreamRecoveryPolicy(UpstreamErrorFact{
		Provider:     PlatformOpenAI,
		ProviderCode: "server_is_overloaded",
		ProviderType: "response.failed",
	})

	require.True(t, ok)
	require.Equal(t, UpstreamAttemptDirectReturn, policy.Disposition)
	require.Zero(t, policy.SameAccountRetryBudget)
	require.Zero(t, policy.AccountTransitionBudget)
	require.Equal(t, UpstreamHealthNone, policy.AccountHealthAction)
	require.Equal(t, UpstreamCandidateStructured, policy.CandidateRank)
	require.Equal(t, http.StatusServiceUnavailable, policy.Presentation.HTTPStatus)
}

func TestResolveUpstreamRecoveryPolicy_AccountQuotaAllowsOneTransition(t *testing.T) {
	policy, ok := ResolveUpstreamRecoveryPolicy(UpstreamErrorFact{
		Provider:        PlatformOpenAI,
		HTTPStatus:      http.StatusTooManyRequests,
		HTTPStatusKnown: true,
		ProviderCode:    "rate_limit_exceeded",
		ProviderType:    "rate_limit_error",
		SafeMessage:     "quota exceeded",
	})

	require.True(t, ok)
	require.Equal(t, UpstreamAttemptFailover, policy.Disposition)
	require.LessOrEqual(t, policy.SameAccountRetryBudget, 1)
	require.Equal(t, 1, policy.AccountTransitionBudget)
	require.Equal(t, UpstreamHealthApplyRateLimit, policy.AccountHealthAction)
	require.Equal(t, UpstreamCandidateStructured, policy.CandidateRank)
}

func TestResolveUpstreamRecoveryPolicy_RateLimitKeepsStructuredSemanticsWith5xxStatus(t *testing.T) {
	policy, ok := ResolveUpstreamRecoveryPolicy(UpstreamErrorFact{
		HTTPStatusKnown: true,
		HTTPStatus:      http.StatusServiceUnavailable,
		ProviderCode:    "rate_limit_exceeded",
		ProviderType:    "rate_limit_error",
		SafeMessage:     "quota exceeded",
	})

	require.True(t, ok)
	require.Equal(t, UpstreamHealthApplyRateLimit, policy.AccountHealthAction)
	require.Equal(t, UpstreamCandidateStructured, policy.CandidateRank)
	require.Equal(t, 1, policy.AccountTransitionBudget)
}

func TestResolveUpstreamRecoveryPolicy_GenericProvider5xxAllowsOneTransition(t *testing.T) {
	policy, ok := ResolveUpstreamRecoveryPolicy(UpstreamErrorFact{
		Provider:        PlatformOpenAI,
		HTTPStatusKnown: true,
		HTTPStatus:      http.StatusBadGateway,
	})

	require.True(t, ok)
	require.Equal(t, UpstreamAttemptFailover, policy.Disposition)
	require.Zero(t, policy.SameAccountRetryBudget)
	require.Equal(t, 1, policy.AccountTransitionBudget)
	require.Equal(t, UpstreamHealthRecordOnly, policy.AccountHealthAction)
	require.Equal(t, UpstreamCandidateStatusOnly, policy.CandidateRank)
}

func TestUpstreamErrorCandidatePrecedenceAndSafety(t *testing.T) {
	structured := NewUpstreamErrorCandidate(UpstreamErrorFact{
		ProviderCode: "rate_limit_exceeded",
		ProviderType: "rate_limit_error",
		SafeMessage:  "quota exceeded",
	}, UpstreamCandidateStructured)
	transport := NewUpstreamErrorCandidate(UpstreamErrorFact{
		Source:      UpstreamErrorSourceTransport,
		SafeMessage: "proxy failure",
	}, UpstreamCandidateGenericTransport)

	require.Equal(t, UpstreamCandidateStructured, structured.Rank)
	require.Equal(t, "rate_limit_exceeded", structured.Presentation.ErrorCode)
	require.NotContains(t, structured.Presentation.Message, "Authorization")
	require.Equal(t, UpstreamCandidateGenericTransport, transport.Rank)
}
