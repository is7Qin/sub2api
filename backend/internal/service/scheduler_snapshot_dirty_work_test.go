package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type dirtyWorkTestCache struct {
	outboxPollCache
	setAccounts    []*Account
	deletedAccount []int64
	setAccountErr  error
	lockAcquired   bool
	lockTTL        time.Duration
}

func (c *dirtyWorkTestCache) SetAccount(_ context.Context, account *Account) error {
	c.setAccounts = append(c.setAccounts, account)
	return c.setAccountErr
}

func (c *dirtyWorkTestCache) DeleteAccount(_ context.Context, accountID int64) error {
	c.deletedAccount = append(c.deletedAccount, accountID)
	return nil
}

func (c *dirtyWorkTestCache) TryLockBucket(_ context.Context, _ SchedulerBucket, ttl time.Duration) (string, bool, error) {
	c.lockTTL = ttl
	if !c.lockAcquired {
		return "", false, nil
	}
	return "test-lock", true, nil
}

type dirtyWorkTestAccountRepo struct {
	AccountRepository
	account *Account
	err     error
}

func (r *dirtyWorkTestAccountRepo) GetByID(context.Context, int64) (*Account, error) {
	return r.account, r.err
}

func (r *dirtyWorkTestAccountRepo) GetByIDs(_ context.Context, ids []int64) ([]*Account, error) {
	if r.err != nil {
		return nil, r.err
	}
	if r.account == nil {
		return nil, nil
	}
	out := make([]*Account, 0, len(ids))
	for _, id := range ids {
		if id == r.account.ID {
			out = append(out, r.account)
		}
	}
	return out, nil
}

type dirtyWorkTestRepo struct {
	promoteResults      []int
	promoteCalls        int
	work                []SchedulerDirtyWork
	listErr             error
	acknowledged        []SchedulerDirtyWork
	failures            []SchedulerDirtyWork
	stats               SchedulerDirtyWorkStats
	statsErr            error
	fullRebuildRequests int
	fullRebuildErr      error
}

func (r *dirtyWorkTestRepo) Promote(context.Context, SchedulerOwnership, int) (int, error) {
	result := 0
	if r.promoteCalls < len(r.promoteResults) {
		result = r.promoteResults[r.promoteCalls]
	}
	r.promoteCalls++
	return result, nil
}

func (r *dirtyWorkTestRepo) RequestFullRebuild(context.Context) error {
	r.fullRebuildRequests++
	return r.fullRebuildErr
}
func (r *dirtyWorkTestRepo) List(context.Context, int) ([]SchedulerDirtyWork, error) {
	return r.work, r.listErr
}
func (r *dirtyWorkTestRepo) RecordFailure(_ context.Context, _ SchedulerOwnership, work SchedulerDirtyWork, _ error) (bool, error) {
	r.failures = append(r.failures, work)
	return true, nil
}
func (r *dirtyWorkTestRepo) Acknowledge(_ context.Context, _ SchedulerOwnership, work SchedulerDirtyWork) (bool, error) {
	r.acknowledged = append(r.acknowledged, work)
	return true, nil
}
func (r *dirtyWorkTestRepo) PendingStats(context.Context) (SchedulerDirtyWorkStats, error) {
	return r.stats, r.statsErr
}

type dirtyWorkTestOwnership struct {
	ctx    context.Context
	cancel context.CancelFunc
	lost   chan struct{}
}

func newDirtyWorkTestOwnership() *dirtyWorkTestOwnership {
	ctx, cancel := context.WithCancel(context.Background())
	return &dirtyWorkTestOwnership{ctx: ctx, cancel: cancel, lost: make(chan struct{})}
}

func (o *dirtyWorkTestOwnership) Epoch() int64             { return 1 }
func (o *dirtyWorkTestOwnership) Context() context.Context { return o.ctx }
func (o *dirtyWorkTestOwnership) Lost() <-chan struct{}    { return o.lost }
func (o *dirtyWorkTestOwnership) Err() error               { return o.ctx.Err() }
func (o *dirtyWorkTestOwnership) Close() error {
	o.cancel()
	return nil
}

type dirtyWorkTestOwnershipRepo struct{}

func (dirtyWorkTestOwnershipRepo) TryAcquire(context.Context) (SchedulerOwnership, bool, error) {
	return nil, false, nil
}

