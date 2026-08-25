package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const schedulerSupportDecisionMigration = "178_scheduler_support_decision.sql"
const schedulerSupportPublicationGenerationSequence = "scheduler_support_publication_generation_seq"

func readSchedulerSupportDecisionMigration(t *testing.T) string {
	t.Helper()
	raw, err := FS.ReadFile(schedulerSupportDecisionMigration)
	require.NoError(t, err)
	return strings.ToLower(string(raw))
}

func TestSchedulerSupportDecisionMigrationIsEmbedded(t *testing.T) {
	entries, err := FS.ReadDir(".")
	require.NoError(t, err)
	for _, entry := range entries {
		if entry.Name() == schedulerSupportDecisionMigration {
			return
		}
	}
	t.Fatalf("%s is not embedded", schedulerSupportDecisionMigration)
}

func TestSchedulerSupportPublicationGenerationSequenceIsIdempotent(t *testing.T) {
	sql := readSchedulerSupportDecisionMigration(t)
	require.Contains(t, sql, "create sequence if not exists "+schedulerSupportPublicationGenerationSequence)
	require.Contains(t, sql, "as bigint")
	require.Contains(t, sql, "minvalue 1")
	require.Contains(t, sql, "start with 1")
	require.Contains(t, sql, "increment by 1")
	require.Contains(t, sql, "no cycle")
}
