// Package repository 实现数据访问层（Repository Pattern）。
//
// 该包提供了与数据库交互的所有操作，包括 CRUD、复杂查询和批量操作。
// 采用 Repository 模式将数据访问逻辑与业务逻辑分离，便于测试和维护。
//
// 主要特性：
//   - 使用 Ent ORM 进行类型安全的数据库操作
//   - 对于复杂查询（如批量更新、聚合统计）使用原生 SQL
//   - 提供统一的错误翻译机制，将数据库错误转换为业务错误
//   - 支持软删除，所有查询自动过滤已删除记录
package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	dbaccountgroup "github.com/Wei-Shaw/sub2api/ent/accountgroup"
	dbgroup "github.com/Wei-Shaw/sub2api/ent/group"
	dbpredicate "github.com/Wei-Shaw/sub2api/ent/predicate"
	dbproxy "github.com/Wei-Shaw/sub2api/ent/proxy"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"

	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"
)

// accountRepository 实现 service.AccountRepository 接口。
// 提供 AI API 账户的完整数据访问功能。
//
// 设计说明：
//   - client: Ent 客户端，用于类型安全的 ORM 操作
//   - sql: 原生 SQL 执行器，用于复杂查询和批量操作
//   - schedulerCache: 调度器缓存，用于在账号状态变更时同步快照
type accountRepository struct {
	client *dbent.Client // Ent ORM 客户端
	sql    sqlExecutor   // 原生 SQL 执行接口
	// schedulerCache 用于在账号状态变更时主动同步快照到缓存，
	// 确保粘性会话能及时感知账号不可用状态。
	// Used to proactively sync account snapshot to cache when status changes,
	// ensuring sticky sessions can promptly detect unavailable accounts.
	schedulerCache service.SchedulerCache
}

// Keep this allowlist exact: unknown Extra keys must remain lifecycle-relevant,
// even when they share a prefix with an observational usage producer.
var schedulerNeutralExtraKeys = map[string]struct{}{
	service.OpenAICodexFingerprintExtraKey: {},
	"codex_usage_updated_at":               {},
	"model_rate_limits":                    {},
	"session_window_utilization":           {},
	"codex_primary_used_percent":           {},
	"codex_primary_reset_after_seconds":    {},
	"codex_primary_window_minutes":         {},
	"codex_primary_over_secondary_percent": {},
	"codex_secondary_used_percent":         {},
	"codex_secondary_reset_after_seconds":  {},
	"codex_secondary_window_minutes":       {},
	"codex_5h_used_percent":                {},
	"codex_5h_reset_after_seconds":         {},
	"codex_5h_window_minutes":              {},
	"codex_5h_reset_at":                    {},
	"codex_7d_used_percent":                {},
	"codex_7d_reset_after_seconds":         {},
	"codex_7d_window_minutes":              {},
	"codex_7d_reset_at":                    {},
	"passive_usage_7d_utilization":         {},
	"passive_usage_7d_reset":               {},
	"passive_usage_7d_oi_utilization":      {},
	"passive_usage_7d_oi_reset":            {},
	"passive_usage_sampled_at":             {},
}

const (
	postgresParameterBatchSize         = 50000
	schedulerSnapshotPostCommitTimeout = 5 * time.Second
)

// NewAccountRepository 创建账户仓储实例。
// 这是对外暴露的构造函数，返回接口类型以便于依赖注入。
func NewAccountRepository(client *dbent.Client, sqlDB *sql.DB, schedulerCache service.SchedulerCache) service.AccountRepository {
	return newAccountRepositoryWithSQL(client, sqlDB, schedulerCache)
}

// newAccountRepositoryWithSQL 是内部构造函数，支持依赖注入 SQL 执行器。
// 这种设计便于单元测试时注入 mock 对象。
func newAccountRepositoryWithSQL(client *dbent.Client, sqlq sqlExecutor, schedulerCache service.SchedulerCache) *accountRepository {
	return &accountRepository{client: client, sql: sqlq, schedulerCache: schedulerCache}
}

func (r *accountRepository) sqlFromContext(ctx context.Context) sqlExecutor {
	if tx := dbent.TxFromContext(ctx); tx != nil {
		return tx.Client()
	}
	return r.sql
}

func (r *accountRepository) Create(ctx context.Context, account *service.Account) error {
	if account == nil {
		return service.ErrAccountNilInput
	}

	client := clientFromContext(ctx, r.client)
	builder := client.Account.Create().
		SetName(account.Name).
		SetNillableNotes(account.Notes).
		SetPlatform(account.Platform).
		SetType(account.Type).
		SetCredentials(normalizeJSONMap(account.Credentials)).
		SetExtra(normalizeJSONMap(account.Extra)).
		SetConcurrency(account.Concurrency).
		SetPriority(account.Priority).
		SetStatus(account.Status).
		SetErrorMessage(account.ErrorMessage).
		SetSchedulable(account.Schedulable).
		SetAutoPauseOnExpired(account.AutoPauseOnExpired)

	if account.RateMultiplier != nil {
		builder.SetRateMultiplier(*account.RateMultiplier)
	}
	if account.LoadFactor != nil {
		builder.SetLoadFactor(*account.LoadFactor)
	}

	if account.ProxyID != nil {
		builder.SetProxyID(*account.ProxyID)
	}
	if account.LastUsedAt != nil {
		builder.SetLastUsedAt(*account.LastUsedAt)
	}
	if account.ExpiresAt != nil {
		builder.SetExpiresAt(*account.ExpiresAt)
	}
	if account.RateLimitedAt != nil {
		builder.SetRateLimitedAt(*account.RateLimitedAt)
	}
	if account.RateLimitResetAt != nil {
		builder.SetRateLimitResetAt(*account.RateLimitResetAt)
	}
	if account.OverloadUntil != nil {
		builder.SetOverloadUntil(*account.OverloadUntil)
	}
	if account.SessionWindowStart != nil {
		builder.SetSessionWindowStart(*account.SessionWindowStart)
	}
	if account.SessionWindowEnd != nil {
		builder.SetSessionWindowEnd(*account.SessionWindowEnd)
	}
	if account.SessionWindowStatus != "" {
		builder.SetSessionWindowStatus(account.SessionWindowStatus)
	}

	created, err := builder.Save(ctx)
	if err != nil {
		return translatePersistenceError(err, service.ErrAccountNotFound, nil)
	}

	account.ID = created.ID
	account.CreatedAt = created.CreatedAt
	account.UpdatedAt = created.UpdatedAt
	if err := enqueueSchedulerOutbox(ctx, r.sqlFromContext(ctx), service.SchedulerOutboxEventAccountChanged, &account.ID, nil, buildSchedulerGroupPayload(account.GroupIDs)); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue account create failed: account=%d err=%v", account.ID, err)
	}
	return nil
}

func (r *accountRepository) GetByID(ctx context.Context, id int64) (*service.Account, error) {
	m, err := r.client.Account.Query().Where(dbaccount.IDEQ(id)).Only(ctx)
	if err != nil {
		return nil, translatePersistenceError(err, service.ErrAccountNotFound, nil)
	}

	accounts, err := r.accountsToService(ctx, []*dbent.Account{m})
	if err != nil {
		return nil, err
	}
	if len(accounts) == 0 {
		return nil, service.ErrAccountNotFound
	}
	return &accounts[0], nil
}

func (r *accountRepository) GetByIDs(ctx context.Context, ids []int64) ([]*service.Account, error) {
	if len(ids) == 0 {
		return []*service.Account{}, nil
	}

	// De-duplicate while preserving order of first occurrence.
	uniqueIDs := make([]int64, 0, len(ids))
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		uniqueIDs = append(uniqueIDs, id)
	}
	if len(uniqueIDs) == 0 {
		return []*service.Account{}, nil
	}

	entAccounts, err := r.client.Account.
		Query().
		Where(dbaccount.IDIn(uniqueIDs...)).
		WithProxy().
		All(ctx)
	if err != nil {
		return nil, err
	}
	if len(entAccounts) == 0 {
		return []*service.Account{}, nil
	}

	accountIDs := make([]int64, 0, len(entAccounts))
	entByID := make(map[int64]*dbent.Account, len(entAccounts))
	for _, acc := range entAccounts {
		entByID[acc.ID] = acc
		accountIDs = append(accountIDs, acc.ID)
	}

	groupsByAccount, groupIDsByAccount, accountGroupsByAccount, err := r.loadAccountGroups(ctx, accountIDs)
	if err != nil {
		return nil, err
	}

	outByID := make(map[int64]*service.Account, len(entAccounts))
	for _, entAcc := range entAccounts {
		out := accountEntityToService(entAcc)
		if out == nil {
			continue
		}

		// Prefer the preloaded proxy edge when available.
		if entAcc.Edges.Proxy != nil {
			out.Proxy = proxyEntityToService(entAcc.Edges.Proxy)
		}

		if groups, ok := groupsByAccount[entAcc.ID]; ok {
			out.Groups = groups
		}
		if groupIDs, ok := groupIDsByAccount[entAcc.ID]; ok {
			out.GroupIDs = groupIDs
		}
		if ags, ok := accountGroupsByAccount[entAcc.ID]; ok {
			out.AccountGroups = ags
		}
		outByID[entAcc.ID] = out
	}

	// Preserve input order (first occurrence), and ignore missing IDs.
	out := make([]*service.Account, 0, len(uniqueIDs))
	for _, id := range uniqueIDs {
		if _, ok := entByID[id]; !ok {
			continue
		}
		if acc, ok := outByID[id]; ok && acc != nil {
			out = append(out, acc)
		}
	}

	return out, nil
}

// ExistsByID 检查指定 ID 的账号是否存在。
// 相比 GetByID，此方法性能更优，因为：
//   - 使用 Exist() 方法生成 SELECT EXISTS 查询，只返回布尔值
//   - 不加载完整的账号实体及其关联数据（Groups、Proxy 等）
//   - 适用于删除前的存在性检查等只需判断有无的场景
func (r *accountRepository) ExistsByID(ctx context.Context, id int64) (bool, error) {
	exists, err := r.client.Account.Query().Where(dbaccount.IDEQ(id)).Exist(ctx)
	if err != nil {
		return false, err
	}
	return exists, nil
}

func (r *accountRepository) GetByCRSAccountID(ctx context.Context, crsAccountID string) (*service.Account, error) {
	if crsAccountID == "" {
		return nil, nil
	}

	// 使用 sqljson.ValueEQ 生成 JSON 路径过滤，避免手写 SQL 片段导致语法兼容问题。
	m, err := r.client.Account.Query().
		Where(func(s *entsql.Selector) {
			s.Where(sqljson.ValueEQ(dbaccount.FieldExtra, crsAccountID, sqljson.Path("crs_account_id")))
		}).
		Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}

	accounts, err := r.accountsToService(ctx, []*dbent.Account{m})
	if err != nil {
		return nil, err
	}
	if len(accounts) == 0 {
		return nil, nil
	}
	return &accounts[0], nil
}

