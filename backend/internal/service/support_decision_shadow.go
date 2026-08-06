package service

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/domain"
)

var ErrSupportDecisionShadowMismatch = errors.New("support decision shadow mismatch")

type supportDecisionShadowOperationCounterKey struct{}

func supportDecisionCountShadowOperation(ctx context.Context) {
	if counter, ok := ctx.Value(supportDecisionShadowOperationCounterKey{}).(*uint64); ok {
		(*counter)++
	}
}

// VerifySupportDecisionShadow compares every finite published branch against an
// independently derived legacy oracle. Source accounts are frozen and indexed
// once per scope; no builder-produced profile is used as an expected result.
func VerifySupportDecisionShadow(ctx context.Context, snapshot *SupportDecisionConstructionSnapshot, options SupportDecisionBuildOptions, table *SupportDecisionTable) (uint64, error) {
	if ctx == nil {
		return 0, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if snapshot == nil || table == nil || !table.verified || table.Generation != options.Generation {
		return 0, ErrSupportDecisionShadowMismatch
	}
	scopes, err := collectSupportDecisionScopesContext(ctx, snapshot, options.HotModels)
	if err != nil {
		return 0, err
	}
	oracles := make(map[supportDecisionScopeKey]*supportDecisionLegacyScopeOracle, len(scopes))
	for i := range scopes {
		oracle, err := newSupportDecisionLegacyScopeOracle(ctx, &scopes[i], options.OpenAIWS)
		if err != nil {
			return 0, err
		}
		if _, exists := oracles[scopes[i].key]; exists {
			return 0, ErrSupportDecisionShadowMismatch
		}
		oracles[scopes[i].key] = oracle
	}
	if len(oracles) != len(table.runtimeScopes) {
		return 0, ErrSupportDecisionShadowMismatch
	}
	var checks uint64
	for i := range table.runtimeScopes {
		if err := ctx.Err(); err != nil {
			return checks, err
		}
		runtimeScope := &table.runtimeScopes[i]
		oracle := oracles[runtimeScope.Key]
		if oracle == nil || oracle.openAI != runtimeScope.OpenAI {
			return checks, ErrSupportDecisionShadowMismatch
		}
		models, ok := supportDecisionShadowModels(table.runtimeStrings, runtimeScope)
		if !ok {
			return checks, ErrSupportDecisionShadowMismatch
		}
		coordinateCount := supportDecisionGenericCoordinateCount
		if runtimeScope.OpenAI {
			coordinateCount = supportDecisionOpenAICoordinateCount
		}
		for _, witness := range models {
			expected, err := oracle.profile(ctx, witness.model, coordinateCount)
			if err != nil {
				return checks, err
			}
			for coordinate := 0; coordinate < coordinateCount; coordinate++ {
				if err := ctx.Err(); err != nil {
					return checks, err
				}
				query := supportDecisionShadowQuery(runtimeScope.Key, witness.model, coordinate, runtimeScope.OpenAI)
				checks++
				if table.Lookup(query) != profileResult(expected, coordinate) {
					return checks, ErrSupportDecisionShadowMismatch
				}
			}
		}
	}
	return checks, nil
}

func supportDecisionShadowQuery(key supportDecisionScopeKey, model string, coordinate int, openAI bool) SupportDecisionQuery {
	query := SupportDecisionQuery{Scope: SupportDecisionScope(key), RequestedModel: model}
	if openAI {
		decoded := decodeOpenAICoordinate(coordinate)
		query.RequiresPrivacy, query.EndpointCapability, query.ImageCapability = decoded.RequiresPrivacy, decoded.EndpointCapability, decoded.ImageCapability
		query.RequireCompact, query.Transport = decoded.RequireCompact, decoded.Transport
	} else {
		query.RequiresPrivacy, query.ThinkingEnabled = coordinate&1 != 0, coordinate&2 != 0
	}
	return query
}

func supportDecisionShadowAggregateProfile(scope *supportDecisionTemporaryScope, witness supportDecisionShadowWitness) (supportDecisionProfile, bool) {
	switch witness.branch {
	case supportDecisionShadowHot:
		profile, ok := scope.hot[witness.rule]
		return profile, ok
	case supportDecisionShadowExact:
		rule, ok := scope.exact[witness.rule]
		return rule.profile, ok
	case supportDecisionShadowWildcard:
		rule, ok := scope.wildcard[witness.rule]
		return rule.profile, ok
	case supportDecisionShadowCatchAll:
		if scope.catchAll == nil {
			return supportDecisionProfile{}, false
		}
		return scope.catchAll.profile, true
	case supportDecisionShadowChannel:
		return scope.channelAllowed, true
	case supportDecisionShadowCodex:
		return supportDecisionProfile{SupportBits: scope.knownCodexSupport, EligibleBits: make([]byte, len(scope.knownCodexSupport))}, len(scope.knownCodexSupport) > 0
	case supportDecisionShadowBedrock:
		return supportDecisionProfile{SupportBits: scope.knownBedrockSupport, EligibleBits: make([]byte, len(scope.knownBedrockSupport))}, len(scope.knownBedrockSupport) > 0
	case supportDecisionShadowDefault:
		return scope.fallback, true
	default:
		return supportDecisionProfile{}, false
	}
}

type supportDecisionShadowBranch uint8

const (
	supportDecisionShadowHot supportDecisionShadowBranch = iota
	supportDecisionShadowExact
	supportDecisionShadowWildcard
	supportDecisionShadowCatchAll
	supportDecisionShadowChannel
	supportDecisionShadowCodex
	supportDecisionShadowBedrock
	supportDecisionShadowDefault
)

type supportDecisionShadowWitness struct {
	model, rule string
	branch      supportDecisionShadowBranch
}

func supportDecisionShadowModels(stringsTable []string, scope *supportDecisionScopeTable) ([]supportDecisionShadowWitness, bool) {
	models := make([]supportDecisionShadowWitness, 0, len(scope.Hot)+len(scope.ExactWire)+len(scope.Wildcard)+len(scope.ChannelExact)*3+len(scope.ChannelWildcard)*3+64)
	seen := make(map[string]struct{}, cap(models))
	add := func(model, rule string, branch supportDecisionShadowBranch) {
		if model == "" || len(model) > SupportDecisionMaxModelBytes {
			return
		}
		if _, ok := seen[model]; !ok {
			seen[model] = struct{}{}
			models = append(models, supportDecisionShadowWitness{model: model, rule: rule, branch: branch})
		}
	}
	for _, hot := range scope.Hot {
		if int(hot.StringID) >= len(stringsTable) {
			return nil, false
		}
		model := stringsTable[hot.StringID]
		add(model, model, supportDecisionShadowHot)
	}
	for _, exact := range scope.ExactWire {
		if int(exact.StringID) >= len(stringsTable) {
			return nil, false
		}
		model := stringsTable[exact.StringID]
		add(model, model, supportDecisionShadowExact)
	}
	for _, wildcard := range scope.Wildcard {
		if int(wildcard.PrefixID) >= len(stringsTable) {
			return nil, false
		}
		prefix := stringsTable[wildcard.PrefixID]
		witness, reachable, valid := supportDecisionShadowPrefixWitness(stringsTable, scope, wildcard.PrefixID)
		if !valid {
			return nil, false
		}
		if reachable {
			add(witness, prefix, supportDecisionShadowWildcard)
		}
	}
	for _, id := range scope.ChannelExact {
		if int(id) >= len(stringsTable) {
			return nil, false
		}
		model := stringsTable[id]
		for _, alias := range supportDecisionChannelAliases(model) {
			if branch, _ := supportDecisionShadowBranchForModel(stringsTable, scope, alias); branch == supportDecisionShadowChannel {
				add(alias, "", supportDecisionShadowChannel)
			}
		}
	}
	for _, id := range scope.ChannelWildcard {
		if int(id) >= len(stringsTable) {
			return nil, false
		}
		prefix := stringsTable[id]
		for _, alias := range supportDecisionChannelAliases(prefix) {
			candidate := alias
			if branch, _ := supportDecisionShadowBranchForModel(stringsTable, scope, candidate); branch != supportDecisionShadowChannel && len(alias) < SupportDecisionMaxModelBytes {
				candidate = alias + "\x00"
			}
			if branch, _ := supportDecisionShadowBranchForModel(stringsTable, scope, candidate); branch == supportDecisionShadowChannel {
				add(candidate, "", supportDecisionShadowChannel)
			}
		}
	}
	if scope.ChannelCatchAll {
		if fallback, reachable, valid := supportDecisionFallbackWitness(stringsTable, scope, supportDecisionShadowChannel); !valid {
			return nil, false
		} else if reachable {
			add(fallback, "", supportDecisionShadowChannel)
		}
	}
	if len(scope.KnownCodexSupport) > 0 {
		aliases := make([]string, 0, len(codexModelMap))
		for alias := range codexModelMap {
			aliases = append(aliases, alias)
		}
		sort.Strings(aliases)
		for _, alias := range aliases {
			add(alias, "", supportDecisionShadowCodex)
			add("openai/"+alias, "", supportDecisionShadowCodex)
		}
		for _, base := range supportDecisionCodexPrefixAliases() {
			for _, suffix := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "2026-01-02"} {
				add(base+"-"+suffix, "", supportDecisionShadowCodex)
				add("openai/"+base+"-"+suffix, "", supportDecisionShadowCodex)
			}
		}
	}
	if len(scope.KnownBedrockSupport) > 0 {
		aliases := make([]string, 0, len(domain.DefaultBedrockModelMapping))
		for alias := range domain.DefaultBedrockModelMapping {
			aliases = append(aliases, alias)
		}
		sort.Strings(aliases)
		for _, alias := range aliases {
			add(alias, "", supportDecisionShadowBedrock)
			target := domain.DefaultBedrockModelMapping[alias]
			direct := supportDecisionBedrockDirectModel(target)
			add(direct, "", supportDecisionShadowBedrock)
			for _, region := range []string{"us.", "eu.", "apac."} {
				add(region+direct, "", supportDecisionShadowBedrock)
			}
		}
	}
	fallbackBranch := supportDecisionShadowDefault
	if scope.CatchAll != nil {
		fallbackBranch = supportDecisionShadowCatchAll
	}
	if fallback, reachable, valid := supportDecisionFallbackWitness(stringsTable, scope, fallbackBranch); !valid {
		return nil, false
	} else if reachable {
		add(fallback, "", fallbackBranch)
	}
	return models, true
}

