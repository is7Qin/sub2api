//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/stretchr/testify/require"
)

type quotaStateRepoStub struct {
	quotaBaseAPIKeyRepoStub
	stateCalls int
	state      *APIKeyQuotaUsageState
	stateErr   error
}

func (s *quotaStateRepoStub) IncrementQuotaUsedAndGetState(ctx context.Context, id int64, amount float64) (*APIKeyQuotaUsageState, error) {
	s.stateCalls++
	if s.stateErr != nil {
		return nil, s.stateErr
	}
	if s.state == nil {
		return nil, nil
	}
	out := *s.state
	return &out, nil
}

type quotaStateCacheStub struct {
	deleteAuthKeys []string
}

func (s *quotaStateCacheStub) GetCreateAttemptCount(context.Context, int64) (int, error) {
	return 0, nil
}

func (s *quotaStateCacheStub) IncrementCreateAttemptCount(context.Context, int64) error {
	return nil
}

func (s *quotaStateCacheStub) DeleteCreateAttemptCount(context.Context, int64) error {
	return nil
}

func (s *quotaStateCacheStub) IncrementDailyUsage(context.Context, string) error {
	return nil
}

func (s *quotaStateCacheStub) SetDailyUsageExpiry(context.Context, string, time.Duration) error {
	return nil
}

func (s *quotaStateCacheStub) GetAuthCache(context.Context, string) (*APIKeyAuthCacheEntry, error) {
	return nil, nil
}

func (s *quotaStateCacheStub) SetAuthCache(context.Context, string, *APIKeyAuthCacheEntry, time.Duration) error {
	return nil
}

func (s *quotaStateCacheStub) DeleteAuthCache(_ context.Context, key string) error {
	s.deleteAuthKeys = append(s.deleteAuthKeys, key)
	return nil
}

func (s *quotaStateCacheStub) PublishAuthCacheInvalidation(context.Context, string) error {
	return nil
}

func (s *quotaStateCacheStub) SubscribeAuthCacheInvalidation(context.Context, func(string)) error {
	return nil
}

type quotaBaseAPIKeyRepoStub struct {
	getByIDCalls int
}

