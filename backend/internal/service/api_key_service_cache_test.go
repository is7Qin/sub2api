//go:build unit

package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type authRepoStub struct {
	getByKeyForAuth   func(ctx context.Context, key string) (*APIKey, error)
	listByUserID      func(ctx context.Context, userID int64, params pagination.PaginationParams, filters APIKeyListFilters) ([]APIKey, *pagination.PaginationResult, error)
	listKeysByUserID  func(ctx context.Context, userID int64) ([]string, error)
	listKeysByGroupID func(ctx context.Context, groupID int64) ([]string, error)
}

func (s *authRepoStub) Create(ctx context.Context, key *APIKey) error {
	panic("unexpected Create call")
}

func (s *authRepoStub) GetByID(ctx context.Context, id int64) (*APIKey, error) {
	panic("unexpected GetByID call")
}

func (s *authRepoStub) GetKeyAndOwnerID(ctx context.Context, id int64) (string, int64, error) {
	panic("unexpected GetKeyAndOwnerID call")
}

func (s *authRepoStub) GetByKey(ctx context.Context, key string) (*APIKey, error) {
	panic("unexpected GetByKey call")
}

func (s *authRepoStub) GetByKeyForAuth(ctx context.Context, key string) (*APIKey, error) {
	if s.getByKeyForAuth == nil {
		panic("unexpected GetByKeyForAuth call")
	}
	return s.getByKeyForAuth(ctx, key)
}

func (s *authRepoStub) Update(ctx context.Context, key *APIKey) error {
	panic("unexpected Update call")
}
func (s *authRepoStub) UpdateConfig(context.Context, int64, int64, APIKeyConfigPatch) (*APIKey, error) {
	panic("unexpected UpdateConfig call")
}
func (s *authRepoStub) UpdateGroupID(context.Context, int64, *int64) (*APIKey, error) {
	panic("unexpected UpdateGroupID call")
}
func (s *authRepoStub) ResetRateLimitUsage(context.Context, int64) (*APIKey, error) {
	panic("unexpected ResetRateLimitUsage call")
}
func (s *authRepoStub) IncrementQuotaUsedAndGetState(context.Context, int64, float64) (*APIKeyQuotaUsageState, error) {
	panic("unexpected IncrementQuotaUsedAndGetState call")
}

func (s *authRepoStub) Delete(ctx context.Context, id int64) error {
	panic("unexpected Delete call")
}

func (s *authRepoStub) DeleteWithAudit(ctx context.Context, id int64) error {
	panic("unexpected DeleteWithAudit call")
}

func (s *authRepoStub) ListByUserID(ctx context.Context, userID int64, params pagination.PaginationParams, filters APIKeyListFilters) ([]APIKey, *pagination.PaginationResult, error) {
	if s.listByUserID == nil {
		panic("unexpected ListByUserID call")
	}
	return s.listByUserID(ctx, userID, params, filters)
}

type apiKeyConcurrencyBatchStub struct {
	counts map[int64]int
	err    error
	ids    []int64
}

func (s *apiKeyConcurrencyBatchStub) GetAPIKeyConcurrencyBatch(_ context.Context, ids []int64) (map[int64]int, error) {
	s.ids = append([]int64(nil), ids...)
	return s.counts, s.err
}

func TestAPIKeyService_ListHydratesEnabledKeyConcurrencyInOneBatch(t *testing.T) {
	repo := &authRepoStub{listByUserID: func(context.Context, int64, pagination.PaginationParams, APIKeyListFilters) ([]APIKey, *pagination.PaginationResult, error) {
		return []APIKey{{ID: 1, Concurrency: 5}, {ID: 2, Concurrency: 0}, {ID: 3, Concurrency: 7}}, &pagination.PaginationResult{Total: 3}, nil
	}}
	reader := &apiKeyConcurrencyBatchStub{counts: map[int64]int{1: 2, 3: 0}}
	svc := NewAPIKeyService(repo, nil, nil, nil, nil, nil, &config.Config{})
	svc.SetAPIKeyConcurrencyBatchReader(reader)

	keys, _, err := svc.List(context.Background(), 9, pagination.PaginationParams{}, APIKeyListFilters{})
	require.NoError(t, err)
	require.Equal(t, []int64{1, 3}, reader.ids)
	require.NotNil(t, keys[0].CurrentConcurrency)
	require.Equal(t, 2, *keys[0].CurrentConcurrency)
	require.Nil(t, keys[1].CurrentConcurrency)
	require.NotNil(t, keys[2].CurrentConcurrency)
	require.Zero(t, *keys[2].CurrentConcurrency)
}

