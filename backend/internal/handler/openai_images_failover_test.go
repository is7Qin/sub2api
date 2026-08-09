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
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type openAIImagesFailoverAccountRepo struct {
	service.AccountRepository
	accounts []service.Account
}

func (r openAIImagesFailoverAccountRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	for i := range r.accounts {
		if r.accounts[i].ID == id {
			account := r.accounts[i]
			return &account, nil
		}
	}
	return nil, service.ErrNoAvailableAccounts
}

func (r openAIImagesFailoverAccountRepo) UpdateExtra(_ context.Context, _ int64, _ map[string]any) error {
	// OAuth forwarding ensures a Codex fingerprint before contacting upstream;
	// this test repository only needs to accept that persistence hook.
	return nil
}

func (r openAIImagesFailoverAccountRepo) ListSchedulableByGroupIDAndPlatform(_ context.Context, _ int64, platform string) ([]service.Account, error) {
	return r.accountsForPlatform(platform), nil
}

func (r openAIImagesFailoverAccountRepo) ListSchedulableByPlatform(_ context.Context, platform string) ([]service.Account, error) {
	return r.accountsForPlatform(platform), nil
}

func (r openAIImagesFailoverAccountRepo) ListSchedulableUngroupedByPlatform(_ context.Context, platform string) ([]service.Account, error) {
	return r.accountsForPlatform(platform), nil
}

func (r openAIImagesFailoverAccountRepo) accountsForPlatform(platform string) []service.Account {
	out := make([]service.Account, 0, len(r.accounts))
	for _, account := range r.accounts {
		if account.Platform == platform {
			out = append(out, account)
		}
	}
	return out
}

type openAIImagesFailoverHTTPUpstream struct {
	service.HTTPUpstream
	mu         sync.Mutex
	accountIDs []int64
}

type cancelOnAccountAcquireCache struct {
	fakeConcurrencyCache
	mu          sync.Mutex
	cancel      context.CancelFunc
	cancelAt    int
	acquireCall int
	releaseCall int
}

func (c *cancelOnAccountAcquireCache) AcquireAccountSlot(_ context.Context, _ int64, _ int, _ string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.acquireCall++
	acquired := c.acquireCall >= c.cancelAt
	if c.acquireCall == c.cancelAt {
		c.cancel()
	}
	return acquired, nil
}

func (c *cancelOnAccountAcquireCache) ReleaseAccountSlot(_ context.Context, _ int64, _ string) error {
	c.mu.Lock()
	c.releaseCall++
	c.mu.Unlock()
	return nil
}

func (c *cancelOnAccountAcquireCache) snapshot() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.acquireCall, c.releaseCall
}

type openAICanceled520Upstream struct {
	service.HTTPUpstream
	mu         sync.Mutex
	accountIDs []int64
	cancel     context.CancelFunc
}

func (u *openAICanceled520Upstream) Do(_ *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.accountIDs = append(u.accountIDs, accountID)
	first := len(u.accountIDs) == 1
	u.mu.Unlock()
	if first {
		u.cancel()
	}
	return &http.Response{
		StatusCode: 520,
		Header:     http.Header{"Content-Type": []string{"text/html"}},
		Body:       io.NopCloser(bytes.NewBufferString("<html>520</html>")),
	}, nil
}

func (u *openAICanceled520Upstream) calls() []int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]int64(nil), u.accountIDs...)
}

func (u *openAIImagesFailoverHTTPUpstream) Do(_ *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.accountIDs = append(u.accountIDs, accountID)
	u.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"text/event-stream"},
			"X-Request-Id": []string{"req_img_failover"},
		},
		Body: io.NopCloser(bytes.NewBufferString(
			"data: {\"type\":\"error\",\"error\":{\"type\":\"server_error\",\"code\":\"server_error\",\"message\":\"image backend unavailable\"}}\n\n",
		)),
	}, nil
}

func (u *openAIImagesFailoverHTTPUpstream) calls() []int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]int64(nil), u.accountIDs...)
}

