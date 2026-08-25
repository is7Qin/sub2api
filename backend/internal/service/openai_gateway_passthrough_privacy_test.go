package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIStreamingResponseFailedSanitizesVerboseResponseForClientAndKeepsUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	failedPayload := passthroughPrivacyVerboseFailedPayload("resp_failed_native")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"response.created","response":{"id":"resp_failed_native"}}`,
			"",
			`data: {"type":"response.output_text.delta","delta":"partial"}`,
			"",
			"event: response.failed",
			"data: " + failedPayload,
			"",
		}, "\n"))),
		Header: http.Header{"X-Request-Id": []string{"rid-failed-native"}},
	}

	svc := &OpenAIGatewayService{cfg: passthroughPrivacyTestConfig(), toolCorrector: NewCodexToolCorrector()}
	result, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI, Name: "acc"}, time.Now(), "gpt-5.4", "gpt-5.4")
	requestErr := requireOpenAIContextRequestError(t, err)
	require.True(t, requestErr.OutputStarted)
	require.NotNil(t, requestErr.Usage)
	require.NotNil(t, result)
	require.NotNil(t, result.usage)
	require.Equal(t, 123, result.usage.InputTokens)
	require.Equal(t, 4, result.usage.CacheReadInputTokens)

	passthroughPrivacyRequireSanitizedFailedBody(t, rec.Body.String())
}

func TestOpenAIStreamingContextFailedReturnsSanitizedRequestError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	failedPayload := passthroughPrivacyVerboseFailedPayload("resp_failed_native_failover")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			"event: response.failed",
			"data: " + failedPayload,
			"",
		}, "\n"))),
		Header: http.Header{"X-Request-Id": []string{"rid-failed-native-failover"}},
	}

	svc := &OpenAIGatewayService{cfg: passthroughPrivacyTestConfig(), toolCorrector: NewCodexToolCorrector()}
	result, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI, Name: "acc"}, time.Now(), "gpt-5.4", "gpt-5.4")
	requestErr := requireOpenAIContextRequestError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.usage)
	require.Equal(t, 123, result.usage.InputTokens)
	require.NotNil(t, requestErr.Usage)
	require.Equal(t, 123, requestErr.Usage.InputTokens)
	passthroughPrivacyRequireSanitizedFailedBody(t, rec.Body.String())
}

func TestOpenAIStreamingPassthroughResponseFailedSanitizesVerboseResponseForClientAndKeepsUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	failedPayload := passthroughPrivacyVerboseFailedPayload("resp_failed_passthrough")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"response.created","response":{"id":"resp_failed_passthrough"}}`,
			"",
			`data: {"type":"response.output_text.delta","delta":"partial"}`,
			"",
			"event: response.failed",
			"data: " + failedPayload,
			"",
		}, "\n"))),
		Header: http.Header{"X-Request-Id": []string{"rid-failed-passthrough"}},
	}

	svc := &OpenAIGatewayService{cfg: passthroughPrivacyTestConfig()}
	result, err := svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI, Name: "acc"}, time.Now(), "gpt-5.4", "gpt-5.4")
	require.Error(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.usage)
	require.Equal(t, 123, result.usage.InputTokens)
	require.Equal(t, 4, result.usage.CacheReadInputTokens)

	passthroughPrivacyRequireSanitizedFailedBody(t, rec.Body.String())
}

func TestOpenAIStreamingPassthroughDeduplicatesFunctionCallArguments(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"response.output_item.done","item":{"type":"function_call","name":"apply_patch","arguments":"{\"cmd\":\"pwd\"}{\"cmd\":\"pwd\"}"}}`,
			"",
			`data: {"type":"response.completed","response":{"id":"resp_dedupe","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`,
			"",
			"data: [DONE]",
			"",
		}, "\n"))),
		Header: http.Header{"X-Request-Id": []string{"rid-dedupe-passthrough"}},
	}

	svc := &OpenAIGatewayService{cfg: passthroughPrivacyTestConfig()}
	result, err := svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI, Name: "acc"}, time.Now(), "gpt-5.4", "gpt-5.4")
	require.NoError(t, err)
	require.NotNil(t, result)
	body := rec.Body.String()
	require.Contains(t, body, `"name":"apply_patch"`)
	require.Contains(t, body, `\"cmd\":\"pwd\"`)
	require.NotContains(t, body, `{\"cmd\":\"pwd\"}{\"cmd\":\"pwd\"}`)
	require.NotContains(t, body, `"name":"edit"`)
}

