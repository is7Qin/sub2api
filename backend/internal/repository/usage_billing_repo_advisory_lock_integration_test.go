//go:build integration

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// insertProcessingOutboxRow 直接插入一条 status='processing'、带有效租约的
// 出站记录，供 apply staging 路径使用。
func insertProcessingOutboxRow(t *testing.T, attemptID string, apiKeyID int64, leasedBy string) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		INSERT INTO billing_attempt_outbox
			(attempt_id, api_key_id, request_fingerprint, command, status, leased_by, lease_until, attempts)
		VALUES ($1, $2, $3, '{}'::jsonb, 'processing', $4, NOW() + INTERVAL '1 hour', 1)
		RETURNING id
	`, attemptID, apiKeyID, uuid.NewString(), leasedBy).Scan(&id))
	return id
}

// waitForBillingAdvisoryLock 轮询 pg_locks 直到出现（或消失条件外的）指定
// advisory 锁：granted=true 表示某会话持有，granted=false 表示某会话正在
// 等待。真实 PG 状态是唯一的同步原语；轮询只做有界重试，不做时序假设。
func waitForBillingAdvisoryLock(t *testing.T, ctx context.Context, db *sql.DB, shard int64, granted bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT count(*) FROM pg_locks
			WHERE locktype = 'advisory' AND classid = $1 AND objid = $2 AND granted = $3
		`, int64(billingAdvisoryLockClass), shard, granted).Scan(&n))
		if n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("advisory lock (class 0x%08X, shard %d, granted=%v) not observed within 10s",
		uint32(billingAdvisoryLockClass), shard, granted)
}

// TestUsageBillingRepositoryAdvisoryLock_SerializesSameUserAcrossSessions 是
// 跨实例串行穿透测试：两个独立会话（模拟两个 worker 实例）同时 apply 同一
// 用户，DB 层 advisory xact lock 必须把第二条事务钉在锁上，直到第一条提交。
// 会话 A 走批量路径（含一次 savepoint 回滚，验证锁不受 savepoint 影响），
// 会话 B 走单条路径（验证两条路径互斥、锁语义一致）。
func TestUsageBillingRepositoryAdvisoryLock_SerializesSameUserAcrossSessions(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	stagedRepo, ok := NewUsageBillingRepository(client, integrationDB).(service.UsageBillingFinalizationRepository)
	require.True(t, ok, "usage billing repo must implement the staged outbox interface")
	batchRepo, ok := NewUsageBillingRepository(client, integrationDB).(service.UsageBillingBatchFinalizationRepository)
	require.True(t, ok, "usage billing repo must implement the batch outbox interface")

	user := mustCreateUser(t, client, &service.User{
		Email:        fmt.Sprintf("advisory-lock-user-%d@example.com", time.Now().UnixNano()),
		PasswordHash: "hash",
		Balance:      100,
	})
	apiKey := mustCreateApiKey(t, client, &service.APIKey{
		UserID: user.ID,
		Key:    "sk-advisory-lock-" + uuid.NewString(),
		Name:   "advisory-lock",
	})
	account := mustCreateAccount(t, client, &service.Account{
		Name: "advisory-lock-account-" + uuid.NewString(),
		Type: service.AccountTypeAPIKey,
	})

	shard := int64(uint64(user.ID) % billingApplyUserShardCount)

	// 出站记录：A 的第一条租约归属他人（staging 阶段失败 → savepoint 回滚），
	// A 的第二条与 B 各自正常 staging。
	outboxA1 := insertProcessingOutboxRow(t, "al-a1-"+uuid.NewString(), apiKey.ID, "worker-other")
	outboxA2 := insertProcessingOutboxRow(t, "al-a2-"+uuid.NewString(), apiKey.ID, "worker-a")
	outboxB := insertProcessingOutboxRow(t, "al-b-"+uuid.NewString(), apiKey.ID, "worker-b")

	// 会话 C（独立连接）对 users 行加行锁：把 A 的批量事务钉在余额扣减上。
	// A 的事务先取 advisory 锁、再碰行，因此"持 advisory 锁且卡在行锁"的
	// 状态把 A 的持锁窗口拉长到测试可控，给 B 留出确定的阻塞观察窗口。
	rowLockConn, err := integrationDB.Conn(ctx)
	require.NoError(t, err)
	defer rowLockConn.Close()
	rowLockTx, err := rowLockConn.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() {
		if rowLockTx != nil {
			_ = rowLockTx.Rollback()
		}
	}()
	var lockedBalance float64
	require.NoError(t, rowLockTx.QueryRowContext(ctx,
		"SELECT balance FROM users WHERE id = $1 FOR UPDATE", user.ID).Scan(&lockedBalance))

	cmdA1 := &service.UsageBillingCommand{
		RequestID: "al-a1-" + uuid.NewString(), APIKeyID: apiKey.ID, UserID: user.ID, AccountID: account.ID,
	}
	cmdA2 := &service.UsageBillingCommand{
		RequestID: "al-a2-" + uuid.NewString(), APIKeyID: apiKey.ID, UserID: user.ID, AccountID: account.ID,
		BalanceCost: 2.5,
	}
	cmdB := &service.UsageBillingCommand{
		RequestID: "al-b-" + uuid.NewString(), APIKeyID: apiKey.ID, UserID: user.ID, AccountID: account.ID,
		BalanceCost: 3.75,
	}

	// 会话 A：批量路径。第一条在 staging 失败并 savepoint 回滚（锁必须仍
	// 持有），第二条扣费被会话 C 的行锁钉住。
	type batchResult struct {
		outcomes []service.UsageBillingBatchOutcome
		err      error
	}
	batchDone := make(chan batchResult, 1)
	go func() {
		outcomes, err := batchRepo.ApplyBatchAndStageOutboxFinalizations(ctx, []service.UsageBillingBatchItem{
			{Command: *cmdA1, Binding: service.UsageBillingOutboxBinding{OutboxID: outboxA1, WorkerID: "worker-a"}},
			{Command: *cmdA2, Binding: service.UsageBillingOutboxBinding{OutboxID: outboxA2, WorkerID: "worker-a"}},
		})
		batchDone <- batchResult{outcomes: outcomes, err: err}
	}()

	// 观察者：等 A 的事务实际持有 advisory 锁（A 已被行锁钉住，锁必已获取）。
	waitForBillingAdvisoryLock(t, ctx, integrationDB, shard, true)

	// 会话 B：单条路径，同用户。跨实例串行生效时 B 阻塞在 advisory 锁上
	// （而非行锁上）：pg_locks 出现 waiting 的 advisory 锁即证明。
	type singleResult struct {
		result *service.UsageBillingApplyResult
		err    error
	}
	singleDone := make(chan singleResult, 1)
	go func() {
		result, err := stagedRepo.ApplyAndStageOutboxFinalization(ctx, cmdB, service.UsageBillingOutboxBinding{OutboxID: outboxB, WorkerID: "worker-b"})
		singleDone <- singleResult{result: result, err: err}
	}()
	waitForBillingAdvisoryLock(t, ctx, integrationDB, shard, false)

	select {
	case <-singleDone:
		t.Fatal("session B completed while session A held the advisory lock: same-user applies are not serialized across sessions")
	case <-time.After(300 * time.Millisecond):
	}
	select {
	case <-batchDone:
		t.Fatal("session A completed while its users row lock was still held")
	case <-time.After(100 * time.Millisecond):
	}

	// 释放行锁：A 的扣减继续、提交并释放 advisory 锁；B 随后取锁完成。
	require.NoError(t, rowLockTx.Rollback())
	rowLockTx = nil

	aRes := <-batchDone
	require.NoError(t, aRes.err)
	require.Len(t, aRes.outcomes, 2)
	require.ErrorIs(t, aRes.outcomes[0].Err, service.ErrBillingOutboxClaimLost,
		"item 1 must fail at staging and roll back to its savepoint while the advisory lock stays held")
	require.Nil(t, aRes.outcomes[0].Result)
	require.NoError(t, aRes.outcomes[1].Err)
	require.True(t, aRes.outcomes[1].Result.Applied)

	bRes := <-singleDone
	require.NoError(t, bRes.err)
	require.NotNil(t, bRes.result)
	require.True(t, bRes.result.Applied)

	// 两笔扣费都落库：串行化没有丢失更新。
	var balance float64
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT balance FROM users WHERE id = $1", user.ID).Scan(&balance))
	require.InDelta(t, 100-2.5-3.75, balance, 0.000001)
}

