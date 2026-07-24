package service

import (
	"fmt"
	"strings"
	"testing"

	openaipkg "github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

func TestApplyCodexOAuthTransform_ToolContinuationPreservesInput(t *testing.T) {
	// 续链场景：保留 item_reference 与 id，但不再强制 store=true。

	reqBody := map[string]any{
		"model": "gpt-5.2",
		"input": []any{
			map[string]any{"type": "item_reference", "id": "ref1", "text": "x"},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "ok", "id": "o1"},
		},
		"tool_choice": "auto",
	}

	applyCodexOAuthTransform(reqBody, false, false)

	// store 由 Forward markPatchDelete / AnthropicToResponses 统一处理；transform 不再注入。
	_, hasStore := reqBody["store"]
	require.False(t, hasStore, "store is not injected by transform")

	input, ok := reqBody["input"].([]any)
	require.True(t, ok)
	require.Len(t, input, 2)

	// 校验 input[0] 为 map，避免断言失败导致测试中断。
	first, ok := input[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "item_reference", first["type"])
	require.Equal(t, "ref1", first["id"])

	// 校验 input[1] 为 map，确保后续字段断言安全。
	second, ok := input[1].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "o1", second["id"])
	require.Equal(t, "fc_1", second["call_id"])
}

func TestApplyCodexOAuthTransform_MessagesBridgePromptCacheKeyIsHeaderOnly(t *testing.T) {
	reqBody := map[string]any{
		"model":            "gpt-5.5",
		"prompt_cache_key": "anthropic-metadata-session-1",
		"input": []any{
			map[string]any{
				"type": "message",
				"role": "developer",
				"content": []any{
					map[string]any{
						"type": "input_text",
						"text": openAICompatClaudeCodeTodoGuardMarker,
					},
				},
			},
			map[string]any{
				"type":    "message",
				"role":    "user",
				"content": "hello",
			},
		},
	}

	result := applyCodexOAuthTransformWithOptions(reqBody, codexOAuthTransformOptions{
		SkipDefaultInstructions: true,
		PreserveToolCallIDs:     true,
	})

	require.Equal(t, "anthropic-metadata-session-1", result.PromptCacheKey)
	require.True(t, result.Modified)
	require.NotContains(t, reqBody, "prompt_cache_key")
}

func TestApplyCodexOAuthTransform_ToolContinuationPreservesNativeMessageAndReasoningIDs(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.2",
		"input": []any{
			map[string]any{"type": "message", "id": "msg_0", "role": "user", "content": "hi"},
			map[string]any{"type": "item_reference", "id": "rs_123"},
		},
		"tool_choice": "auto",
	}

	applyCodexOAuthTransform(reqBody, false, false)

	input, ok := reqBody["input"].([]any)
	require.True(t, ok)
	require.Len(t, input, 2)

	first, ok := input[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "msg_0", first["id"])

	second, ok := input[1].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "rs_123", second["id"])
}

func TestApplyCodexOAuthTransform_ToolContinuationNormalizesToolReferenceIDsOnly(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.2",
		"input": []any{
			map[string]any{"type": "item_reference", "id": "call_1"},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "ok"},
		},
		"tool_choice": "auto",
	}

	applyCodexOAuthTransform(reqBody, false, false)

	input, ok := reqBody["input"].([]any)
	require.True(t, ok)
	require.Len(t, input, 2)

	first, ok := input[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "fc_1", first["id"])

	second, ok := input[1].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "fc_1", second["call_id"])
}

func TestApplyCodexOAuthTransform_BoundsLongCallIDsAndPreservesPairing(t *testing.T) {
	suffix := strings.Repeat("z", 62)
	for _, tc := range []struct {
		name         string
		callID       string
		outputCallID string
	}{
		{name: "non-native boundary id", callID: "call-" + strings.Repeat("x", 59), outputCallID: "call-" + strings.Repeat("x", 59)},
		{name: "overlong fc id", callID: "fc_" + strings.Repeat("y", 62), outputCallID: "fc_" + strings.Repeat("y", 62)},
		{name: "equivalent prefixes", callID: "call_" + suffix, outputCallID: "fc_" + suffix},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reqBody := map[string]any{
				"model": "gpt-5.2",
				"input": []any{
					map[string]any{"type": "function_call", "call_id": tc.callID, "name": "shell"},
					map[string]any{"type": "function_call_output", "call_id": tc.outputCallID, "output": "done"},
				},
			}

			applyCodexOAuthTransform(reqBody, false, false)

			input, ok := reqBody["input"].([]any)
			require.True(t, ok)
			call := input[0].(map[string]any)
			output := input[1].(map[string]any)
			fixedCallID, ok := call["call_id"].(string)
			require.True(t, ok)
			require.LessOrEqual(t, len(fixedCallID), codexCallIDMaxLength)
			require.True(t, strings.HasPrefix(fixedCallID, codexCallIDPrefix))
			require.Equal(t, fixedCallID, output["call_id"])
		})
	}
}

func TestApplyCodexOAuthTransform_PreservesLongCallIDsWhenRequested(t *testing.T) {
	callID := "call-" + strings.Repeat("x", 70)
	reqBody := map[string]any{
		"model": "gpt-5.2",
		"input": []any{
			map[string]any{"type": "function_call", "call_id": callID, "name": "shell"},
			map[string]any{"type": "function_call_output", "call_id": callID, "output": "done"},
		},
	}

	applyCodexOAuthTransformWithOptions(reqBody, codexOAuthTransformOptions{PreserveToolCallIDs: true})

	input, ok := reqBody["input"].([]any)
	require.True(t, ok)
	require.Equal(t, callID, input[0].(map[string]any)["call_id"])
	require.Equal(t, callID, input[1].(map[string]any)["call_id"])
}

func TestApplyCodexOAuthTransform_ToolSearchOutputPreservesCallID(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.2",
		"input": []any{
			map[string]any{"type": "tool_search_output", "call_id": "call_1", "output": "ok"},
		},
	}

	applyCodexOAuthTransform(reqBody, false, false)

	input, ok := reqBody["input"].([]any)
	require.True(t, ok)
	require.Len(t, input, 1)

	first, ok := input[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "tool_search_output", first["type"])
	require.Equal(t, "fc_1", first["call_id"])
}

func TestApplyCodexOAuthTransform_CustomAndMCPToolOutputsPreserveCallID(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.2",
		"input": []any{
			map[string]any{"type": "custom_tool_call_output", "call_id": "call_custom", "output": "ok"},
			map[string]any{"type": "mcp_tool_call_output", "call_id": "call_mcp", "output": "ok"},
		},
	}

	applyCodexOAuthTransform(reqBody, false, false)

	input, ok := reqBody["input"].([]any)
	require.True(t, ok)
	require.Len(t, input, 2)

	first, ok := input[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "fc_custom", first["call_id"])

	second, ok := input[1].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "fc_mcp", second["call_id"])
}