func TestOpenAINonStreamingPassthroughRejectsSuccessfulUnusableUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name string
		body string
	}{
		{name: "missing usage", body: `{"id":"resp_missing","output":[]}`},
		{name: "non-object usage", body: `{"id":"resp_invalid","output":[],"usage":"sk-upstream-secret"}`},
		{name: "empty usage", body: `{"id":"resp_empty","output":[],"usage":{}}`},
		{name: "invalid numeric usage", body: `{"id":"resp_bad_numbers","output":[],"usage":{"input_tokens":1.5,"output_tokens":-2}}`},
		{name: "negative nested cache usage", body: `{"id":"resp_bad_cache","output":[],"usage":{"input_tokens":10,"output_tokens":0,"input_tokens_details":{"cached_tokens":-1}}}`},
		{name: "malformed token details", body: `{"id":"resp_bad_details","output":[],"usage":{"input_tokens":10,"output_tokens":0,"input_tokens_details":"malformed"}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(tt.body)),
				Header:     http.Header{"Content-Type": []string{"application/json"}},
			}
			svc := &OpenAIGatewayService{cfg: passthroughPrivacyTestConfig()}

			result, err := svc.handleNonStreamingResponsePassthrough(c.Request.Context(), resp, c, "client-model", "upstream-model")

			require.Error(t, err)
			require.Nil(t, result)
			require.Equal(t, http.StatusBadGateway, rec.Code)
			require.Equal(t, "upstream_error", gjson.Get(rec.Body.String(), "error.type").String())
			require.NotContains(t, rec.Body.String(), "sk-upstream-secret")
			require.NotContains(t, err.Error(), "sk-upstream-secret")
		})
	}
}

func TestOpenAINonStreamingPassthroughAcceptsExplicitZeroUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	body := `{"id":"resp_zero","output":[],"usage":{"input_tokens":0,"output_tokens":0}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
	svc := &OpenAIGatewayService{cfg: passthroughPrivacyTestConfig()}

	result, err := svc.handleNonStreamingResponsePassthrough(c.Request.Context(), resp, c, "client-model", "upstream-model")

	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.usage)
	require.Zero(t, result.usage.InputTokens)
	require.Zero(t, result.usage.OutputTokens)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestOpenAINonStreamingPassthroughDeduplicatesFunctionCallOutputArguments(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	body := `{"id":"resp_dedupe_passthrough","model":"upstream-model","output":[{"type":"function_call","name":"apply_patch","arguments":"{\"cmd\":\"pwd\"}{\"cmd\":\"pwd\"}"},{"type":"message","arguments":"{\"cmd\":\"pwd\"}{\"cmd\":\"pwd\"}"},{"type":"function_call","name":"exec","arguments":"{\"cmd\":\"pwd\"}{\"cmd\":\"ls\"}"}],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"X-Request-Id": []string{"rid-nonstream-dedupe-passthrough"},
		},
	}

	svc := &OpenAIGatewayService{cfg: passthroughPrivacyTestConfig(), toolCorrector: NewCodexToolCorrector()}
	result, err := svc.handleNonStreamingResponsePassthrough(c.Request.Context(), resp, c, "client-model", "upstream-model")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 7, result.usage.InputTokens)
	require.Equal(t, "resp_dedupe_passthrough", result.responseID)

	clientBody := rec.Body.String()
	require.Equal(t, "client-model", gjson.Get(clientBody, "model").String())
	require.Equal(t, "apply_patch", gjson.Get(clientBody, "output.0.name").String())
	require.JSONEq(t, `{"cmd":"pwd"}`, gjson.Get(clientBody, "output.0.arguments").String())
	require.Equal(t, `{"cmd":"pwd"}{"cmd":"pwd"}`, gjson.Get(clientBody, "output.1.arguments").String())
	require.Equal(t, `{"cmd":"pwd"}{"cmd":"ls"}`, gjson.Get(clientBody, "output.2.arguments").String())
	require.NotContains(t, clientBody, `"name":"edit"`)
}

func TestOpenAIStreamingNativeDeduplicatesFunctionCallArguments(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"response.output_item.done","item":{"type":"function_call","name":"apply_patch","arguments":"{\"cmd\":\"pwd\"}{\"cmd\":\"pwd\"}"}}`,
			"",
			`data: {"type":"response.completed","response":{"id":"resp_dedupe_native","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`,
			"",
			"data: [DONE]",
			"",
		}, "\n"))),
		Header: http.Header{"X-Request-Id": []string{"rid-dedupe-native"}},
	}

	svc := &OpenAIGatewayService{cfg: passthroughPrivacyTestConfig(), toolCorrector: NewCodexToolCorrector()}
	result, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI, Name: "acc"}, time.Now(), "gpt-5.4", "gpt-5.4")
	require.NoError(t, err)
	require.NotNil(t, result)
	body := rec.Body.String()
	require.Contains(t, body, `"name":"apply_patch"`)
	require.Contains(t, body, `\"cmd\":\"pwd\"`)
	require.NotContains(t, body, `{\"cmd\":\"pwd\"}{\"cmd\":\"pwd\"}`)
}

