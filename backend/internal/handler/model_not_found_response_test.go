package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestWriteModelNotFoundIfPureSupportMissSanitizesBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	err := fmt.Errorf("selection failed: %w", &service.ModelNotSupportedByAccountsError{
		RequestedModel: "gpt-public-404",
	})
	require.True(t, writeModelNotFoundIfPureSupportMiss(c, err))
	require.Equal(t, http.StatusNotFound, rec.Code)

	body := rec.Body.String()
	require.NotContains(t, strings.ToLower(body), "account")
	require.NotContains(t, strings.ToLower(body), "group")
	require.NotContains(t, body, "total=")
	require.NotContains(t, body, "https://")
	require.NotContains(t, body, "sk-")
	require.NotContains(t, body, "mapped-internal-model")

	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Equal(t, "model_not_found", payload.Error.Code)
	require.Equal(t, "model_not_found", payload.Error.Type)
	require.Contains(t, payload.Error.Message, "gpt-public-404")
}

func TestWriteModelNotFoundIfPureSupportMissForModelUsesPublicRequestedModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	err := fmt.Errorf("selection failed: %w", &service.ModelNotSupportedByAccountsError{
		RequestedModel: "gpt-5.3-codex",
	})

	require.True(t, writeModelNotFoundIfPureSupportMissForModel(c, err, "claude-sonnet-4-5-20250929"))
	require.Equal(t, http.StatusNotFound, rec.Code)

	body := rec.Body.String()
	require.Contains(t, body, "claude-sonnet-4-5-20250929")
	require.NotContains(t, body, "gpt-5.3-codex")
	require.NotContains(t, strings.ToLower(body), "account")
	require.NotContains(t, strings.ToLower(body), "group")
	require.NotContains(t, body, "https://")
	require.NotContains(t, body, "sk-")
}

func TestOpenAIMessagesModelNotFoundUsesRequestedPublicModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4-5-20250929","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	groupID := int64(7041)
	accountRepo := &modelNotFoundAccountRepoStub{accounts: []service.Account{{
		ID:          704101,
		Name:        "openai-messages-public-model",
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 0,
		Credentials: map[string]any{
			"api_key":       "sk-test",
			"model_mapping": map[string]any{"gpt-5.4": "gpt-5.4"},
		},
	}}}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCacheSvc.Stop)
	concurrencySvc := service.NewConcurrencyService(nil)
	gatewaySvc := service.NewOpenAIGatewayService(
		accountRepo,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		cfg,
		nil,
		concurrencySvc,
		service.NewBillingService(cfg, nil),
		nil,
		billingCacheSvc,
		nil,
		&service.DeferredService{},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil, // usageRecordWorkerPool
	)
	h := NewOpenAIGatewayHandler(gatewaySvc, concurrencySvc, billingCacheSvc, &service.APIKeyService{}, nil, nil, nil, cfg)
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
		ID:      704100,
		GroupID: &groupID,
		User:    &service.User{ID: 7041000, Status: service.StatusActive},
		Group: &service.Group{
			ID:                    groupID,
			Platform:              service.PlatformOpenAI,
			Status:                service.StatusActive,
			AllowMessagesDispatch: true,
		},
	})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7041000, Concurrency: 0})

	h.Messages(c)

	require.Equal(t, http.StatusNotFound, rec.Code)
	body := rec.Body.String()

	var payload struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Equal(t, "error", payload.Type)
	require.Equal(t, "model_not_found", payload.Error.Type)
	require.Equal(t, `The model "claude-sonnet-4-5-20250929" was not found.`, payload.Error.Message)
	require.NotContains(t, body, `"code"`)
	require.NotContains(t, body, "gpt-5.3-codex")
	require.NotContains(t, body, "gpt-5.4")
	require.NotContains(t, strings.ToLower(body), "account")
	require.NotContains(t, strings.ToLower(body), "group")
}

