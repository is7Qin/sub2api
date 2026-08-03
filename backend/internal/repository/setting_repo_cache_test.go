//go:build unit

package repository

import (
	"context"
	"database/sql"
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

func newSettingRepoWithSQLMock(t *testing.T) (*settingRepository, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	driver := entsql.OpenDB(dialect.Postgres, db)
	client := dbent.NewClient(dbent.Driver(driver))
	t.Cleanup(func() { _ = client.Close() })
	return &settingRepository{client: client}, mock
}

func expectSettingSelect(mock sqlmock.Sqlmock, key, value string) {
	mock.ExpectQuery(`(?s)SELECT .* FROM "settings" WHERE "settings"."key" = \$1`).
		WithArgs(key).
		WillReturnRows(sqlmock.NewRows([]string{"id", "key", "value", "updated_at"}).
			AddRow(int64(1), key, value, time.Now()))
}

func expectSettingSelectNotFound(mock sqlmock.Sqlmock, key string) {
	mock.ExpectQuery(`(?s)SELECT .* FROM "settings" WHERE "settings"."key" = \$1`).
		WithArgs(key).
		WillReturnRows(sqlmock.NewRows([]string{"id", "key", "value", "updated_at"}))
}

func expectSettingUpsert(mock sqlmock.Sqlmock, key, value string) {
	// ent Create().Exec() 带 RETURNING "id"，实际走 Query 通道；
	// 单条 upsert 列序为 (key, value, updated_at)。
	mock.ExpectQuery(`(?s)INSERT INTO "settings".*ON CONFLICT .* DO UPDATE`).
		WithArgs(key, value, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(1)))
}

func expectSettingBulkUpsert(mock sqlmock.Sqlmock) {
	// 批量 upsert 的键序取决于 map 迭代序，参数只做数量断言。
	mock.ExpectQuery(`(?s)INSERT INTO "settings".*ON CONFLICT .* DO UPDATE`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(1)).AddRow(int64(2)))
}

