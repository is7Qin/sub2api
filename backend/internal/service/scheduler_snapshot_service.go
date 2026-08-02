package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

var (
	ErrSchedulerCacheNotReady   = errors.New("scheduler cache not ready")
	ErrSchedulerFallbackLimited = errors.New("scheduler db fallback limited")
	errSchedulerBucketLockBusy  = errors.New("scheduler bucket rebuild lock contended")
)

const (
	outboxEventTimeout          = 2 * time.Minute
	dirtyWorkBatchSize          = 100
	schedulerBucketRebuildLimit = 30 * time.Second
	schedulerBucketLockTTL      = schedulerBucketRebuildLimit + 5*time.Second
)

// batchSeenKey tracks which (groupID, platform) bucket sets have already been
// rebuilt within a single pollOutbox call, to avoid redundant work when multiple
// account_changed events share the same groups.
type batchSeenKey struct {
	groupID  int64
	platform string
}

type SchedulerSnapshotService struct {
	cache                   SchedulerCache
	outboxRepo              SchedulerOutboxRepository
	dirtyWorkRepo           SchedulerDirtyWorkRepository
	ownershipRepo           SchedulerOwnershipRepository
	accountRepo             AccountRepository
	groupRepo               GroupRepository
	cfg                     *config.Config
	stopCh                  chan struct{}
	stopOnce                sync.Once
	workerCtx               context.Context
	workerCancel            context.CancelFunc
	wg                      sync.WaitGroup
	fallbackLimit           *fallbackLimiter
	lagMu                   sync.Mutex
	lagFailures             int
	dirtyLagFailures        int
	dirtyRebuildLatched     bool
	dirtyListFailures       int
	dirtyListRebuildLatched bool
	// 快照解码缓存：GetSnapshot 每次调用都对整个桶的账号做 JSON 解码
	// （大账号池下 O(N) 且与请求成功率无关），高频网关请求下是 CPU 主源。
	// 短 TTL 缓存解码结果，命中时免解码；调度器对快照账号只读使用。
	decodeCache sync.Map // bucket.String() -> *snapshotDecodeCacheEntry
}

// snapshotDecodeCacheTTL 是版本不可用（接口缺失或版本读取失败）时快照解码缓存
// 的保留时间。5 秒内的过期由候选 freshness recheck 兜底，不影响正确性。
const snapshotDecodeCacheTTL = 5 * time.Second

// snapshotDecodeVersionedTTL 是版本可用的快照解码缓存保留时间。重建后 active
// 版本立即递增使缓存失效，TTL 只是版本丢失时的兜底上限，不再承担主要失效职责。
const snapshotDecodeVersionedTTL = 30 * time.Second

type snapshotDecodeCacheEntry struct {
	accounts []*Account
	version  string
	exp      time.Time
}

// snapshotVersionReader 是可选接口：解码缓存通过它读取分桶激活版本，重建后立即
// 失效本地条目。cache 未实现时（仅测试 stub 或第三方实现）退化为 TTL 兜底。
type snapshotVersionReader interface {
	GetSnapshotVersion(ctx context.Context, bucket SchedulerBucket) (string, error)
}

func NewSchedulerSnapshotService(
	cache SchedulerCache,
	outboxRepo SchedulerOutboxRepository,
	accountRepo AccountRepository,
	groupRepo GroupRepository,
	cfg *config.Config,
) *SchedulerSnapshotService {
	return newSchedulerSnapshotService(cache, outboxRepo, nil, nil, accountRepo, groupRepo, cfg)
}

func newSchedulerSnapshotService(
	cache SchedulerCache,
	outboxRepo SchedulerOutboxRepository,
	dirtyWorkRepo SchedulerDirtyWorkRepository,
	ownershipRepo SchedulerOwnershipRepository,
	accountRepo AccountRepository,
	groupRepo GroupRepository,
	cfg *config.Config,
) *SchedulerSnapshotService {
	maxQPS := 0
	if cfg != nil {
		maxQPS = cfg.Gateway.Scheduling.DbFallbackMaxQPS
	}
	workerCtx, workerCancel := context.WithCancel(context.Background())
	return &SchedulerSnapshotService{
		cache:         cache,
		outboxRepo:    outboxRepo,
		dirtyWorkRepo: dirtyWorkRepo,
		ownershipRepo: ownershipRepo,
		accountRepo:   accountRepo,
		groupRepo:     groupRepo,
		cfg:           cfg,
		stopCh:        make(chan struct{}),
		workerCtx:     workerCtx,
		workerCancel:  workerCancel,
		fallbackLimit: newFallbackLimiter(maxQPS),
	}
}

func (s *SchedulerSnapshotService) Start() {
	if s == nil || s.cache == nil {
		return
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.runInitialRebuild()
	}()

	interval := s.outboxPollInterval()
	if s.dirtyWorkRepo != nil && s.ownershipRepo != nil && interval > 0 {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.runDirtyWorkWorker(interval)
		}()
	}
	// Keep the legacy worker during producer migration: observational last-used
	// events are intentionally absent from lifecycle dirty sources.
	if s.outboxRepo != nil && interval > 0 {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.runOutboxWorker(interval)
		}()
	}

	fullInterval := s.fullRebuildInterval()
	if fullInterval > 0 {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.runFullRebuildWorker(fullInterval)
		}()
	}
}