func TestApplyCodexOAuthTransform_ImageAndWebSearchCallsDoNotGainCallID(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.2",
		"input": []any{
			map[string]any{"type": "image_generation_call", "id": "ig_123", "status": "completed"},
			map[string]any{"type": "web_search_call", "call_id": "call_bad", "status": "completed"},
		},
		"tool_choice": "auto",
	}

	applyCodexOAuthTransform(reqBody, false, false)

	input, ok := reqBody["input"].([]any)
	require.True(t, ok)
	require.Len(t, input, 2)

	first, ok := input[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "ig_123", first["id"])
	_, hasCallID := first["call_id"]
	require.False(t, hasCallID)

	second, ok := input[1].(map[string]any)
	require.True(t, ok)
	_, hasCallID = second["call_id"]
	require.False(t, hasCallID)
}

func TestApplyCodexOAuthTransform_ConvertsToolRoleMessageToFunctionCallOutput(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.4",
		"input": []any{
			map[string]any{
				"type":         "message",
				"role":         "tool",
				"tool_call_id": "call_1",
				"content":      "ok",
			},
		},
	}

	applyCodexOAuthTransform(reqBody, true, false)

	input, ok := reqBody["input"].([]any)
	require.True(t, ok)
	require.Len(t, input, 1)

	item, ok := input[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "function_call_output", item["type"])
	require.Equal(t, "fc_1", item["call_id"])
	require.Equal(t, "ok", item["output"])
	_, hasRole := item["role"]
	require.False(t, hasRole)
}

func TestApplyCodexOAuthTransform_StringifiesNonStringMessageContentText(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.4",
		"input": []any{
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": []any{"a", "b"}},
				},
			},
		},
	}

	applyCodexOAuthTransform(reqBody, true, false)

	input, ok := reqBody["input"].([]any)
	require.True(t, ok)
	item, ok := input[0].(map[string]any)
	require.True(t, ok)
	content, ok := item["content"].([]any)
	require.True(t, ok)
	part, ok := content[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, `["a","b"]`, part["text"])
}

func TestApplyCodexOAuthTransform_DowngradesUnknownToolChoice(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.4",
		"tools": []any{
			map[string]any{"type": "function", "name": "shell"},
		},
		"tool_choice": map[string]any{"type": "custom"},
	}

	applyCodexOAuthTransform(reqBody, true, false)

	require.Equal(t, "auto", reqBody["tool_choice"])
}

func TestApplyCodexOAuthTransform_PreservesKnownToolChoice(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.4",
		"tools": []any{
			map[string]any{"type": "custom", "name": "shell"},
		},
		"tool_choice": map[string]any{"type": "custom"},
	}

	applyCodexOAuthTransform(reqBody, true, false)

	choice, ok := reqBody["tool_choice"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "custom", choice["type"])
}

func TestApplyCodexOAuthTransform_NormalizesLegacyFunctionToolChoice(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.4",
		"tools": []any{
			map[string]any{"type": "function", "name": "shell"},
		},
		"tool_choice": map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "shell"},
		},
	}

	applyCodexOAuthTransform(reqBody, true, false)

	choice, ok := reqBody["tool_choice"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "function", choice["type"])
	require.Equal(t, "shell", choice["name"])
	require.NotContains(t, choice, "function")
}

func TestApplyCodexOAuthTransform_DowngradesMissingFunctionToolChoice(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.4",
		"tools": []any{
			map[string]any{"type": "function", "name": "shell"},
		},
		"tool_choice": map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "missing"},
		},
	}

	applyCodexOAuthTransform(reqBody, true, false)

	require.Equal(t, "auto", reqBody["tool_choice"])
}

func TestApplyCodexOAuthTransform_AddsFallbackNameForFunctionCallInput(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.4",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "run tool"},
			map[string]any{"type": "function_call", "call_id": "call_1", "arguments": "{}"},
		},
	}

	applyCodexOAuthTransform(reqBody, true, false)

	input, ok := reqBody["input"].([]any)
	require.True(t, ok)
	require.Len(t, input, 2)
	item, ok := input[1].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "function_call", item["type"])
	require.Equal(t, "tool", item["name"])
	require.Equal(t, "fc_1", item["call_id"])
}

func TestApplyCodexOAuthTransform_StripsClientNamespaceFromReplayedInput(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.4",
		"input": []any{
			map[string]any{
				"type":      "function_call",
				"call_id":   "call_1",
				"name":      "read",
				"namespace": "mcp",
				"arguments": "{}",
			},
			map[string]any{
				"type":      "custom_tool_call",
				"call_id":   "call_2",
				"name":      "shell",
				"namespace": "tools",
				"input":     "pwd",
			},
			map[string]any{
				"type":      "message",
				"role":      "user",
				"content":   "keep metadata",
				"namespace": "application-data",
			},
		},
	}

	applyCodexOAuthTransform(reqBody, true, false)

	input, ok := reqBody["input"].([]any)
	require.True(t, ok)
	require.Len(t, input, 3)
	call, ok := input[0].(map[string]any)
	require.True(t, ok)
	require.NotContains(t, call, "namespace")
	require.Equal(t, "read", call["name"])
	require.Equal(t, "fc_1", call["call_id"])

	customCall, ok := input[1].(map[string]any)
	require.True(t, ok)
	require.NotContains(t, customCall, "namespace")
	require.Equal(t, "shell", customCall["name"])
	require.Equal(t, "fc_2", customCall["call_id"])

	message, ok := input[2].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "application-data", message["namespace"])
}

func TestApplyCodexOAuthTransform_PreservesFunctionCallInputName(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.4",
		"input": []any{
			map[string]any{"type": "custom_tool_call", "call_id": "call_1", "name": "shell", "input": "pwd"},
		},
	}

	applyCodexOAuthTransform(reqBody, true, false)

	input, ok := reqBody["input"].([]any)
	require.True(t, ok)
	require.Len(t, input, 1)
	item, ok := input[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "shell", item["name"])
	require.Equal(t, "fc_1", item["call_id"])
}

func TestApplyCodexOAuthTransform_PreservesMCPToolCallIDAndName(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.4",
		"input": []any{
			map[string]any{
				"type":      "mcp_tool_call",
				"call_id":   "call_abc",
				"name":      "remote_tool",
				"arguments": "{}",
			},
		},
	}

	applyCodexOAuthTransform(reqBody, true, false)

	input, ok := reqBody["input"].([]any)
	require.True(t, ok)
	require.Len(t, input, 1)
	item, ok := input[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "mcp_tool_call", item["type"])
	require.Equal(t, "remote_tool", item["name"])
	require.Equal(t, "fc_abc", item["call_id"])
}

func TestCodexInputItemRequiresNameTypesAllowCallID(t *testing.T) {
	for _, typ := range []string{"function_call", "custom_tool_call", "mcp_tool_call"} {
		require.True(t, codexInputItemRequiresName(typ), typ)
		require.True(t, isCodexToolCallItemType(typ), typ)
	}
}

