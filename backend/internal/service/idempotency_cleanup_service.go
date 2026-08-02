package service

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// IdempotencyCleanupService cleans expired idempotency records to prevent unbounded table growth.
type IdempotencyCleanupService struct {
	repo     IdempotencyRepository
	interval time.Duration
	batch    int
}

func NewIdempotencyCleanupService(repo IdempotencyRepository, cfg *config.Config) *IdempotencyCleanupService {
	interval := 60 * time.Second
	batch := 500
	if cfg != nil {
		if cfg.Idempotency.CleanupIntervalSeconds > 0 {
			interval = time.Duration(cfg.Idempotency.CleanupIntervalSeconds) * time.Second
		}
		if cfg.Idempotency.CleanupBatchSize > 0 {
			batch = cfg.Idempotency.CleanupBatchSize
		}
	}
	return &IdempotencyCleanupService{
		repo:     repo,
		interval: interval,
		batch:    batch,
	}
}

// Interval returns the configured worker interval.
func (s *IdempotencyCleanupService) Interval() time.Duration {
	if s == nil {
		return 0
	}
	return s.interval
}

// BatchSize returns the configured cleanup batch size.
func (s *IdempotencyCleanupService) BatchSize() int {
	if s == nil {
		return 0
	}
	return s.batch
}

// Start is retained as a no-op while runtime lifecycle ownership is introduced.
func (s *IdempotencyCleanupService) Start() {}

// Run deletes one batch of expired idempotency records.
func (s *IdempotencyCleanupService) Run(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	deleted, err := s.repo.DeleteExpired(ctx, time.Now(), s.batch)
	if err != nil {
		logger.LegacyPrintf("service.idempotency_cleanup", "[IdempotencyCleanup] cleanup failed err=%v", err)
		return err
	}
	if deleted > 0 {
		logger.LegacyPrintf("service.idempotency_cleanup", "[IdempotencyCleanup] cleaned expired records count=%d", deleted)
	}
	return nil
}