func (s *SchedulerSnapshotService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stopCh)
		if s.workerCancel != nil {
			s.workerCancel()
		}
	})
	s.wg.Wait()
}

func (s *SchedulerSnapshotService) ListSchedulableAccounts(ctx context.Context, groupID *int64, platform string, hasForcePlatform bool) ([]Account, bool, error) {
	useMixed := (platform == PlatformAnthropic || platform == PlatformGemini) && !hasForcePlatform
	mode := s.resolveMode(platform, hasForcePlatform)
	bucket := s.bucketFor(groupID, platform, mode)

	if s.cache != nil {
		cacheKey := bucket.String()
		// 先读激活版本：重建后 active 版本立即递增，据此失效本地解码缓存，
		// 避免 rebuild 后 TTL 内仍返回旧账号集合。版本不可用（接口缺失/
		// 读取失败）时退化为纯 TTL 兜底。
		version := s.readSnapshotVersion(ctx, bucket)
		// 再查解码缓存：同一桶的 GetSnapshot 解码结果在 TTL 内复用，
		// 避免每个网关请求对全桶账号重新 JSON 解码。
		if entry, ok := s.decodeCache.Load(cacheKey); ok {
			if e, ok := entry.(*snapshotDecodeCacheEntry); ok {
				if version == "" {
					if time.Now().Before(e.exp) {
						return derefAccounts(e.accounts), useMixed, nil
					}
				} else if e.version == version && time.Now().Before(e.exp) {
					return derefAccounts(e.accounts), useMixed, nil
				}
			}
		}
		cached, hit, err := s.cache.GetSnapshot(ctx, bucket)
		if err != nil {
			logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] cache read failed: bucket=%s err=%v", bucket.String(), err)
		} else if hit {
			ttl := snapshotDecodeCacheTTL
			if version != "" {
				ttl = snapshotDecodeVersionedTTL
			}
			s.decodeCache.Store(cacheKey, &snapshotDecodeCacheEntry{
				accounts: cached,
				version:  version,
				exp:      time.Now().Add(ttl),
			})
			return derefAccounts(cached), useMixed, nil
		}
	}

	if err := s.guardFallback(ctx); err != nil {
		return nil, useMixed, err
	}

	fallbackCtx, cancel := s.withFallbackTimeout(ctx)
	defer cancel()

	accounts, err := s.loadAccountsFromDB(fallbackCtx, bucket, useMixed)
	if err != nil {
		return nil, useMixed, err
	}

	if s.cache != nil {
		if err := s.cache.SetSnapshot(fallbackCtx, bucket, accounts); err != nil {
			logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] cache write failed: bucket=%s err=%v", bucket.String(), err)
		}
	}

	return accounts, useMixed, nil
}

// readSnapshotVersion 通过可选接口读取分桶激活版本；cache 未实现
// snapshotVersionReader 或读取失败时返回空串，由调用方退化为 TTL 兜底。
// 读取失败只记 debug：随后 GetSnapshot 失败会记录完整错误，避免重复告警。
func (s *SchedulerSnapshotService) readSnapshotVersion(ctx context.Context, bucket SchedulerBucket) string {
	reader, ok := s.cache.(snapshotVersionReader)
	if !ok {
		return ""
	}
	version, err := reader.GetSnapshotVersion(ctx, bucket)
	if err != nil {
		slog.Debug("[Scheduler] snapshot version read failed", "bucket", bucket.String(), "err", err)
		return ""
	}
	return version
}

func (s *SchedulerSnapshotService) GetAccount(ctx context.Context, accountID int64) (*Account, error) {
	if accountID <= 0 {
		return nil, nil
	}
	if s.cache != nil {
		account, err := s.cache.GetAccount(ctx, accountID)
		if err != nil {
			logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] account cache read failed: id=%d err=%v", accountID, err)
		} else if account != nil {
			return account, nil
		}
	}

	if err := s.guardFallback(ctx); err != nil {
		return nil, err
	}
	fallbackCtx, cancel := s.withFallbackTimeout(ctx)
	defer cancel()
	return s.accountRepo.GetByID(fallbackCtx, accountID)
}

// GetGroupByID 获取分组信息（供调度器使用）
func (s *SchedulerSnapshotService) GetGroupByID(ctx context.Context, groupID int64) (*Group, error) {
	if s.groupRepo == nil {
		return nil, nil
	}
	return s.groupRepo.GetByID(ctx, groupID)
}

// UpdateAccountInCache 立即更新 Redis 中单个账号的数据（用于模型限流后立即生效）
func (s *SchedulerSnapshotService) UpdateAccountInCache(ctx context.Context, account *Account) error {
	if s.cache == nil || account == nil {
		return nil
	}
	return s.cache.SetAccount(ctx, account)
}

func (s *SchedulerSnapshotService) runInitialRebuild() {
	if s.cache == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	buckets, err := s.cache.ListBuckets(ctx)
	if err != nil {
		logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] list buckets failed: %v", err)
	}
	if len(buckets) == 0 {
		buckets, err = s.defaultBuckets(ctx)
		if err != nil {
			logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] default buckets failed: %v", err)
			return
		}
	}
	if err := s.rebuildBuckets(ctx, buckets, "startup"); err != nil {
		logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] rebuild startup failed: %v", err)
	}
}

