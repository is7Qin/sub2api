package service

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/require"
)

type totpLoggingSettingRepo struct {
	SettingRepository
}

func (totpLoggingSettingRepo) GetValue(context.Context, string) (string, error) {
	return "true", nil
}

type totpLoggingUserRepo struct {
	UserRepository
	user *User
}

func (r *totpLoggingUserRepo) GetByID(context.Context, int64) (*User, error) {
	return r.user, nil
}

func (r *totpLoggingUserRepo) UpdateTotpSecret(context.Context, int64, *string) error {
	return nil
}

func (r *totpLoggingUserRepo) EnableTotp(context.Context, int64) error {
	return nil
}

type totpLoggingCache struct {
	TotpCache
	session *TotpSetupSession
}

func (c *totpLoggingCache) GetSetupSession(context.Context, int64) (*TotpSetupSession, error) {
	return c.session, nil
}

func (*totpLoggingCache) DeleteSetupSession(context.Context, int64) error {
	return nil
}

func (*totpLoggingCache) GetVerifyAttempts(context.Context, int64) (int, error) {
	return 0, nil
}

func (*totpLoggingCache) ClearVerifyAttempts(context.Context, int64) error {
	return nil
}

func captureTotpDebugLogs(t *testing.T, run func()) string {
	t.Helper()

	var output bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	run()
	return output.String()
}

func requireTotpSecretAbsentFromLogs(t *testing.T, logs, secret string) {
	t.Helper()

	for _, sensitive := range []string{secret, secret[:4], secret[:8], strings.ToLower(secret[:8])} {
		require.NotContains(t, logs, sensitive)
	}
}

func TestTotpServiceCompleteSetupDoesNotLogSecretOrPrefixes(t *testing.T) {
	secret := backupS3TotpTestSecret
	code, err := totp.GenerateCode(secret, time.Now().UTC())
	require.NoError(t, err)

	cache := &totpLoggingCache{session: &TotpSetupSession{Secret: secret, SetupToken: "setup-token"}}
	service := NewTotpService(
		&totpLoggingUserRepo{user: &User{ID: 7}},
		backupS3TotpEncryptorStub{},
		cache,
		NewSettingService(totpLoggingSettingRepo{}, &config.Config{}),
		nil,
		nil,
	)

	logs := captureTotpDebugLogs(t, func() {
		require.NoError(t, service.CompleteSetup(context.Background(), 7, code, "setup-token"))
	})

	require.Contains(t, logs, "totp_complete_setup_verified")
	requireTotpSecretAbsentFromLogs(t, logs, secret)
}

func TestTotpServiceVerifyCodeDoesNotLogSecretOrPrefixes(t *testing.T) {
	secret := backupS3TotpTestSecret
	service := newBackupS3TotpService(&totpLoggingCache{})
	code, err := totp.GenerateCode(secret, time.Now().UTC())
	require.NoError(t, err)

	logs := captureTotpDebugLogs(t, func() {
		require.NoError(t, service.VerifyCode(context.Background(), 7, code))
	})

	require.Contains(t, logs, "totp_verify_result")
	requireTotpSecretAbsentFromLogs(t, logs, secret)
}
