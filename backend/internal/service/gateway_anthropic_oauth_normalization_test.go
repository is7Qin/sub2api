package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const proxiedClaudeCodeMetadataUserID = "user_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef_account__session_aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"

func proxiedClaudeCodeBody(metadataUserID, billingText string) []byte {
	return []byte(`{"model":"claude-sonnet-4-20250514","system":[{"type":"text","text":"` + billingText + `","cache_control":{"type":"ephemeral","ttl":"1h"}},{"type":"text","text":"original system instruction","cache_control":{"type":"ephemeral"}}],"metadata":{"user_id":"` + metadataUserID + `"},"messages":[{"role":"user","content":"hi"}]}`)
}

func proxiedClaudeCodeBillingText() string {
	return claudeCodeBillingHeaderPrefix + ": cc_version=2.1.0; " + claudeCodeEntrypointMarker + "cli"
}

func TestIsProxiedClaudeCodeOAuthRequest(t *testing.T) {
	validParsed := &ParsedRequest{MetadataUserID: proxiedClaudeCodeMetadataUserID}
	tests := []struct {
		name   string
		parsed *ParsedRequest
		body   []byte
		want   bool
	}{
		{"valid direct billing block", validParsed, proxiedClaudeCodeBody(proxiedClaudeCodeMetadataUserID, proxiedClaudeCodeBillingText()), true},
		{"missing parsed request", nil, proxiedClaudeCodeBody(proxiedClaudeCodeMetadataUserID, proxiedClaudeCodeBillingText()), false},
		{"invalid metadata", &ParsedRequest{MetadataUserID: "not-a-claude-code-id"}, proxiedClaudeCodeBody("not-a-claude-code-id", proxiedClaudeCodeBillingText()), false},
		{"missing metadata", &ParsedRequest{}, proxiedClaudeCodeBody("", proxiedClaudeCodeBillingText()), false},
		{"weak marker", validParsed, proxiedClaudeCodeBody(proxiedClaudeCodeMetadataUserID, claudeCodeBillingHeaderPrefix+": cc_entrypoint"), false},
		{"unrelated marker", validParsed, proxiedClaudeCodeBody(proxiedClaudeCodeMetadataUserID, claudeCodeBillingHeaderPrefix+": other_entrypoint=value"), false},
		{"plain system", validParsed, []byte(`{"system":"` + proxiedClaudeCodeBillingText() + `","metadata":{"user_id":"` + proxiedClaudeCodeMetadataUserID + `"}}`), false},
		{"nested text is not direct", validParsed, []byte(`{"system":[{"type":"text","content":{"text":"` + proxiedClaudeCodeBillingText() + `"}}],"metadata":{"user_id":"` + proxiedClaudeCodeMetadataUserID + `"}}`), false},
		{"non-text system block is not direct", validParsed, []byte(`{"system":[{"type":"tool_result","text":"` + proxiedClaudeCodeBillingText() + `"}],"metadata":{"user_id":"` + proxiedClaudeCodeMetadataUserID + `"}}`), false},
		{"marker in user content is not direct", validParsed, []byte(`{"system":[{"type":"text","text":"ordinary system"}],"metadata":{"user_id":"` + proxiedClaudeCodeMetadataUserID + `"},"messages":[{"role":"user","content":"` + proxiedClaudeCodeBillingText() + `"}]}`), false},
		{"marker in tool payload is not direct", validParsed, []byte(`{"system":[{"type":"text","text":"ordinary system"}],"metadata":{"user_id":"` + proxiedClaudeCodeMetadataUserID + `"},"tools":[{"name":"search","input_schema":{"marker":"` + proxiedClaudeCodeBillingText() + `"}}]}`), false},
		{"malformed body", validParsed, []byte(`{"system":[{"type":"text","text":"` + proxiedClaudeCodeBillingText() + `"}]`), false},
		{"metadata only exists in body", &ParsedRequest{}, proxiedClaudeCodeBody(proxiedClaudeCodeMetadataUserID, proxiedClaudeCodeBillingText()), false},
		{"parsed metadata does not match body", &ParsedRequest{MetadataUserID: proxiedClaudeCodeMetadataUserID}, proxiedClaudeCodeBody("invalid", proxiedClaudeCodeBillingText()), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isProxiedClaudeCodeOAuthRequest(tt.parsed, tt.body))
		})
	}

	t.Run("valid JSON metadata user ID", func(t *testing.T) {
		metadataUserID := `{"device_id":"client","account_uuid":"account","session_id":"session"}`
		body := proxiedClaudeCodeBody(strings.ReplaceAll(metadataUserID, `"`, `\"`), proxiedClaudeCodeBillingText())
		parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), domain.PlatformAnthropic)
		require.NoError(t, err)
		require.True(t, isProxiedClaudeCodeOAuthRequest(parsed, body))
	})
}

