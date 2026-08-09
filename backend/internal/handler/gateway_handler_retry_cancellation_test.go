package handler

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type retryCancellationSchedulerCache struct {
	accounts []*service.Account
}

func (f *retryCancellationSchedulerCache) GetSnapshot(context.Context, service.SchedulerBucket) ([]*service.Account, bool, error) {
	return f.accounts, true, nil
}
func (f *retryCancellationSchedulerCache) SetSnapshot(context.Context, service.SchedulerBucket, []service.Account) error {
	return nil
}
func (f *retryCancellationSchedulerCache) GetAccount(_ context.Context, accountID int64) (*service.Account, error) {
	for _, account := range f.accounts {
		if account != nil && account.ID == accountID {
			return account, nil
		}
	}
	return nil, nil
}
func (f *retryCancellationSchedulerCache) SetAccount(context.Context, *service.Account) error {
	return nil
}
func (f *retryCancellationSchedulerCache) DeleteAccount(context.Context, int64) error { return nil }
func (f *retryCancellationSchedulerCache) UpdateLastUsed(context.Context, map[int64]time.Time) error {
	return nil
}
func (f *retryCancellationSchedulerCache) TryLockBucket(context.Context, service.SchedulerBucket, time.Duration) (string, bool, error) {
	return "test-lock", true, nil
}
func (f *retryCancellationSchedulerCache) UnlockBucket(context.Context, service.SchedulerBucket, string) error {
	return nil
}
func (f *retryCancellationSchedulerCache) ListBuckets(context.Context) ([]service.SchedulerBucket, error) {
	return nil, nil
}
func (f *retryCancellationSchedulerCache) GetOutboxWatermark(context.Context) (int64, error) {
	return 0, nil
}
func (f *retryCancellationSchedulerCache) SetOutboxWatermark(context.Context, int64) error {
	return nil
}

type retryCancellationConcurrencyCache struct {
	accountAcquires atomic.Int32
	accountReleases atomic.Int32
	keyAcquires     atomic.Int32
	keyReleases     atomic.Int32
	userAcquires    atomic.Int32
	userReleases    atomic.Int32
}

func (f *retryCancellationConcurrencyCache) AcquireAccountSlot(context.Context, int64, int, string) (bool, error) {
	f.accountAcquires.Add(1)
	return true, nil
}
func (f *retryCancellationConcurrencyCache) ReleaseAccountSlot(context.Context, int64, string) error {
	f.accountReleases.Add(1)
	return nil
}
func (f *retryCancellationConcurrencyCache) GetAccountConcurrency(context.Context, int64) (int, error) {
	return 0, nil
}
func (f *retryCancellationConcurrencyCache) GetAccountConcurrencyBatch(context.Context, []int64) (map[int64]int, error) {
	return map[int64]int{}, nil
}
func (f *retryCancellationConcurrencyCache) IncrementAccountWaitCount(context.Context, int64, int) (bool, error) {
	return true, nil
}
func (f *retryCancellationConcurrencyCache) DecrementAccountWaitCount(context.Context, int64) error {
	return nil
}
func (f *retryCancellationConcurrencyCache) GetAccountWaitingCount(context.Context, int64) (int, error) {
	return 0, nil
}
func (f *retryCancellationConcurrencyCache) AcquireAPIKeySlot(context.Context, int64, int, string) (bool, error) {
	f.keyAcquires.Add(1)
	return true, nil
}
func (f *retryCancellationConcurrencyCache) ReleaseAPIKeySlot(context.Context, int64, string) error {
	f.keyReleases.Add(1)
	return nil
}
func (f *retryCancellationConcurrencyCache) GetAPIKeyConcurrency(context.Context, int64) (int, error) {
	return 0, nil
}
func (f *retryCancellationConcurrencyCache) GetAPIKeyConcurrencyBatch(context.Context, []int64) (map[int64]int, error) {
	return map[int64]int{}, nil
}
func (f *retryCancellationConcurrencyCache) AcquireUserSlot(context.Context, int64, int, string) (bool, error) {
	f.userAcquires.Add(1)
	return true, nil
}
func (f *retryCancellationConcurrencyCache) ReleaseUserSlot(context.Context, int64, string) error {
	f.userReleases.Add(1)
	return nil
}
func (f *retryCancellationConcurrencyCache) GetUserConcurrency(context.Context, int64) (int, error) {
	return 0, nil
}
func (f *retryCancellationConcurrencyCache) IncrementWaitCount(context.Context, int64, int) (bool, error) {
	return true, nil
}
func (f *retryCancellationConcurrencyCache) DecrementWaitCount(context.Context, int64) error {
	return nil
}
func (f *retryCancellationConcurrencyCache) GetAccountsLoadBatch(context.Context, []service.AccountWithConcurrency) (map[int64]*service.AccountLoadInfo, error) {
	return map[int64]*service.AccountLoadInfo{}, nil
}
func (f *retryCancellationConcurrencyCache) GetUsersLoadBatch(context.Context, []service.UserWithConcurrency) (map[int64]*service.UserLoadInfo, error) {
	return map[int64]*service.UserLoadInfo{}, nil
}
func (f *retryCancellationConcurrencyCache) CleanupExpiredAccountSlots(context.Context, int64) error {
	return nil
}
func (f *retryCancellationConcurrencyCache) CleanupExpiredAccountSlotKeys(context.Context) error {
	return nil
}
func (f *retryCancellationConcurrencyCache) CleanupStaleProcessSlots(context.Context, string) error {
	return nil
}