func TestAPIKeyService_ListDegradesWhenConcurrencyUnavailable(t *testing.T) {
	repo := &authRepoStub{listByUserID: func(context.Context, int64, pagination.PaginationParams, APIKeyListFilters) ([]APIKey, *pagination.PaginationResult, error) {
		return []APIKey{{ID: 1, Concurrency: 5}}, &pagination.PaginationResult{Total: 1}, nil
	}}
	svc := NewAPIKeyService(repo, nil, nil, nil, nil, nil, &config.Config{})
	svc.SetAPIKeyConcurrencyBatchReader(&apiKeyConcurrencyBatchStub{err: errors.New("redis unavailable")})

	keys, _, err := svc.List(context.Background(), 9, pagination.PaginationParams{}, APIKeyListFilters{})
	require.NoError(t, err)
	require.Nil(t, keys[0].CurrentConcurrency)
}

func (s *authRepoStub) VerifyOwnership(ctx context.Context, userID int64, apiKeyIDs []int64) ([]int64, error) {
	panic("unexpected VerifyOwnership call")
}

func (s *authRepoStub) CountByUserID(ctx context.Context, userID int64) (int64, error) {
	panic("unexpected CountByUserID call")
}

func (s *authRepoStub) ExistsByKey(ctx context.Context, key string) (bool, error) {
	panic("unexpected ExistsByKey call")
}

func (s *authRepoStub) ListByGroupID(ctx context.Context, groupID int64, params pagination.PaginationParams) ([]APIKey, *pagination.PaginationResult, error) {
	panic("unexpected ListByGroupID call")
}

func (s *authRepoStub) SearchAPIKeys(ctx context.Context, userID int64, keyword string, limit int) ([]APIKey, error) {
	panic("unexpected SearchAPIKeys call")
}

func (s *authRepoStub) ClearGroupIDByGroupID(ctx context.Context, groupID int64) (int64, error) {
	panic("unexpected ClearGroupIDByGroupID call")
}
func (s *authRepoStub) UpdateGroupIDByUserAndGroup(ctx context.Context, userID, oldGroupID, newGroupID int64) (int64, error) {
	panic("unexpected UpdateGroupIDByUserAndGroup call")
}

func (s *authRepoStub) CountByGroupID(ctx context.Context, groupID int64) (int64, error) {
	panic("unexpected CountByGroupID call")
}

func (s *authRepoStub) ListKeysByUserID(ctx context.Context, userID int64) ([]string, error) {
	if s.listKeysByUserID == nil {
		panic("unexpected ListKeysByUserID call")
	}
	return s.listKeysByUserID(ctx, userID)
}

func (s *authRepoStub) ListKeysByGroupID(ctx context.Context, groupID int64) ([]string, error) {
	if s.listKeysByGroupID == nil {
		panic("unexpected ListKeysByGroupID call")
	}
	return s.listKeysByGroupID(ctx, groupID)
}

func (s *authRepoStub) IncrementQuotaUsed(ctx context.Context, id int64, amount float64) (float64, error) {
	panic("unexpected IncrementQuotaUsed call")
}

func (s *authRepoStub) UpdateLastUsed(ctx context.Context, id int64, usedAt time.Time) error {
	panic("unexpected UpdateLastUsed call")
}
func (s *authRepoStub) IncrementRateLimitUsage(ctx context.Context, id int64, cost float64) error {
	panic("unexpected IncrementRateLimitUsage call")
}
func (s *authRepoStub) ResetRateLimitWindows(ctx context.Context, id int64) error {
	panic("unexpected ResetRateLimitWindows call")
}
func (s *authRepoStub) GetRateLimitData(ctx context.Context, id int64) (*APIKeyRateLimitData, error) {
	panic("unexpected GetRateLimitData call")
}

