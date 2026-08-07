package service

import (
	"context"
	"sort"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
)

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

type supportDecisionLegacyWildcard struct {
	prefix string
	target string
}

type supportDecisionLegacyAccountFact struct {
	account          Account
	modelMapping     map[string]string
	modelExact       map[string]string
	modelWildcards   []supportDecisionLegacyWildcard
	compactMapping   map[string]string
	compactExact     map[string]string
	compactWildcards []supportDecisionLegacyWildcard
	privacyBlocked   bool
	endpointSupport  uint8
	imageSupport     uint8
	compactSupported bool
	transport        OpenAIUpstreamTransport
	eligible         []byte
}

type supportDecisionLegacyCompiledProfile struct {
	supported         bool
	eligibleFixed     []byte
	eligibleRequested []byte
}

type supportDecisionLegacyMappingEvent struct {
	account int
	target  string
	compact bool
}

type supportDecisionLegacyMappingState struct {
	event  supportDecisionLegacyMappingEvent
	active bool
}

type supportDecisionLegacyTrie struct {
	wildcards []supportDecisionLegacyMappingEvent
	exacts    []supportDecisionLegacyMappingEvent
	children  map[byte]*supportDecisionLegacyTrie
	wildcard  supportDecisionLegacyCompiledProfile
	exact     *supportDecisionLegacyCompiledProfile
}

type supportDecisionLegacyScopeOracle struct {
	key      supportDecisionScopeKey
	openAI   bool
	accounts []supportDecisionLegacyAccountFact
	channel  *SupportDecisionChannel

	openAISupportAll        bool
	openAISupportCodex      bool
	openAIChannelRestricted bool
	openAIEligible          []byte
	openAIRoot              *supportDecisionLegacyTrie
}

func newSupportDecisionLegacyScopeOracle(ctx context.Context, scope *supportDecisionBuildScope, wsConfig config.GatewayOpenAIWSConfig) (*supportDecisionLegacyScopeOracle, error) {
	accounts := make([]supportDecisionLegacyAccountFact, 0, len(scope.accounts))
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS = wsConfig
	resolver := NewOpenAIWSProtocolResolver(cfg)
	for _, source := range scope.accounts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		supportDecisionCountShadowOperation(ctx)
		if source == nil {
			continue
		}
		account := *source
		modelMapping := cloneSupportDecisionStringMap(source.GetModelMapping())
		compactMapping := cloneSupportDecisionStringMap(source.GetCompactModelMapping())
		modelExact, modelWildcards := supportDecisionLegacyMappingIndex(modelMapping)
		compactExact, compactWildcards := supportDecisionLegacyMappingIndex(compactMapping)
		fact := supportDecisionLegacyAccountFact{
			account: account, modelMapping: modelMapping, modelExact: modelExact, modelWildcards: modelWildcards,
			compactMapping: compactMapping, compactExact: compactExact, compactWildcards: compactWildcards,
			privacyBlocked:   legacyPrivacyRequirementBlocks(source, true),
			compactSupported: openAICompactSupportTier(source) != 0,
			transport:        resolver.Resolve(source).Transport,
		}
		fact.endpointSupport = supportDecisionLegacyEndpointBits(source)
		fact.imageSupport = supportDecisionLegacyImageBits(source)
		accounts = append(accounts, fact)
	}
	oracle := &supportDecisionLegacyScopeOracle{key: scope.key, openAI: scope.key.Platform == PlatformOpenAI, accounts: accounts, channel: scope.channel}
	if oracle.openAI {
		if err := oracle.buildOpenAIIndex(ctx); err != nil {
			return nil, err
		}
	}
	return oracle, nil
}

