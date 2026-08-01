//go:build unit

package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// testConfig 返回一个用于测试的默认配置
func testConfig() *config.Config {
	return &config.Config{RunMode: config.RunModeStandard}
}

// mockAccountRepoForPlatform 单平台测试用的 mock
type mockAccountRepoForPlatform struct {
	accounts                        []Account
	accountsByID                    map[int64]*Account
	listPlatformFunc                func(ctx context.Context, platform string) ([]Account, error)
	listModelAvailabilityCandidates func(ctx context.Context, groupID *int64, platforms []string, includeGrouped bool) ([]Account, error)
	getByIDCalls                    int
	setErrorCalls                   int
	setSchedulableCalls             int
}

func (m *mockAccountRepoForPlatform) GetByID(ctx context.Context, id int64) (*Account, error) {
	m.getByIDCalls++
	if acc, ok := m.accountsByID[id]; ok {
		return acc, nil
	}
	return nil, errors.New("account not found")
}

func (m *mockAccountRepoForPlatform) GetByIDs(ctx context.Context, ids []int64) ([]*Account, error) {
	var result []*Account
	for _, id := range ids {
		if acc, ok := m.accountsByID[id]; ok {
			result = append(result, acc)
		}
	}
	return result, nil
}

func (m *mockAccountRepoForPlatform) ExistsByID(ctx context.Context, id int64) (bool, error) {
	if m.accountsByID == nil {
		return false, nil
	}
	_, ok := m.accountsByID[id]
	return ok, nil
}

func (m *mockAccountRepoForPlatform) ListSchedulableByPlatform(ctx context.Context, platform string) ([]Account, error) {
	if m.listPlatformFunc != nil {
		return m.listPlatformFunc(ctx, platform)
	}
	var result []Account
	for _, acc := range m.accounts {
		if acc.Platform == platform && acc.IsSchedulable() {
			result = append(result, acc)
		}
	}
	return result, nil
}

func (m *mockAccountRepoForPlatform) ListSchedulableByGroupIDAndPlatform(ctx context.Context, groupID int64, platform string) ([]Account, error) {
	return m.ListSchedulableByPlatform(ctx, platform)
}

func (m *mockAccountRepoForPlatform) ListModelAvailabilityCandidates(ctx context.Context, groupID *int64, platforms []string, includeGrouped bool) ([]Account, error) {
	if m.listModelAvailabilityCandidates != nil {
		return m.listModelAvailabilityCandidates(ctx, groupID, platforms, includeGrouped)
	}
	allowed := make(map[string]struct{}, len(platforms))
	for _, platform := range platforms {
		allowed[platform] = struct{}{}
	}
	result := make([]Account, 0, len(m.accounts))
	for _, account := range m.accounts {
		if account.Status != StatusActive || !account.Schedulable {
			continue
		}
		if _, ok := allowed[account.Platform]; !ok {
			continue
		}
		result = append(result, account)
	}
	return result, nil
}

// Stub methods to implement AccountRepository interface
func (m *mockAccountRepoForPlatform) Create(ctx context.Context, account *Account) error {
	return nil
}
func (m *mockAccountRepoForPlatform) GetByCRSAccountID(ctx context.Context, crsAccountID string) (*Account, error) {
	return nil, nil
}

func (m *mockAccountRepoForPlatform) FindByExtraField(ctx context.Context, key string, value any) ([]Account, error) {
	return nil, nil
}

func (m *mockAccountRepoForPlatform) ListCRSAccountIDs(ctx context.Context) (map[string]int64, error) {
	return nil, nil
}
func (m *mockAccountRepoForPlatform) Update(ctx context.Context, account *Account) error {
	return nil
}
func (m *mockAccountRepoForPlatform) Delete(ctx context.Context, id int64) error { return nil }
func (m *mockAccountRepoForPlatform) List(ctx context.Context, params pagination.PaginationParams) ([]Account, *pagination.PaginationResult, error) {
	return nil, nil, nil
}
func (m *mockAccountRepoForPlatform) ListWithFilters(_ context.Context, _ pagination.PaginationParams, _ AccountListFilters) ([]Account, *pagination.PaginationResult, error) {
	return nil, nil, nil
}
func (m *mockAccountRepoForPlatform) ListByGroup(ctx context.Context, groupID int64) ([]Account, error) {
	return nil, nil
}
func (m *mockAccountRepoForPlatform) ListActive(ctx context.Context) ([]Account, error) {
	return nil, nil
}
func (m *mockAccountRepoForPlatform) ListOAuthRefreshCandidates(ctx context.Context) ([]Account, error) {
	return nil, nil
}
func (m *mockAccountRepoForPlatform) ListByPlatform(ctx context.Context, platform string) ([]Account, error) {
	return nil, nil
}
func (m *mockAccountRepoForPlatform) ListByPlatformForValidation(ctx context.Context, platform string) ([]Account, error) {
	return nil, nil
}
func (m *mockAccountRepoForPlatform) UpdateLastUsed(ctx context.Context, id int64) error {
	return nil
}
func (m *mockAccountRepoForPlatform) BatchUpdateLastUsed(ctx context.Context, updates map[int64]time.Time) error {
	return nil
}
func (m *mockAccountRepoForPlatform) SetError(ctx context.Context, id int64, errorMsg string) error {
	m.setErrorCalls++
	return nil
}
func (m *mockAccountRepoForPlatform) ClearError(ctx context.Context, id int64) error {
	return nil
}
func (m *mockAccountRepoForPlatform) SetSchedulable(ctx context.Context, id int64, schedulable bool) error {
	m.setSchedulableCalls++
	return nil
}
func (m *mockAccountRepoForPlatform) AutoPauseExpiredAccounts(ctx context.Context, now time.Time) (int64, error) {
	return 0, nil
}
func (m *mockAccountRepoForPlatform) BindGroups(ctx context.Context, accountID int64, groupIDs []int64) error {
	return nil
}
func (m *mockAccountRepoForPlatform) ListSchedulable(ctx context.Context) ([]Account, error) {
	return nil, nil
}
func (m *mockAccountRepoForPlatform) ListSchedulableByGroupID(ctx context.Context, groupID int64) ([]Account, error) {
	return nil, nil
}
func (m *mockAccountRepoForPlatform) ListSchedulableByPlatforms(ctx context.Context, platforms []string) ([]Account, error) {
	var result []Account
	platformSet := make(map[string]bool)
	for _, p := range platforms {
		platformSet[p] = true
	}
	for _, acc := range m.accounts {
		if platformSet[acc.Platform] && acc.IsSchedulable() {
			result = append(result, acc)
		}
	}
	return result, nil
}
func (m *mockAccountRepoForPlatform) ListSchedulableByGroupIDAndPlatforms(ctx context.Context, groupID int64, platforms []string) ([]Account, error) {
	return m.ListSchedulableByPlatforms(ctx, platforms)
}
func (m *mockAccountRepoForPlatform) ListSchedulableUngroupedByPlatform(ctx context.Context, platform string) ([]Account, error) {
	return m.ListSchedulableByPlatform(ctx, platform)
}
func (m *mockAccountRepoForPlatform) ListSchedulableUngroupedByPlatforms(ctx context.Context, platforms []string) ([]Account, error) {
	return m.ListSchedulableByPlatforms(ctx, platforms)
}
func (m *mockAccountRepoForPlatform) SetRateLimited(ctx context.Context, id int64, resetAt time.Time) error {
	return nil
}
func (m *mockAccountRepoForPlatform) SetModelRateLimit(ctx context.Context, id int64, scope string, resetAt time.Time, reason ...string) error {
	return nil
}
func (m *mockAccountRepoForPlatform) SetOverloaded(ctx context.Context, id int64, until time.Time) error {
	return nil
}
func (m *mockAccountRepoForPlatform) SetTempUnschedulable(ctx context.Context, id int64, until time.Time, reason string) error {
	return nil
}
func (m *mockAccountRepoForPlatform) ClearTempUnschedulable(ctx context.Context, id int64) error {
	return nil
}
func (m *mockAccountRepoForPlatform) ClearRateLimit(ctx context.Context, id int64) error {
	return nil
}
func (m *mockAccountRepoForPlatform) ClearAntigravityQuotaScopes(ctx context.Context, id int64) error {
	return nil
}
func (m *mockAccountRepoForPlatform) ClearModelRateLimits(ctx context.Context, id int64) error {
	return nil
}
func (m *mockAccountRepoForPlatform) UpdateSessionWindow(ctx context.Context, id int64, start, end *time.Time, status string) error {
	return nil
}
func (m *mockAccountRepoForPlatform) UpdateExtra(ctx context.Context, id int64, updates map[string]any) error {
	return nil
}
func (m *mockAccountRepoForPlatform) BulkUpdate(ctx context.Context, ids []int64, updates AccountBulkUpdate) (int64, error) {
	return 0, nil
}

func (m *mockAccountRepoForPlatform) IncrementQuotaUsed(ctx context.Context, id int64, amount float64) error {
	return nil
}

func (m *mockAccountRepoForPlatform) ResetQuotaUsed(ctx context.Context, id int64) error {
	return nil
}

// Verify interface implementation
var _ AccountRepository = (*mockAccountRepoForPlatform)(nil)

func TestGatewayTruncateForLogSanitizesPrivateUpstreamBody(t *testing.T) {
	body := []byte(
		`{"error":{"message":"token=do-not-leak https://internal.example/secret"},` +
			`"metadata":{"authorization":"Bearer secret","request_body":"private prompt"}}`,
	)

	got := truncateForLog(body, 4096)

	require.NotContains(t, got, "do-not-leak")
	require.NotContains(t, got, "Bearer secret")
	require.NotContains(t, got, "internal.example")
	require.NotContains(t, got, "private prompt")
	require.LessOrEqual(t, len(got), 4096)
}

