package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	dbpredicate "github.com/Wei-Shaw/sub2api/ent/predicate"
	"golang.org/x/sync/singleflight"
)

// modelAvailabilityCandidatesCacheTTL 是模型可用性候选的缓存 TTL。
// 该查询只服务于 404-vs-503 判别（“请求模型是否被任何持久启用的账号支持”），
// 30s 内的陈旧结论无害。不采用账号列表缓存的 max(accounts.updated_at)
// 版本失效：那会在每请求热路径上增加一次全表 max(updated_at) 扫描，
// 与本修复“消除热路径 DB 往返与解码开销”的目标相悖。
const modelAvailabilityCandidatesCacheTTL = 30 * time.Second

// modelAvailabilityCandidateCache 按 groupID（含未分组）缓存判别候选账号，
// TTL 到期后由 singleflight 合并并发 miss，避免缓存击穿。
type modelAvailabilityCandidateCache struct {
	mu      sync.Mutex
	entries map[string]modelAvailabilityCandidateEntry
	now     func() time.Time // 可注入时钟，便于测试 TTL 行为
	sf      singleflight.Group
}

type modelAvailabilityCandidateEntry struct {
	accounts  []service.Account
	expiresAt time.Time
}

func newModelAvailabilityCandidateCache() *modelAvailabilityCandidateCache {
	return &modelAvailabilityCandidateCache{
		entries: make(map[string]modelAvailabilityCandidateEntry),
		now:     time.Now,
	}
}

func (c *modelAvailabilityCandidateCache) get(key string) ([]service.Account, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || c.now().After(e.expiresAt) {
		return nil, false
	}
	return e.accounts, true
}

func (c *modelAvailabilityCandidateCache) set(key string, accounts []service.Account) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = modelAvailabilityCandidateEntry{
		accounts:  accounts,
		expiresAt: c.now().Add(modelAvailabilityCandidatesCacheTTL),
	}
}

// modelAvailabilityCandidatesCacheKey 归一化缓存键：groupID（nil=未分组）、
// 平台列表（排序后拼接，忽略顺序差异）、includeGrouped。
func modelAvailabilityCandidatesCacheKey(groupID *int64, platforms []string, includeGrouped bool) string {
	groupPart := "ungrouped"
	if groupID != nil {
		groupPart = "group:" + strconv.FormatInt(*groupID, 10)
	}
	sorted := append([]string(nil), platforms...)
	sort.Strings(sorted)
	return groupPart + "|" + strings.Join(sorted, ",") + "|" + strconv.FormatBool(includeGrouped)
}

// modelAvailabilityCredentialsSubKeys 是 404-vs-503 判别实际消费的 credentials
// JSONB 子键白名单。消费证据（按调用链 isPureOpenAIModelSupportMiss /
// isPureModelSupportMiss → 各账号方法）：
//   - model_mapping：GetModelMapping（IsModelSupported / mapAntigravityModel /
//     ResolveMappedModel，映射后的上游模型还参与渠道限制检查）
//   - compact_model_mapping：GetCompactModelMapping（/responses/compact 上游模型）
//   - openai_capabilities：SupportsOpenAIEndpointCapability
//   - aws_region / aws_force_global：ResolveBedrockModelID 的区域前缀调整
var modelAvailabilityCredentialsSubKeys = []string{
	"model_mapping",
	"compact_model_mapping",
	"openai_capabilities",
	"aws_region",
	"aws_force_global",
}

// modelAvailabilityExtraSubKeys 同理，是判别消费的 extra JSONB 子键白名单：
//   - privacy_mode：IsPrivacySet（分组要求隐私时屏蔽非隐私账号）
//   - openai_passthrough：IsOpenAIAPIKeyPassthroughEnabled（APIKey fail-open）
//   - mixed_scheduling：IsMixedSchedulingEnabled（antigravity 混排）
//   - openai_compact_mode / openai_compact_supported：OpenAICompactSupportKnown
//   - openai_ws_force_http / openai_oauth_ws_mode /
//     openai_apikey_responses_websockets_v2_mode /
//     openai_apikey_responses_websockets_v2_enabled /
//     responses_websockets_v2_enabled / openai_ws_enabled：默认 WS 协议决策器
//     （isOpenAIAccountTransportCompatible → OpenAIWSProtocolResolver）
var modelAvailabilityExtraSubKeys = []string{
	"privacy_mode",
	"openai_passthrough",
	"mixed_scheduling",
	"openai_compact_mode",
	"openai_compact_supported",
	"openai_ws_force_http",
	"openai_oauth_ws_mode",
	"openai_apikey_responses_websockets_v2_mode",
	"openai_apikey_responses_websockets_v2_enabled",
	"responses_websockets_v2_enabled",
	"openai_ws_enabled",
}

