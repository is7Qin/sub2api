package service

import "github.com/gin-gonic/gin"

const errorPassthroughServiceContextKey = "error_passthrough_service"

// BindErrorPassthroughService 将错误透传服务绑定到请求上下文，供 service 层在非 failover 场景下复用规则。
func BindErrorPassthroughService(c *gin.Context, svc *ErrorPassthroughService) {
	if c == nil || svc == nil {
		return
	}
	c.Set(errorPassthroughServiceContextKey, svc)
}

func getBoundErrorPassthroughService(c *gin.Context) *ErrorPassthroughService {
	if c == nil {
		return nil
	}
	v, ok := c.Get(errorPassthroughServiceContextKey)
	if !ok {
		return nil
	}
	svc, ok := v.(*ErrorPassthroughService)
	if !ok {
		return nil
	}
	return svc
}

// NewLegacyUpstreamErrorFact creates the bounded fact used by legacy final-error
// boundaries before any configurable rule can inspect it.
func NewLegacyUpstreamErrorFact(platform string, upstreamStatus int, responseBody []byte) UpstreamErrorFact {
	fact := ParseOpenAIJSONErrorFact(platform, UpstreamErrorSourceHTTP, responseBody, "")
	if upstreamStatus > 0 {
		fact.HTTPStatusKnown = true
		fact.HTTPStatus = upstreamStatus
	}
	return fact
}

// applyErrorPassthroughRule 按规则改写错误响应；未命中时返回默认响应参数。
func applyErrorPassthroughRule(
	c *gin.Context,
	platform string,
	upstreamStatus int,
	responseBody []byte,
	defaultStatus int,
	defaultErrType string,
	defaultErrMsg string,
) (status int, errType string, errMsg string, matched bool) {
	status = defaultStatus
	errType = defaultErrType
	errMsg = defaultErrMsg

	svc := getBoundErrorPassthroughService(c)
	if svc == nil {
		return status, errType, errMsg, false
	}

	fact := NewLegacyUpstreamErrorFact(platform, upstreamStatus, responseBody)
	if policy, recognized := RecognizeUpstreamErrorFact(fact); recognized {
		if policy.Presentation.HTTPStatus > 0 {
			status = policy.Presentation.HTTPStatus
		}
		if policy.Presentation.ErrorType != "" {
			errType = policy.Presentation.ErrorType
		}
		if policy.Presentation.Message != "" {
			errMsg = policy.Presentation.Message
		}
		return status, errType, errMsg, true
	}

	resolved := ResolveFinalUpstreamError(fact, svc)
	if !resolved.RuleMatched {
		return status, errType, errMsg, false
	}
	if resolved.SkipMonitoring {
		c.Set(OpsSkipPassthroughKey, true)
	}
	presentation := resolved.Presentation
	return presentation.HTTPStatus, presentation.ErrorType, presentation.Message, true
}
