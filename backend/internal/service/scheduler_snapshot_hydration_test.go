//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

type snapshotHydrationCache struct {
	snapshot         []*Account
	accounts         map[int64]*Account
	staticAccounts   map[int64]*Account
	getAccountCalls  int
	staticReadCalls  int
	lastStaticBucket SchedulerBucket
}

func (c *snapshotHydrationCache) GetSnapshot(ctx context.Context, bucket SchedulerBucket) ([]*Account, bool, error) {
	return c.snapshot, true, nil
}

func (c *snapshotHydrationCache) SetSnapshot(ctx context.Context, bucket SchedulerBucket, accounts []Account) error {
	return nil
}

func (c *snapshotHydrationCache) GetAccount(ctx context.Context, accountID int64) (*Account, error) {
	c.getAccountCalls++
	if c.accounts == nil {
		return nil, nil
	}
	return c.accounts[accountID], nil
}

func (c *snapshotHydrationCache) GetStaticCandidateAccount(_ context.Context, bucket SchedulerBucket, accountID int64) (*Account, error) {
	c.staticReadCalls++
	c.lastStaticBucket = bucket
	if c.staticAccounts != nil {
		return c.staticAccounts[accountID], nil
	}
	return c.accounts[accountID], nil
}

func (c *snapshotHydrationCache) GetStaticCandidateAccountsByIDs(_ context.Context, bucket SchedulerBucket, ids []int64) (map[int64]*Account, error) {
	c.staticReadCalls++
	c.lastStaticBucket = bucket
	source := c.staticAccounts
	if source == nil {
		source = c.accounts
	}
	accounts := make(map[int64]*Account, len(ids))
	for _, id := range ids {
		if account := source[id]; account != nil {
			accounts[id] = account
			continue
		}
		for _, account := range c.snapshot {
			if account != nil && account.ID == id {
				accounts[id] = account
				break
			}
		}
	}
	return accounts, nil
}

// GetSchedulableAccountsByIDs supplies the published candidate metadata used by
// scheduler-backed request selection before the selected account is hydrated.
func (c *snapshotHydrationCache) GetSchedulableAccountsByIDs(_ context.Context, ids []int64) (map[int64]*Account, error) {
	requested := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		requested[id] = struct{}{}
	}
	accounts := make(map[int64]*Account, len(ids))
	for _, account := range c.snapshot {
		if account == nil {
			continue
		}
		if _, ok := requested[account.ID]; ok {
			accounts[account.ID] = account
		}
	}
	return accounts, nil
}

func (c *snapshotHydrationCache) SetAccount(ctx context.Context, account *Account) error {
	return nil
}

func (c *snapshotHydrationCache) DeleteAccount(ctx context.Context, accountID int64) error {
	return nil
}

func (c *snapshotHydrationCache) UpdateLastUsed(ctx context.Context, updates map[int64]time.Time) error {
	return nil
}

func (c *snapshotHydrationCache) TryLockBucket(ctx context.Context, bucket SchedulerBucket, ttl time.Duration) (string, bool, error) {
	return "test-lock", true, nil
}

func (c *snapshotHydrationCache) UnlockBucket(ctx context.Context, bucket SchedulerBucket, token string) error {
	return nil
}

func (c *snapshotHydrationCache) ListBuckets(ctx context.Context) ([]SchedulerBucket, error) {
	return nil, nil
}

func (c *snapshotHydrationCache) GetOutboxWatermark(ctx context.Context) (int64, error) {
	return 0, nil
}

func (c *snapshotHydrationCache) SetOutboxWatermark(ctx context.Context, id int64) error {
	return nil
}

