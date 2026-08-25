package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

var benchmarkImageGenerationIntentSink bool

const benchmarkImageIntentExitStatus = 599

type benchmarkImageIntentUpstream struct {
	body []byte
}

func (u *benchmarkImageIntentUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	if req != nil && req.Body != nil {
		u.body, _ = io.ReadAll(req.Body)
	}
	return &http.Response{
		StatusCode: benchmarkImageIntentExitStatus,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"error":"benchmark exit"}`)),
		Request:    req,
	}, nil
}

func (u *benchmarkImageIntentUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

func BenchmarkOpenAIGatewayServiceForwardImageIntentGate(b *testing.B) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name             string
		body             []byte
		account          *Account
		wantUpstreamBody func(*testing.B, []byte)
	}{
		{
			name:    "raw_text_responses",
			body:    []byte(`{"model":"gpt-5.5","stream":false,"instructions":"benchmark","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"` + strings.Repeat("ordinary request text ", 512) + `","nonce":9007199254740993}]}],"tools":[{"type":"function","name":"read_file"}],"tool_choice":"auto"}`),
			account: benchmarkOpenAIForwardAccount("gpt-5.5", "gpt-5.5"),
			wantUpstreamBody: func(b *testing.B, upstreamBody []byte) {
				b.Helper()
				if got := gjson.GetBytes(upstreamBody, "input.0.content.0.nonce").Raw; got != "9007199254740993" {
					b.Fatalf("raw input was decoded and re-marshaled: nonce = %q", got)
				}
			},
		},
		{
			name:    "decoded_map_spark_tool_strip",
			body:    []byte(`{"model":"gpt-5.3-codex-spark","stream":false,"instructions":"benchmark","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"ordinary request","nonce":9007199254740993}]},{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"image_gen","tools":[{"type":"function","name":"imagegen"}]}]}],"tools":[{"type":"function","name":"read_file"},{"type":"image_generation"}],"tool_choice":{"type":"image_generation"}}`),
			account: benchmarkOpenAIForwardAccount("gpt-5.3-codex-spark", "gpt-5.3-codex-spark"),
			wantUpstreamBody: func(b *testing.B, upstreamBody []byte) {
				b.Helper()
				if gjson.GetBytes(upstreamBody, `tools.#(type=="image_generation")`).Exists() || gjson.GetBytes(upstreamBody, "tool_choice").Exists() {
					b.Fatal("Spark image tooling was not stripped through the decoded-map mutation path")
				}
				if got := gjson.GetBytes(upstreamBody, "input.0.content.0.nonce").Raw; got == "9007199254740993" {
					b.Fatal("mapped fixture did not pass through decoded-map serialization")
				}
			},
		},
	}

	for _, benchmark := range cases {
		b.Run(benchmark.name, func(b *testing.B) {
			assertBenchmarkJSONValid(b, benchmark.body)
			upstream := &benchmarkImageIntentUpstream{}
			svc := benchmarkOpenAIForwardService(upstream)
			c := benchmarkOpenAIForwardContext()
			result, err := svc.Forward(context.Background(), c, benchmark.account, benchmark.body)
			if result != nil || !isBenchmarkImageIntentExit(err) {
				b.Fatalf("Forward() = (%v, %v), want deterministic upstream exit", result, err)
			}
			benchmark.wantUpstreamBody(b, upstream.body)

			contexts := make([]*gin.Context, b.N)
			for i := range contexts {
				contexts[i] = benchmarkOpenAIForwardContext()
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				upstream.body = nil
				result, err = svc.Forward(context.Background(), contexts[i], benchmark.account, benchmark.body)
				if result != nil || !isBenchmarkImageIntentExit(err) {
					b.Fatalf("Forward() = (%v, %v), want deterministic upstream exit", result, err)
				}
			}
		})
	}
}

func isBenchmarkImageIntentExit(err error) bool {
	var failoverErr *UpstreamFailoverError
	return errors.As(err, &failoverErr) && failoverErr.StatusCode == benchmarkImageIntentExitStatus
}

