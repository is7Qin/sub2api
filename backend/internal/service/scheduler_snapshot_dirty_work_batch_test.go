package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// dirtyWorkBatchTestCache 记录批量写入调用，并实现 schedulerAccountBatchWriter。
type dirtyWorkBatchTestCache struct {
	outboxPollCache
	setAccountsCalls int
	setAccounts      []Account
	deletedAccount   []int64
	setAccountsErr   error
}

func (c *dirtyWorkBatchTestCache) SetAccount(_ context.Context, account *Account) error {
	c.setAccountsCalls++
	c.setAccounts = append(c.setAccounts, *account)
	return nil
}

func (c *dirtyWorkBatchTestCache) SetAccounts(_ context.Context, accounts []Account) error {
	c.setAccountsCalls++
	c.setAccounts = append(c.setAccounts, accounts...)
	return c.setAccountsErr
}

func (c *dirtyWorkBatchTestCache) DeleteAccount(_ context.Context, accountID int64) error {
	c.deletedAccount = append(c.deletedAccount, accountID)
	return nil
}

// dirtyWorkBatchTestAccountRepo 统计 GetByIDs 调用次数，返回预设账号集。
type dirtyWorkBatchTestAccountRepo struct {
	AccountRepository
	getByIDsCalls int
	accounts      map[int64]*Account
	err           error
}

func (r *dirtyWorkBatchTestAccountRepo) GetByIDs(_ context.Context, ids []int64) ([]*Account, error) {
	r.getByIDsCalls++
	if r.err != nil {
		return nil, r.err
	}
	out := make([]*Account, 0, len(ids))
	for _, id := range ids {
		if account, ok := r.accounts[id]; ok {
			out = append(out, account)
		}
	}
	return out, nil
}

func newBatchDirtyWorkService(cache SchedulerCache, repo AccountRepository, throttle *accountWriteThrottle) *SchedulerSnapshotService {
	workerCtx, workerCancel := context.WithCancel(context.Background())
	return &SchedulerSnapshotService{
		cache:                cache,
		dirtyWorkRepo:        &dirtyWorkTestRepo{},
		accountRepo:          repo,
		dirtyRefreshThrottle: throttle,
		workerCtx:            workerCtx,
		workerCancel:         workerCancel,
	}
}

func TestSchedulerSnapshotDirtyWorkRefreshesAccountsInOneBatch(t *testing.T) {
	cache := &dirtyWorkBatchTestCache{}
	repo := &dirtyWorkBatchTestAccountRepo{
		accounts: map[int64]*Account{
			1: {ID: 1, Name: "a"},
			2: {ID: 2, Name: "b"},
			3: {ID: 3, Name: "c"},
		},
	}
	workRepo := &dirtyWorkTestRepo{
		promoteResults: []int{0},
		work: []SchedulerDirtyWork{
			{Kind: SchedulerDirtyWorkAccount, EntityID: 1, Generation: 1},
			{Kind: SchedulerDirtyWorkAccount, EntityID: 2, Generation: 1},
			{Kind: SchedulerDirtyWorkAccount, EntityID: 3, Generation: 1},
		},
	}
	svc := &SchedulerSnapshotService{
		cache:         cache,
		dirtyWorkRepo: workRepo,
		accountRepo:   repo,
		workerCtx:     context.Background(),
	}
	results := svc.ApplyDirtyWorkBatch(context.Background(), workRepo.work)

	// 3 个脏账号只做一次批量 DB 读取和一次批量缓存写入。
	require.Equal(t, 1, repo.getByIDsCalls)
	require.Equal(t, 1, cache.setAccountsCalls)
	require.Len(t, cache.setAccounts, 3)
	require.Len(t, results, len(workRepo.work))
	for i := range results {
		require.Equal(t, workRepo.work[i], results[i].Work)
		require.NoError(t, results[i].Err)
	}
	require.Empty(t, workRepo.acknowledged)
	require.Empty(t, workRepo.failures)
}