func (s *quotaBaseAPIKeyRepoStub) Create(context.Context, *APIKey) error {
	panic("unexpected Create call")
}
func (s *quotaBaseAPIKeyRepoStub) GetByID(context.Context, int64) (*APIKey, error) {
	s.getByIDCalls++
	return nil, nil
}
func (s *quotaBaseAPIKeyRepoStub) GetKeyAndOwnerID(context.Context, int64) (string, int64, error) {
	panic("unexpected GetKeyAndOwnerID call")
}
func (s *quotaBaseAPIKeyRepoStub) GetByKey(context.Context, string) (*APIKey, error) {
	panic("unexpected GetByKey call")
}
func (s *quotaBaseAPIKeyRepoStub) GetByKeyForAuth(context.Context, string) (*APIKey, error) {
	panic("unexpected GetByKeyForAuth call")
}
func (s *quotaBaseAPIKeyRepoStub) Update(context.Context, *APIKey) error {
	panic("unexpected Update call")
}
func (s *quotaBaseAPIKeyRepoStub) UpdateConfig(context.Context, int64, int64, APIKeyConfigPatch) (*APIKey, error) {
	panic("unexpected UpdateConfig call")
}
func (s *quotaBaseAPIKeyRepoStub) UpdateGroupID(context.Context, int64, *int64) (*APIKey, error) {
	panic("unexpected UpdateGroupID call")
}
func (s *quotaBaseAPIKeyRepoStub) ResetRateLimitUsage(context.Context, int64) (*APIKey, error) {
	panic("unexpected ResetRateLimitUsage call")
}
func (s *quotaBaseAPIKeyRepoStub) Delete(context.Context, int64) error {
	panic("unexpected Delete call")
}
func (s *quotaBaseAPIKeyRepoStub) DeleteWithAudit(context.Context, int64) error {
	panic("unexpected DeleteWithAudit call")
}
func (s *quotaBaseAPIKeyRepoStub) ListByUserID(context.Context, int64, pagination.PaginationParams, APIKeyListFilters) ([]APIKey, *pagination.PaginationResult, error) {
	panic("unexpected ListByUserID call")
}
func (s *quotaBaseAPIKeyRepoStub) VerifyOwnership(context.Context, int64, []int64) ([]int64, error) {
	panic("unexpected VerifyOwnership call")
}
func (s *quotaBaseAPIKeyRepoStub) CountByUserID(context.Context, int64) (int64, error) {
	panic("unexpected CountByUserID call")
}
func (s *quotaBaseAPIKeyRepoStub) ExistsByKey(context.Context, string) (bool, error) {
	panic("unexpected ExistsByKey call")
}
func (s *quotaBaseAPIKeyRepoStub) ListByGroupID(context.Context, int64, pagination.PaginationParams) ([]APIKey, *pagination.PaginationResult, error) {
	panic("unexpected ListByGroupID call")
}
func (s *quotaBaseAPIKeyRepoStub) SearchAPIKeys(context.Context, int64, string, int) ([]APIKey, error) {
	panic("unexpected SearchAPIKeys call")
}
func (s *quotaBaseAPIKeyRepoStub) ClearGroupIDByGroupID(context.Context, int64) (int64, error) {
	panic("unexpected ClearGroupIDByGroupID call")
}
func (s *quotaBaseAPIKeyRepoStub) UpdateGroupIDByUserAndGroup(context.Context, int64, int64, int64) (int64, error) {
	panic("unexpected UpdateGroupIDByUserAndGroup call")
}
func (s *quotaBaseAPIKeyRepoStub) CountByGroupID(context.Context, int64) (int64, error) {
	panic("unexpected CountByGroupID call")
}
func (s *quotaBaseAPIKeyRepoStub) ListKeysByUserID(context.Context, int64) ([]string, error) {
	panic("unexpected ListKeysByUserID call")
}
func (s *quotaBaseAPIKeyRepoStub) ListKeysByGroupID(context.Context, int64) ([]string, error) {
	panic("unexpected ListKeysByGroupID call")
}
func (s *quotaBaseAPIKeyRepoStub) IncrementQuotaUsed(context.Context, int64, float64) (float64, error) {
	panic("unexpected IncrementQuotaUsed call")
}
func (s *quotaBaseAPIKeyRepoStub) IncrementQuotaUsedAndGetState(context.Context, int64, float64) (*APIKeyQuotaUsageState, error) {
	panic("unexpected IncrementQuotaUsedAndGetState call")
}
func (s *quotaBaseAPIKeyRepoStub) UpdateLastUsed(context.Context, int64, time.Time) error {
	panic("unexpected UpdateLastUsed call")
}
func (s *quotaBaseAPIKeyRepoStub) IncrementRateLimitUsage(context.Context, int64, float64) error {
	panic("unexpected IncrementRateLimitUsage call")
}
func (s *quotaBaseAPIKeyRepoStub) ResetRateLimitWindows(context.Context, int64) error {
	panic("unexpected ResetRateLimitWindows call")
}
func (s *quotaBaseAPIKeyRepoStub) GetRateLimitData(context.Context, int64) (*APIKeyRateLimitData, error) {
	panic("unexpected GetRateLimitData call")
}

type quotaUpdateAPIKeyRepoStub struct {
	quotaBaseAPIKeyRepoStub
	apiKey     *APIKey
	updatedKey *APIKey
	patch      APIKeyConfigPatch
}

func (s *quotaUpdateAPIKeyRepoStub) GetByID(context.Context, int64) (*APIKey, error) {
	if s.apiKey == nil {
		return nil, ErrAPIKeyNotFound
	}
	out := *s.apiKey
	return &out, nil
}

func (s *quotaUpdateAPIKeyRepoStub) Update(_ context.Context, key *APIKey) error {
	out := *key
	s.updatedKey = &out
	return nil
}
func (s *quotaUpdateAPIKeyRepoStub) UpdateConfig(_ context.Context, _ int64, _ int64, p APIKeyConfigPatch) (*APIKey, error) {
	s.patch = p
	out := *s.apiKey
	if p.Name != nil {
		out.Name = *p.Name
	}
	if p.Quota != nil {
		out.Quota = *p.Quota
	}
	if p.Concurrency != nil {
		out.Concurrency = *p.Concurrency
	}
	if p.ExpiresAt != nil {
		out.ExpiresAt = *p.ExpiresAt
	}
	if p.Status != nil {
		out.Status = *p.Status
	}
	if p.ResetQuota {
		out.QuotaUsed = 0
	}
	out.Status = reconcileAPIKeyTerminalStatus(&out)
	s.updatedKey = &out
	return &out, nil
}

