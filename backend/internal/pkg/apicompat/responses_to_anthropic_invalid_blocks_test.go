package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func responsesToAnthropicMessages(t *testing.T, input string) []AnthropicMessage {
	t.Helper()
	var req ResponsesRequest
	require.NoError(t, json.Unmarshal([]byte(`{"model":"glm-5.2","input":`+input+`}`), &req))
	out, err := ResponsesToAnthropicRequest(&req)
	require.NoError(t, err)
	return out.Messages
}

func TestResponsesToAnthropicDropsReasoningContentBlocks(t *testing.T) {
	messages := responsesToAnthropicMessages(t, `[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"run command"}]},
		{"type":"reasoning","id":"rs_1","content":[{"type":"reasoning_text","text":"private thought"}]}
	]`)

	require.Len(t, messages, 1)
	require.NotContains(t, string(messages[0].Content), "reasoning_text")
	require.NotContains(t, string(messages[0].Content), "private thought")
}

func TestResponsesToAnthropicSanitizesUnknownItemContent(t *testing.T) {
	messages := responsesToAnthropicMessages(t, `[
		{"type":"some_future_item","content":[
			{"type":"input_text","text":"keep me"},
			{"type":"reasoning_text","text":"drop me"}
		]}
	]`)

	require.Len(t, messages, 1)
	require.Contains(t, string(messages[0].Content), "keep me")
	require.NotContains(t, string(messages[0].Content), "reasoning_text")
	require.NotContains(t, string(messages[0].Content), "drop me")
}

func TestResponsesToAnthropicDropsMessagesWithOnlyUnknownParts(t *testing.T) {
	t.Run("user", func(t *testing.T) {
		messages := responsesToAnthropicMessages(t, `[
			{"type":"message","role":"user","content":[{"type":"input_file","file_id":"file_1"}]}
		]`)
		require.Empty(t, messages)
	})

	t.Run("assistant", func(t *testing.T) {
		messages := responsesToAnthropicMessages(t, `[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"no"}]}
		]`)
		require.Len(t, messages, 1)
		require.Equal(t, "user", messages[0].Role)
	})
}