func TestSchedulerSnapshotDirtyWorkPromotesUntilDrainedAndAcknowledges(t *testing.T) {
	cache := &dirtyWorkTestCache{}
	account := &Account{ID: 42, Name: "fresh"}
	repo := &dirtyWorkTestRepo{
		promoteResults: []int{1, 1, 0},
		work:           []SchedulerDirtyWork{{Kind: SchedulerDirtyWorkAccount, EntityID: account.ID, Generation: 7}},
	}
	svc := &SchedulerSnapshotService{
		cache:         cache,
		dirtyWorkRepo: repo,
		accountRepo:   &dirtyWorkTestAccountRepo{account: account},
		workerCtx:     context.Background(),
	}
	results := svc.ApplyDirtyWorkBatch(context.Background(), repo.work)

	require.Zero(t, repo.promoteCalls)
	require.Empty(t, repo.acknowledged)
	require.Empty(t, repo.failures)
	require.Len(t, results, 1)
	require.Equal(t, repo.work[0], results[0].Work)
	require.NoError(t, results[0].Err)
	require.Equal(t, []*Account{account}, cache.setAccounts)
}

func TestSchedulerSnapshotDirtyWorkRecordsFailureWithoutAcknowledging(t *testing.T) {
	cache := &dirtyWorkTestCache{setAccountErr: errors.New("redis unavailable")}
	account := &Account{ID: 54, Name: "retry"}
	repo := &dirtyWorkTestRepo{
		promoteResults: []int{0},
		work:           []SchedulerDirtyWork{{Kind: SchedulerDirtyWorkAccount, EntityID: account.ID, Generation: 3}},
	}
	svc := &SchedulerSnapshotService{
		cache:         cache,
		dirtyWorkRepo: repo,
		accountRepo:   &dirtyWorkTestAccountRepo{account: account},
		workerCtx:     context.Background(),
	}
	results := svc.ApplyDirtyWorkBatch(context.Background(), repo.work)

	require.Len(t, results, 1)
	require.Error(t, results[0].Err)
	require.Empty(t, repo.failures)
	require.Empty(t, repo.acknowledged)
}

func TestSchedulerSnapshotDirtyWorkDeletesMissingAccount(t *testing.T) {
	cache := &dirtyWorkTestCache{}
	svc := &SchedulerSnapshotService{
		cache:       cache,
		accountRepo: &dirtyWorkTestAccountRepo{err: ErrAccountNotFound},
	}

	err := svc.handleDirtyWork(context.Background(), SchedulerDirtyWork{Kind: SchedulerDirtyWorkAccount, EntityID: 73})

	require.NoError(t, err)
	require.Equal(t, []int64{73}, cache.deletedAccount)
}

func TestSchedulerSnapshotDirtyWorkDrainsLegacyLifecycleEventWithoutDuplicateRebuild(t *testing.T) {
	cache := &dirtyWorkTestCache{lockAcquired: false}
	svc := &SchedulerSnapshotService{
		cache:         cache,
		dirtyWorkRepo: &dirtyWorkTestRepo{},
		ownershipRepo: dirtyWorkTestOwnershipRepo{},
	}
	accountID := int64(42)

	err := svc.handleOutboxEvent(context.Background(), SchedulerOutboxEvent{
		EventType: SchedulerOutboxEventAccountChanged,
		AccountID: &accountID,
	}, nil)

	require.NoError(t, err)
}

func TestSchedulerSnapshotDirtyWorkRetriesContendedBucket(t *testing.T) {
	cache := &dirtyWorkTestCache{lockAcquired: false}
	svc := &SchedulerSnapshotService{cache: cache}

	err := svc.handleDirtyWork(context.Background(), SchedulerDirtyWork{Kind: SchedulerDirtyWorkGroup, EntityID: 91})

	require.ErrorIs(t, err, errSchedulerBucketLockBusy)
	require.Greater(t, cache.lockTTL, schedulerBucketRebuildLimit)
}

func TestSchedulerSnapshotDirtyWorkRejectsUnknownKind(t *testing.T) {
	svc := &SchedulerSnapshotService{}
	err := svc.handleDirtyWork(context.Background(), SchedulerDirtyWork{Kind: 99})
	require.Error(t, err)
	require.False(t, errors.Is(err, context.Canceled))
}