func TestGatewayForward_HTTPFailoverCarriersRetainParsedFact(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newAttempt := func(requestID string) *http.Response {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Header:     http.Header{"X-Request-Id": []string{requestID}},
			Body: io.NopCloser(bytes.NewReader([]byte(
				`{"error":{"type":"vendor_failure","message":"token=do-not-leak https://internal.example/secret"},` +
					`"metadata":{"authorization":"Bearer secret"}}`,
			))),
		}
	}

	for _, test := range []struct {
		name              string
		account           *Account
		responses         []*http.Response
		expectedCalls     int
		expectedRequestID string
	}{
		{
			name: "immediate failover",
			account: &Account{
				ID: 910, Platform: PlatformAnthropic, Type: AccountTypeAPIKey,
				Credentials: map[string]any{
					"api_key":                    "test-key",
					"custom_error_codes_enabled": true,
					"custom_error_codes":         []any{float64(http.StatusServiceUnavailable)},
				},
			},
			responses:         []*http.Response{newAttempt("req-immediate")},
			expectedCalls:     1,
			expectedRequestID: "req-immediate",
		},
		{
			name: "retry exhausted failover",
			account: &Account{
				ID: 911, Platform: PlatformAnthropic, Type: AccountTypeAPIKey,
				Credentials: map[string]any{
					"api_key":                    "test-key",
					"custom_error_codes_enabled": true,
					"custom_error_codes":         []any{float64(http.StatusUnauthorized)},
				},
			},
			responses: []*http.Response{
				newAttempt("req-retry-1"),
				newAttempt("req-retry-2"),
				newAttempt("req-retry-3"),
				newAttempt("req-retry-4"),
				newAttempt("req-retry-5"),
			},
			expectedCalls:     maxRetryAttempts,
			expectedRequestID: "req-retry-5",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			accountRepo := &mockAccountRepoForPlatform{accountsByID: map[int64]*Account{test.account.ID: test.account}}
			upstream := &queuedHTTPUpstream{responses: test.responses}
			cfg := testConfig()
			cfg.Gateway.LogUpstreamErrorBody = true
			cfg.Gateway.LogUpstreamErrorBodyMaxBytes = 4096
			svc := &GatewayService{
				cfg:              cfg,
				httpUpstream:     upstream,
				rateLimitService: NewRateLimitService(accountRepo, nil, testConfig(), nil, nil),
			}
			parsed, err := ParseGatewayRequest(NewRequestBodyRef([]byte(`{"model":"claude-test","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)), PlatformAnthropic)
			require.NoError(t, err)

			_, err = svc.Forward(context.Background(), c, test.account, parsed)

			var failoverErr *UpstreamFailoverError
			require.ErrorAs(t, err, &failoverErr)
			fact, ok := failoverErr.UpstreamFact()
			require.True(t, ok)
			require.Equal(t, PlatformAnthropic, fact.Provider)
			require.Equal(t, http.StatusServiceUnavailable, fact.HTTPStatus)
			require.Equal(t, "vendor_failure", fact.ProviderType)
			require.Equal(t, test.expectedRequestID, fact.RequestID)
			require.Len(t, upstream.requests, test.expectedCalls)
			rawEvents, exists := c.Get(OpsUpstreamErrorsKey)
			require.True(t, exists)
			events := rawEvents.([]*OpsUpstreamErrorEvent)
			require.NotEmpty(t, events)
			for _, event := range events {
				require.NotContains(t, event.Detail, "do-not-leak")
				require.NotContains(t, event.Detail, "internal.example")
				require.NotContains(t, event.Detail, "Bearer secret")
			}
		})
	}
}

func TestGatewayHandleErrorResponse_DisablingCarrierRetainsParsedFact(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	account := &Account{
		ID: 912, Platform: PlatformAnthropic, Type: AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":                    "test-key",
			"custom_error_codes_enabled": true,
			"custom_error_codes":         []any{float64(http.StatusPaymentRequired)},
		},
	}
	accountRepo := &mockAccountRepoForPlatform{accountsByID: map[int64]*Account{account.ID: account}}
	cfg := testConfig()
	cfg.Gateway.LogUpstreamErrorBody = true
	cfg.Gateway.LogUpstreamErrorBodyMaxBytes = 4096
	svc := &GatewayService{
		cfg:              cfg,
		rateLimitService: NewRateLimitService(accountRepo, nil, testConfig(), nil, nil),
	}
	resp := &http.Response{
		StatusCode: http.StatusPaymentRequired,
		Header:     http.Header{"X-Request-Id": []string{"req-disable"}},
		Body: io.NopCloser(bytes.NewReader([]byte(
			`{"error":{"type":"billing_error","code":"billing_required","message":"token=do-not-leak https://internal.example/secret"},` +
				`"metadata":{"authorization":"Bearer secret"}}`,
		))),
	}

	_, err := svc.handleErrorResponse(context.Background(), resp, c, account)

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	fact, ok := failoverErr.UpstreamFact()
	require.True(t, ok)
	require.Equal(t, PlatformAnthropic, fact.Provider)
	require.Equal(t, http.StatusPaymentRequired, fact.HTTPStatus)
	require.Equal(t, "billing_required", fact.ProviderCode)
	require.Equal(t, "billing_error", fact.ProviderType)
	require.Equal(t, "req-disable", fact.RequestID)
	rawEvents, exists := c.Get(OpsUpstreamErrorsKey)
	require.True(t, exists)
	events := rawEvents.([]*OpsUpstreamErrorEvent)
	require.Len(t, events, 1)
	require.NotContains(t, events[0].Detail, "do-not-leak")
	require.NotContains(t, events[0].Detail, "internal.example")
	require.NotContains(t, events[0].Detail, "Bearer secret")
}

func TestGatewayForward_RecognizedHTTPErrorBeforeFailoverHealthAndRules(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	conflictingCode := http.StatusTeapot
	conflictingMessage := "database must not win"
	rules := &ErrorPassthroughService{}
	rules.setLocalCache([]*model.ErrorPassthroughRule{{
		Enabled: true, Priority: 1, Platforms: []string{PlatformAnthropic},
		Keywords: []string{"cyber_policy"}, MatchMode: model.MatchModeAny,
		ResponseCode: &conflictingCode, CustomMessage: &conflictingMessage, SkipMonitoring: true,
	}})
	BindErrorPassthroughService(c, rules)

	account := &Account{
		ID: 901, Platform: PlatformAnthropic, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "test-key"},
	}
	accountRepo := &mockAccountRepoForPlatform{accountsByID: map[int64]*Account{account.ID: account}}
	rateLimits := NewRateLimitService(accountRepo, nil, testConfig(), nil, nil)
	upstream := &queuedHTTPUpstream{responses: []*http.Response{{
		StatusCode: http.StatusUnauthorized,
		Header:     http.Header{"x-request-id": []string{"req-policy"}},
		Body: io.NopCloser(bytes.NewReader([]byte(
			`{"error":{"code":"cyber_policy","type":"invalid_request_error","message":"policy rejected"},` +
				`"metadata":{"authorization":"Bearer secret","token":"do-not-leak","private_url":"https://internal.example/secret"}}`,
		))),
	}}}
	cfg := testConfig()
	cfg.Gateway.LogUpstreamErrorBody = true
	cfg.Gateway.LogUpstreamErrorBodyMaxBytes = 4096
	svc := &GatewayService{cfg: cfg, httpUpstream: upstream, rateLimitService: rateLimits}
	parsed, err := ParseGatewayRequest(NewRequestBodyRef([]byte(`{"model":"claude-test","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)), PlatformAnthropic)
	require.NoError(t, err)

	_, err = svc.Forward(context.Background(), c, account, parsed)

	require.Error(t, err)
	require.NotErrorAs(t, err, new(*UpstreamFailoverError))
	require.Len(t, upstream.requests, 1)
	require.Zero(t, accountRepo.setErrorCalls)
	require.Zero(t, accountRepo.setSchedulableCalls)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "invalid_request_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
	require.Equal(t, "policy rejected", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
	require.NotContains(t, rec.Body.String(), conflictingMessage)
	_, skip := c.Get(OpsSkipPassthroughKey)
	require.False(t, skip)
	rawEvents, exists := c.Get(OpsUpstreamErrorsKey)
	require.True(t, exists)
	events := rawEvents.([]*OpsUpstreamErrorEvent)
	require.Len(t, events, 1)
	require.NotNil(t, events[0].UpstreamFact)
	require.Equal(t, "cyber_policy", events[0].UpstreamFact.ProviderCode)
	require.NotContains(t, events[0].Detail, "do-not-leak")
	require.NotContains(t, events[0].Detail, "internal.example")
	require.NotContains(t, events[0].Detail, "Bearer secret")
	require.True(t, IsResponseCommitted(c))
}

func TestGatewayForward_SignatureRetryRecognizedErrorBeforeFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	account := &Account{
		ID: 902, Platform: PlatformAnthropic, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"api_key": "test-key"},
	}
	accountRepo := &mockAccountRepoForPlatform{accountsByID: map[int64]*Account{account.ID: account}}
	rateLimits := NewRateLimitService(accountRepo, nil, testConfig(), nil, nil)
	settings := NewSettingService(&gatewayTTLSettingRepo{data: map[string]string{
		SettingKeyRectifierSettings: `{"enabled":true,"thinking_signature_enabled":true,"thinking_budget_enabled":true,"apikey_signature_enabled":true}`,
	}}, testConfig())
	upstream := &queuedHTTPUpstream{responses: []*http.Response{
		newJSONResponse(http.StatusBadRequest, `{"error":{"type":"invalid_request_error","message":"Invalid signature in thinking block"}}`),
		newJSONResponse(http.StatusUnauthorized, `{"error":{"code":"cyber_policy","type":"invalid_request_error","message":"policy rejected"}}`),
	}}
	svc := &GatewayService{
		cfg: testConfig(), httpUpstream: upstream, rateLimitService: rateLimits,
		settingService: settings,
	}
	parsed, err := ParseGatewayRequest(NewRequestBodyRef([]byte(`{"model":"claude-3-5-sonnet-latest","max_tokens":1024,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"x","signature":"bad"}]},{"role":"user","content":"hi"}]}`)), PlatformAnthropic)
	require.NoError(t, err)

	result, err := svc.Forward(context.Background(), c, account, parsed)

	require.Nil(t, result)
	require.Error(t, err)
	require.NotErrorAs(t, err, new(*UpstreamFailoverError))
	require.Len(t, upstream.requests, 2)
	require.Zero(t, accountRepo.setErrorCalls)
	require.Zero(t, accountRepo.setSchedulableCalls)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "invalid_request_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
	require.Equal(t, "policy rejected", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
}

func TestGatewayForwardAnthropicAPIKeyPassthrough_RecognizedErrorBeforeFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	account := newAnthropicAPIKeyAccountForTest()
	account.ID = 902
	accountRepo := &mockAccountRepoForPlatform{accountsByID: map[int64]*Account{account.ID: account}}
	rateLimits := NewRateLimitService(accountRepo, nil, testConfig(), nil, nil)
	upstream := &anthropicQueuedHTTPUpstream{responses: []*http.Response{{
		StatusCode: http.StatusUnauthorized,
		Header:     http.Header{"x-request-id": []string{"req-policy"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(`{"error":{"code":"cyber_policy","type":"invalid_request_error","message":"policy rejected"}}`))),
	}}}
	svc := &GatewayService{cfg: testConfig(), httpUpstream: upstream, rateLimitService: rateLimits}

	result, err := svc.forwardAnthropicAPIKeyPassthrough(
		context.Background(), c, account,
		[]byte(`{"model":"claude-3-5-sonnet-latest","messages":[]}`),
		"claude-3-5-sonnet-latest", "claude-3-5-sonnet-latest", false, time.Now(),
	)

	require.Nil(t, result)
	require.Error(t, err)
	require.NotErrorAs(t, err, new(*UpstreamFailoverError))
	require.Equal(t, 1, upstream.calls)
	require.Zero(t, accountRepo.setErrorCalls)
	require.Zero(t, accountRepo.setSchedulableCalls)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "invalid_request_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
	require.Equal(t, "policy rejected", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
	require.True(t, IsResponseCommitted(c))
}

type prefixErrorReader struct {
	prefix []byte
	read   bool
}

func (r *prefixErrorReader) Read(p []byte) (int, error) {
	if r.read {
		return 0, errors.New("prefix reader exhausted")
	}
	r.read = true
	n := copy(p, r.prefix)
	return n, errors.New("upstream body read failed")
}

func (r *prefixErrorReader) Close() error { return nil }

func TestHandleRecognizedHTTPErrorResponse_RestoresPartialBodyOnReadError(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Body:       &prefixErrorReader{prefix: []byte(`{"error":{"message":"partial"}}`)},
	}
	account := &Account{ID: 904, Platform: PlatformAnthropic, Type: AccountTypeAPIKey}

	_, _, handled := (&GatewayService{cfg: testConfig()}).handleRecognizedHTTPErrorResponse(resp, nil, account)

	require.False(t, handled)
	restored, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, `{"error":{"message":"partial"}}`, string(restored))
}

func TestHandleRecognizedHTTPErrorResponse_CommittedStreamReturnsTypedError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	_, err := c.Writer.WriteString(": ping\n\n")
	require.NoError(t, err)

	resp := &http.Response{
		StatusCode: http.StatusUnauthorized,
		Header:     http.Header{"x-request-id": []string{"req-policy"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(`{"error":{"code":"cyber_policy","type":"invalid_request_error","message":"policy rejected"}}`))),
	}
	account := &Account{ID: 903, Platform: PlatformAnthropic, Type: AccountTypeAPIKey}

	_, err, handled := (&GatewayService{cfg: testConfig()}).handleRecognizedHTTPErrorResponse(resp, c, account)

	require.True(t, handled)
	var recognizedErr *RecognizedUpstreamError
	require.ErrorAs(t, err, &recognizedErr)
	require.True(t, recognizedErr.OutputStarted)
	require.Equal(t, ": ping\n\n", rec.Body.String())
	require.False(t, IsResponseCommitted(c))
}

func TestGatewayHandleErrorResponse_RecognizedDirectBeforeHealthAndRules(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	body := []byte(`{"error":{"code":"cyber_policy","type":"invalid_request_error","message":"policy rejected"}}`)
	resp := &http.Response{
		StatusCode: http.StatusUnauthorized,
		Header:     http.Header{"x-request-id": []string{"req-policy"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
	conflictingCode := http.StatusTeapot
	conflictingMessage := "database must not win"
	rules := &ErrorPassthroughService{}
	rules.setLocalCache([]*model.ErrorPassthroughRule{{
		Enabled: true, Priority: 1, Platforms: []string{PlatformAnthropic},
		Keywords: []string{"cyber_policy"}, MatchMode: model.MatchModeAny,
		ResponseCode: &conflictingCode, CustomMessage: &conflictingMessage, SkipMonitoring: true,
	}})
	BindErrorPassthroughService(c, rules)
	account := &Account{ID: 901, Platform: PlatformAnthropic, Type: AccountTypeAPIKey}
	accountRepo := &mockAccountRepoForPlatform{
		accountsByID: map[int64]*Account{account.ID: account},
	}
	rateLimits := NewRateLimitService(accountRepo, nil, testConfig(), nil, nil)
	svc := &GatewayService{rateLimitService: rateLimits}

	_, err := svc.handleErrorResponse(context.Background(), resp, c, account)

	require.Error(t, err)
	require.NotErrorAs(t, err, new(*UpstreamFailoverError))
	require.Zero(t, accountRepo.setErrorCalls)
	require.Zero(t, accountRepo.setSchedulableCalls)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "invalid_request_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
	require.Equal(t, "policy rejected", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
	require.NotContains(t, rec.Body.String(), conflictingMessage)
	_, skip := c.Get(OpsSkipPassthroughKey)
	require.False(t, skip)
	rawEvents, exists := c.Get(OpsUpstreamErrorsKey)
	require.True(t, exists)
	events := rawEvents.([]*OpsUpstreamErrorEvent)
	require.Len(t, events, 1)
	require.NotNil(t, events[0].UpstreamFact)
	require.Equal(t, "cyber_policy", events[0].UpstreamFact.ProviderCode)
	require.True(t, IsResponseCommitted(c))
}

// mockGatewayCacheForPlatform 单平台测试用的 cache mock
type mockGatewayCacheForPlatform struct {
	sessionBindings map[string]int64
	deletedSessions map[string]int
}

func (m *mockGatewayCacheForPlatform) GetSessionAccountID(ctx context.Context, groupID int64, sessionHash string) (int64, error) {
	if id, ok := m.sessionBindings[sessionHash]; ok {
		return id, nil
	}
	return 0, errors.New("not found")
}

func (m *mockGatewayCacheForPlatform) SetSessionAccountID(ctx context.Context, groupID int64, sessionHash string, accountID int64, ttl time.Duration) error {
	if m.sessionBindings == nil {
		m.sessionBindings = make(map[string]int64)
	}
	m.sessionBindings[sessionHash] = accountID
	return nil
}

func (m *mockGatewayCacheForPlatform) RefreshSessionTTL(ctx context.Context, groupID int64, sessionHash string, ttl time.Duration) error {
	return nil
}

func (m *mockGatewayCacheForPlatform) DeleteSessionAccountID(ctx context.Context, groupID int64, sessionHash string) error {
	if m.sessionBindings == nil {
		return nil
	}
	if m.deletedSessions == nil {
		m.deletedSessions = make(map[string]int)
	}
	m.deletedSessions[sessionHash]++
	delete(m.sessionBindings, sessionHash)
	return nil
}

type mockGroupRepoForGateway struct {
	groups           map[int64]*Group
	getByIDCalls     int
	getByIDLiteCalls int
}

func (m *mockGroupRepoForGateway) GetByID(ctx context.Context, id int64) (*Group, error) {
	m.getByIDCalls++
	if g, ok := m.groups[id]; ok {
		return g, nil
	}
	return nil, ErrGroupNotFound
}

func (m *mockGroupRepoForGateway) GetByIDLite(ctx context.Context, id int64) (*Group, error) {
	m.getByIDLiteCalls++
	if g, ok := m.groups[id]; ok {
		return g, nil
	}
	return nil, ErrGroupNotFound
}

func (m *mockGroupRepoForGateway) Create(ctx context.Context, group *Group) error { return nil }
func (m *mockGroupRepoForGateway) Update(ctx context.Context, group *Group) error { return nil }
func (m *mockGroupRepoForGateway) Delete(ctx context.Context, id int64) error     { return nil }
func (m *mockGroupRepoForGateway) DeleteCascade(ctx context.Context, id int64) ([]int64, error) {
	return nil, nil
}
func (m *mockGroupRepoForGateway) List(ctx context.Context, params pagination.PaginationParams) ([]Group, *pagination.PaginationResult, error) {
	return nil, nil, nil
}
func (m *mockGroupRepoForGateway) ListWithFilters(ctx context.Context, params pagination.PaginationParams, platform, status, search string, isExclusive *bool) ([]Group, *pagination.PaginationResult, error) {
	return nil, nil, nil
}
func (m *mockGroupRepoForGateway) ListActive(ctx context.Context) ([]Group, error) {
	return nil, nil
}
func (m *mockGroupRepoForGateway) ListActiveByPlatform(ctx context.Context, platform string) ([]Group, error) {
	return nil, nil
}
func (m *mockGroupRepoForGateway) ListAllIncludingInactive(ctx context.Context, platform string) ([]Group, error) {
	return nil, nil
}
func (m *mockGroupRepoForGateway) ExistsByName(ctx context.Context, name string) (bool, error) {
	return false, nil
}
func (m *mockGroupRepoForGateway) GetAccountCount(ctx context.Context, groupID int64) (int64, int64, error) {
	return 0, 0, nil
}
func (m *mockGroupRepoForGateway) DeleteAccountGroupsByGroupID(ctx context.Context, groupID int64) (int64, error) {
	return 0, nil
}

func (m *mockGroupRepoForGateway) BindAccountsToGroup(ctx context.Context, groupID int64, accountIDs []int64) error {
	return nil
}

func (m *mockGroupRepoForGateway) GetAccountIDsByGroupIDs(ctx context.Context, groupIDs []int64) ([]int64, error) {
	return nil, nil
}

func (m *mockGroupRepoForGateway) UpdateSortOrders(ctx context.Context, updates []GroupSortOrderUpdate) error {
	return nil
}

func ptr[T any](v T) *T {
	return &v
}

// TestGatewayService_SelectAccountForModelWithPlatform_Anthropic 测试 anthropic 单平台选择
func TestGatewayService_SelectAccountForModelWithPlatform_Anthropic(t *testing.T) {
	ctx := context.Background()

	repo := &mockAccountRepoForPlatform{
		accounts: []Account{
			{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true},
			{ID: 2, Platform: PlatformAnthropic, Priority: 2, Status: StatusActive, Schedulable: true},
			{ID: 3, Platform: PlatformAntigravity, Priority: 1, Status: StatusActive, Schedulable: true}, // 应被隔离
		},
		accountsByID: map[int64]*Account{},
	}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}

	cache := &mockGatewayCacheForPlatform{}

	svc := &GatewayService{
		accountRepo: repo,
		cache:       cache,
		cfg:         testConfig(),
	}

	acc, err := svc.selectAccountForModelWithPlatform(ctx, nil, "", "claude-3-5-sonnet-20241022", nil, PlatformAnthropic)
	require.NoError(t, err)
	require.NotNil(t, acc)
	require.Equal(t, int64(1), acc.ID, "应选择优先级最高的 anthropic 账户")
	require.Equal(t, PlatformAnthropic, acc.Platform, "应只返回 anthropic 平台账户")
}

// TestGatewayService_SelectAccountForModelWithPlatform_Antigravity 测试 antigravity 单平台选择
func TestGatewayService_SelectAccountForModelWithPlatform_Antigravity(t *testing.T) {
	ctx := context.Background()

	repo := &mockAccountRepoForPlatform{
		accounts: []Account{
			{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true}, // 应被隔离
			{ID: 2, Platform: PlatformAntigravity, Priority: 1, Status: StatusActive, Schedulable: true},
		},
		accountsByID: map[int64]*Account{},
	}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}

	cache := &mockGatewayCacheForPlatform{}

	svc := &GatewayService{
		accountRepo: repo,
		cache:       cache,
		cfg:         testConfig(),
	}

	acc, err := svc.selectAccountForModelWithPlatform(ctx, nil, "", "claude-sonnet-4-5", nil, PlatformAntigravity)
	require.NoError(t, err)
	require.NotNil(t, acc)
	require.Equal(t, int64(2), acc.ID)
	require.Equal(t, PlatformAntigravity, acc.Platform, "应只返回 antigravity 平台账户")
}

// TestGatewayService_SelectAccountForModelWithPlatform_PriorityAndLastUsed 测试优先级和最后使用时间
func TestGatewayService_SelectAccountForModelWithPlatform_PriorityAndLastUsed(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	repo := &mockAccountRepoForPlatform{
		accounts: []Account{
			{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, LastUsedAt: ptr(now.Add(-1 * time.Hour))},
			{ID: 2, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, LastUsedAt: ptr(now.Add(-2 * time.Hour))},
		},
		accountsByID: map[int64]*Account{},
	}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}

	cache := &mockGatewayCacheForPlatform{}

	svc := &GatewayService{
		accountRepo: repo,
		cache:       cache,
		cfg:         testConfig(),
	}

	acc, err := svc.selectAccountForModelWithPlatform(ctx, nil, "", "claude-3-5-sonnet-20241022", nil, PlatformAnthropic)
	require.NoError(t, err)
	require.NotNil(t, acc)
	require.Equal(t, int64(2), acc.ID, "同优先级应选择最久未用的账户")
}

func TestGatewayService_SelectAccountForModelWithPlatform_GeminiOAuthPreference(t *testing.T) {
	ctx := context.Background()

	repo := &mockAccountRepoForPlatform{
		accounts: []Account{
			{ID: 1, Platform: PlatformGemini, Priority: 1, Status: StatusActive, Schedulable: true, Type: AccountTypeAPIKey},
			{ID: 2, Platform: PlatformGemini, Priority: 1, Status: StatusActive, Schedulable: true, Type: AccountTypeOAuth},
		},
		accountsByID: map[int64]*Account{},
	}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}

	cache := &mockGatewayCacheForPlatform{}

	svc := &GatewayService{
		accountRepo: repo,
		cache:       cache,
		cfg:         testConfig(),
	}

	acc, err := svc.selectAccountForModelWithPlatform(ctx, nil, "", "gemini-2.5-pro", nil, PlatformGemini)
	require.NoError(t, err)
	require.NotNil(t, acc)
	require.Equal(t, int64(2), acc.ID, "同优先级且未使用时应优先选择OAuth账户")
}

// TestGatewayService_SelectAccountForModelWithPlatform_NoAvailableAccounts 测试无可用账户
func TestGatewayService_SelectAccountForModelWithPlatform_NoAvailableAccounts(t *testing.T) {
	ctx := context.Background()

	repo := &mockAccountRepoForPlatform{
		accounts:     []Account{},
		accountsByID: map[int64]*Account{},
	}

	cache := &mockGatewayCacheForPlatform{}

	svc := &GatewayService{
		accountRepo: repo,
		cache:       cache,
		cfg:         testConfig(),
	}

	acc, err := svc.selectAccountForModelWithPlatform(ctx, nil, "", "claude-3-5-sonnet-20241022", nil, PlatformAnthropic)
	require.Error(t, err)
	require.Nil(t, acc)
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.NotErrorIs(t, err, ErrModelNotSupportedByAccounts)
}

func TestGatewayService_SelectAccountForModelWithPlatform_ModelRateLimitedNotUnsupportedModel(t *testing.T) {
	ctx := WithPublicModelSupportMiss404(context.Background())
	resetAt := time.Now().Add(time.Hour)

	repo := &mockAccountRepoForPlatform{
		accounts: []Account{
			{
				ID:                     1,
				Platform:               PlatformAnthropic,
				Priority:               1,
				Status:                 StatusActive,
				Schedulable:            true,
				Concurrency:            1,
				RateLimitedAt:          &resetAt,
				RateLimitResetAt:       &resetAt,
				TempUnschedulableUntil: nil,
			},
		},
		accountsByID: map[int64]*Account{},
	}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}

	svc := &GatewayService{
		accountRepo: repo,
		cache:       &mockGatewayCacheForPlatform{},
		cfg:         testConfig(),
	}

	acc, err := svc.selectAccountForModelWithPlatform(ctx, nil, "", "claude-3-5-sonnet-20241022", nil, PlatformAnthropic)
	require.Error(t, err)
	require.Nil(t, acc)
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.NotErrorIs(t, err, ErrModelNotSupportedByAccounts)
}

func TestGatewayService_SelectAccountForModelWithPlatform_AvailabilityLookupFailureStaysRetryable(t *testing.T) {
	repo := &mockAccountRepoForPlatform{
		accounts: []Account{{
			ID:          1,
			Platform:    PlatformAnthropic,
			Status:      StatusActive,
			Schedulable: true,
			Credentials: map[string]any{"model_mapping": map[string]any{"claude-haiku": "claude-haiku"}},
		}},
		accountsByID: map[int64]*Account{},
		listModelAvailabilityCandidates: func(context.Context, *int64, []string, bool) ([]Account, error) {
			return nil, errors.New("database unavailable")
		},
	}
	svc := &GatewayService{accountRepo: repo, cache: &mockGatewayCacheForPlatform{}, cfg: testConfig()}

	acc, err := svc.selectAccountForModelWithPlatform(WithPublicModelSupportMiss404(context.Background()), nil, "", "claude-sonnet", nil, PlatformAnthropic)
	require.Error(t, err)
	require.Nil(t, acc)
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.NotErrorIs(t, err, ErrModelNotSupportedByAccounts)
}

func TestGatewayService_IsPureModelSupportMiss_MixedSchedulingScope(t *testing.T) {
	tests := []struct {
		name         string
		mixedEnabled bool
		wantMiss     bool
	}{
		{name: "enabled Antigravity support keeps miss retryable", mixedEnabled: true, wantMiss: false},
		{name: "disabled Antigravity support stays out of scope", mixedEnabled: false, wantMiss: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var requestedPlatforms []string
			repo := &mockAccountRepoForPlatform{
				listModelAvailabilityCandidates: func(_ context.Context, _ *int64, platforms []string, _ bool) ([]Account, error) {
					requestedPlatforms = append([]string(nil), platforms...)
					return []Account{
						{ID: 1, Platform: PlatformAnthropic, Status: StatusActive, Schedulable: true, Credentials: map[string]any{"model_mapping": map[string]any{"claude-haiku": "claude-haiku"}}},
						{ID: 2, Platform: PlatformAntigravity, Status: StatusActive, Schedulable: true, Extra: map[string]any{"mixed_scheduling": tc.mixedEnabled}},
					}, nil
				},
			}
			svc := &GatewayService{accountRepo: repo, cfg: testConfig()}
			miss := svc.isPureModelSupportMiss(WithPublicModelSupportMiss404(context.Background()), nil, "claude-sonnet-4-5", PlatformAnthropic, nil, true, nil, nil)
			require.Equal(t, tc.wantMiss, miss)
			require.ElementsMatch(t, []string{PlatformAnthropic, PlatformAntigravity}, requestedPlatforms)
		})
	}
}

