//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type rateLimit429AccountRepoStub struct {
	mockAccountRepoForGemini
	rateLimitCalls      int
	lastRateLimitID     int64
	lastRateLimitReset  time.Time
	modelRateLimitCalls []modelNotFoundRateLimitCall
}

func (r *rateLimit429AccountRepoStub) SetRateLimited(_ context.Context, id int64, resetAt time.Time) error {
	r.rateLimitCalls++
	r.lastRateLimitID = id
	r.lastRateLimitReset = resetAt
	return nil
}

func (r *rateLimit429AccountRepoStub) SetModelRateLimit(_ context.Context, id int64, scope string, resetAt time.Time, reason ...string) error {
	call := modelNotFoundRateLimitCall{accountID: id, scope: scope, resetAt: resetAt}
	if len(reason) > 0 {
		call.reason = reason[0]
	}
	r.modelRateLimitCalls = append(r.modelRateLimitCalls, call)
	return nil
}

func TestGetRateLimit429CooldownSettings_DefaultsWhenNotSet(t *testing.T) {
	repo := newMockSettingRepo()
	svc := NewSettingService(repo, &config.Config{})

	settings, err := svc.GetRateLimit429CooldownSettings(context.Background())
	require.NoError(t, err)
	require.True(t, settings.Enabled)
	require.Equal(t, 5, settings.CooldownSeconds)
}

func TestGetRateLimit429CooldownSettings_ReadsFromDB(t *testing.T) {
	repo := newMockSettingRepo()
	data, _ := json.Marshal(RateLimit429CooldownSettings{Enabled: false, CooldownSeconds: 12})
	repo.data[SettingKeyRateLimit429CooldownSettings] = string(data)
	svc := NewSettingService(repo, &config.Config{})

	settings, err := svc.GetRateLimit429CooldownSettings(context.Background())
	require.NoError(t, err)
	require.False(t, settings.Enabled)
	require.Equal(t, 12, settings.CooldownSeconds)
}

func TestSetRateLimit429CooldownSettings_EnabledRejectsOutOfRange(t *testing.T) {
	svc := NewSettingService(newMockSettingRepo(), &config.Config{})

	for _, seconds := range []int{0, -1, 7201, 99999} {
		err := svc.SetRateLimit429CooldownSettings(context.Background(), &RateLimit429CooldownSettings{
			Enabled: true, CooldownSeconds: seconds,
		})
		require.Error(t, err, "should reject enabled=true + cooldown_seconds=%d", seconds)
		require.Contains(t, err.Error(), "cooldown_seconds must be between 1-7200")
	}
}

func TestHandle429_FallbackUsesDBSeconds(t *testing.T) {
	accountRepo := &rateLimit429AccountRepoStub{}
	settingRepo := newMockSettingRepo()
	data, _ := json.Marshal(RateLimit429CooldownSettings{Enabled: true, CooldownSeconds: 12})
	settingRepo.data[SettingKeyRateLimit429CooldownSettings] = string(data)

	settingSvc := NewSettingService(settingRepo, &config.Config{})
	svc := NewRateLimitService(accountRepo, nil, &config.Config{}, nil, nil)
	svc.SetSettingService(settingSvc)

	account := &Account{ID: 42, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	before := time.Now()
	svc.handle429(context.Background(), account, http.Header{}, []byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))
	after := time.Now()

	require.Equal(t, 1, accountRepo.rateLimitCalls)
	require.Equal(t, int64(42), accountRepo.lastRateLimitID)
	require.True(t, !accountRepo.lastRateLimitReset.Before(before.Add(12*time.Second)) && !accountRepo.lastRateLimitReset.After(after.Add(12*time.Second)))
}

func TestHandle429_FallbackDisabledSkipsLocalMark(t *testing.T) {
	accountRepo := &rateLimit429AccountRepoStub{}
	settingRepo := newMockSettingRepo()
	data, _ := json.Marshal(RateLimit429CooldownSettings{Enabled: false, CooldownSeconds: 12})
	settingRepo.data[SettingKeyRateLimit429CooldownSettings] = string(data)

	settingSvc := NewSettingService(settingRepo, &config.Config{})
	svc := NewRateLimitService(accountRepo, nil, &config.Config{}, nil, nil)
	svc.SetSettingService(settingSvc)

	account := &Account{ID: 43, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	svc.handle429(context.Background(), account, http.Header{}, []byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))

	require.Zero(t, accountRepo.rateLimitCalls)
}