type authCacheStub struct {
	getAuthCache   func(ctx context.Context, key string) (*APIKeyAuthCacheEntry, error)
	setAuthKeys    []string
	deleteAuthKeys []string
}

func (s *authCacheStub) GetCreateAttemptCount(ctx context.Context, userID int64) (int, error) {
	return 0, nil
}

func (s *authCacheStub) IncrementCreateAttemptCount(ctx context.Context, userID int64) error {
	return nil
}

func (s *authCacheStub) DeleteCreateAttemptCount(ctx context.Context, userID int64) error {
	return nil
}

func (s *authCacheStub) IncrementDailyUsage(ctx context.Context, apiKey string) error {
	return nil
}

func (s *authCacheStub) SetDailyUsageExpiry(ctx context.Context, apiKey string, ttl time.Duration) error {
	return nil
}

func (s *authCacheStub) GetAuthCache(ctx context.Context, key string) (*APIKeyAuthCacheEntry, error) {
	if s.getAuthCache == nil {
		return nil, redis.Nil
	}
	return s.getAuthCache(ctx, key)
}

func (s *authCacheStub) SetAuthCache(ctx context.Context, key string, entry *APIKeyAuthCacheEntry, ttl time.Duration) error {
	s.setAuthKeys = append(s.setAuthKeys, key)
	return nil
}

func (s *authCacheStub) DeleteAuthCache(ctx context.Context, key string) error {
	s.deleteAuthKeys = append(s.deleteAuthKeys, key)
	return nil
}

func (s *authCacheStub) PublishAuthCacheInvalidation(ctx context.Context, cacheKey string) error {
	return nil
}

func (s *authCacheStub) SubscribeAuthCacheInvalidation(ctx context.Context, handler func(cacheKey string)) error {
	return nil
}

func TestAPIKeyService_GetByKey_UsesL2Cache(t *testing.T) {
	cache := &authCacheStub{}
	repo := &authRepoStub{
		getByKeyForAuth: func(ctx context.Context, key string) (*APIKey, error) {
			return nil, errors.New("unexpected repo call")
		},
	}
	cfg := &config.Config{
		APIKeyAuth: config.APIKeyAuthCacheConfig{
			L2TTLSeconds:       60,
			NegativeTTLSeconds: 30,
		},
	}
	svc := NewAPIKeyService(repo, nil, nil, nil, nil, cache, cfg)

	groupID := int64(9)
	cacheEntry := &APIKeyAuthCacheEntry{
		Snapshot: &APIKeyAuthSnapshot{
			Version:  apiKeyAuthSnapshotVersion,
			APIKeyID: 1,
			UserID:   2,
			GroupID:  &groupID,
			Status:   StatusActive,
			User: APIKeyAuthUserSnapshot{
				ID:          2,
				Status:      StatusActive,
				Role:        RoleUser,
				Balance:     10,
				Concurrency: 3,
			},
			Group: &APIKeyAuthGroupSnapshot{
				ID:                  groupID,
				Name:                "g",
				Platform:            PlatformAnthropic,
				Status:              StatusActive,
				SubscriptionType:    SubscriptionTypeStandard,
				RateMultiplier:      1,
				ModelRoutingEnabled: true,
				ModelRouting: map[string][]int64{
					"claude-opus-*": {1, 2},
				},
			},
		},
	}
	cache.getAuthCache = func(ctx context.Context, key string) (*APIKeyAuthCacheEntry, error) {
		return cacheEntry, nil
	}

	apiKey, err := svc.GetByKey(context.Background(), "k1")
	require.NoError(t, err)
	require.Equal(t, int64(1), apiKey.ID)
	require.Equal(t, int64(2), apiKey.User.ID)
	require.Equal(t, groupID, apiKey.Group.ID)
	require.True(t, apiKey.Group.ModelRoutingEnabled)
	require.Equal(t, map[string][]int64{"claude-opus-*": {1, 2}}, apiKey.Group.ModelRouting)
}

