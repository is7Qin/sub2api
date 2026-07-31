package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGeminiV1BetaGetModelRejectsUnsafeModelAtHandlerEdge(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1beta/models/unsafe", nil)
	c.Params = gin.Params{{Key: "model", Value: "../unsafe"}}
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Platform: service.PlatformGemini}})

	(&GatewayHandler{}).GeminiV1BetaGetModel(c)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "Invalid model in URL")
}

func TestGeminiV1BetaModelsRejectsUnsafeModelAtHandlerEdge(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/unsafe:generateContent", nil)
	c.Params = gin.Params{{Key: "modelAction", Value: "/../unsafe:generateContent"}}
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Platform: service.PlatformGemini}})
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1})

	(&GatewayHandler{}).GeminiV1BetaModels(c)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "Invalid model in URL")
}
