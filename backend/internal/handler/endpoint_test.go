package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func init() { gin.SetMode(gin.TestMode) }

// ──────────────────────────────────────────────────────────
// NormalizeInboundEndpoint
// ──────────────────────────────────────────────────────────

func TestNormalizeInboundEndpoint(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		// Direct canonical paths.
		{"/v1/messages", EndpointMessages},
		{"/v1/chat/completions", EndpointChatCompletions},
		{"/v1/embeddings", EndpointEmbeddings},
		{"/v1/alpha/search", EndpointAlphaSearch},
		{"/v1/responses", EndpointResponses},
		{"/v1/images/generations", EndpointImagesGenerations},
		{"/v1/images/edits", EndpointImagesEdits},
		{"/v1beta/models", EndpointGeminiModels},

		// Prefixed paths (antigravity, openai).
		{"/antigravity/v1/messages", EndpointMessages},
		{"/openai/v1/responses", EndpointResponses},
		{"/openai/v1/responses/compact", EndpointResponsesCompact},
		{"/responses", EndpointResponses},
		{"/responses/compact", EndpointResponsesCompact},
		{"/responses/compact/", EndpointResponsesCompact},
		{"/responses/compact?stream=false", EndpointResponsesCompact},
		{"/backend-api/codex/responses/compact/detail", EndpointResponsesCompact},
		{"/backend-api/codex/responses", EndpointResponses},
		{"/alpha/search", EndpointAlphaSearch},
		{"/backend-api/codex/alpha/search", EndpointAlphaSearch},
		{"/openai/v1/alpha/search", EndpointAlphaSearch},
		{"/v1/alpha/searchX", "/v1/alpha/searchX"},
		{"/prefix/v1/messages/later/v1/responses/compact", EndpointMessages},
		{"/responses/compact/later/responses/foo", EndpointResponsesCompact},
		{"/openai/v1/images/generations", EndpointImagesGenerations},
		{"/openai/v1/images/edits", EndpointImagesEdits},
		{"/antigravity/v1beta/models/gemini:generateContent", EndpointGeminiModels},

		// Gin route patterns with wildcards.
		{"/v1beta/models/*modelAction", EndpointGeminiModels},
		{"/v1/responses/*subpath", EndpointResponses},
		{"/v1/responses/compact/v1/messages", EndpointResponsesCompact},

		// Endpoint-like substrings are not endpoint segments.
		{"/v1/responses/compactness", EndpointResponses},
		{"/v1/responses/compact-old", EndpointResponses},
		{"/v1/responses-old", "/v1/responses-old"},
		{"/v1/responsesX", "/v1/responsesX"},
		{"/prefix/v1/responses-old/v1/messages", EndpointMessages},
		{"/prefix/responsesX/v1/responses/compact", EndpointResponsesCompact},

		// Unknown path is returned as-is.
		{"/v1/embeddings", "/v1/embeddings"},
		{"", ""},
		{"  /v1/messages  ", EndpointMessages},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			require.Equal(t, tt.want, NormalizeInboundEndpoint(tt.path))
		})
	}
}

// ──────────────────────────────────────────────────────────
// DeriveUpstreamEndpoint
// ──────────────────────────────────────────────────────────

func TestDeriveUpstreamEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		inbound  string
		rawPath  string
		platform string
		want     string
	}{
		// Anthropic.
		{"anthropic messages", EndpointMessages, "/v1/messages", service.PlatformAnthropic, EndpointMessages},

		// Gemini.
		{"gemini models", EndpointGeminiModels, "/v1beta/models/gemini:gen", service.PlatformGemini, EndpointGeminiModels},

		// OpenAI — always /v1/responses.
		{"openai responses root", EndpointResponses, "/v1/responses", service.PlatformOpenAI, EndpointResponses},
		{"openai responses alias root", EndpointResponses, "/responses/", service.PlatformOpenAI, EndpointResponses},
		{"openai responses compact", EndpointResponses, "/openai/v1/responses/compact", service.PlatformOpenAI, "/v1/responses/compact"},
		{"openai compact canonical inbound", EndpointResponsesCompact, "/responses/compact/", service.PlatformOpenAI, EndpointResponsesCompact},
		{"openai responses nested", EndpointResponses, "/openai/v1/responses/compact/detail", service.PlatformOpenAI, "/v1/responses/compact/detail"},
		{"openai repeated responses uses normalized occurrence", EndpointResponsesCompact, "/responses/compact/later/responses/foo", service.PlatformOpenAI, "/v1/responses/compact/later/responses/foo"},
		{"openai skips invalid response occurrence", EndpointResponsesCompact, "/responses-old/later/responses/compact", service.PlatformOpenAI, EndpointResponsesCompact},
		{"openai from messages", EndpointMessages, "/v1/messages", service.PlatformOpenAI, EndpointResponses},
		{"openai from messages ignores later responses", EndpointMessages, "/v1/messages/later/responses/compact", service.PlatformOpenAI, EndpointResponses},
		{"openai from completions", EndpointChatCompletions, "/v1/chat/completions", service.PlatformOpenAI, EndpointResponses},
		{"openai embeddings", EndpointEmbeddings, "/v1/embeddings", service.PlatformOpenAI, EndpointEmbeddings},
		{"openai alpha search", EndpointAlphaSearch, "/backend-api/codex/alpha/search", service.PlatformOpenAI, EndpointAlphaSearch},
		{"openai image generations", EndpointImagesGenerations, "/v1/images/generations", service.PlatformOpenAI, EndpointImagesGenerations},
		{"openai image edits", EndpointImagesEdits, "/openai/v1/images/edits", service.PlatformOpenAI, EndpointImagesEdits},

		// Antigravity — uses inbound to pick Claude vs Gemini upstream.
		{"antigravity claude", EndpointMessages, "/antigravity/v1/messages", service.PlatformAntigravity, EndpointMessages},
		{"antigravity gemini", EndpointGeminiModels, "/antigravity/v1beta/models", service.PlatformAntigravity, EndpointGeminiModels},

		// Unknown platform — passthrough.
		{"unknown platform", "/v1/embeddings", "/v1/embeddings", "unknown", "/v1/embeddings"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, DeriveUpstreamEndpoint(tt.inbound, tt.rawPath, tt.platform))
		})
	}
}

