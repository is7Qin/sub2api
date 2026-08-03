package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	schedulerBucketSetKey          = "sched:buckets"
	schedulerOutboxWatermarkKey    = "sched:outbox:watermark"
	schedulerAccountPrefix         = "sched:acc:"
	schedulerAccountMetaPrefix     = "sched:meta:"
	schedulerAccountLastUsedPrefix = "sched:acc:last_used:"
	schedulerActivePrefix          = "sched:active:"
	schedulerReadyPrefix           = "sched:ready:"
	schedulerVersionPrefix         = "sched:ver:"
	schedulerSnapshotPrefix        = "sched:"
	schedulerLockPrefix            = "sched:lock:"

	defaultSchedulerSnapshotMGetChunkSize  = 128
	defaultSchedulerSnapshotWriteChunkSize = 256
	schedulerLastUsedUpdateChunkSize       = 256

	// schedulerLastUsedTTLSeconds bounds side-key retention during a rolling upgrade:
	// a pre-side-key binary can delete full/meta keys without knowing the additive key.
	// LastUsedAt is only a scheduling hint and is durably persisted before this cache update,
	// so expiry falls back to the next embedded snapshot rather than retaining an orphan forever.
	schedulerLastUsedTTLSeconds = 24 * 60 * 60

	// snapshotGraceTTLSeconds 旧快照过期的宽限期（秒）。
	// 替代立即 DEL，让正在读取旧版本的 reader 有足够时间完成 ZRANGE。
	snapshotGraceTTLSeconds = 60
)

var errSchedulerLastUsedCacheMalformed = errors.New("malformed scheduler last-used cache value")

