package service

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// monitorHTTPClient 共享一个 http.Client，避免每次检测重建 transport。
// 自定义 Transport 在 dial 时强制再次校验 IP，防止 DNS rebinding 绕过 validateEndpoint。
var monitorHTTPClient = newSSRFSafeHTTPClient(monitorRequestTimeout)

// monitorPingHTTPClient 用于 endpoint origin 的 HEAD ping，超时更短。
var monitorPingHTTPClient = newSSRFSafeHTTPClient(monitorPingTimeout)

// newSSRFSafeHTTPClient 返回一个使用 safeDialContext 的 http.Client。
// 仅供监控模块对外发起请求使用——所有目标都应是公网 endpoint。
func newSSRFSafeHTTPClient(timeout time.Duration) *http.Client {
	tr := &http.Transport{
		DialContext:           safeDialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		IdleConnTimeout:       monitorIdleConnTimeout,
		TLSHandshakeTimeout:   monitorTLSHandshakeTimeout,
		ResponseHeaderTimeout: monitorResponseHeaderTimeout,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// CheckOptions 承载一次检测的自定义入参。
// 所有字段都是可选（零值即等价于"用默认行为"）。
type CheckOptions struct {
	// APIMode 仅对 OpenAI provider 生效；空串等同 chat_completions。
	APIMode string
	// ExtraHeaders 用户自定义 HTTP 头（merge 到 adapter 默认 headers，用户优先）。
	ExtraHeaders map[string]string
	// BodyOverrideMode: off | merge | replace
	BodyOverrideMode string
	// BodyOverride 在 merge 模式下做浅合并（key 命中黑名单时静默丢弃），
	// 在 replace 模式下直接当作完整 body。
	BodyOverride map[string]any
}

// runCheckForModel 对单个 (provider, model) 做一次完整检测。
// 不返回 error：所有失败都包装进 CheckResult.Status=error/failed。
//
// opts 承载模板 / 监控快照带来的自定义配置。nil 等同于 "off + 无 extra headers"。
func runCheckForModel(ctx context.Context, provider, endpoint, apiKey, model string, opts *CheckOptions) *CheckResult {
	res := &CheckResult{
		Model:     model,
		Status:    MonitorStatusError,
		CheckedAt: time.Now(),
	}

	challenge := generateChallenge()
	mode := bodyOverrideMode(opts)

	start := time.Now()
	respText, _, statusCode, err := callProvider(ctx, provider, endpoint, apiKey, model, challenge.Prompt, opts)
	latency := time.Since(start)
	latencyMs := int(latency / time.Millisecond)
	res.LatencyMs = &latencyMs

	if err != nil {
		res.Status = MonitorStatusError
		res.Message = err.Error()
		return res
	}
	if statusCode < 200 || statusCode >= 300 {
		res.Status = MonitorStatusError
		res.Message = fmt.Sprintf("upstream HTTP %d", statusCode)
		return res
	}

	// Replace 模式：跳过 challenge 校验（用户 body 是静态的，challenge 没法嵌入）。
	// 改用「HTTP 2xx + 响应文本（adapter.textPath 抽取）非空」作为 operational 判定。
	// 响应文本为空则降级为 failed（视为上游回了 200 但没实际内容）。
	if mode == MonitorBodyOverrideModeReplace {
		if strings.TrimSpace(respText) == "" {
			res.Status = MonitorStatusFailed
			res.Message = truncateMessage("replace-mode: upstream returned 2xx with empty text")
			return res
		}
		return finalizeOperationalOrDegraded(res, latency, latencyMs)
	}

	if !validateChallenge(respText, challenge.Expected) {
		res.Status = MonitorStatusFailed
		res.Message = "challenge response mismatch"
		return res
	}

	return finalizeOperationalOrDegraded(res, latency, latencyMs)
}

// finalizeOperationalOrDegraded 负责走到最后一步的 operational/degraded 判定。
// 拆出来是为了让 runCheckForModel 不超过 30 行。
func finalizeOperationalOrDegraded(res *CheckResult, latency time.Duration, latencyMs int) *CheckResult {
	if latency >= monitorDegradedThreshold {
		res.Status = MonitorStatusDegraded
		res.Message = truncateMessage(fmt.Sprintf("slow response: %dms", latencyMs))
		return res
	}
	res.Status = MonitorStatusOperational
	return res
}

// bodyOverrideMode 归一取 opts.BodyOverrideMode，nil opts / 空串都视为 off。
func bodyOverrideMode(opts *CheckOptions) string {
	if opts == nil || opts.BodyOverrideMode == "" {
		return MonitorBodyOverrideModeOff
	}
	return opts.BodyOverrideMode
}

// pingEndpointOrigin 对 endpoint 的 origin (scheme://host) 发起 HEAD 请求，返回耗时。
// 失败时返回 nil（不影响主状态判定）。
func pingEndpointOrigin(ctx context.Context, endpoint string) *int {
	origin, err := extractOrigin(endpoint)
	if err != nil || origin == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, origin, nil)
	if err != nil {
		return nil
	}
	start := time.Now()
	resp, err := monitorPingHTTPClient.Do(req)
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, monitorPingDiscardMaxBytes))
	ms := int(time.Since(start) / time.Millisecond)
	return &ms
}

