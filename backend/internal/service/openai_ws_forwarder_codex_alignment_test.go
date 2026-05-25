package service

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIWSHeadersOAuthAddsCodexIdentityFallbacks(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	c.Request.Header.Set("User-Agent", "opencode/0.9")
	c.Request.Header.Set("originator", "opencode")

	svc := &OpenAIGatewayService{}
	account := &Account{
		ID:          43,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "chatgpt-acc"},
	}
	fallbackSessionID := fallbackOpenAICodexSessionID(c, account, []byte(`{"model":"gpt-5","input":"hello"}`))

	headers, resolution := svc.buildOpenAIWSHeaders(
		c,
		account,
		"token",
		OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2},
		openai.IsCodexOfficialClientByHeaders(c.GetHeader("User-Agent"), c.GetHeader("originator")),
		"",
		"",
		"",
		fallbackSessionID,
	)

	require.NotEmpty(t, resolution.SessionID)
	require.Equal(t, "fallback_session_id", resolution.SessionSource)
	require.Equal(t, "fallback_session_id", resolution.ThreadSource)
	require.Equal(t, "codex_cli_rs", headers.Get("originator"))
	require.Equal(t, codexCLIVersion, headers.Get("Version"))
	require.Equal(t, codexCLIUserAgent, headers.Get("User-Agent"))
	require.Equal(t, openAIWSBetaV2Value, headers.Get("OpenAI-Beta"))
	require.NotEmpty(t, headers.Get(openAICodexSessionIDHeader))
	require.NotEmpty(t, headers.Get(openAICodexThreadIDHeader))
	require.Equal(t, headers.Get(openAICodexThreadIDHeader), headers.Get(openAICodexClientRequestIDHeader))
	require.NotEmpty(t, headers.Get(openAICodexInstallationIDHeader))
	require.NotEmpty(t, headers.Get(openAICodexWindowIDHeader))
}

func TestOpenAIWSCodexClientMetadataIncludesRequestStart(t *testing.T) {
	payload := map[string]any{"type": "response.create", "model": "gpt-5"}
	headers := http.Header{}
	headers.Set(openAICodexInstallationIDHeader, "installation-1")
	headers.Set(openAICodexWindowIDHeader, "thread-1:0")
	headers.Set(openAICodexThreadIDHeader, "thread-1")

	setOpenAIWSCodexClientMetadata(payload, headers)

	metadata, ok := payload["client_metadata"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "installation-1", metadata[openAICodexInstallationIDHeader])
	require.Equal(t, "thread-1:0", metadata[openAICodexWindowIDHeader])
	require.Equal(t, "thread-1", metadata[openAICodexThreadIDHeader])
	startMS, ok := metadata[openAICodexWSStreamRequestStartMSKey].(string)
	require.True(t, ok)
	parsedStartMS, err := strconv.ParseInt(startMS, 10, 64)
	require.NoError(t, err)
	require.Positive(t, parsedStartMS)
}
