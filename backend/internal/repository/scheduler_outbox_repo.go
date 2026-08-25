package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type schedulerOutboxRepository struct {
	db *sql.DB
}

func NewSchedulerOutboxRepository(db *sql.DB) service.SchedulerOutboxRepository {
	return &schedulerOutboxRepository{db: db}
}

func (r *schedulerOutboxRepository) ListAfterAndReleaseDedup(ctx context.Context, afterID int64, limit int) ([]service.SchedulerOutboxEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	// Sequence IDs are not commit ordered. Rows that commit below the watermark
	// cannot be replayed safely with an ID-only watermark, but their pending keys
	// must be released so future same-key scheduler events are not suppressed.
	rows, err := r.db.QueryContext(ctx, `
			WITH stranded AS (
				UPDATE scheduler_outbox
				SET dedup_key = NULL
				WHERE id <= $1
					AND dedup_key IS NOT NULL
				RETURNING id
			), selected AS MATERIALIZED (
				SELECT id, event_type, account_id, group_id, payload, created_at
				FROM scheduler_outbox
				WHERE id > $1
				ORDER BY id ASC
				LIMIT $2
				FOR UPDATE
			), released AS (
				UPDATE scheduler_outbox AS o
				SET dedup_key = NULL
				FROM selected AS s
				WHERE o.id = s.id
					AND o.dedup_key IS NOT NULL
				RETURNING o.id
			)
			SELECT s.id, s.event_type, s.account_id, s.group_id, s.payload, s.created_at
			FROM selected AS s
			CROSS JOIN (SELECT COUNT(*) FROM stranded) AS stranded_barrier
			CROSS JOIN (SELECT COUNT(*) FROM released) AS release_barrier
			ORDER BY s.id ASC
		`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = rows.Close()
	}()

	events := make([]service.SchedulerOutboxEvent, 0, limit)
	for rows.Next() {
		var (
			payloadRaw []byte
			accountID  sql.NullInt64
			groupID    sql.NullInt64
			event      service.SchedulerOutboxEvent
		)
		if err := rows.Scan(&event.ID, &event.EventType, &accountID, &groupID, &payloadRaw, &event.CreatedAt); err != nil {
			return nil, err
		}
		if accountID.Valid {
			v := accountID.Int64
			event.AccountID = &v
		}
		if groupID.Valid {
			v := groupID.Int64
			event.GroupID = &v
		}
		if len(payloadRaw) > 0 {
			var payload map[string]any
			if err := json.Unmarshal(payloadRaw, &payload); err != nil {
				return nil, err
			}
			event.Payload = payload
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

// CleanupConsumed 批量删除 id <= watermark 的已消费行。
// watermark 仅在整批事件全部成功后才推进（scheduler_snapshot_service），
// 因此这些行已完全消费（含 dedup_key 释放），删除不丢事件；
// enqueue 的 dedup 冲突子句引用 MAX(id)，不受删除影响。
func (r *schedulerOutboxRepository) CleanupConsumed(ctx context.Context, watermark int64, limit int) (int64, error) {
	if r == nil || r.db == nil {
		return 0, errors.New("scheduler outbox database is nil")
	}
	if watermark <= 0 {
		return 0, nil
	}
	if limit <= 0 {
		limit = 5000
	}
	result, err := r.db.ExecContext(ctx, `
		DELETE FROM scheduler_outbox
		WHERE id IN (
			SELECT id FROM scheduler_outbox
			WHERE id <= $1
			ORDER BY id
			LIMIT $2
		)
	`, watermark, limit)
	if err != nil {
		return 0, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return affected, nil
}

func (r *schedulerOutboxRepository) MaxID(ctx context.Context) (int64, error) {
	var maxID int64
	if err := r.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(id), 0) FROM scheduler_outbox").Scan(&maxID); err != nil {
		return 0, err
	}
	return maxID, nil
}

func enqueueSchedulerOutbox(ctx context.Context, exec sqlExecutor, eventType string, accountID *int64, groupID *int64, payload any) error {
	if exec == nil {
		return nil
	}
	var payloadArg any
	var payloadJSON []byte
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		payloadArg = encoded
		payloadJSON = encoded
	}
	query := `
			INSERT INTO scheduler_outbox (event_type, account_id, group_id, payload)
			VALUES ($1, $2, $3, $4)
		`
	args := []any{eventType, accountID, groupID, payloadArg}
	if schedulerOutboxEventSupportsDedup(eventType) {
		dedupKey := schedulerOutboxDedupKey(eventType, accountID, groupID, payloadJSON)
		// Refresh a lagging conflicting row to the proposed id so a same-key event
		// can requeue a late-commit row that is already below the Redis watermark.
		query = `
				INSERT INTO scheduler_outbox (event_type, account_id, group_id, payload, dedup_key)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (dedup_key) WHERE dedup_key IS NOT NULL DO UPDATE
				SET id = EXCLUDED.id,
					event_type = EXCLUDED.event_type,
					account_id = EXCLUDED.account_id,
					group_id = EXCLUDED.group_id,
					payload = EXCLUDED.payload,
					created_at = EXCLUDED.created_at
				WHERE scheduler_outbox.id < (SELECT COALESCE(MAX(id), 0) FROM scheduler_outbox)
			`
		args = append(args, dedupKey)
	}
	_, err := exec.ExecContext(ctx, query, args...)
	return err
}

func schedulerOutboxDedupKey(eventType string, accountID *int64, groupID *int64, payloadJSON []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte(eventType))
	_, _ = h.Write([]byte{0})
	if accountID != nil {
		_, _ = h.Write([]byte(strconv.FormatInt(*accountID, 10)))
	}
	_, _ = h.Write([]byte{0})
	if groupID != nil {
		_, _ = h.Write([]byte(strconv.FormatInt(*groupID, 10)))
	}
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(payloadJSON)
	return fmt.Sprintf("scheduler_outbox:%s", hex.EncodeToString(h.Sum(nil)))
}

func schedulerOutboxEventSupportsDedup(eventType string) bool {
	switch eventType {
	case service.SchedulerOutboxEventAccountChanged,
		service.SchedulerOutboxEventGroupChanged,
		service.SchedulerOutboxEventFullRebuild:
		return true
	default:
		return false
	}
}