func cloneSupportDecisionStringMap(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func supportDecisionLegacyMappingIndex(mapping map[string]string) (map[string]string, []supportDecisionLegacyWildcard) {
	exact := make(map[string]string, len(mapping))
	wildcards := make([]supportDecisionLegacyWildcard, 0)
	for pattern, target := range mapping {
		if strings.HasSuffix(pattern, "*") {
			wildcards = append(wildcards, supportDecisionLegacyWildcard{prefix: strings.TrimSuffix(pattern, "*"), target: target})
		} else {
			exact[pattern] = target
		}
	}
	sort.Slice(wildcards, func(i, j int) bool { return len(wildcards[i].prefix) > len(wildcards[j].prefix) })
	return exact, wildcards
}

func supportDecisionLegacyResolve(exact map[string]string, wildcards []supportDecisionLegacyWildcard, model string) (string, bool) {
	if target, ok := exact[model]; ok {
		return target, true
	}
	for i := range wildcards {
		if strings.HasPrefix(model, wildcards[i].prefix) {
			return wildcards[i].target, true
		}
	}
	return "", false
}

func supportDecisionLegacyEndpointBits(account *Account) uint8 {
	var bits uint8 = 1
	for i, capability := range []OpenAIEndpointCapability{OpenAIEndpointCapabilityChatCompletions, OpenAIEndpointCapabilityEmbeddings, OpenAIEndpointCapabilityOAuthCompactBodySignal} {
		if account.SupportsOpenAIEndpointCapability(capability) {
			bits |= 1 << uint(i+1)
		}
	}
	return bits
}

func supportDecisionLegacyImageBits(account *Account) uint8 {
	var bits uint8 = 1
	for i, capability := range []OpenAIImagesCapability{OpenAIImagesCapabilityBasic, OpenAIImagesCapabilityNative} {
		if account.SupportsOpenAIImageCapability(capability) {
			bits |= 1 << uint(i+1)
		}
	}
	return bits
}

func (o *supportDecisionLegacyScopeOracle) buildOpenAIIndex(ctx context.Context) error {
	byteCount := (supportDecisionOpenAICoordinateCount + 7) / 8
	o.openAIEligible = make([]byte, byteCount)
	o.openAIChannelRestricted = o.channel != nil && o.channel.Status == StatusActive && o.channel.RestrictModels && o.channel.BillingModelSource == BillingModelSourceUpstream
	o.openAIRoot = &supportDecisionLegacyTrie{}
	for i := range o.accounts {
		if err := ctx.Err(); err != nil {
			return err
		}
		fact := &o.accounts[i]
		if !fact.account.IsOpenAI() {
			continue
		}
		fact.eligible = make([]byte, byteCount)
		for coordinate := 0; coordinate < supportDecisionOpenAICoordinateCount; coordinate++ {
			supportDecisionCountShadowOperation(ctx)
			if supportDecisionLegacyOpenAIEligibleWithoutModel(fact, decodeOpenAICoordinate(coordinate)) {
				setBit(fact.eligible, coordinate)
				setBit(o.openAIEligible, coordinate)
			}
		}
		switch {
		case fact.account.IsOpenAIAPIKeyPassthroughEnabled(), len(fact.modelMapping) == 0 && !fact.account.IsOpenAIOAuth():
			o.openAISupportAll = true
		case len(fact.modelMapping) == 0:
			o.openAISupportCodex = true
		default:
			for model, target := range fact.modelExact {
				o.openAIRoot.insert(model, supportDecisionLegacyMappingEvent{account: i, target: target})
			}
			for _, wildcard := range fact.modelWildcards {
				o.openAIRoot.insertWildcard(wildcard.prefix, supportDecisionLegacyMappingEvent{account: i, target: wildcard.target})
			}
		}
	}
	return o.openAIRoot.compile(ctx, o)
}

func supportDecisionLegacyOpenAIEligibleWithoutModel(fact *supportDecisionLegacyAccountFact, query SupportDecisionQuery) bool {
	if query.RequiresPrivacy && fact.privacyBlocked {
		return false
	}
	if !supportDecisionLegacyEndpointAllowed(fact.endpointSupport, query.EndpointCapability) || !supportDecisionLegacyImageAllowed(fact.imageSupport, query.ImageCapability) {
		return false
	}
	if query.RequireCompact && !fact.compactSupported || query.Transport != OpenAIUpstreamTransportAny && query.Transport != OpenAIUpstreamTransportHTTPSSE && fact.transport != query.Transport {
		return false
	}
	return true
}

func (n *supportDecisionLegacyTrie) child(value string) *supportDecisionLegacyTrie {
	for i := 0; i < len(value); i++ {
		if n.children == nil {
			n.children = make(map[byte]*supportDecisionLegacyTrie)
		}
		next := n.children[value[i]]
		if next == nil {
			next = &supportDecisionLegacyTrie{}
			n.children[value[i]] = next
		}
		n = next
	}
	return n
}

func (n *supportDecisionLegacyTrie) insert(model string, event supportDecisionLegacyMappingEvent) {
	node := n.child(model)
	node.exacts = append(node.exacts, event)
}

func (n *supportDecisionLegacyTrie) insertWildcard(prefix string, event supportDecisionLegacyMappingEvent) {
	node := n.child(prefix)
	node.wildcards = append(node.wildcards, event)
}

func cloneSupportDecisionBits(bits []byte) []byte {
	if len(bits) == 0 {
		return nil
	}
	return append([]byte(nil), bits...)
}

func supportDecisionLegacyOrBits(dst, src []byte) {
	for i := range dst {
		dst[i] |= src[i]
	}
}

func supportDecisionLegacyClearBits(dst []byte) {
	for i := range dst {
		dst[i] = 0
	}
}

func (n *supportDecisionLegacyTrie) compile(ctx context.Context, oracle *supportDecisionLegacyScopeOracle) error {
	states := make([]supportDecisionLegacyMappingState, len(oracle.accounts))
	var walk func(*supportDecisionLegacyTrie) error
	walk = func(node *supportDecisionLegacyTrie) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		changed := make([]supportDecisionLegacyMappingState, 0, len(node.wildcards))
		for _, event := range node.wildcards {
			changed = append(changed, states[event.account])
			states[event.account] = supportDecisionLegacyMappingState{event: event, active: true}
		}
		node.wildcard = oracle.compileOpenAIStates(states, nil)
		if len(node.exacts) > 0 {
			node.exact = new(supportDecisionLegacyCompiledProfile)
			*node.exact = oracle.compileOpenAIStates(states, node.exacts)
		}
		for _, child := range node.children {
			if err := walk(child); err != nil {
				return err
			}
		}
		for i := len(node.wildcards) - 1; i >= 0; i-- {
			states[node.wildcards[i].account] = changed[i]
		}
		return nil
	}
	return walk(n)
}