func TestAPIKeyService_UpdateEscapesNameInConfigPatch(t *testing.T) {
	repo := &quotaUpdateAPIKeyRepoStub{apiKey: &APIKey{
		ID: 1, UserID: 2, Key: "sk-test", Name: "before", Status: StatusAPIKeyActive,
	}}
	svc := &APIKeyService{apiKeyRepo: repo}
	name := `<script>alert("xss")</script>`

	updated, err := svc.Update(context.Background(), 1, 2, UpdateAPIKeyRequest{Name: &name})
	require.NoError(t, err)
	require.NotNil(t, repo.patch.Name)
	require.Equal(t, `&lt;script&gt;alert(&#34;xss&#34;)&lt;/script&gt;`, *repo.patch.Name)
	require.Equal(t, *repo.patch.Name, updated.Name)
}

func TestAPIKeyService_UpdatePreservesConcurrencyWhenOmitted(t *testing.T) {
	repo := &quotaUpdateAPIKeyRepoStub{apiKey: &APIKey{
		ID: 1, UserID: 2, Key: "sk-test", Status: StatusAPIKeyActive, Concurrency: 7,
	}}
	svc := &APIKeyService{apiKeyRepo: repo}

	updated, err := svc.Update(context.Background(), 1, 2, UpdateAPIKeyRequest{})
	require.NoError(t, err)
	require.Nil(t, repo.patch.Concurrency)
	require.Equal(t, 7, updated.Concurrency)
}

func TestAPIKeyService_UpdateCanDisableConcurrency(t *testing.T) {
	repo := &quotaUpdateAPIKeyRepoStub{apiKey: &APIKey{
		ID: 1, UserID: 2, Key: "sk-test", Status: StatusAPIKeyActive, Concurrency: 7,
	}}
	svc := &APIKeyService{apiKeyRepo: repo}
	concurrency := 0

	updated, err := svc.Update(context.Background(), 1, 2, UpdateAPIKeyRequest{Concurrency: &concurrency})
	require.NoError(t, err)
	require.NotNil(t, repo.patch.Concurrency)
	require.Zero(t, *repo.patch.Concurrency)
	require.Zero(t, updated.Concurrency)
}

func TestAPIKeyService_UpdateRejectsNegativeConcurrency(t *testing.T) {
	repo := &quotaUpdateAPIKeyRepoStub{apiKey: &APIKey{
		ID: 1, UserID: 2, Key: "sk-test", Status: StatusAPIKeyActive, Concurrency: 7,
	}}
	svc := &APIKeyService{apiKeyRepo: repo}
	concurrency := -1

	_, err := svc.Update(context.Background(), 1, 2, UpdateAPIKeyRequest{Concurrency: &concurrency})
	require.ErrorIs(t, err, ErrInvalidAPIKeyConcurrency)
	require.Nil(t, repo.patch.Concurrency)
}

func TestAPIKeyService_UpdatePreservesIPRulesWhenOmitted(t *testing.T) {
	repo := &quotaUpdateAPIKeyRepoStub{apiKey: &APIKey{
		ID:          1,
		UserID:      2,
		Key:         "sk-test",
		Status:      StatusAPIKeyActive,
		IPWhitelist: []string{"192.0.2.1"},
		IPBlacklist: []string{"198.51.100.1"},
	}}
	svc := &APIKeyService{apiKeyRepo: repo}

	_, err := svc.Update(context.Background(), 1, 2, UpdateAPIKeyRequest{})
	require.NoError(t, err)
	require.Nil(t, repo.patch.IPWhitelist)
	require.Nil(t, repo.patch.IPBlacklist)
}

func TestAPIKeyService_UpdateCanClearIPRules(t *testing.T) {
	repo := &quotaUpdateAPIKeyRepoStub{apiKey: &APIKey{
		ID:          1,
		UserID:      2,
		Key:         "sk-test",
		Status:      StatusAPIKeyActive,
		IPWhitelist: []string{"192.0.2.1"},
		IPBlacklist: []string{"198.51.100.1"},
	}}
	svc := &APIKeyService{apiKeyRepo: repo}
	emptyWhitelist := []string{}
	emptyBlacklist := []string{}

	_, err := svc.Update(context.Background(), 1, 2, UpdateAPIKeyRequest{
		IPWhitelist: &emptyWhitelist,
		IPBlacklist: &emptyBlacklist,
	})
	require.NoError(t, err)
	require.NotNil(t, repo.patch.IPWhitelist)
	require.NotNil(t, repo.patch.IPBlacklist)
	require.Empty(t, *repo.patch.IPWhitelist)
	require.Empty(t, *repo.patch.IPBlacklist)
}