func (s *SchedulerSnapshotService) runDirtyWorkWorker(interval time.Duration) {
	if s.dirtyWorkRepo == nil || s.ownershipRepo == nil {
		return
	}
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if s.workerCtx.Err() != nil {
			return
		}
		ownership, acquired, err := s.ownershipRepo.TryAcquire(s.workerCtx)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] ownership acquisition failed: %v", err)
			}
		} else if acquired {
			s.consumeDirtyWork(ownership, interval)
			if err := ownership.Close(); err != nil {
				logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] ownership release failed: %v", err)
			}
		}

		select {
		case <-ticker.C:
		case <-s.workerCtx.Done():
			return
		}
	}
}

func (s *SchedulerSnapshotService) consumeDirtyWork(ownership SchedulerOwnership, interval time.Duration) {
	if ownership == nil {
		return
	}
	if interval <= 0 {
		interval = time.Second
	}
	consumeCtx, cancel := context.WithCancel(ownership.Context())
	stopWorkerCancel := context.AfterFunc(s.workerCtx, cancel)
	defer func() {
		stopWorkerCancel()
		cancel()
	}()

	poll := func() {
		for range dirtyWorkBatchSize {
			promoted, err := s.dirtyWorkRepo.Promote(consumeCtx, ownership, dirtyWorkBatchSize)
			if err != nil {
				if consumeCtx.Err() == nil {
					logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] dirty source promotion failed: %v", err)
				}
				return
			}
			if promoted == 0 {
				break
			}
		}

		work, err := s.dirtyWorkRepo.List(consumeCtx, dirtyWorkBatchSize)
		if err != nil {
			if consumeCtx.Err() == nil {
				logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] dirty work list failed: %v", err)
				s.recordDirtyListFailure(consumeCtx)
			}
			return
		}
		s.clearDirtyListFailure()
		for _, item := range work {
			if consumeCtx.Err() != nil {
				return
			}
			err := s.handleDirtyWork(consumeCtx, item)
			if err != nil {
				recorded, recordErr := s.dirtyWorkRepo.RecordFailure(consumeCtx, ownership, item, err)
				if recordErr != nil && consumeCtx.Err() == nil {
					logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] dirty work failure recording failed: kind=%d entity=%d err=%v", item.Kind, item.EntityID, recordErr)
				} else if recorded {
					logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] dirty work failed: kind=%d entity=%d", item.Kind, item.EntityID)
				}
				continue
			}
			if _, err := s.dirtyWorkRepo.Acknowledge(consumeCtx, ownership, item); err != nil && consumeCtx.Err() == nil {
				logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] dirty work acknowledgement failed: kind=%d entity=%d err=%v", item.Kind, item.EntityID, err)
			}
		}
		s.checkDirtyWorkLag(consumeCtx)
	}

	poll()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			poll()
		case <-ownership.Lost():
			return
		case <-s.workerCtx.Done():
			return
		}
	}
}

func (s *SchedulerSnapshotService) handleDirtyWork(ctx context.Context, work SchedulerDirtyWork) error {
	switch work.Kind {
	case SchedulerDirtyWorkAccount:
		return s.refreshDirtyAccount(ctx, work.EntityID)
	case SchedulerDirtyWorkGroup:
		if work.EntityID == 0 {
			return s.rebuildByGroupIDs(ctx, []int64{0}, "dirty_global_bucket", nil)
		}
		return s.rebuildByGroupIDs(ctx, []int64{work.EntityID}, "dirty_group", nil)
	case SchedulerDirtyWorkGlobal:
		return s.triggerFullRebuildContext(ctx, "dirty_global")
	default:
		return errors.New("unknown scheduler dirty work kind")
	}
}

func (s *SchedulerSnapshotService) refreshDirtyAccount(ctx context.Context, accountID int64) error {
	if accountID <= 0 || s.accountRepo == nil {
		return nil
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		if errors.Is(err, ErrAccountNotFound) {
			if s.cache != nil {
				return s.cache.DeleteAccount(ctx, accountID)
			}
			return nil
		}
		return err
	}
	if s.cache != nil {
		return s.cache.SetAccount(ctx, account)
	}
	return nil
}

func (s *SchedulerSnapshotService) runOutboxWorker(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	s.pollOutbox()
	for {
		select {
		case <-ticker.C:
			s.pollOutbox()
		case <-s.stopCh:
			return
		}
	}
}

func (s *SchedulerSnapshotService) runFullRebuildWorker(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := s.triggerFullRebuild("interval"); err != nil {
				logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] full rebuild failed: %v", err)
			}
		case <-s.stopCh:
			return
		}
	}
}