func TestOpenAIForwardStreamingResponseFailedAfterOutputReturnsUsageWithError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","stream":true,"input":"hi"}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: passthroughPrivacyResponseFailedHTTPResponse("resp_forward_native", "rid-forward-native")}
	svc := &OpenAIGatewayService{
		cfg:           passthroughPrivacyTestConfig(),
		httpUpstream:  upstream,
		toolCorrector: NewCodexToolCorrector(),
	}
	account := passthroughPrivacyNativeAccount()

	result, err := svc.Forward(context.Background(), c, account, body)
	require.Error(t, err)
	require.NotNil(t, result)
	require.Equal(t, "rid-forward-native", result.RequestID)
	require.Equal(t, "resp_forward_native", result.ResponseID)
	require.True(t, result.Stream)
	require.Equal(t, 123, result.Usage.InputTokens)
	require.Equal(t, 4, result.Usage.CacheReadInputTokens)
	require.Equal(t, "gpt-5.4", result.Model)
	require.Equal(t, "gpt-5.4", result.UpstreamModel)
	require.NotNil(t, upstream.lastReq)

	passthroughPrivacyRequireSanitizedFailedBody(t, rec.Body.String())
}

func TestOpenAIForwardStreamingResponseFailedAsyncBufferedOutputDataReturnsUsageWithError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","stream":true,"input":"hi"}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")

	cfg := passthroughPrivacyTestConfig()
	cfg.Gateway.StreamKeepaliveInterval = 3600
	upstream := &httpUpstreamRecorder{resp: passthroughPrivacyAsyncBufferedOutputDataThenPolicyFailedHTTPResponse("resp_forward_native_async", "rid-forward-native-async")}
	svc := &OpenAIGatewayService{
		cfg:           cfg,
		httpUpstream:  upstream,
		toolCorrector: NewCodexToolCorrector(),
	}

	result, err := svc.Forward(context.Background(), c, passthroughPrivacyNativeAccount(), body)
	require.Error(t, err)
	require.NotNil(t, result)
	require.Equal(t, "rid-forward-native-async", result.RequestID)
	require.Equal(t, "resp_forward_native_async", result.ResponseID)
	require.True(t, result.Stream)
	require.Equal(t, 123, result.Usage.InputTokens)
	require.Equal(t, 4, result.Usage.CacheReadInputTokens)

	clientBody := rec.Body.String()
	require.Contains(t, clientBody, "event: response.output_text.delta")
	require.Contains(t, clientBody, "event: response.failed")
	require.Contains(t, clientBody, "\n\n: queued after failed")
	require.Contains(t, clientBody, "request violates safety policy")
	passthroughPrivacyRequireNoVerboseFailedText(t, clientBody)
}

func TestOpenAIForwardStreamingContextFailedOutputEventOnlyReturnsRequestErrorWithUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","stream":true,"input":"hi"}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")

	cfg := passthroughPrivacyTestConfig()
	cfg.Gateway.StreamKeepaliveInterval = 3600
	upstream := &httpUpstreamRecorder{resp: passthroughPrivacyOutputEventOnlyThenResponseFailedHTTPResponse("resp_forward_native_event_only", "rid-forward-native-event-only")}
	svc := &OpenAIGatewayService{
		cfg:           cfg,
		httpUpstream:  upstream,
		toolCorrector: NewCodexToolCorrector(),
	}

	result, err := svc.Forward(context.Background(), c, passthroughPrivacyNativeAccount(), body)
	requestErr := requireOpenAIContextRequestError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 123, result.Usage.InputTokens)
	require.NotNil(t, requestErr.Usage)
	require.Equal(t, 123, requestErr.Usage.InputTokens)
	passthroughPrivacyRequireSanitizedFailedBody(t, rec.Body.String())
}