func TestAPIKeyService_UpdateValidatesProvidedIPRules(t *testing.T) {
	repo := &quotaUpdateAPIKeyRepoStub{apiKey: &APIKey{
		ID: 1, UserID: 2, Key: "sk-test", Status: StatusAPIKeyActive,
	}}
	svc := &APIKeyService{apiKeyRepo: repo}
	invalid := []string{"not-an-ip"}

	_, err := svc.Update(context.Background(), 1, 2, UpdateAPIKeyRequest{IPWhitelist: &invalid})
	require.ErrorIs(t, err, ErrInvalidIPPattern)
}

func TestAPIKeyService_UpdateQuotaUsed_UsesAtomicStatePath(t *testing.T) {
	repo := &quotaStateRepoStub{
		state: &APIKeyQuotaUsageState{
			QuotaUsed: 12,
			Quota:     10,
			Key:       "sk-test-quota",
			Status:    StatusAPIKeyQuotaExhausted,
		},
	}
	cache := &quotaStateCacheStub{}
	svc := &APIKeyService{
		apiKeyRepo: repo,
		cache:      cache,
	}

	err := svc.UpdateQuotaUsed(context.Background(), 101, 2)
	require.NoError(t, err)
	require.Equal(t, 1, repo.stateCalls)
	require.Equal(t, 0, repo.getByIDCalls, "fast path should not re-read API key by id")
	require.Equal(t, []string{svc.authCacheKey("sk-test-quota")}, cache.deleteAuthKeys)
}

func TestAPIKeyService_Update_ReactivatesQuotaExhaustedWhenQuotaUnlimited(t *testing.T) {
	repo := &quotaUpdateAPIKeyRepoStub{
		apiKey: &APIKey{
			ID:        10,
			UserID:    7,
			Key:       "sk-test-unlimited",
			Status:    StatusAPIKeyQuotaExhausted,
			Quota:     10,
			QuotaUsed: 12,
		},
	}
	svc := &APIKeyService{apiKeyRepo: repo}
	quota := 0.0

	updated, err := svc.Update(context.Background(), 10, 7, UpdateAPIKeyRequest{Quota: &quota})

	require.NoError(t, err)
	require.Equal(t, StatusActive, updated.Status)
	require.Equal(t, 0.0, updated.Quota)
	require.NotNil(t, repo.updatedKey)
	require.Equal(t, StatusActive, repo.updatedKey.Status)
	require.Equal(t, 0.0, repo.updatedKey.Quota)
}

func TestAPIKeyService_Update_StatusActiveKeepsQuotaExhaustedWhenQuotaStillExhausted(t *testing.T) {
	repo := &quotaUpdateAPIKeyRepoStub{
		apiKey: &APIKey{
			ID:        11,
			UserID:    7,
			Key:       "sk-test-still-exhausted",
			Status:    StatusAPIKeyQuotaExhausted,
			Quota:     10,
			QuotaUsed: 12,
		},
	}
	svc := &APIKeyService{apiKeyRepo: repo}
	status := StatusActive

	updated, err := svc.Update(context.Background(), 11, 7, UpdateAPIKeyRequest{Status: &status})

	require.NoError(t, err)
	require.Equal(t, StatusAPIKeyQuotaExhausted, updated.Status)
	require.NotNil(t, repo.updatedKey)
	require.Equal(t, StatusAPIKeyQuotaExhausted, repo.updatedKey.Status)
}

func TestAPIKeyService_Update_StatusActiveKeepsExpiredWhenExpiryStillExpired(t *testing.T) {
	expiredAt := time.Now().Add(-time.Hour)
	repo := &quotaUpdateAPIKeyRepoStub{
		apiKey: &APIKey{
			ID:        12,
			UserID:    7,
			Key:       "sk-test-still-expired",
			Status:    StatusAPIKeyExpired,
			ExpiresAt: &expiredAt,
		},
	}
	svc := &APIKeyService{apiKeyRepo: repo}
	status := StatusActive

	updated, err := svc.Update(context.Background(), 12, 7, UpdateAPIKeyRequest{Status: &status})

	require.NoError(t, err)
	require.Equal(t, StatusAPIKeyExpired, updated.Status)
	require.NotNil(t, repo.updatedKey)
	require.Equal(t, StatusAPIKeyExpired, repo.updatedKey.Status)
}