// TestGatewayService_SelectAccountForModelWithPlatform_AllExcluded 测试所有账户被排除
func TestGatewayService_SelectAccountForModelWithPlatform_AllExcluded(t *testing.T) {
	ctx := context.Background()

	repo := &mockAccountRepoForPlatform{
		accounts: []Account{
			{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true},
			{ID: 2, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true},
		},
		accountsByID: map[int64]*Account{},
	}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}

	cache := &mockGatewayCacheForPlatform{}

	svc := &GatewayService{
		accountRepo: repo,
		cache:       cache,
		cfg:         testConfig(),
	}

	excludedIDs := map[int64]struct{}{1: {}, 2: {}}
	acc, err := svc.selectAccountForModelWithPlatform(ctx, nil, "", "claude-3-5-sonnet-20241022", excludedIDs, PlatformAnthropic)
	require.Error(t, err)
	require.Nil(t, acc)
}

// TestGatewayService_SelectAccountForModelWithPlatform_Schedulability 测试账户可调度性检查
func TestGatewayService_SelectAccountForModelWithPlatform_Schedulability(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	tests := []struct {
		name       string
		accounts   []Account
		expectedID int64
	}{
		{
			name: "过载账户被跳过",
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, OverloadUntil: ptr(now.Add(1 * time.Hour))},
				{ID: 2, Platform: PlatformAnthropic, Priority: 2, Status: StatusActive, Schedulable: true},
			},
			expectedID: 2,
		},
		{
			name: "限流账户被跳过",
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, RateLimitResetAt: ptr(now.Add(1 * time.Hour))},
				{ID: 2, Platform: PlatformAnthropic, Priority: 2, Status: StatusActive, Schedulable: true},
			},
			expectedID: 2,
		},
		{
			name: "非active账户被跳过",
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: "error", Schedulable: true},
				{ID: 2, Platform: PlatformAnthropic, Priority: 2, Status: StatusActive, Schedulable: true},
			},
			expectedID: 2,
		},
		{
			name: "schedulable=false被跳过",
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: false},
				{ID: 2, Platform: PlatformAnthropic, Priority: 2, Status: StatusActive, Schedulable: true},
			},
			expectedID: 2,
		},
		{
			name: "过期的过载账户可调度",
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, OverloadUntil: ptr(now.Add(-1 * time.Hour))},
				{ID: 2, Platform: PlatformAnthropic, Priority: 2, Status: StatusActive, Schedulable: true},
			},
			expectedID: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &mockAccountRepoForPlatform{
				accounts:     tt.accounts,
				accountsByID: map[int64]*Account{},
			}
			for i := range repo.accounts {
				repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
			}

			cache := &mockGatewayCacheForPlatform{}

			svc := &GatewayService{
				accountRepo: repo,
				cache:       cache,
				cfg:         testConfig(),
			}

			acc, err := svc.selectAccountForModelWithPlatform(ctx, nil, "", "claude-3-5-sonnet-20241022", nil, PlatformAnthropic)
			require.NoError(t, err)
			require.NotNil(t, acc)
			require.Equal(t, tt.expectedID, acc.ID)
		})
	}
}

// TestGatewayService_SelectAccountForModelWithPlatform_StickySession 测试粘性会话
func TestGatewayService_SelectAccountForModelWithPlatform_StickySession(t *testing.T) {
	ctx := context.Background()

	t.Run("粘性会话命中-同平台", func(t *testing.T) {
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 2, Status: StatusActive, Schedulable: true},
				{ID: 2, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{
			sessionBindings: map[string]int64{"session-123": 1},
		}

		svc := &GatewayService{
			accountRepo: repo,
			cache:       cache,
			cfg:         testConfig(),
		}

		acc, err := svc.selectAccountForModelWithPlatform(ctx, nil, "session-123", "claude-3-5-sonnet-20241022", nil, PlatformAnthropic)
		require.NoError(t, err)
		require.NotNil(t, acc)
		require.Equal(t, int64(1), acc.ID, "应返回粘性会话绑定的账户")
	})

	t.Run("粘性会话不匹配平台-降级选择", func(t *testing.T) {
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAntigravity, Priority: 2, Status: StatusActive, Schedulable: true}, // 粘性会话绑定但平台不匹配
				{ID: 2, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{
			sessionBindings: map[string]int64{"session-123": 1}, // 绑定 antigravity 账户
		}

		svc := &GatewayService{
			accountRepo: repo,
			cache:       cache,
			cfg:         testConfig(),
		}

		// 请求 anthropic 平台，但粘性会话绑定的是 antigravity 账户
		acc, err := svc.selectAccountForModelWithPlatform(ctx, nil, "session-123", "claude-3-5-sonnet-20241022", nil, PlatformAnthropic)
		require.NoError(t, err)
		require.NotNil(t, acc)
		require.Equal(t, int64(2), acc.ID, "粘性会话账户平台不匹配，应降级选择同平台账户")
		require.Equal(t, PlatformAnthropic, acc.Platform)
	})

	t.Run("粘性会话账户被排除-降级选择", func(t *testing.T) {
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 2, Status: StatusActive, Schedulable: true},
				{ID: 2, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{
			sessionBindings: map[string]int64{"session-123": 1},
		}

		svc := &GatewayService{
			accountRepo: repo,
			cache:       cache,
			cfg:         testConfig(),
		}

		excludedIDs := map[int64]struct{}{1: {}}
		acc, err := svc.selectAccountForModelWithPlatform(ctx, nil, "session-123", "claude-3-5-sonnet-20241022", excludedIDs, PlatformAnthropic)
		require.NoError(t, err)
		require.NotNil(t, acc)
		require.Equal(t, int64(2), acc.ID, "粘性会话账户被排除，应选择其他账户")
	})

	t.Run("粘性会话账户不可调度-降级选择", func(t *testing.T) {
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 2, Status: "error", Schedulable: true},
				{ID: 2, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{
			sessionBindings: map[string]int64{"session-123": 1},
		}

		svc := &GatewayService{
			accountRepo: repo,
			cache:       cache,
			cfg:         testConfig(),
		}

		acc, err := svc.selectAccountForModelWithPlatform(ctx, nil, "session-123", "claude-3-5-sonnet-20241022", nil, PlatformAnthropic)
		require.NoError(t, err)
		require.NotNil(t, acc)
		require.Equal(t, int64(2), acc.ID, "粘性会话账户不可调度，应选择其他账户")
	})
}

func TestGatewayService_SelectAccountForModelWithExclusions_ForcePlatform(t *testing.T) {
	ctx := context.Background()
	ctx = context.WithValue(ctx, ctxkey.ForcePlatform, PlatformAntigravity)

	repo := &mockAccountRepoForPlatform{
		accounts: []Account{
			{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true},
			{ID: 2, Platform: PlatformAntigravity, Priority: 2, Status: StatusActive, Schedulable: true},
		},
		accountsByID: map[int64]*Account{},
	}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}

	cache := &mockGatewayCacheForPlatform{}

	svc := &GatewayService{
		accountRepo: repo,
		cache:       cache,
		cfg:         testConfig(),
	}

	acc, err := svc.SelectAccountForModelWithExclusions(ctx, nil, "", "claude-sonnet-4-5", nil)
	require.NoError(t, err)
	require.NotNil(t, acc)
	require.Equal(t, int64(2), acc.ID)
	require.Equal(t, PlatformAntigravity, acc.Platform)
}

func TestGatewayService_SelectAccountForModelWithPlatform_RoutedStickySessionClears(t *testing.T) {
	ctx := context.Background()
	groupID := int64(10)
	requestedModel := "claude-3-5-sonnet-20241022"

	repo := &mockAccountRepoForPlatform{
		accounts: []Account{
			{ID: 1, Platform: PlatformAnthropic, Priority: 2, Status: StatusDisabled, Schedulable: true},
			{ID: 2, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true},
		},
		accountsByID: map[int64]*Account{},
	}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}

	cache := &mockGatewayCacheForPlatform{
		sessionBindings: map[string]int64{"session-123": 1},
	}

	groupRepo := &mockGroupRepoForGateway{
		groups: map[int64]*Group{
			groupID: {
				ID:                  groupID,
				Name:                "route-group",
				Platform:            PlatformAnthropic,
				Status:              StatusActive,
				Hydrated:            true,
				ModelRoutingEnabled: true,
				ModelRouting: map[string][]int64{
					requestedModel: {1, 2},
				},
			},
		},
	}

	svc := &GatewayService{
		accountRepo: repo,
		cache:       cache,
		cfg:         testConfig(),
		groupRepo:   groupRepo,
	}

	acc, err := svc.selectAccountForModelWithPlatform(ctx, &groupID, "session-123", requestedModel, nil, PlatformAnthropic)
	require.NoError(t, err)
	require.NotNil(t, acc)
	require.Equal(t, int64(2), acc.ID)
	require.Equal(t, 1, cache.deletedSessions["session-123"])
	require.Equal(t, int64(2), cache.sessionBindings["session-123"])
}

