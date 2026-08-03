//go:build unit

package repository

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
)

// countingQueryMatcher 与 captureProjectionQueryMatcher 行为一致（匹配任意查询），
// 额外记录实际执行的查询次数，用于断言缓存命中 / singleflight 场景下 DB 只被查询一次。
type countingQueryMatcher struct {
	mu    sync.Mutex
	count int
	sql   *string
}

func (m *countingQueryMatcher) Match(_, actual string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.count++
	if m.sql != nil {
		*m.sql = actual
	}
	return nil
}

func (m *countingQueryMatcher) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.count
}

func newModelAvailabilityCandidateRepo(t *testing.T, counter *countingQueryMatcher) (*accountRepository, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(counter))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	driver := entsql.OpenDB(dialect.Postgres, db)
	client := dbent.NewClient(dbent.Driver(driver))
	t.Cleanup(func() { _ = client.Close() })
	return newAccountRepositoryWithSQL(client, db, nil), mock
}

// modelAvailabilityCandidateRow 声明投影查询的固定列序（与实现 SELECT 列表一一对应）。
func modelAvailabilityCandidateRow() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "platform", "type", "concurrency", "priority",
		"model_mapping", "compact_model_mapping", "openai_capabilities",
		"aws_region", "aws_force_global",
		"privacy_mode", "openai_passthrough", "mixed_scheduling",
		"openai_compact_mode", "openai_compact_supported",
		"openai_ws_force_http", "openai_oauth_ws_mode",
		"openai_apikey_responses_websockets_v2_mode",
		"openai_apikey_responses_websockets_v2_enabled",
		"responses_websockets_v2_enabled", "openai_ws_enabled",
	})
}

// addOAuthCandidateRow 追加一个典型的 OpenAI OAuth 候选行：显式 model_mapping、
// openai_capabilities、privacy_mode、WS OAuth 模式等子键齐全。
func addOAuthCandidateRow(rows *sqlmock.Rows) *sqlmock.Rows {
	return rows.AddRow(
		int64(7), "openai", "oauth", 3, 100,
		`{"gpt-4o":"gpt-4o","codex-mini-latest":"codex-mini-latest"}`, nil,
		`["chat_completions"]`, nil, nil,
		`"training_off"`, nil, nil,
		`"auto"`, nil,
		nil, `"managed_session"`, nil,
		nil, nil, nil,
	)
}