func TestOpenAISelectAccountWithLoadAwareness_HydratesSelectedAccountFromSchedulerSnapshot(t *testing.T) {
	cache := &snapshotHydrationCache{
		snapshot: []*Account{
			{
				ID:          1,
				Platform:    PlatformOpenAI,
				Type:        AccountTypeAPIKey,
				Status:      StatusActive,
				Schedulable: true,
				Concurrency: 1,
				Priority:    1,
				Credentials: map[string]any{
					"model_mapping": map[string]any{
						"gpt-4": "gpt-4",
					},
				},
			},
		},
		accounts: map[int64]*Account{
			1: {
				ID:          1,
				Platform:    PlatformOpenAI,
				Type:        AccountTypeAPIKey,
				Status:      StatusActive,
				Schedulable: true,
				Concurrency: 1,
				Priority:    1,
				Credentials: map[string]any{
					"api_key":       "sk-live",
					"model_mapping": map[string]any{"gpt-4": "gpt-4"},
				},
			},
		},
	}

	schedulerSnapshot := NewSchedulerSnapshotService(cache, nil, nil, nil, nil)
	groupID := int64(2)
	svc := &OpenAIGatewayService{
		schedulerSnapshot: schedulerSnapshot,
		cache:             &stubGatewayCache{},
	}

	selection, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, "", "gpt-4", nil)
	if err != nil {
		t.Fatalf("SelectAccountWithLoadAwareness error: %v", err)
	}
	if selection == nil || selection.Account == nil {
		t.Fatalf("expected selected account")
	}
	if got := selection.Account.GetOpenAIApiKey(); got != "sk-live" {
		t.Fatalf("expected hydrated api key, got %q", got)
	}
}

func TestOpenAINewAcquiredSelectionResult_ReleasesSlotWhenHydrationFails(t *testing.T) {
	cache := &snapshotHydrationCache{
		accounts: map[int64]*Account{},
	}
	schedulerSnapshot := NewSchedulerSnapshotService(cache, nil, stubOpenAIAccountRepo{}, nil, nil)
	svc := &OpenAIGatewayService{
		schedulerSnapshot: schedulerSnapshot,
	}
	releaseCalls := 0

	selection, err := svc.newAcquiredSelectionResult(context.Background(), nil, &Account{ID: 1001}, func() {
		releaseCalls++
	})

	if err == nil {
		t.Fatalf("expected hydration error")
	}
	if selection != nil {
		t.Fatalf("expected nil selection on hydration error")
	}
	if releaseCalls != 1 {
		t.Fatalf("expected release to be called once, got %d", releaseCalls)
	}
}

func TestGatewayHydrateSelectedAccount_UsesRequestBucketOnColdGlobalCache(t *testing.T) {
	groupID := int64(77)
	full := &Account{
		ID:          9,
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{"api_key": "bucket-key"},
	}
	cache := &snapshotHydrationCache{
		accounts:       map[int64]*Account{},
		staticAccounts: map[int64]*Account{full.ID: full},
	}
	svc := &GatewayService{schedulerSnapshot: NewSchedulerSnapshotService(cache, nil, nil, nil, nil)}

	hydrated, err := svc.hydrateSelectedAccount(context.Background(), &groupID, PlatformAnthropic, false, &Account{ID: full.ID})
	if err != nil {
		t.Fatalf("hydrateSelectedAccount error: %v", err)
	}
	if hydrated == nil || hydrated.GetCredential("api_key") != "bucket-key" {
		t.Fatalf("expected bucket-hydrated account, got %#v", hydrated)
	}
	if cache.getAccountCalls != 0 {
		t.Fatalf("global account cache must not be read, got %d calls", cache.getAccountCalls)
	}
	wantBucket := (SchedulerBucket{GroupID: groupID, Platform: PlatformAnthropic, Mode: SchedulerModeMixed})
	if cache.lastStaticBucket != wantBucket {
		t.Fatalf("unexpected static bucket: got %+v want %+v", cache.lastStaticBucket, wantBucket)
	}
}

