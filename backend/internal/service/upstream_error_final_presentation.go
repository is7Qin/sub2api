package service

import "net/http"

const genericUpstreamFailureMessage = "Upstream request failed"

type FinalUpstreamErrorResolution struct {
	Presentation   UpstreamClientPresentation
	RuleMatched    bool
	SkipMonitoring bool
}

func ResolveFinalUpstreamError(fact UpstreamErrorFact, rules *ErrorPassthroughService) FinalUpstreamErrorResolution {
	if policy, ok := ResolveUpstreamRecoveryPolicy(fact); ok && policy.CandidateRank == UpstreamCandidateStructured {
		return FinalUpstreamErrorResolution{Presentation: policy.Presentation}
	}

	fallback := safeUnknownUpstreamPresentation(fact)
	if rules == nil {
		return FinalUpstreamErrorResolution{Presentation: fallback}
	}
	// Built-in semantics have already declined this fact, so query the mutable
	// rule set exactly once before choosing the safe fallback.
	rule := rules.MatchUnknownRule(fact)
	if rule == nil {
		return FinalUpstreamErrorResolution{Presentation: fallback}
	}

	presentation := fallback
	if rule.PassthroughCode {
		// A rule can preserve only a real upstream HTTP status. Status-unknown
		// SSE/WS/transport facts retain the safe 502 fallback.
		if fact.HTTPStatusKnown && fact.HTTPStatus >= 400 && fact.HTTPStatus <= 599 {
			presentation.HTTPStatus = fact.HTTPStatus
		}
	} else if rule.ResponseCode != nil {
		presentation.HTTPStatus = *rule.ResponseCode
	}
	if rule.PassthroughBody && fact.SafeMessage != "" {
		presentation.Message = sanitizeUpstreamErrorFactScalar(fact.SafeMessage, upstreamErrorFactMaxScalarBytes)
	} else if !rule.PassthroughBody && rule.CustomMessage != nil {
		presentation.Message = sanitizeUpstreamErrorFactScalar(*rule.CustomMessage, upstreamErrorFactMaxScalarBytes)
	}
	if presentation.Message == "" {
		presentation.Message = genericUpstreamFailureMessage
	}
	return FinalUpstreamErrorResolution{
		Presentation:   presentation,
		RuleMatched:    true,
		SkipMonitoring: rule.SkipMonitoring,
	}
}

func safeUnknownUpstreamPresentation(fact UpstreamErrorFact) UpstreamClientPresentation {
	status := http.StatusBadGateway
	if fact.HTTPStatusKnown && fact.HTTPStatus >= http.StatusBadRequest && fact.HTTPStatus < http.StatusInternalServerError {
		status = fact.HTTPStatus
	}
	return UpstreamClientPresentation{
		HTTPStatus: status,
		ErrorType:  "upstream_error",
		Message:    genericUpstreamFailureMessage,
	}
}
