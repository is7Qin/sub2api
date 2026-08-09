package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func testBillingOutboxCommand() *service.BillingOutboxCommand {
	return &service.BillingOutboxCommand{
		AttemptID: "attempt-1", RequestID: "request-1", APIKeyID: 9,
		RequestFingerprint: "fingerprint-1",
		Billing:            service.UsageBillingCommand{RequestID: "request-1", APIKeyID: 9, AccountID: 2, RequestFingerprint: "fingerprint-1", BalanceCost: 0.25},
	}
}

func TestBillingOutboxRepository_EnqueuePersistsImmutableJSONEnvelope(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	command := testBillingOutboxCommand()
	expected, err := json.Marshal(command)
	require.NoError(t, err)
	mock.ExpectQuery("(?s)INSERT INTO billing_attempt_outbox.*ON CONFLICT.*RETURNING").
		WithArgs("attempt-1", int64(9), "fingerprint-1", expected).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status", "attempts", "available_at", "lease_until", "leased_by", "last_error", "created_at", "updated_at"}).
			AddRow(int64(3), "pending", 0, time.Now(), nil, nil, nil, time.Now(), time.Now()))
	repo := NewBillingOutboxRepository(db)
	record, err := repo.Enqueue(context.Background(), command)
	require.NoError(t, err)
	require.Equal(t, int64(3), record.ID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBillingOutboxRepository_ClaimUsesLeaseAndSkipLocked(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	payload, err := json.Marshal(testBillingOutboxCommand())
	require.NoError(t, err)
	mock.ExpectQuery("(?s)FOR UPDATE SKIP LOCKED.*RETURNING").
		WithArgs("worker-a", int64(100), int64(30)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "attempt_id", "api_key_id", "request_fingerprint", "command", "status", "attempts", "apply_result", "available_at", "lease_until", "leased_by", "last_error", "created_at", "updated_at"}).
			AddRow(int64(4), "attempt-1", int64(9), "fingerprint-1", payload, "processing", 1, nil, time.Now(), time.Now().Add(time.Minute), "worker-a", nil, time.Now(), time.Now()))
	repo := NewBillingOutboxRepository(db)
	rows, err := repo.Claim(context.Background(), "worker-a", 100, 30*time.Second)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "attempt-1", rows[0].Command.AttemptID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBillingOutboxRepository_EnqueueUsesColumnIdentityOverStoredPayload(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	command := testBillingOutboxCommand()
	payloadCommand := testBillingOutboxCommand()
	payloadCommand.AttemptID = "payload-attempt"
	payloadCommand.APIKeyID = 77
	payloadCommand.RequestFingerprint = "payload-fingerprint"
	payload, err := json.Marshal(payloadCommand)
	require.NoError(t, err)
	mock.ExpectQuery("(?s)INSERT INTO billing_attempt_outbox.*ON CONFLICT.*RETURNING").
		WithArgs("attempt-1", int64(9), "fingerprint-1", sqlmock.AnyArg()).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("(?s)SELECT id, attempt_id, api_key_id, request_fingerprint, command.*billing_attempt_outbox").
		WithArgs("attempt-1", int64(9)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "attempt_id", "api_key_id", "request_fingerprint", "command", "status", "attempts", "apply_result", "available_at", "lease_until", "leased_by", "last_error", "created_at", "updated_at"}).
			AddRow(int64(4), "attempt-1", int64(9), "fingerprint-1", payload, "pending", 0, nil, time.Now(), nil, nil, nil, time.Now(), time.Now()))

	repo := NewBillingOutboxRepository(db)
	record, err := repo.Enqueue(context.Background(), command)
	require.NoError(t, err)
	require.Equal(t, "attempt-1", record.Command.AttemptID)
	require.Equal(t, int64(9), record.Command.APIKeyID)
	require.Equal(t, "fingerprint-1", record.Command.RequestFingerprint)
	require.Equal(t, int64(9), record.Command.Billing.APIKeyID)
	require.Equal(t, "fingerprint-1", record.Command.Billing.RequestFingerprint)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBillingOutboxRepository_ClaimUsesColumnIdentityOverPayload(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	payloadCommand := testBillingOutboxCommand()
	payloadCommand.AttemptID = "payload-attempt"
	payloadCommand.APIKeyID = 77
	payloadCommand.RequestFingerprint = "payload-fingerprint"
	payload, err := json.Marshal(payloadCommand)
	require.NoError(t, err)
	mock.ExpectQuery("(?s)FOR UPDATE SKIP LOCKED.*RETURNING").
		WithArgs("worker-a", int64(100), int64(30)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "attempt_id", "api_key_id", "request_fingerprint", "command", "status", "attempts", "apply_result", "available_at", "lease_until", "leased_by", "last_error", "created_at", "updated_at"}).
			AddRow(int64(5), "column-attempt", int64(9), "column-fingerprint", payload, "processing", 1, nil, time.Now(), time.Now().Add(time.Minute), "worker-a", nil, time.Now(), time.Now()))

	repo := NewBillingOutboxRepository(db)
	rows, err := repo.Claim(context.Background(), "worker-a", 100, 30*time.Second)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "column-attempt", rows[0].Command.AttemptID)
	require.Equal(t, int64(9), rows[0].Command.APIKeyID)
	require.Equal(t, "column-fingerprint", rows[0].Command.RequestFingerprint)
	require.Equal(t, "request-1", rows[0].Command.Billing.RequestID)
	require.Equal(t, int64(9), rows[0].Command.Billing.APIKeyID)
	require.Equal(t, "column-fingerprint", rows[0].Command.Billing.RequestFingerprint)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBillingOutboxRepository_ClaimUsesSinglePendingBranchMatchingPartialIndex(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	payload, err := json.Marshal(testBillingOutboxCommand())
	require.NoError(t, err)
	// Claim 必须是单分支（status = 'pending'）且 ORDER BY (available_at, id)，
	// 与部分索引 idx_billing_attempt_outbox_claim (status, available_at, id) 的
	// 前导列一致；旧的 OR 双分支 + ORDER BY id 无法匹配索引，planner 回退
	// pkey 全表扫描（生产积压时每轮过滤数百万 succeeded 行）。
	mock.ExpectQuery(`(?s)WHERE status = 'pending' AND available_at <= NOW\(\).*ORDER BY available_at, id.*FOR UPDATE SKIP LOCKED.*RETURNING`).
		WithArgs("worker-a", int64(100), int64(30)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "attempt_id", "api_key_id", "request_fingerprint", "command", "status", "attempts", "apply_result", "available_at", "lease_until", "leased_by", "last_error", "created_at", "updated_at"}).
			AddRow(int64(4), "attempt-1", int64(9), "fingerprint-1", payload, "processing", 1, nil, time.Now(), time.Now().Add(time.Minute), "worker-a", nil, time.Now(), time.Now()))
	repo := NewBillingOutboxRepository(db)
	rows, err := repo.Claim(context.Background(), "worker-a", 100, 30*time.Second)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "attempt-1", rows[0].Command.AttemptID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBillingOutboxRepository_ClaimExpiredLeasedReclaimsExpiredProcessingLeases(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	payload, err := json.Marshal(testBillingOutboxCommand())
	require.NoError(t, err)
	// 过期租约的 processing 记录走独立语句（不能与 pending 分支 UNION/OR，
	// 否则再次破坏部分索引匹配），补领时替换 leased_by 为当前 worker。
	mock.ExpectQuery(`(?s)WHERE status = 'processing' AND lease_until <= NOW\(\).*ORDER BY available_at, id.*FOR UPDATE SKIP LOCKED.*RETURNING`).
		WithArgs("worker-b", int64(100), int64(30)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "attempt_id", "api_key_id", "request_fingerprint", "command", "status", "attempts", "apply_result", "available_at", "lease_until", "leased_by", "last_error", "created_at", "updated_at"}).
			AddRow(int64(5), "attempt-1", int64(9), "fingerprint-1", payload, "processing", 2, nil, time.Now(), time.Now().Add(time.Minute), "worker-b", nil, time.Now(), time.Now()))

	repo := NewBillingOutboxRepository(db)
	rows, err := repo.ClaimExpiredLeased(context.Background(), "worker-b", 100, 30*time.Second)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "worker-b", rows[0].LeasedBy)
	require.Equal(t, int64(9), rows[0].Command.APIKeyID)
	require.Equal(t, int64(9), rows[0].Command.Billing.APIKeyID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBillingOutboxRepository_ClaimFinalizationUsesSinglePendingBranchMatchingPartialIndex(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	payload, err := json.Marshal(testBillingOutboxCommand())
	require.NoError(t, err)
	applyResult, err := json.Marshal(&service.UsageBillingApplyResult{Applied: true})
	require.NoError(t, err)
	// 与 Claim 同构：单分支 + ORDER BY (available_at, id)，命中
	// idx_billing_attempt_outbox_finalize_claim 部分索引。
	mock.ExpectQuery(`(?s)WHERE status = 'finalization_pending' AND available_at <= NOW\(\).*ORDER BY available_at, id.*FOR UPDATE SKIP LOCKED.*RETURNING`).
		WithArgs("worker-a", int64(100), int64(30)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "attempt_id", "api_key_id", "request_fingerprint", "command", "status", "attempts", "apply_result", "available_at", "lease_until", "leased_by", "last_error", "created_at", "updated_at"}).
			AddRow(int64(6), "attempt-1", int64(9), "fingerprint-1", payload, "finalizing", 2, applyResult, time.Now(), time.Now().Add(time.Minute), "worker-a", nil, time.Now(), time.Now()))

	repo := NewBillingOutboxRepository(db)
	finalizer, ok := repo.(service.BillingOutboxFinalizationRepository)
	require.True(t, ok)
	records, err := finalizer.ClaimFinalization(context.Background(), "worker-a", 100, 30*time.Second)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.NotNil(t, records[0].ApplyResult)
	require.True(t, records[0].ApplyResult.Applied)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBillingOutboxRepository_ClaimFinalizationExpiredLeasedReclaimsExpiredFinalizingLeases(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	payload, err := json.Marshal(testBillingOutboxCommand())
	require.NoError(t, err)
	applyResult, err := json.Marshal(&service.UsageBillingApplyResult{Applied: true})
	require.NoError(t, err)
	mock.ExpectQuery(`(?s)WHERE status = 'finalizing' AND lease_until <= NOW\(\).*ORDER BY available_at, id.*FOR UPDATE SKIP LOCKED.*RETURNING`).
		WithArgs("worker-b", int64(100), int64(30)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "attempt_id", "api_key_id", "request_fingerprint", "command", "status", "attempts", "apply_result", "available_at", "lease_until", "leased_by", "last_error", "created_at", "updated_at"}).
			AddRow(int64(7), "attempt-1", int64(9), "fingerprint-1", payload, "finalizing", 3, applyResult, time.Now(), time.Now().Add(time.Minute), "worker-b", nil, time.Now(), time.Now()))

	repo := NewBillingOutboxRepository(db)
	finalizer, ok := repo.(service.BillingOutboxFinalizationRepository)
	require.True(t, ok)
	records, err := finalizer.ClaimFinalizationExpiredLeased(context.Background(), "worker-b", 100, 30*time.Second)
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, "worker-b", records[0].LeasedBy)
	require.True(t, records[0].ApplyResult.Applied)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBillingOutboxRepository_RenewFinalizationLeaseFencesExpiredOrReassignedClaims(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectExec(`(?s)UPDATE billing_attempt_outbox.*lease_until = NOW\(\) \+ \(\$3 \* INTERVAL '1 second'\).*status = 'finalizing'.*leased_by = \$2.*lease_until > NOW\(\)`).
		WithArgs(int64(6), "worker-a", int64(30)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	repo := NewBillingOutboxRepository(db)
	renewer, ok := repo.(interface {
		RenewFinalizationLease(context.Context, int64, string, time.Duration) error
	})
	require.True(t, ok, "billing outbox repository must expose fenced finalization lease renewal")
	require.NoError(t, renewer.RenewFinalizationLease(context.Background(), 6, "worker-a", 30*time.Second))

	mock.ExpectExec(`(?s)UPDATE billing_attempt_outbox.*status = 'finalizing'.*leased_by = \$2.*lease_until > NOW\(\)`).
		WithArgs(int64(6), "worker-b", int64(30)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	err = renewer.RenewFinalizationLease(context.Background(), 6, "worker-b", 30*time.Second)
	require.ErrorIs(t, err, service.ErrBillingOutboxClaimLost)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBillingOutboxRepository_FinalizationTransitionsRequireUnexpiredOwnership(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	repo := NewBillingOutboxRepository(db).(service.BillingOutboxFinalizationRepository)
	next := time.Now().Add(time.Minute)

	mock.ExpectExec(`(?s)UPDATE billing_attempt_outbox.*status = \$5.*leased_by = \$2.*status = 'finalizing'.*lease_until > NOW\(\)`).
		WithArgs(int64(1), "worker", next, "retry", "finalization_pending").
		WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, repo.RetryFinalization(context.Background(), 1, "worker", next, "retry", false))

	mock.ExpectExec(`(?s)UPDATE billing_attempt_outbox.*status = \$4.*leased_by = \$2.*status = 'finalizing'.*lease_until > NOW\(\)`).
		WithArgs(int64(2), "worker", "terminal", "terminal").
		WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, repo.RetryFinalization(context.Background(), 2, "worker", next, "terminal", true))

	mock.ExpectExec(`(?s)UPDATE billing_attempt_outbox.*status = 'succeeded'.*leased_by = \$2.*status = 'finalizing'.*lease_until > NOW\(\)`).
		WithArgs(int64(2), "worker").
		WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, repo.AckFinalization(context.Background(), 2, "worker"))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBillingOutboxRepository_RetryAckAndTerminalRetainOwnership(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	repo := NewBillingOutboxRepository(db)
	next := time.Now().Add(time.Minute)
	mock.ExpectExec("UPDATE billing_attempt_outbox").WithArgs(int64(1), "worker", next, "db unavailable", "pending").WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, repo.Retry(context.Background(), 1, "worker", next, "db unavailable", false))
	mock.ExpectExec("UPDATE billing_attempt_outbox").WithArgs(int64(2), "worker", "terminal error", "terminal").WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, repo.Retry(context.Background(), 2, "worker", next, "terminal error", true))
	mock.ExpectExec("UPDATE billing_attempt_outbox").WithArgs(int64(3), "worker").WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, repo.Ack(context.Background(), 3, "worker"))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBillingOutboxRepository_StatsIncludeFinalizationStates(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	oldest := time.Now().Add(-time.Minute)
	mock.ExpectQuery(`(?s)SELECT COUNT\(\*\).*finalization_pending.*finalizing.*billing_attempt_outbox`).WillReturnRows(
		sqlmock.NewRows([]string{"pending", "processing", "terminal", "max_attempts", "oldest", "last_error"}).AddRow(3, 2, 1, 4, oldest, "provider failed"))
	repo := NewBillingOutboxRepository(db)
	stats, err := repo.Stats(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(3), stats.Pending)
	require.Equal(t, int64(2), stats.Processing)
	require.Equal(t, int64(1), stats.Terminal)
}

