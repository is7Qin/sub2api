package service

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
)

// SupportDecisionBuildOptions contains process-stable inputs captured by the
// background owner at startup.
type SupportDecisionBuildOptions struct {
	Generation uint64
	HotModels  config.SupportDecisionHotModelsConfig
	OpenAIWS   config.GatewayOpenAIWSConfig
}

type supportDecisionBuildScope struct {
	key      supportDecisionScopeKey
	group    *SupportDecisionGroup
	accounts []*Account
	channel  *SupportDecisionChannel
	hot      []string
	exact    []string
	wildcard []string
}

type supportDecisionTemporaryRule struct {
	target  string
	profile supportDecisionProfile
}

type supportDecisionTemporaryScope struct {
	key                 supportDecisionScopeKey
	openAI              bool
	hot                 map[string]supportDecisionProfile
	exact               map[string]supportDecisionTemporaryRule
	wildcard            map[string]supportDecisionTemporaryRule
	channelExact        []string
	channelWildcard     []string
	channelAllowed      supportDecisionProfile
	knownCodexSupport   []byte
	knownBedrockSupport []byte
	fallback            supportDecisionProfile
}

func BuildSupportDecisionTable(snapshot *SupportDecisionConstructionSnapshot, options SupportDecisionBuildOptions) (*SupportDecisionTable, error) {
	if snapshot == nil {
		return nil, fmt.Errorf("support decision snapshot is nil")
	}
	if options.Generation == 0 {
		return nil, fmt.Errorf("support decision generation must be positive")
	}
	scopes, err := collectSupportDecisionScopes(snapshot, options.HotModels)
	if err != nil {
		return nil, err
	}
	temporary := make([]supportDecisionTemporaryScope, 0, len(scopes))
	internSet := make(map[string]struct{})
	for i := range scopes {
		built, err := buildSupportDecisionScope(&scopes[i], options.OpenAIWS)
		if err != nil {
			return nil, err
		}
		for model := range built.hot {
			internSet[model] = struct{}{}
		}
		for model, rule := range built.exact {
			internSet[model] = struct{}{}
			internSet[rule.target] = struct{}{}
		}
		for prefix, rule := range built.wildcard {
			internSet[prefix] = struct{}{}
			internSet[rule.target] = struct{}{}
		}
		for _, model := range built.channelExact {
			internSet[model] = struct{}{}
		}
		for _, prefix := range built.channelWildcard {
			internSet[prefix] = struct{}{}
		}
		temporary = append(temporary, built)
	}

	interned := make([]string, 0, len(internSet))
	for value := range internSet {
		interned = append(interned, value)
	}
	sort.Strings(interned)
	ordinals := make(map[string]uint32, len(interned))
	for i, value := range interned {
		ordinals[value] = uint32(i)
	}

	table := &SupportDecisionTable{
		SchemaVersion: SupportDecisionSchemaVersion,
		Generation:    options.Generation,
		Strings:       interned,
		Scopes:        make([]supportDecisionScopeTable, 0, len(temporary)),
	}
	for i := range temporary {
		scope := materializeSupportDecisionScope(temporary[i], ordinals)
		table.Scopes = append(table.Scopes, scope)
	}
	sort.Slice(table.Scopes, func(i, j int) bool {
		left, right := table.Scopes[i].Key, table.Scopes[j].Key
		if left.Platform != right.Platform {
			return left.Platform < right.Platform
		}
		if left.GroupID != right.GroupID {
			return left.GroupID < right.GroupID
		}
		if left.IncludeGrouped != right.IncludeGrouped {
			return !left.IncludeGrouped && right.IncludeGrouped
		}
		return !left.AllowMixedScheduling && right.AllowMixedScheduling
	})
	if !table.prepareIndexes() {
		return nil, fmt.Errorf("built support decision table is invalid")
	}
	payload, err := encodeSupportDecisionDocumentUnchecked(table)
	if err != nil {
		return nil, err
	}
	if len(payload) > SupportDecisionMaxDocumentSize {
		return nil, fmt.Errorf("support decision document exceeds %d bytes", SupportDecisionMaxDocumentSize)
	}
	table.TypicalSizeExceeded = len(payload) > SupportDecisionTypicalDocumentSize
	return table, nil
}

