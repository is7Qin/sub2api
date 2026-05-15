package service

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSafeUpstreamURL(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"strips query", "https://api.anthropic.com/v1/messages?beta=true", "https://api.anthropic.com/v1/messages"},
		{"strips fragment", "https://api.openai.com/v1/responses#frag", "https://api.openai.com/v1/responses"},
		{"strips both", "https://host/path?token=secret#x", "https://host/path"},
		{"no query or fragment", "https://host/path", "https://host/path"},
		{"empty string", "", ""},
		{"whitespace only", "  ", ""},
		{"query before fragment", "https://h/p?a=1#f", "https://h/p"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, safeUpstreamURL(tt.input))
		})
	}
}

func TestAppendOpsUpstreamError_UsesRequestBodyBytesFromContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	setOpsUpstreamRequestBody(c, []byte(`{"model":"gpt-5"}`))
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Kind:    "http_error",
		Message: "upstream failed",
	})

	v, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok)
	events, ok := v.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.Len(t, events, 1)
	require.Equal(t, `{"model":"gpt-5"}`, events[0].UpstreamRequestBody)
}

func TestAppendOpsUpstreamError_TruncatedSnapshotMarksKind(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	body := []byte(`{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"` + strings.Repeat("x", OpsUpstreamRequestBodySnapshotBytes*2) + `"}]}`)
	setOpsUpstreamRequestBody(c, body)
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Kind:    "http_error",
		Message: "upstream failed",
	})

	v, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok)
	events, ok := v.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.Len(t, events, 1)
	require.Contains(t, events[0].Kind, "request_body_truncated")
	require.LessOrEqual(t, len(events[0].UpstreamRequestBody), OpsUpstreamRequestBodySnapshotBytes)
}

func TestSanitizeOpsUpstreamErrors_TruncatedSnapshotKeepsValidRequestBody(t *testing.T) {
	largeBody := `{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"` + strings.Repeat("x", OpsUpstreamRequestBodySnapshotBytes*2) + `"}],"api_key":"secret"}`
	snapshot := NewOpsRequestBodySnapshot([]byte(largeBody), OpsUpstreamRequestBodySnapshotBytes)
	require.NotNil(t, snapshot)

	entry := &OpsInsertErrorLogInput{
		UpstreamErrors: []*OpsUpstreamErrorEvent{
			{
				Kind:                "http_error:request_body_truncated",
				Message:             "upstream failed",
				UpstreamRequestBody: OpsRequestBodySnapshotText(snapshot),
			},
		},
	}
	require.NoError(t, sanitizeOpsUpstreamErrors(entry))
	require.NotNil(t, entry.UpstreamErrorsJSON)

	events, err := ParseOpsUpstreamErrors(*entry.UpstreamErrorsJSON)
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.NotEmpty(t, events[0].UpstreamRequestBody)
	require.NotContains(t, events[0].UpstreamRequestBody, "secret")

	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(events[0].UpstreamRequestBody), &body))
	require.Equal(t, "gpt-5", body["model"])
	require.Equal(t, true, body["stream"])
	require.Equal(t, true, body["request_body_truncated"])
}

func TestSetOpsUpstreamRequestBody_StoresBoundedSnapshot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	body := []byte(`{"model":"gpt-5","input":"` + strings.Repeat("x", OpsUpstreamRequestBodySnapshotBytes*2) + `"}`)
	setOpsUpstreamRequestBody(c, body)

	v, ok := c.Get(OpsUpstreamRequestBodyKey)
	require.True(t, ok)
	snapshot, ok := v.(*OpsRequestBodySnapshot)
	require.True(t, ok)
	require.Equal(t, len(body), snapshot.Bytes)
	require.True(t, snapshot.Truncated)
	require.Len(t, snapshot.Body, OpsUpstreamRequestBodySnapshotBytes)
}

func TestAppendOpsUpstreamError_UsesRequestBodyStringFromContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	c.Set(OpsUpstreamRequestBodyKey, `{"model":"gpt-4"}`)
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Kind:    "request_error",
		Message: "dial timeout",
	})

	v, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok)
	events, ok := v.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.Len(t, events, 1)
	require.Equal(t, `{"model":"gpt-4"}`, events[0].UpstreamRequestBody)
}