func TestHandle429_FallbackUsesDefaultSecondsWhenSettingServiceMissing(t *testing.T) {
	accountRepo := &rateLimit429AccountRepoStub{}
	cfg := &config.Config{}
	svc := NewRateLimitService(accountRepo, nil, cfg, nil, nil)

	account := &Account{ID: 44, Platform: PlatformGemini, Type: AccountTypeAPIKey}
	before := time.Now()
	svc.handle429(context.Background(), account, http.Header{}, []byte(`{"error":{"message":"slow down"}}`))
	after := time.Now()

	require.Equal(t, 1, accountRepo.rateLimitCalls)
	require.Equal(t, int64(44), accountRepo.lastRateLimitID)
	require.True(t, !accountRepo.lastRateLimitReset.Before(before.Add(5*time.Second)) && !accountRepo.lastRateLimitReset.After(after.Add(5*time.Second)))
}

func storeOpenAIOAuth429DynamicSettings(t *testing.T, repo *mockSettingRepo, settings OpenAIOAuth429DynamicSettings) {
	t.Helper()
	data, err := json.Marshal(settings)
	require.NoError(t, err)
	repo.data[SettingKeyOpenAIOAuth429DynamicSettings] = string(data)
}

func TestHandle429_OpenAIOAuthDynamicDisabledIgnoresResetSignals(t *testing.T) {
	accountRepo := &rateLimit429AccountRepoStub{}
	settingSvc := NewSettingService(newMockSettingRepo(), &config.Config{})
	svc := NewRateLimitService(accountRepo, nil, &config.Config{}, nil, nil)
	svc.SetSettingService(settingSvc)

	headers := http.Header{}
	headers.Set("x-codex-primary-used-percent", "100")
	headers.Set("x-codex-primary-reset-after-seconds", "604800")
	headers.Set("x-codex-primary-window-minutes", "10080")
	account := &Account{ID: 45, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	svc.handle429(context.Background(), account, headers, []byte(`{"error":{"type":"usage_limit_reached","message":"limit reached","resets_at":1777283883}}`))

	require.Zero(t, accountRepo.rateLimitCalls)
}

func TestOpenAIOAuth429Dynamic_RateLimitsAfterThreshold(t *testing.T) {
	accountRepo := &rateLimit429AccountRepoStub{}
	settingRepo := newMockSettingRepo()
	storeOpenAIOAuth429DynamicSettings(t, settingRepo, OpenAIOAuth429DynamicSettings{
		Enabled:        true,
		WindowSeconds:  60,
		MinSamples:     3,
		Min429:         2,
		RatioThreshold: 0.6,
		BlockSeconds:   12,
	})
	settingSvc := NewSettingService(settingRepo, &config.Config{})
	svc := NewRateLimitService(accountRepo, nil, &config.Config{}, nil, nil)
	svc.SetSettingService(settingSvc)
	account := &Account{ID: 46, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	before := time.Now()
	svc.handle429(context.Background(), account, http.Header{}, []byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))
	require.Zero(t, accountRepo.rateLimitCalls)
	svc.RecordOpenAIOAuthUpstreamOutcome(context.Background(), account, http.StatusOK)
	require.Zero(t, accountRepo.rateLimitCalls)
	svc.handle429(context.Background(), account, http.Header{}, []byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))
	after := time.Now()

	require.Equal(t, 1, accountRepo.rateLimitCalls)
	require.Equal(t, account.ID, accountRepo.lastRateLimitID)
	require.True(t, !accountRepo.lastRateLimitReset.Before(before.Add(12*time.Second)) && !accountRepo.lastRateLimitReset.After(after.Add(12*time.Second)))
	svc.openAIOAuth429DynamicMu.Lock()
	_, hasStat := svc.openAIOAuth429DynamicStat[account.ID]
	svc.openAIOAuth429DynamicMu.Unlock()
	require.False(t, hasStat)
}