func collectSupportDecisionScopes(snapshot *SupportDecisionConstructionSnapshot, configured config.SupportDecisionHotModelsConfig) ([]supportDecisionBuildScope, error) {
	accountsByID := make(map[int64]*Account, len(snapshot.Accounts))
	for i := range snapshot.Accounts {
		account := &snapshot.Accounts[i]
		accountsByID[account.ID] = account
	}
	membersByGroup := make(map[int64][]*Account)
	grouped := make(map[int64]struct{})
	for _, membership := range snapshot.Memberships {
		account := accountsByID[membership.AccountID]
		if account == nil {
			continue
		}
		membersByGroup[membership.GroupID] = append(membersByGroup[membership.GroupID], account)
		grouped[membership.AccountID] = struct{}{}
	}
	channelsByGroup := make(map[int64]*SupportDecisionChannel)
	for i := range snapshot.Channels {
		channel := &snapshot.Channels[i]
		if channel.Status != StatusActive {
			continue
		}
		for _, groupID := range channel.GroupIDs {
			channelsByGroup[groupID] = channel
		}
	}

	var scopes []supportDecisionBuildScope
	for i := range snapshot.Groups {
		group := &snapshot.Groups[i]
		accounts := membersByGroup[group.ID]
		scopes = append(scopes, newSupportDecisionBuildScope(
			supportDecisionScopeKey{Platform: group.Platform, GroupID: group.ID}, group, accounts, channelsByGroup[group.ID], configured,
		))
		if group.Platform == PlatformAnthropic || group.Platform == PlatformGemini {
			// Grouped mixed scheduling changes platform eligibility, not membership.
			scopes = append(scopes, newSupportDecisionBuildScope(
				supportDecisionScopeKey{Platform: group.Platform, GroupID: group.ID, AllowMixedScheduling: true}, group, accounts, channelsByGroup[group.ID], configured,
			))
		}
	}
	platformSet := make(map[string]struct{})
	for i := range snapshot.Accounts {
		platformSet[snapshot.Accounts[i].Platform] = struct{}{}
	}
	for _, platform := range []string{PlatformOpenAI, PlatformAnthropic, PlatformGemini, PlatformAntigravity} {
		platformSet[platform] = struct{}{}
	}
	platforms := make([]string, 0, len(platformSet))
	for platform := range platformSet {
		platforms = append(platforms, platform)
	}
	sort.Strings(platforms)
	for _, platform := range platforms {
		ungrouped := make([]*Account, 0)
		all := make([]*Account, 0)
		for i := range snapshot.Accounts {
			account := &snapshot.Accounts[i]
			if !legacyAccountAllowedForPlatform(
				account,
				platform,
				platform == PlatformAnthropic || platform == PlatformGemini,
			) {
				continue
			}
			all = append(all, account)
			if _, isGrouped := grouped[account.ID]; !isGrouped {
				ungrouped = append(ungrouped, account)
			}
		}
		allowMixed := platform == PlatformAnthropic || platform == PlatformGemini
		scopes = append(scopes,
			newSupportDecisionBuildScope(supportDecisionScopeKey{Platform: platform, AllowMixedScheduling: allowMixed}, nil, ungrouped, nil, configured),
			newSupportDecisionBuildScope(supportDecisionScopeKey{Platform: platform, IncludeGrouped: true, AllowMixedScheduling: allowMixed}, nil, all, nil, configured),
		)
	}
	return scopes, nil
}

func newSupportDecisionBuildScope(key supportDecisionScopeKey, group *SupportDecisionGroup, accounts []*Account, channel *SupportDecisionChannel, configured config.SupportDecisionHotModelsConfig) supportDecisionBuildScope {
	var catalogGroup *Group
	if group != nil {
		catalogGroup = &Group{ModelsListConfig: group.ModelsListConfig}
	}
	catalog := ResolveSupportDecisionHotModels(key.Platform, catalogGroup, configured)
	return supportDecisionBuildScope{key: key, group: group, accounts: accounts, channel: channel, hot: catalog.HotModels, exact: catalog.FallbackModels}
}

