package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIGatewayService_Forward_WSv2_RecognizedOverloadBeforeOutputReturnsDirectRequestError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
	c.Request.Header.Set("User-Agent", "unit-test-agent/1.0")

	cfg := newOpenAIWSV2TestConfig()
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	captureConn := &openAIWSCaptureConn{events: [][]byte{
		[]byte(`{"type":"response.failed","response":{"id":"resp_overloaded","status":"failed","error":{"code":"server_is_overloaded","type":"response.failed","message":"temporary overload","metadata":{"authorization":"Bearer secret-token"}},"usage":{"input_tokens":7}}}`),
	}}
	captureDialer := &openAIWSCaptureDialer{conn: captureConn}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(captureDialer)

	result, err := newOpenAIWSRecognizedErrorService(cfg, pool).Forward(
		context.Background(), c, newOpenAIWSRecognizedErrorAccount(701),
		[]byte(`{"model":"gpt-5.1","stream":true,"input":[{"type":"input_text","text":"hello"}]}`),
	)

	requestErr := requireOpenAIWSRecognizedRequestError(t, err, http.StatusServiceUnavailable, "server_is_overloaded")
	require.Equal(t, "service_unavailable_error", requestErr.Type)
	require.False(t, requestErr.OutputStarted)
	require.NotNil(t, result)
	require.NotNil(t, requestErr.Usage)
	require.Equal(t, 7, requestErr.Usage.InputTokens)
	require.Empty(t, recorder.Body.String(), "pre-output direct errors must not commit SSE")
	require.NotContains(t, recorder.Body.String(), "secret-token")
}

func TestOpenAIHasContextWindowCandidate(t *testing.T) {
	require.False(t, openAIHasContextWindowCandidate([]byte(`{"type":"response.failed","response":{"error":{"code":"server_is_overloaded"}}}`)))
	require.True(t, openAIHasContextWindowCandidate([]byte(`{"type":"response.failed","response":{"error":{"code":"server_is_overloaded"}},"error":{"code":"context_length_exceeded"}}`)))
}

func TestOpenAIGatewayService_Forward_WSv2_ConflictingContextCandidateDoesNotBroadenToOverload(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
	c.Request.Header.Set("User-Agent", "unit-test-agent/1.0")

	cfg := newOpenAIWSV2TestConfig()
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	captureConn := &openAIWSCaptureConn{events: [][]byte{
		[]byte(`{"type":"response.failed","response":{"id":"resp_conflict","status":"failed","error":{"code":"server_is_overloaded","message":"temporary overload"}},"error":{"code":"context_length_exceeded","message":"context window"}}`),
	}}
	captureDialer := &openAIWSCaptureDialer{conn: captureConn}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(captureDialer)

	_, err := newOpenAIWSRecognizedErrorService(cfg, pool).Forward(
		context.Background(), c, newOpenAIWSRecognizedErrorAccount(705),
		[]byte(`{"model":"gpt-5.1","stream":true,"input":[{"type":"input_text","text":"hello"}]}`),
	)

	var requestErr *OpenAIUpstreamRequestError
	require.NoError(t, err, "the conflict remains unknown compatibility behavior")
	require.False(t, errors.As(err, &requestErr), "the conflict must not become a recognized overload or context direct return")
}

func TestOpenAIGatewayService_Forward_WSv2_RecognizedOverloadErrorEventBeforeOutputReturnsSafeDirectRequestError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
	c.Request.Header.Set("User-Agent", "unit-test-agent/1.0")

	cfg := newOpenAIWSV2TestConfig()
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	captureConn := &openAIWSCaptureConn{events: [][]byte{
		[]byte(`{"type":"error","id":"evt_overloaded","error":{"code":"server_is_overloaded","type":"response.failed","message":"temporary overload","metadata":{"authorization":"Bearer secret-token"}}}`),
	}}
	captureDialer := &openAIWSCaptureDialer{conn: captureConn}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(captureDialer)

	result, err := newOpenAIWSRecognizedErrorService(cfg, pool).Forward(
		context.Background(), c, newOpenAIWSRecognizedErrorAccount(703),
		[]byte(`{"model":"gpt-5.1","stream":true,"input":[{"type":"input_text","text":"hello"}]}`),
	)

	requestErr := requireOpenAIWSRecognizedRequestError(t, err, http.StatusServiceUnavailable, "server_is_overloaded")
	require.Equal(t, "service_unavailable_error", requestErr.Type)
	require.False(t, requestErr.OutputStarted)
	require.NotNil(t, result)
	require.Empty(t, recorder.Body.String(), "pre-output direct errors must not commit SSE")
	require.NotContains(t, recorder.Body.String(), "secret-token")
	require.Equal(t, "", requestErr.RequestID, "an error event ID is not a response ID")
}