func TestNormalizeOpenAIFunctionToolChoiceForAnthropic(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		changed bool
		want    string
	}{
		{"flat", `{"tool_choice":{"type":"function","name":"Read","extra":7}}`, true, `{"type":"tool","name":"Read","extra":7}`},
		{"nested", `{"tool_choice":{"type":"function","function":{"name":"Read"},"extra":7}}`, true, `{"type":"tool","name":"Read","extra":7}`},
		{"same flat nested", `{"tool_choice":{"type":"function","name":"Read","function":{"name":"Read"}}}`, true, `{"type":"tool","name":"Read"}`},
		{"exact whitespace name", `{"tool_choice":{"type":"function","name":" Read "}}`, true, `{"type":"tool","name":" Read "}`},
		{"whitespace only name", `{"tool_choice":{"type":"function","name":" "}}`, true, `{"type":"tool","name":" "}`},
		{"conflict", `{"tool_choice":{"type":"function","name":"Read","function":{"name":"Write"}}}`, false, ""},
		{"conflict after trimming", `{"tool_choice":{"type":"function","name":"Read","function":{"name":" Read "}}}`, false, ""},
		{"missing", `{"tool_choice":{"type":"function"}}`, false, ""},
		{"non string", `{"tool_choice":{"type":"function","name":7}}`, false, ""},
		{"non string flat with nested", `{"tool_choice":{"type":"function","name":7,"function":{"name":"Read"}}}`, false, ""},
		{"non string nested", `{"tool_choice":{"type":"function","function":{"name":7}}}`, false, ""},
		{"auto", `{"tool_choice":{"type":"auto"}}`, false, ""},
		{"any", `{"tool_choice":{"type":"any"}}`, false, ""},
		{"none", `{"tool_choice":{"type":"none"}}`, false, ""},
		{"tool", `{"tool_choice":{"type":"tool","name":"Read"}}`, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(tt.input)
			got, changed := normalizeOpenAIFunctionToolChoiceForAnthropic(body)
			require.Equal(t, tt.changed, changed)
			if tt.changed {
				require.JSONEq(t, tt.want, gjson.GetBytes(got, "tool_choice").Raw)
			} else {
				require.Equal(t, body, got)
			}
		})
	}
}

func TestNormalizeNativeAnthropicOAuthRequestBody_ToolChoiceBeforeRewrite(t *testing.T) {
	body := []byte(`{"tools":[{"name":"sessions_Read","input_schema":{"type":"object"}}],"tool_choice":{"type":"function","function":{"name":"sessions_Read"}},"messages":[{"role":"user","content":"hi"}]}`)
	normalized := normalizeNativeAnthropicOAuthRequestBody(body)
	rw := buildToolNameRewriteFromBody(normalized)
	require.NotNil(t, rw)
	rewritten := applyToolNameRewriteToBody(normalized, rw)
	require.Equal(t, "cc_sess_Read", gjson.GetBytes(rewritten, "tools.0.name").String())
	require.Equal(t, "cc_sess_Read", gjson.GetBytes(rewritten, "tool_choice.name").String())
}

func TestLiftInitialAnthropicSystemMessages(t *testing.T) {
	body := []byte(`{"system":[{"type":"text","text":"existing\u0020block"}],"messages":[{"role":"system","content":"first\nline"},{"role":"system","content":[],"output_config":{"effort":"high"}},{"role":"system","content":[{"type":"text","text":"second\u0020prompt"}]},{"role":"assistant","content":[{"type":"thinking","thinking":"signed\nbytes","signature":"sig\u003d","opaque":9007199254740993123456789},{"type":"text","text":"answer"}]},{"role":"system","content":"later"}],"large":1e+100}`)
	beforeAssistant := gjson.GetBytes(body, "messages.3").Raw
	beforeDirective := gjson.GetBytes(body, "messages.1").Raw
	beforeLater := gjson.GetBytes(body, "messages.4").Raw

	got, changed := liftInitialAnthropicSystemMessages(body)
	require.True(t, changed)
	require.Equal(t, []string{"existing block", "first\nline", "second prompt"}, []string{
		gjson.GetBytes(got, "system.0.text").String(),
		gjson.GetBytes(got, "system.1.text").String(),
		gjson.GetBytes(got, "system.2.text").String(),
	})
	require.Equal(t, beforeDirective, gjson.GetBytes(got, "messages.0").Raw)
	require.Equal(t, beforeAssistant, gjson.GetBytes(got, "messages.1").Raw)
	require.Equal(t, beforeLater, gjson.GetBytes(got, "messages.2").Raw)
	require.Equal(t, "1e+100", gjson.GetBytes(got, "large").Raw)
	require.True(t, bytes.Equal(got, normalizeNativeAnthropicOAuthRequestBody(got)))
}