func supportDecisionChannelAliases(model string) []string {
	return []string{model, strings.ToUpper(model), supportDecisionShadowDecimalAlias(model), strings.ToUpper(supportDecisionShadowDecimalAlias(model))}
}

func supportDecisionCodexPrefixAliases() []string {
	aliases := make([]string, 0, len(codexVersionModelPrefixes))
	for _, item := range codexVersionModelPrefixes {
		aliases = append(aliases, item.prefix)
	}
	sort.Strings(aliases)
	return aliases
}

func supportDecisionBedrockDirectModel(model string) string {
	for _, region := range []string{"us.", "eu.", "apac."} {
		if strings.HasPrefix(model, region) {
			return strings.TrimPrefix(model, region)
		}
	}
	return model
}

func supportDecisionShadowDecimalAlias(model string) string {
	separator := claudePricingRevisionAliasFoldASCII(model)
	if separator < 0 {
		return model
	}
	out := []byte(model)
	if out[separator] == '.' {
		out[separator] = '-'
	} else {
		out[separator] = '.'
	}
	return string(out)
}

type supportDecisionPrefixTrie struct {
	terminal bool
	children map[byte]*supportDecisionPrefixTrie
}

func (n *supportDecisionPrefixTrie) insert(value string) {
	for i := 0; i < len(value); i++ {
		if n.children == nil {
			n.children = make(map[byte]*supportDecisionPrefixTrie)
		}
		child := n.children[value[i]]
		if child == nil {
			child = &supportDecisionPrefixTrie{}
			n.children[value[i]] = child
		}
		n = child
	}
	n.terminal = true
}

