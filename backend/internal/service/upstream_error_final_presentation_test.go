package service

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/stretchr/testify/require"
)

func TestResolveFinalUpstreamError(t *testing.T) {
	responseCode := http.StatusTeapot
	customMessage := "administrator-approved message"
	rules := &ErrorPassthroughService{}
	rules.setLocalCache([]*model.ErrorPassthroughRule{{
		Enabled:         true,
		Priority:        1,
		Platforms:       []string{PlatformOpenAI},
		Keywords:        []string{"vendor_failure"},
		MatchMode:       model.MatchModeAny,
		PassthroughCode: false,
		ResponseCode:    &responseCode,
		PassthroughBody: false,
		CustomMessage:   &customMessage,
		SkipMonitoring:  true,
	}})

	tests := []struct {
		name       string
		fact       UpstreamErrorFact
		rules      *ErrorPassthroughService
		wantStatus int
		wantCode   string
		wantType   string
		wantMsg    string
		wantSkip   bool
	}{
		{
			name: "recognized overload bypasses conflicting rules",
			fact: UpstreamErrorFact{
				Provider: PlatformOpenAI, Source: UpstreamErrorSourceHTTP,
				HTTPStatusKnown: true, HTTPStatus: http.StatusServiceUnavailable,
				ProviderCode: "server_is_overloaded", ProviderType: "service_unavailable_error",
				SafeMessage: "retry later", InternalMatchText: "openai server_is_overloaded retry later",
			},
			rules: rules, wantStatus: http.StatusServiceUnavailable,
			wantCode: "server_is_overloaded", wantType: "service_unavailable_error", wantMsg: "retry later",
		},
		{
			name: "structured rate limit remains authoritative",
			fact: UpstreamErrorFact{
				Provider: PlatformOpenAI, Source: UpstreamErrorSourceHTTP,
				HTTPStatusKnown: true, HTTPStatus: http.StatusTooManyRequests,
				ProviderCode: "rate_limit_exceeded", ProviderType: "rate_limit_error",
				SafeMessage: "quota exceeded", InternalMatchText: "openai rate_limit_exceeded quota exceeded",
			},
			wantStatus: http.StatusTooManyRequests, wantCode: "rate_limit_exceeded",
			wantType: "rate_limit_error", wantMsg: "quota exceeded",
		},
		{
			name: "unknown matched custom-code rule uses approved presentation",
			fact: UpstreamErrorFact{
				Provider: PlatformOpenAI, Source: UpstreamErrorSourceHTTP,
				HTTPStatusKnown: true, HTTPStatus: http.StatusBadGateway,
				ProviderCode: "vendor_failure", SafeMessage: "bounded vendor text",
				InternalMatchText: "openai vendor_failure bounded vendor text",
			},
			rules: rules, wantStatus: http.StatusTeapot, wantType: "upstream_error",
			wantMsg: customMessage, wantSkip: true,
		},
		{
			name: "unknown real 400 keeps safe 400",
			fact: UpstreamErrorFact{
				Provider: PlatformAnthropic, Source: UpstreamErrorSourceHTTP,
				HTTPStatusKnown: true, HTTPStatus: http.StatusBadRequest,
				SafeMessage: "secret upstream detail",
			},
			wantStatus: http.StatusBadRequest, wantType: "upstream_error", wantMsg: "Upstream request failed",
		},
		{
			name: "unknown real 503 becomes 502",
			fact: UpstreamErrorFact{
				Provider: PlatformOpenAI, Source: UpstreamErrorSourceHTTP,
				HTTPStatusKnown: true, HTTPStatus: http.StatusServiceUnavailable,
				SafeMessage: "unknown provider failure",
			},
			wantStatus: http.StatusBadGateway, wantType: "upstream_error", wantMsg: "Upstream request failed",
		},
		{
			name: "status unknown sse becomes 502",
			fact: UpstreamErrorFact{
				Provider: PlatformAnthropic, Source: UpstreamErrorSourceSSE,
				SafeMessage: "unknown stream failure",
			},
			wantStatus: http.StatusBadGateway, wantType: "upstream_error", wantMsg: "Upstream request failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveFinalUpstreamError(tt.fact, tt.rules)
			require.Equal(t, tt.wantStatus, got.Presentation.HTTPStatus)
			require.Equal(t, tt.wantCode, got.Presentation.ErrorCode)
			require.Equal(t, tt.wantType, got.Presentation.ErrorType)
			require.Equal(t, tt.wantMsg, got.Presentation.Message)
			require.Equal(t, tt.wantSkip, got.SkipMonitoring)
			wantRule := tt.name == "unknown matched custom-code rule uses approved presentation"
			require.Equal(t, wantRule, got.RuleMatched)
		})
	}
}

func TestResolveFinalUpstreamErrorMatchedPassthroughCodeUsesRealStatusOnly(t *testing.T) {
	customMessage := "administrator-approved message"
	rules := &ErrorPassthroughService{}
	rules.setLocalCache([]*model.ErrorPassthroughRule{{
		Enabled: true, Priority: 1, Platforms: []string{PlatformOpenAI},
		Keywords: []string{"vendor_failure"}, MatchMode: model.MatchModeAny,
		PassthroughCode: true, PassthroughBody: false,
		CustomMessage: &customMessage,
	}})

	known := ResolveFinalUpstreamError(UpstreamErrorFact{
		Provider: PlatformOpenAI, Source: UpstreamErrorSourceHTTP,
		HTTPStatusKnown: true, HTTPStatus: http.StatusTooManyRequests,
		ProviderCode: "vendor_failure", InternalMatchText: "vendor_failure",
	}, rules)
	require.Equal(t, http.StatusTooManyRequests, known.Presentation.HTTPStatus)
	require.True(t, known.RuleMatched)

	unknown := ResolveFinalUpstreamError(UpstreamErrorFact{
		Provider: PlatformOpenAI, Source: UpstreamErrorSourceSSE,
		ProviderCode: "vendor_failure", InternalMatchText: "vendor_failure",
	}, rules)
	require.Equal(t, http.StatusBadGateway, unknown.Presentation.HTTPStatus)
	require.True(t, unknown.RuleMatched)
}

func TestResolveFinalUpstreamErrorMatchedPassthroughBodyUsesBoundedFactMessage(t *testing.T) {
	rules := &ErrorPassthroughService{}
	rules.setLocalCache([]*model.ErrorPassthroughRule{{
		Enabled: true, Priority: 1, Platforms: []string{PlatformOpenAI},
		Keywords: []string{"vendor_failure"}, MatchMode: model.MatchModeAny,
		PassthroughCode: true, PassthroughBody: true,
	}})
	fact := ParseOpenAIJSONErrorFact(PlatformOpenAI, UpstreamErrorSourceHTTP, []byte(
		`{"error":{"code":"vendor_failure","message":"Bearer sk-secret https://internal.example/ `+
			strings.Repeat("x", upstreamErrorFactMaxScalarBytes*2)+`"}}`,
	), "")
	fact.HTTPStatusKnown = true
	fact.HTTPStatus = http.StatusBadRequest

	got := ResolveFinalUpstreamError(fact, rules)

	require.Equal(t, http.StatusBadRequest, got.Presentation.HTTPStatus)
	require.LessOrEqual(t, len(got.Presentation.Message), upstreamErrorFactMaxScalarBytes)
	require.NotContains(t, got.Presentation.Message, "sk-secret")
	require.NotContains(t, got.Presentation.Message, "internal.example")
}
