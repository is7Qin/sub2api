package routes

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAdminBackupS3ConfigRouteRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	v1 := router.Group("/api/v1")
	pass := func(c *gin.Context) { c.Next() }

	RegisterAdminRoutes(
		v1,
		&handler.Handlers{Admin: &handler.AdminHandlers{}},
		middleware.AdminAuthMiddleware(pass),
	)

	routes := make(map[string]struct{}, len(router.Routes()))
	for _, route := range router.Routes() {
		routes[route.Method+" "+route.Path] = struct{}{}
	}
	require.Contains(t, routes, "GET /api/v1/admin/backups/s3-config")
}