// modelAvailabilityCandidateSelectList 生成投影 SELECT 列表：除 id/platform/type/
// concurrency/priority 标量列外，credentials/extra 只以 JSONB 子键表达式出现。
// 全量凭据列（access_token/refresh_token 等敏感字段）不进入扫描与解码。
func modelAvailabilityCandidateSelectList(alias string) string {
	cols := []string{
		alias + ".id",
		alias + ".platform",
		alias + ".type",
		alias + ".concurrency",
		alias + ".priority",
	}
	for _, key := range modelAvailabilityCredentialsSubKeys {
		cols = append(cols, alias+".credentials->'"+key+"'")
	}
	for _, key := range modelAvailabilityExtraSubKeys {
		cols = append(cols, alias+".extra->'"+key+"'")
	}
	return strings.Join(cols, ", ")
}

// queryModelAvailabilityCandidatesProjected 以投影 SQL 查询模型可用性候选，
// 语义与原 ent 查询逐项对齐（见 ListModelAvailabilityCandidates 的英文注释）：
//   - 分组路径：account_groups 按 group priority、account priority 排序，
//     DISTINCT ON (a.id) 保留首个分组优先级出现（与 ent 端 accountMap 去重一致）
//   - 未分组路径：NOT EXISTS 过滤已绑定分组的账号
//   - 两者均只叠加 status/schedulable/platform 持久过滤（软删除由
//     deleted_at IS NULL 显式承担，等价于 ent SoftDeleteMixin 拦截器），
//     transient 冷却/过载状态一律不参与，避免瞬时容量不足被误判为永久 404
func (r *accountRepository) queryModelAvailabilityCandidatesProjected(ctx context.Context, groupID *int64, platforms []string, includeGrouped bool) ([]service.Account, error) {
	// 平台过滤使用 IN 占位符展开（与 ent PlatformIn 生成的 SQL 一致），
	// 避免 ANY($n) 依赖驱动对 []string 参数的原生支持。
	platformArgs := make([]any, len(platforms))
	for i, p := range platforms {
		platformArgs[i] = p
	}
	// 占位符起点：分组路径 $1=group_id、$2=status；未分组路径 $1=status。
	base := 3
	if groupID == nil {
		base = 2
	}
	platformPlaceholders := make([]string, len(platforms))
	for i := range platforms {
		platformPlaceholders[i] = "$" + strconv.Itoa(base+i)
	}
	platformsClause := "(" + strings.Join(platformPlaceholders, ", ") + ")"

	// 未分组路径：默认排除已绑定分组的账号（与旧 ent 查询一致）；
	// includeGrouped=true（简单模式下全量池判别）时不加该过滤。
	groupMembershipClause := ""
	if !includeGrouped {
		groupMembershipClause = "\n  AND NOT EXISTS (SELECT 1 FROM account_groups ag WHERE ag.account_id = a.id)"
	}

	var query string
	var args []any
	if groupID != nil {
		query = fmt.Sprintf(`SELECT DISTINCT ON (a.id) %s
FROM account_groups ag JOIN accounts a ON a.id = ag.account_id AND a.deleted_at IS NULL
WHERE ag.group_id = $1 AND a.status = $2 AND a.schedulable = TRUE AND a.platform IN %s
ORDER BY a.id, ag.priority, a.priority`, modelAvailabilityCandidateSelectList("a"), platformsClause)
		args = append([]any{*groupID, service.StatusActive}, platformArgs...)
	} else {
		query = fmt.Sprintf(`SELECT %s
FROM accounts a
WHERE a.deleted_at IS NULL AND a.status = $1 AND a.schedulable = TRUE
  AND a.platform IN %s%s
ORDER BY a.priority`, modelAvailabilityCandidateSelectList("a"), platformsClause, groupMembershipClause)
		args = append([]any{service.StatusActive}, platformArgs...)
	}

	rows, err := r.sqlFromContext(ctx).QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	subKeyCount := len(modelAvailabilityCredentialsSubKeys) + len(modelAvailabilityExtraSubKeys)
	out := make([]service.Account, 0, 16)
	for rows.Next() {
		var acc service.Account
		subValues := make([]sql.NullString, subKeyCount)
		dest := make([]any, 0, 5+subKeyCount)
		dest = append(dest, &acc.ID, &acc.Platform, &acc.Type, &acc.Concurrency, &acc.Priority)
		for i := range subValues {
			dest = append(dest, &subValues[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}

		// 仅解码白名单子键：与 ListAccountCredentialSubset 相同的扫描策略，
		// jsonb->'key' 返回带类型的 JSON 文本，统一 json.Unmarshal 还原类型。
		credentials := make(map[string]any, len(modelAvailabilityCredentialsSubKeys))
		for i, key := range modelAvailabilityCredentialsSubKeys {
			if subValues[i].Valid {
				credentials[key] = decodeProjectedJSONValue(subValues[i])
			}
		}
		extra := make(map[string]any, len(modelAvailabilityExtraSubKeys))
		offset := len(modelAvailabilityCredentialsSubKeys)
		for i, key := range modelAvailabilityExtraSubKeys {
			if subValues[offset+i].Valid {
				extra[key] = decodeProjectedJSONValue(subValues[offset+i])
			}
		}
		acc.Credentials = credentials
		acc.Extra = extra
		out = append(out, acc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// decodeProjectedJSONValue 反序列化单个 jsonb 子键文本；解析失败时兜底为原样
// 文本（与 ListAccountCredentialSubset 行为一致）。
func decodeProjectedJSONValue(raw sql.NullString) any {
	var v any
	if err := json.Unmarshal([]byte(raw.String), &v); err == nil {
		return v
	}
	return raw.String
}

// ListModelAvailabilityCandidates returns the persistently enabled pool used
// only to classify a model miss. Runtime cooldown, overload, and expiry state
// must not turn a temporary capacity shortage into a permanent 404.
//
// 生产路径（已注入 SQL 执行器）走 30s TTL 缓存 + 投影查询：判别结论在 TTL 内
// 按 groupID 复用，且只加载判别所需字段，避免每请求全量解码候选账号的
// credentials JSONB（pprof 定位的 CPU 热点，见 .superpowers/sdd/modelcheck-fix-report.md）。
// 未注入 SQL 执行器的构造路径回退到原 ent 全量查询，行为与历史一致。
func (r *accountRepository) ListModelAvailabilityCandidates(ctx context.Context, groupID *int64, platforms []string, includeGrouped bool) ([]service.Account, error) {
	if len(platforms) == 0 {
		return []service.Account{}, nil
	}
	if r == nil || r.sql == nil {
		return r.listModelAvailabilityCandidatesEnt(ctx, groupID, platforms, includeGrouped)
	}

	key := modelAvailabilityCandidatesCacheKey(groupID, platforms, includeGrouped)
	if cached, ok := r.modelAvailabilityCache.get(key); ok {
		return cached, nil
	}
	v, err, _ := r.modelAvailabilityCache.sf.Do(key, func() (any, error) {
		accounts, err := r.queryModelAvailabilityCandidatesProjected(ctx, groupID, platforms, includeGrouped)
		if err != nil {
			// 查询失败不缓存：下个请求重试，行为与无缓存时一致。
			return nil, err
		}
		r.modelAvailabilityCache.set(key, accounts)
		return accounts, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]service.Account), nil
}

// listModelAvailabilityCandidatesEnt 是未注入 SQL 执行器时的回退实现，
// 与历史实现逐字一致（ent 全量查询 + accountsToService 水合，无缓存无投影）。
func (r *accountRepository) listModelAvailabilityCandidatesEnt(ctx context.Context, groupID *int64, platforms []string, includeGrouped bool) ([]service.Account, error) {
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
