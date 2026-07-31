package handler

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpenAIResponsesPassthroughReasoningFailoverState(t *testing.T) {
	canonical := []byte(`{"model":"gpt-5","input":[{"type":"reasoning","encrypted_content":"foreign"},{"type":"message","content":"hello"}]}`)
	passthrough := &service.Account{Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Extra: map[string]any{"openai_passthrough": true}}
	nonPassthrough := &service.Account{Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth}

	state := newOpenAIResponsesReasoningFailoverState(canonical, true)
	body, err := state.bodyForAttempt(nonPassthrough)
	require.NoError(t, err)
	require.Equal(t, canonical, body)

	state.recordAttempt(passthrough)
	body, err = state.bodyForAttempt(passthrough)
	require.NoError(t, err)
	require.Equal(t, canonical, body)

	body, err = state.bodyForAttempt(nonPassthrough)
	require.NoError(t, err)
	require.NotEqual(t, canonical, body)
	require.NotContains(t, string(body), "encrypted_content")
	require.Contains(t, string(body), `"type":"message"`)
	require.Equal(t, canonical, state.canonicalBody)

	bodyAgain, err := state.bodyForAttempt(nonPassthrough)
	require.NoError(t, err)
	require.Equal(t, body, bodyAgain)

	other := newOpenAIResponsesReasoningFailoverState(canonical, true)
	body, err = other.bodyForAttempt(nonPassthrough)
	require.NoError(t, err)
	require.Equal(t, canonical, body)

	state.recordAttempt(nonPassthrough)
	body, err = state.bodyForAttempt(passthrough)
	require.NoError(t, err)
	require.Equal(t, canonical, body, "later passthrough attempts retain canonical bytes")
}

func TestOpenAIResponsesReasoningFailoverStateDisabled(t *testing.T) {
	canonical := []byte(`{"model":"gpt-5","input":[{"type":"reasoning","encrypted_content":"foreign"}]}`)
	passthrough := &service.Account{Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Extra: map[string]any{"openai_passthrough": true}}
	nonPassthrough := &service.Account{Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth}

	state := newOpenAIResponsesReasoningFailoverState(canonical, false)
	state.recordAttempt(passthrough)
	body, err := state.bodyForAttempt(nonPassthrough)

	require.NoError(t, err)
	require.Equal(t, canonical, body, "excluded request classes must retain their canonical body")
}
