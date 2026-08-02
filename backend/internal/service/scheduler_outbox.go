package service

import (
	"context"
	"time"
)

type SchedulerOutboxEvent struct {
	ID        int64
	EventType string
	AccountID *int64
	GroupID   *int64
	Payload   map[string]any
	CreatedAt time.Time
}

// SchedulerOutboxRepository 提供调度 outbox 的读取接口。
type SchedulerOutboxRepository interface {
	ListAfterAndReleaseDedup(ctx context.Context, afterID int64, limit int) ([]SchedulerOutboxEvent, error)
	MaxID(ctx context.Context) (int64, error)
	// CleanupConsumed 批量删除 id <= watermark 的已消费行（watermark 仅在整批
	// 事件全部成功后才推进，因此这些行不会再有重放需求）。
	CleanupConsumed(ctx context.Context, watermark int64, limit int) (int64, error)
}
