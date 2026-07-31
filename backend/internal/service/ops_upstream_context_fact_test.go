package service

import (
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAppendOpsUpstreamError_RecognizedFactDeclinesRulePrecedence(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)
	service := &ErrorPassthroughService{}
	service.setLocalCache([]*model.ErrorPassthroughRule{{
		Enabled: true, Priority: 1, Platforms: []string{PlatformAnthropic},
		Keywords: []string{"cyber_policy"}, MatchMode: model.MatchModeAny,
		SkipMonitoring: true,
	}})
	BindErrorPassthroughService(c, service)
	fact := UpstreamErrorFact{
		Provider: PlatformAnthropic, Source: UpstreamErrorSourceHTTP,
		HTTPStatusKnown: true, HTTPStatus: http.StatusUnauthorized,
		ProviderCode: "cyber_policy", InternalMatchText: "anthropic cyber_policy",
	}

	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform: PlatformAnthropic, UpstreamStatusCode: http.StatusUnauthorized,
		Message: "policy rejected", UpstreamFact: &fact,
	})

	_, skipped := c.Get(OpsSkipPassthroughKey)
	require.False(t, skipped)
}

func TestAppendOpsUpstreamError_UnknownSSEFactUsesRulePrecedenceWithoutStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)
	rules := &ErrorPassthroughService{}
	rules.setLocalCache([]*model.ErrorPassthroughRule{{
		Enabled: true, Priority: 1, Platforms: []string{PlatformAnthropic},
		Keywords: []string{"stream failed"}, MatchMode: model.MatchModeAny,
		SkipMonitoring: true,
	}})
	BindErrorPassthroughService(c, rules)
	fact := ParseAnthropicSSEErrorFact(PlatformAnthropic, []byte(`{"error":{"type":"vendor_stream_error","message":"stream failed"}}`), "req_sse")

	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform: PlatformAnthropic, UpstreamStatusCode: 0, UpstreamFact: &fact,
		UpstreamRequestID: fact.RequestID, Message: fact.SafeMessage,
	})

	_, skipped := c.Get(OpsSkipPassthroughKey)
	require.True(t, skipped)
	rawEvents, exists := c.Get(OpsUpstreamErrorsKey)
	require.True(t, exists)
	events := rawEvents.([]*OpsUpstreamErrorEvent)
	require.Len(t, events, 1)
	require.Zero(t, events[0].UpstreamStatusCode)
	require.NotNil(t, events[0].UpstreamFact)
	require.Equal(t, UpstreamErrorSourceSSE, events[0].UpstreamFact.Source)
	require.False(t, events[0].UpstreamFact.HTTPStatusKnown)
}

func TestAppendOpsUpstreamError_UnknownFactUsesRulePrecedence(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(nil)
	service := &ErrorPassthroughService{}
	service.setLocalCache([]*model.ErrorPassthroughRule{{
		Enabled: true, Priority: 1, Platforms: []string{PlatformAnthropic},
		Keywords: []string{"vendor_failure"}, MatchMode: model.MatchModeAny,
		SkipMonitoring: true,
	}})
	BindErrorPassthroughService(c, service)
	fact := UpstreamErrorFact{
		Provider: PlatformAnthropic, Source: UpstreamErrorSourceHTTP,
		HTTPStatusKnown: true, HTTPStatus: http.StatusServiceUnavailable,
		ProviderCode: "vendor_failure", InternalMatchText: "anthropic vendor_failure",
	}

	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform: PlatformAnthropic, UpstreamStatusCode: http.StatusServiceUnavailable,
		Message: "vendor failure", UpstreamFact: &fact,
	})

	_, skipped := c.Get(OpsSkipPassthroughKey)
	require.True(t, skipped)
}