func TestGatewayService_SelectAccountForModelWithPlatform_RoutedStickySessionHit(t *testing.T) {
	ctx := context.Background()
	groupID := int64(11)
	requestedModel := "claude-3-5-sonnet-20241022"

	repo := &mockAccountRepoForPlatform{
		accounts: []Account{
			{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true},
			{ID: 2, Platform: PlatformAnthropic, Priority: 2, Status: StatusActive, Schedulable: true},
		},
		accountsByID: map[int64]*Account{},
	}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}

	cache := &mockGatewayCacheForPlatform{
		sessionBindings: map[string]int64{"session-456": 1},
	}

	groupRepo := &mockGroupRepoForGateway{
		groups: map[int64]*Group{
			groupID: {
				ID:                  groupID,
				Name:                "route-group-hit",
				Platform:            PlatformAnthropic,
				Status:              StatusActive,
				Hydrated:            true,
				ModelRoutingEnabled: true,
				ModelRouting: map[string][]int64{
					requestedModel: {1, 2},
				},
			},
		},
	}

	svc := &GatewayService{
		accountRepo: repo,
		cache:       cache,
		cfg:         testConfig(),
		groupRepo:   groupRepo,
	}

	acc, err := svc.selectAccountForModelWithPlatform(ctx, &groupID, "session-456", requestedModel, nil, PlatformAnthropic)
	require.NoError(t, err)
	require.NotNil(t, acc)
	require.Equal(t, int64(1), acc.ID)
}

func TestGatewayService_SelectAccountForModelWithPlatform_RoutedFallbackToNormal(t *testing.T) {
	ctx := context.Background()
	groupID := int64(12)
	requestedModel := "claude-3-5-sonnet-20241022"

	repo := &mockAccountRepoForPlatform{
		accounts: []Account{
			{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true},
			{ID: 2, Platform: PlatformAnthropic, Priority: 2, Status: StatusActive, Schedulable: true},
		},
		accountsByID: map[int64]*Account{},
	}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}

	cache := &mockGatewayCacheForPlatform{}

	groupRepo := &mockGroupRepoForGateway{
		groups: map[int64]*Group{
			groupID: {
				ID:                  groupID,
				Name:                "route-fallback",
				Platform:            PlatformAnthropic,
				Status:              StatusActive,
				Hydrated:            true,
				ModelRoutingEnabled: true,
				ModelRouting: map[string][]int64{
					requestedModel: {99},
				},
			},
		},
	}

	svc := &GatewayService{
		accountRepo: repo,
		cache:       cache,
		cfg:         testConfig(),
		groupRepo:   groupRepo,
	}

	acc, err := svc.selectAccountForModelWithPlatform(ctx, &groupID, "", requestedModel, nil, PlatformAnthropic)
	require.NoError(t, err)
	require.NotNil(t, acc)
	require.Equal(t, int64(1), acc.ID)
}

func TestGatewayService_SelectAccountForModelWithPlatform_NoModelSupport(t *testing.T) {
	ctx := context.Background()

	repo := &mockAccountRepoForPlatform{
		accounts: []Account{
			{
				ID:          1,
				Platform:    PlatformAnthropic,
				Priority:    1,
				Status:      StatusActive,
				Schedulable: true,
				Credentials: map[string]any{"model_mapping": map[string]any{"claude-3-5-haiku-20241022": "claude-3-5-haiku-20241022"}},
			},
		},
		accountsByID: map[int64]*Account{},
	}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}

	cache := &mockGatewayCacheForPlatform{}

	svc := &GatewayService{
		accountRepo: repo,
		cache:       cache,
		cfg:         testConfig(),
	}

	acc, err := svc.selectAccountForModelWithPlatform(ctx, nil, "", "claude-3-5-sonnet-20241022", nil, PlatformAnthropic)
	require.Error(t, err)
	require.Nil(t, acc)
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.NotErrorIs(t, err, ErrModelNotSupportedByAccounts)
	require.Contains(t, err.Error(), "supporting model")
}

func TestGatewayService_SelectAccountForModelWithPlatform_BedrockSupportMissRequiresPublicOptIn(t *testing.T) {
	repo := &mockAccountRepoForPlatform{
		accounts: []Account{
			{
				ID:          1,
				Platform:    PlatformAnthropic,
				Type:        AccountTypeBedrock,
				Priority:    1,
				Status:      StatusActive,
				Schedulable: true,
				Credentials: map[string]any{"aws_region": "us-east-1"},
			},
		},
		accountsByID: map[int64]*Account{},
	}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}
	svc := &GatewayService{
		accountRepo: repo,
		cache:       &mockGatewayCacheForPlatform{},
		cfg:         testConfig(),
	}

	acc, err := svc.selectAccountForModelWithPlatform(context.Background(), nil, "", "claude-3-5-sonnet-20241022", nil, PlatformAnthropic)
	require.Error(t, err)
	require.Nil(t, acc)
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.NotErrorIs(t, err, ErrModelNotSupportedByAccounts)
	require.Contains(t, err.Error(), "supporting model")

	acc, err = svc.selectAccountForModelWithPlatform(WithPublicModelSupportMiss404(context.Background()), nil, "", "claude-3-5-sonnet-20241022", nil, PlatformAnthropic)
	require.Error(t, err)
	require.Nil(t, acc)
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.ErrorIs(t, err, ErrModelNotSupportedByAccounts)
	model, ok := ModelNotSupportedRequestedModel(err)
	require.True(t, ok)
	require.Equal(t, "claude-3-5-sonnet-20241022", model)
}

func TestGatewayService_SelectAccountForModelWithPlatform_AntigravitySupportMissRequiresPublicOptIn(t *testing.T) {
	repo := &mockAccountRepoForPlatform{
		accounts: []Account{
			{
				ID:          1,
				Platform:    PlatformAntigravity,
				Type:        AccountTypeAPIKey,
				Priority:    1,
				Status:      StatusActive,
				Schedulable: true,
			},
		},
		accountsByID: map[int64]*Account{},
	}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}
	svc := &GatewayService{
		accountRepo: repo,
		cache:       &mockGatewayCacheForPlatform{},
		cfg:         testConfig(),
	}

	acc, err := svc.selectAccountForModelWithPlatform(context.Background(), nil, "", "gpt-4", nil, PlatformAntigravity)
	require.Error(t, err)
	require.Nil(t, acc)
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.NotErrorIs(t, err, ErrModelNotSupportedByAccounts)
	require.Contains(t, err.Error(), "supporting model")

	acc, err = svc.selectAccountForModelWithPlatform(WithPublicModelSupportMiss404(context.Background()), nil, "", "gpt-4", nil, PlatformAntigravity)
	require.Error(t, err)
	require.Nil(t, acc)
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.ErrorIs(t, err, ErrModelNotSupportedByAccounts)
	model, ok := ModelNotSupportedRequestedModel(err)
	require.True(t, ok)
	require.Equal(t, "gpt-4", model)
}

func TestGatewayService_SelectAccountForModelWithPlatform_GeminiPreferOAuth(t *testing.T) {
	ctx := context.Background()

	repo := &mockAccountRepoForPlatform{
		accounts: []Account{
			{ID: 1, Platform: PlatformGemini, Priority: 1, Status: StatusActive, Schedulable: true, Type: AccountTypeAPIKey},
			{ID: 2, Platform: PlatformGemini, Priority: 1, Status: StatusActive, Schedulable: true, Type: AccountTypeOAuth},
		},
		accountsByID: map[int64]*Account{},
	}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}

	cache := &mockGatewayCacheForPlatform{}

	svc := &GatewayService{
		accountRepo: repo,
		cache:       cache,
		cfg:         testConfig(),
	}

	acc, err := svc.selectAccountForModelWithPlatform(ctx, nil, "", "gemini-2.5-pro", nil, PlatformGemini)
	require.NoError(t, err)
	require.NotNil(t, acc)
	require.Equal(t, int64(2), acc.ID)
}

func TestGatewayService_SelectAccountForModelWithPlatform_GeminiAPIKeyModelMappingFilter(t *testing.T) {
	ctx := context.Background()

	repo := &mockAccountRepoForPlatform{
		accounts: []Account{
			{
				ID:          1,
				Platform:    PlatformGemini,
				Type:        AccountTypeAPIKey,
				Priority:    1,
				Status:      StatusActive,
				Schedulable: true,
				Credentials: map[string]any{"model_mapping": map[string]any{"gemini-2.5-pro": "gemini-2.5-pro"}},
			},
			{
				ID:          2,
				Platform:    PlatformGemini,
				Type:        AccountTypeAPIKey,
				Priority:    2,
				Status:      StatusActive,
				Schedulable: true,
				Credentials: map[string]any{"model_mapping": map[string]any{"gemini-2.5-flash": "gemini-2.5-flash"}},
			},
		},
		accountsByID: map[int64]*Account{},
	}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}

	cache := &mockGatewayCacheForPlatform{}

	svc := &GatewayService{
		accountRepo: repo,
		cache:       cache,
		cfg:         testConfig(),
	}

	acc, err := svc.selectAccountForModelWithPlatform(ctx, nil, "", "gemini-2.5-flash", nil, PlatformGemini)
	require.NoError(t, err)
	require.NotNil(t, acc)
	require.Equal(t, int64(2), acc.ID, "应过滤不支持请求模型的 APIKey 账号")

	acc, err = svc.selectAccountForModelWithPlatform(ctx, nil, "", "gemini-3-pro-preview", nil, PlatformGemini)
	require.Error(t, err)
	require.Nil(t, acc)
	require.Contains(t, err.Error(), "supporting model")
}

func TestGatewayService_SelectAccountForModelWithPlatform_StickyInGroup(t *testing.T) {
	ctx := context.Background()
	groupID := int64(50)

	repo := &mockAccountRepoForPlatform{
		accounts: []Account{
			{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, AccountGroups: []AccountGroup{{GroupID: groupID}}},
			{ID: 2, Platform: PlatformAnthropic, Priority: 2, Status: StatusActive, Schedulable: true, AccountGroups: []AccountGroup{{GroupID: groupID}}},
		},
		accountsByID: map[int64]*Account{},
	}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}

	cache := &mockGatewayCacheForPlatform{
		sessionBindings: map[string]int64{"session-group": 1},
	}

	svc := &GatewayService{
		accountRepo: repo,
		cache:       cache,
		cfg:         testConfig(),
	}

	acc, err := svc.selectAccountForModelWithPlatform(ctx, &groupID, "session-group", "", nil, PlatformAnthropic)
	require.NoError(t, err)
	require.NotNil(t, acc)
	require.Equal(t, int64(1), acc.ID)
}

func TestGatewayService_SelectAccountForModelWithPlatform_StickyModelMismatchFallback(t *testing.T) {
	ctx := context.Background()

	repo := &mockAccountRepoForPlatform{
		accounts: []Account{
			{
				ID:          1,
				Platform:    PlatformAnthropic,
				Priority:    1,
				Status:      StatusActive,
				Schedulable: true,
				Credentials: map[string]any{"model_mapping": map[string]any{"claude-3-5-haiku-20241022": "claude-3-5-haiku-20241022"}},
			},
			{ID: 2, Platform: PlatformAnthropic, Priority: 2, Status: StatusActive, Schedulable: true},
		},
		accountsByID: map[int64]*Account{},
	}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}

	cache := &mockGatewayCacheForPlatform{
		sessionBindings: map[string]int64{"session-miss": 1},
	}

	svc := &GatewayService{
		accountRepo: repo,
		cache:       cache,
		cfg:         testConfig(),
	}

	acc, err := svc.selectAccountForModelWithPlatform(ctx, nil, "session-miss", "claude-3-5-sonnet-20241022", nil, PlatformAnthropic)
	require.NoError(t, err)
	require.NotNil(t, acc)
	require.Equal(t, int64(2), acc.ID)
}

func TestGatewayService_SelectAccountForModelWithPlatform_PreferNeverUsed(t *testing.T) {
	ctx := context.Background()
	lastUsed := time.Now().Add(-1 * time.Hour)

	repo := &mockAccountRepoForPlatform{
		accounts: []Account{
			{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, LastUsedAt: &lastUsed},
			{ID: 2, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true},
		},
		accountsByID: map[int64]*Account{},
	}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}

	cache := &mockGatewayCacheForPlatform{}

	svc := &GatewayService{
		accountRepo: repo,
		cache:       cache,
		cfg:         testConfig(),
	}

	acc, err := svc.selectAccountForModelWithPlatform(ctx, nil, "", "claude-3-5-sonnet-20241022", nil, PlatformAnthropic)
	require.NoError(t, err)
	require.NotNil(t, acc)
	require.Equal(t, int64(2), acc.ID)
}

func TestGatewayService_SelectAccountForModelWithPlatform_NoAccounts(t *testing.T) {
	ctx := context.Background()
	repo := &mockAccountRepoForPlatform{
		accounts:     []Account{},
		accountsByID: map[int64]*Account{},
	}

	cache := &mockGatewayCacheForPlatform{}

	svc := &GatewayService{
		accountRepo: repo,
		cache:       cache,
		cfg:         testConfig(),
	}

	acc, err := svc.selectAccountForModelWithPlatform(ctx, nil, "", "", nil, PlatformAnthropic)
	require.Error(t, err)
	require.Nil(t, acc)
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
}

func TestGatewayService_isModelSupportedByAccount(t *testing.T) {
	svc := &GatewayService{}

	tests := []struct {
		name     string
		account  *Account
		model    string
		expected bool
	}{
		{
			name:     "Antigravity平台-支持默认映射中的claude模型",
			account:  &Account{Platform: PlatformAntigravity},
			model:    "claude-sonnet-4-5",
			expected: true,
		},
		{
			name:     "Antigravity平台-不支持非默认映射中的claude模型",
			account:  &Account{Platform: PlatformAntigravity},
			model:    "claude-3-5-sonnet-20241022",
			expected: false,
		},
		{
			name:     "Antigravity平台-支持gemini模型",
			account:  &Account{Platform: PlatformAntigravity},
			model:    "gemini-2.5-flash",
			expected: true,
		},
		{
			name:     "Antigravity平台-不支持gpt模型",
			account:  &Account{Platform: PlatformAntigravity},
			model:    "gpt-4",
			expected: false,
		},
		{
			name:     "Anthropic平台-无映射配置-支持所有模型",
			account:  &Account{Platform: PlatformAnthropic},
			model:    "claude-3-5-sonnet-20241022",
			expected: true,
		},
		{
			name: "Anthropic平台-有映射配置-只支持配置的模型",
			account: &Account{
				Platform:    PlatformAnthropic,
				Credentials: map[string]any{"model_mapping": map[string]any{"claude-opus-4": "x"}},
			},
			model:    "claude-3-5-sonnet-20241022",
			expected: false,
		},
		{
			name: "Anthropic平台-有映射配置-支持配置的模型",
			account: &Account{
				Platform:    PlatformAnthropic,
				Credentials: map[string]any{"model_mapping": map[string]any{"claude-3-5-sonnet-20241022": "x"}},
			},
			model:    "claude-3-5-sonnet-20241022",
			expected: true,
		},
		{
			name:     "Gemini平台-无映射配置-支持所有模型",
			account:  &Account{Platform: PlatformGemini, Type: AccountTypeAPIKey},
			model:    "gemini-2.5-flash",
			expected: true,
		},
		{
			name: "Gemini平台-有映射配置-只支持配置的模型",
			account: &Account{
				Platform: PlatformGemini,
				Type:     AccountTypeAPIKey,
				Credentials: map[string]any{
					"model_mapping": map[string]any{"gemini-2.5-pro": "gemini-2.5-pro"},
				},
			},
			model:    "gemini-2.5-flash",
			expected: false,
		},
		{
			name: "Gemini平台-有映射配置-支持配置的模型",
			account: &Account{
				Platform: PlatformGemini,
				Type:     AccountTypeAPIKey,
				Credentials: map[string]any{
					"model_mapping": map[string]any{"gemini-2.5-pro": "gemini-2.5-pro"},
				},
			},
			model:    "gemini-2.5-pro",
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := svc.isModelSupportedByAccount(tt.account, tt.model)
			require.Equal(t, tt.expected, got)
		})
	}
}

