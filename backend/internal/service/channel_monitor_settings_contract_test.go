//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestSettingService_ChannelMonitorSettingsUseSafeDefaultsAndNormalizeMode(t *testing.T) {
	svc := NewSettingService(&settingUpdateRepoStub{}, &config.Config{})

	defaults := svc.parseSettings(map[string]string{})
	require.Equal(t, ChannelMonitorModeV1, defaults.ChannelMonitorMode)
	require.True(t, defaults.ChannelMonitorHideThroughput)

	invalid := svc.parseSettings(map[string]string{
		SettingKeyChannelMonitorMode:           "unexpected",
		SettingKeyChannelMonitorHideThroughput: "false",
	})
	require.Equal(t, ChannelMonitorModeV1, invalid.ChannelMonitorMode)
	require.False(t, invalid.ChannelMonitorHideThroughput)

	v2 := svc.parseSettings(map[string]string{
		SettingKeyChannelMonitorMode:           " V2 ",
		SettingKeyChannelMonitorHideThroughput: "true",
	})
	require.Equal(t, ChannelMonitorModeV2, v2.ChannelMonitorMode)
	require.True(t, v2.ChannelMonitorHideThroughput)
}

func TestSettingService_GetPublicSettingsExposesChannelMonitorRuntimeContract(t *testing.T) {
	svc := NewSettingService(&settingPublicRepoStub{values: map[string]string{
		SettingKeyChannelMonitorMode:           ChannelMonitorModeV2,
		SettingKeyChannelMonitorHideThroughput: "false",
	}}, &config.Config{})

	settings, err := svc.GetPublicSettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, ChannelMonitorModeV2, settings.ChannelMonitorMode)
	require.False(t, settings.ChannelMonitorHideThroughput)

	defaults, err := NewSettingService(&settingPublicRepoStub{values: map[string]string{
		SettingKeyChannelMonitorMode: "invalid",
	}}, &config.Config{}).GetPublicSettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, ChannelMonitorModeV1, defaults.ChannelMonitorMode)
	require.True(t, defaults.ChannelMonitorHideThroughput)
}

func TestSettingService_UpdateSettingsNormalizesChannelMonitorMode(t *testing.T) {
	repo := &settingUpdateRepoStub{}
	svc := NewSettingService(repo, &config.Config{})

	err := svc.UpdateSettings(context.Background(), &SystemSettings{
		ChannelMonitorMode:           "invalid",
		ChannelMonitorHideThroughput: false,
	})
	require.NoError(t, err)
	require.Equal(t, ChannelMonitorModeV1, repo.updates[SettingKeyChannelMonitorMode])
	require.Equal(t, "false", repo.updates[SettingKeyChannelMonitorHideThroughput])

	err = svc.UpdateSettings(context.Background(), &SystemSettings{
		ChannelMonitorMode:           " V2 ",
		ChannelMonitorHideThroughput: true,
	})
	require.NoError(t, err)
	require.Equal(t, ChannelMonitorModeV2, repo.updates[SettingKeyChannelMonitorMode])
	require.Equal(t, "true", repo.updates[SettingKeyChannelMonitorHideThroughput])
}
