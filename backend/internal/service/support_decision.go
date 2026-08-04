package service

import "context"

// SupportDecisionSource is the worker-only authoritative input for decision
// table construction. It intentionally remains separate from request-path
// repository protocols.
type SupportDecisionSource interface {
	Load(ctx context.Context) (*SupportDecisionConstructionSnapshot, error)
}

// SupportDecisionConstructionSnapshot may carry persistence identifiers while
// a worker builds the identifier-free published decision document.
type SupportDecisionConstructionSnapshot struct {
	Accounts    []Account
	Memberships []SupportDecisionMembership
	Groups      []SupportDecisionGroup
	Channels    []SupportDecisionChannel
}

type SupportDecisionMembership struct {
	AccountID int64
	GroupID   int64
}

type SupportDecisionGroup struct {
	ID                int64
	Platform          string
	RequirePrivacySet bool
	ModelsListConfig  GroupModelsListConfig
}

type SupportDecisionChannel struct {
	ID                 int64
	Status             string
	GroupIDs           []int64
	ModelMapping       map[string]map[string]string
	RestrictModels     bool
	BillingModelSource string
	PricingModels      []SupportDecisionPricingModels
}

type SupportDecisionPricingModels struct {
	Platform string
	Models   []string
}

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