type openAIImagesSequenceHTTPUpstream struct {
	service.HTTPUpstream
	mu         sync.Mutex
	accountIDs []int64
	responses  []*http.Response
}

func (u *openAIImagesSequenceHTTPUpstream) Do(_ *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.accountIDs = append(u.accountIDs, accountID)
	idx := len(u.accountIDs) - 1
	if idx >= len(u.responses) {
		idx = len(u.responses) - 1
	}
	return u.responses[idx], nil
}

func (u *openAIImagesSequenceHTTPUpstream) calls() []int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]int64(nil), u.accountIDs...)
}

type openAIImagesFailoverUsageRepo struct {
	service.UsageLogRepository
	mu      sync.Mutex
	calls   int
	lastLog *service.UsageLog
}

func (r *openAIImagesFailoverUsageRepo) Create(_ context.Context, log *service.UsageLog) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.lastLog = log
	return true, nil
}

func (r *openAIImagesFailoverUsageRepo) snapshot() (int, *service.UsageLog) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, r.lastLog
}

func TestBoundedImageDiagnosticValue(t *testing.T) {
	for _, tt := range []struct {
		name  string
		value string
		known map[string]struct{}
		want  string
	}{
		{name: "known", value: "HIGH", known: imageQualityDiagnosticValues, want: "high"},
		{name: "missing", value: "", known: imageQualityDiagnosticValues, want: "default"},
		{name: "arbitrary long", value: strings.Repeat("private-", 100), known: imageQualityDiagnosticValues, want: "other"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, boundedImageDiagnosticValue(tt.value, tt.known))
		})
	}
}

