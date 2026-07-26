//go:build unit

package repository

import (
	"context"
	"database/sql/driver"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestSafeDateFormat(t *testing.T) {
	tests := []struct {
		name        string
		granularity string
		expected    string
	}{
		// 合法值
		{"hour", "hour", "YYYY-MM-DD HH24:00"},
		{"day", "day", "YYYY-MM-DD"},
		{"week", "week", "IYYY-IW"},
		{"month", "month", "YYYY-MM"},

		// 非法值回退到默认
		{"空字符串", "", "YYYY-MM-DD"},
		{"未知粒度 year", "year", "YYYY-MM-DD"},
		{"未知粒度 minute", "minute", "YYYY-MM-DD"},

		// 恶意字符串
		{"SQL 注入尝试", "'; DROP TABLE users; --", "YYYY-MM-DD"},
		{"带引号", "day'", "YYYY-MM-DD"},
		{"带括号", "day)", "YYYY-MM-DD"},
		{"Unicode", "日", "YYYY-MM-DD"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := safeDateFormat(tc.granularity)
			require.Equal(t, tc.expected, got, "safeDateFormat(%q)", tc.granularity)
		})
	}
}

func TestBuildUsageLogBatchInsertQuery_UsesConflictDoNothing(t *testing.T) {
	log := &service.UsageLog{
		UserID:       1,
		APIKeyID:     2,
		AccountID:    usageLogAccountIDPointer(3),
		RequestID:    "req-batch-no-update",
		Model:        "gpt-5",
		InputTokens:  10,
		OutputTokens: 5,
		TotalCost:    1.2,
		ActualCost:   1.2,
		CreatedAt:    time.Now().UTC(),
	}
	prepared, err := prepareUsageLogInsert(log)
	require.NoError(t, err)

	query, _, err := buildUsageLogBatchInsertQuery([]string{usageLogBatchKey(log.RequestID, log.APIKeyID)}, map[string]usageLogInsertPrepared{
		usageLogBatchKey(log.RequestID, log.APIKeyID): prepared,
	})
	require.NoError(t, err)

	require.Contains(t, query, "ON CONFLICT (request_id, api_key_id) DO NOTHING")
	require.NotContains(t, strings.ToUpper(query), "DO UPDATE")
}

func TestUsageLogScanAccountIDNullable(t *testing.T) {
	for _, tc := range []struct {
		name      string
		accountID any
		want      *int64
	}{
		{name: "missing", accountID: nil},
		{name: "present", accountID: int64(123), want: usageLogAccountIDPointer(123)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })

			row := usageLogSQLMockRow(tc.accountID)
			mock.ExpectQuery("SELECT ").WillReturnRows(row)

			rows, err := db.QueryContext(context.Background(), "SELECT "+usageLogSelectColumns)
			require.NoError(t, err)
			require.True(t, rows.Next())

			log, err := scanUsageLog(rows)
			require.NoError(t, err)
			require.Equal(t, tc.want, log.AccountID)
			require.NoError(t, rows.Close())
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestCollectUsageLogIDsSkipsMissingAndNonpositiveAccountIDs(t *testing.T) {
	logs := []service.UsageLog{
		{AccountID: nil},
		{AccountID: usageLogAccountIDPointer(0)},
		{AccountID: usageLogAccountIDPointer(-1)},
		{AccountID: usageLogAccountIDPointer(123)},
	}

	ids := collectUsageLogIDs(logs)

	require.ElementsMatch(t, []int64{123}, ids.accountIDs)
	require.NotContains(t, ids.accountIDs, int64(0))
}

func TestHydrateUsageLogAccountsSkipsMissingAndNonpositiveAccountIDs(t *testing.T) {
	logs := []service.UsageLog{
		{AccountID: nil},
		{AccountID: usageLogAccountIDPointer(0)},
		{AccountID: usageLogAccountIDPointer(-1)},
		{AccountID: usageLogAccountIDPointer(123)},
	}

	hydrateUsageLogAccounts(logs, map[int64]*service.Account{
		0:   {ID: 0},
		-1:  {ID: -1},
		123: {ID: 123},
	})

	require.Nil(t, logs[0].Account)
	require.Nil(t, logs[1].Account)
	require.Nil(t, logs[2].Account)
	require.NotNil(t, logs[3].Account)
	require.Equal(t, int64(123), logs[3].Account.ID)
}

func usageLogSQLMockRow(accountID any) *sqlmock.Rows {
	columns := strings.Split(usageLogSelectColumns, ", ")
	values := make([]driver.Value, len(columns))
	values[0] = int64(1)
	values[1] = int64(2)
	values[2] = int64(3)
	values[3] = accountID
	values[5] = "gpt-5"
	for _, index := range []int{9, 10, 11, 12, 13, 14, 15, 16, 26, 27, 32, 34} {
		values[index] = int64(0)
	}
	for _, index := range []int{17, 18, 19, 20, 21, 22, 23, 24} {
		values[index] = float64(0)
	}
	for _, index := range []int{28, 29, 44} {
		values[index] = false
	}
	values[50] = time.Now().UTC()
	return sqlmock.NewRows(columns).AddRow(values...)
}
