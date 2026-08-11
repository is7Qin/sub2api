package repository

import (
	"database/sql"

	"github.com/jackc/pgx/v5/pgtype"
)

// postgresInt64Array keeps nil slices as '{}' instead of SQL NULL for NOT NULL BIGINT[] columns.
func postgresStringArray(values []string) pgtype.FlatArray[string] {
	if values == nil {
		values = []string{}
	}
	return pgtype.FlatArray[string](values)
}

func postgresInt64Array(ids []int64) pgtype.FlatArray[int64] {
	if ids == nil {
		ids = []int64{}
	}
	return pgtype.FlatArray[int64](ids)
}

// scanPostgresInt64Array lets pgx decode BIGINT[] values before database/sql scans them into []int64.
func scanPostgresInt64Array(dest *[]int64) sql.Scanner {
	return pgtype.NewMap().SQLScanner(dest)
}

func scanPostgresStringArray(dest *[]string) sql.Scanner {
	return pgtype.NewMap().SQLScanner(dest)
}
