package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestStripOpenAIResponsesInputNamespaces_RemovesImageGenPolicyCallsWithOtherDirectCalls(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"tool_call","namespace":"image_gen","name":"generate"},
		{"type":"function_call","namespace":"mcp","name":"read"}
	]}`)

	stripped, err := stripOpenAIResponsesInputNamespaces(body)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(stripped, "input.0.namespace").Exists())
	require.False(t, gjson.GetBytes(stripped, "input.1.namespace").Exists())
}

func TestStripOpenAIResponsesInputNamespaces_StripsOnlyVerifiedDirectCallTypes(t *testing.T) {
	body := []byte(`{
		"meta":9007199254740993,
		"tools":[{"type":"function","name":"keep","namespace":"tool-meta"}],
		"input":[
			{"type":"function_call","namespace":"n0","name":"one","content":{"namespace":"nested"},"large":9007199254740995},
			{"type":"tool_call","namespace":"n1","name":"two"},
			{"type":"custom_tool_call","namespace":"n2","name":"three"},
			{"type":"mcp_tool_call","namespace":"n3","name":"four"},
			{"type":"local_shell_call","namespace":"n4"},
			{"type":"tool_search_call","namespace":"n5"},
			{"type":"message","namespace":"message-meta","content":[{"type":"input_text","namespace":"nested-content","text":"hello"}]},
			{"type":"function_call_output","namespace":"output-meta","output":"ok"},
			{"type":"unknown","namespace":"unknown-meta"}
		]
	}`)

	stripped, err := stripOpenAIResponsesInputNamespaces(body)
	require.NoError(t, err)
	for i := 0; i < 6; i++ {
		require.False(t, gjson.GetBytes(stripped, "input."+string(rune('0'+i))+".namespace").Exists())
	}
	require.Equal(t, "message-meta", gjson.GetBytes(stripped, "input.6.namespace").String())
	require.Equal(t, "nested-content", gjson.GetBytes(stripped, "input.6.content.0.namespace").String())
	require.Equal(t, "output-meta", gjson.GetBytes(stripped, "input.7.namespace").String())
	require.Equal(t, "unknown-meta", gjson.GetBytes(stripped, "input.8.namespace").String())
	require.Equal(t, "nested", gjson.GetBytes(stripped, "input.0.content.namespace").String())
	require.Equal(t, "tool-meta", gjson.GetBytes(stripped, "tools.0.namespace").String())
	require.Equal(t, "9007199254740993", gjson.GetBytes(stripped, "meta").Raw)
	require.Equal(t, "9007199254740995", gjson.GetBytes(stripped, "input.0.large").Raw)
}

func TestOpenAIGatewayService_OAuthHTTPFlattensNamespaceWireBodyAndRestoresBufferedResponseWhenEnabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, accountType := range []string{AccountTypeOAuth} {
		t.Run(accountType, func(t *testing.T) {
			body := []byte(`{
				"model":"gpt-5.4","stream":false,
				"tools":[
					{"type":"namespace","name":"mcp","tools":[{"type":"function","name":"read","parameters":{"type":"object","properties":{"id":{"const":9007199254740993}}}}]},
					{"type":"namespace","name":"image_gen","tools":[{"type":"function","name":"generate"}]}
				],
				"tool_choice":{"type":"function","namespace":"mcp","name":"read"},
				"input":[
					{"type":"function_call","namespace":"mcp","name":"read","call_id":"fc_1","arguments":"{}"},
					{"type":"custom_tool_call","namespace":"mcp","name":"write","call_id":"fc_2","input":"{}"},
					{"type":"function_call","namespace":"image_gen","name":"generate","call_id":"fc_3","arguments":"{}"},
					{"type":"message","role":"user","namespace":"message-meta","content":[{"type":"input_text","text":"hello","namespace":"nested-meta"}]}
				],
				"client_metadata":{"trace_id":9007199254740995}
			}`)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)

			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{
					"id":"resp_namespace","status":"completed","model":"gpt-5.4",
					"output":[
						{"type":"function_call","name":"mcp__read","call_id":"fc_1","arguments":"{}"},
						{"type":"message","name":"mcp__read","namespace":"upstream-message","content":[{"type":"output_text","text":"ok","namespace":"upstream-nested"}]}
					],
					"usage":{"input_tokens":1,"output_tokens":1}
				}`)),
			}}
			svc := &OpenAIGatewayService{httpUpstream: upstream}
			account := httptestOpenAIOAuthBodyPolicyAccount()
			account.Type = accountType
			if account.Extra == nil {
				account.Extra = make(map[string]any)
			}
			account.Extra["openai_responses_flatten_namespaces"] = true

			_, err := svc.Forward(context.Background(), c, account, body)
			require.NoError(t, err)
			require.Equal(t, "function", gjson.GetBytes(upstream.lastBody, "tools.0.type").String())
			require.Equal(t, "mcp__read", gjson.GetBytes(upstream.lastBody, "tools.0.name").String())
			require.Equal(t, "9007199254740993", gjson.GetBytes(upstream.lastBody, "tools.0.parameters.properties.id.const").Raw)
			require.False(t, gjson.GetBytes(upstream.lastBody, "tools.#(name==\"mcp\")").Exists())
			require.Equal(t, "mcp__read", gjson.GetBytes(upstream.lastBody, "tool_choice.name").String())
			require.False(t, gjson.GetBytes(upstream.lastBody, "tool_choice.namespace").Exists())
			require.Equal(t, "mcp__read", gjson.GetBytes(upstream.lastBody, "input.0.name").String())
			require.False(t, gjson.GetBytes(upstream.lastBody, "input.0.namespace").Exists())
			require.Equal(t, "write", gjson.GetBytes(upstream.lastBody, "input.1.name").String())
			require.False(t, gjson.GetBytes(upstream.lastBody, "input.1.namespace").Exists())
			require.False(t, gjson.GetBytes(upstream.lastBody, "input.2.namespace").Exists())
			require.Equal(t, "message-meta", gjson.GetBytes(upstream.lastBody, "input.3.namespace").String())
			require.Equal(t, "nested-meta", gjson.GetBytes(upstream.lastBody, "input.3.content.0.namespace").String())
			require.Equal(t, "9007199254740995", gjson.GetBytes(upstream.lastBody, "client_metadata.trace_id").Raw)
			require.False(t, bytes.Contains(upstream.lastBody, []byte(`"name":"image_gen"`)), "existing image_gen policy should remain authoritative")

			require.Equal(t, "read", gjson.GetBytes(rec.Body.Bytes(), "output.0.name").String())
			require.Equal(t, "mcp", gjson.GetBytes(rec.Body.Bytes(), "output.0.namespace").String())
			require.Equal(t, "mcp__read", gjson.GetBytes(rec.Body.Bytes(), "output.1.name").String())
			require.Equal(t, "upstream-message", gjson.GetBytes(rec.Body.Bytes(), "output.1.namespace").String())
			require.Equal(t, "upstream-nested", gjson.GetBytes(rec.Body.Bytes(), "output.1.content.0.namespace").String())
		})
	}
}

