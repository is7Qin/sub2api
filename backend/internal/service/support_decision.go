package service

// SupportDecisionResult is the local classifier result. Unknown state must
// retain the caller's ordinary availability error rather than become a 404.
type SupportDecisionResult uint8

const (
	SupportDecisionUnknown SupportDecisionResult = iota
	SupportDecisionNotPureMiss
	SupportDecisionPureMiss
)

// SupportDecisionScope identifies persistent account membership. GroupID zero
// denotes the ungrouped/default scope.
type SupportDecisionScope struct {
	Platform       string
	GroupID        int64
	IncludeGrouped bool
}

type SupportDecisionQuery struct {
	Scope              SupportDecisionScope
	RequestedModel     string
	RequiresPrivacy    bool
	EndpointCapability OpenAIEndpointCapability
	ImageCapability    OpenAIImagesCapability
	RequireCompact     bool
	Transport          OpenAIUpstreamTransport
}

type SupportDecisionReader interface {
	Lookup(query SupportDecisionQuery) SupportDecisionResult
}