// ──────────────────────────────────────────────────────────
// responsesSubpathSuffix
// ──────────────────────────────────────────────────────────

func TestResponsesSubpathSuffix(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{"/v1/responses", ""},
		{"/v1/responses/", ""},
		{"/v1/responses/compact", "/compact"},
		{"/openai/v1/responses/compact/detail", "/compact/detail"},
		{"/responses/compact/later/responses/foo", "/compact/later/responses/foo"},
		{"/responses-old/later/responses/compact", "/compact"},
		{"/responsesX/later/responses/foo", "/foo"},
		{"/responses-old", ""},
		{"/responsesX", ""},
		{"/v1/messages", ""},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			require.Equal(t, tt.want, responsesSubpathSuffix(tt.raw))
		})
	}
}

// ──────────────────────────────────────────────────────────
// InboundEndpointMiddleware + context helpers
// ──────────────────────────────────────────────────────────

func TestInboundEndpointMiddleware(t *testing.T) {
	router := gin.New()
	router.Use(InboundEndpointMiddleware())

	var captured string
	router.POST("/v1/messages", func(c *gin.Context) {
		captured = GetInboundEndpoint(c)
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, EndpointMessages, captured)
}

func TestGetInboundEndpoint_FallbackWithoutMiddleware(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/antigravity/v1/messages", nil)

	// Middleware did not run — fallback to normalizing c.Request.URL.Path.
	got := GetInboundEndpoint(c)
	require.Equal(t, EndpointMessages, got)
}

func TestInboundEndpointMiddleware_WildcardRouteUsesRawRequestPath(t *testing.T) {
	router := gin.New()
	router.Use(InboundEndpointMiddleware())

	var inbound string
	var upstream string
	router.POST("/v1/responses/*subpath", func(c *gin.Context) {
		require.Equal(t, "/v1/responses/*subpath", c.FullPath())
		inbound = GetInboundEndpoint(c)
		upstream = GetUpstreamEndpoint(c, service.PlatformOpenAI)
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, EndpointResponsesCompact, inbound)
	require.Equal(t, "/v1/responses/compact", upstream)
}

func TestGetInboundEndpoint_FallbackWildcardRouteUsesRawRequestPath(t *testing.T) {
	router := gin.New()

	var inbound string
	var upstream string
	router.POST("/v1/responses/*subpath", func(c *gin.Context) {
		require.Equal(t, "/v1/responses/*subpath", c.FullPath())
		inbound = GetInboundEndpoint(c)
		upstream = GetUpstreamEndpoint(c, service.PlatformOpenAI)
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, EndpointResponsesCompact, inbound)
	require.Equal(t, "/v1/responses/compact", upstream)
}

func TestGetUpstreamEndpoint_FullFlow(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses/compact", nil)

	// Simulate middleware.
	c.Set(ctxKeyInboundEndpoint, NormalizeInboundEndpoint(c.Request.URL.Path))

	got := GetUpstreamEndpoint(c, service.PlatformOpenAI)
	require.Equal(t, "/v1/responses/compact", got)
}

func TestResolveOpenAIUpstreamEndpointUsesChatOnlyAPIKeyAcrossIngresses(t *testing.T) {
	for _, inbound := range []string{EndpointChatCompletions, EndpointMessages, EndpointResponses} {
		t.Run(inbound, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, inbound, nil)
			c.Set(ctxKeyInboundEndpoint, inbound)

			account := &service.Account{
				Platform: service.PlatformOpenAI,
				Type:     service.AccountTypeAPIKey,
				Extra: map[string]any{
					openai_compat.ExtraKeyResponsesSupported: false,
				},
			}

			got := resolveOpenAIUpstreamEndpoint(c, account, &service.OpenAIForwardResult{})
			require.Equal(t, EndpointChatCompletions, got)
		})
	}
}

func TestResolveOpenAIUpstreamEndpointUsesResultUpstreamModel(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Set(ctxKeyInboundEndpoint, EndpointMessages)

	account := &service.Account{
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeAPIKey,
		Extra: map[string]any{
			openai_compat.ExtraKeyResponsesSupported: true,
			openai_compat.ExtraKeyResponsesSupportedByModel: map[string]any{
				"mapped-raw-model": false,
			},
		},
	}

	got := resolveOpenAIUpstreamEndpoint(c, account, &service.OpenAIForwardResult{UpstreamModel: "mapped-raw-model"})
	require.Equal(t, EndpointChatCompletions, got)
}

func TestResolveOpenAIUpstreamEndpointPrefersResultOverride(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Set(ctxKeyInboundEndpoint, EndpointChatCompletions)

	account := &service.Account{
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeAPIKey,
		Extra: map[string]any{
			openai_compat.ExtraKeyResponsesSupported: true,
		},
	}

	got := resolveOpenAIUpstreamEndpoint(c, account, &service.OpenAIForwardResult{UpstreamEndpoint: EndpointChatCompletions})
	require.Equal(t, EndpointChatCompletions, got)
}
