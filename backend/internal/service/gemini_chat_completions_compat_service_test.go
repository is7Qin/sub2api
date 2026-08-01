package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

func TestGeminiResponseToChatCompletionsPreservesInlineImageOnly(t *testing.T) {
	geminiResp, rawData := geminiResponseWithParts(t, []any{
		map[string]any{"inlineData": map[string]any{"mimeType": "image/png", "data": "aW1hZ2U="}},
	})

	got, _, err := geminiResponseToChatCompletions(geminiResp, "gemini-test", rawData, nil)
	require.NoError(t, err)
	require.Equal(t, "![image](data:image/png;base64,aW1hZ2U=)", chatCompletionContent(t, got))
	require.Equal(t, "stop", got.Choices[0].FinishReason)
}

func TestGeminiResponseToChatCompletionsPreservesTextImageOrder(t *testing.T) {
	geminiResp, rawData := geminiResponseWithParts(t, []any{
		map[string]any{"text": "before\n"},
		map[string]any{"inlineData": map[string]any{"mimeType": "image/webp", "data": "d2VicA=="}},
		map[string]any{"text": "\nafter"},
	})

	got, _, err := geminiResponseToChatCompletions(geminiResp, "gemini-test", rawData, nil)
	require.NoError(t, err)
	require.Equal(t, "before\n![image](data:image/webp;base64,d2VicA==)\nafter", chatCompletionContent(t, got))
}

func TestConvertGeminiToClaudeMessagePreservesInlineImageFunctionOrdering(t *testing.T) {
	tests := []struct {
		name      string
		parts     []any
		wantTypes []string
		wantTexts map[int]string
		toolIndex int
	}{
		{
			name: "image at intermediate function boundary",
			parts: []any{
				map[string]any{"text": "before"},
				map[string]any{"inlineData": map[string]any{"mimeType": "image/png", "data": "aW1hZ2U="}},
				map[string]any{"functionCall": map[string]any{"name": "get_weather", "args": map[string]any{"city": "Paris"}}},
				map[string]any{"text": "after"},
			},
			wantTypes: []string{"text", "text", "tool_use", "text"},
			wantTexts: map[int]string{
				0: "before",
				1: "![image](data:image/png;base64,aW1hZ2U=)",
				3: "after",
			},
			toolIndex: 2,
		},
		{
			name: "image at final function boundary",
			parts: []any{
				map[string]any{"text": "before"},
				map[string]any{"functionCall": map[string]any{"name": "get_weather", "args": map[string]any{"city": "Paris"}}},
				map[string]any{"inlineData": map[string]any{"mimeType": "image/jpeg", "data": "anBlZw=="}},
			},
			wantTypes: []string{"text", "tool_use", "text"},
			wantTexts: map[int]string{
				0: "before",
				2: "![image](data:image/jpeg;base64,anBlZw==)",
			},
			toolIndex: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			geminiResp, rawData := geminiResponseWithParts(t, tt.parts)

			got, _ := convertGeminiToClaudeMessage(geminiResp, "gemini-test", rawData, true)
			blocks := claudeContentBlocks(t, got)
			require.Len(t, blocks, len(tt.wantTypes))
			for i, wantType := range tt.wantTypes {
				require.Equal(t, wantType, blocks[i]["type"])
			}
			for i, wantText := range tt.wantTexts {
				require.Equal(t, wantText, blocks[i]["text"])
			}
			require.Equal(t, "get_weather", blocks[tt.toolIndex]["name"])
			require.Equal(t, "tool_use", got["stop_reason"])
		})
	}
}

func TestGeminiResponseToChatCompletionsAllowsExactRasterMIMETypes(t *testing.T) {
	tests := []struct {
		mimeType string
		data     string
	}{
		{mimeType: "image/gif", data: "Z2lm"},
		{mimeType: "image/jpeg", data: "anBlZw=="},
		{mimeType: "image/png", data: "cG5n"},
		{mimeType: "image/webp", data: "d2VicA=="},
	}

	for _, tt := range tests {
		t.Run(tt.mimeType, func(t *testing.T) {
			geminiResp, rawData := geminiResponseWithParts(t, []any{
				map[string]any{"inlineData": map[string]any{"mimeType": tt.mimeType, "data": tt.data}},
			})

			got, _, err := geminiResponseToChatCompletions(geminiResp, "gemini-test", rawData, nil)
			require.NoError(t, err)
			require.Equal(t, "![image](data:"+tt.mimeType+";base64,"+tt.data+")", chatCompletionContent(t, got))
		})
	}
}

