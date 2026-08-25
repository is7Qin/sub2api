//go:build integration

package repository

import (
	"context"
	"sync"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

type APIKeyRepoSuite struct {
	suite.Suite
	ctx    context.Context
	client *dbent.Client
	repo   *apiKeyRepository
}

func (s *APIKeyRepoSuite) SetupTest() {
	s.ctx = context.Background()
	tx := testEntTx(s.T())
	s.client = tx.Client()
	s.repo = newAPIKeyRepositoryWithSQL(s.client, tx)
}

func TestAPIKeyRepoSuite(t *testing.T) {
	suite.Run(t, new(APIKeyRepoSuite))
}

// --- Create / GetByID / GetByKey ---

func (s *APIKeyRepoSuite) TestCreate() {
	user := s.mustCreateUser("create@test.com")

	key := &service.APIKey{
		UserID: user.ID,
		Key:    "sk-create-test",
		Name:   "Test Key",
		Status: service.StatusActive,
	}

	err := s.repo.Create(s.ctx, key)
	s.Require().NoError(err, "Create")
	s.Require().NotZero(key.ID, "expected ID to be set")

	got, err := s.repo.GetByID(s.ctx, key.ID)
	s.Require().NoError(err, "GetByID")
	s.Require().Equal("sk-create-test", got.Key)
}

func (s *APIKeyRepoSuite) TestCreateDefaultsConcurrencyToZero() {
	user := s.mustCreateUser("create-concurrency-default@test.com")
	key := &service.APIKey{UserID: user.ID, Key: "sk-create-concurrency-default", Name: "Default", Status: service.StatusActive}

	s.Require().NoError(s.repo.Create(s.ctx, key))
	got, err := s.repo.GetByID(s.ctx, key.ID)
	s.Require().NoError(err)
	s.Zero(got.Concurrency)
}

func (s *APIKeyRepoSuite) TestConcurrencyRoundTripsThroughAuthProjectionAndUpdate() {
	user := s.mustCreateUser("concurrency-roundtrip@test.com")
	key := &service.APIKey{UserID: user.ID, Key: "sk-concurrency-roundtrip", Name: "Limited", Status: service.StatusActive, Concurrency: 6}

	s.Require().NoError(s.repo.Create(s.ctx, key))
	authenticated, err := s.repo.GetByKeyForAuth(s.ctx, key.Key)
	s.Require().NoError(err)
	s.Equal(6, authenticated.Concurrency)

	zero := 0
	updated, err := s.repo.UpdateConfig(s.ctx, key.ID, user.ID, service.APIKeyConfigPatch{Concurrency: &zero})
	s.Require().NoError(err)
	s.Zero(updated.Concurrency)
}

func (s *APIKeyRepoSuite) TestCreateRejectsNegativeConcurrency() {
	user := s.mustCreateUser("negative-concurrency@test.com")
	key := &service.APIKey{UserID: user.ID, Key: "sk-negative-concurrency", Name: "Invalid", Status: service.StatusActive, Concurrency: -1}

	err := s.repo.Create(s.ctx, key)
	s.Require().Error(err)
}

func (s *APIKeyRepoSuite) TestGetByID_NotFound() {
	_, err := s.repo.GetByID(s.ctx, 999999)
	s.Require().Error(err, "expected error for non-existent ID")
}

func (s *APIKeyRepoSuite) TestGetByKey() {
	user := s.mustCreateUser("getbykey@test.com")
	group := s.mustCreateGroup("g-key")

	key := &service.APIKey{
		UserID:  user.ID,
		Key:     "sk-getbykey",
		Name:    "My Key",
		GroupID: &group.ID,
		Status:  service.StatusActive,
	}
	s.Require().NoError(s.repo.Create(s.ctx, key))

	got, err := s.repo.GetByKey(s.ctx, key.Key)
	s.Require().NoError(err, "GetByKey")
	s.Require().Equal(key.ID, got.ID)
	s.Require().NotNil(got.User, "expected User preload")
	s.Require().Equal(user.ID, got.User.ID)
	s.Require().NotNil(got.Group, "expected Group preload")
	s.Require().Equal(group.ID, got.Group.ID)
}

func (s *APIKeyRepoSuite) TestGetByKey_NotFound() {
	_, err := s.repo.GetByKey(s.ctx, "non-existent-key")
	s.Require().Error(err, "expected error for non-existent key")
}

func (s *APIKeyRepoSuite) TestGetByKeyForAuth_PreservesMessagesDispatchModelConfig() {
	user := s.mustCreateUser("getbykey-auth-dispatch@test.com")
	group, err := s.client.Group.Create().
		SetName("g-auth-dispatch").
		SetPlatform(service.PlatformOpenAI).
		SetStatus(service.StatusActive).
		SetSubscriptionType(service.SubscriptionTypeStandard).
		SetRateMultiplier(1).
		SetAllowMessagesDispatch(true).
		SetDefaultMappedModel("gpt-5.4").
		SetMessagesDispatchModelConfig(service.OpenAIMessagesDispatchModelConfig{
			OpusMappedModel:   "gpt-5.4-nano",
			SonnetMappedModel: "gpt-5.3-codex",
			HaikuMappedModel:  "gpt-5.4-mini",
			ExactModelMappings: map[string]string{
				"claude-sonnet-4.5": "gpt-5.4-nano",
			},
		}).
		Save(s.ctx)
	s.Require().NoError(err)

	key := &service.APIKey{
		UserID:  user.ID,
		Key:     "sk-getbykey-auth-dispatch",
		Name:    "Dispatch Key",
		GroupID: &group.ID,
		Status:  service.StatusActive,
	}
	s.Require().NoError(s.repo.Create(s.ctx, key))

	got, err := s.repo.GetByKeyForAuth(s.ctx, key.Key)
	s.Require().NoError(err)
	s.Require().NotNil(got.Group)
	s.Require().True(got.Group.AllowMessagesDispatch)
	s.Require().Equal("gpt-5.4", got.Group.DefaultMappedModel)
	s.Require().Equal("gpt-5.4-nano", got.Group.MessagesDispatchModelConfig.OpusMappedModel)
	s.Require().Equal("gpt-5.4-nano", got.Group.MessagesDispatchModelConfig.ExactModelMappings["claude-sonnet-4.5"])
}

// --- Update ---

func (s *APIKeyRepoSuite) TestUpdate() {
	user := s.mustCreateUser("update@test.com")
	key := &service.APIKey{
		UserID: user.ID,
		Key:    "sk-update",
		Name:   "Original",
		Status: service.StatusActive,
	}
	s.Require().NoError(s.repo.Create(s.ctx, key))

	name := "Renamed"
	status := service.StatusDisabled
	_, err := s.repo.UpdateConfig(s.ctx, key.ID, user.ID, service.APIKeyConfigPatch{Name: &name, Status: &status})
	s.Require().NoError(err, "Update")

	got, err := s.repo.GetByID(s.ctx, key.ID)
	s.Require().NoError(err, "GetByID after update")
	s.Require().Equal("sk-update", got.Key, "Update should not change key")
	s.Require().Equal(user.ID, got.UserID, "Update should not change user_id")
	s.Require().Equal("Renamed", got.Name)
	s.Require().Equal(service.StatusDisabled, got.Status)
}

func (s *APIKeyRepoSuite) TestUpdate_ClearGroupID() {
	user := s.mustCreateUser("cleargroup@test.com")
	group := s.mustCreateGroup("g-clear")
	key := &service.APIKey{
		UserID:  user.ID,
		Key:     "sk-clear-group",
		Name:    "Group Key",
		GroupID: &group.ID,
		Status:  service.StatusActive,
	}
	s.Require().NoError(s.repo.Create(s.ctx, key))

	err := func() error { _, err := s.repo.UpdateGroupID(s.ctx, key.ID, nil); return err }()
	s.Require().NoError(err, "Update")

	got, err := s.repo.GetByID(s.ctx, key.ID)
	s.Require().NoError(err)
	s.Require().Nil(got.GroupID, "expected GroupID to be cleared")
}

func (s *APIKeyRepoSuite) TestUpdateConfigPreservesConcurrentRuntimeState() {
	user := s.mustCreateUser("scoped-update@test.com")
	key := &service.APIKey{UserID: user.ID, Key: "sk-scoped-update", Name: "before", Status: service.StatusActive, Quota: 10, QuotaUsed: 9}
	s.Require().NoError(s.repo.Create(s.ctx, key))

	_, err := s.repo.IncrementQuotaUsedAndGetState(s.ctx, key.ID, 2)
	s.Require().NoError(err)
	s.Require().NoError(s.repo.IncrementRateLimitUsage(s.ctx, key.ID, 3))

	name := "after"
	updated, err := s.repo.UpdateConfig(s.ctx, key.ID, user.ID, service.APIKeyConfigPatch{Name: &name})
	s.Require().NoError(err)
	s.Equal("after", updated.Name)
	s.Equal(11.0, updated.QuotaUsed)
	s.Equal(service.StatusAPIKeyQuotaExhausted, updated.Status)
	s.Equal(3.0, updated.Usage5h)
	s.NotNil(updated.Window5hStart)
}

func TestAPIKeyUpdateResponseViewConcurrentGroupChanges(t *testing.T) {
	client := testEntClient(t)
	repo := NewAPIKeyRepository(client, integrationDB).(*apiKeyRepository)
	ctx := context.Background()
	user, err := client.User.Create().SetEmail("response-view-" + time.Now().Format(time.RFC3339Nano) + "@test.com").SetPasswordHash("hash").SetStatus(service.StatusActive).SetRole(service.RoleUser).Save(ctx)
	require.NoError(t, err)
	groupA, err := client.Group.Create().SetName("response-a-" + time.Now().Format(time.RFC3339Nano)).SetStatus(service.StatusActive).Save(ctx)
	require.NoError(t, err)
	groupB, err := client.Group.Create().SetName("response-b-" + time.Now().Format(time.RFC3339Nano)).SetStatus(service.StatusActive).Save(ctx)
	require.NoError(t, err)
	cleanupAPIKeyResponseViewFixtures(t, user.ID, groupA.ID, groupB.ID)

	cases := []struct {
		name    string
		initial *int64
		target  *int64
	}{
		{name: "unchanged", initial: &groupA.ID, target: &groupA.ID},
		{name: "bind", target: &groupA.ID},
		{name: "unbind", initial: &groupA.ID},
		{name: "rebind", initial: &groupA.ID, target: &groupB.ID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := &service.APIKey{UserID: user.ID, Key: "sk-response-" + tc.name + time.Now().Format(time.RFC3339Nano), Name: "before", GroupID: tc.initial, Status: service.StatusActive}
			require.NoError(t, repo.Create(ctx, key))

			tx, err := client.Tx(ctx)
			require.NoError(t, err)
			txCtx := dbent.NewTxContext(ctx, tx)
			txRepo := newAPIKeyRepositoryWithSQL(tx.Client(), tx.Client())
			adminView, err := txRepo.UpdateGroupID(txCtx, key.ID, tc.target)
			require.NoError(t, err)
			assertAPIKeyResponseEdges(t, adminView, user.ID, tc.target)

			result := make(chan struct {
				key *service.APIKey
				err error
			}, 1)
			go func() {
				name := "after"
				got, updateErr := repo.UpdateConfig(ctx, key.ID, user.ID, service.APIKeyConfigPatch{Name: &name})
				result <- struct {
					key *service.APIKey
					err error
				}{got, updateErr}
			}()
			select {
			case got := <-result:
				t.Fatalf("user update bypassed held row lock: %v", got.err)
			case <-time.After(100 * time.Millisecond):
			}
			require.NoError(t, tx.Commit())
			got := <-result
			require.NoError(t, got.err)
			assertAPIKeyResponseEdges(t, got.key, user.ID, tc.target)
		})
	}
}

