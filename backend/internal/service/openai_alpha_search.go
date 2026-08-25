package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	chatgptCodexAlphaSearchURL   = "https://chatgpt.com/backend-api/codex/alpha/search"
	openAIPlatformAlphaSearchURL = "https://api.openai.com/v1/alpha/search"
)

// ForwardAlphaSearch proxies Codex standalone web search without binding the
// evolving alpha request or response schema.
//
// 返回值约定：仅当上游返回 2xx（一次真实成功的搜索）时返回非 nil 的
// *OpenAIForwardResult（WebSearchCalls=1，供按次计费）；上游错误被原样透传
// 给客户端时返回 (nil, nil)，不产生计费。
func (s *OpenAIGatewayService) ForwardAlphaSearch(ctx context.Context, c *gin.Context, account *Account, body []byte) (*OpenAIForwardResult, error) {
	if s == nil || c == nil || account == nil {
		return nil, fmt.Errorf("service, context, and account are required")
	}
	modelResult := gjson.GetBytes(body, "model")
	requestedModel := strings.TrimSpace(modelResult.String())
	if modelResult.Type != gjson.String || requestedModel == "" {
		return nil, fmt.Errorf("model is required")
	}

	upstreamModel := normalizeOpenAIModelForUpstream(account, account.GetMappedModel(requestedModel))
	if upstreamModel != "" && upstreamModel != requestedModel {
		body = ReplaceModelInBody(body, upstreamModel)
	}

	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}

	req, err := s.buildOpenAIAlphaSearchRequest(ctx, c, account, body, token)
	if err != nil {
		return nil, err
	}

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	upstreamStart := time.Now()
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(upstreamStart).Milliseconds())
	if err != nil {
		return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, true)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
	if err != nil {
		return nil, fmt.Errorf("read alpha search response: %w", err)
	}

	if resp.StatusCode >= http.StatusBadRequest {
		upstreamMessage := sanitizeUpstreamErrorMessage(strings.TrimSpace(extractUpstreamErrorMessage(respBody)))
		if s.shouldFailoverOpenAIUpstreamResponse(resp.StatusCode, upstreamMessage, respBody) {
			resp.Body = io.NopCloser(bytes.NewReader(respBody))
			s.handleFailoverSideEffects(ctx, resp, account, respBody, upstreamModel)
			return nil, &UpstreamFailoverError{
				StatusCode:             resp.StatusCode,
				ResponseBody:           respBody,
				RetryableOnSameAccount: account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
			}
		}
	}

	// Local fork has no shadow accounts, so the snapshot update is unconditional.
	s.UpdateCodexUsageSnapshotFromHeaders(ctx, account.ID, resp.Header)
	writeOpenAIPassthroughResponseHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	c.Data(resp.StatusCode, contentType, respBody)
	if resp.StatusCode >= http.StatusBadRequest {
		// 上游错误已原样透传给客户端：不是一次成功的搜索，不计费。
		return nil, nil
	}
	return &OpenAIForwardResult{
		RequestID:      strings.TrimSpace(resp.Header.Get("x-request-id")),
		Model:          requestedModel,
		UpstreamModel:  upstreamModel,
		Duration:       time.Since(upstreamStart),
		WebSearchCalls: 1,
	}, nil
}

