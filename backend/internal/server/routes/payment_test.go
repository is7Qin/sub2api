package routes

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestPaymentChannelRouteBoundaries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	v1 := router.Group("/api/v1")
	pass := func(c *gin.Context) { c.Next() }
	handlers := &handler.Handlers{
		Admin:            &handler.AdminHandlers{},
		AvailableChannel: &handler.AvailableChannelHandler{},
	}

	RegisterUserRoutes(v1, handlers, middleware.JWTAuthMiddleware(pass), nil)
	RegisterAdminRoutes(v1, handlers, middleware.AdminAuthMiddleware(pass), nil)
	RegisterPaymentRoutes(
		v1,
		&handler.PaymentHandler{},
		&handler.PaymentWebhookHandler{},
		&admin.PaymentHandler{},
		middleware.JWTAuthMiddleware(pass),
		middleware.AdminAuthMiddleware(pass),
		nil,
	)

	routes := make(map[string]struct{}, len(router.Routes()))
	for _, route := range router.Routes() {
		routes[route.Method+" "+route.Path] = struct{}{}
	}

	require.NotContains(t, routes, "GET /api/v1/payment/channels")
	require.Contains(t, routes, "GET /api/v1/payment/checkout-info")
	require.Contains(t, routes, "GET /api/v1/channels/available")
	require.Contains(t, routes, "GET /api/v1/admin/payment/providers")
}