func TestSchedulerSnapshotDirtyWorkListFailuresRequestLatchedRebuild(t *testing.T) {
	repo := &dirtyWorkTestRepo{listErr: errors.New("database unavailable")}
	workerCtx, workerCancel := context.WithCancel(context.Background())
	svc := &SchedulerSnapshotService{
		dirtyWorkRepo: repo,
		workerCtx:     workerCtx,
		workerCancel:  workerCancel,
		cfg: &config.Config{Gateway: config.GatewayConfig{Scheduling: config.GatewaySchedulingConfig{
			OutboxLagRebuildFailures: 2,
		}}},
	}
	workerCancel()
	svc.recordDirtyListFailure(context.Background())
	require.Zero(t, repo.fullRebuildRequests)

	svc.recordDirtyListFailure(context.Background())
	require.Equal(t, 1, repo.fullRebuildRequests)

	svc.recordDirtyListFailure(context.Background())
	require.Equal(t, 1, repo.fullRebuildRequests)

	// A successful list proves recovery and rearms the next outage.
	repo.listErr = nil
	svc.clearDirtyListFailure()
	repo.listErr = errors.New("database unavailable")
	svc.recordDirtyListFailure(context.Background())
	svc.recordDirtyListFailure(context.Background())
	require.Equal(t, 2, repo.fullRebuildRequests)
}

func TestSchedulerSnapshotDirtyWorkListFailureRebuildRequestRetries(t *testing.T) {
	repo := &dirtyWorkTestRepo{fullRebuildErr: errors.New("database unavailable")}
	svc := &SchedulerSnapshotService{
		dirtyWorkRepo: repo,
		cfg: &config.Config{Gateway: config.GatewayConfig{Scheduling: config.GatewaySchedulingConfig{
			OutboxLagRebuildFailures: 1,
		}}},
	}

	svc.recordDirtyListFailure(context.Background())
	repo.fullRebuildErr = nil
	svc.recordDirtyListFailure(context.Background())

	require.Equal(t, 2, repo.fullRebuildRequests)
}

func TestSchedulerSnapshotDirtyWorkDegradationUsesPersistentStatsAndLatches(t *testing.T) {
	oldest := time.Now().Add(-time.Minute)
	repo := &dirtyWorkTestRepo{stats: SchedulerDirtyWorkStats{
		Count:           12,
		OldestUpdatedAt: &oldest,
		FailedCount:     2,
	}}
	svc := &SchedulerSnapshotService{
		dirtyWorkRepo: repo,
		cfg: &config.Config{Gateway: config.GatewayConfig{Scheduling: config.GatewaySchedulingConfig{
			OutboxLagRebuildSeconds:  10,
			OutboxLagRebuildFailures: 2,
		}}},
	}

	svc.checkDirtyWorkLag(context.Background())
	require.Zero(t, repo.fullRebuildRequests)
	svc.checkDirtyWorkLag(context.Background())
	require.Equal(t, 1, repo.fullRebuildRequests)

	// Persistent degradation must not advance the coalesced global generation on
	// every poll. Clearing the condition rearms a later incident.
	svc.checkDirtyWorkLag(context.Background())
	require.Equal(t, 1, repo.fullRebuildRequests)
	repo.stats = SchedulerDirtyWorkStats{}
	svc.checkDirtyWorkLag(context.Background())
	repo.stats = SchedulerDirtyWorkStats{Count: 12, OldestUpdatedAt: &oldest}
	svc.checkDirtyWorkLag(context.Background())
	svc.checkDirtyWorkLag(context.Background())
	require.Equal(t, 2, repo.fullRebuildRequests)
}

func TestSchedulerSnapshotDirtyWorkDegradationCountsCanonicalRows(t *testing.T) {
	repo := &dirtyWorkTestRepo{stats: SchedulerDirtyWorkStats{Count: 4}}
	svc := &SchedulerSnapshotService{
		dirtyWorkRepo: repo,
		cfg: &config.Config{Gateway: config.GatewayConfig{Scheduling: config.GatewaySchedulingConfig{
			OutboxLagRebuildFailures: 1,
			OutboxBacklogRebuildRows: 4,
		}}},
	}

	svc.checkDirtyWorkLag(context.Background())

	require.Equal(t, 1, repo.fullRebuildRequests)
}

func TestSchedulerSnapshotDirtyWorkDegradationRetriesFailedRequest(t *testing.T) {
	oldest := time.Now().Add(-time.Minute)
	repo := &dirtyWorkTestRepo{
		stats:          SchedulerDirtyWorkStats{Count: 1, OldestUpdatedAt: &oldest},
		fullRebuildErr: errors.New("database unavailable"),
	}
	svc := &SchedulerSnapshotService{
		dirtyWorkRepo: repo,
		cfg: &config.Config{Gateway: config.GatewayConfig{Scheduling: config.GatewaySchedulingConfig{
			OutboxLagRebuildSeconds:  10,
			OutboxLagRebuildFailures: 1,
		}}},
	}

	svc.checkDirtyWorkLag(context.Background())
	repo.fullRebuildErr = nil
	svc.checkDirtyWorkLag(context.Background())

	require.Equal(t, 2, repo.fullRebuildRequests)
}