func (s *SchedulerSnapshotService) pollOutbox() {
	if s.outboxRepo == nil || s.cache == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	watermark, err := s.cache.GetOutboxWatermark(ctx)
	if err != nil {
		logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] outbox watermark read failed: %v", err)
		return
	}

	events, err := s.outboxRepo.ListAfterAndReleaseDedup(ctx, watermark, 200)
	if err != nil {
		logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] outbox poll failed: %v", err)
		return
	}
	if len(events) == 0 {
		return
	}

	watermarkForCheck := watermark
	seen := make(map[batchSeenKey]struct{})
	for _, event := range events {
		eventCtx, cancel := context.WithTimeout(context.Background(), outboxEventTimeout)
		err := s.handleOutboxEvent(eventCtx, event, seen)
		cancel()
		if err != nil {
			logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] outbox handle failed: id=%d type=%s err=%v", event.ID, event.EventType, err)
			return
		}
	}

	lastID := watermark
	for _, event := range events {
		if event.ID > lastID {
			lastID = event.ID
		}
	}
	var wmErr error
	for i := range 3 {
		wmCtx, wmCancel := context.WithTimeout(context.Background(), 5*time.Second)
		wmErr = s.cache.SetOutboxWatermark(wmCtx, lastID)
		wmCancel()
		if wmErr == nil {
			break
		}
		if i < 2 {
			time.Sleep(200 * time.Millisecond)
		}
	}
	if wmErr != nil {
		logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] outbox watermark write failed: %v", wmErr)
	} else {
		watermarkForCheck = lastID
	}

	// Lifecycle degradation is measured from persistent dirty state once the
	// fenced consumer is active. Legacy ID arithmetic is not commit-order safe.
	if s.dirtyWorkRepo == nil || s.ownershipRepo == nil {
		s.checkOutboxLag(ctx, events[0], watermarkForCheck)
	}
}

func (s *SchedulerSnapshotService) handleOutboxEvent(ctx context.Context, event SchedulerOutboxEvent, seen map[batchSeenKey]struct{}) error {
	switch event.EventType {
	case SchedulerOutboxEventAccountLastUsed:
		return s.handleLastUsedEvent(ctx, event.Payload)
	case SchedulerOutboxEventFullRebuild:
		return s.triggerFullRebuildContext(ctx, "outbox")
	}

	// Lifecycle invalidation is transactionally captured by dirty source triggers.
	// During migration, drain legacy events without rebuilding the same scopes twice.
	if s.dirtyWorkRepo != nil && s.ownershipRepo != nil {
		return nil
	}

	switch event.EventType {
	case SchedulerOutboxEventAccountBulkChanged:
		return s.handleBulkAccountEvent(ctx, event.Payload, seen)
	case SchedulerOutboxEventAccountGroupsChanged:
		return s.handleAccountEvent(ctx, event.AccountID, event.Payload, seen)
	case SchedulerOutboxEventAccountChanged:
		return s.handleAccountEvent(ctx, event.AccountID, event.Payload, seen)
	case SchedulerOutboxEventGroupChanged:
		return s.handleGroupEvent(ctx, event.GroupID, seen)
	default:
		return nil
	}
}