// TestUsageBillingRepositoryAdvisoryLock_SkipsLockForNonPositiveUser 验证
// userID<=0 的指令免锁：另一会话持有任意分片的 advisory 锁时，该指令必须
// 立即完成（实现误取锁时会阻塞直至超时）。
func TestUsageBillingRepositoryAdvisoryLock_SkipsLockForNonPositiveUser(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	stagedRepo, ok := NewUsageBillingRepository(client, integrationDB).(service.UsageBillingFinalizationRepository)
	require.True(t, ok, "usage billing repo must implement the staged outbox interface")

	user := mustCreateUser(t, client, &service.User{
		Email:        fmt.Sprintf("advisory-no-user-%d@example.com", time.Now().UnixNano()),
		PasswordHash: "hash",
		Balance:      100,
	})
	apiKey := mustCreateApiKey(t, client, &service.APIKey{
		UserID: user.ID,
		Key:    "sk-advisory-no-user-" + uuid.NewString(),
		Name:   "advisory-no-user",
	})
	account := mustCreateAccount(t, client, &service.Account{
		Name: "advisory-no-user-account-" + uuid.NewString(),
		Type: service.AccountTypeAPIKey,
	})
	outboxID := insertProcessingOutboxRow(t, "al-nu-"+uuid.NewString(), apiKey.ID, "worker-nu")

	// 会话 C 手动持有 (billing class, shard 5) 的 advisory xact 锁。
	lockConn, err := integrationDB.Conn(ctx)
	require.NoError(t, err)
	defer lockConn.Close()
	lockTx, err := lockConn.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() {
		if lockTx != nil {
			_ = lockTx.Rollback()
		}
	}()
	_, err = lockTx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1, $2)", int64(billingAdvisoryLockClass), int64(5))
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		_, err := stagedRepo.ApplyAndStageOutboxFinalization(ctx, &service.UsageBillingCommand{
			RequestID: "al-nu-" + uuid.NewString(), APIKeyID: apiKey.ID, AccountID: account.ID, // UserID 0
		}, service.UsageBillingOutboxBinding{OutboxID: outboxID, WorkerID: "worker-nu"})
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err, "userID<=0 apply must complete without the advisory lock")
	case <-time.After(5 * time.Second):
		t.Fatal("userID<=0 apply blocked on an advisory lock held by another session")
	}

	require.NoError(t, lockTx.Rollback())
	lockTx = nil
}
