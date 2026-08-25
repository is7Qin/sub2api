package apicompat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// assertAnthropicPairing enforces the Anthropic Messages tool-pairing invariants
// that, when violated, surface as upstream 400s.
func assertAnthropicPairing(t *testing.T, messages []AnthropicMessage) {
	t.Helper()
	for i, m := range messages {
		blocks := parseContentBlocks(m.Content)

		// No two consecutive same-role messages.
		if i > 0 {
			require.NotEqualf(t, messages[i-1].Role, m.Role, "consecutive %s messages at %d", m.Role, i)
		}

		for _, b := range blocks {
			switch b.Type {
			case "tool_result":
				// Must have a matching tool_use in the immediately previous message.
				require.Positivef(t, i, "tool_result %s has no previous message", b.ToolUseID)
				prev := parseContentBlocks(messages[i-1].Content)
				require.Truef(t, hasToolUse(prev, b.ToolUseID),
					"tool_result %s has no corresponding tool_use in previous message", b.ToolUseID)
			case "tool_use":
				// Must be answered by a tool_result in the immediately next message.
				require.Lessf(t, i+1, len(messages), "tool_use %s has no following message", b.ID)
				next := parseContentBlocks(messages[i+1].Content)
				require.Truef(t, hasToolResult(next, b.ID),
					"tool_use %s is not answered in the next message", b.ID)
			}
		}
	}
}

func hasToolUse(blocks []AnthropicContentBlock, id string) bool {
	for _, b := range blocks {
		if b.Type == "tool_use" && b.ID == id {
			return true
		}
	}
	return false
}

func hasToolResult(blocks []AnthropicContentBlock, toolUseID string) bool {
	for _, b := range blocks {
		if b.Type == "tool_result" && b.ToolUseID == toolUseID {
			return true
		}
	}
	return false
}

func convertAnthropic(t *testing.T, input string) []AnthropicMessage {
	t.Helper()
	_, messages, err := convertResponsesInputToAnthropic("", json.RawMessage(input))
	require.NoError(t, err)
	assertAnthropicPairing(t, messages)
	return messages
}

// Tests use call_-prefixed ids because fromResponsesCallIDToAnthropic passes
// those through unchanged (matching codex's real call_00_... ids); bare ids
// would be rewritten to toolu_<id>.

// A developer/approval message injected between a function_call and its output
// must be moved out of the tool_use→tool_result adjacency. This is the shape
// that produced the production 400 "tool_result ... must have a corresponding
// tool_use block in the previous message".
func TestAnthropicPairing_DeveloperMessageBetween(t *testing.T) {
	msgs := convertAnthropic(t, `[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"do it"}]},
		{"type":"function_call","call_id":"call_A","name":"exec","arguments":"{}"},
		{"type":"message","role":"developer","content":[{"type":"input_text","text":"Approved command prefix saved"}]},
		{"type":"function_call_output","call_id":"call_A","output":"ok"}
	]`)
	// The assistant tool_use message is immediately followed by its tool_result.
	for i, m := range msgs {
		if hasToolUse(parseContentBlocks(m.Content), "call_A") {
			require.Equal(t, "user", msgs[i+1].Role)
			require.True(t, hasToolResult(parseContentBlocks(msgs[i+1].Content), "call_A"))
		}
	}
}

// Parallel tool calls where both outputs arrive stay grouped: one assistant
// message with both tool_use blocks, the next user message with both results.
func TestAnthropicPairing_ParallelBothAnswered(t *testing.T) {
	msgs := convertAnthropic(t, `[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"features?"}]},
		{"type":"function_call","call_id":"call_c0","name":"exec","arguments":"{}"},
		{"type":"function_call","call_id":"call_c1","name":"exec","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_c0","output":"log"},
		{"type":"function_call_output","call_id":"call_c1","output":"tags"}
	]`)
	var sawGrouped bool
	for _, m := range msgs {
		blocks := parseContentBlocks(m.Content)
		if hasToolUse(blocks, "call_c0") && hasToolUse(blocks, "call_c1") {
			sawGrouped = true
		}
	}
	require.True(t, sawGrouped, "parallel tool_use blocks should share one assistant message")
}

// A parallel call whose sibling output never arrived must be dropped so every
// remaining tool_use is answered.
func TestAnthropicPairing_ParallelOneUnanswered(t *testing.T) {
	msgs := convertAnthropic(t, `[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]},
		{"type":"function_call","call_id":"call_A","name":"exec","arguments":"{}"},
		{"type":"function_call","call_id":"call_B","name":"exec","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_A","output":"oa"}
	]`)
	for _, m := range msgs {
		require.Falsef(t, hasToolUse(parseContentBlocks(m.Content), "call_B"),
			"unanswered tool_use call_B should have been dropped")
	}
}

