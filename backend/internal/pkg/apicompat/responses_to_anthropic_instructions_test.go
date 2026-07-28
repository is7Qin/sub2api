package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func anthropicSystemTexts(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var blocks []AnthropicContentBlock
	require.NoError(t, json.Unmarshal(raw, &blocks))
	texts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		texts = append(texts, block.Text)
	}
	return texts
}

func TestResponsesToAnthropic_InstructionsWithStringInput(t *testing.T) {
	out, err := ResponsesToAnthropicRequest(&ResponsesRequest{Model: "claude", Instructions: "  keep exact  ", Input: json.RawMessage(`"hello"`)})
	require.NoError(t, err)
	require.Equal(t, []string{"  keep exact  "}, anthropicSystemTexts(t, out.System))
	require.Len(t, out.Messages, 1)
}

func TestResponsesToAnthropic_DirectiveOrderAndBlankOmission(t *testing.T) {
	out, err := ResponsesToAnthropicRequest(&ResponsesRequest{
		Model: "claude", Instructions: "first",
		Input: json.RawMessage(`[
			{"role":"system","content":"system one"},
			{"role":"user","content":"hello"},
			{"role":"developer","content":[{"type":"input_text","text":"developer two"}]},
			{"role":"system","content":"  "},
			{"role":"developer","content":"developer three"}
		]`),
	})
	require.NoError(t, err)
	require.Equal(t, []string{"first", "system one", "developer two", "developer three"}, anthropicSystemTexts(t, out.System))
	require.Len(t, out.Messages, 1)

	blank, err := ResponsesToAnthropicRequest(&ResponsesRequest{Model: "claude", Instructions: " \n ", Input: json.RawMessage(`[{"role":"developer","content":"\t"},{"role":"user","content":"hi"}]`)})
	require.NoError(t, err)
	require.Empty(t, blank.System)
}

func TestResponsesToAnthropic_DeveloperDirectivePreservesToolPair(t *testing.T) {
	out, err := ResponsesToAnthropicRequest(&ResponsesRequest{
		Model: "claude",
		Input: json.RawMessage(`[
			{"type":"function_call","call_id":"call_A","name":"Read","arguments":"{}"},
			{"role":"developer","content":"approval"},
			{"type":"function_call_output","call_id":"call_A","output":"ok"}
		]`),
	})
	require.NoError(t, err)
	require.Equal(t, []string{"approval"}, anthropicSystemTexts(t, out.System))
	require.Len(t, out.Messages, 2)
	assertAnthropicPairing(t, out.Messages)
}