// providerAdapter 描述某个 provider 在 challenge 检测中需要的 4 件事：
//   - 拼出请求路径（含 model 占位）
//   - 序列化请求体
//   - 构造鉴权头
//   - 从响应 JSON 中按 path 提取文本（gjson path）
//
// 加新 provider 只需要在 providerAdapters 里增加一个条目，无需触碰 callProvider / validateProvider。
type providerAdapter struct {
	buildPath    func(baseURL, model string) (string, error)
	buildBody    func(model, prompt string) ([]byte, error)
	buildHeaders func(apiKey string) map[string]string
	textPath     string // gjson 提取响应文本的 path
}

// providerAdapters 全部已支持的 provider。键值即 MonitorProvider* 字符串。
//
//nolint:gochecknoglobals // 适配器表是只读静态数据，初始化后不变更。
var providerAdapters = map[string]providerAdapter{
	MonitorProviderOpenAI: providerOpenAIChatAdapter,
	MonitorProviderAnthropic: {
		buildPath: func(baseURL, _ string) (string, error) { return joinURL(baseURL, providerAnthropicPath), nil },
		buildBody: func(model, prompt string) ([]byte, error) {
			return json.Marshal(map[string]any{
				"model":      model,
				"messages":   []map[string]string{{"role": "user", "content": prompt}},
				"max_tokens": monitorChallengeMaxTokens,
			})
		},
		buildHeaders: func(apiKey string) map[string]string {
			return map[string]string{
				"x-api-key":         apiKey,
				"anthropic-version": monitorAnthropicAPIVersion,
			}
		},
		textPath: "content.0.text",
	},
	MonitorProviderGemini: {
		// Gemini 把 model 名写在 URL path 上：/v1beta/models/{model}:generateContent
		buildPath: func(baseURL, model string) (string, error) {
			return buildGeminiAIStudioModelActionURL(baseURL, model, "generateContent", false)
		},
		buildBody: func(_, prompt string) ([]byte, error) {
			return json.Marshal(map[string]any{
				"contents": []map[string]any{
					{"parts": []map[string]any{{"text": prompt}}},
				},
				"generationConfig": map[string]any{"maxOutputTokens": monitorChallengeMaxTokens},
			})
		},
		// 使用 x-goog-api-key header 而不是 ?key= query，避免 *url.Error 把 key 回填到错误日志。
		buildHeaders: func(apiKey string) map[string]string {
			return map[string]string{"x-goog-api-key": apiKey}
		},
		textPath: "candidates.0.content.parts.0.text",
	},
}

//nolint:gochecknoglobals // 适配器表是只读静态数据，初始化后不变更。
var providerOpenAIChatAdapter = providerAdapter{
	buildPath: func(baseURL, _ string) (string, error) { return joinURL(baseURL, providerOpenAIPath), nil },
	buildBody: func(model, prompt string) ([]byte, error) {
		return json.Marshal(map[string]any{
			"model":      model,
			"messages":   []map[string]string{{"role": "user", "content": prompt}},
			"max_tokens": monitorChallengeMaxTokens,
			"stream":     false,
		})
	},
	buildHeaders: func(apiKey string) map[string]string {
		return map[string]string{"Authorization": "Bearer " + apiKey}
	},
	textPath: "choices.0.message.content",
}

