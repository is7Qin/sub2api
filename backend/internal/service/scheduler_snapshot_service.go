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
	"golang.org/x/sync/singleflight"
)

var (
	ErrSchedulerCacheNotReady   = errors.New("scheduler cache not ready")
	ErrSchedulerFallbackLimited = errors.New("scheduler db fallback limited")
	errSchedulerBucketLockBusy  = errors.New("scheduler bucket rebuild lock contended")
	// errSchedulerRebuildRetryPending 表示全量重建处于失败退避窗口内；dirty 消费端
	// 据此让 Global 项保持挂起，而不是每轮 poll 立即重试执行重建。
	errSchedulerRebuildRetryPending = errors.New("scheduler rebuild retry pending")
)

const (
	outboxEventTimeout          = 2 * time.Minute
	dirtyWorkBatchSize          = 100
	schedulerBucketRebuildLimit = 30 * time.Second
	schedulerBucketLockTTL      = schedulerBucketRebuildLimit + 5*time.Second
	// outboxRebuildRetryBaseDelay/outboxRebuildRetryMaxDelay 控制重建失败后的
	// 指数退避：5s 起、每次失败翻倍、5min 封顶，防止一秒一轮的 poll 把失败重建
	// 变成重建失败→请求回源→DB 过载→重建再失败的风暴。
	outboxRebuildRetryBaseDelay = 5 * time.Second
	outboxRebuildRetryMaxDelay  = 5 * time.Minute

	// snapshotStatsLogInterval 调度快照 GetSnapshot 命中/未命中观测日志的聚合窗口。
	snapshotStatsLogInterval = 10 * time.Second
)

// batchSeenKey tracks which (groupID, platform) bucket sets have already been
// rebuilt within a single pollOutbox call, to avoid redundant work when multiple
// account_changed events share the same groups.
type batchSeenKey struct {
	groupID  int64
	platform string
}

// schedulerAccountQueryKey 标识一次可跨分桶复用的账号查询。
type schedulerAccountQueryKey struct {
	groupID  int64
	platform string
}

// 查询结果只在一次 rebuild batch 内，按原始 groupID+platform 复用成功的 single/forced 查询；
// mixed 与其他模式保持独立。每个桶都用 defer 消费 remaining，最后一个消费者会立即释放结果，
// 避免把账号切片的生命周期扩大到整轮 full rebuild。
type schedulerAccountQueryCache struct {
	remaining          map[schedulerAccountQueryKey]int
	accounts           map[schedulerAccountQueryKey][]Account
	snapshotAccountIDs map[schedulerAccountQueryKey][]int64
}

// schedulerSnapshotAccountIDWriter 是 SchedulerCache 的可选批次优化能力。
// 首次完整发布成功后返回实际可编码账号 ID；同一查询结果的后续桶只需发布这些 ID，
// 避免重复序列化并覆盖全局账号缓存。未实现该接口的缓存继续走原 SetSnapshot 路径。
type schedulerSnapshotAccountIDWriter interface {
	SetSnapshotAndReturnAccountIDs(ctx context.Context, bucket SchedulerBucket, accounts []Account) ([]int64, error)
	SetSnapshotByAccountIDs(ctx context.Context, bucket SchedulerBucket, accountIDs []int64) error
}

func newSchedulerAccountQueryCache(bucketSets ...[]SchedulerBucket) *schedulerAccountQueryCache {
	queries := &schedulerAccountQueryCache{
		remaining:          make(map[schedulerAccountQueryKey]int),
		accounts:           make(map[schedulerAccountQueryKey][]Account),
		snapshotAccountIDs: make(map[schedulerAccountQueryKey][]int64),
	}
	for _, buckets := range bucketSets {
		for _, bucket := range buckets {
			if key, ok := schedulerAccountQueryKeyForBucket(bucket); ok {
				queries.remaining[key]++
			}
		}
	}
	return queries
}

func schedulerAccountQueryKeyForBucket(bucket SchedulerBucket) (schedulerAccountQueryKey, bool) {
	if bucket.Mode != SchedulerModeSingle && bucket.Mode != SchedulerModeForced {
		return schedulerAccountQueryKey{}, false
	}
	return schedulerAccountQueryKey{groupID: bucket.GroupID, platform: bucket.Platform}, true
}