func TestApplyCodexOAuthTransform_ExplicitStoreFalsePreserved(t *testing.T) {
	// 续链场景：显式 store=false 不再强制为 true，保持 false。

	reqBody := map[string]any{
		"model": "gpt-5.1",
		"store": false,
		"input": []any{
			map[string]any{"type": "function_call_output", "call_id": "call_1"},
		},
		"tool_choice": "auto",
	}

	applyCodexOAuthTransform(reqBody, false, false)

	store, ok := reqBody["store"].(bool)
	require.True(t, ok)
	require.False(t, store)
}

func TestApplyCodexOAuthTransform_ExplicitStoreTrueForcedFalse(t *testing.T) {
	// store 由 Forward markPatchDelete 统一处理；transform 不再覆盖。

	reqBody := map[string]any{
		"model": "gpt-5.1",
		"store": true,
		"input": []any{
			map[string]any{"type": "function_call_output", "call_id": "call_1"},
		},
		"tool_choice": "auto",
	}

	applyCodexOAuthTransform(reqBody, false, false)

	store, ok := reqBody["store"].(bool)
	require.True(t, ok)
	require.True(t, store, "store preserved by transform; forcing handled by Forward markPatchDelete")
}

func TestApplyCodexOAuthTransform_CompactForcesNonStreaming(t *testing.T) {
	// compact 的 store/stream 由 normalizeOpenAICompactRequestBody allowlist 统一丢弃；
	// transform 不再负责删除。
	reqBody := map[string]any{
		"model":  "gpt-5.1-codex",
		"store":  true,
		"stream": true,
	}

	applyCodexOAuthTransform(reqBody, true, true)

	_, hasStore := reqBody["store"]
	require.True(t, hasStore, "store preserved by transform; dropping handled by compact allowlist")
	_, hasStream := reqBody["stream"]
	require.True(t, hasStream, "stream preserved by transform; dropping handled by compact allowlist")
}

func TestApplyCodexOAuthTransform_NonContinuationDefaultsStoreFalseAndStripsIDs(t *testing.T) {
	// store 由 Forward markPatchDelete / AnthropicToResponses 统一处理；transform 不再注入。
	// input 中的 id 仍由 transform 过滤。

	reqBody := map[string]any{
		"model": "gpt-5.1",
		"input": []any{
			map[string]any{"type": "text", "id": "t1", "text": "hi"},
		},
	}

	applyCodexOAuthTransform(reqBody, false, false)

	_, hasStore := reqBody["store"]
	require.False(t, hasStore, "store not injected by transform; handled by Forward markPatchDelete")

	input, ok := reqBody["input"].([]any)
	require.True(t, ok)
	require.Len(t, input, 1)
	// 校验 input[0] 为 map，避免类型不匹配触发 errcheck。
	item, ok := input[0].(map[string]any)
	require.True(t, ok)
	_, hasID := item["id"]
	require.False(t, hasID)
}

func TestFilterCodexInput_RemovesItemReferenceWhenNotPreserved(t *testing.T) {
	input := []any{
		map[string]any{"type": "item_reference", "id": "ref1"},
		map[string]any{"type": "text", "id": "t1", "text": "hi"},
	}

	filtered := filterCodexInput(input, false)
	require.Len(t, filtered, 1)
	// 校验 filtered[0] 为 map，确保字段检查可靠。
	item, ok := filtered[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "text", item["type"])
	_, hasID := item["id"]
	require.False(t, hasID)
}

func TestApplyCodexOAuthTransform_NormalizeCodexTools_PreservesResponsesFunctionTools(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.1",
		"tools": []any{
			map[string]any{
				"type":        "function",
				"name":        "bash",
				"description": "desc",
				"parameters":  map[string]any{"type": "object"},
			},
			map[string]any{
				"type":     "function",
				"function": nil,
			},
		},
	}

	applyCodexOAuthTransform(reqBody, false, false)

	tools, ok := reqBody["tools"].([]any)
	require.True(t, ok)
	require.Len(t, tools, 1)

	first, ok := tools[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "function", first["type"])
	require.Equal(t, "bash", first["name"])
}

func TestApplyCodexOAuthTransform_StripsExactImageGenNamespaceDeclarations(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.5",
		"tools": []any{
			map[string]any{"type": "namespace", "name": "image_gen"},
			map[string]any{"type": "namespace", "name": "media_tools", "tools": []any{
				map[string]any{"type": "function", "name": "imagegen"},
			}},
			map[string]any{"type": "image_generation", "output_format": "png"},
		},
		"tool_choice": map[string]any{"type": "namespace", "name": "image_gen"},
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "draw"},
			map[string]any{"type": "additional_tools", "tools": []any{
				map[string]any{"type": "namespace", "name": "image_gen"},
				map[string]any{"type": "namespace", "name": "other"},
			}},
		},
	}

	result := applyCodexOAuthTransform(reqBody, true, false)

	require.True(t, result.Modified)
	tools, ok := reqBody["tools"].([]any)
	require.True(t, ok)
	require.Len(t, tools, 2)
	require.Equal(t, "media_tools", tools[0].(map[string]any)["name"])
	require.Equal(t, "image_generation", tools[1].(map[string]any)["type"])
	require.NotContains(t, reqBody, "tool_choice")

	input, ok := reqBody["input"].([]any)
	require.True(t, ok)
	require.Len(t, input, 2)
	additionalTools, ok := input[1].(map[string]any)["tools"].([]any)
	require.True(t, ok)
	require.Len(t, additionalTools, 1)
	require.Equal(t, "other", additionalTools[0].(map[string]any)["name"])
}

func TestApplyCodexOAuthTransform_StripsImageGenNamespaceToolChoiceWithoutDeclaration(t *testing.T) {
	reqBody := map[string]any{
		"model":       "gpt-5.5",
		"tool_choice": map[string]any{"type": "namespace", "name": "image_gen"},
		"input":       "draw",
	}

	applyCodexOAuthTransform(reqBody, true, false)

	require.NotContains(t, reqBody, "tool_choice")
}

func TestApplyCodexOAuthTransform_DropsEmptyAdditionalToolsAfterImageGenNamespaceStrip(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.5",
		"input": []any{
			map[string]any{"type": "additional_tools", "tools": []any{
				map[string]any{"type": "namespace", "name": "image_gen"},
			}},
			map[string]any{"type": "message", "role": "user", "content": "draw"},
		},
	}

	applyCodexOAuthTransform(reqBody, true, false)

	input, ok := reqBody["input"].([]any)
	require.True(t, ok)
	require.Len(t, input, 1)
	require.Equal(t, "message", input[0].(map[string]any)["type"])
}

func TestNormalizeOpenAIResponsesImageGenerationTools_RewritesLegacyFields(t *testing.T) {
	reqBody := map[string]any{
		"tools": []any{
			map[string]any{
				"type":        "image_generation",
				"format":      "png",
				"compression": 60,
			},
		},
	}

	modified := normalizeOpenAIResponsesImageGenerationTools(reqBody)
	require.True(t, modified)

	tools, ok := reqBody["tools"].([]any)
	require.True(t, ok)
	first, ok := tools[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "png", first["output_format"])
	require.Equal(t, 60, first["output_compression"])
	_, hasFormat := first["format"]
	require.False(t, hasFormat)
	_, hasCompression := first["compression"]
	require.False(t, hasCompression)
}