// TestGatewayService_selectAccountWithMixedScheduling 测试混合调度
func TestGatewayService_selectAccountWithMixedScheduling(t *testing.T) {
	ctx := context.Background()

	t.Run("混合调度-Gemini优先选择OAuth账户", func(t *testing.T) {
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformGemini, Priority: 1, Status: StatusActive, Schedulable: true, Type: AccountTypeAPIKey},
				{ID: 2, Platform: PlatformGemini, Priority: 1, Status: StatusActive, Schedulable: true, Type: AccountTypeOAuth},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{}

		svc := &GatewayService{
			accountRepo: repo,
			cache:       cache,
			cfg:         testConfig(),
		}

		acc, err := svc.selectAccountWithMixedScheduling(ctx, nil, "", "gemini-2.5-pro", nil, PlatformGemini)
		require.NoError(t, err)
		require.NotNil(t, acc)
		require.Equal(t, int64(2), acc.ID, "同优先级且未使用时应优先选择OAuth账户")
	})

	t.Run("混合调度-包含启用mixed_scheduling的antigravity账户", func(t *testing.T) {
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 2, Status: StatusActive, Schedulable: true},
				{ID: 2, Platform: PlatformAntigravity, Priority: 1, Status: StatusActive, Schedulable: true, Extra: map[string]any{"mixed_scheduling": true}},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{}

		svc := &GatewayService{
			accountRepo: repo,
			cache:       cache,
			cfg:         testConfig(),
		}

		acc, err := svc.selectAccountWithMixedScheduling(ctx, nil, "", "claude-sonnet-4-5", nil, PlatformAnthropic)
		require.NoError(t, err)
		require.NotNil(t, acc)
		require.Equal(t, int64(2), acc.ID, "应选择优先级最高的账户（包含启用混合调度的antigravity）")
	})

	t.Run("混合调度-Gemini家族限流后跳过Antigravity账户", func(t *testing.T) {
		resetAt := time.Now().Add(10 * time.Minute).Format(time.RFC3339)
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{
					ID:          1,
					Platform:    PlatformAntigravity,
					Priority:    1,
					Status:      StatusActive,
					Schedulable: true,
					Extra: map[string]any{
						"mixed_scheduling": true,
						modelRateLimitsKey: map[string]any{
							antigravityGeminiModelRateLimitKey: map[string]any{
								"rate_limit_reset_at": resetAt,
							},
						},
					},
				},
				{
					ID:          2,
					Platform:    PlatformAntigravity,
					Priority:    1,
					Status:      StatusActive,
					Schedulable: true,
					Extra: map[string]any{
						"mixed_scheduling": true,
						modelRateLimitsKey: map[string]any{
							antigravityGeminiModelRateLimitKey: map[string]any{
								"rate_limit_reset_at": resetAt,
							},
						},
					},
				},
				{
					ID:          3,
					Platform:    PlatformAntigravity,
					Priority:    2,
					Status:      StatusActive,
					Schedulable: true,
					Extra:       map[string]any{"mixed_scheduling": true},
				},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		svc := &GatewayService{
			accountRepo: repo,
			cache:       &mockGatewayCacheForPlatform{},
			cfg:         testConfig(),
		}

		acc, err := svc.selectAccountWithMixedScheduling(ctx, nil, "", "gemini-3-pro-preview", nil, PlatformGemini)
		require.NoError(t, err)
		require.NotNil(t, acc)
		require.Equal(t, int64(3), acc.ID)
	})

	t.Run("混合调度-Gemini家族限流不影响Claude调度", func(t *testing.T) {
		resetAt := time.Now().Add(10 * time.Minute).Format(time.RFC3339)
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{
					ID:          1,
					Platform:    PlatformAntigravity,
					Priority:    1,
					Status:      StatusActive,
					Schedulable: true,
					Extra: map[string]any{
						"mixed_scheduling": true,
						modelRateLimitsKey: map[string]any{
							antigravityGeminiModelRateLimitKey: map[string]any{
								"rate_limit_reset_at": resetAt,
							},
						},
					},
				},
				{ID: 2, Platform: PlatformAnthropic, Priority: 2, Status: StatusActive, Schedulable: true},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		svc := &GatewayService{
			accountRepo: repo,
			cache:       &mockGatewayCacheForPlatform{},
			cfg:         testConfig(),
		}

		acc, err := svc.selectAccountWithMixedScheduling(ctx, nil, "", "claude-sonnet-4-5", nil, PlatformAnthropic)
		require.NoError(t, err)
		require.NotNil(t, acc)
		require.Equal(t, int64(1), acc.ID)
	})

	t.Run("混合调度-路由优先选择路由账号", func(t *testing.T) {
		groupID := int64(30)
		requestedModel := "claude-sonnet-4-5"
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true},
				{ID: 2, Platform: PlatformAntigravity, Priority: 2, Status: StatusActive, Schedulable: true, Extra: map[string]any{"mixed_scheduling": true}},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{}

		groupRepo := &mockGroupRepoForGateway{
			groups: map[int64]*Group{
				groupID: {
					ID:                  groupID,
					Name:                "route-mixed-select",
					Platform:            PlatformAnthropic,
					Status:              StatusActive,
					Hydrated:            true,
					ModelRoutingEnabled: true,
					ModelRouting: map[string][]int64{
						requestedModel: {2},
					},
				},
			},
		}

		svc := &GatewayService{
			accountRepo: repo,
			cache:       cache,
			cfg:         testConfig(),
			groupRepo:   groupRepo,
		}

		acc, err := svc.selectAccountWithMixedScheduling(ctx, &groupID, "", requestedModel, nil, PlatformAnthropic)
		require.NoError(t, err)
		require.NotNil(t, acc)
		require.Equal(t, int64(2), acc.ID)
	})

	t.Run("混合调度-路由粘性命中", func(t *testing.T) {
		groupID := int64(31)
		requestedModel := "claude-sonnet-4-5"
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true},
				{ID: 2, Platform: PlatformAntigravity, Priority: 2, Status: StatusActive, Schedulable: true, Extra: map[string]any{"mixed_scheduling": true}, AccountGroups: []AccountGroup{{GroupID: groupID}}},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{
			sessionBindings: map[string]int64{"session-777": 2},
		}

		groupRepo := &mockGroupRepoForGateway{
			groups: map[int64]*Group{
				groupID: {
					ID:                  groupID,
					Name:                "route-mixed-sticky",
					Platform:            PlatformAnthropic,
					Status:              StatusActive,
					Hydrated:            true,
					ModelRoutingEnabled: true,
					ModelRouting: map[string][]int64{
						requestedModel: {2},
					},
				},
			},
		}

		svc := &GatewayService{
			accountRepo: repo,
			cache:       cache,
			cfg:         testConfig(),
			groupRepo:   groupRepo,
		}

		acc, err := svc.selectAccountWithMixedScheduling(ctx, &groupID, "session-777", requestedModel, nil, PlatformAnthropic)
		require.NoError(t, err)
		require.NotNil(t, acc)
		require.Equal(t, int64(2), acc.ID)
	})

	t.Run("混合调度-路由账号缺失回退", func(t *testing.T) {
		groupID := int64(32)
		requestedModel := "claude-3-5-sonnet-20241022"
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true},
				{ID: 2, Platform: PlatformAntigravity, Priority: 2, Status: StatusActive, Schedulable: true, Extra: map[string]any{"mixed_scheduling": true}},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{}

		groupRepo := &mockGroupRepoForGateway{
			groups: map[int64]*Group{
				groupID: {
					ID:                  groupID,
					Name:                "route-mixed-miss",
					Platform:            PlatformAnthropic,
					Status:              StatusActive,
					Hydrated:            true,
					ModelRoutingEnabled: true,
					ModelRouting: map[string][]int64{
						requestedModel: {99},
					},
				},
			},
		}

		svc := &GatewayService{
			accountRepo: repo,
			cache:       cache,
			cfg:         testConfig(),
			groupRepo:   groupRepo,
		}

		acc, err := svc.selectAccountWithMixedScheduling(ctx, &groupID, "", requestedModel, nil, PlatformAnthropic)
		require.NoError(t, err)
		require.NotNil(t, acc)
		require.Equal(t, int64(1), acc.ID)
	})

	t.Run("混合调度-路由账号未启用mixed_scheduling回退", func(t *testing.T) {
		groupID := int64(33)
		requestedModel := "claude-3-5-sonnet-20241022"
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true},
				{ID: 2, Platform: PlatformAntigravity, Priority: 2, Status: StatusActive, Schedulable: true}, // 未启用 mixed_scheduling
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{}

		groupRepo := &mockGroupRepoForGateway{
			groups: map[int64]*Group{
				groupID: {
					ID:                  groupID,
					Name:                "route-mixed-disabled",
					Platform:            PlatformAnthropic,
					Status:              StatusActive,
					Hydrated:            true,
					ModelRoutingEnabled: true,
					ModelRouting: map[string][]int64{
						requestedModel: {2},
					},
				},
			},
		}

		svc := &GatewayService{
			accountRepo: repo,
			cache:       cache,
			cfg:         testConfig(),
			groupRepo:   groupRepo,
		}

		acc, err := svc.selectAccountWithMixedScheduling(ctx, &groupID, "", requestedModel, nil, PlatformAnthropic)
		require.NoError(t, err)
		require.NotNil(t, acc)
		require.Equal(t, int64(1), acc.ID)
	})

	t.Run("混合调度-路由过滤覆盖", func(t *testing.T) {
		groupID := int64(35)
		requestedModel := "claude-3-5-sonnet-20241022"
		resetAt := time.Now().Add(10 * time.Minute)
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true},
				{ID: 2, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: false},
				{ID: 3, Platform: PlatformAntigravity, Priority: 1, Status: StatusActive, Schedulable: true},
				{
					ID:          4,
					Platform:    PlatformAnthropic,
					Priority:    1,
					Status:      StatusActive,
					Schedulable: true,
					Extra: map[string]any{
						"model_rate_limits": map[string]any{
							"claude-3-5-sonnet-20241022": map[string]any{
								"rate_limit_reset_at": resetAt.Format(time.RFC3339),
							},
						},
					},
				},
				{
					ID:          5,
					Platform:    PlatformAnthropic,
					Priority:    1,
					Status:      StatusActive,
					Schedulable: true,
					Credentials: map[string]any{"model_mapping": map[string]any{"claude-3-5-haiku-20241022": "claude-3-5-haiku-20241022"}},
				},
				{ID: 6, Platform: PlatformAnthropic, Priority: 2, Status: StatusActive, Schedulable: true},
				{ID: 7, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{}

		groupRepo := &mockGroupRepoForGateway{
			groups: map[int64]*Group{
				groupID: {
					ID:                  groupID,
					Name:                "route-mixed-filter",
					Platform:            PlatformAnthropic,
					Status:              StatusActive,
					Hydrated:            true,
					ModelRoutingEnabled: true,
					ModelRouting: map[string][]int64{
						requestedModel: {1, 2, 3, 4, 5, 6, 7},
					},
				},
			},
		}

		svc := &GatewayService{
			accountRepo: repo,
			cache:       cache,
			cfg:         testConfig(),
			groupRepo:   groupRepo,
		}

		excluded := map[int64]struct{}{1: {}}
		acc, err := svc.selectAccountWithMixedScheduling(ctx, &groupID, "", requestedModel, excluded, PlatformAnthropic)
		require.NoError(t, err)
		require.NotNil(t, acc)
		require.Equal(t, int64(7), acc.ID)
	})

	t.Run("混合调度-粘性命中分组账号", func(t *testing.T) {
		groupID := int64(34)
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, AccountGroups: []AccountGroup{{GroupID: groupID}}},
				{ID: 2, Platform: PlatformAnthropic, Priority: 2, Status: StatusActive, Schedulable: true, AccountGroups: []AccountGroup{{GroupID: groupID}}},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{
			sessionBindings: map[string]int64{"session-group": 1},
		}

		groupRepo := &mockGroupRepoForGateway{
			groups: map[int64]*Group{
				groupID: {
					ID:       groupID,
					Platform: PlatformAnthropic,
					Status:   StatusActive,
					Hydrated: true,
				},
			},
		}

		svc := &GatewayService{
			accountRepo: repo,
			cache:       cache,
			cfg:         testConfig(),
			groupRepo:   groupRepo,
		}

		acc, err := svc.selectAccountWithMixedScheduling(ctx, &groupID, "session-group", "claude-3-5-sonnet-20241022", nil, PlatformAnthropic)
		require.NoError(t, err)
		require.NotNil(t, acc)
		require.Equal(t, int64(1), acc.ID)
	})

	t.Run("混合调度-过滤未启用mixed_scheduling的antigravity账户", func(t *testing.T) {
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 2, Status: StatusActive, Schedulable: true},
				{ID: 2, Platform: PlatformAntigravity, Priority: 1, Status: StatusActive, Schedulable: true}, // 未启用 mixed_scheduling
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{}

		svc := &GatewayService{
			accountRepo: repo,
			cache:       cache,
			cfg:         testConfig(),
		}

		acc, err := svc.selectAccountWithMixedScheduling(ctx, nil, "", "claude-3-5-sonnet-20241022", nil, PlatformAnthropic)
		require.NoError(t, err)
		require.NotNil(t, acc)
		require.Equal(t, int64(1), acc.ID, "未启用mixed_scheduling的antigravity账户应被过滤")
		require.Equal(t, PlatformAnthropic, acc.Platform)
	})

	t.Run("混合调度-粘性会话命中启用mixed_scheduling的antigravity账户", func(t *testing.T) {
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true},
				{ID: 2, Platform: PlatformAntigravity, Priority: 2, Status: StatusActive, Schedulable: true, Extra: map[string]any{"mixed_scheduling": true}},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{
			sessionBindings: map[string]int64{"session-123": 2},
		}

		svc := &GatewayService{
			accountRepo: repo,
			cache:       cache,
			cfg:         testConfig(),
		}

		acc, err := svc.selectAccountWithMixedScheduling(ctx, nil, "session-123", "claude-sonnet-4-5", nil, PlatformAnthropic)
		require.NoError(t, err)
		require.NotNil(t, acc)
		require.Equal(t, int64(2), acc.ID, "应返回粘性会话绑定的启用mixed_scheduling的antigravity账户")
	})

	t.Run("混合调度-粘性会话命中未启用mixed_scheduling的antigravity账户-降级选择", func(t *testing.T) {
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true},
				{ID: 2, Platform: PlatformAntigravity, Priority: 2, Status: StatusActive, Schedulable: true}, // 未启用 mixed_scheduling
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{
			sessionBindings: map[string]int64{"session-123": 2},
		}

		svc := &GatewayService{
			accountRepo: repo,
			cache:       cache,
			cfg:         testConfig(),
		}

		acc, err := svc.selectAccountWithMixedScheduling(ctx, nil, "session-123", "claude-3-5-sonnet-20241022", nil, PlatformAnthropic)
		require.NoError(t, err)
		require.NotNil(t, acc)
		require.Equal(t, int64(1), acc.ID, "粘性会话绑定的账户未启用mixed_scheduling，应降级选择anthropic账户")
	})

	t.Run("混合调度-粘性会话不可调度-清理并回退", func(t *testing.T) {
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAntigravity, Priority: 1, Status: StatusDisabled, Schedulable: true, Extra: map[string]any{"mixed_scheduling": true}},
				{ID: 2, Platform: PlatformAnthropic, Priority: 2, Status: StatusActive, Schedulable: true},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{
			sessionBindings: map[string]int64{"session-123": 1},
		}

		svc := &GatewayService{
			accountRepo: repo,
			cache:       cache,
			cfg:         testConfig(),
		}

		acc, err := svc.selectAccountWithMixedScheduling(ctx, nil, "session-123", "claude-3-5-sonnet-20241022", nil, PlatformAnthropic)
		require.NoError(t, err)
		require.NotNil(t, acc)
		require.Equal(t, int64(2), acc.ID)
		require.Equal(t, 1, cache.deletedSessions["session-123"])
		require.Equal(t, int64(2), cache.sessionBindings["session-123"])
	})

	t.Run("混合调度-路由粘性不可调度-清理并回退", func(t *testing.T) {
		groupID := int64(12)
		requestedModel := "claude-3-5-sonnet-20241022"
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAntigravity, Priority: 1, Status: StatusDisabled, Schedulable: true, Extra: map[string]any{"mixed_scheduling": true}},
				{ID: 2, Platform: PlatformAnthropic, Priority: 2, Status: StatusActive, Schedulable: true},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{
			sessionBindings: map[string]int64{"session-123": 1},
		}

		groupRepo := &mockGroupRepoForGateway{
			groups: map[int64]*Group{
				groupID: {
					ID:                  groupID,
					Name:                "route-mixed",
					Platform:            PlatformAnthropic,
					Status:              StatusActive,
					Hydrated:            true,
					ModelRoutingEnabled: true,
					ModelRouting: map[string][]int64{
						requestedModel: {1, 2},
					},
				},
			},
		}

		svc := &GatewayService{
			accountRepo: repo,
			cache:       cache,
			cfg:         testConfig(),
			groupRepo:   groupRepo,
		}

		acc, err := svc.selectAccountWithMixedScheduling(ctx, &groupID, "session-123", requestedModel, nil, PlatformAnthropic)
		require.NoError(t, err)
		require.NotNil(t, acc)
		require.Equal(t, int64(2), acc.ID)
		require.Equal(t, 1, cache.deletedSessions["session-123"])
		require.Equal(t, int64(2), cache.sessionBindings["session-123"])
	})

	t.Run("混合调度-仅有启用mixed_scheduling的antigravity账户", func(t *testing.T) {
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAntigravity, Priority: 1, Status: StatusActive, Schedulable: true, Extra: map[string]any{"mixed_scheduling": true}},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{}

		svc := &GatewayService{
			accountRepo: repo,
			cache:       cache,
			cfg:         testConfig(),
		}

		acc, err := svc.selectAccountWithMixedScheduling(ctx, nil, "", "claude-sonnet-4-5", nil, PlatformAnthropic)
		require.NoError(t, err)
		require.NotNil(t, acc)
		require.Equal(t, int64(1), acc.ID)
		require.Equal(t, PlatformAntigravity, acc.Platform)
	})

	t.Run("混合调度-无可用账户", func(t *testing.T) {
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAntigravity, Priority: 1, Status: StatusActive, Schedulable: true}, // 未启用 mixed_scheduling
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{}

		svc := &GatewayService{
			accountRepo: repo,
			cache:       cache,
			cfg:         testConfig(),
		}

		acc, err := svc.selectAccountWithMixedScheduling(ctx, nil, "", "claude-3-5-sonnet-20241022", nil, PlatformAnthropic)
		require.Error(t, err)
		require.Nil(t, acc)
		require.ErrorIs(t, err, ErrNoAvailableAccounts)
	})

	t.Run("混合调度-不支持模型返回错误", func(t *testing.T) {
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{
					ID:          1,
					Platform:    PlatformAnthropic,
					Priority:    1,
					Status:      StatusActive,
					Schedulable: true,
					Credentials: map[string]any{"model_mapping": map[string]any{"claude-3-5-haiku-20241022": "claude-3-5-haiku-20241022"}},
				},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{}

		svc := &GatewayService{
			accountRepo: repo,
			cache:       cache,
			cfg:         testConfig(),
		}

		acc, err := svc.selectAccountWithMixedScheduling(ctx, nil, "", "claude-3-5-sonnet-20241022", nil, PlatformAnthropic)
		require.Error(t, err)
		require.Nil(t, acc)
		require.Contains(t, err.Error(), "supporting model")
	})

	t.Run("混合调度-优先未使用账号", func(t *testing.T) {
		lastUsed := time.Now().Add(-2 * time.Hour)
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, LastUsedAt: &lastUsed},
				{ID: 2, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{}

		svc := &GatewayService{
			accountRepo: repo,
			cache:       cache,
			cfg:         testConfig(),
		}

		acc, err := svc.selectAccountWithMixedScheduling(ctx, nil, "", "claude-3-5-sonnet-20241022", nil, PlatformAnthropic)
		require.NoError(t, err)
		require.NotNil(t, acc)
		require.Equal(t, int64(2), acc.ID)
	})
}

