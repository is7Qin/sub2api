package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/imroc/req/v3"
)

type accountUsageCodexProbeRepo struct {
	stubOpenAIAccountRepo
	updateExtraCh chan map[string]any
	rateLimitCh   chan time.Time
	bulkUpdateCh  chan AccountBulkUpdate
}

func (r *accountUsageCodexProbeRepo) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	if r.updateExtraCh != nil {
		copied := make(map[string]any, len(updates))
		for k, v := range updates {
			copied[k] = v
		}
		r.updateExtraCh <- copied
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

	repo := &accountUsageCodexProbeRepo{bulkUpdateCh: make(chan AccountBulkUpdate, 1)}
	svc := &AccountUsageService{
		accountRepo: repo,
		privacyClientFactory: func(_ string) (*req.Client, error) {
			return req.C(), nil
		},
	}
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

	_, err := svc.getOpenAIUsage(context.Background(), account, false)
	if err != nil {
		t.Fatalf("getOpenAIUsage() error = %v", err)
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
			"openai_oauth_responses_websockets_v2_enabled": true,
			"codex_usage_updated_at":                       time.Now().UTC().Format(time.RFC3339),
			"codex_5h_used_percent":                        10.0,
			"codex_5h_reset_at":                            time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"codex_7d_used_percent":                        20.0,
			"codex_7d_reset_at":                            time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
		},
	}

	if _, err := svc.getOpenAIUsage(context.Background(), account, false); err != nil {
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
			"openai_oauth_responses_websockets_v2_enabled": true,
			"codex_usage_updated_at":                       staleAt,
		},
	}, usage, now) {
		t.Fatal("expected stale ws snapshot to trigger refresh")
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

func TestAccountUsageService_PersistOpenAICodexProbeSnapshotOnlyUpdatesExtra(t *testing.T) {
	t.Parallel()

	repo := &accountUsageCodexProbeRepo{
		updateExtraCh: make(chan map[string]any, 1),
		rateLimitCh:   make(chan time.Time, 1),
	}
	svc := &AccountUsageService{accountRepo: repo}
	svc.persistOpenAICodexProbeSnapshot(321, map[string]any{
		"codex_7d_used_percent": 100.0,
		"codex_7d_reset_at":     time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second).Format(time.RFC3339),
	})

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

	usage, err := svc.getOpenAIUsage(context.Background(), account, false)
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
	})
}