func TestGeminiResponseToChatCompletionsOmitsInvalidInlineData(t *testing.T) {
	tests := []struct {
		name       string
		inlineData map[string]any
	}{
		{
			name:       "SVG MIME type",
			inlineData: map[string]any{"mimeType": "image/svg+xml", "data": "PHN2Zz48L3N2Zz4="},
		},
		{
			name:       "MIME parameters",
			inlineData: map[string]any{"mimeType": "image/png; charset=utf-8", "data": "aW1hZ2U="},
		},
		{
			name:       "blank data",
			inlineData: map[string]any{"mimeType": "image/png", "data": ""},
		},
		{
			name:       "malformed base64",
			inlineData: map[string]any{"mimeType": "image/png", "data": "not-valid-base64!!!"},
		},
		{
			name:       "non-standard unpadded base64",
			inlineData: map[string]any{"mimeType": "image/png", "data": "aW1hZ2U"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			geminiResp, rawData := geminiResponseWithParts(t, []any{
				map[string]any{"text": "before"},
				map[string]any{"inlineData": tt.inlineData},
				map[string]any{"text": "after"},
			})

			got, _, err := geminiResponseToChatCompletions(geminiResp, "gemini-test", rawData, nil)
			require.NoError(t, err)
			require.Equal(t, "beforeafter", chatCompletionContent(t, got))
			require.Equal(t, "stop", got.Choices[0].FinishReason)
		})
	}
}

func TestConvertGeminiToClaudeMessageOmitsInlineDataForAnthropicMessages(t *testing.T) {
	geminiResp, rawData := geminiResponseWithParts(t, []any{
		map[string]any{"text": "before"},
		map[string]any{"inlineData": map[string]any{"mimeType": "image/png", "data": "aW1hZ2U="}},
		map[string]any{"functionCall": map[string]any{"name": "get_weather", "args": map[string]any{"city": "Paris"}}},
		map[string]any{"text": "after"},
	})

	got, _ := convertGeminiToClaudeMessage(geminiResp, "gemini-test", rawData, false)
	blocks := claudeContentBlocks(t, got)
	require.Len(t, blocks, 3)
	require.Equal(t, map[string]any{"type": "text", "text": "before"}, blocks[0])
	require.Equal(t, "tool_use", blocks[1]["type"])
	require.Equal(t, "get_weather", blocks[1]["name"])
	require.Equal(t, map[string]any{"type": "text", "text": "after"}, blocks[2])
	require.Equal(t, "tool_use", got["stop_reason"])
}

func TestGeminiResponseToChatCompletionsRetainsToolBehaviorWithInlineImage(t *testing.T) {
	geminiResp, rawData := geminiResponseWithParts(t, []any{
		map[string]any{"text": "checking\n"},
		map[string]any{"inlineData": map[string]any{"mimeType": "image/gif", "data": "Z2lm"}},
		map[string]any{"functionCall": map[string]any{
			"name": "get_weather",
			"args": map[string]any{"city": "Paris"},
		}},
	})

	got, _, err := geminiResponseToChatCompletions(geminiResp, "gemini-test", rawData, nil)
	require.NoError(t, err)
	require.Equal(t, "checking\n![image](data:image/gif;base64,Z2lm)", chatCompletionContent(t, got))
	require.Equal(t, "tool_calls", got.Choices[0].FinishReason)
	require.Len(t, got.Choices[0].Message.ToolCalls, 1)
	require.Equal(t, "get_weather", got.Choices[0].Message.ToolCalls[0].Function.Name)
	require.JSONEq(t, `{"city":"Paris"}`, got.Choices[0].Message.ToolCalls[0].Function.Arguments)
}