func TestOpenAIOAuth429Dynamic_AllowsThirtyDayPause(t *testing.T) {
	accountRepo := &rateLimit429AccountRepoStub{}
	settingRepo := newMockSettingRepo()
	storeOpenAIOAuth429DynamicSettings(t, settingRepo, OpenAIOAuth429DynamicSettings{
		Enabled:        true,
		WindowSeconds:  60,
		MinSamples:     2,
		Min429:         2,
		RatioThreshold: 1,
		BlockSeconds:   OpenAIOAuth429DynamicMaxBlockSeconds,
	})
	settingSvc := NewSettingService(settingRepo, &config.Config{})
	svc := NewRateLimitService(accountRepo, nil, &config.Config{}, nil, nil)
	svc.SetSettingService(settingSvc)
	account := &Account{ID: 52, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	before := time.Now()
	svc.handle429(context.Background(), account, http.Header{}, []byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))
	svc.handle429(context.Background(), account, http.Header{}, []byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))
	after := time.Now()

	require.Equal(t, 1, accountRepo.rateLimitCalls)
	wantPause := time.Duration(OpenAIOAuth429DynamicMaxBlockSeconds) * time.Second
	require.False(t, accountRepo.lastRateLimitReset.Before(before.Add(wantPause)))
	require.False(t, accountRepo.lastRateLimitReset.After(after.Add(wantPause)))
}

func TestOpenAIOAuth429Dynamic_RatioBelowThresholdDoesNotRateLimit(t *testing.T) {
	accountRepo := &rateLimit429AccountRepoStub{}
	settingRepo := newMockSettingRepo()
	storeOpenAIOAuth429DynamicSettings(t, settingRepo, OpenAIOAuth429DynamicSettings{
		Enabled:        true,
		WindowSeconds:  60,
		MinSamples:     4,
		Min429:         2,
		RatioThreshold: 0.75,
		BlockSeconds:   12,
	})
	settingSvc := NewSettingService(settingRepo, &config.Config{})
	svc := NewRateLimitService(accountRepo, nil, &config.Config{}, nil, nil)
	svc.SetSettingService(settingSvc)
	account := &Account{ID: 47, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	svc.handle429(context.Background(), account, http.Header{}, []byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))
	svc.RecordOpenAIOAuthUpstreamOutcome(context.Background(), account, http.StatusOK)
	svc.RecordOpenAIOAuthUpstreamOutcome(context.Background(), account, http.StatusOK)
	svc.handle429(context.Background(), account, http.Header{}, []byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))

	require.Zero(t, accountRepo.rateLimitCalls)
}

func TestOpenAIOAuth429Dynamic_ResetStatsClearsWindow(t *testing.T) {
	accountRepo := &rateLimit429AccountRepoStub{}
	settingRepo := newMockSettingRepo()
	storeOpenAIOAuth429DynamicSettings(t, settingRepo, OpenAIOAuth429DynamicSettings{
		Enabled:        true,
		WindowSeconds:  60,
		MinSamples:     3,
		Min429:         2,
		RatioThreshold: 0.6,
		BlockSeconds:   12,
	})
	settingSvc := NewSettingService(settingRepo, &config.Config{})
	svc := NewRateLimitService(accountRepo, nil, &config.Config{}, nil, nil)
	svc.SetSettingService(settingSvc)
	account := &Account{ID: 48, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	svc.handle429(context.Background(), account, http.Header{}, []byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))
	svc.ResetOpenAIOAuth429DynamicStats(account.ID)
	svc.RecordOpenAIOAuthUpstreamOutcome(context.Background(), account, http.StatusOK)
	svc.handle429(context.Background(), account, http.Header{}, []byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))

	require.Zero(t, accountRepo.rateLimitCalls)
}