func (c *schedulerAccountQueryCache) release(bucket SchedulerBucket) {
	if c == nil {
		return
	}
	key, ok := schedulerAccountQueryKeyForBucket(bucket)
	if !ok {
		return
	}
	remaining := c.remaining[key] - 1
	if remaining <= 0 {
		delete(c.remaining, key)
		delete(c.accounts, key)
		delete(c.snapshotAccountIDs, key)
		return
	}
	c.remaining[key] = remaining
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
	workerCtx               context.Context // retained for focused legacy construction compatibility; not used for dirty ownership
	workerCancel            context.CancelFunc
	wg                      sync.WaitGroup
	fallbackLimit           *fallbackLimiter
	lagMu                   sync.Mutex
	lagFailures             int
	dirtyLagFailures        int
	dirtyRebuildLatched     bool
	dirtyListFailures       int
	dirtyListRebuildLatched bool
	// 重建失败退避状态：outbox/dirty 触发的全量重建失败后，下一次尝试推迟到
	// outboxRebuildRetryAt，延迟随 outboxRebuildFailures 指数增长（5s 起、5min 封顶）。
	outboxRebuildFailures    int
	outboxRebuildRetryAt     time.Time
	outboxRebuildRetryReason string
	// 快照解码缓存：GetSnapshot 每次调用都对整个桶的账号做 JSON 解码
	// （大账号池下 O(N) 且与请求成功率无关），高频网关请求下是 CPU 主源。
	// 命中时免解码；版本可用时以 active version 失效为主、TTL 兜底，
	// 版本不可用时退化为短 TTL（见下方两个 TTL 常量）；调度器对快照账号只读使用。
	decodeCache sync.Map // bucket.String() -> *snapshotDecodeCacheEntry
	// snapshotVersionCacheTTL 分桶激活版本的本地读取窗口：解码缓存命中路径在
	// 窗口内直接复用上次读到的版本，避免每个请求一次 Redis 版本 GET；窗口过期
	// 后下一次命中才重新读取（每个分桶每秒最多一次版本往返）。0/负值表示每次
	// 调用都重新读 Redis（测试直接构造结构体时保持旧行为）。
	snapshotVersionCacheTTL time.Duration
	// snapshotVersionCache 记录每个分桶最近一次成功的版本读取（仅非空版本）。
	snapshotVersionCache sync.Map // bucket.String() -> *snapshotVersionCacheEntry
	// snapshotFallbackGroup 按分桶合并快照 miss 后的 DB 回源：同一时刻一个分桶
	// 只允许一个请求实际查询数据库并写回快照，其余请求等待其结果。
	snapshotFallbackGroup singleflight.Group
	// dirtyRefreshThrottle 限制同一账号的脏刷新频率（见 dirtyAccountRefreshMinInterval）。
	dirtyRefreshThrottle *accountWriteThrottle
}

// snapshotDecodeCacheTTL 是版本不可用（接口缺失或版本读取失败）时快照解码缓存
// 的保留时间。5 秒内的过期由候选 freshness recheck 兜底，不影响正确性。
const snapshotDecodeCacheTTL = 5 * time.Second

// snapshotDecodeVersionedTTL 是版本可用的快照解码缓存保留时间。重建后 active
// 版本立即递增使缓存失效，TTL 只是版本丢失时的兜底上限，不再承担主要失效职责。
const snapshotDecodeVersionedTTL = 30 * time.Second

// snapshotVersionCacheWindow 分桶激活版本的本地缓存窗口。解码缓存命中路径不再
// 每个请求读一次 Redis 版本：窗口内直接用本地版本判断条目是否仍有效（零往返），
// 重建导致的版本递增最多滞后一个窗口即被感知并失效本地条目。失效延迟因此
// 从“立即”放宽为 ≤snapshotVersionCacheWindow，代价是每个分桶每秒最多一次版本
// GET（而非每个请求一次），且条目 TTL 始终是陈旧性的最终兜底。
const snapshotVersionCacheWindow = time.Second

type snapshotDecodeCacheEntry struct {
	accounts []*Account
	version  string
	exp      time.Time
}

// snapshotVersionCacheEntry 记录一次成功的版本读取时间，供解码缓存命中路径
// 判断是否可直接复用本地版本（窗口内）而非重新向 Redis 读取。
type snapshotVersionCacheEntry struct {
	version string
	readAt  time.Time
}

