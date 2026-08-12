//go:build unit

package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSettingHandler_UpdateSettingsPreservesOmittedChannelMonitorRuntimeFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &settingHandlerRepoStub{values: map[string]string{
		service.SettingKeyChannelMonitorEnabled:                "true",
		service.SettingKeyChannelMonitorMode:                   service.ChannelMonitorModeV2,
		service.SettingKeyChannelMonitorDefaultIntervalSeconds: "300",
		service.SettingKeyChannelMonitorHideThroughput:         "false",
	}}
	handler := NewSettingHandler(
		service.NewSettingService(repo, &config.Config{Default: config.DefaultConfig{UserConcurrency: 5}}),
		nil, nil, nil, nil, nil, nil,
	)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/admin/settings", bytes.NewBufferString(`{"site_name":"Updated"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateSettings(c)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, service.ChannelMonitorModeV2, repo.lastUpdates[service.SettingKeyChannelMonitorMode])
	require.Equal(t, "false", repo.lastUpdates[service.SettingKeyChannelMonitorHideThroughput])
}

func TestSettingHandler_UpdateSettingsExposesChannelMonitorRuntimeFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &settingHandlerRepoStub{values: map[string]string{}}
	handler := NewSettingHandler(
		service.NewSettingService(repo, &config.Config{Default: config.DefaultConfig{UserConcurrency: 5}}),
		nil, nil, nil, nil, nil, nil,
	)

	body, err := json.Marshal(map[string]any{
		"channel_monitor_mode":            service.ChannelMonitorModeV2,
		"channel_monitor_hide_throughput": false,
	})
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/admin/settings", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateSettings(c)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, service.ChannelMonitorModeV2, repo.lastUpdates[service.SettingKeyChannelMonitorMode])
	require.Equal(t, "false", repo.lastUpdates[service.SettingKeyChannelMonitorHideThroughput])

	var response struct {
		Data struct {
			ChannelMonitorMode           string `json:"channel_monitor_mode"`
			ChannelMonitorHideThroughput bool   `json:"channel_monitor_hide_throughput"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	require.Equal(t, service.ChannelMonitorModeV2, response.Data.ChannelMonitorMode)
	require.False(t, response.Data.ChannelMonitorHideThroughput)
}
