package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type billingOutboxRepository struct {
	db *sql.DB
}

func NewBillingOutboxRepository(db *sql.DB) service.BillingOutboxRepository {
	return &billingOutboxRepository{db: db}
}

func (r *billingOutboxRepository) Enqueue(ctx context.Context, command *service.BillingOutboxCommand) (*service.BillingOutboxRecord, error) {
	if command == nil {
		return nil, service.ErrBillingOutboxAttemptIDRequired
	}
	command.Normalize()
	if err := command.Validate(); err != nil {
		return nil, err
	}
	if r == nil || r.db == nil {
		return nil, errors.New("billing outbox database is nil")
	}
	payload, err := json.Marshal(command)
	if err != nil {
		return nil, err
	}
	query := `
		INSERT INTO billing_attempt_outbox (attempt_id, api_key_id, request_fingerprint, command)
		VALUES ($1, $2, $3, $4::jsonb)
		ON CONFLICT (attempt_id, api_key_id) DO NOTHING
		RETURNING id, status, attempts, available_at, lease_until, leased_by, last_error, created_at, updated_at
	`
	record := &service.BillingOutboxRecord{Command: *command}
	var leaseUntil sql.NullTime
	var leasedBy, lastError sql.NullString
	err = r.db.QueryRowContext(ctx, query, command.AttemptID, command.APIKeyID, command.RequestFingerprint, payload).
		Scan(&record.ID, &record.Status, &record.Attempts, &record.AvailableAt, &leaseUntil, &leasedBy, &lastError, &record.CreatedAt, &record.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		record, err = r.findByIdentity(ctx, command.AttemptID, command.APIKeyID)
		if err != nil {
			return nil, err
		}
		if record.Command.RequestFingerprint != command.RequestFingerprint {
			return nil, service.ErrBillingOutboxFingerprintConflict
		}
		return record, nil
	}
	if err != nil {
		return nil, err
	}
	setNullableOutboxFields(record, leaseUntil, leasedBy, lastError)
	return record, nil
}

