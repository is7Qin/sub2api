package apicompat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFlattenResponsesNamespacesExcept_RewritesDeclarationsCallsAndChoice(t *testing.T) {
	req := map[string]any{
		"tools": []any{
			map[string]any{"type": "namespace", "name": "mcp", "tools": []any{
				map[string]any{"type": "function", "name": "read", "description": "read a file", "parameters": map[string]any{"type": "object"}},
			}},
			map[string]any{"type": "namespace", "name": "image_gen", "tools": []any{
				map[string]any{"type": "function", "name": "generate"},
			}},
		},
		"tool_choice": map[string]any{"type": "function", "namespace": "mcp", "name": "read"},
		"input": []any{
			map[string]any{"type": "function_call", "namespace": "mcp", "name": "read", "arguments": "{}"},
			map[string]any{"type": "message", "namespace": "message-meta", "content": []any{
				map[string]any{"type": "input_text", "text": "hello", "namespace": "nested-meta"},
			}},
		},
	}

	names, changed, err := FlattenResponsesNamespacesExcept(req, map[string]bool{"image_gen": true})
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, ResponsesNamespaceName{Namespace: "mcp", Name: "read"}, names["mcp__read"])

	tools := req["tools"].([]any)
	require.Len(t, tools, 2)
	require.Equal(t, "function", tools[0].(map[string]any)["type"])
	require.Equal(t, "mcp__read", tools[0].(map[string]any)["name"])
	require.Equal(t, "image_gen", tools[1].(map[string]any)["name"])

	choice := req["tool_choice"].(map[string]any)
	require.Equal(t, "mcp__read", choice["name"])
	require.NotContains(t, choice, "namespace")

	input := req["input"].([]any)
	call := input[0].(map[string]any)
	require.Equal(t, "mcp__read", call["name"])
	require.NotContains(t, call, "namespace")
	message := input[1].(map[string]any)
	require.Equal(t, "message-meta", message["namespace"])
	nested := message["content"].([]any)[0].(map[string]any)
	require.Equal(t, "nested-meta", nested["namespace"])
}

func TestFlattenResponsesNamespacesExcept_RewritesNestedNamespaceToolChoice(t *testing.T) {
	req := map[string]any{
		"tools": []any{map[string]any{
			"type": "namespace", "name": "mcp",
			"children": []any{map[string]any{"type": "function", "name": "read"}},
		}},
		"tool_choice": map[string]any{
			"type": "namespace", "name": "mcp",
			"function": map[string]any{"name": "read"},
		},
	}

	_, changed, err := FlattenResponsesNamespaces(req)
	require.NoError(t, err)
	require.True(t, changed)
	choice := req["tool_choice"].(map[string]any)
	require.Equal(t, "function", choice["type"])
	require.Equal(t, "mcp__read", choice["name"])
	require.NotContains(t, choice, "function")
}

func TestFlattenResponsesNamespacesExcept_RewritesLegacyQualifiedFunctionToolChoice(t *testing.T) {
	req := map[string]any{
		"tools": []any{map[string]any{
			"type": "namespace", "name": "mcp",
			"tools": []any{map[string]any{"type": "function", "name": "read"}},
		}},
		"tool_choice": map[string]any{
			"type": "function", "namespace": "mcp",
			"function": map[string]any{"name": "read"},
		},
	}

	_, changed, err := FlattenResponsesNamespaces(req)
	require.NoError(t, err)
	require.True(t, changed)
	choice := req["tool_choice"].(map[string]any)
	require.Equal(t, "function", choice["type"])
	require.Equal(t, "mcp__read", choice["name"])
	require.NotContains(t, choice, "function")
	require.NotContains(t, choice, "namespace")
}

func TestFlattenResponsesNamespacesExcept_LeavesUnrelatedDuplicateTopLevelToolsToUpstream(t *testing.T) {
	req := map[string]any{"tools": []any{
		map[string]any{"type": "function", "name": "plain", "description": "one"},
		map[string]any{"type": "function", "name": "plain", "description": "two"},
		map[string]any{"type": "namespace", "name": "mcp", "tools": []any{
			map[string]any{"type": "function", "name": "read"},
		}},
	}}

	_, changed, err := FlattenResponsesNamespaces(req)
	require.NoError(t, err)
	require.True(t, changed)
	require.Len(t, req["tools"], 3)
}

