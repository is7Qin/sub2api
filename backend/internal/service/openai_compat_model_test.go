package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func init() {
	gin.SetMode(gin.TestMode)
}

type openAICompatFailingWriter struct {
	gin.ResponseWriter
	failAfter int
	writes    int
	attempts  int
}

func (w *openAICompatFailingWriter) Write(p []byte) (int, error) {
	w.attempts++
	if w.writes >= w.failAfter {
		return 0, errors.New("write failed: client disconnected")
	}
	w.writes++
	return w.ResponseWriter.Write(p)
}

type openAICompatBlockingReadCloser struct {
	data      []byte
	offset    int
	closed    chan struct{}
	closeOnce sync.Once
}

func newOpenAICompatBlockingReadCloser(data []byte) *openAICompatBlockingReadCloser {
	return &openAICompatBlockingReadCloser{
		data:   data,
		closed: make(chan struct{}),
	}
}

func (r *openAICompatBlockingReadCloser) Read(p []byte) (int, error) {
	if r.offset < len(r.data) {
		n := copy(p, r.data[r.offset:])
		r.offset += n
		return n, nil
	}
	<-r.closed
	return 0, io.EOF
}

func (r *openAICompatBlockingReadCloser) Close() error {
	r.closeOnce.Do(func() {
		close(r.closed)
	})
	return nil
}

type openAICompatDelayedChunkReadCloser struct {
	chunks   []string
	delays   []time.Duration
	chunkIdx int
	offset   int
}

func newOpenAICompatDelayedChunkReadCloser(chunks []string, delays []time.Duration) *openAICompatDelayedChunkReadCloser {
	return &openAICompatDelayedChunkReadCloser{chunks: chunks, delays: delays}
}

func (r *openAICompatDelayedChunkReadCloser) Read(p []byte) (int, error) {
	for r.chunkIdx < len(r.chunks) {
		chunk := []byte(r.chunks[r.chunkIdx])
		if len(chunk) == 0 {
			r.chunkIdx++
			continue
		}
		if r.offset == 0 && r.chunkIdx < len(r.delays) && r.delays[r.chunkIdx] > 0 {
			time.Sleep(r.delays[r.chunkIdx])
			r.delays[r.chunkIdx] = 0
		}
		n := copy(p, chunk[r.offset:])
		r.offset += n
		if r.offset >= len(chunk) {
			r.chunkIdx++
			r.offset = 0
		}
		return n, nil
	}
	return 0, io.EOF
}

func (r *openAICompatDelayedChunkReadCloser) Close() error {
	r.chunkIdx = len(r.chunks)
	return nil
}

func newOpenAICompatMessagesTestContext(body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c, rec
}

func newOpenAICompatMessagesTestAccount() *Account {
	return &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}
}

func newOpenAICompatMessagesSSEService(body io.ReadCloser, requestID string, cfg *config.Config) *OpenAIGatewayService {
	if cfg == nil {
		cfg = &config.Config{}
	}
	cfg.Security.URLAllowlist.Enabled = false
	return &OpenAIGatewayService{
		httpUpstream: &httpUpstreamRecorder{resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{requestID}},
			Body:       body,
		}},
		cfg: cfg,
	}
}

func TestNormalizeOpenAICompatRequestedModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "gpt reasoning alias strips xhigh", input: "gpt-5.4-xhigh", want: "gpt-5.4"},
		{name: "gpt reasoning alias strips none", input: "gpt-5.4-none", want: "gpt-5.4"},
		{name: "codex max model stays intact", input: "gpt-5.1-codex-max", want: "gpt-5.1-codex-max"},
		{name: "non openai model unchanged", input: "claude-opus-4-6", want: "claude-opus-4-6"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, NormalizeOpenAICompatRequestedModel(tt.input))
		})
	}
}

func TestApplyOpenAICompatModelNormalization(t *testing.T) {
	t.Parallel()

	t.Run("derives xhigh from model suffix when output config missing", func(t *testing.T) {
		req := &apicompat.AnthropicRequest{Model: "gpt-5.4-xhigh"}

		applyOpenAICompatModelNormalization(req)

		require.Equal(t, "gpt-5.4", req.Model)
		require.NotNil(t, req.OutputConfig)
		require.Equal(t, "max", req.OutputConfig.Effort)
	})

	t.Run("explicit output config wins over model suffix", func(t *testing.T) {
		req := &apicompat.AnthropicRequest{
			Model:        "gpt-5.4-xhigh",
			OutputConfig: &apicompat.AnthropicOutputConfig{Effort: "low"},
		}

		applyOpenAICompatModelNormalization(req)

		require.Equal(t, "gpt-5.4", req.Model)
		require.NotNil(t, req.OutputConfig)
		require.Equal(t, "low", req.OutputConfig.Effort)
	})

	t.Run("non openai model is untouched", func(t *testing.T) {
		req := &apicompat.AnthropicRequest{Model: "claude-opus-4-6"}

		applyOpenAICompatModelNormalization(req)

		require.Equal(t, "claude-opus-4-6", req.Model)
		require.Nil(t, req.OutputConfig)
	})
}

func TestForwardAsAnthropic_NormalizesRoutingAndEffortForGpt54XHigh(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4-xhigh","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := strings.Join([]string{
		`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","model":"gpt-5.4","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_compat"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}

	svc := &OpenAIGatewayService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
			"model_mapping": map[string]any{
				"gpt-5.4": "gpt-5.4",
			},
		},
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-5.1")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "gpt-5.4-xhigh", result.Model)
	require.Equal(t, "gpt-5.4", result.UpstreamModel)
	require.Equal(t, "gpt-5.4", result.BillingModel)
	require.NotNil(t, result.ReasoningEffort)
	require.Equal(t, "xhigh", *result.ReasoningEffort)

	require.Equal(t, "gpt-5.4", gjson.GetBytes(upstream.lastBody, "model").String())
	require.Equal(t, "xhigh", gjson.GetBytes(upstream.lastBody, "reasoning.effort").String())
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "gpt-5.4-xhigh", gjson.GetBytes(rec.Body.Bytes(), "model").String())
	require.Equal(t, "ok", gjson.GetBytes(rec.Body.Bytes(), "content.0.text").String())
	t.Logf("upstream body: %s", string(upstream.lastBody))
	t.Logf("response body: %s", rec.Body.String())
}

func TestForwardAsAnthropic_PlanGateUsesFinalUpstreamModel(t *testing.T) {
	body := []byte(`{"model":"claude-fable-5","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c, _ := newOpenAICompatMessagesTestContext(body)
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"The 'gpt-5.4' model is not supported when using Codex with a ChatGPT account."}}`)),
	}}
	repo := &modelNotFoundAccountRepoStub{}
	svc := &OpenAIGatewayService{
		httpUpstream: upstream,
		rateLimitService: &RateLimitService{
			accountRepo: repo,
		},
		cfg: &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          4,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
			"model_mapping": map[string]any{
				"gpt-5.4": "gpt-5.4-xhigh",
			},
		},
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-5.4")
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	wireModel := gjson.GetBytes(upstream.lastBody, "model").String()
	require.Equal(t, "gpt-5.4", wireModel)
	require.Len(t, repo.modelRateLimitCalls, 1)
	require.Equal(t, wireModel, repo.modelRateLimitCalls[0].scope)
	require.Equal(t, openAIPlanGatedModelReason, repo.modelRateLimitCalls[0].reason)
}

func TestForwardAsAnthropic_ExactMessagesDispatchPreservesPublicModel(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"claude-fable-5","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c, rec := newOpenAICompatMessagesTestContext(body)
	upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_exact", "gpt-5.6-sol")}
	svc := &OpenAIGatewayService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, newOpenAICompatMessagesTestAccount(), body, "", "gpt-5.6-sol")
	require.NoError(t, err)
	require.Equal(t, "gpt-5.6-sol", gjson.GetBytes(upstream.lastBody, "model").String())
	require.Equal(t, "claude-fable-5", result.Model)
	require.Equal(t, "gpt-5.6-sol", result.UpstreamModel)
	require.Equal(t, "claude-fable-5", gjson.GetBytes(rec.Body.Bytes(), "model").String())
}

func TestForwardAsAnthropic_BufferedResponseFailedTriggersFailover(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := strings.Join([]string{
		`data: {"type":"response.failed","response":{"id":"resp_failed","object":"response","model":"gpt-5.4","status":"failed","error":{"code":"server_error","message":"bad upstream"},"output":[],"usage":{"input_tokens":5,"output_tokens":0,"total_tokens":5}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "X-Request-Id": []string{"rid_failed"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}

	svc := &OpenAIGatewayService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-5.4")
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.Equal(t, "bad upstream", gjson.GetBytes(failoverErr.ResponseBody, "error.message").String())
	fact, ok := failoverErr.UpstreamFact()
	require.True(t, ok)
	require.Equal(t, UpstreamErrorSourceSSE, fact.Source)
	require.False(t, fact.HTTPStatusKnown)
	require.Zero(t, fact.HTTPStatus)
	require.Equal(t, "server_error", fact.ProviderCode)
	require.Equal(t, "bad upstream", fact.SafeMessage)
	require.Equal(t, "rid_failed", fact.RequestID)
}

func TestForwardAsAnthropic_StreamingResponseFailedBeforeOutputTriggersFailover(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := strings.Join([]string{
		`data: {"type":"response.failed","response":{"id":"resp_failed","object":"response","model":"gpt-5.4","status":"failed","error":{"code":"server_error","message":"bad upstream"},"output":[],"usage":{"input_tokens":5,"output_tokens":0,"total_tokens":5}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_stream_failed"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}

	svc := &OpenAIGatewayService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-5.4")
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.Equal(t, "bad upstream", gjson.GetBytes(failoverErr.ResponseBody, "error.message").String())
	require.Empty(t, rec.Body.String())
}

func TestForwardAsAnthropic_StreamingPreludeThenResponseFailedBeforeOutputTriggersFailover(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	c, rec := newOpenAICompatMessagesTestContext(body)

	upstreamBody := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_failed","object":"response","model":"gpt-5.4","status":"in_progress","output":[]}}`,
		"",
		`data: {"type":"response.failed","response":{"id":"resp_failed","object":"response","model":"gpt-5.4","status":"failed","error":{"code":"server_error","message":"bad upstream"},"output":[],"usage":{"input_tokens":5,"output_tokens":0,"total_tokens":5}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	svc := newOpenAICompatMessagesSSEService(io.NopCloser(strings.NewReader(upstreamBody)), "rid_stream_prelude_failed", nil)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, newOpenAICompatMessagesTestAccount(), body, "", "gpt-5.4")
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.Equal(t, "bad upstream", gjson.GetBytes(failoverErr.ResponseBody, "error.message").String())
	require.False(t, OpenAICompatAnthropicClientOutputStarted(c))
	require.Empty(t, rec.Body.String())
}

func TestForwardAsAnthropic_StreamingKeepaliveBeforeResponseFailedDoesNotBlockFailover(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	c, rec := newOpenAICompatMessagesTestContext(body)

	failedFrame := strings.Join([]string{
		`data: {"type":"response.failed","response":{"id":"resp_failed","object":"response","model":"gpt-5.4","status":"failed","error":{"code":"server_error","message":"bad upstream"},"output":[],"usage":{"input_tokens":5,"output_tokens":0,"total_tokens":5}}}`,
		"",
	}, "\n")
	bodyReader := newOpenAICompatDelayedChunkReadCloser(
		[]string{failedFrame},
		[]time.Duration{1500 * time.Millisecond},
	)
	svc := newOpenAICompatMessagesSSEService(bodyReader, "rid_stream_ping_failed", &config.Config{
		Gateway: config.GatewayConfig{StreamKeepaliveInterval: 1},
	})

	result, err := svc.ForwardAsAnthropic(context.Background(), c, newOpenAICompatMessagesTestAccount(), body, "", "gpt-5.4")
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, "bad upstream", gjson.GetBytes(failoverErr.ResponseBody, "error.message").String())
	require.False(t, OpenAICompatAnthropicClientOutputStarted(c))
	require.Contains(t, rec.Body.String(), `event: ping`)
}

func TestForwardAsAnthropic_StreamingKeepaliveBeforePolicyFailureWritesSSEError(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	c, rec := newOpenAICompatMessagesTestContext(body)

	failedFrame := strings.Join([]string{
		`data: {"type":"response.failed","response":{"id":"resp_failed","object":"response","model":"gpt-5.4","status":"failed","error":{"code":"content_policy_violation","message":"request violates safety policy"},"output":[],"usage":{"input_tokens":5,"output_tokens":0,"total_tokens":5}}}`,
		"",
	}, "\n")
	bodyReader := newOpenAICompatDelayedChunkReadCloser(
		[]string{failedFrame},
		[]time.Duration{1500 * time.Millisecond},
	)
	svc := newOpenAICompatMessagesSSEService(bodyReader, "rid_stream_ping_policy_failed", &config.Config{
		Gateway: config.GatewayConfig{StreamKeepaliveInterval: 1},
	})

	result, err := svc.ForwardAsAnthropic(context.Background(), c, newOpenAICompatMessagesTestAccount(), body, "", "gpt-5.4")
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	require.NotNil(t, result)
	require.Equal(t, 5, result.Usage.InputTokens)
	require.False(t, OpenAICompatAnthropicClientOutputStarted(c))
	require.Contains(t, rec.Body.String(), `event: ping`)
	require.Contains(t, rec.Body.String(), `event: error`)
	require.Contains(t, rec.Body.String(), `request violates safety policy`)
	require.NotContains(t, rec.Body.String(), "\n\n{\"error\"")
}

func TestForwardAsAnthropic_StreamingResponseFailedAfterOutputReturnsUsage(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	c, rec := newOpenAICompatMessagesTestContext(body)

	upstreamBody := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_failed","object":"response","model":"gpt-5.4","status":"in_progress","output":[]}}`,
		"",
		`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hello"}`,
		"",
		`data: {"type":"response.failed","response":{"id":"resp_failed","object":"response","model":"gpt-5.4","status":"failed","error":{"code":"server_error","message":"late upstream failure"},"output":[],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`,
		"",
	}, "\n")
	svc := newOpenAICompatMessagesSSEService(io.NopCloser(strings.NewReader(upstreamBody)), "rid_stream_late_failed", nil)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, newOpenAICompatMessagesTestAccount(), body, "", "gpt-5.4")
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	require.NotNil(t, result)
	require.Equal(t, 5, result.Usage.InputTokens)
	require.Equal(t, 2, result.Usage.OutputTokens)
	require.True(t, OpenAICompatAnthropicClientOutputStarted(c))
	require.Contains(t, rec.Body.String(), `event: content_block_delta`)
	require.Contains(t, rec.Body.String(), `late upstream failure`)

	events := openAICompatOpsEvents(t, c)
	require.Len(t, events, 1)
	require.Equal(t, "stream_failed", events[0].Kind)
	require.Equal(t, "/v1/responses", events[0].UpstreamEndpoint)
}

func TestForwardAsAnthropic_StreamingBareErrorBeforeOutputTriggersFailover(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	c, rec := newOpenAICompatMessagesTestContext(body)
	upstreamBody := strings.Join([]string{
		`data: {"type":"error","error":{"code":"server_error","message":"bare upstream failure"}}`,
		"",
		`data: {"type":"response.output_text.delta","delta":"must not leak"}`,
		"",
	}, "\n")
	svc := newOpenAICompatMessagesSSEService(io.NopCloser(strings.NewReader(upstreamBody)), "rid_bare_error", nil)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, newOpenAICompatMessagesTestAccount(), body, "", "gpt-5.4")
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, "bare upstream failure", gjson.GetBytes(failoverErr.ResponseBody, "error.message").String())
	require.False(t, OpenAICompatAnthropicClientOutputStarted(c))
	require.Empty(t, rec.Body.String())
}