func (o *supportDecisionLegacyScopeOracle) compileOpenAIStates(states []supportDecisionLegacyMappingState, exact []supportDecisionLegacyMappingEvent) supportDecisionLegacyCompiledProfile {
	byteCount := (supportDecisionOpenAICoordinateCount + 7) / 8
	profile := supportDecisionLegacyCompiledProfile{eligibleFixed: make([]byte, byteCount), eligibleRequested: make([]byte, byteCount)}
	for i := range states {
		if states[i].active {
			o.applyOpenAIEvent(&profile, states[i].event)
		}
	}
	for _, event := range exact {
		o.applyOpenAIEvent(&profile, event)
	}
	return profile
}

func (o *supportDecisionLegacyScopeOracle) applyOpenAIEvent(profile *supportDecisionLegacyCompiledProfile, event supportDecisionLegacyMappingEvent) {
	fact := &o.accounts[event.account]
	if event.compact {
		return
	}
	if fact.account.IsOpenAIOAuth() {
		if _, known := normalizeKnownCodexModel(strings.TrimSpace(event.target)); !known {
			return
		}
	}
	profile.supported = true
	if o.channel == nil || o.channel.Status != StatusActive || !o.channel.RestrictModels || o.channel.BillingModelSource != BillingModelSourceUpstream {
		return
	}
	if supportDecisionChannelAllowsModel(o.channel, o.key.Platform, event.target) {
		supportDecisionLegacyOrBits(profile.eligibleFixed, fact.eligible)
		return
	}
	// Compact routing can turn the ordinary target into a channel-allowed model.
	// It depends only on the ordinary target, so precompute the finite coordinate mask.
	compact := event.target
	if mapped, matched := supportDecisionLegacyResolve(fact.compactExact, fact.compactWildcards, compact); matched {
		compact = mapped
	}
	if compact != "" && supportDecisionChannelAllowsModel(o.channel, o.key.Platform, compact) {
		for coordinate := 0; coordinate < supportDecisionOpenAICoordinateCount; coordinate++ {
			if decodeOpenAICoordinate(coordinate).RequireCompact && fact.eligible[coordinate>>3]&(1<<uint(coordinate&7)) != 0 {
				setBit(profile.eligibleFixed, coordinate)
			}
		}
	}
}

func (o *supportDecisionLegacyScopeOracle) openAIProfile(model string) supportDecisionLegacyCompiledProfile {
	if o.openAISupportAll {
		return supportDecisionLegacyCompiledProfile{supported: true}
	}
	node := o.openAIRoot
	profile := node.wildcard
	profile.eligibleFixed = cloneSupportDecisionBits(o.openAIEligible)
	for i := 0; i < len(model); i++ {
		next := node.children[model[i]]
		if next == nil {
			break
		}
		node = next
		profile = node.wildcard
		if i == len(model)-1 && node.exact != nil {
			profile = *node.exact
		}
	}
	if o.openAISupportCodex {
		if _, known := normalizeKnownCodexModel(model); known {
			profile.supported = true
		}
	}
	return profile
}