func TestOpenAIAPIKeyPassthroughPreservesImageGenNamespaceBytes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte("{\n  \"model\": \"gpt-5.5\",\n  \"stream\": true,\n  \"tools\": [{\"type\":\"namespace\",\"name\":\"image_gen\"}],\n  \"input\": \"draw\"\n}")
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")
	upstream := &httpUpstreamRecorder{resp: passthroughPrivacyOutputEventOnlyThenResponseFailedHTTPResponse("resp_image_gen_passthrough", "rid-image-gen-passthrough")}
	svc := &OpenAIGatewayService{cfg: passthroughPrivacyTestConfig(), httpUpstream: upstream}

	_, err := svc.Forward(context.Background(), c, passthroughPrivacyAPIKeyPassthroughAccount(), body)

	require.Error(t, err)
	require.Equal(t, body, upstream.lastBody)
}

func TestOpenAIForwardStreamingContextFailedOutputEventOnlyPassthroughReturnsRequestErrorWithUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","stream":true,"input":"hi"}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: passthroughPrivacyOutputEventOnlyThenResponseFailedHTTPResponse("resp_forward_passthrough_event_only", "rid-forward-passthrough-event-only")}
	svc := &OpenAIGatewayService{
		cfg:          passthroughPrivacyTestConfig(),
		httpUpstream: upstream,
	}

	result, err := svc.Forward(context.Background(), c, passthroughPrivacyAPIKeyPassthroughAccount(), body)
	requestErr := requireOpenAIContextRequestError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 123, result.Usage.InputTokens)
	require.Equal(t, 4, result.Usage.CacheReadInputTokens)
	require.Equal(t, 123, requestErr.Usage.InputTokens)
	require.Contains(t, rec.Body.String(), "event: response.failed")
	require.Equal(t, body, upstream.lastBody)
	passthroughPrivacyRequireSanitizedFailedText(t, rec.Body.String())
}

func TestOpenAIForwardStreamingResponseFailedAsyncOnlyReturnsNoBillableResult(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","stream":true,"input":"hi"}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")

	cfg := passthroughPrivacyTestConfig()
	cfg.Gateway.StreamKeepaliveInterval = 3600
	upstream := &httpUpstreamRecorder{resp: passthroughPrivacyPolicyResponseFailedOnlyHTTPResponse("resp_forward_native_async_failed_only", "rid-forward-native-async-failed-only")}
	svc := &OpenAIGatewayService{
		cfg:           cfg,
		httpUpstream:  upstream,
		toolCorrector: NewCodexToolCorrector(),
	}

	result, err := svc.Forward(context.Background(), c, passthroughPrivacyNativeAccount(), body)
	require.Error(t, err)
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))

	clientBody := rec.Body.String()
	require.Contains(t, clientBody, "event: response.failed")
	require.Contains(t, clientBody, "request violates safety policy")
	passthroughPrivacyRequireNoVerboseFailedText(t, clientBody)
}

func TestOpenAIForwardStreamingResponseFailedAfterOutputPassthroughReturnsUsageWithError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","stream":true,"input":"hi"}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: passthroughPrivacyAsyncBufferedOutputDataThenPolicyFailedHTTPResponse("resp_forward_passthrough", "rid-forward-passthrough")}
	svc := &OpenAIGatewayService{
		cfg:          passthroughPrivacyTestConfig(),
		httpUpstream: upstream,
	}
	account := passthroughPrivacyAPIKeyPassthroughAccount()

	result, err := svc.Forward(context.Background(), c, account, body)
	require.Error(t, err)
	require.NotNil(t, result)
	require.Equal(t, "rid-forward-passthrough", result.RequestID)
	require.Equal(t, "resp_forward_passthrough", result.ResponseID)
	require.True(t, result.Stream)
	require.Equal(t, 123, result.Usage.InputTokens)
	require.Equal(t, 4, result.Usage.CacheReadInputTokens)
	require.Equal(t, "gpt-5.4", result.Model)
	require.Empty(t, result.UpstreamModel)
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, body, upstream.lastBody)

	clientBody := rec.Body.String()
	require.Contains(t, clientBody, "event: response.output_text.delta")
	require.Contains(t, clientBody, "event: response.failed")
	require.Contains(t, clientBody, "\n\n: queued after failed")
	require.Contains(t, clientBody, "request violates safety policy")
	passthroughPrivacyRequireNoVerboseFailedText(t, clientBody)
}

