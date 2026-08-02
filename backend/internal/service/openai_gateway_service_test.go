package service

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/cespare/xxhash/v2"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 编译期接口断言
var _ AccountRepository = (*stubOpenAIAccountRepo)(nil)
var _ GatewayCache = (*stubGatewayCache)(nil)

func TestOpenAIForwardResultFromPassthroughNonStreamingResultPreservesAttemptID(t *testing.T) {
	attemptID := "physical-attempt-19"
	request, err := http.NewRequestWithContext(
		context.WithValue(context.Background(), httpAttemptIDKey{}, attemptID),
		http.MethodPost,
		"https://upstream.example/v1/responses",
		nil,
	)
	require.NoError(t, err)

	result := openAIForwardResultFromPassthroughNonStreamingResult(
		&openaiNonStreamingResultPassthrough{responseID: "resp-19", usage: &OpenAIUsage{}},
		&http.Response{Header: http.Header{"X-Request-Id": []string{"upstream-19"}}, Request: request},
		time.Now().Add(-time.Second),
		"gpt-5",
		"gpt-5",
		"gpt-5-upstream",
		nil,
		nil,
		"",
		"",
		"",
	)

	require.Equal(t, attemptID, result.AttemptID)
}

type stubOpenAIAccountRepo struct {
	AccountRepository
	accounts []Account
}

type openAITestSettingRepo struct {
	values map[string]string
}

type openAIStreamRateLimitAccountRepo struct {
	AccountRepository
	rateLimitResets []time.Time
}

func (r *openAIStreamRateLimitAccountRepo) SetRateLimited(_ context.Context, _ int64, resetAt time.Time) error {
	r.rateLimitResets = append(r.rateLimitResets, resetAt)
	return nil
}

func (s *openAITestSettingRepo) Get(ctx context.Context, key string) (*Setting, error) {
	panic("unexpected Get call")
}

func (s *openAITestSettingRepo) GetValue(ctx context.Context, key string) (string, error) {
	if v, ok := s.values[key]; ok {
		return v, nil
	}
	return "", ErrSettingNotFound
}

func (s *openAITestSettingRepo) Set(ctx context.Context, key, value string) error {
	panic("unexpected Set call")
}

func (s *openAITestSettingRepo) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	result := make(map[string]string, len(keys))
	for _, key := range keys {
		if v, ok := s.values[key]; ok {
			result[key] = v
		}
	}
	return result, nil
}

func (s *openAITestSettingRepo) SetMultiple(ctx context.Context, settings map[string]string) error {
	panic("unexpected SetMultiple call")
}

func (s *openAITestSettingRepo) GetAll(ctx context.Context) (map[string]string, error) {
	panic("unexpected GetAll call")
}

func (s *openAITestSettingRepo) Delete(ctx context.Context, key string) error {
	panic("unexpected Delete call")
}

type snapshotUpdateAccountRepo struct {
	stubOpenAIAccountRepo
	updateExtraCalls      chan map[string]any
	updateExtraErr        error
	updateExtraErrFor     func(map[string]any) error
	sessionWindowEndCalls chan struct {
		id  int64
		end time.Time
	}
	setErrorID  int64
	setErrorMsg string
}

func (r *snapshotUpdateAccountRepo) UpdateExtra(ctx context.Context, id int64, updates map[string]any) error {
	if r.updateExtraCalls != nil {
		copied := make(map[string]any, len(updates))
		for k, v := range updates {
			copied[k] = v
		}
		r.updateExtraCalls <- copied
	}
	if r.updateExtraErrFor != nil {
		if err := r.updateExtraErrFor(updates); err != nil {
			return err
		}
	}
	if r.updateExtraErr != nil {
		return r.updateExtraErr
	}
	return nil
}

func (r *snapshotUpdateAccountRepo) SetError(_ context.Context, id int64, errorMsg string) error {
	r.setErrorID = id
	r.setErrorMsg = errorMsg
	return nil
}

func (r *snapshotUpdateAccountRepo) UpdateSessionWindowEnd(_ context.Context, id int64, end time.Time) error {
	if r.sessionWindowEndCalls != nil {
		r.sessionWindowEndCalls <- struct {
			id  int64
			end time.Time
		}{id: id, end: end}
	}
	return nil
}

func (r stubOpenAIAccountRepo) GetByID(ctx context.Context, id int64) (*Account, error) {
	for i := range r.accounts {
		if r.accounts[i].ID == id {
			return &r.accounts[i], nil
		}
	}
	return nil, errors.New("account not found")
}

func (r stubOpenAIAccountRepo) GetByIDs(ctx context.Context, ids []int64) ([]*Account, error) {
	// 与生产 GetByIDs 语义一致：缺失 ID 忽略，不报错。
	out := make([]*Account, 0, len(ids))
	for _, id := range ids {
		if acc, err := r.GetByID(ctx, id); err == nil {
			out = append(out, acc)
		}
	}
	return out, nil
}

func (r stubOpenAIAccountRepo) ListSchedulableByGroupIDAndPlatform(ctx context.Context, groupID int64, platform string) ([]Account, error) {
	var result []Account
	for _, acc := range r.accounts {
		if acc.Platform == platform {
			result = append(result, acc)
		}
	}
	return result, nil
}

func (r stubOpenAIAccountRepo) ListSchedulableByPlatform(ctx context.Context, platform string) ([]Account, error) {
	var result []Account
	for _, acc := range r.accounts {
		if acc.Platform == platform {
			result = append(result, acc)
		}
	}
	return result, nil
}

func (r stubOpenAIAccountRepo) ListSchedulableUngroupedByPlatform(ctx context.Context, platform string) ([]Account, error) {
	return r.ListSchedulableByPlatform(ctx, platform)
}

type groupAwareStubOpenAIAccountRepo struct {
	stubOpenAIAccountRepo
}

func (r groupAwareStubOpenAIAccountRepo) ListSchedulableByGroupIDAndPlatform(ctx context.Context, groupID int64, platform string) ([]Account, error) {
	var result []Account
	for _, acc := range r.accounts {
		if acc.Platform == platform && openAIStickyAccountMatchesGroup(&acc, &groupID) {
			result = append(result, acc)
		}
	}
	return result, nil
}

func (r groupAwareStubOpenAIAccountRepo) ListSchedulableUngroupedByPlatform(ctx context.Context, platform string) ([]Account, error) {
	var result []Account
	for _, acc := range r.accounts {
		if acc.Platform == platform && openAIStickyAccountMatchesGroup(&acc, nil) {
			result = append(result, acc)
		}
	}
	return result, nil
}

type stubConcurrencyCache struct {
	ConcurrencyCache
	loadBatchErr    error
	loadMap         map[int64]*AccountLoadInfo
	acquireResults  map[int64]bool
	waitCounts      map[int64]int
	skipDefaultLoad bool
}

type cancelReadCloser struct{}

func (c cancelReadCloser) Read(p []byte) (int, error) { return 0, context.Canceled }
func (c cancelReadCloser) Close() error               { return nil }

type errReadCloser struct {
	err error
}

func (r errReadCloser) Read([]byte) (int, error) { return 0, r.err }
func (r errReadCloser) Close() error             { return nil }

func markRemoteCompactionV2Request(t *testing.T, c *gin.Context, ctx context.Context) {
	t.Helper()
	body := []byte(`{"model":"gpt-5.5","stream":true,"input":[{"type":"compaction_trigger"}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)).WithContext(ctx)
	c.Request.Header.Set("User-Agent", "codex_cli_rs/0.136.0")
	require.True(t, PromoteOpenAICompactBodySignal(c, body, false))
}

type failingGinWriter struct {
	gin.ResponseWriter
	failAfter int
	writes    int
}

func (w *failingGinWriter) Write(p []byte) (int, error) {
	if w.writes >= w.failAfter {
		return 0, errors.New("write failed")
	}
	w.writes++
	return w.ResponseWriter.Write(p)
}

func (c stubConcurrencyCache) AcquireAccountSlot(ctx context.Context, accountID int64, maxConcurrency int, requestID string) (bool, error) {
	if c.acquireResults != nil {
		if result, ok := c.acquireResults[accountID]; ok {
			return result, nil
		}
	}
	return true, nil
}

func (c stubConcurrencyCache) ReleaseAccountSlot(ctx context.Context, accountID int64, requestID string) error {
	return nil
}

func (c stubConcurrencyCache) GetAccountsLoadBatch(ctx context.Context, accounts []AccountWithConcurrency) (map[int64]*AccountLoadInfo, error) {
	if c.loadBatchErr != nil {
		return nil, c.loadBatchErr
	}
	out := make(map[int64]*AccountLoadInfo, len(accounts))
	if c.skipDefaultLoad && c.loadMap != nil {
		for _, acc := range accounts {
			if load, ok := c.loadMap[acc.ID]; ok {
				out[acc.ID] = load
			}
		}
		return out, nil
	}
	for _, acc := range accounts {
		if c.loadMap != nil {
			if load, ok := c.loadMap[acc.ID]; ok {
				out[acc.ID] = load
				continue
			}
		}
		out[acc.ID] = &AccountLoadInfo{AccountID: acc.ID, LoadRate: 0}
	}
	return out, nil
}

func TestOpenAIGatewayService_GenerateSessionHash_Priority(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)

	svc := &OpenAIGatewayService{}

	bodyWithKey := []byte(`{"prompt_cache_key":"ses_aaa"}`)

	// 1) session_id header wins
	c.Request.Header.Set("session_id", "sess-123")
	c.Request.Header.Set("conversation_id", "conv-456")
	h1 := svc.GenerateSessionHash(c, bodyWithKey)
	if h1 == "" {
		t.Fatalf("expected non-empty hash")
	}

	// 2) conversation_id used when session_id absent
	c.Request.Header.Del("session_id")
	h2 := svc.GenerateSessionHash(c, bodyWithKey)
	if h2 == "" {
		t.Fatalf("expected non-empty hash")
	}
	if h1 == h2 {
		t.Fatalf("expected different hashes for different keys")
	}

	// 3) prompt_cache_key used when both headers absent
	c.Request.Header.Del("conversation_id")
	h3 := svc.GenerateSessionHash(c, bodyWithKey)
	if h3 == "" {
		t.Fatalf("expected non-empty hash")
	}
	if h2 == h3 {
		t.Fatalf("expected different hashes for different keys")
	}

	// 4) empty when no signals
	h4 := svc.GenerateSessionHash(c, []byte(`{}`))
	if h4 != "" {
		t.Fatalf("expected empty hash when no signals")
	}
}

func TestOpenAIGatewayService_GenerateSessionHash_UsesXXHash64(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)

	c.Request.Header.Set("session_id", "sess-fixed-value")
	svc := &OpenAIGatewayService{}

	got := svc.GenerateSessionHash(c, nil)
	want := fmt.Sprintf("%016x", xxhash.Sum64String("sess-fixed-value"))
	require.Equal(t, want, got)
}

func TestOpenAIGatewayService_GenerateSessionHash_AttachesLegacyHashToContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)

	c.Request.Header.Set("session_id", "sess-legacy-check")
	svc := &OpenAIGatewayService{}

	sessionHash := svc.GenerateSessionHash(c, nil)
	require.NotEmpty(t, sessionHash)
	require.NotNil(t, c.Request)
	require.NotNil(t, c.Request.Context())
	require.NotEmpty(t, openAILegacySessionHashFromContext(c.Request.Context()))
}

func TestOpenAIGatewayService_GenerateExplicitSessionHash_SkipsContentFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{}
	body := []byte(`{"model":"gpt-image-2","prompt":"draw a cat"}`)

	t.Run("stateless image body stays unstuck", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)

		require.Empty(t, svc.GenerateExplicitSessionHash(c, body))
		require.Empty(t, openAILegacySessionHashFromContext(c.Request.Context()))
	})

	t.Run("prompt_cache_key is explicit", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)

		got := svc.GenerateExplicitSessionHash(c, []byte(`{"model":"gpt-image-2","prompt_cache_key":"image-session"}`))
		require.Equal(t, fmt.Sprintf("%016x", xxhash.Sum64String("image-session")), got)
		require.NotEmpty(t, openAILegacySessionHashFromContext(c.Request.Context()))
	})

	t.Run("header overrides body", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
		c.Request.Header.Set("session_id", "header-session")

		got := svc.GenerateExplicitSessionHash(c, []byte(`{"prompt_cache_key":"body-session"}`))
		require.Equal(t, fmt.Sprintf("%016x", xxhash.Sum64String("header-session")), got)
	})
}

func TestOpenAIGatewayService_GenerateSessionHashWithFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)

	svc := &OpenAIGatewayService{}
	seed := "openai_ws_ingress:9:100:200"

	got := svc.GenerateSessionHashWithFallback(c, []byte(`{}`), seed)
	want := fmt.Sprintf("%016x", xxhash.Sum64String(seed))
	require.Equal(t, want, got)
	require.NotEmpty(t, openAILegacySessionHashFromContext(c.Request.Context()))

	empty := svc.GenerateSessionHashWithFallback(c, []byte(`{}`), "   ")
	require.Equal(t, "", empty)
}

func TestOpenAIGatewayService_GenerateSessionHash_ContentFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", nil)

	svc := &OpenAIGatewayService{}

	body := []byte(`{"model":"gpt-5.4","messages":[{"role":"system","content":"You are helpful."},{"role":"user","content":"Hello"}]}`)

	hash := svc.GenerateSessionHash(c, body)
	require.NotEmpty(t, hash, "content-based fallback should produce a hash")

	hash2 := svc.GenerateSessionHash(c, body)
	require.Equal(t, hash, hash2, "same content should produce same hash")

	bodyExtended := []byte(`{"model":"gpt-5.4","messages":[{"role":"system","content":"You are helpful."},{"role":"user","content":"Hello"},{"role":"assistant","content":"Hi!"},{"role":"user","content":"How are you?"}]}`)
	hashExtended := svc.GenerateSessionHash(c, bodyExtended)
	require.Equal(t, hash, hashExtended, "hash should be stable across later turns")

	bodyDifferent := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"Different question"}]}`)
	hashDifferent := svc.GenerateSessionHash(c, bodyDifferent)
	require.NotEqual(t, hash, hashDifferent, "different content should produce different hash")
}