func (o *supportDecisionLegacyScopeOracle) profile(ctx context.Context, model string, coordinateCount int) (supportDecisionProfile, error) {
	profile := supportDecisionProfile{
		SupportBits:  make([]byte, (coordinateCount+7)/8),
		EligibleBits: make([]byte, (coordinateCount+7)/8),
	}
	if o.openAI {
		if err := ctx.Err(); err != nil {
			return supportDecisionProfile{}, err
		}
		compiled := o.openAIProfile(model)
		if compiled.supported {
			for coordinate := 0; coordinate < coordinateCount; coordinate++ {
				setBit(profile.SupportBits, coordinate)
			}
			return profile, nil
		}
		copy(profile.EligibleBits, compiled.eligibleFixed)
		return profile, nil
	}
	for i := range o.accounts {
		if err := ctx.Err(); err != nil {
			return supportDecisionProfile{}, err
		}
		fact := &o.accounts[i]
		if legacyAccountAllowedForPlatform(&fact.account, o.key.Platform, o.key.AllowMixedScheduling) {
			for coordinate := 0; coordinate < coordinateCount; coordinate++ {
				query := supportDecisionShadowQuery(o.key, model, coordinate, false)
				if supportDecisionLegacyGenericModelSupported(fact, model, query.ThinkingEnabled) {
					setBit(profile.SupportBits, coordinate)
				}
			}
		}
	}
	for coordinate := 0; coordinate < coordinateCount; coordinate++ {
		query := supportDecisionShadowQuery(o.key, model, coordinate, o.openAI)
		for i := range o.accounts {
			fact := &o.accounts[i]
			if o.eligible(fact, query) {
				setBit(profile.EligibleBits, coordinate)
				break
			}
		}
	}
	return profile, nil
}

func (o *supportDecisionLegacyScopeOracle) eligible(fact *supportDecisionLegacyAccountFact, query SupportDecisionQuery) bool {
	account := &fact.account
	if o.openAI {
		if !account.IsOpenAI() || supportDecisionLegacyOpenAIModelSupported(fact, query.RequestedModel) {
			return false
		}
		if query.RequiresPrivacy && fact.privacyBlocked || supportDecisionLegacyUpstreamRestricted(o.channel, o.key.Platform, fact, query.RequestedModel, query.RequireCompact) {
			return false
		}
		if !supportDecisionLegacyEndpointAllowed(fact.endpointSupport, query.EndpointCapability) || !supportDecisionLegacyImageAllowed(fact.imageSupport, query.ImageCapability) {
			return false
		}
		if query.RequireCompact && !fact.compactSupported || query.Transport != OpenAIUpstreamTransportAny && query.Transport != OpenAIUpstreamTransportHTTPSSE && fact.transport != query.Transport {
			return false
		}
		return true
	}
	if !legacyAccountAllowedForPlatform(account, o.key.Platform, o.key.AllowMixedScheduling) || supportDecisionLegacyGenericModelSupported(fact, query.RequestedModel, query.ThinkingEnabled) {
		return false
	}
	return !(query.RequiresPrivacy && fact.privacyBlocked || supportDecisionLegacyUpstreamRestricted(o.channel, o.key.Platform, fact, query.RequestedModel, false))
}

func (o *supportDecisionLegacyScopeOracle) lookup(ctx context.Context, query SupportDecisionQuery) (SupportDecisionResult, error) {
	otherwiseEligible := false
	for i := range o.accounts {
		if err := ctx.Err(); err != nil {
			return SupportDecisionUnknown, err
		}
		fact := &o.accounts[i]
		account := &fact.account
		if o.openAI {
			if !account.IsOpenAI() {
				continue
			}
			if supportDecisionLegacyOpenAIModelSupported(fact, query.RequestedModel) {
				return SupportDecisionNotPureMiss, nil
			}
			if query.RequiresPrivacy && fact.privacyBlocked || supportDecisionLegacyUpstreamRestricted(o.channel, o.key.Platform, fact, query.RequestedModel, query.RequireCompact) {
				continue
			}
			if !supportDecisionLegacyEndpointAllowed(fact.endpointSupport, query.EndpointCapability) || !supportDecisionLegacyImageAllowed(fact.imageSupport, query.ImageCapability) {
				continue
			}
			if query.RequireCompact && !fact.compactSupported || query.Transport != OpenAIUpstreamTransportAny && query.Transport != OpenAIUpstreamTransportHTTPSSE && fact.transport != query.Transport {
				continue
			}
			otherwiseEligible = true
			continue
		}
		if !legacyAccountAllowedForPlatform(account, o.key.Platform, o.key.AllowMixedScheduling) {
			continue
		}
		if supportDecisionLegacyGenericModelSupported(fact, query.RequestedModel, query.ThinkingEnabled) {
			return SupportDecisionNotPureMiss, nil
		}
		if query.RequiresPrivacy && fact.privacyBlocked || supportDecisionLegacyUpstreamRestricted(o.channel, o.key.Platform, fact, query.RequestedModel, false) {
			continue
		}
		otherwiseEligible = true
	}
	return supportDecisionLegacyResult(otherwiseEligible), nil
}

