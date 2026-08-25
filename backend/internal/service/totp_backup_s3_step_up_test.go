package service

import (
	"context"
	"errors"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/require"
)

const backupS3TotpTestSecret = "JBSWY3DPEHPK3PXP"

type backupS3TotpUserRepoStub struct {
	UserRepository
	user *User
}

func (s *backupS3TotpUserRepoStub) GetByID(context.Context, int64) (*User, error) {
	return s.user, nil
}

type backupS3TotpCacheStub struct {
	TotpCache
	usedSteps    map[int64]bool
	getErr       error
	incrementErr error
	clearErr     error
	useErr       error
	lastStep     int64
	lastTTL      time.Duration
	useCalls     int
}

func (s *backupS3TotpCacheStub) GetVerifyAttempts(context.Context, int64) (int, error) {
	return 0, s.getErr
}

func (s *backupS3TotpCacheStub) IncrementVerifyAttempts(context.Context, int64) (int, error) {
	return 0, s.incrementErr
}

func (s *backupS3TotpCacheStub) ClearVerifyAttempts(context.Context, int64) error {
	return s.clearErr
}

func (s *backupS3TotpCacheStub) UseBackupS3TotpStep(_ context.Context, _ int64, step int64, ttl time.Duration) (bool, error) {
	s.useCalls++
	if s.useErr != nil {
		return false, s.useErr
	}
	if s.usedSteps == nil {
		s.usedSteps = map[int64]bool{}
	}
	used := s.usedSteps[step]
	s.usedSteps[step] = true
	s.lastStep = step
	s.lastTTL = ttl
	return used, nil
}

func newBackupS3TotpService(cache TotpCache) *TotpService {
	secret := backupS3TotpTestSecret
	return NewTotpService(
		&backupS3TotpUserRepoStub{user: &User{ID: 7, TotpEnabled: true, TotpSecretEncrypted: &secret}},
		backupS3TotpEncryptorStub{},
		cache,
		nil,
		nil,
		nil,
	)
}

type backupS3TotpEncryptorStub struct{}

func (backupS3TotpEncryptorStub) Encrypt(plaintext string) (string, error)  { return plaintext, nil }
func (backupS3TotpEncryptorStub) Decrypt(ciphertext string) (string, error) { return ciphertext, nil }

func TestTotpServiceVerifyBackupS3UpdateConsumesMatchedSkewStep(t *testing.T) {
	now := time.Now().UTC()
	code, err := totp.GenerateCode(backupS3TotpTestSecret, now.Add(-30*time.Second))
	require.NoError(t, err)

	cache := &backupS3TotpCacheStub{}
	service := newBackupS3TotpService(cache)

	require.NoError(t, service.VerifyBackupS3Update(context.Background(), 7, code))
	require.Equal(t, now.Add(-30*time.Second).Unix()/30, cache.lastStep)
	require.GreaterOrEqual(t, cache.lastTTL, 90*time.Second)

	err = service.VerifyBackupS3Update(context.Background(), 7, code)
	require.ErrorContains(t, err, "TOTP code has already been used")
}

func TestTotpServiceVerifyBackupS3UpdateFailsClosedWhenReplayCacheUnavailable(t *testing.T) {
	code, err := totp.GenerateCode(backupS3TotpTestSecret, time.Now().UTC())
	require.NoError(t, err)

	service := newBackupS3TotpService(&backupS3TotpCacheStub{useErr: errors.New("redis unavailable")})

	err = service.VerifyBackupS3Update(context.Background(), 7, code)
	require.Error(t, err)
	require.ErrorContains(t, err, "TOTP verification service unavailable")
	require.NotContains(t, err.Error(), code)
}

func TestTotpServiceVerifyBackupS3UpdateFailsClosedWhenAttemptReadFails(t *testing.T) {
	code, err := totp.GenerateCode(backupS3TotpTestSecret, time.Now().UTC())
	require.NoError(t, err)
	cache := &backupS3TotpCacheStub{getErr: errors.New("redis unavailable")}

	err = newBackupS3TotpService(cache).VerifyBackupS3Update(context.Background(), 7, code)

	require.Equal(t, "BACKUP_S3_STEP_UP_UNAVAILABLE", infraerrors.Reason(err))
	require.Zero(t, cache.useCalls)
	require.NotContains(t, err.Error(), code)
	require.NotContains(t, err.Error(), backupS3TotpTestSecret)
}

func TestTotpServiceVerifyBackupS3UpdateFailsClosedWhenInvalidAttemptIncrementFails(t *testing.T) {
	cache := &backupS3TotpCacheStub{incrementErr: errors.New("redis unavailable")}
	const invalidCode = "not-a-totp-code"

	err := newBackupS3TotpService(cache).VerifyBackupS3Update(context.Background(), 7, invalidCode)

	require.Equal(t, "BACKUP_S3_STEP_UP_UNAVAILABLE", infraerrors.Reason(err))
	require.Zero(t, cache.useCalls)
	require.NotContains(t, err.Error(), invalidCode)
	require.NotContains(t, err.Error(), backupS3TotpTestSecret)
}

func TestTotpServiceVerifyBackupS3UpdateRejectsValidCodeWhenAttemptClearFails(t *testing.T) {
	code, err := totp.GenerateCode(backupS3TotpTestSecret, time.Now().UTC())
	require.NoError(t, err)
	cache := &backupS3TotpCacheStub{clearErr: errors.New("redis unavailable")}

	err = newBackupS3TotpService(cache).VerifyBackupS3Update(context.Background(), 7, code)

	require.Equal(t, "BACKUP_S3_STEP_UP_UNAVAILABLE", infraerrors.Reason(err))
	require.Zero(t, cache.useCalls)
	require.NotContains(t, err.Error(), code)
	require.NotContains(t, err.Error(), backupS3TotpTestSecret)
}
