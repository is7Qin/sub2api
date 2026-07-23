package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/apikey"
	"github.com/Wei-Shaw/sub2api/ent/group"
	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"
	"github.com/Wei-Shaw/sub2api/ent/user"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
)

type apiKeyRepository struct {
	client *dbent.Client
	sql    sqlExecutor
}

func NewAPIKeyRepository(client *dbent.Client, sqlDB *sql.DB) service.APIKeyRepository {
	return newAPIKeyRepositoryWithSQL(client, sqlDB)
}

func newAPIKeyRepositoryWithSQL(client *dbent.Client, sqlq sqlExecutor) *apiKeyRepository {
	return &apiKeyRepository{client: client, sql: sqlq}
}

func (r *apiKeyRepository) activeQuery() *dbent.APIKeyQuery {
	// 默认过滤已软删除记录，避免删除后仍被查询到。
	return r.client.APIKey.Query().Where(apikey.DeletedAtIsNil())
}

func (r *apiKeyRepository) Create(ctx context.Context, key *service.APIKey) error {
	if existingTx := dbent.TxFromContext(ctx); existingTx != nil {
		return r.create(ctx, existingTx.Client(), key)
	}

	tx, err := r.client.Tx(ctx)
	if err != nil && !errors.Is(err, dbent.ErrTxStarted) {
		return translatePersistenceError(err, nil, service.ErrAPIKeyExists)
	}
	exec := r.client
	if err == nil {
		defer func() { _ = tx.Rollback() }()
		exec = tx.Client()
	}
	// err == dbent.ErrTxStarted 时复用当前事务(exec = r.client)。

	if err := r.create(ctx, exec, key); err != nil {
		return err
	}
	if tx != nil {
		return translatePersistenceError(tx.Commit(), nil, service.ErrAPIKeyExists)
	}
	return nil
}

func (r *apiKeyRepository) create(ctx context.Context, exec *dbent.Client, key *service.APIKey) error {
	// 与删除用户流程串行化：PostgreSQL 下创建 Key 前锁定 active user row，避免在
	// 用户软删除并清理 Key 的事务窗口内插入新的 active Key。SQLite 测试库不支持
	// SELECT ... FOR UPDATE，保留 active-user 校验但不加锁。
	userQuery := exec.User.Query().
		Where(user.IDEQ(key.UserID), user.DeletedAtIsNil())
	if exec.Driver().Dialect() == dialect.Postgres {
		userQuery.ForUpdate()
	}
	if _, err := userQuery.Only(ctx); err != nil {
		if dbent.IsNotFound(err) {
			return service.ErrUserNotFound
		}
		return err
	}

	builder := exec.APIKey.Create().
		SetUserID(key.UserID).
		SetKey(key.Key).
		SetName(key.Name).
		SetStatus(key.Status).
		SetNillableGroupID(key.GroupID).
		SetNillableLastUsedAt(key.LastUsedAt).
		SetConcurrency(key.Concurrency).
		SetQuota(key.Quota).
		SetQuotaUsed(key.QuotaUsed).
		SetNillableExpiresAt(key.ExpiresAt).
		SetRateLimit5h(key.RateLimit5h).
		SetRateLimit1d(key.RateLimit1d).
		SetRateLimit7d(key.RateLimit7d).
		SetOpenaiForcePriorityTier(key.OpenAIForcePriorityTier)

	if len(key.IPWhitelist) > 0 {
		builder.SetIPWhitelist(key.IPWhitelist)
	}
	if len(key.IPBlacklist) > 0 {
		builder.SetIPBlacklist(key.IPBlacklist)
	}

	created, err := builder.Save(ctx)
	if err == nil {
		key.ID = created.ID
		key.LastUsedAt = created.LastUsedAt
		key.CreatedAt = created.CreatedAt
		key.UpdatedAt = created.UpdatedAt
	}
	return translatePersistenceError(err, nil, service.ErrAPIKeyExists)
}

func (r *apiKeyRepository) GetByID(ctx context.Context, id int64) (*service.APIKey, error) {
	m, err := r.activeQuery().
		Where(apikey.IDEQ(id)).
		WithUser().
		WithGroup().
		Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, service.ErrAPIKeyNotFound
		}
		return nil, err
	}
	return apiKeyEntityToService(m), nil
}

// GetKeyAndOwnerID 根据 API Key ID 获取其 key 与所有者（用户）ID。
// 相比 GetByID，此方法性能更优，因为：
//   - 使用 Select() 只查询必要字段，减少数据传输量
//   - 不加载完整的 API Key 实体及其关联数据（User、Group 等）
//   - 适用于删除等只需 key 与用户 ID 的场景
func (r *apiKeyRepository) GetKeyAndOwnerID(ctx context.Context, id int64) (string, int64, error) {
	m, err := r.activeQuery().
		Where(apikey.IDEQ(id)).
		Select(apikey.FieldKey, apikey.FieldUserID).
		Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return "", 0, service.ErrAPIKeyNotFound
		}
		return "", 0, err
	}
	return m.Key, m.UserID, nil
}