func TestOpenAIMessagesStreamingModelNotFoundAfterPingUsesAnthropicSSE(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4-5-20250929","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	groupID := int64(7043)
	accountRepo := &modelNotFoundAccountRepoStub{accounts: []service.Account{{
		ID:          704301,
		Name:        "openai-messages-stream-public-model",
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 0,
		Credentials: map[string]any{
			"api_key":       "sk-test",
			"model_mapping": map[string]any{"gpt-5.4": "gpt-5.4"},
		},
	}}}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCacheSvc.Stop)
	concurrencyCache := newModelNotFoundPingConcurrencyCache()
	concurrencySvc := service.NewConcurrencyService(concurrencyCache)
	gatewaySvc := service.NewOpenAIGatewayService(
		accountRepo,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		cfg,
		nil,
		concurrencySvc,
		service.NewBillingService(cfg, nil),
		nil,
		billingCacheSvc,
		nil,
		&service.DeferredService{},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil, // usageRecordWorkerPool
	)
	h := NewOpenAIGatewayHandler(gatewaySvc, concurrencySvc, billingCacheSvc, &service.APIKeyService{}, nil, nil, nil, cfg)
	h.concurrencyHelper = NewConcurrencyHelper(concurrencySvc, SSEPingFormatComment, time.Millisecond)
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
		ID:      704300,
		GroupID: &groupID,
		User:    &service.User{ID: 7043000, Status: service.StatusActive},
		Group: &service.Group{
			ID:                    groupID,
			Platform:              service.PlatformOpenAI,
			Status:                service.StatusActive,
			AllowMessagesDispatch: true,
		},
	})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7043000, Concurrency: 1})

	h.Messages(c)

	body := rec.Body.String()
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, body, ":\n\n")
	require.Contains(t, body, "event: error\n")

	eventPayload := decodeModelNotFoundSSEPayload(t, body)
	require.Equal(t, "error", eventPayload.Type)
	require.Equal(t, "model_not_found", eventPayload.Error.Type)
	require.Equal(t, `The model "claude-sonnet-4-5-20250929" was not found.`, eventPayload.Error.Message)
	require.NotContains(t, body, `"code":"model_not_found"`)
	require.NotContains(t, body, `"error":{"code":`)
	require.NotContains(t, body, "gpt-5.3-codex")
	require.NotContains(t, body, "gpt-5.4")
	require.NotContains(t, strings.ToLower(body), "account")
	require.NotContains(t, strings.ToLower(body), "group")
}

func TestOpenAICompatibleChatCompletionsStreamingModelNotFoundAfterPingUsesSSE(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"claude-sonnet-4-5-20250929","stream":true,"messages":[{"role":"user","content":"hello"}]}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	groupID := int64(7044)
	accountRepo := &modelNotFoundAccountRepoStub{accounts: []service.Account{{
		ID:          704401,
		Name:        "openai-compatible-chat-stream-public-model",
		Platform:    service.PlatformAnthropic,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 0,
		Credentials: map[string]any{
			"api_key":       "sk-ant-test",
			"model_mapping": map[string]any{"claude-haiku-3-5-20241022": "claude-3-5-haiku-20241022"},
		},
	}}}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCacheSvc.Stop)
	concurrencyCache := newModelNotFoundPingConcurrencyCache()
	concurrencySvc := service.NewConcurrencyService(concurrencyCache)
	gatewaySvc := service.NewGatewayService(
		accountRepo,
		&fakeGroupRepo{group: &service.Group{ID: groupID, Platform: service.PlatformAnthropic, Status: service.StatusActive, Hydrated: true}},
		nil,
		nil,
		nil,
		nil,
		nil,
		&modelNotFoundGatewayCacheStub{},
		cfg,
		nil,
		concurrencySvc,
		service.NewBillingService(cfg, nil),
		nil,
		billingCacheSvc,
		nil,
		nil,
		&service.DeferredService{},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil, // usageRecordWorkerPool
	)
	h := &GatewayHandler{
		gatewayService:           gatewaySvc,
		billingCacheService:      billingCacheSvc,
		concurrencyHelper:        NewConcurrencyHelper(concurrencySvc, SSEPingFormatComment, time.Millisecond),
		maxAccountSwitches:       1,
		maxAccountSwitchesGemini: 1,
	}
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
		ID:      704400,
		GroupID: &groupID,
		User:    &service.User{ID: 7044000, Status: service.StatusActive},
		Group:   &service.Group{ID: groupID, Platform: service.PlatformAnthropic, Status: service.StatusActive},
	})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7044000, Concurrency: 1})

	h.ChatCompletions(c)

	body := rec.Body.String()
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, body, ":\n\n")
	require.Contains(t, body, `data: {"type":"error"`)
	require.Contains(t, body, "model_not_found")
	eventPayload := decodeGenericModelNotFoundSSEPayload(t, body)
	require.Equal(t, "error", eventPayload.Type)
	require.Equal(t, "model_not_found", eventPayload.Error.Type)
	require.Equal(t, `The model "claude-sonnet-4-5-20250929" was not found.`, eventPayload.Error.Message)
	require.NotContains(t, body, "\n\n{\"error\"")
	require.NotContains(t, body, `"code":"model_not_found"`)
	require.NotContains(t, body, "claude-3-5-haiku-20241022")
	require.NotContains(t, strings.ToLower(body), "account")
	require.NotContains(t, strings.ToLower(body), "group")
}