func buildSupportDecisionScope(scope *supportDecisionBuildScope, wsConfig config.GatewayOpenAIWSConfig) (supportDecisionTemporaryScope, error) {
	openAI := scope.key.Platform == PlatformOpenAI
	coordinateCount := supportDecisionGenericCoordinateCount
	if openAI {
		coordinateCount = supportDecisionOpenAICoordinateCount
	}
	exactSet := make(map[string]struct{}, len(scope.exact))
	wildcardSet := make(map[string]struct{})
	for _, model := range scope.exact {
		if err := validateSupportDecisionModel(model); err != nil {
			return supportDecisionTemporaryScope{}, err
		}
		exactSet[model] = struct{}{}
	}
	for _, account := range scope.accounts {
		for pattern := range account.GetModelMapping() {
			pattern = strings.TrimSpace(pattern)
			if pattern == "" {
				continue
			}
			if strings.HasSuffix(pattern, "*") {
				prefix := strings.TrimSuffix(pattern, "*")
				if err := validateSupportDecisionModel(prefix); err != nil {
					return supportDecisionTemporaryScope{}, err
				}
				wildcardSet[prefix] = struct{}{}
				if account.Platform == PlatformAntigravity {
					wildcardSet["models/"+prefix] = struct{}{}
				}
			} else {
				if err := validateSupportDecisionModel(pattern); err != nil {
					return supportDecisionTemporaryScope{}, err
				}
				exactSet[pattern] = struct{}{}
			}
		}
		if openAI {
			for pattern := range account.GetCompactModelMapping() {
				pattern = strings.TrimSpace(pattern)
				if strings.HasSuffix(pattern, "*") {
					prefix := strings.TrimSuffix(pattern, "*")
					if err := validateSupportDecisionModel(prefix); err != nil {
						return supportDecisionTemporaryScope{}, err
					}
					wildcardSet[prefix] = struct{}{}
				} else if pattern != "" {
					if err := validateSupportDecisionModel(pattern); err != nil {
						return supportDecisionTemporaryScope{}, err
					}
					exactSet[pattern] = struct{}{}
				}
			}
		}
	}
	if len(exactSet) > SupportDecisionExactLimit {
		return supportDecisionTemporaryScope{}, fmt.Errorf("scope %+v has %d exact keys; limit is %d", scope.key, len(exactSet), SupportDecisionExactLimit)
	}
	if len(wildcardSet) > SupportDecisionWildcardLimit {
		return supportDecisionTemporaryScope{}, fmt.Errorf("scope %+v has %d wildcard rules; limit is %d", scope.key, len(wildcardSet), SupportDecisionWildcardLimit)
	}

	built := supportDecisionTemporaryScope{
		key:      scope.key,
		openAI:   openAI,
		hot:      make(map[string]supportDecisionProfile, len(scope.hot)),
		exact:    make(map[string]supportDecisionTemporaryRule, len(exactSet)),
		wildcard: make(map[string]supportDecisionTemporaryRule, len(wildcardSet)),
	}
	for _, model := range scope.hot {
		if err := validateSupportDecisionModel(model); err != nil {
			return supportDecisionTemporaryScope{}, err
		}
		built.hot[model] = evaluateSupportDecisionModel(scope, model, coordinateCount, wsConfig)
	}
	for _, alias := range supportDecisionNormalizedAliases(scope) {
		if err := validateSupportDecisionModel(alias); err != nil {
			return supportDecisionTemporaryScope{}, err
		}
		exactSet[alias] = struct{}{}
	}
	if len(exactSet) > SupportDecisionExactLimit {
		return supportDecisionTemporaryScope{}, fmt.Errorf("scope %+v has %d exact keys; limit is %d", scope.key, len(exactSet), SupportDecisionExactLimit)
	}
	for model := range exactSet {
		target, err := supportDecisionRuleTarget(scope, model, false)
		if err != nil {
			return supportDecisionTemporaryScope{}, err
		}
		built.exact[model] = supportDecisionTemporaryRule{target: target, profile: evaluateSupportDecisionModel(scope, model, coordinateCount, wsConfig)}
	}
	for prefix := range wildcardSet {
		target, err := supportDecisionRuleTarget(scope, prefix, true)
		if err != nil {
			return supportDecisionTemporaryScope{}, err
		}
		// Terminal wildcard mappings have a constant target under the frozen mapper.
		probe := prefix + "__support_decision_probe__"
		built.wildcard[prefix] = supportDecisionTemporaryRule{target: target, profile: evaluateSupportDecisionModel(scope, probe, coordinateCount, wsConfig)}
	}
	built.fallback = evaluateSupportDecisionModel(scope, "__support_decision_unseen_model__", coordinateCount, wsConfig)
	built.channelExact, built.channelWildcard = supportDecisionChannelModelPatterns(scope)
	if len(built.channelExact) > 0 || len(built.channelWildcard) > 0 {
		built.channelAllowed = supportDecisionChannelAllowedProfile(scope, coordinateCount, wsConfig)
	}
	if openAI {
		built.knownCodexSupport = supportDecisionDefaultOAuthCodexSupport(scope, coordinateCount)
	} else if supportDecisionScopeHasBedrock(scope) {
		known := evaluateSupportDecisionModel(scope, "anthropic.claude-sonnet-4-5-20250929-v1:0", coordinateCount, wsConfig)
		built.knownBedrockSupport = known.SupportBits
	}

	hotBytes := profilesSize(built.hot) + len(built.fallback.EligibleBits)
	if hotBytes > SupportDecisionHotDataLimit {
		return supportDecisionTemporaryScope{}, fmt.Errorf("scope %+v hot decisions exceed %d bytes", scope.key, SupportDecisionHotDataLimit)
	}
	fallbackBytes := profileMapsSize(built.exact, built.wildcard, built.fallback)
	if fallbackBytes > SupportDecisionFallbackDataLimit {
		return supportDecisionTemporaryScope{}, fmt.Errorf("scope %+v fallback exceeds %d bytes", scope.key, SupportDecisionFallbackDataLimit)
	}
	return built, nil
}