func TestOpenAIGatewayService_OAuthHTTPKeepsNamespacesNativeByDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.4","stream":false,"tools":[{"type":"namespace","name":"mcp","tools":[{"type":"function","name":"read"}]}],"input":[{"type":"function_call","namespace":"mcp","name":"read","arguments":"{}"}]}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"resp_oauth","status":"completed","model":"gpt-5.4","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)),
	}}
	svc := &OpenAIGatewayService{httpUpstream: upstream}

	_, err := svc.Forward(context.Background(), c, httptestOpenAIOAuthBodyPolicyAccount(), body)
	require.NoError(t, err)
	require.Equal(t, "namespace", gjson.GetBytes(upstream.lastBody, "tools.0.type").String())
	require.Equal(t, "mcp", gjson.GetBytes(upstream.lastBody, "tools.0.name").String())
	require.Equal(t, "mcp", gjson.GetBytes(upstream.lastBody, "input.0.namespace").String())
}

func TestOpenAIGatewayService_SetupTokenHTTPKeepsNamespacesNative(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.4","stream":false,"tools":[{"type":"namespace","name":"mcp","tools":[{"type":"function","name":"read"}]}],"input":[{"type":"tool_call","namespace":"mcp","name":"read"}]}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"resp_setup","status":"completed","model":"gpt-5.4","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)),
	}}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := httptestOpenAIOAuthBodyPolicyAccount()
	account.Type = AccountTypeSetupToken

	_, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.Equal(t, "namespace", gjson.GetBytes(upstream.lastBody, "tools.0.type").String())
	require.Equal(t, "mcp", gjson.GetBytes(upstream.lastBody, "tools.0.name").String())
	require.Equal(t, "mcp", gjson.GetBytes(upstream.lastBody, "input.0.namespace").String())
}