// TestListModelAvailabilityCandidates_CacheHitSkipsSecondQuery 验证 TTL 内相同
// groupID 的第二次调用直接命中缓存，DB 查询次数必须为 1。
func TestListModelAvailabilityCandidates_CacheHitSkipsSecondQuery(t *testing.T) {
	counter := &countingQueryMatcher{}
	repo, mock := newModelAvailabilityCandidateRepo(t, counter)

	mock.ExpectQuery("model availability candidates").
		WillReturnRows(addOAuthCandidateRow(modelAvailabilityCandidateRow()))

	groupID := int64(42)
	first, err := repo.ListModelAvailabilityCandidates(context.Background(), &groupID, []string{service.PlatformOpenAI}, false)
	require.NoError(t, err)
	require.Len(t, first, 1)
	require.Equal(t, int64(7), first[0].ID)

	// 相同 groupID + 相同参数在 TTL 内再次调用，应命中缓存，不再查询 DB。
	second, err := repo.ListModelAvailabilityCandidates(context.Background(), &groupID, []string{service.PlatformOpenAI}, false)
	require.NoError(t, err)
	require.Equal(t, first, second)
	require.Equal(t, 1, counter.Count(), "第二次调用应命中缓存，DB 查询次数必须为 1")
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestListModelAvailabilityCandidates_CacheExpiresAfterTTL 验证 TTL 到期后重新查询。
func TestListModelAvailabilityCandidates_CacheExpiresAfterTTL(t *testing.T) {
	counter := &countingQueryMatcher{}
	repo, mock := newModelAvailabilityCandidateRepo(t, counter)

	// 注入可控时钟，模拟 30s TTL 到期。
	fakeNow := time.Unix(1_700_000_000, 0)
	repo.modelAvailabilityCache.now = func() time.Time { return fakeNow }

	mock.ExpectQuery("model availability candidates").
		WillReturnRows(addOAuthCandidateRow(modelAvailabilityCandidateRow()))
	rows := modelAvailabilityCandidateRow()
	rows.AddRow(int64(9), "openai", "api_key", 2, 80, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	mock.ExpectQuery("model availability candidates").WillReturnRows(rows)

	groupID := int64(42)
	first, err := repo.ListModelAvailabilityCandidates(context.Background(), &groupID, []string{service.PlatformOpenAI}, false)
	require.NoError(t, err)
	require.Len(t, first, 1)

	fakeNow = fakeNow.Add(modelAvailabilityCandidatesCacheTTL + time.Second)
	second, err := repo.ListModelAvailabilityCandidates(context.Background(), &groupID, []string{service.PlatformOpenAI}, false)
	require.NoError(t, err)
	require.Len(t, second, 1)
	require.Equal(t, int64(9), second[0].ID, "TTL 到期后应重新查询并看到最新数据")
	require.Equal(t, 2, counter.Count())
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestListModelAvailabilityCandidates_ProjectionDecodesOnlySubset 验证投影解码
// 形状：凭据只包含判别所需子键，敏感凭据（access_token 等）不进内存。
func TestListModelAvailabilityCandidates_ProjectionDecodesOnlySubset(t *testing.T) {
	counter := &countingQueryMatcher{}
	repo, mock := newModelAvailabilityCandidateRepo(t, counter)

	mock.ExpectQuery("model availability candidates").
		WillReturnRows(addOAuthCandidateRow(modelAvailabilityCandidateRow()))

	groupID := int64(42)
	accounts, err := repo.ListModelAvailabilityCandidates(context.Background(), &groupID, []string{service.PlatformOpenAI}, false)
	require.NoError(t, err)
	require.Len(t, accounts, 1)
	acc := accounts[0]
	require.Equal(t, int64(7), acc.ID)
	require.Equal(t, "openai", acc.Platform)
	require.Equal(t, "oauth", acc.Type)
	require.Equal(t, 3, acc.Concurrency)

	require.Equal(t, map[string]any{"gpt-4o": "gpt-4o", "codex-mini-latest": "codex-mini-latest"}, acc.Credentials["model_mapping"])
	require.Equal(t, []any{"chat_completions"}, acc.Credentials["openai_capabilities"])
	for _, sensitive := range []string{"access_token", "refresh_token", "id_token", "api_key", "session_key", "personal_access_token", "user_agent"} {
		_, ok := acc.Credentials[sensitive]
		require.False(t, ok, "投影结果不得包含敏感凭据字段 %s", sensitive)
	}

	require.Equal(t, "training_off", acc.Extra["privacy_mode"])
	for _, unrelated := range []string{"openai_device_id", "openai_session_id", "codex_usage_updated_at", "model_rate_limits"} {
		_, ok := acc.Extra[unrelated]
		require.False(t, ok, "投影结果不得包含无关 extra 字段 %s", unrelated)
	}
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestListModelAvailabilityCandidates_ProjectedSQLSelectsSubKeysOnly 验证 SQL
// 投影：SELECT 列表只含判别所需列与 credentials/extra 的 JSONB 子键，
// 不选择裸的 credentials/extra 全量列。
func TestListModelAvailabilityCandidates_ProjectedSQLSelectsSubKeysOnly(t *testing.T) {
	// 分组路径
	{
		var captured string
		counter := &countingQueryMatcher{sql: &captured}
		repo, mock := newModelAvailabilityCandidateRepo(t, counter)
		mock.ExpectQuery("model availability candidates").
			WillReturnRows(addOAuthCandidateRow(modelAvailabilityCandidateRow()))
		groupID := int64(42)
		_, err := repo.ListModelAvailabilityCandidates(context.Background(), &groupID, []string{service.PlatformOpenAI}, false)
		require.NoError(t, err)
		require.NoError(t, mock.ExpectationsWereMet())

		normalized := normalizeSQLWhitespace(captured)
		assertModelAvailabilityProjectionSelect(t, normalized)
		require.Contains(t, normalized, "group_id")
	}

	// 未分组路径
	{
		var captured string
		counter := &countingQueryMatcher{sql: &captured}
		repo, mock := newModelAvailabilityCandidateRepo(t, counter)
		mock.ExpectQuery("model availability candidates").
			WillReturnRows(modelAvailabilityCandidateRow())
		_, err := repo.ListModelAvailabilityCandidates(context.Background(), nil, []string{service.PlatformOpenAI}, false)
		require.NoError(t, err)
		require.NoError(t, mock.ExpectationsWereMet())

		normalized := normalizeSQLWhitespace(captured)
		assertModelAvailabilityProjectionSelect(t, normalized)
		require.Contains(t, normalized, "NOT EXISTS")
	}

	// 未分组路径 + includeGrouped=true（简单模式全量池判别）：不排除已绑定分组的账号。
	{
		var captured string
		counter := &countingQueryMatcher{sql: &captured}
		repo, mock := newModelAvailabilityCandidateRepo(t, counter)
		mock.ExpectQuery("model availability candidates").
			WillReturnRows(modelAvailabilityCandidateRow())
		_, err := repo.ListModelAvailabilityCandidates(context.Background(), nil, []string{service.PlatformOpenAI}, true)
		require.NoError(t, err)
		require.NoError(t, mock.ExpectationsWereMet())

		normalized := normalizeSQLWhitespace(captured)
		assertModelAvailabilityProjectionSelect(t, normalized)
		require.NotContains(t, normalized, "NOT EXISTS", "includeGrouped=true 时不得排除已绑定分组的账号")
	}
}

// assertModelAvailabilityProjectionSelect 断言 SELECT 列表不含裸的 credentials/extra
// 列，且包含判别所需的 JSONB 子键投影。
func assertModelAvailabilityProjectionSelect(t *testing.T, normalized string) {
	t.Helper()
	selectClause, _, found := strings.Cut(normalized, " FROM ")
	require.True(t, found, "unexpected SQL: %s", normalized)
	for _, item := range strings.Split(selectClause, ",") {
		item = strings.TrimSpace(item)
		for _, forbidden := range []string{"credentials", "extra", "a.credentials", "a.extra"} {
			require.NotEqual(t, forbidden, item, "禁止全量选择 %s 列: %s", forbidden, normalized)
		}
	}
	require.Contains(t, selectClause, "credentials->'model_mapping'")
	require.Contains(t, selectClause, "credentials->'compact_model_mapping'")
	require.Contains(t, selectClause, "credentials->'openai_capabilities'")
	require.Contains(t, selectClause, "extra->'privacy_mode'")
	require.Contains(t, selectClause, "extra->'openai_passthrough'")
}

// TestListModelAvailabilityCandidates_ProjectedAccountsPreserveSupportSemantics
// 验证投影结果与原全量账号在 IsModelSupported 判别上语义一致：
// mapping 白名单与 passthrough fail-open 行为不变。
func TestListModelAvailabilityCandidates_ProjectedAccountsPreserveSupportSemantics(t *testing.T) {
	counter := &countingQueryMatcher{}
	repo, mock := newModelAvailabilityCandidateRepo(t, counter)

	rows := modelAvailabilityCandidateRow()
	// OAuth：显式 model_mapping 决定支持集。
	rows.AddRow(int64(7), "openai", "oauth", 3, 100,
		`{"gpt-5.4-high":"gpt-5.4-high"}`, nil, nil, nil, nil,
		`"training_off"`, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	// APIKey：openai_passthrough=true → 任何模型都支持。
	rows.AddRow(int64(9), "openai", "api_key", 2, 80,
		nil, nil, nil, nil, nil,
		nil, true, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	mock.ExpectQuery("model availability candidates").WillReturnRows(rows)

	groupID := int64(42)
	accounts, err := repo.ListModelAvailabilityCandidates(context.Background(), &groupID, []string{service.PlatformOpenAI}, false)
	require.NoError(t, err)
	require.Len(t, accounts, 2)

	require.True(t, accounts[0].IsModelSupported("gpt-5.4-high"), "mapping 内模型应受支持")
	require.False(t, accounts[0].IsModelSupported("deepseek-v4"), "mapping 外模型应不受支持")
	require.True(t, accounts[1].IsModelSupported("anything-else"), "passthrough 账号应支持任意模型")
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestListModelAvailabilityCandidates_ConcurrentMissQueriesOnce 验证 TTL 到期后的
// 并发 miss 被 singleflight 合并为一次 DB 查询。
func TestListModelAvailabilityCandidates_ConcurrentMissQueriesOnce(t *testing.T) {
	counter := &countingQueryMatcher{}
	repo, mock := newModelAvailabilityCandidateRepo(t, counter)

	mock.ExpectQuery("model availability candidates").
		WillReturnRows(addOAuthCandidateRow(modelAvailabilityCandidateRow()))

	groupID := int64(42)
	var wg sync.WaitGroup
	results := make([][]service.Account, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = repo.ListModelAvailabilityCandidates(context.Background(), &groupID, []string{service.PlatformOpenAI}, false)
		}(i)
	}
	wg.Wait()
	for i := 0; i < 2; i++ {
		require.NoError(t, errs[i])
		require.Len(t, results[i], 1)
	}
	require.Equal(t, 1, counter.Count(), "并发 miss 应被 singleflight 合并为一次查询")
	require.NoError(t, mock.ExpectationsWereMet())
}