func (r *billingOutboxRepository) findByIdentity(ctx context.Context, attemptID string, apiKeyID int64) (*service.BillingOutboxRecord, error) {
	record := &service.BillingOutboxRecord{}
	var payload, applyResult []byte
	var requestFingerprint string
	var leaseUntil sql.NullTime
	var leasedBy, lastError sql.NullString
	err := r.db.QueryRowContext(ctx, `
		SELECT id, attempt_id, api_key_id, request_fingerprint, command, status, attempts, apply_result,
			available_at, lease_until, leased_by, last_error, created_at, updated_at
		FROM billing_attempt_outbox
		WHERE attempt_id = $1 AND api_key_id = $2
	`, attemptID, apiKeyID).Scan(
		&record.ID, &attemptID, &apiKeyID, &requestFingerprint, &payload,
		&record.Status, &record.Attempts, &applyResult, &record.AvailableAt, &leaseUntil, &leasedBy, &lastError,
		&record.CreatedAt, &record.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(payload, &record.Command); err != nil {
		return nil, err
	}
	if len(applyResult) > 0 && string(applyResult) != "null" {
		record.ApplyResult = &service.UsageBillingApplyResult{}
		if err := json.Unmarshal(applyResult, record.ApplyResult); err != nil {
			return nil, err
		}
	}
	// The dedicated identity columns are authoritative, including collision
	// checks after an idempotent enqueue. Payload identity must not redefine it.
	record.Command.AttemptID = attemptID
	record.Command.APIKeyID = apiKeyID
	record.Command.RequestFingerprint = requestFingerprint
	record.Command.Normalize()
	setNullableOutboxFields(record, leaseUntil, leasedBy, lastError)
	return record, nil
}

func setNullableOutboxFields(record *service.BillingOutboxRecord, leaseUntil sql.NullTime, leasedBy, lastError sql.NullString) {
	if leaseUntil.Valid {
		record.LeaseUntil = &leaseUntil.Time
	}
	if leasedBy.Valid {
		record.LeasedBy = leasedBy.String
	}
	if lastError.Valid {
		record.LastError = lastError.String
	}
}

func (r *billingOutboxRepository) Claim(ctx context.Context, workerID string, limit int, lease time.Duration) ([]service.BillingOutboxRecord, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("billing outbox database is nil")
	}
	workerID = strings.TrimSpace(workerID)
	if workerID == "" {
		return nil, errors.New("billing outbox worker id is required")
	}
	if limit <= 0 {
		limit = 100
	}
	leaseSeconds := int64(lease / time.Second)
	if leaseSeconds < 1 {
		leaseSeconds = 30
	}
	rows, err := r.db.QueryContext(ctx, `
		WITH candidates AS (
			SELECT id
			FROM billing_attempt_outbox
			WHERE (status = 'pending' AND available_at <= NOW())
			   OR (status = 'processing' AND lease_until <= NOW())
			ORDER BY id ASC
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		UPDATE billing_attempt_outbox AS o
		SET status = 'processing', attempts = o.attempts + 1,
			lease_until = NOW() + ($3 * INTERVAL '1 second'), leased_by = $1,
			updated_at = NOW()
		FROM candidates AS c
		WHERE o.id = c.id
		RETURNING o.id, o.attempt_id, o.api_key_id, o.request_fingerprint, o.command,
			o.status, o.attempts, o.apply_result, o.available_at, o.lease_until, o.leased_by,
			o.last_error, o.created_at, o.updated_at
	`, workerID, limit, leaseSeconds)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	records := make([]service.BillingOutboxRecord, 0, limit)
	for rows.Next() {
		record, err := scanBillingOutboxRecord(rows)
		if err != nil {
			return nil, err
		}
		record.Command.Normalize()
		record.LeasedBy = workerID
		record.Command.Billing.APIKeyID = record.Command.APIKeyID
		records = append(records, *record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

func scanBillingOutboxRecord(scanner interface{ Scan(...any) error }) (*service.BillingOutboxRecord, error) {
	record := &service.BillingOutboxRecord{}
	var payload, applyResult []byte
	var leaseUntil sql.NullTime
	var leasedBy, lastError sql.NullString
	var attemptID, requestFingerprint string
	var apiKeyID int64
	if err := scanner.Scan(
		&record.ID, &attemptID, &apiKeyID, &requestFingerprint,
		&payload, &record.Status, &record.Attempts, &applyResult, &record.AvailableAt, &leaseUntil, &leasedBy, &lastError,
		&record.CreatedAt, &record.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(payload, &record.Command); err != nil {
		return nil, err
	}
	if len(applyResult) > 0 && string(applyResult) != "null" {
		record.ApplyResult = &service.UsageBillingApplyResult{}
		if err := json.Unmarshal(applyResult, record.ApplyResult); err != nil {
			return nil, err
		}
	}
	// The dedicated identity columns are authoritative. Reapply them after
	// decoding so stale or incomplete legacy payloads cannot alter replay scope.
	record.Command.AttemptID = attemptID
	record.Command.APIKeyID = apiKeyID
	record.Command.RequestFingerprint = requestFingerprint
	record.Command.Normalize()
	if leaseUntil.Valid {
		record.LeaseUntil = &leaseUntil.Time
	}
	record.LeasedBy = leasedBy.String
	record.LastError = lastError.String
	return record, nil
}

func (r *billingOutboxRepository) ClaimFinalization(ctx context.Context, workerID string, limit int, lease time.Duration) ([]service.BillingOutboxRecord, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("billing outbox database is nil")
	}
	workerID = strings.TrimSpace(workerID)
	if workerID == "" {
		return nil, errors.New("billing outbox worker id is required")
	}
	if limit <= 0 {
		limit = 100
	}
	leaseSeconds := int64(lease / time.Second)
	if leaseSeconds < 1 {
		leaseSeconds = 30
	}
	rows, err := r.db.QueryContext(ctx, `
		WITH candidates AS (
			SELECT id
			FROM billing_attempt_outbox
			WHERE (status = 'finalization_pending' AND available_at <= NOW())
			   OR (status = 'finalizing' AND lease_until <= NOW())
			ORDER BY id ASC
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		UPDATE billing_attempt_outbox AS o
		SET status = 'finalizing', attempts = o.attempts + 1,
			lease_until = NOW() + ($3 * INTERVAL '1 second'), leased_by = $1,
			updated_at = NOW()
		FROM candidates AS c
		WHERE o.id = c.id
		RETURNING o.id, o.attempt_id, o.api_key_id, o.request_fingerprint, o.command,
			o.status, o.attempts, o.apply_result, o.available_at, o.lease_until, o.leased_by,
			o.last_error, o.created_at, o.updated_at
	`, workerID, limit, leaseSeconds)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	records := make([]service.BillingOutboxRecord, 0, limit)
	for rows.Next() {
		record, err := scanBillingOutboxRecord(rows)
		if err != nil {
			return nil, err
		}
		record.LeasedBy = workerID
		records = append(records, *record)
	}
	return records, rows.Err()
}

func (r *billingOutboxRepository) RetryFinalization(ctx context.Context, id int64, workerID string, availableAt time.Time, lastError string, terminal bool) error {
	lastError = boundedBillingOutboxError(lastError)
	status := "finalization_pending"
	if terminal {
		status = "terminal"
	}
	var (
		result sql.Result
		err    error
	)
	if terminal {
		result, err = r.db.ExecContext(ctx, `
			UPDATE billing_attempt_outbox
			SET status = $4, last_error = $3, lease_until = NULL, leased_by = NULL, updated_at = NOW()
			WHERE id = $1 AND leased_by = $2 AND status = 'finalizing'
		`, id, workerID, lastError, status)
	} else {
		result, err = r.db.ExecContext(ctx, `
			UPDATE billing_attempt_outbox
			SET status = $5, available_at = $3, last_error = $4,
				lease_until = NULL, leased_by = NULL, updated_at = NOW()
			WHERE id = $1 AND leased_by = $2 AND status = 'finalizing'
		`, id, workerID, availableAt, lastError, status)
	}
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return fmt.Errorf("%w: %d", service.ErrBillingOutboxClaimLost, id)
	}
	return nil
}

func (r *billingOutboxRepository) AckFinalization(ctx context.Context, id int64, workerID string) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE billing_attempt_outbox
		SET status = 'succeeded', lease_until = NULL, leased_by = NULL, last_error = NULL, updated_at = NOW()
		WHERE id = $1 AND leased_by = $2 AND status = 'finalizing'
	`, id, workerID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return fmt.Errorf("%w: %d", service.ErrBillingOutboxClaimLost, id)
	}
	return nil
}

func (r *billingOutboxRepository) Retry(ctx context.Context, id int64, workerID string, availableAt time.Time, lastError string, terminal bool) error {
	lastError = boundedBillingOutboxError(lastError)
	status := "pending"
	if terminal {
		status = "terminal"
	}
	var (
		result sql.Result
		err    error
	)
	if terminal {
		result, err = r.db.ExecContext(ctx, `
			UPDATE billing_attempt_outbox
			SET status = $4, last_error = $3, lease_until = NULL, leased_by = NULL, updated_at = NOW()
			WHERE id = $1 AND leased_by = $2 AND status = 'processing'
		`, id, workerID, lastError, status)
	} else {
		result, err = r.db.ExecContext(ctx, `
			UPDATE billing_attempt_outbox
			SET status = $5, attempts = attempts, available_at = $3, last_error = $4,
				lease_until = NULL, leased_by = NULL, updated_at = NOW()
			WHERE id = $1 AND leased_by = $2 AND status = 'processing'
		`, id, workerID, availableAt, lastError, status)
	}
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return fmt.Errorf("%w: %d", service.ErrBillingOutboxClaimLost, id)
	}
	return nil
}

func (r *billingOutboxRepository) Ack(ctx context.Context, id int64, workerID string) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE billing_attempt_outbox
		SET status = 'succeeded', lease_until = NULL, leased_by = NULL, last_error = NULL, updated_at = NOW()
		WHERE id = $1 AND leased_by = $2 AND status = 'processing'
	`, id, workerID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return fmt.Errorf("%w: %d", service.ErrBillingOutboxClaimLost, id)
	}
	return nil
}