func TestOpenAIForwardStreamingContextFailedBeforeOutputReturnsRequestErrorWithUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","stream":true,"input":"hi"}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: passthroughPrivacyResponseFailedBeforeOutputHTTPResponse("resp_forward_native_pre", "rid-forward-native-pre")}
	svc := &OpenAIGatewayService{
		cfg:           passthroughPrivacyTestConfig(),
		httpUpstream:  upstream,
		toolCorrector: NewCodexToolCorrector(),
	}
	account := passthroughPrivacyNativeAccount()

	result, err := svc.Forward(context.Background(), c, account, body)
	requestErr := requireOpenAIContextRequestError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 123, result.Usage.InputTokens)
	require.NotNil(t, requestErr.Usage)
	require.Equal(t, 123, requestErr.Usage.InputTokens)
	passthroughPrivacyRequireSanitizedFailedBody(t, rec.Body.String())
}

func TestOpenAIForwardStreamingContextFailedBeforeOutputPassthroughReturnsRequestErrorWithUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","stream":true,"input":"hi"}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: passthroughPrivacyResponseFailedBeforeOutputHTTPResponse("resp_forward_passthrough_pre", "rid-forward-passthrough-pre")}
	svc := &OpenAIGatewayService{
		cfg:          passthroughPrivacyTestConfig(),
		httpUpstream: upstream,
	}
	account := passthroughPrivacyAPIKeyPassthroughAccount()

	result, err := svc.Forward(context.Background(), c, account, body)
	requestErr := requireOpenAIContextRequestError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 123, result.Usage.InputTokens)
	require.Equal(t, 4, result.Usage.CacheReadInputTokens)
	require.Equal(t, 123, requestErr.Usage.InputTokens)
	require.Contains(t, rec.Body.String(), "event: response.failed")
	passthroughPrivacyRequireSanitizedFailedText(t, rec.Body.String())
}

func TestOpenAIForwardStreamingResponseFailedBeforeOutputNonRetryableReturnsNoBillableResult(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","stream":true,"input":"hi"}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: passthroughPrivacyPolicyResponseFailedBeforeOutputHTTPResponse("resp_forward_native_policy_pre", "rid-forward-native-policy-pre")}
	svc := &OpenAIGatewayService{
		cfg:           passthroughPrivacyTestConfig(),
		httpUpstream:  upstream,
		toolCorrector: NewCodexToolCorrector(),
	}
	account := passthroughPrivacyNativeAccount()

	result, err := svc.Forward(context.Background(), c, account, body)
	require.Error(t, err)
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))

	clientBody := rec.Body.String()
	require.Contains(t, clientBody, "event: response.failed")
	require.Contains(t, clientBody, "request violates safety policy")
	passthroughPrivacyRequireNoVerboseFailedText(t, clientBody)
}

func TestOpenAIForwardStreamingResponseFailedBeforeOutputNonRetryablePassthroughReturnsNoBillableResult(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","stream":true,"input":"hi"}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: passthroughPrivacyPolicyResponseFailedBeforeOutputHTTPResponse("resp_forward_passthrough_policy_pre", "rid-forward-passthrough-policy-pre")}
	svc := &OpenAIGatewayService{
		cfg:          passthroughPrivacyTestConfig(),
		httpUpstream: upstream,
	}
	account := passthroughPrivacyAPIKeyPassthroughAccount()

	result, err := svc.Forward(context.Background(), c, account, body)
	require.Error(t, err)
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	require.Equal(t, body, upstream.lastBody)

	clientBody := rec.Body.String()
	require.Contains(t, clientBody, "event: response.failed")
	require.Contains(t, clientBody, "request violates safety policy")
	passthroughPrivacyRequireNoVerboseFailedText(t, clientBody)
}

