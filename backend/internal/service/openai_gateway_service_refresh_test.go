//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// batchReaderSnapshotCache 实现 SchedulerCache + snapshotAccountBatchReader
// （可选接口），用于验证网关候选刷新优先读快照、不再回源 DB。
type batchReaderSnapshotCache struct {
	SchedulerCache
	accounts map[int64]*Account
	err      error
	calls    int
}

func (c *batchReaderSnapshotCache) GetSchedulableAccountsByIDs(_ context.Context, ids []int64) (map[int64]*Account, error) {
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	out := make(map[int64]*Account, len(ids))
	for _, id := range ids {
		if account, ok := c.accounts[id]; ok {
			out[id] = account
		}
	}
	return out, nil
}

// metaHydrationSnapshotCache 模拟生产快照缓存的双形状读取：批量读
// （GetSchedulableAccountsByIDs）返回调度 meta payload（无 api_key 等凭据），
// 单账号读（GetAccount）与快照列表（GetSnapshot）返回全量 payload（含凭据）。
// 用于端到端验证调度器选中 meta 候选后必须经 GetAccount 重新水合。
type metaHydrationSnapshotCache struct {
	SchedulerCache
	fullAccounts    map[int64]*Account
	metaAccounts    map[int64]*Account
	batchCalls      int
	getAccountCalls int
}

func (c *metaHydrationSnapshotCache) GetSnapshot(_ context.Context, _ SchedulerBucket) ([]*Account, bool, error) {
	out := make([]*Account, 0, len(c.fullAccounts))
	for _, account := range c.fullAccounts {
		cloned := *account
		out = append(out, &cloned)
	}
	return out, true, nil
}

func (c *metaHydrationSnapshotCache) GetAccount(_ context.Context, accountID int64) (*Account, error) {
	c.getAccountCalls++
	account := c.fullAccounts[accountID]
	if account == nil {
		return nil, nil
	}
	cloned := *account
	return &cloned, nil
}

func (c *metaHydrationSnapshotCache) GetSchedulableAccountsByIDs(_ context.Context, ids []int64) (map[int64]*Account, error) {
	c.batchCalls++
	out := make(map[int64]*Account, len(ids))
	for _, id := range ids {
		if account, ok := c.metaAccounts[id]; ok {
			out[id] = account
		}
	}
	return out, nil
}

// ttlOnlyRefreshCache 不实现 snapshotAccountBatchReader：验证网关排除缺少批量快照的候选。
type ttlOnlyRefreshCache struct {
	SchedulerCache
}

type spyGetByIDsAccountRepo struct {
	AccountRepository
	getByIDsCalls int
	accounts      []*Account
	err           error
}

func (r *spyGetByIDsAccountRepo) GetByIDs(context.Context, []int64) ([]*Account, error) {
	r.getByIDsCalls++
	if r.err != nil {
		return nil, r.err
	}
	return r.accounts, nil
}

func newRefreshTestService(cache SchedulerCache, repo AccountRepository) *OpenAIGatewayService {
	var snapshot *SchedulerSnapshotService
	if cache != nil {
		snapshot = NewSchedulerSnapshotService(cache, nil, nil, nil, nil)
	}
	return &OpenAIGatewayService{accountRepo: repo, schedulerSnapshot: snapshot}
}

// 快照批量读成功时：候选刷新来自 Redis 快照，不再逐请求回源 DB。
func TestRefreshOpenAICandidates_ReadsFromSnapshotBatch(t *testing.T) {
	cache := &batchReaderSnapshotCache{
		accounts: map[int64]*Account{
			1: {ID: 1, Platform: PlatformOpenAI, Status: StatusActive},
			2: {ID: 2, Platform: PlatformOpenAI, Status: StatusActive},
		},
	}
	repo := &spyGetByIDsAccountRepo{accounts: []*Account{{ID: 99, Platform: PlatformOpenAI}}}
	svc := newRefreshTestService(cache, repo)

	got := svc.refreshOpenAICandidatesFromSchedulerCache(context.Background(), []*Account{
		{ID: 1}, {ID: 2}, {ID: 2}, nil, {ID: 3},
	})
	require.Equal(t, 1, cache.calls)
	require.Zero(t, repo.getByIDsCalls, "快照命中时不得回源 DB")
	require.Len(t, got, 2)
	require.Equal(t, int64(1), got[1].ID)
	require.Equal(t, int64(2), got[2].ID)
	require.Nil(t, got[3], "快照缺失的 ID 与 DB 版“未在刷新 map 中”语义一致：视为不存在")
}

