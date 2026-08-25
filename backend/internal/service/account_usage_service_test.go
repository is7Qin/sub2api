package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	httppool "github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/imroc/req/v3"
	"github.com/tidwall/gjson"
)

type rewriteChatGPTTestRoundTripper struct {
	target *url.URL
	base   http.RoundTripper
}

func (rt rewriteChatGPTTestRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req != nil && req.URL != nil && req.URL.Host == "chatgpt.com" && rt.target != nil {
		clone := req.Clone(req.Context())
		clone.URL.Scheme = rt.target.Scheme
		clone.URL.Host = rt.target.Host
		clone.Host = req.Host
		req = clone
	}
	base := rt.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

type accountUsageCodexProbeRepo struct {
	stubOpenAIAccountRepo
	updateExtraCh      chan map[string]any
	updateExtraErr     error
	updateExtraErrFor  func(map[string]any) error
	rateLimitCh        chan time.Time
	bulkUpdateCh       chan AccountBulkUpdate
	sessionWindowEndCh chan struct {
		id  int64
		end time.Time
	}
	getByID func(context.Context, int64) (*Account, error)
}

func (r *accountUsageCodexProbeRepo) GetByID(ctx context.Context, id int64) (*Account, error) {
	if r.getByID != nil {
		return r.getByID(ctx, id)
	}
	return r.stubOpenAIAccountRepo.GetByID(ctx, id)
}

func (r *accountUsageCodexProbeRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	if r.updateExtraCh != nil {
		copied := make(map[string]any, len(updates))
		for k, v := range updates {
			copied[k] = v
		}
		r.updateExtraCh <- copied
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

func (r *accountUsageCodexProbeRepo) SetRateLimited(_ context.Context, _ int64, resetAt time.Time) error {
	if r.rateLimitCh != nil {
		r.rateLimitCh <- resetAt
	}
	return nil
}

func (r *accountUsageCodexProbeRepo) BulkUpdate(_ context.Context, _ []int64, updates AccountBulkUpdate) (int64, error) {
	if r.bulkUpdateCh != nil {
		r.bulkUpdateCh <- updates
	}
	return 1, nil
}

func (r *accountUsageCodexProbeRepo) UpdateSessionWindowEnd(_ context.Context, id int64, end time.Time) error {
	if r.sessionWindowEndCh != nil {
		r.sessionWindowEndCh <- struct {
			id  int64
			end time.Time
		}{id: id, end: end}
	}
	return nil
}

func TestAccountUsageService_GetOpenAIUsageRefreshesPlanType(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/accounts/check/v4-2023-04-27" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer access-token" {
			t.Fatalf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accounts":{"org-123":{"account":{"plan_type":"plus","is_default":true},"entitlement":{"expires_at":"2026-06-30T00:00:00Z"}}}}`))
	}))
	defer server.Close()
	originalURL := chatGPTAccountsCheckURL
	chatGPTAccountsCheckURL = server.URL + "/backend-api/accounts/check/v4-2023-04-27"
	t.Cleanup(func() { chatGPTAccountsCheckURL = originalURL })

	account := &Account{
		ID:       42,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":    "access-token",
			"organization_id": "org-123",
			"plan_type":       "free",
		},
	}
	repo := &accountUsageCodexProbeRepo{
		bulkUpdateCh: make(chan AccountBulkUpdate, 1),
		getByID: func(_ context.Context, id int64) (*Account, error) {
			if id == account.ID {
				return account, nil
			}
			return nil, errors.New("not found")
		},
	}
	svc := &AccountUsageService{
		accountRepo: repo,
		privacyClientFactory: func(_ string) (*req.Client, error) {
			return req.C(), nil
		},
	}

	_, err := svc.GetUsage(context.Background(), account.ID)
	if err != nil {
		t.Fatalf("GetUsage() error = %v", err)
	}

	select {
	case updates := <-repo.bulkUpdateCh:
		if got := updates.Credentials["plan_type"]; got != "plus" {
			t.Fatalf("plan_type update = %v, want plus", got)
		}
		if got := updates.Credentials["subscription_expires_at"]; got != "2026-06-30T00:00:00Z" {
			t.Fatalf("subscription_expires_at update = %v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("等待 plan_type 写入 credentials 超时")
	}

	if got := account.Credentials["plan_type"]; got != "plus" {
		t.Fatalf("in-memory plan_type = %v, want plus", got)
	}
}

func TestAccountUsageService_RefreshOpenAIPlanTypeUpdatesOAuthAccount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/accounts/check/v4-2023-04-27" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer batch-token" {
			t.Fatalf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"accounts":{"org-123":{"account":{"plan_type":"pro","is_default":true},"entitlement":{"expires_at":"2026-07-31T00:00:00Z"}}}}`)
	}))
	defer server.Close()
	originalURL := chatGPTAccountsCheckURL
	chatGPTAccountsCheckURL = server.URL + "/backend-api/accounts/check/v4-2023-04-27"
	t.Cleanup(func() { chatGPTAccountsCheckURL = originalURL })

	repo := &accountUsageCodexProbeRepo{bulkUpdateCh: make(chan AccountBulkUpdate, 1)}
	svc := &AccountUsageService{
		accountRepo: repo,
		privacyClientFactory: func(_ string) (*req.Client, error) {
			return req.C(), nil
		},
	}
	account := &Account{
		ID:       101,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token":    "batch-token",
			"organization_id": "org-123",
			"plan_type":       "free",
		},
	}

	if err := svc.RefreshOpenAIPlanType(context.Background(), account); err != nil {
		t.Fatalf("RefreshOpenAIPlanType() error = %v", err)
	}

	select {
	case updates := <-repo.bulkUpdateCh:
		if got := updates.Credentials["plan_type"]; got != "pro" {
			t.Fatalf("plan_type update = %v, want pro", got)
		}
		if got := updates.Credentials["subscription_expires_at"]; got != "2026-07-31T00:00:00Z" {
			t.Fatalf("subscription_expires_at update = %v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("等待批量 plan_type 写入 credentials 超时")
	}

	if got := account.Credentials["plan_type"]; got != "pro" {
		t.Fatalf("in-memory plan_type = %v, want pro", got)
	}
}

func TestAccountUsageService_GetOpenAIUsageSkipsPlanTypeRefreshWhenCodexSnapshotIsFresh(t *testing.T) {
	t.Parallel()

	repo := &accountUsageCodexProbeRepo{bulkUpdateCh: make(chan AccountBulkUpdate, 1)}
	svc := &AccountUsageService{
		accountRepo: repo,
		privacyClientFactory: func(_ string) (*req.Client, error) {
			t.Fatal("fresh codex usage snapshot should not refresh plan_type")
			return nil, nil
		},
	}
	account := &Account{
		ID:       42,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token": "access-token",
			"plan_type":    "plus",
		},
		Extra: map[string]any{
			"openai_oauth_ws_mode":   OpenAIOAuthWSModeManagedSession,
			"codex_usage_updated_at": time.Now().UTC().Format(time.RFC3339),
			"codex_5h_used_percent":  10.0,
			"codex_5h_reset_at":      time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"codex_7d_used_percent":  20.0,
			"codex_7d_reset_at":      time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
		},
	}

	if _, _, err := svc.getOpenAIUsage(context.Background(), account, false); err != nil {
		t.Fatalf("getOpenAIUsage() error = %v", err)
	}

	select {
	case updates := <-repo.bulkUpdateCh:
		t.Fatalf("不应刷新新鲜 usage 快照的 plan_type: %#v", updates)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestShouldRefreshOpenAICodexSnapshot(t *testing.T) {
	t.Parallel()

	rateLimitedUntil := time.Now().Add(5 * time.Minute)
	now := time.Now()
	usage := &UsageInfo{
		FiveHour: &UsageProgress{Utilization: 0},
		SevenDay: &UsageProgress{Utilization: 0},
	}

	if !shouldRefreshOpenAICodexSnapshot(&Account{RateLimitResetAt: &rateLimitedUntil}, usage, now) {
		t.Fatal("expected rate-limited account to force codex snapshot refresh")
	}

	if shouldRefreshOpenAICodexSnapshot(&Account{}, usage, now) {
		t.Fatal("expected complete non-rate-limited usage to skip codex snapshot refresh")
	}

	if !shouldRefreshOpenAICodexSnapshot(&Account{}, &UsageInfo{FiveHour: nil, SevenDay: &UsageProgress{}}, now) {
		t.Fatal("expected missing 5h snapshot to require refresh")
	}

	staleAt := now.Add(-(openAIProbeCacheTTL + time.Minute)).Format(time.RFC3339)
	if !shouldRefreshOpenAICodexSnapshot(&Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"openai_oauth_ws_mode":   OpenAIOAuthWSModeManagedSession,
			"codex_usage_updated_at": staleAt,
		},
	}, usage, now) {
		t.Fatal("expected stale ws snapshot to trigger refresh")
	}

	if !shouldRefreshOpenAICodexSnapshot(&Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeSetupToken,
		Extra: map[string]any{
			"openai_oauth_ws_mode":   OpenAIOAuthWSModeManagedSession,
			"codex_usage_updated_at": staleAt,
		},
	}, usage, now) {
		t.Fatal("expected stale setup-token ws snapshot to trigger refresh")
	}
}

func TestExtractOpenAICodexProbeUpdatesAccepts429WithCodexHeaders(t *testing.T) {
	t.Parallel()

	headers := make(http.Header)
	headers.Set("x-codex-primary-used-percent", "100")
	headers.Set("x-codex-primary-reset-after-seconds", "604800")
	headers.Set("x-codex-primary-window-minutes", "10080")
	headers.Set("x-codex-secondary-used-percent", "100")
	headers.Set("x-codex-secondary-reset-after-seconds", "18000")
	headers.Set("x-codex-secondary-window-minutes", "300")

	updates, err := extractOpenAICodexProbeUpdates(&http.Response{StatusCode: http.StatusTooManyRequests, Header: headers})
	if err != nil {
		t.Fatalf("extractOpenAICodexProbeUpdates() error = %v", err)
	}
	if len(updates) == 0 {
		t.Fatal("expected codex probe updates from 429 headers")
	}
	if got := updates["codex_5h_used_percent"]; got != 100.0 {
		t.Fatalf("codex_5h_used_percent = %v, want 100", got)
	}
	if got := updates["codex_7d_used_percent"]; got != 100.0 {
		t.Fatalf("codex_7d_used_percent = %v, want 100", got)
	}
}

func TestAccountUsageService_GetUsageOpenAISetupTokenUsesCodexFingerprintProbe(t *testing.T) {
	var capturedReq *http.Request
	var capturedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedReq = r.Clone(context.Background())
		var err error
		capturedBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		w.Header().Set(openAICodexPrimaryUsedPercentHeader, "12")
		w.Header().Set(openAICodexPrimaryResetSecondsHeader, "604800")
		w.Header().Set(openAICodexPrimaryWindowMinutesHeader, "10080")
		w.Header().Set(openAICodexSecondUsedPercentHeader, "34")
		w.Header().Set(openAICodexSecondResetSecondsHeader, "18000")
		w.Header().Set(openAICodexSecondWindowMinutesHeader, "300")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	targetURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	originalFactory := openAICodexProbeHTTPClientFactory
	openAICodexProbeHTTPClientFactory = func(_ httppool.Options) (*http.Client, error) {
		client := server.Client()
		client.Transport = rewriteChatGPTTestRoundTripper{target: targetURL, base: client.Transport}
		return client, nil
	}
	t.Cleanup(func() { openAICodexProbeHTTPClientFactory = originalFactory })

	repo := &accountUsageCodexProbeRepo{updateExtraCh: make(chan map[string]any, 2)}
	account := &Account{
		ID:       4242,
		Platform: PlatformOpenAI,
		Type:     AccountTypeSetupToken,
		Credentials: map[string]any{
			"access_token":               "setup-access-token",
			"chatgpt_account_id":         "chatgpt-acc",
			"chatgpt_account_is_fedramp": true,
			"user_agent":                 "malicious-inbound/1.0",
		},
		Extra: map[string]any{"openai_oauth_ws_mode": OpenAIOAuthWSModeManagedSession},
	}
	repo.getByID = func(_ context.Context, id int64) (*Account, error) {
		if id == account.ID {
			return account, nil
		}
		return nil, errors.New("not found")
	}
	svc := &AccountUsageService{
		accountRepo:             repo,
		cache:                   NewUsageCache(),
		codexFingerprintService: NewOpenAICodexFingerprintService(repo, nil),
	}

	usage, err := svc.GetUsage(context.Background(), account.ID, true)
	if err != nil {
		t.Fatalf("GetUsage() error = %v", err)
	}
	if usage.FiveHour == nil || usage.FiveHour.Utilization != 34.0 {
		t.Fatalf("FiveHour = %#v, want utilization 34", usage.FiveHour)
	}
	if usage.SevenDay == nil || usage.SevenDay.Utilization != 12.0 {
		t.Fatalf("SevenDay = %#v, want utilization 12", usage.SevenDay)
	}
	if capturedReq == nil {
		t.Fatal("expected setup-token Codex probe request")
	}
	if got := capturedReq.Header.Get("Authorization"); got != "Bearer setup-access-token" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := capturedReq.Header.Get("chatgpt-account-id"); got != "chatgpt-acc" {
		t.Fatalf("chatgpt-account-id = %q, want chatgpt-acc", got)
	}
	if got := capturedReq.Header.Get("x-openai-fedramp"); got != "true" {
		t.Fatalf("x-openai-fedramp = %q, want true", got)
	}
	if got := capturedReq.Header.Get("User-Agent"); got != DefaultOpenAICodexUserAgent {
		t.Fatalf("User-Agent = %q, want fingerprint default", got)
	}
	if got := capturedReq.Header.Get("originator"); got != codexOfficialOriginator {
		t.Fatalf("originator = %q", got)
	}
	if got := capturedReq.Header.Get("Version"); got != codexCLIVersion {
		t.Fatalf("Version = %q", got)
	}
	fp, ok := coerceOpenAICodexFingerprint(account.Extra[OpenAICodexFingerprintExtraKey])
	if !ok {
		t.Fatalf("expected in-memory Codex fingerprint, got %#v", account.Extra[OpenAICodexFingerprintExtraKey])
	}
	if got := capturedReq.Header.Get(openAICodexInstallationIDHeader); got != fp.InstallationID {
		t.Fatalf("%s = %q, want fingerprint installation id", openAICodexInstallationIDHeader, got)
	}
	if got := gjson.GetBytes(capturedBody, "client_metadata.x-codex-installation-id").String(); got != fp.InstallationID {
		t.Fatalf("body installation id = %q, want fingerprint installation id", got)
	}
	if got := capturedReq.Header.Get(openAICodexWindowIDHeader); got == "" || got == "probe_openai_usage:0" {
		t.Fatalf("expected server-resolved OAuth-like window id, got %q", got)
	}
	promptCacheKey := gjson.GetBytes(capturedBody, "prompt_cache_key").String()
	if promptCacheKey == "" || promptCacheKey == "probe_openai_usage" {
		t.Fatalf("expected account-scoped usage probe prompt cache key, got %q", promptCacheKey)
	}

	var sawFingerprint, sawUsage bool
	for i := 0; i < 2; i++ {
		select {
		case updates := <-repo.updateExtraCh:
			if _, ok := updates[OpenAICodexFingerprintExtraKey]; ok {
				sawFingerprint = true
			}
			if got, ok := updates["codex_5h_used_percent"]; ok {
				sawUsage = got == 34.0
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("waiting for setup-token update %d timed out", i+1)
		}
	}
	if !sawFingerprint {
		t.Fatal("expected setup-token usage path to persist Codex fingerprint")
	}
	if !sawUsage {
		t.Fatal("expected setup-token usage path to persist Codex usage snapshot")
	}
}

func TestAccountUsageService_GetUsageOpenAISetupTokenNonFedRAMPOmitsFedRAMPHeader(t *testing.T) {
	tests := []struct {
		name           string
		fedrampPresent bool
		fedrampValue   any
	}{
		{name: "absent"},
		{name: "false", fedrampPresent: true, fedrampValue: false},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var capturedReq *http.Request
			var capturedBody []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				capturedReq = r.Clone(context.Background())
				var err error
				capturedBody, err = io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("read body: %v", err)
				}
				w.Header().Set(openAICodexPrimaryUsedPercentHeader, "78")
				w.Header().Set(openAICodexPrimaryResetSecondsHeader, "604800")
				w.Header().Set(openAICodexPrimaryWindowMinutesHeader, "10080")
				w.Header().Set(openAICodexSecondUsedPercentHeader, "56")
				w.Header().Set(openAICodexSecondResetSecondsHeader, "18000")
				w.Header().Set(openAICodexSecondWindowMinutesHeader, "300")
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			targetURL, err := url.Parse(server.URL)
			if err != nil {
				t.Fatalf("parse test server URL: %v", err)
			}
			originalFactory := openAICodexProbeHTTPClientFactory
			openAICodexProbeHTTPClientFactory = func(_ httppool.Options) (*http.Client, error) {
				client := server.Client()
				client.Transport = rewriteChatGPTTestRoundTripper{target: targetURL, base: client.Transport}
				return client, nil
			}
			t.Cleanup(func() { openAICodexProbeHTTPClientFactory = originalFactory })

			credentials := map[string]any{
				"access_token":       "setup-access-token",
				"chatgpt_account_id": "chatgpt-acc",
				"user_agent":         "malicious-inbound/1.0",
			}
			if tc.fedrampPresent {
				credentials["chatgpt_account_is_fedramp"] = tc.fedrampValue
			}
			repo := &accountUsageCodexProbeRepo{updateExtraCh: make(chan map[string]any, 2)}
			account := &Account{
				ID:          int64(4243 + i),
				Platform:    PlatformOpenAI,
				Type:        AccountTypeSetupToken,
				Credentials: credentials,
				Extra:       map[string]any{"openai_oauth_ws_mode": OpenAIOAuthWSModeManagedSession},
			}
			repo.getByID = func(_ context.Context, id int64) (*Account, error) {
				if id == account.ID {
					return account, nil
				}
				return nil, errors.New("not found")
			}
			svc := &AccountUsageService{
				accountRepo:             repo,
				cache:                   NewUsageCache(),
				codexFingerprintService: NewOpenAICodexFingerprintService(repo, nil),
			}

			usage, err := svc.GetUsage(context.Background(), account.ID, true)
			if err != nil {
				t.Fatalf("GetUsage() error = %v", err)
			}
			if usage.FiveHour == nil || usage.FiveHour.Utilization != 56.0 {
				t.Fatalf("FiveHour = %#v, want utilization 56", usage.FiveHour)
			}
			if usage.SevenDay == nil || usage.SevenDay.Utilization != 78.0 {
				t.Fatalf("SevenDay = %#v, want utilization 78", usage.SevenDay)
			}
			if capturedReq == nil {
				t.Fatal("expected setup-token Codex probe request")
			}
			if got := capturedReq.Header.Get("x-openai-fedramp"); got != "" {
				t.Fatalf("x-openai-fedramp = %q, want empty", got)
			}
			if got := capturedReq.Header.Get("Authorization"); got != "Bearer setup-access-token" {
				t.Fatalf("Authorization = %q", got)
			}
			if got := capturedReq.Header.Get("chatgpt-account-id"); got != "chatgpt-acc" {
				t.Fatalf("chatgpt-account-id = %q, want chatgpt-acc", got)
			}
			if got := capturedReq.Header.Get("User-Agent"); got != DefaultOpenAICodexUserAgent {
				t.Fatalf("User-Agent = %q, want fingerprint default", got)
			}
			if got := capturedReq.Header.Get("originator"); got != codexOfficialOriginator {
				t.Fatalf("originator = %q", got)
			}
			if got := capturedReq.Header.Get("Version"); got != codexCLIVersion {
				t.Fatalf("Version = %q", got)
			}
			fp, ok := coerceOpenAICodexFingerprint(account.Extra[OpenAICodexFingerprintExtraKey])
			if !ok {
				t.Fatalf("expected in-memory Codex fingerprint, got %#v", account.Extra[OpenAICodexFingerprintExtraKey])
			}
			if got := capturedReq.Header.Get(openAICodexInstallationIDHeader); got != fp.InstallationID {
				t.Fatalf("%s = %q, want fingerprint installation id", openAICodexInstallationIDHeader, got)
			}
			if got := gjson.GetBytes(capturedBody, "client_metadata.x-codex-installation-id").String(); got != fp.InstallationID {
				t.Fatalf("body installation id = %q, want fingerprint installation id", got)
			}
		})
	}
}

func TestAccountUsageService_GetUsageOpenAISetupTokenReturnsFingerprintPersistError(t *testing.T) {
	t.Parallel()

	repo := &accountUsageCodexProbeRepo{updateExtraErr: errors.New("persist failed")}
	account := &Account{
		ID:       4545,
		Platform: PlatformOpenAI,
		Type:     AccountTypeSetupToken,
		Credentials: map[string]any{
			"access_token": "setup-access-token",
		},
		Extra: map[string]any{"openai_oauth_ws_mode": OpenAIOAuthWSModeManagedSession},
	}
	svc := &AccountUsageService{
		accountRepo:             repo,
		cache:                   NewUsageCache(),
		codexFingerprintService: NewOpenAICodexFingerprintService(repo, nil),
	}

	usage, _, err := svc.getOpenAIUsage(context.Background(), account, true)
	if err == nil {
		t.Fatal("expected fingerprint persist failure")
	}
	if usage != nil {
		t.Fatalf("usage = %#v, want nil on fingerprint persist failure", usage)
	}
	if !errors.Is(err, errOpenAICodexFingerprintEnsure) {
		t.Fatalf("error = %v, want errOpenAICodexFingerprintEnsure", err)
	}
	if _, ok := account.Extra[OpenAICodexFingerprintExtraKey]; ok {
		t.Fatal("failed fingerprint persist must not appear as converged in account extra")
	}
}

func TestAccountUsageService_GetUsageOpenAISetupTokenReturnsSnapshotPersistError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(openAICodexPrimaryUsedPercentHeader, "12")
		w.Header().Set(openAICodexPrimaryResetSecondsHeader, "604800")
		w.Header().Set(openAICodexPrimaryWindowMinutesHeader, "10080")
		w.Header().Set(openAICodexSecondUsedPercentHeader, "34")
		w.Header().Set(openAICodexSecondResetSecondsHeader, "18000")
		w.Header().Set(openAICodexSecondWindowMinutesHeader, "300")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	targetURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	originalFactory := openAICodexProbeHTTPClientFactory
	openAICodexProbeHTTPClientFactory = func(_ httppool.Options) (*http.Client, error) {
		client := server.Client()
		client.Transport = rewriteChatGPTTestRoundTripper{target: targetURL, base: client.Transport}
		return client, nil
	}
	t.Cleanup(func() { openAICodexProbeHTTPClientFactory = originalFactory })

	repo := &accountUsageCodexProbeRepo{
		updateExtraErrFor: func(updates map[string]any) error {
			if _, ok := updates["codex_usage_updated_at"]; ok {
				return errors.New("snapshot persist failed")
			}
			return nil
		},
	}
	account := &Account{
		ID:       4747,
		Platform: PlatformOpenAI,
		Type:     AccountTypeSetupToken,
		Credentials: map[string]any{
			"access_token": "setup-access-token",
		},
		Extra: map[string]any{"openai_oauth_ws_mode": OpenAIOAuthWSModeManagedSession},
	}
	svc := &AccountUsageService{
		accountRepo:             repo,
		cache:                   NewUsageCache(),
		codexFingerprintService: NewOpenAICodexFingerprintService(repo, nil),
	}

	usage, _, err := svc.getOpenAIUsage(context.Background(), account, true)
	if err == nil {
		t.Fatal("expected snapshot persist failure")
	}
	if usage != nil {
		t.Fatalf("usage = %#v, want nil on snapshot persist failure", usage)
	}
	if !errors.Is(err, errOpenAICodexProbeSnapshotPersist) {
		t.Fatalf("error = %v, want errOpenAICodexProbeSnapshotPersist", err)
	}
	if _, ok := account.Extra["codex_usage_updated_at"]; ok {
		t.Fatal("failed snapshot persist must not appear as converged in account extra")
	}
	if _, ok := svc.cache.openAIProbeCache.Load(account.ID); ok {
		t.Fatal("failed snapshot persist must clear probe throttle cache")
	}
}

func TestAccountUsageService_EnsureFingerprintFallbackPersistsWithAccountRepo(t *testing.T) {
	t.Parallel()

	repo := &accountUsageCodexProbeRepo{updateExtraCh: make(chan map[string]any, 1)}
	account := &Account{
		ID:       4646,
		Platform: PlatformOpenAI,
		Type:     AccountTypeSetupToken,
	}
	req := httptest.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	svc := &AccountUsageService{accountRepo: repo}

	fp, err := svc.ensureOpenAICodexFingerprint(context.Background(), account, req)
	if err != nil {
		t.Fatalf("ensureOpenAICodexFingerprint() error = %v", err)
	}
	if fp.InstallationID == "" {
		t.Fatal("expected generated fingerprint")
	}
	if got := req.Header.Get("User-Agent"); got != fp.UAProfile.UserAgent() {
		t.Fatalf("User-Agent = %q, want fingerprint user agent", got)
	}
	select {
	case updates := <-repo.updateExtraCh:
		if _, ok := updates[OpenAICodexFingerprintExtraKey]; !ok {
			t.Fatalf("expected fallback to persist fingerprint, got %#v", updates)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiting for fallback fingerprint persist timed out")
	}
}

func TestAccountUsageService_GetUsageOpenAISetupTokenPreservesEstimateWhenProbeHasNoSnapshot(t *testing.T) {
	t.Parallel()

	windowEnd := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	account := &Account{
		ID:               4343,
		Platform:         PlatformOpenAI,
		Type:             AccountTypeSetupToken,
		SessionWindowEnd: &windowEnd,
		Extra: map[string]any{
			"session_window_utilization": 0.42,
		},
	}
	svc := &AccountUsageService{}
	usage, _, err := svc.getOpenAIUsage(context.Background(), account, false)
	if err != nil {
		t.Fatalf("getOpenAIUsage() error = %v", err)
	}
	if usage.FiveHour == nil {
		t.Fatal("expected setup-token 5h estimate")
	}
	if usage.FiveHour.Utilization != 42.0 {
		t.Fatalf("FiveHour.Utilization = %v, want 42", usage.FiveHour.Utilization)
	}
	if usage.FiveHour.ResetsAt == nil || !usage.FiveHour.ResetsAt.Equal(windowEnd) {
		t.Fatalf("FiveHour.ResetsAt = %v, want %v", usage.FiveHour.ResetsAt, windowEnd)
	}
}

func TestAccountUsageService_GetUsageOpenAIAPIKeyDoesNotUseCodexFingerprintProbe(t *testing.T) {
	t.Parallel()

	repo := &accountUsageCodexProbeRepo{updateExtraCh: make(chan map[string]any, 1)}
	account := &Account{ID: 4444, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	repo.getByID = func(_ context.Context, id int64) (*Account, error) {
		if id == account.ID {
			return account, nil
		}
		return nil, errors.New("not found")
	}
	svc := &AccountUsageService{accountRepo: repo, cache: NewUsageCache()}

	_, err := svc.GetUsage(context.Background(), account.ID, true)
	if err == nil {
		t.Fatal("expected APIKey usage query to stay unsupported")
	}
	if _, ok := account.Extra[OpenAICodexFingerprintExtraKey]; ok {
		t.Fatal("APIKey usage must not create OAuth Codex fingerprint")
	}
	select {
	case updates := <-repo.updateExtraCh:
		t.Fatalf("APIKey usage must not persist Codex updates: %#v", updates)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestAccountUsageService_PersistOpenAICodexProbeSnapshotOnlyUpdatesExtraWithoutFiveHourReset(t *testing.T) {
	t.Parallel()

	repo := &accountUsageCodexProbeRepo{
		updateExtraCh: make(chan map[string]any, 1),
		rateLimitCh:   make(chan time.Time, 1),
		sessionWindowEndCh: make(chan struct {
			id  int64
			end time.Time
		}, 1),
	}
	svc := &AccountUsageService{accountRepo: repo}
	err := svc.persistOpenAICodexProbeSnapshot(context.Background(), 321, map[string]any{
		"codex_usage_updated_at": time.Now().UTC().Format(time.RFC3339),
		"codex_7d_used_percent":  100.0,
		"codex_7d_reset_at":      time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("persistOpenAICodexProbeSnapshot() error = %v", err)
	}

	select {
	case updates := <-repo.updateExtraCh:
		if got := updates["codex_7d_used_percent"]; got != 100.0 {
			t.Fatalf("codex_7d_used_percent = %v, want 100", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("等待 codex 探测快照写入 extra 超时")
	}

	select {
	case got := <-repo.rateLimitCh:
		t.Fatalf("不应将探测快照写入运行时限流状态: %v", got)
	case <-time.After(200 * time.Millisecond):
	}
	select {
	case got := <-repo.sessionWindowEndCh:
		t.Fatalf("不应在缺少 5h reset 时回写 SessionWindowEnd: %#v", got)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestAccountUsageService_PersistOpenAICodexProbeSnapshotWritesFiveHourSessionWindowEnd(t *testing.T) {
	t.Parallel()

	repo := &accountUsageCodexProbeRepo{
		updateExtraCh: make(chan map[string]any, 1),
		sessionWindowEndCh: make(chan struct {
			id  int64
			end time.Time
		}, 1),
	}
	svc := &AccountUsageService{accountRepo: repo}
	resetAt := time.Now().Add(90 * time.Minute).UTC().Truncate(time.Second)
	err := svc.persistOpenAICodexProbeSnapshot(context.Background(), 321, map[string]any{
		"codex_usage_updated_at": time.Now().UTC().Format(time.RFC3339),
		"codex_5h_used_percent":  42.0,
		"codex_5h_reset_at":      resetAt.Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("persistOpenAICodexProbeSnapshot() error = %v", err)
	}

	select {
	case <-repo.updateExtraCh:
	case <-time.After(2 * time.Second):
		t.Fatal("等待 codex 探测快照写入 extra 超时")
	}
	select {
	case got := <-repo.sessionWindowEndCh:
		if got.id != 321 {
			t.Fatalf("SessionWindowEnd account id = %d, want 321", got.id)
		}
		if !got.end.Equal(resetAt) {
			t.Fatalf("SessionWindowEnd = %v, want %v", got.end, resetAt)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("等待 5h reset 回写 SessionWindowEnd 超时")
	}
}

func TestBuildUsageInfo_FableWindow(t *testing.T) {
	resetAt := time.Now().Add(6 * 24 * time.Hour).UTC().Truncate(time.Second)
	resp := &ClaudeUsageResponse{
		SevenDayOverageIncluded: ClaudeUsageWindow{
			Utilization: 87,
			ResetsAt:    resetAt.Format(time.RFC3339),
		},
	}

	info := (&AccountUsageService{}).buildUsageInfo(resp, nil)
	if info.SevenDayFable == nil {
		t.Fatal("expected Fable usage window")
	}
	if info.SevenDayFable.Utilization != 87 {
		t.Fatalf("utilization = %v, want 87", info.SevenDayFable.Utilization)
	}
	if info.SevenDayFable.ResetsAt == nil || !info.SevenDayFable.ResetsAt.Equal(resetAt) {
		t.Fatalf("reset = %v, want %v", info.SevenDayFable.ResetsAt, resetAt)
	}
}

func TestBuildPassiveUsageWindow_Fable(t *testing.T) {
	resetAt := time.Now().Add(6 * 24 * time.Hour).Unix()
	window := buildPassiveUsageWindow(map[string]any{
		"passive_usage_7d_oi_utilization": 0.87,
		"passive_usage_7d_oi_reset":       resetAt,
	}, "passive_usage_7d_oi_utilization", "passive_usage_7d_oi_reset")

	if window == nil {
		t.Fatal("expected Fable passive usage window")
	}
	if window.Utilization != 87 {
		t.Fatalf("utilization = %v, want 87", window.Utilization)
	}
	if window.ResetsAt == nil || window.ResetsAt.Unix() != resetAt {
		t.Fatalf("reset = %v, want %d", window.ResetsAt, resetAt)
	}
}

func TestSyncActiveToPassive_WritesFableWindow(t *testing.T) {
	repo := &accountUsageCodexProbeRepo{updateExtraCh: make(chan map[string]any, 1)}
	svc := &AccountUsageService{accountRepo: repo}
	resetAt := time.Now().Add(6 * 24 * time.Hour).UTC().Truncate(time.Second)

	svc.syncActiveToPassive(context.Background(), 456, &UsageInfo{
		SevenDayFable: &UsageProgress{Utilization: 87, ResetsAt: &resetAt},
	})

	select {
	case updates := <-repo.updateExtraCh:
		if got := updates["passive_usage_7d_oi_utilization"]; got != 0.87 {
			t.Fatalf("passive_usage_7d_oi_utilization = %v, want 0.87", got)
		}
		if got := updates["passive_usage_7d_oi_reset"]; got != resetAt.Unix() {
			t.Fatalf("passive_usage_7d_oi_reset = %v, want %d", got, resetAt.Unix())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("等待 Fable usage 写入 extra 超时")
	}
}

func TestSyncActiveToPassive_WritesFiveHourSessionWindowEnd(t *testing.T) {
	t.Parallel()

	repo := &accountUsageCodexProbeRepo{
		updateExtraCh: make(chan map[string]any, 1),
		sessionWindowEndCh: make(chan struct {
			id  int64
			end time.Time
		}, 1),
	}
	svc := &AccountUsageService{accountRepo: repo}
	resetAt := time.Now().Add(time.Hour).UTC().Truncate(time.Second)

	svc.syncActiveToPassive(context.Background(), 456, &UsageInfo{
		FiveHour: &UsageProgress{Utilization: 42, ResetsAt: &resetAt},
	})

	select {
	case got := <-repo.sessionWindowEndCh:
		if got.id != 456 {
			t.Fatalf("SessionWindowEnd account id = %d, want 456", got.id)
		}
		if !got.end.Equal(resetAt) {
			t.Fatalf("SessionWindowEnd = %v, want %v", got.end, resetAt)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("等待 active usage 5h reset 回写 SessionWindowEnd 超时")
	}
	select {
	case updates := <-repo.updateExtraCh:
		if got := updates["session_window_utilization"]; got != 0.42 {
			t.Fatalf("session_window_utilization = %v, want 0.42", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("等待 active usage 写入 extra 超时")
	}
}

func TestSyncActiveToPassive_SkipsSessionWindowEndWhenResetMissing(t *testing.T) {
	t.Parallel()

	repo := &accountUsageCodexProbeRepo{
		updateExtraCh: make(chan map[string]any, 1),
		sessionWindowEndCh: make(chan struct {
			id  int64
			end time.Time
		}, 1),
	}
	svc := &AccountUsageService{accountRepo: repo}

	svc.syncActiveToPassive(context.Background(), 456, &UsageInfo{
		FiveHour: &UsageProgress{Utilization: 42},
	})

	select {
	case <-repo.updateExtraCh:
	case <-time.After(2 * time.Second):
		t.Fatal("等待 active usage 写入 extra 超时")
	}
	select {
	case got := <-repo.sessionWindowEndCh:
		t.Fatalf("不应在缺少 5h reset 时回写 SessionWindowEnd: %#v", got)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestAccountUsageService_GetOpenAIUsage_DoesNotPromoteCodexExtraToRateLimit(t *testing.T) {
	t.Parallel()

	resetAt := time.Now().Add(6 * 24 * time.Hour).UTC().Truncate(time.Second)
	repo := &accountUsageCodexProbeRepo{
		rateLimitCh: make(chan time.Time, 1),
	}
	svc := &AccountUsageService{accountRepo: repo}
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"codex_5h_used_percent": 1.0,
			"codex_5h_reset_at":     time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339),
			"codex_7d_used_percent": 100.0,
			"codex_7d_reset_at":     resetAt.Format(time.RFC3339),
		},
	}

	usage, _, err := svc.getOpenAIUsage(context.Background(), account, false)
	if err != nil {
		t.Fatalf("getOpenAIUsage() error = %v", err)
	}
	if usage.SevenDay == nil || usage.SevenDay.Utilization != 100.0 {
		t.Fatalf("预期 7 天用量仍然可见，实际为 %#v", usage.SevenDay)
	}
	if account.RateLimitResetAt != nil {
		t.Fatalf("不应让已耗尽的 codex extra 改写运行时限流状态: %v", account.RateLimitResetAt)
	}
	select {
	case got := <-repo.rateLimitCh:
		t.Fatalf("不应将已耗尽的 codex extra 持久化为运行时限流状态: %v", got)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestEstimateSetupTokenUsage_ExpiredWindowZeroesAndClearsReset(t *testing.T) {
	t.Parallel()

	windowEnd := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	account := &Account{
		SessionWindowEnd:    &windowEnd,
		SessionWindowStatus: "rejected",
		Extra: map[string]any{
			"session_window_utilization": 0.99,
		},
	}

	usage := (&AccountUsageService{}).estimateSetupTokenUsage(account)
	if usage.FiveHour == nil {
		t.Fatal("expected FiveHour usage")
	}
	if usage.FiveHour.Utilization != 0 {
		t.Fatalf("expired setup-token utilization = %v, want 0", usage.FiveHour.Utilization)
	}
	if usage.FiveHour.ResetsAt != nil {
		t.Fatalf("expired setup-token ResetsAt = %v, want nil", usage.FiveHour.ResetsAt)
	}
	if usage.FiveHour.RemainingSeconds != 0 {
		t.Fatalf("expired setup-token RemainingSeconds = %v, want 0", usage.FiveHour.RemainingSeconds)
	}
}

func TestEstimateSetupTokenUsage_ActiveWindowPreservesUtilization(t *testing.T) {
	t.Parallel()

	windowEnd := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	account := &Account{
		SessionWindowEnd: &windowEnd,
		Extra: map[string]any{
			"session_window_utilization": 0.42,
		},
	}

	usage := (&AccountUsageService{}).estimateSetupTokenUsage(account)
	if usage.FiveHour == nil {
		t.Fatal("expected FiveHour usage")
	}
	if usage.FiveHour.Utilization != 42.0 {
		t.Fatalf("active setup-token utilization = %v, want 42", usage.FiveHour.Utilization)
	}
	if usage.FiveHour.ResetsAt == nil || !usage.FiveHour.ResetsAt.Equal(windowEnd) {
		t.Fatalf("active setup-token ResetsAt = %v, want %v", usage.FiveHour.ResetsAt, windowEnd)
	}
	if usage.FiveHour.RemainingSeconds <= 0 {
		t.Fatalf("active setup-token RemainingSeconds = %v, want > 0", usage.FiveHour.RemainingSeconds)
	}
}

func TestBuildCodexUsageProgressFromExtra_ZerosExpiredWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 16, 12, 0, 0, 0, time.UTC)

	t.Run("expired 5h window zeroes utilization", func(t *testing.T) {
		extra := map[string]any{
			"codex_5h_used_percent": 42.0,
			"codex_5h_reset_at":     "2026-03-16T10:00:00Z", // 2h ago
		}
		progress := buildCodexUsageProgressFromExtra(extra, "5h", now)
		if progress == nil {
			t.Fatal("expected non-nil progress")
		}
		if progress.Utilization != 0 {
			t.Fatalf("expected Utilization=0 for expired window, got %v", progress.Utilization)
		}
		if progress.ResetsAt != nil {
			t.Fatalf("expected ResetsAt=nil for expired window, got %v", progress.ResetsAt)
		}
		if progress.RemainingSeconds != 0 {
			t.Fatalf("expected RemainingSeconds=0, got %v", progress.RemainingSeconds)
		}
	})

	t.Run("active 5h window keeps utilization", func(t *testing.T) {
		resetAt := now.Add(2 * time.Hour).Format(time.RFC3339)
		extra := map[string]any{
			"codex_5h_used_percent": 42.0,
			"codex_5h_reset_at":     resetAt,
		}
		progress := buildCodexUsageProgressFromExtra(extra, "5h", now)
		if progress == nil {
			t.Fatal("expected non-nil progress")
		}
		if progress.Utilization != 42.0 {
			t.Fatalf("expected Utilization=42, got %v", progress.Utilization)
		}
	})

	t.Run("expired 7d window zeroes utilization", func(t *testing.T) {
		extra := map[string]any{
			"codex_7d_used_percent": 88.0,
			"codex_7d_reset_at":     "2026-03-15T00:00:00Z", // yesterday
		}
		progress := buildCodexUsageProgressFromExtra(extra, "7d", now)
		if progress == nil {
			t.Fatal("expected non-nil progress")
		}
		if progress.Utilization != 0 {
			t.Fatalf("expected Utilization=0 for expired 7d window, got %v", progress.Utilization)
		}
		if progress.ResetsAt != nil {
			t.Fatalf("expected ResetsAt=nil for expired 7d window, got %v", progress.ResetsAt)
		}
	})
}

type openAIWindowStatsRepoStub struct {
	UsageLogRepository
	callCount atomic.Int32
}

func (s *openAIWindowStatsRepoStub) GetAccountWindowStats(ctx context.Context, accountID int64, startTime time.Time) (*usagestats.AccountStats, error) {
	s.callCount.Add(1)
	return &usagestats.AccountStats{Requests: 1, Tokens: 100, Cost: 0.5, StandardCost: 0.5, UserCost: 0.5}, nil
}

func TestAccountUsageService_OpenAIWindowStatsCache(t *testing.T) {
	repo := &openAIWindowStatsRepoStub{}
	svc := &AccountUsageService{
		usageLogRepo: repo,
		cache:        NewUsageCache(),
	}
	ctx := context.Background()
	start := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)

	// 首次查询入库；TTL 内同起点第二次命中缓存
	if got := svc.openAIAccountWindowStats(ctx, 7, "5h", start, false); got == nil || got.Requests != 1 {
		t.Fatalf("first call: unexpected result %+v", got)
	}
	if got := svc.openAIAccountWindowStats(ctx, 7, "5h", start, false); got == nil || got.Requests != 1 {
		t.Fatalf("cached call: unexpected result %+v", got)
	}
	if got := repo.callCount.Load(); got != 1 {
		t.Fatalf("DB call count = %d, want 1 (second call must hit cache)", got)
	}

	// 窗口起点变化 → 缓存失效重查
	if svc.openAIAccountWindowStats(ctx, 7, "5h", start.Add(6*time.Hour), false) == nil {
		t.Fatal("window-rolled call returned nil")
	}
	if got := repo.callCount.Load(); got != 2 {
		t.Fatalf("DB call count after window roll = %d, want 2", got)
	}

	// 不同窗口类型独立缓存
	if svc.openAIAccountWindowStats(ctx, 7, "7d", start, false) == nil {
		t.Fatal("7d call returned nil")
	}
	if got := repo.callCount.Load(); got != 3 {
		t.Fatalf("DB call count after 7d = %d, want 3", got)
	}

	// force=true 绕过缓存
	if svc.openAIAccountWindowStats(ctx, 7, "5h", start, true) == nil {
		t.Fatal("force call returned nil")
	}
	if got := repo.callCount.Load(); got != 4 {
		t.Fatalf("DB call count after force = %d, want 4", got)
	}
}