func TestGeminiHydrateSelectedAccount_UsesForcedRequestBucketOnColdGlobalCache(t *testing.T) {
	groupID := int64(78)
	full := &Account{
		ID:          10,
		Platform:    PlatformGemini,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{"api_key": "gemini-bucket-key"},
	}
	cache := &snapshotHydrationCache{
		accounts:       map[int64]*Account{},
		staticAccounts: map[int64]*Account{full.ID: full},
	}
	svc := &GeminiMessagesCompatService{schedulerSnapshot: NewSchedulerSnapshotService(cache, nil, nil, nil, nil)}

	hydrated, err := svc.hydrateSelectedAccount(context.Background(), &groupID, PlatformGemini, true, &Account{ID: full.ID})
	if err != nil {
		t.Fatalf("hydrateSelectedAccount error: %v", err)
	}
	if hydrated == nil || hydrated.GetCredential("api_key") != "gemini-bucket-key" {
		t.Fatalf("expected bucket-hydrated account, got %#v", hydrated)
	}
	if cache.getAccountCalls != 0 {
		t.Fatalf("global account cache must not be read, got %d calls", cache.getAccountCalls)
	}
	wantBucket := (SchedulerBucket{GroupID: groupID, Platform: PlatformGemini, Mode: SchedulerModeForced})
	if cache.lastStaticBucket != wantBucket {
		t.Fatalf("unexpected static bucket: got %+v want %+v", cache.lastStaticBucket, wantBucket)
	}
}

func TestOpenAIPrivacyRequirement_UsesBucketAccountOnColdGlobalCache(t *testing.T) {
	groupID := int64(79)
	full := &Account{
		ID:          11,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Extra:       map[string]any{"privacy_mode": PrivacyModeTrainingOff},
	}
	cache := &snapshotHydrationCache{
		accounts:       map[int64]*Account{},
		staticAccounts: map[int64]*Account{full.ID: full},
	}
	svc := &OpenAIGatewayService{schedulerSnapshot: NewSchedulerSnapshotService(cache, nil, nil, nil, nil)}
	group := &Group{ID: groupID, Platform: PlatformOpenAI, RequirePrivacySet: true}

	hydrated, ok := svc.resolveOpenAIAccountForPrivacyRequirement(context.Background(), &groupID, &Account{ID: full.ID, Platform: PlatformOpenAI, Type: AccountTypeOAuth}, group)
	if !ok || hydrated == nil || !hydrated.IsPrivacySet() {
		t.Fatalf("expected privacy-qualified bucket account, got %#v", hydrated)
	}
	if cache.getAccountCalls != 0 {
		t.Fatalf("global account cache must not be read, got %d calls", cache.getAccountCalls)
	}
}

func TestOpenAIPreviousResponseSelection_DoesNotReadGlobalAccountAfterBucketHydration(t *testing.T) {
	groupID := int64(80)
	full := &Account{
		ID:          12,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Extra: map[string]any{
			"openai_oauth_responses_websockets_v2_enabled": true,
			"privacy_mode": PrivacyModeTrainingOff,
		},
	}
	cache := &snapshotHydrationCache{
		accounts:       map[int64]*Account{},
		staticAccounts: map[int64]*Account{full.ID: full},
	}
	svc := &OpenAIGatewayService{schedulerSnapshot: NewSchedulerSnapshotService(cache, nil, nil, nil, nil)}

	selected := svc.refreshSelectedOpenAIAccountFromSchedulerCache(context.Background(), &groupID, &Account{ID: full.ID})
	if selected == nil {
		t.Fatal("expected bucket-hydrated account")
	}
	selected, ok := svc.resolveOpenAIAccountForPrivacyRequirement(context.Background(), &groupID, selected, &Group{ID: groupID, Platform: PlatformOpenAI, RequirePrivacySet: true})
	if !ok || selected == nil {
		t.Fatal("expected privacy-qualified account")
	}
	if cache.getAccountCalls != 0 {
		t.Fatalf("global account cache must not be read, got %d calls", cache.getAccountCalls)
	}
}