func (s *SchedulerSnapshotService) handleLastUsedEvent(ctx context.Context, payload map[string]any) error {
	if s.cache == nil || payload == nil {
		return nil
	}
	raw, ok := payload["last_used"].(map[string]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	updates := make(map[int64]time.Time, len(raw))
	for key, value := range raw {
		id, err := strconv.ParseInt(key, 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		sec, ok := toInt64(value)
		if !ok || sec <= 0 {
			continue
		}
		updates[id] = time.Unix(sec, 0)
	}
	if len(updates) == 0 {
		return nil
	}
	return s.cache.UpdateLastUsed(ctx, updates)
}

func (s *SchedulerSnapshotService) handleBulkAccountEvent(ctx context.Context, payload map[string]any, seen map[batchSeenKey]struct{}) error {
	if payload == nil {
		return nil
	}
	if s.accountRepo == nil {
		return nil
	}

	rawIDs := parseInt64Slice(payload["account_ids"])
	if len(rawIDs) == 0 {
		return nil
	}

	ids := make([]int64, 0, len(rawIDs))
	seenIDs := make(map[int64]struct{}, len(rawIDs))
	for _, id := range rawIDs {
		if id <= 0 {
			continue
		}
		if _, exists := seenIDs[id]; exists {
			continue
		}
		seenIDs[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil
	}

	preloadGroupIDs := parseInt64Slice(payload["group_ids"])
	accounts, err := s.accountRepo.GetByIDs(ctx, ids)
	if err != nil {
		return err
	}

	found := make(map[int64]struct{}, len(accounts))
	rebuildGroupSet := make(map[int64]struct{}, len(preloadGroupIDs))
	for _, gid := range preloadGroupIDs {
		if gid > 0 {
			rebuildGroupSet[gid] = struct{}{}
		}
	}

	for _, account := range accounts {
		if account == nil || account.ID <= 0 {
			continue
		}
		found[account.ID] = struct{}{}
		if s.cache != nil {
			if err := s.cache.SetAccount(ctx, account); err != nil {
				return err
			}
		}
		for _, gid := range account.GroupIDs {
			if gid > 0 {
				rebuildGroupSet[gid] = struct{}{}
			}
		}
	}

	if s.cache != nil {
		for _, id := range ids {
			if _, ok := found[id]; ok {
				continue
			}
			if err := s.cache.DeleteAccount(ctx, id); err != nil {
				return err
			}
		}
	}

	rebuildGroupIDs := make([]int64, 0, len(rebuildGroupSet))
	for gid := range rebuildGroupSet {
		rebuildGroupIDs = append(rebuildGroupIDs, gid)
	}
	return s.rebuildByGroupIDs(ctx, rebuildGroupIDs, "account_bulk_change", seen)
}

func (s *SchedulerSnapshotService) handleAccountEvent(ctx context.Context, accountID *int64, payload map[string]any, seen map[batchSeenKey]struct{}) error {
	if accountID == nil || *accountID <= 0 {
		return nil
	}
	if s.accountRepo == nil {
		return nil
	}

	var groupIDs []int64
	if payload != nil {
		groupIDs = parseInt64Slice(payload["group_ids"])
	}

	account, err := s.accountRepo.GetByID(ctx, *accountID)
	if err != nil {
		if errors.Is(err, ErrAccountNotFound) {
			if s.cache != nil {
				if err := s.cache.DeleteAccount(ctx, *accountID); err != nil {
					return err
				}
			}
			return s.rebuildByGroupIDs(ctx, groupIDs, "account_miss", seen)
		}
		return err
	}
	if s.cache != nil {
		if err := s.cache.SetAccount(ctx, account); err != nil {
			return err
		}
	}
	if len(groupIDs) == 0 {
		groupIDs = account.GroupIDs
	}
	return s.rebuildByAccount(ctx, account, groupIDs, "account_change", seen)
}

func (s *SchedulerSnapshotService) handleGroupEvent(ctx context.Context, groupID *int64, seen map[batchSeenKey]struct{}) error {
	if groupID == nil || *groupID <= 0 {
		return nil
	}
	groupIDs := []int64{*groupID}
	return s.rebuildByGroupIDs(ctx, groupIDs, "group_change", seen)
}

func (s *SchedulerSnapshotService) rebuildByAccount(ctx context.Context, account *Account, groupIDs []int64, reason string, seen map[batchSeenKey]struct{}) error {
	if account == nil {
		return nil
	}
	groupIDs = s.normalizeGroupIDs(groupIDs)
	if len(groupIDs) == 0 {
		return nil
	}

	var firstErr error
	if err := s.rebuildBucketsForPlatform(ctx, account.Platform, groupIDs, reason, seen); err != nil && firstErr == nil {
		firstErr = err
	}
	if account.Platform == PlatformAntigravity && account.IsMixedSchedulingEnabled() {
		if err := s.rebuildBucketsForPlatform(ctx, PlatformAnthropic, groupIDs, reason, seen); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := s.rebuildBucketsForPlatform(ctx, PlatformGemini, groupIDs, reason, seen); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (s *SchedulerSnapshotService) rebuildByGroupIDs(ctx context.Context, groupIDs []int64, reason string, seen map[batchSeenKey]struct{}) error {
	groupIDs = s.normalizeGroupIDs(groupIDs)
	if len(groupIDs) == 0 {
		return nil
	}
	platforms := []string{PlatformAnthropic, PlatformGemini, PlatformOpenAI, PlatformAntigravity}
	var firstErr error
	for _, platform := range platforms {
		if err := s.rebuildBucketsForPlatform(ctx, platform, groupIDs, reason, seen); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (s *SchedulerSnapshotService) rebuildBucketsForPlatform(ctx context.Context, platform string, groupIDs []int64, reason string, seen map[batchSeenKey]struct{}) error {
	if platform == "" {
		return nil
	}
	var firstErr error
	for _, gid := range groupIDs {
		// Within a single poll batch, skip (groupID, platform) pairs that were
		// already rebuilt. The first rebuild loads fresh DB data for all accounts
		// in the group, so subsequent rebuilds for the same group+platform within
		// the same batch are redundant.
		if seen != nil {
			key := batchSeenKey{gid, platform}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
		}
		if err := s.rebuildBucket(ctx, SchedulerBucket{GroupID: gid, Platform: platform, Mode: SchedulerModeSingle}, reason); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := s.rebuildBucket(ctx, SchedulerBucket{GroupID: gid, Platform: platform, Mode: SchedulerModeForced}, reason); err != nil && firstErr == nil {
			firstErr = err
		}
		if platform == PlatformAnthropic || platform == PlatformGemini {
			if err := s.rebuildBucket(ctx, SchedulerBucket{GroupID: gid, Platform: platform, Mode: SchedulerModeMixed}, reason); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (s *SchedulerSnapshotService) rebuildBuckets(ctx context.Context, buckets []SchedulerBucket, reason string) error {
	var firstErr error
	for _, bucket := range buckets {
		if err := s.rebuildBucket(ctx, bucket, reason); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (s *SchedulerSnapshotService) rebuildBucket(ctx context.Context, bucket SchedulerBucket, reason string) error {
	if s.cache == nil {
		return ErrSchedulerCacheNotReady
	}
	// Keep the lease strictly longer than the bounded rebuild. Equal deadlines can
	// let a successor acquire while the first writer is still finishing Redis I/O.
	lockToken, ok, err := s.cache.TryLockBucket(ctx, bucket, schedulerBucketLockTTL)
	if err != nil {
		return err
	}
	if !ok {
		return errSchedulerBucketLockBusy
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = s.cache.UnlockBucket(unlockCtx, bucket, lockToken)
	}()

	rebuildCtx, cancel := context.WithTimeout(ctx, schedulerBucketRebuildLimit)
	defer cancel()

	accounts, err := s.loadAccountsFromDB(rebuildCtx, bucket, bucket.Mode == SchedulerModeMixed)
	if err != nil {
		logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] rebuild failed: bucket=%s reason=%s err=%v", bucket.String(), reason, err)
		return err
	}
	if err := s.cache.SetSnapshot(rebuildCtx, bucket, accounts); err != nil {
		logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] rebuild cache failed: bucket=%s reason=%s err=%v", bucket.String(), reason, err)
		return err
	}
	slog.Debug("[Scheduler] rebuild ok", "bucket", bucket.String(), "reason", reason, "size", len(accounts))
	return nil
}

func (s *SchedulerSnapshotService) triggerFullRebuild(reason string) error {
	return s.triggerFullRebuildContext(context.Background(), reason)
}

func (s *SchedulerSnapshotService) triggerFullRebuildContext(ctx context.Context, reason string) error {
	if s.cache == nil {
		return ErrSchedulerCacheNotReady
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	buckets, err := s.cache.ListBuckets(ctx)
	if err != nil {
		logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] list buckets failed: %v", err)
		return err
	}
	if len(buckets) == 0 {
		buckets, err = s.defaultBuckets(ctx)
		if err != nil {
			logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] default buckets failed: %v", err)
			return err
		}
	}
	return s.rebuildBuckets(ctx, buckets, reason)
}

func (s *SchedulerSnapshotService) recordDirtyListFailure(ctx context.Context) {
	if s.dirtyWorkRepo == nil || s.cfg == nil {
		return
	}
	threshold := s.cfg.Gateway.Scheduling.OutboxLagRebuildFailures
	if threshold <= 0 {
		return
	}

	s.lagMu.Lock()
	if s.dirtyListRebuildLatched {
		s.lagMu.Unlock()
		return
	}
	s.dirtyListFailures++
	failures := s.dirtyListFailures
	if failures < threshold {
		s.lagMu.Unlock()
		return
	}
	// The request is durable canonical work. Latch the incident so a persistent
	// listing outage does not advance the global generation on every poll.
	s.dirtyListRebuildLatched = true
	s.dirtyListFailures = 0
	s.lagMu.Unlock()

	if err := s.dirtyWorkRepo.RequestFullRebuild(ctx); err != nil {
		s.lagMu.Lock()
		s.dirtyListRebuildLatched = false
		s.lagMu.Unlock()
		if ctx.Err() == nil {
			logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] dirty work list failure rebuild request failed: %v", err)
		}
		return
	}
	logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] dirty work list failure rebuild requested: failures=%d", failures)
}

func (s *SchedulerSnapshotService) clearDirtyListFailure() {
	s.lagMu.Lock()
	s.dirtyListFailures = 0
	s.dirtyListRebuildLatched = false
	s.lagMu.Unlock()
}

func (s *SchedulerSnapshotService) checkDirtyWorkLag(ctx context.Context) {
	if s.dirtyWorkRepo == nil || s.cfg == nil {
		return
	}
	stats, err := s.dirtyWorkRepo.PendingStats(ctx)
	if err != nil {
		if ctx.Err() == nil {
			logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] dirty work stats failed: %v", err)
		}
		return
	}

	lagSeconds := 0
	if stats.OldestUpdatedAt != nil {
		lagSeconds = max(0, int(time.Since(*stats.OldestUpdatedAt).Seconds()))
	}
	warnSeconds := s.cfg.Gateway.Scheduling.OutboxLagWarnSeconds
	if warnSeconds > 0 && lagSeconds >= warnSeconds {
		logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] dirty work lag warning: lag=%ds pending=%d failed=%d", lagSeconds, stats.Count, stats.FailedCount)
	}

	rebuildSeconds := s.cfg.Gateway.Scheduling.OutboxLagRebuildSeconds
	backlogRows := s.cfg.Gateway.Scheduling.OutboxBacklogRebuildRows
	degraded := rebuildSeconds > 0 && lagSeconds >= rebuildSeconds
	if backlogRows > 0 && stats.Count >= int64(backlogRows) {
		degraded = true
	}

	s.lagMu.Lock()
	if !degraded {
		s.dirtyLagFailures = 0
		s.dirtyRebuildLatched = false
		s.lagMu.Unlock()
		return
	}
	if s.dirtyRebuildLatched {
		s.lagMu.Unlock()
		return
	}
	s.dirtyLagFailures++
	failures := s.dirtyLagFailures
	threshold := s.cfg.Gateway.Scheduling.OutboxLagRebuildFailures
	if failures < threshold {
		s.lagMu.Unlock()
		return
	}
	// Latch before enqueueing: one persistent degraded interval should create one
	// coalesced generation, not a new global generation on every poll.
	s.dirtyRebuildLatched = true
	s.dirtyLagFailures = 0
	s.lagMu.Unlock()

	if err := s.dirtyWorkRepo.RequestFullRebuild(ctx); err != nil {
		s.lagMu.Lock()
		s.dirtyRebuildLatched = false
		s.lagMu.Unlock()
		if ctx.Err() == nil {
			logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] dirty work degraded rebuild request failed: %v", err)
		}
		return
	}
	logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] dirty work degraded rebuild requested: lag=%ds pending=%d failed=%d", lagSeconds, stats.Count, stats.FailedCount)
}

func (s *SchedulerSnapshotService) checkOutboxLag(ctx context.Context, oldest SchedulerOutboxEvent, watermark int64) {
	if oldest.CreatedAt.IsZero() || s.cfg == nil {
		return
	}

	lag := time.Since(oldest.CreatedAt)
	if lagSeconds := int(lag.Seconds()); lagSeconds >= s.cfg.Gateway.Scheduling.OutboxLagWarnSeconds && s.cfg.Gateway.Scheduling.OutboxLagWarnSeconds > 0 {
		logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] outbox lag warning: %ds", lagSeconds)
	}

	if s.cfg.Gateway.Scheduling.OutboxLagRebuildSeconds > 0 && int(lag.Seconds()) >= s.cfg.Gateway.Scheduling.OutboxLagRebuildSeconds {
		s.lagMu.Lock()
		s.lagFailures++
		failures := s.lagFailures
		s.lagMu.Unlock()

		if failures >= s.cfg.Gateway.Scheduling.OutboxLagRebuildFailures {
			logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] outbox lag rebuild triggered: lag=%s failures=%d", lag, failures)
			s.lagMu.Lock()
			s.lagFailures = 0
			s.lagMu.Unlock()
			if err := s.triggerFullRebuild("outbox_lag"); err != nil {
				logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] outbox lag rebuild failed: %v", err)
			}
		}
	} else {
		s.lagMu.Lock()
		s.lagFailures = 0
		s.lagMu.Unlock()
	}

	threshold := s.cfg.Gateway.Scheduling.OutboxBacklogRebuildRows
	if threshold <= 0 || s.outboxRepo == nil {
		return
	}
	maxID, err := s.outboxRepo.MaxID(ctx)
	if err != nil {
		return
	}
	if maxID-watermark >= int64(threshold) {
		logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] outbox backlog rebuild triggered: backlog=%d", maxID-watermark)
		if err := s.triggerFullRebuild("outbox_backlog"); err != nil {
			logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] outbox backlog rebuild failed: %v", err)
		}
	}
}