func (r *accountRepository) ListCRSAccountIDs(ctx context.Context) (map[string]int64, error) {
	rows, err := r.sql.QueryContext(ctx, `
		SELECT id, extra->>'crs_account_id'
		FROM accounts
		WHERE deleted_at IS NULL
			AND extra->>'crs_account_id' IS NOT NULL
			AND extra->>'crs_account_id' != ''
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	result := make(map[string]int64)
	for rows.Next() {
		var id int64
		var crsID string
		if err := rows.Scan(&id, &crsID); err != nil {
			return nil, err
		}
		result[crsID] = id
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (r *accountRepository) Update(ctx context.Context, account *service.Account) error {
	if account == nil {
		return nil
	}

	if err := r.updateAccountPreservingRuntime(ctx, account); err != nil {
		return err
	}

	if err := enqueueSchedulerOutbox(ctx, r.sqlFromContext(ctx), service.SchedulerOutboxEventAccountChanged, &account.ID, nil, buildSchedulerGroupPayload(account.GroupIDs)); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue account update failed: account=%d err=%v", account.ID, err)
	}
	// 普通账号编辑（如 model_mapping / credentials）也需要及时刷新单账号快照，
	// 否则网关在 outbox worker 延迟或异常时仍可能读到旧配置。
	// Transactional callers must publish only after their update becomes visible.
	r.syncSchedulerAccountSnapshotAfterCommit(ctx, account.ID)
	return nil
}

func (r *accountRepository) updateAccountPreservingRuntime(ctx context.Context, account *service.Account) error {
	if tx := dbent.TxFromContext(ctx); tx != nil {
		return r.updateAccountPreservingRuntimeWithClient(ctx, tx.Client(), account)
	}

	tx, err := r.client.Tx(ctx)
	if err != nil {
		if errors.Is(err, dbent.ErrTxStarted) {
			return r.updateAccountPreservingRuntimeWithClient(ctx, r.client, account)
		}
		return err
	}
	defer func() { _ = tx.Rollback() }()

	txCtx := dbent.NewTxContext(ctx, tx)
	if err := r.updateAccountPreservingRuntimeWithClient(txCtx, tx.Client(), account); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *accountRepository) updateAccountPreservingRuntimeWithClient(ctx context.Context, client *dbent.Client, account *service.Account) error {
	if err := r.mergeCurrentRuntimeForFullUpdate(ctx, client, account); err != nil {
		return err
	}
	return r.updateAccountRow(ctx, client, account)
}

func (r *accountRepository) mergeCurrentRuntimeForFullUpdate(ctx context.Context, client *dbent.Client, account *service.Account) error {
	rows, err := client.QueryContext(ctx, `
SELECT COALESCE(extra, '{}'::jsonb)::text,
	rate_limited_at,
	rate_limit_reset_at,
	overload_until,
	last_used_at,
	session_window_start,
	session_window_end,
	session_window_status
FROM accounts
WHERE id = $1 AND deleted_at IS NULL
FOR UPDATE`, account.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return service.ErrAccountNotFound
	}

	var rawExtra string
	var rateLimitedAt, rateLimitResetAt, overloadUntil sql.NullTime
	var lastUsedAt, sessionWindowStart, sessionWindowEnd sql.NullTime
	var sessionWindowStatus sql.NullString
	if err := rows.Scan(
		&rawExtra,
		&rateLimitedAt,
		&rateLimitResetAt,
		&overloadUntil,
		&lastUsedAt,
		&sessionWindowStart,
		&sessionWindowEnd,
		&sessionWindowStatus,
	); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}

	var currentExtra map[string]any
	if err := json.Unmarshal([]byte(rawExtra), &currentExtra); err != nil {
		return err
	}
	if account.Extra == nil {
		account.Extra = map[string]any{}
	}
	for key := range schedulerNeutralExtraKeys {
		if value, ok := currentExtra[key]; ok {
			account.Extra[key] = value
		} else {
			delete(account.Extra, key)
		}
	}

	account.RateLimitedAt = nullTimePointer(rateLimitedAt)
	account.RateLimitResetAt = nullTimePointer(rateLimitResetAt)
	account.OverloadUntil = nullTimePointer(overloadUntil)
	account.LastUsedAt = nullTimePointer(lastUsedAt)
	account.SessionWindowStart = nullTimePointer(sessionWindowStart)
	account.SessionWindowEnd = nullTimePointer(sessionWindowEnd)
	if sessionWindowStatus.Valid {
		account.SessionWindowStatus = sessionWindowStatus.String
	} else {
		account.SessionWindowStatus = ""
	}
	return nil
}

func nullTimePointer(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	return &value.Time
}

func (r *accountRepository) updateAccountRow(ctx context.Context, client *dbent.Client, account *service.Account) error {
	schedulable := account.Schedulable
	if account.Status == service.StatusError {
		schedulable = false
	}

	builder := client.Account.UpdateOneID(account.ID).
		SetName(account.Name).
		SetNillableNotes(account.Notes).
		SetPlatform(account.Platform).
		SetType(account.Type).
		SetCredentials(normalizeJSONMap(account.Credentials)).
		SetExtra(normalizeJSONMap(account.Extra)).
		SetConcurrency(account.Concurrency).
		SetPriority(account.Priority).
		SetStatus(account.Status).
		SetErrorMessage(account.ErrorMessage).
		SetSchedulable(schedulable).
		SetAutoPauseOnExpired(account.AutoPauseOnExpired)

	if account.RateMultiplier != nil {
		builder.SetRateMultiplier(*account.RateMultiplier)
	}
	if account.LoadFactor != nil {
		builder.SetLoadFactor(*account.LoadFactor)
	} else {
		builder.ClearLoadFactor()
	}

	if account.ProxyID != nil {
		builder.SetProxyID(*account.ProxyID)
	} else {
		builder.ClearProxyID()
	}
	if account.LastUsedAt != nil {
		builder.SetLastUsedAt(*account.LastUsedAt)
	} else {
		builder.ClearLastUsedAt()
	}
	if account.ExpiresAt != nil {
		builder.SetExpiresAt(*account.ExpiresAt)
	} else {
		builder.ClearExpiresAt()
	}
	if account.RateLimitedAt != nil {
		builder.SetRateLimitedAt(*account.RateLimitedAt)
	} else {
		builder.ClearRateLimitedAt()
	}
	if account.RateLimitResetAt != nil {
		builder.SetRateLimitResetAt(*account.RateLimitResetAt)
	} else {
		builder.ClearRateLimitResetAt()
	}
	if account.OverloadUntil != nil {
		builder.SetOverloadUntil(*account.OverloadUntil)
	} else {
		builder.ClearOverloadUntil()
	}
	if account.SessionWindowStart != nil {
		builder.SetSessionWindowStart(*account.SessionWindowStart)
	} else {
		builder.ClearSessionWindowStart()
	}
	if account.SessionWindowEnd != nil {
		builder.SetSessionWindowEnd(*account.SessionWindowEnd)
	} else {
		builder.ClearSessionWindowEnd()
	}
	if account.SessionWindowStatus != "" {
		builder.SetSessionWindowStatus(account.SessionWindowStatus)
	} else {
		builder.ClearSessionWindowStatus()
	}
	if account.Notes == nil {
		builder.ClearNotes()
	}

	updated, err := builder.Save(ctx)
	if err != nil {
		return translatePersistenceError(err, service.ErrAccountNotFound, nil)
	}
	account.UpdatedAt = updated.UpdatedAt
	return nil
}

func (r *accountRepository) UpdateCredentials(ctx context.Context, id int64, credentials map[string]any) error {
	client := clientFromContext(ctx, r.client)
	_, err := client.Account.UpdateOneID(id).
		SetCredentials(normalizeJSONMap(credentials)).
		Save(ctx)
	if err != nil {
		return translatePersistenceError(err, service.ErrAccountNotFound, nil)
	}
	r.syncSchedulerAccountSnapshotAfterCommit(ctx, id)
	return nil
}

func (r *accountRepository) UpdateAuthAndMergeExtra(ctx context.Context, id int64, accountType string, credentials, extraUpdates map[string]any, extraDeleteKeys []string) error {
	credentialsPayload, err := json.Marshal(normalizeJSONMap(credentials))
	if err != nil {
		return err
	}
	extraPayload, err := json.Marshal(normalizeJSONMap(extraUpdates))
	if err != nil {
		return err
	}

	extraExpr := "COALESCE(extra, '{}'::jsonb)"
	args := []any{accountType, string(credentialsPayload)}
	idx := 3
	// Transitional cleanup: re-auth JSONB merge cannot remove old OAuth passthrough/WS keys.
	// Delete requested keys first, then merge; remove this path after legacy extras age out.
	deleteKeys, err := service.NormalizeOpenAIOAuthExtraDeleteKeys(extraDeleteKeys)
	if err != nil {
		return err
	}
	for _, key := range deleteKeys {
		extraExpr = "(" + extraExpr + " - $" + itoa(idx) + ")"
		args = append(args, key)
		idx++
	}
	extraExpr += " || $" + itoa(idx) + "::jsonb"
	args = append(args, string(extraPayload), id)

	client := clientFromContext(ctx, r.client)
	result, err := client.ExecContext(
		ctx,
		"UPDATE accounts SET type = $1, credentials = $2::jsonb, extra = "+extraExpr+", updated_at = NOW() WHERE id = $"+itoa(idx+1)+" AND deleted_at IS NULL",
		args...,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return service.ErrAccountNotFound
	}
	if err := enqueueSchedulerOutbox(ctx, r.sqlFromContext(ctx), service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue auth/extra update failed: account=%d err=%v", id, err)
	}
	r.syncSchedulerAccountSnapshotAfterCommit(ctx, id)
	return nil
}

func (r *accountRepository) Delete(ctx context.Context, id int64) error {
	if tx := dbent.TxFromContext(ctx); tx != nil {
		return r.deleteWithClient(ctx, tx.Client(), id)
	}

	// Keep account deletion, membership evidence, and cache eviction ordered by
	// the same commit boundary.
	tx, err := r.client.Tx(ctx)
	if errors.Is(err, dbent.ErrTxStarted) {
		return r.deleteWithClient(ctx, r.client, id)
	}
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	txCtx := dbent.NewTxContext(ctx, tx)
	if err := r.deleteWithClient(txCtx, tx.Client(), id); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *accountRepository) deleteWithClient(ctx context.Context, client *dbent.Client, id int64) error {
	groupIDs, err := r.loadAccountGroupIDsWithClient(ctx, client, id)
	if err != nil {
		return err
	}
	if _, err := client.AccountGroup.Delete().Where(dbaccountgroup.AccountIDEQ(id)).Exec(ctx); err != nil {
		return err
	}
	if _, err := client.ExecContext(ctx, "DELETE FROM scheduled_test_plans WHERE account_id = $1", id); err != nil {
		return err
	}
	if _, err := client.Account.Delete().Where(dbaccount.IDEQ(id)).Exec(ctx); err != nil {
		return err
	}
	if err := enqueueSchedulerOutbox(ctx, r.sqlFromContext(ctx), service.SchedulerOutboxEventAccountChanged, &id, nil, buildSchedulerGroupPayload(groupIDs)); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue account delete failed: account=%d err=%v", id, err)
	}
	r.deleteSchedulerAccountSnapshotAfterCommit(ctx, id)
	return nil
}

func (r *accountRepository) List(ctx context.Context, params pagination.PaginationParams) ([]service.Account, *pagination.PaginationResult, error) {
	return r.ListWithFilters(ctx, params, service.AccountListFilters{})
}

// buildListWithFiltersQuery 构建账号列表的过滤查询（WHERE 部分）。
// 供全量版（ListWithFilters）、投影版（ListWithFiltersProjected）与
// 导出全量版（ListWithFiltersFull）共用。
func (r *accountRepository) buildListWithFiltersQuery(filters service.AccountListFilters) *dbent.AccountQuery {
	q := r.client.Account.Query()

	if filters.Platform != "" {
		q = q.Where(dbaccount.PlatformEQ(filters.Platform))
	}
	if filters.AccountType != "" {
		q = q.Where(dbaccount.TypeEQ(filters.AccountType))
	}
	if filters.Status != "" {
		switch filters.Status {
		case service.StatusActive:
			q = q.Where(
				dbaccount.StatusEQ(filters.Status),
				dbaccount.SchedulableEQ(true),
				dbaccount.Or(
					dbaccount.RateLimitResetAtIsNil(),
					dbaccount.RateLimitResetAtLTE(time.Now()),
				),
				dbpredicate.Account(func(s *entsql.Selector) {
					col := s.C("temp_unschedulable_until")
					s.Where(entsql.Or(
						entsql.IsNull(col),
						entsql.LTE(col, entsql.Expr("NOW()")),
					))
				}),
			)
		case "rate_limited":
			q = q.Where(
				dbaccount.StatusEQ(service.StatusActive),
				dbaccount.RateLimitResetAtGT(time.Now()),
				dbpredicate.Account(func(s *entsql.Selector) {
					col := s.C("temp_unschedulable_until")
					s.Where(entsql.Or(
						entsql.IsNull(col),
						entsql.LTE(col, entsql.Expr("NOW()")),
					))
				}),
			)
		case "temp_unschedulable":
			q = q.Where(
				dbaccount.StatusEQ(service.StatusActive),
				dbpredicate.Account(func(s *entsql.Selector) {
					col := s.C("temp_unschedulable_until")
					s.Where(entsql.And(
						entsql.Not(entsql.IsNull(col)),
						entsql.GT(col, entsql.Expr("NOW()")),
					))
				}),
			)
		case "unschedulable":
			q = q.Where(
				dbaccount.StatusEQ(service.StatusActive),
				dbaccount.SchedulableEQ(false),
				dbaccount.Or(
					dbaccount.RateLimitResetAtIsNil(),
					dbaccount.RateLimitResetAtLTE(time.Now()),
				),
				dbpredicate.Account(func(s *entsql.Selector) {
					col := s.C("temp_unschedulable_until")
					s.Where(entsql.Or(
						entsql.IsNull(col),
						entsql.LTE(col, entsql.Expr("NOW()")),
					))
				}),
			)
		default:
			q = q.Where(dbaccount.StatusEQ(filters.Status))
		}
	}
	if filters.Search != "" {
		q = q.Where(dbaccount.NameContainsFold(filters.Search))
	}
	if filters.GroupID == service.AccountListGroupUngrouped {
		q = q.Where(dbaccount.Not(dbaccount.HasAccountGroups()))
	} else if filters.GroupID > 0 {
		q = q.Where(dbaccount.HasAccountGroupsWith(dbaccountgroup.GroupIDEQ(filters.GroupID)))
	}
	if filters.PrivacyMode != "" {
		q = q.Where(dbpredicate.Account(func(s *entsql.Selector) {
			path := sqljson.Path("privacy_mode")
			switch filters.PrivacyMode {
			case service.AccountPrivacyModeUnsetFilter:
				s.Where(entsql.Or(
					entsql.Not(sqljson.HasKey(dbaccount.FieldExtra, path)),
					sqljson.ValueEQ(dbaccount.FieldExtra, "", path),
				))
			default:
				s.Where(sqljson.ValueEQ(dbaccount.FieldExtra, filters.PrivacyMode, path))
			}
		}))
	}
	if filters.PlanType != "" {
		q = q.Where(dbpredicate.Account(func(s *entsql.Selector) {
			s.Where(sqljson.ValueEQ(dbaccount.FieldCredentials, filters.PlanType, sqljson.Path("plan_type")))
		}))
	}
	return q
}

// ListWithFilters 是通用账号列表契约：返回完整账号（含 credentials 全量
// JSONB 解码）。AccountService.List 依赖该契约。
// 需要瘦身凭据的路径必须使用 ListWithFiltersProjected（admin 列表专用），
// 不要在此方法上加投影。
func (r *accountRepository) ListWithFilters(ctx context.Context, params pagination.PaginationParams, filters service.AccountListFilters) ([]service.Account, *pagination.PaginationResult, error) {
	q := r.buildListWithFiltersQuery(filters)

	total, err := q.Clone().Count(ctx)
	if err != nil {
		return nil, nil, err
	}

	accountsQuery := q.
		Offset(params.Offset()).
		Limit(params.Limit())
	for _, order := range accountListOrder(params) {
		accountsQuery = accountsQuery.Order(order)
	}

	accounts, err := accountsQuery.All(ctx)
	if err != nil {
		return nil, nil, err
	}

	outAccounts, err := r.accountsToService(ctx, accounts)
	if err != nil {
		return nil, nil, err
	}
	return outAccounts, paginationResultFromTotal(int64(total), params), nil
}

// ListWithFiltersProjected 是 admin 账号列表专用投影版：排除 credentials 列
// （平均 3.2KB/账号，最大 27KB），列表不需要完整凭据。前端需要的少量
// credentials 字段由 ListAccountCredentialSubset 单独批量提取，避免 ent 全量
// 解码 JSONB 造成的 CPU 风暴与 800KB/页 的响应膨胀。
// 注意：通用契约 ListWithFilters 保持全量，本方法只供 admin 列表路径使用。
func (r *accountRepository) ListWithFiltersProjected(ctx context.Context, params pagination.PaginationParams, filters service.AccountListFilters) ([]service.Account, *pagination.PaginationResult, error) {
	q := r.buildListWithFiltersQuery(filters)

	total, err := q.Clone().Count(ctx)
	if err != nil {
		return nil, nil, err
	}

	accountsQuery := q.
		Select(accountListProjectionFields...).
		Offset(params.Offset()).
		Limit(params.Limit())
	for _, order := range accountListOrder(params) {
		accountsQuery = accountsQuery.Order(order)
	}

	accounts, err := accountsQuery.All(ctx)
	if err != nil {
		return nil, nil, err
	}

	outAccounts, err := r.accountsToService(ctx, accounts)
	if err != nil {
		return nil, nil, err
	}
	return outAccounts, paginationResultFromTotal(int64(total), params), nil
}

// MaxAccountUpdatedAt 返回 accounts 表最大 updated_at，作为账号列表缓存的
// 失效版本：任何账号变更都会推进该值，列表缓存据此立即失效。
// 26K 行规模下 max(updated_at) 毫秒级完成。
func (r *accountRepository) MaxAccountUpdatedAt(ctx context.Context) (*time.Time, error) {
	if r == nil || r.sql == nil {
		return nil, nil
	}
	rows, err := r.sql.QueryContext(ctx, `SELECT max(updated_at) FROM accounts WHERE deleted_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return nil, nil
	}
	var latest sql.NullTime
	if err := rows.Scan(&latest); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !latest.Valid {
		return nil, nil
	}
	v := latest.Time
	return &v, nil
}

