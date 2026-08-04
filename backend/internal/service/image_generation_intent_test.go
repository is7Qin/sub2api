package service

import (
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsImageGenerationIntent(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		model    string
		body     []byte
		want     bool
	}{
		{
			name:     "images endpoint",
			endpoint: "/v1/images/generations",
			body:     []byte(`{"model":"gpt-image-2"}`),
			want:     true,
		},
		{
			name:     "image model",
			endpoint: "/v1/responses",
			model:    "gpt-image-2",
			body:     []byte(`{"model":"gpt-image-2"}`),
			want:     true,
		},
		{
			name:     "image tool",
			endpoint: "/v1/responses",
			model:    "gpt-5.4",
			body:     []byte(`{"model":"gpt-5.4","tools":[{"type":"image_generation"}]}`),
			want:     true,
		},
		{
			name:     "image tool choice",
			endpoint: "/v1/responses",
			model:    "gpt-5.4",
			body:     []byte(`{"model":"gpt-5.4","tool_choice":{"type":"image_generation"}}`),
			want:     true,
		},
		{
			name:     "required tool choice alone is text",
			endpoint: "/v1/responses",
			model:    "gpt-5.4",
			body:     []byte(`{"model":"gpt-5.4","tool_choice":"required"}`),
			want:     false,
		},
		{
			name:     "text only gpt 5.4",
			endpoint: "/v1/responses",
			model:    "gpt-5.4",
			body:     []byte(`{"model":"gpt-5.4","input":"write code"}`),
			want:     false,
		},
		{
			name:     "codex image_gen namespace top level tool",
			endpoint: "/v1/responses",
			model:    "gpt-5.5",
			body:     []byte(`{"model":"gpt-5.5","tools":[{"type":"namespace","name":"image_gen","tools":[{"type":"function","name":"imagegen"}]}]}`),
			want:     true,
		},
		{
			name:     "codex image_gen namespace additional tools",
			endpoint: "/v1/responses",
			model:    "gpt-5.5",
			body:     []byte(`{"model":"gpt-5.5","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"image_gen","tools":[{"type":"function","name":"imagegen"}]}]}]}`),
			want:     true,
		},
		{
			name:     "native image_generation additional tools",
			endpoint: "/v1/responses",
			model:    "gpt-5.5",
			body:     []byte(`{"model":"gpt-5.5","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"image_generation","output_format":"png"}]}]}`),
			want:     true,
		},
		{
			name:     "non image namespace remains text intent",
			endpoint: "/v1/responses",
			model:    "gpt-5.5",
			body:     []byte(`{"model":"gpt-5.5","tools":[{"type":"namespace","name":"code_tools","tools":[{"type":"function","name":"run"}]}]}`),
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, IsImageGenerationIntent(tt.endpoint, tt.model, tt.body))
		})
	}
}

func TestOpenAIRequestBodyMayContainAdditionalImageTooling(t *testing.T) {
	textOnlyInput := []byte(`{"model":"gpt-5.5","input":[{"type":"message","content":[{"type":"input_text","text":"write a long plain text answer"}]}]}`)
	require.False(t, openAIRequestBodyMayContainAdditionalImageTooling(textOnlyInput))

	largeTextOnlyInput := []byte(`{"model":"gpt-5.5","input":[{"type":"message","content":[{"type":"input_text","text":"` + strings.Repeat("ordinary text ", 20000) + `"}]}]}`)
	require.False(t, openAIRequestBodyMayContainAdditionalImageTooling(largeTextOnlyInput))

	require.True(t, openAIRequestBodyMayContainAdditionalImageTooling(
		[]byte(`{"input":[{"type":"additional_tools","tools":[{"type":"image_generation"}]}]}`),
	))
	require.True(t, openAIRequestBodyMayContainAdditionalImageTooling(
		[]byte(`{"input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"image_gen"}]}]}`),
	))
}

