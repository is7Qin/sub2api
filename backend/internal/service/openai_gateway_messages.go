package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

const openAIMessagesErrorMessageMaxBytes = 4 * 1024

func boundOpenAIMessagesErrorMessage(message string) string {
	message = sanitizeOpenAIUpstreamDiagnosticText(message)
	if len(message) <= openAIMessagesErrorMessageMaxBytes {
		return message
	}

	message = message[:openAIMessagesErrorMessageMaxBytes]
	for !utf8.ValidString(message) {
		message = message[:len(message)-1]
	}
	return message
}

func extractOpenAIMessagesRawSSEErrorMessage(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	for _, path := range []string{"response.error.message", "error.message", "message"} {
		if message := strings.TrimSpace(gjson.GetBytes(payload, path).String()); message != "" {
			return message
		}
	}
	return strings.TrimSpace(extractUpstreamErrorMessage(payload))
}

// ForwardAsAnthropic accepts an Anthropic Messages request body, converts it
// to OpenAI Responses API format, forwards to the OpenAI upstream, and converts
// the response back to Anthropic Messages format. This enables Claude Code
// clients to access OpenAI models through the standard /v1/messages endpoint.
func (s *OpenAIGatewayService) ForwardAsAnthropic(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	promptCacheKey string,
	defaultMappedModel string,
) (*OpenAIForwardResult, error) {
	ctx = withHTTPAttemptAuthority(ctx)
	startTime := time.Now()

	// 1. Parse Anthropic request
	var anthropicReq apicompat.AnthropicRequest
	if err := json.Unmarshal(body, &anthropicReq); err != nil {
		return nil, fmt.Errorf("parse anthropic request: %w", err)
	}
	anthropicDigestReq := cloneAnthropicRequestForDigest(&anthropicReq)
	originalModel := anthropicReq.Model
	applyOpenAICompatModelNormalization(&anthropicReq)
	normalizedModel := anthropicReq.Model
	clientStream := anthropicReq.Stream // client's original stream preference

	// 2. Model mapping
	billingModel := resolveOpenAIMessagesForwardModel(account, normalizedModel, defaultMappedModel)
	upstreamModel := normalizeOpenAIModelForUpstream(account, billingModel)
	promptCacheKey = strings.TrimSpace(promptCacheKey)
	apiKeyID := getAPIKeyIDFromContext(c)
	anthropicDigestChain := ""
	anthropicMatchedDigestChain := ""
	compatPromptCacheInjected := false
	if promptCacheKey == "" && shouldAutoInjectPromptCacheKeyForCompat(upstreamModel) {
		promptCacheKey = promptCacheKeyFromAnthropicMetadataSession(&anthropicReq)
		if promptCacheKey == "" {
			promptCacheKey = deriveAnthropicCacheControlPromptCacheKey(&anthropicReq)
		}
		if promptCacheKey == "" {
			anthropicDigestChain = buildOpenAICompatAnthropicDigestChain(anthropicDigestReq)
			if reusedKey, matchedChain := s.findOpenAICompatAnthropicDigestPromptCacheKey(account, apiKeyID, anthropicDigestChain); reusedKey != "" {
				promptCacheKey = reusedKey
				anthropicMatchedDigestChain = matchedChain
			} else {
				promptCacheKey = promptCacheKeyFromAnthropicDigest(anthropicDigestChain)
			}
		}
		compatPromptCacheInjected = promptCacheKey != ""
	}
	compatReplayGuardEnabled := shouldAutoInjectPromptCacheKeyForCompat(upstreamModel)
	compatContinuationEnabled := openAICompatContinuationEnabled(account, upstreamModel)
	previousResponseID := ""
	if compatContinuationEnabled {
		previousResponseID = s.getOpenAICompatSessionResponseID(ctx, c, account, promptCacheKey)
	}
	compatTurnState := ""

	// 3. Convert the complete client-maintained Anthropic history. Only a valid
	// previous_response_id permits reducing the replay to the latest turn below.
	responsesReq, err := apicompat.AnthropicToResponses(&anthropicReq)
	if err != nil {
		return nil, fmt.Errorf("convert anthropic to responses: %w", err)
	}

	// Upstream always uses streaming (upstream may not support sync mode).
	// The client's original preference determines the response format.
	responsesReq.Stream = true
	isStream := true

	// 3b. Handle BetaFastMode → service_tier: "priority"
	if containsBetaToken(c.GetHeader("anthropic-beta"), claude.BetaFastMode) {
		responsesReq.ServiceTier = "priority"
	}

	responsesReq.Model = upstreamModel
	if previousResponseID != "" {
		responsesReq.PreviousResponseID = previousResponseID
		trimAnthropicCompatResponsesInputToLatestTurn(responsesReq)
	}
	if compatReplayGuardEnabled && !account.IsOpenAIOAuthLike() {
		appendOpenAICompatClaudeCodeTodoGuard(responsesReq)
	}

	logFields := []zap.Field{
		zap.Int64("account_id", account.ID),
		zap.String("original_model", originalModel),
		zap.String("normalized_model", normalizedModel),
		zap.String("billing_model", billingModel),
		zap.String("upstream_model", upstreamModel),
		zap.Bool("stream", isStream),
	}
	if compatPromptCacheInjected {
		logFields = append(logFields,
			zap.Bool("compat_prompt_cache_key_injected", true),
			zap.String("compat_prompt_cache_key_sha256", hashSensitiveValueForLog(promptCacheKey)),
		)
	}
	if previousResponseID != "" {
		logFields = append(logFields,
			zap.Bool("compat_previous_response_id_attached", true),
			zap.String("compat_previous_response_id", truncateOpenAIWSLogValue(previousResponseID, openAIWSIDValueMaxLen)),
		)
	}
	if compatTurnState != "" {
		logFields = append(logFields, zap.Bool("compat_turn_state_attached", true))
	}
	logger.L().Debug("openai messages: model mapping applied", logFields...)

	// 4. Marshal Responses request body, then apply OAuth codex transform
	responsesBody, err := json.Marshal(responsesReq)
	if err != nil {
		return nil, fmt.Errorf("marshal responses request: %w", err)
	}

	if account.IsOpenAIOAuthLike() {
		reqBody, err := decodeOpenAIRequestBodyMapUseNumber(responsesBody)
		if err != nil {
			return nil, fmt.Errorf("unmarshal for codex transform: %w", err)
		}
		codexResult := applyCodexOAuthTransformWithOptions(reqBody, codexOAuthTransformOptions{
			SkipDefaultInstructions: true,
			PreserveToolCallIDs:     true,
		})
		forcedTemplateText := ""
		if s.cfg != nil {
			forcedTemplateText = s.cfg.Gateway.ForcedCodexInstructionsTemplate
		}
		templateUpstreamModel := upstreamModel
		if codexResult.NormalizedModel != "" {
			templateUpstreamModel = codexResult.NormalizedModel
		}
		existingInstructions, _ := reqBody["instructions"].(string)
		if strings.TrimSpace(existingInstructions) == "" {
			existingInstructions = extractPromptLikeInstructionsFromInput(reqBody)
		}
		if _, err := applyForcedCodexInstructionsTemplate(reqBody, forcedTemplateText, forcedCodexInstructionsTemplateData{
			ExistingInstructions: strings.TrimSpace(existingInstructions),
			OriginalModel:        originalModel,
			NormalizedModel:      normalizedModel,
			BillingModel:         billingModel,
			UpstreamModel:        templateUpstreamModel,
		}); err != nil {
			return nil, err
		}
		ensureCodexOAuthInstructionsField(reqBody)
		if shouldAutoInjectPromptCacheKeyForCompat(upstreamModel) {
			appendOpenAICompatClaudeCodeTodoGuardToRequestBody(reqBody)
		}
		if codexResult.NormalizedModel != "" {
			upstreamModel = codexResult.NormalizedModel
		}
		if codexResult.PromptCacheKey != "" {
			promptCacheKey = codexResult.PromptCacheKey
		}
		delete(reqBody, "prompt_cache_key")
		if shouldAutoInjectPromptCacheKeyForCompat(upstreamModel) {
			compatTurnState = s.getOpenAICompatSessionTurnState(ctx, c, account, promptCacheKey)
		}
		// OAuth codex transform forces stream=true upstream, so always use
		// the streaming response handler regardless of what the client asked.
		isStream = true
		responsesBody, err = marshalOpenAIUpstreamJSON(reqBody)
		if err != nil {
			return nil, fmt.Errorf("remarshal after codex transform: %w", err)
		}
	}

	// For API key accounts (including OpenAI-compatible upstream gateways),
	// ensure promptCacheKey is also propagated via the request body so that
	// upstreams using the Responses API can derive a stable session identifier
	// from prompt_cache_key. This makes our Anthropic /v1/messages compatibility
	// path behave more like a native Responses client.
	if account.Type == AccountTypeAPIKey {
		if trimmedKey := strings.TrimSpace(promptCacheKey); trimmedKey != "" {
			var reqBody map[string]any
			if err := json.Unmarshal(responsesBody, &reqBody); err != nil {
				return nil, fmt.Errorf("unmarshal for prompt cache key injection: %w", err)
			}
			if existing, ok := reqBody["prompt_cache_key"].(string); !ok || strings.TrimSpace(existing) == "" {
				reqBody["prompt_cache_key"] = trimmedKey
				updated, err := json.Marshal(reqBody)
				if err != nil {
					return nil, fmt.Errorf("remarshal after prompt cache key injection: %w", err)
				}
				responsesBody = updated
			}
		}
	}

	forcedBody, forceErr := forceOpenAIPriorityTierInBody(getAPIKeyFromContext(c), responsesBody)
	if forceErr != nil {
		return nil, forceErr
	}
	responsesBody = forcedBody

	// 4c. Apply OpenAI fast policy (may filter service_tier or block the request).
	// Mirrors the Claude anthropic-beta "fast-mode-2026-02-01" filter, but keyed
	// on the body-level service_tier field (priority/flex).
	updatedBody, policyErr := s.applyOpenAIFastPolicyToBody(ctx, account, upstreamModel, responsesBody)
	if policyErr != nil {
		var blocked *OpenAIFastBlockedError
		if errors.As(policyErr, &blocked) {
			MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalPolicyDenied)
			writeAnthropicError(c, http.StatusForbidden, "forbidden_error", blocked.Message)
		}
		return nil, policyErr
	}
	responsesBody = updatedBody

	// Refresh ServiceTier so billing reflects the final body sent upstream.
	if responsesReq != nil {
		responsesReq.ServiceTier = strings.TrimSpace(gjson.GetBytes(responsesBody, "service_tier").String())
	}

	// 5. Get access token
	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, fmt.Errorf("get access token: %w", err)
	}

	// 6. Build upstream request
	setOpenAICompatMessagesBridgeContext(c, account.IsOpenAIOAuthLike())
	upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
	upstreamReq, err := s.buildUpstreamRequest(upstreamCtx, c, account, responsesBody, token, isStream, promptCacheKey, false)
	releaseUpstreamCtx()
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}

	// Override session_id with a deterministic UUID derived from the isolated
	// session key, ensuring different API keys produce different upstream sessions.
	if promptCacheKey != "" {
		isolatedSessionID := ""
		if account.IsOpenAIOAuthLike() {
			// Keep OAuth-like compatibility traffic UUID-shaped instead of seeding
			// Codex identity from the legacy 16-hex isolation helper.
			isolatedSessionID = isolateOpenAICodexOAuthSessionID(apiKeyID, promptCacheKey, "session")
		} else {
			isolatedSessionID = generateSessionUUID(isolateOpenAISessionID(apiKeyID, promptCacheKey))
		}
		upstreamReq.Header.Set("session_id", isolatedSessionID)
		if upstreamReq.Header.Get("conversation_id") != "" {
			upstreamReq.Header.Set("conversation_id", isolatedSessionID)
		}
	}
	if account.IsOpenAIOAuthLike() {
		// Anthropic Messages compatibility uses the ChatGPT Codex SSE endpoint.
		// Match airgate-openai's request shape: the SSE endpoint does not need
		// the Responses experimental beta header; Codex identity headers remain
		// fingerprint-owned for OAuth-like accounts.
		upstreamReq.Header.Del("OpenAI-Beta")
	}
	if account.IsOpenAIOAuthLike() && promptCacheKey != "" && strings.TrimSpace(c.GetHeader("conversation_id")) == "" {
		upstreamReq.Header.Del("conversation_id")
	}
	if compatTurnState != "" && upstreamReq.Header.Get("x-codex-turn-state") == "" {
		upstreamReq.Header.Set("x-codex-turn-state", compatTurnState)
	}

	// 7. Send request
	proxyURL := ""
	if account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := doHTTPUpstream(ctx, s.httpUpstream, upstreamReq, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false)
	}
	defer func() { _ = resp.Body.Close() }()

	// 8. Handle error response with failover
	if resp.StatusCode >= 400 {
		respBody := s.readUpstreamErrorBody(resp)
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(respBody))

		upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(respBody))
		upstreamMsg = sanitizeOpenAIUpstreamDiagnosticText(upstreamMsg)
		if previousResponseID != "" && (isOpenAICompatPreviousResponseNotFound(resp.StatusCode, upstreamMsg, respBody) || isOpenAICompatPreviousResponseUnsupported(resp.StatusCode, upstreamMsg, respBody)) {
			if isOpenAICompatPreviousResponseUnsupported(resp.StatusCode, upstreamMsg, respBody) {
				s.disableOpenAICompatSessionContinuation(ctx, c, account, promptCacheKey)
			} else {
				s.deleteOpenAICompatSessionResponseID(ctx, c, account, promptCacheKey)
			}
			logger.L().Info("openai messages: previous_response_id unavailable, retrying without continuation",
				zap.Int64("account_id", account.ID),
				zap.String("previous_response_id", truncateOpenAIWSLogValue(previousResponseID, openAIWSIDValueMaxLen)),
				zap.String("upstream_model", upstreamModel),
			)
			return s.ForwardAsAnthropic(ctx, c, account, body, promptCacheKey, defaultMappedModel)
		}
		if s.shouldFailoverOpenAIUpstreamResponse(resp.StatusCode, upstreamMsg, respBody) {
			upstreamDetail := ""
			if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
				maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
				if maxBytes <= 0 {
					maxBytes = 2048
				}
				upstreamDetail = sanitizeOpenAIUpstreamDiagnosticBodyForLog(respBody, maxBytes)
			}
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: resp.StatusCode,
				UpstreamRequestID:  resp.Header.Get("x-request-id"),
				Kind:               "failover",
				Message:            upstreamMsg,
				Detail:             upstreamDetail,
			})
			s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody, upstreamModel)
			return nil, &UpstreamFailoverError{
				StatusCode:             resp.StatusCode,
				ResponseBody:           respBody,
				RetryableOnSameAccount: account.IsPoolMode() && (account.IsPoolModeRetryableStatus(resp.StatusCode) || isOpenAITransientProcessingError(resp.StatusCode, upstreamMsg, respBody)),
			}
		}
		// Non-failover error: return Anthropic-formatted error to client
		return s.handleAnthropicErrorResponse(resp, c, account, upstreamModel)
	}

	if account.IsOpenAIOAuthLike() && promptCacheKey != "" {
		if turnState := strings.TrimSpace(resp.Header.Get("x-codex-turn-state")); turnState != "" {
			s.bindOpenAICompatSessionTurnState(ctx, c, account, promptCacheKey, turnState)
		}
	}

	// 9. Handle normal response
	// Upstream is always streaming; choose response format based on client preference.
	var result *OpenAIForwardResult
	var handleErr error
	if clientStream {
		result, handleErr = s.handleAnthropicStreamingResponse(resp, c, account, originalModel, billingModel, upstreamModel, startTime)
	} else {
		// Client wants JSON: buffer the streaming response and assemble a JSON reply.
		result, handleErr = s.handleAnthropicBufferedStreamingResponse(resp, c, account, originalModel, billingModel, upstreamModel, startTime)
	}

	// Propagate billing annotations even when the stream ends with a post-output error;
	// session/cache bindings remain success-only because failed responses are not reusable.
	if result != nil {
		if handleErr == nil {
			if compatContinuationEnabled && promptCacheKey != "" && result.ResponseID != "" {
				s.bindOpenAICompatSessionResponseID(ctx, c, account, promptCacheKey, result.ResponseID)
			}
			if promptCacheKey != "" && anthropicDigestChain != "" {
				s.bindOpenAICompatAnthropicDigestPromptCacheKey(account, apiKeyID, anthropicDigestChain, promptCacheKey, anthropicMatchedDigestChain)
			}
		}
		if responsesReq.ServiceTier != "" {
			st := responsesReq.ServiceTier
			result.ServiceTier = &st
		}
		if responsesReq.Reasoning != nil && responsesReq.Reasoning.Effort != "" {
			re := responsesReq.Reasoning.Effort
			result.ReasoningEffort = &re
		}
	}

	// Extract and save Codex usage snapshot from response headers for OAuth-like
	// accounts; setup-token uses the same ChatGPT Codex surface as OAuth.
	if handleErr == nil && account.IsOpenAIOAuthLike() {
		if snapshot := ParseCodexRateLimitHeaders(resp.Header); snapshot != nil {
			s.updateCodexUsageSnapshot(ctx, account.ID, snapshot)
		}
	}

	return result, handleErr
}

