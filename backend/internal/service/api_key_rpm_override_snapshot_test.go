package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// userGroupRateRepoCallStub 记录 rpm_override 查询次数，模拟 DB 读。
type userGroupRateRepoCallStub struct {
	mu          sync.Mutex
	rpmCalls    int
	rpmOverride *int
	rpmErr      error
}

func (s *userGroupRateRepoCallStub) GetRPMOverrideByUserAndGroup(context.Context, int64, int64) (*int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rpmCalls++
	return s.rpmOverride, s.rpmErr
}

func (s *userGroupRateRepoCallStub) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rpmCalls
}

func (s *userGroupRateRepoCallStub) GetByUserID(context.Context, int64) (map[int64]float64, error) {
	return nil, nil
}
func (s *userGroupRateRepoCallStub) GetByUserAndGroup(context.Context, int64, int64) (*float64, error) {
	return nil, nil
}
func (s *userGroupRateRepoCallStub) GetByGroupID(context.Context, int64) ([]UserGroupRateEntry, error) {
	return nil, nil
}
func (s *userGroupRateRepoCallStub) SyncUserGroupRates(context.Context, int64, map[int64]*float64) error {
	return nil
}
func (s *userGroupRateRepoCallStub) SyncGroupRateMultipliers(context.Context, int64, []GroupRateMultiplierInput) error {
	return nil
}
func (s *userGroupRateRepoCallStub) SyncGroupRPMOverrides(context.Context, int64, []GroupRPMOverrideInput) error {
	return nil
}
func (s *userGroupRateRepoCallStub) ClearGroupRPMOverrides(context.Context, int64) error { return nil }
func (s *userGroupRateRepoCallStub) DeleteByGroupID(context.Context, int64) error        { return nil }
func (s *userGroupRateRepoCallStub) DeleteByUserID(context.Context, int64) error         { return nil }

func authSnapshotAPIKey() *APIKey {
	groupID := int64(9)
	return &APIKey{
		ID: 1, UserID: 2, GroupID: &groupID, Key: "k", Status: StatusActive,
		User: &User{
			ID: 2, Status: StatusActive, Role: RoleUser, Balance: 10, Concurrency: 3,
			AllowedGroups: []int64{9},
		},
		Group: &Group{
			ID: 9, Name: "openai", Platform: PlatformOpenAI, Status: StatusActive,
			SubscriptionType: SubscriptionTypeStandard, RateMultiplier: 1, Hydrated: true,
		},
	}
}

func TestAuthSnapshot_NegativeRPMOverrideIsCachedAsLoaded(t *testing.T) {
	repo := &userGroupRateRepoCallStub{} // 无 override → nil
	svc := NewAPIKeyService(nil, nil, nil, nil, repo, nil, &config.Config{})

	snapshot := svc.snapshotFromAPIKey(context.Background(), authSnapshotAPIKey())
	require.NotNil(t, snapshot)
	require.Nil(t, snapshot.User.UserGroupRPMOverride)
	require.True(t, snapshot.User.UserGroupRPMOverrideLoaded, "nil override must be recorded as loaded so checkRPM skips the DB")

	apiKey := svc.snapshotToAPIKey("k", snapshot)
	require.True(t, apiKey.User.UserGroupRPMOverrideLoaded)
	require.Nil(t, apiKey.User.UserGroupRPMOverride)
}

func TestAuthSnapshot_PositiveRPMOverrideIsCachedAsLoaded(t *testing.T) {
	override := 42
	repo := &userGroupRateRepoCallStub{rpmOverride: &override}
	svc := NewAPIKeyService(nil, nil, nil, nil, repo, nil, &config.Config{})

	snapshot := svc.snapshotFromAPIKey(context.Background(), authSnapshotAPIKey())
	require.NotNil(t, snapshot)
	require.Equal(t, 42, *snapshot.User.UserGroupRPMOverride)
	require.True(t, snapshot.User.UserGroupRPMOverrideLoaded)

	apiKey := svc.snapshotToAPIKey("k", snapshot)
	require.Equal(t, 42, *apiKey.User.UserGroupRPMOverride)
	require.True(t, apiKey.User.UserGroupRPMOverrideLoaded)
}

func TestAuthSnapshot_RPMOverrideQueryErrorStaysUnloaded(t *testing.T) {
	repo := &userGroupRateRepoCallStub{rpmErr: errors.New("db down")}
	svc := NewAPIKeyService(nil, nil, nil, nil, repo, nil, &config.Config{})

	snapshot := svc.snapshotFromAPIKey(context.Background(), authSnapshotAPIKey())
	require.NotNil(t, snapshot)
	require.False(t, snapshot.User.UserGroupRPMOverrideLoaded, "query failure must keep the DB fallback open")
	require.Nil(t, snapshot.User.UserGroupRPMOverride)
}

// rpmCounterStub 记录 RPM 递增调用。
type rpmCounterStub struct {
	mu              sync.Mutex
	groupIncrements int
	userIncrements  int
}

func (s *rpmCounterStub) IncrementUserGroupRPM(context.Context, int64, int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.groupIncrements++
	return 1, nil
}

func (s *rpmCounterStub) IncrementUserRPM(context.Context, int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.userIncrements++
	return 1, nil
}

func (s *rpmCounterStub) GetUserGroupRPM(context.Context, int64, int64) (int, error) { return 0, nil }
func (s *rpmCounterStub) GetUserRPM(context.Context, int64) (int, error)             { return 0, nil }

func (s *rpmCounterStub) groupCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.groupIncrements
}

func TestCheckRPM_NegativeSnapshotOverrideSkipsDBFallback(t *testing.T) {
	repo := &userGroupRateRepoCallStub{}
	svc := NewBillingCacheService(nil, nil, nil, nil, &rpmCounterStub{}, repo, &config.Config{}, nil)

	user := &User{ID: 2, UserGroupRPMOverrideLoaded: true, UserGroupRPMOverride: nil, RPMLimit: 0}
	group := &Group{ID: 9, RPMLimit: 100, Status: StatusActive}

	require.NoError(t, svc.checkRPM(context.Background(), user, group))
	require.Zero(t, repo.calls(), "loaded negative override must not hit the DB on every request")
}

func TestCheckRPM_SnapshotOverrideSkipsDBFallback(t *testing.T) {
	override := 10
	repo := &userGroupRateRepoCallStub{rpmOverride: &override}
	svc := NewBillingCacheService(nil, nil, nil, nil, &rpmCounterStub{}, repo, &config.Config{}, nil)

	user := &User{ID: 2, UserGroupRPMOverrideLoaded: true, UserGroupRPMOverride: &override, RPMLimit: 0}
	group := &Group{ID: 9, RPMLimit: 100, Status: StatusActive}

	require.NoError(t, svc.checkRPM(context.Background(), user, group))
	require.Zero(t, repo.calls(), "snapshot override must not hit the DB")
}

func TestCheckRPM_FallsBackToDBWhenSnapshotNotLoaded(t *testing.T) {
	repo := &userGroupRateRepoCallStub{}
	svc := NewBillingCacheService(nil, nil, nil, nil, &rpmCounterStub{}, repo, &config.Config{}, nil)

	user := &User{ID: 2, UserGroupRPMOverrideLoaded: false, UserGroupRPMOverride: nil, RPMLimit: 0}
	group := &Group{ID: 9, RPMLimit: 100, Status: StatusActive}

	require.NoError(t, svc.checkRPM(context.Background(), user, group))
	require.Equal(t, 1, repo.calls(), "unloaded snapshot keeps the existing DB fallback")
}
