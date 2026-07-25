//go:build unit

package service

import (
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

func TestHasOpenAICompactionTriggerInInput(t *testing.T) {
	tests := []struct {
		name string
		body []byte
		want bool
	}{
		{
			name: "detects trigger item",
			body: []byte(`{"model":"gpt-5.5","input":[{"type":"message","content":"hello"},{"type":"compaction_trigger"}]}`),
			want: true,
		},
		{
			name: "missing trigger",
			body: []byte(`{"input":[{"type":"message","content":"hello"}]}`),
			want: false,
		},
		{
			name: "non array input",
			body: []byte(`{"input":"compaction_trigger"}`),
			want: false,
		},
		{
			name: "missing input",
			body: []byte(`{"model":"gpt-5.5"}`),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, HasOpenAICompactionTriggerInInput(tt.body))
		})
	}
}

func TestOpenAIGatewayService_OAuthRemoteCompactionUsesNormalResponsesPolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	c.Request.Header.Set("User-Agent", "codex_cli_rs/0.136.0")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid-compact-signal"}},
		Body: io.NopCloser(strings.NewReader("event: response.completed\n" +
			`data: {"type":"response.completed","response":{"id":"resp_compact","model":"gpt-5.4","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n")),
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{
		ID:          77,
		Name:        "oauth-codex",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":          "oauth-token",
			"model_mapping":         map[string]any{"billing-alias": "gpt-5.4"},
			"compact_model_mapping": map[string]any{"gpt-5.4": "gpt-5.4-openai-compact"},
		},
	}
	body := []byte(`{"model":"billing-alias","stream":true,"store":true,"client_metadata":{"drop":true},"input":[{"type":"message","role":"user","content":"hello"},{"type":"compaction_trigger"}]}`)
	require.True(t, PromoteOpenAICompactBodySignal(c, body, false))

	result, err := svc.Forward(context.Background(), c, account, body)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "billing-alias", result.Model)
	require.Equal(t, "gpt-5.4", result.BillingModel)
	require.Equal(t, "gpt-5.4", result.UpstreamModel)
	require.Equal(t, "gpt-5.4", gjson.GetBytes(upstream.lastBody, "model").String())
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, "/v1/responses", c.Request.URL.Path)
	require.Equal(t, chatgptCodexURL, upstream.lastReq.URL.String())
	require.True(t, gjson.GetBytes(upstream.lastBody, "stream").Bool())
	require.False(t, gjson.GetBytes(upstream.lastBody, "store").Bool())
	require.True(t, gjson.GetBytes(upstream.lastBody, "client_metadata").Exists())
	require.Equal(t, "compaction_trigger", gjson.GetBytes(upstream.lastBody, "input.1.type").String())
}

func TestOpenAIGatewayService_APIKeyBodySignalStaysNativeResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	c.Request.Header.Set("User-Agent", "codex_cli_rs/0.136.0")

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid-apikey-signal"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"resp_native","usage":{"input_tokens":1,"output_tokens":1}}`)),
	}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	account := &Account{
		ID:          78,
		Name:        "apikey",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test"},
	}
	body := []byte(`{"model":"gpt-5.5","stream":false,"store":true,"input":[{"type":"compaction_trigger"}]}`)
	require.True(t, PromoteOpenAICompactBodySignal(c, body, false))

	result, err := svc.Forward(context.Background(), c, account, body)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, "/v1/responses", c.Request.URL.Path)
	require.Equal(t, "https://api.openai.com/v1/responses", upstream.lastReq.URL.String())
	require.True(t, gjson.GetBytes(upstream.lastBody, "store").Bool())
	require.Equal(t, "compaction_trigger", gjson.GetBytes(upstream.lastBody, "input.0.type").String())
}