// An orphan tool_result whose tool_use was never announced must be dropped.
func TestAnthropicPairing_OrphanToolResultDropped(t *testing.T) {
	msgs := convertAnthropic(t, `[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]},
		{"type":"function_call_output","call_id":"call_ghost","output":"orphan"}
	]`)
	for _, m := range msgs {
		require.Falsef(t, hasToolResult(parseContentBlocks(m.Content), "call_ghost"),
			"orphan tool_result should have been dropped")
	}
}

// A dangling tool_call at the end of the history (no output yet) drops the
// assistant message holding only that call, leaving no tool_use behind.
func TestAnthropicPairing_DanglingCallDropped(t *testing.T) {
	msgs := convertAnthropic(t, `[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]},
		{"type":"function_call","call_id":"call_A","name":"exec","arguments":"{}"}
	]`)
	for _, m := range msgs {
		require.Falsef(t, hasToolUse(parseContentBlocks(m.Content), "call_A"),
			"dangling tool_use call_A should have been dropped")
	}
}

// Baseline: a single answered call pairs correctly and preserves the surrounding
// turns.
func TestAnthropicPairing_SingleCall(t *testing.T) {
	msgs := convertAnthropic(t, `[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"latest sha?"}]},
		{"type":"function_call","call_id":"call_A","name":"exec","arguments":"{\"cmd\":\"git rev-parse HEAD\"}"},
		{"type":"function_call_output","call_id":"call_A","output":"deadbeef"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"It is deadbeef."}]}
	]`)
	// user, assistant(tool_use), user(tool_result), assistant(text)
	require.GreaterOrEqual(t, len(msgs), 4)
	require.Equal(t, "user", msgs[0].Role)
	require.True(t, hasToolUse(parseContentBlocks(msgs[1].Content), "call_A"))
	require.True(t, hasToolResult(parseContentBlocks(msgs[2].Content), "call_A"))
}

func TestResponsesInputToAnthropic_RejectsMalformedFunctionArguments(t *testing.T) {
	for _, arguments := range []string{
		`{"cmd":`,
		`{"cmd":"pwd"}{"cmd":"ls"}`,
	} {
		t.Run(arguments, func(t *testing.T) {
			input, err := json.Marshal([]ResponsesInputItem{{
				Type:      "function_call",
				CallID:    "call_bad",
				Name:      "exec",
				Arguments: arguments,
			}})
			require.NoError(t, err)

			_, _, err = convertResponsesInputToAnthropic("", input)
			require.ErrorContains(t, err, `responses input item 0 function_call "call_bad" arguments: must be a valid JSON object:`)
		})
	}
}

func TestResponsesInputToAnthropic_AcceptsValidFunctionArguments(t *testing.T) {
	msgs := convertAnthropic(t, `[
		{"type":"function_call","call_id":"call_A","name":"exec","arguments":"{\"cmd\":\"pwd\"}"},
		{"type":"function_call_output","call_id":"call_A","output":"ok"}
	]`)

	blocks := parseContentBlocks(msgs[0].Content)
	require.Len(t, blocks, 1)
	require.JSONEq(t, `{"cmd":"pwd"}`, string(blocks[0].Input))
}

func functionOutputBlock(t *testing.T, input string) AnthropicContentBlock {
	t.Helper()
	msgs := convertAnthropic(t, input)
	for _, msg := range msgs {
		for _, block := range parseContentBlocks(msg.Content) {
			if block.Type == "tool_result" {
				return block
			}
		}
	}
	t.Fatalf("missing tool_result block")
	return AnthropicContentBlock{}
}

func TestResponsesToAnthropic_FunctionOutputStringPreserved(t *testing.T) {
	block := functionOutputBlock(t, `[
		{"type":"function_call","call_id":"call_A","name":"exec","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_A","output":"plain result"}
	]`)
	require.JSONEq(t, `"plain result"`, string(block.Content))
}

func TestResponsesInputItemStructuredOutputWireRoundTrip(t *testing.T) {
	input := []byte(`{"type":"function_call_output","call_id":"call_A","output":[{"type":"input_text","text":"result"}]}`)
	var item ResponsesInputItem
	require.NoError(t, json.Unmarshal(input, &item))
	wire, err := json.Marshal(item)
	require.NoError(t, err)
	require.JSONEq(t, string(input), string(wire))
}

func TestResponsesInputItemNonFunctionOutputRejectsStructuredValue(t *testing.T) {
	var item ResponsesInputItem
	err := json.Unmarshal([]byte(`{"type":"message","output":[{"type":"input_text","text":"not allowed"}]}`), &item)
	require.Error(t, err)
}

