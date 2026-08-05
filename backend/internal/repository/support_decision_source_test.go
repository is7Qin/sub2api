//go:build unit

package repository

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type supportDecisionQueryCapture struct {
	mu      sync.Mutex
	queries []string
}

func (m *supportDecisionQueryCapture) Match(_, actual string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queries = append(m.queries, normalizeSQLWhitespace(actual))
	return nil
}

func (m *supportDecisionQueryCapture) all() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.queries...)
}

func newSupportDecisionSourceTestDB(t *testing.T) (*supportDecisionSource, sqlmock.Sqlmock, *supportDecisionQueryCapture) {
	t.Helper()
	capture := &supportDecisionQueryCapture{}
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(capture))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return &supportDecisionSource{db: db}, mock, capture
}

func supportDecisionAccountRows() *sqlmock.Rows {
	columns := []string{"id", "platform", "type"}
	columns = append(columns, supportDecisionCredentialsSubKeys...)
	columns = append(columns, supportDecisionExtraSubKeys...)
	return sqlmock.NewRows(columns)
}

func addSupportDecisionAccountRow(rows *sqlmock.Rows) *sqlmock.Rows {
	return rows.AddRow(
		int64(7), service.PlatformOpenAI, service.AccountTypeOAuth,
		`{"gpt-5.4-high":"gpt-5.4-high"}`, nil, `["chat_completions"]`, nil, nil,
		`"training_off"`, nil, nil, `"auto"`, nil, nil, `"managed_session"`, nil, nil, nil, nil,
	)
}

func expectCompleteSupportDecisionSnapshot(mock sqlmock.Sqlmock, accounts *sqlmock.Rows) {
	mock.ExpectBegin()
	mock.ExpectQuery("accounts").WillReturnRows(accounts)
	mock.ExpectQuery("memberships").WillReturnRows(sqlmock.NewRows([]string{"account_id", "group_id"}).AddRow(int64(7), int64(42)))
	mock.ExpectQuery("groups").WillReturnRows(sqlmock.NewRows([]string{"id", "platform", "require_privacy_set", "models_list_config"}).AddRow(int64(42), service.PlatformOpenAI, true, `{"enabled":true,"models":["gpt-5.4-high"]}`))
	mock.ExpectQuery("channels").WillReturnRows(sqlmock.NewRows([]string{"id", "status", "model_mapping", "restrict_models", "billing_model_source", "group_ids", "pricing_models"}).AddRow(int64(5), service.StatusActive, `{"openai":{"alias":"gpt-5.4-high"}}`, true, service.BillingModelSourceRequested, `[42]`, `[{"platform":"openai","models":["gpt-5.4-high"]}]`))
	mock.ExpectCommit()
}

func TestSupportDecisionSourceUsesProjectedAccountColumns(t *testing.T) {
	source, mock, capture := newSupportDecisionSourceTestDB(t)
	expectCompleteSupportDecisionSnapshot(mock, supportDecisionAccountRows())

	_, err := source.Load(context.Background())
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())

	queries := capture.all()
	require.Len(t, queries, 4)
	selectClause, _, ok := strings.Cut(queries[0], " FROM ")
	require.True(t, ok)
	for _, item := range strings.Split(selectClause, ",") {
		item = strings.TrimSpace(item)
		require.NotEqual(t, "a.credentials", item)
		require.NotEqual(t, "a.extra", item)
	}
	for _, forbidden := range []string{
		"concurrency", "expires_at", "last_used_at", "rate_limited_at",
		"rate_limit_reset_at", "overload_until", "temp_unschedulable_until",
	} {
		require.NotContains(t, selectClause, forbidden)
	}
	require.Contains(t, selectClause, "a.id")
	require.Contains(t, selectClause, "a.platform")
	require.Contains(t, selectClause, "a.type")
}

func TestSupportDecisionSourceSelectsOnlyWhitelistedCredentialSubkeys(t *testing.T) {
	source, mock, capture := newSupportDecisionSourceTestDB(t)
	expectCompleteSupportDecisionSnapshot(mock, addSupportDecisionAccountRow(supportDecisionAccountRows()))

	snapshot, err := source.Load(context.Background())
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
	require.Len(t, snapshot.Accounts, 1)
	require.ElementsMatch(t, []string{"model_mapping", "openai_capabilities"}, mapKeys(snapshot.Accounts[0].Credentials))

	selectClause, _, ok := strings.Cut(capture.all()[0], " FROM ")
	require.True(t, ok)
	for _, key := range supportDecisionCredentialsSubKeys {
		require.Contains(t, selectClause, "credentials->'"+key+"'")
	}
	for _, sensitive := range []string{"access_token", "refresh_token", "id_token", "api_key", "session_key"} {
		require.NotContains(t, selectClause, sensitive)
	}
}

