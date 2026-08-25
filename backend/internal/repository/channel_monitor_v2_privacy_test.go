package repository

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChannelMonitorV2AdminErrorDetailsDoNotReadFreeFormBodies(t *testing.T) {
	source, err := os.ReadFile("channel_monitor_v2_repo.go")
	require.NoError(t, err)

	text := string(source)
	start := strings.Index(text, "func (r *channelMonitorV2Repository) loadErrorDetails")
	end := strings.Index(text[start:], "func (r *channelMonitorV2Repository) GetUsers")
	require.GreaterOrEqual(t, start, 0)
	require.Greater(t, end, 0)
	section := text[start : start+end]

	require.NotContains(t, section, "current_error.error_body")
	require.NotContains(t, section, "current_error.upstream_error_message")
	require.NotContains(t, section, "current_error.upstream_error_detail")
	require.NotContains(t, section, "current_error.error_message")
	require.Contains(t, section, "current_error.error_type")
	require.Contains(t, section, "current_error.provider_error_code")
}