func ensureCodexOAuthInstructionsField(reqBody map[string]any) {
	if reqBody == nil {
		return
	}
	if value, ok := reqBody["instructions"]; !ok || value == nil {
		reqBody["instructions"] = ""
		return
	}
	if _, ok := reqBody["instructions"].(string); !ok {
		reqBody["instructions"] = ""
	}
}

// handleAnthropicErrorResponse reads an upstream error and returns it in
// Anthropic error format.
func (s *OpenAIGatewayService) handleAnthropicErrorResponse(
	resp *http.Response,
	c *gin.Context,
	account *Account,
	requestedModel ...string,
) (*OpenAIForwardResult, error) {
	return s.handleCompatErrorResponse(resp, c, account, writeAnthropicError, requestedModel...)
}

// handleAnthropicBufferedStreamingResponse reads all Responses SSE events from
// the upstream streaming response, finds the terminal event (response.completed
// / response.incomplete / response.failed), converts the complete response to
// Anthropic Messages JSON format, and writes it to the client.
// This is used when the client requested stream=false but the upstream is always
// streaming.
func (s *OpenAIGatewayService) handleAnthropicBufferedStreamingResponse(
	resp *http.Response,
	c *gin.Context,
	account *Account,
	originalModel string,
	billingModel string,
	upstreamModel string,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	requestID := resp.Header.Get("x-request-id")

	finalResponse, usage, acc, terminalPayload, err := s.readOpenAICompatBufferedTerminal(resp, "openai messages buffered", requestID)
	if err != nil {
		return nil, err
	}

	if finalResponse == nil {
		message := "OpenAI messages stream ended before a terminal event"
		return nil, s.newOpenAIStreamFailoverError(c, account, false, requestID, nil, message)
	}
	if strings.TrimSpace(finalResponse.Status) == "failed" {
		payload := terminalPayload
		if len(payload) == 0 {
			payload, _ = json.Marshal(gin.H{"type": "response.failed", "response": finalResponse})
		}
		rawMessage := openAICompatFailedResponseMessage(finalResponse)
		if strings.TrimSpace(rawMessage) == "" {
			rawMessage = "OpenAI messages response failed"
		}
		if requestErr := newOpenAIUpstreamRequestError(payload, requestID); requestErr != nil {
			requestErr.attachUsage(usage)
			writeAnthropicError(c, requestErr.StatusCode, requestErr.AnthropicErrorType(), requestErr.Message)
			return &OpenAIForwardResult{
				RequestID: requestID, AttemptID: forwardResultAttemptID(resp), ResponseID: finalResponse.ID, Usage: usage,
				Model: originalModel, BillingModel: billingModel, UpstreamModel: upstreamModel,
				Stream: false, Duration: time.Since(startTime),
			}, requestErr
		}
		if openAIStreamFailedEventShouldFailover(payload, rawMessage) {
			message := boundOpenAIMessagesErrorMessage(rawMessage)
			return nil, s.newOpenAIStreamFailoverError(c, account, false, requestID, payload, message, resp.Header)
		}
		message := boundOpenAIMessagesErrorMessage(rawMessage)
		s.recordOpenAIMessagesStreamUpstreamError(c, account, requestID, upstreamURLFromResponse(resp), "stream_failed", message)
		// response.failed arrives inside an HTTP 200 SSE stream, so there is no upstream HTTP error status to pass through.
		status, errType, errMsg, matched := applyErrorPassthroughRule(
			c, account.Platform, 0, payload,
			http.StatusBadGateway, "api_error", message,
		)
		if status == 0 {
			status = http.StatusBadGateway
		}
		if strings.TrimSpace(errMsg) == "" {
			errMsg = message
		}
		errMsg = boundOpenAIMessagesErrorMessage(errMsg)
		if matched {
			MarkResponseCommitted(c)
			writeAnthropicError(c, status, errType, errMsg)
		} else {
			writeAnthropicError(c, http.StatusBadGateway, "api_error", message)
		}
		return &OpenAIForwardResult{
			RequestID:     requestID,
			AttemptID:     forwardResultAttemptID(resp),
			ResponseID:    finalResponse.ID,
			Usage:         usage,
			Model:         originalModel,
			BillingModel:  billingModel,
			UpstreamModel: upstreamModel,
			Stream:        false,
			Duration:      time.Since(startTime),
		}, fmt.Errorf("upstream response failed: %s", errMsg)
	}

	// When the terminal event has an empty output array, reconstruct from
	// accumulated delta events so the client receives the full content.
	acc.SupplementResponseOutput(finalResponse)

	anthropicResp := apicompat.ResponsesToAnthropic(finalResponse, originalModel)

	if s.responseHeaderFilter != nil {
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	}
	// Upstream is forced to stream for buffering, so its Content-Type is SSE;
	// c.JSON will not override an existing header. Force JSON for non-streaming clients.
	c.Writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	c.JSON(http.StatusOK, anthropicResp)

	return &OpenAIForwardResult{
		RequestID:     requestID,
		AttemptID:     forwardResultAttemptID(resp),
		ResponseID:    finalResponse.ID,
		Usage:         usage,
		Model:         originalModel,
		BillingModel:  billingModel,
		UpstreamModel: upstreamModel,
		Stream:        false,
		Duration:      time.Since(startTime),
	}, nil
}

