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
)

func TestOpenAIWSHTTPBridgePreservesAdmittedAttemptID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	clientCtx, cancelClient := context.WithCancel(context.Background())
	admissionCtx := WithHTTPAttemptAuthority(clientCtx, nil, cancelClient)
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(
			`data: {"type":"response.completed","response":{"id":"resp_bridge","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n",
		)),
	}}
	svc := &OpenAIGatewayService{
		cfg:          &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}},
		httpUpstream: upstream,
	}
	account := &Account{
		ID:          7,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "test-key", "base_url": "https://api.openai.com"},
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	payload := []byte(`{"type":"response.create","model":"gpt-5","input":"hello"}`)

	result, err := svc.proxyOpenAIWSHTTPBridgeTurn(
		admissionCtx, c, account, "test-key", payload, len(payload), "gpt-5", "", "", "", 1,
		func([]byte) error { return nil },
	)

	require.NoError(t, err)
	require.NotNil(t, upstream.lastReq)
	require.NotEmpty(t, HTTPAttemptID(upstream.lastReq.Context()))
	require.Equal(t, HTTPAttemptID(upstream.lastReq.Context()), result.AttemptID)
	require.ErrorIs(t, clientCtx.Err(), context.Canceled)
}
