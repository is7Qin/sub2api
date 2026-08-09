//go:build unit

package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type resetRecoveryQuotaStub struct {
	resetResult *service.OpenAIQuotaResetResult
	resetErr    error
	queryResult *service.OpenAIQuotaUsage
	queryErr    error
	resetCalls  int
	queryCalls  int
	queryCtxErr error
}

func (s *resetRecoveryQuotaStub) ResetCredit(context.Context, int64) (*service.OpenAIQuotaResetResult, error) {
	s.resetCalls++
	return s.resetResult, s.resetErr
}

func (s *resetRecoveryQuotaStub) QueryUsage(ctx context.Context, _ int64) (*service.OpenAIQuotaUsage, error) {
	s.queryCalls++
	s.queryCtxErr = ctx.Err()
	return s.queryResult, s.queryErr
}

type resetRecoveryStub struct {
	err       error
	calls     int
	ctxErr    error
	accountID int64
}

func (s *resetRecoveryStub) RecoverAccountState(ctx context.Context, accountID int64, _ service.AccountRecoveryOptions) (*service.SuccessfulTestRecoveryResult, error) {
	s.calls++
	s.ctxErr = ctx.Err()
	s.accountID = accountID
	return &service.SuccessfulTestRecoveryResult{}, s.err
}

type resetRecoveryAdminStub struct {
	service.AdminService
	account *service.Account
	err     error
	calls   int
}

func (s *resetRecoveryAdminStub) GetAccount(context.Context, int64) (*service.Account, error) {
	s.calls++
	return s.account, s.err
}

type resetRecoveryEnvelope struct {
	Code int `json:"code"`
	Data struct {
		WarningCode           string                    `json:"warning_code"`
		AccountStateRecovered bool                      `json:"account_state_recovered"`
		Quota                 *service.OpenAIQuotaUsage `json:"quota"`
		Account               map[string]any            `json:"account"`
	} `json:"data"`
}

func performResetRecoveryRequest(t *testing.T, handler *OpenAIOAuthHandler, ctx context.Context) resetRecoveryEnvelope {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/accounts/:id/reset-quota", handler.ResetQuota)
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/accounts/42/reset-quota", nil).WithContext(ctx)
	router.ServeHTTP(recorder, req)
	require.Equal(t, http.StatusOK, recorder.Code)
	var envelope resetRecoveryEnvelope
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	return envelope
}

func successfulResetRecoveryQuotaStub() *resetRecoveryQuotaStub {
	return &resetRecoveryQuotaStub{
		resetResult: &service.OpenAIQuotaResetResult{Code: "success", WindowsReset: 1},
		queryResult: &service.OpenAIQuotaUsage{FetchedAt: 123},
	}
}

func TestOpenAIResetQuotaRecoversBeforeReturningPostResetState(t *testing.T) {
	quota := successfulResetRecoveryQuotaStub()
	recoverer := &resetRecoveryStub{}
	admin := &resetRecoveryAdminStub{account: &service.Account{
		ID: 42, Name: "recovered", Platform: service.PlatformOpenAI,
		Type: service.AccountTypeOAuth, Status: service.StatusActive,
	}}
	handler := &OpenAIOAuthHandler{
		quotaService:     quota,
		rateLimitService: recoverer,
		adminService:     admin,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	envelope := performResetRecoveryRequest(t, handler, ctx)

	require.Empty(t, envelope.Data.WarningCode)
	require.True(t, envelope.Data.AccountStateRecovered)
	require.Equal(t, 1, quota.resetCalls)
	require.Equal(t, 1, quota.queryCalls)
	require.Equal(t, 1, recoverer.calls)
	require.Equal(t, int64(42), recoverer.accountID)
	require.NoError(t, recoverer.ctxErr)
	require.NoError(t, quota.queryCtxErr)
	require.Equal(t, 1, admin.calls)
}

func TestOpenAIResetQuotaQueryFailureStillReturnsRecoveredAccount(t *testing.T) {
	quota := successfulResetRecoveryQuotaStub()
	quota.queryResult = nil
	quota.queryErr = errors.New("quota query failed")
	recoverer := &resetRecoveryStub{}
	admin := &resetRecoveryAdminStub{account: &service.Account{
		ID: 42, Name: "recovered", Platform: service.PlatformOpenAI,
		Type: service.AccountTypeOAuth, Status: service.StatusActive,
	}}
	handler := &OpenAIOAuthHandler{
		quotaService:     quota,
		rateLimitService: recoverer,
		adminService:     admin,
	}

	envelope := performResetRecoveryRequest(t, handler, context.Background())

	require.Equal(t, openAIQuotaResetWarningCacheRefreshFailed, envelope.Data.WarningCode)
	require.True(t, envelope.Data.AccountStateRecovered)
	require.Nil(t, envelope.Data.Quota)
	require.NotNil(t, envelope.Data.Account)
	require.Equal(t, 1, admin.calls)
}

func TestOpenAIResetQuotaRecoveryFailureStopsLaterWork(t *testing.T) {
	quota := successfulResetRecoveryQuotaStub()
	recoverer := &resetRecoveryStub{err: errors.New("recovery failed")}
	handler := &OpenAIOAuthHandler{
		quotaService:     quota,
		rateLimitService: recoverer,
		adminService:     &resetRecoveryAdminStub{},
	}

	envelope := performResetRecoveryRequest(t, handler, context.Background())

	require.Equal(t, openAIQuotaResetWarningAccountRecoveryFailed, envelope.Data.WarningCode)
	require.False(t, envelope.Data.AccountStateRecovered)
	require.Zero(t, quota.queryCalls)
}