func (r *apiKeyRepository) GetByKey(ctx context.Context, key string) (*service.APIKey, error) {
	m, err := r.activeQuery().
		Where(apikey.KeyEQ(key)).
		WithUser(func(q *dbent.UserQuery) {
			q.WithAllowedGroups(func(gq *dbent.GroupQuery) {
				gq.Select(group.FieldID)
			})
		}).
		WithGroup().
		Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, service.ErrAPIKeyNotFound
		}
		return nil, err
	}
	return apiKeyEntityToService(m), nil
}

func (r *apiKeyRepository) GetByKeyForAuth(ctx context.Context, key string) (*service.APIKey, error) {
	m, err := r.activeQuery().
		Where(apikey.KeyEQ(key)).
		Select(
			apikey.FieldID,
			apikey.FieldUserID,
			apikey.FieldGroupID,
			apikey.FieldName,
			apikey.FieldStatus,
			apikey.FieldIPWhitelist,
			apikey.FieldIPBlacklist,
			apikey.FieldConcurrency,
			apikey.FieldQuota,
			apikey.FieldQuotaUsed,
			apikey.FieldExpiresAt,
			apikey.FieldRateLimit5h,
			apikey.FieldRateLimit1d,
			apikey.FieldRateLimit7d,
			apikey.FieldOpenaiForcePriorityTier,
		).
		WithUser(func(q *dbent.UserQuery) {
			q.Select(
				user.FieldID,
				user.FieldEmail,
				user.FieldUsername,
				user.FieldStatus,
				user.FieldRole,
				user.FieldBalance,
				user.FieldConcurrency,
				user.FieldBalanceNotifyEnabled,
				user.FieldBalanceNotifyThresholdType,
				user.FieldBalanceNotifyThreshold,
				user.FieldBalanceNotifyExtraEmails,
				user.FieldTotalRecharged,
				user.FieldSignupSource,
				user.FieldLastLoginAt,
				user.FieldLastActiveAt,
				user.FieldRpmLimit,
			)
			q.WithAllowedGroups(func(gq *dbent.GroupQuery) {
				gq.Select(group.FieldID)
			})
		}).
		WithGroup(func(q *dbent.GroupQuery) {
			q.Select(
				group.FieldID,
				group.FieldName,
				group.FieldPlatform,
				group.FieldIsExclusive,
				group.FieldStatus,
				group.FieldSubscriptionType,
				group.FieldRateMultiplier,
				group.FieldDailyLimitUsd,
				group.FieldWeeklyLimitUsd,
				group.FieldMonthlyLimitUsd,
				group.FieldAllowImageGeneration,
				group.FieldImageRateIndependent,
				group.FieldImageRateMultiplier,
				group.FieldImagePrice1k,
				group.FieldImagePrice2k,
				group.FieldImagePrice4k,
				group.FieldClaudeCodeOnly,
				group.FieldFallbackGroupID,
				group.FieldFallbackGroupIDOnInvalidRequest,
				group.FieldModelRoutingEnabled,
				group.FieldModelRouting,
				group.FieldMcpXMLInject,
				group.FieldSupportedModelScopes,
				group.FieldAllowMessagesDispatch,
				group.FieldRequirePrivacySet,
				group.FieldDefaultMappedModel,
				group.FieldMessagesDispatchModelConfig,
				group.FieldModelsListConfig,
				group.FieldOpenaiLongContextBillingEnabled,
				group.FieldRpmLimit,
			)
		}).
		Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, service.ErrAPIKeyNotFound
		}
		return nil, err
	}
	return apiKeyEntityToService(m), nil
}

