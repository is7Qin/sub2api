package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
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
	handler := NewBackupHandler(backupService, nil, nil)
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

type backupStepUpVerifierStub struct {
	err   error
	calls int
}

func (s *backupStepUpVerifierStub) VerifyBackupS3Update(ctx context.Context, userID int64, code string) error {
	s.calls++
	return s.err
}

func newBackupS3UpdateRouter(t *testing.T, verifier *backupStepUpVerifierStub, repo service.SettingRepository) (*gin.Engine, *service.BackupService) {
	t.Helper()
	backupService := service.NewBackupService(
		repo,
		&config.Config{Totp: config.TotpConfig{EncryptionKeyConfigured: true}},
		backupPassthroughEncryptor{},
		nil,
		nil,
	)
	handler := NewBackupHandler(backupService, nil, verifier)
	router := gin.New()
	router.PUT("/api/v1/admin/backups/s3-config", func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7})
		c.Set("auth_method", "jwt")
		c.Next()
	}, handler.UpdateS3Config)
	return router, backupService
}

type backupPassthroughEncryptor struct{}

func (backupPassthroughEncryptor) Encrypt(plaintext string) (string, error) {
	return "ENC:" + plaintext, nil
}
func (backupPassthroughEncryptor) Decrypt(ciphertext string) (string, error) { return ciphertext, nil }

func putS3Config(router *gin.Engine, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/backups/s3-config", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestBackupHandlerUpdateS3Config_RequiresFreshTotpCodeForOmittedSecret(t *testing.T) {
	repo := &backupSettingRepo{values: map[string]string{"backup_s3_config": `{"bucket":"old","access_key_id":"AK","secret_access_key":"ENC:old"}`}}
	router, _ := newBackupS3UpdateRouter(t, &backupStepUpVerifierStub{}, repo)

	rec := putS3Config(router, `{"bucket":"new","access_key_id":"AK","secret_access_key":"","prefix":"backups/"}`)

	require.Equal(t, http.StatusForbidden, rec.Code)
	require.JSONEq(t, `{"code":403,"reason":"BACKUP_S3_STEP_UP_REQUIRED","message":"recent TOTP verification is required to update backup S3 configuration"}`, rec.Body.String())
	require.Equal(t, `{"bucket":"old","access_key_id":"AK","secret_access_key":"ENC:old"}`, repo.values["backup_s3_config"])
}

func TestBackupHandlerUpdateS3Config_RejectsInvalidTotpCodeWithoutSavingNewSecret(t *testing.T) {
	repo := &backupSettingRepo{values: map[string]string{}}
	verifier := &backupStepUpVerifierStub{err: service.ErrTotpInvalidCode}
	router, _ := newBackupS3UpdateRouter(t, verifier, repo)

	rec := putS3Config(router, `{"bucket":"new","access_key_id":"AK","secret_access_key":"new-secret","totp_code":"000000"}`)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "TOTP_INVALID_CODE")
	require.Empty(t, repo.values["backup_s3_config"])
	require.Equal(t, 1, verifier.calls)
}

func TestBackupHandlerUpdateS3Config_AllowsVerifiedTotpCodeOnlyOnce(t *testing.T) {
	repo := &backupSettingRepo{values: map[string]string{}}
	verifier := &backupStepUpVerifierStub{}
	router, _ := newBackupS3UpdateRouter(t, verifier, repo)

	rec := putS3Config(router, `{"bucket":"new","access_key_id":"AK","secret_access_key":"new-secret","totp_code":"123456"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotContains(t, rec.Body.String(), "new-secret")

	rec = putS3Config(router, `{"bucket":"newer","access_key_id":"AK","secret_access_key":"","prefix":"new/"}`)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, 1, verifier.calls)
}

func TestBackupHandlerUpdateS3Config_RejectsReplayedTotpCodeWithoutSaving(t *testing.T) {
	repo := &backupSettingRepo{values: map[string]string{}}
	verifier := &backupStepUpVerifierStub{err: infraerrors.BadRequest("BACKUP_S3_TOTP_REPLAYED", "TOTP code has already been used for a backup S3 update; wait for a new code")}
	router, _ := newBackupS3UpdateRouter(t, verifier, repo)

	rec := putS3Config(router, `{"bucket":"new","access_key_id":"AK","secret_access_key":"new-secret","totp_code":"123456"}`)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "BACKUP_S3_TOTP_REPLAYED")
	require.Empty(t, repo.values["backup_s3_config"])
	require.Equal(t, 1, verifier.calls)
}

func TestBackupHandlerUpdateS3Config_RejectsAdminAPIKeyCaller(t *testing.T) {
	repo := &backupSettingRepo{values: map[string]string{}}
	verifier := &backupStepUpVerifierStub{}
	backupService := service.NewBackupService(repo, &config.Config{}, backupPassthroughEncryptor{}, nil, nil)
	handler := NewBackupHandler(backupService, nil, verifier)
	router := gin.New()
	router.PUT("/api/v1/admin/backups/s3-config", func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7})
		c.Set("auth_method", "admin_api_key")
		c.Next()
	}, handler.UpdateS3Config)

	rec := putS3Config(router, `{"bucket":"new","access_key_id":"AK","secret_access_key":"new-secret","totp_code":"123456"}`)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Contains(t, rec.Body.String(), "BACKUP_S3_STEP_UP_ADMIN_API_KEY_FORBIDDEN")
	require.Zero(t, verifier.calls)
}

func TestBackupHandlerUpdateS3Config_DoesNotPersistWhenServiceValidationFailsAfterTotp(t *testing.T) {
	repo := &backupSettingRepo{values: map[string]string{}}
	verifier := &backupStepUpVerifierStub{}
	backupService := service.NewBackupService(repo, &config.Config{Totp: config.TotpConfig{EncryptionKeyConfigured: false}}, backupPassthroughEncryptor{}, nil, nil)
	handler := NewBackupHandler(backupService, nil, verifier)
	router := gin.New()
	router.PUT("/api/v1/admin/backups/s3-config", func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 7})
		c.Set("auth_method", "jwt")
		c.Next()
	}, handler.UpdateS3Config)

	rec := putS3Config(router, `{"bucket":"new","access_key_id":"AK","secret_access_key":"new-secret","totp_code":"123456"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "SECRET_ENCRYPTION_KEY_NOT_CONFIGURED")
	require.Empty(t, repo.values["backup_s3_config"])
	require.Equal(t, 1, verifier.calls)
}

type backupSettingRepo struct {
	service.SettingRepository
	values map[string]string
}

func (r *backupSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	value, ok := r.values[key]
	if !ok {
		return "", service.ErrSettingNotFound
	}
	return value, nil
}

func (r *backupSettingRepo) Set(_ context.Context, key, value string) error {
	r.values[key] = value
	return nil
}
