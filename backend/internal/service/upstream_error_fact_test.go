package service

import (
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func countUpstreamErrorFactParses(t *testing.T) *atomic.Int64 {
	t.Helper()
	original := parseJSONUpstreamErrorFact
	var calls atomic.Int64
	parseJSONUpstreamErrorFact = func(provider string, source UpstreamErrorSource, payload []byte, requestID string) UpstreamErrorFact {
		calls.Add(1)
		return original(provider, source, payload, requestID)
	}
	t.Cleanup(func() {
		parseJSONUpstreamErrorFact = original
	})
	return &calls
}

func TestParseHTTPUpstreamErrorFactPreservesActualStatus(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": []string{"30"}},
	}

	fact := ParseHTTPUpstreamErrorFact(PlatformOpenAI, resp, []byte(`{
		"error": {
			"code": "rate_limit_exceeded",
			"type": "rate_limit_error",
			"message": "Retry later"
		}
	}`))

	require.Equal(t, UpstreamErrorSourceHTTP, fact.Source)
	require.True(t, fact.HTTPStatusKnown)
	require.Equal(t, http.StatusTooManyRequests, fact.HTTPStatus)
	require.Equal(t, "rate_limit_exceeded", fact.ProviderCode)
	require.Equal(t, "rate_limit_error", fact.ProviderType)
	require.Equal(t, "Retry later", fact.SafeMessage)
	require.Equal(t, "30", fact.RetryAfter)
}

func TestParseGeminiHTTPUpstreamErrorFactExtractsNestedResponseStatus(t *testing.T) {
	body := []byte(`{"response":{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"quota exhausted"}}}`)
	resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}}

	fact := ParseGeminiHTTPUpstreamErrorFact(resp, body)

	require.Equal(t, "RESOURCE_EXHAUSTED", fact.ProviderCode)
	require.Equal(t, "rate_limit_error", fact.ProviderType)
	policy, recognized := ResolveUpstreamRecoveryPolicy(fact)
	require.True(t, recognized)
	require.Equal(t, UpstreamAttemptFailover, policy.Disposition)
	require.Equal(t, 1, policy.AccountTransitionBudget)
}

func TestParseAnthropicSSEErrorFactDoesNotInventHTTPStatus(t *testing.T) {
	fact := ParseAnthropicSSEErrorFact(PlatformAnthropic, []byte(`{
		"type": "error",
		"error": {
			"type": "invalid_request_error",
			"message": "Invalid request"
		}
	}`), "req_123")

	require.Equal(t, UpstreamErrorSourceSSE, fact.Source)
	require.False(t, fact.HTTPStatusKnown)
	require.Zero(t, fact.HTTPStatus)
	require.Equal(t, "invalid_request_error", fact.ProviderType)
	require.Equal(t, "Invalid request", fact.SafeMessage)
	require.Equal(t, "req_123", fact.RequestID)
}

func TestParseOpenAIJSONErrorFactExtractsResponseFailedFields(t *testing.T) {
	fact := ParseOpenAIJSONErrorFact(PlatformOpenAI, UpstreamErrorSourceSSE, []byte(`{
		"type": "response.failed",
		"response": {
			"error": {
				"code": "server_is_overloaded",
				"type": "service_unavailable_error",
				"message": "Please retry later"
			}
		}
	}`), "req_456")

	require.Equal(t, UpstreamErrorSourceSSE, fact.Source)
	require.False(t, fact.HTTPStatusKnown)
	require.Zero(t, fact.HTTPStatus)
	require.Equal(t, "server_is_overloaded", fact.ProviderCode)
	require.Equal(t, "service_unavailable_error", fact.ProviderType)
	require.Equal(t, "Please retry later", fact.SafeMessage)
	require.Equal(t, "req_456", fact.RequestID)
}

func TestParseOpenAIJSONErrorFactMalformedPayloadIsSafeUnknown(t *testing.T) {
	fact := ParseOpenAIJSONErrorFact(PlatformOpenAI, UpstreamErrorSourceSSE, []byte(`{"error":`), "")

	require.Equal(t, UpstreamErrorSourceSSE, fact.Source)
	require.False(t, fact.HTTPStatusKnown)
	require.Equal(t, UpstreamErrorScopeUnknown, fact.ScopeHint)
	require.Empty(t, fact.ProviderCode)
	require.Empty(t, fact.ProviderType)
	require.Empty(t, fact.SafeMessage)
	require.Empty(t, fact.InternalMatchText)
}