//nolint:gochecknoglobals // 适配器表是只读静态数据，初始化后不变更。
var providerOpenAIResponsesAdapter = providerAdapter{
	buildPath: func(baseURL, _ string) (string, error) { return joinURL(baseURL, providerOpenAIResponsesPath), nil },
	buildBody: func(model, prompt string) ([]byte, error) {
		return json.Marshal(map[string]any{
			"model":             model,
			"instructions":      "You are a channel health-check endpoint. Answer the arithmetic challenge exactly and briefly.",
			"input":             prompt,
			"max_output_tokens": monitorChallengeMaxTokens,
			"stream":            false,
		})
	},
	buildHeaders: func(apiKey string) map[string]string {
		return map[string]string{"Authorization": "Bearer " + apiKey}
	},
	textPath: "output.0.content.0.text",
}

// providerAdapterFor 按 provider + api_mode 选择具体 adapter。
func providerAdapterFor(provider, apiMode string) (providerAdapter, string, bool) {
	if provider == MonitorProviderOpenAI && defaultAPIMode(apiMode) == MonitorAPIModeResponses {
		return providerOpenAIResponsesAdapter, MonitorAPIModeResponses, true
	}
	adapter, ok := providerAdapters[provider]
	return adapter, MonitorAPIModeChatCompletions, ok
}

// isSupportedProvider 校验 provider 字符串是否在 adapter 表中。
// 供 validate.go 的 validateProvider 复用，避免两份 switch 漂移。
func isSupportedProvider(p string) bool {
	_, ok := providerAdapters[p]
	return ok
}

// callProvider 通过 providerAdapters 分发到具体实现。
// opts 承载用户的自定义 headers / body 覆盖（可为 nil）。
//
// 返回值：
//   - extractedText: 按 textPath 抽出的成功文本，仅在 status 2xx 时有意义；非 2xx 时通常为空串
//   - rawBody: 完整响应体的字符串形式（已被 monitorResponseMaxBytes 截断），用于错误路径保留上游真实回包
//   - status: HTTP 状态码
//   - err: 网络 / 序列化错误
func callProvider(ctx context.Context, provider, endpoint, apiKey, model, prompt string, opts *CheckOptions) (extractedText, rawBody string, status int, err error) {
	requestedAPIMode := checkAPIMode(opts)
	if err := validateAPIMode(provider, requestedAPIMode); err != nil {
		return "", "", 0, err
	}
	adapter, apiMode, ok := providerAdapterFor(provider, requestedAPIMode)
	if !ok {
		return "", "", 0, fmt.Errorf("unsupported provider %q", provider)
	}
	body, err := buildRequestBody(adapter, provider, apiMode, model, prompt, opts)
	if err != nil {
		return "", "", 0, err
	}
	full, err := adapter.buildPath(endpoint, model)
	if err != nil {
		return "", "", 0, err
	}
	headers := mergeHeaders(adapter.buildHeaders(apiKey), opts)
	respBytes, status, err := postRawJSON(ctx, full, body, headers)
	if err != nil {
		return "", "", status, err
	}
	if provider == MonitorProviderOpenAI && apiMode == MonitorAPIModeResponses {
		return extractOpenAIResponsesText(respBytes), string(respBytes), status, nil
	}
	if provider == MonitorProviderAnthropic {
		return extractAnthropicMonitorText(respBytes), string(respBytes), status, nil
	}
	return gjson.GetBytes(respBytes, adapter.textPath).String(), string(respBytes), status, nil
}

// extractAnthropicMonitorText 聚合 Anthropic messages API 响应中的所有 text block。
// 思考模型（或网关 emulation 开启 thinking）会把 thinking/tool_use block 排在文本
// block 前面，content.0.text 会落空导致 challenge 误判——必须跳过非 text block，
// 把散落的文本拼接起来再校验。与 extractOpenAIResponsesText 同一类问题。
func extractAnthropicMonitorText(respBytes []byte) string {
	content := gjson.GetBytes(respBytes, "content")
	if !content.IsArray() {
		return ""
	}

	parts := make([]string, 0, 1)
	content.ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() != "text" {
			return true
		}
		text := strings.TrimSpace(item.Get("text").String())
		if text != "" {
			parts = append(parts, text)
		}
		return true
	})
	return strings.Join(parts, "\n")
}

