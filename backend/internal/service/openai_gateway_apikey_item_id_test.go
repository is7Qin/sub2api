//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestSanitizeOpenAIResponsesInputItemIDs(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"message","id":"item_bad_message","content":"hello"},{"type":"function_call","id":"item_bad_call","call_id":"call_123"},{"type":"message","id":"msg_valid"},{"type":"function_call","id":"fc_valid"},{"type":"function_call_output","id":"item_output","call_id":"call_123"},{"type":"web_search_call","id":"item_unknown"},{"type":"message","id":""}]}`)

	sanitized, changed, err := sanitizeOpenAIResponsesInputItemIDs(body)

	require.NoError(t, err)
	require.True(t, changed)
	require.False(t, gjson.GetBytes(sanitized, "input.0.id").Exists())
	require.False(t, gjson.GetBytes(sanitized, "input.1.id").Exists())
	require.Equal(t, "msg_valid", gjson.GetBytes(sanitized, "input.2.id").String())
	require.Equal(t, "fc_valid", gjson.GetBytes(sanitized, "input.3.id").String())
	require.Equal(t, "item_output", gjson.GetBytes(sanitized, "input.4.id").String())
	require.Equal(t, "item_unknown", gjson.GetBytes(sanitized, "input.5.id").String())
	require.True(t, gjson.GetBytes(sanitized, "input.6.id").Exists())
}

func TestSanitizeOpenAIResponsesInputItemIDsRejectsMalformedJSON(t *testing.T) {
	body := []byte(`{"model":"gpt-5","input":[{"type":"message","id":"item_bad"},]}`)

	sanitized, changed, err := sanitizeOpenAIResponsesInputItemIDs(body)

	require.Error(t, err)
	require.False(t, changed)
	require.Equal(t, body, sanitized)
}

func TestSanitizeOpenAIResponsesInputItemIDsPreservesLargeNumbers(t *testing.T) {
	body := []byte(`{"model":"gpt-5","input":[{"type":"message","id":"item_bad"}],"metadata":{"sequence":90071992547409931234567890}}`)

	sanitized, changed, err := sanitizeOpenAIResponsesInputItemIDs(body)

	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "90071992547409931234567890", gjson.GetBytes(sanitized, "metadata.sequence").Raw)
}

func TestSanitizeOpenAIResponsesInputItemIDsNoop(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"model":"gpt-5","input":"hello"}`),
		[]byte(`{"model":"gpt-5"}`),
		[]byte(`{"model":"gpt-5","input":[{"type":"message","id":"msg_1"},{"type":"function_call","id":"fc_1"}]}`),
	} {
		sanitized, changed, err := sanitizeOpenAIResponsesInputItemIDs(body)
		require.NoError(t, err)
		require.False(t, changed)
		require.Equal(t, body, sanitized)
	}
}

func TestOpenAIGatewayServiceAPIKeyResponsesStripsInvalidInputItemIDs(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(map[bool]string{false: "native", true: "passthrough"}[passthrough], func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"id":"resp_test","model":"gpt-5.6-sol","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)),
			}}
			svc := newOpenAIImageGenerationControlTestService(upstream)
			c, _ := newOpenAIImageGenerationControlTestContext(true, "codex_cli_rs/0.144.1")
			account := newOpenAIImageGenerationControlTestAccount()
			if passthrough {
				account.Extra = map[string]any{"openai_passthrough": true}
			}
			body := []byte(`{"model":"gpt-5.6-sol","stream":false,"input":[{"type":"message","id":"item_bad","content":"hello"},{"type":"function_call","id":"item_call","call_id":"call_123","name":"lookup","arguments":"{}"},{"type":"message","id":"msg_valid"},{"type":"function_call_output","id":"item_output","call_id":"call_123","output":"done"}]}`)

			result, err := svc.Forward(context.Background(), c, account, body)

			require.NoError(t, err)
			require.NotNil(t, result)
			require.NotNil(t, upstream.lastReq)
			require.False(t, gjson.GetBytes(upstream.lastBody, "input.0.id").Exists())
			require.False(t, gjson.GetBytes(upstream.lastBody, "input.1.id").Exists())
			require.Equal(t, "msg_valid", gjson.GetBytes(upstream.lastBody, "input.2.id").String())
			require.Equal(t, "item_output", gjson.GetBytes(upstream.lastBody, "input.3.id").String())
			require.Equal(t, "call_123", gjson.GetBytes(upstream.lastBody, "input.3.call_id").String())
		})
	}
}