// snapshotVersionReader 是可选接口：解码缓存通过它读取分桶激活版本，重建后立即
// 失效本地条目。cache 未实现时（仅测试 stub 或第三方实现）退化为 TTL 兜底。
type snapshotVersionReader interface {
	GetSnapshotVersion(ctx context.Context, bucket SchedulerBucket) (string, error)
}

// snapshotAccountBatchReader 是可选接口：网关批量刷新候选账号时通过它直接读
// Redis 快照全量 payload（秒级更新 + 限流状态实时写回），避免逐请求回源 DB。
// cache 未实现时（仅测试 stub 或第三方实现）由调用方降级 DB 查询。
type snapshotAccountBatchReader interface {
	GetSchedulableAccountsByIDs(ctx context.Context, ids []int64) (map[int64]*Account, error)
}

// schedulerAccountBatchWriter 是 SchedulerCache 的可选批量写入能力：dirty 工作
// 消费端把整批脏账号合并成一次 Redis 管线写入，避免逐账号往返。cache 未实现
// 时（仅测试 stub 或第三方实现）退化为逐账号 SetAccount。
type schedulerAccountBatchWriter interface {
	SetAccounts(ctx context.Context, accounts []Account) error
}

// dirtyAccountRefreshMinInterval 同一账号两次脏刷新之间的最小间隔。1s 轮询下
// 高频重复脏化的账号（例如批量生命周期变更）最多每秒全字段刷新一次，避免
// “每轮 poll 都 3 条 SELECT + ~9KB Redis 写入”的放大。
const dirtyAccountRefreshMinInterval = time.Second

// snapshotStatsReporter 是可选接口：缓存实现时由统计 worker 周期输出
// GetSnapshot 命中/未命中观测日志；未实现（仅测试 stub）时静默跳过。
type snapshotStatsReporter interface {
	LogSnapshotStats()
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
		cache:                   cache,
		outboxRepo:              outboxRepo,
		dirtyWorkRepo:           dirtyWorkRepo,
		ownershipRepo:           ownershipRepo,
		accountRepo:             accountRepo,
		groupRepo:               groupRepo,
		cfg:                     cfg,
		stopCh:                  make(chan struct{}),
		workerCtx:               workerCtx,
		workerCancel:            workerCancel,
		fallbackLimit:           newFallbackLimiter(maxQPS),
		dirtyRefreshThrottle:    newAccountWriteThrottle(dirtyAccountRefreshMinInterval),
		snapshotVersionCacheTTL: snapshotVersionCacheWindow,
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

	// GetSnapshot 命中/未命中观测日志独立于重建与消费 worker，10s 一个窗口聚合输出。
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.runSnapshotStatsWorker()
	}()
}

