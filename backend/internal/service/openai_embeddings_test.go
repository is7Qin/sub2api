package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestBuildOpenAIEmbeddingsURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		base string
		want string
	}{
		{"bare domain", "https://api.openai.com", "https://api.openai.com/v1/embeddings"},
		{"bare /v1", "https://api.openai.com/v1", "https://api.openai.com/v1/embeddings"},
		{"already embeddings", "https://api.openai.com/v1/embeddings", "https://api.openai.com/v1/embeddings"},
		{"third-party versioned path", "https://open.bigmodel.cn/api/paas/v4", "https://open.bigmodel.cn/api/paas/v4/embeddings"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, buildOpenAIEmbeddingsURL(tt.base))
		})
	}
}

func TestExtractOpenAIEmbeddingsUsage_ParsesImageInputTokens(t *testing.T) {
	usage := extractOpenAIEmbeddingsUsage([]byte(`{
		"usage": {
			"prompt_tokens": 1340,
			"prompt_tokens_details": {"image_tokens": 28, "cached_tokens": 7}
		}
	}`))

	require.Equal(t, 1340, usage.InputTokens)
	require.Equal(t, 28, usage.ImageInputTokens)
	require.Equal(t, 7, usage.CacheReadInputTokens)

	usage = extractOpenAIEmbeddingsUsage([]byte(`{
		"usage": {
			"input_tokens": 500,
			"input_tokens_details": {"image_tokens": 30}
		}
	}`))

	require.Equal(t, 500, usage.InputTokens)
	require.Equal(t, 30, usage.ImageInputTokens)
}

func TestForwardEmbeddings_RejectsSuccessfulResponseWithUnusableUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name string
		body string
	}{
		{name: "missing usage", body: `{"object":"list","data":[],"secret":"sk-upstream-secret"}`},
		{name: "non-object usage", body: `{"object":"list","data":[],"usage":"hidden https://internal.example"}`},
		{name: "empty usage", body: `{"object":"list","data":[],"usage":{}}`},
		{name: "negative tokens", body: `{"object":"list","data":[],"usage":{"prompt_tokens":-1}}`},
		{name: "fractional tokens", body: `{"object":"list","data":[],"usage":{"prompt_tokens":1.5}}`},
		{name: "string tokens", body: `{"object":"list","data":[],"usage":{"prompt_tokens":"1"}}`},
		{name: "null tokens", body: `{"object":"list","data":[],"usage":{"prompt_tokens":null}}`},
		{name: "negative nested cache tokens", body: `{"object":"list","data":[],"usage":{"prompt_tokens":10,"prompt_tokens_details":{"cached_tokens":-1}}}`},
		{name: "fractional nested image tokens", body: `{"object":"list","data":[],"usage":{"prompt_tokens":10,"input_tokens_details":{"image_tokens":1.5}}}`},
		{name: "malformed token details", body: `{"object":"list","data":[],"usage":{"prompt_tokens":10,"input_tokens_details":"malformed"}}`},
		{name: "string cache alias", body: `{"object":"list","data":[],"usage":{"prompt_tokens":10,"cache_read_tokens":"1"}}`},
		{name: "malformed json", body: `{"object":"list","data":[],"usage":{"prompt_tokens":3},"secret":"sk-upstream-secret"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			reqBody := []byte(`{"model":"text-embedding-3-small","input":"hello"}`)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader(reqBody))

			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(tt.body)),
			}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			account := &Account{
				ID:       42,
				Platform: PlatformOpenAI,
				Type:     AccountTypeAPIKey,
				Credentials: map[string]any{
					"api_key": "sk-test",
				},
			}

			result, err := svc.ForwardEmbeddings(context.Background(), c, account, reqBody)

			require.Error(t, err)
			require.Nil(t, result)
			require.Equal(t, http.StatusBadGateway, rec.Code)
			require.Equal(t, "api_error", gjson.Get(rec.Body.String(), "error.type").String())
			require.Equal(t, "Upstream returned invalid usage", gjson.Get(rec.Body.String(), "error.message").String())
			require.NotContains(t, rec.Body.String(), "sk-upstream-secret")
			require.NotContains(t, rec.Body.String(), "internal.example")
			require.NotContains(t, err.Error(), "sk-upstream-secret")
			require.NotContains(t, err.Error(), "internal.example")
			require.Len(t, upstream.requests, 1)
		})
	}
}

func TestForwardEmbeddings_AcceptsExplicitZeroUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	reqBody := []byte(`{"model":"text-embedding-3-small","input":"hello"}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader(reqBody))

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{
			"object":"list",
			"data":[],
			"usage":{"prompt_tokens":0,"total_tokens":0}
		}`)),
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{
		ID:       42,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key": "sk-test",
		},
	}

	result, err := svc.ForwardEmbeddings(context.Background(), c, account, reqBody)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Zero(t, result.Usage.InputTokens)
	require.Zero(t, result.Usage.OutputTokens)
}

func TestForwardEmbeddings_APIKeyPassthroughRecordsUsageAndBatchInput(t *testing.T) {
	gin.SetMode(gin.TestMode)

	reqBody := []byte(`{
		"model":"nowledge-embedding",
		"input":["hello","world"],
		"encoding_format":"float",
		"dimensions":256
	}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/embeddings", bytes.NewReader(reqBody))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"X-Request-Id": []string{"emb-rid"},
		},
		Body: io.NopCloser(strings.NewReader(`{
			"object":"list",
			"data":[
				{"object":"embedding","index":0,"embedding":[0.1,0.2]},
				{"object":"embedding","index":1,"embedding":[0.3,0.4]}
			],
			"model":"jina-embeddings-v5-text-small",
			"usage":{"prompt_tokens":13,"total_tokens":13}
		}`)),
	}}
	svc := &OpenAIGatewayService{
		cfg:          &config.Config{},
		httpUpstream: upstream,
	}
	account := &Account{
		ID:       42,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://api.jina.ai",
			"model_mapping": map[string]any{
				"nowledge-embedding": "jina-embeddings-v5-text-small",
			},
		},
	}

	result, err := svc.ForwardEmbeddings(context.Background(), c, account, reqBody)

	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, result)
	require.Equal(t, "emb-rid", result.RequestID)
	require.NotEmpty(t, result.AttemptID)
	require.Equal(t, HTTPAttemptID(upstream.lastReq.Context()), result.AttemptID)
	require.Equal(t, "nowledge-embedding", result.Model)
	require.Equal(t, "jina-embeddings-v5-text-small", result.BillingModel)
	require.Equal(t, "jina-embeddings-v5-text-small", result.UpstreamModel)
	require.Equal(t, 13, result.Usage.InputTokens)
	require.Equal(t, 0, result.Usage.ImageInputTokens)
	require.Equal(t, 0, result.Usage.OutputTokens)
	require.Equal(t, "https://api.jina.ai/v1/embeddings", upstream.lastReq.URL.String())
	require.Equal(t, "Bearer sk-test", upstream.lastReq.Header.Get("Authorization"))
	require.Equal(t, "jina-embeddings-v5-text-small", gjson.GetBytes(upstream.lastBody, "model").String())
	require.Equal(t, int64(2), gjson.GetBytes(upstream.lastBody, "input.#").Int())
	require.Equal(t, "hello", gjson.GetBytes(upstream.lastBody, "input.0").String())
	require.Equal(t, "world", gjson.GetBytes(upstream.lastBody, "input.1").String())
	require.Equal(t, "float", gjson.GetBytes(upstream.lastBody, "encoding_format").String())
	require.Equal(t, int64(256), gjson.GetBytes(upstream.lastBody, "dimensions").Int())
}
