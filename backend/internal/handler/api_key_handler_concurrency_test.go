package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type updateAPIKeyRepoCallTracker struct {
	service.APIKeyRepository
	getByIDCalls int
}

func (r *updateAPIKeyRepoCallTracker) GetByID(context.Context, int64) (*service.APIKey, error) {
	r.getByIDCalls++
	return nil, service.ErrAPIKeyNotFound
}

func TestUpdateAPIKeyRejectsConcurrencyOverflowBeforeService(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &updateAPIKeyRepoCallTracker{}
	svc := service.NewAPIKeyService(repo, nil, nil, nil, nil, nil, nil)
	handler := NewAPIKeyHandler(svc)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 42})
		c.Next()
	})
	router.PUT("/api/v1/api-keys/:id", handler.Update)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/api-keys/7", bytes.NewBufferString(`{"concurrency":2147483648}`))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), "Invalid request")
	require.Zero(t, repo.getByIDCalls, "binding must reject overflow before service persistence access")
}

func TestCreateAPIKeyRequestConcurrencyPropagation(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{name: "omitted defaults to zero", body: `{"name":"key"}`, want: 0},
		{name: "positive value propagates", body: `{"name":"key","concurrency":7}`, want: 7},
		{name: "maximum value propagates", body: `{"name":"key","concurrency":2147483647}`, want: 2147483647},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req CreateAPIKeyRequest
			require.NoError(t, json.Unmarshal([]byte(tt.body), &req))

			svcReq := req.toServiceRequest()

			require.Equal(t, tt.want, svcReq.Concurrency)
		})
	}
}
