package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
)

func TestAppendUsageLogModelWhereConditionRequested(t *testing.T) {
	conditions, args := appendUsageLogModelWhereCondition([]string{"user_id = $1"}, []any{int64(7)}, "requested-model", usagestats.ModelSourceRequested)
	require.Equal(t, []string{"user_id = $1", "(requested_model = $2 OR (requested_model IS NULL AND model = $2))"}, conditions)
	require.Equal(t, []any{int64(7), "requested-model"}, args)
}

func TestAppendUsageLogModelWhereConditionRawDefault(t *testing.T) {
	conditions, args := appendUsageLogModelWhereCondition(nil, nil, "upstream-model", "")
	require.Equal(t, []string{"model = $1"}, conditions)
	require.Equal(t, []any{"upstream-model"}, args)
}

func TestAppendUsageLogModelQueryFilterRequested(t *testing.T) {
	query, args := appendUsageLogModelQueryFilter("SELECT * FROM usage_logs WHERE user_id = $1", []any{int64(7)}, "requested-model", usagestats.ModelSourceRequested)
	require.Equal(t, "SELECT * FROM usage_logs WHERE user_id = $1 AND (requested_model = $2 OR (requested_model IS NULL AND model = $2))", query)
	require.Equal(t, []any{int64(7), "requested-model"}, args)
}

func TestAppendUsageLogRequestIDWhereCondition(t *testing.T) {
	conditions, args := appendUsageLogRequestIDWhereCondition([]string{"user_id = $1"}, []any{int64(7)}, " req-0123 ")
	require.Equal(t, []string{"user_id = $1", "request_id = $2"}, conditions)
	require.Equal(t, []any{int64(7), "req-0123"}, args)

	conditions, args = appendUsageLogRequestIDWhereCondition(conditions, args, " ")
	require.Len(t, conditions, 2)
	require.Len(t, args, 2)
}

func TestAppendUsageLogModelWhereConditionRequestedEmpty(t *testing.T) {
	conditions, args := appendUsageLogModelWhereCondition([]string{"user_id = $1"}, []any{int64(7)}, "  ", usagestats.ModelSourceRequested)
	require.Equal(t, []string{"user_id = $1"}, conditions)
	require.Equal(t, []any{int64(7)}, args)
}
