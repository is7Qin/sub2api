package service

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

type UpstreamAttemptDisposition string

const (
	UpstreamAttemptDirectReturn     UpstreamAttemptDisposition = "direct_return"
	UpstreamAttemptRetrySameAccount UpstreamAttemptDisposition = "retry_same_account"
	UpstreamAttemptFailover         UpstreamAttemptDisposition = "failover"
	UpstreamAttemptGenericAbort     UpstreamAttemptDisposition = "generic_abort"
)

type UpstreamClientPresentation struct {
	HTTPStatus int
	ErrorCode  string
	ErrorType  string
	Message    string
}

type RecognizedUpstreamErrorPolicy struct {
	Disposition  UpstreamAttemptDisposition
	Presentation UpstreamClientPresentation
}

// RecognizedUpstreamError is a direct, safe presentation that must bypass
// account health and failover handling.
type RecognizedUpstreamError struct {
	Presentation  UpstreamClientPresentation
	RequestID     string
	Usage         *OpenAIUsage
	OutputStarted bool
}

func (e *RecognizedUpstreamError) Error() string {
	if e == nil {
		return "recognized upstream error"
	}
	return "recognized upstream error: " + e.Presentation.ErrorCode
}

func newRecognizedUpstreamError(policy RecognizedUpstreamErrorPolicy, fact UpstreamErrorFact) *RecognizedUpstreamError {
	return &RecognizedUpstreamError{
		Presentation: policy.Presentation,
		RequestID:    fact.RequestID,
	}
}

func newRecognizedOpenAIUpstreamRequestError(fact UpstreamErrorFact) *OpenAIUpstreamRequestError {
	policy, ok := RecognizeUpstreamErrorFact(fact)
	if !ok {
		return nil
	}
	return newRecognizedOpenAIUpstreamRequestErrorForPolicy(policy, fact.RequestID)
}

func newRecognizedOpenAIUpstreamRequestErrorForPolicy(policy RecognizedUpstreamErrorPolicy, requestID string) *OpenAIUpstreamRequestError {
	return &OpenAIUpstreamRequestError{
		StatusCode: policy.Presentation.HTTPStatus,
		Code:       policy.Presentation.ErrorCode,
		Type:       policy.Presentation.ErrorType,
		Message:    policy.Presentation.Message,
		RequestID:  requestID,
	}
}

func isNewRecognizedDirectOpenAIError(fact UpstreamErrorFact) bool {
	if strings.EqualFold(fact.ProviderCode, "context_length_exceeded") {
		return false
	}
	_, ok := RecognizeUpstreamErrorFact(fact)
	return ok
}

func (e *RecognizedUpstreamError) observeTerminal(usage OpenAIUsage, outputStarted bool) {
	if e == nil {
		return
	}
	usageCopy := usage
	e.Usage = &usageCopy
	e.OutputStarted = outputStarted
}

func RecognizeUpstreamErrorFact(fact UpstreamErrorFact) (RecognizedUpstreamErrorPolicy, bool) {
	code := strings.TrimSpace(fact.ProviderCode)
	errType := strings.TrimSpace(fact.ProviderType)
	presentation := normalizedUpstreamClientPresentation(fact)
	if strings.EqualFold(fact.Provider, PlatformAnthropic) && fact.Source == UpstreamErrorSourceSSE {
		switch {
		case strings.EqualFold(errType, "invalid_request_error"):
			return RecognizedUpstreamErrorPolicy{
				Disposition:  UpstreamAttemptDirectReturn,
				Presentation: withUpstreamClientPresentationStatus(presentation, http.StatusBadRequest),
			}, true
		case strings.EqualFold(errType, "overloaded_error"):
			presentation = withUpstreamClientPresentationStatus(presentation, http.StatusServiceUnavailable)
			if presentation.Message == "" {
				presentation.Message = "The upstream service is temporarily overloaded"
			}
			return RecognizedUpstreamErrorPolicy{
				Disposition:  UpstreamAttemptDirectReturn,
				Presentation: presentation,
			}, true
		}
	}
	if strings.EqualFold(code, "cyber_policy") {
		presentation = withUpstreamClientPresentationStatus(presentation, http.StatusBadRequest)
		if presentation.ErrorType == "" || strings.EqualFold(presentation.ErrorType, "response.failed") {
			presentation.ErrorType = "invalid_request_error"
		}
		return RecognizedUpstreamErrorPolicy{
			Disposition:  UpstreamAttemptDirectReturn,
			Presentation: presentation,
		}, true
	}
	if strings.EqualFold(code, "context_length_exceeded") {
		return RecognizedUpstreamErrorPolicy{
			Disposition: UpstreamAttemptDirectReturn,
			Presentation: UpstreamClientPresentation{
				HTTPStatus: http.StatusBadRequest,
				ErrorCode:  "context_length_exceeded",
				ErrorType:  "invalid_request_error",
				Message:    presentation.Message,
			},
		}, true
	}
	if strings.EqualFold(code, "server_is_overloaded") {
		presentation = withUpstreamClientPresentationStatus(presentation, http.StatusServiceUnavailable)
		if presentation.ErrorType == "" || strings.EqualFold(presentation.ErrorType, "response.failed") {
			presentation.ErrorType = "service_unavailable_error"
		}
		if presentation.Message == "" {
			presentation.Message = "The upstream service is temporarily overloaded"
		}
		return RecognizedUpstreamErrorPolicy{
			Disposition:  UpstreamAttemptDirectReturn,
			Presentation: presentation,
		}, true
	}
	return RecognizedUpstreamErrorPolicy{}, false
}

func writeRecognizedOpenAIHTTPError(c *gin.Context, presentation UpstreamClientPresentation) {
	MarkResponseCommitted(c)
	c.JSON(presentation.HTTPStatus, gin.H{
		"error": gin.H{
			"code":    presentation.ErrorCode,
			"type":    presentation.ErrorType,
			"message": presentation.Message,
		},
	})
}

func normalizedUpstreamClientPresentation(fact UpstreamErrorFact) UpstreamClientPresentation {
	return UpstreamClientPresentation{
		ErrorCode: sanitizeUpstreamErrorFactScalar(fact.ProviderCode, upstreamErrorFactMaxScalarBytes),
		ErrorType: sanitizeUpstreamErrorFactScalar(fact.ProviderType, upstreamErrorFactMaxScalarBytes),
		Message:   sanitizeUpstreamErrorFactScalar(fact.SafeMessage, upstreamErrorFactMaxScalarBytes),
	}
}

func withUpstreamClientPresentationStatus(presentation UpstreamClientPresentation, status int) UpstreamClientPresentation {
	presentation.HTTPStatus = status
	return presentation
}