func TestOpenAIGatewayHandlerImages_ServerErrorFailsOverAndReturnsClearErrorWhenExhausted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	groupID := int64(3130)
	accounts := []service.Account{
		{
			ID:          1,
			Name:        "image-account-1",
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeOAuth,
			Status:      service.StatusActive,
			Schedulable: true,
			Concurrency: 0,
			Priority:    0,
			Credentials: map[string]any{"access_token": "token-1", "chatgpt_account_id": "chatgpt-1"},
		},
		{
			ID:          2,
			Name:        "image-account-2",
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeOAuth,
			Status:      service.StatusActive,
			Schedulable: true,
			Concurrency: 0,
			Priority:    1,
			Credentials: map[string]any{"access_token": "token-2", "chatgpt_account_id": "chatgpt-2"},
		},
	}
	accountRepo := openAIImagesFailoverAccountRepo{accounts: accounts}
	upstream := &openAIImagesFailoverHTTPUpstream{}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	gatewayService := service.NewOpenAIGatewayService(
		accountRepo,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		cfg,
		nil,
		nil,
		nil,
		nil,
		nil,
		upstream,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil, nil, // usageRecordWorkerPool
	)
	billingService := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingService.Stop)
	concurrencyService := service.NewConcurrencyService(nil)
	handler := NewOpenAIGatewayHandler(
		gatewayService,
		concurrencyService,
		billingService,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg),
		nil,
		nil,
		nil,
		cfg,
	)
	handler.maxAccountSwitches = 10

	secretPrompt := "draw a private cat"
	arbitraryQuality := strings.Repeat("secret-quality-", 100)
	body := []byte(`{"model":"gpt-image-2","prompt":"` + secretPrompt + `","quality":"` + arbitraryQuality + `","size":"1536x1024","image_url":"https://secret.example/image"}`)
	core, observedLogs := observer.New(zap.DebugLevel)
	requestCtx := logger.IntoContext(context.Background(), zap.New(core))
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(body)).WithContext(requestCtx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		ID:      99,
		GroupID: &groupID,
		Group: &service.Group{
			ID:                   groupID,
			AllowImageGeneration: true,
		},
		User: &service.User{ID: 100},
	})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100, Concurrency: 0})

	handler.Images(c)

	accountSelectingLogs := observedLogs.FilterMessage("openai.images.account_selecting").All()
	require.Len(t, accountSelectingLogs, 3)
	for _, entry := range accountSelectingLogs {
		fields := entry.ContextMap()
		require.Equal(t, "other", fields["img_quality"])
		require.Equal(t, "1536x1024", fields["img_size"])
		encoded, err := json.Marshal(fields)
		require.NoError(t, err)
		require.NotContains(t, string(encoded), secretPrompt)
		require.NotContains(t, string(encoded), arbitraryQuality)
		require.NotContains(t, string(encoded), "secret.example")
		require.NotContains(t, fields, "prompt")
		require.NotContains(t, fields, "body")
		require.NotContains(t, fields, "image_url")
	}

	require.Equal(t, []int64{1, 2}, upstream.calls())
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Equal(t, "upstream_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
	require.Equal(t, "Upstream request failed", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())

	rawEvents, ok := c.Get(service.OpsUpstreamErrorsKey)
	require.True(t, ok)
	events, ok := rawEvents.([]*service.OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.Len(t, events, 2)
	require.Equal(t, "failover", events[0].Kind)
	require.Equal(t, "failover", events[1].Kind)
}

func TestOpenAIGatewayHandlerImages_Canceled520DoesNotReplayOrReportExhausted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	groupID := int64(3132)
	accounts := []service.Account{
		{ID: 1, Name: "image-account-1", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Priority: 0, Credentials: map[string]any{"access_token": "token-1", "chatgpt_account_id": "chatgpt-1"}},
		{ID: 2, Name: "image-account-2", Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true, Priority: 1, Credentials: map[string]any{"access_token": "token-2", "chatgpt_account_id": "chatgpt-2"}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	upstream := &openAICanceled520Upstream{cancel: cancel}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	gatewayService := service.NewOpenAIGatewayService(
		openAIImagesFailoverAccountRepo{accounts: accounts}, nil, nil, nil, nil, nil, nil, cfg,
		nil, nil, nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil,
		nil, nil, // usageRecordWorkerPool
	)
	billingService := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingService.Stop)
	handler := NewOpenAIGatewayHandler(
		gatewayService, service.NewConcurrencyService(nil), billingService,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, cfg,
	)
	handler.maxAccountSwitches = 10

	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader([]byte(`{"model":"gpt-image-2","prompt":"draw"}`))).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 99, GroupID: &groupID, Group: &service.Group{ID: groupID, AllowImageGeneration: true}, User: &service.User{ID: 100}})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100})

	handler.Images(c)

	require.Equal(t, []int64{1}, upstream.calls())
	require.Equal(t, statusClientClosedRequest, c.Writer.Status())
	require.Zero(t, rec.Body.Len(), "cancellation must not emit an exhausted 502")
	_, exhausted := c.Get(service.OpsUpstreamStatusCodeKey)
	require.False(t, exhausted)
	rawEvents, ok := c.Get(service.OpsUpstreamErrorsKey)
	require.True(t, ok)
	events, ok := rawEvents.([]*service.OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.Len(t, events, 1)
	require.Equal(t, "failover", events[0].Kind)
	require.Equal(t, 520, events[0].UpstreamStatusCode)
}

