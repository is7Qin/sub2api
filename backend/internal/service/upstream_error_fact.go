package service

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
)

const (
	upstreamErrorFactMaxBodyBytes      = 64 << 10
	upstreamErrorFactMaxMatchTextBytes = 4 << 10
	upstreamErrorFactMaxScalarBytes    = 512
	upstreamErrorFactMaxRequestIDBytes = 256
)

type UpstreamErrorSource string

const (
	UpstreamErrorSourceHTTP              UpstreamErrorSource = "http"
	UpstreamErrorSourceSSE               UpstreamErrorSource = "sse"
	UpstreamErrorSourceWebSocket         UpstreamErrorSource = "websocket"
	UpstreamErrorSourceTransport         UpstreamErrorSource = "transport"
	UpstreamErrorSourceStreamTermination UpstreamErrorSource = "stream_termination"
)

type UpstreamErrorScope string

const (
	UpstreamErrorScopeRequest  UpstreamErrorScope = "request"
	UpstreamErrorScopeAccount  UpstreamErrorScope = "account"
	UpstreamErrorScopeModel    UpstreamErrorScope = "model"
	UpstreamErrorScopeProvider UpstreamErrorScope = "provider"
	UpstreamErrorScopeUnknown  UpstreamErrorScope = "unknown"
)

type UpstreamErrorFact struct {
	Provider          string
	Source            UpstreamErrorSource
	HTTPStatusKnown   bool
	HTTPStatus        int
	ProviderCode      string
	ProviderType      string
	SafeMessage       string
	RequestID         string
	RetryAfter        string
	ScopeHint         UpstreamErrorScope
	InternalMatchText string
}

var (
	upstreamErrorFactURLPattern        = regexp.MustCompile(`(?i)\b(?:https?|wss?)://[^\s"'<>]+|\b[^\s/@:]+:[^\s/@]+@[^\s/]+`)
	upstreamErrorFactNetworkPattern    = regexp.MustCompile(`(?i)\b(?:[a-z0-9-]+\.)+[a-z]{2,}(?::\d+)?\b|\b(?:\d{1,3}\.){3}\d{1,3}(?::\d+)?\b`)
	upstreamErrorFactCredentialPattern = regexp.MustCompile(`(?i)\b(?:username/password|proxy authentication)\b`)
)

func ParseHTTPUpstreamErrorFact(provider string, resp *http.Response, body []byte) UpstreamErrorFact {
	fact := parseJSONUpstreamErrorFact(provider, UpstreamErrorSourceHTTP, body, "")
	if resp == nil {
		return fact
	}
	fact.HTTPStatusKnown = true
	fact.HTTPStatus = resp.StatusCode
	fact.RequestID = boundedUpstreamErrorFactScalar(firstNonEmptyUpstreamErrorFact(
		resp.Header.Get("x-request-id"),
		resp.Header.Get("request-id"),
		resp.Header.Get("x-goog-request-id"),
	), upstreamErrorFactMaxRequestIDBytes)
	fact.RetryAfter = boundedUpstreamErrorFactScalar(resp.Header.Get("Retry-After"), upstreamErrorFactMaxScalarBytes)
	return fact
}

func ParseGeminiHTTPUpstreamErrorFact(resp *http.Response, body []byte) UpstreamErrorFact {
	fact := ParseHTTPUpstreamErrorFact(PlatformGemini, resp, body)
	if status := sanitizeUpstreamErrorFactScalar(firstJSONScalar(body, "error.status", "response.error.status"), upstreamErrorFactMaxScalarBytes); status != "" {
		fact.ProviderCode = status
		fact.ProviderType = mapGeminiStatusToClaudeErrorType(status)
		fact.InternalMatchText = buildUpstreamErrorFactMatchText(fact)
	}
	return fact
}

func ParseAnthropicSSEErrorFact(provider string, data []byte, requestID string) UpstreamErrorFact {
	return parseJSONUpstreamErrorFact(provider, UpstreamErrorSourceSSE, data, requestID)
}

func ParseOpenAIJSONErrorFact(provider string, source UpstreamErrorSource, payload []byte, requestID string) UpstreamErrorFact {
	return parseJSONUpstreamErrorFact(provider, source, payload, requestID)
}

func ParseOpenAIWebSocketErrorFact(provider string, payload []byte, requestID string) UpstreamErrorFact {
	return parseJSONUpstreamErrorFact(provider, UpstreamErrorSourceWebSocket, payload, requestID)
}

func newOpenAIWebSocketFailoverError(statusCode int, payload []byte, headers http.Header) *UpstreamFailoverError {
	fact := ParseOpenAIWebSocketErrorFact(PlatformOpenAI, payload, headers.Get("x-request-id"))
	return &UpstreamFailoverError{
		StatusCode:      statusCode,
		ResponseBody:    payload,
		ResponseHeaders: headers.Clone(),
		upstreamFact:    &fact,
	}
}