func TestEnsureOpenAIResponsesImageGenerationTool_NoTools(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.4",
		"input": "draw a cat",
	}

	modified := ensureOpenAIResponsesImageGenerationTool(reqBody)
	require.True(t, modified)

	tools, ok := reqBody["tools"].([]any)
	require.True(t, ok)
	require.Len(t, tools, 1)
	tool, ok := tools[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "image_generation", tool["type"])
	require.Equal(t, "png", tool["output_format"])
}

func TestEnsureOpenAIResponsesImageGenerationTool_SkipsSpark(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.3-codex-spark",
		"input": "draw a cat",
	}

	modified := ensureOpenAIResponsesImageGenerationTool(reqBody)
	require.False(t, modified)
	require.NotContains(t, reqBody, "tools")
}

func TestEnsureOpenAIResponsesImageGenerationTool_AppendsToExistingTools(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.4",
		"tools": []any{
			map[string]any{"type": "web_search"},
		},
	}

	modified := ensureOpenAIResponsesImageGenerationTool(reqBody)
	require.True(t, modified)

	tools, ok := reqBody["tools"].([]any)
	require.True(t, ok)
	require.Len(t, tools, 2)
	first, ok := tools[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "web_search", first["type"])
	second, ok := tools[1].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "image_generation", second["type"])
	require.Equal(t, "png", second["output_format"])
}

func TestEnsureOpenAIResponsesImageGenerationTool_PreservesExistingImageTool(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.4",
		"tools": []any{
			map[string]any{"type": "image_generation", "output_format": "webp"},
			map[string]any{"type": "web_search"},
		},
	}

	modified := ensureOpenAIResponsesImageGenerationTool(reqBody)
	require.False(t, modified)

	tools, ok := reqBody["tools"].([]any)
	require.True(t, ok)
	require.Len(t, tools, 2)
	tool, ok := tools[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "webp", tool["output_format"])
}

func TestApplyCodexImageGenerationBridgeInstructions_AppendsBridgeOnce(t *testing.T) {
	reqBody := map[string]any{
		"model":        "gpt-5.4",
		"instructions": "existing instructions",
		"tools": []any{
			map[string]any{"type": "image_generation", "output_format": "png"},
		},
	}

	modified := applyCodexImageGenerationBridgeInstructions(reqBody)
	require.True(t, modified)

	instructions, ok := reqBody["instructions"].(string)
	require.True(t, ok)
	require.Contains(t, instructions, "existing instructions")
	require.Contains(t, instructions, codexImageGenerationBridgeMarker)
	require.Contains(t, instructions, "Responses native `image_generation` tool")

	modified = applyCodexImageGenerationBridgeInstructions(reqBody)
	require.False(t, modified)
}

func TestApplyCodexImageGenerationBridgeInstructions_SkipsSpark(t *testing.T) {
	reqBody := map[string]any{
		"model":        "gpt-5.3-codex-spark",
		"instructions": "existing instructions",
		"tools": []any{
			map[string]any{"type": "image_generation", "output_format": "png"},
		},
	}

	modified := applyCodexImageGenerationBridgeInstructions(reqBody)
	require.False(t, modified)
	require.Equal(t, "existing instructions", reqBody["instructions"])
}

func TestApplyCodexImageGenerationBridgeInstructions_SkipsWithoutImageTool(t *testing.T) {
	reqBody := map[string]any{
		"instructions": "existing instructions",
		"tools": []any{
			map[string]any{"type": "web_search"},
		},
	}

	modified := applyCodexImageGenerationBridgeInstructions(reqBody)
	require.False(t, modified)
	require.Equal(t, "existing instructions", reqBody["instructions"])
}

func TestValidateCodexSparkInputRejectsInputImage(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.3-codex-spark",
		"input": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": "describe"},
					map[string]any{"type": "input_image", "image_url": "data:image/png;base64,aGVsbG8="},
				},
			},
		},
	}

	err := validateCodexSparkInput(reqBody, "gpt-5.3-codex-spark")
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not support image input")
}

func TestValidateCodexSparkInputRejectsChatImageURL(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.3-codex-spark",
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "describe"},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,aGVsbG8="}},
				},
			},
		},
	}

	err := validateCodexSparkInput(reqBody, "gpt-5.3-codex-spark")
	require.Error(t, err)
}

func TestValidateCodexSparkInputAllowsTextOnly(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.3-codex-spark",
		"input": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": "hello"},
				},
			},
		},
	}

	require.NoError(t, validateCodexSparkInput(reqBody, "gpt-5.3-codex-spark"))
}

func TestApplyCodexOAuthTransform_AddsSparkImageUnsupportedInstructions(t *testing.T) {
	reqBody := map[string]any{
		"model":        "gpt-5.3-codex-spark",
		"instructions": "existing instructions",
		"input":        "hello",
	}

	result := applyCodexOAuthTransform(reqBody, true, false)
	require.True(t, result.Modified)

	instructions, ok := reqBody["instructions"].(string)
	require.True(t, ok)
	require.Contains(t, instructions, "existing instructions")
	require.Contains(t, instructions, codexSparkImageUnsupportedMarker)
	require.Contains(t, instructions, "does not support image generation")
	require.Contains(t, instructions, "switch to a non-Spark Codex model")
	require.NotContains(t, instructions, codexImageGenerationBridgeMarker)
}

func TestApplyCodexOAuthTransform_DoesNotAddSparkImageUnsupportedForNonSpark(t *testing.T) {
	reqBody := map[string]any{
		"model":        "gpt-5.4",
		"instructions": "existing instructions",
		"input":        "hello",
	}

	applyCodexOAuthTransform(reqBody, true, false)
	instructions, ok := reqBody["instructions"].(string)
	require.True(t, ok)
	require.NotContains(t, instructions, codexSparkImageUnsupportedMarker)
}

func TestApplyCodexOAuthTransform_StripsImageGenerationToolForSpark(t *testing.T) {
	reqBody := map[string]any{
		"model":       "gpt-5.3-codex-spark",
		"input":       "hello",
		"tool_choice": map[string]any{"type": "image_generation"},
		"tools": []any{
			map[string]any{"type": "function", "name": "shell"},
			map[string]any{"type": "image_generation", "output_format": "png"},
		},
	}

	result := applyCodexOAuthTransform(reqBody, true, false)
	require.True(t, result.Modified)
	require.False(t, hasOpenAIImageGenerationTool(reqBody))
	require.NotContains(t, reqBody, "tool_choice")

	tools, ok := reqBody["tools"].([]any)
	require.True(t, ok)
	require.Len(t, tools, 1)
	first, ok := tools[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "function", first["type"])
	require.Equal(t, "shell", first["name"])
}

func TestApplyCodexOAuthTransform_StripsImageGenerationToolForSparkAlias(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.3-codex-spark-high",
		"input": "hello",
		"tools": []any{
			map[string]any{"type": "image_generation", "output_format": "png"},
		},
	}

	result := applyCodexOAuthTransform(reqBody, true, false)
	require.True(t, result.Modified)
	require.False(t, hasOpenAIImageGenerationTool(reqBody))
	require.NotContains(t, reqBody, "tools")
}