// runSnapshotStatsWorker 周期输出调度快照 GetSnapshot 命中/未命中观测日志。
// cache 未实现 snapshotStatsReporter（仅测试 stub）时立即返回。
func (s *SchedulerSnapshotService) runSnapshotStatsWorker() {
	reporter, ok := s.cache.(snapshotStatsReporter)
	if !ok {
		return
	}
	ticker := time.NewTicker(snapshotStatsLogInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			reporter.LogSnapshotStats()
		case <-s.stopCh:
			return
		}
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
		// 解码缓存命中路径不再每个请求先读一次 Redis 激活版本：条目在存储时已
		// 用当时的 active 版本校验，陈旧性由条目 TTL 兜底（见 snapshotDecodeCacheTTL
		// / snapshotDecodeVersionedTTL）；重建导致的版本递增经本地版本缓存窗口
		// 感知，最多滞后 snapshotVersionCacheWindow 即失效重取，窗口内命中为
		// 纯本地判断（零 Redis 往返）。
		version := ""
		if entry, ok := s.decodeCache.Load(cacheKey); ok {
			if e, ok := entry.(*snapshotDecodeCacheEntry); ok && time.Now().Before(e.exp) {
				if v, fresh := s.cachedSnapshotVersion(cacheKey, time.Now()); fresh {
					if e.version == v {
						return derefAccounts(e.accounts), useMixed, nil
					}
					version = v
				} else if v := s.readSnapshotVersion(ctx, bucket); v != "" {
					s.storeSnapshotVersion(cacheKey, v, time.Now())
					version = v
					if e.version == v {
						return derefAccounts(e.accounts), useMixed, nil
					}
				} else if e.version == "" {
					// 版本不可读时只允许命中无版本条目：带版本号的条目可能
					// 对应重建前的旧账号集合，命中会穿透版本失效逻辑。
					return derefAccounts(e.accounts), useMixed, nil
				}
			}
		}
		// 解码缓存未命中（或版本不一致）才读激活版本：重建后 active 版本立即
		// 递增，据此失效本地解码缓存并选择条目 TTL。版本不可用（接口缺失/
		// 读取失败）时退化为纯 TTL 兜底。
		if version == "" {
			version = s.readSnapshotVersion(ctx, bucket)
			if version != "" {
				s.storeSnapshotVersion(cacheKey, version, time.Now())
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

	// 快照 miss 后的 DB 回源是全桶共享的慢路径：按分桶 singleflight 合并并发
	// 请求，同一时刻只有一个请求实际执行 DB 查询 + SetSnapshot 写回，其余请求
	// 等待同一结果（回源失败时共享同一错误，下一请求自然重试）。leader 的 DB
	// 工作使用脱离调用方取消的上下文，避免首个请求断连把整批等待者一起拖垮；
	// 执行时长由 fallbackQueryContext 施加的硬性上限限制。
	value, err, _ := s.snapshotFallbackGroup.Do(bucket.String(), func() (any, error) {
		if err := s.guardFallback(ctx); err != nil {
			return nil, err
		}
		fallbackCtx, cancel := s.fallbackQueryContext(ctx)
		defer cancel()

		accounts, err := s.loadAccountsFromDB(fallbackCtx, bucket, useMixed)
		if err != nil {
			return nil, err
		}

		if s.cache != nil {
			if err := s.cache.SetSnapshot(fallbackCtx, bucket, accounts); err != nil {
				logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] cache write failed: bucket=%s err=%v", bucket.String(), err)
			}
		}

		return accounts, nil
	})
	if err != nil {
		return nil, useMixed, err
	}
	accounts, _ := value.([]Account)
	return accounts, useMixed, nil
}

// cachedSnapshotVersion 返回本地版本缓存中窗口内的版本；ttl<=0 时总是返回
// fresh=false（每次都重新读 Redis，测试直接构造结构体时保持旧行为）。
func (s *SchedulerSnapshotService) cachedSnapshotVersion(cacheKey string, now time.Time) (string, bool) {
	if s.snapshotVersionCacheTTL <= 0 {
		return "", false
	}
	if raw, ok := s.snapshotVersionCache.Load(cacheKey); ok {
		if e, ok := raw.(*snapshotVersionCacheEntry); ok && e.version != "" && now.Sub(e.readAt) < s.snapshotVersionCacheTTL {
			return e.version, true
		}
	}
	return "", false
}

// storeSnapshotVersion 记录一次成功的版本读取；空版本与窗口关闭（ttl<=0）时
// 不缓存，保证本地缓存只含“窗口内可复用的真实版本”。
func (s *SchedulerSnapshotService) storeSnapshotVersion(cacheKey, version string, now time.Time) {
	if s.snapshotVersionCacheTTL <= 0 || version == "" {
		return
	}
	s.snapshotVersionCache.Store(cacheKey, &snapshotVersionCacheEntry{version: version, readAt: now})
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

// GetSchedulableAccountsByIDs 从快照批量读取账号调度元数据（meta payload）；
// 缺失的 ID 不在返回 map 中（与“未在刷新 map 中视为已删除”的调用方语义一致）。
// cache 缺失或未实现批量读（可选接口）时返回错误，由调用方降级 DB 查询。
func (s *SchedulerSnapshotService) GetSchedulableAccountsByIDs(ctx context.Context, ids []int64) (map[int64]*Account, error) {
	if s == nil || s.cache == nil {
		return nil, ErrSchedulerCacheNotReady
	}
	reader, ok := s.cache.(snapshotAccountBatchReader)
	if !ok {
		return nil, ErrSchedulerCacheNotReady
	}
	return reader.GetSchedulableAccountsByIDs(ctx, ids)
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

type SchedulerDirtyWorkResult struct {
	Work SchedulerDirtyWork
	Err  error
}

type SchedulerSnapshotDirtyProcessor interface {
	ApplyDirtyWorkBatch(ctx context.Context, work []SchedulerDirtyWork) []SchedulerDirtyWorkResult
}

// ApplyDirtyWorkBatch applies only snapshot/cache effects. Dirty ownership,
// repository failure state, publication, and acknowledgement belong to the
// runtime-owned scheduler support publisher.
func (s *SchedulerSnapshotService) ApplyDirtyWorkBatch(ctx context.Context, work []SchedulerDirtyWork) []SchedulerDirtyWorkResult {
	results := make([]SchedulerDirtyWorkResult, len(work))
	accountWork := make([]SchedulerDirtyWork, 0, len(work))
	accountIndexes := make([]int, 0, len(work))
	for i, item := range work {
		results[i].Work = item
		if err := ctx.Err(); err != nil {
			for j := i; j < len(results); j++ {
				results[j] = SchedulerDirtyWorkResult{Work: work[j], Err: err}
			}
			break
		}
		if item.Kind == SchedulerDirtyWorkAccount {
			accountWork = append(accountWork, item)
			accountIndexes = append(accountIndexes, i)
			continue
		}
		results[i].Err = s.handleDirtyWork(ctx, item)
	}
	if len(accountWork) == 0 || ctx.Err() != nil {
		return results
	}
	accountIDs := make([]int64, len(accountWork))
	for i := range accountWork {
		accountIDs[i] = accountWork[i].EntityID
	}
	accountResults := s.refreshDirtyAccounts(ctx, accountIDs)
	for i, resultIndex := range accountIndexes {
		results[resultIndex].Err = accountResults[i]
	}
	return results
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
		// 全量重建失败后进入指数退避窗口；窗口内不实际执行重建（返回错误让脏项
		// 保持挂起，DB 端 retry_at 继续延长其可见时间），到期后自动重试。这防止
		// 重建失败→请求回源→DB 过载→重建再失败的死循环。
		if s.rebuildRetryPending(time.Now()) {
			return errSchedulerRebuildRetryPending
		}
		return s.runRebuildWithRetryBackoff(ctx, "dirty_global")
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

// refreshDirtyAccounts 批量刷新脏账号：一次 GetByIDs 全字段读取 + 一次批量
// 缓存写入（缓存实现 schedulerAccountBatchWriter 时），替代逐账号
// GetByID×3 + 逐账号 Redis 往返。返回与输入等长的错误切片（nil 表示成功），
// 单个账号的失败（含缺失删除、缓存写入失败）不影响其他账号。
//
// 最小刷新间隔内的重复脏化直接视为成功：缓存最多落后 dirtyAccountRefreshMinInterval，
// 这是节流策略接受的语义（失败项未被确认，下一轮在间隔到期后重试）。
func (s *SchedulerSnapshotService) refreshDirtyAccounts(ctx context.Context, ids []int64) []error {
	results := make([]error, len(ids))
	if len(ids) == 0 {
		return results
	}
	if s.accountRepo == nil {
		err := errors.New("account repository unavailable")
		for i := range results {
			results[i] = err
		}
		return results
	}

	// 去重并保留首次出现顺序；重复项与节流跳过项都直接视为成功。
	uniqueIDs := make([]int64, 0, len(ids))
	indexByID := make(map[int64]int, len(ids))
	now := time.Now()
	for i, id := range ids {
		if id <= 0 {
			continue
		}
		if _, dup := indexByID[id]; dup {
			continue
		}
		indexByID[id] = i
		if s.dirtyRefreshThrottle != nil && !s.dirtyRefreshThrottle.Allow(id, now) {
			continue
		}
		uniqueIDs = append(uniqueIDs, id)
	}
	if len(uniqueIDs) == 0 {
		return results
	}

	accounts, err := s.accountRepo.GetByIDs(ctx, uniqueIDs)
	if err != nil {
		for _, id := range uniqueIDs {
			results[indexByID[id]] = err
		}
		return results
	}

	if s.cache == nil {
		return results
	}

	foundByID := make(map[int64]Account, len(accounts))
	found := make([]Account, 0, len(accounts))
	for _, account := range accounts {
		if account == nil || account.ID <= 0 {
			continue
		}
		if _, ok := indexByID[account.ID]; !ok {
			continue
		}
		foundByID[account.ID] = *account
		found = append(found, *account)
	}

	// 缺失账号只做删除；其余账号合并为一次批量写入（可选接口，未实现则逐账号
	// SetAccount，逐账号记录错误）。
	batchWrite := false
	var batchErr error
	if writer, ok := s.cache.(schedulerAccountBatchWriter); ok {
		batchWrite = true
		batchErr = writer.SetAccounts(ctx, found)
	} else {
		for _, account := range found {
			if err := s.cache.SetAccount(ctx, &account); err != nil {
				results[indexByID[account.ID]] = err
			}
		}
	}

	for _, id := range uniqueIDs {
		index := indexByID[id]
		if _, exists := foundByID[id]; !exists {
			if err := s.cache.DeleteAccount(ctx, id); err != nil {
				results[index] = err
			}
			continue
		}
		if batchWrite {
			results[index] = batchErr
		}
	}
	return results
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
	buckets := make([]SchedulerBucket, 0, len(groupIDs)*3)
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
		buckets = append(buckets, SchedulerBucket{GroupID: gid, Platform: platform, Mode: SchedulerModeSingle})
		buckets = append(buckets, SchedulerBucket{GroupID: gid, Platform: platform, Mode: SchedulerModeForced})
		if platform == PlatformAnthropic || platform == PlatformGemini {
			buckets = append(buckets, SchedulerBucket{GroupID: gid, Platform: platform, Mode: SchedulerModeMixed})
		}
	}
	return s.rebuildBuckets(ctx, buckets, reason)
}

func (s *SchedulerSnapshotService) rebuildBuckets(ctx context.Context, buckets []SchedulerBucket, reason string) error {
	queries := newSchedulerAccountQueryCache(buckets)
	var firstErr error
	for _, bucket := range buckets {
		if err := s.rebuildBucketWithQueryCache(ctx, bucket, reason, queries); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (s *SchedulerSnapshotService) rebuildBucketWithQueryCache(ctx context.Context, bucket SchedulerBucket, reason string, queries *schedulerAccountQueryCache) error {
	if queries != nil {
		// 无论本次重建是否成功，都消费一次 remaining；最后一个消费者立即释放
		// 账号切片与可复用 ID，避免把结果保留到整轮 rebuild 结束。
		defer queries.release(bucket)
	}
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

	accounts, err := s.loadAccountsForRebuild(rebuildCtx, bucket, queries)
	if err != nil {
		logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] rebuild failed: bucket=%s reason=%s err=%v", bucket.String(), reason, err)
		return err
	}
	if err := s.setRebuildSnapshot(rebuildCtx, bucket, accounts, queries); err != nil {
		logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] rebuild cache failed: bucket=%s reason=%s err=%v", bucket.String(), reason, err)
		return err
	}
	slog.Debug("[Scheduler] rebuild ok", "bucket", bucket.String(), "reason", reason, "size", len(accounts))
	return nil
}

// loadAccountsForRebuild 读取 bucket 的账号集；single/forced 桶在同一重建批次内
// 共享一次数据库查询。mixed 与其他模式不可复用，直接走原查询路径。
func (s *SchedulerSnapshotService) loadAccountsForRebuild(ctx context.Context, bucket SchedulerBucket, queries *schedulerAccountQueryCache) ([]Account, error) {
	key, cacheable := schedulerAccountQueryKeyForBucket(bucket)
	if queries == nil || !cacheable {
		return s.loadAccountsFromDB(ctx, bucket, bucket.Mode == SchedulerModeMixed)
	}

	if accounts, ok := queries.accounts[key]; ok {
		return accounts, nil
	}
	if queries.remaining[key] <= 1 {
		// 最后一个消费者直接查询，避免为无人再消费的结果保留切片。
		return s.loadAccountsFromDB(ctx, bucket, false)
	}
	accounts, err := s.loadAccountsFromDB(ctx, bucket, false)
	if err != nil {
		return nil, err
	}
	queries.accounts[key] = accounts
	return accounts, nil
}

// setRebuildSnapshot 发布 bucket 快照；缓存实现了 schedulerSnapshotAccountIDWriter
// 且该桶的查询可复用时，首个消费者完整发布并登记实际写入的账号 ID，后续桶只发布
// ID（省略重复的账号序列化与全局键写入），最后一个消费者回到原 SetSnapshot。
func (s *SchedulerSnapshotService) setRebuildSnapshot(ctx context.Context, bucket SchedulerBucket, accounts []Account, queries *schedulerAccountQueryCache) error {
	writer, ok := s.cache.(schedulerSnapshotAccountIDWriter)
	key, reusable := schedulerAccountQueryKeyForBucket(bucket)
	if !ok || queries == nil || !reusable {
		return s.cache.SetSnapshot(ctx, bucket, accounts)
	}

	if accountIDs, exists := queries.snapshotAccountIDs[key]; exists {
		return writer.SetSnapshotByAccountIDs(ctx, bucket, accountIDs)
	}
	if queries.remaining[key] <= 1 {
		return s.cache.SetSnapshot(ctx, bucket, accounts)
	}

	accountIDs, err := writer.SetSnapshotAndReturnAccountIDs(ctx, bucket, accounts)
	if err != nil {
		return err
	}
	if queries.remaining[key] > 1 {
		// 必须保存实际成功编码并写入的有序 ID，不能从原账号切片重新推导；
		// 否则不可编码账号会只出现在后续桶中，破坏两个快照的成员一致性。
		queries.snapshotAccountIDs[key] = accountIDs
	}
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
		// 条件恢复后清除重建失败退避状态，避免旧失败污染下一轮退化场景。
		s.outboxRebuildFailures = 0
		s.outboxRebuildRetryAt = time.Time{}
		s.outboxRebuildRetryReason = ""
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

// runRebuildWithRetryBackoff 执行一次全量重建并登记退避状态：成功清除失败计数与
// 重试时间，失败按指数退避（5s 起，5min 封顶）推迟下一次尝试。所有 outbox/dirty
// 触发的重建都经由这里，失败后不会在下一轮 poll 立即重试，避免重建失败→请求
// 回源 DB→DB 过载→重建再失败的死循环。
func (s *SchedulerSnapshotService) runRebuildWithRetryBackoff(ctx context.Context, reason string) error {
	err := s.triggerFullRebuildContext(ctx, reason)
	s.lagMu.Lock()
	if err == nil {
		s.outboxRebuildFailures = 0
		s.outboxRebuildRetryAt = time.Time{}
		s.outboxRebuildRetryReason = ""
	} else {
		s.outboxRebuildFailures++
		s.outboxRebuildRetryAt = time.Now().Add(outboxRebuildRetryDelay(s.outboxRebuildFailures))
		s.outboxRebuildRetryReason = reason
	}
	s.lagMu.Unlock()
	return err
}

// rebuildRetryPending 报告全量重建是否处于失败退避窗口内。
func (s *SchedulerSnapshotService) rebuildRetryPending(now time.Time) bool {
	s.lagMu.Lock()
	defer s.lagMu.Unlock()
	return !s.outboxRebuildRetryAt.IsZero() && now.Before(s.outboxRebuildRetryAt)
}

// outboxRebuildRetryDelay 计算第 failures 次失败后的退避延迟：5s 起翻倍，封顶 5min。
func outboxRebuildRetryDelay(failures int) time.Duration {
	delay := outboxRebuildRetryBaseDelay
	for i := 1; i < failures && delay < outboxRebuildRetryMaxDelay; i++ {
		delay *= 2
		if delay >= outboxRebuildRetryMaxDelay {
			return outboxRebuildRetryMaxDelay
		}
	}
	return delay
}

func (s *SchedulerSnapshotService) checkOutboxLag(ctx context.Context, oldest SchedulerOutboxEvent, watermark int64) {
	if oldest.CreatedAt.IsZero() || s.cfg == nil {
		return
	}

	lag := time.Since(oldest.CreatedAt)
	if lagSeconds := int(lag.Seconds()); lagSeconds >= s.cfg.Gateway.Scheduling.OutboxLagWarnSeconds && s.cfg.Gateway.Scheduling.OutboxLagWarnSeconds > 0 {
		logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] outbox lag warning: %ds", lagSeconds)
	}

	lagDegraded := s.cfg.Gateway.Scheduling.OutboxLagRebuildSeconds > 0 && int(lag.Seconds()) >= s.cfg.Gateway.Scheduling.OutboxLagRebuildSeconds

	threshold := s.cfg.Gateway.Scheduling.OutboxBacklogRebuildRows
	backlogDegraded := false
	var backlog int64
	if threshold > 0 && s.outboxRepo != nil {
		maxID, err := s.outboxRepo.MaxID(ctx)
		if err != nil {
			// MaxID 失败只视为 backlog 未退化；lag 决策独立继续，避免单次读失败
			// 同时吞掉 lag 的重建触发。
			backlogDegraded = false
		} else {
			backlog = maxID - watermark
			backlogDegraded = backlog >= int64(threshold)
		}
	}

	now := time.Now()
	s.lagMu.Lock()
	if !lagDegraded && !backlogDegraded {
		s.lagFailures = 0
		s.outboxRebuildFailures = 0
		s.outboxRebuildRetryAt = time.Time{}
		s.outboxRebuildRetryReason = ""
		s.lagMu.Unlock()
		return
	}

	// 退避原因对应的退化条件已消失时，旧的失败状态不再有意义，立即清除；
	// 否则会永远压住下一次触发（例如 backlog 恢复后残留的 outbox_backlog 退避）。
	if s.outboxRebuildRetryReason != "" {
		retryReasonActive := (s.outboxRebuildRetryReason == "outbox_lag" && lagDegraded) ||
			(s.outboxRebuildRetryReason == "outbox_backlog" && backlogDegraded)
		if !retryReasonActive {
			s.outboxRebuildFailures = 0
			s.outboxRebuildRetryAt = time.Time{}
			s.outboxRebuildRetryReason = ""
		}
	}

	// 上一次 lag 重建失败后处于退避窗口内时，不再累计 lag 失败计数；
	// 退避到期后由 retryDue 直接触发下一次重建。
	lagRetryPending := s.outboxRebuildRetryReason == "outbox_lag" && !s.outboxRebuildRetryAt.IsZero()
	if lagDegraded && !lagRetryPending {
		s.lagFailures++
	}
	failures := s.lagFailures
	lagReady := lagDegraded && failures >= s.cfg.Gateway.Scheduling.OutboxLagRebuildFailures
	retryDue := !s.outboxRebuildRetryAt.IsZero() && !now.Before(s.outboxRebuildRetryAt)

	reason := ""
	switch {
	case lagReady && s.outboxRebuildRetryReason != "outbox_lag":
		// lag 就绪可抢占挂起的 backlog 退避：lag 反映最新消费进度，优先重建。
		if s.outboxRebuildRetryReason != "" {
			s.outboxRebuildFailures = 0
			s.outboxRebuildRetryAt = time.Time{}
			s.outboxRebuildRetryReason = ""
		}
		reason = "outbox_lag"
	case retryDue && s.outboxRebuildRetryReason == "outbox_lag" && lagDegraded:
		reason = "outbox_lag"
	case backlogDegraded && (s.outboxRebuildRetryReason == "" || (retryDue && s.outboxRebuildRetryReason == "outbox_backlog")):
		reason = "outbox_backlog"
	}
	if reason != "" {
		s.lagFailures = 0
	}
	s.lagMu.Unlock()

	if reason == "" {
		return
	}

	logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] %s rebuild triggered: lag=%s failures=%d backlog=%d", reason, lag, failures, backlog)
	// 重建独立于 poll 的 10s 超时运行（沿用原实现传入 Background），
	// 避免 poll 上下文过期把最长 30s 的重建提前中断。
	if err := s.runRebuildWithRetryBackoff(context.Background(), reason); err != nil {
		logger.LegacyPrintf("service.scheduler_snapshot", "[Scheduler] %s rebuild failed: %v", reason, err)
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

// fallbackQueryContext 构造 singleflight leader 的回源查询上下文：脱离调用方
// 取消（leader 的 DB 工作不随首个请求断连中断），并施加硬性执行上限——
// db_fallback_timeout_seconds 未配置（生产默认 0）时也必须兜底有限，否则 DB
// 挂起会让 leader 无限阻塞，整桶 singleflight 等待者一起卡死。
func (s *SchedulerSnapshotService) fallbackQueryContext(ctx context.Context) (context.Context, context.CancelFunc) {
	bounded, boundedCancel := context.WithTimeout(context.WithoutCancel(ctx), schedulerBucketRebuildLimit)
	fallbackCtx, fallbackCancel := s.withFallbackTimeout(bounded)
	return fallbackCtx, func() {
		boundedCancel()
		fallbackCancel()
	}
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