func assertAPIKeyResponseEdges(t *testing.T, key *service.APIKey, userID int64, groupID *int64) {
	t.Helper()
	require.NotNil(t, key.User)
	require.Equal(t, userID, key.User.ID)
	if groupID == nil {
		require.Nil(t, key.GroupID)
		require.Nil(t, key.Group)
		return
	}
	require.NotNil(t, key.GroupID)
	require.NotNil(t, key.Group)
	require.Equal(t, *key.GroupID, key.Group.ID)
	require.Equal(t, *groupID, key.Group.ID)
}

func TestAPIKeyUpdateResponseViewRollbackAndNotFound(t *testing.T) {
	client := testEntClient(t)
	repo := NewAPIKeyRepository(client, integrationDB).(*apiKeyRepository)
	ctx := context.Background()
	_, err := repo.UpdateGroupID(ctx, 999999999, nil)
	require.ErrorIs(t, err, service.ErrAPIKeyNotFound)
	_, err = repo.ResetRateLimitUsage(ctx, 999999999)
	require.ErrorIs(t, err, service.ErrAPIKeyNotFound)

	user, err := client.User.Create().SetEmail("response-rollback-" + time.Now().Format(time.RFC3339Nano) + "@test.com").SetPasswordHash("hash").SetStatus(service.StatusActive).SetRole(service.RoleUser).Save(ctx)
	require.NoError(t, err)
	group, err := client.Group.Create().SetName("response-rollback-" + time.Now().Format(time.RFC3339Nano)).SetStatus(service.StatusActive).Save(ctx)
	require.NoError(t, err)
	cleanupAPIKeyResponseViewFixtures(t, user.ID, group.ID)
	key := &service.APIKey{UserID: user.ID, Key: "sk-response-rollback-" + time.Now().Format(time.RFC3339Nano), Name: "before", Status: service.StatusActive}
	require.NoError(t, repo.Create(ctx, key))
	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	txCtx := dbent.NewTxContext(ctx, tx)
	txRepo := newAPIKeyRepositoryWithSQL(tx.Client(), tx.Client())
	view, err := txRepo.UpdateGroupID(txCtx, key.ID, &group.ID)
	require.NoError(t, err)
	assertAPIKeyResponseEdges(t, view, user.ID, &group.ID)
	require.NoError(t, tx.Rollback())
	view, err = repo.GetByID(ctx, key.ID)
	require.NoError(t, err)
	assertAPIKeyResponseEdges(t, view, user.ID, nil)
}