func TestApplyCodexOAuthTransform_KeepsImageGenerationToolForNonSpark(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.3-codex",
		"input": "hello",
		"tools": []any{
			map[string]any{"type": "image_generation", "output_format": "png"},
		},
	}

	applyCodexOAuthTransform(reqBody, true, false)
	require.True(t, hasOpenAIImageGenerationTool(reqBody))
}

func TestStripCodexSparkImageGenerationTooling_RemovesToolChoice(t *testing.T) {
	reqBody := map[string]any{
		"model":       "gpt-5.3-codex-spark",
		"tool_choice": map[string]any{"type": "image_generation"},
		"tools": []any{
			map[string]any{"type": "function", "name": "shell"},
			map[string]any{"type": "image_generation", "output_format": "png"},
		},
	}

	modified := stripCodexSparkImageGenerationTooling(reqBody, "gpt-5.3-codex-spark")
	require.True(t, modified)
	require.False(t, hasOpenAIImageGenerationTool(reqBody))
	require.NotContains(t, reqBody, "tool_choice")

	tools, ok := reqBody["tools"].([]any)
	require.True(t, ok)
	require.Len(t, tools, 1)
	first, ok := tools[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "function", first["type"])
	require.Equal(t, "shell", first["name"])
}

func TestStripCodexSparkImageGenerationTooling_RemovesOnlyToolChoice(t *testing.T) {
	reqBody := map[string]any{
		"model":       "gpt-5.3-codex-spark-high",
		"tool_choice": map[string]any{"type": "image_generation"},
		"tools": []any{
			map[string]any{"type": "function", "name": "shell"},
		},
	}

	modified := stripCodexSparkImageGenerationTooling(reqBody, "gpt-5.3-codex-spark")
	require.True(t, modified)
	require.NotContains(t, reqBody, "tool_choice")
	require.True(t, codexToolsContainFunctionName(reqBody["tools"], "shell"))
}

func TestStripCodexSparkImageGenerationTooling_PreservesNonImageToolChoice(t *testing.T) {
	reqBody := map[string]any{
		"model":       "gpt-5.3-codex-spark",
		"tool_choice": map[string]any{"type": "function", "name": "shell"},
		"tools": []any{
			map[string]any{"type": "function", "name": "shell"},
			map[string]any{"type": "image_generation", "output_format": "png"},
		},
	}

	modified := stripCodexSparkImageGenerationTooling(reqBody, "gpt-5.3-codex-spark")
	require.True(t, modified)
	require.False(t, hasOpenAIImageGenerationTool(reqBody))
	choice, ok := reqBody["tool_choice"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "function", choice["type"])
	require.Equal(t, "shell", choice["name"])
}

func TestStripCodexSparkImageGenerationTooling_PreservesNonSpark(t *testing.T) {
	reqBody := map[string]any{
		"model":       "gpt-5.3-codex",
		"tool_choice": map[string]any{"type": "image_generation"},
		"tools": []any{
			map[string]any{"type": "image_generation", "output_format": "png"},
		},
	}

	modified := stripCodexSparkImageGenerationTooling(reqBody, "gpt-5.3-codex")
	require.False(t, modified)
	require.True(t, hasOpenAIImageGenerationTool(reqBody))
	require.Contains(t, reqBody, "tool_choice")
}
func TestNormalizeOpenAIResponsesImageOnlyModel_BuildsImageToolRequest(t *testing.T) {
	reqBody := map[string]any{
		"model":         "gpt-image-2",
		"prompt":        "draw a cat",
		"size":          "1024x1024",
		"output_format": "png",
	}

	modified := normalizeOpenAIResponsesImageOnlyModel(reqBody)
	require.True(t, modified)
	require.Equal(t, openAIImagesResponsesMainModel, reqBody["model"])
	require.Equal(t, "draw a cat", reqBody["input"])
	_, hasPrompt := reqBody["prompt"]
	require.False(t, hasPrompt)
	_, hasTopLevelSize := reqBody["size"]
	require.False(t, hasTopLevelSize)

	tools, ok := reqBody["tools"].([]any)
	require.True(t, ok)
	require.Len(t, tools, 1)
	tool, ok := tools[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "image_generation", tool["type"])
	require.Equal(t, "gpt-image-2", tool["model"])
	require.Equal(t, "1024x1024", tool["size"])
	require.Equal(t, "png", tool["output_format"])

	choice, ok := reqBody["tool_choice"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "image_generation", choice["type"])
}

func TestNormalizeOpenAIResponsesImageOnlyModel_PreservesExistingImageTool(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-image-2",
		"input": "draw a cat",
		"tools": []any{
			map[string]any{
				"type":  "image_generation",
				"model": "gpt-image-1.5",
			},
		},
		"tool_choice": "auto",
	}

	modified := normalizeOpenAIResponsesImageOnlyModel(reqBody)
	require.True(t, modified)
	require.Equal(t, openAIImagesResponsesMainModel, reqBody["model"])
	require.Equal(t, "auto", reqBody["tool_choice"])

	tools, ok := reqBody["tools"].([]any)
	require.True(t, ok)
	require.Len(t, tools, 1)
	tool, ok := tools[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "gpt-image-1.5", tool["model"])
}

func TestValidateOpenAIResponsesImageModel_RejectsImageOnlyModel(t *testing.T) {
	err := validateOpenAIResponsesImageModel(map[string]any{
		"tools": []any{
			map[string]any{"type": "image_generation"},
		},
	}, "gpt-image-2")

	require.ErrorContains(t, err, `/v1/responses image_generation requests require a Responses-capable text model`)
}

func TestApplyCodexOAuthTransform_EmptyInput(t *testing.T) {
	// 空 input 应保持为空且不触发异常。

	reqBody := map[string]any{
		"model": "gpt-5.1",
		"input": []any{},
	}

	applyCodexOAuthTransform(reqBody, false, false)

	input, ok := reqBody["input"].([]any)
	require.True(t, ok)
	require.Len(t, input, 0)
}