func TestSupportDecisionSourceSelectsOnlyWhitelistedExtraSubkeys(t *testing.T) {
	source, mock, capture := newSupportDecisionSourceTestDB(t)
	expectCompleteSupportDecisionSnapshot(mock, addSupportDecisionAccountRow(supportDecisionAccountRows()))

	snapshot, err := source.Load(context.Background())
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
	require.Len(t, snapshot.Accounts, 1)
	require.ElementsMatch(t, []string{"privacy_mode", "openai_compact_mode", "openai_oauth_ws_mode"}, mapKeys(snapshot.Accounts[0].Extra))

	selectClause, _, ok := strings.Cut(capture.all()[0], " FROM ")
	require.True(t, ok)
	for _, key := range supportDecisionExtraSubKeys {
		require.Contains(t, selectClause, "extra->'"+key+"'")
	}
	for _, unrelated := range []string{"openai_device_id", "openai_session_id", "codex_usage_updated_at", "model_rate_limits"} {
		require.NotContains(t, selectClause, unrelated)
	}
}

func TestSupportDecisionSourceLoadsMembershipsGroupsAndChannelsInOneSnapshot(t *testing.T) {
	source, mock, capture := newSupportDecisionSourceTestDB(t)
	expectCompleteSupportDecisionSnapshot(mock, addSupportDecisionAccountRow(supportDecisionAccountRows()))

	snapshot, err := source.Load(context.Background())
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
	require.Len(t, capture.all(), 4)
	require.Equal(t, []service.SupportDecisionMembership{{AccountID: 7, GroupID: 42}}, snapshot.Memberships)
	require.Equal(t, service.PlatformOpenAI, snapshot.Groups[0].Platform)
	require.True(t, snapshot.Groups[0].RequirePrivacySet)
	require.True(t, snapshot.Groups[0].ModelsListConfig.Enabled)
	require.Equal(t, []int64{42}, snapshot.Channels[0].GroupIDs)
	require.Equal(t, []string{"gpt-5.4-high"}, snapshot.Channels[0].PricingModels[0].Models)
}

