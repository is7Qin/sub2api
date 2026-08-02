package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/stretchr/testify/require"
)

type accountsListCacheRepoStub struct {
	AccountRepository
	listCalls atomic.Int32
}

func (s *accountsListCacheRepoStub) ListWithFilters(_ context.Context, params pagination.PaginationParams, _ AccountListFilters) ([]Account, *pagination.PaginationResult, error) {
	s.listCalls.Add(1)
	return []Account{{ID: 1, Name: "acc-1"}}, &pagination.PaginationResult{Total: 1}, nil
}

func TestAdminServiceImpl_ListAccountsCacheHits(t *testing.T) {
	repo := &accountsListCacheRepoStub{}
	svc := &adminServiceImpl{accountRepo: repo, accountsListCache: newAccountsListTTLCache()}

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		accounts, total, err := svc.ListAccounts(ctx, 1, 200, AccountListFilters{}, "created_at", "desc")
		require.NoError(t, err)
		require.Equal(t, int64(1), total)
		require.Len(t, accounts, 1)
	}
	require.Equal(t, int32(1), repo.listCalls.Load(), "repeated calls within TTL must hit cache")
}

func TestAdminServiceImpl_ListAccountsCacheKeyedByParams(t *testing.T) {
	repo := &accountsListCacheRepoStub{}
	svc := &adminServiceImpl{accountRepo: repo, accountsListCache: newAccountsListTTLCache()}

	ctx := context.Background()
	_, _, err := svc.ListAccounts(ctx, 1, 200, AccountListFilters{}, "created_at", "desc")
	require.NoError(t, err)
	_, _, err = svc.ListAccounts(ctx, 2, 200, AccountListFilters{}, "created_at", "desc")
	require.NoError(t, err)
	_, _, err = svc.ListAccounts(ctx, 1, 200, AccountListFilters{Status: "active"}, "created_at", "desc")
	require.NoError(t, err)
	require.Equal(t, int32(3), repo.listCalls.Load(), "different page/filters must not share cache entries")
}

func TestAdminServiceImpl_ListAccountsCacheExpires(t *testing.T) {
	repo := &accountsListCacheRepoStub{}
	svc := &adminServiceImpl{accountRepo: repo, accountsListCache: newAccountsListTTLCache()}

	ctx := context.Background()
	_, _, err := svc.ListAccounts(ctx, 1, 200, AccountListFilters{}, "created_at", "desc")
	require.NoError(t, err)

	// 手动让缓存过期（TTL 3 秒，测试不等待真实时间）
	svc.accountsListCache.mu.Lock()
	for k := range svc.accountsListCache.items {
		svc.accountsListCache.items[k] = accountsListCacheEntry{
			accounts: svc.accountsListCache.items[k].accounts,
			total:    svc.accountsListCache.items[k].total,
			exp:      time.Now().Add(-time.Second),
		}
	}
	svc.accountsListCache.mu.Unlock()

	_, _, err = svc.ListAccounts(ctx, 1, 200, AccountListFilters{}, "created_at", "desc")
	require.NoError(t, err)
	require.Equal(t, int32(2), repo.listCalls.Load(), "expired entry must be re-queried")
}

func TestAdminServiceImpl_ListAccountsCacheReturnsCopies(t *testing.T) {
	repo := &accountsListCacheRepoStub{}
	svc := &adminServiceImpl{accountRepo: repo, accountsListCache: newAccountsListTTLCache()}

	ctx := context.Background()
	accounts, _, err := svc.ListAccounts(ctx, 1, 200, AccountListFilters{}, "created_at", "desc")
	require.NoError(t, err)
	accounts[0].Name = "mutated"

	// 第二次调用不受第一次修改影响（返回的是副本）
	accounts2, _, err := svc.ListAccounts(ctx, 1, 200, AccountListFilters{}, "created_at", "desc")
	require.NoError(t, err)
	require.Equal(t, "acc-1", accounts2[0].Name)
}

type accountsListCacheRepoWithSubsetStub struct {
	accountsListCacheRepoStub
	subsets map[int64]map[string]any
}

func (s *accountsListCacheRepoWithSubsetStub) ListAccountCredentialSubset(_ context.Context, ids []int64) (map[int64]map[string]any, error) {
	return s.subsets, nil
}