func supportDecisionDefaultOAuthCodexSupport(scope *supportDecisionBuildScope, coordinateCount int) []byte {
	for _, account := range scope.accounts {
		if !account.IsOpenAIOAuth() || len(account.GetModelMapping()) != 0 {
			continue
		}
		// The family fact represents only OAuth's no-mapping default. Explicit
		// mappings stay in exact/wildcard profiles and must not authorize aliases.
		bits := make([]byte, (coordinateCount+7)/8)
		for coordinate := 0; coordinate < coordinateCount; coordinate++ {
			setBit(bits, coordinate)
		}
		return bits
	}
	return nil
}

func supportDecisionChannelModelPatterns(scope *supportDecisionBuildScope) ([]string, []string) {
	if scope.channel == nil || scope.channel.Status != StatusActive ||
		!scope.channel.RestrictModels ||
		scope.channel.BillingModelSource != BillingModelSourceUpstream {
		return nil, nil
	}
	exact, wildcard := make(map[string]struct{}), make(map[string]struct{})
	for _, pricing := range scope.channel.PricingModels {
		if pricing.Platform != scope.key.Platform {
			continue
		}
		for _, model := range pricing.Models {
			model = strings.ToLower(strings.TrimSpace(model))
			if model == "" {
				continue
			}
			target := exact
			key := model
			if strings.HasSuffix(model, "*") {
				target = wildcard
				key = strings.TrimSuffix(model, "*")
			}
			target[key] = struct{}{}
			if alias := claudePricingRevisionAlias(key); alias != "" {
				target[alias] = struct{}{}
			}
		}
	}
	return sortedStringSet(exact), sortedStringSet(wildcard)
}

func sortedStringSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func supportDecisionChannelAllowedProfile(scope *supportDecisionBuildScope, coordinateCount int, wsConfig config.GatewayOpenAIWSConfig) supportDecisionProfile {
	channel := scope.channel
	scope.channel = nil
	profile := evaluateSupportDecisionModel(scope, "__support_decision_channel_allowed__", coordinateCount, wsConfig)
	scope.channel = channel
	return profile
}