func TestOpenAIGatewayService_GenerateSessionHash_ExplicitSignalWinsOverContent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", nil)

	svc := &OpenAIGatewayService{}
	body := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"Hello"}]}`)

	contentHash := svc.GenerateSessionHash(c, body)
	require.NotEmpty(t, contentHash)

	c.Request.Header.Set("session_id", "explicit-session")
	explicitHash := svc.GenerateSessionHash(c, body)
	require.NotEmpty(t, explicitHash)
	require.NotEqual(t, contentHash, explicitHash, "explicit session_id should override content fallback")
}

func TestOpenAIGatewayService_GenerateSessionHash_EmptyBodyStillEmpty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", nil)

	svc := &OpenAIGatewayService{}
	require.Empty(t, svc.GenerateSessionHash(c, []byte(`{}`)))
	require.Empty(t, svc.GenerateSessionHash(c, nil))
}

func TestOpenAIGatewayService_BindHTTPResponseAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
	groupID := int64(4201)
	c.Set("api_key", &APIKey{ID: 501, GroupID: &groupID})

	svc := &OpenAIGatewayService{}
	account := &Account{ID: 37001, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	svc.bindHTTPResponseAccount(context.Background(), c, account, "resp_http_001")

	got, err := svc.getOpenAIWSStateStore().GetResponseAccount(context.Background(), groupID, "resp_http_001")
	require.NoError(t, err)
	require.Equal(t, account.ID, got)
}

func (c stubConcurrencyCache) GetAccountWaitingCount(ctx context.Context, accountID int64) (int, error) {
	if c.waitCounts != nil {
		if count, ok := c.waitCounts[accountID]; ok {
			return count, nil
		}
	}
	return 0, nil
}

type stubGatewayCache struct {
	sessionBindings map[string]int64
	deletedSessions map[string]int
}

func (c *stubGatewayCache) GetSessionAccountID(ctx context.Context, groupID int64, sessionHash string) (int64, error) {
	if id, ok := c.sessionBindings[sessionHash]; ok {
		return id, nil
	}
	return 0, errors.New("not found")
}

func (c *stubGatewayCache) SetSessionAccountID(ctx context.Context, groupID int64, sessionHash string, accountID int64, ttl time.Duration) error {
	if c.sessionBindings == nil {
		c.sessionBindings = make(map[string]int64)
	}
	c.sessionBindings[sessionHash] = accountID
	return nil
}

func (c *stubGatewayCache) RefreshSessionTTL(ctx context.Context, groupID int64, sessionHash string, ttl time.Duration) error {
	return nil
}

func (c *stubGatewayCache) DeleteSessionAccountID(ctx context.Context, groupID int64, sessionHash string) error {
	if c.sessionBindings == nil {
		return nil
	}
	if c.deletedSessions == nil {
		c.deletedSessions = make(map[string]int)
	}
	c.deletedSessions[sessionHash]++
	delete(c.sessionBindings, sessionHash)
	return nil
}

func TestOpenAISelectAccountWithLoadAwareness_FiltersUnschedulable(t *testing.T) {
	now := time.Now()
	resetAt := now.Add(10 * time.Minute)
	groupID := int64(1)

	rateLimited := Account{
		ID:               1,
		Platform:         PlatformOpenAI,
		Type:             AccountTypeAPIKey,
		Status:           StatusActive,
		Schedulable:      true,
		Concurrency:      1,
		Priority:         0,
		RateLimitResetAt: &resetAt,
	}
	available := Account{
		ID:          2,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Priority:    1,
	}

	svc := &OpenAIGatewayService{
		accountRepo:        stubOpenAIAccountRepo{accounts: []Account{rateLimited, available}},
		concurrencyService: NewConcurrencyService(stubConcurrencyCache{}),
	}

	selection, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, "", "gpt-5.2", nil)
	if err != nil {
		t.Fatalf("SelectAccountWithLoadAwareness error: %v", err)
	}
	if selection == nil || selection.Account == nil {
		t.Fatalf("expected selection with account")
	}
	if selection.Account.ID != available.ID {
		t.Fatalf("expected account %d, got %d", available.ID, selection.Account.ID)
	}
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAISelectAccountWithLoadAwareness_ImageRateLimitSkipsOnlyImageRequests(t *testing.T) {
	future := time.Now().Add(10 * time.Minute).Format(time.RFC3339)
	groupID := int64(1)

	imageLimited := Account{
		ID:          1,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Priority:    0,
		Extra: map[string]any{
			modelRateLimitsKey: map[string]any{
				openAIImageGenerationRateLimitKey: map[string]any{
					"rate_limit_reset_at": future,
				},
			},
		},
	}
	available := Account{
		ID:          2,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Priority:    1,
	}
	svc := &OpenAIGatewayService{
		accountRepo:        stubOpenAIAccountRepo{accounts: []Account{imageLimited, available}},
		concurrencyService: NewConcurrencyService(stubConcurrencyCache{}),
	}

	imageSelection, err := svc.SelectAccountWithLoadAwareness(WithOpenAIImageGenerationIntent(context.Background()), &groupID, "", "gpt-5.4", nil)
	require.NoError(t, err)
	require.NotNil(t, imageSelection)
	require.Equal(t, available.ID, imageSelection.Account.ID)
	if imageSelection.ReleaseFunc != nil {
		imageSelection.ReleaseFunc()
	}

	textSelection, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, "", "gpt-5.4", nil)
	require.NoError(t, err)
	require.NotNil(t, textSelection)
	require.Equal(t, imageLimited.ID, textSelection.Account.ID)
	if textSelection.ReleaseFunc != nil {
		textSelection.ReleaseFunc()
	}
}

func TestOpenAISelectAccountWithLoadAwareness_FiltersUnschedulableWhenNoConcurrencyService(t *testing.T) {
	now := time.Now()
	resetAt := now.Add(10 * time.Minute)
	groupID := int64(1)

	rateLimited := Account{
		ID:               1,
		Platform:         PlatformOpenAI,
		Type:             AccountTypeAPIKey,
		Status:           StatusActive,
		Schedulable:      true,
		Concurrency:      1,
		Priority:         0,
		RateLimitResetAt: &resetAt,
	}
	available := Account{
		ID:          2,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Priority:    1,
	}

	svc := &OpenAIGatewayService{
		accountRepo: stubOpenAIAccountRepo{accounts: []Account{rateLimited, available}},
		// concurrencyService is nil, forcing the non-load-batch selection path.
	}

	selection, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, "", "gpt-5.2", nil)
	if err != nil {
		t.Fatalf("SelectAccountWithLoadAwareness error: %v", err)
	}
	if selection == nil || selection.Account == nil {
		t.Fatalf("expected selection with account")
	}
	if selection.Account.ID != available.ID {
		t.Fatalf("expected account %d, got %d", available.ID, selection.Account.ID)
	}
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAISelectAccountForModelWithExclusions_StickyUnschedulableClearsSession(t *testing.T) {
	sessionHash := "session-1"
	repo := stubOpenAIAccountRepo{
		accounts: []Account{
			{ID: 1, Platform: PlatformOpenAI, Status: StatusDisabled, Schedulable: true, Concurrency: 1},
			{ID: 2, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true, Concurrency: 1},
		},
	}
	cache := &stubGatewayCache{
		sessionBindings: map[string]int64{"openai:" + sessionHash: 1},
	}

	svc := &OpenAIGatewayService{
		accountRepo: repo,
		cache:       cache,
	}

	acc, err := svc.SelectAccountForModelWithExclusions(context.Background(), nil, sessionHash, "gpt-4", nil)
	if err != nil {
		t.Fatalf("SelectAccountForModelWithExclusions error: %v", err)
	}
	if acc == nil || acc.ID != 2 {
		t.Fatalf("expected account 2, got %+v", acc)
	}
	if cache.deletedSessions["openai:"+sessionHash] != 1 {
		t.Fatalf("expected sticky session to be deleted")
	}
	if cache.sessionBindings["openai:"+sessionHash] != 2 {
		t.Fatalf("expected sticky session to bind to account 2")
	}
}

func TestOpenAISelectAccountWithLoadAwareness_StickyUnschedulableClearsSession(t *testing.T) {
	sessionHash := "session-2"
	groupID := int64(1)
	repo := stubOpenAIAccountRepo{
		accounts: []Account{
			{ID: 1, Platform: PlatformOpenAI, Status: StatusDisabled, Schedulable: true, Concurrency: 1},
			{ID: 2, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true, Concurrency: 1},
		},
	}
	cache := &stubGatewayCache{
		sessionBindings: map[string]int64{"openai:" + sessionHash: 1},
	}

	svc := &OpenAIGatewayService{
		accountRepo:        repo,
		cache:              cache,
		concurrencyService: NewConcurrencyService(stubConcurrencyCache{}),
	}

	selection, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, sessionHash, "gpt-4", nil)
	if err != nil {
		t.Fatalf("SelectAccountWithLoadAwareness error: %v", err)
	}
	if selection == nil || selection.Account == nil || selection.Account.ID != 2 {
		t.Fatalf("expected account 2, got %+v", selection)
	}
	if cache.deletedSessions["openai:"+sessionHash] != 1 {
		t.Fatalf("expected sticky session to be deleted")
	}
	if cache.sessionBindings["openai:"+sessionHash] != 2 {
		t.Fatalf("expected sticky session to bind to account 2")
	}
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAISelectAccountForModelWithExclusions_StickyOutsideGroupClearsSession(t *testing.T) {
	sessionHash := "outside-group"
	groupID := int64(12)
	repo := groupAwareStubOpenAIAccountRepo{
		stubOpenAIAccountRepo: stubOpenAIAccountRepo{
			accounts: []Account{
				{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0},
				{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 5, GroupIDs: []int64{groupID}},
			},
		},
	}
	cache := &stubGatewayCache{
		sessionBindings: map[string]int64{"openai:" + sessionHash: 1},
	}

	svc := &OpenAIGatewayService{
		accountRepo: repo,
		cache:       cache,
	}

	acc, err := svc.SelectAccountForModelWithExclusions(context.Background(), &groupID, sessionHash, "gpt-4", nil)
	if err != nil {
		t.Fatalf("SelectAccountForModelWithExclusions error: %v", err)
	}
	if acc == nil || acc.ID != 2 {
		t.Fatalf("expected account 2, got %+v", acc)
	}
	if cache.deletedSessions["openai:"+sessionHash] != 1 {
		t.Fatalf("expected sticky session to be deleted")
	}
	if cache.sessionBindings["openai:"+sessionHash] != 2 {
		t.Fatalf("expected sticky session to bind to account 2")
	}
}

func TestOpenAISelectAccountForModelWithExclusions_StickyOutsideGroupClearsBeforeEligibility(t *testing.T) {
	sessionHash := "outside-group-unsupported"
	groupID := int64(14)
	repo := groupAwareStubOpenAIAccountRepo{
		stubOpenAIAccountRepo: stubOpenAIAccountRepo{
			accounts: []Account{
				{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0, Credentials: map[string]any{"model_mapping": map[string]any{"gpt-3.5-turbo": "gpt-3.5-turbo"}}},
				{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 5, GroupIDs: []int64{groupID}},
			},
		},
	}
	cache := &stubGatewayCache{
		sessionBindings: map[string]int64{"openai:" + sessionHash: 1},
	}

	svc := &OpenAIGatewayService{
		accountRepo: repo,
		cache:       cache,
	}

	acc, err := svc.SelectAccountForModelWithExclusions(context.Background(), &groupID, sessionHash, "gpt-4", nil)
	if err != nil {
		t.Fatalf("SelectAccountForModelWithExclusions error: %v", err)
	}
	if acc == nil || acc.ID != 2 {
		t.Fatalf("expected account 2, got %+v", acc)
	}
	if cache.deletedSessions["openai:"+sessionHash] != 1 {
		t.Fatalf("expected sticky session to be deleted before eligibility checks")
	}
}

func TestOpenAISelectAccountForModelWithExclusions_SimpleModeAllowsGroupedStickyWithoutGroup(t *testing.T) {
	sessionHash := "simple-grouped"
	repo := stubOpenAIAccountRepo{
		accounts: []Account{
			{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0, GroupIDs: []int64{99}},
		},
	}
	cache := &stubGatewayCache{
		sessionBindings: map[string]int64{"openai:" + sessionHash: 1},
	}
	cfg := &config.Config{RunMode: config.RunModeSimple}

	svc := &OpenAIGatewayService{
		accountRepo: repo,
		cache:       cache,
		cfg:         cfg,
	}

	acc, err := svc.SelectAccountForModelWithExclusions(context.Background(), nil, sessionHash, "gpt-4", nil)
	if err != nil {
		t.Fatalf("SelectAccountForModelWithExclusions error: %v", err)
	}
	if acc == nil || acc.ID != 1 {
		t.Fatalf("expected sticky account 1, got %+v", acc)
	}
	if cache.deletedSessions["openai:"+sessionHash] != 0 {
		t.Fatalf("simple mode should not delete grouped sticky binding")
	}
}

func TestOpenAISelectAccountWithLoadAwareness_StickyOutsideGroupClearsSession(t *testing.T) {
	sessionHash := "outside-group-load"
	groupID := int64(13)
	repo := groupAwareStubOpenAIAccountRepo{
		stubOpenAIAccountRepo: stubOpenAIAccountRepo{
			accounts: []Account{
				{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0},
				{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 5, AccountGroups: []AccountGroup{{AccountID: 2, GroupID: groupID}}},
			},
		},
	}
	cache := &stubGatewayCache{
		sessionBindings: map[string]int64{"openai:" + sessionHash: 1},
	}

	svc := &OpenAIGatewayService{
		accountRepo:        repo,
		cache:              cache,
		concurrencyService: NewConcurrencyService(stubConcurrencyCache{}),
	}

	selection, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, sessionHash, "gpt-4", nil)
	if err != nil {
		t.Fatalf("SelectAccountWithLoadAwareness error: %v", err)
	}
	if selection == nil || selection.Account == nil || selection.Account.ID != 2 {
		t.Fatalf("expected account 2, got %+v", selection)
	}
	if cache.deletedSessions["openai:"+sessionHash] != 1 {
		t.Fatalf("expected sticky session to be deleted")
	}
	if cache.sessionBindings["openai:"+sessionHash] != 2 {
		t.Fatalf("expected sticky session to bind to account 2")
	}
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAISelectAccountForModelWithExclusions_NoModelSupport(t *testing.T) {
	repo := stubOpenAIAccountRepo{
		accounts: []Account{
			{
				ID:          1,
				Platform:    PlatformOpenAI,
				Status:      StatusActive,
				Schedulable: true,
				Credentials: map[string]any{"model_mapping": map[string]any{"gpt-3.5-turbo": "gpt-3.5-turbo"}},
			},
		},
	}
	cache := &stubGatewayCache{}

	svc := &OpenAIGatewayService{
		accountRepo: repo,
		cache:       cache,
	}

	acc, err := svc.SelectAccountForModelWithExclusions(context.Background(), nil, "", "gpt-4", nil)
	if err == nil {
		t.Fatalf("expected error for unsupported model")
	}
	if acc != nil {
		t.Fatalf("expected nil account for unsupported model")
	}
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.NotErrorIs(t, err, ErrModelNotSupportedByAccounts)
	require.Contains(t, err.Error(), "no available OpenAI accounts supporting model")
}

func TestOpenAISelectAccountForModelWithExclusions_WildcardMappingDoesNotReturnUnsupportedModel(t *testing.T) {
	repo := stubOpenAIAccountRepo{
		accounts: []Account{
			{
				ID:          1,
				Platform:    PlatformOpenAI,
				Status:      StatusActive,
				Schedulable: true,
				Credentials: map[string]any{"model_mapping": map[string]any{"gpt-*": "gpt-4o"}},
			},
		},
	}
	svc := &OpenAIGatewayService{
		accountRepo: repo,
		cache:       &stubGatewayCache{},
	}

	acc, err := svc.SelectAccountForModelWithExclusions(context.Background(), nil, "", "gpt-4.1", nil)
	require.NoError(t, err)
	require.NotNil(t, acc)
}

func TestOpenAISelectAccountForModelWithExclusions_EmptyMappingAllowsModel(t *testing.T) {
	repo := stubOpenAIAccountRepo{
		accounts: []Account{
			{
				ID:          1,
				Platform:    PlatformOpenAI,
				Status:      StatusActive,
				Schedulable: true,
				Credentials: map[string]any{"model_mapping": map[string]any{}},
			},
		},
	}
	svc := &OpenAIGatewayService{
		accountRepo: repo,
		cache:       &stubGatewayCache{},
	}

	acc, err := svc.SelectAccountForModelWithExclusions(context.Background(), nil, "", "gpt-any-public", nil)
	require.NoError(t, err)
	require.NotNil(t, acc)
}

func TestOpenAISelectAccountWithLoadAwareness_LoadBatchErrorFallback(t *testing.T) {
	groupID := int64(1)
	repo := stubOpenAIAccountRepo{
		accounts: []Account{
			{ID: 1, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 2},
			{ID: 2, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 1},
		},
	}
	cache := &stubGatewayCache{}
	concurrencyCache := stubConcurrencyCache{
		loadBatchErr: errors.New("load batch failed"),
	}

	svc := &OpenAIGatewayService{
		accountRepo:        repo,
		cache:              cache,
		concurrencyService: NewConcurrencyService(concurrencyCache),
	}

	selection, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, "fallback", "gpt-4", nil)
	if err != nil {
		t.Fatalf("SelectAccountWithLoadAwareness error: %v", err)
	}
	if selection == nil || selection.Account == nil {
		t.Fatalf("expected selection")
	}
	if selection.Account.ID != 2 {
		t.Fatalf("expected account 2, got %d", selection.Account.ID)
	}
	if cache.sessionBindings["openai:fallback"] != 2 {
		t.Fatalf("expected sticky session updated")
	}
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAISelectAccountWithLoadAwareness_NoSlotFallbackWait(t *testing.T) {
	groupID := int64(1)
	repo := stubOpenAIAccountRepo{
		accounts: []Account{
			{ID: 1, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 1},
		},
	}
	cache := &stubGatewayCache{}
	concurrencyCache := stubConcurrencyCache{
		acquireResults: map[int64]bool{1: false},
		loadMap: map[int64]*AccountLoadInfo{
			1: {AccountID: 1, LoadRate: 10},
		},
	}

	svc := &OpenAIGatewayService{
		accountRepo:        repo,
		cache:              cache,
		concurrencyService: NewConcurrencyService(concurrencyCache),
	}

	selection, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, "", "gpt-4", nil)
	if err != nil {
		t.Fatalf("SelectAccountWithLoadAwareness error: %v", err)
	}
	if selection == nil || selection.WaitPlan == nil {
		t.Fatalf("expected wait plan fallback")
	}
	if selection.Account == nil || selection.Account.ID != 1 {
		t.Fatalf("expected account 1")
	}
}

func TestOpenAISelectAccountForModelWithExclusions_SetsStickyBinding(t *testing.T) {
	sessionHash := "bind"
	repo := stubOpenAIAccountRepo{
		accounts: []Account{
			{ID: 1, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 1},
		},
	}
	cache := &stubGatewayCache{}

	svc := &OpenAIGatewayService{
		accountRepo: repo,
		cache:       cache,
	}

	acc, err := svc.SelectAccountForModelWithExclusions(context.Background(), nil, sessionHash, "gpt-4", nil)
	if err != nil {
		t.Fatalf("SelectAccountForModelWithExclusions error: %v", err)
	}
	if acc == nil || acc.ID != 1 {
		t.Fatalf("expected account 1")
	}
	if cache.sessionBindings["openai:"+sessionHash] != 1 {
		t.Fatalf("expected sticky session binding")
	}
}

func TestOpenAISelectAccountWithLoadAwareness_StickyWaitPlan(t *testing.T) {
	sessionHash := "sticky-wait"
	groupID := int64(1)
	repo := stubOpenAIAccountRepo{
		accounts: []Account{
			{ID: 1, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 1},
		},
	}
	cache := &stubGatewayCache{
		sessionBindings: map[string]int64{"openai:" + sessionHash: 1},
	}
	concurrencyCache := stubConcurrencyCache{
		acquireResults: map[int64]bool{1: false},
		waitCounts:     map[int64]int{1: 0},
	}

	svc := &OpenAIGatewayService{
		accountRepo:        repo,
		cache:              cache,
		concurrencyService: NewConcurrencyService(concurrencyCache),
	}

	selection, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, sessionHash, "gpt-4", nil)
	if err != nil {
		t.Fatalf("SelectAccountWithLoadAwareness error: %v", err)
	}
	if selection == nil || selection.WaitPlan == nil {
		t.Fatalf("expected sticky wait plan")
	}
	if selection.Account == nil || selection.Account.ID != 1 {
		t.Fatalf("expected account 1")
	}
}

func TestOpenAISelectAccountWithLoadAwareness_PrefersLowerLoad(t *testing.T) {
	groupID := int64(1)
	repo := stubOpenAIAccountRepo{
		accounts: []Account{
			{ID: 1, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 1},
			{ID: 2, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 1},
		},
	}
	cache := &stubGatewayCache{}
	concurrencyCache := stubConcurrencyCache{
		loadMap: map[int64]*AccountLoadInfo{
			1: {AccountID: 1, LoadRate: 80},
			2: {AccountID: 2, LoadRate: 10},
		},
	}

	svc := &OpenAIGatewayService{
		accountRepo:        repo,
		cache:              cache,
		concurrencyService: NewConcurrencyService(concurrencyCache),
	}

	selection, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, "load", "gpt-4", nil)
	if err != nil {
		t.Fatalf("SelectAccountWithLoadAwareness error: %v", err)
	}
	if selection == nil || selection.Account == nil || selection.Account.ID != 2 {
		t.Fatalf("expected account 2")
	}
	if cache.sessionBindings["openai:load"] != 2 {
		t.Fatalf("expected sticky session updated")
	}
}

func TestOpenAISelectAccountForModelWithExclusions_StickyExcludedFallback(t *testing.T) {
	sessionHash := "excluded"
	repo := stubOpenAIAccountRepo{
		accounts: []Account{
			{ID: 1, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 1},
			{ID: 2, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 2},
		},
	}
	cache := &stubGatewayCache{
		sessionBindings: map[string]int64{"openai:" + sessionHash: 1},
	}

	svc := &OpenAIGatewayService{
		accountRepo: repo,
		cache:       cache,
	}

	excluded := map[int64]struct{}{1: {}}
	acc, err := svc.SelectAccountForModelWithExclusions(context.Background(), nil, sessionHash, "gpt-4", excluded)
	if err != nil {
		t.Fatalf("SelectAccountForModelWithExclusions error: %v", err)
	}
	if acc == nil || acc.ID != 2 {
		t.Fatalf("expected account 2")
	}
}

func TestOpenAISelectAccountForModelWithExclusions_StickyNonOpenAI(t *testing.T) {
	sessionHash := "non-openai"
	repo := stubOpenAIAccountRepo{
		accounts: []Account{
			{ID: 1, Platform: PlatformAnthropic, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 1},
			{ID: 2, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 2},
		},
	}
	cache := &stubGatewayCache{
		sessionBindings: map[string]int64{"openai:" + sessionHash: 1},
	}

	svc := &OpenAIGatewayService{
		accountRepo: repo,
		cache:       cache,
	}

	acc, err := svc.SelectAccountForModelWithExclusions(context.Background(), nil, sessionHash, "gpt-4", nil)
	if err != nil {
		t.Fatalf("SelectAccountForModelWithExclusions error: %v", err)
	}
	if acc == nil || acc.ID != 2 {
		t.Fatalf("expected account 2")
	}
}

func TestOpenAISelectAccountForModelWithExclusions_NoAccounts(t *testing.T) {
	repo := stubOpenAIAccountRepo{accounts: []Account{}}
	cache := &stubGatewayCache{}

	svc := &OpenAIGatewayService{
		accountRepo: repo,
		cache:       cache,
	}

	acc, err := svc.SelectAccountForModelWithExclusions(context.Background(), nil, "", "", nil)
	if err == nil {
		t.Fatalf("expected error for no accounts")
	}
	if acc != nil {
		t.Fatalf("expected nil account")
	}
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.NotErrorIs(t, err, ErrModelNotSupportedByAccounts)
}

func TestOpenAISelectAccountWithLoadAwareness_NoCandidates(t *testing.T) {
	groupID := int64(1)
	resetAt := time.Now().Add(1 * time.Hour)
	repo := stubOpenAIAccountRepo{
		accounts: []Account{
			{ID: 1, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 1, RateLimitResetAt: &resetAt},
		},
	}
	cache := &stubGatewayCache{}
	concurrencyCache := stubConcurrencyCache{}

	svc := &OpenAIGatewayService{
		accountRepo:        repo,
		cache:              cache,
		concurrencyService: NewConcurrencyService(concurrencyCache),
	}

	selection, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, "", "gpt-4", nil)
	if err == nil {
		t.Fatalf("expected error for no candidates")
	}
	if selection != nil {
		t.Fatalf("expected nil selection")
	}
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.NotErrorIs(t, err, ErrModelNotSupportedByAccounts)
}

func TestOpenAISelectAccountWithLoadAwareness_EndpointCapabilityMismatchNotUnsupportedModel(t *testing.T) {
	groupID := int64(1)
	repo := stubOpenAIAccountRepo{
		accounts: []Account{
			{
				ID:          1,
				Platform:    PlatformOpenAI,
				Type:        AccountTypeAPIKey,
				Status:      StatusActive,
				Schedulable: true,
				Concurrency: 1,
				Priority:    1,
				Credentials: map[string]any{
					"openai_capabilities": []any{"chat_completions"},
				},
			},
		},
	}
	svc := &OpenAIGatewayService{
		accountRepo:        repo,
		cache:              &stubGatewayCache{},
		concurrencyService: NewConcurrencyService(stubConcurrencyCache{}),
	}

	selection, _, err := svc.SelectAccountWithSchedulerForCapability(
		context.Background(),
		&groupID,
		"",
		"",
		"text-embedding-3-small",
		nil,
		OpenAIUpstreamTransportHTTPSSE,
		OpenAIEndpointCapabilityEmbeddings,
		false,
	)
	require.Error(t, err)
	require.Nil(t, selection)
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.NotErrorIs(t, err, ErrModelNotSupportedByAccounts)
}

func TestOpenAISelectAccountWithLoadAwareness_AllFullWaitPlan(t *testing.T) {
	groupID := int64(1)
	repo := stubOpenAIAccountRepo{
		accounts: []Account{
			{ID: 1, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 1},
		},
	}
	cache := &stubGatewayCache{}
	concurrencyCache := stubConcurrencyCache{
		loadMap: map[int64]*AccountLoadInfo{
			1: {AccountID: 1, LoadRate: 100},
		},
	}

	svc := &OpenAIGatewayService{
		accountRepo:        repo,
		cache:              cache,
		concurrencyService: NewConcurrencyService(concurrencyCache),
	}

	selection, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, "", "gpt-4", nil)
	if err != nil {
		t.Fatalf("SelectAccountWithLoadAwareness error: %v", err)
	}
	if selection == nil || selection.WaitPlan == nil {
		t.Fatalf("expected wait plan")
	}
}

func TestOpenAISelectAccountWithLoadAwareness_LoadBatchErrorNoAcquire(t *testing.T) {
	groupID := int64(1)
	repo := stubOpenAIAccountRepo{
		accounts: []Account{
			{ID: 1, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 1},
		},
	}
	cache := &stubGatewayCache{}
	concurrencyCache := stubConcurrencyCache{
		loadBatchErr:   errors.New("load batch failed"),
		acquireResults: map[int64]bool{1: false},
	}

	svc := &OpenAIGatewayService{
		accountRepo:        repo,
		cache:              cache,
		concurrencyService: NewConcurrencyService(concurrencyCache),
	}

	selection, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, "", "gpt-4", nil)
	if err != nil {
		t.Fatalf("SelectAccountWithLoadAwareness error: %v", err)
	}
	if selection == nil || selection.WaitPlan == nil {
		t.Fatalf("expected wait plan")
	}
}

func TestOpenAISelectAccountWithLoadAwareness_MissingLoadInfo(t *testing.T) {
	groupID := int64(1)
	repo := stubOpenAIAccountRepo{
		accounts: []Account{
			{ID: 1, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 1},
			{ID: 2, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 1},
		},
	}
	cache := &stubGatewayCache{}
	concurrencyCache := stubConcurrencyCache{
		loadMap: map[int64]*AccountLoadInfo{
			1: {AccountID: 1, LoadRate: 50},
		},
		skipDefaultLoad: true,
	}

	svc := &OpenAIGatewayService{
		accountRepo:        repo,
		cache:              cache,
		concurrencyService: NewConcurrencyService(concurrencyCache),
	}

	selection, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, "", "gpt-4", nil)
	if err != nil {
		t.Fatalf("SelectAccountWithLoadAwareness error: %v", err)
	}
	if selection == nil || selection.Account == nil || selection.Account.ID != 2 {
		t.Fatalf("expected account 2")
	}
}

func TestOpenAISelectAccountForModelWithExclusions_LeastRecentlyUsed(t *testing.T) {
	oldTime := time.Now().Add(-2 * time.Hour)
	newTime := time.Now().Add(-1 * time.Hour)
	repo := stubOpenAIAccountRepo{
		accounts: []Account{
			{ID: 1, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true, Priority: 1, LastUsedAt: &newTime},
			{ID: 2, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true, Priority: 1, LastUsedAt: &oldTime},
		},
	}
	cache := &stubGatewayCache{}

	svc := &OpenAIGatewayService{
		accountRepo: repo,
		cache:       cache,
	}

	acc, err := svc.SelectAccountForModelWithExclusions(context.Background(), nil, "", "gpt-4", nil)
	if err != nil {
		t.Fatalf("SelectAccountForModelWithExclusions error: %v", err)
	}
	if acc == nil || acc.ID != 2 {
		t.Fatalf("expected account 2")
	}
}

func TestOpenAISelectAccountWithLoadAwareness_PreferNeverUsed(t *testing.T) {
	groupID := int64(1)
	lastUsed := time.Now().Add(-1 * time.Hour)
	repo := stubOpenAIAccountRepo{
		accounts: []Account{
			{ID: 1, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 1, LastUsedAt: &lastUsed},
			{ID: 2, Platform: PlatformOpenAI, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 1},
		},
	}
	cache := &stubGatewayCache{}
	concurrencyCache := stubConcurrencyCache{
		loadMap: map[int64]*AccountLoadInfo{
			1: {AccountID: 1, LoadRate: 10},
			2: {AccountID: 2, LoadRate: 10},
		},
	}

	svc := &OpenAIGatewayService{
		accountRepo:        repo,
		cache:              cache,
		concurrencyService: NewConcurrencyService(concurrencyCache),
	}

	selection, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, "", "gpt-4", nil)
	if err != nil {
		t.Fatalf("SelectAccountWithLoadAwareness error: %v", err)
	}
	if selection == nil || selection.Account == nil || selection.Account.ID != 2 {
		t.Fatalf("expected account 2")
	}
}

func TestOpenAIStreamingTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			StreamDataIntervalTimeout: 1,
			StreamKeepaliveInterval:   0,
			MaxLineSize:               defaultMaxLineSize,
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	markRemoteCompactionV2Request(t, c, context.Background())

	pr, pw := io.Pipe()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       pr,
		Header:     http.Header{},
	}

	start := time.Now()
	_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1}, start, "model", "model")
	_ = pw.Close()
	_ = pr.Close()

	if err == nil || !strings.Contains(err.Error(), "stream data interval timeout") {
		t.Fatalf("expected stream timeout error, got %v", err)
	}
	if !strings.Contains(rec.Body.String(), "\"type\":\"error\"") || !strings.Contains(rec.Body.String(), "stream_timeout") {
		t.Fatalf("expected OpenAI-compatible error SSE event, got %q", rec.Body.String())
	}
	outcome, ok := GetOpenAIRemoteCompactionSemanticOutcome(c)
	require.True(t, ok)
	require.Equal(t, OpenAIRemoteCompactionSemanticOutcomeFailed, outcome)
}

func TestOpenAIStreamingContextCanceledReturnsIncompleteErrorWithoutInjectingErrorEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			StreamDataIntervalTimeout: 0,
			StreamKeepaliveInterval:   0,
			MaxLineSize:               defaultMaxLineSize,
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	markRemoteCompactionV2Request(t, c, ctx)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       cancelReadCloser{},
		Header:     http.Header{},
	}

	_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1}, time.Now(), "model", "model")
	if err == nil || !strings.Contains(err.Error(), "stream usage incomplete") {
		t.Fatalf("expected incomplete stream error, got %v", err)
	}
	if strings.Contains(rec.Body.String(), "event: error") || strings.Contains(rec.Body.String(), "stream_read_error") {
		t.Fatalf("expected no injected SSE error event, got %q", rec.Body.String())
	}
	outcome, ok := GetOpenAIRemoteCompactionSemanticOutcome(c)
	require.True(t, ok)
	require.Equal(t, OpenAIRemoteCompactionSemanticOutcomeFailed, outcome)
}

func TestOpenAIStreamingReadErrorBeforeOutputReturnsFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			StreamDataIntervalTimeout: 0,
			StreamKeepaliveInterval:   0,
			MaxLineSize:               defaultMaxLineSize,
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       errReadCloser{err: io.ErrUnexpectedEOF},
		Header:     http.Header{"X-Request-Id": []string{"rid-disconnect"}},
	}

	_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI, Name: "acc"}, time.Now(), "model", "model")
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.False(t, c.Writer.Written())
	require.Empty(t, rec.Body.String())
}

func TestOpenAIStreamingResponseFailedBeforeOutputReturnsFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			StreamDataIntervalTimeout: 0,
			StreamKeepaliveInterval:   0,
			MaxLineSize:               defaultMaxLineSize,
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			"event: response.created",
			`data: {"type":"response.created","response":{"id":"resp_1"}}`,
			"",
			"event: response.in_progress",
			`data: {"type":"response.in_progress","response":{"id":"resp_1"}}`,
			"",
			"event: response.failed",
			`data: {"type":"response.failed","error":{"message":"An error occurred while processing your request."}}`,
			"",
		}, "\n"))),
		Header: http.Header{"X-Request-Id": []string{"rid-failed"}},
	}

	_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI, Name: "acc"}, time.Now(), "model", "model")
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.False(t, failoverErr.RetryableOnSameAccount)
	require.Contains(t, string(failoverErr.ResponseBody), "An error occurred while processing your request")
	require.False(t, c.Writer.Written())
	require.Empty(t, rec.Body.String())
}

func TestOpenAIStreamingResponseFailedBeforeOutputCapacityErrorReturnsFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			StreamDataIntervalTimeout: 0,
			StreamKeepaliveInterval:   0,
			MaxLineSize:               defaultMaxLineSize,
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			"event: response.created",
			`data: {"type":"response.created","response":{"id":"resp_1"}}`,
			"",
			"event: response.in_progress",
			`data: {"type":"response.in_progress","response":{"id":"resp_1"}}`,
			"",
			"event: response.failed",
			`data: {"type":"response.failed","error":{"message":"Selected model is at capacity. Please try a different model.","type":"invalid_request_error"}}`,
			"",
		}, "\n"))),
		Header: http.Header{"X-Request-Id": []string{"rid-capacity-failed"}},
	}

	account := &Account{
		ID:       1,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Name:     "pool-account",
		Credentials: map[string]any{
			"pool_mode": true,
		},
	}
	_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "model", "model")
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.True(t, failoverErr.RetryableOnSameAccount)
	require.Contains(t, string(failoverErr.ResponseBody), "Selected model is at capacity")
	require.False(t, c.Writer.Written())
	require.Empty(t, rec.Body.String())
}

func TestOpenAIStreamingResponseFailedBeforeOutputRateLimitUsesPoolRetryPolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			"event: response.created",
			`data: {"type":"response.created","response":{"id":"resp_rate_limit"}}`,
			"",
			"event: response.failed",
			`data: {"type":"response.failed","response":{"id":"resp_rate_limit","status":"failed","error":{"code":"rate_limit_exceeded","type":"invalid_request_error","message":"Concurrency limit exceeded; retry later"}}}`,
			"",
		}, "\n"))),
		Header: http.Header{
			"X-Request-Id": []string{"rid-rate-limit-failed"},
			"Retry-After":  []string{"1"},
		},
	}
	account := &Account{
		ID:       1,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Name:     "pool-account",
		Credentials: map[string]any{
			"pool_mode":                    true,
			"pool_mode_retry_status_codes": []any{float64(http.StatusTooManyRequests)},
		},
	}

	_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "model", "model")
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusTooManyRequests, failoverErr.StatusCode)
	require.True(t, failoverErr.RetryableOnSameAccount)
	require.Equal(t, "1", failoverErr.ResponseHeaders.Get("Retry-After"))
	require.Equal(t, "rate_limit_error", gjson.GetBytes(failoverErr.ResponseBody, "error.type").String())
	fact, ok := failoverErr.UpstreamFact()
	require.True(t, ok)
	require.True(t, fact.HTTPStatusKnown)
	require.Equal(t, http.StatusTooManyRequests, fact.HTTPStatus)
	require.Equal(t, "1", fact.RetryAfter)
	require.False(t, c.Writer.Written())
	require.Empty(t, rec.Body.String())
}

func TestOpenAIStreamingResponseFailedRateLimitDoesNotBlockAccountScheduling(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			"event: response.created",
			`data: {"type":"response.created","response":{"id":"resp_rate_limit"}}`,
			"",
			"event: response.failed",
			`data: {"type":"response.failed","response":{"id":"resp_rate_limit","status":"failed","error":{"code":"rate_limit_exceeded","type":"invalid_request_error","message":"Concurrency limit exceeded; retry later"}}}`,
			"",
		}, "\n"))),
		Header: http.Header{
			"X-Codex-Primary-Used-Percent":        []string{"12"},
			"X-Codex-Primary-Reset-After-Seconds": []string{"604800"},
			"Retry-After":                         []string{"1"},
		},
	}
	account := &Account{ID: 11, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Name: "oauth-account"}

	_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "model", "model")
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusTooManyRequests, failoverErr.StatusCode)
	require.False(t, failoverErr.RetryableOnSameAccount)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
}

func TestOpenAIStreamingResponseFailedRateLimitRecordsOAuthDynamicSignalWithoutSnapshot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &modelNotFoundAccountRepoStub{}
	settingRepo := newMockSettingRepo()
	storeOpenAIOAuth429DynamicSettings(t, settingRepo, OpenAIOAuth429DynamicSettings{
		Enabled:        true,
		WindowSeconds:  60,
		MinSamples:     2,
		Min429:         1,
		RatioThreshold: 0.5,
		BlockSeconds:   12,
	})
	rateLimitSvc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	rateLimitSvc.SetSettingService(NewSettingService(settingRepo, &config.Config{}))
	svc := &OpenAIGatewayService{
		cfg:              &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}},
		rateLimitService: rateLimitSvc,
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			"event: response.created",
			`data: {"type":"response.created","response":{"id":"resp_rate_limit"}}`,
			"",
			"event: response.failed",
			`data: {"type":"response.failed","response":{"id":"resp_rate_limit","status":"failed","error":{"code":"rate_limit_exceeded","message":"Concurrency limit exceeded; retry later"}}}`,
			"",
		}, "\n"))),
		Header: http.Header{
			"X-Codex-Primary-Used-Percent":        []string{"100"},
			"X-Codex-Primary-Reset-After-Seconds": []string{"604800"},
			"Retry-After":                         []string{"1"},
		},
	}
	account := &Account{ID: 12, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Name: "oauth-account"}

	_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "model", "model")
	require.Error(t, err)

	rateLimitSvc.openAIOAuth429DynamicMu.Lock()
	stat := rateLimitSvc.openAIOAuth429DynamicStat[account.ID]
	rateLimitSvc.openAIOAuth429DynamicMu.Unlock()
	require.NotNil(t, stat)
	require.Equal(t, 1, stat.total)
	require.Equal(t, 1, stat.count429)
	require.Nil(t, stat.usage5h)
	require.Nil(t, stat.usage7d)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
}

func TestOpenAIStreamingResponseFailedRateLimitPersistsAPIKeyCooldown(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &openAIStreamRateLimitAccountRepo{}
	svc := &OpenAIGatewayService{
		cfg:              &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}},
		rateLimitService: NewRateLimitService(repo, nil, &config.Config{}, nil, nil),
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			"event: response.failed",
			`data: {"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded","message":"Concurrency limit exceeded; retry later"}}}`,
			"",
		}, "\n"))),
		Header: http.Header{"Retry-After": []string{"1"}},
	}
	account := &Account{ID: 13, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Name: "api-key-account"}

	_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "model", "model")
	require.Error(t, err)
	require.Len(t, repo.rateLimitResets, 1)
	require.WithinDuration(t, time.Now().Add(time.Second), repo.rateLimitResets[0], time.Second)
}

func TestOpenAIStreamingResponseFailedBeforeOutputServerOverloadedCodeReturnsDirectRequestError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			StreamDataIntervalTimeout: 0,
			StreamKeepaliveInterval:   0,
			MaxLineSize:               defaultMaxLineSize,
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			"event: response.created",
			`data: {"type":"response.created","response":{"id":"resp_1"}}`,
			"",
			"event: response.failed",
			`data: {"type":"response.failed","response":{"id":"resp_1","error":{"code":"server_is_overloaded","message":"Please retry later."}}}`,
			"",
		}, "\n"))),
		Header: http.Header{"X-Request-Id": []string{"rid-overloaded-failed"}},
	}

	_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI, Name: "acc"}, time.Now(), "model", "model")
	require.Error(t, err)
	var requestErr *OpenAIUpstreamRequestError
	require.ErrorAs(t, err, &requestErr)
	require.Equal(t, http.StatusServiceUnavailable, requestErr.StatusCode)
	require.Equal(t, "server_is_overloaded", requestErr.Code)
	require.Equal(t, "Please retry later.", requestErr.Message)
	require.False(t, c.Writer.Written())
	require.Empty(t, rec.Body.String())
}

func TestOpenAIStreamingPreambleOnlyMissingTerminalReturnsFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			StreamDataIntervalTimeout: 0,
			StreamKeepaliveInterval:   0,
			MaxLineSize:               defaultMaxLineSize,
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			"event: response.created",
			`data: {"type":"response.created","response":{"id":"resp_1"}}`,
			"",
			"event: response.in_progress",
			`data: {"type":"response.in_progress","response":{"id":"resp_1"}}`,
			"",
		}, "\n"))),
		Header: http.Header{"X-Request-Id": []string{"rid-missing-terminal"}},
	}

	_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI, Name: "acc"}, time.Now(), "model", "model")
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.False(t, c.Writer.Written())
	require.Empty(t, rec.Body.String())
}

func TestOpenAIStreamingPreambleKeepaliveUsesDownstreamIdle(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			StreamDataIntervalTimeout: 0,
			StreamKeepaliveInterval:   1,
			MaxLineSize:               defaultMaxLineSize,
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	pr, pw := io.Pipe()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       pr,
		Header:     http.Header{},
	}

	go func() {
		defer func() { _ = pw.Close() }()
		_, _ = pw.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n"))
		for i := 0; i < 6; i++ {
			time.Sleep(250 * time.Millisecond)
			_, _ = pw.Write([]byte("data: {\"type\":\"response.in_progress\",\"response\":{\"id\":\"resp_1\"}}\n\n"))
		}
		_, _ = pw.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}}\n\n"))
	}()

	result, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI, Name: "acc"}, time.Now(), "model", "model")
	_ = pr.Close()
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Contains(t, rec.Body.String(), ":\n\n")
	require.Contains(t, rec.Body.String(), "response.completed")
}

func TestOpenAIStreamingNormalizesTerminalOutputFromDeltas(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			StreamDataIntervalTimeout: 0,
			StreamKeepaliveInterval:   0,
			MaxLineSize:               defaultMaxLineSize,
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"response.created","response":{"id":"resp_sdk_parse"}}`,
			"",
			`data: {"type":"response.output_text.delta","delta":"pon"}`,
			"",
			`data: {"type":"response.output_text.delta","delta":"g"}`,
			"",
			`data: {"type":"response.completed","response":{"id":"resp_sdk_parse","status":"completed","output":null,"usage":{"input_tokens":1,"output_tokens":1}}}`,
			"",
		}, "\n"))),
		Header: http.Header{"X-Request-Id": []string{"rid-sdk-parse"}},
	}

	result, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI, Name: "acc"}, time.Now(), "model", "model")
	require.NoError(t, err)
	require.NotNil(t, result)

	terminalType, terminalPayload, ok := extractOpenAISSETerminalEvent(rec.Body.String())
	require.True(t, ok)
	require.Equal(t, "response.completed", terminalType)
	output := gjson.GetBytes(terminalPayload, "response.output")
	require.True(t, output.IsArray())
	require.Len(t, output.Array(), 1)
	require.Equal(t, "pong", gjson.GetBytes(terminalPayload, "response.output.0.content.0.text").String())
}