func BenchmarkIsImageGenerationIntent(b *testing.B) {
	textOnlyBody := []byte(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"` + strings.Repeat("ordinary request text ", 512) + `"}]}],"tools":[{"type":"function","name":"read_file"}],"tool_choice":"auto"}`)
	shortTextOnlyBody := []byte(`{"model":"gpt-5.5","input":"write a concise release note","tools":[{"type":"function","name":"read_file"}],"tool_choice":"auto"}`)
	topLevelImageToolBody := []byte(`{"model":"gpt-5.5","input":"draw a lighthouse at sunset","tools":[{"type":"image_generation","size":"1536x1024"}]}`)
	additionalToolsNamespaceBody := []byte(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"draw a lighthouse at sunset"}]},{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"image_gen","tools":[{"type":"function","name":"imagegen"}]}]}]}`)
	bodyImageModel := []byte(`{"model":"gpt-image-2","input":"draw a lighthouse at sunset"}`)

	benchmarks := []struct {
		name  string
		model string
		body  []byte
		want  bool
	}{
		{name: "responses/text_only_large_input_fast_negative", model: "gpt-5.5", body: textOnlyBody},
		{name: "responses/text_only_small_request_fast_negative", model: "gpt-5.5", body: shortTextOnlyBody},
		{name: "responses/top_level_image_tool", model: "gpt-5.5", body: topLevelImageToolBody, want: true},
		{name: "responses/additional_tools_namespace", model: "gpt-5.5", body: additionalToolsNamespaceBody, want: true},
		{name: "responses/body_image_model", model: "gpt-5.5", body: bodyImageModel, want: true},
		{name: "responses/requested_image_model_fast_path", model: "gpt-image-2", body: textOnlyBody, want: true},
	}

	for _, benchmark := range benchmarks {
		b.Run(benchmark.name, func(b *testing.B) {
			assertBenchmarkJSONValid(b, benchmark.body)
			if got := IsImageGenerationIntent(openAIResponsesEndpoint, benchmark.model, benchmark.body); got != benchmark.want {
				b.Fatalf("IsImageGenerationIntent() = %v, want %v", got, benchmark.want)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				benchmarkImageGenerationIntentSink = IsImageGenerationIntent(openAIResponsesEndpoint, benchmark.model, benchmark.body)
			}
		})
	}
}

func BenchmarkIsImageGenerationIntentMap(b *testing.B) {
	fixtures := []struct {
		name string
		body []byte
		want bool
	}{
		{name: "decoded_map_negative", body: []byte(`{"model":"gpt-5.5","input":"write code","tools":[{"type":"function","name":"read_file"}],"tool_choice":"auto"}`)},
		{name: "decoded_map_native_image_tool", body: []byte(`{"model":"gpt-5.5","input":"draw","tools":[{"type":"image_generation"}]}`), want: true},
		{name: "decoded_map_additional_tools_image_gen_namespace", body: []byte(`{"model":"gpt-5.5","input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"image_gen","tools":[{"type":"function","name":"imagegen"}]}]}]}`), want: true},
	}

	for _, fixture := range fixtures {
		b.Run(fixture.name, func(b *testing.B) {
			assertBenchmarkJSONValid(b, fixture.body)
			var reqBody map[string]any
			if err := json.Unmarshal(fixture.body, &reqBody); err != nil {
				b.Fatalf("decode benchmark fixture: %v", err)
			}
			if got := IsImageGenerationIntentMap(openAIResponsesEndpoint, "gpt-5.5", reqBody); got != fixture.want {
				b.Fatalf("IsImageGenerationIntentMap() = %v, want %v", got, fixture.want)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				benchmarkImageGenerationIntentSink = IsImageGenerationIntentMap(openAIResponsesEndpoint, "gpt-5.5", reqBody)
			}
		})
	}
}

func benchmarkOpenAIForwardService(upstream HTTPUpstream) *OpenAIGatewayService {
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	return &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
}

func benchmarkOpenAIForwardContext() *gin.Context {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
	c.Set("api_key", &APIKey{Group: &Group{AllowImageGeneration: false}})
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	return c
}

func benchmarkOpenAIForwardAccount(inboundModel string, mappedModel string) *Account {
	credentials := map[string]any{"api_key": "sk-benchmark", "base_url": "https://benchmark.invalid"}
	if mappedModel != inboundModel {
		credentials["model_mapping"] = map[string]any{inboundModel: mappedModel}
	}
	return &Account{
		ID: 1, Name: "benchmark-openai", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1,
		Credentials: credentials,
		Extra:       map[string]any{"use_responses_api": true},
	}
}

func assertBenchmarkJSONValid(b *testing.B, body []byte) {
	b.Helper()
	if len(body) > 0 && !json.Valid(body) {
		b.Fatal("benchmark fixture is invalid JSON")
	}
}