func TestForwardAsAnthropic_StreamingEventNamedBareErrorSuppliesType(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	c, rec := newOpenAICompatMessagesTestContext(body)
	upstreamBody := "event: error\ndata: {\"error\":{\"code\":\"server_error\",\"message\":\"event named failure\"}}\n\n"
	svc := newOpenAICompatMessagesSSEService(io.NopCloser(strings.NewReader(upstreamBody)), "rid_named_bare_error", nil)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, newOpenAICompatMessagesTestAccount(), body, "", "gpt-5.4")
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, "event named failure", gjson.GetBytes(failoverErr.ResponseBody, "error.message").String())
	require.Empty(t, rec.Body.String())
}

func TestForwardAsAnthropic_StreamingBareErrorAfterOutputReturnsUsageOnce(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	c, rec := newOpenAICompatMessagesTestContext(body)
	upstreamBody := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_bare","model":"gpt-5.4","status":"in_progress","output":[]}}`, "",
		`data: {"type":"response.output_text.delta","delta":"hello"}`, "",
		`data: {"type":"error","error":{"code":"server_error","message":"first terminal"},"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}`, "",
		`data: {"type":"error","error":{"code":"server_error","message":"second terminal"}}`, "",
	}, "\n")
	svc := newOpenAICompatMessagesSSEService(io.NopCloser(strings.NewReader(upstreamBody)), "rid_late_bare_error", nil)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, newOpenAICompatMessagesTestAccount(), body, "", "gpt-5.4")
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	require.NotNil(t, result)
	require.Equal(t, 7, result.Usage.InputTokens)
	require.Equal(t, 3, result.Usage.OutputTokens)
	require.Equal(t, 1, strings.Count(rec.Body.String(), "event: error"))
	require.Contains(t, rec.Body.String(), "first terminal")
	require.NotContains(t, rec.Body.String(), "second terminal")
	require.NotContains(t, rec.Body.String(), "event: message_stop")
	require.Len(t, openAICompatOpsEvents(t, c), 1)
}

func TestForwardAsAnthropic_StreamingBarePolicyErrorDoesNotFailover(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	c, rec := newOpenAICompatMessagesTestContext(body)
	upstreamBody := `data: {"type":"error","error":{"code":"content_policy_violation","message":"request violates safety policy"},"usage":{"input_tokens":5,"output_tokens":0,"total_tokens":5}}` + "\n\n"
	svc := newOpenAICompatMessagesSSEService(io.NopCloser(strings.NewReader(upstreamBody)), "rid_bare_policy", nil)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, newOpenAICompatMessagesTestAccount(), body, "", "gpt-5.4")
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	require.NotNil(t, result)
	require.Equal(t, 5, result.Usage.InputTokens)
	require.Contains(t, rec.Body.String(), "request violates safety policy")
}

func TestForwardAsAnthropic_StreamingBareErrorLatePolicyMarkerDoesNotFailover(t *testing.T) {
	message := "authorization=" + strings.Repeat("x", openAISensitiveDiagnosticKVValueMaxScan+1) + " request violates safety policy"
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	c, rec := newOpenAICompatMessagesTestContext(body)
	upstreamBody := fmt.Sprintf("event: error\ndata: {\"error\":{\"code\":\"server_error\",\"type\":\"upstream_error\",\"message\":%q}}\n\n", message)
	svc := newOpenAICompatMessagesSSEService(io.NopCloser(strings.NewReader(upstreamBody)), "rid_late_policy", nil)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, newOpenAICompatMessagesTestAccount(), body, "", "gpt-5.4")
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	require.NotNil(t, result)
	outwardMessage := gjson.GetBytes(rec.Body.Bytes(), "error.message").String()
	require.Contains(t, outwardMessage, "authorization=[redacted]")
	require.NotContains(t, outwardMessage, strings.Repeat("x", 64))
	require.NotContains(t, outwardMessage, "request violates safety policy")
	require.LessOrEqual(t, len(outwardMessage), openAIMessagesErrorMessageMaxBytes)
}

func TestForwardAsAnthropic_StreamingMalformedBareErrorRemainsMissingTerminal(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	c, rec := newOpenAICompatMessagesTestContext(body)
	upstreamBody := "event: error\ndata: {not-json}\n\ndata: [DONE]\n\n"
	svc := newOpenAICompatMessagesSSEService(io.NopCloser(strings.NewReader(upstreamBody)), "rid_malformed_bare", nil)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, newOpenAICompatMessagesTestAccount(), body, "", "gpt-5.4")
	require.NotNil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Contains(t, string(failoverErr.ResponseBody), "terminal event")
	require.Empty(t, rec.Body.String())
}

func TestForwardAsAnthropic_StreamingBareErrorMessageIsRedactedAndCapped(t *testing.T) {
	secrets := []string{"bearer-secret", "auth-secret", "api-secret", "refresh-secret", "person@example.com", "acct-secret", "123e4567-e89b-12d3-a456-426614174000", "query-secret"}
	longMessage := `failure Bearer bearer-secret authorization=auth-secret api_key=api-secret refresh_token=refresh-secret ` +
		`{"email":"person@example.com","chatgpt_account_id":"acct-secret"} uuid=123e4567-e89b-12d3-a456-426614174000 ` +
		`https://host.invalid/path?access_token=query-secret ` + strings.Repeat("x", openAIMessagesErrorMessageMaxBytes*2)
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	c, rec := newOpenAICompatMessagesTestContext(body)
	upstreamBody := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"hello"}`, "",
		fmt.Sprintf(`data: {"type":"error","error":{"code":"server_error","message":%q}}`, longMessage), "",
	}, "\n")
	svc := newOpenAICompatMessagesSSEService(io.NopCloser(strings.NewReader(upstreamBody)), "rid_large_bare", nil)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, newOpenAICompatMessagesTestAccount(), body, "", "gpt-5.4")
	require.NotNil(t, result)
	require.Error(t, err)
	for _, secret := range secrets {
		require.NotContains(t, err.Error(), secret)
		require.NotContains(t, rec.Body.String(), secret)
	}
	require.Contains(t, rec.Body.String(), "[redacted]")
	require.LessOrEqual(t, len(rec.Body.String()), openAIMessagesErrorMessageMaxBytes+2048)
	events := openAICompatOpsEvents(t, c)
	require.Len(t, events, 1)
	for _, secret := range secrets {
		require.NotContains(t, events[0].Message, secret)
	}
	require.LessOrEqual(t, len(events[0].Message), openAIMessagesErrorMessageMaxBytes)
}

func TestForwardAsAnthropic_StreamingBareErrorAfterClientWriteFailureDoesNotFailoverOrWrite(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	c, rec := newOpenAICompatMessagesTestContext(body)
	failingWriter := &openAICompatFailingWriter{ResponseWriter: c.Writer, failAfter: 0}
	c.Writer = failingWriter
	upstreamBody := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_disconnect","model":"gpt-5.4","status":"in_progress","output":[]}}`, "",
		`data: {"type":"response.output_text.delta","delta":"hello"}`, "",
		`data: {"type":"error","error":{"code":"server_error","message":"terminal after disconnect"},"usage":{"input_tokens":11,"output_tokens":2,"total_tokens":13}}`, "",
	}, "\n")
	svc := newOpenAICompatMessagesSSEService(io.NopCloser(strings.NewReader(upstreamBody)), "rid_disconnected_bare", nil)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, newOpenAICompatMessagesTestAccount(), body, "", "gpt-5.4")
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	require.NotNil(t, result)
	require.True(t, result.ClientDisconnect)
	require.Equal(t, 11, result.Usage.InputTokens)
	require.Equal(t, 2, result.Usage.OutputTokens)
	require.Equal(t, 1, failingWriter.attempts)
	require.Empty(t, rec.Body.String())
}

