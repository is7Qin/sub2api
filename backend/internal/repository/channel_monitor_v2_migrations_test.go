package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestChannelMonitorV2MigrationsUseLocalSequenceAndSafeFactoryDefaults(t *testing.T) {
	files, err := migrations.FS.ReadDir(".")
	require.NoError(t, err)

	want := []string{
		"179_channel_monitor_v2.sql",
		"180_channel_monitor_mode.sql",
	}
	for _, name := range want {
		found := false
		for _, file := range files {
			if file.Name() == name {
				found = true
				break
			}
		}
		require.Truef(t, found, "missing locally numbered channel monitor v2 migration %s", name)
	}

	base, err := migrations.FS.ReadFile("179_channel_monitor_v2.sql")
	require.NoError(t, err)
	require.NotContains(t, string(base), `"platform":"grok"`)
	require.Contains(t, string(base), "ignored_error_categories TEXT[] NOT NULL")
	require.Contains(t, string(base), "health_thresholds JSONB NOT NULL")
	require.Contains(t, string(base), "channel_monitor_v2_metrics_rollup")
	require.Contains(t, string(base), "channel_monitor_v2_latency_histograms_rollup")
	require.Contains(t, string(base), "refresh_interval_seconds INTEGER NOT NULL DEFAULT 300")
	require.Contains(t, string(base), "GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE")

	mode, err := migrations.FS.ReadFile("180_channel_monitor_mode.sql")
	require.NoError(t, err)
	require.Contains(t, string(mode), "VALUES ('channel_monitor_mode', 'v1')")
	require.Contains(t, string(mode), "VALUES ('channel_monitor_hide_throughput', 'true')")
}