// TestAccount_IsMixedSchedulingEnabled 测试混合调度开关检查
func TestAccount_IsMixedSchedulingEnabled(t *testing.T) {
	tests := []struct {
		name     string
		account  Account
		expected bool
	}{
		{
			name:     "非antigravity平台-返回false",
			account:  Account{Platform: PlatformAnthropic},
			expected: false,
		},
		{
			name:     "antigravity平台-无extra-返回false",
			account:  Account{Platform: PlatformAntigravity},
			expected: false,
		},
		{
			name:     "antigravity平台-extra无mixed_scheduling-返回false",
			account:  Account{Platform: PlatformAntigravity, Extra: map[string]any{}},
			expected: false,
		},
		{
			name:     "antigravity平台-mixed_scheduling=false-返回false",
			account:  Account{Platform: PlatformAntigravity, Extra: map[string]any{"mixed_scheduling": false}},
			expected: false,
		},
		{
			name:     "antigravity平台-mixed_scheduling=true-返回true",
			account:  Account{Platform: PlatformAntigravity, Extra: map[string]any{"mixed_scheduling": true}},
			expected: true,
		},
		{
			name:     "antigravity平台-mixed_scheduling非bool类型-返回false",
			account:  Account{Platform: PlatformAntigravity, Extra: map[string]any{"mixed_scheduling": "true"}},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.account.IsMixedSchedulingEnabled()
			require.Equal(t, tt.expected, got)
		})
	}
}

// mockConcurrencyService for testing
type mockConcurrencyService struct {
	accountLoads      map[int64]*AccountLoadInfo
	accountWaitCounts map[int64]int
	acquireResults    map[int64]bool
}

func (m *mockConcurrencyService) GetAccountsLoadBatch(ctx context.Context, accounts []AccountWithConcurrency) (map[int64]*AccountLoadInfo, error) {
	if m.accountLoads == nil {
		return map[int64]*AccountLoadInfo{}, nil
	}
	result := make(map[int64]*AccountLoadInfo)
	for _, acc := range accounts {
		if load, ok := m.accountLoads[acc.ID]; ok {
			result[acc.ID] = load
		} else {
			result[acc.ID] = &AccountLoadInfo{
				AccountID:          acc.ID,
				CurrentConcurrency: 0,
				WaitingCount:       0,
				LoadRate:           0,
			}
		}
	}
	return result, nil
}

func (m *mockConcurrencyService) GetAccountWaitingCount(ctx context.Context, accountID int64) (int, error) {
	if m.accountWaitCounts == nil {
		return 0, nil
	}
	return m.accountWaitCounts[accountID], nil
}

type mockConcurrencyCache struct {
	acquireAccountCalls int
	loadBatchCalls      int
	acquireResults      map[int64]bool
	loadBatchErr        error
	loadMap             map[int64]*AccountLoadInfo
	waitCounts          map[int64]int
	skipDefaultLoad     bool
}

func (m *mockConcurrencyCache) AcquireAccountSlot(ctx context.Context, accountID int64, maxConcurrency int, requestID string) (bool, error) {
	m.acquireAccountCalls++
	if m.acquireResults != nil {
		if result, ok := m.acquireResults[accountID]; ok {
			return result, nil
		}
	}
	return true, nil
}

func (m *mockConcurrencyCache) ReleaseAccountSlot(ctx context.Context, accountID int64, requestID string) error {
	return nil
}

func (m *mockConcurrencyCache) GetAccountConcurrency(ctx context.Context, accountID int64) (int, error) {
	return 0, nil
}

func (m *mockConcurrencyCache) GetAccountConcurrencyBatch(ctx context.Context, accountIDs []int64) (map[int64]int, error) {
	result := make(map[int64]int, len(accountIDs))
	for _, accountID := range accountIDs {
		result[accountID] = 0
	}
	return result, nil
}

func (m *mockConcurrencyCache) IncrementAccountWaitCount(ctx context.Context, accountID int64, maxWait int) (bool, error) {
	return true, nil
}

func (m *mockConcurrencyCache) DecrementAccountWaitCount(ctx context.Context, accountID int64) error {
	return nil
}

func (m *mockConcurrencyCache) GetAccountWaitingCount(ctx context.Context, accountID int64) (int, error) {
	if m.waitCounts != nil {
		if count, ok := m.waitCounts[accountID]; ok {
			return count, nil
		}
	}
	return 0, nil
}

func (m *mockConcurrencyCache) AcquireUserSlot(ctx context.Context, userID int64, maxConcurrency int, requestID string) (bool, error) {
	return true, nil
}

func (m *mockConcurrencyCache) ReleaseUserSlot(ctx context.Context, userID int64, requestID string) error {
	return nil
}

func (m *mockConcurrencyCache) GetUserConcurrency(ctx context.Context, userID int64) (int, error) {
	return 0, nil
}

func (m *mockConcurrencyCache) IncrementWaitCount(ctx context.Context, userID int64, maxWait int) (bool, error) {
	return true, nil
}

func (m *mockConcurrencyCache) DecrementWaitCount(ctx context.Context, userID int64) error {
	return nil
}

func (m *mockConcurrencyCache) GetAccountsLoadBatch(ctx context.Context, accounts []AccountWithConcurrency) (map[int64]*AccountLoadInfo, error) {
	m.loadBatchCalls++
	if m.loadBatchErr != nil {
		return nil, m.loadBatchErr
	}
	result := make(map[int64]*AccountLoadInfo, len(accounts))
	if m.skipDefaultLoad && m.loadMap != nil {
		for _, acc := range accounts {
			if load, ok := m.loadMap[acc.ID]; ok {
				result[acc.ID] = load
			}
		}
		return result, nil
	}
	for _, acc := range accounts {
		if m.loadMap != nil {
			if load, ok := m.loadMap[acc.ID]; ok {
				result[acc.ID] = load
				continue
			}
		}
		result[acc.ID] = &AccountLoadInfo{
			AccountID:          acc.ID,
			CurrentConcurrency: 0,
			WaitingCount:       0,
			LoadRate:           0,
		}
	}
	return result, nil
}

func (m *mockConcurrencyCache) CleanupExpiredAccountSlots(ctx context.Context, accountID int64) error {
	return nil
}

func (m *mockConcurrencyCache) CleanupExpiredAccountSlotKeys(ctx context.Context) error {
	return nil
}

func (m *mockConcurrencyCache) CleanupStaleProcessSlots(ctx context.Context, activeRequestPrefix string) error {
	return nil
}

func (m *mockConcurrencyCache) GetUsersLoadBatch(ctx context.Context, users []UserWithConcurrency) (map[int64]*UserLoadInfo, error) {
	result := make(map[int64]*UserLoadInfo, len(users))
	for _, user := range users {
		result[user.ID] = &UserLoadInfo{
			UserID:             user.ID,
			CurrentConcurrency: 0,
			WaitingCount:       0,
			LoadRate:           0,
		}
	}
	return result, nil
}

