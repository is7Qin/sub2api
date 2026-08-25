package handler

import "github.com/Wei-Shaw/sub2api/internal/service"

// openAIResponsesReasoningFailoverState keeps capability-specific request state
// inside one HTTP Responses failover loop. Its canonical body is never modified.
type openAIResponsesReasoningFailoverState struct {
	canonicalBody   []byte
	passthroughSeen bool
	enabled         bool
}

func newOpenAIResponsesReasoningFailoverState(canonicalBody []byte, enabled bool) *openAIResponsesReasoningFailoverState {
	return &openAIResponsesReasoningFailoverState{
		canonicalBody: canonicalBody,
		enabled:       enabled,
	}
}

func (s *openAIResponsesReasoningFailoverState) bodyForAttempt(account *service.Account) ([]byte, error) {
	if s == nil || !s.enabled || !s.passthroughSeen || account == nil || account.IsOpenAIPassthroughEnabled() {
		if s == nil {
			return nil, nil
		}
		return s.canonicalBody, nil
	}
	sanitized, _, err := service.SanitizeOpenAICrossModeFailoverReasoning(s.canonicalBody)
	if err != nil {
		return nil, err
	}
	return sanitized, nil
}

func (s *openAIResponsesReasoningFailoverState) recordAttempt(account *service.Account) {
	if s != nil && s.enabled && account != nil && account.IsOpenAIPassthroughEnabled() {
		s.passthroughSeen = true
	}
}
