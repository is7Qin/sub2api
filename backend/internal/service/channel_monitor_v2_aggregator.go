package service

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	channelMonitorV2AggregatorLockKey  = "channel-monitor-v2-aggregator"
	channelMonitorV2AggregationTick    = time.Minute
	channelMonitorV2AggregationTimeout = 55 * time.Second
	channelMonitorV2AggregatorLockTTL  = 2 * time.Minute

	channelMonitorV2RetentionMax   = 90 * 24 * time.Hour
	channelMonitorV2BootstrapFirst = 2 * time.Hour
	channelMonitorV2RecentOverlap  = 10 * time.Minute

	channelMonitorV2BackfillChunkInit = time.Hour
	channelMonitorV2MinBackfillChunk  = 15 * time.Minute
	channelMonitorV2MaxChunkNear1d    = 2 * time.Hour
	channelMonitorV2MaxChunkNear7d    = 4 * time.Hour
	channelMonitorV2MaxChunkFar       = 6 * time.Hour
	channelMonitorV2GrowChunkUnder    = 15 * time.Second
	channelMonitorV2MaxBackoff        = 10 * time.Minute
)

// ChannelMonitorV2Aggregator owns aggregation state only. Scheduling and lifecycle
// are delegated to the unified worker runtime via NewChannelMonitorV2AggregationWorker.
type ChannelMonitorV2Aggregator struct {
	repo      ChannelMonitorV2Repository
	settings  channelMonitorRuntimeReader
	lockCache LeaderLockCache
	db        *sql.DB
	owner     string
	now       func() time.Time

	mu               sync.Mutex
	backfillAt       time.Time
	backfillChunk    time.Duration
	backfillFailures int
	nextRunAt        time.Time
	cursorLoaded     bool
	hasAggregated    bool
}

func NewChannelMonitorV2Aggregator(
	repo ChannelMonitorV2Repository,
	settings channelMonitorRuntimeReader,
	lockCache LeaderLockCache,
	db *sql.DB,
) *ChannelMonitorV2Aggregator {
	return &ChannelMonitorV2Aggregator{
		repo:          repo,
		settings:      settings,
		lockCache:     lockCache,
		db:            db,
		owner:         uuid.NewString(),
		now:           func() time.Time { return time.Now().UTC() },
		backfillChunk: channelMonitorV2BackfillChunkInit,
	}
}

func (s *ChannelMonitorV2Aggregator) Interval() time.Duration {
	return channelMonitorV2AggregationTick
}