func TestParseOpenAIJSONErrorFactBoundsAndRedactsFactFields(t *testing.T) {
	secret := "sk-this-must-not-leak"
	payload := []byte(`{"error":{"code":"invalid_request","type":"invalid_request_error","message":"authorization: Bearer ` + secret + ` session_id=session-secret"}}`)

	fact := ParseOpenAIJSONErrorFact(PlatformOpenAI, UpstreamErrorSourceSSE, payload, "req_123")

	require.NotContains(t, fact.SafeMessage, secret)
	require.NotContains(t, fact.InternalMatchText, secret)
	require.NotContains(t, fact.SafeMessage, "session-secret")
	require.LessOrEqual(t, len(fact.SafeMessage), upstreamErrorFactMaxScalarBytes)
	require.LessOrEqual(t, len(fact.InternalMatchText), upstreamErrorFactMaxMatchTextBytes)

	oversized := []byte(`{"error":{"message":"` + strings.Repeat("x", upstreamErrorFactMaxBodyBytes*2) + `"}}`)
	bounded := ParseOpenAIJSONErrorFact(PlatformOpenAI, UpstreamErrorSourceSSE, oversized, "")
	require.LessOrEqual(t, len(bounded.SafeMessage), upstreamErrorFactMaxScalarBytes)
	require.LessOrEqual(t, len(bounded.InternalMatchText), upstreamErrorFactMaxMatchTextBytes)
}

func TestUpstreamFailoverErrorFactDoesNotOverwriteCompatibilityStatus(t *testing.T) {
	fact := ParseAnthropicSSEErrorFact(PlatformAnthropic, []byte(`{"error":{"type":"invalid_request_error","message":"Invalid request"}}`), "req_123")
	err := &UpstreamFailoverError{StatusCode: http.StatusForbidden, upstreamFact: &fact}

	attached, ok := err.UpstreamFact()
	require.True(t, ok)
	require.Equal(t, http.StatusForbidden, err.StatusCode)
	require.False(t, attached.HTTPStatusKnown)
	require.Zero(t, attached.HTTPStatus)
}

func TestNewAnthropicSSEFailoverErrorKeepsStatusUnknown(t *testing.T) {
	body := []byte(`{"error":{"type":"invalid_request_error","message":"Invalid request"}}`)
	err := newAnthropicSSEFailoverError(&Account{Platform: PlatformAnthropic}, body, "req_123")

	attached, ok := err.UpstreamFact()
	require.True(t, ok)
	require.Zero(t, err.StatusCode)
	require.Equal(t, body, err.ResponseBody)
	require.False(t, attached.HTTPStatusKnown)
	require.Zero(t, attached.HTTPStatus)
	require.Equal(t, UpstreamErrorSourceSSE, attached.Source)
	require.Equal(t, "invalid_request_error", attached.ProviderType)
	require.Equal(t, "Invalid request", attached.SafeMessage)
}

func TestParseOpenAIWebSocketErrorFactHasUnknownHTTPStatus(t *testing.T) {
	payload := []byte(`{"type":"response.failed","response":{"error":{"code":"server_is_overloaded","type":"service_unavailable_error","message":"Retry later"}}}`)

	fact := ParseOpenAIWebSocketErrorFact(PlatformOpenAI, payload, "req_terminal")

	require.Equal(t, UpstreamErrorSourceWebSocket, fact.Source)
	require.False(t, fact.HTTPStatusKnown)
	require.Zero(t, fact.HTTPStatus)
	require.Equal(t, "server_is_overloaded", fact.ProviderCode)
	require.Equal(t, "service_unavailable_error", fact.ProviderType)
	require.Equal(t, "Retry later", fact.SafeMessage)
	require.Equal(t, "req_terminal", fact.RequestID)
}

func TestNewOpenAIWebSocketFailoverErrorPreservesSyntheticStatus(t *testing.T) {
	body := []byte(`{"type":"error","error":{"code":"rate_limit_exceeded","type":"rate_limit_error","message":"Retry later"}}`)

	err := newOpenAIWebSocketFailoverError(
		http.StatusTooManyRequests,
		body,
		http.Header{"X-Request-Id": []string{"req_ws"}},
	)

	require.Equal(t, http.StatusTooManyRequests, err.StatusCode)
	require.Equal(t, body, err.ResponseBody)
	fact, ok := err.UpstreamFact()
	require.True(t, ok)
	require.Equal(t, UpstreamErrorSourceWebSocket, fact.Source)
	require.False(t, fact.HTTPStatusKnown)
	require.Zero(t, fact.HTTPStatus)
	require.Equal(t, "rate_limit_exceeded", fact.ProviderCode)
	require.Equal(t, "rate_limit_error", fact.ProviderType)
	require.Equal(t, "Retry later", fact.SafeMessage)
	require.Equal(t, "req_ws", fact.RequestID)
}