func (r *apiKeyRepository) UpdateConfig(ctx context.Context, id, expectedUserID int64, patch service.APIKeyConfigPatch) (*service.APIKey, error) {
	var result *service.APIKey
	err := r.withAPIKeyTx(ctx, func(opCtx context.Context, client *dbent.Client) error {
		q := client.APIKey.Query().Where(apikey.IDEQ(id), apikey.UserIDEQ(expectedUserID), apikey.DeletedAtIsNil())
		if client.Driver().Dialect() == dialect.Postgres {
			q.ForUpdate()
		}
		current, err := q.Only(opCtx)
		if dbent.IsNotFound(err) {
			return service.ErrAPIKeyNotFound
		}
		if err != nil {
			return err
		}
		b := current.Update().SetUpdatedAt(time.Now())
		if patch.Name != nil {
			b.SetName(*patch.Name)
		}
		if patch.GroupID != nil {
			if *patch.GroupID == nil {
				b.ClearGroupID()
			} else {
				b.SetGroupID(**patch.GroupID)
			}
		}
		if patch.Quota != nil {
			b.SetQuota(*patch.Quota)
		}
		if patch.Concurrency != nil {
			b.SetConcurrency(*patch.Concurrency)
		}
		if patch.ExpiresAt != nil {
			if *patch.ExpiresAt == nil {
				b.ClearExpiresAt()
			} else {
				b.SetExpiresAt(**patch.ExpiresAt)
			}
		}
		if patch.IPWhitelist != nil {
			if len(*patch.IPWhitelist) == 0 {
				b.ClearIPWhitelist()
			} else {
				b.SetIPWhitelist(*patch.IPWhitelist)
			}
		}
		if patch.IPBlacklist != nil {
			if len(*patch.IPBlacklist) == 0 {
				b.ClearIPBlacklist()
			} else {
				b.SetIPBlacklist(*patch.IPBlacklist)
			}
		}
		if patch.RateLimit5h != nil {
			b.SetRateLimit5h(*patch.RateLimit5h)
		}
		if patch.RateLimit1d != nil {
			b.SetRateLimit1d(*patch.RateLimit1d)
		}
		if patch.RateLimit7d != nil {
			b.SetRateLimit7d(*patch.RateLimit7d)
		}
		if patch.OpenAIForcePriorityTier != nil {
			b.SetOpenaiForcePriorityTier(*patch.OpenAIForcePriorityTier)
		}
		if patch.Status != nil {
			current.Status = *patch.Status
		}
		if patch.ResetQuota {
			b.SetQuotaUsed(0)
			current.QuotaUsed = 0
		}
		if patch.ResetRateLimitUsage {
			b.SetUsage5h(0).SetUsage1d(0).SetUsage7d(0).ClearWindow5hStart().ClearWindow1dStart().ClearWindow7dStart()
		}
		if patch.Quota != nil {
			current.Quota = *patch.Quota
		}
		if patch.ExpiresAt != nil {
			current.ExpiresAt = *patch.ExpiresAt
		}
		status := current.Status
		managedStatus := status == service.StatusAPIKeyActive || status == "inactive" || status == service.StatusAPIKeyExpired || status == service.StatusAPIKeyQuotaExhausted
		if !managedStatus {
			// Preserve disabled and administrator-defined statuses exactly as
			// reconcileAPIKeyTerminalStatus does at the service boundary.
		} else if current.ExpiresAt != nil && !current.ExpiresAt.After(time.Now()) {
			status = service.StatusAPIKeyExpired
		} else if current.Quota > 0 && current.QuotaUsed >= current.Quota {
			status = service.StatusAPIKeyQuotaExhausted
		} else if patch.Status != nil && (*patch.Status == service.StatusAPIKeyDisabled || *patch.Status == "inactive") {
			status = *patch.Status
		} else if patch.Status != nil || current.Status == service.StatusAPIKeyExpired || current.Status == service.StatusAPIKeyQuotaExhausted {
			status = service.StatusAPIKeyActive
		}
		b.SetStatus(status)
		if _, err := b.Save(opCtx); err != nil {
			return err
		}
		result, err = loadAPIKeyResponseView(opCtx, client, id)
		if err != nil {
			return err
		}
		return nil
	})
	return result, err
}

func (r *apiKeyRepository) UpdateGroupID(ctx context.Context, id int64, groupID *int64) (*service.APIKey, error) {
	var result *service.APIKey
	err := r.withAPIKeyTx(ctx, func(opCtx context.Context, client *dbent.Client) error {
		b := client.APIKey.UpdateOneID(id).Where(apikey.DeletedAtIsNil()).SetUpdatedAt(time.Now())
		if groupID == nil {
			b.ClearGroupID()
		} else {
			b.SetGroupID(*groupID)
		}
		if _, err := b.Save(opCtx); err != nil {
			if dbent.IsNotFound(err) {
				return service.ErrAPIKeyNotFound
			}
			return err
		}
		var err error
		result, err = loadAPIKeyResponseView(opCtx, client, id)
		return err
	})
	return result, err
}

func (r *apiKeyRepository) ResetRateLimitUsage(ctx context.Context, id int64) (*service.APIKey, error) {
	var result *service.APIKey
	err := r.withAPIKeyTx(ctx, func(opCtx context.Context, client *dbent.Client) error {
		_, err := client.APIKey.UpdateOneID(id).Where(apikey.DeletedAtIsNil()).
			SetUsage5h(0).SetUsage1d(0).SetUsage7d(0).
			ClearWindow5hStart().ClearWindow1dStart().ClearWindow7dStart().SetUpdatedAt(time.Now()).Save(opCtx)
		if dbent.IsNotFound(err) {
			return service.ErrAPIKeyNotFound
		}
		if err != nil {
			return err
		}
		result, err = loadAPIKeyResponseView(opCtx, client, id)
		return err
	})
	return result, err
}

// loadAPIKeyResponseView materializes the scalar row and response edges from one
// transaction. Callers keep the row's update lock until this view is loaded.
func loadAPIKeyResponseView(ctx context.Context, client *dbent.Client, id int64) (*service.APIKey, error) {
	m, err := client.APIKey.Query().
		Where(apikey.IDEQ(id), apikey.DeletedAtIsNil()).
		WithUser().
		WithGroup().
		Only(ctx)
	if dbent.IsNotFound(err) {
		return nil, service.ErrAPIKeyNotFound
	}
	if err != nil {
		return nil, err
	}
	return apiKeyEntityToService(m), nil
}

func (r *apiKeyRepository) withAPIKeyTx(ctx context.Context, fn func(context.Context, *dbent.Client) error) error {
	if tx := dbent.TxFromContext(ctx); tx != nil {
		return fn(ctx, tx.Client())
	}
	tx, err := r.client.Tx(ctx)
	if errors.Is(err, dbent.ErrTxStarted) {
		return fn(ctx, r.client)
	}
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	opCtx := dbent.NewTxContext(ctx, tx)
	if err := fn(opCtx, tx.Client()); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *apiKeyRepository) sqlFromContext(ctx context.Context) sqlExecutor {
	if tx := dbent.TxFromContext(ctx); tx != nil {
		return tx.Client()
	}
	return r.sql
}