func TestOpenAIStreamingNormalizesTerminalOutputToEmptyArray(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			StreamDataIntervalTimeout: 0,
			StreamKeepaliveInterval:   0,
			MaxLineSize:               defaultMaxLineSize,
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"response.completed","response":{"id":"resp_empty","status":"completed","output":null,"usage":{"input_tokens":1,"output_tokens":0}}}`,
			"",
		}, "\n"))),
		Header: http.Header{"X-Request-Id": []string{"rid-empty-output"}},
	}

	result, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI, Name: "acc"}, time.Now(), "model", "model")
	require.NoError(t, err)
	require.NotNil(t, result)

	terminalType, terminalPayload, ok := extractOpenAISSETerminalEvent(rec.Body.String())
	require.True(t, ok)
	require.Equal(t, "response.completed", terminalType)
	output := gjson.GetBytes(terminalPayload, "response.output")
	require.True(t, output.IsArray())
	require.Len(t, output.Array(), 0)
}

func TestOpenAIStreamingPolicyResponseFailedBeforeOutputPassesThrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			StreamDataIntervalTimeout: 0,
			StreamKeepaliveInterval:   0,
			MaxLineSize:               defaultMaxLineSize,
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			"event: response.created",
			`data: {"type":"response.created","response":{"id":"resp_1"}}`,
			"",
			"event: response.failed",
			`data: {"type":"response.failed","error":{"type":"safety_error","message":"This request has been flagged for potentially high-risk cyber activity."}}`,
			"",
		}, "\n"))),
		Header: http.Header{"X-Request-Id": []string{"rid-policy-failed"}},
	}

	_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI, Name: "acc"}, time.Now(), "model", "model")
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr))
	require.True(t, c.Writer.Written())
	require.Contains(t, rec.Body.String(), "response.failed")
	require.Contains(t, rec.Body.String(), "high-risk cyber activity")
}

func TestOpenAIStreamingClientDisconnectDrainsUpstreamUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			StreamDataIntervalTimeout: 0,
			StreamKeepaliveInterval:   0,
			MaxLineSize:               defaultMaxLineSize,
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	c.Writer = &failingGinWriter{ResponseWriter: c.Writer, failAfter: 0}

	pr, pw := io.Pipe()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       pr,
		Header:     http.Header{},
	}

	go func() {
		defer func() { _ = pw.Close() }()
		_, _ = pw.Write([]byte("data: {\"type\":\"response.in_progress\",\"response\":{}}\n\n"))
		_, _ = pw.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"input_tokens_details\":{\"cached_tokens\":1}}}}\n\n"))
	}()

	result, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1}, time.Now(), "model", "model")
	_ = pr.Close()
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if result == nil || result.usage == nil {
		t.Fatalf("expected usage result")
	}
	if result.usage.InputTokens != 3 || result.usage.OutputTokens != 5 || result.usage.CacheReadInputTokens != 1 {
		t.Fatalf("unexpected usage: %+v", *result.usage)
	}
	if strings.Contains(rec.Body.String(), "event: error") || strings.Contains(rec.Body.String(), "write_failed") {
		t.Fatalf("expected no injected SSE error event, got %q", rec.Body.String())
	}
}

func TestOpenAIStreamingRemoteCompactionClientDisconnectThenCompletedSucceeds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	markRemoteCompactionV2Request(t, c, context.Background())
	c.Writer = &failingGinWriter{ResponseWriter: c.Writer, failAfter: 0}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":5}}}\n\n")),
		Header: http.Header{},
	}

	_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1}, time.Now(), "model", "model")

	require.NoError(t, err)
	outcome, ok := GetOpenAIRemoteCompactionSemanticOutcome(c)
	require.True(t, ok)
	require.Equal(t, OpenAIRemoteCompactionSemanticOutcomeSucceeded, outcome)
}

func TestOpenAIStreamingPassthroughRemoteCompactionClientDisconnectThenCompletedSucceeds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	markRemoteCompactionV2Request(t, c, context.Background())
	c.Writer = &failingGinWriter{ResponseWriter: c.Writer, failAfter: 0}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":5}}}\n\n")),
		Header: http.Header{},
	}

	_, err := svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, &Account{ID: 1}, time.Now(), "", "")

	require.NoError(t, err)
	outcome, ok := GetOpenAIRemoteCompactionSemanticOutcome(c)
	require.True(t, ok)
	require.Equal(t, OpenAIRemoteCompactionSemanticOutcomeSucceeded, outcome)
}

func TestOpenAIStreamingRemoteCompactionTerminalOutcomes(t *testing.T) {
	testOpenAIStreamingRemoteCompactionTerminalOutcomes(t, "adapter", func(svc *OpenAIGatewayService, c *gin.Context, resp *http.Response) error {
		_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1}, time.Now(), "model", "model")
		return err
	})
}

func TestOpenAIStreamingPassthroughRemoteCompactionTerminalOutcomes(t *testing.T) {
	testOpenAIStreamingRemoteCompactionTerminalOutcomes(t, "passthrough", func(svc *OpenAIGatewayService, c *gin.Context, resp *http.Response) error {
		_, err := svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, &Account{ID: 1}, time.Now(), "", "")
		return err
	})
}

func testOpenAIStreamingRemoteCompactionTerminalOutcomes(
	t *testing.T,
	implementation string,
	forward func(*OpenAIGatewayService, *gin.Context, *http.Response) error,
) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name        string
		eventType   string
		wantOutcome OpenAIRemoteCompactionSemanticOutcome
	}{
		{name: "incomplete fails", eventType: "response.incomplete", wantOutcome: OpenAIRemoteCompactionSemanticOutcomeFailed},
		{name: "cancelled fails", eventType: "response.cancelled", wantOutcome: OpenAIRemoteCompactionSemanticOutcomeFailed},
		{name: "canceled fails", eventType: "response.canceled", wantOutcome: OpenAIRemoteCompactionSemanticOutcomeFailed},
		{name: "failed fails", eventType: "response.failed", wantOutcome: OpenAIRemoteCompactionSemanticOutcomeFailed},
		{name: "completed succeeds", eventType: "response.completed", wantOutcome: OpenAIRemoteCompactionSemanticOutcomeSucceeded},
		{name: "done succeeds", eventType: "response.done", wantOutcome: OpenAIRemoteCompactionSemanticOutcomeSucceeded},
	}
	for _, tt := range tests {
		t.Run(implementation+"/"+tt.name, func(t *testing.T) {
			svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			markRemoteCompactionV2Request(t, c, context.Background())
			payload := fmt.Sprintf("event: %s\ndata: {\"type\":%q,\"response\":{\"id\":\"resp_terminal\"}}\n\n", tt.eventType, tt.eventType)
			resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(payload)), Header: http.Header{}}

			_ = forward(svc, c, resp)
			outcome, ok := GetOpenAIRemoteCompactionSemanticOutcome(c)
			require.True(t, ok)
			require.Equal(t, tt.wantOutcome, outcome)
		})
	}
}

func TestOpenAIStreamingMissingTerminalEventReturnsIncompleteError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			StreamDataIntervalTimeout: 0,
			StreamKeepaliveInterval:   0,
			MaxLineSize:               defaultMaxLineSize,
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	markRemoteCompactionV2Request(t, c, context.Background())

	pr, pw := io.Pipe()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       pr,
		Header:     http.Header{},
	}

	go func() {
		defer func() { _ = pw.Close() }()
		_, _ = pw.Write([]byte("data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"message\"},\"output_index\":0}\n\n"))
	}()

	_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1}, time.Now(), "model", "model")
	_ = pr.Close()
	if err == nil || !strings.Contains(err.Error(), "missing terminal event") {
		t.Fatalf("expected missing terminal event error, got %v", err)
	}
	outcome, ok := GetOpenAIRemoteCompactionSemanticOutcome(c)
	require.True(t, ok)
	require.Equal(t, OpenAIRemoteCompactionSemanticOutcomeFailed, outcome)
}

func TestOpenAIStreamingPassthroughMissingTerminalEventReturnsIncompleteError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			MaxLineSize: defaultMaxLineSize,
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	markRemoteCompactionV2Request(t, c, context.Background())

	pr, pw := io.Pipe()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       pr,
		Header:     http.Header{},
	}

	go func() {
		defer func() { _ = pw.Close() }()
		_, _ = pw.Write([]byte("data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"message\"},\"output_index\":0}\n\n"))
	}()

	_, err := svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, &Account{ID: 1}, time.Now(), "", "")
	_ = pr.Close()
	if err == nil || !strings.Contains(err.Error(), "missing terminal event") {
		t.Fatalf("expected missing terminal event error, got %v", err)
	}
	outcome, ok := GetOpenAIRemoteCompactionSemanticOutcome(c)
	require.True(t, ok)
	require.Equal(t, OpenAIRemoteCompactionSemanticOutcomeFailed, outcome)
}

func TestOpenAIStreamingPassthroughRecognizedResponseFailedBeforeOutputReturnsDirectRequestError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			MaxLineSize: defaultMaxLineSize,
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			"event: response.created",
			`data: {"type":"response.created","response":{"id":"resp_recognized"}}`,
			"",
			"event: response.failed",
			`data: {"type":"response.failed","response":{"id":"resp_recognized","status":"failed","error":{"code":"server_is_overloaded","type":"response.failed","message":"Bearer sk-private"},"safety_identifier":"user-private"}}`,
			"",
		}, "\n"))),
		Header: http.Header{"X-Request-Id": []string{"rid-passthrough-recognized"}},
	}

	result, err := svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI, Name: "acc"}, time.Now(), "", "")
	require.Error(t, err)
	require.NotErrorAs(t, err, new(*UpstreamFailoverError))
	var requestErr *OpenAIUpstreamRequestError
	require.ErrorAs(t, err, &requestErr)
	require.Equal(t, http.StatusServiceUnavailable, requestErr.StatusCode)
	require.Equal(t, "server_is_overloaded", requestErr.Code)
	require.False(t, requestErr.OutputStarted)
	require.NotNil(t, result)
	require.False(t, c.Writer.Written())
	require.Empty(t, rec.Body.String())
}

func TestOpenAIStreamingPassthroughResponseFailedBeforeOutputReturnsFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			MaxLineSize: defaultMaxLineSize,
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			"event: response.created",
			`data: {"type":"response.created","response":{"id":"resp_1"}}`,
			"",
			"event: response.failed",
			`data: {"type":"response.failed","error":{"message":"upstream processing failed"}}`,
			"",
		}, "\n"))),
		Header: http.Header{"X-Request-Id": []string{"rid-passthrough-failed"}},
	}

	_, err := svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI, Name: "acc"}, time.Now(), "", "")
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.Contains(t, string(failoverErr.ResponseBody), "upstream processing failed")
	require.False(t, c.Writer.Written())
	require.Empty(t, rec.Body.String())
}

func TestOpenAIStreamingPassthroughResponseDoneWithoutDoneMarkerStillSucceeds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			MaxLineSize: defaultMaxLineSize,
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	pr, pw := io.Pipe()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       pr,
		Header:     http.Header{},
	}

	go func() {
		defer func() { _ = pw.Close() }()
		_, _ = pw.Write([]byte("data: {\"type\":\"response.done\",\"response\":{\"usage\":{\"input_tokens\":2,\"output_tokens\":3,\"input_tokens_details\":{\"cached_tokens\":1}}}}\n\n"))
	}()

	result, err := svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, &Account{ID: 1}, time.Now(), "", "")
	_ = pr.Close()
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.usage)
	require.Equal(t, 2, result.usage.InputTokens)
	require.Equal(t, 3, result.usage.OutputTokens)
	require.Equal(t, 1, result.usage.CacheReadInputTokens)
}

func TestOpenAIStreamingPassthroughResponseIncompleteWithoutDoneMarkerStillSucceeds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			MaxLineSize: defaultMaxLineSize,
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	pr, pw := io.Pipe()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       pr,
		Header:     http.Header{},
	}

	go func() {
		defer func() { _ = pw.Close() }()
		_, _ = pw.Write([]byte("data: {\"type\":\"response.incomplete\",\"response\":{\"usage\":{\"input_tokens\":2,\"output_tokens\":3,\"input_tokens_details\":{\"cached_tokens\":1}}}}\n\n"))
	}()

	result, err := svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, &Account{ID: 1}, time.Now(), "", "")
	_ = pr.Close()
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.usage)
	require.Equal(t, 2, result.usage.InputTokens)
	require.Equal(t, 3, result.usage.OutputTokens)
	require.Equal(t, 1, result.usage.CacheReadInputTokens)
}

func TestOpenAIStreamingTooLong(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			StreamDataIntervalTimeout: 0,
			StreamKeepaliveInterval:   0,
			MaxLineSize:               64 * 1024,
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	pr, pw := io.Pipe()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       pr,
		Header:     http.Header{},
	}

	go func() {
		defer func() { _ = pw.Close() }()
		// 写入超过 MaxLineSize 的单行数据，触发 ErrTooLong
		payload := "data: " + strings.Repeat("a", 128*1024) + "\n"
		_, _ = pw.Write([]byte(payload))
	}()

	_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 2}, time.Now(), "model", "model")
	_ = pr.Close()

	if !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("expected ErrTooLong, got %v", err)
	}
	if !strings.Contains(rec.Body.String(), "\"type\":\"error\"") || !strings.Contains(rec.Body.String(), "response_too_large") {
		t.Fatalf("expected OpenAI-compatible error SSE event, got %q", rec.Body.String())
	}
}

func TestOpenAIHandleNonStreamingResponse_RejectsSuccessfulUnusableUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name string
		body string
	}{
		{name: "missing usage", body: `{"id":"resp_missing","output":[]}`},
		{name: "non-object usage", body: `{"id":"resp_invalid","output":[],"usage":"sk-upstream-secret"}`},
		{name: "empty usage", body: `{"id":"resp_empty","output":[],"usage":{}}`},
		{name: "string tokens", body: `{"id":"resp_string","output":[],"usage":{"input_tokens":"bad","output_tokens":null}}`},
		{name: "fractional and negative tokens", body: `{"id":"resp_bad_numbers","output":[],"usage":{"input_tokens":1.5,"output_tokens":-2}}`},
		{name: "negative nested cache tokens", body: `{"id":"resp_bad_cache","output":[],"usage":{"input_tokens":10,"output_tokens":0,"input_tokens_details":{"cached_tokens":-1}}}`},
		{name: "fractional nested image tokens", body: `{"id":"resp_bad_image","output":[],"usage":{"input_tokens":10,"output_tokens":0,"output_tokens_details":{"image_tokens":1.5}}}`},
		{name: "string nested cache write tokens", body: `{"id":"resp_bad_cache_write","output":[],"usage":{"input_tokens":10,"output_tokens":0,"input_tokens_details":{"cache_write_tokens":"1"}}}`},
		{name: "malformed token details", body: `{"id":"resp_bad_details","output":[],"usage":{"input_tokens":10,"output_tokens":0,"input_tokens_details":"malformed"}}`},
		{name: "malformed hosted image usage", body: `{"id":"resp_bad_hosted_image","output":[],"usage":{"input_tokens":10,"output_tokens":0},"tool_usage":{"image_gen":"malformed"}}`},
		{name: "malformed json", body: `{"id":"resp_malformed","output":[],"usage":{"input_tokens":1}`},
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
			svc := &OpenAIGatewayService{cfg: &config.Config{}}

			result, err := svc.handleNonStreamingResponse(c.Request.Context(), resp, c, &Account{}, "model", "model")

			require.Error(t, err)
			require.Nil(t, result)
			require.Equal(t, http.StatusBadGateway, rec.Code)
			require.Equal(t, "upstream_error", gjson.Get(rec.Body.String(), "error.type").String())
			require.NotContains(t, rec.Body.String(), "sk-upstream-secret")
			require.NotContains(t, err.Error(), "sk-upstream-secret")
		})
	}
}

func TestOpenAISSEToJSON_RejectsSuccessfulUnusableUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, passthrough := range []bool{false, true} {
		name := "native"
		if passthrough {
			name = "passthrough"
		}
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			body := []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_bad_sse\",\"output\":[],\"usage\":{\"input_tokens\":-1,\"output_tokens\":0}}}\n\ndata: [DONE]\n\n")
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			}
			svc := &OpenAIGatewayService{cfg: &config.Config{}}

			var err error
			if passthrough {
				_, err = svc.handlePassthroughSSEToJSON(resp, c, body, "model", "model")
			} else {
				_, err = svc.handleSSEToJSON(resp, c, body, "model", "model")
			}

			require.Error(t, err)
			require.Equal(t, http.StatusBadGateway, rec.Code)
			require.Equal(t, "upstream_error", gjson.Get(rec.Body.String(), "error.type").String())
		})
	}
}

func TestOpenAISSEToJSON_RejectsMissingTerminalUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, passthrough := range []bool{false, true} {
		name := "native"
		if passthrough {
			name = "passthrough"
		}
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			body := []byte("data: not-json\n\ndata: [DONE]\n\n")
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}}

			var err error
			if passthrough {
				_, err = svc.handlePassthroughSSEToJSON(resp, c, body, "model", "model")
			} else {
				_, err = svc.handleSSEToJSON(resp, c, body, "model", "model")
			}

			require.Error(t, err)
			require.Equal(t, http.StatusBadGateway, rec.Code)
			require.Equal(t, "upstream_error", gjson.Get(rec.Body.String(), "error.type").String())
		})
	}
}

func TestOpenAIHandleNonStreamingResponse_PreservesHostedImageUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	body := []byte(`{"id":"resp_image","output":[],"usage":{"input_tokens":10,"output_tokens":0},"tool_usage":{"image_gen":{"input_tokens_details":{"image_tokens":3},"output_tokens_details":{"image_tokens":2}}}}`)
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: http.Header{"Content-Type": []string{"application/json"}}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}}

	result, err := svc.handleNonStreamingResponse(c.Request.Context(), resp, c, &Account{}, "model", "model")

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 3, result.usage.ImageInputTokens)
	require.Equal(t, 2, result.usage.ImageOutputTokens)
}

func TestOpenAIHandleNonStreamingResponse_RejectsInvalidHostedImageUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	body := []byte(`{"id":"resp_bad_image","output":[],"usage":{"input_tokens":10,"output_tokens":0},"tool_usage":{"image_gen":{"input_tokens_details":{"image_tokens":-1}}}}`)
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: http.Header{"Content-Type": []string{"application/json"}}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}}

	result, err := svc.handleNonStreamingResponse(c.Request.Context(), resp, c, &Account{}, "model", "model")

	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, http.StatusBadGateway, rec.Code)
}

func TestOpenAIHandleNonStreamingResponse_AcceptsExplicitZeroUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	body := []byte(`{"id":"resp_zero","output":[],"usage":{"input_tokens":0,"output_tokens":0}}`)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
	svc := &OpenAIGatewayService{cfg: &config.Config{}}

	result, err := svc.handleNonStreamingResponse(c.Request.Context(), resp, c, &Account{}, "model", "model")

	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.usage)
	require.Zero(t, result.usage.InputTokens)
	require.Zero(t, result.usage.OutputTokens)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestOpenAINonStreamingContentTypePassThrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Security: config.SecurityConfig{
			ResponseHeaders: config.ResponseHeaderConfig{Enabled: false},
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	body := []byte(`{"usage":{"input_tokens":1,"output_tokens":2,"input_tokens_details":{"cached_tokens":0}}}`)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/vnd.test+json"}},
	}

	_, err := svc.handleNonStreamingResponse(c.Request.Context(), resp, c, &Account{}, "model", "model")
	if err != nil {
		t.Fatalf("handleNonStreamingResponse error: %v", err)
	}

	if !strings.Contains(rec.Header().Get("Content-Type"), "application/vnd.test+json") {
		t.Fatalf("expected Content-Type passthrough, got %q", rec.Header().Get("Content-Type"))
	}
}

func TestOpenAINonStreamingContentTypeDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Security: config.SecurityConfig{
			ResponseHeaders: config.ResponseHeaderConfig{Enabled: false},
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	body := []byte(`{"usage":{"input_tokens":1,"output_tokens":2,"input_tokens_details":{"cached_tokens":0}}}`)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     http.Header{},
	}

	_, err := svc.handleNonStreamingResponse(c.Request.Context(), resp, c, &Account{}, "model", "model")
	if err != nil {
		t.Fatalf("handleNonStreamingResponse error: %v", err)
	}

	if !strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("expected default Content-Type, got %q", rec.Header().Get("Content-Type"))
	}
}

func TestOpenAIStreamingHeadersOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Security: config.SecurityConfig{
			ResponseHeaders: config.ResponseHeaderConfig{Enabled: false},
		},
		Gateway: config.GatewayConfig{
			StreamDataIntervalTimeout: 0,
			StreamKeepaliveInterval:   0,
			MaxLineSize:               defaultMaxLineSize,
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	pr, pw := io.Pipe()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       pr,
		Header: http.Header{
			"Cache-Control": []string{"upstream"},
			"X-Request-Id":  []string{"req-123"},
			"Content-Type":  []string{"application/custom"},
		},
	}

	go func() {
		defer func() { _ = pw.Close() }()
		_, _ = pw.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{}}\n\n"))
	}()

	_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1}, time.Now(), "model", "model")
	_ = pr.Close()
	if err != nil {
		t.Fatalf("handleStreamingResponse error: %v", err)
	}

	if rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("expected Cache-Control override, got %q", rec.Header().Get("Cache-Control"))
	}
	if rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("expected Content-Type override, got %q", rec.Header().Get("Content-Type"))
	}
	if rec.Header().Get("X-Request-Id") != "req-123" {
		t.Fatalf("expected X-Request-Id passthrough, got %q", rec.Header().Get("X-Request-Id"))
	}
}

func TestOpenAIStreamingReuseScannerBufferAndStillWorks(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Gateway: config.GatewayConfig{
			StreamDataIntervalTimeout: 0,
			StreamKeepaliveInterval:   0,
			MaxLineSize:               defaultMaxLineSize,
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	pr, pw := io.Pipe()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       pr,
		Header:     http.Header{},
	}

	go func() {
		defer func() { _ = pw.Close() }()
		_, _ = pw.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"input_tokens_details\":{\"cached_tokens\":3}}}}\n\n"))
	}()

	result, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1}, time.Now(), "model", "model")
	_ = pr.Close()
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.usage)
	require.Equal(t, 1, result.usage.InputTokens)
	require.Equal(t, 2, result.usage.OutputTokens)
	require.Equal(t, 3, result.usage.CacheReadInputTokens)
}

func TestOpenAIInvalidBaseURLWhenAllowlistDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{Enabled: false},
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	account := &Account{
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"base_url": "://invalid-url"},
	}

	_, err := svc.buildUpstreamRequest(c.Request.Context(), c, account, []byte("{}"), "token", false, "", false)
	if err == nil {
		t.Fatalf("expected error for invalid base_url when allowlist disabled")
	}
}

func TestOpenAIValidateUpstreamBaseURLRejectsQueryAndFragment(t *testing.T) {
	for _, cfg := range []*config.Config{
		{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
		{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{
			Enabled: true, UpstreamHosts: []string{"api.openai.com"}, AllowPrivateHosts: false,
		}}},
	} {
		svc := &OpenAIGatewayService{cfg: cfg}
		for _, raw := range []string{"https://api.openai.com?tenant=x", "https://api.openai.com#fragment"} {
			_, err := svc.validateUpstreamBaseURL(raw)
			require.Error(t, err, raw)
		}
	}
}

func TestOpenAIValidateUpstreamBaseURLDisabledRequiresHTTPS(t *testing.T) {
	cfg := &config.Config{
		Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{Enabled: false},
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg}

	if _, err := svc.validateUpstreamBaseURL("http://not-https.example.com"); err == nil {
		t.Fatalf("expected http to be rejected when allow_insecure_http is false")
	}
	normalized, err := svc.validateUpstreamBaseURL("https://example.com")
	if err != nil {
		t.Fatalf("expected https to be allowed when allowlist disabled, got %v", err)
	}
	if normalized != "https://example.com" {
		t.Fatalf("expected raw url passthrough, got %q", normalized)
	}
}

func TestOpenAIValidateUpstreamBaseURLDisabledAllowsHTTP(t *testing.T) {
	cfg := &config.Config{
		Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{
				Enabled:           false,
				AllowInsecureHTTP: true,
			},
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg}

	normalized, err := svc.validateUpstreamBaseURL("http://not-https.example.com")
	if err != nil {
		t.Fatalf("expected http allowed when allow_insecure_http is true, got %v", err)
	}
	if normalized != "http://not-https.example.com" {
		t.Fatalf("expected raw url passthrough, got %q", normalized)
	}
}

func TestOpenAIValidateUpstreamBaseURLEnabledEnforcesAllowlist(t *testing.T) {
	cfg := &config.Config{
		Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{
				Enabled:       true,
				UpstreamHosts: []string{"example.com"},
			},
		},
	}
	svc := &OpenAIGatewayService{cfg: cfg}

	if _, err := svc.validateUpstreamBaseURL("https://example.com"); err != nil {
		t.Fatalf("expected allowlisted host to pass, got %v", err)
	}
	if _, err := svc.validateUpstreamBaseURL("https://evil.com"); err == nil {
		t.Fatalf("expected non-allowlisted host to fail")
	}
}

func TestOpenAIUpdateCodexUsageSnapshotFromHeaders(t *testing.T) {
	repo := &snapshotUpdateAccountRepo{
		updateExtraCalls: make(chan map[string]any, 1),
		sessionWindowEndCalls: make(chan struct {
			id  int64
			end time.Time
		}, 1),
	}
	svc := &OpenAIGatewayService{accountRepo: repo}
	headers := http.Header{}
	headers.Set("x-codex-primary-used-percent", "12")
	headers.Set("x-codex-secondary-used-percent", "34")
	headers.Set("x-codex-primary-window-minutes", "300")
	headers.Set("x-codex-secondary-window-minutes", "10080")
	headers.Set("x-codex-primary-reset-after-seconds", "600")
	headers.Set("x-codex-secondary-reset-after-seconds", "86400")

	svc.UpdateCodexUsageSnapshotFromHeaders(context.Background(), 123, headers)

	var resetAt string
	select {
	case updates := <-repo.updateExtraCalls:
		require.Equal(t, 12.0, updates["codex_5h_used_percent"])
		require.Equal(t, 34.0, updates["codex_7d_used_percent"])
		require.Equal(t, 600, updates["codex_5h_reset_after_seconds"])
		require.Equal(t, 86400, updates["codex_7d_reset_after_seconds"])
		resetAt, _ = updates["codex_5h_reset_at"].(string)
		require.NotEmpty(t, resetAt)
	case <-time.After(2 * time.Second):
		t.Fatal("expected UpdateExtra to be called")
	}

	select {
	case call := <-repo.sessionWindowEndCalls:
		require.Equal(t, int64(123), call.id)
		want, err := parseTime(resetAt)
		require.NoError(t, err)
		require.True(t, call.end.Equal(want), "session window end = %v, want %v", call.end, want)
	case <-time.After(2 * time.Second):
		t.Fatal("expected UpdateSessionWindowEnd to be called")
	}
}

func TestOpenAIResponsesRequestPathSuffix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "exact v1 responses", path: "/v1/responses", want: ""},
		{name: "compact v1 responses", path: "/v1/responses/compact", want: "/compact"},
		{name: "trailing slash normalized", path: "/responses/compact/", want: "/compact"},
		{name: "nested suffix", path: "/v1/responses/compact/detail", want: "/compact/detail"},
		{name: "normalized OpenAI prefix", path: "/openai/v1/responses/compact/detail", want: "/compact/detail"},
		{name: "unknown prefix rejected", path: "/proxy/v1/responses/compact/detail", want: ""},
		{name: "unrelated path", path: "/v1/chat/completions", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c.Request = httptest.NewRequest(http.MethodPost, tt.path, nil)
			require.Equal(t, tt.want, openAIResponsesRequestPathSuffix(c))
		})
	}
}

func TestNormalizeOpenAICompactRequestBodyPreservesCurrentCodexPayloadFields(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":"compact me"}],"instructions":"compact-test","tools":[{"type":"function","name":"shell"}],"parallel_tool_calls":true,"reasoning":{"effort":"high"},"text":{"verbosity":"low"},"service_tier":"priority","prompt_cache_key":"cache_123","metadata":{"keep":"yes"},"max_output_tokens":1024,"temperature":0.2,"top_p":0.9,"truncation":"auto","background":false,"conversation":"none","safety_identifier":"safe-user","user":"end-user","previous_response_id":"resp_123","type":"response.create","generate":false,"store":true,"stream":true,"include":["reasoning.encrypted_content"],"tool_choice":"auto","client_metadata":{"x":"y"}}`)

	normalized, changed, err := normalizeOpenAICompactRequestBody(body)

	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "gpt-5.5", gjson.GetBytes(normalized, "model").String())
	require.True(t, gjson.GetBytes(normalized, "tools").Exists())
	require.True(t, gjson.GetBytes(normalized, "parallel_tool_calls").Bool())
	require.Equal(t, "high", gjson.GetBytes(normalized, "reasoning.effort").String())
	require.Equal(t, "low", gjson.GetBytes(normalized, "text.verbosity").String())
	require.Equal(t, "priority", gjson.GetBytes(normalized, "service_tier").String())
	require.Equal(t, "cache_123", gjson.GetBytes(normalized, "prompt_cache_key").String())
	// 以下字段不在 Codex CompactionInput schema 中，应被丢弃。
	require.False(t, gjson.GetBytes(normalized, "metadata").Exists())
	require.False(t, gjson.GetBytes(normalized, "max_output_tokens").Exists())
	require.False(t, gjson.GetBytes(normalized, "temperature").Exists())
	require.False(t, gjson.GetBytes(normalized, "top_p").Exists())
	require.False(t, gjson.GetBytes(normalized, "truncation").Exists())
	require.False(t, gjson.GetBytes(normalized, "background").Exists())
	require.False(t, gjson.GetBytes(normalized, "conversation").Exists())
	require.False(t, gjson.GetBytes(normalized, "safety_identifier").Exists())
	require.False(t, gjson.GetBytes(normalized, "user").Exists())
	require.False(t, gjson.GetBytes(normalized, "previous_response_id").Exists())
	require.False(t, gjson.GetBytes(normalized, "type").Exists())
	require.False(t, gjson.GetBytes(normalized, "generate").Exists())
	require.False(t, gjson.GetBytes(normalized, "store").Exists())
	require.False(t, gjson.GetBytes(normalized, "stream").Exists())
	require.False(t, gjson.GetBytes(normalized, "include").Exists())
	require.False(t, gjson.GetBytes(normalized, "tool_choice").Exists())
	require.False(t, gjson.GetBytes(normalized, "client_metadata").Exists())
}