func cleanupAPIKeyResponseViewFixtures(t *testing.T, userID int64, groupIDs ...int64) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM api_keys WHERE user_id = $1`, userID)
		for _, groupID := range groupIDs {
			_, _ = integrationDB.ExecContext(ctx, `DELETE FROM groups WHERE id = $1`, groupID)
		}
		_, _ = integrationDB.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID)
	})
}

func (s *APIKeyRepoSuite) TestUpdateConfigOwnershipAndExplicitResets() {
	owner := s.mustCreateUser("scoped-owner@test.com")
	other := s.mustCreateUser("scoped-other@test.com")
	key := &service.APIKey{UserID: owner.ID, Key: "sk-scoped-owner", Name: "before", Status: service.StatusAPIKeyQuotaExhausted, Quota: 10, QuotaUsed: 10, Usage5h: 4}
	s.Require().NoError(s.repo.Create(s.ctx, key))

	name := "forbidden"
	_, err := s.repo.UpdateConfig(s.ctx, key.ID, other.ID, service.APIKeyConfigPatch{Name: &name})
	s.ErrorIs(err, service.ErrAPIKeyNotFound)

	updated, err := s.repo.UpdateConfig(s.ctx, key.ID, owner.ID, service.APIKeyConfigPatch{ResetQuota: true, ResetRateLimitUsage: true})
	s.Require().NoError(err)
	s.Zero(updated.QuotaUsed)
	s.Zero(updated.Usage5h)
	s.Nil(updated.Window5hStart)
	s.Equal(service.StatusAPIKeyActive, updated.Status)
}

func (s *APIKeyRepoSuite) TestUpdateConfigPreservesUnmanagedStatuses() {
	user := s.mustCreateUser("unmanaged-status@test.com")
	past := time.Now().Add(-time.Hour)

	for _, status := range []string{service.StatusAPIKeyDisabled, "admin_suspended"} {
		s.Run(status, func() {
			key := &service.APIKey{
				UserID: user.ID, Key: "sk-unmanaged-" + status, Name: "before", Status: status,
				Quota: 10, QuotaUsed: 10, ExpiresAt: &past,
			}
			s.Require().NoError(s.repo.Create(s.ctx, key))

			name := "after"
			quota := 20.0
			updated, err := s.repo.UpdateConfig(s.ctx, key.ID, user.ID, service.APIKeyConfigPatch{
				Name: &name, Quota: &quota, ResetQuota: true,
			})
			s.Require().NoError(err)
			s.Equal(status, updated.Status)
			s.Equal("after", updated.Name)
			s.Zero(updated.QuotaUsed)
		})
	}
}

// --- Delete ---

func (s *APIKeyRepoSuite) TestDelete() {
	user := s.mustCreateUser("delete@test.com")
	key := &service.APIKey{
		UserID: user.ID,
		Key:    "sk-delete",
		Name:   "Delete Me",
		Status: service.StatusActive,
	}
	s.Require().NoError(s.repo.Create(s.ctx, key))

	err := s.repo.Delete(s.ctx, key.ID)
	s.Require().NoError(err, "Delete")

	_, err = s.repo.GetByID(s.ctx, key.ID)
	s.Require().Error(err, "expected error after delete")
}

func (s *APIKeyRepoSuite) TestCreate_AfterSoftDelete_AllowsSameKey() {
	user := s.mustCreateUser("recreate-after-soft-delete@test.com")
	const reusedKey = "sk-reuse-after-soft-delete"

	first := &service.APIKey{
		UserID: user.ID,
		Key:    reusedKey,
		Name:   "First Key",
		Status: service.StatusActive,
	}
	s.Require().NoError(s.repo.Create(s.ctx, first), "create first key")

	s.Require().NoError(s.repo.Delete(s.ctx, first.ID), "soft delete first key")

	second := &service.APIKey{
		UserID: user.ID,
		Key:    reusedKey,
		Name:   "Second Key",
		Status: service.StatusActive,
	}
	s.Require().NoError(s.repo.Create(s.ctx, second), "create second key with same key")
	s.Require().NotZero(second.ID)
	s.Require().NotEqual(first.ID, second.ID, "recreated key should be a new row")
}

// --- ListByUserID / CountByUserID ---

func (s *APIKeyRepoSuite) TestListByUserID() {
	user := s.mustCreateUser("listbyuser@test.com")
	s.mustCreateApiKey(user.ID, "sk-list-1", "Key 1", nil)
	s.mustCreateApiKey(user.ID, "sk-list-2", "Key 2", nil)

	keys, page, err := s.repo.ListByUserID(s.ctx, user.ID, pagination.PaginationParams{Page: 1, PageSize: 10}, service.APIKeyListFilters{})
	s.Require().NoError(err, "ListByUserID")
	s.Require().Len(keys, 2)
	s.Require().Equal(int64(2), page.Total)
}

func (s *APIKeyRepoSuite) TestListByUserID_Pagination() {
	user := s.mustCreateUser("paging@test.com")
	for i := 0; i < 5; i++ {
		s.mustCreateApiKey(user.ID, "sk-page-"+string(rune('a'+i)), "Key", nil)
	}

	keys, page, err := s.repo.ListByUserID(s.ctx, user.ID, pagination.PaginationParams{Page: 1, PageSize: 2}, service.APIKeyListFilters{})
	s.Require().NoError(err)
	s.Require().Len(keys, 2)
	s.Require().Equal(int64(5), page.Total)
	s.Require().Equal(3, page.Pages)
}

func (s *APIKeyRepoSuite) TestCountByUserID() {
	user := s.mustCreateUser("count@test.com")
	s.mustCreateApiKey(user.ID, "sk-count-1", "K1", nil)
	s.mustCreateApiKey(user.ID, "sk-count-2", "K2", nil)

	count, err := s.repo.CountByUserID(s.ctx, user.ID)
	s.Require().NoError(err, "CountByUserID")
	s.Require().Equal(int64(2), count)
}

// --- ListByGroupID / CountByGroupID ---

func (s *APIKeyRepoSuite) TestListByGroupID() {
	user := s.mustCreateUser("listbygroup@test.com")
	group := s.mustCreateGroup("g-list")

	s.mustCreateApiKey(user.ID, "sk-grp-1", "K1", &group.ID)
	s.mustCreateApiKey(user.ID, "sk-grp-2", "K2", &group.ID)
	s.mustCreateApiKey(user.ID, "sk-grp-3", "K3", nil) // no group

	keys, page, err := s.repo.ListByGroupID(s.ctx, group.ID, pagination.PaginationParams{Page: 1, PageSize: 10})
	s.Require().NoError(err, "ListByGroupID")
	s.Require().Len(keys, 2)
	s.Require().Equal(int64(2), page.Total)
	// User preloaded
	s.Require().NotNil(keys[0].User)
}

func (s *APIKeyRepoSuite) TestCountByGroupID() {
	user := s.mustCreateUser("countgroup@test.com")
	group := s.mustCreateGroup("g-count")
	s.mustCreateApiKey(user.ID, "sk-gc-1", "K1", &group.ID)

	count, err := s.repo.CountByGroupID(s.ctx, group.ID)
	s.Require().NoError(err, "CountByGroupID")
	s.Require().Equal(int64(1), count)
}

// --- ExistsByKey ---

func (s *APIKeyRepoSuite) TestExistsByKey() {
	user := s.mustCreateUser("exists@test.com")
	s.mustCreateApiKey(user.ID, "sk-exists", "K", nil)

	exists, err := s.repo.ExistsByKey(s.ctx, "sk-exists")
	s.Require().NoError(err, "ExistsByKey")
	s.Require().True(exists)

	notExists, err := s.repo.ExistsByKey(s.ctx, "sk-not-exists")
	s.Require().NoError(err)
	s.Require().False(notExists)
}

// --- SearchAPIKeys ---

func (s *APIKeyRepoSuite) TestSearchAPIKeys() {
	user := s.mustCreateUser("search@test.com")
	s.mustCreateApiKey(user.ID, "sk-search-1", "Production Key", nil)
	s.mustCreateApiKey(user.ID, "sk-search-2", "Development Key", nil)

	found, err := s.repo.SearchAPIKeys(s.ctx, user.ID, "prod", 10)
	s.Require().NoError(err, "SearchAPIKeys")
	s.Require().Len(found, 1)
	s.Require().Contains(found[0].Name, "Production")
}

func (s *APIKeyRepoSuite) TestSearchAPIKeys_NoKeyword() {
	user := s.mustCreateUser("searchnokw@test.com")
	s.mustCreateApiKey(user.ID, "sk-nk-1", "K1", nil)
	s.mustCreateApiKey(user.ID, "sk-nk-2", "K2", nil)

	found, err := s.repo.SearchAPIKeys(s.ctx, user.ID, "", 10)
	s.Require().NoError(err)
	s.Require().Len(found, 2)
}

func (s *APIKeyRepoSuite) TestSearchAPIKeys_NoUserID() {
	user := s.mustCreateUser("searchnouid@test.com")
	s.mustCreateApiKey(user.ID, "sk-nu-1", "TestKey", nil)

	found, err := s.repo.SearchAPIKeys(s.ctx, 0, "testkey", 10)
	s.Require().NoError(err)
	s.Require().Len(found, 1)
}

// --- ClearGroupIDByGroupID ---

func (s *APIKeyRepoSuite) TestClearGroupIDByGroupID() {
	user := s.mustCreateUser("cleargrp@test.com")
	group := s.mustCreateGroup("g-clear-bulk")

	k1 := s.mustCreateApiKey(user.ID, "sk-clr-1", "K1", &group.ID)
	k2 := s.mustCreateApiKey(user.ID, "sk-clr-2", "K2", &group.ID)
	s.mustCreateApiKey(user.ID, "sk-clr-3", "K3", nil) // no group

	affected, err := s.repo.ClearGroupIDByGroupID(s.ctx, group.ID)
	s.Require().NoError(err, "ClearGroupIDByGroupID")
	s.Require().Equal(int64(2), affected)

	got1, _ := s.repo.GetByID(s.ctx, k1.ID)
	got2, _ := s.repo.GetByID(s.ctx, k2.ID)
	s.Require().Nil(got1.GroupID)
	s.Require().Nil(got2.GroupID)

	count, _ := s.repo.CountByGroupID(s.ctx, group.ID)
	s.Require().Zero(count)
}

// --- Combined CRUD/Search/ClearGroupID (original test preserved as integration) ---

func (s *APIKeyRepoSuite) TestCRUD_Search_ClearGroupID() {
	user := s.mustCreateUser("k@example.com")
	group := s.mustCreateGroup("g-k")
	key := s.mustCreateApiKey(user.ID, "sk-test-1", "My Key", &group.ID)
	key.GroupID = &group.ID

	got, err := s.repo.GetByKey(s.ctx, key.Key)
	s.Require().NoError(err, "GetByKey")
	s.Require().Equal(key.ID, got.ID)
	s.Require().NotNil(got.User)
	s.Require().Equal(user.ID, got.User.ID)
	s.Require().NotNil(got.Group)
	s.Require().Equal(group.ID, got.Group.ID)

	name := "Renamed"
	status := service.StatusDisabled
	clearGroup := (*int64)(nil)
	_, err = s.repo.UpdateConfig(s.ctx, key.ID, user.ID, service.APIKeyConfigPatch{Name: &name, Status: &status, GroupID: &clearGroup})
	s.Require().NoError(err, "UpdateConfig")

	got2, err := s.repo.GetByID(s.ctx, key.ID)
	s.Require().NoError(err, "GetByID")
	s.Require().Equal("sk-test-1", got2.Key, "Update should not change key")
	s.Require().Equal(user.ID, got2.UserID, "Update should not change user_id")
	s.Require().Equal("Renamed", got2.Name)
	s.Require().Equal(service.StatusDisabled, got2.Status)
	s.Require().Nil(got2.GroupID)

	keys, page, err := s.repo.ListByUserID(s.ctx, user.ID, pagination.PaginationParams{Page: 1, PageSize: 10}, service.APIKeyListFilters{})
	s.Require().NoError(err, "ListByUserID")
	s.Require().Equal(int64(1), page.Total)
	s.Require().Len(keys, 1)

	exists, err := s.repo.ExistsByKey(s.ctx, "sk-test-1")
	s.Require().NoError(err, "ExistsByKey")
	s.Require().True(exists, "expected key to exist")

	found, err := s.repo.SearchAPIKeys(s.ctx, user.ID, "renam", 10)
	s.Require().NoError(err, "SearchAPIKeys")
	s.Require().Len(found, 1)
	s.Require().Equal(key.ID, found[0].ID)

	// ClearGroupIDByGroupID
	k2 := s.mustCreateApiKey(user.ID, "sk-test-2", "Group Key", &group.ID)
	k2.GroupID = &group.ID

	countBefore, err := s.repo.CountByGroupID(s.ctx, group.ID)
	s.Require().NoError(err, "CountByGroupID")
	s.Require().Equal(int64(1), countBefore, "expected 1 key in group before clear")

	affected, err := s.repo.ClearGroupIDByGroupID(s.ctx, group.ID)
	s.Require().NoError(err, "ClearGroupIDByGroupID")
	s.Require().Equal(int64(1), affected, "expected 1 affected row")

	got3, err := s.repo.GetByID(s.ctx, k2.ID)
	s.Require().NoError(err, "GetByID")
	s.Require().Nil(got3.GroupID, "expected GroupID cleared")

	countAfter, err := s.repo.CountByGroupID(s.ctx, group.ID)
	s.Require().NoError(err, "CountByGroupID after clear")
	s.Require().Equal(int64(0), countAfter, "expected 0 keys in group after clear")
}

func (s *APIKeyRepoSuite) mustCreateUser(email string) *service.User {
	s.T().Helper()

	u, err := s.client.User.Create().
		SetEmail(email).
		SetPasswordHash("test-password-hash").
		SetStatus(service.StatusActive).
		SetRole(service.RoleUser).
		Save(s.ctx)
	s.Require().NoError(err, "create user")
	return userEntityToService(u)
}

func (s *APIKeyRepoSuite) mustCreateGroup(name string) *service.Group {
	s.T().Helper()

	g, err := s.client.Group.Create().
		SetName(name).
		SetStatus(service.StatusActive).
		Save(s.ctx)
	s.Require().NoError(err, "create group")
	return groupEntityToService(g)
}

func (s *APIKeyRepoSuite) mustCreateApiKey(userID int64, key, name string, groupID *int64) *service.APIKey {
	s.T().Helper()

	k := &service.APIKey{
		UserID:  userID,
		Key:     key,
		Name:    name,
		GroupID: groupID,
		Status:  service.StatusActive,
	}
	s.Require().NoError(s.repo.Create(s.ctx, k), "create api key")
	return k
}

// --- IncrementQuotaUsed ---

func (s *APIKeyRepoSuite) TestIncrementQuotaUsed_Basic() {
	user := s.mustCreateUser("incr-basic@test.com")
	key := s.mustCreateApiKey(user.ID, "sk-incr-basic", "Incr", nil)

	newQuota, err := s.repo.IncrementQuotaUsed(s.ctx, key.ID, 1.5)
	s.Require().NoError(err, "IncrementQuotaUsed")
	s.Require().Equal(1.5, newQuota, "第一次递增后应为 1.5")

	newQuota, err = s.repo.IncrementQuotaUsed(s.ctx, key.ID, 2.5)
	s.Require().NoError(err, "IncrementQuotaUsed second")
	s.Require().Equal(4.0, newQuota, "第二次递增后应为 4.0")
}

func (s *APIKeyRepoSuite) TestIncrementQuotaUsed_NotFound() {
	_, err := s.repo.IncrementQuotaUsed(s.ctx, 999999, 1.0)
	s.Require().ErrorIs(err, service.ErrAPIKeyNotFound, "不存在的 key 应返回 ErrAPIKeyNotFound")
}

func (s *APIKeyRepoSuite) TestIncrementQuotaUsed_DeletedKey() {
	user := s.mustCreateUser("incr-deleted@test.com")
	key := s.mustCreateApiKey(user.ID, "sk-incr-del", "Deleted", nil)

	s.Require().NoError(s.repo.Delete(s.ctx, key.ID), "Delete")

	_, err := s.repo.IncrementQuotaUsed(s.ctx, key.ID, 1.0)
	s.Require().ErrorIs(err, service.ErrAPIKeyNotFound, "已删除的 key 应返回 ErrAPIKeyNotFound")
}

func (s *APIKeyRepoSuite) TestIncrementQuotaUsedAndGetState() {
	user := s.mustCreateUser("quota-state@test.com")
	key := s.mustCreateApiKey(user.ID, "sk-quota-state", "QuotaState", nil)
	quota := 3.0
	_, err := s.repo.UpdateConfig(s.ctx, key.ID, user.ID, service.APIKeyConfigPatch{Quota: &quota})
	s.Require().NoError(err, "Update quota")
	_, err = s.repo.IncrementQuotaUsed(s.ctx, key.ID, 1)
	s.Require().NoError(err)

	state, err := s.repo.IncrementQuotaUsedAndGetState(s.ctx, key.ID, 2.5)
	s.Require().NoError(err, "IncrementQuotaUsedAndGetState")
	s.Require().NotNil(state)
	s.Require().Equal(3.5, state.QuotaUsed)
	s.Require().Equal(3.0, state.Quota)
	s.Require().Equal(service.StatusAPIKeyQuotaExhausted, state.Status)
	s.Require().Equal(key.Key, state.Key)

	got, err := s.repo.GetByID(s.ctx, key.ID)
	s.Require().NoError(err, "GetByID")
	s.Require().Equal(3.5, got.QuotaUsed)
	s.Require().Equal(service.StatusAPIKeyQuotaExhausted, got.Status)
}

// TestIncrementQuotaUsed_Concurrent 使用真实数据库验证并发原子性。
// 注意：此测试使用 testEntClient（非事务隔离），数据会真正写入数据库。
func TestIncrementQuotaUsed_Concurrent(t *testing.T) {
	client := testEntClient(t)
	repo := NewAPIKeyRepository(client, integrationDB).(*apiKeyRepository)
	ctx := context.Background()

	// 创建测试用户和 API Key
	u, err := client.User.Create().
		SetEmail("concurrent-incr-" + time.Now().Format(time.RFC3339Nano) + "@test.com").
		SetPasswordHash("hash").
		SetStatus(service.StatusActive).
		SetRole(service.RoleUser).
		Save(ctx)
	require.NoError(t, err, "create user")

	k := &service.APIKey{
		UserID: u.ID,
		Key:    "sk-concurrent-" + time.Now().Format(time.RFC3339Nano),
		Name:   "Concurrent",
		Status: service.StatusActive,
	}
	require.NoError(t, repo.Create(ctx, k), "create api key")
	t.Cleanup(func() {
		_ = client.APIKey.DeleteOneID(k.ID).Exec(ctx)
		_ = client.User.DeleteOneID(u.ID).Exec(ctx)
	})

	// 10 个 goroutine 各递增 1.0，总计应为 10.0
	const goroutines = 10
	const increment = 1.0
	var wg sync.WaitGroup
	errs := make([]error, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, errs[idx] = repo.IncrementQuotaUsed(ctx, k.ID, increment)
		}(i)
	}
	wg.Wait()

	for i, e := range errs {
		require.NoError(t, e, "goroutine %d failed", i)
	}

	// 验证最终结果
	got, err := repo.GetByID(ctx, k.ID)
	require.NoError(t, err, "GetByID")
	require.Equal(t, float64(goroutines)*increment, got.QuotaUsed,
		"并发递增后总和应为 %v，实际为 %v", float64(goroutines)*increment, got.QuotaUsed)
}

func (s *APIKeyRepoSuite) TestDeleteWithAudit_WritesAuditAndSoftDeletes() {
	user := s.mustCreateUser("delwithaudit@test.com")
	key := &service.APIKey{
		UserID: user.ID,
		Key:    "sk-del-audit-1",
		Name:   "Audit Me",
		Status: service.StatusActive,
	}
	s.Require().NoError(s.repo.Create(s.ctx, key))

	s.Require().NoError(s.repo.DeleteWithAudit(s.ctx, key.ID))

	_, err := s.repo.GetByID(s.ctx, key.ID)
	s.Require().Error(err)

	rows, qErr := s.client.QueryContext(s.ctx,
		`SELECT key, key_name, user_id, api_key_id FROM deleted_api_key_audits WHERE api_key_id = $1`, key.ID)
	s.Require().NoError(qErr)
	defer rows.Close()
	s.Require().True(rows.Next(), "expected one audit row")
	var auditKey, auditName string
	var auditUserID, auditAPIKeyID int64
	s.Require().NoError(rows.Scan(&auditKey, &auditName, &auditUserID, &auditAPIKeyID))
	s.Require().Equal("sk-del-audit-1", auditKey)
	s.Require().Equal("Audit Me", auditName)
	s.Require().Equal(user.ID, auditUserID)
	s.Require().Equal(key.ID, auditAPIKeyID)
}

func (s *APIKeyRepoSuite) TestDeleteWithAudit_RepeatIsIdempotent() {
	user := s.mustCreateUser("delwithaudit-idem@test.com")
	key := &service.APIKey{UserID: user.ID, Key: "sk-del-audit-2", Name: "K", Status: service.StatusActive}
	s.Require().NoError(s.repo.Create(s.ctx, key))

	s.Require().NoError(s.repo.DeleteWithAudit(s.ctx, key.ID))
	s.Require().NoError(s.repo.DeleteWithAudit(s.ctx, key.ID))
}

func (s *APIKeyRepoSuite) TestDeleteWithAudit_NotFound() {
	err := s.repo.DeleteWithAudit(s.ctx, 999999)
	s.Require().ErrorIs(err, service.ErrAPIKeyNotFound)
}