// Snapshot errors exclude candidates instead of falling back to DB.
func TestRefreshOpenAICandidates_SnapshotErrorDoesNotQueryDB(t *testing.T) {
	cache := &batchReaderSnapshotCache{err: errors.New("redis down")}
	repo := &spyGetByIDsAccountRepo{accounts: []*Account{{ID: 1, Platform: PlatformOpenAI}, {ID: 3, Platform: PlatformOpenAI}}}
	svc := newRefreshTestService(cache, repo)

	got := svc.refreshOpenAICandidatesFromSchedulerCache(context.Background(), []*Account{{ID: 1}, {ID: 3}})
	require.Equal(t, 1, cache.calls)
	require.Zero(t, repo.getByIDsCalls)
	require.Empty(t, got)
}

// A cache without the optional batch reader also excludes candidates without DB access.
func TestRefreshOpenAICandidates_NoBatchReaderDoesNotQueryDB(t *testing.T) {
	cache := &ttlOnlyRefreshCache{}
	repo := &spyGetByIDsAccountRepo{accounts: []*Account{{ID: 5, Platform: PlatformOpenAI}}}
	svc := newRefreshTestService(cache, repo)

	got := svc.refreshOpenAICandidatesFromSchedulerCache(context.Background(), []*Account{{ID: 5}})
	require.Zero(t, repo.getByIDsCalls)
	require.Empty(t, got)
}

// 快照服务缺失（未启用）时不刷新也不查询 DB，与既有行为一致。
func TestRefreshOpenAICandidates_NoSnapshotReturnsNil(t *testing.T) {
	repo := &spyGetByIDsAccountRepo{}
	svc := &OpenAIGatewayService{accountRepo: repo}

	got := svc.refreshOpenAICandidatesFromSchedulerCache(context.Background(), []*Account{{ID: 1}})
	require.Nil(t, got)
	require.Zero(t, repo.getByIDsCalls)
	require.Nil(t, svc.refreshOpenAICandidatesFromSchedulerCache(context.Background(), nil))
}

// 端到端调度器水合测试的公共构造：开启高级调度器 + 快照（meta 批量读/全量单读）。
func newMetaHydrationSchedulerService(cache *metaHydrationSnapshotCache, accountID int64, blocked bool) *OpenAIGatewayService {
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	concurrencyCache := schedulerTestConcurrencyCache{}
	if blocked {
		concurrencyCache.acquireResults = map[int64]bool{accountID: false}
	}
	return &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{},
		cache:              &schedulerTestGatewayCache{},
		cfg:                cfg,
		schedulerSnapshot:  NewSchedulerSnapshotService(cache, nil, nil, nil, nil),
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(concurrencyCache),
	}
}

func newMetaHydrationAccounts(groupID, accountID int64) (full, meta *Account) {
	full = &Account{
		ID:          accountID,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Priority:    0,
		GroupIDs:    []int64{groupID},
		Credentials: map[string]any{
			"api_key":             "sk-hydrate-test",
			"openai_capabilities": []any{"chat_completions"},
		},
	}
	// meta payload 与生产 filterSchedulerCredentials 语义一致：去掉 api_key，仅保留
	// has_api_key 标记与调度所需字段。
	meta = &Account{
		ID:          accountID,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Priority:    0,
		GroupIDs:    []int64{groupID},
		Credentials: map[string]any{
			"has_api_key":         true,
			"openai_capabilities": []any{"chat_completions"},
		},
	}
	return full, meta
}