func TestOpenAIForwardHTTPDoesNotSendWSCreateFrameFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"type":"response.create","generate":false,"previous_response_id":"resp_warm","model":"gpt-5","stream":false,"input":"hello","metadata":{"drop":"yes"},"client_metadata":{"keep":"yes"}}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)

	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"resp_1","model":"gpt-5","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)),
	}}
	svc := &OpenAIGatewayService{
		cfg:          &config.Config{},
		httpUpstream: upstream,
	}
	account := &Account{
		ID:          77,
		Name:        "openai-apikey",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "test-key"},
	}

	result, err := svc.Forward(context.Background(), c, account, body)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, upstream.lastReq)
	require.False(t, gjson.GetBytes(upstream.lastBody, "type").Exists())
	require.False(t, gjson.GetBytes(upstream.lastBody, "generate").Exists())
	require.False(t, gjson.GetBytes(upstream.lastBody, "previous_response_id").Exists())
	require.False(t, gjson.GetBytes(upstream.lastBody, "metadata").Exists())
	require.Equal(t, "yes", gjson.GetBytes(upstream.lastBody, "client_metadata.keep").String())
}

func TestOpenAIBuildUpstreamRequestNativeOpenAIRelayRejectsOAuth(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{Type: AccountTypeOAuth}

	req, err := svc.buildUpstreamRequestOpenAIPassthrough(context.Background(), nil, account, []byte(`{"model":"gpt-5"}`), "token")

	require.Nil(t, req)
	require.Error(t, err)
	require.Contains(t, err.Error(), "requires APIKey account")
}