func TestGatewaySelectAccountWithLoadAwareness_HydratesSelectedAccountFromSchedulerSnapshot(t *testing.T) {
	cache := &snapshotHydrationCache{
		snapshot: []*Account{
			{
				ID:          9,
				Platform:    PlatformAnthropic,
				Type:        AccountTypeAPIKey,
				Status:      StatusActive,
				Schedulable: true,
				Concurrency: 1,
				Priority:    1,
			},
		},
		accounts: map[int64]*Account{
			9: {
				ID:          9,
				Platform:    PlatformAnthropic,
				Type:        AccountTypeAPIKey,
				Status:      StatusActive,
				Schedulable: true,
				Concurrency: 1,
				Priority:    1,
				Credentials: map[string]any{
					"api_key": "anthropic-live-key",
				},
			},
		},
	}

	schedulerSnapshot := NewSchedulerSnapshotService(cache, nil, nil, nil, nil)
	svc := &GatewayService{
		schedulerSnapshot: schedulerSnapshot,
		cache:             &mockGatewayCacheForPlatform{},
		cfg:               testConfig(),
	}

	result, err := svc.SelectAccountWithLoadAwareness(context.Background(), nil, "", "claude-3-5-sonnet-20241022", nil, "", 0)
	if err != nil {
		t.Fatalf("SelectAccountWithLoadAwareness error: %v", err)
	}
	if result == nil || result.Account == nil {
		t.Fatalf("expected selected account")
	}
	if got := result.Account.GetCredential("api_key"); got != "anthropic-live-key" {
		t.Fatalf("expected hydrated api key, got %q", got)
	}
}

func TestGatewaySelectAccountWithLoadAwareness_SkipsAntigravityGeminiFamilyRateLimitedSnapshot(t *testing.T) {
	resetAt := time.Now().Add(10 * time.Minute).Format(time.RFC3339)
	cache := &snapshotHydrationCache{
		snapshot: []*Account{
			{
				ID:          1,
				Platform:    PlatformAntigravity,
				Type:        AccountTypeOAuth,
				Status:      StatusActive,
				Schedulable: true,
				Concurrency: 1,
				Priority:    1,
				AccountGroups: []AccountGroup{
					{AccountID: 1, GroupID: 22},
				},
				GroupIDs: []int64{22},
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
				Type:        AccountTypeOAuth,
				Status:      StatusActive,
				Schedulable: true,
				Concurrency: 1,
				Priority:    2,
				AccountGroups: []AccountGroup{
					{AccountID: 2, GroupID: 22},
				},
				GroupIDs: []int64{22},
				Extra: map[string]any{
					"mixed_scheduling": true,
				},
			},
		},
		accounts: map[int64]*Account{
			1: {
				ID: 1, Platform: PlatformAntigravity, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1,
				GroupIDs: []int64{22}, Extra: map[string]any{"mixed_scheduling": true, modelRateLimitsKey: map[string]any{antigravityGeminiModelRateLimitKey: map[string]any{"rate_limit_reset_at": resetAt}}},
			},
			2: {ID: 2, Platform: PlatformAntigravity, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, GroupIDs: []int64{22}, Extra: map[string]any{"mixed_scheduling": true}},
		},
	}
	groupID := int64(22)
	svc := &GatewayService{
		schedulerSnapshot: NewSchedulerSnapshotService(cache, nil, nil, nil, nil),
		groupRepo: &mockGroupRepoForGateway{
			groups: map[int64]*Group{
				groupID: {
					ID:       groupID,
					Platform: PlatformGemini,
					Status:   StatusActive,
					Hydrated: true,
				},
			},
		},
		concurrencyService: NewConcurrencyService(&mockConcurrencyCache{}),
		cfg: &config.Config{
			Gateway: config.GatewayConfig{
				Scheduling: config.GatewaySchedulingConfig{
					LoadBatchEnabled:         true,
					StickySessionMaxWaiting:  3,
					StickySessionWaitTimeout: time.Second,
					FallbackWaitTimeout:      time.Second,
					FallbackMaxWaiting:       10,
				},
			},
		},
	}

	result, err := svc.SelectAccountWithLoadAwareness(context.Background(), &groupID, "", "gemini-3-flash-preview", nil, "", 0)
	if err != nil {
		t.Fatalf("SelectAccountWithLoadAwareness error: %v", err)
	}
	if result == nil || result.Account == nil {
		t.Fatalf("expected selected account")
	}
	if result.Account.ID != 2 {
		t.Fatalf("expected scheduler to skip Gemini-family limited antigravity account 1, got %d", result.Account.ID)
	}
}