func TestBillingOutboxHealthOldestLagExcludesTerminalRetention(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	oldestActionable := time.Now().Add(-time.Minute)
	mock.ExpectQuery(`(?s)MIN\(created_at\) FILTER \(WHERE status IN \('pending', 'processing', 'finalization_pending', 'finalizing'\)\).*billing_attempt_outbox`).WillReturnRows(
		sqlmock.NewRows([]string{"pending", "processing", "terminal", "max_attempts", "oldest", "last_error"}).AddRow(2, 1, 1, 4, oldestActionable, "provider failed"))
	repo := NewBillingOutboxRepository(db)
	stats, err := repo.Stats(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(2), stats.Pending)
	require.Equal(t, int64(1), stats.Terminal)
	require.Equal(t, oldestActionable, *stats.OldestCreatedAt)
	require.Equal(t, "provider failed", stats.LastError)
}

func TestBillingOutboxMigrationDefinesDurableContract(t *testing.T) {
	content, err := migrations.FS.ReadFile("171_billing_attempt_outbox.sql")
	require.NoError(t, err)
	sqlText := string(content)
	for _, required := range []string{"command JSONB NOT NULL", "attempt_id", "api_key_id", "request_fingerprint", "UNIQUE (attempt_id, api_key_id)", "lease_until", "last_error", "terminal", "finalization_pending", "finalizing", "FOR UPDATE SKIP LOCKED"} {
		require.Contains(t, sqlText, required)
	}
	require.Contains(t, sqlText, "idx_billing_attempt_outbox_finalize_claim")
}
