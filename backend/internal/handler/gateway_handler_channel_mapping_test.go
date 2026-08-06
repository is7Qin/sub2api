//go:build unit

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type gatewayChannelMappingRepoStub struct {
	service.ChannelRepository
	channel  service.Channel
	platform string
}

func (s *gatewayChannelMappingRepoStub) ListAll(context.Context) ([]service.Channel, error) {
	return []service.Channel{s.channel}, nil
}

func (s *gatewayChannelMappingRepoStub) GetGroupPlatforms(context.Context, []int64) (map[int64]string, error) {
	return map[int64]string{s.channel.GroupIDs[0]: s.platform}, nil
}

type gatewayChannelMappingHTTPUpstream struct {
	service.HTTPUpstream
	responseBody string
	lastRequest  *http.Request
	lastBody     []byte
}

func (u *gatewayChannelMappingHTTPUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.lastRequest = req
	if req.Body != nil {
		u.lastBody, _ = io.ReadAll(req.Body)
	}
	contentType := "application/json"
	if strings.Contains(req.URL.Path, "v1internal") {
		contentType = "text/event-stream"
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(u.responseBody)),
	}, nil
}

func (u *gatewayChannelMappingHTTPUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

func newGatewayChannelMappingHandler(t *testing.T, group *service.Group, account *service.Account, upstream service.HTTPUpstream) (*GatewayHandler, func()) {
	return newGatewayChannelMappingHandlerWithMapping(t, group, account, upstream, map[string]string{"alias-model": "wire-model"})
}

func newGatewayChannelMappingHandlerWithMapping(t *testing.T, group *service.Group, account *service.Account, upstream service.HTTPUpstream, mapping map[string]string) (*GatewayHandler, func()) {
	t.Helper()

	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Gateway.Scheduling.LoadBatchEnabled = false
	channelService := service.NewChannelService(&gatewayChannelMappingRepoStub{
		platform: group.Platform,
		channel: service.Channel{
			ID:       7001,
			Status:   service.StatusActive,
			GroupIDs: []int64{group.ID},
			ModelMapping: map[string]map[string]string{
				group.Platform: mapping,
			},
		},
	}, nil, nil, nil)
	concurrencyService := service.NewConcurrencyService(&fakeConcurrencyCache{})
	schedulerSnapshot := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: []*service.Account{account}}, nil, nil, nil, nil)
	gatewayService := service.NewGatewayService(
		nil,
		&fakeGroupRepo{group: group},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		cfg,
		schedulerSnapshot,
		concurrencyService,
		service.NewBillingService(cfg, nil),
		&service.RateLimitService{},
		nil,
		nil,
		upstream,
		service.NewDeferredService(nil, nil, 0),
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		channelService,
		nil,
		nil,
		nil,
		nil, nil, // usageRecordWorkerPool
	)
	billingCacheService := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	settingService := service.NewSettingService(nil, cfg)
	antigravityService := service.NewAntigravityGatewayService(
		nil,
		nil,
		schedulerSnapshot,
		service.NewAntigravityTokenProvider(nil, nil, service.NewAntigravityOAuthService(nil)),
		&service.RateLimitService{},
		upstream,
		settingService,
		nil,
	)
	geminiCompatService := service.NewGeminiMessagesCompatService(
		nil,
		&fakeGroupRepo{group: group},
		nil,
		schedulerSnapshot,
		nil,
		&service.RateLimitService{},
		upstream,
		antigravityService,
		cfg,
	)
	handler := NewGatewayHandler(
		gatewayService,
		nil,
		geminiCompatService,
		antigravityService,
		nil,
		concurrencyService,
		billingCacheService,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		cfg,
		settingService,
	)
	return handler, billingCacheService.Stop
}