// openAIRequestBodyMayContainAdditionalImageToolingBruteForce 是旧逐字节实现的副本，仅用于差分验证。
func openAIRequestBodyMayContainAdditionalImageToolingBruteForce(body []byte) bool {
	const (
		additionalToolsMarker = "additional_tools"
		imageGenerationMarker = "image_generation"
		imageGenMarker        = "image_gen"
		namespaceMarker       = "namespace"
	)

	seenAdditionalTools := false
	seenImageMarker := false
	for i := 0; i < len(body); i++ {
		if !seenAdditionalTools && hasMarkerAt(body, i, additionalToolsMarker) {
			seenAdditionalTools = true
		}
		if !seenImageMarker &&
			(hasMarkerAt(body, i, imageGenerationMarker) ||
				hasMarkerAt(body, i, imageGenMarker) ||
				hasMarkerAt(body, i, namespaceMarker)) {
			seenImageMarker = true
		}
		if seenAdditionalTools && seenImageMarker {
			return true
		}
	}
	return false
}

func TestOpenAIRequestBodyMayContainAdditionalImageToolingMatchesReference(t *testing.T) {
	table := []struct {
		name string
		body []byte
	}{
		{name: "empty nil", body: nil},
		{name: "empty slice", body: []byte{}},
		{name: "single byte", body: []byte("a")},
		{name: "additional_tools at start", body: []byte(`additional_tools{"input":[]}`)},
		{name: "additional_tools at end", body: []byte(`{"input":[]}additional_tools`)},
		{name: "image marker at start", body: []byte(`image_generation{"type":"additional_tools"}`)},
		{name: "image_gen at boundary", body: []byte(`{"type":"additional_tools"image_gen`)},
		{name: "marker truncated at end", body: []byte(`{"type":"additional_too`)},
		{name: "partial marker only", body: []byte(`additional_tool`)},
		{name: "namespace only", body: []byte(`{"namespace":true}`)},
		{name: "additional_tools only", body: []byte(`{"type":"additional_tools"}`)},
		{name: "both categories", body: []byte(`{"type":"additional_tools","tools":[{"type":"image_generation"}]}`)},
		{name: "namespace plus additional_tools", body: []byte(`{"type":"additional_tools","x":"namespace"}`)},
		{name: "marker in json key", body: []byte(`{"additional_tools":1,"image_gen":2}`)},
		{name: "marker in string value", body: []byte(`{"x":"additional_tools","y":"namespace"}`)},
		{name: "overlapping markers", body: []byte(`additional_toolsimage_generation`)},
		{name: "image_generation subsumes image_gen", body: []byte(`image_generation`)},
		{name: "marker inside larger word", body: []byte(`xadditional_toolsy namespacez`)},
		{name: "mutated marker", body: []byte(`additional_toolS`)},
		{name: "quoted marker", body: []byte(`"additional_tools"`)},
	}
	for _, tt := range table {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, openAIRequestBodyMayContainAdditionalImageToolingBruteForce(tt.body), openAIRequestBodyMayContainAdditionalImageTooling(tt.body))
		})
	}

	// 随机差分：种子固定，字母表偏向标记及其残缺片段，覆盖边界、重叠、跨键值等位置。
	rng := rand.New(rand.NewPCG(0x9e3779b97f4a7c15, 0xdeadbeefcafef00d))
	fragments := []string{
		"additional_tools", "additional_tool", "additional_toolz",
		"image_generation", "image_generatio", "image_gen", "image_ge",
		"namespace", "namespa", "namespacee",
	}
	alphabet := []byte(`{"input":[],"type":"tools":"` + "abcdefghijklmnopqrstuvwxyz_0123456789")
	for i := 0; i < 5000; i++ {
		n := rng.IntN(400)
		body := make([]byte, 0, n)
		for len(body) < n {
			if rng.IntN(3) == 0 {
				body = append(body, fragments[rng.IntN(len(fragments))]...)
			} else {
				body = append(body, alphabet[rng.IntN(len(alphabet))])
			}
		}
		want := openAIRequestBodyMayContainAdditionalImageToolingBruteForce(body)
		if got := openAIRequestBodyMayContainAdditionalImageTooling(body); want != got {
			t.Fatalf("body %q: reference=%v new=%v", body, want, got)
		}
	}
}

