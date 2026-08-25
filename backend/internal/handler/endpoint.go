package handler

import (
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// ──────────────────────────────────────────────────────────
// Canonical inbound / upstream endpoint paths.
// All normalization and derivation reference this single set
// of constants — add new paths HERE when a new API surface
// is introduced.
// ──────────────────────────────────────────────────────────

const (
	EndpointMessages          = "/v1/messages"
	EndpointChatCompletions   = "/v1/chat/completions"
	EndpointEmbeddings        = "/v1/embeddings"
	EndpointAlphaSearch       = "/v1/alpha/search"
	EndpointResponses         = "/v1/responses"
	EndpointResponsesCompact  = "/v1/responses/compact"
	EndpointImagesGenerations = "/v1/images/generations"
	EndpointImagesEdits       = "/v1/images/edits"
	EndpointGeminiModels      = "/v1beta/models"
)

// gin.Context keys used by the middleware and helpers below.
const (
	ctxKeyInboundEndpoint = "_gateway_inbound_endpoint"
)

// ──────────────────────────────────────────────────────────
// Normalization functions
// ──────────────────────────────────────────────────────────

// NormalizeInboundEndpoint maps a raw request path (which may carry
// prefixes like /antigravity, /openai) to its canonical form.
//
//	"/antigravity/v1/messages"   → "/v1/messages"
//	"/v1/chat/completions"       → "/v1/chat/completions"
//	"/openai/v1/responses/foo"   → "/v1/responses"
//	"/v1beta/models/gemini:gen"  → "/v1beta/models"
func NormalizeInboundEndpoint(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return path
	}

	bestEndpoint := ""
	bestIndex := len(path) + 1
	consider := func(endpoint, pattern string) {
		idx := strings.Index(path, pattern)
		if idx >= 0 && idx < bestIndex {
			bestEndpoint = endpoint
			bestIndex = idx
		}
	}

	considerBoundary := func(endpoint, pattern string) {
		idx := endpointIndex(path, pattern)
		if idx >= 0 && idx < bestIndex {
			bestEndpoint = endpoint
			bestIndex = idx
		}
	}

	consider(EndpointEmbeddings, EndpointEmbeddings)
	consider(EndpointChatCompletions, EndpointChatCompletions)
	consider(EndpointMessages, EndpointMessages)
	considerBoundary(EndpointAlphaSearch, EndpointAlphaSearch)
	considerBoundary(EndpointAlphaSearch, "/alpha/search")
	considerBoundary(EndpointAlphaSearch, "/backend-api/codex/alpha/search")
	consider(EndpointImagesGenerations, EndpointImagesGenerations)
	consider(EndpointImagesGenerations, "/images/generations")
	consider(EndpointImagesEdits, EndpointImagesEdits)
	consider(EndpointImagesEdits, "/images/edits")
	considerBoundary(EndpointResponsesCompact, EndpointResponsesCompact)
	considerBoundary(EndpointResponsesCompact, "/responses/compact")
	considerBoundary(EndpointResponsesCompact, "/backend-api/codex/responses/compact")
	considerBoundary(EndpointResponses, EndpointResponses)
	considerBoundary(EndpointResponses, "/responses")
	considerBoundary(EndpointResponses, "/backend-api/codex/responses")
	consider(EndpointGeminiModels, EndpointGeminiModels)

	if bestEndpoint != "" {
		return bestEndpoint
	}
	return path
}

func endpointIndex(path, endpoint string) int {
	searchFrom := 0
	for searchFrom < len(path) {
		idx := strings.Index(path[searchFrom:], endpoint)
		if idx < 0 {
			return -1
		}
		idx += searchFrom
		next := idx + len(endpoint)
		if next == len(path) || strings.ContainsRune("/?#", rune(path[next])) {
			return idx
		}
		searchFrom = idx + 1
	}
	return -1
}