var (
	updateSchedulerLastUsedScript = redis.NewScript(`
local ttl = tonumber(ARGV[#ARGV])
if ttl == nil or ttl <= 0 then
    return redis.error_reply('invalid last_used ttl')
end

local updated = 0
for index = 1, #ARGV - 1 do
    local key_index = (index - 1) * 2 + 1
    local candidate = tonumber(ARGV[index])
    if candidate == nil then
        return redis.error_reply('invalid last_used value')
    end
    if redis.call('EXISTS', KEYS[key_index]) == 1 then
        local current = tonumber(redis.call('GET', KEYS[key_index + 1]))
        if current == nil or candidate > current then
            redis.call('SET', KEYS[key_index + 1], ARGV[index], 'EX', ttl)
            updated = updated + 1
        end
    end
end
return updated
`)

	// activateSnapshotScript 原子 CAS 切换快照版本。
	// 仅当新版本号 >= 当前激活版本时才切换，防止并发写入导致版本回滚。
	// 旧快照使用 EXPIRE 设置宽限期而非立即 DEL，避免与 reader 竞态。
	//
	// KEYS[1] = activeKey     (sched:active:{bucket})
	// KEYS[2] = readyKey      (sched:ready:{bucket})
	// KEYS[3] = bucketSetKey  (sched:buckets)
	// KEYS[4] = snapshotKey   (新写入的快照 key)
	// ARGV[1] = 新版本号字符串
	// ARGV[2] = bucket 字符串 (用于 SADD)
	// ARGV[3] = 快照 key 前缀 (用于构造旧快照 key)
	// ARGV[4] = 宽限期 TTL 秒数
	//
	// 返回 1 = 已激活, 0 = 版本过旧未激活
	activateSnapshotScript = redis.NewScript(`
local currentActive = redis.call('GET', KEYS[1])
local newVersion = tonumber(ARGV[1])

if currentActive ~= false then
	local curVersion = tonumber(currentActive)
	if curVersion and newVersion < curVersion then
		redis.call('DEL', KEYS[4])
		return 0
	end
end

redis.call('SET', KEYS[1], ARGV[1])
redis.call('SET', KEYS[2], '1')
redis.call('SADD', KEYS[3], ARGV[2])

if currentActive ~= false and currentActive ~= ARGV[1] then
	redis.call('EXPIRE', ARGV[3] .. currentActive, tonumber(ARGV[4]))
end

return 1
`)

	unlockBucketScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
	return redis.call('DEL', KEYS[1])
end
return 0
`)
)

type schedulerCache struct {
	rdb            *redis.Client
	mgetChunkSize  int
	writeChunkSize int
	stats          *schedulerSnapshotStats
}

func NewSchedulerCache(rdb *redis.Client) service.SchedulerCache {
	return newSchedulerCacheWithChunkSizes(rdb, defaultSchedulerSnapshotMGetChunkSize, defaultSchedulerSnapshotWriteChunkSize)
}

func newSchedulerCacheWithChunkSizes(rdb *redis.Client, mgetChunkSize, writeChunkSize int) service.SchedulerCache {
	if mgetChunkSize <= 0 {
		mgetChunkSize = defaultSchedulerSnapshotMGetChunkSize
	}
	if writeChunkSize <= 0 {
		writeChunkSize = defaultSchedulerSnapshotWriteChunkSize
	}
	return &schedulerCache{
		rdb:            rdb,
		mgetChunkSize:  mgetChunkSize,
		writeChunkSize: writeChunkSize,
		stats:          newSchedulerSnapshotStats(),
	}
}

// ---------------------------------------------------------------------------
// GetSnapshot 命中/未命中观测统计（仅观测，不改变 GetSnapshot 返回语义）。
// 窗口内按分桶累计，由 SchedulerSnapshotService 的周期 goroutine 每 10s
// 聚合输出并重置；进程启动以来的全局滚动命中/未命中单独累计，用于立即
// 评估整体命中率（未命中时网关退化为逐请求 DB 全量查询）。
// ---------------------------------------------------------------------------

// schedulerSnapshotMissReason 区分 GetSnapshot 未命中的原因分类。
type schedulerSnapshotMissReason string

const (
	schedulerMissNotReady      schedulerSnapshotMissReason = "not_ready"
	schedulerMissActiveMissing schedulerSnapshotMissReason = "active_missing"
	schedulerMissSnapshotEmpty schedulerSnapshotMissReason = "snapshot_missing"
	schedulerMissMetaMissing   schedulerSnapshotMissReason = "meta_missing"
	schedulerMissDecodeError   schedulerSnapshotMissReason = "decode_error"
	schedulerMissLastUsedError schedulerSnapshotMissReason = "last_used_error"
	schedulerMissRedisError    schedulerSnapshotMissReason = "redis_error"

	// schedulerSnapshotStatsLogBucketLimit 单次汇总日志最多输出的分桶数（按未命中次数降序）。
	schedulerSnapshotStatsLogBucketLimit = 20
)

// schedulerSnapshotMissReasons 固定原因清单（定长数组，len 为编译期常量），
// 索引与 bucketStats.missReasons 对齐。
var schedulerSnapshotMissReasons = [...]schedulerSnapshotMissReason{
	schedulerMissNotReady,
	schedulerMissActiveMissing,
	schedulerMissSnapshotEmpty,
	schedulerMissMetaMissing,
	schedulerMissDecodeError,
	schedulerMissLastUsedError,
	schedulerMissRedisError,
}

var schedulerSnapshotMissReasonIndex = func() map[schedulerSnapshotMissReason]int {
	index := make(map[schedulerSnapshotMissReason]int, len(schedulerSnapshotMissReasons))
	for i, reason := range schedulerSnapshotMissReasons {
		index[reason] = i
	}
	return index
}()

// schedulerSnapshotBucketStats 单个分桶在统计窗口内的命中/未命中计数。
type schedulerSnapshotBucketStats struct {
	hits        atomic.Uint64
	misses      atomic.Uint64
	missReasons [len(schedulerSnapshotMissReasons)]atomic.Uint64
}

// schedulerSnapshotStats 聚合所有分桶的 GetSnapshot 观测计数。
type schedulerSnapshotStats struct {
	totalHits   atomic.Uint64
	totalMisses atomic.Uint64
	buckets     sync.Map // bucket.String() -> *schedulerSnapshotBucketStats
}

func newSchedulerSnapshotStats() *schedulerSnapshotStats {
	return &schedulerSnapshotStats{}
}

func (s *schedulerSnapshotStats) bucketStats(key string) *schedulerSnapshotBucketStats {
	stats, _ := s.buckets.LoadOrStore(key, &schedulerSnapshotBucketStats{})
	return stats.(*schedulerSnapshotBucketStats)
}

func (s *schedulerSnapshotStats) recordHit(key string) {
	s.totalHits.Add(1)
	s.bucketStats(key).hits.Add(1)
}

func (s *schedulerSnapshotStats) recordMiss(key string, reason schedulerSnapshotMissReason) {
	s.totalMisses.Add(1)
	bucket := s.bucketStats(key)
	bucket.misses.Add(1)
	if index, ok := schedulerSnapshotMissReasonIndex[reason]; ok {
		bucket.missReasons[index].Add(1)
	}
}

// schedulerSnapshotBucketSummary 单个分桶在一个统计窗口内的汇总结果。
type schedulerSnapshotBucketSummary struct {
	Bucket  string                                 `json:"bucket"`
	Hits    uint64                                 `json:"hits"`
	Misses  uint64                                 `json:"misses"`
	Reasons map[schedulerSnapshotMissReason]uint64 `json:"reasons"`
}

// schedulerSnapshotStatsSummary 一个统计窗口的汇总结果。
type schedulerSnapshotStatsSummary struct {
	WindowHits   uint64                           `json:"window_hits"`
	WindowMisses uint64                           `json:"window_misses"`
	TotalHits    uint64                           `json:"total_hits"`
	TotalMisses  uint64                           `json:"total_misses"`
	Buckets      []schedulerSnapshotBucketSummary `json:"buckets"`
}

// summarize 汇总当前窗口并重置窗口计数（滚动总量不清零）。先换入新计数结构
// 再读取旧值：换入后的并发增量进入新结构，只有换入瞬间正持有旧结构的增量
// 会从两个窗口漏计——观测数据，边界误差可容忍。仅输出窗口内有调用的分桶，
// 按未命中次数降序，截断到 schedulerSnapshotStatsLogBucketLimit 个。
func (s *schedulerSnapshotStats) summarize() schedulerSnapshotStatsSummary {
	summary := schedulerSnapshotStatsSummary{
		TotalHits:   s.totalHits.Load(),
		TotalMisses: s.totalMisses.Load(),
	}
	s.buckets.Range(func(key, value any) bool {
		bucketKey := key.(string)
		bucket := value.(*schedulerSnapshotBucketStats)
		s.buckets.Store(bucketKey, &schedulerSnapshotBucketStats{})

		b := schedulerSnapshotBucketSummary{
			Bucket:  bucketKey,
			Hits:    bucket.hits.Load(),
			Misses:  bucket.misses.Load(),
			Reasons: make(map[schedulerSnapshotMissReason]uint64),
		}
		for i, reason := range schedulerSnapshotMissReasons {
			if count := bucket.missReasons[i].Load(); count > 0 {
				b.Reasons[reason] = count
			}
		}
		if b.Hits == 0 && b.Misses == 0 {
			return true
		}
		summary.WindowHits += b.Hits
		summary.WindowMisses += b.Misses
		summary.Buckets = append(summary.Buckets, b)
		return true
	})
	sort.Slice(summary.Buckets, func(i, j int) bool {
		if summary.Buckets[i].Misses != summary.Buckets[j].Misses {
			return summary.Buckets[i].Misses > summary.Buckets[j].Misses
		}
		return summary.Buckets[i].Bucket < summary.Buckets[j].Bucket
	})
	if len(summary.Buckets) > schedulerSnapshotStatsLogBucketLimit {
		summary.Buckets = summary.Buckets[:schedulerSnapshotStatsLogBucketLimit]
	}
	return summary
}

// LogSnapshotStats 输出最近一个统计窗口的 GetSnapshot 命中/未命中汇总并重置
// 窗口计数。窗口内没有 GetSnapshot 调用时不输出，保持日志安静。
// 由 SchedulerSnapshotService 的周期 goroutine 调用；测试直接调用 summarize。
func (c *schedulerCache) LogSnapshotStats() {
	summary := c.stats.summarize()
	if len(summary.Buckets) == 0 {
		return
	}
	attrs := []any{
		slog.String("component", "scheduler.cache"),
		slog.Uint64("window_hits", summary.WindowHits),
		slog.Uint64("window_misses", summary.WindowMisses),
		slog.Uint64("total_hits", summary.TotalHits),
		slog.Uint64("total_misses", summary.TotalMisses),
	}
	if total := summary.TotalHits + summary.TotalMisses; total > 0 {
		attrs = append(attrs, slog.Float64("total_miss_ratio", float64(summary.TotalMisses)/float64(total)))
	}
	attrs = append(attrs, slog.Any("buckets", summary.Buckets))
	// Warn 级别：生产 LOG_LEVEL=error/warn 下 10 秒周期汇总仍可见，
	// 供风暴定位 GetSnapshot miss 分布（每桶 reason 计数）。
	slog.Warn("scheduler snapshot cache stats", attrs...)
}

func (c *schedulerCache) GetSnapshot(ctx context.Context, bucket service.SchedulerBucket) ([]*service.Account, bool, error) {
	bucketKey := bucket.String()

	readyKey := schedulerBucketKey(schedulerReadyPrefix, bucket)
	readyVal, err := c.rdb.Get(ctx, readyKey).Result()
	if err == redis.Nil {
		c.stats.recordMiss(bucketKey, schedulerMissNotReady)
		return nil, false, nil
	}
	if err != nil {
		c.stats.recordMiss(bucketKey, schedulerMissRedisError)
		return nil, false, err
	}
	if readyVal != "1" {
		c.stats.recordMiss(bucketKey, schedulerMissNotReady)
		return nil, false, nil
	}

	activeKey := schedulerBucketKey(schedulerActivePrefix, bucket)
	activeVal, err := c.rdb.Get(ctx, activeKey).Result()
	if err == redis.Nil {
		c.stats.recordMiss(bucketKey, schedulerMissActiveMissing)
		return nil, false, nil
	}
	if err != nil {
		c.stats.recordMiss(bucketKey, schedulerMissRedisError)
		return nil, false, err
	}

	snapshotKey := schedulerSnapshotKey(bucket, activeVal)
	ids, err := c.rdb.ZRange(ctx, snapshotKey, 0, -1).Result()
	if err != nil {
		c.stats.recordMiss(bucketKey, schedulerMissRedisError)
		return nil, false, err
	}
	if len(ids) == 0 {
		// 空快照视为缓存未命中，触发数据库回退查询
		// 这解决了新分组创建后立即绑定账号时的竞态条件问题
		c.stats.recordMiss(bucketKey, schedulerMissSnapshotEmpty)
		return nil, false, nil
	}

	keys := make([]string, 0, len(ids))
	lastUsedKeys := make([]string, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, schedulerAccountMetaKey(id))
		lastUsedKeys = append(lastUsedKeys, schedulerLastUsedKey(id))
	}
	values, err := c.mgetChunked(ctx, keys)
	if err != nil {
		c.stats.recordMiss(bucketKey, schedulerMissRedisError)
		return nil, false, err
	}
	lastUsedValues, err := c.mgetChunked(ctx, lastUsedKeys)
	if err != nil {
		c.stats.recordMiss(bucketKey, schedulerMissRedisError)
		return nil, false, err
	}
	if len(values) != len(ids) || len(lastUsedValues) != len(ids) {
		// MGET 返回条数与请求不一致属于 Redis 响应形状异常，归入 redis_error。
		c.stats.recordMiss(bucketKey, schedulerMissRedisError)
		return nil, false, errors.New("scheduler snapshot cache returned unexpected value count")
	}

	accounts := make([]*service.Account, 0, len(values))
	for i, val := range values {
		if val == nil {
			c.stats.recordMiss(bucketKey, schedulerMissMetaMissing)
			return nil, false, nil
		}
		account, err := decodeCachedAccount(val)
		if err != nil {
			c.stats.recordMiss(bucketKey, schedulerMissDecodeError)
			return nil, false, err
		}
		if err := applySchedulerLastUsed(account, lastUsedValues[i]); err != nil {
			c.stats.recordMiss(bucketKey, schedulerMissLastUsedError)
			return nil, false, err
		}
		accounts = append(accounts, account)
	}

	c.stats.recordHit(bucketKey)
	return accounts, true, nil
}

// GetSnapshotVersion 读取分桶当前激活快照版本号。active key 缺失（从未写入
// 快照）时返回空版本且无错误——与 GetSnapshot 未命中语义一致，此时不存在
// 任何可复用的本地解码缓存条目。
func (c *schedulerCache) GetSnapshotVersion(ctx context.Context, bucket service.SchedulerBucket) (string, error) {
	activeKey := schedulerBucketKey(schedulerActivePrefix, bucket)
	activeVal, err := c.rdb.Get(ctx, activeKey).Result()
	if err == redis.Nil {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return activeVal, nil
}

func (c *schedulerCache) SetSnapshot(ctx context.Context, bucket service.SchedulerBucket, accounts []service.Account) error {
	version, err := c.allocateSnapshotVersion(ctx, bucket)
	if err != nil {
		return err
	}
	accountIDs, err := c.writeAccountIDs(ctx, accounts)
	if err != nil {
		return err
	}
	if err := c.writeSnapshotAccountIDs(ctx, bucket, version, accountIDs); err != nil {
		return err
	}
	return c.activateSnapshotVersion(ctx, bucket, version)
}

// SetSnapshotAndReturnAccountIDs 完整发布快照，并返回实际成功编码并写入的有序账号 ID。
// 该可选能力只供同一重建批次复用：同批次后续桶直接以这些 ID 发布，省略重复的账号
// JSON 序列化与全局键写入。发布语义与 SetSnapshot 完全一致。
func (c *schedulerCache) SetSnapshotAndReturnAccountIDs(ctx context.Context, bucket service.SchedulerBucket, accounts []service.Account) ([]int64, error) {
	version, err := c.allocateSnapshotVersion(ctx, bucket)
	if err != nil {
		return nil, err
	}
	accountIDs, err := c.writeAccountIDs(ctx, accounts)
	if err != nil {
		return nil, err
	}
	if err := c.writeSnapshotAccountIDs(ctx, bucket, version, accountIDs); err != nil {
		return nil, err
	}
	if err := c.activateSnapshotVersion(ctx, bucket, version); err != nil {
		return nil, err
	}
	return accountIDs, nil
}

// SetSnapshotByAccountIDs 复用同批次首次完整写入后得到的账号成员。
// 每个桶仍独立分配版本、写入有序集合并执行激活，只省略重复的账号 JSON 与全局键写入。
func (c *schedulerCache) SetSnapshotByAccountIDs(ctx context.Context, bucket service.SchedulerBucket, accountIDs []int64) error {
	version, err := c.allocateSnapshotVersion(ctx, bucket)
	if err != nil {
		return err
	}
	if err := c.writeSnapshotAccountIDs(ctx, bucket, version, accountIDs); err != nil {
		return err
	}
	return c.activateSnapshotVersion(ctx, bucket, version)
}

// allocateSnapshotVersion 分配新版本号并返回版本字符串。
// INCR 保证每个调用方获得唯一递增版本号；写入的 snapshotKey 是新的版本化 key，
// reader 尚不知晓，因此无竞态。
func (c *schedulerCache) allocateSnapshotVersion(ctx context.Context, bucket service.SchedulerBucket) (string, error) {
	versionKey := schedulerBucketKey(schedulerVersionPrefix, bucket)
	version, err := c.rdb.Incr(ctx, versionKey).Result()
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(version, 10), nil
}

// writeSnapshotAccountIDs 把有序账号 ID 写入版本化快照 key（score 为序号，保持
// 数据库返回的排序语义）。只有每一批有界的 ZADD 都成功后该版本才会被发布，
// 部分写入不能成为激活快照；任一 ZADD 失败都清理该版本 key。
func (c *schedulerCache) writeSnapshotAccountIDs(ctx context.Context, bucket service.SchedulerBucket, version string, accountIDs []int64) error {
	if len(accountIDs) == 0 {
		return nil
	}
	snapshotKey := schedulerSnapshotKey(bucket, version)
	members := make([]redis.Z, 0, len(accountIDs))
	for idx, accountID := range accountIDs {
		members = append(members, redis.Z{
			Score:  float64(idx),
			Member: strconv.FormatInt(accountID, 10),
		})
	}
	for start := 0; start < len(members); start += c.writeChunkSize {
		end := start + c.writeChunkSize
		if end > len(members) {
			end = len(members)
		}
		if err := c.rdb.ZAdd(ctx, snapshotKey, members[start:end]...).Err(); err != nil {
			// This version was never published; best-effort cleanup avoids leaking a
			// partially materialized key while preserving the active snapshot.
			_ = c.rdb.Del(ctx, snapshotKey).Err()
			return err
		}
	}
	return nil
}

// activateSnapshotVersion 原子 CAS 激活版本。
// Lua 脚本保证：仅当新版本 >= 当前激活版本时才切换 active 指针，
// 防止并发写入导致版本回滚。
// 旧快照使用 EXPIRE 宽限期而非立即 DEL，避免 reader 竞态。
func (c *schedulerCache) activateSnapshotVersion(ctx context.Context, bucket service.SchedulerBucket, version string) error {
	activeKey := schedulerBucketKey(schedulerActivePrefix, bucket)
	readyKey := schedulerBucketKey(schedulerReadyPrefix, bucket)
	snapshotKey := schedulerSnapshotKey(bucket, version)
	snapshotKeyPrefix := fmt.Sprintf("%s%d:%s:%s:v", schedulerSnapshotPrefix, bucket.GroupID, bucket.Platform, bucket.Mode)

	keys := []string{activeKey, readyKey, schedulerBucketSetKey, snapshotKey}
	args := []any{version, bucket.String(), snapshotKeyPrefix, snapshotGraceTTLSeconds}

	_, err := activateSnapshotScript.Run(ctx, c.rdb, keys, args...).Result()
	return err
}

func (c *schedulerCache) GetAccount(ctx context.Context, accountID int64) (*service.Account, error) {
	id := strconv.FormatInt(accountID, 10)
	values, err := c.rdb.MGet(ctx, schedulerAccountKey(id), schedulerLastUsedKey(id)).Result()
	if err != nil {
		return nil, err
	}
	if len(values) != 2 || values[0] == nil {
		return nil, nil
	}
	account, err := decodeCachedAccount(values[0])
	if err != nil {
		return nil, err
	}
	if err := applySchedulerLastUsed(account, values[1]); err != nil {
		return nil, err
	}
	return account, nil
}

// GetSchedulableAccountsByIDs 批量读取账号快照的调度子集 meta payload
// （sched:meta:{id}，仅调度/路由所需字段，不携带 api_key/access_token 等凭据）。
// 每请求候选刷新若读全量 payload（~9KB/账号）会放大 Redis 网络吞吐；meta 仅
// 含调度字段，足以支撑存在性判断与资格复检（调用方选中后经 GetAccount 重新
// 水合全量账号再执行请求）。缺失的 ID 直接跳过——快照中不存在即视为不可调度，
// 与“未在刷新 map 中”的调用方语义一致；仅 Redis 错误或 payload 解码失败才
// 返回错误（此时调用方整体降级 DB）。
func (c *schedulerCache) GetSchedulableAccountsByIDs(ctx context.Context, ids []int64) (map[int64]*service.Account, error) {
	if len(ids) == 0 {
		return map[int64]*service.Account{}, nil
	}
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, schedulerAccountMetaKey(strconv.FormatInt(id, 10)))
	}
	values, err := c.mgetChunked(ctx, keys)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]*service.Account, len(ids))
	for _, val := range values {
		if val == nil {
			continue
		}
		account, err := decodeCachedAccount(val)
		if err != nil {
			return nil, err
		}
		if account != nil && account.ID > 0 {
			out[account.ID] = account
		}
	}
	return out, nil
}

func (c *schedulerCache) SetAccount(ctx context.Context, account *service.Account) error {
	if account == nil || account.ID <= 0 {
		return nil
	}
	_, err := c.writeAccountIDs(ctx, []service.Account{*account})
	return err
}

func (c *schedulerCache) DeleteAccount(ctx context.Context, accountID int64) error {
	if accountID <= 0 {
		return nil
	}
	id := strconv.FormatInt(accountID, 10)
	return c.rdb.Del(ctx, schedulerAccountKey(id), schedulerAccountMetaKey(id), schedulerLastUsedKey(id)).Err()
}

func (c *schedulerCache) UpdateLastUsed(ctx context.Context, updates map[int64]time.Time) error {
	if len(updates) == 0 {
		return nil
	}

	keys := make([]string, 0, schedulerLastUsedUpdateChunkSize*2)
	args := make([]any, 0, schedulerLastUsedUpdateChunkSize+1)
	flushBatch := func() error {
		if len(args) == 0 {
			return nil
		}
		args = append(args, schedulerLastUsedTTLSeconds)
		pipe := c.rdb.Pipeline()
		updateSchedulerLastUsedScript.Eval(ctx, pipe, keys, args...)
		_, err := pipe.Exec(ctx)
		keys = keys[:0]
		args = args[:0]
		return err
	}
	for id, usedAt := range updates {
		if id <= 0 {
			continue
		}
		millis, err := schedulerLastUsedMillis(usedAt)
		if err != nil {
			slog.Warn(
				"scheduler cache removes account with unencodable last-used time",
				"account_id", id,
				"error", err,
			)
			idText := strconv.FormatInt(id, 10)
			if err := flushBatch(); err != nil {
				return err
			}
			if err := c.rdb.Del(ctx, schedulerAccountKey(idText), schedulerAccountMetaKey(idText), schedulerLastUsedKey(idText)).Err(); err != nil {
				return err
			}
			continue
		}
		idText := strconv.FormatInt(id, 10)
		keys = append(keys, schedulerAccountKey(idText), schedulerLastUsedKey(idText))
		args = append(args, millis)
		if len(args) >= schedulerLastUsedUpdateChunkSize {
			if err := flushBatch(); err != nil {
				return err
			}
		}
	}
	return flushBatch()
}

func (c *schedulerCache) TryLockBucket(ctx context.Context, bucket service.SchedulerBucket, ttl time.Duration) (string, bool, error) {
	key := schedulerBucketKey(schedulerLockPrefix, bucket)
	token := uuid.NewString()
	acquired, err := c.rdb.SetNX(ctx, key, token, ttl).Result()
	if err != nil || !acquired {
		return "", acquired, err
	}
	return token, true, nil
}

func (c *schedulerCache) UnlockBucket(ctx context.Context, bucket service.SchedulerBucket, token string) error {
	if token == "" {
		return nil
	}
	key := schedulerBucketKey(schedulerLockPrefix, bucket)
	return unlockBucketScript.Run(ctx, c.rdb, []string{key}, token).Err()
}

func (c *schedulerCache) ListBuckets(ctx context.Context) ([]service.SchedulerBucket, error) {
	raw, err := c.rdb.SMembers(ctx, schedulerBucketSetKey).Result()
	if err != nil {
		return nil, err
	}
	out := make([]service.SchedulerBucket, 0, len(raw))
	for _, entry := range raw {
		bucket, ok := service.ParseSchedulerBucket(entry)
		if !ok {
			continue
		}
		out = append(out, bucket)
	}
	return out, nil
}

func (c *schedulerCache) GetOutboxWatermark(ctx context.Context) (int64, error) {
	val, err := c.rdb.Get(ctx, schedulerOutboxWatermarkKey).Result()
	if err == redis.Nil {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	id, err := strconv.ParseInt(val, 10, 64)
	if err != nil {
		return 0, err
	}
	return id, nil
}

func (c *schedulerCache) SetOutboxWatermark(ctx context.Context, id int64) error {
	return c.rdb.Set(ctx, schedulerOutboxWatermarkKey, strconv.FormatInt(id, 10), 0).Err()
}

func schedulerBucketKey(prefix string, bucket service.SchedulerBucket) string {
	return fmt.Sprintf("%s%d:%s:%s", prefix, bucket.GroupID, bucket.Platform, bucket.Mode)
}

func schedulerSnapshotKey(bucket service.SchedulerBucket, version string) string {
	return fmt.Sprintf("%s%d:%s:%s:v%s", schedulerSnapshotPrefix, bucket.GroupID, bucket.Platform, bucket.Mode, version)
}

func schedulerAccountKey(id string) string {
	return schedulerAccountPrefix + id
}

func schedulerAccountMetaKey(id string) string {
	return schedulerAccountMetaPrefix + id
}

func schedulerLastUsedKey(id string) string {
	return schedulerAccountLastUsedPrefix + id
}

func ptrTime(t time.Time) *time.Time {
	return &t
}

func schedulerLastUsedMillis(value time.Time) (int64, error) {
	if _, err := value.MarshalJSON(); err != nil {
		return 0, err
	}
	return value.UTC().UnixMilli(), nil
}

func applySchedulerLastUsed(account *service.Account, value any) error {
	if account == nil || value == nil {
		return nil
	}
	var raw string
	switch typed := value.(type) {
	case string:
		raw = typed
	case []byte:
		raw = string(typed)
	default:
		return fmt.Errorf("%w: unexpected type %T", errSchedulerLastUsedCacheMalformed, value)
	}
	millis, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: invalid milliseconds: %v", errSchedulerLastUsedCacheMalformed, err)
	}
	lastUsedAt := time.UnixMilli(millis).UTC()
	if _, err := lastUsedAt.MarshalJSON(); err != nil {
		return fmt.Errorf("%w: milliseconds outside supported time range", errSchedulerLastUsedCacheMalformed)
	}
	if account.LastUsedAt == nil || lastUsedAt.After(*account.LastUsedAt) {
		account.LastUsedAt = ptrTime(lastUsedAt)
	}
	return nil
}

func decodeCachedAccount(val any) (*service.Account, error) {
	var payload []byte
	switch raw := val.(type) {
	case string:
		payload = []byte(raw)
	case []byte:
		payload = raw
	default:
		return nil, fmt.Errorf("unexpected account cache type: %T", val)
	}
	var account service.Account
	if err := json.Unmarshal(payload, &account); err != nil {
		return nil, err
	}
	return &account, nil
}

func (c *schedulerCache) writeAccountIDs(ctx context.Context, accounts []service.Account) ([]int64, error) {
	if len(accounts) == 0 {
		return nil, nil
	}

	pipe := c.rdb.Pipeline()
	accountIDs := make([]int64, 0, len(accounts))
	pending := 0
	flush := func() error {
		if pending == 0 {
			return nil
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return err
		}
		pipe = c.rdb.Pipeline()
		pending = 0
		return nil
	}

	for _, account := range accounts {
		fullPayload, metaPayload, err := marshalSchedulerCacheAccount(account)
		if err != nil {
			slog.Warn(
				"scheduler cache skips account with unencodable payload",
				"account_id", account.ID,
				"error", err,
			)
			// A successful dirty-work acknowledgement must not leave an older
			// account payload behind when the current row cannot be encoded.
			id := strconv.FormatInt(account.ID, 10)
			pipe.Del(ctx, schedulerAccountKey(id), schedulerAccountMetaKey(id), schedulerLastUsedKey(id))
			pending++
			if pending >= c.writeChunkSize {
				if err := flush(); err != nil {
					return nil, err
				}
			}
			continue
		}

		id := strconv.FormatInt(account.ID, 10)
		pipe.Set(ctx, schedulerAccountKey(id), fullPayload, 0)
		pipe.Set(ctx, schedulerAccountMetaKey(id), metaPayload, 0)
		// Keep hot LastUsedAt state untouched when stale snapshots are rebuilt.
		accountIDs = append(accountIDs, account.ID)
		pending++
		if pending >= c.writeChunkSize {
			if err := flush(); err != nil {
				return nil, err
			}
		}
	}

	if err := flush(); err != nil {
		return nil, err
	}
	return accountIDs, nil
}

func marshalSchedulerCacheAccount(account service.Account) ([]byte, []byte, error) {
	fullPayload, err := json.Marshal(account)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal account: %w", err)
	}
	metaPayload, err := json.Marshal(buildSchedulerMetadataAccount(account))
	if err != nil {
		return nil, nil, fmt.Errorf("marshal account metadata: %w", err)
	}
	return fullPayload, metaPayload, nil
}

func (c *schedulerCache) mgetChunked(ctx context.Context, keys []string) ([]any, error) {
	if len(keys) == 0 {
		return []any{}, nil
	}

	out := make([]any, 0, len(keys))
	chunkSize := c.mgetChunkSize
	if chunkSize <= 0 {
		chunkSize = defaultSchedulerSnapshotMGetChunkSize
	}
	for start := 0; start < len(keys); start += chunkSize {
		end := start + chunkSize
		if end > len(keys) {
			end = len(keys)
		}
		part, err := c.rdb.MGet(ctx, keys[start:end]...).Result()
		if err != nil {
			return nil, err
		}
		out = append(out, part...)
	}
	return out, nil
}

func buildSchedulerMetadataAccount(account service.Account) service.Account {
	return service.Account{
		ID:                      account.ID,
		Name:                    account.Name,
		Platform:                account.Platform,
		Type:                    account.Type,
		Concurrency:             account.Concurrency,
		LoadFactor:              account.LoadFactor,
		Priority:                account.Priority,
		RateMultiplier:          account.RateMultiplier,
		Status:                  account.Status,
		LastUsedAt:              account.LastUsedAt,
		ExpiresAt:               account.ExpiresAt,
		AutoPauseOnExpired:      account.AutoPauseOnExpired,
		Schedulable:             account.Schedulable,
		RateLimitedAt:           account.RateLimitedAt,
		RateLimitResetAt:        account.RateLimitResetAt,
		OverloadUntil:           account.OverloadUntil,
		TempUnschedulableUntil:  account.TempUnschedulableUntil,
		TempUnschedulableReason: account.TempUnschedulableReason,
		SessionWindowStart:      account.SessionWindowStart,
		SessionWindowEnd:        account.SessionWindowEnd,
		SessionWindowStatus:     account.SessionWindowStatus,
		AccountGroups:           filterSchedulerAccountGroups(account.AccountGroups),
		GroupIDs:                filterSchedulerGroupIDs(account.GroupIDs, account.AccountGroups),
		Credentials:             filterSchedulerCredentials(account.Credentials),
		Extra:                   filterSchedulerExtra(account.Extra),
	}
}

func filterSchedulerAccountGroups(accountGroups []service.AccountGroup) []service.AccountGroup {
	if len(accountGroups) == 0 {
		return nil
	}

	filtered := make([]service.AccountGroup, 0, len(accountGroups))
	for _, ag := range accountGroups {
		if ag.GroupID <= 0 {
			continue
		}
		filtered = append(filtered, service.AccountGroup{
			AccountID: ag.AccountID,
			GroupID:   ag.GroupID,
			Priority:  ag.Priority,
			CreatedAt: ag.CreatedAt,
		})
	}
	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

func filterSchedulerGroupIDs(groupIDs []int64, accountGroups []service.AccountGroup) []int64 {
	if len(groupIDs) == 0 && len(accountGroups) == 0 {
		return nil
	}

	seen := make(map[int64]struct{}, len(groupIDs)+len(accountGroups))
	filtered := make([]int64, 0, len(groupIDs)+len(accountGroups))
	for _, id := range groupIDs {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		filtered = append(filtered, id)
	}
	for _, ag := range accountGroups {
		if ag.GroupID <= 0 {
			continue
		}
		if _, ok := seen[ag.GroupID]; ok {
			continue
		}
		seen[ag.GroupID] = struct{}{}
		filtered = append(filtered, ag.GroupID)
	}
	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

func filterSchedulerCredentials(credentials map[string]any) map[string]any {
	if len(credentials) == 0 {
		return nil
	}
	// compact_model_mapping 参与 compact 路径的通道上游模型限制检查，须随 meta 保留。
	keys := []string{"model_mapping", "project_id", "oauth_type", "openai_capabilities", "compact_model_mapping"}
	filtered := make(map[string]any)
	for _, key := range keys {
		if value, ok := credentials[key]; ok && value != nil {
			filtered[key] = value
		}
	}
	if value, ok := credentials["api_key"].(string); ok && strings.TrimSpace(value) != "" {
		// Snapshot metadata must support API-key routing decisions without storing the secret.
		filtered["has_api_key"] = true
	}
	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

func filterSchedulerExtra(extra map[string]any) map[string]any {
	if len(extra) == 0 {
		return nil
	}
	keys := []string{
		"quota_limit",
		"quota_used",
		"quota_daily_limit",
		"quota_daily_used",
		"quota_daily_start",
		"quota_daily_reset_mode",
		"quota_daily_reset_hour",
		"quota_weekly_limit",
		"quota_weekly_used",
		"quota_weekly_start",
		"quota_weekly_reset_mode",
		"quota_weekly_reset_day",
		"quota_weekly_reset_hour",
		"quota_reset_timezone",
		"mixed_scheduling",
		"window_cost_limit",
		"window_cost_sticky_reserve",
		"max_sessions",
		"session_idle_timeout_minutes",
		"openai_oauth_ws_mode",
		"openai_oauth_responses_websockets_v2_enabled",
		"openai_oauth_responses_websockets_v2_mode",
		"openai_apikey_responses_websockets_v2_enabled",
		"openai_apikey_responses_websockets_v2_mode",
		"responses_websockets_v2_enabled",
		"openai_ws_enabled",
		"openai_ws_force_http",
		"openai_responses_mode",
		"openai_responses_supported",
		"privacy_mode",
		// 以下三个字段是候选资格复检的调度决策输入（passthrough 快速通道 /
		// compact tier 判定），全量 payload 中包含但 meta 白名单此前遗漏；
		// 批量刷新改读 meta 后必须保留，否则复检语义会静默退化。
		"openai_passthrough",
		"openai_compact_mode",
		"openai_compact_supported",
		"codex_5h_used_percent",
		"codex_7d_used_percent",
		"codex_5h_reset_at",
		"codex_7d_reset_at",
		"codex_5h_reset_after_seconds",
		"codex_7d_reset_after_seconds",
		"codex_usage_updated_at",
		"auto_pause_5h_threshold",
		"auto_pause_7d_threshold",
		"auto_pause_5h_disabled",
		"auto_pause_7d_disabled",
		"model_rate_limits",
	}
	filtered := make(map[string]any)
	for _, key := range keys {
		if value, ok := extra[key]; ok && value != nil {
			filtered[key] = value
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	return filtered
}