func TestForwardAsAnthropic_BufferedMissingTerminalTriggersFailover(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c, rec := newOpenAICompatMessagesTestContext(body)

	upstreamBody := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_missing","object":"response","model":"gpt-5.4","status":"in_progress","output":[]}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	svc := newOpenAICompatMessagesSSEService(io.NopCloser(strings.NewReader(upstreamBody)), "rid_buffered_missing_terminal", nil)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, newOpenAICompatMessagesTestAccount(), body, "", "gpt-5.4")
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.Contains(t, string(failoverErr.ResponseBody), "terminal event")
	require.Empty(t, rec.Body.String())
}

func TestForwardAsAnthropic_BufferedBareErrorTriggersFailover(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c, rec := newOpenAICompatMessagesTestContext(body)
	upstreamBody := strings.Join([]string{
		`data: {"type":"error","error":{"code":"server_error","message":"bare buffered failure"},"usage":{"input_tokens":7,"output_tokens":2,"total_tokens":9}}`, "",
		`data: {"type":"response.completed","response":{"id":"must_not_win","status":"completed","output":[]}}`, "",
	}, "\n")
	svc := newOpenAICompatMessagesSSEService(io.NopCloser(strings.NewReader(upstreamBody)), "rid_buffered_bare", nil)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, newOpenAICompatMessagesTestAccount(), body, "", "gpt-5.4")
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, "bare buffered failure", gjson.GetBytes(failoverErr.ResponseBody, "error.message").String())
	require.Empty(t, rec.Body.String())
}

func TestForwardAsAnthropic_BufferedBareResponseErrorTriggersFailover(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c, rec := newOpenAICompatMessagesTestContext(body)
	upstreamBody := strings.Join([]string{
		`data: {"type":"error","response":{"error":{"code":"server_error","message":"nested buffered failure"}},"usage":{"input_tokens":8,"output_tokens":3,"total_tokens":11}}`, "",
		`data: {"type":"response.completed","response":{"id":"must_not_win","status":"completed","output":[]}}`, "",
	}, "\n")
	svc := newOpenAICompatMessagesSSEService(io.NopCloser(strings.NewReader(upstreamBody)), "rid_buffered_nested_bare", nil)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, newOpenAICompatMessagesTestAccount(), body, "", "gpt-5.4")
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, "nested buffered failure", gjson.GetBytes(failoverErr.ResponseBody, "error.message").String())
	require.Empty(t, rec.Body.String())
}

func TestForwardAsAnthropic_BufferedBareResponseErrorPreservesNestedUsage(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c, rec := newOpenAICompatMessagesTestContext(body)
	upstreamBody := `data: {"type":"error","response":{"error":{"code":"content_policy_violation","message":"request violates safety policy"},"usage":{"input_tokens":8,"output_tokens":2,"total_tokens":10}}}` + "\n\n"
	svc := newOpenAICompatMessagesSSEService(io.NopCloser(strings.NewReader(upstreamBody)), "rid_buffered_nested_usage", nil)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, newOpenAICompatMessagesTestAccount(), body, "", "gpt-5.4")
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	require.NotNil(t, result)
	require.Equal(t, 8, result.Usage.InputTokens)
	require.Equal(t, 2, result.Usage.OutputTokens)
	require.Equal(t, "request violates safety policy", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
}

func TestForwardAsAnthropic_BufferedBareResponseErrorTopLevelUsageTakesPrecedence(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c, _ := newOpenAICompatMessagesTestContext(body)
	upstreamBody := `data: {"type":"error","response":{"error":{"code":"content_policy_violation","message":"request violates safety policy"},"usage":{"input_tokens":80,"output_tokens":20,"total_tokens":100}},"usage":{"input_tokens":8,"output_tokens":2,"total_tokens":10}}` + "\n\n"
	svc := newOpenAICompatMessagesSSEService(io.NopCloser(strings.NewReader(upstreamBody)), "rid_buffered_usage_precedence", nil)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, newOpenAICompatMessagesTestAccount(), body, "", "gpt-5.4")
	require.Error(t, err)
	require.NotNil(t, result)
	require.Equal(t, 8, result.Usage.InputTokens)
	require.Equal(t, 2, result.Usage.OutputTokens)
}

func TestForwardAsAnthropic_BufferedBareResponseErrorAccountsFirstTerminalOnce(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c, rec := newOpenAICompatMessagesTestContext(body)
	upstreamBody := strings.Join([]string{
		`data: {"type":"error","response":{"error":{"code":"content_policy_violation","message":"first terminal"},"usage":{"input_tokens":8,"output_tokens":2,"total_tokens":10}}}`, "",
		`data: {"type":"error","response":{"error":{"code":"content_policy_violation","message":"second terminal"},"usage":{"input_tokens":80,"output_tokens":20,"total_tokens":100}}}`, "",
	}, "\n")
	svc := newOpenAICompatMessagesSSEService(io.NopCloser(strings.NewReader(upstreamBody)), "rid_buffered_first_terminal_usage", nil)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, newOpenAICompatMessagesTestAccount(), body, "", "gpt-5.4")
	require.Error(t, err)
	require.NotNil(t, result)
	require.Equal(t, 8, result.Usage.InputTokens)
	require.Equal(t, 2, result.Usage.OutputTokens)
	require.Contains(t, rec.Body.String(), "first terminal")
	require.NotContains(t, rec.Body.String(), "second terminal")
}

func TestForwardAsAnthropic_BufferedBareTopLevelPolicyMessagePreservesUsage(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c, rec := newOpenAICompatMessagesTestContext(body)
	upstreamBody := `data: {"type":"error","message":"request violates safety policy","usage":{"input_tokens":6,"output_tokens":1,"total_tokens":7}}` + "\n\n"
	svc := newOpenAICompatMessagesSSEService(io.NopCloser(strings.NewReader(upstreamBody)), "rid_buffered_top_message", nil)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, newOpenAICompatMessagesTestAccount(), body, "", "gpt-5.4")
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	require.NotNil(t, result)
	require.Equal(t, 6, result.Usage.InputTokens)
	require.Equal(t, 1, result.Usage.OutputTokens)
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Equal(t, "request violates safety policy", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
}

func TestForwardAsAnthropic_BufferedEventNamedBarePolicyErrorPreservesUsage(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c, rec := newOpenAICompatMessagesTestContext(body)
	upstreamBody := "event: error\ndata: {\"error\":{\"code\":\"content_policy_violation\",\"message\":\"request violates safety policy\"},\"usage\":{\"input_tokens\":5,\"output_tokens\":0,\"total_tokens\":5}}\n\n"
	svc := newOpenAICompatMessagesSSEService(io.NopCloser(strings.NewReader(upstreamBody)), "rid_buffered_bare_policy", nil)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, newOpenAICompatMessagesTestAccount(), body, "", "gpt-5.4")
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	require.NotNil(t, result)
	require.Equal(t, 5, result.Usage.InputTokens)
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Equal(t, "request violates safety policy", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
}

func TestForwardAsAnthropic_BufferedBareErrorLatePolicyMarkerDoesNotFailover(t *testing.T) {
	message := strings.Repeat("x", openAIMessagesErrorMessageMaxBytes+512) + " content policy violation"
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c, rec := newOpenAICompatMessagesTestContext(body)
	upstreamBody := fmt.Sprintf("event: error\ndata: {\"error\":{\"code\":\"server_error\",\"type\":\"upstream_error\",\"message\":%q}}\n\n", message)
	svc := newOpenAICompatMessagesSSEService(io.NopCloser(strings.NewReader(upstreamBody)), "rid_buffered_late_policy", nil)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, newOpenAICompatMessagesTestAccount(), body, "", "gpt-5.4")
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	require.NotNil(t, result)
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.LessOrEqual(t, len(gjson.GetBytes(rec.Body.Bytes(), "error.message").String()), openAIMessagesErrorMessageMaxBytes)
}

func TestForwardAsAnthropic_BufferedBareErrorMessageIsRedactedAndCapped(t *testing.T) {
	const secret = "buffered-secret-token"
	message := "failure ?access_token=" + secret + " " + strings.Repeat("界", openAIMessagesErrorMessageMaxBytes)
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c, rec := newOpenAICompatMessagesTestContext(body)
	upstreamBody := fmt.Sprintf("event: error\ndata: {\"error\":{\"code\":\"content_policy_violation\",\"message\":%q}}\n\n", message)
	svc := newOpenAICompatMessagesSSEService(io.NopCloser(strings.NewReader(upstreamBody)), "rid_buffered_bare_large", nil)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, newOpenAICompatMessagesTestAccount(), body, "", "gpt-5.4")
	require.NotNil(t, result)
	require.Error(t, err)
	require.NotContains(t, err.Error(), secret)
	require.NotContains(t, rec.Body.String(), secret)
	require.True(t, utf8.Valid(rec.Body.Bytes()))
	require.LessOrEqual(t, len(gjson.GetBytes(rec.Body.Bytes(), "error.message").String()), openAIMessagesErrorMessageMaxBytes)
}