func isOpenAICompatResponsesTerminalEvent(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "response.completed", "response.done", "response.incomplete", "response.failed":
		return true
	default:
		return false
	}
}

func openAICompatTopLevelFailedResponse(event *apicompat.ResponsesStreamEvent, payload []byte) *apicompat.ResponsesResponse {
	if event == nil || event.Response != nil || strings.TrimSpace(event.Type) != "response.failed" || len(payload) == 0 {
		return nil
	}
	var resp apicompat.ResponsesResponse
	if err := json.Unmarshal(payload, &resp); err != nil || strings.TrimSpace(resp.Status) != "failed" {
		return nil
	}
	if resp.Usage == nil && event.Usage != nil {
		resp.Usage = event.Usage
	}
	return &resp
}

func openAICompatBufferedTerminalResponse(event *apicompat.ResponsesStreamEvent, payload []byte) *apicompat.ResponsesResponse {
	if event == nil {
		return nil
	}
	if failedResponse := openAICompatTopLevelFailedResponse(event, payload); failedResponse != nil {
		event.Response = failedResponse
	}
	if isOpenAICompatResponsesTerminalEvent(event.Type) {
		return event.Response
	}
	if strings.TrimSpace(event.Type) != "error" || len(payload) == 0 {
		return nil
	}

	var bareError struct {
		Response *struct {
			Error *apicompat.ResponsesError `json:"error"`
			Usage *apicompat.ResponsesUsage `json:"usage"`
		} `json:"response"`
		Error   *apicompat.ResponsesError `json:"error"`
		Message string                    `json:"message"`
	}
	if err := json.Unmarshal(payload, &bareError); err != nil {
		return nil
	}

	responseError := bareError.Error
	if bareError.Response != nil && bareError.Response.Error != nil {
		responseError = bareError.Response.Error
	} else if responseError == nil && strings.TrimSpace(bareError.Message) != "" {
		responseError = &apicompat.ResponsesError{Message: bareError.Message}
	}
	if responseError == nil {
		return nil
	}
	usage := event.Usage
	if usage == nil && bareError.Response != nil {
		usage = bareError.Response.Usage
	}
	return &apicompat.ResponsesResponse{Status: "failed", Error: responseError, Usage: usage}
}