func TestCountTokensUnsupportedModelReturnsModelNotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/v1/messages/count_tokens",
		strings.NewReader(`{"model":"claude-sonnet-4-5-20250929","messages":[{"role":"user","content":"hello"}]}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	groupID := int64(7045)
	h, cleanup := newCountTokensModelNotFoundGatewayHandler(t, groupID, []service.Account{{
		ID:          704501,
		Name:        "count-tokens-supported-account",
		Platform:    service.PlatformAnthropic,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 0,
		Credentials: map[string]any{
			"api_key":       "sk-ant-test",
			"model_mapping": map[string]any{"claude-haiku-3-5-20241022": "claude-3-5-haiku-20241022"},
		},
	}})
	defer cleanup()
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
		ID:      704500,
		GroupID: &groupID,
		User:    &service.User{ID: 7045000, Status: service.StatusActive},
		Group:   &service.Group{ID: groupID, Platform: service.PlatformAnthropic, Status: service.StatusActive},
	})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7045000, Concurrency: 0})

	h.CountTokens(c)

	require.Equal(t, http.StatusNotFound, rec.Code)
	body := rec.Body.String()
	var payload struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Equal(t, "error", payload.Type)
	require.Equal(t, "model_not_found", payload.Error.Type)
	require.Equal(t, `The model "claude-sonnet-4-5-20250929" was not found.`, payload.Error.Message)
	require.NotContains(t, body, "claude-3-5-haiku-20241022")
	require.NotContains(t, strings.ToLower(body), "account")
	require.NotContains(t, strings.ToLower(body), "group")
}

func TestCountTokensTemporaryUnavailableRemains503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/v1/messages/count_tokens",
		strings.NewReader(`{"model":"claude-haiku-3-5-20241022","messages":[{"role":"user","content":"hello"}]}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	groupID := int64(7046)
	pausedUntil := time.Now().Add(time.Hour)
	h, cleanup := newCountTokensModelNotFoundGatewayHandler(t, groupID, []service.Account{{
		ID:                      704601,
		Name:                    "count-tokens-temporarily-unavailable-account",
		Platform:                service.PlatformAnthropic,
		Type:                    service.AccountTypeAPIKey,
		Status:                  service.StatusActive,
		Schedulable:             true,
		TempUnschedulableUntil:  &pausedUntil,
		TempUnschedulableReason: "transient upstream health",
		Concurrency:             0,
		Credentials: map[string]any{
			"api_key":       "sk-ant-test",
			"model_mapping": map[string]any{"claude-haiku-3-5-20241022": "claude-3-5-haiku-20241022"},
		},
	}})
	defer cleanup()
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
		ID:      704600,
		GroupID: &groupID,
		User:    &service.User{ID: 7046000, Status: service.StatusActive},
		Group:   &service.Group{ID: groupID, Platform: service.PlatformAnthropic, Status: service.StatusActive},
	})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7046000, Concurrency: 0})

	h.CountTokens(c)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Equal(t, "api_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
	require.Equal(t, "Service temporarily unavailable", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
	require.NotContains(t, rec.Body.String(), "model_not_found")
}

func TestGeminiV1BetaModelNotFoundUsesRoutePublicModel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = gin.Params{{Key: "modelAction", Value: "/public-gemini-alias:generateContent"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/public-gemini-alias:generateContent", strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`))
	c.Request.Header.Set("Content-Type", "application/json")

	groupID := int64(7042)
	accountRepo := &modelNotFoundAccountRepoStub{accounts: []service.Account{{
		ID:          704201,
		Name:        "gemini-public-model",
		Platform:    service.PlatformGemini,
		Type:        service.AccountTypeOAuth,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 0,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"gemini-supported-public": "gemini-supported-public"},
		},
	}}}
	channelSvc := service.NewChannelService(&openAIWSUsageHandlerChannelRepoStub{
		channels: []service.Channel{{
			ID:           7042,
			Name:         "gemini-public-model-channel",
			Status:       service.StatusActive,
			GroupIDs:     []int64{groupID},
			ModelMapping: map[string]map[string]string{service.PlatformGemini: {"public-gemini-alias": "gemini-upstream-internal"}},
		}},
		groupPlatforms: map[int64]string{groupID: service.PlatformGemini},
	}, nil, nil, nil)
	cfg := &config.Config{RunMode: config.RunModeSimple}
	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCacheSvc.Stop)
	concurrencySvc := service.NewConcurrencyService(nil)
	gatewaySvc := service.NewGatewayService(
		accountRepo,
		&fakeGroupRepo{group: &service.Group{ID: groupID, Platform: service.PlatformGemini, Status: service.StatusActive, Hydrated: true}},
		nil,
		nil,
		nil,
		nil,
		nil,
		&modelNotFoundGatewayCacheStub{},
		cfg,
		nil,
		concurrencySvc,
		service.NewBillingService(cfg, nil),
		nil,
		billingCacheSvc,
		nil,
		nil,
		&service.DeferredService{},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		channelSvc,
		nil,
		nil,
		nil,
		nil, // usageRecordWorkerPool
	)
	h := &GatewayHandler{
		gatewayService:      gatewaySvc,
		billingCacheService: billingCacheSvc,
		concurrencyHelper:   NewConcurrencyHelper(concurrencySvc, SSEPingFormatNone, time.Second),
	}
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{
		ID:      704200,
		GroupID: &groupID,
		User:    &service.User{ID: 7042000, Status: service.StatusActive},
		Group:   &service.Group{ID: groupID, Platform: service.PlatformGemini, Status: service.StatusActive},
	})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7042000, Concurrency: 0})

	h.GeminiV1BetaModels(c)

	require.Equal(t, http.StatusNotFound, rec.Code)
	body := rec.Body.String()
	var payload struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Equal(t, http.StatusNotFound, payload.Error.Code)
	require.Equal(t, `The model "public-gemini-alias" was not found.`, payload.Error.Message)
	require.Equal(t, "NOT_FOUND", payload.Error.Status)
	require.NotContains(t, body, `"type"`)
	require.NotContains(t, body, "gemini-upstream-internal")
	require.NotContains(t, body, "gemini-supported-public")
	require.NotContains(t, strings.ToLower(body), "account")
	require.NotContains(t, strings.ToLower(body), "group")
}

