//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

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

// ttlOnlyRefreshCache 不实现 snapshotAccountBatchReader：验证网关降级 DB 查询。
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

	got := svc.refreshOpenAICandidatesFromDB(context.Background(), []*Account{
		{ID: 1}, {ID: 2}, {ID: 2}, nil, {ID: 3},
	})
	require.Equal(t, 1, cache.calls)
	require.Zero(t, repo.getByIDsCalls, "快照命中时不得回源 DB")
	require.Len(t, got, 2)
	require.Equal(t, int64(1), got[1].ID)
	require.Equal(t, int64(2), got[2].ID)
	require.Nil(t, got[3], "快照缺失的 ID 与 DB 版“未在刷新 map 中”语义一致：视为不存在")
}

// 快照批量读失败（Redis 故障）时降级 DB GetByIDs。
func TestRefreshOpenAICandidates_SnapshotErrorFallsBackToDB(t *testing.T) {
	cache := &batchReaderSnapshotCache{err: errors.New("redis down")}
	repo := &spyGetByIDsAccountRepo{accounts: []*Account{{ID: 1, Platform: PlatformOpenAI}, {ID: 3, Platform: PlatformOpenAI}}}
	svc := newRefreshTestService(cache, repo)

	got := svc.refreshOpenAICandidatesFromDB(context.Background(), []*Account{{ID: 1}, {ID: 3}})
	require.Equal(t, 1, cache.calls)
	require.Equal(t, 1, repo.getByIDsCalls)
	require.Len(t, got, 2)
	require.Equal(t, int64(1), got[1].ID)
	require.Equal(t, int64(3), got[3].ID)
}

// 快照 cache 未实现批量读（可选接口缺失）时同样降级 DB。
func TestRefreshOpenAICandidates_NoBatchReaderFallsBackToDB(t *testing.T) {
	cache := &ttlOnlyRefreshCache{}
	repo := &spyGetByIDsAccountRepo{accounts: []*Account{{ID: 5, Platform: PlatformOpenAI}}}
	svc := newRefreshTestService(cache, repo)

	got := svc.refreshOpenAICandidatesFromDB(context.Background(), []*Account{{ID: 5}})
	require.Equal(t, 1, repo.getByIDsCalls)
	require.Len(t, got, 1)
	require.Equal(t, int64(5), got[5].ID)
}

// Redis 与 DB 批量查询均失败时沿用既有 fail-open：返回 nil，调用方继续用快照候选。
func TestRefreshOpenAICandidates_BothFailReturnNil(t *testing.T) {
	cache := &batchReaderSnapshotCache{err: errors.New("redis down")}
	repo := &spyGetByIDsAccountRepo{err: errors.New("db down")}
	svc := newRefreshTestService(cache, repo)

	got := svc.refreshOpenAICandidatesFromDB(context.Background(), []*Account{{ID: 1}})
	require.Nil(t, got)
}

// 快照服务缺失（未启用）时不刷新也不查询 DB，与既有行为一致。
func TestRefreshOpenAICandidates_NoSnapshotReturnsNil(t *testing.T) {
	repo := &spyGetByIDsAccountRepo{}
	svc := &OpenAIGatewayService{accountRepo: repo}

	got := svc.refreshOpenAICandidatesFromDB(context.Background(), []*Account{{ID: 1}})
	require.Nil(t, got)
	require.Zero(t, repo.getByIDsCalls)
	require.Nil(t, svc.refreshOpenAICandidatesFromDB(context.Background(), nil))
}