func (s *OpenAIGatewayService) recordOpenAIMessagesStreamUpstreamError(c *gin.Context, account *Account, upstreamRequestID, upstreamURL, kind, message string) {
	if c == nil {
		return
	}
	message = boundOpenAIMessagesErrorMessage(message)
	setOpsUpstreamError(c, http.StatusBadGateway, message, "")
	event := OpsUpstreamErrorEvent{
		Platform:           PlatformOpenAI,
		UpstreamStatusCode: http.StatusBadGateway,
		UpstreamRequestID:  strings.TrimSpace(upstreamRequestID),
		UpstreamURL:        safeUpstreamURL(upstreamURL),
		Kind:               kind,
		Message:            message,
	}
	if account != nil {
		event.Platform = account.Platform
		event.AccountID = account.ID
		event.AccountName = account.Name
	}
	appendOpsUpstreamError(c, event)
}

func isOpenAICompatDoneSentinelLine(line string) bool {
	payload, ok := extractOpenAISSEDataLine(line)
	return ok && strings.TrimSpace(payload) == "[DONE]"
}

func (s *OpenAIGatewayService) readOpenAICompatBufferedTerminal(
	resp *http.Response,
	logPrefix string,
	requestID string,
) (*apicompat.ResponsesResponse, OpenAIUsage, *apicompat.BufferedResponseAccumulator, []byte, error) {
	acc := apicompat.NewBufferedResponseAccumulator()
	var usage OpenAIUsage
	if resp == nil || resp.Body == nil {
		return nil, usage, acc, nil, errors.New("upstream response body is nil")
	}

	scanner := bufio.NewScanner(resp.Body)
	maxLineSize := defaultMaxLineSize
	if s.cfg != nil && s.cfg.Gateway.MaxLineSize > 0 {
		maxLineSize = s.cfg.Gateway.MaxLineSize
	}
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineSize)

	streamInterval := time.Duration(0)
	if s.cfg != nil && s.cfg.Gateway.StreamDataIntervalTimeout > 0 {
		streamInterval = time.Duration(s.cfg.Gateway.StreamDataIntervalTimeout) * time.Second
	}
	var timeoutCh <-chan time.Time
	var timeoutTimer *time.Timer
	resetTimeout := func() {
		if streamInterval <= 0 {
			return
		}
		if timeoutTimer == nil {
			timeoutTimer = time.NewTimer(streamInterval)
			timeoutCh = timeoutTimer.C
			return
		}
		if !timeoutTimer.Stop() {
			select {
			case <-timeoutTimer.C:
			default:
			}
		}
		timeoutTimer.Reset(streamInterval)
	}
	stopTimeout := func() {
		if timeoutTimer == nil {
			return
		}
		if !timeoutTimer.Stop() {
			select {
			case <-timeoutTimer.C:
			default:
			}
		}
	}
	resetTimeout()
	defer stopTimeout()

	type scanEvent struct {
		line string
		err  error
	}
	events := make(chan scanEvent, 16)
	done := make(chan struct{})
	go func() {
		defer close(events)
		for scanner.Scan() {
			select {
			case events <- scanEvent{line: scanner.Text()}:
			case <-done:
				return
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case events <- scanEvent{err: err}:
			case <-done:
			}
		}
	}()
	defer close(done)

	var parser openAICompatSSEFrameParser
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				if frame, ok := parser.Finish(); ok {
					payload := openAICompatPayloadWithEventType(frame.Data, frame.EventType)
					payloadBytes := []byte(payload)
					var event apicompat.ResponsesStreamEvent
					if err := json.Unmarshal(payloadBytes, &event); err == nil {
						terminalResponse := openAICompatBufferedTerminalResponse(&event, payloadBytes)
						acc.ProcessEvent(&event)
						if terminalResponse != nil {
							if event.Usage != nil {
								usage = copyOpenAIUsageFromResponsesUsage(event.Usage)
								if terminalResponse.Usage == nil {
									terminalResponse.Usage = event.Usage
								}
							}
							if terminalResponse.Usage != nil {
								usage = copyOpenAIUsageFromResponsesUsage(terminalResponse.Usage)
							}
							return terminalResponse, usage, acc, payloadBytes, nil
						}
					}
				}
				return nil, usage, acc, nil, nil
			}
			resetTimeout()
			if ev.err != nil {
				if !errors.Is(ev.err, context.Canceled) && !errors.Is(ev.err, context.DeadlineExceeded) {
					logger.L().Warn(logPrefix+": read error",
						zap.Error(ev.err),
						zap.String("request_id", requestID),
					)
				}
				return nil, usage, acc, nil, ev.err
			}

			if isOpenAICompatDoneSentinelLine(ev.line) {
				return nil, usage, acc, nil, nil
			}
			frame, ok := parser.AddLine(ev.line)
			if !ok {
				continue
			}
			payload := openAICompatPayloadWithEventType(frame.Data, frame.EventType)
			payloadBytes := []byte(payload)

			var event apicompat.ResponsesStreamEvent
			if err := json.Unmarshal(payloadBytes, &event); err != nil {
				logger.L().Warn(logPrefix+": failed to parse event",
					zap.Error(err),
					zap.String("request_id", requestID),
				)
				continue
			}
			terminalResponse := openAICompatBufferedTerminalResponse(&event, payloadBytes)

			acc.ProcessEvent(&event)

			if terminalResponse != nil {
				if event.Usage != nil {
					usage = copyOpenAIUsageFromResponsesUsage(event.Usage)
					if terminalResponse.Usage == nil {
						terminalResponse.Usage = event.Usage
					}
				}
				if terminalResponse.Usage != nil {
					usage = copyOpenAIUsageFromResponsesUsage(terminalResponse.Usage)
				}
				return terminalResponse, usage, acc, payloadBytes, nil
			}

		case <-timeoutCh:
			_ = resp.Body.Close()
			logger.L().Warn(logPrefix+": data interval timeout",
				zap.String("request_id", requestID),
				zap.Duration("interval", streamInterval),
			)
			return nil, usage, acc, nil, fmt.Errorf("stream data interval timeout")
		}
	}
}