func decodeModelNotFoundSSEPayload(t *testing.T, body string) struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
} {
	t.Helper()
	idx := strings.LastIndex(body, "event: error\n")
	require.NotEqual(t, -1, idx, "body: %q", body)
	event := strings.TrimSuffix(body[idx:], "\n\n")
	lines := strings.Split(event, "\n")
	require.Len(t, lines, 2)
	require.Equal(t, "event: error", lines[0])
	require.True(t, strings.HasPrefix(lines[1], "data: "), "line: %q", lines[1])

	var payload struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &payload))
	return payload
}

func decodeGenericModelNotFoundSSEPayload(t *testing.T, body string) struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
} {
	t.Helper()
	idx := strings.LastIndex(body, "data: {")
	require.NotEqual(t, -1, idx, "body: %q", body)
	event := strings.TrimSuffix(body[idx:], "\n\n")
	require.True(t, strings.HasPrefix(event, "data: "), "event: %q", event)

	var payload struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(event, "data: ")), &payload))
	return payload
}

func newCountTokensModelNotFoundGatewayHandler(t *testing.T, groupID int64, accounts []service.Account) (*GatewayHandler, func()) {
	t.Helper()
	cfg := &config.Config{RunMode: config.RunModeSimple}
	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	concurrencySvc := service.NewConcurrencyService(nil)
	group := &service.Group{ID: groupID, Platform: service.PlatformAnthropic, Status: service.StatusActive}
	gatewaySvc := service.NewGatewayService(
		&modelNotFoundAccountRepoStub{accounts: accounts},
		&fakeGroupRepo{group: group},
		nil,
		nil,
		nil,
		nil,
		nil,
		&modelNotFoundGatewayCacheStub{},
		cfg,
		nil,
		concurrencySvc,
		service.NewBillingService(cfg, nil),
		nil,
		billingCacheSvc,
		nil,
		nil,
		&service.DeferredService{},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil, // usageRecordWorkerPool
	)
	return &GatewayHandler{gatewayService: gatewaySvc, billingCacheService: billingCacheSvc}, billingCacheSvc.Stop
}