// DeriveUpstreamEndpoint determines the upstream endpoint from the
// account platform and the normalized inbound endpoint.
//
// Platform-specific rules:
//   - OpenAI always forwards to /v1/responses (with optional subpath
//     such as /v1/responses/compact preserved from the raw URL).
//   - Anthropic  → /v1/messages
//   - Gemini     → /v1beta/models
//   - Antigravity → /v1/messages (Claude) or gemini (Gemini)
//   - Antigravity routes may target either Claude or Gemini, so the
//     inbound endpoint is used to distinguish.
func DeriveUpstreamEndpoint(inbound, rawRequestPath, platform string) string {
	inbound = strings.TrimSpace(inbound)

	switch platform {
	case service.PlatformOpenAI:
		if inbound == EndpointEmbeddings || inbound == EndpointAlphaSearch || inbound == EndpointImagesGenerations || inbound == EndpointImagesEdits {
			return inbound
		}
		// OpenAI forwards everything to the Responses API.
		// Preserve subresource suffix (e.g. /v1/responses/compact).
		if inbound == EndpointResponses || inbound == EndpointResponsesCompact {
			if suffix := responsesSubpathSuffix(rawRequestPath); suffix != "" {
				return EndpointResponses + suffix
			}
			if inbound == EndpointResponsesCompact {
				return EndpointResponsesCompact
			}
		}
		return EndpointResponses

	case service.PlatformAnthropic:
		return EndpointMessages

	case service.PlatformGemini:
		return EndpointGeminiModels

	case service.PlatformAntigravity:
		// Antigravity accounts serve both Claude and Gemini.
		if inbound == EndpointGeminiModels {
			return EndpointGeminiModels
		}
		return EndpointMessages
	}

	// Unknown platform — fall back to inbound.
	return inbound
}

// responsesSubpathSuffix extracts the part after "/responses" in a raw
// request path, e.g. "/openai/v1/responses/compact" → "/compact".
// Returns "" when there is no meaningful suffix.
func responsesSubpathSuffix(rawPath string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(rawPath), "/")
	idx := endpointIndex(trimmed, "/responses")
	if idx < 0 {
		return ""
	}
	suffix := trimmed[idx+len("/responses"):]
	if suffix == "" || suffix == "/" {
		return ""
	}
	if !strings.HasPrefix(suffix, "/") {
		return ""
	}
	return suffix
}

// ──────────────────────────────────────────────────────────
// Middleware
// ──────────────────────────────────────────────────────────

// InboundEndpointMiddleware normalizes the request path and stores the
// canonical inbound endpoint in gin.Context so that every handler in
// the chain can read it via GetInboundEndpoint.
//
// Apply this middleware to all gateway route groups.
func InboundEndpointMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := ""
		if c.Request != nil && c.Request.URL != nil {
			path = c.Request.URL.Path
		}
		if path == "" {
			path = c.FullPath()
		}
		c.Set(ctxKeyInboundEndpoint, NormalizeInboundEndpoint(path))
		c.Next()
	}
}

// ──────────────────────────────────────────────────────────
// Context helpers — used by handlers before building
// RecordUsageInput / RecordUsageLongContextInput.
// ──────────────────────────────────────────────────────────

// GetInboundEndpoint returns the canonical inbound endpoint stored by
// InboundEndpointMiddleware. If the middleware did not run (e.g. in
// tests), it falls back to normalizing c.Request.URL.Path on the fly.
func GetInboundEndpoint(c *gin.Context) string {
	if v, ok := c.Get(ctxKeyInboundEndpoint); ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	// Fallback: normalize on the fly.
	path := ""
	if c != nil {
		if c.Request != nil && c.Request.URL != nil {
			path = c.Request.URL.Path
		}
		if path == "" {
			path = c.FullPath()
		}
	}
	return NormalizeInboundEndpoint(path)
}

// GetUpstreamEndpoint derives the upstream endpoint from the context
// and the account platform. Handlers call this after scheduling an
// account, passing account.Platform.
func GetUpstreamEndpoint(c *gin.Context, platform string) string {
	inbound := GetInboundEndpoint(c)
	rawPath := ""
	if c != nil && c.Request != nil && c.Request.URL != nil {
		rawPath = c.Request.URL.Path
	}
	return DeriveUpstreamEndpoint(inbound, rawPath, platform)
}
