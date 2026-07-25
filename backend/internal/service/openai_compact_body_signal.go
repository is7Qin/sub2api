package service

import (
	"context"
	"path"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const openAIRemoteCompactionSemanticOutcomeKey = "openai_remote_compaction_semantic_outcome"

type (
	openAIRemoteCompactionContextKey      struct{}
	OpenAIRemoteCompactionSemanticOutcome string
)

const (
	OpenAIRemoteCompactionSemanticOutcomeSucceeded OpenAIRemoteCompactionSemanticOutcome = "succeeded"
	OpenAIRemoteCompactionSemanticOutcomeFailed    OpenAIRemoteCompactionSemanticOutcome = "failed"
)

func HasOpenAICompactionTriggerInInput(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return false
	}
	found := false
	input.ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "compaction_trigger" {
			found = true
			return false
		}
		return true
	})
	return found
}

func isBareOpenAIResponsesPath(c *gin.Context) bool {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return false
	}
	normalizedPath := path.Clean(strings.TrimSpace(c.Request.URL.Path))
	switch normalizedPath {
	case "/v1/responses", "/responses", "/backend-api/codex/responses":
		return true
	default:
		return false
	}
}

// PromoteOpenAICompactBodySignal is retained for compatibility. It marks an
// official Codex compaction trigger for telemetry without promoting the request
// to the legacy /responses/compact endpoint.
func PromoteOpenAICompactBodySignal(c *gin.Context, body []byte, forceCodexCLI bool) bool {
	if c == nil || c.Request == nil || !isBareOpenAIResponsesPath(c) || !HasOpenAICompactionTriggerInInput(body) {
		return false
	}
	if !forceCodexCLI && !openai.IsCodexOfficialClientByHeadersStrict(c.GetHeader("User-Agent"), c.GetHeader("originator")) {
		return false
	}
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), openAIRemoteCompactionContextKey{}, true))
	return true
}

func isOpenAIRemoteCompactionRequest(c *gin.Context) bool {
	if c == nil || c.Request == nil {
		return false
	}
	marked, _ := c.Request.Context().Value(openAIRemoteCompactionContextKey{}).(bool)
	return marked
}

func IsOpenAIRemoteCompactionRequest(c *gin.Context) bool {
	return isOpenAIRemoteCompactionRequest(c)
}

// SetOpenAIRemoteCompactionSemanticOutcome annotates only marked V2 requests;
// ordinary Responses and legacy compact requests retain their existing handling.
func SetOpenAIRemoteCompactionSemanticOutcome(c *gin.Context, outcome OpenAIRemoteCompactionSemanticOutcome) {
	if !IsOpenAIRemoteCompactionRequest(c) || (outcome != OpenAIRemoteCompactionSemanticOutcomeSucceeded && outcome != OpenAIRemoteCompactionSemanticOutcomeFailed) {
		return
	}
	c.Set(openAIRemoteCompactionSemanticOutcomeKey, outcome)
}

func setOpenAIRemoteCompactionNonStreamingOutcome(c *gin.Context, body []byte) {
	status := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "status").String()))
	switch status {
	case "failed", "incomplete", "cancelled", "canceled":
		SetOpenAIRemoteCompactionSemanticOutcome(c, OpenAIRemoteCompactionSemanticOutcomeFailed)
	default:
		SetOpenAIRemoteCompactionSemanticOutcome(c, OpenAIRemoteCompactionSemanticOutcomeSucceeded)
	}
}

func GetOpenAIRemoteCompactionSemanticOutcome(c *gin.Context) (OpenAIRemoteCompactionSemanticOutcome, bool) {
	if c == nil {
		return "", false
	}
	value, ok := c.Get(openAIRemoteCompactionSemanticOutcomeKey)
	if !ok {
		return "", false
	}
	outcome, ok := value.(OpenAIRemoteCompactionSemanticOutcome)
	return outcome, ok && (outcome == OpenAIRemoteCompactionSemanticOutcomeSucceeded || outcome == OpenAIRemoteCompactionSemanticOutcomeFailed)
}

// IsOpenAICompactBodySignalRequest is retained for compatibility; the marker
// denotes Remote Compaction V2, not legacy compact.
func IsOpenAICompactBodySignalRequest(c *gin.Context) bool {
	return isOpenAIRemoteCompactionRequest(c)
}