func TestForwardAsAnthropic_BufferedTopLevelResponseFailedTriggersFailover(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c, _ := newOpenAICompatMessagesTestContext(body)

	upstreamBody := strings.Join([]string{
		`event: response.failed`,
		`data: {"id":"resp_failed","object":"response","model":"gpt-5.4","status":"failed","error":{"code":"server_error","message":"bad upstream"},"output":[],"usage":{"input_tokens":5,"output_tokens":0,"total_tokens":5}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	svc := newOpenAICompatMessagesSSEService(io.NopCloser(strings.NewReader(upstreamBody)), "rid_buffered_top_failed", nil)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, newOpenAICompatMessagesTestAccount(), body, "", "gpt-5.4")
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, "bad upstream", gjson.GetBytes(failoverErr.ResponseBody, "error.message").String())
}

func TestForwardAsAnthropic_BufferedPolicyResponseFailedDoesNotFailover(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c, rec := newOpenAICompatMessagesTestContext(body)

	upstreamBody := strings.Join([]string{
		`data: {"type":"response.failed","response":{"id":"resp_failed","object":"response","model":"gpt-5.4","status":"failed","error":{"code":"content_policy_violation","message":"request violates safety policy"},"output":[],"usage":{"input_tokens":5,"output_tokens":0,"total_tokens":5}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	svc := newOpenAICompatMessagesSSEService(io.NopCloser(strings.NewReader(upstreamBody)), "rid_buffered_policy_failed", nil)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, newOpenAICompatMessagesTestAccount(), body, "", "gpt-5.4")
	require.NotNil(t, result)
	require.Equal(t, 5, result.Usage.InputTokens)
	require.False(t, result.Stream)
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Equal(t, "error", gjson.GetBytes(rec.Body.Bytes(), "type").String())
	require.Equal(t, "request violates safety policy", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
}

func TestForwardAsAnthropic_BufferedInvalidRequestTypeDoesNotFailover(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c, rec := newOpenAICompatMessagesTestContext(body)

	upstreamBody := strings.Join([]string{
		`data: {"type":"response.failed","response":{"id":"resp_failed","object":"response","model":"gpt-5.4","status":"failed","error":{"type":"invalid_request_error","message":"Unsupported parameter"},"output":[],"usage":{"input_tokens":5,"output_tokens":0,"total_tokens":5}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	svc := newOpenAICompatMessagesSSEService(io.NopCloser(strings.NewReader(upstreamBody)), "rid_buffered_invalid_request_type", nil)

	result, err := svc.ForwardAsAnthropic(context.Background(), c, newOpenAICompatMessagesTestAccount(), body, "", "gpt-5.4")
	require.NotNil(t, result)
	require.Equal(t, 5, result.Usage.InputTokens)
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Equal(t, "Unsupported parameter", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
}

func TestOpenAIStreamFailoverErrorHonorsPoolModeRetryStatusPolicy(t *testing.T) {
	t.Parallel()

	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":                      "sk-test",
			"pool_mode":                    true,
			"pool_mode_retry_status_codes": []any{},
		},
	}
	svc := &OpenAIGatewayService{}
	payload := []byte(`{"type":"response.failed","response":{"error":{"code":"server_error","message":"server overloaded"}}}`)

	err := svc.newOpenAIStreamFailoverError(nil, account, false, "rid", payload, "server overloaded")
	require.False(t, err.RetryableOnSameAccount)
	require.Equal(t, http.StatusBadGateway, err.StatusCode)
	fact, ok := err.UpstreamFact()
	require.True(t, ok)
	require.False(t, fact.HTTPStatusKnown)
	require.Zero(t, fact.HTTPStatus)
	require.Equal(t, "server_error", fact.ProviderCode)
	require.Equal(t, "server overloaded", fact.SafeMessage)

	account.Credentials["pool_mode_retry_status_codes"] = []any{502}
	err = svc.newOpenAIStreamFailoverError(nil, account, false, "rid", payload, "server overloaded")
	require.True(t, err.RetryableOnSameAccount)
}

func TestForwardAsAnthropic_BufferedResponseForcesJSONContentType(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := strings.Join([]string{
		`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","model":"gpt-5.4","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_json"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}

	svc := &OpenAIGatewayService{
		httpUpstream:         upstream,
		cfg:                  &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
		responseHeaderFilter: responseheaders.CompileHeaderFilter(config.ResponseHeaderConfig{}),
	}
	account := &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-5.4")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
	require.Equal(t, "ok", gjson.GetBytes(rec.Body.Bytes(), "content.0.text").String())
}

func TestForwardAsAnthropic_MappedClaudeModelAcceptsChatUsageShape(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"claude-opus-4-7","max_tokens":16,"messages":[{"role":"user","content":"compact this"}],"stream":true}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_compact","model":"gpt-5.5","status":"in_progress","output":[]}}`,
		"",
		`data: {"type":"response.output_text.delta","delta":"ok"}`,
		"",
		`data: {"type":"response.completed","response":{"id":"resp_compact","object":"response","model":"gpt-5.5","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"prompt_tokens":31,"completion_tokens":9,"total_tokens":40,"prompt_tokens_details":{"cached_tokens":11}}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_compact_usage"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}

	svc := &OpenAIGatewayService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          1,
		Name:        "openai-apikey",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://api.openai.com/v1",
			"model_mapping": map[string]any{
				"gpt-5.5": "gpt-5.5",
			},
		},
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-5.5")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "claude-opus-4-7", result.Model)
	require.Equal(t, "gpt-5.5", result.BillingModel)
	require.Equal(t, "gpt-5.5", result.UpstreamModel)
	require.Equal(t, 31, result.Usage.InputTokens)
	require.Equal(t, 9, result.Usage.OutputTokens)
	require.Equal(t, 11, result.Usage.CacheReadInputTokens)
	require.Equal(t, "gpt-5.5", gjson.GetBytes(upstream.lastBody, "model").String())
}

func TestForwardAsAnthropic_InjectsPromptCacheKeyForAPIKeyMessagesDispatch(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"claude-sonnet-4-5","max_tokens":16,"metadata":{"user_id":"claude-session-1"},"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := strings.Join([]string{
		`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","model":"gpt-5.3-codex","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7,"input_tokens_details":{"cached_tokens":3}}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_cache_key"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}

	svc := &OpenAIGatewayService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          1,
		Name:        "openai-apikey",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://api.openai.com/v1",
		},
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "stable-cache-key", "gpt-5.3-codex")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "stable-cache-key", gjson.GetBytes(upstream.lastBody, "prompt_cache_key").String())
	require.Equal(t, "gpt-5.3-codex", gjson.GetBytes(upstream.lastBody, "model").String())
	require.Equal(t, 3, result.Usage.CacheReadInputTokens)
}

func TestForwardAsAnthropic_AutoDerivesPromptCacheKeyWhenMessagesDispatchHasNoSessionID(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"claude-sonnet-4-5","max_tokens":16,"system":"You are helpful.","messages":[{"role":"user","content":"open repo"}],"stream":false}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := strings.Join([]string{
		`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","model":"gpt-5.3-codex","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7,"input_tokens_details":{"cached_tokens":3}}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_auto_cache_key"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}

	svc := &OpenAIGatewayService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          1,
		Name:        "openai-apikey",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://api.openai.com/v1",
		},
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-5.3-codex")
	require.NoError(t, err)
	require.NotNil(t, result)
	cacheKey := gjson.GetBytes(upstream.lastBody, "prompt_cache_key").String()
	require.NotEmpty(t, cacheKey)
	require.True(t, strings.HasPrefix(cacheKey, "anthropic-digest-"))
	require.Equal(t, generateSessionUUID(isolateOpenAISessionID(0, cacheKey)), upstream.lastReq.Header.Get("session_id"))
}

func TestForwardAsAnthropic_DoesNotAutoDerivePromptCacheKeyForNonCodexModel(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := strings.Join([]string{
		`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","model":"gpt-4o","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_no_cache_key"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}

	svc := &OpenAIGatewayService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          1,
		Name:        "openai-apikey",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://api.openai.com/v1",
		},
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-4o")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, gjson.GetBytes(upstream.lastBody, "prompt_cache_key").Exists())
	require.Empty(t, upstream.lastReq.Header.Get("session_id"))
}

func TestForwardAsAnthropic_KeepsFullReplayWithoutContinuation(t *testing.T) {
	t.Parallel()

	const messageCount = 15
	messages := make([]string, 0, messageCount)
	for i := 0; i < messageCount; i++ {
		messages = append(messages, `{"role":"user","content":"message-`+fmt.Sprintf("%02d", i)+`"}`)
	}
	body := []byte(`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[` + strings.Join(messages, ",") + `],"stream":false}`)

	run := func(t *testing.T, mappedModel string) []byte {
		t.Helper()

		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")

		upstreamBody := strings.Join([]string{
			`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","model":"` + mappedModel + `","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`,
			"",
			"data: [DONE]",
			"",
		}, "\n")
		upstream := &httpUpstreamRecorder{resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_trim"}},
			Body:       io.NopCloser(strings.NewReader(upstreamBody)),
		}}

		svc := &OpenAIGatewayService{
			httpUpstream: upstream,
			cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
		}
		account := &Account{
			ID:          1,
			Name:        "openai-apikey",
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Concurrency: 1,
			Credentials: map[string]any{
				"api_key":  "sk-test",
				"base_url": "https://api.openai.com/v1",
			},
		}

		result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", mappedModel)
		require.NoError(t, err)
		require.NotNil(t, result)
		return upstream.lastBody
	}

	codexBody := run(t, "gpt-5.3-codex")
	require.Equal(t, int64(messageCount+1), gjson.GetBytes(codexBody, "input.#").Int())
	require.Equal(t, "developer", gjson.GetBytes(codexBody, "input.0.role").String())
	require.Contains(t, gjson.GetBytes(codexBody, "input.0.content.0.text").String(), "<sub2api-claude-code-todo-guard>")
	require.Equal(t, "message-00", gjson.GetBytes(codexBody, "input.1.content.0.text").String())
	require.Equal(t, "message-14", gjson.GetBytes(codexBody, "input.15.content.0.text").String())

	nonCompatBody := run(t, "gpt-4o")
	require.Equal(t, int64(messageCount), gjson.GetBytes(nonCompatBody, "input.#").Int())
	require.Equal(t, "message-00", gjson.GetBytes(nonCompatBody, "input.0.content.0.text").String())
}