func TestOpenAIOAuth429Dynamic_SettingsCacheAvoidsRepeatedHotPathReads(t *testing.T) {
	accountRepo := &rateLimit429AccountRepoStub{}
	settingRepo := newCountingOpenAIOAuth429SettingRepo()
	storeOpenAIOAuth429DynamicSettings(t, &settingRepo.mockSettingRepo, OpenAIOAuth429DynamicSettings{
		Enabled:        true,
		WindowSeconds:  60,
		MinSamples:     100,
		Min429:         100,
		RatioThreshold: 1,
		BlockSeconds:   12,
	})
	settingSvc := NewSettingService(settingRepo, &config.Config{})
	svc := NewRateLimitService(accountRepo, nil, &config.Config{}, nil, nil)
	svc.SetSettingService(settingSvc)
	account := &Account{ID: 50, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	svc.handle429(context.Background(), account, http.Header{}, []byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))
	svc.RecordOpenAIOAuthUpstreamOutcome(context.Background(), account, http.StatusOK)
	svc.RecordOpenAIOAuthUpstreamOutcome(context.Background(), account, http.StatusOK)

	require.Equal(t, 1, settingRepo.openAIOAuth429Reads)
	require.Zero(t, accountRepo.rateLimitCalls)
}

func TestOpenAIOAuth429Dynamic_DisabledSettingsClearExistingStats(t *testing.T) {
	accountRepo := &rateLimit429AccountRepoStub{}
	settingRepo := newMockSettingRepo()
	storeOpenAIOAuth429DynamicSettings(t, settingRepo, OpenAIOAuth429DynamicSettings{
		Enabled:        true,
		WindowSeconds:  60,
		MinSamples:     100,
		Min429:         100,
		RatioThreshold: 1,
		BlockSeconds:   12,
	})
	settingSvc := NewSettingService(settingRepo, &config.Config{})
	svc := NewRateLimitService(accountRepo, nil, &config.Config{}, nil, nil)
	svc.SetSettingService(settingSvc)
	account := &Account{ID: 51, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	svc.handle429(context.Background(), account, http.Header{}, []byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))
	require.True(t, svc.hasOpenAIOAuth429DynamicStats(account.ID))
	require.NoError(t, settingSvc.SetOpenAIOAuth429DynamicSettings(context.Background(), &OpenAIOAuth429DynamicSettings{
		Enabled:        false,
		WindowSeconds:  60,
		MinSamples:     100,
		Min429:         100,
		RatioThreshold: 1,
		BlockSeconds:   12,
	}))

	svc.RecordOpenAIOAuthUpstreamOutcome(context.Background(), account, http.StatusOK)

	require.False(t, svc.hasOpenAIOAuth429DynamicStats(account.ID))
	require.Zero(t, accountRepo.rateLimitCalls)
}

func TestOpenAIOAuth429Dynamic_LegacyJSONDefaultsPlanTypeSettings(t *testing.T) {
	settingRepo := newMockSettingRepo()
	require.NoError(t, settingRepo.Set(context.Background(), SettingKeyOpenAIOAuth429DynamicSettings, `{"enabled":true,"window_seconds":300,"min_samples":20,"min_429":3,"ratio_threshold":0.5,"block_seconds":60}`))
	settingSvc := NewSettingService(settingRepo, &config.Config{})

	settings, err := settingSvc.GetOpenAIOAuth429DynamicSettings(context.Background())
	require.NoError(t, err)
	require.True(t, settings.Enabled)
	require.Empty(t, settings.PlanTypeSettings)

	policy, err := settingSvc.GetOpenAIOAuth429DynamicPolicy(context.Background(), "unknown")
	require.NoError(t, err)
	require.True(t, policy.Enabled)
	require.Equal(t, 300, policy.WindowSeconds)
}

func TestOpenAIOAuth429Dynamic_RuntimePolicyUsesCompiledCache(t *testing.T) {
	settingRepo := newMockSettingRepo()
	storeOpenAIOAuth429DynamicSettings(t, settingRepo, OpenAIOAuth429DynamicSettings{
		Enabled:        false,
		WindowSeconds:  300,
		MinSamples:     20,
		Min429:         3,
		RatioThreshold: 0.5,
		BlockSeconds:   60,
		PlanTypeSettings: []OpenAIOAuth429DynamicPlanTypeSettings{{
			PlanType: " Plus ",
			OpenAIOAuth429DynamicPolicy: OpenAIOAuth429DynamicPolicy{
				Enabled: true, WindowSeconds: 120, MinSamples: 4, Min429: 2, RatioThreshold: 0.5, BlockSeconds: 30,
			},
		}},
	})
	settingSvc := NewSettingService(settingRepo, &config.Config{})

	policy, err := settingSvc.GetOpenAIOAuth429DynamicPolicy(context.Background(), " PLUS ")
	require.NoError(t, err)
	require.True(t, policy.Enabled)
	require.Equal(t, 120, policy.WindowSeconds)

	cached := settingSvc.getCachedOpenAIOAuth429DynamicSnapshot(false)
	require.NotNil(t, cached)
	require.Contains(t, cached.byPlanType, "plus")
}

