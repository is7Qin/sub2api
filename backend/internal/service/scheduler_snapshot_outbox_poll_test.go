package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type outboxPollCache struct {
	watermark     int64
	setWatermarks []int64
	updateErr     error
}

func (c *outboxPollCache) GetSnapshot(context.Context, SchedulerBucket) ([]*Account, bool, error) {
	return nil, false, nil
}

func (c *outboxPollCache) SetSnapshot(context.Context, SchedulerBucket, []Account) error {
	return nil
}

func (c *outboxPollCache) GetAccount(context.Context, int64) (*Account, error) {
	return nil, nil
}

func (c *outboxPollCache) SetAccount(context.Context, *Account) error {
	return nil
}

func (c *outboxPollCache) DeleteAccount(context.Context, int64) error {
	return nil
}

func (c *outboxPollCache) UpdateLastUsed(context.Context, map[int64]time.Time) error {
	return c.updateErr
}

func (c *outboxPollCache) TryLockBucket(context.Context, SchedulerBucket, time.Duration) (string, bool, error) {
	return "test-lock", true, nil
}

func (c *outboxPollCache) UnlockBucket(context.Context, SchedulerBucket, string) error {
	return nil
}

func (c *outboxPollCache) ListBuckets(context.Context) ([]SchedulerBucket, error) {
	return nil, nil
}

func (c *outboxPollCache) GetOutboxWatermark(context.Context) (int64, error) {
	return c.watermark, nil
}

func (c *outboxPollCache) SetOutboxWatermark(_ context.Context, id int64) error {
	c.watermark = id
	c.setWatermarks = append(c.setWatermarks, id)
	return nil
}

type outboxPollRepo struct {
	events     []SchedulerOutboxEvent
	rows       []int64
	maxIDCalls int
}

func (r *outboxPollRepo) ListAfterAndReleaseDedup(_ context.Context, afterID int64, limit int) ([]SchedulerOutboxEvent, error) {
	events := make([]SchedulerOutboxEvent, 0, len(r.events))
	for _, event := range r.events {
		if event.ID <= afterID {
			continue
		}
		events = append(events, event)
		if limit > 0 && len(events) >= limit {
			break
		}
	}
	return events, nil
}

func (r *outboxPollRepo) MaxID(context.Context) (int64, error) {
	r.maxIDCalls++
	var maxID int64
	for _, id := range r.rows {
		if id > maxID {
			maxID = id
		}
	}
	return maxID, nil
}

func (r *outboxPollRepo) CleanupConsumed(context.Context, int64, int) (int64, error) {
	return 0, nil
}

func TestSchedulerSnapshotServicePollOutboxAdvancesWatermarkAfterHandling(t *testing.T) {
	cache := &outboxPollCache{}
	repo := &outboxPollRepo{
		events: []SchedulerOutboxEvent{
			{ID: 10000, EventType: SchedulerOutboxEventAccountLastUsed},
		},
		rows: []int64{1, 10000, 10001},
	}
	svc := NewSchedulerSnapshotService(cache, repo, nil, nil, nil)

	svc.pollOutbox()

	if cache.watermark != 10000 {
		t.Fatalf("expected watermark 10000, got %d", cache.watermark)
	}
	if len(cache.setWatermarks) != 1 || cache.setWatermarks[0] != 10000 {
		t.Fatalf("unexpected watermark writes: %#v", cache.setWatermarks)
	}
}

func TestSchedulerSnapshotServicePollOutboxSkipsLegacyLagArithmeticInDirtyMode(t *testing.T) {
	cache := &outboxPollCache{}
	repo := &outboxPollRepo{
		events: []SchedulerOutboxEvent{{
			ID:        100,
			CreatedAt: time.Now().Add(-time.Minute),
			EventType: SchedulerOutboxEventAccountChanged,
		}},
		rows: []int64{100, 20000},
	}
	dirtyRepo := &dirtyWorkTestRepo{}
	svc := &SchedulerSnapshotService{
		cache:         cache,
		outboxRepo:    repo,
		dirtyWorkRepo: dirtyRepo,
		ownershipRepo: dirtyWorkTestOwnershipRepo{},
		cfg: &config.Config{Gateway: config.GatewayConfig{Scheduling: config.GatewaySchedulingConfig{
			OutboxBacklogRebuildRows: 1,
		}}},
	}

	svc.pollOutbox()

	require.Zero(t, repo.maxIDCalls)
	require.Zero(t, dirtyRepo.fullRebuildRequests)
}

func TestSchedulerSnapshotServicePollOutboxDoesNotAdvanceWatermarkOnHandleFailure(t *testing.T) {
	cache := &outboxPollCache{
		updateErr: errors.New("cache update failed"),
	}
	repo := &outboxPollRepo{
		events: []SchedulerOutboxEvent{
			{
				ID:        5,
				EventType: SchedulerOutboxEventAccountLastUsed,
				Payload: map[string]any{
					"last_used": map[string]any{"101": float64(123)},
				},
			},
		},
	}
	svc := NewSchedulerSnapshotService(cache, repo, nil, nil, nil)

	svc.pollOutbox()

	if len(cache.setWatermarks) != 0 {
		t.Fatalf("expected no watermark write on handle failure, got %#v", cache.setWatermarks)
	}
}