func TestForwardAsAnthropic_OAuthCompatKeepsFullReplayForCacheGrowth(t *testing.T) {
	t.Parallel()

	const messageCount = 15
	messages := make([]string, 0, messageCount)
	for i := 0; i < messageCount; i++ {
		messages = append(messages, `{"role":"user","content":"message-`+fmt.Sprintf("%02d", i)+`"}`)
	}
	body := []byte(`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[` + strings.Join(messages, ",") + `],"stream":false}`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_oauth_trim", "gpt-5.4")}
	svc := &OpenAIGatewayService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-5.4")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, int64(messageCount+1), gjson.GetBytes(upstream.lastBody, "input.#").Int())
	require.Equal(t, "developer", gjson.GetBytes(upstream.lastBody, "input.0.role").String())
	require.Contains(t, gjson.GetBytes(upstream.lastBody, "input.0.content.0.text").String(), "<sub2api-claude-code-todo-guard>")
	require.Equal(t, "message-00", gjson.GetBytes(upstream.lastBody, "input.1.content.0.text").String())
	require.Equal(t, "message-14", gjson.GetBytes(upstream.lastBody, "input.15.content.0.text").String())
	require.False(t, gjson.GetBytes(upstream.lastBody, "prompt_cache_key").Exists())
}

func TestForwardAsAnthropic_AttachesPreviousResponseIDForCompatContinuation(t *testing.T) {
	t.Parallel()

	upstream := &httpUpstreamRecorder{}
	svc := &OpenAIGatewayService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          1,
		Name:        "openai-apikey",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://api.openai.com/v1",
		},
	}

	firstBody := []byte(`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"first"}],"stream":false}`)
	upstream.resp = openAICompatSSECompletedResponse("resp_first", "gpt-5.3-codex")
	firstRec := httptest.NewRecorder()
	firstCtx, _ := gin.CreateTestContext(firstRec)
	firstCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(firstBody))
	firstCtx.Request.Header.Set("Content-Type", "application/json")

	firstResult, err := svc.ForwardAsAnthropic(context.Background(), firstCtx, account, firstBody, "stable-cache-key", "gpt-5.3-codex")
	require.NoError(t, err)
	require.NotNil(t, firstResult)
	require.Equal(t, "resp_first", firstResult.ResponseID)
	require.False(t, gjson.GetBytes(upstream.lastBody, "previous_response_id").Exists())

	secondBody := []byte(`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"ok"},{"role":"user","content":"second"}],"stream":false}`)
	upstream.resp = openAICompatSSECompletedResponse("resp_second", "gpt-5.3-codex")
	secondRec := httptest.NewRecorder()
	secondCtx, _ := gin.CreateTestContext(secondRec)
	secondCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(secondBody))
	secondCtx.Request.Header.Set("Content-Type", "application/json")

	secondResult, err := svc.ForwardAsAnthropic(context.Background(), secondCtx, account, secondBody, "stable-cache-key", "gpt-5.3-codex")
	require.NoError(t, err)
	require.NotNil(t, secondResult)
	require.Equal(t, "resp_second", secondResult.ResponseID)
	require.Equal(t, "resp_first", gjson.GetBytes(upstream.lastBody, "previous_response_id").String())
	require.Equal(t, int64(2), gjson.GetBytes(upstream.lastBody, "input.#").Int())
	require.Equal(t, "developer", gjson.GetBytes(upstream.lastBody, "input.0.role").String())
	require.Contains(t, gjson.GetBytes(upstream.lastBody, "input.0.content.0.text").String(), "<sub2api-claude-code-todo-guard>")
	require.Equal(t, "second", gjson.GetBytes(upstream.lastBody, "input.1.content.0.text").String())
}

func TestForwardAsAnthropic_PreviousResponseIDKeepsMultiToolCallContext(t *testing.T) {
	t.Parallel()

	upstream := &httpUpstreamRecorder{}
	svc := &OpenAIGatewayService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          1,
		Name:        "openai-apikey",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://api.openai.com/v1",
		},
	}

	firstBody := []byte(`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"inspect files"}],"stream":false}`)
	upstream.resp = openAICompatSSECompletedResponse("resp_first_tools", "gpt-5.3-codex")
	firstRec := httptest.NewRecorder()
	firstCtx, _ := gin.CreateTestContext(firstRec)
	firstCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(firstBody))
	firstCtx.Request.Header.Set("Content-Type", "application/json")

	firstResult, err := svc.ForwardAsAnthropic(context.Background(), firstCtx, account, firstBody, "stable-cache-key", "gpt-5.3-codex")
	require.NoError(t, err)
	require.NotNil(t, firstResult)

	secondBody := []byte(`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"inspect files"},{"role":"assistant","content":[{"type":"tool_use","id":"call_one","name":"Read","input":{"file_path":"a.go"}},{"type":"tool_use","id":"call_two","name":"Read","input":{"file_path":"b.go"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_one","content":"package a"},{"type":"tool_result","tool_use_id":"call_two","content":"package b"},{"type":"text","text":"continue"}]}],"tools":[{"name":"Read","description":"read a file","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}}}}],"stream":false}`)
	upstream.resp = openAICompatSSECompletedResponse("resp_second_tools", "gpt-5.3-codex")
	secondRec := httptest.NewRecorder()
	secondCtx, _ := gin.CreateTestContext(secondRec)
	secondCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(secondBody))
	secondCtx.Request.Header.Set("Content-Type", "application/json")

	secondResult, err := svc.ForwardAsAnthropic(context.Background(), secondCtx, account, secondBody, "stable-cache-key", "gpt-5.3-codex")
	require.NoError(t, err)
	require.NotNil(t, secondResult)
	require.Equal(t, "resp_first_tools", gjson.GetBytes(upstream.lastBody, "previous_response_id").String())

	require.Equal(t, "function_call", gjson.GetBytes(upstream.lastBody, "input.1.type").String())
	require.Equal(t, "call_one", gjson.GetBytes(upstream.lastBody, "input.1.call_id").String())
	require.Equal(t, "function_call", gjson.GetBytes(upstream.lastBody, "input.2.type").String())
	require.Equal(t, "call_two", gjson.GetBytes(upstream.lastBody, "input.2.call_id").String())
	require.Equal(t, "function_call_output", gjson.GetBytes(upstream.lastBody, "input.3.type").String())
	require.Equal(t, "call_one", gjson.GetBytes(upstream.lastBody, "input.3.call_id").String())
	require.Equal(t, "function_call_output", gjson.GetBytes(upstream.lastBody, "input.4.type").String())
	require.Equal(t, "call_two", gjson.GetBytes(upstream.lastBody, "input.4.call_id").String())
	require.Equal(t, "continue", gjson.GetBytes(upstream.lastBody, "input.5.content.0.text").String())
}

func TestForwardAsAnthropic_ReplaysWithoutContinuationWhenPreviousResponseMissing(t *testing.T) {
	t.Parallel()

	upstream := &httpUpstreamRecorder{}
	svc := &OpenAIGatewayService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          1,
		Name:        "openai-apikey",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://api.openai.com/v1",
		},
	}

	svc.bindOpenAICompatSessionResponseID(context.Background(), nil, account, "stable-cache-key", "resp_missing")
	const messageCount = 15
	messages := make([]string, 0, messageCount)
	for i := 0; i < messageCount; i++ {
		messages = append(messages, `{"role":"user","content":"message-`+fmt.Sprintf("%02d", i)+`"}`)
	}
	secondBody := []byte(`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[` + strings.Join(messages, ",") + `],"stream":false}`)
	upstream.responses = []*http.Response{
		{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid_prev_missing"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"previous_response_not_found","message":"previous response not found"}}`)),
		},
		openAICompatSSECompletedResponse("resp_replayed", "gpt-5.3-codex"),
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(secondBody))
	c.Request.Header.Set("Content-Type", "application/json")

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, secondBody, "stable-cache-key", "gpt-5.3-codex")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "resp_replayed", result.ResponseID)
	require.Len(t, upstream.requests, 2)
	require.Equal(t, "resp_missing", gjson.GetBytes(upstream.bodies[0], "previous_response_id").String())
	require.False(t, gjson.GetBytes(upstream.bodies[1], "previous_response_id").Exists())
	require.Equal(t, int64(messageCount+1), gjson.GetBytes(upstream.bodies[1], "input.#").Int())
	require.Equal(t, "developer", gjson.GetBytes(upstream.bodies[1], "input.0.role").String())
	require.Contains(t, gjson.GetBytes(upstream.bodies[1], "input.0.content.0.text").String(), "<sub2api-claude-code-todo-guard>")
	require.Equal(t, "message-00", gjson.GetBytes(upstream.bodies[1], "input.1.content.0.text").String())
	require.Equal(t, "message-14", gjson.GetBytes(upstream.bodies[1], "input.15.content.0.text").String())
}

func TestForwardAsAnthropic_DisablesAPIKeyContinuationWhenUpstreamRequiresWebSocketV2(t *testing.T) {
	t.Parallel()

	upstream := &httpUpstreamRecorder{}
	svc := &OpenAIGatewayService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          1,
		Name:        "openai-apikey",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://api.openai.com/v1",
		},
	}

	svc.bindOpenAICompatSessionResponseID(context.Background(), nil, account, "stable-cache-key", "resp_http_unsupported")
	body := []byte(`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"ok"},{"role":"user","content":"second"}],"stream":false}`)
	upstream.responses = []*http.Response{
		{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid_prev_http_unsupported"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"previous_response_id is only supported on Responses WebSocket v2","type":"invalid_request_error"}}`)),
		},
		openAICompatSSECompletedResponse("resp_replayed", "gpt-5.5"),
		openAICompatSSECompletedResponse("resp_later", "gpt-5.5"),
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "stable-cache-key", "gpt-5.5")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "resp_replayed", result.ResponseID)
	require.Len(t, upstream.requests, 2)
	require.Equal(t, "resp_http_unsupported", gjson.GetBytes(upstream.bodies[0], "previous_response_id").String())
	require.False(t, gjson.GetBytes(upstream.bodies[1], "previous_response_id").Exists())

	laterRec := httptest.NewRecorder()
	laterCtx, _ := gin.CreateTestContext(laterRec)
	laterCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	laterCtx.Request.Header.Set("Content-Type", "application/json")

	laterResult, err := svc.ForwardAsAnthropic(context.Background(), laterCtx, account, body, "stable-cache-key", "gpt-5.5")
	require.NoError(t, err)
	require.NotNil(t, laterResult)
	require.Equal(t, "resp_later", laterResult.ResponseID)
	require.Len(t, upstream.requests, 3)
	require.False(t, gjson.GetBytes(upstream.bodies[2], "previous_response_id").Exists())
}

func TestForwardAsAnthropic_APIKeyMetadataSessionSurvivesChangingCacheControlAnchorAfterContinuationDisabled(t *testing.T) {
	t.Parallel()

	metadata := `{"user_id":"{\"device_id\":\"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\",\"account_uuid\":\"\",\"session_id\":\"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa\"}"}`
	firstBody := []byte(`{"model":"claude-haiku-4-5-20251001","max_tokens":16,"metadata":` + metadata + `,"system":[{"type":"text","text":"project docs","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"first"}],"stream":false}`)
	const messageCount = 16
	messages := make([]string, 0, messageCount)
	messages = append(messages, `{"role":"user","content":[{"type":"text","text":"rewritten context","cache_control":{"type":"ephemeral"}}]}`)
	for i := 1; i < messageCount; i++ {
		messages = append(messages, `{"role":"user","content":"message-`+fmt.Sprintf("%02d", i)+`"}`)
	}
	secondBody := []byte(`{"model":"claude-haiku-4-5-20251001","max_tokens":16,"metadata":` + metadata + `,"messages":[` + strings.Join(messages, ",") + `],"stream":false}`)

	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		openAICompatSSECompletedResponse("resp_first", "gpt-5.4-mini"),
		openAICompatSSECompletedResponse("resp_second", "gpt-5.4-mini"),
	}}
	svc := &OpenAIGatewayService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          1,
		Name:        "openai-apikey",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://api.openai.com/v1",
		},
	}

	firstRec := httptest.NewRecorder()
	firstCtx, _ := gin.CreateTestContext(firstRec)
	firstCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(firstBody))
	firstCtx.Request.Header.Set("Content-Type", "application/json")

	firstResult, err := svc.ForwardAsAnthropic(context.Background(), firstCtx, account, firstBody, "", "gpt-5.4-mini")
	require.NoError(t, err)
	require.NotNil(t, firstResult)
	firstKey := gjson.GetBytes(upstream.bodies[0], "prompt_cache_key").String()
	require.NotEmpty(t, firstKey)
	require.True(t, strings.HasPrefix(firstKey, "anthropic-metadata-"))

	svc.disableOpenAICompatSessionContinuation(context.Background(), nil, account, firstKey)

	secondRec := httptest.NewRecorder()
	secondCtx, _ := gin.CreateTestContext(secondRec)
	secondCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(secondBody))
	secondCtx.Request.Header.Set("Content-Type", "application/json")

	secondResult, err := svc.ForwardAsAnthropic(context.Background(), secondCtx, account, secondBody, "", "gpt-5.4-mini")
	require.NoError(t, err)
	require.NotNil(t, secondResult)
	require.Len(t, upstream.requests, 2)
	require.Equal(t, firstKey, gjson.GetBytes(upstream.bodies[1], "prompt_cache_key").String())
	require.False(t, gjson.GetBytes(upstream.bodies[1], "previous_response_id").Exists())
	require.Equal(t, int64(messageCount+1), gjson.GetBytes(upstream.bodies[1], "input.#").Int())
	require.Equal(t, "developer", gjson.GetBytes(upstream.bodies[1], "input.0.role").String())
	require.Contains(t, gjson.GetBytes(upstream.bodies[1], "input.0.content.0.text").String(), "<sub2api-claude-code-todo-guard>")
	require.Equal(t, "rewritten context", gjson.GetBytes(upstream.bodies[1], "input.1.content.0.text").String())
	require.Equal(t, "message-15", gjson.GetBytes(upstream.bodies[1], "input.16.content.0.text").String())
}