func TestOpenAIOAuth429Dynamic_PolicyForPlanType(t *testing.T) {
	settings := *DefaultOpenAIOAuth429DynamicSettings()
	settings.Enabled = true
	settings.BlockSeconds = 10
	settings.PlanTypeSettings = []OpenAIOAuth429DynamicPlanTypeSettings{
		{
			PlanType: "plus",
			OpenAIOAuth429DynamicPolicy: OpenAIOAuth429DynamicPolicy{
				Enabled:        true,
				WindowSeconds:  60,
				MinSamples:     2,
				Min429:         1,
				RatioThreshold: 0.5,
				BlockSeconds:   99,
			},
		},
	}

	require.Equal(t, 99, settings.PolicyForPlanType(" PLUS ").BlockSeconds)
	require.Equal(t, 10, settings.PolicyForPlanType("pro").BlockSeconds)
	require.Equal(t, 10, settings.PolicyForPlanType("").BlockSeconds)
}

func TestOpenAIOAuth429Dynamic_PlanTypeOverrideControlsScheduling(t *testing.T) {
	accountRepo := &rateLimit429AccountRepoStub{}
	settingRepo := newMockSettingRepo()
	storeOpenAIOAuth429DynamicSettings(t, settingRepo, OpenAIOAuth429DynamicSettings{
		Enabled:        false,
		WindowSeconds:  300,
		MinSamples:     20,
		Min429:         3,
		RatioThreshold: 0.5,
		BlockSeconds:   60,
		PlanTypeSettings: []OpenAIOAuth429DynamicPlanTypeSettings{
			{
				PlanType: "plus",
				OpenAIOAuth429DynamicPolicy: OpenAIOAuth429DynamicPolicy{
					Enabled:        true,
					WindowSeconds:  60,
					MinSamples:     2,
					Min429:         2,
					RatioThreshold: 1,
					BlockSeconds:   12,
				},
			},
		},
	})
	settingSvc := NewSettingService(settingRepo, &config.Config{})
	svc := NewRateLimitService(accountRepo, nil, &config.Config{}, nil, nil)
	svc.SetSettingService(settingSvc)
	plus := &Account{ID: 52, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"plan_type": "plus"}}
	pro := &Account{ID: 53, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"plan_type": "pro"}}

	svc.handle429(context.Background(), plus, http.Header{}, []byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))
	svc.handle429(context.Background(), plus, http.Header{}, []byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))
	svc.handle429(context.Background(), pro, http.Header{}, []byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))

	require.Equal(t, 1, accountRepo.rateLimitCalls)
	require.Equal(t, plus.ID, accountRepo.lastRateLimitID)
}

func TestOpenAIOAuth429Dynamic_PlanTypeChangeResetsExistingWindow(t *testing.T) {
	accountRepo := &rateLimit429AccountRepoStub{}
	settingRepo := newMockSettingRepo()
	policy := OpenAIOAuth429DynamicPolicy{Enabled: true, WindowSeconds: 60, MinSamples: 2, Min429: 2, RatioThreshold: 1, BlockSeconds: 12}
	storeOpenAIOAuth429DynamicSettings(t, settingRepo, OpenAIOAuth429DynamicSettings{
		Enabled: false, WindowSeconds: 300, MinSamples: 20, Min429: 3, RatioThreshold: 0.5, BlockSeconds: 60,
		PlanTypeSettings: []OpenAIOAuth429DynamicPlanTypeSettings{
			{PlanType: "plus", OpenAIOAuth429DynamicPolicy: policy},
			{PlanType: "pro", OpenAIOAuth429DynamicPolicy: policy},
		},
	})
	settingSvc := NewSettingService(settingRepo, &config.Config{})
	svc := NewRateLimitService(accountRepo, nil, &config.Config{}, nil, nil)
	svc.SetSettingService(settingSvc)
	account := &Account{ID: 54, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"plan_type": "plus"}}

	svc.handle429(context.Background(), account, http.Header{}, []byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))
	account.Credentials["plan_type"] = "pro"
	svc.handle429(context.Background(), account, http.Header{}, []byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))
	require.Zero(t, accountRepo.rateLimitCalls)
	svc.handle429(context.Background(), account, http.Header{}, []byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))
	require.Equal(t, 1, accountRepo.rateLimitCalls)
}

