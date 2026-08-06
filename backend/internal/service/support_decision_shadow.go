package service

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/domain"
)

var ErrSupportDecisionShadowMismatch = errors.New("support decision shadow mismatch")

// VerifySupportDecisionShadow exhaustively checks all finite coordinates for
// every model rule represented in each built scope. Fallback behavior receives
// one deterministic model absent from every represented exact/prefix family.
func VerifySupportDecisionShadow(ctx context.Context, snapshot *SupportDecisionConstructionSnapshot, options SupportDecisionBuildOptions, table *SupportDecisionTable) (uint64, error) {
	if ctx == nil {
		return 0, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if snapshot == nil || table == nil || !table.verified {
		return 0, ErrSupportDecisionShadowMismatch
	}
	scopes, err := collectSupportDecisionScopesContext(ctx, snapshot, options.HotModels)
	if err != nil {
		return 0, err
	}
	byKey := make(map[supportDecisionScopeKey]*supportDecisionBuildScope, len(scopes))
	for i := range scopes {
		byKey[scopes[i].key] = &scopes[i]
	}
	var checks uint64
	for i := range table.runtimeScopes {
		if err := ctx.Err(); err != nil {
			return checks, err
		}
		built := &table.runtimeScopes[i]
		source := byKey[built.Key]
		if source == nil {
			return checks, ErrSupportDecisionShadowMismatch
		}
		models := supportDecisionShadowModels(table.runtimeStrings, built)
		accounts := supportDecisionShadowAccounts(source.accounts)
		coordinateCount := supportDecisionGenericCoordinateCount
		if built.OpenAI {
			coordinateCount = supportDecisionOpenAICoordinateCount
		}
		for _, model := range models {
			for coordinate := 0; coordinate < coordinateCount; coordinate++ {
				if err := ctx.Err(); err != nil {
					return checks, err
				}
				query := SupportDecisionQuery{Scope: SupportDecisionScope(built.Key), RequestedModel: model}
				if built.OpenAI {
					decoded := decodeOpenAICoordinate(coordinate)
					query.RequiresPrivacy, query.EndpointCapability, query.ImageCapability = decoded.RequiresPrivacy, decoded.EndpointCapability, decoded.ImageCapability
					query.RequireCompact, query.Transport = decoded.RequireCompact, decoded.Transport
				} else {
					query.RequiresPrivacy, query.ThinkingEnabled = coordinate&1 != 0, coordinate&2 != 0
				}
				expected, err := supportDecisionLegacyShadowResult(ctx, source, accounts, query, options)
				if err != nil {
					return checks, err
				}
				checks++
				if table.Lookup(query) != expected {
					return checks, ErrSupportDecisionShadowMismatch
				}
			}
		}
	}
	return checks, nil
}

func supportDecisionShadowModels(stringsTable []string, scope *supportDecisionScopeTable) []string {
	models := make([]string, 0, len(scope.Hot)+len(scope.ExactWire)+len(scope.Wildcard)+len(scope.ChannelExact)+len(scope.ChannelWildcard)+2)
	seen := make(map[string]struct{}, cap(models))
	add := func(model string) {
		if model != "" {
			if _, ok := seen[model]; !ok {
				seen[model] = struct{}{}
				models = append(models, model)
			}
		}
	}
	for _, hot := range scope.Hot {
		if int(hot.StringID) < len(stringsTable) {
			add(stringsTable[hot.StringID])
		}
	}
	for _, exact := range scope.ExactWire {
		if int(exact.StringID) < len(stringsTable) {
			add(stringsTable[exact.StringID])
		}
	}
	for _, wildcard := range scope.Wildcard {
		if int(wildcard.PrefixID) < len(stringsTable) {
			add(supportDecisionShadowPrefixProbe(stringsTable, scope.Wildcard, wildcard.PrefixID))
		}
	}
	for _, id := range scope.ChannelExact {
		if int(id) < len(stringsTable) {
			add(stringsTable[id])
		}
	}
	for _, id := range scope.ChannelWildcard {
		if int(id) < len(stringsTable) {
			add(stringsTable[id] + "shadow-probe")
		}
	}
	// Normalized model families are represented as compact bitsets, so check
	// every finite built-in alias rather than selecting a sample.
	if len(scope.KnownCodexSupport) > 0 {
		aliases := make([]string, 0, len(codexModelMap))
		for alias := range codexModelMap {
			aliases = append(aliases, alias)
		}
		sort.Strings(aliases)
		for _, alias := range aliases {
			add(alias)
		}
	}
	if len(scope.KnownBedrockSupport) > 0 {
		aliases := make([]string, 0, len(domain.DefaultBedrockModelMapping))
		for alias := range domain.DefaultBedrockModelMapping {
			aliases = append(aliases, alias)
		}
		sort.Strings(aliases)
		for _, alias := range aliases {
			add(alias)
		}
	}
	// The sentinel is selected outside all represented exact/prefix families.
	for n := 0; ; n++ {
		candidate := "support-decision-shadow-fallback"
		if n > 0 {
			candidate += "-x"
		}
		matched := false
		if _, ok := scope.Exact[candidate]; ok {
			matched = true
		}
		for _, rule := range scope.Wildcard {
			if int(rule.PrefixID) < len(stringsTable) && len(stringsTable[rule.PrefixID]) <= len(candidate) && candidate[:len(stringsTable[rule.PrefixID])] == stringsTable[rule.PrefixID] {
				matched = true
				break
			}
		}
		if !matched {
			add(candidate)
			break
		}
	}
	return models
}

func supportDecisionShadowPrefixProbe(stringsTable []string, rules []supportDecisionWildcard, target uint32) string {
	prefix := stringsTable[target]
	// At most 256 wildcard prefixes are allowed. Trying two distinct suffix
	// families beyond that bound either finds this rule's reachable region or
	// proves it is entirely shadowed by a more-specific represented rule.
	for n := 1; n <= SupportDecisionWildcardLimit+1; n++ {
		for _, separator := range []string{"x", "-"} {
			candidate := prefix + strings.Repeat(separator, n)
			winner := target
			for _, rule := range rules {
				if int(rule.PrefixID) >= len(stringsTable) {
					continue
				}
				other := stringsTable[rule.PrefixID]
				if len(other) <= len(candidate) && candidate[:len(other)] == other {
					winner = rule.PrefixID
					break
				}
			}
			if winner == target {
				return candidate
			}
		}
	}
	return ""
}

func supportDecisionLegacyShadowResult(ctx context.Context, scope *supportDecisionBuildScope, accounts []Account, query SupportDecisionQuery, options SupportDecisionBuildOptions) (SupportDecisionResult, error) {
	if scope.key.Platform == PlatformOpenAI {
		miss, err := legacyPureOpenAIModelSupportMissContext(ctx, legacyOpenAIModelSupportMissInput{
			Accounts: accounts, RequestedModel: query.RequestedModel, RequirePrivacy: query.RequiresPrivacy,
			EndpointCapability: query.EndpointCapability, ImageCapability: query.ImageCapability, RequireCompact: query.RequireCompact, Transport: query.Transport,
			UpstreamRestricted: func(account *Account, model string, compact bool) bool {
				return supportDecisionUpstreamRestricted(scope.channel, scope.key.Platform, account, model, compact)
			},
			TransportCompatible: func(account *Account, transport OpenAIUpstreamTransport) bool {
				return supportDecisionTransportCompatible(account, transport, options.OpenAIWS)
			},
		})
		if err != nil {
			return SupportDecisionUnknown, err
		}
		if miss {
			return SupportDecisionPureMiss, nil
		}
		return SupportDecisionNotPureMiss, nil
	}
	miss, err := legacyPureModelSupportMissContext(ctx, legacyModelSupportMissInput{
		Accounts: accounts, RequestedModel: query.RequestedModel, Platform: scope.key.Platform,
		AllowMixedScheduling: scopeAllowsMixedScheduling(scope), RequirePrivacy: query.RequiresPrivacy, ThinkingEnabled: query.ThinkingEnabled,
		ModelSupported: supportDecisionGenericModelSupported,
		UpstreamRestricted: func(account *Account, model string) bool {
			return supportDecisionUpstreamRestricted(scope.channel, scope.key.Platform, account, model, false)
		},
	})
	if err != nil {
		return SupportDecisionUnknown, err
	}
	if miss {
		return SupportDecisionPureMiss, nil
	}
	return SupportDecisionNotPureMiss, nil
}

func supportDecisionShadowAccounts(accounts []*Account) []Account {
	out := make([]Account, len(accounts))
	for i, account := range accounts {
		if account != nil {
			out[i] = *account
		}
	}
	return out
}