type modelNotFoundGatewayCacheStub struct{}

func (modelNotFoundGatewayCacheStub) GetSessionAccountID(context.Context, int64, string) (int64, error) {
	return 0, nil
}

func (modelNotFoundGatewayCacheStub) SetSessionAccountID(context.Context, int64, string, int64, time.Duration) error {
	return nil
}

func (modelNotFoundGatewayCacheStub) RefreshSessionTTL(context.Context, int64, string, time.Duration) error {
	return nil
}

func (modelNotFoundGatewayCacheStub) DeleteSessionAccountID(context.Context, int64, string) error {
	return nil
}

type modelNotFoundAccountRepoStub struct {
	service.AccountRepository
	accounts []service.Account
}

func (s *modelNotFoundAccountRepoStub) ListSchedulableByPlatform(_ context.Context, platform string) ([]service.Account, error) {
	return s.listSchedulableByPlatforms([]string{platform}), nil
}

func (s *modelNotFoundAccountRepoStub) ListSchedulableByGroupIDAndPlatform(ctx context.Context, _ int64, platform string) ([]service.Account, error) {
	return s.ListSchedulableByPlatform(ctx, platform)
}

func (s *modelNotFoundAccountRepoStub) ListSchedulableUngroupedByPlatform(ctx context.Context, platform string) ([]service.Account, error) {
	return s.ListSchedulableByPlatform(ctx, platform)
}

func (s *modelNotFoundAccountRepoStub) ListSchedulableByPlatforms(_ context.Context, platforms []string) ([]service.Account, error) {
	return s.listSchedulableByPlatforms(platforms), nil
}

func (s *modelNotFoundAccountRepoStub) ListSchedulableByGroupIDAndPlatforms(ctx context.Context, _ int64, platforms []string) ([]service.Account, error) {
	return s.ListSchedulableByPlatforms(ctx, platforms)
}

func (s *modelNotFoundAccountRepoStub) ListSchedulableUngroupedByPlatforms(ctx context.Context, platforms []string) ([]service.Account, error) {
	return s.ListSchedulableByPlatforms(ctx, platforms)
}

func (s *modelNotFoundAccountRepoStub) GetByID(_ context.Context, id int64) (*service.Account, error) {
	for _, account := range s.accounts {
		if account.ID == id {
			acc := account
			return &acc, nil
		}
	}
	return nil, nil
}