// 调度器槽位获取成功路径（tryAcquireOpenAISelectionOrder）：批量候选刷新读到 meta
// payload（无凭据），选中结果必须经 GetAccount 水合为全量账号，可直接执行请求。
func TestOpenAIGatewayService_SelectAccountWithScheduler_MetaBatchReadHydratesAcquiredSelection(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()

	ctx := context.Background()
	groupID := int64(10421)
	accountID := int64(37421)
	full, meta := newMetaHydrationAccounts(groupID, accountID)
	cache := &metaHydrationSnapshotCache{
		fullAccounts: map[int64]*Account{accountID: full},
		metaAccounts: map[int64]*Account{accountID: meta},
	}
	svc := newMetaHydrationSchedulerService(cache, accountID, false)

	selection, decision, err := svc.SelectAccountWithScheduler(
		ctx, &groupID, "", "", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, accountID, selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
	require.Equal(t, 1, cache.batchCalls, "候选刷新必须走快照 meta 批量读")
	// 选中结果必须是水合后的全量账号：api_key 可用于请求执行，而非 meta 的 has_api_key 标记。
	require.Equal(t, "sk-hydrate-test", selection.Account.GetOpenAIApiKey())
	require.True(t, cache.getAccountCalls > 0, "选中后必须经 GetAccount 重新水合")
}

// 多候选选择路径的 Redis 往返收敛：三个候选只发生一次批量 meta 读 + 一次最终
// 水合 GetAccount。此前每个候选单独 GetAccount（每个候选一次 MGET）+ 批量读 +
// 最终水合共 N+2 次；现在 per-candidate 读已移除，GetAccount 只在水合最终选中
// 账号时发生一次。
func TestOpenAIGatewayService_SelectAccountWithScheduler_BatchReadSingleHydration(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()

	ctx := context.Background()
	groupID := int64(10431)
	fullAccounts := map[int64]*Account{}
	metaAccounts := map[int64]*Account{}
	for _, id := range []int64{1041, 1042, 1043} {
		full, meta := newMetaHydrationAccounts(groupID, id)
		fullAccounts[id] = full
		metaAccounts[id] = meta
	}
	cache := &metaHydrationSnapshotCache{
		fullAccounts: fullAccounts,
		metaAccounts: metaAccounts,
	}
	svc := newMetaHydrationSchedulerService(cache, 1041, false)

	selection, _, err := svc.SelectAccountWithScheduler(
		ctx, &groupID, "", "", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, 1, cache.batchCalls, "多候选必须合并为一次批量 meta 读")
	require.Equal(t, 1, cache.getAccountCalls, "每个候选不得单独 GetAccount；最终水合只发生一次")
	require.Equal(t, "sk-hydrate-test", selection.Account.GetOpenAIApiKey())
}

// 调度器兜底等待路径（selectByLoadBalance 尾段 fallback）：槽位获取失败后返回
// WaitPlan 候选，同样必须水合为含凭据的全量账号。
func TestOpenAIGatewayService_SelectAccountWithScheduler_MetaBatchReadHydratesFallbackWaitPlan(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()

	ctx := context.Background()
	groupID := int64(10422)
	accountID := int64(37422)
	full, meta := newMetaHydrationAccounts(groupID, accountID)
	cache := &metaHydrationSnapshotCache{
		fullAccounts: map[int64]*Account{accountID: full},
		metaAccounts: map[int64]*Account{accountID: meta},
	}
	svc := newMetaHydrationSchedulerService(cache, accountID, true)

	selection, decision, err := svc.SelectAccountWithScheduler(
		ctx, &groupID, "", "", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, accountID, selection.Account.ID)
	require.NotNil(t, selection.WaitPlan, "槽位获取失败应返回 WaitPlan 兜底")
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
	require.Equal(t, "sk-hydrate-test", selection.Account.GetOpenAIApiKey())
	require.True(t, cache.getAccountCalls > 0, "兜底选中后必须经 GetAccount 重新水合")
}