func TestOpenAIForwardNonStreamingSSEResponseFailedEventLineSanitizesNative(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","stream":false,"input":"hi"}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: passthroughPrivacyNonStreamingResponseFailedNoTypeStatusHTTPResponse("resp_nonstream_native", "rid-nonstream-native")}
	svc := &OpenAIGatewayService{
		cfg:           passthroughPrivacyTestConfig(),
		httpUpstream:  upstream,
		toolCorrector: NewCodexToolCorrector(),
	}

	result, err := svc.Forward(context.Background(), c, passthroughPrivacyNativeAccount(), body)
	requestErr := requireOpenAIContextRequestError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 123, result.Usage.InputTokens)
	require.NotNil(t, requestErr.Usage)
	require.Equal(t, 123, requestErr.Usage.InputTokens)
	require.NotNil(t, upstream.lastReq)

	clientBody := rec.Body.String()
	require.NotContains(t, clientBody, "event: response.failed")
	require.Contains(t, clientBody, "Your input exceeds the context window")
	passthroughPrivacyRequireNoVerboseFailedText(t, clientBody)
}

func TestOpenAIForwardNonStreamingSSEResponseFailedEventLineSanitizesAPIKeyPassthrough(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","stream":false,"input":"hi"}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: passthroughPrivacyNonStreamingResponseFailedNoTypeStatusHTTPResponse("resp_nonstream_passthrough", "rid-nonstream-passthrough")}
	svc := &OpenAIGatewayService{
		cfg:          passthroughPrivacyTestConfig(),
		httpUpstream: upstream,
	}

	result, err := svc.Forward(context.Background(), c, passthroughPrivacyAPIKeyPassthroughAccount(), body)
	requestErr := requireOpenAIContextRequestError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 123, result.Usage.InputTokens)
	require.Equal(t, 4, result.Usage.CacheReadInputTokens)
	require.Equal(t, 123, requestErr.Usage.InputTokens)
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, body, upstream.lastBody)

	clientBody := rec.Body.String()
	require.NotContains(t, clientBody, "event: response.failed")
	require.Contains(t, clientBody, "Your input exceeds the context window")
	passthroughPrivacyRequireNoVerboseFailedText(t, clientBody)
}

func TestOpenAIPassthroughDoesNotStripSparkImageGenerationTooling(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.3-codex-spark","stream":false,"input":"hi","tools":[{"type":"image_generation"}],"tool_choice":{"type":"image_generation"}}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: passthroughPrivacyNonStreamingResponseFailedNoTypeStatusHTTPResponse("resp_passthrough_spark_tooling", "rid-passthrough-spark-tooling")}
	svc := &OpenAIGatewayService{
		cfg:          passthroughPrivacyTestConfig(),
		httpUpstream: upstream,
	}

	result, err := svc.Forward(context.Background(), c, passthroughPrivacyAPIKeyPassthroughAccount(), body)
	_ = result
	require.Error(t, err)
	require.Equal(t, body, upstream.lastBody)
	require.True(t, gjson.GetBytes(upstream.lastBody, `tools.#(type=="image_generation")`).Exists())
	require.Equal(t, "image_generation", gjson.GetBytes(upstream.lastBody, "tool_choice.type").String())
}

func passthroughPrivacyTestConfig() *config.Config {
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			MaxLineSize:                  defaultMaxLineSize,
			StreamDataIntervalTimeout:    0,
			StreamKeepaliveInterval:      0,
			LogUpstreamErrorBody:         true,
			LogUpstreamErrorBodyMaxBytes: 4096,
		},
	}
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	return cfg
}

func passthroughPrivacyNativeAccount() *Account {
	return &Account{
		ID:          101,
		Name:        "openai-apikey-native",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "http://upstream.example",
		},
		Extra: map[string]any{
			openai_compat.ExtraKeyResponsesMode:      string(openai_compat.ResponsesSupportModeAuto),
			openai_compat.ExtraKeyResponsesSupported: true,
		},
		Status:      StatusActive,
		Schedulable: true,
	}
}

func passthroughPrivacyAPIKeyPassthroughAccount() *Account {
	return &Account{
		ID:          102,
		Name:        "openai-apikey-passthrough",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "http://upstream.example",
		},
		Extra: map[string]any{
			"openai_passthrough": true,
		},
		Status:      StatusActive,
		Schedulable: true,
	}
}