func TestOpenAIBuildUpstreamRequestOpenAIOAuthAdapterAcceptsSetupToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader([]byte(`{"model":"gpt-5","input":"hello"}`)))

	svc := &OpenAIGatewayService{}
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeSetupToken}

	req, err := svc.buildUpstreamRequest(c.Request.Context(), c, account, []byte(`{"model":"gpt-5","input":"hello"}`), "token", false, "", true)

	require.NoError(t, err)
	require.Equal(t, chatgptCodexURL, req.URL.String())
	require.Equal(t, codexCLIVersion, req.Header.Get("Version"))
	require.NotEmpty(t, req.Header.Get("Session_Id"))
	require.Equal(t, HTTPUpstreamProfileOpenAI, HTTPUpstreamProfileFromContext(req.Context()))
}

func TestOpenAIBuildUpstreamRequestOpenAIOAuthAdapterPreservesCompactPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader([]byte(`{"model":"gpt-5"}`)))
	c.Request.Header.Set(openAICodexSubagentHeader, "compact")
	c.Request.Header.Set(openAITraceparentHeader, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	c.Request.Header.Set("X-Stainless-Timeout", "1")

	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	svc.cfg.Gateway.OpenAIPassthroughAllowTimeoutHeaders = true
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	req, err := svc.buildUpstreamRequestOpenAIOAuthAdapter(c.Request.Context(), c, account, []byte(`{"model":"gpt-5"}`), "token", "", true)
	require.NoError(t, err)
	require.Equal(t, chatgptCodexURL+"/compact", req.URL.String())
	require.Equal(t, "application/json", req.Header.Get("Accept"))
	require.Equal(t, codexCLIVersion, req.Header.Get("Version"))
	require.NotEmpty(t, req.Header.Get("Session_Id"))
	require.Equal(t, HTTPUpstreamProfileOpenAI, HTTPUpstreamProfileFromContext(req.Context()))
	require.Equal(t, req.Header.Get("Session_Id"), req.Header.Get(openAICodexSessionIDHeader))
	require.Equal(t, req.Header.Get("Conversation_Id"), req.Header.Get(openAICodexThreadIDHeader))
	require.NotEmpty(t, req.Header.Get(openAICodexClientRequestIDHeader))
	require.NotEmpty(t, req.Header.Get(openAICodexInstallationIDHeader))
	require.NotEmpty(t, req.Header.Get(openAICodexWindowIDHeader))
	require.Equal(t, "compact", req.Header.Get(openAICodexSubagentHeader))
	require.Equal(t, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", req.Header.Get(openAITraceparentHeader))
	require.Empty(t, req.Header.Get("X-Stainless-Timeout"), "OAuth adapter must not use APIKey passthrough timeout-header config")
	bodyBytes, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(bodyBytes, "client_metadata").Exists())
}