func (s *OpenAIGatewayService) buildOpenAIAlphaSearchRequest(ctx context.Context, c *gin.Context, account *Account, body []byte, token string) (*http.Request, error) {
	var req *http.Request
	var err error
	if account.IsOpenAIApiKey() {
		req, err = s.buildUpstreamRequestOpenAIPassthrough(ctx, c, account, body, token)
	} else if account.IsOpenAIOAuthLike() {
		req, err = s.buildOpenAIAlphaSearchOAuthRequest(ctx, c, account, body, token)
	} else {
		return nil, fmt.Errorf("unsupported OpenAI account type: %s", account.Type)
	}
	if err != nil {
		return nil, err
	}

	targetURL, err := s.openAIAlphaSearchURL(account)
	if err != nil {
		return nil, err
	}
	parsedURL, err := url.Parse(targetURL)
	if err != nil {
		return nil, fmt.Errorf("parse alpha search URL: %w", err)
	}
	if c != nil && c.Request != nil && c.Request.URL != nil {
		query := parsedURL.Query()
		for key, values := range c.Request.URL.Query() {
			for _, value := range values {
				query.Add(key, value)
			}
		}
		parsedURL.RawQuery = query.Encode()
	}
	req.URL = parsedURL
	req.Header.Set("Accept", "application/json")
	if clientBeta := strings.TrimSpace(c.GetHeader("OpenAI-Beta")); clientBeta == "" {
		req.Header.Del("OpenAI-Beta")
	} else {
		req.Header.Set("OpenAI-Beta", clientBeta)
	}
	// APIKey 透传是透明度边界：客户端自报身份原样上送。OAuth 的
	// user-agent/originator/version 由上面的规范身份构造，客户端头不参与。
	if account.IsOpenAIApiKey() {
		if version := strings.TrimSpace(c.GetHeader("Version")); version != "" {
			req.Header.Set("Version", version)
		}
	}
	return req, nil
}

// buildOpenAIAlphaSearchOAuthRequest builds the minimal ChatGPT backend-api
// request for OAuth accounts. The local passthrough builder is APIKey-only,
// and the Responses OAuth adapter is endpoint-specific, so alpha search gets
// its own lightweight adapter.
func (s *OpenAIGatewayService) buildOpenAIAlphaSearchOAuthRequest(ctx context.Context, c *gin.Context, account *Account, body []byte, token string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, chatgptCodexAlphaSearchURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))

	if c != nil && c.Request != nil {
		for key, values := range c.Request.Header {
			if !openaiAllowedHeaders[strings.ToLower(strings.TrimSpace(key))] {
				continue
			}
			for _, v := range values {
				req.Header.Add(key, v)
			}
		}
	}

	authHeaders, authErr := s.buildOpenAIAuthenticationHeaders(ctx, account, token)
	if authErr != nil {
		return nil, authErr
	}
	setHeaderRaw(req.Header, "Authorization", authHeaders.Get("Authorization"))

	req.Host = "chatgpt.com"
	if chatgptAccountID := account.GetChatGPTAccountID(); chatgptAccountID != "" {
		req.Header.Set("ChatGPT-Account-ID", chatgptAccountID)
	}
	// 出站身份统一为网关规范身份（面板覆写 → 内置常量），客户端自报的
	// user-agent/originator/version 不参与构造——上游容量紧张时按客户端身份
	// 分优先级降载，陈旧自报身份会稳定落在被优先丢弃的一侧。与 Responses
	// OAuth adapter 同一套指纹身份逻辑（openai_gateway_service.go:4810）。
	fingerprint, err := s.ensureOpenAICodexFingerprint(ctx, account)
	if err != nil {
		return nil, err
	}
	applyOpenAICodexFingerprintHeaders(req, fingerprint)
	if getHeaderRaw(req.Header, "Version") == "" {
		setHeaderRaw(req.Header, "Version", codexCLIVersion)
	}
	codexUA := s.resolveOpenAICodexUserAgent(ctx)
	if ua := fingerprint.UAProfile.UserAgent(); ua != "" {
		codexUA = ua
	}
	setHeaderRaw(req.Header, "User-Agent", codexUA)

	if getHeaderRaw(req.Header, "Content-Type") == "" {
		setHeaderRaw(req.Header, "Content-Type", "application/json")
	}
	return req, nil
}

func (s *OpenAIGatewayService) openAIAlphaSearchURL(account *Account) (string, error) {
	if account == nil {
		return "", fmt.Errorf("account is required")
	}
	switch account.Type {
	case AccountTypeOAuth:
		return chatgptCodexAlphaSearchURL, nil
	case AccountTypeAPIKey:
		baseURL := account.GetOpenAIBaseURL()
		if baseURL == "" {
			return openAIPlatformAlphaSearchURL, nil
		}
		validatedURL, err := s.validateUpstreamBaseURL(baseURL)
		if err != nil {
			return "", err
		}
		return buildOpenAIEndpointURL(validatedURL, "/v1/alpha/search"), nil
	default:
		return "", fmt.Errorf("unsupported OpenAI account type: %s", account.Type)
	}
}