func passthroughPrivacyResponseFailedHTTPResponse(responseID, requestID string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			"X-Request-Id": []string{requestID},
		},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"response.created","response":{"id":` + strconv.Quote(responseID) + `}}`,
			"",
			`data: {"type":"response.output_text.delta","delta":"partial"}`,
			"",
			"event: response.failed",
			"data: " + passthroughPrivacyVerboseFailedPayload(responseID),
			"",
		}, "\n"))),
	}
}

func passthroughPrivacyNonStreamingResponseFailedNoTypeStatusHTTPResponse(responseID, requestID string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			"X-Request-Id": []string{requestID},
		},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			"event: response.failed",
			"data: " + passthroughPrivacyVerboseFailedPayloadNoTypeStatus(responseID),
			"",
		}, "\n"))),
	}
}

func passthroughPrivacyResponseFailedBeforeOutputHTTPResponse(responseID, requestID string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			"X-Request-Id": []string{requestID},
		},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			"event: response.failed",
			"data: " + passthroughPrivacyVerboseFailedPayload(responseID),
			"",
		}, "\n"))),
	}
}

func passthroughPrivacyPolicyResponseFailedBeforeOutputHTTPResponse(responseID, requestID string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			"X-Request-Id": []string{requestID},
		},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"response.created","response":{"id":` + strconv.Quote(responseID) + `}}`,
			"",
			"event: response.failed",
			"data: " + passthroughPrivacyPolicyFailedPayload(responseID),
			"",
		}, "\n"))),
	}
}

func passthroughPrivacyAsyncBufferedOutputDataThenPolicyFailedHTTPResponse(responseID, requestID string) *http.Response {
	lines := []string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":` + strconv.Quote(responseID) + `}}`,
		"",
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","delta":"partial"}`,
		"",
		"event: response.failed",
		"data: " + passthroughPrivacyPolicyFailedPayload(responseID),
		"",
	}
	for i := 0; i < 24; i++ {
		lines = append(lines, ": queued after failed")
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			"X-Request-Id": []string{requestID},
		},
		Body: io.NopCloser(strings.NewReader(strings.Join(lines, "\n"))),
	}
}

func passthroughPrivacyOutputEventOnlyThenResponseFailedHTTPResponse(responseID, requestID string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			"X-Request-Id": []string{requestID},
		},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`event: response.created`,
			`data: {"type":"response.created","response":{"id":` + strconv.Quote(responseID) + `}}`,
			"",
			"event: response.output_text.delta",
			"event: response.failed",
			"data: " + passthroughPrivacyVerboseFailedPayload(responseID),
			"",
		}, "\n"))),
	}
}

func passthroughPrivacyPolicyResponseFailedOnlyHTTPResponse(responseID, requestID string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			"X-Request-Id": []string{requestID},
		},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			"event: response.failed",
			"data: " + passthroughPrivacyPolicyFailedPayload(responseID),
		}, "\n"))),
	}
}

func passthroughPrivacyPolicyFailedPayload(responseID string) string {
	longInstructions := strings.Repeat("You are GPT-5.1 running in the Codex CLI. ", 20)
	usage := `"usage":{"input_tokens":123,"output_tokens":0,"input_tokens_details":{"cached_tokens":4}}`
	verboseFields := `"instructions":` + strconv.Quote(longInstructions) + `,"input":[{"role":"user","content":"sensitive prompt"}],"output":[{"type":"message","content":[{"type":"output_text","text":"large"}]}],` + usage + `,"metadata":{"tenant":"secret"},"tools":[{"type":"function","name":"secret_tool"}],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":{"effort":"high"},"text":{"verbosity":"high"},"truncation":"auto","max_output_tokens":8192,"incomplete_details":{"reason":"max_output_tokens"}`
	return `{"type":"response.failed","response":{"id":` + strconv.Quote(responseID) + `,"object":"response","created_at":1782446336,"status":"failed",` + verboseFields + `,"error":{"type":"invalid_request_error","code":"content_policy_violation","message":"request violates safety policy"}}}`
}

