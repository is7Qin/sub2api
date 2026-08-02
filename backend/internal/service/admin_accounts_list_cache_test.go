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
