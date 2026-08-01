package service

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Gin context keys used by Ops error logger for capturing upstream error details.
// These keys are set by gateway services and consumed by handler/ops_error_logger.go.
const (
	OpsUpstreamStatusCodeKey   = "ops_upstream_status_code"
	OpsUpstreamErrorMessageKey = "ops_upstream_error_message"
	OpsUpstreamErrorDetailKey  = "ops_upstream_error_detail"
	OpsUpstreamErrorsKey       = "ops_upstream_errors"

	// Optional stage latencies (milliseconds) for troubleshooting and alerting.
	OpsAuthLatencyMsKey      = "ops_auth_latency_ms"
	OpsRoutingLatencyMsKey   = "ops_routing_latency_ms"
	OpsUpstreamLatencyMsKey  = "ops_upstream_latency_ms"
	OpsResponseLatencyMsKey  = "ops_response_latency_ms"
	OpsTimeToFirstTokenMsKey = "ops_time_to_first_token_ms"
	// OpenAI WS 关键观测字段
	OpsOpenAIWSQueueWaitMsKey = "ops_openai_ws_queue_wait_ms"
	OpsOpenAIWSConnPickMsKey  = "ops_openai_ws_conn_pick_ms"
	OpsOpenAIWSConnReusedKey  = "ops_openai_ws_conn_reused"
	OpsOpenAIWSConnIDKey      = "ops_openai_ws_conn_id"

	// OpsSkipPassthroughKey 由 applyErrorPassthroughRule 在命中 skip_monitoring=true 的规则时设置。
	// ops_error_logger 中间件检查此 key，为 true 时跳过错误记录。
	OpsSkipPassthroughKey = "ops_skip_passthrough"

	// ResponseCommittedKey 由服务层响应写入器或 handler 的 SSE 终止写入器设置。
	// 它让后续兜底路径不能在已完成的 HTTP/SSE 响应后追加第二份终止响应。
	ResponseCommittedKey = "response_committed"

	// Client-side configuration denials should remain visible in ops_error_logs,
	// but should be excluded from SLA/error-rate calculations.
	OpsClientBusinessLimitedKey                          = "ops_client_business_limited"
	OpsClientBusinessLimitedReasonKey                    = "ops_client_business_limited_reason"
	OpsClientBusinessLimitedReasonIPRestriction          = "api_key_ip_restriction"
	OpsClientBusinessLimitedReasonAPIKeyGroupUnavailable = "api_key_group_unavailable"
	OpsClientBusinessLimitedReasonAPIKeyGroupUnassigned  = "api_key_group_unassigned"
	OpsClientBusinessLimitedReasonLocalFeatureGate       = "local_feature_gate"
	OpsClientBusinessLimitedReasonLocalPolicyDenied      = "local_policy_denied"
)

func MarkResponseCommitted(c *gin.Context) {
	if c == nil {
		return
	}
	c.Set(ResponseCommittedKey, true)
}

func IsResponseCommitted(c *gin.Context) bool {
	if c == nil {
		return false
	}
	v, ok := c.Get(ResponseCommittedKey)
	if !ok {
		return false
	}
	marked, _ := v.(bool)
	return marked
}

func SetOpsLatencyMs(c *gin.Context, key string, value int64) {
	if c == nil || strings.TrimSpace(key) == "" || value < 0 {
		return
	}
	c.Set(key, value)
}

func MarkOpsClientBusinessLimited(c *gin.Context, reason string) {
	if c == nil {
		return
	}
	c.Set(OpsClientBusinessLimitedKey, true)
	if reason = strings.TrimSpace(reason); reason != "" {
		c.Set(OpsClientBusinessLimitedReasonKey, reason)
	}
}

func HasOpsClientBusinessLimited(c *gin.Context) bool {
	if c == nil {
		return false
	}
	v, ok := c.Get(OpsClientBusinessLimitedKey)
	if !ok {
		return false
	}
	marked, _ := v.(bool)
	return marked
}

// SetOpsUpstreamError is the exported wrapper for setOpsUpstreamError, used by
// handler-layer code (e.g. failover-exhausted paths) that needs to record the
// original upstream status code before mapping it to a client-facing code.
func SetOpsUpstreamError(c *gin.Context, upstreamStatusCode int, upstreamMessage, upstreamDetail string) {
	setOpsUpstreamError(c, upstreamStatusCode, upstreamMessage, upstreamDetail)
}