// The trie search is exhaustive over the byte-string complement, deterministic,
// and bounded by the model length and finite exact/wildcard table limits.
func supportDecisionShadowComplement(node *supportDecisionPrefixTrie, candidate string, minLength int, pointBlocked func(string) bool) (string, bool) {
	if node != nil && node.terminal {
		return "", false
	}
	if len(candidate) >= minLength && !pointBlocked(candidate) {
		return candidate, true
	}
	if len(candidate) >= SupportDecisionMaxModelBytes {
		return "", false
	}
	for value := 0; value <= 255; value++ {
		var child *supportDecisionPrefixTrie
		if node != nil {
			child = node.children[byte(value)]
		}
		if witness, ok := supportDecisionShadowComplement(child, candidate+string([]byte{byte(value)}), minLength, pointBlocked); ok {
			return witness, true
		}
	}
	return "", false
}

func supportDecisionShadowPointBlocked(scope *supportDecisionScopeTable, model string) bool {
	_, exists := scope.Exact[model]
	return exists
}

func supportDecisionShadowPointBlockedWithStrings(stringsTable []string, scope *supportDecisionScopeTable, model string) bool {
	for _, hot := range scope.Hot {
		if int(hot.StringID) < len(stringsTable) && stringsTable[hot.StringID] == model {
			return true
		}
	}
	return supportDecisionShadowPointBlocked(scope, model)
}