func TestSchedulerSnapshotDirtyWorkBatchDeletesMissingAccountOnly(t *testing.T) {
	cache := &dirtyWorkBatchTestCache{}
	repo := &dirtyWorkBatchTestAccountRepo{
		accounts: map[int64]*Account{
			1: {ID: 1, Name: "a"},
			2: {ID: 2, Name: "b"},
		},
	}
	workRepo := &dirtyWorkTestRepo{
		promoteResults: []int{0},
		work: []SchedulerDirtyWork{
			{Kind: SchedulerDirtyWorkAccount, EntityID: 1, Generation: 1},
			{Kind: SchedulerDirtyWorkAccount, EntityID: 3, Generation: 1},
			{Kind: SchedulerDirtyWorkAccount, EntityID: 2, Generation: 1},
		},
	}
	svc := &SchedulerSnapshotService{
		cache:         cache,
		dirtyWorkRepo: workRepo,
		accountRepo:   repo,
		workerCtx:     context.Background(),
	}
	results := svc.ApplyDirtyWorkBatch(context.Background(), workRepo.work)

	require.Equal(t, 1, repo.getByIDsCalls)
	// 缺失账号只走 DeleteAccount，不进入批量写入。
	require.Equal(t, []int64{3}, cache.deletedAccount)
	require.Equal(t, 1, cache.setAccountsCalls)
	require.Len(t, cache.setAccounts, 2)
	require.Len(t, results, len(workRepo.work))
	for i := range results {
		require.Equal(t, workRepo.work[i], results[i].Work)
		require.NoError(t, results[i].Err)
	}
	require.Empty(t, workRepo.acknowledged)
	require.Empty(t, workRepo.failures)
}

func TestSchedulerSnapshotDirtyWorkBatchFailureRecordsAllWithoutAck(t *testing.T) {
	cache := &dirtyWorkBatchTestCache{setAccountsErr: errors.New("redis unavailable")}
	repo := &dirtyWorkBatchTestAccountRepo{
		accounts: map[int64]*Account{
			1: {ID: 1, Name: "a"},
			2: {ID: 2, Name: "b"},
		},
	}
	workRepo := &dirtyWorkTestRepo{
		promoteResults: []int{0},
		work: []SchedulerDirtyWork{
			{Kind: SchedulerDirtyWorkAccount, EntityID: 1, Generation: 1},
			{Kind: SchedulerDirtyWorkAccount, EntityID: 2, Generation: 1},
		},
	}
	svc := &SchedulerSnapshotService{
		cache:         cache,
		dirtyWorkRepo: workRepo,
		accountRepo:   repo,
		workerCtx:     context.Background(),
	}
	results := svc.ApplyDirtyWorkBatch(context.Background(), workRepo.work)

	require.Equal(t, 1, repo.getByIDsCalls)
	require.Len(t, results, len(workRepo.work))
	for i := range results {
		require.Equal(t, workRepo.work[i], results[i].Work)
		require.Error(t, results[i].Err)
	}
	require.Empty(t, workRepo.failures)
	require.Empty(t, workRepo.acknowledged)
}

func TestSchedulerSnapshotDirtyWorkBatchReadFailureIsolatesPerAccount(t *testing.T) {
	cache := &dirtyWorkBatchTestCache{}
	repo := &dirtyWorkBatchTestAccountRepo{err: errors.New("database unavailable")}
	workRepo := &dirtyWorkTestRepo{
		promoteResults: []int{0},
		work: []SchedulerDirtyWork{
			{Kind: SchedulerDirtyWorkAccount, EntityID: 1, Generation: 1},
			{Kind: SchedulerDirtyWorkAccount, EntityID: 2, Generation: 1},
		},
	}
	svc := &SchedulerSnapshotService{
		cache:         cache,
		dirtyWorkRepo: workRepo,
		accountRepo:   repo,
		workerCtx:     context.Background(),
	}
	results := svc.ApplyDirtyWorkBatch(context.Background(), workRepo.work)

	// 批量读取失败逐项映射错误，但 processor 不拥有 repository failure/ack。
	require.Len(t, results, 2)
	require.Error(t, results[0].Err)
	require.Error(t, results[1].Err)
	require.Empty(t, workRepo.failures)
	require.Empty(t, workRepo.acknowledged)
}

func TestSchedulerSnapshotDirtyWorkBatchDuplicateAccountsShareOutcome(t *testing.T) {
	cache := &dirtyWorkBatchTestCache{setAccountsErr: errors.New("cache write failed")}
	repo := &dirtyWorkBatchTestAccountRepo{accounts: map[int64]*Account{1: {ID: 1, Name: "a"}}}
	work := []SchedulerDirtyWork{
		{Kind: SchedulerDirtyWorkAccount, EntityID: 1, Generation: 1},
		{Kind: SchedulerDirtyWorkAccount, EntityID: 1, Generation: 2},
	}
	svc := &SchedulerSnapshotService{cache: cache, accountRepo: repo, workerCtx: context.Background()}
	results := svc.ApplyDirtyWorkBatch(context.Background(), work)
	require.Len(t, results, 2)
	require.Error(t, results[0].Err)
	require.ErrorIs(t, results[1].Err, results[0].Err)
}