func TestOpenAIGatewayService_OAuthHTTPRestoresNamespaceStreamingResponseWhenEnabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.4","stream":true,"tools":[{"type":"namespace","name":"mcp","tools":[{"type":"function","name":"read"}]}],"input":"hello"}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)

	frames := []string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_stream","status":"in_progress","model":"gpt-5.4","output":[]}}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","name":"mcp__read","call_id":"fc_1","arguments":""}}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_stream","status":"completed","model":"gpt-5.4","output":[{"type":"function_call","name":"mcp__read","call_id":"fc_1","arguments":"{}"}],"usage":{"input_tokens":1,"output_tokens":1}}}`,
		``,
	}
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(strings.Join(frames, "\n"))),
	}}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := httptestOpenAIOAuthBodyPolicyAccount()
	account.Extra = map[string]any{"openai_responses_flatten_namespaces": true}

	_, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.Contains(t, rec.Body.String(), `"name":"read"`)
	require.Contains(t, rec.Body.String(), `"namespace":"mcp"`)
	require.NotContains(t, rec.Body.String(), `"name":"mcp__read"`)
}

func TestOpenAIGatewayService_OAuthNamespaceCollisionRejectedBeforeUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.4","stream":false,"tools":[{"type":"function","name":"mcp__read"},{"type":"namespace","name":"mcp","tools":[{"type":"function","name":"read"}]}],"input":"hello"}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`))}}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	account := httptestOpenAIOAuthBodyPolicyAccount()
	account.Extra = map[string]any{"openai_responses_flatten_namespaces": true}

	_, err := svc.Forward(context.Background(), c, account, body)
	require.Error(t, err)
	require.Empty(t, upstream.requests)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "tools", gjson.GetBytes(rec.Body.Bytes(), "error.param").String())
	require.NotContains(t, rec.Body.String(), "mcp")
	require.NotContains(t, rec.Body.String(), "read")
	require.NotContains(t, rec.Body.String(), "arguments")
	_, hasOpsMessage := c.Get(OpsUpstreamErrorMessageKey)
	require.False(t, hasOpsMessage, "local validation details must not be recorded as upstream errors")
}

func TestOpenAIGatewayService_APIKeyPassthroughDoesNotNormalizeNamespaces(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.4","stream":false,"tools":[{"type":"namespace","name":"mcp","tools":[{"type":"function","name":"read"}]}],"input":[{"type":"function_call","namespace":"mcp","name":"read","arguments":"{}"}]}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"id":"resp_api","status":"completed","model":"gpt-5.4","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)),
	}}
	svc := &OpenAIGatewayService{httpUpstream: upstream, cfg: &config.Config{}}
	svc.cfg.Security.URLAllowlist.Enabled = false
	account := &Account{ID: 55, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1, Credentials: map[string]any{"api_key": "secret"}, Extra: map[string]any{"openai_passthrough": true}}

	_, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.JSONEq(t, string(body), string(upstream.lastBody))
}