type retryCancellationHTTPUpstream struct {
	cancel context.CancelFunc
	calls  atomic.Int32
}

func (u *retryCancellationHTTPUpstream) Do(*http.Request, string, int64, int) (*http.Response, error) {
	call := u.calls.Add(1)
	if call == 1 {
		return &http.Response{
			StatusCode: 529,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"completed overload"}}`)),
		}, nil
	}
	u.cancel()
	return nil, errors.New("transport failure")
}

func (u *retryCancellationHTTPUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

func TestGatewayHandlerMessages_AdmittedRetryCancellationDoesNotWriteFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(9201)
	group := &service.Group{
		ID:       groupID,
		Hydrated: true,
		Platform: service.PlatformAnthropic,
		Status:   service.StatusActive,
	}
	account := &service.Account{
		ID:          9202,
		Name:        "retry-cancellation",
		Platform:    service.PlatformAnthropic,
		Type:        service.AccountTypeAPIKey,
		Concurrency: 1,
		Priority:    1,
		Status:      service.StatusActive,
		Schedulable: true,
		Credentials: map[string]any{
			"api_key":                    "test-key",
			"base_url":                   "https://example.com",
			"custom_error_codes_enabled": true,
			"custom_error_codes":         []any{float64(http.StatusBadRequest)},
		},
		AccountGroups: []service.AccountGroup{{AccountID: 9202, GroupID: groupID}},
	}

	requestCtx, cancel := context.WithCancel(context.Background())
	upstream := &retryCancellationHTTPUpstream{cancel: cancel}
	concurrencyCache := &retryCancellationConcurrencyCache{}
	concurrencyService := service.NewConcurrencyService(concurrencyCache)
	schedulerSnapshot := service.NewSchedulerSnapshotService(
		&retryCancellationSchedulerCache{accounts: []*service.Account{account}},
		nil,
		nil,
		nil,
		nil,
	)
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Security.URLAllowlist.Enabled = false
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
		nil,
		&service.RateLimitService{},
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
		nil,
		nil,
		nil,
		nil,
		nil, nil, // usageRecordWorkerPool
	)
	billingCacheService := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	defer billingCacheService.Stop()
	h := &GatewayHandler{
		gatewayService:      gatewayService,
		billingCacheService: billingCacheService,
		concurrencyHelper:   NewConcurrencyHelper(concurrencyService, SSEPingFormatClaude, 0),
		maxAccountSwitches:  1,
		cfg:                 cfg,
	}

	body := []byte(`{"model":"claude-3-5-sonnet-latest","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body)).WithContext(requestCtx)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), ctxkey.Group, group))
	c.Request = req
	apiKey := &service.APIKey{
		ID:          9203,
		UserID:      9204,
		Concurrency: 1,
		GroupID:     &groupID,
		Status:      service.StatusActive,
		User: &service.User{
			ID:          9204,
			Concurrency: 1,
			Balance:     100,
		},
		Group: group,
	}
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID, Concurrency: 1})

	h.Messages(c)

	require.Equal(t, int32(2), upstream.calls.Load())
	require.Equal(t, statusClientClosedRequest, c.Writer.Status())
	require.NotContains(t, recorder.Body.String(), "Upstream request failed")
	require.Empty(t, recorder.Body.String())
	require.Eventually(t, func() bool {
		return concurrencyCache.accountAcquires.Load() == concurrencyCache.accountReleases.Load() &&
			concurrencyCache.userAcquires.Load() == concurrencyCache.userReleases.Load() &&
			concurrencyCache.keyAcquires.Load() == concurrencyCache.keyReleases.Load()
	}, time.Second, 10*time.Millisecond)
	require.Equal(t, int32(1), concurrencyCache.keyAcquires.Load(), "key lease must remain logical across failover")
	require.Equal(t, int32(1), concurrencyCache.userAcquires.Load(), "user lease must remain logical across failover")
	require.Equal(t, int32(1), concurrencyCache.accountAcquires.Load(), "same-account service retry keeps its admitted account attempt")
}
