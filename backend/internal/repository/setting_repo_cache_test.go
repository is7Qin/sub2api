//go:build unit

package repository

import (
	"context"
	"database/sql"
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

// blockingQueryDriver 包一层 dialect.Driver：匹配到的 SQL 先阻塞在
// readStarted/release 通道上，用于确定性构造"读与写并发"的竞态场景。
type blockingQueryDriver struct {
	dialect.Driver
	triggerSQL  string
	readStarted chan struct{}
	release     chan struct{}
	startOnce   sync.Once
	releaseOnce sync.Once
}

func (d *blockingQueryDriver) Query(ctx context.Context, query string, args, v any) error {
	if strings.Contains(query, d.triggerSQL) {
		d.startOnce.Do(func() { close(d.readStarted) })
		<-d.release
	}
	return d.Driver.Query(ctx, query, args, v)
}

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

// TestSettingRepository_StaleInFlightLoadDiscardedAfterSet 复现读失效竞态：
// 慢读（DB 查询进行中）与 Set 提交重叠时，若慢读把读到的旧值回填缓存，
// 下一次读取会命中旧值而非刚写入的新值。修复后回填必须被丢弃。
func TestSettingRepository_StaleInFlightLoadDiscardedAfterSet(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	// 慢读在 wrapper 处阻塞、未达 sqlmock，Set 的 INSERT 先到：乱序匹配。
	mock.MatchExpectationsInOrder(false)

	readStarted := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	blocking := &blockingQueryDriver{
		Driver:      entsql.OpenDB(dialect.Postgres, db),
		triggerSQL:  `FROM "settings" WHERE "settings"."key" = $1`,
		readStarted: readStarted,
		release:     release,
	}
	client := dbent.NewClient(dbent.Driver(blocking))
	t.Cleanup(func() { _ = client.Close() })
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	repo := &settingRepository{client: client}

	// 慢读先发起旧值查询并阻塞；Set 在慢读返回前提交新值并失效缓存。
	expectSettingSelect(mock, "feature_x", "old")
	expectSettingUpsert(mock, "feature_x", "new")

	slowResult := make(chan error, 1)
	go func() {
		_, err := repo.GetValue(context.Background(), "feature_x")
		slowResult <- err
	}()

	<-readStarted // 慢读已进入 DB 查询
	require.NoError(t, repo.Set(context.Background(), "feature_x", "new"))

	// 放行慢读：它此刻只能读到旧值，回填动作必须因版本推进而被丢弃。
	releaseOnce.Do(func() { close(release) })
	require.NoError(t, <-slowResult)

	// 缓存未被旧值污染：下一次读取仍走 DB 并得到新值。
	expectSettingSelect(mock, "feature_x", "new")
	value, err := repo.GetValue(context.Background(), "feature_x")
	require.NoError(t, err)
	require.Equal(t, "new", value)
	require.NoError(t, mock.ExpectationsWereMet())
}
