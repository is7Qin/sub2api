-- Adapted for local migration sequence; source: upstream PR #5356.
-- Channel monitor exclusive mode: v1 (active probes) or v2 (passive aggregation).
-- Default v1 preserves existing active-probe behavior; operators opt in to v2.
INSERT INTO settings (key, value)
VALUES ('channel_monitor_mode', 'v1')
ON CONFLICT (key) DO NOTHING;

-- User-facing passive monitoring hides fleet throughput unless an operator opts out.
INSERT INTO settings (key, value)
VALUES ('channel_monitor_hide_throughput', 'true')
ON CONFLICT (key) DO NOTHING;