func TestNormalizeCodexModel_Gpt53(t *testing.T) {
	cases := map[string]string{
		"gpt-5.4":                   "gpt-5.4",
		"gpt5.5":                    "gpt-5.5",
		"openai/gpt5.5":             "gpt-5.5",
		"codex-auto-review":         "codex-auto-review",
		"gpt5.4":                    "gpt-5.4",
		"gpt-5.4-high":              "gpt-5.4",
		"gpt-5.4-chat-latest":       "gpt-5.4",
		"gpt 5.4":                   "gpt-5.4",
		"gpt-5.4-mini":              "gpt-5.4-mini",
		"gpt5.4-mini":               "gpt-5.4-mini",
		"gpt5.4mini":                "gpt-5.4-mini",
		"gpt 5.4 mini":              "gpt-5.4-mini",
		"gpt-5.3":                   "gpt-5.3-codex",
		"gpt5.3":                    "gpt-5.3-codex",
		"gpt-5.3-codex":             "gpt-5.3-codex",
		"gpt5.3-codex":              "gpt-5.3-codex",
		"gpt5.3codex":               "gpt-5.3-codex",
		"gpt-5.3-codex-xhigh":       "gpt-5.3-codex",
		"gpt-5.3-codex-spark":       "gpt-5.3-codex-spark",
		"gpt5.3-codex-spark":        "gpt-5.3-codex-spark",
		"gpt5.3codexspark":          "gpt-5.3-codex-spark",
		"gpt 5.3 codex spark":       "gpt-5.3-codex-spark",
		"gpt-5.3-codex-spark-high":  "gpt-5.3-codex-spark",
		"gpt-5.3-codex-spark-xhigh": "gpt-5.3-codex-spark",
		"gpt 5.3 codex":             "gpt-5.3-codex",
	}

	for input, expected := range cases {
		require.Equal(t, expected, normalizeCodexModel(input))
	}
}

func TestNormalizeCodexModel_RemovedModelsFallbackToSupportedTargets(t *testing.T) {
	cases := map[string]string{
		"":                   "gpt-5.4",
		"gpt-5":              "gpt-5.4",
		"gpt-5-mini":         "gpt-5.4",
		"gpt-5-nano":         "gpt-5.4",
		"gpt-5.1":            "gpt-5.4",
		"gpt-5.1-codex":      "gpt-5.3-codex",
		"gpt-5.1-codex-max":  "gpt-5.3-codex",
		"gpt-5.1-codex-mini": "gpt-5.3-codex",
		"gpt-5.2-codex":      "gpt-5.2",
		"codex-mini-latest":  "gpt-5.3-codex",
		"gpt-5-codex":        "gpt-5.3-codex",
	}

	for input, expected := range cases {
		require.Equal(t, expected, normalizeCodexModel(input))
	}
}

func TestApplyCodexOAuthTransform_PreservesBareSparkModel(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.3-codex-spark",
		"input": []any{},
	}

	result := applyCodexOAuthTransform(reqBody, false, false)

	require.Equal(t, "gpt-5.3-codex-spark", reqBody["model"])
	require.Equal(t, "gpt-5.3-codex-spark", result.NormalizedModel)
	// store 由 Forward markPatchDelete 统一处理；transform 不再注入。
	_, hasStore := reqBody["store"]
	require.False(t, hasStore, "store not injected by transform")
}

func TestApplyCodexOAuthTransform_TrimmedModelWithoutPolicyRewrite(t *testing.T) {
	reqBody := map[string]any{
		"model": "  gpt-5.3-codex-spark  ",
		"input": []any{},
	}

	result := applyCodexOAuthTransform(reqBody, false, false)

	require.Equal(t, "gpt-5.3-codex-spark", reqBody["model"])
	require.Equal(t, "gpt-5.3-codex-spark", result.NormalizedModel)
	require.True(t, result.Modified)
}

func TestApplyCodexOAuthTransform_CodexCLI_PreservesExistingInstructions(t *testing.T) {
	// Codex CLI 场景：已有 instructions 时不修改

	reqBody := map[string]any{
		"model":        "gpt-5.5",
		"instructions": "existing instructions",
	}

	result := applyCodexOAuthTransform(reqBody, true, false) // isCodexCLI=true

	instructions, ok := reqBody["instructions"].(string)
	require.True(t, ok)
	require.Equal(t, "existing instructions", instructions)
	// Modified 仍可能为 true（因为其他字段被修改），但 instructions 应保持不变
	_ = result
}

func TestApplyCodexOAuthTransform_CodexCLI_SuppliesDefaultWhenEmpty(t *testing.T) {
	// Codex CLI 场景：无 instructions 时补充默认值

	reqBody := map[string]any{
		"model": "gpt-5.5",
		// 没有 instructions 字段
	}

	result := applyCodexOAuthTransform(reqBody, true, false) // isCodexCLI=true

	instructions, ok := reqBody["instructions"].(string)
	require.True(t, ok)
	require.Equal(t, openaipkg.CodexSyntheticDefaultInstructionsForModel("gpt-5.5"), instructions)
	require.True(t, result.Modified)
}

func TestApplyCodexOAuthTransform_SuppliesLatestDefaultForFallbackModelWhenEmpty(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.4",
	}

	result := applyCodexOAuthTransform(reqBody, true, false)

	instructions, ok := reqBody["instructions"].(string)
	require.True(t, ok)
	require.Equal(t, openaipkg.CodexSyntheticDefaultInstructionsForModel("gpt-5.4"), instructions)
	require.True(t, result.Modified)
}

func TestApplyCodexOAuthTransform_SkipDefaultInstructionsDoesNotInject(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.5",
		"input": "hello",
	}

	result := applyCodexOAuthTransformWithOptions(reqBody, codexOAuthTransformOptions{
		IsCodexCLI:              true,
		SkipDefaultInstructions: true,
	})

	require.True(t, result.Modified)
	require.NotContains(t, reqBody, "instructions")
}

func TestApplyCodexOAuthTransform_NonCodexCLI_PreservesExistingInstructions(t *testing.T) {
	// 非 Codex CLI 场景：已有 instructions 时保留客户端的值，不再覆盖

	reqBody := map[string]any{
		"model":        "gpt-5.5",
		"instructions": "old instructions",
	}

	applyCodexOAuthTransform(reqBody, false, false) // isCodexCLI=false

	instructions, ok := reqBody["instructions"].(string)
	require.True(t, ok)
	require.Equal(t, "old instructions", instructions)
}

func TestApplyCodexOAuthTransform_StringInputConvertedToArray(t *testing.T) {
	reqBody := map[string]any{"model": "gpt-5.4", "input": "Hello, world!"}
	result := applyCodexOAuthTransform(reqBody, false, false)
	require.True(t, result.Modified)
	input, ok := reqBody["input"].([]any)
	require.True(t, ok)
	require.Len(t, input, 1)
	msg, ok := input[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "message", msg["type"])
	require.Equal(t, "user", msg["role"])
	require.Equal(t, "Hello, world!", msg["content"])
}

func TestApplyCodexOAuthTransform_EmptyStringInputBecomesEmptyArray(t *testing.T) {
	reqBody := map[string]any{"model": "gpt-5.4", "input": ""}
	result := applyCodexOAuthTransform(reqBody, false, false)
	require.True(t, result.Modified)
	input, ok := reqBody["input"].([]any)
	require.True(t, ok)
	require.Len(t, input, 0)
}

func TestApplyCodexOAuthTransform_WhitespaceStringInputBecomesEmptyArray(t *testing.T) {
	reqBody := map[string]any{"model": "gpt-5.4", "input": "   "}
	result := applyCodexOAuthTransform(reqBody, false, false)
	require.True(t, result.Modified)
	input, ok := reqBody["input"].([]any)
	require.True(t, ok)
	require.Len(t, input, 0)
}