func runGatewayMessagesChannelMappingCase(t *testing.T, account *service.Account, upstream *gatewayChannelMappingHTTPUpstream) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	groupID := int64(7101)
	group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformGemini, Status: service.StatusActive}
	account.AccountGroups = []service.AccountGroup{{AccountID: account.ID, GroupID: groupID}}
	handler, cleanup := newGatewayChannelMappingHandler(t, group, account, upstream)
	t.Cleanup(cleanup)

	body := []byte(`{"model":"alias-model","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), ctxkey.Group, group))
	c.Request = req
	apiKey := &service.APIKey{
		ID:          7102,
		UserID:      7103,
		Concurrency: 0,
		GroupID:     &groupID,
		Status:      service.StatusActive,
		User:        &service.User{ID: 7103, Concurrency: 1, Balance: 100},
		Group:       group,
	}
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID, Concurrency: 1})

	handler.Messages(c)
	return recorder, c
}

func TestGatewayHandlerResponses_MappedImageModelSetsSelectionIntent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(7106)
	group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformOpenAI, Status: service.StatusActive}
	upstream := &gatewayChannelMappingHTTPUpstream{
		responseBody: `{"id":"resp_image_alias","object":"response","status":"completed","model":"wire-model","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`,
	}
	account := &service.Account{
		ID: 7206, Name: "openai", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Concurrency: 1, Priority: 1, Status: service.StatusActive, Schedulable: true,
		Credentials: map[string]any{
			"api_key":       "test-key",
			"base_url":      "https://upstream.example",
			"model_mapping": map[string]any{"gpt-image-1": "gpt-image-1"},
		},
		AccountGroups: []service.AccountGroup{{AccountID: 7206, GroupID: groupID}},
		Extra: map[string]any{
			"anthropic_passthrough": true,
			"model_rate_limits": map[string]any{
				"openai:image_generation": map[string]any{
					"rate_limit_reset_at": time.Now().Add(time.Hour).Format(time.RFC3339),
				},
			},
		},
	}
	handler, cleanup := newGatewayChannelMappingHandlerWithMapping(t, group, account, upstream, map[string]string{"alias-model": "gpt-image-1"})
	t.Cleanup(cleanup)

	body := []byte(`{"model":"alias-model","input":"draw"}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req.WithContext(context.WithValue(req.Context(), ctxkey.Group, group))
	apiKey := &service.APIKey{ID: 7107, UserID: 7108, GroupID: &groupID, Status: service.StatusActive, User: &service.User{ID: 7108, Balance: 100}, Group: group}
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID})

	handler.Responses(c)

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code, recorder.Body.String())
	require.Nil(t, upstream.lastRequest, "mapped image intent must exclude an image-rate-limited account")
	require.Equal(t, "alias-model", c.Request.Context().Value(ctxkey.Model))
}

func TestGatewayHandlerChatCompletions_ChannelMappingSelectionAndForwarding(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(7100)
	group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	upstream := &gatewayChannelMappingHTTPUpstream{
		responseBody: strings.Join([]string{
			`event: message_start`,
			`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"wire-model","usage":{"input_tokens":1}}}`,
			``,
			`event: content_block_start`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"ok"}}`,
			``,
			`event: message_delta`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
			``,
		}, "\n"),
	}
	account := &service.Account{
		ID: 7200, Name: "anthropic", Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey,
		Concurrency: 1, Priority: 1, Status: service.StatusActive, Schedulable: true,
		Extra: map[string]any{"anthropic_passthrough": true},
		Credentials: map[string]any{
			"api_key":       "test-key",
			"base_url":      "https://upstream.example",
			"model_mapping": map[string]any{"wire-model": "upstream-wire-model"},
		},
		AccountGroups: []service.AccountGroup{{AccountID: 7200, GroupID: groupID}},
	}
	handler, cleanup := newGatewayChannelMappingHandler(t, group, account, upstream)
	t.Cleanup(cleanup)

	body := []byte(`{"model":"alias-model","messages":[{"role":"user","content":"hello"}]}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req.WithContext(context.WithValue(req.Context(), ctxkey.Group, group))
	apiKey := &service.APIKey{ID: 7101, UserID: 7102, GroupID: &groupID, Status: service.StatusActive, User: &service.User{ID: 7102, Balance: 100}, Group: group}
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID})

	handler.ChatCompletions(c)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.NotNil(t, upstream.lastRequest, "wire-model-only account must be selected")
	require.Equal(t, "upstream-wire-model", gjson.GetBytes(upstream.lastBody, "model").String())
	require.Equal(t, "alias-model", c.Request.Context().Value(ctxkey.Model))
}

func TestGatewayHandlerMessages_NonGeminiChannelMappingSelectionAndForwarding(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(7104)
	group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	upstream := &gatewayChannelMappingHTTPUpstream{
		responseBody: `{"id":"msg_2","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"wire-model","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
	}
	account := &service.Account{
		ID: 7204, Name: "anthropic", Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey,
		Concurrency: 1, Priority: 1, Status: service.StatusActive, Schedulable: true,
		Extra: map[string]any{"anthropic_passthrough": true},
		Credentials: map[string]any{
			"api_key":       "test-key",
			"base_url":      "https://upstream.example",
			"model_mapping": map[string]any{"wire-model": "upstream-wire-model"},
		},
		AccountGroups: []service.AccountGroup{{AccountID: 7204, GroupID: groupID}},
	}
	handler, cleanup := newGatewayChannelMappingHandler(t, group, account, upstream)
	t.Cleanup(cleanup)

	body := []byte(`{"model":"alias-model","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req.WithContext(context.WithValue(req.Context(), ctxkey.Group, group))
	apiKey := &service.APIKey{ID: 7105, UserID: 7106, GroupID: &groupID, Status: service.StatusActive, User: &service.User{ID: 7106, Balance: 100}, Group: group}
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID})

	handler.Messages(c)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.NotNil(t, upstream.lastRequest, "wire-model-only account must be selected")
	require.Equal(t, "upstream-wire-model", gjson.GetBytes(upstream.lastBody, "model").String())
	require.Equal(t, "alias-model", c.Request.Context().Value(ctxkey.Model))
}

func TestGatewayHandlerMessages_GeminiChannelMappingSelectionAndForwarding(t *testing.T) {
	t.Run("Gemini compatibility", func(t *testing.T) {
		upstream := &gatewayChannelMappingHTTPUpstream{
			responseBody: `{"candidates":[{"content":{"parts":[{"text":"ok"}]}}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1}}`,
		}
		account := &service.Account{
			ID: 7201, Name: "gemini", Platform: service.PlatformGemini, Type: service.AccountTypeAPIKey,
			Concurrency: 1, Priority: 1, Status: service.StatusActive, Schedulable: true,
			Credentials: map[string]any{
				"api_key":       "test-key",
				"model_mapping": map[string]any{"wire-model": "wire-model"},
			},
		}

		recorder, c := runGatewayMessagesChannelMappingCase(t, account, upstream)

		require.NotNil(t, upstream.lastRequest, "wire-model-only account must be selected and forwarded: status=%d body=%s", recorder.Code, recorder.Body.String())
		require.Contains(t, upstream.lastRequest.URL.Path, "/models/wire-model:")
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		require.Equal(t, "alias-model", c.Request.Context().Value(ctxkey.Model))
	})

	t.Run("Antigravity OAuth", func(t *testing.T) {
		upstream := &gatewayChannelMappingHTTPUpstream{
			responseBody: "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":1}}}\n\n",
		}
		account := &service.Account{
			ID: 7202, Name: "antigravity", Platform: service.PlatformAntigravity, Type: service.AccountTypeOAuth,
			Concurrency: 1, Priority: 1, Status: service.StatusActive, Schedulable: true,
			Extra: map[string]any{"mixed_scheduling": true},
			Credentials: map[string]any{
				"access_token":  "test-token",
				"project_id":    "test-project",
				"model_mapping": map[string]any{"wire-model": "upstream-wire-model"},
			},
		}

		recorder, c := runGatewayMessagesChannelMappingCase(t, account, upstream)

		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		require.NotNil(t, upstream.lastRequest, "wire-model-only account must be selected and forwarded: status=%d body=%s", recorder.Code, recorder.Body.String())
		var wrapped map[string]any
		require.NoError(t, json.Unmarshal(upstream.lastBody, &wrapped))
		require.Equal(t, "upstream-wire-model", wrapped["model"])
		request, ok := wrapped["request"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "wire-model", request["model"])
		require.Equal(t, "alias-model", c.Request.Context().Value(ctxkey.Model))
	})
}