func TestLiftInitialAnthropicSystemMessages_StringSystemAndBoundary(t *testing.T) {
	body := []byte(`{"system":"existing", "messages":[{"role":"system","content":"lift"},{"role":"user","content":"stop"},{"role":"system","content":"later"}]}`)
	got, changed := liftInitialAnthropicSystemMessages(body)
	require.True(t, changed)
	require.Equal(t, "existing", gjson.GetBytes(got, "system.0.text").String())
	require.Equal(t, "lift", gjson.GetBytes(got, "system.1.text").String())
	require.Equal(t, "user", gjson.GetBytes(got, "messages.0.role").String())
	require.Equal(t, "system", gjson.GetBytes(got, "messages.1.role").String())
}

func TestGatewayService_AnthropicOAuthSetupTokenAndAPIKeyPassthroughClassification(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"lift me"},{"role":"user","content":"hi"}]}`)
	oauth := &Account{Platform: PlatformAnthropic, Type: AccountTypeOAuth}
	setup := &Account{Platform: PlatformAnthropic, Type: AccountTypeSetupToken}
	apiKey := &Account{Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Extra: map[string]any{"anthropic_passthrough": true}}

	for _, account := range []*Account{oauth, setup} {
		got := normalizeNativeAnthropicRequestForAccount(account, body)
		require.Equal(t, "lift me", gjson.GetBytes(got, "system.0.text").String())
		require.Equal(t, "user", gjson.GetBytes(got, "messages.0.role").String())
	}
	require.True(t, oauth.IsOAuth())
	require.True(t, setup.IsOAuth())
	require.False(t, apiKey.IsOAuth())
	require.True(t, apiKey.IsAnthropicAPIKeyPassthroughEnabled())
	require.True(t, bytes.Equal(body, normalizeNativeAnthropicRequestForAccount(apiKey, body)))
}

func TestGatewayService_AnthropicOAuthAndSetupToken_NormalizesWireBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeSetupToken} {
		t.Run(string(accountType), func(t *testing.T) {
			body := []byte(`{"model":"claude-3-haiku-20240307","stream":true,"tools":[{"name":"sessions_Read","input_schema":{"type":"object"}}],"tool_choice":{"type":"function","function":{"name":"sessions_Read"}},"messages":[{"role":"system","content":"lift me"},{"role":"user","content":"hi"}]}`)
			parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), domain.PlatformAnthropic)
			require.NoError(t, err)

			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			c.Request.Header.Set("User-Agent", "claude-cli/1.0.0")
			upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("event: message_stop\ndata: {}\n\n")),
			}}
			svc := &GatewayService{
				cfg:              &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}},
				httpUpstream:     upstream,
				rateLimitService: &RateLimitService{},
			}
			account := &Account{
				ID: 301, Name: "anthropic-adapter", Platform: PlatformAnthropic, Type: accountType,
				Concurrency: 1, Credentials: map[string]any{"access_token": "token"},
				Status: StatusActive, Schedulable: true,
			}

			result, err := svc.Forward(context.Background(), c, account, parsed)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Contains(t, string(upstream.lastBody), "lift me")
			require.Equal(t, "user", gjson.GetBytes(upstream.lastBody, "messages.0.role").String())
			require.Equal(t, "tool", gjson.GetBytes(upstream.lastBody, "tool_choice.type").String())
			require.Equal(t, "cc_sess_Read", gjson.GetBytes(upstream.lastBody, "tool_choice.name").String())
			require.Equal(t, "cc_sess_Read", gjson.GetBytes(upstream.lastBody, "tools.0.name").String())
		})
	}
}

func TestGatewayService_ProxiedClaudeCodeOAuthAndSetupToken_PreserveSystemWireLayout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeSetupToken} {
		t.Run(accountType, func(t *testing.T) {
			body := proxiedClaudeCodeBody(proxiedClaudeCodeMetadataUserID, proxiedClaudeCodeBillingText())
			parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), domain.PlatformAnthropic)
			require.NoError(t, err)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			c.Request.Header.Set("User-Agent", "proxy/1.0")
			upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-20250514","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))}}
			svc := &GatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}, httpUpstream: upstream, rateLimitService: &RateLimitService{}}
			account := &Account{ID: 302, Name: "anthropic-proxy", Platform: PlatformAnthropic, Type: accountType, Concurrency: 1, Credentials: map[string]any{"access_token": "token"}, Status: StatusActive, Schedulable: true}

			_, err = svc.Forward(context.Background(), c, account, parsed)
			require.NoError(t, err)
			require.JSONEq(t, gjson.GetBytes(body, "system").Raw, gjson.GetBytes(upstream.lastBody, "system").Raw)
		})
	}
}

func TestGatewayService_ProxiedClaudeCodeOAuth_InvalidEvidenceStillMimicsWireBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := proxiedClaudeCodeBody("invalid", claudeCodeBillingHeaderPrefix+": cc_entrypoint")
	parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), domain.PlatformAnthropic)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("User-Agent", "proxy/1.0")
	upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-20250514","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))}}
	svc := &GatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}, httpUpstream: upstream, rateLimitService: &RateLimitService{}}
	account := &Account{ID: 303, Platform: PlatformAnthropic, Type: AccountTypeOAuth, Concurrency: 1, Credentials: map[string]any{"access_token": "token"}, Status: StatusActive, Schedulable: true}

	_, err = svc.Forward(context.Background(), c, account, parsed)
	require.NoError(t, err)
	require.NotEqual(t, gjson.GetBytes(body, "system").Raw, gjson.GetBytes(upstream.lastBody, "system").Raw)
}

func TestGatewayService_ProxiedClaudeCodeOAuthAndSetupToken_CountTokensPreserveSystemWireLayout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, accountType := range []string{AccountTypeOAuth, AccountTypeSetupToken} {
		t.Run(accountType, func(t *testing.T) {
			body := proxiedClaudeCodeBody(proxiedClaudeCodeMetadataUserID, proxiedClaudeCodeBillingText())
			parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), domain.PlatformAnthropic)
			require.NoError(t, err)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", nil)
			c.Request.Header.Set("User-Agent", "proxy/1.0")
			upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"input_tokens":42}`))}}
			svc := &GatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}, httpUpstream: upstream, rateLimitService: &RateLimitService{}}
			account := &Account{ID: 304, Platform: PlatformAnthropic, Type: accountType, Concurrency: 1, Credentials: map[string]any{"access_token": "token"}, Status: StatusActive, Schedulable: true}

			err = svc.ForwardCountTokens(context.Background(), c, account, parsed)
			require.NoError(t, err)
			require.JSONEq(t, gjson.GetBytes(body, "system").Raw, gjson.GetBytes(upstream.lastBody, "system").Raw)
		})
	}
}

