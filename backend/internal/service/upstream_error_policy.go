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

type UpstreamAccountHealthAction string

const (
	UpstreamHealthNone                  UpstreamAccountHealthAction = "none"
	UpstreamHealthRecordOnly            UpstreamAccountHealthAction = "record_only"
	UpstreamHealthApplyRateLimit        UpstreamAccountHealthAction = "apply_rate_limit"
	UpstreamHealthTemporarilyUnschedule UpstreamAccountHealthAction = "temporarily_unschedule"
	UpstreamHealthPermanentlyDisable    UpstreamAccountHealthAction = "permanently_disable"
	UpstreamHealthApplyModelRateLimit   UpstreamAccountHealthAction = "apply_model_rate_limit"
)

type UpstreamCandidateRank uint8

const (
	UpstreamCandidateGenericTransport UpstreamCandidateRank = iota
	UpstreamCandidateStatusOnly
	UpstreamCandidateStructured
)

type UpstreamErrorCandidate struct {
	Presentation UpstreamClientPresentation
	Rank         UpstreamCandidateRank
}

// UpstreamRecoveryPolicy separates bounded recovery decisions from client
// presentation and the legacy account-health/error carriers.
type UpstreamRecoveryPolicy struct {
	Disposition             UpstreamAttemptDisposition
	SameAccountRetryBudget  int
	AccountTransitionBudget int
	AccountHealthAction     UpstreamAccountHealthAction
	CandidateRank           UpstreamCandidateRank
	Presentation            UpstreamClientPresentation
}

func NewUpstreamErrorCandidate(fact UpstreamErrorFact, rank UpstreamCandidateRank) *UpstreamErrorCandidate {
	presentation := normalizedUpstreamClientPresentation(fact)
	if presentation.HTTPStatus == 0 {
		if fact.HTTPStatusKnown && fact.HTTPStatus > 0 {
			presentation.HTTPStatus = fact.HTTPStatus
		} else {
			presentation.HTTPStatus = http.StatusBadGateway
		}
	}
	if presentation.ErrorType == "" {
		presentation.ErrorType = "api_error"
	}
	if presentation.Message == "" {
		presentation.Message = "Upstream request failed"
	}
	return &UpstreamErrorCandidate{Presentation: presentation, Rank: rank}
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

func ResolveUpstreamRecoveryPolicy(fact UpstreamErrorFact) (UpstreamRecoveryPolicy, bool) {
	if recognized, ok := RecognizeUpstreamErrorFact(fact); ok {
		return UpstreamRecoveryPolicy{
			Disposition:         recognized.Disposition,
			AccountHealthAction: UpstreamHealthNone,
			CandidateRank:       UpstreamCandidateStructured,
			Presentation:        recognized.Presentation,
		}, true
	}

	if strings.EqualFold(strings.TrimSpace(fact.ProviderCode), "rate_limit_exceeded") ||
		strings.EqualFold(strings.TrimSpace(fact.ProviderType), "rate_limit_error") {
		return UpstreamRecoveryPolicy{
			Disposition:             UpstreamAttemptFailover,
			SameAccountRetryBudget:  1,
			AccountTransitionBudget: 1,
			AccountHealthAction:     UpstreamHealthApplyRateLimit,
			CandidateRank:           UpstreamCandidateStructured,
			Presentation:            NewUpstreamErrorCandidate(fact, UpstreamCandidateStructured).Presentation,
		}, true
	}
	if fact.HTTPStatusKnown && fact.HTTPStatus >= http.StatusInternalServerError {
		return UpstreamRecoveryPolicy{
			Disposition:             UpstreamAttemptFailover,
			AccountTransitionBudget: 1,
			AccountHealthAction:     UpstreamHealthRecordOnly,
			CandidateRank:           UpstreamCandidateStatusOnly,
			Presentation:            NewUpstreamErrorCandidate(fact, UpstreamCandidateStatusOnly).Presentation,
		}, true
	}
	return UpstreamRecoveryPolicy{}, false
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

// RecognizeLegacyUpstreamError classifies body-only error boundaries before
// legacy database passthrough matching. It uses only the bounded fact fields.
func RecognizeLegacyUpstreamError(provider string, statusCode int, body []byte) (RecognizedUpstreamErrorPolicy, bool) {
	source := UpstreamErrorSourceHTTP
	if strings.EqualFold(provider, PlatformAnthropic) {
		// Anthropic native stream errors arrive as event:error with HTTP 200;
		// legacy failover callers retain only the bounded terminal body.
		source = UpstreamErrorSourceSSE
	}
	fact := ParseOpenAIJSONErrorFact(provider, source, body, "")
	fact.HTTPStatusKnown = statusCode > 0
	fact.HTTPStatus = statusCode
	return RecognizeUpstreamErrorFact(fact)
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