func TestForwardAsAnthropic_DoesNotAttachPreviousResponseIDForOAuthCompat(t *testing.T) {
	t.Parallel()

	upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_oauth_next", "gpt-5.4")}
	svc := &OpenAIGatewayService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}
	svc.bindOpenAICompatSessionResponseID(context.Background(), nil, account, "stable-cache-key", "resp_oauth_prev")

	body := []byte(`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"ok"},{"role":"user","content":"second"}],"stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "stable-cache-key", "gpt-5.4")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, gjson.GetBytes(upstream.lastBody, "previous_response_id").Exists())
}

func TestForwardAsAnthropic_ReusesOAuthCodexTurnState(t *testing.T) {
	t.Parallel()

	firstResp := openAICompatSSECompletedResponse("resp_oauth_first", "gpt-5.4")
	firstResp.Header.Set("x-codex-turn-state", "turn_state_first")
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		firstResp,
		openAICompatSSECompletedResponse("resp_oauth_second", "gpt-5.4"),
	}}
	svc := &OpenAIGatewayService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}

	firstBody := []byte(`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"first"}],"stream":false}`)
	firstRec := httptest.NewRecorder()
	firstCtx, _ := gin.CreateTestContext(firstRec)
	firstCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(firstBody))
	firstCtx.Request.Header.Set("Content-Type", "application/json")

	firstResult, err := svc.ForwardAsAnthropic(context.Background(), firstCtx, account, firstBody, "stable-cache-key", "gpt-5.4")
	require.NoError(t, err)
	require.NotNil(t, firstResult)
	require.Empty(t, upstream.requests[0].Header.Get("x-codex-turn-state"))
	require.Empty(t, upstream.requests[0].Header.Get("OpenAI-Beta"))
	require.Equal(t, codexOfficialOriginator, upstream.requests[0].Header.Get("originator"))

	secondBody := []byte(`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"ok"},{"role":"user","content":"second"}],"stream":false}`)
	secondRec := httptest.NewRecorder()
	secondCtx, _ := gin.CreateTestContext(secondRec)
	secondCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(secondBody))
	secondCtx.Request.Header.Set("Content-Type", "application/json")

	secondResult, err := svc.ForwardAsAnthropic(context.Background(), secondCtx, account, secondBody, "stable-cache-key", "gpt-5.4")
	require.NoError(t, err)
	require.NotNil(t, secondResult)
	require.Equal(t, "turn_state_first", upstream.requests[1].Header.Get("x-codex-turn-state"))
	require.Equal(t, isolateOpenAICodexOAuthSessionID(0, "stable-cache-key", "session"), upstream.requests[1].Header.Get("session_id"))
	require.Empty(t, upstream.requests[1].Header.Get("conversation_id"))
	require.Empty(t, upstream.requests[1].Header.Get("OpenAI-Beta"))
	require.Equal(t, codexOfficialOriginator, upstream.requests[1].Header.Get("originator"))
	require.False(t, gjson.GetBytes(upstream.bodies[1], "prompt_cache_key").Exists())
	require.False(t, gjson.GetBytes(upstream.bodies[1], "previous_response_id").Exists())
}

func TestForwardAsAnthropic_OAuthDigestFallbackReusesTurnStateWithoutExplicitKey(t *testing.T) {
	t.Parallel()

	firstResp := openAICompatSSECompletedResponse("resp_oauth_digest_first", "gpt-5.4")
	firstResp.Header.Set("x-codex-turn-state", "turn_state_digest_first")
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		firstResp,
		openAICompatSSECompletedResponse("resp_oauth_digest_second", "gpt-5.4"),
	}}
	svc := &OpenAIGatewayService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}

	firstBody := []byte(`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"first"}],"stream":false}`)
	firstRec := httptest.NewRecorder()
	firstCtx, _ := gin.CreateTestContext(firstRec)
	firstCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(firstBody))
	firstCtx.Request.Header.Set("Content-Type", "application/json")

	firstResult, err := svc.ForwardAsAnthropic(context.Background(), firstCtx, account, firstBody, "", "gpt-5.4")
	require.NoError(t, err)
	require.NotNil(t, firstResult)
	firstSessionID := upstream.requests[0].Header.Get("session_id")
	require.NotEmpty(t, firstSessionID)
	require.Empty(t, upstream.requests[0].Header.Get("x-codex-turn-state"))
	require.False(t, gjson.GetBytes(upstream.bodies[0], "prompt_cache_key").Exists())

	secondBody := []byte(`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"ok"},{"role":"user","content":"second"}],"stream":false}`)
	secondRec := httptest.NewRecorder()
	secondCtx, _ := gin.CreateTestContext(secondRec)
	secondCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(secondBody))
	secondCtx.Request.Header.Set("Content-Type", "application/json")

	secondResult, err := svc.ForwardAsAnthropic(context.Background(), secondCtx, account, secondBody, "", "gpt-5.4")
	require.NoError(t, err)
	require.NotNil(t, secondResult)
	require.Equal(t, firstSessionID, upstream.requests[1].Header.Get("session_id"))
	require.Equal(t, "turn_state_digest_first", upstream.requests[1].Header.Get("x-codex-turn-state"))
	require.Empty(t, upstream.requests[1].Header.Get("conversation_id"))
	require.False(t, gjson.GetBytes(upstream.bodies[1], "prompt_cache_key").Exists())
	require.False(t, gjson.GetBytes(upstream.bodies[1], "previous_response_id").Exists())
}

func TestForwardAsAnthropic_OAuthMetadataSessionSurvivesDigestPrefixRewrite(t *testing.T) {
	t.Parallel()

	firstResp := openAICompatSSECompletedResponse("resp_oauth_metadata_first", "gpt-5.5")
	firstResp.Header.Set("x-codex-turn-state", "turn_state_metadata_first")
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		firstResp,
		openAICompatSSECompletedResponse("resp_oauth_metadata_second", "gpt-5.5"),
	}}
	svc := &OpenAIGatewayService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}
	metadata := `{"user_id":"{\"device_id\":\"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\",\"account_uuid\":\"\",\"session_id\":\"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa\"}"}`

	firstBody := []byte(`{"model":"claude-sonnet-4-5","max_tokens":16,"metadata":` + metadata + `,"messages":[{"role":"user","content":"first plan"}],"stream":false}`)
	firstRec := httptest.NewRecorder()
	firstCtx, _ := gin.CreateTestContext(firstRec)
	firstCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(firstBody))
	firstCtx.Request.Header.Set("Content-Type", "application/json")

	firstResult, err := svc.ForwardAsAnthropic(context.Background(), firstCtx, account, firstBody, "", "gpt-5.5")
	require.NoError(t, err)
	require.NotNil(t, firstResult)
	firstSessionID := upstream.requests[0].Header.Get("session_id")
	require.NotEmpty(t, firstSessionID)
	require.Empty(t, upstream.requests[0].Header.Get("x-codex-turn-state"))
	require.False(t, gjson.GetBytes(upstream.bodies[0], "prompt_cache_key").Exists())

	secondBody := []byte(`{"model":"claude-sonnet-4-5","max_tokens":16,"metadata":` + metadata + `,"messages":[{"role":"user","content":"rewritten plan"},{"role":"assistant","content":"ok"},{"role":"user","content":"second"}],"stream":false}`)
	secondRec := httptest.NewRecorder()
	secondCtx, _ := gin.CreateTestContext(secondRec)
	secondCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(secondBody))
	secondCtx.Request.Header.Set("Content-Type", "application/json")

	secondResult, err := svc.ForwardAsAnthropic(context.Background(), secondCtx, account, secondBody, "", "gpt-5.5")
	require.NoError(t, err)
	require.NotNil(t, secondResult)
	require.Equal(t, firstSessionID, upstream.requests[1].Header.Get("session_id"))
	require.Equal(t, "turn_state_metadata_first", upstream.requests[1].Header.Get("x-codex-turn-state"))
	require.Empty(t, upstream.requests[1].Header.Get("conversation_id"))
	require.False(t, gjson.GetBytes(upstream.bodies[1], "prompt_cache_key").Exists())
	require.False(t, gjson.GetBytes(upstream.bodies[1], "previous_response_id").Exists())
}

func TestForwardAsAnthropic_OAuthMetadataSessionSurvivesChangingCacheControlAnchor(t *testing.T) {
	t.Parallel()

	firstResp := openAICompatSSECompletedResponse("resp_oauth_cache_anchor_first", "gpt-5.5")
	firstResp.Header.Set("x-codex-turn-state", "turn_state_cache_anchor_first")
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		firstResp,
		openAICompatSSECompletedResponse("resp_oauth_cache_anchor_second", "gpt-5.5"),
	}}
	svc := &OpenAIGatewayService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}
	metadata := `{"user_id":"{\"device_id\":\"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\",\"account_uuid\":\"\",\"session_id\":\"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb\"}"}`

	firstBody := []byte(`{"model":"claude-sonnet-4-5","max_tokens":16,"metadata":` + metadata + `,"system":[{"type":"text","text":"anchor one","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"first"}],"stream":false}`)
	firstRec := httptest.NewRecorder()
	firstCtx, _ := gin.CreateTestContext(firstRec)
	firstCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(firstBody))
	firstCtx.Request.Header.Set("Content-Type", "application/json")

	firstResult, err := svc.ForwardAsAnthropic(context.Background(), firstCtx, account, firstBody, "", "gpt-5.5")
	require.NoError(t, err)
	require.NotNil(t, firstResult)
	firstSessionID := upstream.requests[0].Header.Get("session_id")
	require.NotEmpty(t, firstSessionID)
	require.Empty(t, upstream.requests[0].Header.Get("x-codex-turn-state"))
	require.False(t, gjson.GetBytes(upstream.bodies[0], "prompt_cache_key").Exists())

	secondBody := []byte(`{"model":"claude-sonnet-4-5","max_tokens":16,"metadata":` + metadata + `,"system":[{"type":"text","text":"anchor two","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"ok"},{"role":"user","content":"second"}],"stream":false}`)
	secondRec := httptest.NewRecorder()
	secondCtx, _ := gin.CreateTestContext(secondRec)
	secondCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(secondBody))
	secondCtx.Request.Header.Set("Content-Type", "application/json")

	secondResult, err := svc.ForwardAsAnthropic(context.Background(), secondCtx, account, secondBody, "", "gpt-5.5")
	require.NoError(t, err)
	require.NotNil(t, secondResult)
	require.Equal(t, firstSessionID, upstream.requests[1].Header.Get("session_id"))
	require.Equal(t, "turn_state_cache_anchor_first", upstream.requests[1].Header.Get("x-codex-turn-state"))
	require.Empty(t, upstream.requests[1].Header.Get("conversation_id"))
	require.False(t, gjson.GetBytes(upstream.bodies[1], "prompt_cache_key").Exists())
	require.False(t, gjson.GetBytes(upstream.bodies[1], "previous_response_id").Exists())
}

