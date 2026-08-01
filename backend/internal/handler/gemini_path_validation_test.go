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
	for _, model := range []string{"../unsafe", " gemini-2.5-pro "} {
		t.Run(model, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodGet, "/v1beta/models/unsafe", nil)
			c.Params = gin.Params{{Key: "model", Value: model}}
			c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Platform: service.PlatformGemini}})
			c.Set(string(middleware.ContextKeyForcePlatform), service.PlatformAntigravity)

			(&GatewayHandler{}).GeminiV1BetaGetModel(c)

			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Contains(t, rec.Body.String(), "Invalid model in URL")
		})
	}
}

func TestGeminiV1BetaModelsRejectsUnsafeModelAtHandlerEdge(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, modelAction := range []string{"/../unsafe:generateContent", "/ gemini-2.5-pro:generateContent "} {
		t.Run(modelAction, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/unsafe:generateContent", nil)
			c.Params = gin.Params{{Key: "modelAction", Value: modelAction}}
			c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Platform: service.PlatformGemini}})
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 1})

			(&GatewayHandler{}).GeminiV1BetaModels(c)

			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Contains(t, rec.Body.String(), "Invalid model in URL")
		})
	}
}