func TestOpenAIBuildUpstreamRequestCompactForcesJSONAcceptForOAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader([]byte(`{"model":"gpt-5"}`)))

	svc := &OpenAIGatewayService{}
	account := &Account{
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "chatgpt-acc"},
	}

	req, err := svc.buildUpstreamRequest(c.Request.Context(), c, account, []byte(`{"model":"gpt-5"}`), "token", false, "", true)
	require.NoError(t, err)
	require.Equal(t, chatgptCodexURL+"/compact", req.URL.String())
	require.Equal(t, "application/json", req.Header.Get("Accept"))
	require.Equal(t, codexCLIVersion, req.Header.Get("Version"))
	require.NotEmpty(t, req.Header.Get("Session_Id"))
	require.Equal(t, HTTPUpstreamProfileOpenAI, HTTPUpstreamProfileFromContext(req.Context()))
}

func TestCodexDefaultUserAgentUsesOfficialFingerprint(t *testing.T) {
	require.Equal(t, "0.144.1", codexCLIVersion)
	require.Equal(t, "Mac OS 26.5.0; arm64", codexOSFingerprint)
	require.Equal(t, "Apple_Terminal/470.2", codexTerminalName)
	require.Equal(t, "codex-tui", codexOfficialOriginator)
	require.Equal(t, "codex-tui/0.144.1 (Mac OS 26.5.0; arm64) Apple_Terminal/470.2 (codex-tui; 0.144.1)", codexCLIUserAgent)
}

func TestOpenAIBuildUpstreamRequestOAuthAddsCodexIdentityFallbacks(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader([]byte(`{"model":"gpt-5","input":"hello"}`)))
	c.Request.Header.Set("User-Agent", "opencode/0.9")
	c.Request.Header.Set("originator", "opencode")
	c.Request.Header.Set("OpenAI-Beta", "responses=experimental")

	svc := &OpenAIGatewayService{}
	account := &Account{
		ID:          42,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "chatgpt-acc"},
	}

	req, err := svc.buildUpstreamRequest(c.Request.Context(), c, account, []byte(`{"model":"gpt-5","input":"hello"}`), "token", true, "", false)
	require.NoError(t, err)
	require.Equal(t, "Bearer token", getHeaderRaw(req.Header, "Authorization"))
	require.Contains(t, req.Header, "Authorization")
	require.Equal(t, "chatgpt-acc", getHeaderRaw(req.Header, "ChatGPT-Account-ID"))
	require.Contains(t, req.Header, "ChatGPT-Account-ID")
	require.Equal(t, "text/event-stream", getHeaderRaw(req.Header, "Accept"))
	require.Contains(t, req.Header, "Accept")
	require.Equal(t, "application/json", getHeaderRaw(req.Header, "Content-Type"))
	require.Contains(t, req.Header, "Content-Type")
	require.Empty(t, getHeaderRaw(req.Header, "OpenAI-Beta"))
	require.Equal(t, codexOfficialOriginator, getHeaderRaw(req.Header, "originator"))
	require.Equal(t, codexCLIVersion, getHeaderRaw(req.Header, "Version"))
	require.Equal(t, codexCLIUserAgent, getHeaderRaw(req.Header, "User-Agent"))
	require.NotEmpty(t, req.Header.Get(openAICodexSessionIDHeader))
	require.NotEmpty(t, req.Header.Get(openAICodexThreadIDHeader))
	require.Equal(t, req.Header.Get(openAICodexThreadIDHeader), req.Header.Get(openAICodexClientRequestIDHeader))
	require.NotEmpty(t, req.Header.Get(openAICodexInstallationIDHeader))
	require.NotEmpty(t, req.Header.Get(openAICodexWindowIDHeader))

	bodyBytes, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Equal(t, req.Header.Get(openAICodexThreadIDHeader), gjson.GetBytes(bodyBytes, "prompt_cache_key").String())
	require.Equal(t, req.Header.Get(openAICodexInstallationIDHeader), gjson.GetBytes(bodyBytes, "client_metadata."+openAICodexInstallationIDHeader).String())
	// Codex HTTP 不在 client_metadata 中放 x-codex-window-id，仅放在 HTTP header。
	require.False(t, gjson.GetBytes(bodyBytes, "client_metadata."+openAICodexWindowIDHeader).Exists())
}

func TestOpenAIBuildUpstreamRequestOAuthIgnoresIncomingDesktopUserAgent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","input":"hello"}`)
	incomingUA := "codex-tui/9.9.9 (Injected OS; x64) Injected_Term/1.0 (codex-tui; 9.9.9)"
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("User-Agent", incomingUA)

	svc := &OpenAIGatewayService{}
	account := &Account{
		ID:          42,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "chatgpt-acc"},
	}

	req, err := svc.buildUpstreamRequest(c.Request.Context(), c, account, body, "token", true, "", false)
	require.NoError(t, err)
	require.Equal(t, codexCLIUserAgent, req.Header.Get("User-Agent"))
}

func TestOpenAIBuildUpstreamRequestOAuthSnapshotsConfiguredCodexUserAgentIntoFingerprint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","input":"hello"}`)
	configuredUA := "custom-codex/1.2.3"
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

	svc := &OpenAIGatewayService{
		settingService: NewSettingService(&openAITestSettingRepo{values: map[string]string{
			SettingKeyOpenAICodexUserAgent: configuredUA,
		}}, &config.Config{}),
	}
	account := &Account{
		ID:          42,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "chatgpt-acc"},
	}

	req, err := svc.buildUpstreamRequest(c.Request.Context(), c, account, body, "token", true, "", false)
	require.NoError(t, err)
	require.Equal(t, configuredUA, req.Header.Get("User-Agent"))
}

func TestOpenAIBuildUpstreamRequestOAuthPreservesCodexReleaseHTTPHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.4","input":"hello"}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set(openAICodexSessionIDHeader, "release-session")
	c.Request.Header.Set(openAICodexThreadIDHeader, "release-thread")
	c.Request.Header.Set(openAICodexClientRequestIDHeader, "release-client-request")
	c.Request.Header.Set(openAICodexWindowIDHeader, "attacker-release-thread:7")
	c.Request.Header.Set(openAICodexBetaFeaturesHeader, "feature-a,feature-b")
	c.Request.Header.Set(openAICodexTurnStateHeader, "release-turn-state")
	c.Request.Header.Set(openAICodexTurnMetadataHeader, "release-turn-metadata")

	svc := &OpenAIGatewayService{}
	account := &Account{
		ID:          42,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "chatgpt-acc"},
	}

	req, err := svc.buildUpstreamRequest(c.Request.Context(), c, account, body, "token", true, "", false)
	require.NoError(t, err)
	wantSessionID := isolateOpenAICodexOAuthSessionID(0, "release-session", "session")
	wantThreadID := isolateOpenAICodexOAuthSessionID(0, "release-thread", "thread")
	require.Equal(t, wantSessionID, req.Header.Get(openAICodexSessionIDHeader))
	require.Equal(t, wantThreadID, req.Header.Get(openAICodexThreadIDHeader))
	require.Equal(t, wantThreadID, req.Header.Get(openAICodexClientRequestIDHeader))
	require.Equal(t, wantThreadID+":7", req.Header.Get(openAICodexWindowIDHeader))
	require.Equal(t, "feature-a,feature-b", req.Header.Get(openAICodexBetaFeaturesHeader))
	require.Equal(t, "release-turn-state", req.Header.Get(openAICodexTurnStateHeader))
	require.Equal(t, "release-turn-metadata", req.Header.Get(openAICodexTurnMetadataHeader))
}

func TestOpenAIBuildUpstreamRequestOAuthMessagesBridgeUsesFingerprintIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.5","prompt_cache_key":"anthropic-metadata-session-1","client_metadata":{"keep":"yes","x-codex-installation-id":"attacker-installation","x-codex-window-id":"attacker-window","thread-id":"attacker-thread"},"input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"<sub2api-claude-code-todo-guard>"}]},{"type":"message","role":"user","content":"hello"}]}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("OpenAI-Beta", "responses=experimental")
	c.Request.Header.Set("originator", "attacker-originator")
	c.Request.Header.Set(openAICodexInstallationIDHeader, "11111111-1111-4111-8111-111111111111")

	persisted := OpenAICodexFingerprint{
		SchemaVersion:  openAICodexFingerprintSchemaV1,
		InstallationID: "550e8400-e29b-41d4-a716-446655440000",
		UAProfile:      ParseOpenAICodexUAProfile("persisted-codex/9.9.9 (Persist OS; arch) Persist_Term/1.0 (persisted-codex; 9.9.9)"),
		CreatedAt:      "2026-06-12T00:00:00Z",
		UpdatedAt:      "2026-06-12T00:00:00Z",
	}
	svc := &OpenAIGatewayService{}
	account := &Account{
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "chatgpt-acc"},
		Extra:       map[string]any{OpenAICodexFingerprintExtraKey: persisted},
	}

	req, err := svc.buildUpstreamRequest(c.Request.Context(), c, account, body, "token", true, "anthropic-metadata-session-1", false)
	require.NoError(t, err)
	require.NotEmpty(t, req.Header.Get("Session_Id"))
	require.Empty(t, req.Header.Get("Conversation_Id"))
	require.Empty(t, req.Header.Get("OpenAI-Beta"))
	require.Equal(t, "persisted-codex", req.Header.Get("originator"))
	require.Equal(t, "9.9.9", req.Header.Get("Version"))
	require.Equal(t, persisted.InstallationID, req.Header.Get(openAICodexInstallationIDHeader))
	bodyBytes, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Equal(t, "yes", gjson.GetBytes(bodyBytes, "client_metadata.keep").String())
	require.Equal(t, persisted.InstallationID, gjson.GetBytes(bodyBytes, "client_metadata."+openAICodexInstallationIDHeader).String())
	require.False(t, gjson.GetBytes(bodyBytes, "client_metadata."+openAICodexWindowIDHeader).Exists())
	require.False(t, gjson.GetBytes(bodyBytes, "client_metadata."+openAICodexThreadIDHeader).Exists())
	require.NotContains(t, string(bodyBytes), "attacker-installation")
	require.NotContains(t, string(bodyBytes), "attacker-window")
	require.NotContains(t, string(bodyBytes), "attacker-thread")
}