func TestCollectGeminiSSEPreservesOrderedPartsForNonStreamingChat(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"before"}]}}]}`,
		`data: {"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"aW1hZ2U="}}]}}]}`,
		`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"get_weather","args":{"city":"Paris"}}}]}}]}`,
		`data: {"candidates":[{"content":{"parts":[{"text":"after"}]},"finishReason":"STOP"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")

	collected, _, err := collectGeminiChatCompletionsSSE(strings.NewReader(stream), false)
	require.NoError(t, err)
	rawData, err := json.Marshal(collected)
	require.NoError(t, err)

	got, _, err := geminiResponseToChatCompletions(collected, "gemini-test", rawData, nil)
	require.NoError(t, err)
	require.Equal(t, "before![image](data:image/png;base64,aW1hZ2U=)after", chatCompletionContent(t, got))
	require.Equal(t, "tool_calls", got.Choices[0].FinishReason)
	require.Len(t, got.Choices[0].Message.ToolCalls, 1)
	require.Equal(t, "get_weather", got.Choices[0].Message.ToolCalls[0].Function.Name)
}

func TestCollectGeminiSSEKeepsBaselineNonChatAggregation(t *testing.T) {
	tests := []struct {
		name    string
		isOAuth bool
		wrap    func(string) string
	}{
		{name: "native Gemini", isOAuth: false, wrap: func(payload string) string { return payload }},
		{name: "Anthropic Messages OAuth", isOAuth: true, wrap: func(payload string) string {
			return `{"response":` + payload + `}`
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stream := strings.Join([]string{
				"data: " + tt.wrap(`{"candidates":[{"content":{"parts":[{"text":"before"}]}}]}`),
				"data: " + tt.wrap(`{"candidates":[{"content":{"parts":[{"text":"before"},{"inlineData":{"mimeType":"image/png","data":"aW1hZ2U="}}]}}]}`),
				"data: " + tt.wrap(`{"candidates":[{"content":{"parts":[{"text":"beforeafter"}]},"finishReason":"STOP"}]}`),
				`data: [DONE]`,
				``,
			}, "\n")

			collected, _, err := collectGeminiSSE(strings.NewReader(stream), tt.isOAuth)
			require.NoError(t, err)
			require.Equal(t, []map[string]any{{"text": "beforebeforebeforeafter"}}, extractGeminiParts(collected))
			require.Equal(t, "STOP", collected["candidates"].([]any)[0].(map[string]any)["finishReason"])
		})
	}
}

func TestCollectGeminiSSEDeduplicatesCumulativeTextAroundImage(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"before"}]}}]}`,
		`data: {"candidates":[{"content":{"parts":[{"text":"before"},{"inlineData":{"mimeType":"image/webp","data":"d2VicA=="}}]}}]}`,
		`data: {"candidates":[{"content":{"parts":[{"text":"beforeafter"}]},"finishReason":"STOP"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")

	collected, _, err := collectGeminiChatCompletionsSSE(strings.NewReader(stream), false)
	require.NoError(t, err)
	rawData, err := json.Marshal(collected)
	require.NoError(t, err)

	got, _, err := geminiResponseToChatCompletions(collected, "gemini-test", rawData, nil)
	require.NoError(t, err)
	require.Equal(t, "before![image](data:image/webp;base64,d2VicA==)after", chatCompletionContent(t, got))
}

func TestGeminiResponseToChatCompletionsUsesFirstCandidateOnly(t *testing.T) {
	geminiResp := map[string]any{
		"candidates": []any{
			map[string]any{
				"content":      map[string]any{"parts": []any{map[string]any{"text": "first"}}},
				"finishReason": "MAX_TOKENS",
			},
			map[string]any{
				"content": map[string]any{"parts": []any{
					map[string]any{"text": "second"},
					map[string]any{"inlineData": map[string]any{"mimeType": "image/png", "data": "aW1hZ2U="}},
				}},
				"finishReason": "STOP",
			},
		},
	}
	rawData, err := json.Marshal(geminiResp)
	require.NoError(t, err)

	got, _, err := geminiResponseToChatCompletions(geminiResp, "gemini-test", rawData, nil)
	require.NoError(t, err)
	require.Equal(t, "first", chatCompletionContent(t, got))
	require.Equal(t, "length", got.Choices[0].FinishReason)
}

func TestGeminiInlineImageMarkdownAcceptsLargeStandardBase64(t *testing.T) {
	data := strings.Repeat("QUJD", 256*1024)
	markdown, ok := geminiInlineImageMarkdown(map[string]any{
		"mimeType": "image/png",
		"data":     data,
	})
	require.True(t, ok)
	require.Equal(t, "![image](data:image/png;base64,"+data+")", markdown)
}

func geminiResponseWithParts(t *testing.T, parts []any) (map[string]any, []byte) {
	t.Helper()
	geminiResp := map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"parts": parts},
			"finishReason": "STOP",
		}},
	}
	rawData, err := json.Marshal(geminiResp)
	require.NoError(t, err)
	return geminiResp, rawData
}

func chatCompletionContent(t *testing.T, response *apicompat.ChatCompletionsResponse) string {
	t.Helper()
	require.Len(t, response.Choices, 1)
	var content string
	require.NoError(t, json.Unmarshal(response.Choices[0].Message.Content, &content))
	return content
}

func claudeContentBlocks(t *testing.T, response map[string]any) []map[string]any {
	t.Helper()
	rawBlocks, ok := response["content"].([]any)
	require.True(t, ok)
	blocks := make([]map[string]any, 0, len(rawBlocks))
	for _, rawBlock := range rawBlocks {
		block, ok := rawBlock.(map[string]any)
		require.True(t, ok)
		blocks = append(blocks, block)
	}
	return blocks
}