// ListWithFiltersFull 与 ListWithFilters 相同，均返回全量 credentials
// （JSONB 全量解码），供导出等需要完整凭据（id_token/access_token 等）
// 的路径使用。admin 列表专用投影见 ListWithFiltersProjected，避免敏感
// 凭据进入列表响应。
func (r *accountRepository) ListWithFiltersFull(ctx context.Context, params pagination.PaginationParams, filters service.AccountListFilters) ([]service.Account, *pagination.PaginationResult, error) {
	// 内部委托：两版契约当前逐字节相同，实现体收敛到 ListWithFilters。
	return r.ListWithFilters(ctx, params, filters)
}

// accountListProjectionFields 是 admin 账号列表查询的字段白名单：
// 除 credentials 外的全部列。credentials 是列表响应的体积大头
// （含 token/refresh_token 等真实凭据），列表 UI 只需要其中少量字段
// （见 ListAccountCredentialSubset），不应全量加载与解码。
var accountListProjectionFields = []string{
	dbaccount.FieldID,
	dbaccount.FieldCreatedAt,
	dbaccount.FieldUpdatedAt,
	dbaccount.FieldDeletedAt,
	dbaccount.FieldName,
	dbaccount.FieldNotes,
	dbaccount.FieldPlatform,
	dbaccount.FieldType,
	dbaccount.FieldExtra,
	dbaccount.FieldProxyID,
	dbaccount.FieldConcurrency,
	dbaccount.FieldLoadFactor,
	dbaccount.FieldPriority,
	dbaccount.FieldRateMultiplier,
	dbaccount.FieldStatus,
	dbaccount.FieldErrorMessage,
	dbaccount.FieldLastUsedAt,
	dbaccount.FieldExpiresAt,
	dbaccount.FieldAutoPauseOnExpired,
	dbaccount.FieldSchedulable,
	dbaccount.FieldRateLimitedAt,
	dbaccount.FieldRateLimitResetAt,
	dbaccount.FieldOverloadUntil,
	dbaccount.FieldTempUnschedulableUntil,
	dbaccount.FieldTempUnschedulableReason,
	dbaccount.FieldSessionWindowStart,
	dbaccount.FieldSessionWindowEnd,
	dbaccount.FieldSessionWindowStatus,
}