func newOpenAIWebSocketHandshakeFailoverError(resp *http.Response, body []byte) *UpstreamFailoverError {
	fact := ParseHTTPUpstreamErrorFact(PlatformOpenAI, resp, body)
	fact.Source = UpstreamErrorSourceWebSocket
	statusCode := 0
	var headers http.Header
	if resp != nil {
		statusCode = resp.StatusCode
		headers = resp.Header.Clone()
	}
	return &UpstreamFailoverError{
		StatusCode:      statusCode,
		ResponseBody:    body,
		ResponseHeaders: headers,
		upstreamFact:    &fact,
	}
}

func newOpenAIHTTPFailoverError(resp *http.Response, body []byte, retryableOnSameAccount bool) *UpstreamFailoverError {
	fact := ParseHTTPUpstreamErrorFact(PlatformOpenAI, resp, body)
	statusCode := 0
	if resp != nil {
		statusCode = resp.StatusCode
	}
	return &UpstreamFailoverError{
		StatusCode:             statusCode,
		ResponseBody:           body,
		RetryableOnSameAccount: retryableOnSameAccount,
		upstreamFact:           &fact,
	}
}

func ParseTransportErrorFact(provider string, err error) UpstreamErrorFact {
	fact := newUpstreamErrorFact(provider, UpstreamErrorSourceTransport)
	if err == nil {
		return fact
	}
	message := sanitizeUpstreamErrorFactScalar(err.Error(), upstreamErrorFactMaxScalarBytes)
	message = upstreamErrorFactNetworkPattern.ReplaceAllString(message, "[network-redacted]")
	fact.SafeMessage = upstreamErrorFactCredentialPattern.ReplaceAllString(message, "[credential-redacted]")
	fact.InternalMatchText = buildUpstreamErrorFactMatchText(fact)
	return fact
}

// parseJSONUpstreamErrorFact is replaceable in characterization tests so they can
// prove successful streaming frames never reach the error parser.
var parseJSONUpstreamErrorFact = parseJSONUpstreamErrorFactDirect

func parseJSONUpstreamErrorFactDirect(provider string, source UpstreamErrorSource, payload []byte, requestID string) UpstreamErrorFact {
	fact := newUpstreamErrorFact(provider, source)
	fact.RequestID = boundedUpstreamErrorFactScalar(requestID, upstreamErrorFactMaxRequestIDBytes)

	bounded := payload
	if len(bounded) > upstreamErrorFactMaxBodyBytes {
		bounded = bounded[:upstreamErrorFactMaxBodyBytes]
	}
	if !gjson.ValidBytes(bounded) {
		return fact
	}

	fact.ProviderCode = sanitizeUpstreamErrorFactScalar(firstJSONScalar(bounded,
		"response.error.code",
		"error.code",
		"code",
	), upstreamErrorFactMaxScalarBytes)
	fact.ProviderType = sanitizeUpstreamErrorFactScalar(firstJSONScalar(bounded,
		"response.error.type",
		"error.type",
		"type",
	), upstreamErrorFactMaxScalarBytes)
	fact.SafeMessage = sanitizeUpstreamErrorFactScalar(firstJSONScalar(bounded,
		"response.error.message",
		"error.message",
		"message",
	), upstreamErrorFactMaxScalarBytes)
	fact.InternalMatchText = buildUpstreamErrorFactMatchText(fact)
	return fact
}

func newUpstreamErrorFact(provider string, source UpstreamErrorSource) UpstreamErrorFact {
	return UpstreamErrorFact{
		Provider:  boundedUpstreamErrorFactScalar(provider, upstreamErrorFactMaxScalarBytes),
		Source:    source,
		ScopeHint: UpstreamErrorScopeUnknown,
	}
}

func firstJSONScalar(payload []byte, paths ...string) string {
	for _, path := range paths {
		result := gjson.GetBytes(payload, path)
		if !result.Exists() || result.Type != gjson.String {
			continue
		}
		if value := strings.TrimSpace(result.String()); value != "" {
			return value
		}
	}
	return ""
}

func sanitizeUpstreamErrorFactScalar(value string, limit int) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = sanitizeOpenAIUpstreamDiagnosticText(value)
	value = upstreamErrorFactURLPattern.ReplaceAllString(value, "[url-redacted]")
	return boundedUpstreamErrorFactScalar(value, limit)
}

func boundedUpstreamErrorFactScalar(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit > 0 && len(value) > limit {
		value = value[:limit]
	}
	return strings.ToValidUTF8(value, "")
}

func buildUpstreamErrorFactMatchText(fact UpstreamErrorFact) string {
	parts := []string{fact.Provider, fact.ProviderCode, fact.ProviderType, fact.SafeMessage}
	return boundedUpstreamErrorFactScalar(strings.Join(nonEmptyStrings(parts), " "), upstreamErrorFactMaxMatchTextBytes)
}

func nonEmptyStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func firstNonEmptyUpstreamErrorFact(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
