package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExtractOpenAIUsageFromJSONBytes_ExcludesHostedImageGeneration(t *testing.T) {
	body := []byte(`{"id":"chatcmpl_1","object":"chat.completion","usage":{"prompt_tokens":100,"completion_tokens":50},"tool_usage":{"image_gen":{"input_tokens_details":{"image_tokens":12},"output_tokens_details":{"image_tokens":30}}}}`)

	usage, ok := extractOpenAIUsageFromJSONBytes(body)
	require.True(t, ok)
	require.Equal(t, 100, usage.InputTokens)
	require.Equal(t, 50, usage.OutputTokens)
	require.Zero(t, usage.ImageInputTokens)
	require.Zero(t, usage.ImageOutputTokens)
}

func TestExtractOpenAIResponsesUsageFromJSONBytes_HostedImageGeneration(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "response",
			body: `{"usage":{"input_tokens":5000,"output_tokens":200},"tool_usage":{"image_gen":{"input_tokens_details":{"image_tokens":2800},"output_tokens_details":{"image_tokens":150}}}}`,
		},
		{
			name: "terminal event",
			body: `{"type":"response.completed","response":{"usage":{"input_tokens":5000,"output_tokens":200},"tool_usage":{"image_gen":{"input_tokens_details":{"image_tokens":2800},"output_tokens_details":{"image_tokens":150}}}}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usage, ok := extractOpenAIResponsesUsageFromJSONBytes([]byte(tt.body))
			require.True(t, ok)
			require.Equal(t, 5000, usage.InputTokens)
			require.Equal(t, 200, usage.OutputTokens)
			require.Equal(t, 2800, usage.ImageInputTokens)
			require.Equal(t, 150, usage.ImageOutputTokens)
		})
	}
}

func TestExtractOpenAIResponsesUsageFromJSONBytes_HostedImageGenerationDoesNotOverride(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":100,"output_tokens":50,"input_tokens_details":{"image_tokens":12},"output_tokens_details":{"image_tokens":30}},"tool_usage":{"image_gen":{"input_tokens_details":{"image_tokens":200},"output_tokens_details":{"image_tokens":100}}}}`)
	usage, ok := extractOpenAIResponsesUsageFromJSONBytes(body)
	require.True(t, ok)
	require.Equal(t, 100, usage.InputTokens)
	require.Equal(t, 50, usage.OutputTokens)
	require.Equal(t, 12, usage.ImageInputTokens)
	require.Equal(t, 30, usage.ImageOutputTokens)
}

func TestExtractOpenAIResponsesUsageFromJSONBytes_HostedImageGenerationIgnoresInvalid(t *testing.T) {
	for _, imageGen := range []string{
		"null",
		`"invalid"`,
		`{"input_tokens_details":{"image_tokens":-1},"output_tokens_details":{"image_tokens":-2}}`,
		`{"input_tokens_details":{"image_tokens":"12"}}`,
		`{"input_tokens_details":{"image_tokens":1.5}}`,
		`{"input_tokens_details":{"image_tokens":999999999999999999999999999999999999}}`,
	} {
		body := []byte(`{"usage":{"input_tokens":100,"output_tokens":50},"tool_usage":{"image_gen":` + imageGen + `}}`)
		usage, ok := extractOpenAIResponsesUsageFromJSONBytes(body)
		require.True(t, ok)
		require.Equal(t, 0, usage.ImageInputTokens)
		require.Equal(t, 0, usage.ImageOutputTokens)
	}
}