func TestClassifyOpenAIForwardImageIntentSelectsCurrentRequestRepresentation(t *testing.T) {
	rawImageBody := []byte(`{"model":"gpt-5.5","tools":[{"type":"image_generation"}]}`)
	textOnlyMap := map[string]any{"model": "gpt-5.5", "input": "write code"}

	require.True(t, classifyOpenAIForwardImageIntent("gpt-5.5", "gpt-5.5", rawImageBody, nil))
	require.False(t, classifyOpenAIForwardImageIntent("gpt-5.5", "gpt-5.5", rawImageBody, textOnlyMap))
	require.True(t, classifyOpenAIForwardImageIntent("gpt-5.5", "gpt-image-2", nil, textOnlyMap))
}

func TestIsImageGenerationIntentMapDetectsCodexImageGenNamespace(t *testing.T) {
	tests := []struct {
		name    string
		reqBody map[string]any
		want    bool
	}{
		{
			name: "top level namespace",
			reqBody: map[string]any{
				"model": "gpt-5.5",
				"tools": []any{
					map[string]any{
						"type": "namespace",
						"name": "image_gen",
						"tools": []any{
							map[string]any{"type": "function", "name": "imagegen"},
						},
					},
				},
			},
			want: true,
		},
		{
			name: "additional tools namespace",
			reqBody: map[string]any{
				"model": "gpt-5.5",
				"input": []any{
					map[string]any{
						"type": "additional_tools",
						"tools": []any{
							map[string]any{"type": "namespace", "name": "image_gen"},
						},
					},
				},
			},
			want: true,
		},
		{
			name: "non image namespace",
			reqBody: map[string]any{
				"model": "gpt-5.5",
				"tools": []any{
					map[string]any{"type": "namespace", "name": "code_tools"},
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, IsImageGenerationIntentMap("/v1/responses", "gpt-5.5", tt.reqBody))
		})
	}
}

func TestResolveOpenAIResponsesImageBillingConfigUsesCurrentBodyModel(t *testing.T) {
	imageModel, imageSize, err := resolveOpenAIResponsesImageBillingConfigFromBody(
		[]byte(`{"model":"mapped-image-model","tools":[{"type":"image_generation","size":"1024x1024"}]}`),
		"requested-model",
	)
	require.NoError(t, err)
	require.Equal(t, "mapped-image-model", imageModel)
	require.Equal(t, "1K", imageSize)
}

func TestResolveOpenAIResponsesImageBillingConfigToolModelWins(t *testing.T) {
	imageModel, imageSize, err := resolveOpenAIResponsesImageBillingConfigFromBody(
		[]byte(`{"model":"mapped-text-model","tools":[{"type":"image_generation","model":"gpt-image-2","size":"1536x1024"}]}`),
		"requested-model",
	)
	require.NoError(t, err)
	require.Equal(t, "gpt-image-2", imageModel)
	require.Equal(t, "2K", imageSize)
}

func TestResolveOpenAIResponsesImageBillingConfigNamespaceDefaultsToImageModel(t *testing.T) {
	cfg, err := resolveOpenAIResponsesImageBillingConfigDetailedFromBody(
		[]byte(`{"model":"mapped-text-model","tools":[{"type":"namespace","name":"image_gen","tools":[{"type":"function","name":"imagegen"}]}]}`),
		"requested-model",
	)
	require.NoError(t, err)
	require.Equal(t, "gpt-image-2", cfg.Model)
	require.Equal(t, "2K", cfg.SizeTier)
	require.Equal(t, "", cfg.InputSize)
}