// TestGatewayService_SelectAccountWithLoadAwareness tests load-aware account selection
func TestGatewayService_SelectAccountWithLoadAwareness(t *testing.T) {
	ctx := context.Background()

	t.Run("禁用负载批量查询-降级到传统选择", func(t *testing.T) {
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5},
				{ID: 2, Platform: PlatformAnthropic, Priority: 2, Status: StatusActive, Schedulable: true, Concurrency: 5},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{}

		cfg := testConfig()
		cfg.Gateway.Scheduling.LoadBatchEnabled = false

		svc := &GatewayService{
			accountRepo:        repo,
			cache:              cache,
			cfg:                cfg,
			concurrencyService: nil, // No concurrency service
		}

		result, err := svc.SelectAccountWithLoadAwareness(ctx, nil, "", "claude-3-5-sonnet-20241022", nil, "", int64(0))
		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.Account)
		require.Equal(t, int64(1), result.Account.ID, "应选择优先级最高的账号")
	})

	t.Run("模型路由-无ConcurrencyService也生效", func(t *testing.T) {
		groupID := int64(1)
		sessionHash := "sticky"

		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5, AccountGroups: []AccountGroup{{GroupID: groupID}}},
				{ID: 2, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5, AccountGroups: []AccountGroup{{GroupID: groupID}}},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{
			sessionBindings: map[string]int64{sessionHash: 1},
		}

		groupRepo := &mockGroupRepoForGateway{
			groups: map[int64]*Group{
				groupID: {
					ID:                  groupID,
					Platform:            PlatformAnthropic,
					Status:              StatusActive,
					Hydrated:            true,
					ModelRoutingEnabled: true,
					ModelRouting: map[string][]int64{
						"claude-a": {1},
						"claude-b": {2},
					},
				},
			},
		}

		cfg := testConfig()
		cfg.Gateway.Scheduling.LoadBatchEnabled = true

		svc := &GatewayService{
			accountRepo:        repo,
			groupRepo:          groupRepo,
			cache:              cache,
			cfg:                cfg,
			concurrencyService: nil, // legacy path
		}

		result, err := svc.SelectAccountWithLoadAwareness(ctx, &groupID, sessionHash, "claude-b", nil, "", int64(0))
		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.Account)
		require.Equal(t, int64(2), result.Account.ID, "切换到 claude-b 时应按模型路由切换账号")
		require.Equal(t, int64(2), cache.sessionBindings[sessionHash], "粘性绑定应更新为路由选择的账号")
	})

	t.Run("无ConcurrencyService-降级到传统选择", func(t *testing.T) {
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 2, Status: StatusActive, Schedulable: true, Concurrency: 5},
				{ID: 2, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{}

		cfg := testConfig()
		cfg.Gateway.Scheduling.LoadBatchEnabled = true

		svc := &GatewayService{
			accountRepo:        repo,
			cache:              cache,
			cfg:                cfg,
			concurrencyService: nil,
		}

		result, err := svc.SelectAccountWithLoadAwareness(ctx, nil, "", "claude-3-5-sonnet-20241022", nil, "", int64(0))
		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.Account)
		require.Equal(t, int64(2), result.Account.ID, "应选择优先级最高的账号")
	})

	t.Run("排除账号-不选择被排除的账号", func(t *testing.T) {
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5},
				{ID: 2, Platform: PlatformAnthropic, Priority: 2, Status: StatusActive, Schedulable: true, Concurrency: 5},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{}

		cfg := testConfig()
		cfg.Gateway.Scheduling.LoadBatchEnabled = false

		svc := &GatewayService{
			accountRepo:        repo,
			cache:              cache,
			cfg:                cfg,
			concurrencyService: nil,
		}

		excludedIDs := map[int64]struct{}{1: {}}
		result, err := svc.SelectAccountWithLoadAwareness(ctx, nil, "", "claude-3-5-sonnet-20241022", excludedIDs, "", int64(0))
		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.Account)
		require.Equal(t, int64(2), result.Account.ID, "不应选择被排除的账号")
	})

	t.Run("粘性命中-不调用GetByID", func(t *testing.T) {
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{
			sessionBindings: map[string]int64{"sticky": 1},
		}

		cfg := testConfig()
		cfg.Gateway.Scheduling.LoadBatchEnabled = true

		concurrencyCache := &mockConcurrencyCache{}

		svc := &GatewayService{
			accountRepo:        repo,
			cache:              cache,
			cfg:                cfg,
			concurrencyService: NewConcurrencyService(concurrencyCache),
		}

		result, err := svc.SelectAccountWithLoadAwareness(ctx, nil, "sticky", "claude-3-5-sonnet-20241022", nil, "", int64(0))
		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.Account)
		require.Equal(t, int64(1), result.Account.ID)
		require.Equal(t, 0, repo.getByIDCalls, "粘性命中不应调用GetByID")
		require.Equal(t, 0, concurrencyCache.loadBatchCalls, "粘性命中应在负载批量查询前返回")
	})

	t.Run("粘性账号不在候选集-回退负载感知选择", func(t *testing.T) {
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 2, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{
			sessionBindings: map[string]int64{"sticky": 1},
		}

		cfg := testConfig()
		cfg.Gateway.Scheduling.LoadBatchEnabled = true

		concurrencyCache := &mockConcurrencyCache{}

		svc := &GatewayService{
			accountRepo:        repo,
			cache:              cache,
			cfg:                cfg,
			concurrencyService: NewConcurrencyService(concurrencyCache),
		}

		result, err := svc.SelectAccountWithLoadAwareness(ctx, nil, "sticky", "claude-3-5-sonnet-20241022", nil, "", int64(0))
		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.Account)
		require.Equal(t, int64(2), result.Account.ID, "粘性账号不在候选集时应回退到可用账号")
		require.Equal(t, 0, repo.getByIDCalls, "粘性账号缺失不应回退到GetByID")
		require.Equal(t, 1, concurrencyCache.loadBatchCalls, "应继续进行负载批量查询")
	})

	t.Run("粘性账号禁用-清理会话并回退选择", func(t *testing.T) {
		testCtx := context.WithValue(ctx, ctxkey.ForcePlatform, PlatformAnthropic)
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: false, Concurrency: 5},
				{ID: 2, Platform: PlatformAnthropic, Priority: 2, Status: StatusActive, Schedulable: true, Concurrency: 5},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}
		repo.listPlatformFunc = func(ctx context.Context, platform string) ([]Account, error) {
			return repo.accounts, nil
		}

		cache := &mockGatewayCacheForPlatform{
			sessionBindings: map[string]int64{"sticky": 1},
		}

		cfg := testConfig()
		cfg.Gateway.Scheduling.LoadBatchEnabled = true

		concurrencyCache := &mockConcurrencyCache{}

		svc := &GatewayService{
			accountRepo:        repo,
			cache:              cache,
			cfg:                cfg,
			concurrencyService: NewConcurrencyService(concurrencyCache),
		}

		result, err := svc.SelectAccountWithLoadAwareness(testCtx, nil, "sticky", "claude-3-5-sonnet-20241022", nil, "", int64(0))
		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.Account)
		require.Equal(t, int64(2), result.Account.ID, "粘性账号禁用时应回退到可用账号")
		updatedID, ok := cache.sessionBindings["sticky"]
		require.True(t, ok, "粘性会话应更新绑定")
		require.Equal(t, int64(2), updatedID, "粘性会话应绑定到新账号")
	})

	t.Run("无可用账号-返回错误", func(t *testing.T) {
		repo := &mockAccountRepoForPlatform{
			accounts:     []Account{},
			accountsByID: map[int64]*Account{},
		}

		cache := &mockGatewayCacheForPlatform{}

		cfg := testConfig()
		cfg.Gateway.Scheduling.LoadBatchEnabled = false

		svc := &GatewayService{
			accountRepo:        repo,
			cache:              cache,
			cfg:                cfg,
			concurrencyService: nil,
		}

		result, err := svc.SelectAccountWithLoadAwareness(ctx, nil, "", "claude-3-5-sonnet-20241022", nil, "", int64(0))
		require.Error(t, err)
		require.Nil(t, result)
		require.ErrorIs(t, err, ErrNoAvailableAccounts)
	})

	t.Run("过滤不可调度账号-限流账号被跳过", func(t *testing.T) {
		now := time.Now()
		resetAt := now.Add(10 * time.Minute)

		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5, RateLimitResetAt: &resetAt},
				{ID: 2, Platform: PlatformAnthropic, Priority: 2, Status: StatusActive, Schedulable: true, Concurrency: 5},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{}
		cfg := testConfig()
		cfg.Gateway.Scheduling.LoadBatchEnabled = false

		svc := &GatewayService{
			accountRepo:        repo,
			cache:              cache,
			cfg:                cfg,
			concurrencyService: nil,
		}

		result, err := svc.SelectAccountWithLoadAwareness(ctx, nil, "", "claude-3-5-sonnet-20241022", nil, "", int64(0))
		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.Account)
		require.Equal(t, int64(2), result.Account.ID, "应跳过限流账号，选择可用账号")
	})

	t.Run("过滤不可调度账号-过载账号被跳过", func(t *testing.T) {
		now := time.Now()
		overloadUntil := now.Add(10 * time.Minute)

		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5, OverloadUntil: &overloadUntil},
				{ID: 2, Platform: PlatformAnthropic, Priority: 2, Status: StatusActive, Schedulable: true, Concurrency: 5},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{}
		cfg := testConfig()
		cfg.Gateway.Scheduling.LoadBatchEnabled = false

		svc := &GatewayService{
			accountRepo:        repo,
			cache:              cache,
			cfg:                cfg,
			concurrencyService: nil,
		}

		result, err := svc.SelectAccountWithLoadAwareness(ctx, nil, "", "claude-3-5-sonnet-20241022", nil, "", int64(0))
		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.Account)
		require.Equal(t, int64(2), result.Account.ID, "应跳过过载账号，选择可用账号")
	})

	t.Run("粘性账号槽位满-返回粘性等待计划", func(t *testing.T) {
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{
			sessionBindings: map[string]int64{"sticky": 1},
		}

		cfg := testConfig()
		cfg.Gateway.Scheduling.LoadBatchEnabled = true
		cfg.Gateway.Scheduling.StickySessionMaxWaiting = 1

		concurrencyCache := &mockConcurrencyCache{
			acquireResults: map[int64]bool{1: false},
			waitCounts:     map[int64]int{1: 0},
		}

		svc := &GatewayService{
			accountRepo:        repo,
			cache:              cache,
			cfg:                cfg,
			concurrencyService: NewConcurrencyService(concurrencyCache),
		}

		result, err := svc.SelectAccountWithLoadAwareness(ctx, nil, "sticky", "claude-3-5-sonnet-20241022", nil, "", int64(0))
		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.WaitPlan)
		require.Equal(t, int64(1), result.Account.ID)
		require.Equal(t, 0, concurrencyCache.loadBatchCalls)
	})

	t.Run("负载批量查询失败-降级旧顺序选择", func(t *testing.T) {
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 2, Status: StatusActive, Schedulable: true, Concurrency: 5},
				{ID: 2, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{}

		cfg := testConfig()
		cfg.Gateway.Scheduling.LoadBatchEnabled = true

		concurrencyCache := &mockConcurrencyCache{
			loadBatchErr: errors.New("load batch failed"),
		}

		svc := &GatewayService{
			accountRepo:        repo,
			cache:              cache,
			cfg:                cfg,
			concurrencyService: NewConcurrencyService(concurrencyCache),
		}

		result, err := svc.SelectAccountWithLoadAwareness(ctx, nil, "legacy", "claude-3-5-sonnet-20241022", nil, "", int64(0))
		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.Account)
		require.Equal(t, int64(2), result.Account.ID)
		require.Equal(t, int64(2), cache.sessionBindings["legacy"])
	})

	t.Run("模型路由-粘性账号等待计划", func(t *testing.T) {
		groupID := int64(20)
		sessionHash := "route-sticky"

		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5},
				{ID: 2, Platform: PlatformAnthropic, Priority: 2, Status: StatusActive, Schedulable: true, Concurrency: 5},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{
			sessionBindings: map[string]int64{sessionHash: 1},
		}

		groupRepo := &mockGroupRepoForGateway{
			groups: map[int64]*Group{
				groupID: {
					ID:                  groupID,
					Platform:            PlatformAnthropic,
					Status:              StatusActive,
					Hydrated:            true,
					ModelRoutingEnabled: true,
					ModelRouting: map[string][]int64{
						"claude-3-5-sonnet-20241022": {1, 2},
					},
				},
			},
		}

		cfg := testConfig()
		cfg.Gateway.Scheduling.LoadBatchEnabled = true
		cfg.Gateway.Scheduling.StickySessionMaxWaiting = 1

		concurrencyCache := &mockConcurrencyCache{
			acquireResults: map[int64]bool{1: false},
			waitCounts:     map[int64]int{1: 0},
		}

		svc := &GatewayService{
			accountRepo:        repo,
			groupRepo:          groupRepo,
			cache:              cache,
			cfg:                cfg,
			concurrencyService: NewConcurrencyService(concurrencyCache),
		}

		result, err := svc.SelectAccountWithLoadAwareness(ctx, &groupID, sessionHash, "claude-3-5-sonnet-20241022", nil, "", int64(0))
		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.WaitPlan)
		require.Equal(t, int64(1), result.Account.ID)
	})

	t.Run("模型路由-粘性账号命中", func(t *testing.T) {
		groupID := int64(20)
		sessionHash := "route-hit"

		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5},
				{ID: 2, Platform: PlatformAnthropic, Priority: 2, Status: StatusActive, Schedulable: true, Concurrency: 5},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{
			sessionBindings: map[string]int64{sessionHash: 1},
		}

		groupRepo := &mockGroupRepoForGateway{
			groups: map[int64]*Group{
				groupID: {
					ID:                  groupID,
					Platform:            PlatformAnthropic,
					Status:              StatusActive,
					Hydrated:            true,
					ModelRoutingEnabled: true,
					ModelRouting: map[string][]int64{
						"claude-3-5-sonnet-20241022": {1, 2},
					},
				},
			},
		}

		cfg := testConfig()
		cfg.Gateway.Scheduling.LoadBatchEnabled = true

		concurrencyCache := &mockConcurrencyCache{}

		svc := &GatewayService{
			accountRepo:        repo,
			groupRepo:          groupRepo,
			cache:              cache,
			cfg:                cfg,
			concurrencyService: NewConcurrencyService(concurrencyCache),
		}

		result, err := svc.SelectAccountWithLoadAwareness(ctx, &groupID, sessionHash, "claude-3-5-sonnet-20241022", nil, "", int64(0))
		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.Account)
		require.Equal(t, int64(1), result.Account.ID)
		require.Equal(t, 0, concurrencyCache.loadBatchCalls)
	})

	t.Run("模型路由-粘性账号缺失-清理并回退", func(t *testing.T) {
		groupID := int64(22)
		sessionHash := "route-missing"

		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 2, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{
			sessionBindings: map[string]int64{sessionHash: 1},
		}

		groupRepo := &mockGroupRepoForGateway{
			groups: map[int64]*Group{
				groupID: {
					ID:                  groupID,
					Platform:            PlatformAnthropic,
					Status:              StatusActive,
					Hydrated:            true,
					ModelRoutingEnabled: true,
					ModelRouting: map[string][]int64{
						"claude-3-5-sonnet-20241022": {1, 2},
					},
				},
			},
		}

		cfg := testConfig()
		cfg.Gateway.Scheduling.LoadBatchEnabled = true

		concurrencyCache := &mockConcurrencyCache{}

		svc := &GatewayService{
			accountRepo:        repo,
			groupRepo:          groupRepo,
			cache:              cache,
			cfg:                cfg,
			concurrencyService: NewConcurrencyService(concurrencyCache),
		}

		result, err := svc.SelectAccountWithLoadAwareness(ctx, &groupID, sessionHash, "claude-3-5-sonnet-20241022", nil, "", int64(0))
		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.Account)
		require.Equal(t, int64(2), result.Account.ID)
		require.Equal(t, 1, cache.deletedSessions[sessionHash])
		require.Equal(t, int64(2), cache.sessionBindings[sessionHash])
	})

	t.Run("模型路由-按负载选择账号", func(t *testing.T) {
		groupID := int64(21)

		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5},
				{ID: 2, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{}

		groupRepo := &mockGroupRepoForGateway{
			groups: map[int64]*Group{
				groupID: {
					ID:                  groupID,
					Platform:            PlatformAnthropic,
					Status:              StatusActive,
					Hydrated:            true,
					ModelRoutingEnabled: true,
					ModelRouting: map[string][]int64{
						"claude-3-5-sonnet-20241022": {1, 2},
					},
				},
			},
		}

		cfg := testConfig()
		cfg.Gateway.Scheduling.LoadBatchEnabled = true

		concurrencyCache := &mockConcurrencyCache{
			loadMap: map[int64]*AccountLoadInfo{
				1: {AccountID: 1, LoadRate: 80},
				2: {AccountID: 2, LoadRate: 20},
			},
		}

		svc := &GatewayService{
			accountRepo:        repo,
			groupRepo:          groupRepo,
			cache:              cache,
			cfg:                cfg,
			concurrencyService: NewConcurrencyService(concurrencyCache),
		}

		result, err := svc.SelectAccountWithLoadAwareness(ctx, &groupID, "route", "claude-3-5-sonnet-20241022", nil, "", int64(0))
		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.Account)
		require.Equal(t, int64(2), result.Account.ID)
		require.Equal(t, int64(2), cache.sessionBindings["route"])
	})

	t.Run("模型路由-路由账号全满返回等待计划", func(t *testing.T) {
		groupID := int64(23)

		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5},
				{ID: 2, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{}

		groupRepo := &mockGroupRepoForGateway{
			groups: map[int64]*Group{
				groupID: {
					ID:                  groupID,
					Platform:            PlatformAnthropic,
					Status:              StatusActive,
					Hydrated:            true,
					ModelRoutingEnabled: true,
					ModelRouting: map[string][]int64{
						"claude-3-5-sonnet-20241022": {1, 2},
					},
				},
			},
		}

		cfg := testConfig()
		cfg.Gateway.Scheduling.LoadBatchEnabled = true

		concurrencyCache := &mockConcurrencyCache{
			acquireResults: map[int64]bool{1: false, 2: false},
			loadMap: map[int64]*AccountLoadInfo{
				1: {AccountID: 1, LoadRate: 10},
				2: {AccountID: 2, LoadRate: 20},
			},
		}

		svc := &GatewayService{
			accountRepo:        repo,
			groupRepo:          groupRepo,
			cache:              cache,
			cfg:                cfg,
			concurrencyService: NewConcurrencyService(concurrencyCache),
		}

		result, err := svc.SelectAccountWithLoadAwareness(ctx, &groupID, "route-full", "claude-3-5-sonnet-20241022", nil, "", int64(0))
		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.WaitPlan)
		require.Equal(t, int64(1), result.Account.ID)
	})

	t.Run("模型路由-路由账号全满-回退普通选择", func(t *testing.T) {
		groupID := int64(22)

		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5},
				{ID: 2, Platform: PlatformAnthropic, Priority: 2, Status: StatusActive, Schedulable: true, Concurrency: 5},
				{ID: 3, Platform: PlatformAnthropic, Priority: 0, Status: StatusActive, Schedulable: true, Concurrency: 5},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{}

		groupRepo := &mockGroupRepoForGateway{
			groups: map[int64]*Group{
				groupID: {
					ID:                  groupID,
					Platform:            PlatformAnthropic,
					Status:              StatusActive,
					Hydrated:            true,
					ModelRoutingEnabled: true,
					ModelRouting: map[string][]int64{
						"claude-3-5-sonnet-20241022": {1, 2},
					},
				},
			},
		}

		cfg := testConfig()
		cfg.Gateway.Scheduling.LoadBatchEnabled = true

		concurrencyCache := &mockConcurrencyCache{
			loadMap: map[int64]*AccountLoadInfo{
				1: {AccountID: 1, LoadRate: 100},
				2: {AccountID: 2, LoadRate: 100},
				3: {AccountID: 3, LoadRate: 0},
			},
		}

		svc := &GatewayService{
			accountRepo:        repo,
			groupRepo:          groupRepo,
			cache:              cache,
			cfg:                cfg,
			concurrencyService: NewConcurrencyService(concurrencyCache),
		}

		result, err := svc.SelectAccountWithLoadAwareness(ctx, &groupID, "fallback", "claude-3-5-sonnet-20241022", nil, "", int64(0))
		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.Account)
		require.Equal(t, int64(3), result.Account.ID)
		require.Equal(t, int64(3), cache.sessionBindings["fallback"])
	})

	t.Run("负载批量失败且无法获取-兜底等待", func(t *testing.T) {
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5},
				{ID: 2, Platform: PlatformAnthropic, Priority: 2, Status: StatusActive, Schedulable: true, Concurrency: 5},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{}

		cfg := testConfig()
		cfg.Gateway.Scheduling.LoadBatchEnabled = true

		concurrencyCache := &mockConcurrencyCache{
			loadBatchErr:   errors.New("load batch failed"),
			acquireResults: map[int64]bool{1: false, 2: false},
		}

		svc := &GatewayService{
			accountRepo:        repo,
			cache:              cache,
			cfg:                cfg,
			concurrencyService: NewConcurrencyService(concurrencyCache),
		}

		result, err := svc.SelectAccountWithLoadAwareness(ctx, nil, "", "claude-3-5-sonnet-20241022", nil, "", int64(0))
		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.WaitPlan)
		require.Equal(t, int64(1), result.Account.ID)
	})

	t.Run("Gemini负载排序-优先OAuth", func(t *testing.T) {
		groupID := int64(24)

		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformGemini, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5, Type: AccountTypeAPIKey},
				{ID: 2, Platform: PlatformGemini, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5, Type: AccountTypeOAuth},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{}

		groupRepo := &mockGroupRepoForGateway{
			groups: map[int64]*Group{
				groupID: {
					ID:       groupID,
					Platform: PlatformGemini,
					Status:   StatusActive,
					Hydrated: true,
				},
			},
		}

		cfg := testConfig()
		cfg.Gateway.Scheduling.LoadBatchEnabled = true

		concurrencyCache := &mockConcurrencyCache{
			loadMap: map[int64]*AccountLoadInfo{
				1: {AccountID: 1, LoadRate: 10},
				2: {AccountID: 2, LoadRate: 10},
			},
		}

		svc := &GatewayService{
			accountRepo:        repo,
			groupRepo:          groupRepo,
			cache:              cache,
			cfg:                cfg,
			concurrencyService: NewConcurrencyService(concurrencyCache),
		}

		result, err := svc.SelectAccountWithLoadAwareness(ctx, &groupID, "gemini", "gemini-2.5-pro", nil, "", int64(0))
		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.Account)
		require.Equal(t, int64(2), result.Account.ID)
	})

	t.Run("模型路由-过滤路径覆盖", func(t *testing.T) {
		groupID := int64(70)
		now := time.Now().Add(10 * time.Minute)
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5},
				{ID: 3, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: false, Concurrency: 5},
				{ID: 4, Platform: PlatformAntigravity, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5},
				{
					ID:          5,
					Platform:    PlatformAnthropic,
					Priority:    1,
					Status:      StatusActive,
					Schedulable: true,
					Concurrency: 5,
					Extra: map[string]any{
						"model_rate_limits": map[string]any{
							"claude-3-5-sonnet-20241022": map[string]any{
								"rate_limit_reset_at": now.Format(time.RFC3339),
							},
						},
					},
				},
				{
					ID:          6,
					Platform:    PlatformAnthropic,
					Priority:    1,
					Status:      StatusActive,
					Schedulable: true,
					Concurrency: 5,
					Credentials: map[string]any{"model_mapping": map[string]any{"claude-3-5-haiku-20241022": "claude-3-5-haiku-20241022"}},
				},
				{ID: 7, Platform: PlatformAnthropic, Priority: 2, Status: StatusActive, Schedulable: true, Concurrency: 5},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{}

		groupRepo := &mockGroupRepoForGateway{
			groups: map[int64]*Group{
				groupID: {
					ID:                  groupID,
					Platform:            PlatformAnthropic,
					Status:              StatusActive,
					Hydrated:            true,
					ModelRoutingEnabled: true,
					ModelRouting: map[string][]int64{
						"claude-3-5-sonnet-20241022": {1, 2, 3, 4, 5, 6},
					},
				},
			},
		}

		cfg := testConfig()
		cfg.Gateway.Scheduling.LoadBatchEnabled = true

		concurrencyCache := &mockConcurrencyCache{}

		svc := &GatewayService{
			accountRepo:        repo,
			groupRepo:          groupRepo,
			cache:              cache,
			cfg:                cfg,
			concurrencyService: NewConcurrencyService(concurrencyCache),
		}

		excluded := map[int64]struct{}{1: {}}
		result, err := svc.SelectAccountWithLoadAwareness(ctx, &groupID, "", "claude-3-5-sonnet-20241022", excluded, "", int64(0))
		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.Account)
		require.Equal(t, int64(7), result.Account.ID)
	})

	t.Run("ClaudeCode限制-回退分组", func(t *testing.T) {
		groupID := int64(60)
		fallbackID := int64(61)

		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformGemini, Priority: 1, Status: StatusActive, Schedulable: true},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		groupRepo := &mockGroupRepoForGateway{
			groups: map[int64]*Group{
				groupID: {
					ID:             groupID,
					Platform:       PlatformAnthropic,
					Status:         StatusActive,
					Hydrated:       true,
					ClaudeCodeOnly: true,
					FallbackGroupID: func() *int64 {
						v := fallbackID
						return &v
					}(),
				},
				fallbackID: {
					ID:       fallbackID,
					Platform: PlatformGemini,
					Status:   StatusActive,
					Hydrated: true,
				},
			},
		}

		cfg := testConfig()
		cfg.Gateway.Scheduling.LoadBatchEnabled = false

		svc := &GatewayService{
			accountRepo:        repo,
			groupRepo:          groupRepo,
			cache:              &mockGatewayCacheForPlatform{},
			cfg:                cfg,
			concurrencyService: nil,
		}

		result, err := svc.SelectAccountWithLoadAwareness(ctx, &groupID, "", "gemini-2.5-pro", nil, "", int64(0))
		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.Account)
		require.Equal(t, int64(1), result.Account.ID)
	})

	t.Run("ClaudeCode限制-无降级返回错误", func(t *testing.T) {
		groupID := int64(62)

		groupRepo := &mockGroupRepoForGateway{
			groups: map[int64]*Group{
				groupID: {
					ID:             groupID,
					Platform:       PlatformAnthropic,
					Status:         StatusActive,
					Hydrated:       true,
					ClaudeCodeOnly: true,
				},
			},
		}

		cfg := testConfig()
		cfg.Gateway.Scheduling.LoadBatchEnabled = false

		svc := &GatewayService{
			accountRepo:        &mockAccountRepoForPlatform{},
			groupRepo:          groupRepo,
			cache:              &mockGatewayCacheForPlatform{},
			cfg:                cfg,
			concurrencyService: nil,
		}

		result, err := svc.SelectAccountWithLoadAwareness(ctx, &groupID, "", "claude-3-5-sonnet-20241022", nil, "", int64(0))
		require.Error(t, err)
		require.Nil(t, result)
		require.ErrorIs(t, err, ErrClaudeCodeOnly)
	})

	t.Run("负载可用但无法获取槽位-兜底等待", func(t *testing.T) {
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5},
				{ID: 2, Platform: PlatformAnthropic, Priority: 2, Status: StatusActive, Schedulable: true, Concurrency: 5},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{}

		cfg := testConfig()
		cfg.Gateway.Scheduling.LoadBatchEnabled = true

		concurrencyCache := &mockConcurrencyCache{
			acquireResults: map[int64]bool{1: false, 2: false},
			loadMap: map[int64]*AccountLoadInfo{
				1: {AccountID: 1, LoadRate: 10},
				2: {AccountID: 2, LoadRate: 20},
			},
		}

		svc := &GatewayService{
			accountRepo:        repo,
			cache:              cache,
			cfg:                cfg,
			concurrencyService: NewConcurrencyService(concurrencyCache),
		}

		result, err := svc.SelectAccountWithLoadAwareness(ctx, nil, "wait", "claude-3-5-sonnet-20241022", nil, "", int64(0))
		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.WaitPlan)
		require.Equal(t, int64(1), result.Account.ID)
	})

	t.Run("负载信息缺失-使用默认负载", func(t *testing.T) {
		repo := &mockAccountRepoForPlatform{
			accounts: []Account{
				{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5},
				{ID: 2, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true, Concurrency: 5},
			},
			accountsByID: map[int64]*Account{},
		}
		for i := range repo.accounts {
			repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
		}

		cache := &mockGatewayCacheForPlatform{}

		cfg := testConfig()
		cfg.Gateway.Scheduling.LoadBatchEnabled = true

		concurrencyCache := &mockConcurrencyCache{
			loadMap: map[int64]*AccountLoadInfo{
				1: {AccountID: 1, LoadRate: 50},
			},
			skipDefaultLoad: true,
		}

		svc := &GatewayService{
			accountRepo:        repo,
			cache:              cache,
			cfg:                cfg,
			concurrencyService: NewConcurrencyService(concurrencyCache),
		}

		result, err := svc.SelectAccountWithLoadAwareness(ctx, nil, "missing-load", "claude-3-5-sonnet-20241022", nil, "", int64(0))
		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.Account)
		require.Equal(t, int64(2), result.Account.ID)
	})
}

