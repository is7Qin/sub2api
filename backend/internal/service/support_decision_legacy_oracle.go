package service

import "context"

// legacyModelSupportMissInput contains only in-memory facts consumed by the
// generic classifier after its request guards and persistent-scope load.
type legacyModelSupportMissInput struct {
	Accounts             []Account
	RequestedModel       string
	Platform             string
	AllowMixedScheduling bool
	RequirePrivacy       bool
	ThinkingEnabled      bool
	ModelSupported       func(account *Account, requestedModel string, thinkingEnabled bool) bool
	UpstreamRestricted   func(account *Account, requestedModel string) bool
}

func legacyPureModelSupportMiss(input legacyModelSupportMissInput) bool {
	result, _ := legacyPureModelSupportMissContext(context.Background(), input)
	return result
}

func legacyPureModelSupportMissContext(ctx context.Context, input legacyModelSupportMissInput) (bool, error) {
	otherwiseEligible := 0
	for i := range input.Accounts {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		account := &input.Accounts[i]
		if !legacyAccountAllowedForPlatform(account, input.Platform, input.AllowMixedScheduling) {
			continue
		}
		// Model support deliberately precedes all later eligibility predicates.
		if input.ModelSupported(account, input.RequestedModel, input.ThinkingEnabled) {
			return false, nil
		}
		if legacyPrivacyRequirementBlocks(account, input.RequirePrivacy) {
			continue
		}
		if input.UpstreamRestricted != nil && input.UpstreamRestricted(account, input.RequestedModel) {
			continue
		}
		otherwiseEligible++
	}
	return otherwiseEligible > 0, nil
}

func legacyPrivacyRequirementBlocks(account *Account, requiresPrivacy bool) bool {
	if !requiresPrivacy {
		return false
	}
	return shouldBlockAccountForPrivacyRequirement(account, &Group{RequirePrivacySet: true})
}

func legacyAccountAllowedForPlatform(account *Account, platform string, allowMixedScheduling bool) bool {
	if account == nil {
		return false
	}
	if allowMixedScheduling {
		if account.Platform == platform {
			return true
		}
		return account.Platform == PlatformAntigravity && account.IsMixedSchedulingEnabled()
	}
	return account.Platform == platform
}

// legacyOpenAIModelSupportMissInput contains only in-memory facts consumed by
// the OpenAI classifier after its request guards and persistent-scope load.
type legacyOpenAIModelSupportMissInput struct {
	Accounts            []Account
	RequestedModel      string
	RequirePrivacy      bool
	EndpointCapability  OpenAIEndpointCapability
	ImageCapability     OpenAIImagesCapability
	RequireCompact      bool
	Transport           OpenAIUpstreamTransport
	UpstreamRestricted  func(account *Account, requestedModel string, requireCompact bool) bool
	TransportCompatible func(account *Account, requiredTransport OpenAIUpstreamTransport) bool
}

func legacyPureOpenAIModelSupportMiss(input legacyOpenAIModelSupportMissInput) bool {
	result, _ := legacyPureOpenAIModelSupportMissContext(context.Background(), input)
	return result
}

func legacyPureOpenAIModelSupportMissContext(ctx context.Context, input legacyOpenAIModelSupportMissInput) (bool, error) {
	otherwiseEligible := 0
	for i := range input.Accounts {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		account := &input.Accounts[i]
		if !account.IsOpenAI() {
			continue
		}
		// Model support deliberately precedes all later eligibility predicates.
		if account.IsModelSupported(input.RequestedModel) {
			return false, nil
		}
		if legacyPrivacyRequirementBlocks(account, input.RequirePrivacy) {
			continue
		}
		if input.UpstreamRestricted != nil && input.UpstreamRestricted(account, input.RequestedModel, input.RequireCompact) {
			continue
		}
		if !account.SupportsOpenAIEndpointCapability(input.EndpointCapability) {
			continue
		}
		if !account.SupportsOpenAIImageCapability(input.ImageCapability) {
			continue
		}
		if input.RequireCompact && openAICompactSupportTier(account) == 0 {
			continue
		}
		if input.TransportCompatible != nil && !input.TransportCompatible(account, input.Transport) {
			continue
		}
		otherwiseEligible++
	}
	return otherwiseEligible > 0, nil
}