func supportDecisionScopeHasBedrock(scope *supportDecisionBuildScope) bool {
	for _, account := range scope.accounts {
		if account.IsBedrock() && legacyAccountAllowedForPlatform(account, scope.key.Platform, scopeAllowsMixedScheduling(scope)) {
			return true
		}
	}
	return false
}

func supportDecisionNormalizedAliases(scope *supportDecisionBuildScope) []string {
	aliases := make(map[string]struct{})
	for _, account := range scope.accounts {
		if account.IsBedrock() {
			for model := range domain.DefaultBedrockModelMapping {
				aliases[model] = struct{}{}
			}
		}
		mapping := account.GetModelMapping()
		if account.Platform == PlatformAnthropic && account.Type != AccountTypeAPIKey && !account.IsBedrock() {
			for model := range claude.ModelIDOverrides {
				aliases[model] = struct{}{}
			}
			if account.Type == AccountTypeServiceAccount {
				for mapped := range mapping {
					if strings.Contains(mapped, "@") {
						aliases[strings.Replace(mapped, "@", "-", 1)] = struct{}{}
					}
				}
			}
		}
		if account.Platform == PlatformAntigravity {
			for model := range mapping {
				aliases["models/"+model] = struct{}{}
			}
		}
	}
	result := make([]string, 0, len(aliases))
	for alias := range aliases {
		result = append(result, alias)
	}
	sort.Strings(result)
	return result
}

func supportDecisionRuleTarget(scope *supportDecisionBuildScope, key string, wildcard bool) (string, error) {
	probe := key
	mappingKey := key
	if wildcard {
		probe += "__support_decision_probe__"
	}
	if scope.key.Platform == PlatformAntigravity {
		mappingKey = strings.TrimPrefix(mappingKey, "models/")
	}
	targets := make(map[string]struct{})
	for _, account := range scope.accounts {
		mapping := account.GetModelMapping()
		pattern := mappingKey
		if wildcard {
			pattern += "*"
		}
		if target, exists := mapping[pattern]; exists {
			target = strings.TrimSpace(target)
			if err := validateSupportDecisionModel(target); err != nil {
				return "", err
			}
			targets[target] = struct{}{}
		}
		if scope.key.Platform == PlatformOpenAI {
			if target, exists := account.GetCompactModelMapping()[pattern]; exists {
				target = strings.TrimSpace(target)
				if err := validateSupportDecisionModel(target); err != nil {
					return "", err
				}
				targets[target] = struct{}{}
			}
		}
	}
	if len(targets) == 1 {
		for target := range targets {
			return target, nil
		}
	}
	// Multiple accounts may map the same source differently. The aggregate profile
	// already folds those identities, so retain a deterministic non-secret label.
	return probe, nil
}

func evaluateSupportDecisionModel(scope *supportDecisionBuildScope, model string, coordinateCount int, wsConfig config.GatewayOpenAIWSConfig) supportDecisionProfile {
	byteCount := (coordinateCount + 7) / 8
	profile := supportDecisionProfile{SupportBits: make([]byte, byteCount), EligibleBits: make([]byte, byteCount)}
	for coordinate := 0; coordinate < coordinateCount; coordinate++ {
		support, eligible := evaluateSupportDecisionCoordinate(scope, model, coordinate, wsConfig)
		if support {
			setBit(profile.SupportBits, coordinate)
		} else if eligible {
			setBit(profile.EligibleBits, coordinate)
		}
	}
	return profile
}