func TestAPIKeyService_SnapshotRoundTrip_PreservesMessagesDispatchModelConfig(t *testing.T) {
	svc := NewAPIKeyService(nil, nil, nil, nil, nil, nil, &config.Config{})
	groupID := int64(9)
	apiKey := &APIKey{
		ID:      1,
		UserID:  2,
		GroupID: &groupID,
		Key:     "k-roundtrip",
		Name:    "Audit Key",
		Status:  StatusActive,
		User: &User{
			ID:          2,
			Status:      StatusActive,
			Role:        RoleUser,
			Balance:     10,
			Concurrency: 3,
		},
		Group: &Group{
			ID:                              groupID,
			Name:                            "openai",
			Platform:                        PlatformOpenAI,
			Status:                          StatusActive,
			SubscriptionType:                SubscriptionTypeStandard,
			RateMultiplier:                  1,
			AllowMessagesDispatch:           true,
			OpenAILongContextBillingEnabled: true,
			DefaultMappedModel:              "gpt-5.4",
			MessagesDispatchModelConfig: OpenAIMessagesDispatchModelConfig{
				OpusMappedModel:   "gpt-5.4-nano",
				SonnetMappedModel: "gpt-5.3-codex",
				HaikuMappedModel:  "gpt-5.4-mini",
				ExactModelMappings: map[string]string{
					"claude-sonnet-4.5": "gpt-5.4-nano",
				},
			},
		},
	}

	snapshot := svc.snapshotFromAPIKey(context.Background(), apiKey)
	roundTrip := svc.snapshotToAPIKey(apiKey.Key, snapshot)

	require.NotNil(t, roundTrip)
	require.Equal(t, apiKey.Name, roundTrip.Name)
	require.NotNil(t, roundTrip.Group)
	require.Equal(t, apiKey.Group.MessagesDispatchModelConfig, roundTrip.Group.MessagesDispatchModelConfig)
	require.True(t, roundTrip.Group.OpenAILongContextBillingEnabled)
}

func TestAPIKeyService_SnapshotRoundTrip_PreservesRequirePrivacySet(t *testing.T) {
	svc := NewAPIKeyService(nil, nil, nil, nil, nil, nil, &config.Config{})
	groupID := int64(10)
	apiKey := &APIKey{
		ID:      1,
		UserID:  2,
		GroupID: &groupID,
		Key:     "k-privacy",
		Status:  StatusActive,
		User: &User{
			ID:          2,
			Status:      StatusActive,
			Role:        RoleUser,
			Balance:     10,
			Concurrency: 3,
		},
		Group: &Group{
			ID:                groupID,
			Name:              "openai-privacy",
			Platform:          PlatformOpenAI,
			Status:            StatusActive,
			SubscriptionType:  SubscriptionTypeStandard,
			RateMultiplier:    1,
			RequirePrivacySet: true,
		},
	}

	snapshot := svc.snapshotFromAPIKey(context.Background(), apiKey)
	roundTrip := svc.snapshotToAPIKey(apiKey.Key, snapshot)

	require.NotNil(t, roundTrip)
	require.NotNil(t, roundTrip.Group)
	require.True(t, roundTrip.Group.RequirePrivacySet)
}

func TestAPIKeyService_SnapshotRoundTrip_PreservesAPIKeyConcurrency(t *testing.T) {
	svc := NewAPIKeyService(nil, nil, nil, nil, nil, nil, &config.Config{})
	apiKey := &APIKey{
		ID: 11, UserID: 22, Key: "sk-concurrency", Name: "limited", Status: StatusActive, Concurrency: 5,
		User: &User{ID: 22, Status: StatusActive},
	}

	snapshot := svc.snapshotFromAPIKey(context.Background(), apiKey)
	require.NotNil(t, snapshot)
	require.Equal(t, 5, snapshot.Concurrency)

	restored := svc.snapshotToAPIKey(apiKey.Key, snapshot)
	require.Equal(t, 5, restored.Concurrency)
}

func TestAPIKeyService_SnapshotRoundTrip_PreservesOpenAIForcePriorityTier(t *testing.T) {
	svc := NewAPIKeyService(nil, nil, nil, nil, nil, nil, &config.Config{})
	apiKey := &APIKey{
		ID:                      1,
		UserID:                  2,
		Key:                     "k-priority",
		Name:                    "Priority Key",
		Status:                  StatusActive,
		OpenAIForcePriorityTier: true,
		User: &User{
			ID:          2,
			Status:      StatusActive,
			Role:        RoleUser,
			Balance:     0,
			Concurrency: 1,
		},
	}

	snapshot := svc.snapshotFromAPIKey(context.Background(), apiKey)
	require.NotNil(t, snapshot)
	require.True(t, snapshot.OpenAIForcePriorityTier)

	roundTrip := svc.snapshotToAPIKey(apiKey.Key, snapshot)
	require.NotNil(t, roundTrip)
	require.True(t, roundTrip.OpenAIForcePriorityTier)
}