func setOpsUpstreamError(c *gin.Context, upstreamStatusCode int, upstreamMessage, upstreamDetail string) {
	if c == nil {
		return
	}
	if upstreamStatusCode > 0 {
		c.Set(OpsUpstreamStatusCodeKey, upstreamStatusCode)
	}
	if msg := strings.TrimSpace(upstreamMessage); msg != "" {
		c.Set(OpsUpstreamErrorMessageKey, msg)
	}
	if detail := strings.TrimSpace(upstreamDetail); detail != "" {
		c.Set(OpsUpstreamErrorDetailKey, detail)
	}
}

// OpsUpstreamErrorEvent describes one upstream error attempt during a single gateway request.
// It is stored in ops_error_logs.upstream_errors as a JSON array.
type OpsUpstreamErrorEvent struct {
	AtUnixMs int64 `json:"at_unix_ms,omitempty"`

	// Passthrough 表示本次请求是否命中“原样透传（仅替换认证）”分支。
	// 该字段用于排障与灰度评估；存入 JSON，不涉及 DB schema 变更。
	Passthrough bool `json:"passthrough,omitempty"`

	// Context
	Platform    string `json:"platform,omitempty"`
	AccountID   int64  `json:"account_id,omitempty"`
	AccountName string `json:"account_name,omitempty"`

	// Outcome
	UpstreamStatusCode int    `json:"upstream_status_code,omitempty"`
	UpstreamRequestID  string `json:"upstream_request_id,omitempty"`

	// UpstreamURL is the actual upstream URL that was called (host + path, query/fragment stripped).
	// Helps debug 404/routing errors by showing which endpoint was targeted.
	UpstreamURL string `json:"upstream_url,omitempty"`

	// UpstreamEndpoint is the normalized upstream API endpoint path used for this attempt.
	UpstreamEndpoint string `json:"upstream_endpoint,omitempty"`

	// Best-effort upstream response capture (sanitized+trimmed).
	UpstreamResponseBody string `json:"upstream_response_body,omitempty"`

	// Kind: http_error | request_error | retry_exhausted | failover
	Kind string `json:"kind,omitempty"`

	Message string `json:"message,omitempty"`
	Detail  string `json:"detail,omitempty"`

	// UpstreamFact is a bounded, in-memory semantic fact used to make ops rule
	// matching respect recognized direct-return errors. It is not persisted.
	UpstreamFact *UpstreamErrorFact `json:"-"`
}

func (ev *OpsUpstreamErrorEvent) ResolvedUpstreamEndpoint() string {
	if ev == nil {
		return ""
	}
	if endpoint := strings.TrimSpace(ev.UpstreamEndpoint); endpoint != "" {
		return endpoint
	}
	return endpointFromSafeUpstreamURL(ev.UpstreamURL)
}

func appendOpsUpstreamError(c *gin.Context, ev OpsUpstreamErrorEvent) {
	if c == nil {
		return
	}
	if ev.AtUnixMs <= 0 {
		ev.AtUnixMs = time.Now().UnixMilli()
	}
	ev.Platform = strings.TrimSpace(ev.Platform)
	ev.UpstreamRequestID = strings.TrimSpace(ev.UpstreamRequestID)
	ev.UpstreamResponseBody = strings.TrimSpace(ev.UpstreamResponseBody)
	ev.Kind = strings.TrimSpace(ev.Kind)
	ev.UpstreamURL = strings.TrimSpace(ev.UpstreamURL)
	ev.UpstreamEndpoint = ev.ResolvedUpstreamEndpoint()
	ev.Message = strings.TrimSpace(ev.Message)
	ev.Detail = strings.TrimSpace(ev.Detail)
	if ev.Message != "" {
		ev.Message = sanitizeUpstreamErrorMessage(ev.Message)
	}

	var existing []*OpsUpstreamErrorEvent
	if v, ok := c.Get(OpsUpstreamErrorsKey); ok {
		if arr, ok := v.([]*OpsUpstreamErrorEvent); ok {
			existing = arr
		}
	}

	evCopy := ev
	existing = append(existing, &evCopy)
	c.Set(OpsUpstreamErrorsKey, existing)

	checkSkipMonitoringForUpstreamEvent(c, &evCopy)
}