func supportDecisionShadowPrefixWitness(stringsTable []string, scope *supportDecisionScopeTable, target uint32) (string, bool, bool) {
	if int(target) >= len(stringsTable) {
		return "", false, false
	}
	prefix := stringsTable[target]
	root := &supportDecisionPrefixTrie{}
	for _, rule := range scope.Wildcard {
		if int(rule.PrefixID) >= len(stringsTable) {
			return "", false, false
		}
		other := stringsTable[rule.PrefixID]
		if rule.PrefixID != target && strings.HasPrefix(other, prefix) {
			root.insert(strings.TrimPrefix(other, prefix))
		}
	}
	witness, reachable := supportDecisionShadowComplement(root, prefix, len(prefix), func(model string) bool {
		return supportDecisionShadowPointBlockedWithStrings(stringsTable, scope, model)
	})
	if !reachable {
		return "", false, true
	}
	branch, rule := supportDecisionShadowBranchForModel(stringsTable, scope, witness)
	return witness, branch == supportDecisionShadowWildcard && rule == prefix, true
}

func supportDecisionFallbackWitness(stringsTable []string, scope *supportDecisionScopeTable, wanted supportDecisionShadowBranch) (string, bool, bool) {
	root := &supportDecisionPrefixTrie{}
	for _, rule := range scope.Wildcard {
		if int(rule.PrefixID) >= len(stringsTable) {
			return "", false, false
		}
		root.insert(stringsTable[rule.PrefixID])
	}
	// Channel catch-all reaches every wildcard complement. If a non-channel
	// fallback is requested under it, that branch is unreachable by construction.
	if scope.ChannelCatchAll && wanted != supportDecisionShadowChannel {
		return "", false, true
	}
	if wanted == supportDecisionShadowChannel {
		for _, id := range scope.ChannelExact {
			if int(id) >= len(stringsTable) {
				return "", false, false
			}
			for _, candidate := range supportDecisionChannelAliases(stringsTable[id]) {
				if branch, _ := supportDecisionShadowBranchForModel(stringsTable, scope, candidate); branch == wanted {
					return candidate, true, true
				}
			}
		}
		for _, id := range scope.ChannelWildcard {
			if int(id) >= len(stringsTable) {
				return "", false, false
			}
			for _, alias := range supportDecisionChannelAliases(stringsTable[id]) {
				candidate := alias
				if branch, _ := supportDecisionShadowBranchForModel(stringsTable, scope, candidate); branch != wanted && len(alias) < SupportDecisionMaxModelBytes {
					candidate = alias + "\x00"
				}
				if branch, _ := supportDecisionShadowBranchForModel(stringsTable, scope, candidate); branch == wanted {
					return candidate, true, true
				}
			}
		}
	}
	pointBlocked := func(model string) bool {
		return supportDecisionShadowPointBlockedWithStrings(stringsTable, scope, model)
	}
	candidate := ""
	for {
		witness, ok := supportDecisionShadowComplement(root, candidate, 1, pointBlocked)
		if !ok {
			return "", false, true
		}
		branch, _ := supportDecisionShadowBranchForModel(stringsTable, scope, witness)
		if branch == wanted {
			return witness, true, true
		}
		// A finite exact/channel alias intercepted this complement point. A NUL
		// extension deterministically escapes it without re-entering a wildcard.
		candidate = witness + "\x00"
		if len(candidate) > SupportDecisionMaxModelBytes {
			return "", false, true
		}
	}
}

func supportDecisionShadowBranchForModel(stringsTable []string, scope *supportDecisionScopeTable, model string) (supportDecisionShadowBranch, string) {
	for _, hot := range scope.Hot {
		if int(hot.StringID) < len(stringsTable) && stringsTable[hot.StringID] == model {
			return supportDecisionShadowHot, model
		}
	}
	if _, exists := scope.Exact[model]; exists {
		return supportDecisionShadowExact, model
	}
	if len(scope.KnownCodexSupport) > 0 {
		if _, known := normalizeKnownCodexModel(model); known {
			return supportDecisionShadowCodex, ""
		}
	}
	if len(scope.KnownBedrockSupport) > 0 {
		if _, _, known := normalizeBedrockModelID(model); known {
			return supportDecisionShadowBedrock, ""
		}
	}
	for _, wildcard := range scope.Wildcard {
		if int(wildcard.PrefixID) < len(stringsTable) {
			prefix := stringsTable[wildcard.PrefixID]
			if strings.HasPrefix(model, prefix) {
				return supportDecisionShadowWildcard, prefix
			}
		}
	}
	if scope.CatchAll != nil {
		return supportDecisionShadowCatchAll, ""
	}
	if supportDecisionChannelPatternMatches(stringsTable, scope, model) {
		return supportDecisionShadowChannel, ""
	}
	return supportDecisionShadowDefault, ""
}