func TestOpenAIBuildUpstreamRequestOAuthUsesRequestContextFingerprintWhenExtraRedacted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := []byte(`{"model":"gpt-5.5","client_metadata":{"keep":"yes"},"input":"hello"}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))

	svc := &OpenAIGatewayService{}
	account := &Account{
		ID:          43,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "chatgpt-acc"},
		Extra:       map[string]any{OpenAICodexFingerprintExtraKey: map[string]any{"present": true}},
	}

	req, err := svc.buildUpstreamRequest(c.Request.Context(), c, account, body, "token", true, "", false)
	require.NoError(t, err)
	installationID := req.Header.Get(openAICodexInstallationIDHeader)
	require.NotEmpty(t, installationID)
	_, ok := canonicalOpenAICodexInstallationID(installationID)
	require.True(t, ok)

	bodyBytes, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Equal(t, "yes", gjson.GetBytes(bodyBytes, "client_metadata.keep").String())
	require.Equal(t, installationID, gjson.GetBytes(bodyBytes, "client_metadata."+openAICodexInstallationIDHeader).String())
	require.Equal(t, map[string]any{"present": true}, account.Extra[OpenAICodexFingerprintExtraKey])
}

func TestOpenAIBuildUpstreamRequestPreservesCompactPathForAPIKeyBaseURL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/responses/compact", bytes.NewReader([]byte(`{"model":"gpt-5"}`)))

	svc := &OpenAIGatewayService{cfg: &config.Config{
		Security: config.SecurityConfig{
			URLAllowlist: config.URLAllowlistConfig{Enabled: false},
		},
	}}
	account := &Account{
		Type:        AccountTypeAPIKey,
		Platform:    PlatformOpenAI,
		Credentials: map[string]any{"base_url": "https://example.com/v1"},
	}

	req, err := svc.buildUpstreamRequest(c.Request.Context(), c, account, []byte(`{"model":"gpt-5"}`), "token", false, "", false)
	require.NoError(t, err)
	require.Equal(t, "https://example.com/v1/responses/compact", req.URL.String())
}

func TestOpenAIBuildUpstreamRequestOAuthOfficialClientOriginatorCompatibility(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name           string
		userAgent      string
		originator     string
		wantOriginator string
	}{
		{name: "tui originator comes from fingerprint", userAgent: "codex-tui/1.2.3", originator: codexOfficialOriginator, wantOriginator: codexOfficialOriginator},
		{name: "vscode originator ignored for OAuth fingerprint", userAgent: "codex_vscode/1.2.3", originator: "codex_vscode", wantOriginator: codexOfficialOriginator},
		{name: "official ua fallback to fingerprint originator", userAgent: "codex-tui/1.2.3", wantOriginator: codexOfficialOriginator},
		{name: "non official originator normalized", userAgent: "opencode/0.9", originator: "opencode", wantOriginator: codexOfficialOriginator},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader([]byte(`{"model":"gpt-5"}`)))
			if tt.userAgent != "" {
				c.Request.Header.Set("User-Agent", tt.userAgent)
			}
			if tt.originator != "" {
				c.Request.Header.Set("originator", tt.originator)
			}

			svc := &OpenAIGatewayService{}
			account := &Account{
				Platform:    PlatformOpenAI,
				Type:        AccountTypeOAuth,
				Credentials: map[string]any{"chatgpt_account_id": "chatgpt-acc"},
			}

			isCodexCLI := openai.IsCodexOfficialClientByHeaders(c.GetHeader("User-Agent"), c.GetHeader("originator"))
			req, err := svc.buildUpstreamRequest(c.Request.Context(), c, account, []byte(`{"model":"gpt-5"}`), "token", false, "", isCodexCLI)
			require.NoError(t, err)
			require.Equal(t, tt.wantOriginator, req.Header.Get("originator"))
		})
	}
}

// ==================== P1-08 修复：model 替换性能优化测试 ====================

// ==================== P1-08 修复：model 替换性能优化测试 =============
func TestReplaceModelInSSELine(t *testing.T) {
	svc := &OpenAIGatewayService{}

	tests := []struct {
		name     string
		line     string
		from     string
		to       string
		expected string
	}{
		{
			name:     "顶层 model 字段替换",
			line:     `data: {"id":"chatcmpl-123","model":"gpt-4o","choices":[]}`,
			from:     "gpt-4o",
			to:       "my-custom-model",
			expected: `data: {"id":"chatcmpl-123","model":"my-custom-model","choices":[]}`,
		},
		{
			name:     "嵌套 response.model 替换",
			line:     `data: {"type":"response","response":{"id":"resp-1","model":"gpt-4o","output":[]}}`,
			from:     "gpt-4o",
			to:       "my-model",
			expected: `data: {"type":"response","response":{"id":"resp-1","model":"my-model","output":[]}}`,
		},
		{
			name:     "model 不匹配时不替换",
			line:     `data: {"id":"chatcmpl-123","model":"gpt-3.5-turbo","choices":[]}`,
			from:     "gpt-4o",
			to:       "my-model",
			expected: `data: {"id":"chatcmpl-123","model":"gpt-3.5-turbo","choices":[]}`,
		},
		{
			name:     "无 model 字段时不替换",
			line:     `data: {"id":"chatcmpl-123","choices":[]}`,
			from:     "gpt-4o",
			to:       "my-model",
			expected: `data: {"id":"chatcmpl-123","choices":[]}`,
		},
		{
			name:     "空 data 行",
			line:     `data: `,
			from:     "gpt-4o",
			to:       "my-model",
			expected: `data: `,
		},
		{
			name:     "[DONE] 行",
			line:     `data: [DONE]`,
			from:     "gpt-4o",
			to:       "my-model",
			expected: `data: [DONE]`,
		},
		{
			name:     "非 data: 前缀行",
			line:     `event: message`,
			from:     "gpt-4o",
			to:       "my-model",
			expected: `event: message`,
		},
		{
			name:     "非法 JSON 不替换",
			line:     `data: {invalid json}`,
			from:     "gpt-4o",
			to:       "my-model",
			expected: `data: {invalid json}`,
		},
		{
			name:     "无空格 data: 格式",
			line:     `data:{"id":"x","model":"gpt-4o"}`,
			from:     "gpt-4o",
			to:       "my-model",
			expected: `data: {"id":"x","model":"my-model"}`,
		},
		{
			name:     "model 名含特殊字符",
			line:     `data: {"model":"org/model-v2.1-beta"}`,
			from:     "org/model-v2.1-beta",
			to:       "custom/alias",
			expected: `data: {"model":"custom/alias"}`,
		},
		{
			name:     "空行",
			line:     "",
			from:     "gpt-4o",
			to:       "my-model",
			expected: "",
		},
		{
			name:     "保持其他字段不变",
			line:     `data: {"id":"abc","object":"chat.completion.chunk","model":"gpt-4o","created":1234567890,"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
			from:     "gpt-4o",
			to:       "alias",
			expected: `data: {"id":"abc","object":"chat.completion.chunk","model":"alias","created":1234567890,"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		},
		{
			name:     "顶层优先于嵌套：同时存在两个 model",
			line:     `data: {"model":"gpt-4o","response":{"model":"gpt-4o"}}`,
			from:     "gpt-4o",
			to:       "replaced",
			expected: `data: {"model":"replaced","response":{"model":"gpt-4o"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := svc.replaceModelInSSELine(tt.line, tt.from, tt.to)
			require.Equal(t, tt.expected, got)
		})
	}
}

func TestReplaceModelInSSEBody(t *testing.T) {
	svc := &OpenAIGatewayService{}

	tests := []struct {
		name     string
		body     string
		from     string
		to       string
		expected string
	}{
		{
			name:     "多行 SSE body 替换",
			body:     "data: {\"model\":\"gpt-4o\",\"choices\":[]}\n\ndata: {\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n",
			from:     "gpt-4o",
			to:       "alias",
			expected: "data: {\"model\":\"alias\",\"choices\":[]}\n\ndata: {\"model\":\"alias\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n",
		},
		{
			name:     "无需替换的 body",
			body:     "data: {\"model\":\"gpt-3.5-turbo\"}\n\ndata: [DONE]\n",
			from:     "gpt-4o",
			to:       "alias",
			expected: "data: {\"model\":\"gpt-3.5-turbo\"}\n\ndata: [DONE]\n",
		},
		{
			name:     "混合 event 和 data 行",
			body:     "event: message\ndata: {\"model\":\"gpt-4o\"}\n\n",
			from:     "gpt-4o",
			to:       "alias",
			expected: "event: message\ndata: {\"model\":\"alias\"}\n\n",
		},
		{
			name:     "空 body",
			body:     "",
			from:     "gpt-4o",
			to:       "alias",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := svc.replaceModelInSSEBody(tt.body, tt.from, tt.to)
			require.Equal(t, tt.expected, got)
		})
	}
}

func TestReplaceModelInResponseBody(t *testing.T) {
	svc := &OpenAIGatewayService{}

	tests := []struct {
		name     string
		body     string
		from     string
		to       string
		expected string
	}{
		{
			name:     "替换顶层 model",
			body:     `{"id":"chatcmpl-123","model":"gpt-4o","choices":[]}`,
			from:     "gpt-4o",
			to:       "alias",
			expected: `{"id":"chatcmpl-123","model":"alias","choices":[]}`,
		},
		{
			name:     "model 不匹配不替换",
			body:     `{"id":"chatcmpl-123","model":"gpt-3.5-turbo","choices":[]}`,
			from:     "gpt-4o",
			to:       "alias",
			expected: `{"id":"chatcmpl-123","model":"gpt-3.5-turbo","choices":[]}`,
		},
		{
			name:     "无 model 字段不替换",
			body:     `{"id":"chatcmpl-123","choices":[]}`,
			from:     "gpt-4o",
			to:       "alias",
			expected: `{"id":"chatcmpl-123","choices":[]}`,
		},
		{
			name:     "非法 JSON 返回原值",
			body:     `not json`,
			from:     "gpt-4o",
			to:       "alias",
			expected: `not json`,
		},
		{
			name:     "空 body 返回原值",
			body:     ``,
			from:     "gpt-4o",
			to:       "alias",
			expected: ``,
		},
		{
			name:     "保持嵌套结构不变",
			body:     `{"model":"gpt-4o","usage":{"prompt_tokens":10,"completion_tokens":20},"choices":[{"message":{"role":"assistant","content":"hello"}}]}`,
			from:     "gpt-4o",
			to:       "alias",
			expected: `{"model":"alias","usage":{"prompt_tokens":10,"completion_tokens":20},"choices":[{"message":{"role":"assistant","content":"hello"}}]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := svc.replaceModelInResponseBody([]byte(tt.body), tt.from, tt.to)
			require.Equal(t, tt.expected, string(got))
		})
	}
}

func TestExtractOpenAISSEDataLine(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		wantData string
		wantOK   bool
	}{
		{name: "标准格式", line: `data: {"type":"x"}`, wantData: `{"type":"x"}`, wantOK: true},
		{name: "无空格格式", line: `data:{"type":"x"}`, wantData: `{"type":"x"}`, wantOK: true},
		{name: "纯空数据", line: `data:   `, wantData: ``, wantOK: true},
		{name: "非 data 行", line: `event: message`, wantData: ``, wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := extractOpenAISSEDataLine(tt.line)
			require.Equal(t, tt.wantOK, ok)
			require.Equal(t, tt.wantData, got)
		})
	}
}

func TestParseSSEUsage_SelectiveParsing(t *testing.T) {
	svc := &OpenAIGatewayService{}
	usage := &OpenAIUsage{InputTokens: 9, OutputTokens: 8, CacheReadInputTokens: 7}

	// 非 completed 事件，不应覆盖 usage
	svc.parseSSEUsage(`{"type":"response.in_progress","response":{"usage":{"input_tokens":1,"output_tokens":2}}}`, usage)
	require.Equal(t, 9, usage.InputTokens)
	require.Equal(t, 8, usage.OutputTokens)
	require.Equal(t, 7, usage.CacheReadInputTokens)

	// completed 事件，应提取 usage
	svc.parseSSEUsage(`{"type":"response.completed","response":{"usage":{"input_tokens":3,"output_tokens":5,"input_tokens_details":{"cached_tokens":2}}}}`, usage)
	require.Equal(t, 3, usage.InputTokens)
	require.Equal(t, 5, usage.OutputTokens)
	require.Equal(t, 2, usage.CacheReadInputTokens)

	// done 事件同样可能携带最终 usage
	svc.parseSSEUsage(`{"type":"response.done","response":{"usage":{"input_tokens":13,"output_tokens":15,"input_tokens_details":{"cached_tokens":4}}}}`, usage)
	require.Equal(t, 13, usage.InputTokens)
	require.Equal(t, 15, usage.OutputTokens)
	require.Equal(t, 4, usage.CacheReadInputTokens)

	// failed 事件在部分上游路径也会携带已消耗 usage，应与 WS/passthrough 保持一致
	svc.parseSSEUsage(`{"type":"response.failed","response":{"usage":{"input_tokens":17,"output_tokens":19,"input_tokens_details":{"cached_tokens":6}}}}`, usage)
	require.Equal(t, 17, usage.InputTokens)
	require.Equal(t, 19, usage.OutputTokens)
	require.Equal(t, 6, usage.CacheReadInputTokens)

	svc.parseSSEUsage(`{"type":"response.completed","response":{"usage":{"prompt_tokens":21,"completion_tokens":8,"prompt_tokens_details":{"cached_tokens":6}}}}`, usage)
	require.Equal(t, 21, usage.InputTokens)
	require.Equal(t, 8, usage.OutputTokens)
	require.Equal(t, 6, usage.CacheReadInputTokens)
}

func TestExtractOpenAIUsageFromJSONBytes_AcceptsResponseAndChatUsageShapes(t *testing.T) {
	usage, ok := extractOpenAIUsageFromJSONBytes([]byte(`{"id":"resp_1","usage":{"input_tokens":3,"output_tokens":5,"input_tokens_details":{"cached_tokens":2,"cache_write_tokens":7,"image_tokens":1}}}`))
	require.True(t, ok)
	require.Equal(t, 3, usage.InputTokens)
	require.Equal(t, 1, usage.ImageInputTokens)
	require.Equal(t, 5, usage.OutputTokens)
	require.Equal(t, 7, usage.CacheCreationInputTokens)
	require.Equal(t, 2, usage.CacheReadInputTokens)

	usage, ok = extractOpenAIUsageFromJSONBytes([]byte(`{"type":"response.completed","response":{"usage":{"prompt_tokens":13,"completion_tokens":7,"cache_creation_input_tokens":9,"prompt_tokens_details":{"cached_tokens":4,"cache_creation_tokens":0,"image_tokens":3}}}}`))
	require.True(t, ok)
	require.Equal(t, 13, usage.InputTokens)
	require.Equal(t, 3, usage.ImageInputTokens)
	require.Equal(t, 7, usage.OutputTokens)
	require.Zero(t, usage.CacheCreationInputTokens)
	require.Equal(t, 4, usage.CacheReadInputTokens)

	usage, ok = extractOpenAIUsageFromJSONBytes([]byte(`{"usage":{"input_tokens":11,"output_tokens":2,"cache_creation_input_tokens":0,"cache_write_input_tokens":8,"input_tokens_details":{"cache_write_tokens":0,"cache_creation_tokens":6}}}`))
	require.True(t, ok)
	require.Zero(t, usage.CacheCreationInputTokens)

	usage, ok = extractOpenAIUsageFromJSONBytes([]byte(`{"usage":{"prompt_tokens":11,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":3,"cache_write_tokens":6}}}`))
	require.True(t, ok)
	require.Equal(t, 6, usage.CacheCreationInputTokens)
	require.Equal(t, 3, usage.CacheReadInputTokens)
}

func TestExtractOpenAIResponseIDFromJSONBytes(t *testing.T) {
	require.Equal(t, "resp_json", extractOpenAIResponseIDFromJSONBytes([]byte(`{"id":"resp_json"}`)))
	require.Equal(t, "resp_sse", extractOpenAIResponseIDFromJSONBytes([]byte(`{"type":"response.completed","response":{"id":"resp_sse"}}`)))
	require.Empty(t, extractOpenAIResponseIDFromJSONBytes([]byte(`{"response":{}}`)))
	require.Empty(t, extractOpenAIResponseIDFromJSONBytes([]byte(`not-json`)))
}

func TestExtractCodexFinalResponse_SampleReplay(t *testing.T) {
	body := strings.Join([]string{
		`event: message`,
		`data: {"type":"response.in_progress","response":{"id":"resp_1"}}`,
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"gpt-4o","usage":{"input_tokens":11,"output_tokens":22,"input_tokens_details":{"cached_tokens":3}}}}`,
		`data: [DONE]`,
	}, "\n")

	finalResp, ok := extractCodexFinalResponse(body)
	require.True(t, ok)
	require.Contains(t, string(finalResp), `"id":"resp_1"`)
	require.Contains(t, string(finalResp), `"input_tokens":11`)
}

func TestOpenAINonStreamingRemoteCompactionRecordsSuccessfulOutcome(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name        string
		passthrough bool
		contentType string
		body        string
		want        OpenAIRemoteCompactionSemanticOutcome
	}{
		{
			name:        "adapter JSON",
			contentType: "application/json",
			body:        `{"id":"resp_adapter_json","object":"response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`,
			want:        OpenAIRemoteCompactionSemanticOutcomeSucceeded,
		},
		{
			name:        "adapter JSON incomplete",
			contentType: "application/json",
			body:        `{"id":"resp_adapter_incomplete","object":"response","status":"incomplete","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`,
			want:        OpenAIRemoteCompactionSemanticOutcomeFailed,
		},
		{
			name:        "adapter SSE converted to JSON",
			contentType: "text/event-stream",
			body:        `data: {"type":"response.completed","response":{"id":"resp_adapter_sse","object":"response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`,
			want:        OpenAIRemoteCompactionSemanticOutcomeSucceeded,
		},
		{
			name:        "passthrough JSON",
			passthrough: true,
			contentType: "application/json",
			body:        `{"id":"resp_passthrough_json","object":"response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`,
			want:        OpenAIRemoteCompactionSemanticOutcomeSucceeded,
		},
		{
			name:        "passthrough JSON failed",
			passthrough: true,
			contentType: "application/json",
			body:        `{"id":"resp_passthrough_failed","object":"response","status":"failed","error":{"message":"compaction failed"},"output":[],"usage":{"input_tokens":1,"output_tokens":0}}`,
			want:        OpenAIRemoteCompactionSemanticOutcomeFailed,
		},
		{
			name:        "passthrough SSE converted to JSON",
			passthrough: true,
			contentType: "text/event-stream",
			body:        `data: {"type":"response.done","response":{"id":"resp_passthrough_sse","object":"response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`,
			want:        OpenAIRemoteCompactionSemanticOutcomeSucceeded,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			c.Request.Header.Set("User-Agent", "codex_cli_rs/0.136.0")
			require.True(t, PromoteOpenAICompactBodySignal(c, []byte(`{"input":[{"type":"compaction_trigger"}]}`), false))

			svc := &OpenAIGatewayService{cfg: &config.Config{}}
			resp := &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{tt.contentType}},
				Body:       io.NopCloser(strings.NewReader(tt.body)),
			}
			if tt.passthrough {
				_, err := svc.handleNonStreamingResponsePassthrough(c.Request.Context(), resp, c, "gpt-5.5", "gpt-5.5")
				require.NoError(t, err)
			} else {
				_, err := svc.handleNonStreamingResponse(c.Request.Context(), resp, c, &Account{Type: AccountTypeOAuth}, "gpt-5.5", "gpt-5.5")
				require.NoError(t, err)
			}

			outcome, exists := GetOpenAIRemoteCompactionSemanticOutcome(c)
			require.True(t, exists)
			require.Equal(t, tt.want, outcome)
		})
	}
}