func TestApplyCodexOAuthTransform_StringInputWithToolsField(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.4",
		"input": "Run the tests",
		"tools": []any{map[string]any{"type": "function", "name": "bash"}},
	}
	applyCodexOAuthTransform(reqBody, false, false)
	input, ok := reqBody["input"].([]any)
	require.True(t, ok)
	require.Len(t, input, 1)
}

func TestExtractSystemMessagesFromInput(t *testing.T) {
	t.Run("no system messages", func(t *testing.T) {
		reqBody := map[string]any{
			"input": []any{
				map[string]any{"role": "user", "content": "hello"},
			},
		}
		result := extractSystemMessagesFromInput(reqBody, false)
		require.False(t, result)
		input, ok := reqBody["input"].([]any)
		require.True(t, ok)
		require.Len(t, input, 1)
		_, hasInstructions := reqBody["instructions"]
		require.False(t, hasInstructions)
	})

	t.Run("string content system message", func(t *testing.T) {
		reqBody := map[string]any{
			"input": []any{
				map[string]any{"role": "system", "content": "You are an assistant."},
				map[string]any{"role": "user", "content": "hello"},
			},
		}
		result := extractSystemMessagesFromInput(reqBody, false)
		require.True(t, result)
		input, ok := reqBody["input"].([]any)
		require.True(t, ok)
		require.Len(t, input, 1)
		msg, ok := input[0].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "user", msg["role"])
		require.Equal(t, "You are an assistant.", reqBody["instructions"])
	})

	t.Run("array content system message", func(t *testing.T) {
		reqBody := map[string]any{
			"input": []any{
				map[string]any{
					"role": "system",
					"content": []any{
						map[string]any{"type": "text", "text": "Be helpful."},
					},
				},
			},
		}
		result := extractSystemMessagesFromInput(reqBody, false)
		require.True(t, result)
		require.Equal(t, "Be helpful.", reqBody["instructions"])
		input, ok := reqBody["input"].([]any)
		require.True(t, ok)
		require.Len(t, input, 0)
	})

	t.Run("multiple system messages concatenated", func(t *testing.T) {
		reqBody := map[string]any{
			"input": []any{
				map[string]any{"role": "system", "content": "First."},
				map[string]any{"role": "system", "content": "Second."},
				map[string]any{"role": "user", "content": "hi"},
			},
		}
		result := extractSystemMessagesFromInput(reqBody, false)
		require.True(t, result)
		require.Equal(t, "First.\n\nSecond.", reqBody["instructions"])
		input, ok := reqBody["input"].([]any)
		require.True(t, ok)
		require.Len(t, input, 1)
	})

	t.Run("mixed system and non-system preserves non-system", func(t *testing.T) {
		reqBody := map[string]any{
			"input": []any{
				map[string]any{"role": "user", "content": "hello"},
				map[string]any{"role": "system", "content": "Sys prompt."},
				map[string]any{"role": "assistant", "content": "Hi there"},
			},
		}
		result := extractSystemMessagesFromInput(reqBody, false)
		require.True(t, result)
		input, ok := reqBody["input"].([]any)
		require.True(t, ok)
		require.Len(t, input, 2)
		first, ok := input[0].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "user", first["role"])
		second, ok := input[1].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "assistant", second["role"])
	})

	t.Run("existing instructions prepended", func(t *testing.T) {
		reqBody := map[string]any{
			"input": []any{
				map[string]any{"role": "system", "content": "Extracted."},
				map[string]any{"role": "user", "content": "hi"},
			},
			"instructions": "Existing instructions.",
		}
		result := extractSystemMessagesFromInput(reqBody, false)
		require.True(t, result)
		require.Equal(t, "Extracted.\n\nExisting instructions.", reqBody["instructions"])
		require.Equal(t, "user", reqBody["input"].([]any)[0].(map[string]any)["role"])
	})

	t.Run("system without text is removed without inventing instructions", func(t *testing.T) {
		reqBody := map[string]any{"input": []any{map[string]any{
			"role": "system", "content": []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AA=="}},
		}}}
		require.True(t, extractSystemMessagesFromInput(reqBody, false))
		require.Empty(t, reqBody["input"])
		require.NotContains(t, reqBody, "instructions")
	})

	t.Run("developer role is a protocol boundary and remains unchanged", func(t *testing.T) {
		developer := map[string]any{"role": "developer", "content": "Output JSON only."}
		reqBody := map[string]any{"input": []any{developer}}
		require.False(t, extractSystemMessagesFromInput(reqBody, false))
		require.Equal(t, developer, reqBody["input"].([]any)[0])
		require.NotContains(t, reqBody, "instructions")
	})
}

// TestApplyCodexOAuthTransform_StripsPromptCacheRetention is a regression
// test: some clients (e.g. Cursor cloud via the Responses-shape compat path)
// send prompt_cache_retention, but the ChatGPT internal Codex endpoint
// rejects it with "Unsupported parameter: prompt_cache_retention".
func TestApplyCodexOAuthTransform_StripsPromptCacheRetention(t *testing.T) {
	reqBody := map[string]any{
		"model":                  "gpt-5.1",
		"prompt_cache_retention": "24h",
		"input": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	}

	applyCodexOAuthTransform(reqBody, false, false)

	// 字段过滤已移至 Forward 的 markPatchDelete 统一处理；transform 不再负责删除。
	_, stillThere := reqBody["prompt_cache_retention"]
	require.True(t, stillThere,
		"prompt_cache_retention preserved by transform; deletion handled by Forward markPatchDelete")
}

func TestApplyCodexOAuthTransform_StripsChatGPTInternalUnsupportedFields(t *testing.T) {
	// 字段过滤已移至 Forward 的 markPatchDelete 统一处理；transform 不再负责删除。
	// 此测试验证 transform 不会错误地删除这些字段。
	reqBody := map[string]any{
		"model":                  "gpt-5.4",
		"user":                   "user_123",
		"metadata":               map[string]any{"trace_id": "abc"},
		"prompt_cache_retention": "24h",
		"safety_identifier":      "sid",
		"stream_options":         map[string]any{"include_usage": true},
		"input": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
	}

	applyCodexOAuthTransform(reqBody, true, false)

	// transform 保留这些字段；由 Forward markPatchDelete 在调用方删除。
	for _, field := range []string{"user", "metadata", "prompt_cache_retention", "safety_identifier", "stream_options"} {
		require.Contains(t, reqBody, field, "%s should be preserved by transform", field)
	}
}

func TestApplyCodexOAuthTransform_ExtractsSystemMessages(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.1",
		"input": []any{
			map[string]any{"role": "system", "content": "You are a coding assistant."},
			map[string]any{"role": "user", "content": "Write a function."},
		},
	}

	result := applyCodexOAuthTransform(reqBody, false, false)

	require.True(t, result.Modified)

	input, ok := reqBody["input"].([]any)
	require.True(t, ok)
	require.Len(t, input, 1)
	msg, ok := input[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "user", msg["role"])

	instructions, ok := reqBody["instructions"].(string)
	require.True(t, ok)
	require.Equal(t, "You are a coding assistant.", instructions)
}

