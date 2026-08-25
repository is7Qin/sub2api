package apicompat

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResponsesToChatCompletionsFinishReasonWireSemantics(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		want   string
	}{
		{name: "content filter", reason: "content_filter", want: "content_filter"},
		{name: "token limit", reason: "max_output_tokens", want: "length"},
		{name: "unknown incomplete reason", reason: "other", want: "stop"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := ResponsesToChatCompletions(&ResponsesResponse{
				ID:                "resp_test",
				Status:            "incomplete",
				IncompleteDetails: &ResponsesIncompleteDetails{Reason: tt.reason},
			}, "claude-test")

			wire, err := json.Marshal(resp)
			require.NoError(t, err)
			require.JSONEq(t, `{
				"id":"resp_test",
				"object":"chat.completion",
				"created":`+jsonInt64(resp.Created)+`,
				"model":"claude-test",
				"choices":[{"index":0,"message":{"role":"assistant"},"finish_reason":"`+tt.want+`"}]
			}`, string(wire))
			require.NotContains(t, string(wire), `"error"`)
		})
	}
}

func TestAnthropicStreamingTerminalWireSemantics(t *testing.T) {
	tests := []struct {
		name       string
		stopReason string
		wantType   string
		wantBody   string
	}{
		{
			name:       "max tokens",
			stopReason: "max_tokens",
			wantType:   "response.incomplete",
			wantBody:   `{"id":"msg_test","object":"response","model":"claude-test","status":"incomplete","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0},"incomplete_details":{"reason":"max_output_tokens"}}`,
		},
		{
			name:       "end turn",
			stopReason: "end_turn",
			wantType:   "response.completed",
			wantBody:   `{"id":"msg_test","object":"response","model":"claude-test","status":"completed","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := NewAnthropicEventToResponsesState()
			AnthropicEventToResponsesEvents(&AnthropicStreamEvent{
				Type:    "message_start",
				Message: &AnthropicResponse{ID: "msg_test", Model: "claude-test", Role: "assistant"},
			}, state)
			AnthropicEventToResponsesEvents(&AnthropicStreamEvent{
				Type:  "message_delta",
				Delta: &AnthropicDelta{StopReason: tt.stopReason},
			}, state)

			events := AnthropicEventToResponsesEvents(&AnthropicStreamEvent{Type: "message_stop"}, state)
			require.Len(t, events, 1)
			require.Equal(t, tt.wantType, events[0].Type)

			wire, err := json.Marshal(events[0].Response)
			require.NoError(t, err)
			require.JSONEq(t, tt.wantBody, string(wire))
			require.Empty(t, FinalizeAnthropicResponsesStream(state), "well-formed terminal must not be finalized again")
		})
	}
}

func TestFinalizeAnthropicResponsesStreamTerminalSemantics(t *testing.T) {
	tests := []struct {
		name       string
		stopReason string
		wantType   string
		wantStatus string
		wantReason string
	}{
		{name: "max tokens", stopReason: "max_tokens", wantType: "response.incomplete", wantStatus: "incomplete", wantReason: "max_output_tokens"},
		{name: "end turn", stopReason: "end_turn", wantType: "response.completed", wantStatus: "completed"},
		{name: "unknown", stopReason: "future_reason", wantType: "response.completed", wantStatus: "completed"},
		{name: "missing", wantType: "response.completed", wantStatus: "completed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := NewAnthropicEventToResponsesState()
			AnthropicEventToResponsesEvents(&AnthropicStreamEvent{
				Type: "message_start",
				Message: &AnthropicResponse{
					ID:    "msg_test",
					Model: "claude-test",
					Usage: AnthropicUsage{InputTokens: 7, CacheReadInputTokens: 3},
				},
			}, state)
			AnthropicEventToResponsesEvents(&AnthropicStreamEvent{
				Type:  "message_delta",
				Delta: &AnthropicDelta{StopReason: tt.stopReason},
				Usage: &AnthropicUsage{OutputTokens: 11},
			}, state)

			events := FinalizeAnthropicResponsesStream(state)
			require.Len(t, events, 1)
			terminal := events[0]
			require.Equal(t, tt.wantType, terminal.Type)
			require.NotNil(t, terminal.Response)
			require.Equal(t, tt.wantStatus, terminal.Response.Status)
			require.Equal(t, tt.wantReason, incompleteReason(terminal.Response.IncompleteDetails))
			buffered := AnthropicToResponsesResponse(&AnthropicResponse{StopReason: tt.stopReason})
			require.Equal(t, buffered.Status, terminal.Response.Status, "synthetic and buffered status must agree")
			require.Equal(t, incompleteReason(buffered.IncompleteDetails), incompleteReason(terminal.Response.IncompleteDetails), "synthetic and buffered details must agree")
			require.Equal(t, &ResponsesUsage{
				InputTokens:        10,
				OutputTokens:       11,
				TotalTokens:        21,
				InputTokensDetails: &ResponsesInputTokensDetails{CachedTokens: 3},
			}, terminal.Response.Usage)
			require.Empty(t, FinalizeAnthropicResponsesStream(state), "synthetic terminal must be emitted once")
			require.Empty(t, AnthropicEventToResponsesEvents(&AnthropicStreamEvent{Type: "message_stop"}, state), "late message_stop must not duplicate the terminal")
		})
	}
}

func TestFinalizeAnthropicResponsesStreamChainsMaxTokensToChatLength(t *testing.T) {
	anthropicState := NewAnthropicEventToResponsesState()
	chatState := NewResponsesEventToChatState()
	chatState.IncludeUsage = true

	startEvents := AnthropicEventToResponsesEvents(&AnthropicStreamEvent{
		Type: "message_start",
		Message: &AnthropicResponse{
			ID:    "msg_test",
			Model: "claude-test",
			Usage: AnthropicUsage{InputTokens: 5},
		},
	}, anthropicState)
	for i := range startEvents {
		ResponsesEventToChatChunks(&startEvents[i], chatState)
	}
	AnthropicEventToResponsesEvents(&AnthropicStreamEvent{
		Type:  "message_delta",
		Delta: &AnthropicDelta{StopReason: "max_tokens"},
		Usage: &AnthropicUsage{OutputTokens: 8},
	}, anthropicState)

	terminalEvents := FinalizeAnthropicResponsesStream(anthropicState)
	require.Len(t, terminalEvents, 1)
	chunks := ResponsesEventToChatChunks(&terminalEvents[0], chatState)
	require.Len(t, chunks, 2)
	require.Len(t, chunks[0].Choices, 1)
	require.NotNil(t, chunks[0].Choices[0].FinishReason)
	require.Equal(t, "length", *chunks[0].Choices[0].FinishReason)
	require.Empty(t, chunks[1].Choices)
	require.Equal(t, &ChatUsage{PromptTokens: 5, CompletionTokens: 8, TotalTokens: 13}, chunks[1].Usage)
	require.Empty(t, FinalizeResponsesChatStream(chatState), "chained finalizer must not emit a second terminal")
}

func TestResponsesEventToChatChunksResponseFailedDoesNotSynthesizeStop(t *testing.T) {
	state := NewResponsesEventToChatState()
	state.ID = "resp_failed"
	state.Model = "gpt-5.5"

	chunks := ResponsesEventToChatChunks(&ResponsesStreamEvent{
		Type: "response.failed",
		Response: &ResponsesResponse{
			Status: "failed",
			Error:  &ResponsesError{Code: "context_length_exceeded", Message: "context window"},
		},
	}, state)

	require.Empty(t, chunks)
	require.False(t, state.Finalized, "the service error boundary owns failed-terminal rendering")
}

func TestResponsesEventToChatChunksContentFilterWireSemantics(t *testing.T) {
	state := NewResponsesEventToChatState()
	state.ID = "resp_test"
	state.Model = "claude-test"
	state.SentRole = true

	chunks := ResponsesEventToChatChunks(&ResponsesStreamEvent{
		Type: "response.incomplete",
		Response: &ResponsesResponse{
			Status:            "incomplete",
			IncompleteDetails: &ResponsesIncompleteDetails{Reason: "content_filter"},
		},
	}, state)
	require.Len(t, chunks, 1)

	wire, err := json.Marshal(chunks[0])
	require.NoError(t, err)
	require.JSONEq(t, `{
		"id":"resp_test",
		"object":"chat.completion.chunk",
		"created":`+jsonInt64(chunks[0].Created)+`,
		"model":"claude-test",
		"choices":[{"index":0,"delta":{"content":""},"finish_reason":"content_filter"}]
	}`, string(wire))
	require.NotContains(t, string(wire), `"error"`)
	require.Empty(t, FinalizeResponsesChatStream(state))
}

func jsonInt64(value int64) string {
	return strconv.FormatInt(value, 10)
}

func incompleteReason(details *ResponsesIncompleteDetails) string {
	if details == nil {
		return ""
	}
	return details.Reason
}