func TestHandleSSEToJSON_CompletedEventReturnsJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}
	body := []byte(strings.Join([]string{
		`data: {"type":"response.in_progress","response":{"id":"resp_2"}}`,
		`data: {"type":"response.completed","response":{"id":"resp_2","model":"gpt-4o","usage":{"input_tokens":7,"output_tokens":9,"input_tokens_details":{"cached_tokens":1}}}}`,
		`data: [DONE]`,
	}, "\n"))

	usage, err := svc.handleSSEToJSON(resp, c, body, "gpt-4o", "gpt-4o")
	require.NoError(t, err)
	require.NotNil(t, usage)
	require.Equal(t, 7, usage.InputTokens)
	require.Equal(t, 9, usage.OutputTokens)
	require.Equal(t, 1, usage.CacheReadInputTokens)
	require.Equal(t, "resp_2", usage.responseID)
	// Header 可能由上游 Content-Type 透传；关键是 body 已转换为最终 JSON 响应。
	require.NotContains(t, rec.Body.String(), "event:")
	require.Contains(t, rec.Body.String(), `"id":"resp_2"`)
	require.NotContains(t, rec.Body.String(), "data:")
}

func TestHandleNonStreamingResponse_APIKeyFallsBackToSSEBodyWhenContentTypeIsWrong(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"response.output_text.delta","delta":"hel"}`,
			`data: {"type":"response.output_text.delta","delta":"lo"}`,
			`data: {"type":"response.completed","response":{"id":"resp_api_key_sse","object":"response","model":"gpt-5.4","status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`,
			`data: [DONE]`,
		}, "\n"))),
	}
	account := &Account{ID: 1, Type: AccountTypeAPIKey}

	result, err := svc.handleNonStreamingResponse(context.Background(), resp, c, account, "gpt-5.4", "gpt-5.4")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 3, result.InputTokens)
	require.Equal(t, 2, result.OutputTokens)
	require.Equal(t, "resp_api_key_sse", result.responseID)
	require.NotContains(t, rec.Body.String(), "data:")
	require.Equal(t, "resp_api_key_sse", gjson.Get(rec.Body.String(), "id").String())
	require.Equal(t, "hello", gjson.Get(rec.Body.String(), "output.0.content.0.text").String())
}

func TestHandleNonStreamingResponse_OAuthJSONBodyWithDataEventTextKeepsJSONUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", nil)

	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"id":"resp_oauth_compact","object":"response","model":"gpt-5.4","status":"completed",` +
			`"output":[{"type":"message","content":[{"type":"output_text","text":"processing data: 1,2,3 then event: click finished"}]}],` +
			`"usage":{"input_tokens":11,"output_tokens":22,"total_tokens":33}}`)),
	}
	account := &Account{ID: 146, Type: AccountTypeOAuth}

	result, err := svc.handleNonStreamingResponse(context.Background(), resp, c, account, "gpt-5.4", "gpt-5.4")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 11, result.InputTokens)
	require.Equal(t, 22, result.OutputTokens)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	require.Equal(t, "resp_oauth_compact", gjson.Get(rec.Body.String(), "id").String())
	require.Equal(t, int64(33), gjson.Get(rec.Body.String(), "usage.total_tokens").Int())
	require.Contains(t, rec.Body.String(), "processing data: 1,2,3 then event: click finished")
}

func TestBodyHasSSEFramingMatchesOnlyPhysicalLineStarts(t *testing.T) {
	tests := []struct {
		name string
		body []byte
		want bool
	}{
		{name: "lf data line", body: []byte("data: {}\n\n"), want: true},
		{name: "crlf event line", body: []byte("event: response.completed\r\ndata: {}\r\n\r\n"), want: true},
		{name: "final data line without newline", body: []byte("data: {}"), want: true},
		{name: "json text contains markers", body: []byte(`{"text":"processing data: 1 then event: done"}`), want: false},
		{name: "indented marker is not framing", body: []byte(" data: {}\n"), want: false},
		{name: "marker after json prefix is not framing", body: []byte(`{"text":` + "\n" + `"data: not framing"}`), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, bodyHasSSEFraming(tt.body))
		})
	}
}

func TestHandleSSEToJSON_ReconstructsImageGenerationOutputItemDone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}
	body := []byte(strings.Join([]string{
		`data: {"type":"response.output_item.done","item":{"id":"ig_123","type":"image_generation_call","status":"generating","result":"aGVsbG8=","revised_prompt":"draw a cat","output_format":"png","opaque":{"keep":true}}}`,
		`data: {"type":"response.completed","response":{"id":"resp_img","model":"gpt-5.4","output":[],"usage":{"input_tokens":7,"output_tokens":9,"output_tokens_details":{"image_tokens":4}}}}`,
		`data: [DONE]`,
	}, "\n"))

	usage, err := svc.handleSSEToJSON(resp, c, body, "gpt-5.4", "gpt-5.4")
	require.NoError(t, err)
	require.NotNil(t, usage)
	require.Equal(t, 4, usage.ImageOutputTokens)
	require.NotContains(t, rec.Body.String(), "data:")
	require.Equal(t, "image_generation_call", gjson.Get(rec.Body.String(), "output.0.type").String())
	require.Equal(t, "aGVsbG8=", gjson.Get(rec.Body.String(), "output.0.result").String())
	require.Equal(t, "draw a cat", gjson.Get(rec.Body.String(), "output.0.revised_prompt").String())
	require.Equal(t, "completed", gjson.Get(rec.Body.String(), "output.0.status").String())
	require.True(t, gjson.Get(rec.Body.String(), "output.0.opaque.keep").Bool())
}

func TestHandleSSEToJSON_ReconstructsEventLineOnlyImageGenerationOutput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}}
	body := []byte(strings.Join([]string{
		`event: response.output_item.done`,
		`data: {"item":{"id":"ig_event_only","type":"image_generation_call","status":"generating","result":"aGVsbG8="}}`,
		``,
		`event: response.completed`,
		`data: {"response":{"id":"resp_event_only","output":[],"usage":{"input_tokens":3,"output_tokens":4}}}`,
		``,
	}, "\n"))

	result, err := svc.handleSSEToJSON(resp, c, body, "gpt-5.4", "gpt-5.4")
	require.NoError(t, err)
	require.Equal(t, 3, result.usage.InputTokens)
	require.Equal(t, 4, result.usage.OutputTokens)
	require.Equal(t, "image_generation_call", gjson.Get(rec.Body.String(), "output.0.type").String())
	require.Equal(t, "completed", gjson.Get(rec.Body.String(), "output.0.status").String())
	require.Equal(t, "aGVsbG8=", gjson.Get(rec.Body.String(), "output.0.result").String())
}

func TestHandleSSEToJSON_NormalizesCompletedImageStatuses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}}
	body := []byte(`data: {"type":"response.completed","response":{"id":"resp_img","output":[{"type":"image_generation_call","status":"generating","result":"one","opaque":1},{"type":"image_generation_call","status":"in_progress","result":"two","opaque":2}],"usage":{"input_tokens":5,"output_tokens":6}}}`)

	result, err := svc.handleSSEToJSON(resp, c, body, "gpt-5.4", "gpt-5.4")
	require.NoError(t, err)
	require.Equal(t, 5, result.usage.InputTokens)
	require.Equal(t, 6, result.usage.OutputTokens)
	require.Equal(t, "completed", gjson.Get(rec.Body.String(), "output.0.status").String())
	require.Equal(t, "completed", gjson.Get(rec.Body.String(), "output.1.status").String())
	require.Equal(t, int64(2), gjson.Get(rec.Body.String(), "output.1.opaque").Int())
}

func TestHandlePassthroughSSEToJSON_NormalizesCompletedImageStatuses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}}
	body := []byte(`data: {"type":"response.done","response":{"id":"resp_img","output":[{"type":"image_generation_call","status":"in_progress","result":"one","opaque":"kept"}],"usage":{"input_tokens":7,"output_tokens":8}}}`)

	result, err := svc.handlePassthroughSSEToJSON(resp, c, body, "", "")
	require.NoError(t, err)
	require.Equal(t, 7, result.usage.InputTokens)
	require.Equal(t, 8, result.usage.OutputTokens)
	require.Equal(t, "completed", gjson.Get(rec.Body.String(), "output.0.status").String())
	require.Equal(t, "kept", gjson.Get(rec.Body.String(), "output.0.opaque").String())
}

func TestNormalizeCompletedImageGenerationStatusPreservesOpaqueRawBytes(t *testing.T) {
	input := []byte("{\n  \"type\" : \"response.completed\", \"response\" : { \"status\":\"completed\", \"output\" : [" +
		`{ "type":"image_generation_call", "status" : "generating", "result":"one", "slash":"a\\/b", "unicode":"\\u0061", "number":1.2300e+04, "dup":1, "dup":2, "nested" : { "raw" : [ 1,  2 ] } },` +
		` {"type":"message","status":"in_progress","opaque" : { "x" : "\\u263a" }},` +
		` {"type":"image_generation_call","status":"in_progress","result":"two","raw":[ true ,null, {"k" : "v"} ]}` +
		"] , \"tail\" : { \"keep\" : -0.00E-2 } } }")
	want := bytes.Replace(input, []byte(`"status" : "generating"`), []byte(`"status" : "completed"`), 1)
	want = bytes.Replace(want, []byte(`"status":"in_progress","result":"two"`), []byte(`"status":"completed","result":"two"`), 1)

	got, changed := normalizeCompletedImageGenerationStatus(input)
	require.True(t, changed)
	require.Equal(t, string(want), string(got))
}

func TestNormalizeCompletedImageGenerationStatusLargeOutput(t *testing.T) {
	const itemCount = 5000
	var input strings.Builder
	input.WriteString(`{"type":"response.done","response":{"output":[`)
	for i := 0; i < itemCount; i++ {
		if i > 0 {
			input.WriteByte(',')
		}
		fmt.Fprintf(&input, ` {"type":"image_generation_call","status":"in_progress","result":"%d","opaque":1.00e+02}`, i)
	}
	input.WriteString(`]}}`)

	got, changed := normalizeCompletedImageGenerationStatus([]byte(input.String()))
	require.True(t, changed)
	require.Equal(t, itemCount, bytes.Count(got, []byte(`"status":"completed"`)))
	require.Equal(t, itemCount, bytes.Count(got, []byte(`"opaque":1.00e+02`)))
}

func BenchmarkNormalizeCompletedImageGenerationStatusLargeOutput(b *testing.B) {
	for _, itemCount := range []int{100, 1000, 5000} {
		b.Run(fmt.Sprintf("items_%d", itemCount), func(b *testing.B) {
			var input strings.Builder
			input.WriteString(`{"type":"response.completed","response":{"output":[`)
			for i := 0; i < itemCount; i++ {
				if i > 0 {
					input.WriteByte(',')
				}
				fmt.Fprintf(&input, `{"type":"image_generation_call","status":"generating","result":"%d","opaque":{"n":1.00e+02}}`, i)
			}
			input.WriteString(`]}}`)
			payload := []byte(input.String())
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_, changed := normalizeCompletedImageGenerationStatus(payload)
				if !changed {
					b.Fatal("expected normalization")
				}
			}
		})
	}
}

func TestHandleSSEToJSON_NoFinalResponseReturnsProtocolError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}
	body := []byte(strings.Join([]string{
		`data: {"type":"response.in_progress","response":{"id":"resp_3"}}`,
		`data: [DONE]`,
	}, "\n"))

	usage, err := svc.handleSSEToJSON(resp, c, body, "gpt-4o", "gpt-4o")
	require.Error(t, err)
	require.Nil(t, usage)
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Equal(t, "upstream_error", gjson.Get(rec.Body.String(), "error.type").String())
	require.Equal(t, "Upstream returned invalid usage", gjson.Get(rec.Body.String(), "error.message").String())
	require.NotContains(t, rec.Body.String(), "data:")
}

func TestHandleSSEToJSON_ResponseFailedReturnsProtocolError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
	}
	body := []byte(strings.Join([]string{
		`data: {"type":"response.failed","error":{"message":"upstream rejected request"}}`,
		`data: [DONE]`,
	}, "\n"))

	usage, err := svc.handleSSEToJSON(resp, c, body, "gpt-4o", "gpt-4o")
	require.Nil(t, usage)
	require.Error(t, err)
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Contains(t, rec.Body.String(), "upstream rejected request")
	require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
}

func TestOpenAICompatSSEFrameParserResetsEventTypeAtFrameBoundary(t *testing.T) {
	var parser openAICompatSSEFrameParser

	frame, ok := parser.AddLine("event: response.created")
	require.False(t, ok)
	require.Empty(t, frame)

	frame, ok = parser.AddLine(`data: {"response":{"id":"resp_1"}}`)
	require.False(t, ok)
	require.Empty(t, frame)

	frame, ok = parser.AddLine("")
	require.True(t, ok)
	require.Equal(t, "response.created", frame.EventType)
	require.JSONEq(t, `{"response":{"id":"resp_1"}}`, frame.Data)

	frame, ok = parser.AddLine(`data: {"delta":"ok"}`)
	require.False(t, ok)
	require.Empty(t, frame.EventType)

	frame, ok = parser.AddLine("")
	require.True(t, ok)
	require.Empty(t, frame.EventType)
	require.JSONEq(t, `{"delta":"ok"}`, frame.Data)
}