func (s *SchedulerSnapshotService) loadAccountsFromDB(ctx context.Context, bucket SchedulerBucket, useMixed bool) ([]Account, error) {
	if s.accountRepo == nil {
		return nil, ErrSchedulerCacheNotReady
	}
	groupID := bucket.GroupID
	if s.isRunModeSimple() {
		groupID = 0
	}

	if useMixed {
		platforms := []string{bucket.Platform, PlatformAntigravity}
		var accounts []Account
		var err error
		if groupID > 0 {
			accounts, err = s.accountRepo.ListSchedulableByGroupIDAndPlatforms(ctx, groupID, platforms)
		} else if s.isRunModeSimple() {
			accounts, err = s.accountRepo.ListSchedulableByPlatforms(ctx, platforms)
		} else {
			accounts, err = s.accountRepo.ListSchedulableUngroupedByPlatforms(ctx, platforms)
		}
		if err != nil {
			return nil, err
		}
		filtered := make([]Account, 0, len(accounts))
		for _, acc := range accounts {
			if acc.Platform == PlatformAntigravity && !acc.IsMixedSchedulingEnabled() {
				continue
			}
			filtered = append(filtered, acc)
		}
		return filtered, nil
	}

	if groupID > 0 {
		return s.accountRepo.ListSchedulableByGroupIDAndPlatform(ctx, groupID, bucket.Platform)
	}
	if s.isRunModeSimple() {
		return s.accountRepo.ListSchedulableByPlatform(ctx, bucket.Platform)
	}
	return s.accountRepo.ListSchedulableUngroupedByPlatform(ctx, bucket.Platform)
}