func TestResolveOpenAIResponsesImageBillingConfigNestedNamespaceDefaultsToImageModel(t *testing.T) {
	cfg, err := resolveOpenAIResponsesImageBillingConfigDetailedFromBody(
		[]byte(`{"model":"mapped-text-model","input":[{"type":"message","content":"draw"},{"type":"additional_tools","tools":[{"type":"namespace","name":"image_gen","tools":[{"type":"function","name":"imagegen"}]}]}]}`),
		"requested-model",
	)
	require.NoError(t, err)
	require.Equal(t, "gpt-image-2", cfg.Model)
	require.Equal(t, "2K", cfg.SizeTier)
	require.Equal(t, "", cfg.InputSize)
}

func TestResolveOpenAIResponsesImageBillingConfigNamespaceDefaultsToImageModelMap(t *testing.T) {
	cfg, err := resolveOpenAIResponsesImageBillingConfigDetailed(map[string]any{
		"model": "mapped-text-model",
		"tools": []any{
			map[string]any{
				"type": "namespace",
				"name": "image_gen",
				"tools": []any{
					map[string]any{"type": "function", "name": "imagegen"},
				},
			},
		},
	}, "requested-model")
	require.NoError(t, err)
	require.Equal(t, "gpt-image-2", cfg.Model)
	require.Equal(t, "2K", cfg.SizeTier)
	require.Equal(t, "", cfg.InputSize)
}

func TestResolveOpenAIResponsesImageBillingConfigNestedNamespaceDefaultsToImageModelMap(t *testing.T) {
	cfg, err := resolveOpenAIResponsesImageBillingConfigDetailed(map[string]any{
		"model": "mapped-text-model",
		"input": []any{
			map[string]any{"type": "message", "content": "draw"},
			map[string]any{
				"type": "additional_tools",
				"tools": []any{
					map[string]any{"type": "namespace", "name": "image_gen"},
				},
			},
		},
	}, "requested-model")
	require.NoError(t, err)
	require.Equal(t, "gpt-image-2", cfg.Model)
	require.Equal(t, "2K", cfg.SizeTier)
	require.Equal(t, "", cfg.InputSize)
}

func TestResolveOpenAIResponsesImageBillingConfigNestedAdditionalToolModelWins(t *testing.T) {
	cfg, err := resolveOpenAIResponsesImageBillingConfigDetailedFromBody(
		[]byte(`{"model":"mapped-text-model","input":[{"type":"message","content":"draw"},{"type":"additional_tools","tools":[{"type":"image_generation","model":"gpt-image-2","size":"3840x2160"}]}]}`),
		"requested-model",
	)
	require.NoError(t, err)
	require.Equal(t, "gpt-image-2", cfg.Model)
	require.Equal(t, "4K", cfg.SizeTier)
	require.Equal(t, "3840x2160", cfg.InputSize)
}

func TestResolveOpenAIResponsesImageBillingConfigNestedAdditionalToolModelWinsMap(t *testing.T) {
	cfg, err := resolveOpenAIResponsesImageBillingConfigDetailed(map[string]any{
		"model": "mapped-text-model",
		"input": []any{
			map[string]any{"type": "message", "content": "draw"},
			map[string]any{
				"type": "additional_tools",
				"tools": []any{
					map[string]any{"type": "image_generation", "model": "gpt-image-2", "size": "2048x1152"},
				},
			},
		},
	}, "requested-model")
	require.NoError(t, err)
	require.Equal(t, "gpt-image-2", cfg.Model)
	require.Equal(t, "2K", cfg.SizeTier)
	require.Equal(t, "2048x1152", cfg.InputSize)
}

