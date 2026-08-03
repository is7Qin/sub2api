package handler

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type recognizedAnthropicSSEUpstream struct{}

func (recognizedAnthropicSSEUpstream) Do(*http.Request, string, int64, int) (*http.Response, error) {
	body := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-3-5-sonnet-latest","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":0}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`,
		"",
		"event: error",
		`data: {"type":"error","error":{"type":"invalid_request_error","message":"max_tokens must be positive"}}`,
		"",
		"",
	}, "\n")
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid-recognized-anthropic"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}, nil
}

func (u recognizedAnthropicSSEUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

func TestGatewayHandlerMessages_RecognizedAnthropicErrorAfterOutputEmitsOneNamedErrorEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(9211)
	group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	account := &service.Account{
		ID:          9212,
		Name:        "recognized-anthropic-sse",
		Platform:    service.PlatformAnthropic,
		Type:        service.AccountTypeSetupToken,
		Concurrency: 1,
		Priority:    1,
		Status:      service.StatusActive,
		Schedulable: true,
		Credentials: map[string]any{
			"access_token": "test-token",
		},
		AccountGroups: []service.AccountGroup{{AccountID: 9212, GroupID: groupID}},
	}

	concurrencyCache := &retryCancellationConcurrencyCache{}
	concurrencyService := service.NewConcurrencyService(concurrencyCache)
	schedulerSnapshot := service.NewSchedulerSnapshotService(
		&retryCancellationSchedulerCache{accounts: []*service.Account{account}}, nil, nil, nil, nil,
	)
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Security.URLAllowlist.Enabled = false
	gatewayService := service.NewGatewayService(
		nil, &fakeGroupRepo{group: group}, nil, nil, nil, nil, nil, nil, cfg,
		schedulerSnapshot, concurrencyService, nil, &service.RateLimitService{}, nil, nil,
		recognizedAnthropicSSEUpstream{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		nil, // usageRecordWorkerPool
	)
	billingCacheService := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCacheService.Stop)
	h := &GatewayHandler{
		gatewayService:      gatewayService,
		billingCacheService: billingCacheService,
		concurrencyHelper:   NewConcurrencyHelper(concurrencyService, SSEPingFormatClaude, 0),
		maxAccountSwitches:  1,
		cfg:                 cfg,
	}

	body := []byte(`{"model":"claude-3-5-sonnet-latest","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), ctxkey.Group, group))
	c.Request = req
	apiKey := &service.APIKey{
		ID:          9213,
		UserID:      9214,
		Concurrency: 1,
		GroupID:     &groupID,
		Status:      service.StatusActive,
		User:        &service.User{ID: 9214, Concurrency: 1, Balance: 100},
		Group:       group,
	}
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID, Concurrency: 1})

	h.Messages(c)

	responseBody := recorder.Body.String()
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Contains(t, responseBody, `"text":"partial"`)
	require.Equal(t, 1, strings.Count(responseBody, "event: error\n"), "body=%q", responseBody)
	require.Contains(t, responseBody, `"type":"invalid_request_error"`)
	require.Contains(t, responseBody, `"message":"max_tokens must be positive"`)
	require.NotContains(t, responseBody, `data: {"type":"error"`+"\n\n")
	require.Equal(t, int32(1), concurrencyCache.accountAcquires.Load())
}