// checkSkipMonitoringForUpstreamEvent checks whether the upstream error event
// matches a passthrough rule with skip_monitoring=true and, if so, sets the
// OpsSkipPassthroughKey on the context.  This ensures intermediate retry /
// failover errors (which never go through the final applyErrorPassthroughRule
// path) can still suppress ops_error_logs recording.
func checkSkipMonitoringForUpstreamEvent(c *gin.Context, ev *OpsUpstreamErrorEvent) {
	if ev == nil || (ev.UpstreamStatusCode == 0 && ev.UpstreamFact == nil) {
		return
	}

	svc := getBoundErrorPassthroughService(c)
	if svc == nil {
		return
	}

	if ev.UpstreamFact == nil {
		return
	}

	// Recognized semantics own their presentation and must not be reclassified
	// by mutable database rules for monitoring suppression.
	if _, recognized := RecognizeUpstreamErrorFact(*ev.UpstreamFact); recognized {
		return
	}
	if rule := svc.MatchUnknownRule(*ev.UpstreamFact); rule != nil && rule.SkipMonitoring {
		c.Set(OpsSkipPassthroughKey, true)
	}
}

func marshalOpsUpstreamErrors(events []*OpsUpstreamErrorEvent) *string {
	if len(events) == 0 {
		return nil
	}
	// Ensure we always store a valid JSON value.
	raw, err := json.Marshal(events)
	if err != nil || len(raw) == 0 {
		return nil
	}
	s := string(raw)
	return &s
}

func ParseOpsUpstreamErrors(raw string) ([]*OpsUpstreamErrorEvent, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return []*OpsUpstreamErrorEvent{}, nil
	}
	var out []*OpsUpstreamErrorEvent
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// safeUpstreamURL returns scheme + host + path from a URL, stripping query/fragment
// to avoid leaking sensitive query parameters (e.g. OAuth tokens).
func safeUpstreamURL(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return ""
	}
	if parsed, err := url.Parse(rawURL); err == nil && parsed.Scheme != "" && parsed.Host != "" {
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.Fragment = ""
		return parsed.String()
	}
	if idx := strings.IndexByte(rawURL, '?'); idx >= 0 {
		rawURL = rawURL[:idx]
	}
	if idx := strings.IndexByte(rawURL, '#'); idx >= 0 {
		rawURL = rawURL[:idx]
	}
	return rawURL
}

func endpointFromSafeUpstreamURL(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return ""
	}
	path := rawURL
	if parsed, err := url.Parse(rawURL); err == nil && parsed.Path != "" {
		path = parsed.Path
	}
	for _, match := range []struct {
		path                    string
		endpoint                string
		preserveSuffix          bool
		allowGeminiActionSuffix bool
	}{
		{"/v1/chat/completions", "/v1/chat/completions", false, false},
		{"/v1/responses", "/v1/responses", true, false},
		{"/backend-api/codex/responses", "/v1/responses", true, false},
		{"/v1/messages", "/v1/messages", false, false},
		{"/v1/embeddings", "/v1/embeddings", false, false},
		{"/v1/images/generations", "/v1/images/generations", false, false},
		{"/v1/images/edits", "/v1/images/edits", false, false},
		{"/v1beta/models", "/v1beta/models", false, true},
	} {
		if idx := strings.Index(path, match.path); idx >= 0 {
			suffix := strings.TrimRight(path[idx+len(match.path):], "/")
			if suffix != "" && !strings.HasPrefix(suffix, "/") {
				continue
			}
			if match.preserveSuffix && suffix != "" {
				return match.endpoint + suffix
			}
			if suffix != "" && !(match.allowGeminiActionSuffix && isGeminiGenerateContentSuffix(suffix)) {
				continue
			}
			return match.endpoint
		}
	}
	return ""
}

func isGeminiGenerateContentSuffix(suffix string) bool {
	suffix = strings.TrimPrefix(strings.TrimSpace(suffix), "/")
	if suffix == "" || strings.Contains(suffix, "/") {
		return false
	}
	return strings.HasSuffix(suffix, ":generateContent") || strings.HasSuffix(suffix, ":streamGenerateContent")
}

func upstreamURLFromResponse(resp *http.Response) string {
	if resp == nil || resp.Request == nil || resp.Request.URL == nil {
		return ""
	}
	return safeUpstreamURL(resp.Request.URL.String())
}