func TestResolveOpenAIResponsesImageBillingConfigFromBodyIgnoresUnrelatedLargeInput(t *testing.T) {
	cfg, err := resolveOpenAIResponsesImageBillingConfigDetailedFromBody(
		[]byte(`{"model":"mapped-text-model","tools":[{"type":"image_generation","model":"gpt-image-2","size":"2048x1152"}],"input":[{"type":"message","content":[{"type":"input_text","text":"hi","nonce":1e1000000}]}]}`),
		"requested-model",
	)
	require.NoError(t, err)
	require.Equal(t, "gpt-image-2", cfg.Model)
	require.Equal(t, "2K", cfg.SizeTier)
	require.Equal(t, "2048x1152", cfg.InputSize)
}

func TestResolveOpenAIResponsesImageBillingConfigSupportsOfficialAndCustomSizes(t *testing.T) {
	tests := []struct {
		name     string
		body     []byte
		wantTier string
	}{
		{
			name:     "official 2k landscape",
			body:     []byte(`{"model":"gpt-5.4","tools":[{"type":"image_generation","model":"gpt-image-2","size":"2048x1152"}]}`),
			wantTier: "2K",
		},
		{
			name:     "official 4k landscape",
			body:     []byte(`{"model":"gpt-5.4","tools":[{"type":"image_generation","model":"gpt-image-2","size":"3840x2160"}]}`),
			wantTier: "4K",
		},
		{
			name:     "custom valid 2k",
			body:     []byte(`{"model":"gpt-5.5","tools":[{"type":"image_generation","model":"gpt-image-2","size":"1280x768"}]}`),
			wantTier: "2K",
		},
		{
			name:     "default image tool model supports flexible size",
			body:     []byte(`{"model":"gpt-5.4","tools":[{"type":"image_generation","size":"2048x1152"}]}`),
			wantTier: "2K",
		},
		{
			name:     "top level image size is moved into billing",
			body:     []byte(`{"model":"gpt-image-2","size":"2048x2048","tools":[{"type":"image_generation","model":"gpt-image-2"}]}`),
			wantTier: "2K",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			imageModel, imageSize, err := resolveOpenAIResponsesImageBillingConfigFromBody(tt.body, "requested-model")
			require.NoError(t, err)
			require.NotEmpty(t, imageModel)
			require.Equal(t, tt.wantTier, imageSize)
		})
	}
}

func TestResolveOpenAIResponsesImageBillingConfigDoesNotRejectUnknownSizes(t *testing.T) {
	imageModel, imageSize, err := resolveOpenAIResponsesImageBillingConfigFromBody(
		[]byte(`{"model":"gpt-5.4","tools":[{"type":"image_generation","model":"gpt-image-1.5","size":"2048x1152"}]}`),
		"requested-model",
	)
	require.NoError(t, err)
	require.Equal(t, "gpt-image-1.5", imageModel)
	require.Equal(t, "2K", imageSize)
}

func TestOpenAIImageOutputCounterDeduplicatesFinalImages(t *testing.T) {
	counter := newOpenAIImageOutputCounter()
	counter.AddSSEData([]byte(`{"type":"response.image_generation_call.partial_image","partial_image_b64":"abc"}`))
	counter.AddSSEData([]byte(`{"type":"response.output_item.done","item":{"id":"ig_1","type":"image_generation_call","result":"final-a","size":"1024x1024"}}`))
	counter.AddSSEData([]byte(`{"type":"response.completed","response":{"output":[{"id":"ig_1","type":"image_generation_call","result":"final-a"},{"id":"ig_2","type":"image_generation_call","result":"final-b","size":"3840x2160"}]}}`))
	require.Equal(t, 2, counter.Count())
	require.Equal(t, []string{"1024x1024", "3840x2160"}, counter.Sizes())
}

