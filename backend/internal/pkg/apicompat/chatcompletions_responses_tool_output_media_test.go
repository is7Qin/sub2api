package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResponsesInputToChatMessages_MovesToolOutputImageToUserMessage(t *testing.T) {
	messages, err := responsesInputToChatMessages("", json.RawMessage(`[
		{"type":"function_call","call_id":"call_image","name":"view_image","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_image","output":[{"type":"input_text","text":"render complete"},{"type":"input_image","image_url":"data:image/png;base64,AQID"}]}
	]`))

	require.NoError(t, err)
	require.Len(t, messages, 3)
	require.Equal(t, "assistant", messages[0].Role)
	require.Equal(t, "tool", messages[1].Role)
	require.Equal(t, "call_image", messages[1].ToolCallID)
	require.Equal(t, "user", messages[2].Role)

	var toolOutput string
	require.NoError(t, json.Unmarshal(messages[1].Content, &toolOutput))
	require.Contains(t, toolOutput, "render complete")
	require.Contains(t, toolOutput, "[Tool output media moved to the following user message]")
	require.NotContains(t, toolOutput, "data:image/")

	var parts []ChatContentPart
	require.NoError(t, json.Unmarshal(messages[2].Content, &parts))
	require.Len(t, parts, 2)
	require.Equal(t, "[Tool output media for call call_image]", parts[0].Text)
	require.Equal(t, "image_url", parts[1].Type)
	require.NotNil(t, parts[1].ImageURL)
	require.Equal(t, "data:image/png;base64,AQID", parts[1].ImageURL.URL)
}

func TestResponsesInputToChatMessages_PreservesMediaFreeToolOutput(t *testing.T) {
	output := `{ "type": "result", "text": "complete" }`
	messages, err := responsesInputToChatMessages("", json.RawMessage(`[
		{"type":"function_call","call_id":"call_text","name":"exec","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_text","output":`+output+`}
	]`))

	require.NoError(t, err)
	require.Len(t, messages, 2)
	require.Equal(t, "tool", messages[1].Role)
	var toolOutput string
	require.NoError(t, json.Unmarshal(messages[1].Content, &toolOutput))
	require.Equal(t, output, toolOutput)
}
