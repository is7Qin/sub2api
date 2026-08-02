package service

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

const (
	outboxCleanupInterval = time.Minute
	// 单周期超时需小于 leader 锁 TTL，避免锁到期后仍有清理在跑。
	outboxCleanupCycleTimeout = 25 * time.Second
	outboxCleanupLockTTL      = 30 * time.Second
	outboxCleanupBatchSize    = 5000
	// 每周期最多清理的批次：限速避免清理瞬间占满磁盘 IO / WAL。
	outboxCleanupMaxBatches = 10
)

const outboxCleanupLeaderLockKey = "outbox-cleanup"

// OutboxCleanupService 定期清理已终态 / 已消费的 outbox 行：
//   - billing_attempt_outbox：succeeded/terminal 且超过保留期的行；
//   - scheduler_outbox：id <= Redis watermark 的已消费行（watermark 仅在整批
//     事件全部成功后才推进，删除不丢事件）。
//
// terminalRetention 为 billing 终态行保留期（表注释明确这些行"retained for
// reconciliation"）；<= 0 表示禁用该清理目标。计费幂等由
// usage_billing_dedup / usage_billing_dedup_archive 独立兜底，不依赖 outbox 行。
// 生命周期由 server worker runtime 统一管理（见 NewOutboxCleanupWorker）；
// Run 内部通过 Redis 单例 leader 锁保证多副本下每周期只有一个实例执行，
// Redis 故障时回退 PostgreSQL advisory lock。
type OutboxCleanupService struct {
	billingRepo       BillingOutboxRepository
	schedulerRepo     SchedulerOutboxRepository
	schedulerCache    SchedulerCache
	lockCache         LeaderLockCache
	db                *sql.DB
	instanceID        string
	terminalRetention time.Duration
}

func NewOutboxCleanupService(billingRepo BillingOutboxRepository, schedulerRepo SchedulerOutboxRepository, schedulerCache SchedulerCache, terminalRetention time.Duration) *OutboxCleanupService {
	return &OutboxCleanupService{
		billingRepo:       billingRepo,
		schedulerRepo:     schedulerRepo,
		schedulerCache:    schedulerCache,
		instanceID:        uuid.NewString(),
		terminalRetention: terminalRetention,
	}
}

// SetLeaderLock 注入用于多副本互斥的 leader 锁后端。
func (s *OutboxCleanupService) SetLeaderLock(lockCache LeaderLockCache, db *sql.DB) {
	if s == nil {
		return
	}
	s.lockCache = lockCache
	s.db = db
}

// Interval 返回 worker runtime 使用的执行周期。
func (s *OutboxCleanupService) Interval() time.Duration {
	return outboxCleanupInterval
}

// Start 保留为空实现：生命周期由 server worker runtime 统一接管。
func (s *OutboxCleanupService) Start() {}

// Run 执行一轮 outbox 保留期清理。
func (s *OutboxCleanupService) Run(ctx context.Context) error {
	if s == nil || (s.billingRepo == nil && s.schedulerRepo == nil) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, outboxCleanupCycleTimeout)
	defer cancel()

	release, ok := tryAcquireSingletonLeaderLock(ctx, s.lockCache, s.db, outboxCleanupLeaderLockKey, s.instanceID, outboxCleanupLockTTL)
	if !ok {
		return nil
	}
	defer release()

	if s.terminalRetention <= 0 {
		// 保留期配置为 0 时禁用 billing 终态行清理（scheduler 清理不受影响）。
		if s.schedulerRepo == nil {
			return nil
		}
		return s.cleanupScheduler(ctx)
	}
	cutoff := time.Now().UTC().Add(-s.terminalRetention)
	if s.billingRepo != nil {
		for i := 0; i < outboxCleanupMaxBatches; i++ {
			deleted, err := s.billingRepo.CleanupTerminal(ctx, cutoff, outboxCleanupBatchSize)
			if err != nil {
				slog.Warn("outbox cleanup billing failed", "error", err)
				return err
			}
			if deleted < outboxCleanupBatchSize {
				break
			}
		}
	}
	return s.cleanupScheduler(ctx)
}

// cleanupScheduler 删除 id <= Redis watermark 的已消费行。
func (s *OutboxCleanupService) cleanupScheduler(ctx context.Context) error {
	if s.schedulerRepo == nil || s.schedulerCache == nil {
		return nil
	}
	watermark, err := s.schedulerCache.GetOutboxWatermark(ctx)
	if err != nil {
		slog.Warn("outbox cleanup scheduler watermark read failed", "error", err)
		return err
	}
	for i := 0; i < outboxCleanupMaxBatches && watermark > 0; i++ {
		deleted, err := s.schedulerRepo.CleanupConsumed(ctx, watermark, outboxCleanupBatchSize)
		if err != nil {
			slog.Warn("outbox cleanup scheduler failed", "error", err)
			return err
		}
		if deleted < outboxCleanupBatchSize {
			break
		}
	}
	return nil
}