func (s *SchedulerSnapshotService) bucketFor(groupID *int64, platform string, mode string) SchedulerBucket {
	return SchedulerBucket{
		GroupID:  s.normalizeGroupID(groupID),
		Platform: platform,
		Mode:     mode,
	}
}

func (s *SchedulerSnapshotService) normalizeGroupID(groupID *int64) int64 {
	if s.isRunModeSimple() {
		return 0
	}
	if groupID == nil || *groupID <= 0 {
		return 0
	}
	return *groupID
}

func (s *SchedulerSnapshotService) normalizeGroupIDs(groupIDs []int64) []int64 {
	if s.isRunModeSimple() {
		return []int64{0}
	}
	if len(groupIDs) == 0 {
		return []int64{0}
	}
	seen := make(map[int64]struct{}, len(groupIDs))
	out := make([]int64, 0, len(groupIDs))
	for _, id := range groupIDs {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	if len(out) == 0 {
		return []int64{0}
	}
	return out
}

func (s *SchedulerSnapshotService) resolveMode(platform string, hasForcePlatform bool) string {
	if hasForcePlatform {
		return SchedulerModeForced
	}
	if platform == PlatformAnthropic || platform == PlatformGemini {
		return SchedulerModeMixed
	}
	return SchedulerModeSingle
}

func (s *SchedulerSnapshotService) guardFallback(ctx context.Context) error {
	if s.cfg == nil || s.cfg.Gateway.Scheduling.DbFallbackEnabled {
		if s.fallbackLimit == nil || s.fallbackLimit.Allow() {
			return nil
		}
		return ErrSchedulerFallbackLimited
	}
	return ErrSchedulerCacheNotReady
}

func (s *SchedulerSnapshotService) withFallbackTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if s.cfg == nil || s.cfg.Gateway.Scheduling.DbFallbackTimeoutSeconds <= 0 {
		return context.WithCancel(ctx)
	}
	timeout := time.Duration(s.cfg.Gateway.Scheduling.DbFallbackTimeoutSeconds) * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return context.WithCancel(ctx)
		}
		if remaining < timeout {
			timeout = remaining
		}
	}
	return context.WithTimeout(ctx, timeout)
}

