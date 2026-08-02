package service

import (
	"context"
	"fmt"
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

func TestAdminServiceImpl_ListAccountsCacheKeyedByPlanType(t *testing.T) {
	repo := &accountsListCacheRepoStub{}
	svc := &adminServiceImpl{accountRepo: repo, accountsListCache: newAccountsListTTLCache()}

	ctx := context.Background()
	_, _, err := svc.ListAccounts(ctx, 1, 200, AccountListFilters{PlanType: "plus"}, "created_at", "desc")
	require.NoError(t, err)
	_, _, err = svc.ListAccounts(ctx, 1, 200, AccountListFilters{PlanType: "pro"}, "created_at", "desc")
	require.NoError(t, err)
	require.Equal(t, int32(2), repo.listCalls.Load(), "different PlanType must not share cache entries")
}

func TestAccountsListCacheKeyIncludesPlanType(t *testing.T) {
	base := AccountListFilters{Platform: "openai", AccountType: "oauth", Status: "active", Search: "s", GroupID: 3, PrivacyMode: "cf", PlanType: "plus"}
	withPlus := accountsListCacheKey(1, 20, base, "created_at", "desc")
	base.PlanType = "pro"
	withPro := accountsListCacheKey(1, 20, base, "created_at", "desc")
	require.NotEqual(t, withPlus, withPro, "PlanType must be part of the cache key")
	require.Contains(t, withPlus, "plus")
	require.Contains(t, withPro, "pro")
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

func TestAccountsListCacheDeepCopiesNestedValues(t *testing.T) {
	c := newAccountsListTTLCache()
	const key, version = "page-1", "v1"
	accounts := []Account{{
		ID:   1,
		Name: "acc-1",
		Credentials: map[string]any{
			"email":         "a@example.com",
			"model_mapping": map[string]any{"gpt-5": "gpt-5-upstream"},
			"tokens":        []any{"t1", "t2"},
		},
		Extra:        map[string]any{"privacy_mode": "training_off"},
		AccountGroups: []AccountGroup{{AccountID: 1, GroupID: 7}},
		GroupIDs:      []int64{7},
		Groups: []*Group{{
			ID: 7, Name: "g1", Platform: "openai", SubscriptionType: "plus",
			RateMultiplier: 1.5,
			ModelRouting:   map[string][]int64{"gpt-5": {1, 2}},
		}},
	}}
	c.set(key, accounts, 1, version)

	got, total, ok := c.get(key, version)
	require.True(t, ok)
	require.Equal(t, int64(1), total)
	require.Len(t, got, 1)

	// 修改返回对象的所有嵌套结构：缓存条目与最初传入 set 的 accounts 都不应受影响
	got[0].Credentials["email"] = "mutated"
	got[0].Credentials["model_mapping"].(map[string]any)["gpt-5"] = "mutated"
	got[0].Credentials["tokens"].([]any)[0] = "mutated"
	got[0].Extra["privacy_mode"] = "mutated"
	got[0].Groups[0].Name = "mutated"
	got[0].Groups[0].ModelRouting["gpt-5"][0] = 99

	// 缓存条目未被污染（重新读取仍为原始值）
	got2, _, ok := c.get(key, version)
	require.True(t, ok)
	require.Equal(t, "a@example.com", got2[0].Credentials["email"])
	require.Equal(t, "gpt-5-upstream", got2[0].Credentials["model_mapping"].(map[string]any)["gpt-5"])
	require.Equal(t, "t1", got2[0].Credentials["tokens"].([]any)[0])
	require.Equal(t, "training_off", got2[0].Extra["privacy_mode"])
	require.Equal(t, "g1", got2[0].Groups[0].Name)
	require.Equal(t, int64(1), got2[0].Groups[0].ModelRouting["gpt-5"][0])
	// 传入 set 的原始切片同样未被污染
	require.Equal(t, "a@example.com", accounts[0].Credentials["email"])
	require.Equal(t, "gpt-5-upstream", accounts[0].Credentials["model_mapping"].(map[string]any)["gpt-5"])
	require.Equal(t, "t1", accounts[0].Credentials["tokens"].([]any)[0])
	require.Equal(t, "g1", accounts[0].Groups[0].Name)
	require.Equal(t, int64(1), accounts[0].Groups[0].ModelRouting["gpt-5"][0])
}

func TestAccountsListCacheEvictsOldestWhenOverCapacity(t *testing.T) {
	c := newAccountsListTTLCache()
	const version = "v1"
	// 容量上限 512：写入 513 个不同 key，最早写入的应被淘汰、最新的保留
	for i := 0; i < 513; i++ {
		key := fmt.Sprintf("page-%d", i)
		c.set(key, []Account{{ID: int64(i), Name: key}}, 1, version)
	}
	require.Len(t, c.items, 512, "cache must stay bounded at maxEntries")
	_, _, ok := c.get("page-0", version)
	require.False(t, ok, "oldest entry must be evicted")
	for _, i := range []int{1, 512} {
		key := fmt.Sprintf("page-%d", i)
		got, _, ok := c.get(key, version)
		require.True(t, ok, "entry %s must be retained", key)
		require.Equal(t, key, got[0].Name)
	}
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
