package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

var handlerStructuredLogCaptureMu sync.Mutex

type handlerInMemoryLogSink struct {
	mu     sync.Mutex
	events []*logger.LogEvent
}

func (s *handlerInMemoryLogSink) WriteLogEvent(event *logger.LogEvent) {
	if event == nil {
		return
	}
	cloned := *event
	if event.Fields != nil {
		cloned.Fields = make(map[string]any, len(event.Fields))
		for k, v := range event.Fields {
			cloned.Fields[k] = v
		}
	}
	s.mu.Lock()
	s.events = append(s.events, &cloned)
	s.mu.Unlock()
}

func (s *handlerInMemoryLogSink) ContainsMessageAtLevel(substr, level string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	wantLevel := strings.ToLower(strings.TrimSpace(level))
	for _, ev := range s.events {
		if ev == nil {
			continue
		}
		if strings.Contains(ev.Message, substr) && strings.ToLower(strings.TrimSpace(ev.Level)) == wantLevel {
			return true
		}
	}
	return false
}

func (s *handlerInMemoryLogSink) EventWithMessage(substr string) *logger.LogEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ev := range s.events {
		if ev != nil && strings.Contains(ev.Message, substr) {
			cloned := *ev
			cloned.Fields = make(map[string]any, len(ev.Fields))
			for k, v := range ev.Fields {
				cloned.Fields[k] = v
			}
			return &cloned
		}
	}
	return nil
}

func requireLogField(t *testing.T, event *logger.LogEvent, field string, expected any) {
	t.Helper()
	require.NotNil(t, event)
	actual, ok := event.Fields[field]
	require.Truef(t, ok, "expected log field %q", field)
	require.Equal(t, expected, actual)
}

func captureHandlerStructuredLog(t *testing.T) (*handlerInMemoryLogSink, func()) {
	t.Helper()
	handlerStructuredLogCaptureMu.Lock()

	err := logger.Init(logger.InitOptions{
		Level:       "debug",
		Format:      "json",
		ServiceName: "sub2api",
		Environment: "test",
		Output: logger.OutputOptions{
			ToStdout: true,
			ToFile:   false,
		},
		Sampling: logger.SamplingOptions{Enabled: false},
	})
	require.NoError(t, err)

	sink := &handlerInMemoryLogSink{}
	logger.SetSink(sink)
	return sink, func() {
		logger.SetSink(nil)
		handlerStructuredLogCaptureMu.Unlock()
	}
}

func TestIsOpenAIRemoteCompactPath(t *testing.T) {
	require.False(t, isOpenAIRemoteCompactPath(nil))

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", nil)
	require.True(t, isOpenAIRemoteCompactPath(c))

	c.Request = httptest.NewRequest(http.MethodPost, "/responses/compact/", nil)
	require.True(t, isOpenAIRemoteCompactPath(c))

	c.Request = httptest.NewRequest(http.MethodPost, "/responses/compact%20", nil)
	require.False(t, isOpenAIRemoteCompactPath(c))

	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	require.False(t, isOpenAIRemoteCompactPath(c))
}

func TestLogOpenAIRemoteCompactOutcome_LegacySucceeded(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logSink, restore := captureHandlerStructuredLog(t)
	defer restore()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", nil)
	c.Request.Header.Set("User-Agent", "codex_cli_rs/0.125.0")
	c.Set(opsModelKey, "gpt-5.3-codex")
	c.Set(opsAccountIDKey, int64(123))
	c.Header("x-request-id", "rid-compact-ok")
	c.Status(http.StatusOK)

	h := &OpenAIGatewayHandler{cfg: &config.Config{Gateway: config.GatewayConfig{ForceCodexCLI: true}}}
	h.logOpenAIRemoteCompactOutcome(c, time.Now().Add(-8*time.Millisecond))

	event := logSink.EventWithMessage("codex.remote_compact.succeeded")
	require.NotNil(t, event)
	require.Equal(t, "info", strings.ToLower(strings.TrimSpace(event.Level)))
	requireLogField(t, event, "compact_outcome", "succeeded")
	requireLogField(t, event, "status_code", int64(http.StatusOK))
	requireLogField(t, event, "path", "/v1/responses/compact")
	requireLogField(t, event, "compact_operation", "remote_compaction_legacy")
	requireLogField(t, event, "compact_protocol", "legacy")
	_, hasSignal := event.Fields["compact_signal"]
	require.False(t, hasSignal)
	requireLogField(t, event, "request_model", "gpt-5.3-codex")
	requireLogField(t, event, "account_id", int64(123))
	requireLogField(t, event, "upstream_request_id", "rid-compact-ok")
	requireLogField(t, event, "force_codex_cli", true)
	requireLogField(t, event, "request_user_agent", "codex_cli_rs/0.125.0")
	require.Contains(t, event.Fields, "latency_ms")
}

