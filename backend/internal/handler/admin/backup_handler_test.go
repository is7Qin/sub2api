package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type missingBackupSettingRepo struct {
	service.SettingRepository
}

func (missingBackupSettingRepo) GetValue(context.Context, string) (string, error) {
	return "", service.ErrSettingNotFound
}

func TestBackupHandlerGetS3Config_MissingSettingReturnsEmptyConfig(t *testing.T) {
	gin.SetMode(gin.TestMode)
	backupService := service.NewBackupService(
		missingBackupSettingRepo{},
		&config.Config{},
		nil,
		nil,
		nil,
	)
	handler := NewBackupHandler(backupService, nil)
	router := gin.New()
	router.GET("/api/v1/admin/backups/s3-config", handler.GetS3Config)

	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/admin/backups/s3-config?timezone=Asia%2FTaipei",
		nil,
	)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var response struct {
		Code int                    `json:"code"`
		Data service.BackupS3Config `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	require.Zero(t, response.Code)
	require.Equal(t, service.BackupS3Config{}, response.Data)
}