func TestApplyCodexOAuthTransform_JSONModePreservesDeveloperInput(t *testing.T) {
	reqBody := map[string]any{
		"model": "gpt-5.4",
		"input": []any{
			map[string]any{"role": "system", "content": "Output JSON only."},
			map[string]any{"role": "user", "content": "Return the result."},
		},
		"text": map[string]any{"format": map[string]any{"type": "json_object"}},
	}

	result := applyCodexOAuthTransformWithOptions(reqBody, codexOAuthTransformOptions{
		SkipDefaultInstructions: true,
	})

	require.True(t, result.Modified)
	require.NotContains(t, reqBody, "instructions")
	input := reqBody["input"].([]any)
	require.Len(t, input, 2)
	require.Equal(t, "developer", input[0].(map[string]any)["role"])
	require.Equal(t, "Output JSON only.", input[0].(map[string]any)["content"])
	require.Equal(t, "user", input[1].(map[string]any)["role"])
}

func TestApplyCodexOAuthTransform_JSONModeKeepsExistingInstructionsSeparate(t *testing.T) {
	reqBody := map[string]any{
		"model":        "gpt-5.4",
		"instructions": "Existing instructions.",
		"input": []any{
			map[string]any{"role": "system", "content": "Output JSON only."},
			map[string]any{"role": "user", "content": "Return the result."},
		},
		"text": map[string]any{"format": map[string]any{"type": "json_object"}},
	}

	applyCodexOAuthTransform(reqBody, false, false)

	require.Equal(t, "Existing instructions.", reqBody["instructions"])
	input := reqBody["input"].([]any)
	require.Len(t, input, 2)
	require.Equal(t, "developer", input[0].(map[string]any)["role"])
	require.Equal(t, "Output JSON only.", input[0].(map[string]any)["content"])
}

func TestIsInstructionsEmpty(t *testing.T) {
	tests := []struct {
		name     string
		reqBody  map[string]any
		expected bool
	}{
		{"missing field", map[string]any{}, true},
		{"nil value", map[string]any{"instructions": nil}, true},
		{"empty string", map[string]any{"instructions": ""}, true},
		{"whitespace only", map[string]any{"instructions": "   "}, true},
		{"non-string", map[string]any{"instructions": 123}, true},
		{"valid string", map[string]any{"instructions": "hello"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isInstructionsEmpty(tt.reqBody)
			require.Equal(t, tt.expected, result)
		})
	}
}

func TestFilterCodexInput_PreservesEncryptedReasoningAndStripsID(t *testing.T) {
	for _, preserve := range []bool{true, false} {
		t.Run(fmt.Sprintf("preserveReferences=%v", preserve), func(t *testing.T) {
			original := map[string]any{"type": "reasoning", "id": "rs_stale", "encrypted_content": "gAAA", "content": []any{"opaque"}}
			filtered := filterCodexInput([]any{original}, preserve)
			require.Len(t, filtered, 1)
			item := filtered[0].(map[string]any)
			require.Equal(t, "gAAA", item["encrypted_content"])
			require.NotContains(t, item, "id")
			require.Empty(t, item["summary"])
			require.Equal(t, "rs_stale", original["id"], "filter must not mutate caller input")
		})
	}
}

func TestFilterCodexInput_DropsBareReasoningReferences(t *testing.T) {
	filtered := filterCodexInput([]any{map[string]any{"type": "reasoning", "id": "rs_stale", "summary": []any{}}}, false)
	require.Empty(t, filtered)
}

func TestFilterCodexInput_DropsMalformedEncryptedReasoning(t *testing.T) {
	for _, tc := range []struct {
		name       string
		ciphertext any
	}{
		{name: "empty", ciphertext: ""},
		{name: "whitespace", ciphertext: " \t\r\n "},
		{name: "non-string", ciphertext: []any{"gAAA"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			filtered := filterCodexInput([]any{map[string]any{
				"type":              "reasoning",
				"id":                "rs_stale",
				"encrypted_content": tc.ciphertext,
			}}, false)
			require.Empty(t, filtered)
		})
	}
}

func TestFilterCodexInput_StripsNonFcIDFromToolCallInputsWhenPreservingReferences(t *testing.T) {
	for _, typ := range []string{
		"function_call",
		"tool_call",
		"local_shell_call",
		"tool_search_call",
		"custom_tool_call",
		"mcp_tool_call",
	} {
		t.Run(typ, func(t *testing.T) {
			filtered := filterCodexInputWithOptions([]any{
				map[string]any{
					"type":    typ,
					"id":      "item_123",
					"call_id": "fc_123",
					"name":    "tool",
				},
			}, codexInputFilterOptions{PreserveReferences: true})

			require.Len(t, filtered, 1)
			item, ok := filtered[0].(map[string]any)
			require.True(t, ok)
			require.Equal(t, typ, item["type"])
			require.Equal(t, "fc_123", item["call_id"])
			require.NotContains(t, item, "id")
		})
	}
}

func TestFilterCodexInput_KeepsValidToolCallInputIDWhenPreservingReferences(t *testing.T) {
	filtered := filterCodexInputWithOptions([]any{
		map[string]any{
			"type":    "function_call",
			"id":      "fc_123",
			"call_id": "fc_123",
			"name":    "tool",
		},
	}, codexInputFilterOptions{PreserveReferences: true})

	require.Len(t, filtered, 1)
	item, ok := filtered[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "fc_123", item["id"])
}

func TestFilterCodexInput_MessageIDsWithPreservedReferences(t *testing.T) {
	invalidMessage := map[string]any{"type": "message", "id": "item_message_123", "role": "user"}
	input := []any{
		map[string]any{"type": "item_reference", "id": "call_123"},
		map[string]any{"type": "function_call_output", "id": "item_output_123", "call_id": "call_123", "output": "ok"},
		invalidMessage,
		map[string]any{"type": "message", "id": "msg_message_123", "role": "assistant"},
	}

	filtered := filterCodexInputWithOptions(input, codexInputFilterOptions{
		PreserveReferences: true,
		PreserveCallIDs:    true,
	})

	require.Len(t, filtered, 4)
	require.Equal(t, "call_123", filtered[0].(map[string]any)["id"])
	require.Equal(t, "call_123", filtered[1].(map[string]any)["call_id"])
	require.Equal(t, "item_output_123", filtered[1].(map[string]any)["id"])
	require.NotContains(t, filtered[2].(map[string]any), "id")
	require.Equal(t, "msg_message_123", filtered[3].(map[string]any)["id"])
	require.Equal(t, "item_message_123", invalidMessage["id"], "caller input must not be mutated")
}

func TestFilterCodexInput_MessageIDsWithoutPreservedReferencesUnchanged(t *testing.T) {
	for _, id := range []string{"item_message_123", "msg_message_123"} {
		filtered := filterCodexInputWithOptions([]any{
			map[string]any{"type": "message", "id": id, "role": "user"},
		}, codexInputFilterOptions{PreserveReferences: false})

		require.Len(t, filtered, 1)
		require.NotContains(t, filtered[0].(map[string]any), "id")
	}
}
