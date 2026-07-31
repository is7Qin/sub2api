package service

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSanitizedUpstreamPathSuffixRejectsUnsafeSegments(t *testing.T) {
	for _, suffix := range []string{
		"/..", "/...", "/compact/..", "/compact//detail", "/compact%2f..",
		"/compact?a=b", "/compact#fragment", "/compact\x00", "/模型", "/a:b", "/a@b",
		"/compact ", " /compact",
	} {
		t.Run(suffix, func(t *testing.T) {
			got, ok := sanitizedUpstreamPathSuffix(suffix)
			require.False(t, ok)
			require.Empty(t, got)
		})
	}
}

func TestSanitizedUpstreamPathSuffixBoundsAndValidShapes(t *testing.T) {
	long := "/" + strings.Repeat("a", 129)
	_, ok := sanitizedUpstreamPathSuffix(long)
	require.False(t, ok)

	_, ok = sanitizedUpstreamPathSuffix(strings.Repeat("/a", 9))
	require.False(t, ok)

	for _, suffix := range []string{"", "/compact", "/compact/detail", "/resp_68f0a1b2/cancel"} {
		got, ok := sanitizedUpstreamPathSuffix(suffix)
		require.True(t, ok)
		require.Equal(t, suffix, got)
	}
}

func TestSanitizedUpstreamPathSuffixNormalizesOneTrailingSlash(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{raw: "/compact/", want: "/compact"},
		{raw: "/resp_68f0a1b2/cancel/", want: "/resp_68f0a1b2/cancel"},
	} {
		got, ok := sanitizedUpstreamPathSuffix(tc.raw)
		require.True(t, ok, tc.raw)
		require.Equal(t, tc.want, got)
	}

	for _, raw := range []string{"/compact//detail", "/compact//"} {
		got, ok := sanitizedUpstreamPathSuffix(raw)
		require.False(t, ok, raw)
		require.Empty(t, got, raw)
	}
}

func TestOpenAIResponsesSuffixRejectsDecodedUnsafePath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{
		"/v1/responses/..%2f..%2fx/y",
		"/responses/%2e%2e%2fx",
		"/backend-api/codex/responses/%3fa=b",
		"/v1/responses/%00compact",
		"/responses/%E6%A8%A1%E5%9E%8B",
		"/v1/responses/compact%20",
		"/v1/responses/bad%3fvalue/responses/compact",
		"/v1/responses" + strings.Repeat("/a", maxUpstreamPathSegments+1) + "/responses/compact",
	} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, path, nil)
		require.False(t, IsForwardableOpenAIResponsesRequestPath(c), path)
		require.Empty(t, openAIResponsesRequestPathSuffix(c), path)
		require.False(t, isOpenAIResponsesCompactPath(c), path)
	}
}

func TestOpenAIResponsesSuffixUsesMatchedRouteWildcard(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/bad%3fvalue/responses/compact", nil)
	c.Params = gin.Params{{Key: "subpath", Value: "/bad?value/responses/compact"}}
	c.FullPath()

	require.False(t, IsForwardableOpenAIResponsesRequestPath(c))
	require.Empty(t, openAIResponsesRequestPathSuffix(c))
}

func TestOpenAIResponsesCompactTrailingSlashNormalizes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact/", nil)
	c.Params = gin.Params{{Key: "subpath", Value: "/compact/"}}

	require.True(t, IsForwardableOpenAIResponsesRequestPath(c))
	require.Equal(t, "/compact", openAIResponsesRequestPathSuffix(c))
	require.True(t, isOpenAIResponsesCompactPath(c))
}

func TestAppendOpenAIResponsesRequestPathSuffixRefusesUnsafeSuffix(t *testing.T) {
	require.Equal(t, chatgptCodexURL, appendOpenAIResponsesRequestPathSuffix(chatgptCodexURL, "/../../x"))
	require.Equal(t, chatgptCodexURL, appendOpenAIResponsesRequestPathSuffix(chatgptCodexURL, "/?a=b"))
	require.Equal(t, chatgptCodexURL+"/compact", appendOpenAIResponsesRequestPathSuffix(chatgptCodexURL, "/compact"))
}