func (r *billingOutboxRepository) Stats(ctx context.Context) (service.BillingOutboxStats, error) {
	var stats service.BillingOutboxStats
	var oldest sql.NullTime
	var lastError sql.NullString
	err := r.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FILTER (WHERE status IN ('pending', 'finalization_pending')),
			COUNT(*) FILTER (WHERE status IN ('processing', 'finalizing')),
			COUNT(*) FILTER (WHERE status = 'terminal'), COALESCE(MAX(attempts), 0),
			MIN(created_at) FILTER (WHERE status IN ('pending', 'processing', 'finalization_pending', 'finalizing', 'terminal')),
			(SELECT last_error FROM billing_attempt_outbox WHERE last_error IS NOT NULL ORDER BY updated_at DESC, id DESC LIMIT 1)
		FROM billing_attempt_outbox
	`).Scan(&stats.Pending, &stats.Processing, &stats.Terminal, &stats.MaxAttempts, &oldest, &lastError)
	if err != nil {
		return stats, err
	}
	if oldest.Valid {
		stats.OldestCreatedAt = &oldest.Time
	}
	if lastError.Valid {
		stats.LastError = lastError.String
	}
	return stats, nil
}

func boundedBillingOutboxError(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > service.BillingOutboxLastErrorLimit {
		return value[:service.BillingOutboxLastErrorLimit]
	}
	return value
}