// handleAnthropicStreamingResponse reads Responses SSE events from upstream,
// converts each to Anthropic SSE events, and writes them to the client.
// When StreamKeepaliveInterval is configured, it uses a goroutine + channel
// pattern to send Anthropic ping events during periods of upstream silence,
// preventing proxy/client timeout disconnections.
func (s *OpenAIGatewayService) handleAnthropicStreamingResponse(
	resp *http.Response,
	c *gin.Context,
	account *Account,
	originalModel string,
	billingModel string,
	upstreamModel string,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	requestID := resp.Header.Get("x-request-id")
	upstreamURL := upstreamURLFromResponse(resp)

	headersWritten := false
	writeStreamHeaders := func() {
		if headersWritten {
			return
		}
		headersWritten = true
		if s.responseHeaderFilter != nil {
			responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
		}
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Header().Set("Connection", "keep-alive")
		c.Writer.Header().Set("X-Accel-Buffering", "no")
		c.Writer.WriteHeader(http.StatusOK)
	}

	state := apicompat.NewResponsesEventToAnthropicState()
	state.Model = originalModel
	var usage OpenAIUsage
	responseID := ""
	var firstTokenMs *int
	clientDisconnected := false
	clientOutputStarted := false
	pendingAnthropicSSE := make([]string, 0, 4)
	var streamFailoverErr *UpstreamFailoverError
	var streamErr error
	markClientOutputStarted := func() {
		if clientOutputStarted {
			return
		}
		clientOutputStarted = true
		markOpenAICompatAnthropicClientOutputStarted(c)
		if firstTokenMs == nil {
			ms := int(time.Since(startTime).Milliseconds())
			firstTokenMs = &ms
		}
	}

	scanner := bufio.NewScanner(resp.Body)
	maxLineSize := defaultMaxLineSize
	if s.cfg != nil && s.cfg.Gateway.MaxLineSize > 0 {
		maxLineSize = s.cfg.Gateway.MaxLineSize
	}
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineSize)

	streamInterval := time.Duration(0)
	if s.cfg != nil && s.cfg.Gateway.StreamDataIntervalTimeout > 0 {
		streamInterval = time.Duration(s.cfg.Gateway.StreamDataIntervalTimeout) * time.Second
	}
	var intervalTicker *time.Ticker
	if streamInterval > 0 {
		intervalTicker = time.NewTicker(streamInterval)
		defer intervalTicker.Stop()
	}
	var intervalCh <-chan time.Time
	if intervalTicker != nil {
		intervalCh = intervalTicker.C
	}

	// resultWithUsage builds the final result snapshot.
	resultWithUsage := func() *OpenAIForwardResult {
		return &OpenAIForwardResult{
			RequestID:        requestID,
			AttemptID:        forwardResultAttemptID(resp),
			ResponseID:       responseID,
			Usage:            usage,
			Model:            originalModel,
			BillingModel:     billingModel,
			UpstreamModel:    upstreamModel,
			Stream:           true,
			Duration:         time.Since(startTime),
			FirstTokenMs:     firstTokenMs,
			ClientDisconnect: clientDisconnected,
		}
	}

	// processDataLine handles a single "data: ..." SSE line from upstream.
	processDataLine := func(payload string) bool {
		payloadBytes := []byte(payload)
		var event apicompat.ResponsesStreamEvent
		if err := json.Unmarshal(payloadBytes, &event); err != nil {
			logger.L().Warn("openai messages stream: failed to parse event",
				zap.Error(err),
				zap.String("request_id", requestID),
			)
			return false
		}

		eventType := strings.TrimSpace(event.Type)
		isBareErrorEvent := eventType == "error"
		isTerminalEvent := isOpenAICompatResponsesTerminalEvent(eventType) || isBareErrorEvent
		if isTerminalEvent {
			if event.Response != nil {
				if id := strings.TrimSpace(event.Response.ID); id != "" {
					responseID = id
				}
				if event.Response.Usage != nil {
					usage = copyOpenAIUsageFromResponsesUsage(event.Response.Usage)
				}
			}
			if event.Usage != nil {
				usage = copyOpenAIUsageFromResponsesUsage(event.Usage)
			}
		}
		if failedResponse := openAICompatTopLevelFailedResponse(&event, payloadBytes); failedResponse != nil {
			event.Response = failedResponse
			usage = copyOpenAIUsageFromResponsesUsage(failedResponse.Usage)
		}
		if eventType == "response.failed" || isBareErrorEvent {
			if requestErr := newOpenAIUpstreamRequestError(payloadBytes, requestID); requestErr != nil {
				requestErr.observeTerminal(usage, clientOutputStarted || OpenAICompatAnthropicClientOutputStarted(c))
				streamErr = requestErr
				if !clientDisconnected {
					writeStreamHeaders()
					if err := writeAnthropicStreamError(c, requestErr.AnthropicErrorType(), requestErr.Message); err != nil {
						clientDisconnected = true
					}
				}
				return true
			}
			rawMessage := extractOpenAIMessagesRawSSEErrorMessage(payloadBytes)
			if strings.TrimSpace(rawMessage) == "" {
				rawMessage = "OpenAI messages stream response failed"
			}
			if !clientDisconnected && !clientOutputStarted && openAIStreamFailedEventShouldFailover(payloadBytes, rawMessage) {
				message := boundOpenAIMessagesErrorMessage(rawMessage)
				streamFailoverErr = s.newOpenAIStreamFailoverError(c, account, false, requestID, payloadBytes, message, resp.Header)
				return true
			}
			message := boundOpenAIMessagesErrorMessage(rawMessage)
			s.recordOpenAIMessagesStreamUpstreamError(c, account, requestID, upstreamURL, "stream_failed", message)
			// response.failed arrives inside an HTTP 200 SSE stream, so there is no upstream HTTP error status to pass through.
			errStatus, errType, errMsg := http.StatusBadGateway, "api_error", message
			if status, mappedType, mappedMsg, matched := applyErrorPassthroughRule(
				c, account.Platform, 0, payloadBytes,
				errStatus, errType, errMsg,
			); matched {
				if status == 0 {
					status = errStatus
				}
				if strings.TrimSpace(mappedMsg) == "" {
					mappedMsg = errMsg
				}
				mappedMsg = boundOpenAIMessagesErrorMessage(mappedMsg)
				errStatus, errType, errMsg = status, mappedType, mappedMsg
				MarkResponseCommitted(c)
			}
			streamErr = fmt.Errorf("upstream response failed: %s", errMsg)
			if !clientDisconnected {
				if c != nil && c.Writer != nil && c.Writer.Written() {
					writeStreamHeaders()
					if err := writeAnthropicStreamError(c, errType, errMsg); err != nil {
						clientDisconnected = true
						logger.L().Info("openai messages stream: client disconnected while writing failure event",
							zap.String("request_id", requestID),
						)
					}
				} else if !OpenAICompatAnthropicClientOutputStarted(c) {
					writeAnthropicError(c, errStatus, errType, errMsg)
				}
			}
			return true
		}

		// Convert to Anthropic events. Protocol prelude events (for example
		// response.created -> message_start) are buffered until actual model output,
		// so a later retryable response.failed can still fail over transparently.
		events := apicompat.ResponsesEventToAnthropicEvents(&event, state)
		if len(events) == 0 || clientDisconnected {
			return isTerminalEvent
		}
		startsClientOutput := openAIStreamDataStartsClientOutput(payload, eventType)
		for _, evt := range events {
			sse, err := apicompat.ResponsesAnthropicEventToSSE(evt)
			if err != nil {
				logger.L().Warn("openai messages stream: failed to marshal event",
					zap.Error(err),
					zap.String("request_id", requestID),
				)
				continue
			}
			if !OpenAICompatAnthropicClientOutputStarted(c) && !startsClientOutput {
				pendingAnthropicSSE = append(pendingAnthropicSSE, sse)
				continue
			}
			writeStreamHeaders()
			if !OpenAICompatAnthropicClientOutputStarted(c) {
				for _, pending := range pendingAnthropicSSE {
					if _, err := fmt.Fprint(c.Writer, pending); err != nil {
						clientDisconnected = true
						logger.L().Info("openai messages stream: client disconnected while flushing pending prelude",
							zap.String("request_id", requestID),
						)
						break
					}
				}
				pendingAnthropicSSE = pendingAnthropicSSE[:0]
				if clientDisconnected {
					break
				}
				markClientOutputStarted()
			}
			if _, err := fmt.Fprint(c.Writer, sse); err != nil {
				clientDisconnected = true
				logger.L().Info("openai messages stream: client disconnected, continuing to drain upstream for billing",
					zap.String("request_id", requestID),
				)
				break
			}
		}
		if !clientDisconnected && clientOutputStarted {
			c.Writer.Flush()
		}
		return isTerminalEvent
	}

	// finalizeStream sends any remaining Anthropic events and returns the result.
	finalizeStream := func() (*OpenAIForwardResult, error) {
		if streamFailoverErr != nil {
			if !OpenAICompatAnthropicClientOutputStarted(c) {
				return nil, streamFailoverErr
			}
			return resultWithUsage(), streamFailoverErr
		}
		if streamErr != nil {
			return resultWithUsage(), streamErr
		}
		if finalEvents := apicompat.FinalizeResponsesAnthropicStream(state); len(finalEvents) > 0 && !clientDisconnected {
			for _, evt := range finalEvents {
				sse, err := apicompat.ResponsesAnthropicEventToSSE(evt)
				if err != nil {
					continue
				}
				writeStreamHeaders()
				if !OpenAICompatAnthropicClientOutputStarted(c) {
					for _, pending := range pendingAnthropicSSE {
						if _, err := fmt.Fprint(c.Writer, pending); err != nil {
							clientDisconnected = true
							logger.L().Info("openai messages stream: client disconnected while flushing pending final prelude",
								zap.String("request_id", requestID),
							)
							break
						}
					}
					pendingAnthropicSSE = pendingAnthropicSSE[:0]
					if clientDisconnected {
						break
					}
					markClientOutputStarted()
				}
				if _, err := fmt.Fprint(c.Writer, sse); err != nil {
					clientDisconnected = true
					logger.L().Info("openai messages stream: client disconnected during final flush",
						zap.String("request_id", requestID),
					)
					break
				}
			}
			if !clientDisconnected {
				c.Writer.Flush()
			}
		}
		return resultWithUsage(), nil
	}

	// handleScanErr logs scanner errors if meaningful.
	handleScanErr := func(err error) {
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			logger.L().Warn("openai messages stream: read error",
				zap.Error(err),
				zap.String("request_id", requestID),
			)
		}
	}
	missingTerminalErr := func() (*OpenAIForwardResult, error) {
		result := resultWithUsage()
		if clientDisconnected {
			return result, fmt.Errorf("stream usage incomplete: missing terminal event")
		}
		message := "OpenAI messages stream ended before a terminal event"
		if !OpenAICompatAnthropicClientOutputStarted(c) {
			return result, s.newOpenAIStreamFailoverError(c, account, false, requestID, nil, message)
		}
		s.recordOpenAIMessagesStreamUpstreamError(c, account, requestID, upstreamURL, "stream_missing_terminal", message)
		return result, fmt.Errorf("stream usage incomplete: missing terminal event")
	}
	processFrame := func(frame openAICompatSSEFrame) bool {
		payload := openAICompatPayloadWithEventType(frame.Data, frame.EventType)
		return processDataLine(payload)
	}

	// ── Determine keepalive interval ──
	keepaliveInterval := time.Duration(0)
	if s.cfg != nil && s.cfg.Gateway.StreamKeepaliveInterval > 0 {
		keepaliveInterval = time.Duration(s.cfg.Gateway.StreamKeepaliveInterval) * time.Second
	}

	// ── No keepalive: fast synchronous path (no goroutine overhead) ──
	if streamInterval <= 0 && keepaliveInterval <= 0 {
		var parser openAICompatSSEFrameParser
		for scanner.Scan() {
			line := scanner.Text()
			if isOpenAICompatDoneSentinelLine(line) {
				return missingTerminalErr()
			}
			frame, ok := parser.AddLine(line)
			if !ok {
				continue
			}
			if processFrame(frame) {
				return finalizeStream()
			}
		}
		if err := scanner.Err(); err != nil {
			handleScanErr(err)
			return resultWithUsage(), fmt.Errorf("stream usage incomplete: %w", err)
		}
		if frame, ok := parser.Finish(); ok {
			if strings.TrimSpace(frame.Data) == "[DONE]" {
				return missingTerminalErr()
			}
			if processFrame(frame) {
				return finalizeStream()
			}
		}
		return missingTerminalErr()
	}

	// ── With keepalive: goroutine + channel + select ──
	type scanEvent struct {
		line string
		err  error
	}
	events := make(chan scanEvent, 16)
	done := make(chan struct{})
	var lastReadAt int64
	atomic.StoreInt64(&lastReadAt, time.Now().UnixNano())
	sendEvent := func(ev scanEvent) bool {
		select {
		case events <- ev:
			return true
		case <-done:
			return false
		}
	}
	go func() {
		defer close(events)
		for scanner.Scan() {
			atomic.StoreInt64(&lastReadAt, time.Now().UnixNano())
			if !sendEvent(scanEvent{line: scanner.Text()}) {
				return
			}
		}
		if err := scanner.Err(); err != nil {
			_ = sendEvent(scanEvent{err: err})
		}
	}()
	defer close(done)

	var keepaliveTicker *time.Ticker
	if keepaliveInterval > 0 {
		keepaliveTicker = time.NewTicker(keepaliveInterval)
		defer keepaliveTicker.Stop()
	}
	var keepaliveCh <-chan time.Time
	if keepaliveTicker != nil {
		keepaliveCh = keepaliveTicker.C
	}
	lastDataAt := time.Now()
	var parser openAICompatSSEFrameParser

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				// Upstream closed
				if frame, ok := parser.Finish(); ok {
					if strings.TrimSpace(frame.Data) == "[DONE]" {
						return missingTerminalErr()
					}
					if processFrame(frame) {
						return finalizeStream()
					}
				}
				return missingTerminalErr()
			}
			if ev.err != nil {
				handleScanErr(ev.err)
				return resultWithUsage(), fmt.Errorf("stream usage incomplete: %w", ev.err)
			}
			lastDataAt = time.Now()
			line := ev.line
			if isOpenAICompatDoneSentinelLine(line) {
				return missingTerminalErr()
			}
			frame, ok := parser.AddLine(line)
			if !ok {
				continue
			}
			if processFrame(frame) {
				return finalizeStream()
			}

		case <-intervalCh:
			lastRead := time.Unix(0, atomic.LoadInt64(&lastReadAt))
			if time.Since(lastRead) < streamInterval {
				continue
			}
			if clientDisconnected {
				return resultWithUsage(), fmt.Errorf("stream usage incomplete after timeout")
			}
			logger.L().Warn("openai messages stream: data interval timeout",
				zap.String("request_id", requestID),
				zap.String("model", originalModel),
				zap.Duration("interval", streamInterval),
			)
			return resultWithUsage(), fmt.Errorf("stream data interval timeout")

		case <-keepaliveCh:
			if clientDisconnected {
				continue
			}
			if time.Since(lastDataAt) < keepaliveInterval {
				continue
			}
			// Send Anthropic-format ping event without marking model output as started;
			// pings alone must not block transparent upstream failover.
			writeStreamHeaders()
			if _, err := fmt.Fprint(c.Writer, "event: ping\ndata: {\"type\":\"ping\"}\n\n"); err != nil {
				// Client disconnected
				logger.L().Info("openai messages stream: client disconnected during keepalive",
					zap.String("request_id", requestID),
				)
				clientDisconnected = true
				continue
			}
			c.Writer.Flush()
		}
	}
}