func TestAPIKeyService_SnapshotRoundTrip_PreservesExclusiveGroupAuthorizationFields(t *testing.T) {
	svc := NewAPIKeyService(nil, nil, nil, nil, nil, nil, &config.Config{})
	groupID := int64(10)
	apiKey := &APIKey{
		ID:      1,
		UserID:  2,
		GroupID: &groupID,
		Key:     "k-exclusive",
		Status:  StatusActive,
		User: &User{
			ID:            2,
			Status:        StatusActive,
			Role:          RoleUser,
			Balance:       10,
			Concurrency:   3,
			AllowedGroups: []int64{groupID, 99},
		},
		Group: &Group{
			ID:               groupID,
			Name:             "exclusive",
			Platform:         PlatformAnthropic,
			Status:           StatusActive,
			SubscriptionType: SubscriptionTypeStandard,
			IsExclusive:      true,
			RateMultiplier:   1,
		},
	}

	snapshot := svc.snapshotFromAPIKey(context.Background(), apiKey)
	require.NotNil(t, snapshot)
	require.Equal(t, []int64{groupID, 99}, snapshot.User.AllowedGroups)
	require.NotNil(t, snapshot.Group)
	require.True(t, snapshot.Group.IsExclusive)

	roundTrip := svc.snapshotToAPIKey(apiKey.Key, snapshot)
	require.NotNil(t, roundTrip)
	require.Equal(t, []int64{groupID, 99}, roundTrip.User.AllowedGroups)
	require.NotNil(t, roundTrip.Group)
	require.True(t, roundTrip.Group.IsExclusive)
	require.True(t, roundTrip.User.CanBindGroup(groupID, roundTrip.Group.IsExclusive))
}

func TestAPIKeyService_GetByKey_IgnoresLegacyAuthCacheSnapshotWithoutMessagesDispatchConfig(t *testing.T) {
	cache := &authCacheStub{}
	var repoCalls int32
	repo := &authRepoStub{
		getByKeyForAuth: func(ctx context.Context, key string) (*APIKey, error) {
			atomic.AddInt32(&repoCalls, 1)
			groupID := int64(9)
			return &APIKey{
				ID:      1,
				UserID:  2,
				GroupID: &groupID,
				Status:  StatusActive,
				User: &User{
					ID:          2,
					Status:      StatusActive,
					Role:        RoleUser,
					Balance:     10,
					Concurrency: 3,
				},
				Group: &Group{
					ID:                    groupID,
					Name:                  "openai",
					Platform:              PlatformOpenAI,
					Status:                StatusActive,
					Hydrated:              true,
					SubscriptionType:      SubscriptionTypeStandard,
					RateMultiplier:        1,
					AllowMessagesDispatch: true,
					DefaultMappedModel:    "gpt-5.4",
					MessagesDispatchModelConfig: OpenAIMessagesDispatchModelConfig{
						OpusMappedModel: "gpt-5.4-nano",
					},
				},
			}, nil
		},
	}
	cfg := &config.Config{
		APIKeyAuth: config.APIKeyAuthCacheConfig{
			L2TTLSeconds: 60,
		},
	}
	svc := NewAPIKeyService(repo, nil, nil, nil, nil, cache, cfg)

	groupID := int64(9)
	cache.getAuthCache = func(ctx context.Context, key string) (*APIKeyAuthCacheEntry, error) {
		return &APIKeyAuthCacheEntry{
			Snapshot: &APIKeyAuthSnapshot{
				APIKeyID: 1,
				UserID:   2,
				GroupID:  &groupID,
				Status:   StatusActive,
				User: APIKeyAuthUserSnapshot{
					ID:          2,
					Status:      StatusActive,
					Role:        RoleUser,
					Balance:     10,
					Concurrency: 3,
				},
				Group: &APIKeyAuthGroupSnapshot{
					ID:                    groupID,
					Name:                  "openai",
					Platform:              PlatformOpenAI,
					Status:                StatusActive,
					SubscriptionType:      SubscriptionTypeStandard,
					RateMultiplier:        1,
					AllowMessagesDispatch: true,
					DefaultMappedModel:    "gpt-5.4",
				},
			},
		}, nil
	}

	apiKey, err := svc.GetByKey(context.Background(), "k-legacy")
	require.NoError(t, err)
	require.Equal(t, int32(1), atomic.LoadInt32(&repoCalls))
	require.NotNil(t, apiKey.Group)
	require.Equal(t, "gpt-5.4-nano", apiKey.Group.MessagesDispatchModelConfig.OpusMappedModel)
}