func (s *SchedulerSnapshotService) isRunModeSimple() bool {
	return s.cfg != nil && s.cfg.RunMode == config.RunModeSimple
}

func (s *SchedulerSnapshotService) outboxPollInterval() time.Duration {
	if s.cfg == nil {
		return time.Second
	}
	sec := s.cfg.Gateway.Scheduling.OutboxPollIntervalSeconds
	if sec <= 0 {
		return time.Second
	}
	return time.Duration(sec) * time.Second
}

func (s *SchedulerSnapshotService) fullRebuildInterval() time.Duration {
	if s.cfg == nil {
		return 0
	}
	sec := s.cfg.Gateway.Scheduling.FullRebuildIntervalSeconds
	if sec <= 0 {
		return 0
	}
	return time.Duration(sec) * time.Second
}

func (s *SchedulerSnapshotService) defaultBuckets(ctx context.Context) ([]SchedulerBucket, error) {
	buckets := make([]SchedulerBucket, 0)
	platforms := []string{PlatformAnthropic, PlatformGemini, PlatformOpenAI, PlatformAntigravity}
	for _, platform := range platforms {
		buckets = append(buckets, SchedulerBucket{GroupID: 0, Platform: platform, Mode: SchedulerModeSingle})
		buckets = append(buckets, SchedulerBucket{GroupID: 0, Platform: platform, Mode: SchedulerModeForced})
		if platform == PlatformAnthropic || platform == PlatformGemini {
			buckets = append(buckets, SchedulerBucket{GroupID: 0, Platform: platform, Mode: SchedulerModeMixed})
		}
	}

	if s.isRunModeSimple() || s.groupRepo == nil {
		return dedupeBuckets(buckets), nil
	}

	groups, err := s.groupRepo.ListActive(ctx)
	if err != nil {
		return dedupeBuckets(buckets), nil
	}
	for _, group := range groups {
		if group.Platform == "" {
			continue
		}
		buckets = append(buckets, SchedulerBucket{GroupID: group.ID, Platform: group.Platform, Mode: SchedulerModeSingle})
		buckets = append(buckets, SchedulerBucket{GroupID: group.ID, Platform: group.Platform, Mode: SchedulerModeForced})
		if group.Platform == PlatformAnthropic || group.Platform == PlatformGemini {
			buckets = append(buckets, SchedulerBucket{GroupID: group.ID, Platform: group.Platform, Mode: SchedulerModeMixed})
		}
	}
	return dedupeBuckets(buckets), nil
}

func dedupeBuckets(in []SchedulerBucket) []SchedulerBucket {
	seen := make(map[string]struct{}, len(in))
	out := make([]SchedulerBucket, 0, len(in))
	for _, bucket := range in {
		key := bucket.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, bucket)
	}
	return out
}

func derefAccounts(accounts []*Account) []Account {
	if len(accounts) == 0 {
		return []Account{}
	}
	out := make([]Account, 0, len(accounts))
	for _, account := range accounts {
		if account == nil {
			continue
		}
		out = append(out, *account)
	}
	return out
}

func parseInt64Slice(value any) []int64 {
	raw, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]int64, 0, len(raw))
	for _, item := range raw {
		if v, ok := toInt64(item); ok && v > 0 {
			out = append(out, v)
		}
	}
	return out
}

func toInt64(value any) (int64, bool) {
	switch v := value.(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case int:
		return int64(v), true
	case json.Number:
		parsed, err := strconv.ParseInt(v.String(), 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

type fallbackLimiter struct {
	maxQPS int
	mu     sync.Mutex
	window time.Time
	count  int
}

func newFallbackLimiter(maxQPS int) *fallbackLimiter {
	if maxQPS <= 0 {
		return nil
	}
	return &fallbackLimiter{
		maxQPS: maxQPS,
		window: time.Now(),
	}
}

func (l *fallbackLimiter) Allow() bool {
	if l == nil || l.maxQPS <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	if now.Sub(l.window) >= time.Second {
		l.window = now
		l.count = 0
	}
	if l.count >= l.maxQPS {
		return false
	}
	l.count++
	return true
}