// extractOpenAIResponsesText 聚合 Responses API 的最终 assistant 文本。
// Responses 的 output 数组顺序由模型决定：reasoning / tool-call item 可能排在 message 前面，
// 因此不能假设文本永远在 output.0.content.0.text。
func extractOpenAIResponsesText(respBytes []byte) string {
	if text := gjson.GetBytes(respBytes, "output_text").String(); strings.TrimSpace(text) != "" {
		return text
	}

	var texts []string
	outputs := gjson.GetBytes(respBytes, "output")
	if outputs.IsArray() {
		outputs.ForEach(func(_, output gjson.Result) bool {
			outputType := output.Get("type").String()
			if outputType != "" && outputType != "message" {
				return true
			}

			content := output.Get("content")
			if !content.IsArray() {
				return true
			}

			content.ForEach(func(_, block gjson.Result) bool {
				blockType := block.Get("type").String()
				if blockType != "" && blockType != "output_text" {
					return true
				}
				if text := block.Get("text").String(); strings.TrimSpace(text) != "" {
					texts = append(texts, text)
				}
				return true
			})
			return true
		})
	}

	if len(texts) > 0 {
		return strings.Join(texts, "")
	}
	return gjson.GetBytes(respBytes, providerOpenAIResponsesAdapter.textPath).String()
}

// mergeHeaders 把用户自定义 headers 合并到 adapter 默认 headers 上。
// 用户值覆盖默认；命中黑名单（hop-by-hop / 由 http.Client 自管的）的 key 静默丢弃。
func mergeHeaders(base map[string]string, opts *CheckOptions) map[string]string {
	if opts == nil || len(opts.ExtraHeaders) == 0 {
		return base
	}
	out := make(map[string]string, len(base)+len(opts.ExtraHeaders))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range opts.ExtraHeaders {
		if IsForbiddenHeaderName(k) {
			continue
		}
		out[k] = v
	}
	return out
}

// buildRequestBody 根据 body_override_mode 构造请求 body。
//
//   - off:     adapter 默认 body
//   - merge:   adapter 默认 body 与 BodyOverride 浅合并；BodyOverride 中命中
//     bodyMergeKeyDenyList[provider] 的 key 会被静默丢弃，避免破坏 challenge / model 路由
//   - replace: 直接 marshal BodyOverride 作为完整 body
//
// 任何 mode 返回的 []byte 都已经是合法 JSON，可直接送入 postRawJSON。
func buildRequestBody(adapter providerAdapter, provider, apiMode, model, prompt string, opts *CheckOptions) ([]byte, error) {
	mode := bodyOverrideMode(opts)

	if mode == MonitorBodyOverrideModeReplace {
		if opts == nil || len(opts.BodyOverride) == 0 {
			return nil, fmt.Errorf("replace mode: body_override is empty")
		}
		if err := validateReplaceRequestBody(provider, apiMode, opts.BodyOverride); err != nil {
			return nil, err
		}
		body, err := json.Marshal(opts.BodyOverride)
		if err != nil {
			return nil, fmt.Errorf("marshal body_override (replace): %w", err)
		}
		return body, nil
	}

	defaultBody, err := adapter.buildBody(model, prompt)
	if err != nil {
		return nil, fmt.Errorf("marshal default body: %w", err)
	}
	if mode != MonitorBodyOverrideModeMerge || opts == nil || len(opts.BodyOverride) == 0 {
		return defaultBody, nil
	}

	var defaultMap map[string]any
	if err := json.Unmarshal(defaultBody, &defaultMap); err != nil {
		return nil, fmt.Errorf("unmarshal default body for merge: %w", err)
	}
	deny := bodyMergeKeyDenyList[bodyMergeDenyKey(provider, apiMode)]
	for k, v := range opts.BodyOverride {
		if deny[k] {
			continue
		}
		defaultMap[k] = v
	}
	merged, err := json.Marshal(defaultMap)
	if err != nil {
		return nil, fmt.Errorf("marshal merged body: %w", err)
	}
	return merged, nil
}

// bodyMergeKeyDenyList 在 merge 模式下，禁止用户覆盖这些 provider-specific 的关键字段。
// 思路抄 check-cx 的 EXCLUDED_METADATA_KEYS：保护 challenge / model 路由不被用户误伤。
// 用户想动这些字段就用 replace 模式（已知会跳 challenge 校验）。
//
//nolint:gochecknoglobals // 静态查表，初始化后不变。
var bodyMergeKeyDenyList = map[string]map[string]bool{
	MonitorProviderOpenAI + ":" + MonitorAPIModeChatCompletions: {"model": true, "messages": true, "stream": true},
	MonitorProviderOpenAI + ":" + MonitorAPIModeResponses:       {"model": true, "instructions": true, "input": true, "stream": true},
	MonitorProviderAnthropic:                                    {"model": true, "messages": true},
	MonitorProviderGemini:                                       {"contents": true},
}

