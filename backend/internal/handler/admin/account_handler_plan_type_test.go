package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type stubOpenAIPlanTypeRefresher struct {
	mu     sync.Mutex
	calls  []int64
	errors map[int64]error
}

func (s *stubOpenAIPlanTypeRefresher) RefreshOpenAIPlanType(_ context.Context, account *service.Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, account.ID)
	if s.errors != nil {
		return s.errors[account.ID]
	}
	return nil
}

func (s *stubOpenAIPlanTypeRefresher) snapshotCalls() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.calls...)
}

func setupBatchRefreshPlanTypeRouter(adminSvc *stubAdminService, refresher openAIPlanTypeRefresher) *gin.Engine {
	handler := NewAccountHandler(adminSvc, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	handler.planTypeRefresher = refresher
	router := gin.New()
	router.POST("/api/v1/admin/accounts/batch-refresh-plan-type", handler.BatchRefreshPlanType)
	return router
}

func TestAccountHandlerBatchRefreshPlanTypeRefreshesSelectedOpenAIOAuthAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	adminSvc := newStubAdminService()
	adminSvc.getAccountsByIDs = func(_ctx context.Context, ids []int64) ([]*service.Account, error) {
		return []*service.Account{
			{ID: ids[0], Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth},
		}, nil
	}

	usageSvc := &stubOpenAIPlanTypeRefresher{}
	router := setupBatchRefreshPlanTypeRouter(adminSvc, usageSvc)

	body, err := json.Marshal(gin.H{"account_ids": []int64{101}})
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/batch-refresh-plan-type", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var payload struct {
		Data struct {
			Total   int `json:"total"`
			Success int `json:"success"`
			Failed  int `json:"failed"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Equal(t, 1, payload.Data.Total)
	require.Equal(t, 1, payload.Data.Success)
	require.Equal(t, 0, payload.Data.Failed)
	require.Equal(t, []int64{101}, usageSvc.snapshotCalls())
}

func TestAccountHandlerBatchRefreshPlanTypeAggregatesFailures(t *testing.T) {
	gin.SetMode(gin.TestMode)
	adminSvc := newStubAdminService()
	adminSvc.getAccountsByIDs = func(_ctx context.Context, ids []int64) ([]*service.Account, error) {
		return []*service.Account{
			{ID: ids[0], Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth},
			{ID: ids[1], Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth},
		}, nil
	}

	usageSvc := &stubOpenAIPlanTypeRefresher{errors: map[int64]error{102: fmt.Errorf("missing token")}}
	router := setupBatchRefreshPlanTypeRouter(adminSvc, usageSvc)

	body, err := json.Marshal(gin.H{"account_ids": []int64{101, 102, 103}})
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/batch-refresh-plan-type", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var payload struct {
		Data struct {
			Total   int `json:"total"`
			Success int `json:"success"`
			Failed  int `json:"failed"`
			Errors  []struct {
				AccountID int64  `json:"account_id"`
				Error     string `json:"error"`
			} `json:"errors"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Equal(t, 3, payload.Data.Total)
	require.Equal(t, 1, payload.Data.Success)
	require.Equal(t, 2, payload.Data.Failed)
	require.Len(t, payload.Data.Errors, 2)
	require.ElementsMatch(t, []int64{101, 102}, usageSvc.snapshotCalls())
}