func TestForwardAsAnthropic_OAuthKeepsSystemAsDeveloperInput(t *testing.T) {
	t.Parallel()

	upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_oauth_system", "gpt-5.4")}
	svc := &OpenAIGatewayService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}

	body := []byte(`{"model":"claude-sonnet-4-5","max_tokens":16,"system":[{"type":"text","text":"project instructions","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"first"}],"stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-5.4")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "developer", gjson.GetBytes(upstream.lastBody, "input.0.role").String())
	require.Equal(t, "input_text", gjson.GetBytes(upstream.lastBody, "input.0.content.0.type").String())
	require.Equal(t, "project instructions", gjson.GetBytes(upstream.lastBody, "input.0.content.0.text").String())
	instructions := gjson.GetBytes(upstream.lastBody, "instructions")
	require.True(t, instructions.Exists())
	require.Empty(t, instructions.String())
	require.Empty(t, upstream.requests[0].Header.Get("OpenAI-Beta"))
	require.Equal(t, codexOfficialOriginator, upstream.requests[0].Header.Get("originator"))
}

func TestForwardAsAnthropic_OAuthAddsClaudeCodeTodoGuardForCompatModel(t *testing.T) {
	t.Parallel()

	upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_oauth_todo_guard", "gpt-5.5")}
	svc := &OpenAIGatewayService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}

	body := []byte(`{"model":"claude-sonnet-4-5","max_tokens":16,"system":"project instructions","messages":[{"role":"user","content":"review files"}],"stream":false}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-5.5")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "developer", gjson.GetBytes(upstream.lastBody, "input.0.role").String())
	require.Equal(t, "project instructions", gjson.GetBytes(upstream.lastBody, "input.0.content.0.text").String())
	require.Equal(t, "developer", gjson.GetBytes(upstream.lastBody, "input.1.role").String())
	require.Contains(t, gjson.GetBytes(upstream.lastBody, "input.1.content.0.text").String(), "<sub2api-claude-code-todo-guard>")
	require.Equal(t, "user", gjson.GetBytes(upstream.lastBody, "input.2.role").String())
}

func TestForwardAsAnthropic_OAuthPreservesClaudeCodeToolCallID(t *testing.T) {
	t.Parallel()

	upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_oauth_tool", "gpt-5.4")}
	svc := &OpenAIGatewayService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}

	toolCallID := "toolu_01JZY8M4D7YAHG9N3Q5R6T2V1W" + strings.Repeat("A", 40)
	body := []byte(fmt.Sprintf(`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"list files"},{"role":"assistant","content":[{"type":"tool_use","id":%q,"name":"Bash","input":{"command":"ls"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"ok"}]}],"tools":[{"name":"Bash","description":"run shell","input_schema":{"type":"object","properties":{"command":{"type":"string"}}}}],"stream":false}`, toolCallID, toolCallID))
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "stable-cache-key", "gpt-5.4")
	require.NoError(t, err)
	require.NotNil(t, result)
	upstreamCallID := gjson.GetBytes(upstream.lastBody, `input.#(type=="function_call").call_id`).String()
	require.Equal(t, upstreamCallID, gjson.GetBytes(upstream.lastBody, `input.#(type=="function_call_output").call_id`).String())
	require.Equal(t, "fc_64930a78b4d188754f5bdcf020e9b5f0b434890cf7987680e959e92183c0f", upstreamCallID)
	require.Len(t, upstreamCallID, codexCallIDMaxLength)
	require.True(t, strings.HasPrefix(upstreamCallID, codexCallIDPrefix))
	require.NotEqual(t, toolCallID, upstreamCallID)
	require.True(t, gjson.GetBytes(upstream.lastBody, "parallel_tool_calls").Bool())
	require.Equal(t, "medium", gjson.GetBytes(upstream.lastBody, "text.verbosity").String())
	require.False(t, gjson.GetBytes(upstream.lastBody, "tools.0.strict").Bool())
}

func TestForwardAsAnthropic_StoresStreamingResponseIDWithoutUsage(t *testing.T) {
	t.Parallel()

	upstream := &httpUpstreamRecorder{}
	svc := &OpenAIGatewayService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	account := &Account{
		ID:          1,
		Name:        "openai-apikey",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://api.openai.com/v1",
		},
	}

	firstBody := []byte(`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"first"}],"stream":true}`)
	upstream.resp = openAICompatSSEResponseWithoutUsage("resp_stream_first", "gpt-5.3-codex")
	firstRec := httptest.NewRecorder()
	firstCtx, _ := gin.CreateTestContext(firstRec)
	firstCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(firstBody))
	firstCtx.Request.Header.Set("Content-Type", "application/json")

	firstResult, err := svc.ForwardAsAnthropic(context.Background(), firstCtx, account, firstBody, "stable-cache-key", "gpt-5.3-codex")
	require.NoError(t, err)
	require.NotNil(t, firstResult)
	require.Equal(t, "resp_stream_first", firstResult.ResponseID)

	secondBody := []byte(`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"ok"},{"role":"user","content":"second"}],"stream":false}`)
	upstream.resp = openAICompatSSECompletedResponse("resp_stream_second", "gpt-5.3-codex")
	secondRec := httptest.NewRecorder()
	secondCtx, _ := gin.CreateTestContext(secondRec)
	secondCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(secondBody))
	secondCtx.Request.Header.Set("Content-Type", "application/json")

	secondResult, err := svc.ForwardAsAnthropic(context.Background(), secondCtx, account, secondBody, "stable-cache-key", "gpt-5.3-codex")
	require.NoError(t, err)
	require.NotNil(t, secondResult)
	require.Equal(t, "resp_stream_first", gjson.GetBytes(upstream.lastBody, "previous_response_id").String())
}

func openAICompatSSECompletedResponse(responseID, model string) *http.Response {
	body := strings.Join([]string{
		`data: {"type":"response.completed","response":{"id":"` + responseID + `","object":"response","model":"` + model + `","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_continuation"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func openAICompatSSEResponseWithoutUsage(responseID, model string) *http.Response {
	body := strings.Join([]string{
		`data: {"type":"response.completed","response":{"id":"` + responseID + `","object":"response","model":"` + model + `","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}]}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_" + responseID}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestForwardAsAnthropic_ForcedCodexInstructionsTemplatePrependsRenderedInstructions(t *testing.T) {
	t.Parallel()

	templateDir := t.TempDir()
	templatePath := filepath.Join(templateDir, "codex-instructions.md.tmpl")
	require.NoError(t, os.WriteFile(templatePath, []byte("server-prefix\n\n{{ .ExistingInstructions }}"), 0o644))

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"system":"client-system","messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := strings.Join([]string{
		`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","model":"gpt-5.4","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_forced"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}

	svc := &OpenAIGatewayService{
		cfg: &config.Config{Gateway: config.GatewayConfig{
			ForcedCodexInstructionsTemplateFile: templatePath,
			ForcedCodexInstructionsTemplate:     "server-prefix\n\n{{ .ExistingInstructions }}",
		}},
		httpUpstream: upstream,
	}
	account := &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-5.1")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "server-prefix\n\nclient-system", gjson.GetBytes(upstream.lastBody, "instructions").String())
}

func TestForwardAsAnthropic_ForcedCodexInstructionsTemplateUsesCachedTemplateContent(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"system":"client-system","messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := strings.Join([]string{
		`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","model":"gpt-5.4","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_forced_cached"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}

	svc := &OpenAIGatewayService{
		cfg: &config.Config{Gateway: config.GatewayConfig{
			ForcedCodexInstructionsTemplateFile: "/path/that/should/not/be/read.tmpl",
			ForcedCodexInstructionsTemplate:     "cached-prefix\n\n{{ .ExistingInstructions }}",
		}},
		httpUpstream: upstream,
	}
	account := &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-5.1")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "cached-prefix\n\nclient-system", gjson.GetBytes(upstream.lastBody, "instructions").String())
}

func TestForwardAsAnthropic_ClientDisconnectDrainsUpstreamUsage(t *testing.T) {

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Writer = &openAICompatFailingWriter{ResponseWriter: c.Writer, failAfter: 0}
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.4","status":"in_progress","output":[]}}`,
		"",
		`data: {"type":"response.output_text.delta","delta":"ok"}`,
		"",
		`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","model":"gpt-5.4","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":9,"output_tokens":4,"total_tokens":13,"input_tokens_details":{"cached_tokens":3}}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_disconnect"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}

	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-5.1")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 9, result.Usage.InputTokens)
	require.Equal(t, 4, result.Usage.OutputTokens)
	require.Equal(t, 3, result.Usage.CacheReadInputTokens)
}

func TestForwardAsAnthropic_TerminalUsageWithoutUpstreamCloseReturns(t *testing.T) {

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Writer = &openAICompatFailingWriter{ResponseWriter: c.Writer, failAfter: 0}
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := []byte(`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","model":"gpt-5.4","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":15,"output_tokens":6,"total_tokens":21,"input_tokens_details":{"cached_tokens":5}}}}` + "\n\n")
	upstreamStream := newOpenAICompatBlockingReadCloser(upstreamBody)
	defer func() {
		require.NoError(t, upstreamStream.Close())
	}()
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_terminal_no_close"}},
		Body:       upstreamStream,
	}}

	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}

	type forwardResult struct {
		result *OpenAIForwardResult
		err    error
	}
	resultCh := make(chan forwardResult, 1)
	go func() {
		result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-5.1")
		resultCh <- forwardResult{result: result, err: err}
	}()

	select {
	case got := <-resultCh:
		require.NoError(t, got.err)
		require.NotNil(t, got.result)
		require.Equal(t, 15, got.result.Usage.InputTokens)
		require.Equal(t, 6, got.result.Usage.OutputTokens)
		require.Equal(t, 5, got.result.Usage.CacheReadInputTokens)
	case <-time.After(time.Second):
		require.Fail(t, "ForwardAsAnthropic should return after terminal usage event even if upstream keeps the connection open")
	}
}

func TestForwardAsAnthropic_EventNamedTerminalWithoutUpstreamCloseReturns(t *testing.T) {

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Writer = &openAICompatFailingWriter{ResponseWriter: c.Writer, failAfter: 0}
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := []byte(strings.Join([]string{
		`event: response.completed`,
		`data: {"response":{"id":"resp_1","object":"response","model":"gpt-5.4","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":15,"output_tokens":6,"total_tokens":21,"input_tokens_details":{"cached_tokens":5}}}}`,
		``,
		``,
	}, "\n"))
	upstreamStream := newOpenAICompatBlockingReadCloser(upstreamBody)
	defer func() {
		require.NoError(t, upstreamStream.Close())
	}()
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_messages_event_named_terminal"}},
		Body:       upstreamStream,
	}}

	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}

	type forwardResult struct {
		result *OpenAIForwardResult
		err    error
	}
	resultCh := make(chan forwardResult, 1)
	go func() {
		result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-5.1")
		resultCh <- forwardResult{result: result, err: err}
	}()

	select {
	case got := <-resultCh:
		require.NoError(t, got.err)
		require.NotNil(t, got.result)
		require.Equal(t, 15, got.result.Usage.InputTokens)
		require.Equal(t, 6, got.result.Usage.OutputTokens)
		require.Equal(t, 5, got.result.Usage.CacheReadInputTokens)
	case <-time.After(time.Second):
		require.Fail(t, "ForwardAsAnthropic should use SSE event names when data payloads omit type")
	}
}