func (s *modelNotFoundAccountRepoStub) SetError(context.Context, int64, string) error {
	return nil
}

func (s *modelNotFoundAccountRepoStub) listSchedulableByPlatforms(platforms []string) []service.Account {
	allowed := make(map[string]struct{}, len(platforms))
	for _, platform := range platforms {
		allowed[platform] = struct{}{}
	}
	out := make([]service.Account, 0, len(s.accounts))
	for _, account := range s.accounts {
		if _, ok := allowed[account.Platform]; ok && account.IsSchedulable() {
			out = append(out, account)
		}
	}
	return out
}

type modelNotFoundPingConcurrencyCache struct {
	mu           sync.Mutex
	userAttempts int
}

func newModelNotFoundPingConcurrencyCache() *modelNotFoundPingConcurrencyCache {
	return &modelNotFoundPingConcurrencyCache{}
}

func (c *modelNotFoundPingConcurrencyCache) AcquireAccountSlot(context.Context, int64, int, string) (bool, error) {
	return true, nil
}

func (c *modelNotFoundPingConcurrencyCache) ReleaseAccountSlot(context.Context, int64, string) error {
	return nil
}

func (c *modelNotFoundPingConcurrencyCache) GetAccountConcurrency(context.Context, int64) (int, error) {
	return 0, nil
}

func (c *modelNotFoundPingConcurrencyCache) GetAccountConcurrencyBatch(_ context.Context, accountIDs []int64) (map[int64]int, error) {
	result := make(map[int64]int, len(accountIDs))
	for _, accountID := range accountIDs {
		result[accountID] = 0
	}
	return result, nil
}

func (c *modelNotFoundPingConcurrencyCache) IncrementAccountWaitCount(context.Context, int64, int) (bool, error) {
	return true, nil
}

func (c *modelNotFoundPingConcurrencyCache) DecrementAccountWaitCount(context.Context, int64) error {
	return nil
}

func (c *modelNotFoundPingConcurrencyCache) GetAccountWaitingCount(context.Context, int64) (int, error) {
	return 0, nil
}

func (c *modelNotFoundPingConcurrencyCache) AcquireUserSlot(context.Context, int64, int, string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.userAttempts++
	if c.userAttempts == 1 {
		return false, nil
	}
	return true, nil
}

func (c *modelNotFoundPingConcurrencyCache) ReleaseUserSlot(context.Context, int64, string) error {
	return nil
}

func (c *modelNotFoundPingConcurrencyCache) GetUserConcurrency(context.Context, int64) (int, error) {
	return 0, nil
}

func (c *modelNotFoundPingConcurrencyCache) IncrementWaitCount(context.Context, int64, int) (bool, error) {
	return true, nil
}

func (c *modelNotFoundPingConcurrencyCache) DecrementWaitCount(context.Context, int64) error {
	return nil
}

func (c *modelNotFoundPingConcurrencyCache) GetAccountsLoadBatch(_ context.Context, accounts []service.AccountWithConcurrency) (map[int64]*service.AccountLoadInfo, error) {
	result := make(map[int64]*service.AccountLoadInfo, len(accounts))
	for _, account := range accounts {
		result[account.ID] = &service.AccountLoadInfo{AccountID: account.ID}
	}
	return result, nil
}

func (c *modelNotFoundPingConcurrencyCache) GetUsersLoadBatch(_ context.Context, users []service.UserWithConcurrency) (map[int64]*service.UserLoadInfo, error) {
	result := make(map[int64]*service.UserLoadInfo, len(users))
	for _, user := range users {
		result[user.ID] = &service.UserLoadInfo{UserID: user.ID}
	}
	return result, nil
}

func (c *modelNotFoundPingConcurrencyCache) CleanupExpiredAccountSlots(context.Context, int64) error {
	return nil
}

func (c *modelNotFoundPingConcurrencyCache) CleanupExpiredAccountSlotKeys(context.Context) error {
	return nil
}

func (c *modelNotFoundPingConcurrencyCache) CleanupStaleProcessSlots(context.Context, string) error {
	return nil
}