func TestOpenAIGatewayHandlerImages_CancellationAroundAccountAcquisitionStopsBeforeForward(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name             string
		cancelAtAcquire  int
		wantAcquireCalls int
	}{
		{name: "successful selection", cancelAtAcquire: 1, wantAcquireCalls: 1},
		{name: "waited acquisition", cancelAtAcquire: 2, wantAcquireCalls: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			groupID := int64(3133)
			ctx, cancel := context.WithCancel(context.Background())
			cache := &cancelOnAccountAcquireCache{cancel: cancel, cancelAt: tc.cancelAtAcquire}
			upstream := &openAIImagesFailoverHTTPUpstream{}
			account := service.Account{
				ID: 1, Name: "image-account", Platform: service.PlatformOpenAI,
				Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true,
				Concurrency: 1, Credentials: map[string]any{"access_token": "token", "chatgpt_account_id": "chatgpt"},
			}
			cfg := &config.Config{RunMode: config.RunModeSimple}
			concurrencyService := service.NewConcurrencyService(cache)
			gatewayService := service.NewOpenAIGatewayService(
				openAIImagesFailoverAccountRepo{accounts: []service.Account{account}}, nil, nil, nil, nil, nil, nil, cfg,
				nil, concurrencyService, nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil,
				nil, nil, // usageRecordWorkerPool
			)
			billingService := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
			t.Cleanup(billingService.Stop)
			handler := NewOpenAIGatewayHandler(
				gatewayService, concurrencyService, billingService,
				service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, cfg,
			)

			req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader([]byte(`{"model":"gpt-image-2","prompt":"draw"}`))).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = req
			c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 99, GroupID: &groupID, Group: &service.Group{ID: groupID, AllowImageGeneration: true}, User: &service.User{ID: 100}})
			c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100})

			handler.Images(c)

			acquires, releases := cache.snapshot()
			require.Equal(t, tc.wantAcquireCalls, acquires)
			require.Equal(t, 1, releases, "the acquired account slot must be released exactly once")
			require.Empty(t, upstream.calls(), "cancellation must stop before Forward")
			_, attributed := c.Get(opsAccountIDKey)
			require.False(t, attributed, "a canceled, unforwarded selection must not become ops attribution")
			require.Equal(t, statusClientClosedRequest, c.Writer.Status())
		})
	}
}

func TestOpenAIGatewayHandlerImages_CancellationAtServiceAdmissionIsClean(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for i := 0; i < 25; i++ {
		groupID := int64(3134)
		ctx, cancel := context.WithCancel(context.Background())
		ctx = service.WithHTTPAttemptAdmissionHook(ctx, cancel)
		cache := &cancelOnAccountAcquireCache{cancelAt: 0}
		upstream := &openAIImagesFailoverHTTPUpstream{}
		account := service.Account{
			ID: 1, Name: "image-account", Platform: service.PlatformOpenAI,
			Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true,
			Concurrency: 1, Credentials: map[string]any{"access_token": "token", "chatgpt_account_id": "chatgpt"},
		}
		cfg := &config.Config{RunMode: config.RunModeSimple}
		concurrencyService := service.NewConcurrencyService(cache)
		gatewayService := service.NewOpenAIGatewayService(
			openAIImagesFailoverAccountRepo{accounts: []service.Account{account}}, nil, nil, nil, nil, nil, nil, cfg,
			nil, concurrencyService, nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil,
			nil, nil, // usageRecordWorkerPool
		)
		billingService := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
		handler := NewOpenAIGatewayHandler(
			gatewayService, concurrencyService, billingService,
			service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg), nil, nil, nil, cfg,
		)

		req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader([]byte(`{"model":"gpt-image-2","prompt":"draw"}`))).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = req
		c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{ID: 99, GroupID: &groupID, Group: &service.Group{ID: groupID, AllowImageGeneration: true}, User: &service.User{ID: 100}})
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 100})

		handler.Images(c)
		billingService.Stop()

		acquires, releases := cache.snapshot()
		require.Equal(t, 1, acquires)
		require.Equal(t, 1, releases, "iteration %d: slot release must be exact", i)
		require.Empty(t, upstream.calls(), "iteration %d: rejected admission must make no upstream request", i)
		require.Equal(t, statusClientClosedRequest, c.Writer.Status())
		require.Zero(t, rec.Body.Len())
		_, attributed := c.Get(opsAccountIDKey)
		require.False(t, attributed)
		_, upstreamErrors := c.Get(service.OpsUpstreamErrorsKey)
		require.False(t, upstreamErrors)
		_, capacityLimited := c.Get(opsRoutingCapacityLimitedKey)
		require.False(t, capacityLimited)
	}
}