func TestOpenAIImageOutputCounterCountsImagesAPIStreamShapes(t *testing.T) {
	counter := newOpenAIImageOutputCounter()
	counter.AddSSEData([]byte(`{"type":"image_generation.completed","id":"ig_complete","b64_json":"final-a"}`))
	counter.AddSSEData([]byte(`{"type":"response.output_item.done","item":{"id":"ig_item","type":"image_generation_call","result":"final-b"}}`))
	counter.AddSSEData([]byte(`{"type":"response.completed","response":{"output":[{"id":"ig_done","type":"image_generation_call","result":"final-c"}]}}`))
	require.Equal(t, 3, counter.Count())

	dataCounter := newOpenAIImageOutputCounter()
	dataCounter.AddSSEData([]byte(`{"data":[{"b64_json":"a"},{"b64_json":"b"}]}`))
	dataCounter.AddSSEData([]byte(`{"data":[{"b64_json":"a"},{"b64_json":"b"},{"b64_json":"c"}]}`))
	require.Equal(t, 3, dataCounter.Count())
}

func TestOpenAIImageOutputCounterCountsMultilineSSEDataPayload(t *testing.T) {
	counter := newOpenAIImageOutputCounter()
	counter.AddSSEData([]byte("{\"type\":\"image_generation.completed\",\n\"b64_json\":\"final-a\"}"))
	require.Equal(t, 1, counter.Count())
}

func TestOpenAIImageOutputCounterCountsMultilineSSEBodyPayload(t *testing.T) {
	counter := newOpenAIImageOutputCounter()
	counter.AddSSEBody(
		"data: {\"type\":\"image_generation.completed\",\n" +
			"data: \"b64_json\":\"final-a\"}\n\n" +
			"data: [DONE]\n\n",
	)
	require.Equal(t, 1, counter.Count())
}

func TestOpenAIImageOutputCounterFallsBackForInvalidMultilineSSEBody(t *testing.T) {
	counter := newOpenAIImageOutputCounter()
	counter.AddSSEBody(
		"data: {\"type\":\"image_generation.completed\",\"b64_json\":\"final-a\"}\n" +
			"data: {\"type\":\"image_generation.completed\",\"b64_json\":\"final-b\"}\n\n",
	)
	require.Equal(t, 2, counter.Count())
}

func TestCollectOpenAIResponseImageOutputSizesFromJSONBytes(t *testing.T) {
	body := []byte(`{
		"output": [
			{"id":"ig_1","type":"image_generation_call","result":"final-a","size":"3840x2160"},
			{"id":"ig_2","type":"image_generation_call","result":"final-b","size":"1024x1024"}
		]
	}`)

	require.Equal(t, 2, countOpenAIResponseImageOutputsFromJSONBytes(body))
	require.Equal(t, []string{"3840x2160", "1024x1024"}, collectOpenAIResponseImageOutputSizesFromJSONBytes(body))
}

func TestCollectOpenAIResponseImageOutputSizesFromImagesAPIData(t *testing.T) {
	body := []byte(`{
		"data": [
			{"b64_json":"final-a","size":"2048x1152"},
			{"b64_json":"final-b","size":"2048x1152"}
		]
	}`)

	require.Equal(t, 2, countOpenAIResponseImageOutputsFromJSONBytes(body))
	require.Equal(t, []string{"2048x1152", "2048x1152"}, collectOpenAIResponseImageOutputSizesFromJSONBytes(body))
}

func TestCollectOpenAIImageOutputSizesFromSSEBody(t *testing.T) {
	body := "data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"ig_1\",\"type\":\"image_generation_call\",\"result\":\"final-a\",\"size\":\"3840x2160\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"id\":\"ig_1\",\"type\":\"image_generation_call\",\"result\":\"final-a\"},{\"id\":\"ig_2\",\"type\":\"image_generation_call\",\"result\":\"final-b\",\"size\":\"1024x1024\"}]}}\n\n" +
		"data: [DONE]\n\n"

	require.Equal(t, 2, countOpenAIImageOutputsFromSSEBody(body))
	require.Equal(t, []string{"3840x2160", "1024x1024"}, collectOpenAIImageOutputSizesFromSSEBody(body))
}