func TestOpenAIResponsesNamespaceAdapter_FlattensOnlyForCompatibility(t *testing.T) {
	oauth := httptestOpenAIOAuthBodyPolicyAccount()
	if oauth.Extra == nil {
		oauth.Extra = make(map[string]any)
	}
	apiKey := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	require.False(t, shouldNormalizeOpenAIResponsesNamespaces(oauth, OpenAIUpstreamTransportHTTPSSE, OpenAIClientTransportHTTP, false))
	require.True(t, shouldNormalizeOpenAIResponsesNamespaces(oauth, OpenAIUpstreamTransportHTTPSSE, OpenAIClientTransportHTTP, true))
	oauth.Extra["openai_responses_flatten_namespaces"] = true
	require.True(t, shouldNormalizeOpenAIResponsesNamespaces(oauth, OpenAIUpstreamTransportHTTPSSE, OpenAIClientTransportHTTP, false))
	require.False(t, shouldNormalizeOpenAIResponsesNamespaces(oauth, OpenAIUpstreamTransportResponsesWebsocketV2, OpenAIClientTransportWS, false))
	require.False(t, shouldNormalizeOpenAIResponsesNamespaces(apiKey, OpenAIUpstreamTransportHTTPSSE, OpenAIClientTransportHTTP, true))
	require.False(t, shouldNormalizeOpenAIResponsesNamespaces(nil, OpenAIUpstreamTransportHTTPSSE, OpenAIClientTransportHTTP, true))
}

func TestOpenAIResponsesNamespaceAdapter_TerminalStripUsesExactOAuthHTTPPredicate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	newContext := func(clientTransport OpenAIClientTransport, upstreamTransport OpenAIUpstreamTransport) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		SetOpenAIClientTransport(c, clientTransport)
		c.Set("openai_ws_transport_decision", string(upstreamTransport))
		return c
	}
	oauth := httptestOpenAIOAuthBodyPolicyAccount()
	setupToken := httptestOpenAIOAuthBodyPolicyAccount()
	setupToken.Type = AccountTypeSetupToken

	require.False(t, shouldStripOpenAIResponsesInputNamespaces(newContext(OpenAIClientTransportHTTP, OpenAIUpstreamTransportHTTPSSE), oauth))
	oauth.Extra = map[string]any{"openai_responses_flatten_namespaces": true}
	require.True(t, shouldStripOpenAIResponsesInputNamespaces(newContext(OpenAIClientTransportHTTP, OpenAIUpstreamTransportHTTPSSE), oauth))
	require.False(t, shouldStripOpenAIResponsesInputNamespaces(newContext(OpenAIClientTransportWS, OpenAIUpstreamTransportResponsesWebsocketV2), oauth))
	require.False(t, shouldStripOpenAIResponsesInputNamespaces(newContext(OpenAIClientTransportHTTP, OpenAIUpstreamTransportHTTPSSE), setupToken))
	require.False(t, shouldStripOpenAIResponsesInputNamespaces(nil, oauth))
}

func TestRestoreOpenAIResponsesNamespacePayload_NonObjectPayloadPassesThrough(t *testing.T) {
	payload := []byte(`[{"type":"function_call","name":"mcp__read"}]`)

	restored, changed, err := apicompat.RestoreResponsesNamespaceCalls(payload, map[string]apicompat.ResponsesNamespaceName{
		"mcp__read": {Namespace: "mcp", Name: "read"},
	})
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, payload, restored)
}

func TestOpenAIGatewayService_OAuthFailoverToAPIKeyClearsNamespaceMapping(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.4","stream":false,"tools":[{"type":"namespace","name":"mcp","tools":[{"type":"function","name":"read"}]}],"input":"hello"}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		{
			StatusCode: http.StatusServiceUnavailable,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"temporarily unavailable"}}`)),
		},
		{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"resp_api","status":"completed","model":"gpt-5.4","output":[{"type":"function_call","name":"mcp__read","call_id":"fc_1","arguments":"{}"}],"usage":{"input_tokens":1,"output_tokens":1}}`)),
		},
	}}
	svc := &OpenAIGatewayService{httpUpstream: upstream, cfg: &config.Config{}}
	svc.cfg.Security.URLAllowlist.Enabled = false

	_, err := svc.Forward(context.Background(), c, httptestOpenAIOAuthBodyPolicyAccount(), body)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Empty(t, rec.Body.String())

	apiKey := &Account{
		ID: 92, Name: "openai-api-key", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Concurrency: 1, Status: StatusActive, Schedulable: true,
		Credentials: map[string]any{"api_key": "sk-api-key"},
		Extra:       map[string]any{"use_responses_api": true},
	}
	_, err = svc.Forward(context.Background(), c, apiKey, body)
	require.NoError(t, err)
	require.Equal(t, "mcp__read", gjson.GetBytes(rec.Body.Bytes(), "output.0.name").String())
	require.False(t, gjson.GetBytes(rec.Body.Bytes(), "output.0.namespace").Exists())
	require.Len(t, upstream.requests, 2)
}