func TestAdminServiceImpl_ListAccountsFillsCredentialSubset(t *testing.T) {
	repo := &accountsListCacheRepoWithSubsetStub{
		subsets: map[int64]map[string]any{
			1: {"email": "a@example.com", "plan_type": "plus", "model_mapping": map[string]any{"gpt-5": "gpt-5-upstream"}},
		},
	}
	svc := &adminServiceImpl{accountRepo: repo, accountsListCache: newAccountsListTTLCache()}

	accounts, _, err := svc.ListAccounts(context.Background(), 1, 200, AccountListFilters{}, "created_at", "desc")
	require.NoError(t, err)
	require.Len(t, accounts, 1)
	require.Equal(t, "a@example.com", accounts[0].Credentials["email"])
	require.Equal(t, "plus", accounts[0].Credentials["plan_type"])
	require.NotNil(t, accounts[0].Credentials["model_mapping"])
}

// accountsListCacheProjectedRepoStub implements the optional
// accountListProjectedReader capability and returns full credentials from the
// general ListWithFilters contract, recording which variant was exercised.
type accountsListCacheProjectedRepoStub struct {
	AccountRepository
	projectedCalls  atomic.Int32
	fullCalls       atomic.Int32
	fullCredentials map[string]any
}

func (s *accountsListCacheProjectedRepoStub) ListWithFilters(_ context.Context, _ pagination.PaginationParams, _ AccountListFilters) ([]Account, *pagination.PaginationResult, error) {
	s.fullCalls.Add(1)
	return []Account{{ID: 1, Name: "acc-1", Credentials: s.fullCredentials}}, &pagination.PaginationResult{Total: 1}, nil
}

func (s *accountsListCacheProjectedRepoStub) ListWithFiltersProjected(_ context.Context, _ pagination.PaginationParams, _ AccountListFilters) ([]Account, *pagination.PaginationResult, error) {
	s.projectedCalls.Add(1)
	// 投影版不带完整凭据。
	return []Account{{ID: 1, Name: "acc-1"}}, &pagination.PaginationResult{Total: 1}, nil
}

func TestAdminServiceImpl_ListAccountsUsesProjectedVariant(t *testing.T) {
	repo := &accountsListCacheProjectedRepoStub{fullCredentials: map[string]any{"access_token": "secret"}}
	svc := &adminServiceImpl{accountRepo: repo, accountsListCache: newAccountsListTTLCache()}

	accounts, _, err := svc.ListAccounts(context.Background(), 1, 200, AccountListFilters{}, "created_at", "desc")
	require.NoError(t, err)
	require.Equal(t, int32(1), repo.projectedCalls.Load(), "admin list must go through the projected variant")
	require.Equal(t, int32(0), repo.fullCalls.Load(), "admin list must not call the full-contract ListWithFilters")
	require.Len(t, accounts, 1)
	require.Nil(t, accounts[0].Credentials, "projected rows must not carry full credentials")
}

func TestAdminServiceImpl_ListWithFiltersGeneralContractReturnsFullCredentials(t *testing.T) {
	creds := map[string]any{"access_token": "secret", "refresh_token": "rt"}
	repo := &accountsListCacheProjectedRepoStub{fullCredentials: creds}

	// 通用契约：AccountService.List 依赖的 ListWithFilters 必须返回完整凭据。
	accounts, _, err := repo.ListWithFilters(context.Background(), pagination.PaginationParams{Page: 1, PageSize: 200}, AccountListFilters{})
	require.NoError(t, err)
	require.Len(t, accounts, 1)
	require.Equal(t, creds, accounts[0].Credentials, "ListWithFilters general contract must return full credentials")
}

func TestAccountListGroupLite(t *testing.T) {
	full := &Group{
		ID: 7, Name: "g", Description: "desc", Platform: "openai",
		RateMultiplier: 1.5, IsExclusive: true, Status: "active",
		SubscriptionType: "plus", DailyLimitUSD: float64Ptr(10),
		ModelRouting: map[string][]int64{"gpt-5": {1}},
	}
	lite := accountListGroupLite([]*Group{full})
	require.Len(t, lite, 1)
	require.Equal(t, int64(7), lite[0].ID)
	require.Equal(t, "g", lite[0].Name)
	require.Equal(t, "openai", lite[0].Platform)
	require.Equal(t, "plus", lite[0].SubscriptionType)
	require.Equal(t, 1.5, lite[0].RateMultiplier)
	require.Empty(t, lite[0].Description)
	require.Empty(t, lite[0].ModelRouting)
	require.Nil(t, lite[0].DailyLimitUSD)
}