func TestOpenAIOAuth429Dynamic_UsageWindowDefense(t *testing.T) {
	accountRepo := &rateLimit429AccountRepoStub{}
	settingRepo := newMockSettingRepo()
	storeOpenAIOAuth429DynamicSettings(t, settingRepo, OpenAIOAuth429DynamicSettings{
		Enabled:                       true,
		WindowSeconds:                 60,
		MinSamples:                    2,
		Min429:                        2,
		RatioThreshold:                1,
		BlockSeconds:                  12,
		UsageWindowCheckEnabled:       true,
		UsageWindow5hThresholdPercent: 90,
		UsageWindow7dThresholdPercent: 95,
	})
	settingSvc := NewSettingService(settingRepo, &config.Config{})
	svc := NewRateLimitService(accountRepo, nil, &config.Config{}, nil, nil)
	svc.SetSettingService(settingSvc)
	account := &Account{ID: 61, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	// Original thresholds fire, but the usage-window defense still blocks the pause.
	used5hLow := 10.0
	window5h := 300
	lowSnapshot := &OpenAICodexUsageSnapshot{
		PrimaryUsedPercent:   &used5hLow,
		PrimaryWindowMinutes: &window5h,
	}
	svc.RecordOpenAIOAuthUpstreamOutcome(context.Background(), account, http.StatusTooManyRequests, lowSnapshot)
	svc.RecordOpenAIOAuthUpstreamOutcome(context.Background(), account, http.StatusTooManyRequests, lowSnapshot)
	require.Equal(t, 0, accountRepo.rateLimitCalls)

	// Once a known window reaches the configured threshold, the existing rule can pause.
	used5hHigh := 90.0
	highSnapshot := &OpenAICodexUsageSnapshot{
		PrimaryUsedPercent:   &used5hHigh,
		PrimaryWindowMinutes: &window5h,
	}
	svc.RecordOpenAIOAuthUpstreamOutcome(context.Background(), account, http.StatusTooManyRequests, highSnapshot)
	require.Equal(t, 1, accountRepo.rateLimitCalls)
}

func TestOpenAIOAuth429Dynamic_UsageWindowDefenseMissingDataWaitsForAccountAge(t *testing.T) {
	accountRepo := &rateLimit429AccountRepoStub{}
	settingRepo := newMockSettingRepo()
	storeOpenAIOAuth429DynamicSettings(t, settingRepo, OpenAIOAuth429DynamicSettings{
		Enabled:                               true,
		WindowSeconds:                         60,
		MinSamples:                            2,
		Min429:                                2,
		RatioThreshold:                        1,
		BlockSeconds:                          12,
		UsageWindowCheckEnabled:               true,
		UsageWindow5hThresholdPercent:         90,
		UsageWindow7dThresholdPercent:         95,
		UsageWindowMissingDataFallbackSeconds: 300,
	})
	settingSvc := NewSettingService(settingRepo, &config.Config{})
	svc := NewRateLimitService(accountRepo, nil, &config.Config{}, nil, nil)
	svc.SetSettingService(settingSvc)
	account := &Account{ID: 62, Platform: PlatformOpenAI, Type: AccountTypeOAuth, CreatedAt: time.Now()}

	svc.RecordOpenAIOAuthUpstreamOutcome(context.Background(), account, http.StatusTooManyRequests)
	svc.RecordOpenAIOAuthUpstreamOutcome(context.Background(), account, http.StatusTooManyRequests)
	require.Equal(t, 0, accountRepo.rateLimitCalls)

	// Keep the collected evidence; once the account is old enough, another
	// outcome can use the original rule without rebuilding the sample window.
	account.CreatedAt = time.Now().Add(-301 * time.Second)
	svc.RecordOpenAIOAuthUpstreamOutcome(context.Background(), account, http.StatusTooManyRequests)
	require.Equal(t, 1, accountRepo.rateLimitCalls)
}

func TestOpenAIOAuth429Dynamic_UsageWindowDefenseMissingDataAccountAgeBoundary(t *testing.T) {
	createdAt := time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC)
	policy := &OpenAIOAuth429DynamicPolicy{UsageWindowMissingDataFallbackSeconds: 300}

	require.False(t, openAIOAuth429UsageWindowReached(nil, nil, createdAt, createdAt.Add(300*time.Second), policy))
	require.True(t, openAIOAuth429UsageWindowReached(nil, nil, createdAt, createdAt.Add(300*time.Second+time.Nanosecond), policy))
}

