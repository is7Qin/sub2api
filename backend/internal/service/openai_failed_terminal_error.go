package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"
)

const openAIUpstreamRequestErrorMessageMaxBytes = 4 * 1024

// OpenAIUpstreamRequestError is a trusted, request-scoped upstream rejection.
// It must not enter account failover or account-health handling.
type OpenAIUpstreamRequestError struct {
	StatusCode    int
	Code          string
	Type          string
	Message       string
	RequestID     string
	Usage         *OpenAIUsage
	OutputStarted bool
}

func (e *OpenAIUpstreamRequestError) Error() string {
	if e == nil {
		return "OpenAI upstream request failed"
	}
	return fmt.Sprintf("OpenAI upstream request failed: %s", e.Message)
}

func (e *OpenAIUpstreamRequestError) ChatErrorBody() []byte {
	if e == nil {
		return nil
	}
	body, _ := json.Marshal(map[string]any{"error": map[string]any{
		"code": e.Code, "type": e.Type, "message": e.Message,
	}})
	return body
}

func (e *OpenAIUpstreamRequestError) AnthropicErrorType() string {
	if e == nil {
		return "invalid_request_error"
	}
	return "invalid_request_error"
}

func (e *OpenAIUpstreamRequestError) observeTerminal(usage OpenAIUsage, outputStarted bool) {
	if e == nil {
		return
	}
	usageCopy := usage
	e.Usage = &usageCopy
	e.OutputStarted = outputStarted
}

func (e *OpenAIUpstreamRequestError) attachUsage(usage OpenAIUsage) {
	e.observeTerminal(usage, false)
}

func openAIHasContextWindowCandidate(payload []byte) bool {
	if len(payload) > openAIErrorClassificationMaxBytes || !utf8.Valid(payload) {
		return false
	}
	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		return false
	}
	return openAIValueHasContextWindowCandidate(root, 1)
}

func openAIValueHasContextWindowCandidate(value any, depth int) bool {
	if depth > openAIErrorClassificationMaxDepth {
		return false
	}
	object, ok := value.(map[string]any)
	if !ok {
		return false
	}
	for key, nested := range object {
		if strings.EqualFold(key, "code") {
			if code, ok := nested.(string); ok && strings.EqualFold(strings.TrimSpace(code), "context_length_exceeded") {
				return true
			}
		}
		if (key == "error" || key == "response") && openAIValueHasContextWindowCandidate(nested, depth+1) {
			return true
		}
	}
	return false
}

func newOpenAIUpstreamRequestError(payload []byte, requestID string) *OpenAIUpstreamRequestError {
	match := classifyOpenAIFailedTerminalContextWindowError(payload)
	if !match.matched() {
		return nil
	}
	message := boundOpenAIUpstreamRequestErrorMessage(match.message)
	if message == "" {
		message = openAIContextWindowClientMessage()
	}
	errType := "invalid_request_error"
	return &OpenAIUpstreamRequestError{
		StatusCode: http.StatusBadRequest,
		Code:       "context_length_exceeded",
		Type:       errType,
		Message:    message,
		RequestID:  strings.TrimSpace(requestID),
	}
}

func boundOpenAIUpstreamRequestErrorMessage(message string) string {
	message = sanitizeOpenAIUpstreamDiagnosticText(strings.TrimSpace(message))
	if len(message) <= openAIUpstreamRequestErrorMessageMaxBytes {
		return message
	}
	message = message[:openAIUpstreamRequestErrorMessageMaxBytes]
	for !utf8.ValidString(message) {
		message = message[:len(message)-1]
	}
	return message
}