func TestForwardAsAnthropic_EventNamedTerminalWithKeepaliveReturns(t *testing.T) {

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Writer = &openAICompatFailingWriter{ResponseWriter: c.Writer, failAfter: 0}
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := []byte(strings.Join([]string{
		`: upstream ping`,
		``,
		`event: response.completed`,
		`data: {"response":{"id":"resp_1","object":"response","model":"gpt-5.4","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":15,"output_tokens":6,"total_tokens":21,"input_tokens_details":{"cached_tokens":5}}}}`,
		``,
		``,
	}, "\n"))
	upstreamStream := newOpenAICompatBlockingReadCloser(upstreamBody)
	defer func() {
		require.NoError(t, upstreamStream.Close())
	}()
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_messages_event_named_keepalive"}},
		Body:       upstreamStream,
	}}

	svc := &OpenAIGatewayService{
		cfg: &config.Config{Gateway: config.GatewayConfig{
			StreamKeepaliveInterval: 5,
		}},
		httpUpstream: upstream,
	}
	account := &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}

	type forwardResult struct {
		result *OpenAIForwardResult
		err    error
	}
	resultCh := make(chan forwardResult, 1)
	go func() {
		result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-5.1")
		resultCh <- forwardResult{result: result, err: err}
	}()

	select {
	case got := <-resultCh:
		require.NoError(t, got.err)
		require.NotNil(t, got.result)
		require.Equal(t, 15, got.result.Usage.InputTokens)
		require.Equal(t, 6, got.result.Usage.OutputTokens)
		require.Equal(t, 5, got.result.Usage.CacheReadInputTokens)
	case <-time.After(time.Second):
		require.Fail(t, "ForwardAsAnthropic keepalive path should use SSE event names when data payloads omit type")
	}
}

func TestForwardAsAnthropic_BufferedTerminalWithoutUpstreamCloseReturns(t *testing.T) {

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := []byte(`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","model":"gpt-5.4","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":15,"output_tokens":6,"total_tokens":21,"input_tokens_details":{"cached_tokens":5}}}}` + "\n\n")
	upstreamStream := newOpenAICompatBlockingReadCloser(upstreamBody)
	defer func() {
		require.NoError(t, upstreamStream.Close())
	}()
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_buffered_terminal_no_close"}},
		Body:       upstreamStream,
	}}

	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}

	type forwardResult struct {
		result *OpenAIForwardResult
		err    error
	}
	resultCh := make(chan forwardResult, 1)
	go func() {
		result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-5.1")
		resultCh <- forwardResult{result: result, err: err}
	}()

	select {
	case got := <-resultCh:
		require.NoError(t, got.err)
		require.NotNil(t, got.result)
		require.Equal(t, 15, got.result.Usage.InputTokens)
		require.Equal(t, 6, got.result.Usage.OutputTokens)
		require.Equal(t, 5, got.result.Usage.CacheReadInputTokens)
		require.Contains(t, rec.Body.String(), `"stop_reason":"end_turn"`)
	case <-time.After(time.Second):
		require.Fail(t, "ForwardAsAnthropic buffered response should return after terminal usage event even if upstream keeps the connection open")
	}
}

func TestForwardAsAnthropic_BufferedEventNamedTerminalWithoutUpstreamCloseReturns(t *testing.T) {

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := []byte(strings.Join([]string{
		`event: response.completed`,
		`data: {"response":{"id":"resp_1","object":"response","model":"gpt-5.4","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":15,"output_tokens":6,"total_tokens":21,"input_tokens_details":{"cached_tokens":5}}}}`,
		``,
		``,
	}, "\n"))
	upstreamStream := newOpenAICompatBlockingReadCloser(upstreamBody)
	defer func() {
		require.NoError(t, upstreamStream.Close())
	}()
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_messages_buffered_event_named"}},
		Body:       upstreamStream,
	}}

	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}

	type forwardResult struct {
		result *OpenAIForwardResult
		err    error
	}
	resultCh := make(chan forwardResult, 1)
	go func() {
		result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-5.1")
		resultCh <- forwardResult{result: result, err: err}
	}()

	select {
	case got := <-resultCh:
		require.NoError(t, got.err)
		require.NotNil(t, got.result)
		require.Equal(t, 15, got.result.Usage.InputTokens)
		require.Equal(t, 6, got.result.Usage.OutputTokens)
		require.Equal(t, 5, got.result.Usage.CacheReadInputTokens)
		require.Contains(t, rec.Body.String(), `"stop_reason":"end_turn"`)
	case <-time.After(time.Second):
		require.Fail(t, "ForwardAsAnthropic buffered response should use SSE event names when data payloads omit type")
	}
}

func TestForwardAsAnthropic_MissingTerminalBeforeOutputReturnsFailoverAndOps(t *testing.T) {

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := "data: [DONE]\n\n"
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "X-Request-Id": []string{"rid_missing_terminal"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}

	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-5.1")
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.True(t, errors.As(err, &failoverErr), "missing terminal before output must use failover path")
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.Contains(t, string(failoverErr.ResponseBody), "OpenAI messages stream ended before a terminal event")
	require.NotNil(t, result)
	require.Zero(t, result.Usage.InputTokens)
	require.Zero(t, result.Usage.OutputTokens)
	require.False(t, c.Writer.Written(), "no client body/header should be committed before safe failover")
	require.Empty(t, rec.Body.String())

	events := openAICompatOpsEvents(t, c)
	require.Len(t, events, 1)
	require.Equal(t, "failover", events[0].Kind)
	require.Equal(t, http.StatusBadGateway, events[0].UpstreamStatusCode)
	require.Equal(t, int64(1), events[0].AccountID)
	require.Equal(t, "rid_missing_terminal", events[0].UpstreamRequestID)
	require.Contains(t, events[0].Message, "terminal event")
}

func TestForwardAsAnthropic_MissingTerminalAfterOutputRecordsOpsWithoutFailover(t *testing.T) {

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.4","status":"in_progress","output":[]}}`,
		"",
		`data: {"type":"response.output_text.delta","delta":"partial"}`,
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "X-Request-Id": []string{"rid_partial_missing_terminal"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}

	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-5.1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing terminal event")
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "partial output must not be replayed through failover")
	require.NotNil(t, result)
	require.False(t, result.ClientDisconnect)
	require.True(t, c.Writer.Written())
	require.Contains(t, rec.Body.String(), "event: message_start")
	require.Contains(t, rec.Body.String(), "partial")

	events := openAICompatOpsEvents(t, c)
	require.Len(t, events, 1)
	require.Equal(t, "stream_missing_terminal", events[0].Kind)
	require.Equal(t, http.StatusBadGateway, events[0].UpstreamStatusCode)
	require.Equal(t, int64(1), events[0].AccountID)
	require.Equal(t, "rid_partial_missing_terminal", events[0].UpstreamRequestID)
	require.Contains(t, events[0].Message, "terminal event")
	require.Equal(t, "https://chatgpt.com/backend-api/codex/responses", events[0].UpstreamURL)
	require.Equal(t, "/v1/responses", events[0].UpstreamEndpoint)
}

func TestForwardAsAnthropic_MissingTerminalAfterClientDisconnectSkipsOpsAndFailover(t *testing.T) {

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Writer = &openAICompatFailingWriter{ResponseWriter: c.Writer, failAfter: 0}
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.4","status":"in_progress","output":[]}}`,
		"",
		`data: {"type":"response.output_text.delta","delta":"ok"}`,
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "X-Request-Id": []string{"rid_client_disconnect_missing_terminal"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}

	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-5.1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing terminal event")
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	require.NotNil(t, result)
	require.True(t, result.ClientDisconnect)
	require.Empty(t, rec.Body.String())
	_, ok := c.Get(OpsUpstreamErrorsKey)
	require.False(t, ok, "client disconnect must not be attributed as an upstream error")
}

func TestForwardAsAnthropic_CompleteStreamDoesNotRecordMissingTerminalOps(t *testing.T) {

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	upstreamBody := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.4","status":"in_progress","output":[]}}`,
		"",
		`data: {"type":"response.output_text.delta","delta":"ok"}`,
		"",
		`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","model":"gpt-5.4","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":9,"output_tokens":4,"total_tokens":13}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "X-Request-Id": []string{"rid_complete_terminal"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}

	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}

	result, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-5.1")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 9, result.Usage.InputTokens)
	require.Equal(t, 4, result.Usage.OutputTokens)
	require.Contains(t, rec.Body.String(), "event: message_stop")
	_, ok := c.Get(OpsUpstreamErrorsKey)
	require.False(t, ok)
}

func openAICompatOpsEvents(t *testing.T, c *gin.Context) []*OpsUpstreamErrorEvent {
	t.Helper()
	v, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok)
	events, ok := v.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	return events
}

func TestForwardAsAnthropic_RejectsClientCancelBeforeAdmission(t *testing.T) {

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	reqCtx, cancel := context.WithCancel(context.Background())
	body := []byte(`{"model":"gpt-5.4","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":false}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body)).WithContext(reqCtx)
	c.Request.Header.Set("Content-Type", "application/json")
	cancel()

	upstreamBody := strings.Join([]string{
		`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","model":"gpt-5.4","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_ctx"}},
		Body:       io.NopCloser(strings.NewReader(upstreamBody)),
	}}

	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := &Account{
		ID:          1,
		Name:        "openai-oauth",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":       "oauth-token",
			"chatgpt_account_id": "chatgpt-acc",
		},
	}

	result, err := svc.ForwardAsAnthropic(reqCtx, c, account, body, "", "gpt-5.1")
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, result)
	require.Nil(t, upstream.lastReq)
}