func evaluateSupportDecisionCoordinate(scope *supportDecisionBuildScope, model string, coordinate int, wsConfig config.GatewayOpenAIWSConfig) (bool, bool) {
	if scope.key.Platform == PlatformOpenAI {
		query := decodeOpenAICoordinate(coordinate)
		otherwiseEligible := false
		for _, account := range scope.accounts {
			if !account.IsOpenAI() {
				continue
			}
			if account.IsModelSupported(model) {
				return true, false
			}
			if supportDecisionAccountEligible(scope, account, model, query, wsConfig) {
				otherwiseEligible = true
			}
		}
		return false, otherwiseEligible
	}
	requiresPrivacy := coordinate&1 != 0
	thinkingEnabled := coordinate&2 != 0
	otherwiseEligible := false
	for _, account := range scope.accounts {
		if !legacyAccountAllowedForPlatform(account, scope.key.Platform, scopeAllowsMixedScheduling(scope)) {
			continue
		}
		if supportDecisionGenericModelSupported(account, model, thinkingEnabled) {
			return true, false
		}
		query := SupportDecisionQuery{RequiresPrivacy: requiresPrivacy, ThinkingEnabled: thinkingEnabled}
		if supportDecisionAccountEligible(scope, account, model, query, wsConfig) {
			otherwiseEligible = true
		}
	}
	return false, otherwiseEligible
}

func supportDecisionAccountEligible(scope *supportDecisionBuildScope, account *Account, model string, query SupportDecisionQuery, wsConfig config.GatewayOpenAIWSConfig) bool {
	if legacyPrivacyRequirementBlocks(account, query.RequiresPrivacy) {
		return false
	}
	if supportDecisionUpstreamRestricted(scope.channel, scope.key.Platform, account, model, query.RequireCompact) {
		return false
	}
	if scope.key.Platform != PlatformOpenAI {
		return true
	}
	if !account.SupportsOpenAIEndpointCapability(query.EndpointCapability) || !account.SupportsOpenAIImageCapability(query.ImageCapability) {
		return false
	}
	if query.RequireCompact && openAICompactSupportTier(account) == 0 {
		return false
	}
	return supportDecisionTransportCompatible(account, query.Transport, wsConfig)
}