func TestAPIKeyService_Update_StatusActiveReactivatesQuotaExhaustedWhenQuotaUnlimited(t *testing.T) {
	repo := &quotaUpdateAPIKeyRepoStub{
		apiKey: &APIKey{
			ID:        13,
			UserID:    7,
			Key:       "sk-test-active-unlimited",
			Status:    StatusAPIKeyQuotaExhausted,
			Quota:     10,
			QuotaUsed: 12,
		},
	}
	svc := &APIKeyService{apiKeyRepo: repo}
	status := StatusActive
	quota := 0.0

	updated, err := svc.Update(context.Background(), 13, 7, UpdateAPIKeyRequest{Status: &status, Quota: &quota})

	require.NoError(t, err)
	require.Equal(t, StatusActive, updated.Status)
	require.Equal(t, 0.0, updated.Quota)
	require.NotNil(t, repo.updatedKey)
	require.Equal(t, StatusActive, repo.updatedKey.Status)
}

func TestAPIKeyService_Update_StatusActiveReactivatesExpiredWhenExpirationCleared(t *testing.T) {
	expiredAt := time.Now().Add(-time.Hour)
	repo := &quotaUpdateAPIKeyRepoStub{
		apiKey: &APIKey{
			ID:        14,
			UserID:    7,
			Key:       "sk-test-active-clear-expiry",
			Status:    StatusAPIKeyExpired,
			ExpiresAt: &expiredAt,
		},
	}
	svc := &APIKeyService{apiKeyRepo: repo}
	status := StatusActive

	updated, err := svc.Update(context.Background(), 14, 7, UpdateAPIKeyRequest{Status: &status, ClearExpiration: true})

	require.NoError(t, err)
	require.Equal(t, StatusActive, updated.Status)
	require.Nil(t, updated.ExpiresAt)
	require.NotNil(t, repo.updatedKey)
	require.Equal(t, StatusActive, repo.updatedKey.Status)
}

func TestAPIKeyService_Update_QuotaUnlimitedKeepsExpiredWhenExpiryStillPast(t *testing.T) {
	expiredAt := time.Now().Add(-time.Hour)
	repo := &quotaUpdateAPIKeyRepoStub{
		apiKey: &APIKey{
			ID:        15,
			UserID:    7,
			Key:       "sk-test-unlimited-still-expired",
			Status:    StatusAPIKeyQuotaExhausted,
			Quota:     10,
			QuotaUsed: 12,
			ExpiresAt: &expiredAt,
		},
	}
	svc := &APIKeyService{apiKeyRepo: repo}
	quota := 0.0

	updated, err := svc.Update(context.Background(), 15, 7, UpdateAPIKeyRequest{Quota: &quota})

	require.NoError(t, err)
	require.Equal(t, StatusAPIKeyExpired, updated.Status)
	require.Equal(t, 0.0, updated.Quota)
	require.NotNil(t, repo.updatedKey)
	require.Equal(t, StatusAPIKeyExpired, repo.updatedKey.Status)
}

func TestAPIKeyService_Update_ClearExpirationKeepsQuotaExhaustedWhenQuotaStillExhausted(t *testing.T) {
	expiredAt := time.Now().Add(-time.Hour)
	repo := &quotaUpdateAPIKeyRepoStub{
		apiKey: &APIKey{
			ID:        16,
			UserID:    7,
			Key:       "sk-test-clear-expiry-still-exhausted",
			Status:    StatusAPIKeyExpired,
			Quota:     10,
			QuotaUsed: 12,
			ExpiresAt: &expiredAt,
		},
	}
	svc := &APIKeyService{apiKeyRepo: repo}

	updated, err := svc.Update(context.Background(), 16, 7, UpdateAPIKeyRequest{ClearExpiration: true})

	require.NoError(t, err)
	require.Equal(t, StatusAPIKeyQuotaExhausted, updated.Status)
	require.Nil(t, updated.ExpiresAt)
	require.NotNil(t, repo.updatedKey)
	require.Equal(t, StatusAPIKeyQuotaExhausted, repo.updatedKey.Status)
}