func TestOpenAIGatewayService_Forward_WSv2_RecognizedOverloadErrorEventAfterCreatedRendersSafeTerminal(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
	c.Request.Header.Set("User-Agent", "unit-test-agent/1.0")

	cfg := newOpenAIWSV2TestConfig()
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	captureConn := &openAIWSCaptureConn{events: [][]byte{
		[]byte(`{"type":"response.created","response":{"id":"resp_overloaded_event","model":"gpt-5.1"}}`),
		[]byte(`{"type":"error","id":"evt_overloaded","error":{"code":"server_is_overloaded","type":"response.failed","message":"temporary overload","metadata":{"authorization":"Bearer secret-token"}}}`),
	}}
	captureDialer := &openAIWSCaptureDialer{conn: captureConn}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(captureDialer)

	result, err := newOpenAIWSRecognizedErrorService(cfg, pool).Forward(
		context.Background(), c, newOpenAIWSRecognizedErrorAccount(704),
		[]byte(`{"model":"gpt-5.1","stream":true,"input":[{"type":"input_text","text":"hello"}]}`),
	)

	requestErr := requireOpenAIWSRecognizedRequestError(t, err, http.StatusServiceUnavailable, "server_is_overloaded")
	require.True(t, requestErr.OutputStarted)
	require.NotNil(t, result)
	body := recorder.Body.String()
	require.Contains(t, body, `"type":"response.created"`)
	require.Equal(t, 1, strings.Count(body, `"type":"error"`), "body=%s", body)
	require.Contains(t, body, `"type":"service_unavailable_error"`)
	require.NotContains(t, body, "secret-token")
}

func TestOpenAIGatewayService_Forward_WSv2_RecognizedOverloadAfterCreatedUsesDeliveryLifecycle(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
	c.Request.Header.Set("User-Agent", "unit-test-agent/1.0")

	cfg := newOpenAIWSV2TestConfig()
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	captureConn := &openAIWSCaptureConn{events: [][]byte{
		[]byte(`{"type":"response.created","response":{"id":"resp_overloaded_created","model":"gpt-5.1"}}`),
		[]byte(`{"type":"response.failed","response":{"id":"resp_overloaded_created","status":"failed","error":{"code":"server_is_overloaded","type":"response.failed","message":"temporary overload","metadata":{"authorization":"Bearer secret-token"}},"usage":{"input_tokens":7}}}`),
	}}
	captureDialer := &openAIWSCaptureDialer{conn: captureConn}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(captureDialer)

	result, err := newOpenAIWSRecognizedErrorService(cfg, pool).Forward(
		context.Background(), c, newOpenAIWSRecognizedErrorAccount(702),
		[]byte(`{"model":"gpt-5.1","stream":true,"input":[{"type":"input_text","text":"hello"}]}`),
	)

	requestErr := requireOpenAIWSRecognizedRequestError(t, err, http.StatusServiceUnavailable, "server_is_overloaded")
	require.True(t, requestErr.OutputStarted, "response.created was delivered even though no token delta was emitted")
	require.Nil(t, result.FirstTokenMs, "TTFT is not an output-start proxy")
	body := recorder.Body.String()
	require.Equal(t, 1, strings.Count(body, `"type":"response.failed"`), "body=%s", body)
	require.Contains(t, body, `"type":"response.created"`)
	require.Contains(t, body, `"type":"response.failed"`)
	require.NotContains(t, body, "secret-token")
}

func newOpenAIWSRecognizedErrorService(cfg *config.Config, pool *openAIWSConnPool) *OpenAIGatewayService {
	return &OpenAIGatewayService{
		cfg:              cfg,
		httpUpstream:     &httpUpstreamRecorder{},
		cache:            &stubGatewayCache{},
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg),
		toolCorrector:    NewCodexToolCorrector(),
		openaiWSPool:     pool,
	}
}

func newOpenAIWSRecognizedErrorAccount(id int64) *Account {
	return &Account{
		ID: id, Name: "openai-ws-recognized", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: 1,
		Credentials: map[string]any{"api_key": "sk-test"},
		Extra:       map[string]any{"responses_websockets_v2_enabled": true},
	}
}

func requireOpenAIWSRecognizedRequestError(t *testing.T, err error, status int, code string) *OpenAIUpstreamRequestError {
	t.Helper()
	var requestErr *OpenAIUpstreamRequestError
	require.True(t, errors.As(err, &requestErr), "err=%v", err)
	require.NotNil(t, requestErr)
	require.Equal(t, status, requestErr.StatusCode)
	require.Equal(t, code, requestErr.Code)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	return requestErr
}