// Run executes at most one bounded aggregation cycle. The fixed one-minute runtime
// tick is throttled here to the persisted 60/300-second refresh interval.
func (s *ChannelMonitorV2Aggregator) Run(ctx context.Context) error {
	if s == nil || s.repo == nil || s.settings == nil {
		return nil
	}
	if !s.settings.GetChannelMonitorRuntime(ctx).PassiveAggregationAllowed() {
		return nil
	}

	cfg, err := s.repo.GetConfig(ctx)
	if err != nil {
		return fmt.Errorf("get channel monitor v2 config: %w", err)
	}
	if cfg == nil || !cfg.Enabled {
		return nil
	}

	now := s.now().UTC().Truncate(time.Minute)
	interval := channelMonitorV2RefreshInterval(cfg.RefreshIntervalSeconds)
	s.mu.Lock()
	if !s.nextRunAt.IsZero() && now.Before(s.nextRunAt) {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	runCtx, cancel := context.WithTimeout(ctx, channelMonitorV2AggregationTimeout)
	defer cancel()
	release, acquired := tryAcquireSingletonLeaderLock(
		runCtx, s.lockCache, s.db, channelMonitorV2AggregatorLockKey, s.owner, channelMonitorV2AggregatorLockTTL,
	)
	if !acquired {
		return nil
	}
	defer release()

	s.mu.Lock()
	s.nextRunAt = now.Add(interval)
	s.mu.Unlock()
	return s.runOnce(runCtx, now)
}

func channelMonitorV2RefreshInterval(seconds int) time.Duration {
	if seconds == 60 {
		return time.Minute
	}
	return 5 * time.Minute
}

func (s *ChannelMonitorV2Aggregator) runOnce(ctx context.Context, now time.Time) error {
	if err := s.ensureCursor(ctx, now); err != nil {
		return fmt.Errorf("load channel monitor v2 watermark: %w", err)
	}

	s.mu.Lock()
	cursor := s.backfillAt
	hasData := s.hasAggregated
	s.mu.Unlock()

	if !hasData || cursor.IsZero() {
		start := now.Add(-channelMonitorV2BootstrapFirst)
		started := time.Now()
		if err := s.repo.RecomputeRange(ctx, start, now); err != nil {
			s.recordBackfillFailure(now, cursor)
			return fmt.Errorf("bootstrap channel monitor v2: %w", err)
		}
		s.recordBackfillSuccess(start, time.Since(started), now)
		return nil
	}

	if err := s.repo.RecomputeRange(ctx, now.Add(-channelMonitorV2RecentOverlap), now); err != nil {
		return fmt.Errorf("refresh channel monitor v2 overlap: %w", err)
	}

	retentionCutoff := now.Add(-channelMonitorV2RetentionMax)
	if !cursor.After(retentionCutoff) {
		return nil
	}

	s.mu.Lock()
	chunk := s.backfillChunk
	s.mu.Unlock()
	if chunk <= 0 {
		chunk = channelMonitorV2BackfillChunkInit
	}
	maxChunk := channelMonitorV2MaxChunkForDepth(now, cursor)
	if chunk > maxChunk {
		chunk = maxChunk
	}
	if chunk < channelMonitorV2MinBackfillChunk {
		chunk = channelMonitorV2MinBackfillChunk
	}
	start := cursor.Add(-chunk)
	if cursor.Before(now.Add(-7 * 24 * time.Hour)) {
		if aligned := start.Truncate(24 * time.Hour); aligned.Before(cursor) {
			start = aligned
		}
	}
	if start.Before(retentionCutoff) {
		start = retentionCutoff
	}
	if !start.Before(cursor) {
		return nil
	}

	started := time.Now()
	if err := s.repo.RecomputeRange(ctx, start, cursor); err != nil {
		s.recordBackfillFailure(now, cursor)
		return fmt.Errorf("backfill channel monitor v2: %w", err)
	}
	s.recordBackfillSuccess(start, time.Since(started), now)
	return nil
}

func channelMonitorV2MaxChunkForDepth(now, end time.Time) time.Duration {
	switch age := now.Sub(end); {
	case age < 24*time.Hour:
		return channelMonitorV2MaxChunkNear1d
	case age < 7*24*time.Hour:
		return channelMonitorV2MaxChunkNear7d
	default:
		return channelMonitorV2MaxChunkFar
	}
}

func (s *ChannelMonitorV2Aggregator) recordBackfillSuccess(coveredFrom time.Time, elapsed time.Duration, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.backfillAt = coveredFrom
	s.hasAggregated = true
	s.backfillFailures = 0
	maxChunk := channelMonitorV2MaxChunkForDepth(now, coveredFrom)
	if elapsed > 0 && elapsed < channelMonitorV2GrowChunkUnder {
		next := s.backfillChunk
		if next <= 0 {
			next = channelMonitorV2BackfillChunkInit
		}
		next = time.Duration(float64(next) * 1.5)
		if next > maxChunk {
			next = maxChunk
		}
		if next < channelMonitorV2MinBackfillChunk {
			next = channelMonitorV2MinBackfillChunk
		}
		s.backfillChunk = next
		return
	}
	if s.backfillChunk <= 0 || s.backfillChunk > maxChunk {
		s.backfillChunk = channelMonitorV2BackfillChunkInit
		if s.backfillChunk > maxChunk {
			s.backfillChunk = maxChunk
		}
	}
}

func (s *ChannelMonitorV2Aggregator) recordBackfillFailure(now, end time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.backfillFailures++
	if s.backfillChunk <= 0 {
		s.backfillChunk = channelMonitorV2BackfillChunkInit
	}
	s.backfillChunk /= 2
	if s.backfillChunk < channelMonitorV2MinBackfillChunk {
		s.backfillChunk = channelMonitorV2MinBackfillChunk
	}
	if maxChunk := channelMonitorV2MaxChunkForDepth(now, end); s.backfillChunk > maxChunk {
		s.backfillChunk = maxChunk
	}
	backoff := time.Minute << uint(s.backfillFailures-1)
	if backoff > channelMonitorV2MaxBackoff {
		backoff = channelMonitorV2MaxBackoff
	}
	candidate := now.Add(backoff)
	if candidate.After(s.nextRunAt) {
		s.nextRunAt = candidate
	}
}

func (s *ChannelMonitorV2Aggregator) ensureCursor(ctx context.Context, now time.Time) error {
	s.mu.Lock()
	loaded := s.cursorLoaded
	s.mu.Unlock()
	if loaded {
		return nil
	}
	wm, err := s.repo.GetAggregationWatermark(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cursorLoaded {
		return nil
	}
	if wm != nil {
		if !wm.BackfillCursor.IsZero() {
			s.backfillAt = wm.BackfillCursor.UTC().Truncate(time.Minute)
		}
		if wm.HasData || !wm.DataThrough.IsZero() {
			s.hasAggregated = true
			if s.backfillAt.IsZero() && !wm.DataThrough.IsZero() {
				inferred := wm.DataThrough.UTC().Truncate(time.Minute).Add(-channelMonitorV2BootstrapFirst)
				if inferred.After(now) {
					inferred = now.Add(-channelMonitorV2BootstrapFirst)
				}
				s.backfillAt = inferred
			}
		}
	}
	s.cursorLoaded = true
	return nil
}