func TestLogOpenAIRemoteCompactOutcome_V2SucceededOnResponsesEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logSink, restore := captureHandlerStructuredLog(t)
	defer restore()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.5","input":[{"type":"compaction_trigger"}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	c.Request.Header.Set("User-Agent", "codex_cli_rs/0.136.0")
	require.True(t, service.PromoteOpenAICompactBodySignal(c, body, false))
	service.SetOpenAIRemoteCompactionSemanticOutcome(c, service.OpenAIRemoteCompactionSemanticOutcomeSucceeded)
	c.Set(opsModelKey, "gpt-5.5")
	c.Set(opsAccountIDKey, int64(456))
	c.Header("x-request-id", "rid-v2-ok")
	c.Status(http.StatusOK)

	(&OpenAIGatewayHandler{}).logOpenAIRemoteCompactOutcome(c, time.Now().Add(-5*time.Millisecond))

	event := logSink.EventWithMessage("codex.remote_compact.succeeded")
	require.NotNil(t, event)
	requireLogField(t, event, "path", "/v1/responses")
	requireLogField(t, event, "compact_operation", "remote_compaction_v2")
	requireLogField(t, event, "compact_protocol", "v2")
	requireLogField(t, event, "compact_signal", "compaction_trigger")
	requireLogField(t, event, "status_code", int64(http.StatusOK))
	requireLogField(t, event, "request_model", "gpt-5.5")
	requireLogField(t, event, "account_id", int64(456))
	requireLogField(t, event, "upstream_request_id", "rid-v2-ok")
	requireLogField(t, event, "force_codex_cli", false)
	requireLogField(t, event, "request_user_agent", "codex_cli_rs/0.136.0")
	require.Contains(t, event.Fields, "latency_ms")
}

func TestLogOpenAIRemoteCompactOutcome_V2WithoutSemanticOutcomeFailsDespiteHTTP200(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logSink, restore := captureHandlerStructuredLog(t)
	defer restore()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.5","input":[{"type":"compaction_trigger"}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	c.Request.Header.Set("User-Agent", "codex_cli_rs/0.136.0")
	require.True(t, service.PromoteOpenAICompactBodySignal(c, body, false))
	c.Status(http.StatusOK)

	(&OpenAIGatewayHandler{}).logOpenAIRemoteCompactOutcome(c, time.Now())

	event := logSink.EventWithMessage("codex.remote_compact.failed")
	require.NotNil(t, event)
	requireLogField(t, event, "compact_outcome", "failed")
	requireLogField(t, event, "status_code", int64(http.StatusOK))
	requireLogField(t, event, "compact_protocol", "v2")
}

func TestLogOpenAIRemoteCompactOutcome_Failed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logSink, restore := captureHandlerStructuredLog(t)
	defer restore()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/responses/compact", nil)
	c.Request.Header.Set("User-Agent", "codex_cli_rs/0.125.0")
	c.Status(http.StatusBadGateway)

	h := &OpenAIGatewayHandler{}
	h.logOpenAIRemoteCompactOutcome(c, time.Now())

	event := logSink.EventWithMessage("codex.remote_compact.failed")
	require.NotNil(t, event)
	require.Equal(t, "warn", strings.ToLower(strings.TrimSpace(event.Level)))
	requireLogField(t, event, "compact_outcome", "failed")
	requireLogField(t, event, "status_code", int64(http.StatusBadGateway))
	requireLogField(t, event, "path", "/responses/compact")
	requireLogField(t, event, "compact_operation", "remote_compaction_legacy")
	requireLogField(t, event, "compact_protocol", "legacy")
	_, hasSignal := event.Fields["compact_signal"]
	require.False(t, hasSignal)
}

func TestLogOpenAIRemoteCompactOutcome_V2FailedOnResponsesEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logSink, restore := captureHandlerStructuredLog(t)
	defer restore()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.5","input":[{"type":"compaction_trigger"}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(string(body)))
	c.Request.Header.Set("User-Agent", "codex_cli_rs/0.136.0")
	require.True(t, service.PromoteOpenAICompactBodySignal(c, body, false))
	c.Status(http.StatusBadGateway)

	(&OpenAIGatewayHandler{}).logOpenAIRemoteCompactOutcome(c, time.Now())

	event := logSink.EventWithMessage("codex.remote_compact.failed")
	require.NotNil(t, event)
	require.Equal(t, "warn", strings.ToLower(strings.TrimSpace(event.Level)))
	requireLogField(t, event, "path", "/responses")
	requireLogField(t, event, "compact_operation", "remote_compaction_v2")
	requireLogField(t, event, "compact_protocol", "v2")
	requireLogField(t, event, "compact_signal", "compaction_trigger")
}

func TestLogOpenAIRemoteCompactOutcome_NonCompactSkips(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logSink, restore := captureHandlerStructuredLog(t)
	defer restore()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Status(http.StatusOK)

	h := &OpenAIGatewayHandler{}
	h.logOpenAIRemoteCompactOutcome(c, time.Now())

	require.False(t, logSink.ContainsMessageAtLevel("codex.remote_compact.succeeded", "info"))
	require.False(t, logSink.ContainsMessageAtLevel("codex.remote_compact.failed", "warn"))
}

func TestOpenAIResponses_CompactUnauthorizedLogsFailed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logSink, restore := captureHandlerStructuredLog(t)
	defer restore()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(`{"model":"gpt-5.3-codex"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("User-Agent", "codex_cli_rs/0.125.0")

	h := &OpenAIGatewayHandler{}
	h.Responses(c)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
	event := logSink.EventWithMessage("codex.remote_compact.failed")
	require.NotNil(t, event)
	require.Equal(t, "warn", strings.ToLower(strings.TrimSpace(event.Level)))
	requireLogField(t, event, "status_code", int64(http.StatusUnauthorized))
	requireLogField(t, event, "path", "/v1/responses/compact")
}