func passthroughPrivacyVerboseFailedPayloadNoTypeStatus(responseID string) string {
	longInstructions := strings.Repeat("You are GPT-5.1 running in the Codex CLI. ", 20)
	usage := `"usage":{"input_tokens":123,"output_tokens":0,"input_tokens_details":{"cached_tokens":4}}`
	verboseFields := `"instructions":` + strconv.Quote(longInstructions) + `,"input":[{"role":"user","content":"sensitive prompt"}],"output":[{"type":"message","content":[{"type":"output_text","text":"large"}]}],` + usage + `,"metadata":{"tenant":"secret"},"tools":[{"type":"function","name":"secret_tool"}],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":{"effort":"high"},"text":{"verbosity":"high"},"truncation":"auto","max_output_tokens":8192,"incomplete_details":{"reason":"max_output_tokens"}`
	return `{` + verboseFields + `,"response":{"id":` + strconv.Quote(responseID) + `,"object":"response","created_at":1782446336,` + verboseFields + `,"error":{"code":"context_length_exceeded","message":"Your input exceeds the context window of this model. Please adjust your input and try again."}}}`
}

func passthroughPrivacyVerboseFailedPayload(responseID string) string {
	longInstructions := strings.Repeat("You are GPT-5.1 running in the Codex CLI. ", 20)
	usage := `"usage":{"input_tokens":123,"output_tokens":0,"input_tokens_details":{"cached_tokens":4}}`
	verboseFields := `"instructions":` + strconv.Quote(longInstructions) + `,"input":[{"role":"user","content":"sensitive prompt"}],"output":[{"type":"message","content":[{"type":"output_text","text":"large"}]}],` + usage + `,"metadata":{"tenant":"secret"},"tools":[{"type":"function","name":"secret_tool"}],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":{"effort":"high"},"text":{"verbosity":"high"},"truncation":"auto","max_output_tokens":8192,"incomplete_details":{"reason":"max_output_tokens"}`
	return `{` + verboseFields + `,"response":{"id":` + strconv.Quote(responseID) + `,"object":"response","created_at":1782446336,"status":"failed",` + verboseFields + `,"error":{"code":"context_length_exceeded","message":"Your input exceeds the context window of this model. Please adjust your input and try again."}}}`
}

func passthroughPrivacyRequireSanitizedFailedBody(t *testing.T, body string) {
	t.Helper()
	require.Contains(t, body, "event: response.failed")
	passthroughPrivacyRequireSanitizedFailedText(t, body)
}

func passthroughPrivacyRequireSanitizedOpsDetail(t *testing.T, c *gin.Context) {
	t.Helper()

	detail := passthroughPrivacyOpsDetail(t, c)
	passthroughPrivacyRequireSanitizedFailedText(t, detail)
}

func passthroughPrivacyOpsDetail(t *testing.T, c *gin.Context) string {
	t.Helper()

	detailValue, ok := c.Get(OpsUpstreamErrorDetailKey)
	require.True(t, ok)
	detail, ok := detailValue.(string)
	require.True(t, ok)

	eventsValue, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok)
	events, ok := eventsValue.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.NotEmpty(t, events)
	passthroughPrivacyRequireSanitizedFailedText(t, events[len(events)-1].Detail)
	return detail
}

func passthroughPrivacyOpsDetailValue(t *testing.T, c *gin.Context) string {
	t.Helper()

	detailValue, ok := c.Get(OpsUpstreamErrorDetailKey)
	require.True(t, ok)
	detail, ok := detailValue.(string)
	require.True(t, ok)
	return detail
}

func passthroughPrivacyRequireSanitizedFailedText(t *testing.T, text string) {
	t.Helper()
	require.Contains(t, text, "context_length_exceeded")
	require.Contains(t, text, "Your input exceeds the context window")
	passthroughPrivacyRequireNoVerboseFailedText(t, text)
}

func passthroughPrivacyRequireNoVerboseFailedText(t *testing.T, text string) {
	t.Helper()
	for _, forbidden := range []string{
		"You are GPT-5.1 running in the Codex CLI",
		"sensitive prompt",
		"secret_tool",
		`"instructions"`,
		`"input"`,
		`"output"`,
		`"usage"`,
		`"metadata"`,
		`"tools"`,
		`"tool_choice"`,
		`"parallel_tool_calls"`,
		`"reasoning"`,
		`"text"`,
		`"truncation"`,
		`"max_output_tokens"`,
		`"incomplete_details"`,
	} {
		require.NotContains(t, text, forbidden)
	}
}