func TestAPIKeyService_GetByKey_NegativeCache(t *testing.T) {
	cache := &authCacheStub{}
	repo := &authRepoStub{
		getByKeyForAuth: func(ctx context.Context, key string) (*APIKey, error) {
			return nil, errors.New("unexpected repo call")
		},
	}
	cfg := &config.Config{
		APIKeyAuth: config.APIKeyAuthCacheConfig{
			L2TTLSeconds:       60,
			NegativeTTLSeconds: 30,
		},
	}
	svc := NewAPIKeyService(repo, nil, nil, nil, nil, cache, cfg)
	cache.getAuthCache = func(ctx context.Context, key string) (*APIKeyAuthCacheEntry, error) {
		return &APIKeyAuthCacheEntry{NotFound: true}, nil
	}

	_, err := svc.GetByKey(context.Background(), "missing")
	require.ErrorIs(t, err, ErrAPIKeyNotFound)
}

func TestAPIKeyService_GetByKey_CacheMissStoresL2(t *testing.T) {
	cache := &authCacheStub{}
	repo := &authRepoStub{
		getByKeyForAuth: func(ctx context.Context, key string) (*APIKey, error) {
			return &APIKey{
				ID:     5,
				UserID: 7,
				Status: StatusActive,
				User: &User{
					ID:          7,
					Status:      StatusActive,
					Role:        RoleUser,
					Balance:     12,
					Concurrency: 2,
				},
			}, nil
		},
	}
	cfg := &config.Config{
		APIKeyAuth: config.APIKeyAuthCacheConfig{
			L2TTLSeconds:       60,
			NegativeTTLSeconds: 30,
		},
	}
	svc := NewAPIKeyService(repo, nil, nil, nil, nil, cache, cfg)
	cache.getAuthCache = func(ctx context.Context, key string) (*APIKeyAuthCacheEntry, error) {
		return nil, redis.Nil
	}

	apiKey, err := svc.GetByKey(context.Background(), "k2")
	require.NoError(t, err)
	require.Equal(t, int64(5), apiKey.ID)
	require.Len(t, cache.setAuthKeys, 1)
}

func TestAPIKeyService_GetByKey_UsesL1Cache(t *testing.T) {
	var calls int32
	cache := &authCacheStub{}
	repo := &authRepoStub{
		getByKeyForAuth: func(ctx context.Context, key string) (*APIKey, error) {
			atomic.AddInt32(&calls, 1)
			return &APIKey{
				ID:     21,
				UserID: 3,
				Status: StatusActive,
				User: &User{
					ID:          3,
					Status:      StatusActive,
					Role:        RoleUser,
					Balance:     5,
					Concurrency: 2,
				},
			}, nil
		},
	}
	cfg := &config.Config{
		APIKeyAuth: config.APIKeyAuthCacheConfig{
			L1Size:       1000,
			L1TTLSeconds: 60,
		},
	}
	svc := NewAPIKeyService(repo, nil, nil, nil, nil, cache, cfg)
	require.NotNil(t, svc.authCacheL1)

	_, err := svc.GetByKey(context.Background(), "k-l1")
	require.NoError(t, err)
	svc.authCacheL1.Wait()
	cacheKey := svc.authCacheKey("k-l1")
	_, ok := svc.authCacheL1.Get(cacheKey)
	require.True(t, ok)
	_, err = svc.GetByKey(context.Background(), "k-l1")
	require.NoError(t, err)
	require.Equal(t, int32(1), atomic.LoadInt32(&calls))
}