func supportDecisionLegacyOpenAIModelSupported(fact *supportDecisionLegacyAccountFact, model string) bool {
	account := &fact.account
	if account.IsOpenAIAPIKeyPassthroughEnabled() {
		return true
	}
	if len(fact.modelMapping) == 0 {
		if account.IsOpenAIOAuth() {
			_, known := normalizeKnownCodexModel(model)
			return known
		}
		return true
	}
	mapped, matched := supportDecisionLegacyResolve(fact.modelExact, fact.modelWildcards, model)
	if !matched {
		normalized := normalizeRequestedModelForLookup(account.Platform, model)
		if normalized != model {
			mapped, matched = supportDecisionLegacyResolve(fact.modelExact, fact.modelWildcards, normalized)
		}
	}
	if !matched {
		return false
	}
	if account.IsOpenAIOAuth() {
		_, known := normalizeKnownCodexModel(mapped)
		return known
	}
	return true
}

func supportDecisionLegacyGenericModelSupported(fact *supportDecisionLegacyAccountFact, model string, thinking bool) bool {
	account := &fact.account
	if account.Platform == PlatformAntigravity {
		model = strings.TrimPrefix(model, "models/")
		mapped, matched := supportDecisionLegacyResolve(fact.modelExact, fact.modelWildcards, model)
		if !matched {
			return false
		}
		final := applyThinkingModelSuffix(mapped, thinking)
		if final == mapped {
			return true
		}
		_, matched = supportDecisionLegacyResolve(fact.modelExact, fact.modelWildcards, final)
		return matched
	}
	if account.IsBedrock() {
		_, ok := ResolveBedrockModelID(account, model)
		return ok
	}
	if account.Platform == PlatformAnthropic && account.Type != AccountTypeAPIKey {
		if account.Type == AccountTypeServiceAccount {
			model = normalizeVertexAnthropicModelID(claude.NormalizeModelID(model))
		} else {
			model = claude.NormalizeModelID(model)
		}
	}
	if len(fact.modelMapping) == 0 {
		return true
	}
	_, matched := supportDecisionLegacyResolve(fact.modelExact, fact.modelWildcards, model)
	return matched
}

func supportDecisionLegacyEndpointAllowed(bits uint8, capability OpenAIEndpointCapability) bool {
	index := map[OpenAIEndpointCapability]uint{OpenAIEndpointCapabilityChatCompletions: 1, OpenAIEndpointCapabilityEmbeddings: 2, OpenAIEndpointCapabilityOAuthCompactBodySignal: 3}[capability]
	if capability != "" && index == 0 {
		return false
	}
	return bits&(1<<index) != 0
}

func supportDecisionLegacyImageAllowed(bits uint8, capability OpenAIImagesCapability) bool {
	index := map[OpenAIImagesCapability]uint{OpenAIImagesCapabilityBasic: 1, OpenAIImagesCapabilityNative: 2}[capability]
	if capability != "" && index == 0 {
		return true
	}
	return bits&(1<<index) != 0
}

func supportDecisionLegacyUpstreamRestricted(channel *SupportDecisionChannel, platform string, fact *supportDecisionLegacyAccountFact, model string, compact bool) bool {
	if channel == nil || channel.Status != StatusActive || !channel.RestrictModels || channel.BillingModelSource != BillingModelSourceUpstream {
		return false
	}
	upstream := model
	if mapped, matched := supportDecisionLegacyResolve(fact.modelExact, fact.modelWildcards, upstream); matched {
		upstream = mapped
	}
	if platform == PlatformOpenAI && compact {
		if mapped, matched := supportDecisionLegacyResolve(fact.compactExact, fact.compactWildcards, upstream); matched {
			upstream = mapped
		}
	}
	return upstream != "" && !supportDecisionChannelAllowsModel(channel, platform, upstream)
}

func supportDecisionLegacyResult(miss bool) SupportDecisionResult {
	if miss {
		return SupportDecisionPureMiss
	}
	return SupportDecisionNotPureMiss
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