// ListAccountCredentialSubset 批量提取列表 UI 实际消费的 credentials 子字段，
// 替代全量 JSONB 解码。统一使用 credentials->'field'（保留 JSON 类型），
// 标量与布尔字段（email/plan_type/temp_unschedulable_enabled 等）反序列化
// 后保持 string/bool 语义。返回 map[accountID]子集 map；查询失败返回错误。
// 注意：必须用原生 SQL（ent 的 Select 不接受任意 SQL 表达式列，scan 会错位）。
func (r *accountRepository) ListAccountCredentialSubset(ctx context.Context, ids []int64) (map[int64]map[string]any, error) {
	if r == nil || r.sql == nil || len(ids) == 0 {
		return map[int64]map[string]any{}, nil
	}
	uniqueIDs := make([]int64, 0, len(ids))
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		uniqueIDs = append(uniqueIDs, id)
	}
	if len(uniqueIDs) == 0 {
		return map[int64]map[string]any{}, nil
	}

	subFieldNames := accountCredentialSubsetFieldNames()
	selectList := "id"
	for _, name := range subFieldNames {
		// credentials->'field'（jsonb 运算符）对所有类型返回带类型的 JSON
		// 文本：标量（email/plan_type）带引号、对象/数组保留结构。统一在
		// 扫描后 json.Unmarshal，标量自动去引号、对象保留类型。
		selectList += ", credentials->'" + name + "'"
	}

	rows, err := r.sql.QueryContext(ctx, `
		SELECT `+selectList+`
		FROM accounts
		WHERE id = ANY($1)
	`, uniqueIDs)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make(map[int64]map[string]any, len(uniqueIDs))
	for rows.Next() {
		var id int64
		values := make([]sql.NullString, len(subFieldNames))
		dest := make([]any, 0, len(subFieldNames)+1)
		dest = append(dest, &id)
		for i := range values {
			dest = append(dest, &values[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		subset := make(map[string]any, len(subFieldNames))
		for i, name := range subFieldNames {
			if !values[i].Valid {
				continue
			}
			// 统一反序列化：标量 JSON（"email"）去引号还原为字符串，
			// 对象/数组/布尔/数字保留类型；解析失败时兜底为原样文本。
			var v any
			if err := json.Unmarshal([]byte(values[i].String), &v); err == nil {
				subset[name] = v
			} else {
				subset[name] = values[i].String
			}
		}
		out[id] = subset
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func accountCredentialSubsetFieldNames() []string {
	return []string{
		"email", "plan_type", "subscription_expires_at", "project_id",
		"antigravity_project_id", "oauth_type",
		"openai_capabilities", "model_mapping",
		"compact_model_mapping", "temp_unschedulable_rules",
		"temp_unschedulable_enabled", "model_whitelist",
		"intercept_warmup_requests", "api_key",
	}
}

// ListOpsAccountsForStats loads only the account fields consumed by the realtime
// concurrency and availability views. Associations remain bulk-loaded by
// accountsToService so group ordering and aggregation semantics stay unchanged.
func (r *accountRepository) ListOpsAccountsForStats(ctx context.Context, platformFilter string, groupIDFilter *int64) ([]service.Account, error) {
	if r == nil || r.client == nil {
		return []service.Account{}, nil
	}

	q := r.client.Account.Query()
	if platformFilter != "" {
		q = q.Where(dbaccount.PlatformEQ(platformFilter))
	}
	if groupIDFilter != nil && *groupIDFilter > 0 {
		q = q.Where(dbaccount.HasAccountGroupsWith(dbaccountgroup.GroupIDEQ(*groupIDFilter)))
	}

	accounts, err := q.
		Select(
			dbaccount.FieldID,
			dbaccount.FieldName,
			dbaccount.FieldPlatform,
			dbaccount.FieldConcurrency,
			dbaccount.FieldLoadFactor,
			dbaccount.FieldStatus,
			dbaccount.FieldErrorMessage,
			dbaccount.FieldSchedulable,
			dbaccount.FieldRateLimitResetAt,
			dbaccount.FieldOverloadUntil,
			dbaccount.FieldTempUnschedulableUntil,
		).
		Order(dbent.Asc(dbaccount.FieldID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return r.accountsToService(ctx, accounts)
}

func accountListOrder(params pagination.PaginationParams) []func(*entsql.Selector) {
	sortBy := strings.ToLower(strings.TrimSpace(params.SortBy))
	sortOrder := params.NormalizedSortOrder(pagination.SortOrderAsc)

	field := dbaccount.FieldName
	defaultOrder := true
	switch sortBy {
	case "", "name":
		field = dbaccount.FieldName
	case "id":
		field = dbaccount.FieldID
		defaultOrder = false
	case "status":
		field = dbaccount.FieldStatus
		defaultOrder = false
	case "schedulable":
		field = dbaccount.FieldSchedulable
		defaultOrder = false
	case "priority":
		field = dbaccount.FieldPriority
		defaultOrder = false
	case "rate_multiplier":
		field = dbaccount.FieldRateMultiplier
		defaultOrder = false
	case "last_used_at":
		field = dbaccount.FieldLastUsedAt
		defaultOrder = false
	case "expires_at":
		field = dbaccount.FieldExpiresAt
		defaultOrder = false
	case "created_at":
		field = dbaccount.FieldCreatedAt
		defaultOrder = false
	}

	if sortOrder == pagination.SortOrderDesc {
		return []func(*entsql.Selector){dbent.Desc(field), dbent.Desc(dbaccount.FieldID)}
	}
	if defaultOrder {
		return []func(*entsql.Selector){dbent.Asc(dbaccount.FieldName), dbent.Asc(dbaccount.FieldID)}
	}
	return []func(*entsql.Selector){dbent.Asc(field), dbent.Asc(dbaccount.FieldID)}
}

func (r *accountRepository) ListByGroup(ctx context.Context, groupID int64) ([]service.Account, error) {
	accounts, err := r.queryAccountsByGroup(ctx, groupID, accountGroupQueryOptions{
		status: service.StatusActive,
	})
	if err != nil {
		return nil, err
	}
	return accounts, nil
}

func (r *accountRepository) ListActive(ctx context.Context) ([]service.Account, error) {
	accounts, err := r.client.Account.Query().
		Where(dbaccount.StatusEQ(service.StatusActive)).
		Order(dbent.Asc(dbaccount.FieldPriority)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return r.accountsToService(ctx, accounts)
}

func (r *accountRepository) ListOAuthRefreshCandidates(ctx context.Context) ([]service.Account, error) {
	if r.sql == nil {
		return nil, errors.New("account repository SQL executor not configured")
	}
	// PostgreSQL three-valued logic: `(cond) IS NOT TRUE` keeps healthy rows whose
	// temp unschedulable fields are NULL, while still skipping retry-exhausted cooldowns.
	rows, err := r.sql.QueryContext(ctx, `
		SELECT id
		FROM accounts
		WHERE deleted_at IS NULL
			AND status = 'active'
			AND type IN ('oauth', 'setup-token')
			AND platform IN ('anthropic', 'openai', 'gemini', 'antigravity')
			AND credentials ? 'refresh_token'
			AND btrim(credentials->>'refresh_token') <> ''
			AND (
				temp_unschedulable_until > NOW()
				AND temp_unschedulable_reason LIKE 'token refresh retry exhausted:%'
			) IS NOT TRUE
		ORDER BY priority ASC, id ASC
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return []service.Account{}, nil
	}

	accounts, err := r.GetByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]service.Account, 0, len(accounts))
	for _, account := range accounts {
		if account != nil {
			out = append(out, *account)
		}
	}
	return out, nil
}

func (r *accountRepository) ListByPlatform(ctx context.Context, platform string) ([]service.Account, error) {
	accounts, err := r.client.Account.Query().
		Where(
			dbaccount.PlatformEQ(platform),
			dbaccount.StatusEQ(service.StatusActive),
		).
		Order(dbent.Asc(dbaccount.FieldPriority)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return r.accountsToService(ctx, accounts)
}

func (r *accountRepository) ListByPlatformForValidation(ctx context.Context, platform string) ([]service.Account, error) {
	accounts, err := r.client.Account.Query().
		Where(
			dbaccount.PlatformEQ(platform),
			dbaccount.DeletedAtIsNil(),
		).
		Order(dbent.Asc(dbaccount.FieldPriority)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return r.accountsToService(ctx, accounts)
}

func (r *accountRepository) UpdateLastUsed(ctx context.Context, id int64) error {
	now := time.Now()
	exec := r.sqlFromContext(ctx)
	rows, err := exec.QueryContext(ctx, `
		UPDATE accounts
		SET last_used_at = GREATEST(last_used_at, $1),
			updated_at = NOW()
		WHERE id = $2 AND deleted_at IS NULL
		RETURNING last_used_at
	`, now, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return service.ErrAccountNotFound
	}
	var stored time.Time
	if err := rows.Scan(&stored); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	payload := map[string]any{
		"last_used": map[string]int64{
			strconv.FormatInt(id, 10): stored.Unix(),
		},
	}
	if err := enqueueSchedulerOutbox(ctx, exec, service.SchedulerOutboxEventAccountLastUsed, &id, nil, payload); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue last used failed: account=%d err=%v", id, err)
	}
	return nil
}

const accountLastUsedBatchSize = 1000

func (r *accountRepository) BatchUpdateLastUsed(ctx context.Context, updates map[int64]time.Time) error {
	if len(updates) == 0 {
		return nil
	}

	ids := make([]int64, 0, len(updates))
	for id := range updates {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	for start := 0; start < len(ids); start += accountLastUsedBatchSize {
		end := start + accountLastUsedBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		if err := r.batchUpdateLastUsedChunk(ctx, ids[start:end], updates); err != nil {
			return err
		}
	}
	return nil
}

func (r *accountRepository) batchUpdateLastUsedChunk(ctx context.Context, ids []int64, updates map[int64]time.Time) error {
	exec := r.sqlFromContext(ctx)
	args := make([]any, 0, len(ids)*2+1)
	caseSQL := "UPDATE accounts SET last_used_at = GREATEST(last_used_at, CASE id"
	idx := 1
	for _, id := range ids {
		caseSQL += " WHEN $" + itoa(idx) + " THEN $" + itoa(idx+1) + "::timestamptz"
		args = append(args, id, updates[id])
		idx += 2
	}
	caseSQL += " END), updated_at = NOW() WHERE id = ANY($" + itoa(idx) + ") AND deleted_at IS NULL RETURNING id, last_used_at"
	args = append(args, ids)

	rows, err := exec.QueryContext(ctx, caseSQL, args...)
	if err != nil {
		return err
	}
	lastUsedPayload := make(map[string]int64, len(ids))
	for rows.Next() {
		var id int64
		var stored time.Time
		if err := rows.Scan(&id, &stored); err != nil {
			_ = rows.Close()
			return err
		}
		lastUsedPayload[strconv.FormatInt(id, 10)] = stored.Unix()
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(lastUsedPayload) == 0 {
		return nil
	}
	payload := map[string]any{"last_used": lastUsedPayload}
	if err := enqueueSchedulerOutbox(ctx, exec, service.SchedulerOutboxEventAccountLastUsed, nil, nil, payload); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue batch last used failed: err=%v", err)
	}
	return nil
}

func (r *accountRepository) SetError(ctx context.Context, id int64, errorMsg string) error {
	client := clientFromContext(ctx, r.client)
	_, err := client.Account.Update().
		Where(dbaccount.IDEQ(id)).
		SetStatus(service.StatusError).
		SetErrorMessage(errorMsg).
		SetSchedulable(false).
		Save(ctx)
	if err != nil {
		return err
	}
	if err := enqueueSchedulerOutbox(ctx, r.sqlFromContext(ctx), service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue set error failed: account=%d err=%v", id, err)
	}
	r.syncSchedulerAccountSnapshotAfterCommit(ctx, id)
	return nil
}

// syncSchedulerAccountSnapshot 在账号状态变更时主动同步快照到调度器缓存。
// 当账号被设置为错误、禁用、不可调度或临时不可调度时调用，
// 确保调度器和粘性会话逻辑能及时感知账号的最新状态，避免继续使用不可用账号。
//
// syncSchedulerAccountSnapshot proactively syncs account snapshot to scheduler cache
// when account status changes. Called when account is set to error, disabled,
// unschedulable, or temporarily unschedulable, ensuring scheduler and sticky session
// logic can promptly detect the latest account state and avoid using unavailable accounts.
func (r *accountRepository) syncSchedulerAccountSnapshotAfterCommit(ctx context.Context, accountID int64) {
	if r == nil || r.schedulerCache == nil || accountID <= 0 {
		return
	}
	if tx := dbent.TxFromContext(ctx); tx != nil {
		tx.OnCommit(func(next dbent.Committer) dbent.Committer {
			return dbent.CommitFunc(func(commitCtx context.Context, committedTx *dbent.Tx) error {
				if err := next.Commit(commitCtx, committedTx); err != nil {
					return err
				}
				// The base repository client can observe the update only after commit;
				// publishing earlier would cache the previous row or a rolled-back value.
				publishCtx, cancel := context.WithTimeout(
					context.WithoutCancel(commitCtx), schedulerSnapshotPostCommitTimeout,
				)
				defer cancel()
				r.syncSchedulerAccountSnapshot(publishCtx, accountID)
				return nil
			})
		})
		return
	}
	r.syncSchedulerAccountSnapshot(ctx, accountID)
}

func (r *accountRepository) syncSchedulerAccountSnapshot(ctx context.Context, accountID int64) {
	if r == nil || r.schedulerCache == nil || accountID <= 0 {
		return
	}
	account, err := r.GetByID(ctx, accountID)
	if err != nil {
		logger.LegacyPrintf("repository.account", "[Scheduler] sync account snapshot read failed: id=%d err=%v", accountID, err)
		return
	}
	if err := r.schedulerCache.SetAccount(ctx, account); err != nil {
		logger.LegacyPrintf("repository.account", "[Scheduler] sync account snapshot write failed: id=%d err=%v", accountID, err)
	}
}

func (r *accountRepository) deleteSchedulerAccountSnapshotAfterCommit(ctx context.Context, accountID int64) {
	if r == nil || r.schedulerCache == nil || accountID <= 0 {
		return
	}
	if tx := dbent.TxFromContext(ctx); tx != nil {
		tx.OnCommit(func(next dbent.Committer) dbent.Committer {
			return dbent.CommitFunc(func(commitCtx context.Context, committedTx *dbent.Tx) error {
				if err := next.Commit(commitCtx, committedTx); err != nil {
					return err
				}
				deleteCtx, cancel := context.WithTimeout(
					context.WithoutCancel(commitCtx), schedulerSnapshotPostCommitTimeout,
				)
				defer cancel()
				r.deleteSchedulerAccountSnapshot(deleteCtx, accountID)
				return nil
			})
		})
		return
	}
	r.deleteSchedulerAccountSnapshot(ctx, accountID)
}

func (r *accountRepository) deleteSchedulerAccountSnapshot(ctx context.Context, accountID int64) {
	if r == nil || r.schedulerCache == nil || accountID <= 0 {
		return
	}
	if err := r.schedulerCache.DeleteAccount(ctx, accountID); err != nil {
		logger.LegacyPrintf("repository.account", "[Scheduler] delete account snapshot failed: id=%d err=%v", accountID, err)
	}
}

func (r *accountRepository) syncSchedulerAccountSnapshotsAfterCommit(ctx context.Context, accountIDs []int64) {
	if r == nil || r.schedulerCache == nil || len(accountIDs) == 0 {
		return
	}
	if tx := dbent.TxFromContext(ctx); tx != nil {
		ids := append([]int64(nil), accountIDs...)
		tx.OnCommit(func(next dbent.Committer) dbent.Committer {
			return dbent.CommitFunc(func(commitCtx context.Context, committedTx *dbent.Tx) error {
				if err := next.Commit(commitCtx, committedTx); err != nil {
					return err
				}
				publishCtx, cancel := context.WithTimeout(
					context.WithoutCancel(commitCtx), schedulerSnapshotPostCommitTimeout,
				)
				defer cancel()
				r.syncSchedulerAccountSnapshots(publishCtx, ids)
				return nil
			})
		})
		return
	}
	r.syncSchedulerAccountSnapshots(ctx, accountIDs)
}

func (r *accountRepository) syncSchedulerAccountSnapshots(ctx context.Context, accountIDs []int64) {
	if r == nil || r.schedulerCache == nil || len(accountIDs) == 0 {
		return
	}

	uniqueIDs := make([]int64, 0, len(accountIDs))
	seen := make(map[int64]struct{}, len(accountIDs))
	for _, id := range accountIDs {
		if id <= 0 {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		uniqueIDs = append(uniqueIDs, id)
	}
	if len(uniqueIDs) == 0 {
		return
	}

	accounts, err := r.GetByIDs(ctx, uniqueIDs)
	if err != nil {
		logger.LegacyPrintf("repository.account", "[Scheduler] batch sync account snapshot read failed: count=%d err=%v", len(uniqueIDs), err)
		return
	}

	for _, account := range accounts {
		if account == nil {
			continue
		}
		if err := r.schedulerCache.SetAccount(ctx, account); err != nil {
			logger.LegacyPrintf("repository.account", "[Scheduler] batch sync account snapshot write failed: id=%d err=%v", account.ID, err)
		}
	}
}

func (r *accountRepository) ClearError(ctx context.Context, id int64) error {
	result, err := r.sqlFromContext(ctx).ExecContext(ctx, `
		UPDATE accounts
		SET status = $2,
			error_message = '',
			updated_at = NOW()
		WHERE id = $1
			AND deleted_at IS NULL
			AND (status IS DISTINCT FROM $2 OR error_message IS DISTINCT FROM '')
	`, id, service.StatusActive)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated == 0 {
		return nil
	}
	if err := enqueueSchedulerOutbox(ctx, r.sqlFromContext(ctx), service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue clear error failed: account=%d err=%v", id, err)
	}
	r.syncSchedulerAccountSnapshotAfterCommit(ctx, id)
	return nil
}

func (r *accountRepository) AddToGroup(ctx context.Context, accountID, groupID int64, priority int) error {
	client := clientFromContext(ctx, r.client)
	_, err := client.AccountGroup.Create().
		SetAccountID(accountID).
		SetGroupID(groupID).
		SetPriority(priority).
		Save(ctx)
	if err != nil {
		return err
	}
	payload := buildSchedulerGroupPayload([]int64{groupID})
	if err := enqueueSchedulerOutbox(ctx, r.sqlFromContext(ctx), service.SchedulerOutboxEventAccountGroupsChanged, &accountID, nil, payload); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue add to group failed: account=%d group=%d err=%v", accountID, groupID, err)
	}
	return nil
}

func (r *accountRepository) RemoveFromGroup(ctx context.Context, accountID, groupID int64) error {
	client := clientFromContext(ctx, r.client)
	_, err := client.AccountGroup.Delete().
		Where(
			dbaccountgroup.AccountIDEQ(accountID),
			dbaccountgroup.GroupIDEQ(groupID),
		).
		Exec(ctx)
	if err != nil {
		return err
	}
	payload := buildSchedulerGroupPayload([]int64{groupID})
	if err := enqueueSchedulerOutbox(ctx, r.sqlFromContext(ctx), service.SchedulerOutboxEventAccountGroupsChanged, &accountID, nil, payload); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue remove from group failed: account=%d group=%d err=%v", accountID, groupID, err)
	}
	return nil
}

func (r *accountRepository) GetGroups(ctx context.Context, accountID int64) ([]service.Group, error) {
	groups, err := r.client.Group.Query().
		Where(
			dbgroup.HasAccountsWith(dbaccount.IDEQ(accountID)),
		).
		All(ctx)
	if err != nil {
		return nil, err
	}

	outGroups := make([]service.Group, 0, len(groups))
	for i := range groups {
		outGroups = append(outGroups, *groupEntityToService(groups[i]))
	}
	return outGroups, nil
}

func (r *accountRepository) BindGroups(ctx context.Context, accountID int64, groupIDs []int64) error {
	if tx := dbent.TxFromContext(ctx); tx != nil {
		return r.bindGroupsWithClient(ctx, tx.Client(), accountID, groupIDs)
	}

	// Keep membership replacement and its invalidation evidence atomic.
	tx, err := r.client.Tx(ctx)
	if errors.Is(err, dbent.ErrTxStarted) {
		return r.bindGroupsWithClient(ctx, r.client, accountID, groupIDs)
	}
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	txCtx := dbent.NewTxContext(ctx, tx)
	if err := r.bindGroupsWithClient(txCtx, tx.Client(), accountID, groupIDs); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *accountRepository) bindGroupsWithClient(ctx context.Context, client *dbent.Client, accountID int64, groupIDs []int64) error {
	existingGroupIDs, err := r.loadAccountGroupIDsWithClient(ctx, client, accountID)
	if err != nil {
		return err
	}
	if _, err := client.AccountGroup.Delete().Where(dbaccountgroup.AccountIDEQ(accountID)).Exec(ctx); err != nil {
		return err
	}

	if len(groupIDs) > 0 {
		builders := make([]*dbent.AccountGroupCreate, 0, len(groupIDs))
		for i, groupID := range groupIDs {
			builders = append(builders, client.AccountGroup.Create().
				SetAccountID(accountID).
				SetGroupID(groupID).
				SetPriority(i+1),
			)
		}
		if _, err := client.AccountGroup.CreateBulk(builders...).Save(ctx); err != nil {
			return err
		}
	}

	payload := buildSchedulerGroupPayload(mergeGroupIDs(existingGroupIDs, groupIDs))
	if err := enqueueSchedulerOutbox(ctx, r.sqlFromContext(ctx), service.SchedulerOutboxEventAccountGroupsChanged, &accountID, nil, payload); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue bind groups failed: account=%d err=%v", accountID, err)
	}
	return nil
}

func (r *accountRepository) ListSchedulable(ctx context.Context) ([]service.Account, error) {
	accounts, err := r.schedulableAccountsQuery(time.Now()).All(ctx)
	if err != nil {
		return nil, err
	}
	return r.accountsToService(ctx, accounts)
}

func (r *accountRepository) ListSchedulableAccountLoads(ctx context.Context) ([]service.AccountWithConcurrency, error) {
	accounts, err := r.schedulableAccountsQuery(time.Now()).
		Select(dbaccount.FieldID, dbaccount.FieldConcurrency, dbaccount.FieldLoadFactor).
		All(ctx)
	if err != nil {
		return nil, err
	}

	loads := make([]service.AccountWithConcurrency, 0, len(accounts))
	for _, account := range accounts {
		projection := service.Account{
			ID:          account.ID,
			Concurrency: account.Concurrency,
			LoadFactor:  account.LoadFactor,
		}
		loads = append(loads, service.AccountWithConcurrency{
			ID:             projection.ID,
			MaxConcurrency: projection.EffectiveLoadFactor(),
		})
	}
	return loads, nil
}

func (r *accountRepository) schedulableAccountsQuery(now time.Time) *dbent.AccountQuery {
	return r.client.Account.Query().
		Where(
			dbaccount.StatusEQ(service.StatusActive),
			dbaccount.SchedulableEQ(true),
			tempUnschedulablePredicate(),
			notExpiredPredicate(now),
			dbaccount.Or(dbaccount.OverloadUntilIsNil(), dbaccount.OverloadUntilLTE(now)),
			dbaccount.Or(dbaccount.RateLimitResetAtIsNil(), dbaccount.RateLimitResetAtLTE(now)),
		).
		Order(dbent.Asc(dbaccount.FieldPriority))
}

func (r *accountRepository) ListSchedulableByGroupID(ctx context.Context, groupID int64) ([]service.Account, error) {
	return r.queryAccountsByGroup(ctx, groupID, accountGroupQueryOptions{
		status:      service.StatusActive,
		schedulable: true,
	})
}

func (r *accountRepository) ListSchedulableByPlatform(ctx context.Context, platform string) ([]service.Account, error) {
	now := time.Now()
	accounts, err := r.client.Account.Query().
		Where(
			dbaccount.PlatformEQ(platform),
			dbaccount.StatusEQ(service.StatusActive),
			dbaccount.SchedulableEQ(true),
			tempUnschedulablePredicate(),
			notExpiredPredicate(now),
			dbaccount.Or(dbaccount.OverloadUntilIsNil(), dbaccount.OverloadUntilLTE(now)),
			dbaccount.Or(dbaccount.RateLimitResetAtIsNil(), dbaccount.RateLimitResetAtLTE(now)),
		).
		Order(dbent.Asc(dbaccount.FieldPriority)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return r.accountsToService(ctx, accounts)
}

func (r *accountRepository) ListSchedulableByGroupIDAndPlatform(ctx context.Context, groupID int64, platform string) ([]service.Account, error) {
	// 单平台查询复用多平台逻辑，保持过滤条件与排序策略一致。
	return r.queryAccountsByGroup(ctx, groupID, accountGroupQueryOptions{
		status:      service.StatusActive,
		schedulable: true,
		platforms:   []string{platform},
	})
}

func (r *accountRepository) ListSchedulableByPlatforms(ctx context.Context, platforms []string) ([]service.Account, error) {
	if len(platforms) == 0 {
		return nil, nil
	}
	// 仅返回可调度的活跃账号，并过滤处于过载/限流窗口的账号。
	// 代理与分组信息统一在 accountsToService 中批量加载，避免 N+1 查询。
	now := time.Now()
	accounts, err := r.client.Account.Query().
		Where(
			dbaccount.PlatformIn(platforms...),
			dbaccount.StatusEQ(service.StatusActive),
			dbaccount.SchedulableEQ(true),
			tempUnschedulablePredicate(),
			notExpiredPredicate(now),
			dbaccount.Or(dbaccount.OverloadUntilIsNil(), dbaccount.OverloadUntilLTE(now)),
			dbaccount.Or(dbaccount.RateLimitResetAtIsNil(), dbaccount.RateLimitResetAtLTE(now)),
		).
		Order(dbent.Asc(dbaccount.FieldPriority)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return r.accountsToService(ctx, accounts)
}

func (r *accountRepository) ListSchedulableUngroupedByPlatform(ctx context.Context, platform string) ([]service.Account, error) {
	now := time.Now()
	accounts, err := r.client.Account.Query().
		Where(
			dbaccount.PlatformEQ(platform),
			dbaccount.StatusEQ(service.StatusActive),
			dbaccount.SchedulableEQ(true),
			dbaccount.Not(dbaccount.HasAccountGroups()),
			tempUnschedulablePredicate(),
			notExpiredPredicate(now),
			dbaccount.Or(dbaccount.OverloadUntilIsNil(), dbaccount.OverloadUntilLTE(now)),
			dbaccount.Or(dbaccount.RateLimitResetAtIsNil(), dbaccount.RateLimitResetAtLTE(now)),
		).
		Order(dbent.Asc(dbaccount.FieldPriority)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return r.accountsToService(ctx, accounts)
}

func (r *accountRepository) ListSchedulableUngroupedByPlatforms(ctx context.Context, platforms []string) ([]service.Account, error) {
	if len(platforms) == 0 {
		return nil, nil
	}
	now := time.Now()
	accounts, err := r.client.Account.Query().
		Where(
			dbaccount.PlatformIn(platforms...),
			dbaccount.StatusEQ(service.StatusActive),
			dbaccount.SchedulableEQ(true),
			dbaccount.Not(dbaccount.HasAccountGroups()),
			tempUnschedulablePredicate(),
			notExpiredPredicate(now),
			dbaccount.Or(dbaccount.OverloadUntilIsNil(), dbaccount.OverloadUntilLTE(now)),
			dbaccount.Or(dbaccount.RateLimitResetAtIsNil(), dbaccount.RateLimitResetAtLTE(now)),
		).
		Order(dbent.Asc(dbaccount.FieldPriority)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return r.accountsToService(ctx, accounts)
}

func (r *accountRepository) ListSchedulableByGroupIDAndPlatforms(ctx context.Context, groupID int64, platforms []string) ([]service.Account, error) {
	if len(platforms) == 0 {
		return nil, nil
	}
	// 复用按分组查询逻辑，保证分组优先级 + 账号优先级的排序与筛选一致。
	return r.queryAccountsByGroup(ctx, groupID, accountGroupQueryOptions{
		status:      service.StatusActive,
		schedulable: true,
		platforms:   platforms,
	})
}

// ListModelAvailabilityCandidates returns the persistently enabled pool used
// only to classify a model miss. Runtime cooldown, overload, and expiry state
// must not turn a temporary capacity shortage into a permanent 404.
func (r *accountRepository) ListModelAvailabilityCandidates(ctx context.Context, groupID *int64, platforms []string, includeGrouped bool) ([]service.Account, error) {
	if len(platforms) == 0 {
		return []service.Account{}, nil
	}
	if groupID != nil {
		return r.queryAccountsByGroup(ctx, *groupID, accountGroupQueryOptions{
			status:               service.StatusActive,
			schedulable:          true,
			ignoreTransientState: true,
			platforms:            platforms,
		})
	}

	preds := []dbpredicate.Account{
		dbaccount.StatusEQ(service.StatusActive),
		dbaccount.SchedulableEQ(true),
		dbaccount.PlatformIn(platforms...),
	}
	if !includeGrouped {
		preds = append(preds, dbaccount.Not(dbaccount.HasAccountGroups()))
	}
	accounts, err := r.client.Account.Query().
		Where(preds...).
		Order(dbent.Asc(dbaccount.FieldPriority)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return r.accountsToService(ctx, accounts)
}

func (r *accountRepository) SetRateLimited(ctx context.Context, id int64, resetAt time.Time) error {
	result, err := r.sqlFromContext(ctx).ExecContext(ctx, `
		UPDATE accounts
		SET rate_limited_at = NOW(),
			rate_limit_reset_at = $1,
			updated_at = NOW()
		WHERE id = $2
			AND deleted_at IS NULL
			AND (rate_limit_reset_at IS NULL OR rate_limit_reset_at < $1)
	`, resetAt, id)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated == 0 {
		return nil
	}
	r.syncSchedulerAccountSnapshotAfterCommit(ctx, id)
	return nil
}

func (r *accountRepository) SetModelRateLimit(ctx context.Context, id int64, scope string, resetAt time.Time, reason ...string) error {
	if scope == "" {
		return nil
	}
	now := time.Now().UTC()
	payload := map[string]string{
		"rate_limited_at":     now.Format(time.RFC3339),
		"rate_limit_reset_at": resetAt.UTC().Format(time.RFC3339),
	}
	if len(reason) > 0 {
		if value := strings.TrimSpace(reason[0]); value != "" {
			payload["reason"] = value
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	client := clientFromContext(ctx, r.client)
	rows, err := client.QueryContext(ctx, `
WITH target AS MATERIALIZED (
	SELECT id,
		COALESCE(extra, '{}'::jsonb) AS extra,
		COALESCE(extra, '{}'::jsonb) #>> ARRAY['model_rate_limits', $1, 'rate_limit_reset_at'] AS current_reset
	FROM accounts
	WHERE id = $3 AND deleted_at IS NULL
	FOR UPDATE
), updated AS (
	UPDATE accounts
	SET extra = jsonb_set(
		jsonb_set(target.extra, '{model_rate_limits}'::text[], COALESCE(target.extra->'model_rate_limits', '{}'::jsonb), true),
		ARRAY['model_rate_limits', $1]::text[],
		$2::jsonb,
		true
	),
		updated_at = NOW()
	FROM target
	WHERE accounts.id = target.id
		AND CASE
			WHEN target.current_reset ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?Z$'
				THEN target.current_reset::timestamptz < $4
			ELSE true
		END
	RETURNING 1
)
SELECT EXISTS (SELECT 1 FROM target), EXISTS (SELECT 1 FROM updated)`,
		scope,
		raw,
		id,
		resetAt,
	)
	if err != nil {
		return err
	}
	if !rows.Next() {
		rowsErr := rows.Err()
		_ = rows.Close()
		if rowsErr != nil {
			return rowsErr
		}
		return errors.New("model rate limit update returned no result")
	}
	var exists, updated bool
	if err := rows.Scan(&exists, &updated); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !exists {
		return service.ErrAccountNotFound
	}
	if !updated {
		return nil
	}
	r.syncSchedulerAccountSnapshotAfterCommit(ctx, id)
	return nil
}

func (r *accountRepository) SetOverloaded(ctx context.Context, id int64, until time.Time) error {
	result, err := r.sqlFromContext(ctx).ExecContext(ctx, `
		UPDATE accounts
		SET overload_until = $1,
			updated_at = NOW()
		WHERE id = $2
			AND deleted_at IS NULL
			AND (overload_until IS NULL OR overload_until < $1)
	`, until, id)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated == 0 {
		return nil
	}
	r.syncSchedulerAccountSnapshotAfterCommit(ctx, id)
	return nil
}

func (r *accountRepository) SetTempUnschedulable(ctx context.Context, id int64, until time.Time, reason string) error {
	result, err := r.sqlFromContext(ctx).ExecContext(ctx, `
		UPDATE accounts
		SET temp_unschedulable_until = $1,
			temp_unschedulable_reason = $2,
			updated_at = NOW()
		WHERE id = $3
			AND deleted_at IS NULL
			AND (temp_unschedulable_until IS NULL OR temp_unschedulable_until < $1)
	`, until, reason, id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected <= 0 {
		return nil
	}
	r.syncSchedulerAccountSnapshotAfterCommit(ctx, id)
	return nil
}

func (r *accountRepository) ClearTempUnschedulable(ctx context.Context, id int64) error {
	result, err := r.sqlFromContext(ctx).ExecContext(ctx, `
		UPDATE accounts
		SET temp_unschedulable_until = NULL,
			temp_unschedulable_reason = NULL,
			updated_at = NOW()
		WHERE id = $1
			AND deleted_at IS NULL
			AND (temp_unschedulable_until IS NOT NULL OR temp_unschedulable_reason IS NOT NULL)
	`, id)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated == 0 {
		return nil
	}
	r.syncSchedulerAccountSnapshotAfterCommit(ctx, id)
	return nil
}

func (r *accountRepository) ClearRateLimit(ctx context.Context, id int64) error {
	result, err := r.sqlFromContext(ctx).ExecContext(ctx, `
		UPDATE accounts
		SET rate_limited_at = NULL,
			rate_limit_reset_at = NULL,
			overload_until = NULL,
			updated_at = NOW()
		WHERE id = $1
			AND deleted_at IS NULL
			AND (
				rate_limited_at IS NOT NULL OR
				rate_limit_reset_at IS NOT NULL OR
				overload_until IS NOT NULL
			)
	`, id)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated == 0 {
		return nil
	}
	r.syncSchedulerAccountSnapshotAfterCommit(ctx, id)
	return nil
}

func (r *accountRepository) ClearAntigravityQuotaScopes(ctx context.Context, id int64) error {
	client := clientFromContext(ctx, r.client)
	rows, err := client.QueryContext(ctx, `
WITH updated AS (
	UPDATE accounts
	SET extra = COALESCE(extra, '{}'::jsonb) - 'antigravity_quota_scopes',
		updated_at = NOW()
	WHERE id = $1
		AND deleted_at IS NULL
		AND COALESCE(extra, '{}'::jsonb) ? 'antigravity_quota_scopes'
	RETURNING 1
)
SELECT EXISTS (
	SELECT 1 FROM accounts WHERE id = $1 AND deleted_at IS NULL
), EXISTS (SELECT 1 FROM updated)`, id)
	if err != nil {
		return err
	}
	if !rows.Next() {
		rowsErr := rows.Err()
		_ = rows.Close()
		if rowsErr != nil {
			return rowsErr
		}
		return errors.New("antigravity quota scope clear returned no result")
	}
	var exists, updated bool
	if err := rows.Scan(&exists, &updated); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !exists {
		return service.ErrAccountNotFound
	}
	if !updated {
		return nil
	}
	if err := enqueueSchedulerOutbox(ctx, r.sqlFromContext(ctx), service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue clear quota scopes failed: account=%d err=%v", id, err)
	}
	return nil
}

func (r *accountRepository) ClearModelRateLimit(ctx context.Context, id int64, scope string) error {
	if strings.TrimSpace(scope) == "" {
		return nil
	}
	client := clientFromContext(ctx, r.client)
	result, err := client.ExecContext(ctx, `
UPDATE accounts
SET extra = COALESCE(extra, '{}'::jsonb) #- ARRAY['model_rate_limits', $2]::text[],
	updated_at = NOW()
WHERE id = $1
	AND deleted_at IS NULL
	AND COALESCE(extra, '{}'::jsonb) #> ARRAY['model_rate_limits', $2]::text[] IS NOT NULL`, id, scope)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated == 0 {
		return nil
	}
	r.syncSchedulerAccountSnapshotAfterCommit(ctx, id)
	return nil
}

func (r *accountRepository) ClearModelRateLimits(ctx context.Context, id int64) error {
	client := clientFromContext(ctx, r.client)
	rows, err := client.QueryContext(ctx, `
WITH updated AS (
	UPDATE accounts
	SET extra = COALESCE(extra, '{}'::jsonb) - 'model_rate_limits',
		updated_at = NOW()
	WHERE id = $1
		AND deleted_at IS NULL
		AND COALESCE(extra, '{}'::jsonb) ? 'model_rate_limits'
	RETURNING 1
)
SELECT EXISTS (
	SELECT 1 FROM accounts WHERE id = $1 AND deleted_at IS NULL
), EXISTS (SELECT 1 FROM updated)`, id)
	if err != nil {
		return err
	}
	if !rows.Next() {
		rowsErr := rows.Err()
		_ = rows.Close()
		if rowsErr != nil {
			return rowsErr
		}
		return errors.New("model rate limit clear returned no result")
	}
	var exists, updated bool
	if err := rows.Scan(&exists, &updated); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !exists {
		return service.ErrAccountNotFound
	}
	if !updated {
		return nil
	}
	r.syncSchedulerAccountSnapshotAfterCommit(ctx, id)
	return nil
}

func (r *accountRepository) UpdateSessionWindow(ctx context.Context, id int64, start, end *time.Time, status string) error {
	client := clientFromContext(ctx, r.client)
	result, err := client.ExecContext(ctx, `
UPDATE accounts
SET session_window_start = CASE WHEN $2 THEN $3::timestamptz ELSE session_window_start END,
	session_window_end = CASE WHEN $4 THEN $5::timestamptz ELSE session_window_end END,
	session_window_status = $6,
	updated_at = NOW()
WHERE id = $1
	AND deleted_at IS NULL
	AND (
		NOT $4 OR
		session_window_end IS NULL OR
		session_window_end <= $5::timestamptz
	)
	AND (
		session_window_status IS DISTINCT FROM $6 OR
		($2 AND session_window_start IS DISTINCT FROM $3::timestamptz) OR
		($4 AND session_window_end IS DISTINCT FROM $5::timestamptz)
	)`, id, start != nil, start, end != nil, end, status)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated == 0 {
		return nil
	}
	// Session-window state is a runtime overlay: publish only real changes so
	// stable response headers do not cause a DB write plus Redis rewrite per call.
	r.syncSchedulerAccountSnapshotAfterCommit(ctx, id)
	return nil
}

// UpdateSessionWindowEnd 仅更新 5h 窗口结束时间，不覆盖请求路径记录的 start/status。
func (r *accountRepository) UpdateSessionWindowEnd(ctx context.Context, id int64, end time.Time) error {
	client := clientFromContext(ctx, r.client)
	rows, err := client.QueryContext(ctx, `
WITH target AS MATERIALIZED (
	SELECT id
	FROM accounts
	WHERE id = $2 AND deleted_at IS NULL
), updated AS (
	UPDATE accounts
	SET session_window_end = $1,
		updated_at = NOW()
	WHERE id IN (SELECT id FROM target)
		AND (session_window_end IS NULL OR session_window_end < $1)
	RETURNING 1
)
SELECT EXISTS (SELECT 1 FROM target), EXISTS (SELECT 1 FROM updated)`, end, id)
	if err != nil {
		return err
	}
	if !rows.Next() {
		rowsErr := rows.Err()
		_ = rows.Close()
		if rowsErr != nil {
			return rowsErr
		}
		return errors.New("session window update returned no result")
	}
	var exists, updated bool
	if err := rows.Scan(&exists, &updated); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !exists {
		return service.ErrAccountNotFound
	}
	if !updated {
		return nil
	}
	r.syncSchedulerAccountSnapshotAfterCommit(ctx, id)
	return nil
}

func (r *accountRepository) SetSchedulable(ctx context.Context, id int64, schedulable bool) error {
	client := clientFromContext(ctx, r.client)
	_, err := client.Account.Update().
		Where(dbaccount.IDEQ(id)).
		SetSchedulable(schedulable).
		Save(ctx)
	if err != nil {
		return err
	}
	if err := enqueueSchedulerOutbox(ctx, r.sqlFromContext(ctx), service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue schedulable change failed: account=%d err=%v", id, err)
	}
	if !schedulable {
		r.syncSchedulerAccountSnapshotAfterCommit(ctx, id)
	}
	return nil
}

func (r *accountRepository) AutoPauseExpiredAccounts(ctx context.Context, now time.Time) (int64, error) {
	result, err := r.sqlFromContext(ctx).ExecContext(ctx, `
		UPDATE accounts
		SET schedulable = FALSE,
			updated_at = NOW()
		WHERE deleted_at IS NULL
			AND schedulable = TRUE
			AND auto_pause_on_expired = TRUE
			AND expires_at IS NOT NULL
			AND expires_at <= $1
	`, now)
	if err != nil {
		return 0, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return rows, nil
}

func (r *accountRepository) UpdateExtra(ctx context.Context, id int64, updates map[string]any) error {
	if len(updates) == 0 {
		return nil
	}
	payload, err := json.Marshal(updates)
	if err != nil {
		return err
	}
	rows, err := clientFromContext(ctx, r.client).QueryContext(ctx, `
WITH target AS MATERIALIZED (
	SELECT id, COALESCE(extra, '{}'::jsonb) AS extra
	FROM accounts
	WHERE id = $2 AND deleted_at IS NULL
), updated AS (
	UPDATE accounts
	SET extra = target.extra || $1::jsonb,
		updated_at = NOW()
	FROM target
	WHERE accounts.id = target.id
		AND target.extra IS DISTINCT FROM target.extra || $1::jsonb
	RETURNING 1
)
SELECT EXISTS (SELECT 1 FROM target), EXISTS (SELECT 1 FROM updated)`, string(payload), id)
	updated, err := scanExtraUpdateResult(rows, err)
	if err != nil {
		return err
	}
	if updated {
		r.afterExtraUpdate(ctx, id, updates, "extra update")
	}
	return nil
}

// UpdateRuntimeExtra applies one runtime snapshot only when its observation time
// is newer than the snapshot already stored on the account.
func (r *accountRepository) UpdateRuntimeExtra(ctx context.Context, id int64, updates map[string]any, observedAtKey string, observedAt time.Time) (bool, error) {
	if len(updates) == 0 {
		return false, nil
	}
	observedAtKey = strings.TrimSpace(observedAtKey)
	if observedAtKey == "" || observedAt.IsZero() {
		return false, errors.New("runtime extra observation is required")
	}
	// Keep the persisted watermark and the comparison value identical. Trusting a
	// caller-supplied timestamp here could accept a snapshot at T2 while storing
	// T1, allowing observations between T1 and T2 to overwrite newer state.
	snapshot := make(map[string]any, len(updates)+1)
	for key, value := range updates {
		snapshot[key] = value
	}
	snapshot[observedAtKey] = observedAt.UTC().Format(time.RFC3339Nano)
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return false, err
	}
	rows, err := clientFromContext(ctx, r.client).QueryContext(ctx, `
WITH target AS MATERIALIZED (
	SELECT id, COALESCE(extra, '{}'::jsonb) AS extra
	FROM accounts
	WHERE id = $2 AND deleted_at IS NULL
	FOR UPDATE
), updated AS (
	UPDATE accounts
	SET extra = target.extra || $1::jsonb,
		updated_at = NOW()
	FROM target
	WHERE accounts.id = target.id
		AND target.extra IS DISTINCT FROM target.extra || $1::jsonb
		AND CASE
			WHEN target.extra #>> ARRAY[$3] ~
				'^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?Z$'
				THEN (target.extra #>> ARRAY[$3])::timestamptz < $4
			ELSE true
		END
	RETURNING 1
)
SELECT EXISTS (SELECT 1 FROM target), EXISTS (SELECT 1 FROM updated)`, string(payload), id, observedAtKey, observedAt)
	updated, err := scanExtraUpdateResult(rows, err)
	if err != nil {
		return false, err
	}
	if updated {
		r.afterExtraUpdate(ctx, id, updates, "runtime extra update")
	}
	return updated, nil
}

func scanExtraUpdateResult(rows *sql.Rows, queryErr error) (bool, error) {
	if queryErr != nil {
		return false, queryErr
	}
	if !rows.Next() {
		rowsErr := rows.Err()
		_ = rows.Close()
		if rowsErr != nil {
			return false, rowsErr
		}
		return false, errors.New("extra update returned no result")
	}
	var exists, updated bool
	if err := rows.Scan(&exists, &updated); err != nil {
		_ = rows.Close()
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	if !exists {
		return false, service.ErrAccountNotFound
	}
	return updated, nil
}

func (r *accountRepository) UpdateExtraNestedBool(ctx context.Context, id int64, updates map[string]any, mapKey string, nestedKey string, value bool) error {
	mapKey = strings.TrimSpace(mapKey)
	nestedKey = strings.TrimSpace(nestedKey)
	if mapKey == "" || nestedKey == "" {
		return errors.New("extra nested bool path cannot be empty")
	}

	shallowUpdates := make(map[string]any, len(updates))
	for key, update := range updates {
		if key == mapKey {
			continue
		}
		shallowUpdates[key] = update
	}
	payload, err := json.Marshal(shallowUpdates)
	if err != nil {
		return err
	}

	client := clientFromContext(ctx, r.client)
	rows, err := client.QueryContext(ctx, `
WITH target AS MATERIALIZED (
	SELECT id,
		extra,
		jsonb_set(
			jsonb_set(
				COALESCE(extra, '{}'::jsonb) || $1::jsonb,
				ARRAY[$2]::text[],
				CASE
					WHEN jsonb_typeof((COALESCE(extra, '{}'::jsonb) || $1::jsonb) -> $2) = 'object'
						THEN (COALESCE(extra, '{}'::jsonb) || $1::jsonb) -> $2
					ELSE '{}'::jsonb
				END,
				true
			),
			ARRAY[$2, $3]::text[],
			$4::jsonb,
			true
		) AS next_extra
	FROM accounts
	WHERE id = $5 AND deleted_at IS NULL
	FOR UPDATE
), updated AS (
	UPDATE accounts
	SET extra = target.next_extra,
		updated_at = NOW()
	FROM target
	WHERE accounts.id = target.id
		AND target.extra IS DISTINCT FROM target.next_extra
	RETURNING 1
)
SELECT EXISTS (SELECT 1 FROM target), EXISTS (SELECT 1 FROM updated)`,
		string(payload), mapKey, nestedKey, strconv.FormatBool(value), id,
	)
	if err != nil {
		return err
	}
	if !rows.Next() {
		rowsErr := rows.Err()
		_ = rows.Close()
		if rowsErr != nil {
			return rowsErr
		}
		return errors.New("nested extra update returned no result")
	}
	var exists, updated bool
	if err := rows.Scan(&exists, &updated); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !exists {
		return service.ErrAccountNotFound
	}
	if !updated {
		return nil
	}

	schedulerUpdates := make(map[string]any, len(updates)+1)
	for key, update := range updates {
		schedulerUpdates[key] = update
	}
	schedulerUpdates[mapKey] = value
	r.afterExtraUpdate(ctx, id, schedulerUpdates, "nested extra update")
	return nil
}

func (r *accountRepository) afterExtraUpdate(ctx context.Context, id int64, updates map[string]any, operation string) {
	if shouldEnqueueSchedulerOutboxForExtraUpdates(updates) {
		if err := enqueueSchedulerOutbox(ctx, r.sqlFromContext(ctx), service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
			logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue %s failed: account=%d err=%v", operation, id, err)
		}
	}
	if shouldSyncSchedulerSnapshotForExtraUpdates(updates) {
		// Runtime overlays need prompt publication even when the same batch also
		// carries lifecycle state whose normal refresh is handled asynchronously.
		// Read the full account after commit to avoid publishing stale or rolled-back
		// state, and to avoid a partial patch losing concurrent fields.
		r.syncSchedulerAccountSnapshotAfterCommit(ctx, id)
	}
}

func (r *accountRepository) ResetOpenAICodexFingerprint(ctx context.Context, id int64, fingerprint service.OpenAICodexFingerprint) error {
	payload, err := json.Marshal(fingerprint)
	if err != nil {
		return err
	}

	client := clientFromContext(ctx, r.client)
	rows, err := client.QueryContext(ctx, `
WITH updated AS (
	UPDATE accounts
	SET extra = jsonb_set(COALESCE(extra, '{}'::jsonb), ARRAY[$2], $3::jsonb, true),
		updated_at = NOW()
	WHERE id = $1
		AND deleted_at IS NULL
		AND platform = $4
		AND type IN ($5, $6)
		AND COALESCE(extra, '{}'::jsonb) -> $2 IS DISTINCT FROM $3::jsonb
	RETURNING 1
)
SELECT EXISTS (
	SELECT 1
	FROM accounts
	WHERE id = $1
		AND deleted_at IS NULL
		AND platform = $4
		AND type IN ($5, $6)
), EXISTS (SELECT 1 FROM updated)`,
		id,
		service.OpenAICodexFingerprintExtraKey,
		string(payload),
		service.PlatformOpenAI,
		service.AccountTypeOAuth,
		service.AccountTypeSetupToken,
	)
	if err != nil {
		return err
	}
	if !rows.Next() {
		rowsErr := rows.Err()
		_ = rows.Close()
		if rowsErr != nil {
			return rowsErr
		}
		return errors.New("codex fingerprint reset returned no result")
	}
	var exists, updated bool
	if err := rows.Scan(&exists, &updated); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !exists {
		return service.ErrAccountNotFound
	}
	if !updated {
		return nil
	}
	r.syncSchedulerAccountSnapshotAfterCommit(ctx, id)
	return nil
}

func (r *accountRepository) EnsureOpenAICodexFingerprint(ctx context.Context, id int64, fingerprint service.OpenAICodexFingerprint, expectedOld *service.OpenAICodexFingerprint) (service.OpenAICodexFingerprint, error) {
	payload, err := json.Marshal(fingerprint)
	if err != nil {
		return service.OpenAICodexFingerprint{}, err
	}

	client := clientFromContext(ctx, r.client)
	fpExpr := "COALESCE(extra, '{}'::jsonb) -> $2"
	var expectedOldPayload any
	if expectedOld != nil {
		raw, marshalErr := json.Marshal(expectedOld)
		if marshalErr != nil {
			return service.OpenAICodexFingerprint{}, marshalErr
		}
		expectedOldPayload = string(raw)
	}
	out, inserted, err := r.queryOpenAICodexFingerprint(ctx, client, `
UPDATE accounts
SET extra = jsonb_set(COALESCE(extra, '{}'::jsonb), ARRAY[$2], $3::jsonb, true), updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL AND platform = $4 AND type IN ($5, $6) AND (
	NOT (COALESCE(extra, '{}'::jsonb) ? $2)
	OR NOT (`+openAICodexFingerprintSQLValid(fpExpr)+`)
	OR ($7::jsonb IS NOT NULL AND `+fpExpr+` = $7::jsonb)
)
RETURNING extra -> $2`, id, service.OpenAICodexFingerprintExtraKey, string(payload), service.PlatformOpenAI, service.AccountTypeOAuth, service.AccountTypeSetupToken, expectedOldPayload)
	if err != nil {
		return service.OpenAICodexFingerprint{}, err
	}
	if inserted {
		r.syncSchedulerAccountSnapshotAfterCommit(ctx, id)
		return out, nil
	}

	out, found, err := r.queryOpenAICodexFingerprint(ctx, client, `
SELECT extra -> $2
FROM accounts
WHERE id = $1 AND deleted_at IS NULL AND platform = $3 AND type IN ($4, $5) AND COALESCE(extra, '{}'::jsonb) ? $2 AND `+openAICodexFingerprintSQLValid(fpExpr), id, service.OpenAICodexFingerprintExtraKey, service.PlatformOpenAI, service.AccountTypeOAuth, service.AccountTypeSetupToken)
	if err != nil {
		return service.OpenAICodexFingerprint{}, err
	}
	if !found {
		return service.OpenAICodexFingerprint{}, service.ErrAccountNotFound
	}
	return out, nil
}

func (r *accountRepository) queryOpenAICodexFingerprint(ctx context.Context, client *dbent.Client, query string, args ...any) (service.OpenAICodexFingerprint, bool, error) {
	rows, err := client.QueryContext(ctx, query, args...)
	if err != nil {
		return service.OpenAICodexFingerprint{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return service.OpenAICodexFingerprint{}, false, err
		}
		return service.OpenAICodexFingerprint{}, false, nil
	}
	var raw []byte
	if err := rows.Scan(&raw); err != nil {
		return service.OpenAICodexFingerprint{}, false, err
	}
	out, err := decodeOpenAICodexFingerprintJSON(raw)
	return out, true, err
}

func decodeOpenAICodexFingerprintJSON(raw []byte) (service.OpenAICodexFingerprint, error) {
	var out service.OpenAICodexFingerprint
	if err := json.Unmarshal(raw, &out); err != nil {
		return service.OpenAICodexFingerprint{}, err
	}
	return out, nil
}

func openAICodexFingerprintSQLValid(expr string) string {
	e := "(" + strings.TrimSpace(expr) + ")"
	conditions := "jsonb_typeof(" + e + ") = 'object'" +
		" AND " + e + " ->> 'schema_version' = '1'" +
		" AND (" + e + " ->> 'installation_id') ~* '^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'" +
		" AND jsonb_typeof(" + e + " -> 'ua_profile') = 'object'" +
		" AND COALESCE(NULLIF(BTRIM(" + e + " #>> '{ua_profile,originator}'), ''), '') <> ''" +
		" AND COALESCE(NULLIF(BTRIM(" + e + " #>> '{ua_profile,codex_version}'), ''), '') <> ''" +
		" AND COALESCE(NULLIF(BTRIM(" + e + " #>> '{ua_profile,os_fingerprint}'), ''), '') <> ''" +
		" AND COALESCE(NULLIF(BTRIM(" + e + " #>> '{ua_profile,terminal_token}'), ''), '') <> ''" +
		" AND COALESCE(NULLIF(BTRIM(" + e + " ->> 'created_at'), ''), '') <> ''" +
		" AND COALESCE(NULLIF(BTRIM(" + e + " ->> 'updated_at'), ''), '') <> ''"
	return "COALESCE((" + conditions + "), false)"
}

func shouldEnqueueSchedulerOutboxForExtraUpdates(updates map[string]any) bool {
	if len(updates) == 0 {
		return false
	}
	for key := range updates {
		if isSchedulerNeutralExtraKey(key) {
			continue
		}
		return true
	}
	return false
}

func shouldSyncSchedulerSnapshotForExtraUpdates(updates map[string]any) bool {
	for key := range updates {
		if isSchedulerNeutralExtraKey(key) {
			return true
		}
	}
	return false
}

func isSchedulerNeutralExtraKey(key string) bool {
	if key == "" {
		return false
	}
	_, ok := schedulerNeutralExtraKeys[key]
	return ok
}

func (r *accountRepository) BulkUpdate(ctx context.Context, ids []int64, updates service.AccountBulkUpdate) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}

	setClauses := make([]string, 0, 8)
	args := make([]any, 0, 8)

	idx := 1
	if updates.Name != nil {
		setClauses = append(setClauses, "name = $"+itoa(idx))
		args = append(args, *updates.Name)
		idx++
	}
	if updates.ProxyID != nil {
		// 0 表示清除代理（前端发送 0 而不是 null 来表达清除意图）
		if *updates.ProxyID == 0 {
			setClauses = append(setClauses, "proxy_id = NULL")
		} else {
			setClauses = append(setClauses, "proxy_id = $"+itoa(idx))
			args = append(args, *updates.ProxyID)
			idx++
		}
	}
	if updates.Concurrency != nil {
		setClauses = append(setClauses, "concurrency = $"+itoa(idx))
		args = append(args, *updates.Concurrency)
		idx++
	}
	if updates.Priority != nil {
		setClauses = append(setClauses, "priority = $"+itoa(idx))
		args = append(args, *updates.Priority)
		idx++
	}
	if updates.RateMultiplier != nil {
		setClauses = append(setClauses, "rate_multiplier = $"+itoa(idx))
		args = append(args, *updates.RateMultiplier)
		idx++
	}
	if updates.LoadFactor != nil {
		if *updates.LoadFactor <= 0 {
			setClauses = append(setClauses, "load_factor = NULL")
		} else {
			setClauses = append(setClauses, "load_factor = $"+itoa(idx))
			args = append(args, *updates.LoadFactor)
			idx++
		}
	}
	if updates.Status != nil {
		setClauses = append(setClauses, "status = $"+itoa(idx))
		args = append(args, *updates.Status)
		idx++
	}
	if updates.Schedulable != nil {
		setClauses = append(setClauses, "schedulable = $"+itoa(idx))
		args = append(args, *updates.Schedulable)
		idx++
	}
	// JSONB 需要合并而非覆盖，使用 raw SQL 保持旧行为。
	if len(updates.Credentials) > 0 {
		payload, err := json.Marshal(updates.Credentials)
		if err != nil {
			return 0, err
		}
		setClauses = append(setClauses, "credentials = COALESCE(credentials, '{}'::jsonb) || $"+itoa(idx)+"::jsonb")
		args = append(args, payload)
		idx++
	}
	if len(updates.Extra) > 0 || len(updates.ExtraDeleteKeys) > 0 {
		extraExpr := "COALESCE(extra, '{}'::jsonb)"
		// Transitional cleanup: bulk JSONB merge cannot remove old OAuth passthrough/WS keys.
		// Delete requested keys first, then merge; remove this path after legacy extras age out.
		deleteKeys, err := service.NormalizeOpenAIOAuthExtraDeleteKeys(updates.ExtraDeleteKeys)
		if err != nil {
			return 0, err
		}
		for _, key := range deleteKeys {
			extraExpr = "(" + extraExpr + " - $" + itoa(idx) + ")"
			args = append(args, key)
			idx++
		}
		if len(updates.Extra) > 0 {
			payload, err := json.Marshal(updates.Extra)
			if err != nil {
				return 0, err
			}
			extraExpr += " || $" + itoa(idx) + "::jsonb"
			args = append(args, payload)
			idx++
		}
		setClauses = append(setClauses, "extra = "+extraExpr)
	}

	if len(setClauses) == 0 {
		return 0, nil
	}

	setClauses = append(setClauses, "updated_at = NOW()")

	query := "UPDATE accounts SET " + joinClauses(setClauses, ", ") + " WHERE id = ANY($" + itoa(idx) + ") AND deleted_at IS NULL"
	args = append(args, ids)

	exec := r.sqlFromContext(ctx)
	result, err := exec.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if rows > 0 {
		payload := map[string]any{"account_ids": ids}
		if err := enqueueSchedulerOutbox(ctx, exec, service.SchedulerOutboxEventAccountBulkChanged, nil, nil, payload); err != nil {
			logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue bulk update failed: err=%v", err)
		}
		shouldSync := false
		if updates.Status != nil && (*updates.Status == service.StatusError || *updates.Status == service.StatusDisabled) {
			shouldSync = true
		}
		if updates.Schedulable != nil && !*updates.Schedulable {
			shouldSync = true
		}
		if shouldSync {
			r.syncSchedulerAccountSnapshotsAfterCommit(ctx, ids)
		}
	}
	return rows, nil
}

type accountGroupQueryOptions struct {
	status               string
	schedulable          bool
	ignoreTransientState bool
	platforms            []string // 允许的多个平台，空切片表示不进行平台过滤
}

func (r *accountRepository) queryAccountsByGroup(ctx context.Context, groupID int64, opts accountGroupQueryOptions) ([]service.Account, error) {
	q := r.client.AccountGroup.Query().
		Where(dbaccountgroup.GroupIDEQ(groupID))

	// 通过 account_groups 中间表查询账号，并按需叠加状态/平台/调度能力过滤。
	preds := make([]dbpredicate.Account, 0, 6)
	preds = append(preds, dbaccount.DeletedAtIsNil())
	if opts.status != "" {
		preds = append(preds, dbaccount.StatusEQ(opts.status))
	}
	if len(opts.platforms) > 0 {
		preds = append(preds, dbaccount.PlatformIn(opts.platforms...))
	}
	if opts.schedulable {
		preds = append(preds, dbaccount.SchedulableEQ(true))
		if !opts.ignoreTransientState {
			now := time.Now()
			preds = append(preds,
				tempUnschedulablePredicate(),
				notExpiredPredicate(now),
				dbaccount.Or(dbaccount.OverloadUntilIsNil(), dbaccount.OverloadUntilLTE(now)),
				dbaccount.Or(dbaccount.RateLimitResetAtIsNil(), dbaccount.RateLimitResetAtLTE(now)),
			)
		}
	}

	if len(preds) > 0 {
		q = q.Where(dbaccountgroup.HasAccountWith(preds...))
	}

	groups, err := q.
		Order(
			dbaccountgroup.ByPriority(),
			dbaccountgroup.ByAccountField(dbaccount.FieldPriority),
		).
		WithAccount().
		All(ctx)
	if err != nil {
		return nil, err
	}

	orderedIDs := make([]int64, 0, len(groups))
	accountMap := make(map[int64]*dbent.Account, len(groups))
	for _, ag := range groups {
		if ag.Edges.Account == nil {
			continue
		}
		if _, exists := accountMap[ag.AccountID]; exists {
			continue
		}
		accountMap[ag.AccountID] = ag.Edges.Account
		orderedIDs = append(orderedIDs, ag.AccountID)
	}

	accounts := make([]*dbent.Account, 0, len(orderedIDs))
	for _, id := range orderedIDs {
		if acc, ok := accountMap[id]; ok {
			accounts = append(accounts, acc)
		}
	}

	return r.accountsToService(ctx, accounts)
}

func (r *accountRepository) accountsToService(ctx context.Context, accounts []*dbent.Account) ([]service.Account, error) {
	if len(accounts) == 0 {
		return []service.Account{}, nil
	}

	accountIDs := make([]int64, 0, len(accounts))
	proxyIDs := make([]int64, 0, len(accounts))
	for _, acc := range accounts {
		accountIDs = append(accountIDs, acc.ID)
		if acc.ProxyID != nil {
			proxyIDs = append(proxyIDs, *acc.ProxyID)
		}
	}

	proxyMap, err := r.loadProxies(ctx, proxyIDs)
	if err != nil {
		return nil, err
	}
	groupsByAccount, groupIDsByAccount, accountGroupsByAccount, err := r.loadAccountGroups(ctx, accountIDs)
	if err != nil {
		return nil, err
	}

	outAccounts := make([]service.Account, 0, len(accounts))
	for _, acc := range accounts {
		out := accountEntityToService(acc)
		if out == nil {
			continue
		}
		if acc.ProxyID != nil {
			if proxy, ok := proxyMap[*acc.ProxyID]; ok {
				out.Proxy = proxy
			}
		}
		if groups, ok := groupsByAccount[acc.ID]; ok {
			out.Groups = groups
		}
		if groupIDs, ok := groupIDsByAccount[acc.ID]; ok {
			out.GroupIDs = groupIDs
		}
		if ags, ok := accountGroupsByAccount[acc.ID]; ok {
			out.AccountGroups = ags
		}
		outAccounts = append(outAccounts, *out)
	}

	return outAccounts, nil
}

func tempUnschedulablePredicate() dbpredicate.Account {
	return dbpredicate.Account(func(s *entsql.Selector) {
		col := s.C("temp_unschedulable_until")
		s.Where(entsql.Or(
			entsql.IsNull(col),
			entsql.LTE(col, entsql.Expr("NOW()")),
		))
	})
}

func notExpiredPredicate(now time.Time) dbpredicate.Account {
	return dbaccount.Or(
		dbaccount.ExpiresAtIsNil(),
		dbaccount.ExpiresAtGT(now),
		dbaccount.AutoPauseOnExpiredEQ(false),
	)
}

func (r *accountRepository) loadProxies(ctx context.Context, proxyIDs []int64) (map[int64]*service.Proxy, error) {
	proxyMap := make(map[int64]*service.Proxy)
	proxyIDs = uniquePositiveInt64s(proxyIDs)
	if len(proxyIDs) == 0 {
		return proxyMap, nil
	}

	for start := 0; start < len(proxyIDs); start += postgresParameterBatchSize {
		end := start + postgresParameterBatchSize
		if end > len(proxyIDs) {
			end = len(proxyIDs)
		}
		proxies, err := r.client.Proxy.Query().Where(dbproxy.IDIn(proxyIDs[start:end]...)).All(ctx)
		if err != nil {
			return nil, err
		}
		for _, p := range proxies {
			proxyMap[p.ID] = proxyEntityToService(p)
		}
	}
	return proxyMap, nil
}

func (r *accountRepository) loadAccountGroups(ctx context.Context, accountIDs []int64) (map[int64][]*service.Group, map[int64][]int64, map[int64][]service.AccountGroup, error) {
	groupsByAccount := make(map[int64][]*service.Group)
	groupIDsByAccount := make(map[int64][]int64)
	accountGroupsByAccount := make(map[int64][]service.AccountGroup)

	accountIDs = uniquePositiveInt64s(accountIDs)
	if len(accountIDs) == 0 {
		return groupsByAccount, groupIDsByAccount, accountGroupsByAccount, nil
	}

	for start := 0; start < len(accountIDs); start += postgresParameterBatchSize {
		end := start + postgresParameterBatchSize
		if end > len(accountIDs) {
			end = len(accountIDs)
		}
		entries, err := r.client.AccountGroup.Query().
			Where(dbaccountgroup.AccountIDIn(accountIDs[start:end]...)).
			Order(dbaccountgroup.ByAccountID(), dbaccountgroup.ByPriority()).
			All(ctx)
		if err != nil {
			return nil, nil, nil, err
		}
		groupIDs := make([]int64, 0, len(entries))
		for _, ag := range entries {
			groupIDs = append(groupIDs, ag.GroupID)
		}
		groupMap, err := r.loadGroups(ctx, groupIDs)
		if err != nil {
			return nil, nil, nil, err
		}

		for _, ag := range entries {
			groupSvc := groupMap[ag.GroupID]
			agSvc := service.AccountGroup{
				AccountID: ag.AccountID,
				GroupID:   ag.GroupID,
				Priority:  ag.Priority,
				CreatedAt: ag.CreatedAt,
				Group:     groupSvc,
			}
			accountGroupsByAccount[ag.AccountID] = append(accountGroupsByAccount[ag.AccountID], agSvc)
			groupIDsByAccount[ag.AccountID] = append(groupIDsByAccount[ag.AccountID], ag.GroupID)
			if groupSvc != nil {
				groupsByAccount[ag.AccountID] = append(groupsByAccount[ag.AccountID], groupSvc)
			}
		}
	}

	return groupsByAccount, groupIDsByAccount, accountGroupsByAccount, nil
}

func (r *accountRepository) loadGroups(ctx context.Context, groupIDs []int64) (map[int64]*service.Group, error) {
	groupMap := make(map[int64]*service.Group)
	groupIDs = uniquePositiveInt64s(groupIDs)
	if len(groupIDs) == 0 {
		return groupMap, nil
	}

	for start := 0; start < len(groupIDs); start += postgresParameterBatchSize {
		end := start + postgresParameterBatchSize
		if end > len(groupIDs) {
			end = len(groupIDs)
		}
		groups, err := r.client.Group.Query().Where(dbgroup.IDIn(groupIDs[start:end]...)).All(ctx)
		if err != nil {
			return nil, err
		}
		for _, g := range groups {
			groupMap[g.ID] = groupEntityToService(g)
		}
	}
	return groupMap, nil
}

func uniquePositiveInt64s(ids []int64) []int64 {
	if len(ids) == 0 {
		return nil
	}
	out := make([]int64, 0, len(ids))
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func (r *accountRepository) loadAccountGroupIDs(ctx context.Context, accountID int64) ([]int64, error) {
	return r.loadAccountGroupIDsWithClient(ctx, clientFromContext(ctx, r.client), accountID)
}

func (r *accountRepository) loadAccountGroupIDsWithClient(ctx context.Context, client *dbent.Client, accountID int64) ([]int64, error) {
	entries, err := client.AccountGroup.
		Query().
		Where(dbaccountgroup.AccountIDEQ(accountID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.GroupID)
	}
	return ids, nil
}

func mergeGroupIDs(a []int64, b []int64) []int64 {
	seen := make(map[int64]struct{}, len(a)+len(b))
	out := make([]int64, 0, len(a)+len(b))
	for _, id := range a {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	for _, id := range b {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// buildSchedulerGroupPayload returns untyped nil for empty groups so payload-any
// callers do not marshal a typed-nil map as JSON null and split dedup keys.
func buildSchedulerGroupPayload(groupIDs []int64) any {
	if len(groupIDs) == 0 {
		return nil
	}
	return map[string]any{"group_ids": groupIDs}
}

func accountEntityToService(m *dbent.Account) *service.Account {
	if m == nil {
		return nil
	}

	rateMultiplier := m.RateMultiplier

	return &service.Account{
		ID:                      m.ID,
		Name:                    m.Name,
		Notes:                   m.Notes,
		Platform:                m.Platform,
		Type:                    m.Type,
		Credentials:             copyJSONMap(m.Credentials),
		Extra:                   copyJSONMap(m.Extra),
		ProxyID:                 m.ProxyID,
		Concurrency:             m.Concurrency,
		Priority:                m.Priority,
		RateMultiplier:          &rateMultiplier,
		LoadFactor:              m.LoadFactor,
		Status:                  m.Status,
		ErrorMessage:            derefString(m.ErrorMessage),
		LastUsedAt:              m.LastUsedAt,
		ExpiresAt:               m.ExpiresAt,
		AutoPauseOnExpired:      m.AutoPauseOnExpired,
		CreatedAt:               m.CreatedAt,
		UpdatedAt:               m.UpdatedAt,
		Schedulable:             m.Schedulable,
		RateLimitedAt:           m.RateLimitedAt,
		RateLimitResetAt:        m.RateLimitResetAt,
		OverloadUntil:           m.OverloadUntil,
		TempUnschedulableUntil:  m.TempUnschedulableUntil,
		TempUnschedulableReason: derefString(m.TempUnschedulableReason),
		SessionWindowStart:      m.SessionWindowStart,
		SessionWindowEnd:        m.SessionWindowEnd,
		SessionWindowStatus:     derefString(m.SessionWindowStatus),
	}
}

func normalizeJSONMap(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	return in
}

func copyJSONMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func joinClauses(clauses []string, sep string) string {
	if len(clauses) == 0 {
		return ""
	}
	out := clauses[0]
	for i := 1; i < len(clauses); i++ {
		out += sep + clauses[i]
	}
	return out
}

func itoa(v int) string {
	return strconv.Itoa(v)
}

// FindByExtraField 根据 extra 字段中的键值对查找账号。
// 使用 PostgreSQL JSONB @> 操作符进行高效查询（需要 GIN 索引支持）。
//
// FindByExtraField finds accounts by key-value pairs in the extra field.
// Uses PostgreSQL JSONB @> operator for efficient queries (requires GIN index).
func (r *accountRepository) FindByExtraField(ctx context.Context, key string, value any) ([]service.Account, error) {
	accounts, err := r.client.Account.Query().
		Where(
			dbaccount.DeletedAtIsNil(),
			func(s *entsql.Selector) {
				path := sqljson.Path(key)
				switch v := value.(type) {
				case string:
					preds := []*entsql.Predicate{sqljson.ValueEQ(dbaccount.FieldExtra, v, path)}
					if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
						preds = append(preds, sqljson.ValueEQ(dbaccount.FieldExtra, parsed, path))
					}
					if len(preds) == 1 {
						s.Where(preds[0])
					} else {
						s.Where(entsql.Or(preds...))
					}
				case int:
					s.Where(entsql.Or(
						sqljson.ValueEQ(dbaccount.FieldExtra, v, path),
						sqljson.ValueEQ(dbaccount.FieldExtra, strconv.Itoa(v), path),
					))
				case int64:
					s.Where(entsql.Or(
						sqljson.ValueEQ(dbaccount.FieldExtra, v, path),
						sqljson.ValueEQ(dbaccount.FieldExtra, strconv.FormatInt(v, 10), path),
					))
				case json.Number:
					if parsed, err := v.Int64(); err == nil {
						s.Where(entsql.Or(
							sqljson.ValueEQ(dbaccount.FieldExtra, parsed, path),
							sqljson.ValueEQ(dbaccount.FieldExtra, v.String(), path),
						))
					} else {
						s.Where(sqljson.ValueEQ(dbaccount.FieldExtra, v.String(), path))
					}
				default:
					s.Where(sqljson.ValueEQ(dbaccount.FieldExtra, value, path))
				}
			},
		).
		All(ctx)
	if err != nil {
		return nil, translatePersistenceError(err, service.ErrAccountNotFound, nil)
	}

	return r.accountsToService(ctx, accounts)
}

// nowUTC is a SQL expression to generate a UTC RFC3339 timestamp string.
const nowUTC = `to_char(NOW() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')`

// dailyExpiredExpr is a SQL expression that evaluates to TRUE when daily quota period has expired.
// Supports both rolling (24h from start) and fixed (pre-computed reset_at) modes.
const dailyExpiredExpr = `(
	CASE WHEN COALESCE(extra->>'quota_daily_reset_mode', 'rolling') = 'fixed'
	THEN NOW() >= COALESCE((extra->>'quota_daily_reset_at')::timestamptz, '1970-01-01'::timestamptz)
	ELSE COALESCE((extra->>'quota_daily_start')::timestamptz, '1970-01-01'::timestamptz)
		+ '24 hours'::interval <= NOW()
	END
)`

// weeklyExpiredExpr is a SQL expression that evaluates to TRUE when weekly quota period has expired.
const weeklyExpiredExpr = `(
	CASE WHEN COALESCE(extra->>'quota_weekly_reset_mode', 'rolling') = 'fixed'
	THEN NOW() >= COALESCE((extra->>'quota_weekly_reset_at')::timestamptz, '1970-01-01'::timestamptz)
	ELSE COALESCE((extra->>'quota_weekly_start')::timestamptz, '1970-01-01'::timestamptz)
		+ '168 hours'::interval <= NOW()
	END
)`

// nextDailyResetAtExpr is a SQL expression to compute the next daily reset_at when a reset occurs.
// For fixed mode: computes the next future reset time based on NOW(), timezone, and configured hour.
// This correctly handles long-inactive accounts by jumping directly to the next valid reset point.
const nextDailyResetAtExpr = `(
	CASE WHEN COALESCE(extra->>'quota_daily_reset_mode', 'rolling') = 'fixed'
	THEN to_char((
		-- Compute today's reset point in the configured timezone, then pick next future one
		CASE WHEN NOW() >= (
			date_trunc('day', NOW() AT TIME ZONE COALESCE(extra->>'quota_reset_timezone', 'UTC'))
			+ (COALESCE((extra->>'quota_daily_reset_hour')::int, 0) || ' hours')::interval
		) AT TIME ZONE COALESCE(extra->>'quota_reset_timezone', 'UTC')
		-- NOW() is at or past today's reset point → next reset is tomorrow
		THEN (
			date_trunc('day', NOW() AT TIME ZONE COALESCE(extra->>'quota_reset_timezone', 'UTC'))
			+ (COALESCE((extra->>'quota_daily_reset_hour')::int, 0) || ' hours')::interval
			+ '1 day'::interval
		) AT TIME ZONE COALESCE(extra->>'quota_reset_timezone', 'UTC')
		-- NOW() is before today's reset point → next reset is today
		ELSE (
			date_trunc('day', NOW() AT TIME ZONE COALESCE(extra->>'quota_reset_timezone', 'UTC'))
			+ (COALESCE((extra->>'quota_daily_reset_hour')::int, 0) || ' hours')::interval
		) AT TIME ZONE COALESCE(extra->>'quota_reset_timezone', 'UTC')
		END
	) AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
	ELSE NULL END
)`

// nextWeeklyResetAtExpr is a SQL expression to compute the next weekly reset_at when a reset occurs.
// For fixed mode: computes the next future reset time based on NOW(), timezone, configured day and hour.
// This correctly handles long-inactive accounts by jumping directly to the next valid reset point.
const nextWeeklyResetAtExpr = `(
	CASE WHEN COALESCE(extra->>'quota_weekly_reset_mode', 'rolling') = 'fixed'
	THEN to_char((
		-- Compute this week's reset point in the configured timezone
		-- Step 1: get today's date at reset hour in configured tz
		-- Step 2: compute days forward to target weekday
		-- Step 3: if same day but past reset hour, advance 7 days
		CASE
		WHEN (
			-- days_forward = (target_day - current_day + 7) % 7
			(COALESCE((extra->>'quota_weekly_reset_day')::int, 1)
			 - EXTRACT(DOW FROM NOW() AT TIME ZONE COALESCE(extra->>'quota_reset_timezone', 'UTC'))::int
			 + 7) % 7
		) = 0 AND NOW() >= (
			date_trunc('day', NOW() AT TIME ZONE COALESCE(extra->>'quota_reset_timezone', 'UTC'))
			+ (COALESCE((extra->>'quota_weekly_reset_hour')::int, 0) || ' hours')::interval
		) AT TIME ZONE COALESCE(extra->>'quota_reset_timezone', 'UTC')
		-- Same weekday and past reset hour → next week
		THEN (
			date_trunc('day', NOW() AT TIME ZONE COALESCE(extra->>'quota_reset_timezone', 'UTC'))
			+ (COALESCE((extra->>'quota_weekly_reset_hour')::int, 0) || ' hours')::interval
			+ '7 days'::interval
		) AT TIME ZONE COALESCE(extra->>'quota_reset_timezone', 'UTC')
		ELSE (
			-- Advance to target weekday this week (or next if days_forward > 0)
			date_trunc('day', NOW() AT TIME ZONE COALESCE(extra->>'quota_reset_timezone', 'UTC'))
			+ (COALESCE((extra->>'quota_weekly_reset_hour')::int, 0) || ' hours')::interval
			+ ((
				(COALESCE((extra->>'quota_weekly_reset_day')::int, 1)
				 - EXTRACT(DOW FROM NOW() AT TIME ZONE COALESCE(extra->>'quota_reset_timezone', 'UTC'))::int
				 + 7) % 7
			) || ' days')::interval
		) AT TIME ZONE COALESCE(extra->>'quota_reset_timezone', 'UTC')
		END
	) AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
	ELSE NULL END
)`

// IncrementQuotaUsed 原子递增账号的配额用量（总/日/周三个维度）
// 日/周额度在周期过期时自动重置为 0 再递增。
// 支持滚动窗口（rolling）和固定时间（fixed）两种重置模式。
func (r *accountRepository) IncrementQuotaUsed(ctx context.Context, id int64, amount float64) error {
	exec := r.sqlFromContext(ctx)
	rows, err := exec.QueryContext(ctx,
		`UPDATE accounts SET extra = (
			COALESCE(extra, '{}'::jsonb)
			-- 总额度：始终递增
			|| jsonb_build_object('quota_used', COALESCE((extra->>'quota_used')::numeric, 0) + $1)
			-- 日额度：仅在 quota_daily_limit > 0 时处理
			|| CASE WHEN COALESCE((extra->>'quota_daily_limit')::numeric, 0) > 0 THEN
				jsonb_build_object(
					'quota_daily_used',
					CASE WHEN `+dailyExpiredExpr+`
					THEN $1
					ELSE COALESCE((extra->>'quota_daily_used')::numeric, 0) + $1 END,
					'quota_daily_start',
					CASE WHEN `+dailyExpiredExpr+`
					THEN `+nowUTC+`
					ELSE COALESCE(extra->>'quota_daily_start', `+nowUTC+`) END
				)
				-- 固定模式重置时更新下次重置时间
				|| CASE WHEN `+dailyExpiredExpr+` AND `+nextDailyResetAtExpr+` IS NOT NULL
				   THEN jsonb_build_object('quota_daily_reset_at', `+nextDailyResetAtExpr+`)
				   ELSE '{}'::jsonb END
			ELSE '{}'::jsonb END
			-- 周额度：仅在 quota_weekly_limit > 0 时处理
			|| CASE WHEN COALESCE((extra->>'quota_weekly_limit')::numeric, 0) > 0 THEN
				jsonb_build_object(
					'quota_weekly_used',
					CASE WHEN `+weeklyExpiredExpr+`
					THEN $1
					ELSE COALESCE((extra->>'quota_weekly_used')::numeric, 0) + $1 END,
					'quota_weekly_start',
					CASE WHEN `+weeklyExpiredExpr+`
					THEN `+nowUTC+`
					ELSE COALESCE(extra->>'quota_weekly_start', `+nowUTC+`) END
				)
				-- 固定模式重置时更新下次重置时间
				|| CASE WHEN `+weeklyExpiredExpr+` AND `+nextWeeklyResetAtExpr+` IS NOT NULL
				   THEN jsonb_build_object('quota_weekly_reset_at', `+nextWeeklyResetAtExpr+`)
				   ELSE '{}'::jsonb END
			ELSE '{}'::jsonb END
		), updated_at = NOW()
		WHERE id = $2 AND deleted_at IS NULL
		RETURNING
			COALESCE((extra->>'quota_used')::numeric, 0),
			COALESCE((extra->>'quota_limit')::numeric, 0)`,
		amount, id)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	var newUsed, limit float64
	if rows.Next() {
		if err := rows.Scan(&newUsed, &limit); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// 任一维度配额刚超限时触发调度快照刷新
	if limit > 0 && newUsed >= limit && (newUsed-amount) < limit {
		if err := enqueueSchedulerOutbox(ctx, exec, service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
			logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue quota exceeded failed: account=%d err=%v", id, err)
		}
	}
	return nil
}

// ResetQuotaUsed 重置账号所有维度的配额用量为 0
// 保留固定重置模式的配置字段（quota_daily_reset_mode 等），仅清零用量和窗口起始时间
func (r *accountRepository) ResetQuotaUsed(ctx context.Context, id int64) error {
	exec := r.sqlFromContext(ctx)
	_, err := exec.ExecContext(ctx,
		`UPDATE accounts SET extra = (
			COALESCE(extra, '{}'::jsonb)
			|| '{"quota_used": 0, "quota_daily_used": 0, "quota_weekly_used": 0}'::jsonb
		) - 'quota_daily_start' - 'quota_weekly_start' - 'quota_daily_reset_at' - 'quota_weekly_reset_at', updated_at = NOW()
		WHERE id = $1 AND deleted_at IS NULL`,
		id)
	if err != nil {
		return err
	}
	// 重置配额后触发调度快照刷新，使账号重新参与调度
	if err := enqueueSchedulerOutbox(ctx, exec, service.SchedulerOutboxEventAccountChanged, &id, nil, nil); err != nil {
		logger.LegacyPrintf("repository.account", "[SchedulerOutbox] enqueue quota reset failed: account=%d err=%v", id, err)
	}
	return nil
}
