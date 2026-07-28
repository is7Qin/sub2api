package setup

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRedisHandlerUsernameContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name       string
		username   *string
		wantStatus int
	}{
		{name: "omitted remains default user", wantStatus: http.StatusBadRequest},
		{name: "empty remains default user", username: stringPointer(""), wantStatus: http.StatusBadRequest},
		{name: "special characters accepted before dial", username: stringPointer(" acl:user/@% "), wantStatus: http.StatusBadRequest},
		{name: "128 UTF-8 bytes accepted before dial", username: stringPointer(strings.Repeat("é", 64)), wantStatus: http.StatusBadRequest},
		{name: "129 UTF-8 bytes rejected before dial", username: stringPointer(strings.Repeat("a", 127) + "é"), wantStatus: http.StatusBadRequest},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"host": "127.0.0.1", "port": 1, "password": "password-secret/@%", "db": 0}
			if tc.username != nil {
				body["username"] = *tc.username
			}
			payload, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}

			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/setup/test-redis", bytes.NewReader(payload))
			ctx.Request.Header.Set("Content-Type", "application/json")
			testRedis(ctx)

			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, tc.wantStatus)
			}
			response := recorder.Body.String()
			if strings.Contains(response, "password-secret/@%") || (tc.username != nil && *tc.username != "" && strings.Contains(response, *tc.username)) {
				t.Fatalf("response leaked Redis credentials: %s", response)
			}
			if tc.name == "129 UTF-8 bytes rejected before dial" {
				if !strings.Contains(response, "at most 128 bytes") || strings.Contains(response, "Connection failed") {
					t.Fatalf("oversize username response = %s", response)
				}
			} else if !strings.Contains(response, "Connection failed") {
				t.Fatalf("valid username did not reach the Redis dial path: %s", response)
			}
		})
	}
}

func TestInstallRejectsOversizeRedisUsernameWithoutCredentialLeak(t *testing.T) {
	gin.SetMode(gin.TestMode)
	username := strings.Repeat("a", 127) + "é"
	password := "password-secret/@%"
	body := map[string]any{
		"database": map[string]any{"host": "localhost", "port": 5432, "user": "postgres", "password": "db-secret", "dbname": "sub2api", "sslmode": "disable"},
		"redis":    map[string]any{"host": "localhost", "port": 6379, "username": username, "password": password, "db": 0},
		"admin":    map[string]any{"email": "admin@example.com", "password": "admin-password"},
		"server":   map[string]any{"port": 8080, "mode": "release"},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATA_DIR", t.TempDir())

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/setup/install", bytes.NewReader(payload))
	ctx.Request.Header.Set("Content-Type", "application/json")
	install(ctx)

	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "at most 128 bytes") {
		t.Fatalf("install response: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), username) || strings.Contains(recorder.Body.String(), password) {
		t.Fatalf("install response leaked Redis credentials: %s", recorder.Body.String())
	}
}

func stringPointer(value string) *string { return &value }