func TestGatewayService_ProxiedClaudeCodeOAuth_CountTokensInvalidEvidenceStillMimics(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := proxiedClaudeCodeBody("invalid", claudeCodeBillingHeaderPrefix+": cc_entrypoint")
	parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), domain.PlatformAnthropic)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", nil)
	c.Request.Header.Set("User-Agent", "proxy/1.0")
	upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"input_tokens":42}`))}}
	svc := &GatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}, httpUpstream: upstream, rateLimitService: &RateLimitService{}}
	account := &Account{ID: 305, Platform: PlatformAnthropic, Type: AccountTypeOAuth, Concurrency: 1, Credentials: map[string]any{"access_token": "token"}, Status: StatusActive, Schedulable: true}

	err = svc.ForwardCountTokens(context.Background(), c, account, parsed)
	require.NoError(t, err)
	require.NotEqual(t, gjson.GetBytes(body, "system").Raw, gjson.GetBytes(upstream.lastBody, "system").Raw)
}

func TestGatewayService_ProxiedClaudeCodeAPIKeyPassthrough_DoesNotMutateBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := proxiedClaudeCodeBody(proxiedClaudeCodeMetadataUserID, proxiedClaudeCodeBillingText())
	parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), domain.PlatformAnthropic)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Request.Header.Set("User-Agent", "proxy/1.0")
	upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-20250514","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))}}
	svc := &GatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}, httpUpstream: upstream, rateLimitService: &RateLimitService{}}
	account := newAnthropicAPIKeyAccountForTest()

	_, err = svc.Forward(context.Background(), c, account, parsed)
	require.NoError(t, err)
	require.Equal(t, body, upstream.lastBody)
}