func TestOpenAIForwardResultUpstreamFactReturnsCopy(t *testing.T) {
	fact := ParseOpenAIWebSocketErrorFact(
		PlatformOpenAI,
		[]byte(`{"type":"response.failed","response":{"error":{"code":"server_error","message":"Retry later"}}}`),
		"req_terminal",
	)
	result := &OpenAIForwardResult{upstreamFact: &fact}

	attached, ok := result.UpstreamFact()
	require.True(t, ok)
	require.Equal(t, UpstreamErrorSourceWebSocket, attached.Source)
	require.False(t, attached.HTTPStatusKnown)
	require.Equal(t, "server_error", attached.ProviderCode)
	attached.SafeMessage = "changed"

	again, ok := result.UpstreamFact()
	require.True(t, ok)
	require.Equal(t, "Retry later", again.SafeMessage)
}

func TestOpenAIWSFallbackErrorCarriesWebSocketFact(t *testing.T) {
	payload := []byte(`{"type":"error","error":{"code":"rate_limit_exceeded","type":"rate_limit_error","message":"Retry later"}}`)
	err := newOpenAIWSFallbackErrorWithFact(
		"rate_limit",
		errors.New("Retry later"),
		"wss://example.test/backend-api/codex/responses",
		payload,
		"req_ws",
	)

	fact, ok := err.UpstreamFact()
	require.True(t, ok)
	require.Equal(t, UpstreamErrorSourceWebSocket, fact.Source)
	require.False(t, fact.HTTPStatusKnown)
	require.Zero(t, fact.HTTPStatus)
	require.Equal(t, "rate_limit_exceeded", fact.ProviderCode)
	require.Equal(t, "rate_limit_error", fact.ProviderType)
	require.Equal(t, "Retry later", fact.SafeMessage)
	require.Equal(t, "req_ws", fact.RequestID)
}

func TestNewOpenAIWebSocketHandshakeFailoverErrorPreservesActualStatus(t *testing.T) {
	body := []byte(`{"error":{"code":"rate_limit_exceeded","type":"rate_limit_error","message":"Retry later"}}`)
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"X-Request-Id": []string{"req_handshake"}},
	}

	err := newOpenAIWebSocketHandshakeFailoverError(resp, body)

	require.Equal(t, http.StatusTooManyRequests, err.StatusCode)
	fact, ok := err.UpstreamFact()
	require.True(t, ok)
	require.Equal(t, UpstreamErrorSourceWebSocket, fact.Source)
	require.True(t, fact.HTTPStatusKnown)
	require.Equal(t, http.StatusTooManyRequests, fact.HTTPStatus)
	require.Equal(t, "req_handshake", fact.RequestID)
}

func TestNewOpenAIHTTPFailoverErrorPreservesActualStatusFact(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header: http.Header{
			"X-Request-Id": []string{"req_http"},
			"Retry-After":  []string{"30"},
		},
	}
	body := []byte(`{"error":{"code":"rate_limit_exceeded","type":"rate_limit_error","message":"Retry later"}}`)

	err := newOpenAIHTTPFailoverError(resp, body, true)

	require.Equal(t, http.StatusTooManyRequests, err.StatusCode)
	require.Equal(t, body, err.ResponseBody)
	require.True(t, err.RetryableOnSameAccount)
	fact, ok := err.UpstreamFact()
	require.True(t, ok)
	require.Equal(t, UpstreamErrorSourceHTTP, fact.Source)
	require.True(t, fact.HTTPStatusKnown)
	require.Equal(t, http.StatusTooManyRequests, fact.HTTPStatus)
	require.Equal(t, "rate_limit_exceeded", fact.ProviderCode)
	require.Equal(t, "rate_limit_error", fact.ProviderType)
	require.Equal(t, "Retry later", fact.SafeMessage)
	require.Equal(t, "req_http", fact.RequestID)
	require.Equal(t, "30", fact.RetryAfter)
}

func TestParseTransportErrorFactHasNoHTTPStatus(t *testing.T) {
	fact := ParseTransportErrorFact(PlatformOpenAI, errors.New("dial tcp user:password@example.internal:443: connect: connection refused"))

	require.Equal(t, UpstreamErrorSourceTransport, fact.Source)
	require.False(t, fact.HTTPStatusKnown)
	require.Zero(t, fact.HTTPStatus)
	require.NotContains(t, fact.SafeMessage, "user:password")
	require.NotContains(t, fact.SafeMessage, "example.internal")
}