func TestOpenAIGatewayHandlerImages_IncompleteFailoverBillsOnlyFinalSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	groupID := int64(3131)
	accounts := []service.Account{
		{
			ID:          11,
			Name:        "image-account-incomplete",
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeOAuth,
			Status:      service.StatusActive,
			Schedulable: true,
			Concurrency: 0,
			Priority:    0,
			Credentials: map[string]any{"access_token": "token-1", "chatgpt_account_id": "chatgpt-1"},
		},
		{
			ID:          12,
			Name:        "image-account-success",
			Platform:    service.PlatformOpenAI,
			Type:        service.AccountTypeOAuth,
			Status:      service.StatusActive,
			Schedulable: true,
			Concurrency: 0,
			Priority:    1,
			Credentials: map[string]any{"access_token": "token-2", "chatgpt_account_id": "chatgpt-2"},
		},
	}
	accountRepo := openAIImagesFailoverAccountRepo{accounts: accounts}
	usageRepo := &openAIImagesFailoverUsageRepo{}
	upstream := &openAIImagesSequenceHTTPUpstream{
		responses: []*http.Response{
			{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"text/event-stream"},
					"X-Request-Id": []string{"req_img_no_completion"},
				},
				Body: io.NopCloser(bytes.NewBufferString(
					"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"ig_no_completion\",\"type\":\"image_generation_call\",\"result\":\"aW5jb21wbGV0ZS1mYWxsYmFjaw==\",\"revised_prompt\":\"draw a cat\",\"output_format\":\"png\"}}\n\n",
				)),
			},
			{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"text/event-stream"},
					"X-Request-Id": []string{"req_img_success"},
				},
				Body: io.NopCloser(bytes.NewBufferString(
					"data: {\"type\":\"response.completed\",\"response\":{\"created_at\":1710000030,\"tool_usage\":{\"image_gen\":{\"images\":1}},\"output\":[{\"type\":\"image_generation_call\",\"result\":\"Zmlyc3Qtc3VjY2Vzcw==\",\"output_format\":\"png\"}]}}\n\n",
				)),
			},
		},
	}
	cfg := &config.Config{RunMode: config.RunModeSimple}
	coreBillingService := service.NewBillingService(cfg, nil)
	gatewayService := service.NewOpenAIGatewayService(
		accountRepo,
		usageRepo,
		nil,
		nil,
		nil,
		nil,
		nil,
		cfg,
		nil,
		nil,
		coreBillingService,
		nil,
		nil,
		upstream,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil, nil, // usageRecordWorkerPool
	)
	billingService := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingService.Stop)
	concurrencyService := service.NewConcurrencyService(nil)
	handler := NewOpenAIGatewayHandler(
		gatewayService,
		concurrencyService,
		billingService,
		service.NewAPIKeyService(nil, nil, nil, nil, nil, nil, cfg),
		nil,
		nil,
		nil,
		cfg,
	)
	handler.maxAccountSwitches = 10

	body := []byte(`{"model":"gpt-image-2","prompt":"draw a cat"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		ID:      199,
		GroupID: &groupID,
		Group: &service.Group{
			ID:                   groupID,
			AllowImageGeneration: true,
		},
		User: &service.User{ID: 200},
	})
	c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 200, Concurrency: 0})

	handler.Images(c)

	require.Equal(t, []int64{11, 12}, upstream.calls())
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "Zmlyc3Qtc3VjY2Vzcw==", gjson.GetBytes(rec.Body.Bytes(), "data.0.b64_json").String())

	calls, lastLog := usageRepo.snapshot()
	require.Equal(t, 1, calls)
	require.NotNil(t, lastLog)
	require.NotNil(t, lastLog.AccountID)
	require.Equal(t, int64(12), *lastLog.AccountID)
	require.Equal(t, 1, lastLog.ImageCount)
	require.Equal(t, "req_img_success", lastLog.RequestID)
}