func (r *apiKeyRepository) Delete(ctx context.Context, id int64) error {
	// 存在唯一键约束 生成tombstone key 用来释放原key，长度远小于 128，满足 schema 限制
	tombstoneKey := fmt.Sprintf("__deleted__%d__%d", id, time.Now().UnixNano())
	// 显式软删除：避免依赖 Hook 行为，确保 deleted_at 一定被设置。
	affected, err := r.client.APIKey.Update().
		Where(apikey.IDEQ(id), apikey.DeletedAtIsNil()).
		SetKey(tombstoneKey).
		SetDeletedAt(time.Now()).
		Save(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return service.ErrAPIKeyNotFound
		}
		return err
	}
	if affected == 0 {
		exists, err := r.client.APIKey.Query().
			Where(apikey.IDEQ(id)).
			Exist(mixins.SkipSoftDelete(ctx))
		if err != nil {
			return err
		}
		if exists {
			return nil
		}
		return service.ErrAPIKeyNotFound
	}
	return nil
}

// DeleteWithAudit 在同一事务内:
//  1. 把(明文 key、所有者、key 名称)写入 deleted_api_key_audits;
//  2. 软删除该 key(tombstone 覆盖 key 列以释放唯一约束)。
//
// 保证"被删除的 key 一定能反查到所有者"。事务模式与 group_repo.DeleteCascade 一致。
func (r *apiKeyRepository) DeleteWithAudit(ctx context.Context, id int64) error {
	tombstoneKey := fmt.Sprintf("__deleted__%d__%d", id, time.Now().UnixNano())

	if existingTx := dbent.TxFromContext(ctx); existingTx != nil {
		return r.deleteWithAudit(ctx, existingTx.Client(), id, tombstoneKey)
	}

	tx, err := r.client.Tx(ctx)
	if err != nil && !errors.Is(err, dbent.ErrTxStarted) {
		return err
	}
	exec := r.client
	if err == nil {
		defer func() { _ = tx.Rollback() }()
		exec = tx.Client()
	}
	// err == dbent.ErrTxStarted 时复用当前事务(exec = r.client)。

	if err := r.deleteWithAudit(ctx, exec, id, tombstoneKey); err != nil {
		return err
	}

	if tx != nil {
		return tx.Commit()
	}
	return nil
}