func TestResponsesToAnthropic_FunctionOutputStructuredTextAndImagePreservesOrder(t *testing.T) {
	block := functionOutputBlock(t, `[
		{"type":"function_call","call_id":"call_A","name":"inspect","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_A","output":[
			{"type":"input_text","text":"before"},
			{"type":"input_image","image_url":"data:image/png;base64,AAAA"},
			{"type":"output_text","text":"after"}
		]}
	]`)
	var blocks []AnthropicContentBlock
	require.NoError(t, json.Unmarshal(block.Content, &blocks))
	require.Len(t, blocks, 3)
	require.Equal(t, "text", blocks[0].Type)
	require.Equal(t, "before", blocks[0].Text)
	require.Equal(t, "image", blocks[1].Type)
	require.NotNil(t, blocks[1].Source)
	require.Equal(t, "image/png", blocks[1].Source.MediaType)
	require.Equal(t, "AAAA", blocks[1].Source.Data)
	require.Equal(t, "text", blocks[2].Type)
	require.Equal(t, "after", blocks[2].Text)
}

func TestResponsesToAnthropic_FunctionOutputSupportedTextAliases(t *testing.T) {
	block := functionOutputBlock(t, `[
		{"type":"function_call","call_id":"call_A","name":"exec","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_A","output":[
			{"type":"input_text","text":"input"},
			{"type":"output_text","text":"output"},
			{"type":"text","text":"plain"}
		]}
	]`)
	var blocks []AnthropicContentBlock
	require.NoError(t, json.Unmarshal(block.Content, &blocks))
	require.Len(t, blocks, 3)
	require.Equal(t, []string{"input", "output", "plain"}, []string{blocks[0].Text, blocks[1].Text, blocks[2].Text})
}

func TestResponsesToAnthropic_FunctionOutputEmptyArrayUsesFallback(t *testing.T) {
	block := functionOutputBlock(t, `[
		{"type":"function_call","call_id":"call_A","name":"exec","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_A","output":[]}
	]`)
	require.JSONEq(t, `"(empty)"`, string(block.Content))
}

func TestResponsesToAnthropic_FunctionOutputUnsupportedPartsUseTextFallback(t *testing.T) {
	block := functionOutputBlock(t, `[
		{"type":"function_call","call_id":"call_A","name":"exec","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_A","output":[{"type":"unknown","text":"ignored"}]}
	]`)
	require.JSONEq(t, `"[{\"type\":\"unknown\",\"text\":\"ignored\"}]"`, string(block.Content))
}

func TestResponsesToAnthropic_FunctionOutputMalformedImageIsOmitted(t *testing.T) {
	block := functionOutputBlock(t, `[
		{"type":"function_call","call_id":"call_A","name":"inspect","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_A","output":[
			{"type":"input_image","image_url":"https://example.test/image.png"},
			{"type":"input_text","text":"still valid"}
		]}
	]`)
	var blocks []AnthropicContentBlock
	require.NoError(t, json.Unmarshal(block.Content, &blocks))
	require.Len(t, blocks, 1)
	require.Equal(t, "still valid", blocks[0].Text)
}

func TestResponsesToAnthropic_FunctionOutputMalformedPartDoesNotDiscardSiblings(t *testing.T) {
	block := functionOutputBlock(t, `[
		{"type":"function_call","call_id":"call_A","name":"inspect","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_A","output":[
			{"type":"input_image","image_url":123},
			{"type":"output_text","text":"kept"}
		]}
	]`)
	var blocks []AnthropicContentBlock
	require.NoError(t, json.Unmarshal(block.Content, &blocks))
	require.Len(t, blocks, 1)
	require.Equal(t, "kept", blocks[0].Text)
}

func TestResponsesInputItemOutputMutationWinsOverRawValue(t *testing.T) {
	var item ResponsesInputItem
	require.NoError(t, json.Unmarshal([]byte(`{"type":"function_call_output","output":[{"type":"input_text","text":"old"}]}`), &item))
	item.Output = "new"
	wire, err := json.Marshal(item)
	require.NoError(t, err)
	var decoded map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(wire, &decoded))
	require.JSONEq(t, `"new"`, string(decoded["output"]))
}

func TestResponsesToAnthropic_FunctionOutputPreservesPairingAndOrdinaryMessages(t *testing.T) {
	msgs := convertAnthropic(t, `[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"question"}]},
		{"type":"function_call","call_id":"call_A","name":"exec","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_A","output":[{"type":"output_text","text":"done"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"final"}]}
	]`)
	require.GreaterOrEqual(t, len(msgs), 4)
	require.Equal(t, "question", parseContentBlocks(msgs[0].Content)[0].Text)
	require.Equal(t, "call_A", parseContentBlocks(msgs[1].Content)[0].ID)
	require.Equal(t, "call_A", parseContentBlocks(msgs[2].Content)[0].ToolUseID)
	require.Equal(t, "final", parseContentBlocks(msgs[3].Content)[0].Text)
}
