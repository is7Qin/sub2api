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

// modelAvailabilityCandidatesQueryTimeout 是 leader 查询脱离调用方取消后的独立
// 执行上限：调用方无 deadline 时查询不能无限执行（等待者共享 leader 的阻塞）。
const modelAvailabilityCandidatesQueryTimeout = 30 * time.Second

// modelAvailabilityCandidateCache 按 groupID（含未分组）缓存判别候选账号，
// TTL 到期后由 singleflight 合并并发 miss，避免缓存击穿。
type modelAvailabilityCandidateCache struct {
	mu      sync.Mutex
	entries map[modelAvailabilityCandidateCacheKey]modelAvailabilityCandidateEntry
	now     func() time.Time // 可注入时钟，便于测试 TTL 行为
	sf      singleflight.Group
}

type modelAvailabilityCandidateEntry struct {
	accounts  []service.Account
	expiresAt time.Time
}

func newModelAvailabilityCandidateCache() *modelAvailabilityCandidateCache {
	return &modelAvailabilityCandidateCache{
		entries: make(map[modelAvailabilityCandidateCacheKey]modelAvailabilityCandidateEntry),
		now:     time.Now,
	}
}

// modelAvailabilityCandidateCacheKey 是零分配的缓存键：固定数组按字典序存放
// 平台名（只复制字符串头部，不复制内容），配合 groupID / includeGrouped 参与
// 相等比较。语义与旧的字符串键逐项对齐：groupID 区分 nil 与 0、平台列表顺序
// 无关。缓存 GET 是全请求热路径，构建键不得产生分配；生产调用方最多传 2 个
// 平台（isPureModelSupportMiss / isPureOpenAIModelSupportMiss），超过固定容量
// 时退化为排序拼接字符串（仅正确性兜底，非热路径）。
type modelAvailabilityCandidateCacheKey struct {
	groupID        int64
	hasGroupID     bool
	includeGrouped bool
	platformCount  int
	platforms      [8]string
	// overflow 仅在平台数超过 platforms 容量时非空，持有排序拼接后的平台串；
	// 此时 platformCount 恒为 -1，正常键与溢出键永不相等。
	overflow string
}

func makeModelAvailabilityCandidateCacheKey(groupID *int64, platforms []string, includeGrouped bool) modelAvailabilityCandidateCacheKey {
	var k modelAvailabilityCandidateCacheKey
	if groupID != nil {
		k.hasGroupID = true
		k.groupID = *groupID
	}
	k.includeGrouped = includeGrouped
	if len(platforms) > len(k.platforms) {
		sorted := append([]string(nil), platforms...)
		sort.Strings(sorted)
		k.platformCount = -1
		k.overflow = strings.Join(sorted, ",")
		return k
	}
	for _, p := range platforms {
		// 按字典序插入，保持与平台列表顺序无关（与旧字符串键一致）。
		i := k.platformCount
		for i > 0 && k.platforms[i-1] > p {
			k.platforms[i] = k.platforms[i-1]
			i--
		}
		k.platforms[i] = p
		k.platformCount++
	}
	return k
}

// modelAvailabilityCandidatesSFKey 由 struct 键生成 singleflight 去重键。
// 仅 miss 路径使用（miss 稀少，字符串构建的分配可接受）；缓存命中路径
// 始终走零分配的 struct 键。
func modelAvailabilityCandidatesSFKey(k modelAvailabilityCandidateCacheKey) string {
	groupPart := "ungrouped"
	if k.hasGroupID {
		groupPart = "group:" + strconv.FormatInt(k.groupID, 10)
	}
	platforms := k.overflow
	if k.platformCount >= 0 {
		platforms = strings.Join(k.platforms[:k.platformCount], ",")
	}
	return groupPart + "|" + platforms + "|" + strconv.FormatBool(k.includeGrouped)
}

func (c *modelAvailabilityCandidateCache) get(key modelAvailabilityCandidateCacheKey) ([]service.Account, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || c.now().After(e.expiresAt) {
		return nil, false
	}
	// 直接返回缓存条目：model_mapping memo 已改为 atomic.Value 原子发布
	// （见 service.Account.GetModelMapping），共享结构体并发读写无数据竞争，
	// 无需再为每次请求克隆切片（克隆曾是 pprof 定位的 5.17GB 分配热点）。
	return e.accounts, true
}

func (c *modelAvailabilityCandidateCache) set(key modelAvailabilityCandidateCacheKey, accounts []service.Account) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = modelAvailabilityCandidateEntry{
		accounts:  accounts,
		expiresAt: c.now().Add(modelAvailabilityCandidatesCacheTTL),
	}
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

// The background support source and the retained request-path diagnostic cache
// consume exactly the same vetted subkeys. Keep aliases rather than duplicate
// allowlists so future support predicates cannot silently diverge.
var (
	supportDecisionCredentialsSubKeys = modelAvailabilityCredentialsSubKeys
	supportDecisionExtraSubKeys       = modelAvailabilityExtraSubKeys
)

func appendProjectedJSONColumns(cols []string, alias, column string, keys []string) []string {
	for _, key := range keys {
		cols = append(cols, alias+"."+column+"->'"+key+"'")
	}
	return cols
}

func decodeProjectedJSONSubKeys(values []sql.NullString, keys []string) map[string]any {
	projected := make(map[string]any, len(keys))
	for i, key := range keys {
		if values[i].Valid {
			projected[key] = decodeProjectedJSONValue(values[i])
		}
	}
	return projected
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
	cols = appendProjectedJSONColumns(cols, alias, "credentials", modelAvailabilityCredentialsSubKeys)
	cols = appendProjectedJSONColumns(cols, alias, "extra", modelAvailabilityExtraSubKeys)
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
		offset := len(modelAvailabilityCredentialsSubKeys)
		acc.Credentials = decodeProjectedJSONSubKeys(subValues[:offset], modelAvailabilityCredentialsSubKeys)
		acc.Extra = decodeProjectedJSONSubKeys(subValues[offset:], modelAvailabilityExtraSubKeys)
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

	key := makeModelAvailabilityCandidateCacheKey(groupID, platforms, includeGrouped)
	if cached, ok := r.modelAvailabilityCache.get(key); ok {
		return cached, nil
	}
	v, err, _ := r.modelAvailabilityCache.sf.Do(modelAvailabilityCandidatesSFKey(key), func() (any, error) {
		// leader 的查询脱离首个调用方的取消：singleflight 的等待者共享 leader
		// 的结果/错误，首个请求断连不应把整批一起拖垮；执行时长用独立上限
		// 兜底，防止 DB 挂起时 leader 无限等待（等待者同样被阻塞）。
		queryCtx := context.WithoutCancel(ctx)
		if deadline, ok := ctx.Deadline(); ok {
			queryCtx, _ = context.WithDeadline(queryCtx, deadline)
		} else {
			queryCtx, _ = context.WithTimeout(queryCtx, modelAvailabilityCandidatesQueryTimeout)
		}
		accounts, err := r.queryModelAvailabilityCandidatesProjected(queryCtx, groupID, platforms, includeGrouped)
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
	// singleflight 结果与缓存条目共享同一切片（见 get 的去克隆注释），
	// 并发调用方各自消费，memo 原子发布保证无数据竞争。
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