func TestOpenAIOAuth429Dynamic_UsageWindowDefenseMissingDataZeroWaitPreservesFallback(t *testing.T) {
	createdAt := time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC)
	policy := &OpenAIOAuth429DynamicPolicy{UsageWindowMissingDataFallbackSeconds: 0}

	require.True(t, openAIOAuth429UsageWindowReached(nil, nil, createdAt, createdAt.Add(time.Nanosecond), policy))
	require.False(t, openAIOAuth429UsageWindowReached(nil, nil, time.Time{}, createdAt, policy))
}

func TestOpenAIOAuth429Dynamic_PolicyChangeResetsExistingWindow(t *testing.T) {
	accountRepo := &rateLimit429AccountRepoStub{}
	settingRepo := newMockSettingRepo()
	storeOpenAIOAuth429DynamicSettings(t, settingRepo, OpenAIOAuth429DynamicSettings{
		Enabled:        true,
		WindowSeconds:  60,
		MinSamples:     3,
		Min429:         3,
		RatioThreshold: 1,
		BlockSeconds:   12,
	})
	settingSvc := NewSettingService(settingRepo, &config.Config{})
	svc := NewRateLimitService(accountRepo, nil, &config.Config{}, nil, nil)
	svc.SetSettingService(settingSvc)
	account := &Account{ID: 54, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	svc.RecordOpenAIOAuthUpstreamOutcome(context.Background(), account, http.StatusTooManyRequests)
	svc.RecordOpenAIOAuthUpstreamOutcome(context.Background(), account, http.StatusTooManyRequests)

	err := settingSvc.SetOpenAIOAuth429DynamicSettings(context.Background(), &OpenAIOAuth429DynamicSettings{
		Enabled:        true,
		WindowSeconds:  60,
		MinSamples:     2,
		Min429:         2,
		RatioThreshold: 1,
		BlockSeconds:   12,
	})
	require.NoError(t, err)

	// The first 429 under the new policy starts a fresh window instead of applying
	// the lowered threshold to samples collected under the previous policy.
	svc.RecordOpenAIOAuthUpstreamOutcome(context.Background(), account, http.StatusTooManyRequests)
	require.Equal(t, 0, accountRepo.rateLimitCalls)
	svc.openAIOAuth429DynamicMu.Lock()
	stat := svc.openAIOAuth429DynamicStat[account.ID]
	svc.openAIOAuth429DynamicMu.Unlock()
	require.NotNil(t, stat)
	require.Equal(t, 1, stat.total)
	require.Equal(t, 1, stat.count429)

	svc.RecordOpenAIOAuthUpstreamOutcome(context.Background(), account, http.StatusTooManyRequests)
	require.Equal(t, 1, accountRepo.rateLimitCalls)
}

func TestOpenAIOAuth429Dynamic_RejectsDuplicatePlanTypes(t *testing.T) {
	policy := OpenAIOAuth429DynamicPolicy{Enabled: true, WindowSeconds: 60, MinSamples: 2, Min429: 1, RatioThreshold: 0.5, BlockSeconds: 12}
	settings := *DefaultOpenAIOAuth429DynamicSettings()
	settings.PlanTypeSettings = []OpenAIOAuth429DynamicPlanTypeSettings{
		{PlanType: "plus", OpenAIOAuth429DynamicPolicy: policy},
		{PlanType: " PLUS ", OpenAIOAuth429DynamicPolicy: policy},
	}

	require.EqualError(t, validateOpenAIOAuth429DynamicSettings(&settings), "duplicate plan_type: plus")
}

func TestOpenAIOAuth429Dynamic_RejectsTooManyPlanTypes(t *testing.T) {
	policy := OpenAIOAuth429DynamicPolicy{Enabled: true, WindowSeconds: 60, MinSamples: 2, Min429: 1, RatioThreshold: 0.5, BlockSeconds: 12}
	settings := *DefaultOpenAIOAuth429DynamicSettings()
	settings.PlanTypeSettings = make([]OpenAIOAuth429DynamicPlanTypeSettings, OpenAIOAuth429DynamicMaxPlanTypeSettings+1)
	for i := range settings.PlanTypeSettings {
		settings.PlanTypeSettings[i] = OpenAIOAuth429DynamicPlanTypeSettings{PlanType: fmt.Sprintf("plan-%d", i), OpenAIOAuth429DynamicPolicy: policy}
	}

	require.EqualError(t, validateOpenAIOAuth429DynamicSettings(&settings), "plan_type_settings must not exceed 100 entries")
}

func TestOpenAIOAuth429Dynamic_BlockSecondsValidationAndNormalization(t *testing.T) {
	max := OpenAIOAuth429DynamicMaxBlockSeconds

	atMax := *DefaultOpenAIOAuth429DynamicSettings()
	atMax.Enabled = true
	atMax.BlockSeconds = max
	require.NoError(t, validateOpenAIOAuth429DynamicSettings(&atMax))

	overMax := atMax
	overMax.BlockSeconds = max + 1
	require.EqualError(t, validateOpenAIOAuth429DynamicSettings(&overMax), "block_seconds must be between 1-2592000")

	normalizeOpenAIOAuth429DynamicSettings(&overMax)
	require.Equal(t, max, overMax.BlockSeconds)
}

func TestOpenAIOAuth429Dynamic_StatsRetainedWhenSetRateLimitedFails(t *testing.T) {
	rateLimitErr := errors.New("db unavailable")
	accountRepo := &rateLimit429FailingAccountRepoStub{err: rateLimitErr}
	settingRepo := newMockSettingRepo()
	storeOpenAIOAuth429DynamicSettings(t, settingRepo, OpenAIOAuth429DynamicSettings{
		Enabled:        true,
		WindowSeconds:  60,
		MinSamples:     2,
		Min429:         2,
		RatioThreshold: 1,
		BlockSeconds:   12,
	})
	settingSvc := NewSettingService(settingRepo, &config.Config{})
	svc := NewRateLimitService(accountRepo, nil, &config.Config{}, nil, nil)
	svc.SetSettingService(settingSvc)
	account := &Account{ID: 49, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	svc.handle429(context.Background(), account, http.Header{}, []byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))
	svc.handle429(context.Background(), account, http.Header{}, []byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))

	require.Equal(t, 1, accountRepo.rateLimitCalls)
	svc.openAIOAuth429DynamicMu.Lock()
	stat := svc.openAIOAuth429DynamicStat[account.ID]
	svc.openAIOAuth429DynamicMu.Unlock()
	require.NotNil(t, stat)
	require.False(t, stat.limiting)
	require.Equal(t, 2, stat.total)
	require.Equal(t, 2, stat.count429)
}

type countingOpenAIOAuth429SettingRepo struct {
	mockSettingRepo
	openAIOAuth429Reads int
}

func newCountingOpenAIOAuth429SettingRepo() *countingOpenAIOAuth429SettingRepo {
	return &countingOpenAIOAuth429SettingRepo{mockSettingRepo: *newMockSettingRepo()}
}

func (r *countingOpenAIOAuth429SettingRepo) GetValue(ctx context.Context, key string) (string, error) {
	if key == SettingKeyOpenAIOAuth429DynamicSettings {
		r.openAIOAuth429Reads++
	}
	return r.mockSettingRepo.GetValue(ctx, key)
}

type rateLimit429FailingAccountRepoStub struct {
	rateLimit429AccountRepoStub
	err error
}

func (r *rateLimit429FailingAccountRepoStub) SetRateLimited(ctx context.Context, id int64, resetAt time.Time) error {
	r.rateLimit429AccountRepoStub.SetRateLimited(ctx, id, resetAt)
	return r.err
}