func (r *apiKeyRepository) deleteWithAudit(ctx context.Context, exec *dbent.Client, id int64, tombstoneKey string) error {
	// 先锁定并 tombstone 覆盖 active key，再用 RETURNING 的原始值写审计，避免并发删除
	// 在审计 INSERT 与软删除 UPDATE 之间交错导致重复审计行。
	rows, err := exec.QueryContext(ctx, `
		WITH locked AS (
			SELECT id, key, user_id, name
			FROM api_keys
			WHERE id = $2 AND deleted_at IS NULL
			FOR UPDATE
		), deleted AS (
			UPDATE api_keys AS ak
			SET key = $1, deleted_at = NOW(), updated_at = NOW()
			FROM locked
			WHERE ak.id = locked.id
			RETURNING locked.key AS original_key, ak.id, ak.user_id, ak.name, ak.deleted_at
		), audited AS (
			INSERT INTO deleted_api_key_audits (key, api_key_id, user_id, key_name, deleted_at)
			SELECT original_key, id, user_id, name, deleted_at
			FROM deleted
			RETURNING api_key_id
		)
		SELECT deleted.id
		FROM deleted
		JOIN audited ON audited.api_key_id = deleted.id`, tombstoneKey, id)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	if rows.Next() {
		return rows.Err()
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// 并发/重复删除:记录已存在(已软删)则幂等返回 nil，否则 NotFound。
	exists, existErr := exec.APIKey.Query().
		Where(apikey.IDEQ(id)).
		Exist(mixins.SkipSoftDelete(ctx))
	if existErr != nil {
		return existErr
	}
	if exists {
		return nil
	}
	return service.ErrAPIKeyNotFound
}

// DeleteByUserIDWithAudit soft-deletes every active API key owned by userID and
// returns the original key values for post-commit auth-cache invalidation.
func (r *apiKeyRepository) DeleteByUserIDWithAudit(ctx context.Context, userID int64) ([]string, error) {
	if existingTx := dbent.TxFromContext(ctx); existingTx != nil {
		return r.deleteByUserIDWithAudit(ctx, existingTx.Client(), userID)
	}

	tx, err := r.client.Tx(ctx)
	if err != nil && !errors.Is(err, dbent.ErrTxStarted) {
		return nil, err
	}
	exec := r.client
	if err == nil {
		defer func() { _ = tx.Rollback() }()
		exec = tx.Client()
	}
	// err == dbent.ErrTxStarted 时复用当前事务(exec = r.client)。

	keys, err := r.deleteByUserIDWithAudit(ctx, exec, userID)
	if err != nil {
		return nil, err
	}
	if tx != nil {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

func (r *apiKeyRepository) deleteByUserIDWithAudit(ctx context.Context, exec *dbent.Client, userID int64) ([]string, error) {
	tombstoneSuffix := fmt.Sprintf("%d", time.Now().UnixNano())

	// Lock and tombstone the complete active-key set in one statement. This avoids
	// offset pagination gaps if another transaction deletes a key while admin user
	// deletion is collecting the target list.
	rows, err := exec.QueryContext(ctx, `
		WITH locked AS (
			SELECT id, key, user_id, name
			FROM api_keys
			WHERE user_id = $1 AND deleted_at IS NULL
			ORDER BY id
			FOR UPDATE
		), deleted AS (
			UPDATE api_keys AS ak
			SET key = CONCAT('__deleted__', ak.id, '__', $2::text), deleted_at = NOW(), updated_at = NOW()
			FROM locked
			WHERE ak.id = locked.id
			RETURNING locked.key AS original_key, ak.id, ak.user_id, ak.name, ak.deleted_at
		), audited AS (
			INSERT INTO deleted_api_key_audits (key, api_key_id, user_id, key_name, deleted_at)
			SELECT original_key, id, user_id, name, deleted_at
			FROM deleted
			RETURNING api_key_id
		)
		SELECT deleted.original_key
		FROM deleted
		JOIN audited ON audited.api_key_id = deleted.id`, userID, tombstoneSuffix)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	keys := make([]string, 0)
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		if key = strings.TrimSpace(key); key != "" {
			keys = append(keys, key)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return keys, nil
}

func (r *apiKeyRepository) ListByUserID(ctx context.Context, userID int64, params pagination.PaginationParams, filters service.APIKeyListFilters) ([]service.APIKey, *pagination.PaginationResult, error) {
	client := clientFromContext(ctx, r.client)
	q := client.APIKey.Query().Where(apikey.DeletedAtIsNil(), apikey.UserIDEQ(userID))

	// Apply filters
	if filters.Search != "" {
		q = q.Where(apikey.Or(
			apikey.NameContainsFold(filters.Search),
			apikey.KeyContainsFold(filters.Search),
		))
	}
	if filters.Status != "" {
		q = q.Where(apikey.StatusEQ(filters.Status))
	}
	if filters.GroupID != nil {
		if *filters.GroupID == 0 {
			q = q.Where(apikey.GroupIDIsNil())
		} else {
			q = q.Where(apikey.GroupIDEQ(*filters.GroupID))
		}
	}

	total, err := q.Count(ctx)
	if err != nil {
		return nil, nil, err
	}

	keysQuery := q.
		WithGroup().
		Offset(params.Offset()).
		Limit(params.Limit())
	for _, order := range apiKeyListOrder(params) {
		keysQuery = keysQuery.Order(order)
	}

	keys, err := keysQuery.All(ctx)
	if err != nil {
		return nil, nil, err
	}

	outKeys := make([]service.APIKey, 0, len(keys))
	for i := range keys {
		outKeys = append(outKeys, *apiKeyEntityToService(keys[i]))
	}

	return outKeys, paginationResultFromTotal(int64(total), params), nil
}

func (r *apiKeyRepository) VerifyOwnership(ctx context.Context, userID int64, apiKeyIDs []int64) ([]int64, error) {
	if len(apiKeyIDs) == 0 {
		return []int64{}, nil
	}

	client := clientFromContext(ctx, r.client)
	ids, err := client.APIKey.Query().
		Where(apikey.UserIDEQ(userID), apikey.IDIn(apiKeyIDs...), apikey.DeletedAtIsNil()).
		IDs(ctx)
	if err != nil {
		return nil, err
	}
	return ids, nil
}

func (r *apiKeyRepository) CountByUserID(ctx context.Context, userID int64) (int64, error) {
	client := clientFromContext(ctx, r.client)
	count, err := client.APIKey.Query().Where(apikey.DeletedAtIsNil(), apikey.UserIDEQ(userID)).Count(ctx)
	return int64(count), err
}

func (r *apiKeyRepository) ExistsByKey(ctx context.Context, key string) (bool, error) {
	count, err := r.activeQuery().Where(apikey.KeyEQ(key)).Count(ctx)
	return count > 0, err
}

func (r *apiKeyRepository) ListByGroupID(ctx context.Context, groupID int64, params pagination.PaginationParams) ([]service.APIKey, *pagination.PaginationResult, error) {
	q := r.activeQuery().Where(apikey.GroupIDEQ(groupID))

	total, err := q.Count(ctx)
	if err != nil {
		return nil, nil, err
	}

	keysQuery := q.
		WithUser().
		Offset(params.Offset()).
		Limit(params.Limit())
	for _, order := range apiKeyListOrder(params) {
		keysQuery = keysQuery.Order(order)
	}

	keys, err := keysQuery.All(ctx)
	if err != nil {
		return nil, nil, err
	}

	outKeys := make([]service.APIKey, 0, len(keys))
	for i := range keys {
		outKeys = append(outKeys, *apiKeyEntityToService(keys[i]))
	}

	return outKeys, paginationResultFromTotal(int64(total), params), nil
}

func apiKeyListOrder(params pagination.PaginationParams) []func(*entsql.Selector) {
	sortBy := strings.ToLower(strings.TrimSpace(params.SortBy))
	sortOrder := params.NormalizedSortOrder(pagination.SortOrderDesc)

	var field string
	switch sortBy {
	case "name":
		field = apikey.FieldName
	case "status":
		field = apikey.FieldStatus
	case "expires_at":
		field = apikey.FieldExpiresAt
	case "last_used_at":
		field = apikey.FieldLastUsedAt
	case "created_at":
		field = apikey.FieldCreatedAt
	default:
		field = apikey.FieldID
	}

	if sortOrder == pagination.SortOrderAsc {
		return []func(*entsql.Selector){dbent.Asc(field), dbent.Asc(apikey.FieldID)}
	}
	return []func(*entsql.Selector){dbent.Desc(field), dbent.Desc(apikey.FieldID)}
}

// SearchAPIKeys searches API keys by user ID and/or keyword (name)
func (r *apiKeyRepository) SearchAPIKeys(ctx context.Context, userID int64, keyword string, limit int) ([]service.APIKey, error) {
	q := r.activeQuery()
	if userID > 0 {
		q = q.Where(apikey.UserIDEQ(userID))
	}

	if keyword != "" {
		q = q.Where(apikey.NameContainsFold(keyword))
	}

	keys, err := q.Limit(limit).Order(dbent.Desc(apikey.FieldID)).All(ctx)
	if err != nil {
		return nil, err
	}

	outKeys := make([]service.APIKey, 0, len(keys))
	for i := range keys {
		outKeys = append(outKeys, *apiKeyEntityToService(keys[i]))
	}
	return outKeys, nil
}

// ClearGroupIDByGroupID 将指定分组的所有 API Key 的 group_id 设为 nil
func (r *apiKeyRepository) ClearGroupIDByGroupID(ctx context.Context, groupID int64) (int64, error) {
	n, err := r.client.APIKey.Update().
		Where(apikey.GroupIDEQ(groupID), apikey.DeletedAtIsNil()).
		ClearGroupID().
		Save(ctx)
	return int64(n), err
}

// UpdateGroupIDByUserAndGroup 将用户下绑定 oldGroupID 的所有 Key 迁移到 newGroupID
func (r *apiKeyRepository) UpdateGroupIDByUserAndGroup(ctx context.Context, userID, oldGroupID, newGroupID int64) (int64, error) {
	client := clientFromContext(ctx, r.client)
	n, err := client.APIKey.Update().
		Where(apikey.UserIDEQ(userID), apikey.GroupIDEQ(oldGroupID), apikey.DeletedAtIsNil()).
		SetGroupID(newGroupID).
		Save(ctx)
	return int64(n), err
}

// CountByGroupID 获取分组的 API Key 数量
func (r *apiKeyRepository) CountByGroupID(ctx context.Context, groupID int64) (int64, error) {
	count, err := r.activeQuery().Where(apikey.GroupIDEQ(groupID)).Count(ctx)
	return int64(count), err
}

func (r *apiKeyRepository) ListKeysByUserID(ctx context.Context, userID int64) ([]string, error) {
	keys, err := r.activeQuery().
		Where(apikey.UserIDEQ(userID)).
		Select(apikey.FieldKey).
		Strings(ctx)
	if err != nil {
		return nil, err
	}
	return keys, nil
}

func (r *apiKeyRepository) ListKeysByGroupID(ctx context.Context, groupID int64) ([]string, error) {
	keys, err := r.activeQuery().
		Where(apikey.GroupIDEQ(groupID)).
		Select(apikey.FieldKey).
		Strings(ctx)
	if err != nil {
		return nil, err
	}
	return keys, nil
}

// IncrementQuotaUsed 使用 Ent 原子递增 quota_used 字段并返回新值
func (r *apiKeyRepository) IncrementQuotaUsed(ctx context.Context, id int64, amount float64) (float64, error) {
	updated, err := r.client.APIKey.UpdateOneID(id).
		Where(apikey.DeletedAtIsNil()).
		AddQuotaUsed(amount).
		Save(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return 0, service.ErrAPIKeyNotFound
		}
		return 0, err
	}
	return updated.QuotaUsed, nil
}

// IncrementQuotaUsedAndGetState atomically increments quota_used, conditionally marks the key
// as quota_exhausted, and returns the latest quota state in one round trip.
func (r *apiKeyRepository) IncrementQuotaUsedAndGetState(ctx context.Context, id int64, amount float64) (*service.APIKeyQuotaUsageState, error) {
	query := `
		UPDATE api_keys
		SET
			quota_used = quota_used + $1,
			status = CASE
				WHEN quota > 0 AND quota_used + $1 >= quota THEN $2
				ELSE status
			END,
			updated_at = NOW()
		WHERE id = $3 AND deleted_at IS NULL
		RETURNING quota_used, quota, key, status
	`

	state := &service.APIKeyQuotaUsageState{}
	if err := scanSingleRow(ctx, r.sqlFromContext(ctx), query, []any{amount, service.StatusAPIKeyQuotaExhausted, id}, &state.QuotaUsed, &state.Quota, &state.Key, &state.Status); err != nil {
		if err == sql.ErrNoRows {
			return nil, service.ErrAPIKeyNotFound
		}
		return nil, err
	}
	return state, nil
}

func (r *apiKeyRepository) UpdateLastUsed(ctx context.Context, id int64, usedAt time.Time) error {
	affected, err := r.client.APIKey.Update().
		Where(apikey.IDEQ(id), apikey.DeletedAtIsNil()).
		SetLastUsedAt(usedAt).
		SetUpdatedAt(usedAt).
		Save(ctx)
	if err != nil {
		return err
	}
	if affected == 0 {
		return service.ErrAPIKeyNotFound
	}
	return nil
}

// IncrementRateLimitUsage atomically increments all rate limit usage counters and initializes
// window start times via COALESCE if not already set.
func (r *apiKeyRepository) IncrementRateLimitUsage(ctx context.Context, id int64, cost float64) error {
	_, err := r.sqlFromContext(ctx).ExecContext(ctx, `
		UPDATE api_keys SET
			usage_5h = CASE WHEN window_5h_start IS NOT NULL AND window_5h_start + INTERVAL '5 hours' <= NOW() THEN $1 ELSE usage_5h + $1 END,
			usage_1d = CASE WHEN window_1d_start IS NOT NULL AND window_1d_start + INTERVAL '24 hours' <= NOW() THEN $1 ELSE usage_1d + $1 END,
			usage_7d = CASE WHEN window_7d_start IS NOT NULL AND window_7d_start + INTERVAL '7 days' <= NOW() THEN $1 ELSE usage_7d + $1 END,
			window_5h_start = CASE WHEN window_5h_start IS NULL OR window_5h_start + INTERVAL '5 hours' <= NOW() THEN NOW() ELSE window_5h_start END,
			window_1d_start = CASE WHEN window_1d_start IS NULL OR window_1d_start + INTERVAL '24 hours' <= NOW() THEN date_trunc('day', NOW()) ELSE window_1d_start END,
			window_7d_start = CASE WHEN window_7d_start IS NULL OR window_7d_start + INTERVAL '7 days' <= NOW() THEN date_trunc('day', NOW()) ELSE window_7d_start END,
			updated_at = NOW()
		WHERE id = $2 AND deleted_at IS NULL`,
		cost, id)
	return err
}

// ResetRateLimitWindows resets expired rate limit windows atomically.
func (r *apiKeyRepository) ResetRateLimitWindows(ctx context.Context, id int64) error {
	_, err := r.sqlFromContext(ctx).ExecContext(ctx, `
		UPDATE api_keys SET
			usage_5h = CASE WHEN window_5h_start IS NOT NULL AND window_5h_start + INTERVAL '5 hours' <= NOW() THEN 0 ELSE usage_5h END,
			window_5h_start = CASE WHEN window_5h_start IS NOT NULL AND window_5h_start + INTERVAL '5 hours' <= NOW() THEN NOW() ELSE window_5h_start END,
			usage_1d = CASE WHEN window_1d_start IS NOT NULL AND window_1d_start + INTERVAL '24 hours' <= NOW() THEN 0 ELSE usage_1d END,
			window_1d_start = CASE WHEN window_1d_start IS NOT NULL AND window_1d_start + INTERVAL '24 hours' <= NOW() THEN date_trunc('day', NOW()) ELSE window_1d_start END,
			usage_7d = CASE WHEN window_7d_start IS NOT NULL AND window_7d_start + INTERVAL '7 days' <= NOW() THEN 0 ELSE usage_7d END,
			window_7d_start = CASE WHEN window_7d_start IS NOT NULL AND window_7d_start + INTERVAL '7 days' <= NOW() THEN date_trunc('day', NOW()) ELSE window_7d_start END,
			updated_at = NOW()
		WHERE id = $1 AND deleted_at IS NULL`,
		id)
	return err
}

// GetRateLimitData returns the current rate limit usage and window start times for an API key.
func (r *apiKeyRepository) GetRateLimitData(ctx context.Context, id int64) (result *service.APIKeyRateLimitData, err error) {
	rows, err := r.sqlFromContext(ctx).QueryContext(ctx, `
		SELECT usage_5h, usage_1d, usage_7d, window_5h_start, window_1d_start, window_7d_start
		FROM api_keys
		WHERE id = $1 AND deleted_at IS NULL`,
		id)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()
	if !rows.Next() {
		return nil, service.ErrAPIKeyNotFound
	}
	data := &service.APIKeyRateLimitData{}
	if err := rows.Scan(&data.Usage5h, &data.Usage1d, &data.Usage7d, &data.Window5hStart, &data.Window1dStart, &data.Window7dStart); err != nil {
		return nil, err
	}
	return data, rows.Err()
}

func apiKeyEntityToService(m *dbent.APIKey) *service.APIKey {
	if m == nil {
		return nil
	}
	out := &service.APIKey{
		ID:            m.ID,
		UserID:        m.UserID,
		Key:           m.Key,
		Name:          m.Name,
		Status:        m.Status,
		IPWhitelist:   m.IPWhitelist,
		IPBlacklist:   m.IPBlacklist,
		Concurrency:   m.Concurrency,
		LastUsedAt:    m.LastUsedAt,
		CreatedAt:     m.CreatedAt,
		UpdatedAt:     m.UpdatedAt,
		GroupID:       m.GroupID,
		Quota:         m.Quota,
		QuotaUsed:     m.QuotaUsed,
		ExpiresAt:     m.ExpiresAt,
		RateLimit5h:   m.RateLimit5h,
		RateLimit1d:   m.RateLimit1d,
		RateLimit7d:   m.RateLimit7d,
		Usage5h:       m.Usage5h,
		Usage1d:       m.Usage1d,
		Usage7d:       m.Usage7d,
		Window5hStart: m.Window5hStart,
		Window1dStart: m.Window1dStart,
		Window7dStart: m.Window7dStart,

		OpenAIForcePriorityTier: m.OpenaiForcePriorityTier,
	}
	if m.Edges.User != nil {
		out.User = userEntityToService(m.Edges.User)
	}
	if m.Edges.Group != nil {
		out.Group = groupEntityToService(m.Edges.Group)
	}
	return out
}

func userEntityToService(u *dbent.User) *service.User {
	if u == nil {
		return nil
	}
	out := &service.User{
		ID:                         u.ID,
		Email:                      u.Email,
		Username:                   u.Username,
		Notes:                      u.Notes,
		PasswordHash:               u.PasswordHash,
		Role:                       u.Role,
		Balance:                    u.Balance,
		Concurrency:                u.Concurrency,
		Status:                     u.Status,
		SignupSource:               u.SignupSource,
		LastLoginAt:                u.LastLoginAt,
		LastActiveAt:               u.LastActiveAt,
		TotpSecretEncrypted:        u.TotpSecretEncrypted,
		TotpEnabled:                u.TotpEnabled,
		TotpEnabledAt:              u.TotpEnabledAt,
		BalanceNotifyEnabled:       u.BalanceNotifyEnabled,
		BalanceNotifyThresholdType: u.BalanceNotifyThresholdType,
		BalanceNotifyThreshold:     u.BalanceNotifyThreshold,
		TotalRecharged:             u.TotalRecharged,
		RPMLimit:                   u.RpmLimit,
		CreatedAt:                  u.CreatedAt,
		UpdatedAt:                  u.UpdatedAt,
		DeletedAt:                  u.DeletedAt,
	}
	// Parse extra emails JSON (supports both old []string and new []NotifyEmailEntry format)
	if u.BalanceNotifyExtraEmails != "" && u.BalanceNotifyExtraEmails != "[]" {
		out.BalanceNotifyExtraEmails = service.ParseNotifyEmails(u.BalanceNotifyExtraEmails)
	}
	if len(u.Edges.AllowedGroups) > 0 {
		out.AllowedGroups = make([]int64, 0, len(u.Edges.AllowedGroups))
		for _, g := range u.Edges.AllowedGroups {
			if g != nil {
				out.AllowedGroups = append(out.AllowedGroups, g.ID)
			}
		}
	}
	return out
}

func groupEntityToService(g *dbent.Group) *service.Group {
	if g == nil {
		return nil
	}
	return &service.Group{
		ID:                              g.ID,
		Name:                            g.Name,
		Description:                     derefString(g.Description),
		Platform:                        g.Platform,
		RateMultiplier:                  g.RateMultiplier,
		IsExclusive:                     g.IsExclusive,
		Status:                          g.Status,
		Hydrated:                        true,
		SubscriptionType:                g.SubscriptionType,
		DailyLimitUSD:                   g.DailyLimitUsd,
		WeeklyLimitUSD:                  g.WeeklyLimitUsd,
		MonthlyLimitUSD:                 g.MonthlyLimitUsd,
		AllowImageGeneration:            g.AllowImageGeneration,
		ImageRateIndependent:            g.ImageRateIndependent,
		ImageRateMultiplier:             g.ImageRateMultiplier,
		ImagePrice1K:                    g.ImagePrice1k,
		ImagePrice2K:                    g.ImagePrice2k,
		ImagePrice4K:                    g.ImagePrice4k,
		DefaultValidityDays:             g.DefaultValidityDays,
		ClaudeCodeOnly:                  g.ClaudeCodeOnly,
		FallbackGroupID:                 g.FallbackGroupID,
		FallbackGroupIDOnInvalidRequest: g.FallbackGroupIDOnInvalidRequest,
		ModelRouting:                    g.ModelRouting,
		ModelRoutingEnabled:             g.ModelRoutingEnabled,
		MCPXMLInject:                    g.McpXMLInject,
		SupportedModelScopes:            g.SupportedModelScopes,
		SortOrder:                       g.SortOrder,
		AllowMessagesDispatch:           g.AllowMessagesDispatch,
		RequireOAuthOnly:                g.RequireOauthOnly,
		RequirePrivacySet:               g.RequirePrivacySet,
		DefaultMappedModel:              g.DefaultMappedModel,
		MessagesDispatchModelConfig:     g.MessagesDispatchModelConfig,
		ModelsListConfig:                g.ModelsListConfig,
		OpenAILongContextBillingEnabled: g.OpenaiLongContextBillingEnabled,
		RPMLimit:                        g.RpmLimit,
		CreatedAt:                       g.CreatedAt,
		UpdatedAt:                       g.UpdatedAt,
	}
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