func TestAPIKeyService_Update_StatusActiveWithOneConditionFixedKeepsOtherTerminalStatus(t *testing.T) {
	t.Run("quota fixed but expiry still past remains expired", func(t *testing.T) {
		expiredAt := time.Now().Add(-time.Hour)
		repo := &quotaUpdateAPIKeyRepoStub{
			apiKey: &APIKey{
				ID:        17,
				UserID:    7,
				Key:       "sk-test-active-fix-quota-only",
				Status:    StatusAPIKeyQuotaExhausted,
				Quota:     10,
				QuotaUsed: 12,
				ExpiresAt: &expiredAt,
			},
		}
		svc := &APIKeyService{apiKeyRepo: repo}
		status := StatusActive
		quota := 0.0

		updated, err := svc.Update(context.Background(), 17, 7, UpdateAPIKeyRequest{Status: &status, Quota: &quota})

		require.NoError(t, err)
		require.Equal(t, StatusAPIKeyExpired, updated.Status)
		require.NotNil(t, repo.updatedKey)
		require.Equal(t, StatusAPIKeyExpired, repo.updatedKey.Status)
	})

	t.Run("expiry fixed but quota still exhausted remains quota exhausted", func(t *testing.T) {
		expiredAt := time.Now().Add(-time.Hour)
		futureAt := time.Now().Add(time.Hour)
		repo := &quotaUpdateAPIKeyRepoStub{
			apiKey: &APIKey{
				ID:        18,
				UserID:    7,
				Key:       "sk-test-active-fix-expiry-only",
				Status:    StatusAPIKeyExpired,
				Quota:     10,
				QuotaUsed: 12,
				ExpiresAt: &expiredAt,
			},
		}
		svc := &APIKeyService{apiKeyRepo: repo}
		status := StatusActive

		updated, err := svc.Update(context.Background(), 18, 7, UpdateAPIKeyRequest{Status: &status, ExpiresAt: &futureAt})

		require.NoError(t, err)
		require.Equal(t, StatusAPIKeyQuotaExhausted, updated.Status)
		require.NotNil(t, repo.updatedKey)
		require.Equal(t, StatusAPIKeyQuotaExhausted, repo.updatedKey.Status)
	})
}

func TestAPIKeyService_Update_StatusInactiveKeepsExpiredWhenExpiryStillExpired(t *testing.T) {
	expiredAt := time.Now().Add(-time.Hour)
	repo := &quotaUpdateAPIKeyRepoStub{
		apiKey: &APIKey{
			ID:        19,
			UserID:    7,
			Key:       "sk-test-inactive-still-expired",
			Status:    StatusAPIKeyExpired,
			ExpiresAt: &expiredAt,
		},
	}
	svc := &APIKeyService{apiKeyRepo: repo}
	status := "inactive"

	updated, err := svc.Update(context.Background(), 19, 7, UpdateAPIKeyRequest{Status: &status})

	require.NoError(t, err)
	require.Equal(t, StatusAPIKeyExpired, updated.Status)
	require.NotNil(t, repo.updatedKey)
	require.Equal(t, StatusAPIKeyExpired, repo.updatedKey.Status)
}

func TestAPIKeyService_Update_StatusInactiveKeepsQuotaExhaustedWhenQuotaStillExhausted(t *testing.T) {
	repo := &quotaUpdateAPIKeyRepoStub{
		apiKey: &APIKey{
			ID:        20,
			UserID:    7,
			Key:       "sk-test-inactive-still-exhausted",
			Status:    StatusAPIKeyQuotaExhausted,
			Quota:     10,
			QuotaUsed: 12,
		},
	}
	svc := &APIKeyService{apiKeyRepo: repo}
	status := "inactive"

	updated, err := svc.Update(context.Background(), 20, 7, UpdateAPIKeyRequest{Status: &status})

	require.NoError(t, err)
	require.Equal(t, StatusAPIKeyQuotaExhausted, updated.Status)
	require.NotNil(t, repo.updatedKey)
	require.Equal(t, StatusAPIKeyQuotaExhausted, repo.updatedKey.Status)
}

func TestAPIKeyService_Update_StatusInactiveRemainsInactiveWithoutTerminalCondition(t *testing.T) {
	repo := &quotaUpdateAPIKeyRepoStub{
		apiKey: &APIKey{
			ID:     21,
			UserID: 7,
			Key:    "sk-test-inactive-no-terminal",
			Status: StatusActive,
		},
	}
	svc := &APIKeyService{apiKeyRepo: repo}
	status := "inactive"

	updated, err := svc.Update(context.Background(), 21, 7, UpdateAPIKeyRequest{Status: &status})

	require.NoError(t, err)
	require.Equal(t, "inactive", updated.Status)
	require.NotNil(t, repo.updatedKey)
	require.Equal(t, "inactive", repo.updatedKey.Status)
}