func TestFlattenResponsesNamespacesExcept_RejectsCollisions(t *testing.T) {
	tests := []struct {
		name  string
		tools []any
	}{
		{
			name: "top level",
			tools: []any{
				map[string]any{"type": "function", "name": "mcp__read"},
				map[string]any{"type": "namespace", "name": "mcp", "tools": []any{map[string]any{"type": "function", "name": "read"}}},
			},
		},
		{
			name: "two namespaces",
			tools: []any{
				map[string]any{"type": "namespace", "name": "mcp", "tools": []any{map[string]any{"type": "function", "name": "fs__read"}}},
				map[string]any{"type": "namespace", "name": "mcp__fs", "children": []any{map[string]any{"type": "function", "name": "read"}}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, changed, err := FlattenResponsesNamespaces(map[string]any{"tools": tt.tools})
			require.Error(t, err)
			require.False(t, changed)
			require.Contains(t, err.Error(), "conflict")
			require.NotContains(t, err.Error(), "mcp")
			require.NotContains(t, err.Error(), "read")
		})
	}
}

func TestFlattenResponsesNamespaces_RejectsUnencodableDuplicateDefinition(t *testing.T) {
	req := map[string]any{"tools": []any{map[string]any{
		"type": "namespace", "name": "mcp",
		"tools": []any{
			map[string]any{"type": "function", "name": "read", "metadata": func() {}},
			map[string]any{"type": "function", "name": "read", "metadata": func() {}},
		},
	}}}

	_, changed, err := FlattenResponsesNamespaces(req)
	require.Error(t, err)
	require.False(t, changed)
	require.Contains(t, err.Error(), "encode namespace tool definition")
}

func TestFlattenResponsesNamespaces_UsesBoundedUTF8Name(t *testing.T) {
	namespace := strings.Repeat("界", 20)
	name := strings.Repeat("名", 20)
	req := map[string]any{"tools": []any{map[string]any{
		"type": "namespace", "name": namespace,
		"tools": []any{map[string]any{"type": "function", "name": name}},
	}}}

	names, changed, err := FlattenResponsesNamespaces(req)
	require.NoError(t, err)
	require.True(t, changed)
	flat := req["tools"].([]any)[0].(map[string]any)["name"].(string)
	require.LessOrEqual(t, len(flat), 64)
	require.True(t, json.Valid([]byte(`"`+flat+`"`)))
	require.Equal(t, ResponsesNamespaceName{Namespace: namespace, Name: name}, names[flat])
}

func TestRestoreResponsesNamespaceCalls_PreservesNumberSpelling(t *testing.T) {
	payload := []byte(`{"output":[{"type":"function_call","name":"mcp__read","arguments":"{}","integer":9007199254740993,"scientific":1.25e+42}]}`)

	restored, changed, err := RestoreResponsesNamespaceCalls(payload, map[string]ResponsesNamespaceName{
		"mcp__read": {Namespace: "mcp", Name: "read"},
	})
	require.NoError(t, err)
	require.True(t, changed)
	var root map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(restored)))
	decoder.UseNumber()
	require.NoError(t, decoder.Decode(&root))
	item := root["output"].([]any)[0].(map[string]any)
	require.Equal(t, json.Number("9007199254740993"), item["integer"])
	require.Equal(t, json.Number("1.25e+42"), item["scientific"])
}

func TestRestoreResponsesNamespaceCalls_RestoresOnlyVerifiedCallItems(t *testing.T) {
	payload := []byte(`{
		"type":"response.completed",
		"response":{"output":[
			{"type":"function_call","name":"mcp__read","arguments":"{}"},
			{"type":"message","name":"mcp__read","namespace":"message-meta","content":[{"type":"output_text","text":"ok","namespace":"nested-meta"}]}
		]},
		"item":{"type":"function_call","name":"mcp__read"},
		"metadata":{"type":"function_call","name":"mcp__read","namespace":"client-meta"}
	}`)

	restored, changed, err := RestoreResponsesNamespaceCalls(payload, map[string]ResponsesNamespaceName{
		"mcp__read": {Namespace: "mcp", Name: "read"},
	})
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "read", jsonPathString(t, restored, "response", "output", 0, "name"))
	require.Equal(t, "mcp", jsonPathString(t, restored, "response", "output", 0, "namespace"))
	require.Equal(t, "read", jsonPathString(t, restored, "item", "name"))
	require.Equal(t, "mcp", jsonPathString(t, restored, "item", "namespace"))
	require.Equal(t, "mcp__read", jsonPathString(t, restored, "response", "output", 1, "name"))
	require.Equal(t, "message-meta", jsonPathString(t, restored, "response", "output", 1, "namespace"))
	require.Equal(t, "nested-meta", jsonPathString(t, restored, "response", "output", 1, "content", 0, "namespace"))
	require.Equal(t, "mcp__read", jsonPathString(t, restored, "metadata", "name"))
	require.Equal(t, "client-meta", jsonPathString(t, restored, "metadata", "namespace"))
}

func jsonPathString(t *testing.T, payload []byte, path ...any) string {
	t.Helper()
	var value any
	require.NoError(t, json.Unmarshal(payload, &value))
	for _, part := range path {
		switch key := part.(type) {
		case string:
			value = value.(map[string]any)[key]
		case int:
			value = value.([]any)[key]
		}
	}
	text, ok := value.(string)
	require.True(t, ok)
	return text
}