func TestSupportDecisionSourceIgnoresTransientSchedulerState(t *testing.T) {
	source, mock, capture := newSupportDecisionSourceTestDB(t)
	expectCompleteSupportDecisionSnapshot(mock, addSupportDecisionAccountRow(supportDecisionAccountRows()))

	snapshot, err := source.Load(context.Background())
	require.NoError(t, err)
	require.Len(t, snapshot.Accounts, 1)
	query := capture.all()[0]
	require.Contains(t, query, "a.deleted_at IS NULL")
	require.Contains(t, query, "a.status = $1")
	require.Contains(t, query, "a.schedulable = TRUE")
	for _, transient := range []string{"expires_at", "last_used_at", "rate_limit", "overload", "temp_unschedulable", "concurrency"} {
		require.NotContains(t, query, transient)
	}
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSupportDecisionSourcePreservesModelSupportSemantics(t *testing.T) {
	source, mock, _ := newSupportDecisionSourceTestDB(t)
	rows := supportDecisionAccountRows()
	addSupportDecisionAccountRow(rows)
	rows.AddRow(int64(8), service.PlatformOpenAI, service.AccountTypeAPIKey,
		nil, nil, nil, nil, nil,
		nil, true, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	expectCompleteSupportDecisionSnapshot(mock, rows)

	snapshot, err := source.Load(context.Background())
	require.NoError(t, err)
	require.False(t, snapshot.Accounts[0].IsModelSupported("deepseek-v4"))
	require.True(t, snapshot.Accounts[0].IsModelSupported("gpt-5.4-high"))
	require.True(t, snapshot.Accounts[1].IsModelSupported("anything"))
	require.True(t, snapshot.Accounts[0].IsPrivacySet())
	require.True(t, snapshot.Accounts[0].SupportsOpenAIEndpointCapability(service.OpenAIEndpointCapabilityChatCompletions))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSupportDecisionSourcePreservesEmptyChannelMappingSemantics(t *testing.T) {
	source, mock, _ := newSupportDecisionSourceTestDB(t)
	mock.ExpectBegin()
	mock.ExpectQuery("accounts").WillReturnRows(supportDecisionAccountRows())
	mock.ExpectQuery("memberships").WillReturnRows(sqlmock.NewRows([]string{"account_id", "group_id"}))
	mock.ExpectQuery("groups").WillReturnRows(sqlmock.NewRows([]string{"id", "platform", "require_privacy_set", "models_list_config"}))
	mock.ExpectQuery("channels").WillReturnRows(sqlmock.NewRows([]string{"id", "status", "model_mapping", "restrict_models", "billing_model_source", "group_ids", "pricing_models"}).
		AddRow(int64(5), service.StatusActive, nil, false, service.BillingModelSourceRequested, `[]`, `[]`))
	mock.ExpectCommit()

	snapshot, err := source.Load(context.Background())
	require.NoError(t, err)
	require.Len(t, snapshot.Channels, 1)
	require.Empty(t, snapshot.Channels[0].ModelMapping)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSupportDecisionSourceRollsBackOnPartialReadFailure(t *testing.T) {
	source, mock, _ := newSupportDecisionSourceTestDB(t)
	mock.ExpectBegin()
	mock.ExpectQuery("accounts").WillReturnRows(addSupportDecisionAccountRow(supportDecisionAccountRows()))
	mock.ExpectQuery("memberships").WillReturnRows(sqlmock.NewRows([]string{"account_id", "group_id"}))
	mock.ExpectQuery("groups").WillReturnError(errors.New("group read failed"))
	mock.ExpectRollback()

	snapshot, err := source.Load(context.Background())
	require.ErrorContains(t, err, "load support decision groups")
	require.Nil(t, snapshot)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSupportDecisionSourceRollsBackOnAccountReadFailure(t *testing.T) {
	source, mock, _ := newSupportDecisionSourceTestDB(t)
	mock.ExpectBegin()
	mock.ExpectQuery("accounts").WillReturnError(errors.New("account read failed"))
	mock.ExpectRollback()

	snapshot, err := source.Load(context.Background())
	require.ErrorContains(t, err, "load support decision accounts")
	require.Nil(t, snapshot)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSupportDecisionSourceRollsBackOnMembershipReadFailure(t *testing.T) {
	source, mock, _ := newSupportDecisionSourceTestDB(t)
	mock.ExpectBegin()
	mock.ExpectQuery("accounts").WillReturnRows(supportDecisionAccountRows())
	mock.ExpectQuery("memberships").WillReturnError(errors.New("membership read failed"))
	mock.ExpectRollback()

	snapshot, err := source.Load(context.Background())
	require.ErrorContains(t, err, "load support decision memberships")
	require.Nil(t, snapshot)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSupportDecisionSourceRollsBackOnChannelReadFailure(t *testing.T) {
	source, mock, _ := newSupportDecisionSourceTestDB(t)
	mock.ExpectBegin()
	mock.ExpectQuery("accounts").WillReturnRows(supportDecisionAccountRows())
	mock.ExpectQuery("memberships").WillReturnRows(sqlmock.NewRows([]string{"account_id", "group_id"}))
	mock.ExpectQuery("groups").WillReturnRows(sqlmock.NewRows([]string{"id", "platform", "require_privacy_set", "models_list_config"}))
	mock.ExpectQuery("channels").WillReturnError(errors.New("channel read failed"))
	mock.ExpectRollback()

	snapshot, err := source.Load(context.Background())
	require.ErrorContains(t, err, "load support decision channels")
	require.Nil(t, snapshot)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSupportDecisionSourceRollsBackOnCommitFailure(t *testing.T) {
	source, mock, _ := newSupportDecisionSourceTestDB(t)
	mock.ExpectBegin()
	mock.ExpectQuery("accounts").WillReturnRows(supportDecisionAccountRows())
	mock.ExpectQuery("memberships").WillReturnRows(sqlmock.NewRows([]string{"account_id", "group_id"}))
	mock.ExpectQuery("groups").WillReturnRows(sqlmock.NewRows([]string{"id", "platform", "require_privacy_set", "models_list_config"}))
	mock.ExpectQuery("channels").WillReturnRows(sqlmock.NewRows([]string{"id", "status", "model_mapping", "restrict_models", "billing_model_source", "group_ids", "pricing_models"}))
	mock.ExpectCommit().WillReturnError(errors.New("commit failed"))

	snapshot, err := source.Load(context.Background())
	require.ErrorContains(t, err, "commit support decision snapshot")
	require.Nil(t, snapshot)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSupportDecisionSourceIsNotExposedThroughRequestRepository(t *testing.T) {
	requestRepo := reflect.TypeOf((*service.AccountRepository)(nil)).Elem()
	_, exposed := requestRepo.MethodByName("Load")
	require.False(t, exposed)
	_, exposed = requestRepo.MethodByName("LoadSupportDecisionSnapshot")
	require.False(t, exposed)

	candidateRepo := reflect.TypeOf((*service.ModelAvailabilityCandidateRepository)(nil)).Elem()
	_, retained := candidateRepo.MethodByName("ListModelAvailabilityCandidates")
	require.True(t, retained)

	var _ service.SupportDecisionSource = (*supportDecisionSource)(nil)
}

func mapKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}