func checkAPIMode(opts *CheckOptions) string {
	if opts == nil {
		return MonitorAPIModeChatCompletions
	}
	return defaultAPIMode(opts.APIMode)
}

func bodyMergeDenyKey(provider, apiMode string) string {
	if provider == MonitorProviderOpenAI {
		return provider + ":" + defaultAPIMode(apiMode)
	}
	return provider
}

func validateReplaceRequestBody(provider, apiMode string, body map[string]any) error {
	if provider != MonitorProviderOpenAI {
		return nil
	}
	switch defaultAPIMode(apiMode) {
	case MonitorAPIModeResponses:
		if strings.TrimSpace(stringFromAny(body["instructions"])) == "" || !hasNonEmptyBodyValue(body["input"]) {
			return fmt.Errorf("replace mode responses body: instructions and input are required")
		}
	case MonitorAPIModeChatCompletions:
		if !hasNonEmptyBodyValue(body["messages"]) {
			return fmt.Errorf("replace mode chat_completions body: messages are required")
		}
	}
	return nil
}

func stringFromAny(v any) string {
	s, _ := v.(string)
	return s
}

func hasNonEmptyBodyValue(v any) bool {
	switch val := v.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(val) != ""
	case []any:
		return len(val) > 0
	case []map[string]any:
		return len(val) > 0
	case []map[string]string:
		return len(val) > 0
	default:
		return true
	}
}

// postRawJSON 发送 POST + 已序列化好的 JSON 字节，限制响应体大小，返回响应字节、HTTP status、错误。
// adapter 自行 marshal 是为了精确控制字段顺序与类型，所以这里直接收 []byte 而不是 any。
func postRawJSON(ctx context.Context, fullURL string, payload []byte, headers map[string]string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, bytes.NewReader(payload))
	if err != nil {
		// url.Error may embed the complete user-controlled URL. Keep it out of
		// persisted monitor messages and any logs that later include them.
		return nil, 0, errors.New("build request failed")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := monitorHTTPClient.Do(req)
	if err != nil {
		return nil, 0, classifyMonitorRequestError(err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, monitorResponseMaxBytes+1))
	if err != nil {
		return nil, resp.StatusCode, errors.New("response read failed")
	}
	if len(respBody) > monitorResponseMaxBytes {
		return nil, resp.StatusCode, errors.New("response body too large")
	}
	return respBody, resp.StatusCode, nil
}

func classifyMonitorRequestError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return errors.New("request canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return errors.New("request timeout")
	case errors.Is(err, errMonitorDialPolicy):
		return errors.New("request blocked by policy")
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return errors.New("DNS lookup failed")
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return errors.New("request timeout")
	}
	var recordErr tls.RecordHeaderError
	var alertErr tls.AlertError
	var verificationErr *tls.CertificateVerificationError
	var authorityErr x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	var certificateErr x509.CertificateInvalidError
	if errors.As(err, &recordErr) || errors.As(err, &alertErr) || errors.As(err, &verificationErr) ||
		errors.As(err, &authorityErr) ||
		errors.As(err, &hostnameErr) || errors.As(err, &certificateErr) {
		return errors.New("TLS handshake failed")
	}
	return errors.New("connection failed")
}

// joinURL 把 base origin 与 path 拼成完整 URL。
// 容忍 base 末尾有/无斜杠，path 必带前导斜杠。
func joinURL(base, path string) string {
	base = strings.TrimRight(base, "/")
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path
}

// extractOrigin 从一个 endpoint URL 中提取 scheme://host[:port] 部分。
func extractOrigin(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", errors.New("endpoint missing scheme or host")
	}
	return u.Scheme + "://" + u.Host, nil
}

// truncateMessage 把消息按 monitorMessageMaxBytes 截断，避免 DB 列溢出与日志过长。
func truncateMessage(msg string) string {
	if len(msg) <= monitorMessageMaxBytes {
		return msg
	}
	const ellipsis = "...(truncated)"
	cutoff := monitorMessageMaxBytes - len(ellipsis)
	if cutoff < 0 {
		cutoff = 0
	}
	return msg[:cutoff] + ellipsis
}