func TestRestoreOpenAIResponsesNamespacePayload_MappingIsRequestScoped(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c1, _ := gin.CreateTestContext(httptest.NewRecorder())
	c2, _ := gin.CreateTestContext(httptest.NewRecorder())
	setOpenAIResponsesNamespaceNames(c1, map[string]apicompat.ResponsesNamespaceName{"mcp__read": {Namespace: "mcp", Name: "read"}})
	payload := []byte(`{"type":"function_call","name":"mcp__read"}`)

	restored, err := restoreOpenAIResponsesNamespacePayload(c1, payload)
	require.NoError(t, err)
	require.Equal(t, "read", gjson.GetBytes(restored, "name").String())
	untouched, err := restoreOpenAIResponsesNamespacePayload(c2, payload)
	require.NoError(t, err)
	require.Equal(t, payload, untouched)
}

func TestHandleStreamingResponse_NamespaceRestorationFailureAfterOutputDoesNotFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	setOpenAIResponsesNamespaceNames(c, map[string]apicompat.ResponsesNamespaceName{"mcp__read": {Namespace: "mcp", Name: "read"}})
	original := []byte(`{"type":"function_call","name":"mcp__read"}`)
	calls := 0
	restore := func(c *gin.Context, payload []byte) ([]byte, error) {
		calls++
		if calls == 2 {
			return payload, errors.New("deterministic restoration failure")
		}
		return restoreOpenAIResponsesNamespacePayload(c, payload)
	}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"response.output_text.delta","delta":"hello"}`,
			``,
			`data: ` + string(original),
			``,
			`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":3}}}`,
			``,
		}, "\n"))),
	}

	result, err := (&OpenAIGatewayService{}).handleStreamingResponseWithNamespaceRestorer(context.Background(), resp, c, &Account{ID: 1}, time.Now(), "gpt-5.4", "gpt-5.4", restore)
	require.NoError(t, err)
	require.Equal(t, 2, result.usage.InputTokens)
	require.Equal(t, 3, result.usage.OutputTokens)
	require.Contains(t, rec.Body.String(), `"delta":"hello"`)
	require.Contains(t, rec.Body.String(), string(original), "failed local conversion must pass through the original event after output starts")
}

func TestHandleStreamingResponse_NamespaceRestorationFailureBeforeOutputReturnsError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n")),
	}
	restoreErr := errors.New("deterministic restoration failure")

	result, err := (&OpenAIGatewayService{}).handleStreamingResponseWithNamespaceRestorer(
		context.Background(), resp, c, &Account{ID: 1}, time.Now(), "gpt-5.4", "gpt-5.4",
		func(_ *gin.Context, payload []byte) ([]byte, error) { return payload, restoreErr },
	)
	require.ErrorIs(t, err, restoreErr)
	require.NotNil(t, result)
	require.Empty(t, rec.Body.String())
}

func TestHandleStreamingResponse_NamespaceRestorationKeepsUsageAndTerminalState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	setOpenAIResponsesNamespaceNames(c, map[string]apicompat.ResponsesNamespaceName{"mcp__read": {Namespace: "mcp", Name: "read"}})
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"response.output_item.added","item":{"type":"function_call","name":"mcp__read","call_id":"fc_1","arguments":""}}`,
			``,
			`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"function_call","name":"mcp__read","call_id":"fc_1","arguments":"{}"}],"usage":{"input_tokens":2,"output_tokens":3}}}`,
			``,
		}, "\n"))),
	}

	result, err := (&OpenAIGatewayService{}).handleStreamingResponse(context.Background(), resp, c, &Account{ID: 1}, time.Now(), "gpt-5.4", "gpt-5.4")
	require.NoError(t, err)
	require.Equal(t, 2, result.usage.InputTokens)
	require.Equal(t, 3, result.usage.OutputTokens)
	require.Contains(t, rec.Body.String(), `"namespace":"mcp"`)
}