func supportDecisionGenericModelSupported(account *Account, model string, thinkingEnabled bool) bool {
	if account.Platform == PlatformAntigravity {
		mapped := mapAntigravityModel(account, model)
		if mapped == "" {
			return false
		}
		finalModel := applyThinkingModelSuffix(mapped, thinkingEnabled)
		return finalModel == mapped || account.IsModelSupported(finalModel)
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
	return account.IsModelSupported(model)
}

func scopeAllowsMixedScheduling(scope *supportDecisionBuildScope) bool {
	return scope.key.AllowMixedScheduling
}

func supportDecisionUpstreamRestricted(channel *SupportDecisionChannel, platform string, account *Account, requestedModel string, requireCompact bool) bool {
	if channel == nil || channel.Status != StatusActive || !channel.RestrictModels || channel.BillingModelSource != BillingModelSourceUpstream {
		return false
	}
	upstream := resolveAccountUpstreamModel(account, requestedModel)
	if platform == PlatformOpenAI {
		upstream = resolveOpenAIAccountUpstreamModelForRequest(account, requestedModel, requireCompact)
	}
	if upstream == "" {
		return false
	}
	return !supportDecisionChannelAllowsModel(channel, platform, upstream)
}

func supportDecisionChannelAllowsModel(channel *SupportDecisionChannel, platform, model string) bool {
	model = strings.ToLower(model)
	alias := claudePricingRevisionAlias(model)
	for _, pricing := range channel.PricingModels {
		if pricing.Platform != platform {
			continue
		}
		for _, configured := range pricing.Models {
			configured = strings.ToLower(configured)
			if configured == model || configured == alias {
				return true
			}
			if strings.HasSuffix(configured, "*") {
				prefix := strings.TrimSuffix(configured, "*")
				if strings.HasPrefix(model, prefix) || (alias != "" && strings.HasPrefix(alias, prefix)) {
					return true
				}
			}
		}
	}
	return false
}

func supportDecisionTransportCompatible(account *Account, required OpenAIUpstreamTransport, wsConfig config.GatewayOpenAIWSConfig) bool {
	if required == OpenAIUpstreamTransportAny || required == OpenAIUpstreamTransportHTTPSSE {
		return true
	}
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS = wsConfig
	return NewOpenAIWSProtocolResolver(cfg).Resolve(account).Transport == required
}

func decodeOpenAICoordinate(coordinate int) SupportDecisionQuery {
	transport := coordinate % 4
	coordinate /= 4
	compact := coordinate % 2
	coordinate /= 2
	image := coordinate % 3
	coordinate /= 3
	endpoint := coordinate % 4
	privacy := coordinate / 4
	query := SupportDecisionQuery{RequiresPrivacy: privacy != 0, RequireCompact: compact != 0}
	query.EndpointCapability = []OpenAIEndpointCapability{"", OpenAIEndpointCapabilityChatCompletions, OpenAIEndpointCapabilityEmbeddings, OpenAIEndpointCapabilityOAuthCompactBodySignal}[endpoint]
	query.ImageCapability = []OpenAIImagesCapability{"", OpenAIImagesCapabilityBasic, OpenAIImagesCapabilityNative}[image]
	query.Transport = []OpenAIUpstreamTransport{OpenAIUpstreamTransportAny, OpenAIUpstreamTransportHTTPSSE, OpenAIUpstreamTransportResponsesWebsocket, OpenAIUpstreamTransportResponsesWebsocketV2}[transport]
	return query
}

func materializeSupportDecisionScope(scope supportDecisionTemporaryScope, ordinals map[string]uint32) supportDecisionScopeTable {
	result := supportDecisionScopeTable{Key: scope.key, OpenAI: scope.openAI, ChannelAllowed: scope.channelAllowed, KnownCodexSupport: scope.knownCodexSupport, KnownBedrockSupport: scope.knownBedrockSupport, Default: scope.fallback, Exact: make(map[string]supportDecisionExactValue, len(scope.exact))}
	for _, model := range scope.channelExact {
		result.ChannelExact = append(result.ChannelExact, ordinals[model])
	}
	for _, prefix := range scope.channelWildcard {
		result.ChannelWildcard = append(result.ChannelWildcard, ordinals[prefix])
	}
	hotModels := sortedProfileKeys(scope.hot)
	for _, model := range hotModels {
		result.Hot = append(result.Hot, supportDecisionHotModel{StringID: ordinals[model], Profile: scope.hot[model]})
	}
	for _, model := range sortedRuleKeys(scope.exact) {
		rule := scope.exact[model]
		value := supportDecisionExactValue{TargetID: ordinals[rule.target], Profile: rule.profile}
		result.Exact[model] = value
		result.ExactWire = append(result.ExactWire, supportDecisionExactEntry{StringID: ordinals[model], TargetID: value.TargetID, Profile: rule.profile})
	}
	prefixes := sortedRuleKeys(scope.wildcard)
	sort.Slice(prefixes, func(i, j int) bool {
		if len(prefixes[i]) != len(prefixes[j]) {
			return len(prefixes[i]) > len(prefixes[j])
		}
		return prefixes[i] < prefixes[j]
	})
	for _, prefix := range prefixes {
		rule := scope.wildcard[prefix]
		result.Wildcard = append(result.Wildcard, supportDecisionWildcard{PrefixID: ordinals[prefix], TargetID: ordinals[rule.target], Profile: rule.profile})
	}
	return result
}

func sortedProfileKeys(values map[string]supportDecisionProfile) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedRuleKeys(values map[string]supportDecisionTemporaryRule) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func validateSupportDecisionModel(model string) error {
	if model == "" {
		return fmt.Errorf("support decision model is empty")
	}
	if len(model) > SupportDecisionMaxModelBytes {
		return fmt.Errorf("support decision model exceeds %d bytes", SupportDecisionMaxModelBytes)
	}
	return nil
}

func profilesSize(values map[string]supportDecisionProfile) int {
	total := 0
	for _, profile := range values {
		total += len(profile.SupportBits) + len(profile.EligibleBits)
	}
	return total
}

func profileMapsSize(exact, wildcard map[string]supportDecisionTemporaryRule, fallback supportDecisionProfile) int {
	total := len(fallback.SupportBits) + len(fallback.EligibleBits)
	for key, rule := range exact {
		total += len(key) + len(rule.target) + len(rule.profile.SupportBits) + len(rule.profile.EligibleBits) + 8
	}
	for key, rule := range wildcard {
		total += len(key) + len(rule.target) + len(rule.profile.SupportBits) + len(rule.profile.EligibleBits) + 8
	}
	return total
}