func TestAPIKeyService_InvalidateAuthCacheByUserID(t *testing.T) {
	cache := &authCacheStub{}
	repo := &authRepoStub{
		listKeysByUserID: func(ctx context.Context, userID int64) ([]string, error) {
			return []string{"k1", "k2"}, nil
		},
	}
	cfg := &config.Config{
		APIKeyAuth: config.APIKeyAuthCacheConfig{
			L2TTLSeconds:       60,
			NegativeTTLSeconds: 30,
		},
	}
	svc := NewAPIKeyService(repo, nil, nil, nil, nil, cache, cfg)

	svc.InvalidateAuthCacheByUserID(context.Background(), 7)
	require.Len(t, cache.deleteAuthKeys, 2)
}

func TestAPIKeyService_InvalidateAuthCacheByGroupID(t *testing.T) {
	cache := &authCacheStub{}
	repo := &authRepoStub{
		listKeysByGroupID: func(ctx context.Context, groupID int64) ([]string, error) {
			return []string{"k1", "k2"}, nil
		},
	}
	cfg := &config.Config{
		APIKeyAuth: config.APIKeyAuthCacheConfig{
			L2TTLSeconds: 60,
		},
	}
	svc := NewAPIKeyService(repo, nil, nil, nil, nil, cache, cfg)

	svc.InvalidateAuthCacheByGroupID(context.Background(), 9)
	require.Len(t, cache.deleteAuthKeys, 2)
}

func TestAPIKeyService_InvalidateAuthCacheByKey(t *testing.T) {
	cache := &authCacheStub{}
	repo := &authRepoStub{
		listKeysByUserID: func(ctx context.Context, userID int64) ([]string, error) {
			return nil, nil
		},
	}
	cfg := &config.Config{
		APIKeyAuth: config.APIKeyAuthCacheConfig{
			L2TTLSeconds: 60,
		},
	}
	svc := NewAPIKeyService(repo, nil, nil, nil, nil, cache, cfg)

	svc.InvalidateAuthCacheByKey(context.Background(), "k1")
	require.Len(t, cache.deleteAuthKeys, 1)
}

func TestAPIKeyService_GetByKey_CachesNegativeOnRepoMiss(t *testing.T) {
	cache := &authCacheStub{}
	repo := &authRepoStub{
		getByKeyForAuth: func(ctx context.Context, key string) (*APIKey, error) {
			return nil, ErrAPIKeyNotFound
		},
	}
	cfg := &config.Config{
		APIKeyAuth: config.APIKeyAuthCacheConfig{
			L2TTLSeconds:       60,
			NegativeTTLSeconds: 30,
		},
	}
	svc := NewAPIKeyService(repo, nil, nil, nil, nil, cache, cfg)
	cache.getAuthCache = func(ctx context.Context, key string) (*APIKeyAuthCacheEntry, error) {
		return nil, redis.Nil
	}

	_, err := svc.GetByKey(context.Background(), "missing")
	require.ErrorIs(t, err, ErrAPIKeyNotFound)
	require.Len(t, cache.setAuthKeys, 1)
}

func TestAPIKeyService_GetByKey_SingleflightCollapses(t *testing.T) {
	var calls int32
	cache := &authCacheStub{}
	repo := &authRepoStub{
		getByKeyForAuth: func(ctx context.Context, key string) (*APIKey, error) {
			atomic.AddInt32(&calls, 1)
			time.Sleep(50 * time.Millisecond)
			return &APIKey{
				ID:     11,
				UserID: 2,
				Status: StatusActive,
				User: &User{
					ID:          2,
					Status:      StatusActive,
					Role:        RoleUser,
					Balance:     1,
					Concurrency: 1,
				},
			}, nil
		},
	}
	cfg := &config.Config{
		APIKeyAuth: config.APIKeyAuthCacheConfig{
			Singleflight: true,
		},
	}
	svc := NewAPIKeyService(repo, nil, nil, nil, nil, cache, cfg)

	start := make(chan struct{})
	wg := sync.WaitGroup{}
	errs := make([]error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			_, err := svc.GetByKey(context.Background(), "k1")
			errs[idx] = err
		}(i)
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, int32(1), atomic.LoadInt32(&calls))
}