func expectSettingDelete(mock sqlmock.Sqlmock, key string) {
	mock.ExpectExec(`(?s)DELETE FROM "settings" WHERE "settings"."key" = \$1`).
		WithArgs(key).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func TestSettingRepository_GetValueCacheHitDoesNotQueryDB(t *testing.T) {
	repo, mock := newSettingRepoWithSQLMock(t)

	// 首次读取：一次 DB 查询并填充缓存。
	expectSettingSelect(mock, "allow_ungrouped_key", "true")
	value, err := repo.GetValue(context.Background(), "allow_ungrouped_key")
	require.NoError(t, err)
	require.Equal(t, "true", value)

	// 缓存命中：不再发出任何查询（sqlmock 无剩余 expectation，多出的查询会失败）。
	value, err = repo.GetValue(context.Background(), "allow_ungrouped_key")
	require.NoError(t, err)
	require.Equal(t, "true", value)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSettingRepository_GetValueCachesNotFound(t *testing.T) {
	repo, mock := newSettingRepoWithSQLMock(t)

	expectSettingSelectNotFound(mock, "missing_key")
	_, err := repo.GetValue(context.Background(), "missing_key")
	require.ErrorIs(t, err, service.ErrSettingNotFound)

	// 负缓存命中：直接返回 ErrSettingNotFound，不再查库。
	_, err = repo.GetValue(context.Background(), "missing_key")
	require.ErrorIs(t, err, service.ErrSettingNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSettingRepository_GetAllCachedUntilWrite(t *testing.T) {
	repo, mock := newSettingRepoWithSQLMock(t)

	mock.ExpectQuery(`(?s)SELECT .* FROM "settings"`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "key", "value", "updated_at"}).
			AddRow(int64(1), "k1", "v1", time.Now()).
			AddRow(int64(2), "k2", "v2", time.Now()))
	all, err := repo.GetAll(context.Background())
	require.NoError(t, err)
	require.Equal(t, map[string]string{"k1": "v1", "k2": "v2"}, all)

	// 第二次 GetAll 命中缓存，不查库。
	all, err = repo.GetAll(context.Background())
	require.NoError(t, err)
	require.Equal(t, map[string]string{"k1": "v1", "k2": "v2"}, all)

	// 写入后失效：再次查库。
	expectSettingUpsert(mock, "k3", "v3")
	require.NoError(t, repo.Set(context.Background(), "k3", "v3"))
	mock.ExpectQuery(`(?s)SELECT .* FROM "settings"`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "key", "value", "updated_at"}).
			AddRow(int64(1), "k1", "v1", time.Now()).
			AddRow(int64(3), "k3", "v3", time.Now()))
	all, err = repo.GetAll(context.Background())
	require.NoError(t, err)
	require.Equal(t, map[string]string{"k1": "v1", "k3": "v3"}, all)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSettingRepository_SetInvalidatesKey(t *testing.T) {
	repo, mock := newSettingRepoWithSQLMock(t)

	expectSettingSelect(mock, "site_name", "Sub2API")
	value, err := repo.GetValue(context.Background(), "site_name")
	require.NoError(t, err)
	require.Equal(t, "Sub2API", value)

	expectSettingUpsert(mock, "site_name", "NewName")
	require.NoError(t, repo.Set(context.Background(), "site_name", "NewName"))

	// 写入后缓存已失效：重新查库。
	expectSettingSelect(mock, "site_name", "NewName")
	value, err = repo.GetValue(context.Background(), "site_name")
	require.NoError(t, err)
	require.Equal(t, "NewName", value)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSettingRepository_SetMultipleInvalidatesKeysAndAll(t *testing.T) {
	repo, mock := newSettingRepoWithSQLMock(t)

	expectSettingSelect(mock, "promo_enabled", "true")
	value, err := repo.GetValue(context.Background(), "promo_enabled")
	require.NoError(t, err)
	require.Equal(t, "true", value)

	expectSettingBulkUpsert(mock)
	require.NoError(t, repo.SetMultiple(context.Background(), map[string]string{"promo_enabled": "false", "new_key": "x"}))

	// promo_enabled 缓存被失效：重新查库。
	expectSettingSelect(mock, "promo_enabled", "false")
	value, err = repo.GetValue(context.Background(), "promo_enabled")
	require.NoError(t, err)
	require.Equal(t, "false", value)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSettingRepository_DeleteInvalidatesKey(t *testing.T) {
	repo, mock := newSettingRepoWithSQLMock(t)

	expectSettingSelect(mock, "site_logo", "/logo.png")
	value, err := repo.GetValue(context.Background(), "site_logo")
	require.NoError(t, err)
	require.Equal(t, "/logo.png", value)

	expectSettingDelete(mock, "site_logo")
	require.NoError(t, repo.Delete(context.Background(), "site_logo"))

	// 删除后缓存失效：重新查库 → not found。
	expectSettingSelectNotFound(mock, "site_logo")
	_, err = repo.GetValue(context.Background(), "site_logo")
	require.ErrorIs(t, err, service.ErrSettingNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSettingRepository_GetMultipleFetchesOnlyMissingKeys(t *testing.T) {
	repo, mock := newSettingRepoWithSQLMock(t)

	// 先缓存 a。
	expectSettingSelect(mock, "a", "1")
	value, err := repo.GetValue(context.Background(), "a")
	require.NoError(t, err)
	require.Equal(t, "1", value)

	// GetMultiple(a, b)：只查缺失的 b。
	mock.ExpectQuery(`(?s)SELECT .* FROM "settings" WHERE "settings"."key" IN`).
		WithArgs("b").
		WillReturnRows(sqlmock.NewRows([]string{"id", "key", "value", "updated_at"}).
			AddRow(int64(2), "b", "2", time.Now()))
	values, err := repo.GetMultiple(context.Background(), []string{"a", "b"})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"a": "1", "b": "2"}, values)

	// 第二次：全部命中缓存，零查询。
	values, err = repo.GetMultiple(context.Background(), []string{"a", "b"})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"a": "1", "b": "2"}, values)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSettingRepository_GetValueFailOpenOnDBError(t *testing.T) {
	repo, mock := newSettingRepoWithSQLMock(t)

	mock.ExpectQuery(`(?s)SELECT .* FROM "settings" WHERE "settings"."key" = \$1`).
		WithArgs("broken").
		WillReturnError(sql.ErrConnDone)
	_, err := repo.GetValue(context.Background(), "broken")
	require.ErrorIs(t, err, sql.ErrConnDone)

	// 错误不落缓存：下一次仍走 DB（fail-open）。
	mock.ExpectQuery(`(?s)SELECT .* FROM "settings" WHERE "settings"."key" = \$1`).
		WithArgs("broken").
		WillReturnRows(sqlmock.NewRows([]string{"id", "key", "value", "updated_at"}).
			AddRow(int64(1), "broken", "v", time.Now()))
	value, err := repo.GetValue(context.Background(), "broken")
	require.NoError(t, err)
	require.Equal(t, "v", value)
	require.NoError(t, mock.ExpectationsWereMet())
}