func TestSchedulerSnapshotDirtyWorkBatchFallsBackToPerAccountWrites(t *testing.T) {
	// 缓存未实现 schedulerAccountBatchWriter 时，逐账号 SetAccount 仍全部生效。
	cache := &dirtyWorkTestCache{}
	repo := &dirtyWorkBatchTestAccountRepo{
		accounts: map[int64]*Account{
			1: {ID: 1, Name: "a"},
			2: {ID: 2, Name: "b"},
		},
	}
	workRepo := &dirtyWorkTestRepo{
		promoteResults: []int{0},
		work: []SchedulerDirtyWork{
			{Kind: SchedulerDirtyWorkAccount, EntityID: 1, Generation: 1},
			{Kind: SchedulerDirtyWorkAccount, EntityID: 2, Generation: 1},
		},
	}
	svc := &SchedulerSnapshotService{
		cache:         cache,
		dirtyWorkRepo: workRepo,
		accountRepo:   repo,
		workerCtx:     context.Background(),
	}
	results := svc.ApplyDirtyWorkBatch(context.Background(), workRepo.work)

	require.Equal(t, 1, repo.getByIDsCalls)
	require.Len(t, cache.setAccounts, 2)
	require.Len(t, results, 2)
	require.NoError(t, results[0].Err)
	require.NoError(t, results[1].Err)
	require.Empty(t, workRepo.acknowledged)
	require.Empty(t, workRepo.failures)
}

func TestSchedulerSnapshotDirtyWorkRefreshThrottleSkipsRecentAccounts(t *testing.T) {
	cache := &dirtyWorkBatchTestCache{}
	repo := &dirtyWorkBatchTestAccountRepo{
		accounts: map[int64]*Account{1: {ID: 1, Name: "a"}},
	}
	svc := newBatchDirtyWorkService(cache, repo, newAccountWriteThrottle(time.Second))

	results := svc.refreshDirtyAccounts(context.Background(), []int64{1})
	require.Len(t, results, 1)
	require.NoError(t, results[0])
	require.Equal(t, 1, repo.getByIDsCalls)
	require.Len(t, cache.setAccounts, 1)

	// 同一账号在最小间隔内再次被脏化：直接视为成功，不重复全字段读取。
	results = svc.refreshDirtyAccounts(context.Background(), []int64{1})
	require.Len(t, results, 1)
	require.NoError(t, results[0])
	require.Equal(t, 1, repo.getByIDsCalls)
	require.Len(t, cache.setAccounts, 1)
}

func TestSchedulerSnapshotDirtyWorkRefreshThrottleAllowsAfterInterval(t *testing.T) {
	cache := &dirtyWorkBatchTestCache{}
	repo := &dirtyWorkBatchTestAccountRepo{
		accounts: map[int64]*Account{1: {ID: 1, Name: "a"}},
	}
	throttle := newAccountWriteThrottle(time.Second)
	svc := newBatchDirtyWorkService(cache, repo, throttle)

	results := svc.refreshDirtyAccounts(context.Background(), []int64{1})
	require.NoError(t, results[0])
	require.Equal(t, 1, repo.getByIDsCalls)

	// 推进 throttle 记录的时间后，下一次刷新恢复执行。
	throttle.mu.Lock()
	throttle.lastByID[1] = time.Now().Add(-2 * time.Second)
	throttle.mu.Unlock()

	results = svc.refreshDirtyAccounts(context.Background(), []int64{1})
	require.NoError(t, results[0])
	require.Equal(t, 2, repo.getByIDsCalls)
	require.Len(t, cache.setAccounts, 2)
}

func TestSchedulerSnapshotDirtyWorkRefreshDeduplicatesWithinRound(t *testing.T) {
	cache := &dirtyWorkBatchTestCache{}
	repo := &dirtyWorkBatchTestAccountRepo{
		accounts: map[int64]*Account{1: {ID: 1, Name: "a"}},
	}
	svc := newBatchDirtyWorkService(cache, repo, nil)

	results := svc.refreshDirtyAccounts(context.Background(), []int64{1, 1, 2})
	require.Len(t, results, 3)
	require.NoError(t, results[0])
	require.NoError(t, results[1])
	require.NoError(t, results[2])
	require.Equal(t, 1, repo.getByIDsCalls)
	require.Len(t, cache.setAccounts, 1)
}