func TestGatewayService_GroupResolution_ReusesContextGroup(t *testing.T) {
	ctx := context.Background()
	groupID := int64(42)
	group := &Group{
		ID:       groupID,
		Platform: PlatformAnthropic,
		Status:   StatusActive,
		Hydrated: true,
	}
	ctx = context.WithValue(ctx, ctxkey.Group, group)

	repo := &mockAccountRepoForPlatform{
		accounts: []Account{
			{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true},
		},
		accountsByID: map[int64]*Account{},
	}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}

	groupRepo := &mockGroupRepoForGateway{
		groups: map[int64]*Group{groupID: group},
	}

	svc := &GatewayService{
		accountRepo: repo,
		groupRepo:   groupRepo,
		cfg:         testConfig(),
	}

	account, err := svc.SelectAccountForModelWithExclusions(ctx, &groupID, "", "claude-3-5-sonnet-20241022", nil)
	require.NoError(t, err)
	require.NotNil(t, account)
	require.Equal(t, 1, groupRepo.getByIDCalls) // +1 for require_privacy_set check
	require.Equal(t, 0, groupRepo.getByIDLiteCalls)
}

func TestGatewayService_GroupResolution_IgnoresInvalidContextGroup(t *testing.T) {
	ctx := context.Background()
	groupID := int64(42)
	ctxGroup := &Group{
		ID:       groupID,
		Platform: PlatformAnthropic,
		Status:   StatusActive,
	}
	ctx = context.WithValue(ctx, ctxkey.Group, ctxGroup)

	repo := &mockAccountRepoForPlatform{
		accounts: []Account{
			{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true},
		},
		accountsByID: map[int64]*Account{},
	}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}

	group := &Group{
		ID:       groupID,
		Platform: PlatformAnthropic,
		Status:   StatusActive,
		Hydrated: true,
	}
	groupRepo := &mockGroupRepoForGateway{
		groups: map[int64]*Group{groupID: group},
	}

	svc := &GatewayService{
		accountRepo: repo,
		groupRepo:   groupRepo,
		cfg:         testConfig(),
	}

	account, err := svc.SelectAccountForModelWithExclusions(ctx, &groupID, "", "claude-3-5-sonnet-20241022", nil)
	require.NoError(t, err)
	require.NotNil(t, account)
	require.Equal(t, 1, groupRepo.getByIDCalls) // +1 for require_privacy_set check
	require.Equal(t, 1, groupRepo.getByIDLiteCalls)
}

func TestGatewayService_GroupContext_OverwritesInvalidContextGroup(t *testing.T) {
	groupID := int64(42)
	invalidGroup := &Group{
		ID:       groupID,
		Platform: PlatformAnthropic,
		Status:   StatusActive,
	}
	hydratedGroup := &Group{
		ID:       groupID,
		Platform: PlatformAnthropic,
		Status:   StatusActive,
		Hydrated: true,
	}

	ctx := context.WithValue(context.Background(), ctxkey.Group, invalidGroup)
	svc := &GatewayService{}
	ctx = svc.withGroupContext(ctx, hydratedGroup)

	got, ok := ctx.Value(ctxkey.Group).(*Group)
	require.True(t, ok)
	require.Same(t, hydratedGroup, got)
}

func TestGatewayService_GroupResolution_FallbackUsesLiteOnce(t *testing.T) {
	ctx := context.Background()
	groupID := int64(10)
	fallbackID := int64(11)
	group := &Group{
		ID:              groupID,
		Platform:        PlatformAnthropic,
		Status:          StatusActive,
		ClaudeCodeOnly:  true,
		FallbackGroupID: &fallbackID,
		Hydrated:        true,
	}
	fallbackGroup := &Group{
		ID:       fallbackID,
		Platform: PlatformAnthropic,
		Status:   StatusActive,
		Hydrated: true,
	}
	ctx = context.WithValue(ctx, ctxkey.Group, group)

	repo := &mockAccountRepoForPlatform{
		accounts: []Account{
			{ID: 1, Platform: PlatformAnthropic, Priority: 1, Status: StatusActive, Schedulable: true},
		},
		accountsByID: map[int64]*Account{},
	}
	for i := range repo.accounts {
		repo.accountsByID[repo.accounts[i].ID] = &repo.accounts[i]
	}

	groupRepo := &mockGroupRepoForGateway{
		groups: map[int64]*Group{fallbackID: fallbackGroup},
	}

	svc := &GatewayService{
		accountRepo: repo,
		groupRepo:   groupRepo,
		cfg:         testConfig(),
	}

	account, err := svc.SelectAccountForModelWithExclusions(ctx, &groupID, "", "claude-3-5-sonnet-20241022", nil)
	require.NoError(t, err)
	require.NotNil(t, account)
	require.Equal(t, 1, groupRepo.getByIDCalls) // +1 for require_privacy_set check
	require.Equal(t, 1, groupRepo.getByIDLiteCalls)
}

func TestGatewayService_ResolveGatewayGroup_DetectsFallbackCycle(t *testing.T) {
	ctx := context.Background()
	groupID := int64(10)
	fallbackID := int64(11)

	group := &Group{
		ID:              groupID,
		Platform:        PlatformAnthropic,
		Status:          StatusActive,
		ClaudeCodeOnly:  true,
		FallbackGroupID: &fallbackID,
	}
	fallbackGroup := &Group{
		ID:              fallbackID,
		Platform:        PlatformAnthropic,
		Status:          StatusActive,
		ClaudeCodeOnly:  true,
		FallbackGroupID: &groupID,
	}

	groupRepo := &mockGroupRepoForGateway{
		groups: map[int64]*Group{
			groupID:    group,
			fallbackID: fallbackGroup,
		},
	}

	svc := &GatewayService{
		groupRepo: groupRepo,
	}

	gotGroup, gotID, err := svc.resolveGatewayGroup(ctx, &groupID)
	require.Error(t, err)
	require.Nil(t, gotGroup)
	require.Nil(t, gotID)
	require.Contains(t, err.Error(), "fallback group cycle")
}