const openAICompatAnthropicClientOutputStartedKey = "openai_compat_anthropic_client_output_started"

func markOpenAICompatAnthropicClientOutputStarted(c *gin.Context) {
	if c != nil {
		c.Set(openAICompatAnthropicClientOutputStartedKey, true)
	}
}

func OpenAICompatAnthropicClientOutputStarted(c *gin.Context) bool {
	if c == nil {
		return false
	}
	started, ok := c.Get(openAICompatAnthropicClientOutputStartedKey)
	return ok && started == true
}

// writeAnthropicError writes an error response in Anthropic Messages API format.
func writeAnthropicError(c *gin.Context, statusCode int, errType, message string) {
	if c == nil || c.Writer == nil {
		return
	}
	MarkResponseCommitted(c)
	if c.Writer.Written() {
		// Once an SSE ping/prelude has committed the response, keep later errors in
		// Anthropic stream format instead of appending JSON to an SSE body.
		_ = writeAnthropicStreamError(c, errType, message)
		return
	}
	c.JSON(statusCode, gin.H{
		"type": "error",
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
}

func writeAnthropicStreamError(c *gin.Context, errType, message string) error {
	if c == nil || c.Writer == nil {
		return nil
	}
	MarkResponseCommitted(c)
	payload, _ := json.Marshal(gin.H{
		"type": "error",
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
	_, err := fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", payload)
	if err == nil {
		c.Writer.Flush()
	}
	return err
}

func copyOpenAIUsageFromResponsesUsage(usage *apicompat.ResponsesUsage) OpenAIUsage {
	if usage == nil {
		return OpenAIUsage{}
	}
	result := OpenAIUsage{
		InputTokens:              usage.InputTokens,
		OutputTokens:             usage.OutputTokens,
		CacheCreationInputTokens: usage.CacheCreationInputTokens,
	}
	if usage.InputTokensDetails != nil {
		result.CacheReadInputTokens = usage.InputTokensDetails.CachedTokens
	}
	return result
}
