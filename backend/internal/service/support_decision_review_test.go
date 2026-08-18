//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/workerruntime"
	"github.com/stretchr/testify/require"
)

func TestSupportDecisionShadowExercisesReachableOverlappingWildcardParent(t *testing.T) {
	snapshot := supportDecisionTestSnapshot([]Account{{Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": map[string]any{
		"parent-*": "parent-target", "parent-x*": "child-x", "parent--*": "child-dash",
	}}}}, PlatformAnthropic, nil)
	options := SupportDecisionBuildOptions{Generation: 1}
	table := buildSupportDecisionTestTable(t, snapshot, options)
	broken := cloneSupportDecisionTableForReview(t, table)
	scope := supportDecisionScopeForReview(t, broken, PlatformAnthropic, 42)
	for i := range scope.Wildcard {
		if broken.runtimeStrings[scope.Wildcard[i].PrefixID] == "parent-" {
			corruptSupportDecisionProfileForReview(&scope.Wildcard[i].Profile)
		}
	}
	_, err := VerifySupportDecisionShadow(context.Background(), snapshot, options, broken)
	require.ErrorIs(t, err, ErrSupportDecisionShadowMismatch)
}

func TestSupportDecisionBuilderWildcardProbeDoesNotReuseExactBoundaryProfile(t *testing.T) {
	prefix := strings.Repeat("p", SupportDecisionMaxModelBytes-1)
	snapshot := supportDecisionTestSnapshot([]Account{{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				prefix:       "gpt-5.4",
				prefix + "*": "private-upstream-model",
			},
		},
	}}, PlatformOpenAI, nil)
	options := SupportDecisionBuildOptions{Generation: 27}
	table := buildSupportDecisionTestTable(t, snapshot, options)
	query := SupportDecisionQuery{
		Scope:          SupportDecisionScope{Platform: PlatformOpenAI, GroupID: 42},
		RequestedModel: prefix + "\x00",
	}
	require.Equal(t, SupportDecisionPureMiss, table.Lookup(query))
	_, err := VerifySupportDecisionShadow(context.Background(), snapshot, options, table)
	require.NoError(t, err)
}

func TestSupportDecisionBuilderKeepsCompactOnlyMappingsOutOfOrdinarySupport(t *testing.T) {
	account := Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"compact_model_mapping": map[string]any{
				"unknown-model": "gpt-5.4",
			},
		},
	}
	snapshot := supportDecisionTestSnapshot([]Account{account}, PlatformOpenAI, nil)
	options := SupportDecisionBuildOptions{Generation: 28}
	table := buildSupportDecisionTestTable(t, snapshot, options)
	oracle := supportDecisionScopeOracleForReview(t, snapshot, options)
	for _, requireCompact := range []bool{false, true} {
		query := SupportDecisionQuery{
			Scope:          SupportDecisionScope{Platform: PlatformOpenAI, GroupID: 42},
			RequestedModel: "unknown-model",
			RequireCompact: requireCompact,
		}
		direct, err := oracle.lookup(context.Background(), query)
		require.NoError(t, err)
		compiled, err := oracle.profile(context.Background(), query.RequestedModel, supportDecisionOpenAICoordinateCount)
		require.NoError(t, err)
		coordinate := 0
		if requireCompact {
			coordinate = 4
		}
		require.Equal(t, SupportDecisionPureMiss, direct)
		require.Equal(t, direct, table.Lookup(query))
		require.Equal(t, direct, profileResult(compiled, coordinate))
	}
	_, err := VerifySupportDecisionShadow(context.Background(), snapshot, options, table)
	require.NoError(t, err)
}

func TestSupportDecisionShadowAcceptsUnsupportedOpenAIModelsAlongMappingPath(t *testing.T) {
	const mappingModel = "gpt-5.4"
	for _, requestedModel := range []string{"gpt-5", "gpt-5.4-preview"} {
		t.Run(requestedModel, func(t *testing.T) {
			snapshot := supportDecisionTestSnapshot([]Account{{
				Platform: PlatformOpenAI,
				Type:     AccountTypeAPIKey,
				Credentials: map[string]any{
					"model_mapping": map[string]any{mappingModel: mappingModel},
				},
			}}, PlatformOpenAI, []string{requestedModel})
			options := SupportDecisionBuildOptions{Generation: 28}
			table := buildSupportDecisionTestTable(t, snapshot, options)

			require.Equal(t, SupportDecisionPureMiss, table.Lookup(SupportDecisionQuery{
				Scope:          SupportDecisionScope{Platform: PlatformOpenAI, GroupID: 42},
				RequestedModel: requestedModel,
			}))
			_, err := VerifySupportDecisionShadow(context.Background(), snapshot, options, table)
			require.NoError(t, err)
		})
	}
}

func TestSupportDecisionShadowAcceptsUnsupportedOpenAIModelWithOverlappingWildcards(t *testing.T) {
	const requestedModel = "gpt-5.4"
	snapshot := supportDecisionTestSnapshot([]Account{{
		ID:       1,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"gpt-*": "upstream"},
		},
	}, {
		ID:       2,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"gpt-5*": "private-upstream-model"},
		},
	}}, PlatformOpenAI, []string{requestedModel})
	options := SupportDecisionBuildOptions{Generation: 29}
	table := buildSupportDecisionTestTable(t, snapshot, options)

	require.Equal(t, SupportDecisionNotPureMiss, table.Lookup(SupportDecisionQuery{
		Scope:          SupportDecisionScope{Platform: PlatformOpenAI, GroupID: 42},
		RequestedModel: requestedModel,
	}))
	_, err := VerifySupportDecisionShadow(context.Background(), snapshot, options, table)
	require.NoError(t, err)
}

func TestSupportDecisionShadowAcceptsUnsupportedOpenAIModelsWithChannelPolicy(t *testing.T) {
	const requestedModel = "gpt-5"
	snapshot := supportDecisionTestSnapshot([]Account{{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"gpt-5.4": "upstream-5.4"},
		},
	}}, PlatformOpenAI, []string{requestedModel})
	snapshot.Channels = []SupportDecisionChannel{{
		Status:             StatusActive,
		GroupIDs:           []int64{42},
		RestrictModels:     true,
		BillingModelSource: BillingModelSourceUpstream,
		PricingModels: []SupportDecisionPricingModels{{
			Platform: PlatformOpenAI,
			Models:   []string{requestedModel},
		}},
	}}
	options := SupportDecisionBuildOptions{Generation: 30}
	table := buildSupportDecisionTestTable(t, snapshot, options)
	scopes, err := collectSupportDecisionScopesContext(context.Background(), snapshot, options.HotModels)
	require.NoError(t, err)
	var scope *supportDecisionBuildScope
	for i := range scopes {
		if scopes[i].key == (supportDecisionScopeKey{Platform: PlatformOpenAI, GroupID: 42}) {
			scope = &scopes[i]
			break
		}
	}
	require.NotNil(t, scope)
	oracle, err := newSupportDecisionLegacyScopeOracle(context.Background(), scope, options.OpenAIWS)
	require.NoError(t, err)

	for _, model := range []string{requestedModel, "\x00"} {
		query := SupportDecisionQuery{
			Scope:          SupportDecisionScope{Platform: PlatformOpenAI, GroupID: 42},
			RequestedModel: model,
		}
		compiled, profileErr := oracle.profile(context.Background(), model, supportDecisionOpenAICoordinateCount)
		require.NoError(t, profileErr)
		direct, lookupErr := oracle.lookup(context.Background(), query)
		require.NoError(t, lookupErr)
		require.Equal(t, table.Lookup(query), direct, "direct oracle model %q", model)
		require.Equal(t, direct, profileResult(compiled, 0), "compiled oracle model %q", model)
	}

	_, err = VerifySupportDecisionShadow(context.Background(), snapshot, options, table)
	require.NoError(t, err)
}

func TestSupportDecisionShadowRejectsUnsupportedOpenAIModelDeniedByChannel(t *testing.T) {
	const requestedModel = "missing"
	account := Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"other": "ordinary-target"},
		},
	}
	snapshot := supportDecisionTestSnapshot([]Account{account}, PlatformOpenAI, []string{requestedModel})
	snapshot.Channels = []SupportDecisionChannel{{
		Status:             StatusActive,
		GroupIDs:           []int64{42},
		RestrictModels:     true,
		BillingModelSource: BillingModelSourceUpstream,
		PricingModels: []SupportDecisionPricingModels{{
			Platform: PlatformOpenAI,
			Models:   []string{"allowed-only"},
		}},
	}}
	options := SupportDecisionBuildOptions{Generation: 31}
	table := buildSupportDecisionTestTable(t, snapshot, options)
	oracle := supportDecisionScopeOracleForReview(t, snapshot, options)
	query := SupportDecisionQuery{
		Scope:          SupportDecisionScope{Platform: PlatformOpenAI, GroupID: 42},
		RequestedModel: requestedModel,
	}
	compiled, err := oracle.profile(context.Background(), requestedModel, supportDecisionOpenAICoordinateCount)
	require.NoError(t, err)
	direct, err := oracle.lookup(context.Background(), query)
	require.NoError(t, err)
	require.Equal(t, SupportDecisionNotPureMiss, table.Lookup(query))
	require.Equal(t, table.Lookup(query), direct)
	require.Equal(t, direct, profileResult(compiled, 0))

	_, err = VerifySupportDecisionShadow(context.Background(), snapshot, options, table)
	require.NoError(t, err)
}

func TestSupportDecisionShadowAcceptsUnsupportedOpenAICompactChannelMapping(t *testing.T) {
	const requestedModel = "missing"
	account := Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"model_mapping":         map[string]any{"other": "ordinary-target"},
			"compact_model_mapping": map[string]any{"*": "compact-target"},
		},
		Extra: map[string]any{"openai_compact_mode": OpenAICompactModeForceOn},
	}
	snapshot := supportDecisionTestSnapshot([]Account{account}, PlatformOpenAI, []string{requestedModel})
	snapshot.Channels = []SupportDecisionChannel{{
		Status:             StatusActive,
		GroupIDs:           []int64{42},
		RestrictModels:     true,
		BillingModelSource: BillingModelSourceUpstream,
		PricingModels: []SupportDecisionPricingModels{{
			Platform: PlatformOpenAI,
			Models:   []string{"compact-target"},
		}},
	}}
	options := SupportDecisionBuildOptions{Generation: 32}
	table := buildSupportDecisionTestTable(t, snapshot, options)
	scope := supportDecisionScopeOracleForReview(t, snapshot, options)

	for _, compact := range []bool{false, true} {
		query := SupportDecisionQuery{
			Scope:          SupportDecisionScope{Platform: PlatformOpenAI, GroupID: 42},
			RequestedModel: requestedModel,
			RequireCompact: compact,
		}
		compiled, err := scope.profile(context.Background(), requestedModel, supportDecisionOpenAICoordinateCount)
		require.NoError(t, err)
		direct, err := scope.lookup(context.Background(), query)
		require.NoError(t, err)
		coordinate := 0
		if compact {
			coordinate = 4
		}
		require.Equal(t, table.Lookup(query), direct)
		require.Equal(t, direct, profileResult(compiled, coordinate), "compact=%v", compact)
	}

	_, err := VerifySupportDecisionShadow(context.Background(), snapshot, options, table)
	require.NoError(t, err)
}

func TestSupportDecisionShadowRejectsUnsupportedOpenAICompactTargetDeniedByChannel(t *testing.T) {
	const requestedModel = "missing"
	account := Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"model_mapping":         map[string]any{"other": "ordinary-target"},
			"compact_model_mapping": map[string]any{"*": "denied-compact-target"},
		},
		Extra: map[string]any{"openai_compact_mode": OpenAICompactModeForceOn},
	}
	snapshot := supportDecisionTestSnapshot([]Account{account}, PlatformOpenAI, []string{requestedModel})
	snapshot.Channels = []SupportDecisionChannel{{
		Status:             StatusActive,
		GroupIDs:           []int64{42},
		RestrictModels:     true,
		BillingModelSource: BillingModelSourceUpstream,
		PricingModels: []SupportDecisionPricingModels{{
			Platform: PlatformOpenAI,
			Models:   []string{requestedModel},
		}},
	}}
	options := SupportDecisionBuildOptions{Generation: 34}
	table := buildSupportDecisionTestTable(t, snapshot, options)
	oracle := supportDecisionScopeOracleForReview(t, snapshot, options)
	query := SupportDecisionQuery{
		Scope:          SupportDecisionScope{Platform: PlatformOpenAI, GroupID: 42},
		RequestedModel: requestedModel,
		RequireCompact: true,
	}
	compiled, err := oracle.profile(context.Background(), requestedModel, supportDecisionOpenAICoordinateCount)
	require.NoError(t, err)
	direct, err := oracle.lookup(context.Background(), query)
	require.NoError(t, err)
	require.Equal(t, SupportDecisionNotPureMiss, table.Lookup(query))
	require.Equal(t, table.Lookup(query), direct)
	require.Equal(t, direct, profileResult(compiled, 4))

	_, err = VerifySupportDecisionShadow(context.Background(), snapshot, options, table)
	require.NoError(t, err)
}

func TestSupportDecisionBuilderPreservesRequestedChannelEligibilityUnderWildcard(t *testing.T) {
	const requestedModel = "gpt-5"
	snapshot := supportDecisionTestSnapshot([]Account{{
		ID:       1,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"gpt-*":  "gpt-5.4",
				"gpt-5*": "compact-target",
			},
		},
	}, {
		ID:       2,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"foo-*": "allowed"},
		},
	}}, PlatformOpenAI, nil)
	snapshot.Channels = []SupportDecisionChannel{{
		Status:             StatusActive,
		GroupIDs:           []int64{42},
		RestrictModels:     true,
		BillingModelSource: BillingModelSourceUpstream,
		PricingModels: []SupportDecisionPricingModels{{
			Platform: PlatformOpenAI,
			Models:   []string{requestedModel},
		}},
	}}
	options := SupportDecisionBuildOptions{Generation: 36}
	table := buildSupportDecisionTestTable(t, snapshot, options)
	query := SupportDecisionQuery{
		Scope:          SupportDecisionScope{Platform: PlatformOpenAI, GroupID: 42},
		RequestedModel: requestedModel,
	}
	require.Equal(t, SupportDecisionPureMiss, table.Lookup(query))
	_, err := VerifySupportDecisionShadow(context.Background(), snapshot, options, table)
	require.NoError(t, err)
}

func TestSupportDecisionBuilderChannelProfilePreservesLaterSupportingAccount(t *testing.T) {
	snapshot := supportDecisionTestSnapshot([]Account{{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"foo-*": "compact-target"},
		},
	}, {
		Platform: PlatformOpenAI,
		Type:     AccountTypeSetupToken,
		Credentials: map[string]any{
			"compact_model_mapping": map[string]any{"known": "gpt-5.2-codex"},
		},
	}}, PlatformOpenAI, nil)
	snapshot.Channels = []SupportDecisionChannel{{
		Status:             StatusActive,
		GroupIDs:           []int64{42},
		RestrictModels:     true,
		BillingModelSource: BillingModelSourceUpstream,
		PricingModels: []SupportDecisionPricingModels{{
			Platform: PlatformOpenAI,
			Models:   []string{"*"},
		}},
	}}
	options := SupportDecisionBuildOptions{Generation: 37}
	table := buildSupportDecisionTestTable(t, snapshot, options)
	query := SupportDecisionQuery{
		Scope:          SupportDecisionScope{Platform: PlatformOpenAI, GroupID: 42},
		RequestedModel: "missing",
	}
	require.Equal(t, SupportDecisionNotPureMiss, table.Lookup(query))
	_, err := VerifySupportDecisionShadow(context.Background(), snapshot, options, table)
	require.NoError(t, err)
}

func TestSupportDecisionLegacyOracleProfilesRemainImmutableAcrossLookups(t *testing.T) {
	const requestedModel = "allowed"
	snapshot := supportDecisionTestSnapshot([]Account{{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"other": "ordinary-target"},
		},
	}}, PlatformOpenAI, []string{requestedModel})
	snapshot.Channels = []SupportDecisionChannel{{
		Status:             StatusActive,
		GroupIDs:           []int64{42},
		RestrictModels:     true,
		BillingModelSource: BillingModelSourceUpstream,
		PricingModels: []SupportDecisionPricingModels{{
			Platform: PlatformOpenAI,
			Models:   []string{requestedModel},
		}},
	}}
	oracle := supportDecisionScopeOracleForReview(t, snapshot, SupportDecisionBuildOptions{Generation: 35})

	first, err := oracle.profile(context.Background(), requestedModel, supportDecisionOpenAICoordinateCount)
	require.NoError(t, err)
	require.Equal(t, SupportDecisionPureMiss, profileResult(first, 0))
	second, err := oracle.profile(context.Background(), "\x00", supportDecisionOpenAICoordinateCount)
	require.NoError(t, err)
	require.Equal(t, SupportDecisionNotPureMiss, profileResult(second, 0))
	third, err := oracle.profile(context.Background(), requestedModel, supportDecisionOpenAICoordinateCount)
	require.NoError(t, err)
	require.Equal(t, SupportDecisionPureMiss, profileResult(third, 0))
}

func supportDecisionScopeOracleForReview(t *testing.T, snapshot *SupportDecisionConstructionSnapshot, options SupportDecisionBuildOptions) *supportDecisionLegacyScopeOracle {
	t.Helper()
	scopes, err := collectSupportDecisionScopesContext(context.Background(), snapshot, options.HotModels)
	require.NoError(t, err)
	for i := range scopes {
		if scopes[i].key == (supportDecisionScopeKey{Platform: PlatformOpenAI, GroupID: 42}) {
			oracle, err := newSupportDecisionLegacyScopeOracle(context.Background(), &scopes[i], options.OpenAIWS)
			require.NoError(t, err)
			return oracle
		}
	}
	require.FailNow(t, "OpenAI group scope not found")
	return nil
}

func TestSupportDecisionShadowDetectsPublishedBranchCorruption(t *testing.T) {
	channel := SupportDecisionChannel{Status: StatusActive, RestrictModels: true, BillingModelSource: BillingModelSourceUpstream, GroupIDs: []int64{42}, PricingModels: []SupportDecisionPricingModels{{Platform: PlatformAnthropic, Models: []string{"DIRECT", "claude-sonnet-4.5*"}}}}
	snapshot := supportDecisionTestSnapshot([]Account{{Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": map[string]any{"direct": "direct", "claude-sonnet-4-5-20250929": "claude-sonnet-4-5-20250929", "wild-*": "wild-target", "*": "catch-target"}}}}, PlatformAnthropic, nil)
	snapshot.Channels = []SupportDecisionChannel{channel}
	options := SupportDecisionBuildOptions{Generation: 2}
	table := buildSupportDecisionTestTable(t, snapshot, options)
	_, err := VerifySupportDecisionShadow(context.Background(), snapshot, options, table)
	require.NoError(t, err)
	tests := []struct {
		name       string
		breakTable func(*SupportDecisionTable)
	}{
		{"exact", func(b *SupportDecisionTable) {
			scope := supportDecisionScopeForReview(t, b, PlatformAnthropic, 42)
			corruptSupportDecisionProfileForReview(&scope.ExactWire[0].Profile)
			model := b.runtimeStrings[scope.ExactWire[0].StringID]
			value := scope.Exact[model]
			value.Profile = scope.ExactWire[0].Profile
			scope.Exact[model] = value
		}},
		{"wildcard", func(b *SupportDecisionTable) {
			corruptSupportDecisionProfileForReview(&supportDecisionScopeForReview(t, b, PlatformAnthropic, 42).Wildcard[0].Profile)
		}},
		{"catch_all", func(b *SupportDecisionTable) {
			corruptSupportDecisionProfileForReview(&supportDecisionScopeForReview(t, b, PlatformAnthropic, 42).CatchAll.Profile)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			broken := cloneSupportDecisionTableForReview(t, table)
			tt.breakTable(broken)
			_, err := VerifySupportDecisionShadow(context.Background(), snapshot, options, broken)
			require.ErrorIs(t, err, ErrSupportDecisionShadowMismatch)
		})
	}
}

func TestSupportDecisionShadowDetectsChannelCaseAndRevisionAliases(t *testing.T) {
	for _, pattern := range []string{"claude-sonnet-4.5", "claude-sonnet-4.5*"} {
		t.Run(pattern, func(t *testing.T) {
			channel := SupportDecisionChannel{Status: StatusActive, RestrictModels: true, BillingModelSource: BillingModelSourceUpstream, GroupIDs: []int64{42}, PricingModels: []SupportDecisionPricingModels{{Platform: PlatformAnthropic, Models: []string{pattern}}}}
			snapshot := supportDecisionTestSnapshot([]Account{{Platform: PlatformAnthropic, Extra: map[string]any{"privacy_mode": PrivacyModeTrainingOff}, Credentials: map[string]any{"model_mapping": map[string]any{"other": "other"}}}}, PlatformAnthropic, nil)
			snapshot.Channels = []SupportDecisionChannel{channel}
			options := SupportDecisionBuildOptions{Generation: 23}
			table := buildSupportDecisionTestTable(t, snapshot, options)
			broken := cloneSupportDecisionTableForReview(t, table)
			profile := &supportDecisionScopeForReview(t, broken, PlatformAnthropic, 42).ChannelAllowed
			profile.EligibleBits[0] ^= 1
			_, err := VerifySupportDecisionShadow(context.Background(), snapshot, options, broken)
			require.ErrorIs(t, err, ErrSupportDecisionShadowMismatch)
		})
	}
}

func TestSupportDecisionShadowDetectsAntigravityAliasesAcrossScopesAndCoordinates(t *testing.T) {
	account := Account{Platform: PlatformAntigravity, Credentials: map[string]any{"model_mapping": map[string]any{"ag-exact": "ag-exact", "ag-wild-*": "ag-target"}}, Extra: map[string]any{"mixed_scheduling": true, "privacy_mode": PrivacyModeTrainingOff}}
	snapshot := supportDecisionTestSnapshot([]Account{account}, PlatformAnthropic, nil)
	options := SupportDecisionBuildOptions{Generation: 24}
	table := buildSupportDecisionTestTable(t, snapshot, options)
	for _, key := range []supportDecisionScopeKey{
		{Platform: PlatformAntigravity, IncludeGrouped: true},
		{Platform: PlatformAnthropic, GroupID: 42, AllowMixedScheduling: true},
		{Platform: PlatformAnthropic, IncludeGrouped: true, AllowMixedScheduling: true},
	} {
		t.Run(fmt.Sprintf("%+v", key), func(t *testing.T) {
			broken := cloneSupportDecisionTableForReview(t, table)
			index, ok := broken.scopeIndex[key]
			require.True(t, ok)
			scope := &broken.runtimeScopes[index]
			var corrupted bool
			for i := range scope.Wildcard {
				if strings.HasPrefix(broken.runtimeStrings[scope.Wildcard[i].PrefixID], "models/ag-wild-") {
					for coordinate := 0; coordinate < supportDecisionGenericCoordinateCount; coordinate++ {
						if profileResult(scope.Wildcard[i].Profile, coordinate) != SupportDecisionPureMiss {
							setBit(scope.Wildcard[i].Profile.EligibleBits, coordinate)
							scope.Wildcard[i].Profile.SupportBits[coordinate>>3] &^= 1 << uint(coordinate&7)
							corrupted = true
							break
						}
					}
					break
				}
			}
			require.True(t, corrupted)
			_, err := VerifySupportDecisionShadow(context.Background(), snapshot, options, broken)
			require.ErrorIs(t, err, ErrSupportDecisionShadowMismatch)
		})
	}
}

func TestSupportDecisionShadowDetectsNormalizedFamilyCorruption(t *testing.T) {
	tests := []struct {
		name     string
		snapshot *SupportDecisionConstructionSnapshot
		platform string
		corrupt  func(*supportDecisionScopeTable)
	}{
		{"codex_direct_namespaced_suffix", supportDecisionTestSnapshot([]Account{{Platform: PlatformOpenAI, Type: AccountTypeOAuth}}, PlatformOpenAI, nil), PlatformOpenAI, func(s *supportDecisionScopeTable) { s.KnownCodexSupport[0] ^= 1 }},
		{"bedrock_default_direct_regional", supportDecisionTestSnapshot([]Account{{Platform: PlatformAnthropic, Type: AccountTypeBedrock, Credentials: map[string]any{"model_mapping": map[string]any{"other": "other"}}}}, PlatformAnthropic, nil), PlatformAnthropic, func(s *supportDecisionScopeTable) {
			for i := range s.KnownBedrockSupport {
				s.KnownBedrockSupport[i] = ^s.KnownBedrockSupport[i]
			}
		}},
		{"antigravity_exact_wildcard_alias", supportDecisionTestSnapshot([]Account{{Platform: PlatformAntigravity, Credentials: map[string]any{"model_mapping": map[string]any{"ag-exact": "ag-exact", "ag-wild-*": "ag-target"}}}}, PlatformAntigravity, nil), PlatformAntigravity, func(s *supportDecisionScopeTable) { corruptSupportDecisionProfileForReview(&s.Wildcard[0].Profile) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			options := SupportDecisionBuildOptions{Generation: 3}
			table := buildSupportDecisionTestTable(t, tt.snapshot, options)
			broken := cloneSupportDecisionTableForReview(t, table)
			scope := supportDecisionScopeForReview(t, broken, tt.platform, 42)
			tt.corrupt(scope)
			_, err := VerifySupportDecisionShadow(context.Background(), tt.snapshot, options, broken)
			require.ErrorIs(t, err, ErrSupportDecisionShadowMismatch)
		})
	}
}

func TestSupportDecisionShadowUsesSuppliedSnapshot(t *testing.T) {
	builtFrom := supportDecisionTestSnapshot([]Account{{Platform: PlatformAnthropic}}, PlatformAnthropic, nil)
	options := SupportDecisionBuildOptions{Generation: 25}
	table := buildSupportDecisionTestTable(t, builtFrom, options)
	changed := supportDecisionTestSnapshot([]Account{{Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": map[string]any{"other": "other"}}}}, PlatformAnthropic, nil)

	_, err := VerifySupportDecisionShadow(context.Background(), changed, options, table)
	require.ErrorIs(t, err, ErrSupportDecisionShadowMismatch)
}

func TestSupportDecisionShadowIndependentFromBuilderProfileSeam(t *testing.T) {
	snapshot := supportDecisionTestSnapshot([]Account{{Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": map[string]any{"requested": "requested"}}}}, PlatformAnthropic, nil)
	options := SupportDecisionBuildOptions{Generation: 26}
	previous := supportDecisionBuilderProfileDefectForTest
	supportDecisionBuilderProfileDefectForTest = func(profile *supportDecisionProfile) {
		profile.SupportBits[0] &^= 1
		profile.EligibleBits[0] |= 1
	}
	t.Cleanup(func() { supportDecisionBuilderProfileDefectForTest = previous })

	table := buildSupportDecisionTestTable(t, snapshot, options)
	_, err := VerifySupportDecisionShadow(context.Background(), snapshot, options, table)
	require.ErrorIs(t, err, ErrSupportDecisionShadowMismatch)
}

func TestSupportDecisionPublisherShadowMismatchSkipsRedis(t *testing.T) {
	snapshot := supportDecisionTestSnapshot([]Account{{Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": map[string]any{"configured-hot-model": "configured-hot-model"}}}}, PlatformAnthropic, nil)
	store := &supportDecisionPublisherStoreStub{}
	publisher := newSupportDecisionPublisherTestSubject(
		&supportDecisionPublisherGenerationStub{generations: []uint64{27}},
		&supportDecisionPublisherSourceStub{snapshots: []*SupportDecisionConstructionSnapshot{snapshot}},
		store,
	)
	publisher.shadow = func(context.Context, *SupportDecisionConstructionSnapshot, SupportDecisionBuildOptions, *SupportDecisionTable) (uint64, error) {
		return 1, ErrSupportDecisionShadowMismatch
	}

	_, err := publisher.Publish(context.Background())
	require.ErrorIs(t, err, ErrSupportDecisionShadowMismatch)
	require.Zero(t, store.putCalls)
	require.Zero(t, store.activates)
	require.Zero(t, store.wakeups)
}

func TestSupportDecisionWildcardWitnessUsesMaxLengthPrefix(t *testing.T) {
	prefix := strings.Repeat("w", SupportDecisionMaxModelBytes)
	stringsTable := []string{prefix}
	scope := &supportDecisionScopeTable{Wildcard: []supportDecisionWildcard{{PrefixID: 0}}}

	witness, reachable, valid := supportDecisionShadowPrefixWitness(stringsTable, scope, 0)
	require.True(t, valid)
	require.True(t, reachable)
	require.Equal(t, prefix, witness)
}

func TestSupportDecisionChannelWildcardWitnessUsesMaxLengthPrefix(t *testing.T) {
	prefix := strings.Repeat("c", SupportDecisionMaxModelBytes)
	scope := &supportDecisionScopeTable{ChannelWildcard: []uint32{0}}
	witnesses, valid := supportDecisionShadowModels([]string{prefix}, scope)
	require.True(t, valid)
	require.Contains(t, witnesses, supportDecisionShadowWitness{model: prefix, branch: supportDecisionShadowChannel})
}

func TestSupportDecisionShadowLegacyOracleWorkIsAggregated(t *testing.T) {
	mapping := make(map[string]any, 16)
	for i := 0; i < 16; i++ {
		mapping[fmt.Sprintf("model-%03d", i)] = "gpt-5.2-codex"
	}
	accounts := make([]Account, 8)
	for i := range accounts {
		accounts[i] = Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"model_mapping": mapping}}
	}
	snapshot := supportDecisionTestSnapshot(accounts, PlatformOpenAI, nil)
	options := SupportDecisionBuildOptions{Generation: 4}
	table := buildSupportDecisionTestTable(t, snapshot, options)
	var operations uint64
	ctx := context.WithValue(context.Background(), supportDecisionShadowOperationCounterKey{}, &operations)
	checks, err := VerifySupportDecisionShadow(ctx, snapshot, options, table)
	require.NoError(t, err)
	require.Positive(t, checks)
	// Construction may inspect each account once and derive each finite coordinate
	// once. Model witnesses must not trigger another account scan.
	maximum := uint64(len(accounts)*(supportDecisionOpenAICoordinateCount+2) + 4096)
	require.LessOrEqual(t, operations, maximum, "oracle work must not multiply accounts by model witnesses")
}

func TestSupportDecisionShadowLegacyOracleCompactChannelWorkIsAggregated(t *testing.T) {
	mapping := make(map[string]any, 16)
	for i := 0; i < 16; i++ {
		mapping[fmt.Sprintf("model-%03d", i)] = "gpt-5.2-codex"
	}
	accounts := make([]Account, 8)
	for i := range accounts {
		accounts[i] = Account{
			Platform: PlatformOpenAI,
			Type:     AccountTypeOAuth,
			Credentials: map[string]any{
				"model_mapping":         mapping,
				"compact_model_mapping": map[string]any{"*": "compact-target"},
			},
			Extra: map[string]any{"openai_compact_mode": OpenAICompactModeForceOn},
		}
	}
	snapshot := supportDecisionTestSnapshot(accounts, PlatformOpenAI, nil)
	snapshot.Channels = []SupportDecisionChannel{{
		Status:             StatusActive,
		GroupIDs:           []int64{42},
		RestrictModels:     true,
		BillingModelSource: BillingModelSourceUpstream,
		PricingModels: []SupportDecisionPricingModels{{
			Platform: PlatformOpenAI,
			Models:   []string{"compact-target"},
		}},
	}}
	options := SupportDecisionBuildOptions{Generation: 33}
	table := buildSupportDecisionTestTable(t, snapshot, options)
	var operations uint64
	ctx := context.WithValue(context.Background(), supportDecisionShadowOperationCounterKey{}, &operations)
	checks, err := VerifySupportDecisionShadow(ctx, snapshot, options, table)
	require.NoError(t, err)
	require.Positive(t, checks)
	maximum := uint64(len(accounts)*(supportDecisionOpenAICoordinateCount+2) + 4096)
	require.LessOrEqual(t, operations, maximum, "compact channel work must not multiply accounts by model witnesses")
}

func TestSupportDecisionLegacyUpstreamRestrictionChainsCompactMapping(t *testing.T) {
	channel := &SupportDecisionChannel{
		Status:             StatusActive,
		RestrictModels:     true,
		BillingModelSource: BillingModelSourceUpstream,
		PricingModels: []SupportDecisionPricingModels{{
			Platform: PlatformOpenAI,
			Models:   []string{"compact-target"},
		}},
	}
	fact := supportDecisionLegacyAccountFact{
		modelMapping:     map[string]string{"requested": "ordinary-target"},
		modelExact:       map[string]string{"requested": "ordinary-target"},
		compactMapping:   map[string]string{"ordinary-target": "compact-target"},
		compactExact:     map[string]string{"ordinary-target": "compact-target"},
		compactSupported: true,
	}

	require.False(t, supportDecisionLegacyUpstreamRestricted(channel, PlatformOpenAI, &fact, "requested", true))
	require.True(t, supportDecisionLegacyUpstreamRestricted(channel, PlatformOpenAI, &fact, "requested", false))
}

func TestSupportDecisionWildcardComplementWitnessIsDeterministicAndExhaustive(t *testing.T) {
	stringsTable := []string{"p", "p\x00", "p\x01", "p\x00\x00"}
	scope := &supportDecisionScopeTable{Wildcard: []supportDecisionWildcard{{PrefixID: 3}, {PrefixID: 1}, {PrefixID: 2}, {PrefixID: 0}}}
	first, reachable, valid := supportDecisionShadowPrefixWitness(stringsTable, scope, 0)
	require.True(t, valid)
	require.True(t, reachable)
	require.Equal(t, "p", first)
	for range 20 {
		got, ok, valid := supportDecisionShadowPrefixWitness(stringsTable, scope, 0)
		require.True(t, valid)
		require.True(t, ok)
		require.Equal(t, first, got)
	}

	fullyCoveredStrings := []string{"p", "p\x00", "p\x01"}
	fullyCovered := &supportDecisionScopeTable{Wildcard: []supportDecisionWildcard{{PrefixID: 1}, {PrefixID: 2}, {PrefixID: 0}}}
	// Model strings permit every byte, but a bounded table cannot contain 256
	// children here. Exercise a truly covered finite domain at the max length.
	fullyCoveredStrings[0] = strings.Repeat("p", SupportDecisionMaxModelBytes-1)
	fullyCoveredStrings[1] = fullyCoveredStrings[0] + "\x00"
	fullyCoveredStrings[2] = fullyCoveredStrings[0] + "\x01"
	_, reachable, valid = supportDecisionShadowPrefixWitness(fullyCoveredStrings, fullyCovered, 0)
	require.True(t, valid)
	require.True(t, reachable, "unrepresented byte values remain a reachable complement")
}

func TestSupportDecisionReplicaFetchCancellationIdentityAndSanitization(t *testing.T) {
	for _, tt := range []struct {
		name  string
		cause error
	}{{"canceled", context.Canceled}, {"deadline", context.DeadlineExceeded}} {
		t.Run(tt.name, func(t *testing.T) {
			h := newSupportDecisionReplicaHarness(t)
			h.store.setActive(2, nil)
			h.store.documentErr = fmt.Errorf("credential=secret: %w", tt.cause)
			err := h.replica.refresh(context.Background())
			require.ErrorIs(t, err, tt.cause)
			require.NotContains(t, err.Error(), "secret")
		})
	}
}

func TestSupportDecisionPublisherRuntimeStatusDoesNotCountActiveAttemptAsFailure(t *testing.T) {
	publisher := newSupportDecisionPublisherTestSubject(
		&supportDecisionPublisherGenerationStub{generations: []uint64{1}},
		&supportDecisionPublisherSourceStub{snapshots: []*SupportDecisionConstructionSnapshot{emptySupportDecisionPublisherSnapshot()}},
		&supportDecisionPublisherStoreStub{},
	)
	publisher.metrics.attempts.Store(1)
	publisher.metrics.active.Store(true)
	worker := newSchedulerSupportPublisherWorker(nil, nil, nil, publisher, time.Second, immediateSchedulerSupportPublisherClock{})

	status := worker.Snapshot().Status.(workerruntime.PeriodicStatus)
	require.Equal(t, uint64(1), status.RunCount)
	require.Zero(t, status.ErrorCount)
	require.True(t, status.StillRunning)
}

func TestSupportDecisionPublisherRuntimeStatusFailureThenSuccess(t *testing.T) {
	now := time.Unix(1000, 0)
	generation := &supportDecisionPublisherGenerationStub{generations: []uint64{1, 2}}
	source := &supportDecisionPublisherSourceStub{errs: []error{errors.New("secret"), nil}, snapshots: []*SupportDecisionConstructionSnapshot{nil, emptySupportDecisionPublisherSnapshot()}}
	publisher := newSupportDecisionPublisherTestSubject(generation, source, &supportDecisionPublisherStoreStub{})
	publisher.now = func() time.Time { return now }
	worker := newSchedulerSupportPublisherWorker(nil, nil, nil, publisher, time.Second, immediateSchedulerSupportPublisherClock{})
	_, _ = publisher.Publish(context.Background())
	first := worker.Snapshot().Status.(workerruntime.PeriodicStatus)
	require.Equal(t, workerruntime.OutcomeError, first.LastOutcome)
	now = now.Add(time.Second)
	_, err := publisher.Publish(context.Background())
	require.NoError(t, err)
	status := worker.Snapshot().Status
	require.NotNil(t, status)
	periodic := status.(workerruntime.PeriodicStatus)
	require.Equal(t, workerruntime.OutcomeSuccess, periodic.LastOutcome)
	require.Equal(t, now, periodic.LastRunAt)
	require.Equal(t, uint64(1), periodic.ErrorCount)
}

func TestSupportDecisionReplicaRuntimeStatusFailureThenSuccessAndSubscriptionFailure(t *testing.T) {
	h := newSupportDecisionReplicaHarness(t)
	h.addDocument(t, 1, []Account{{Platform: PlatformAnthropic}})
	h.store.setActive(1, errors.New("credential=secret"))
	require.Error(t, h.replica.refresh(context.Background()))
	worker := NewSupportDecisionReplicaWorker(h.replica)
	failed := worker.Snapshot().Status.(workerruntime.PeriodicStatus)
	require.Equal(t, workerruntime.OutcomeError, failed.LastOutcome)
	require.Equal(t, uint64(1), failed.ErrorCount)

	h.now = h.now.Add(time.Second)
	h.store.setActive(1, nil)
	require.NoError(t, h.replica.refresh(context.Background()))
	succeeded := worker.Snapshot().Status.(workerruntime.PeriodicStatus)
	require.Equal(t, workerruntime.OutcomeSuccess, succeeded.LastOutcome)
	require.Equal(t, uint64(1), succeeded.ErrorCount)
	require.Equal(t, h.now, succeeded.LastRunAt)

	h2 := newSupportDecisionReplicaHarness(t)
	// Inject a concrete subscription failure through a minimal store wrapper.
	failing := &supportDecisionSubscriptionFailureStore{SupportDecisionPublicationStore: h2.store}
	replica := newSupportDecisionReplica(failing, h2.reader, h2.replica.deps)
	require.Error(t, replica.Start(context.Background()))
	status := NewSupportDecisionReplicaWorker(replica).Snapshot().Status.(workerruntime.PeriodicStatus)
	require.Equal(t, uint64(1), status.ErrorCount)
	require.Equal(t, workerruntime.OutcomeError, status.LastOutcome)
	require.False(t, status.LastRunAt.IsZero())
}

type supportDecisionSubscriptionFailureStore struct {
	SupportDecisionPublicationStore
}

func (s *supportDecisionSubscriptionFailureStore) SubscribeWakeups(context.Context) (SupportDecisionWakeupSubscription, error) {
	return nil, errors.New("credential=secret")
}

func TestSupportDecisionSnapshotsConcurrentCoherent(t *testing.T) {
	generation := &supportDecisionPublisherGenerationStub{generations: make([]uint64, 100)}
	for i := range generation.generations {
		generation.generations[i] = uint64(i + 1)
	}
	source := &supportDecisionPublisherSourceStub{snapshots: make([]*SupportDecisionConstructionSnapshot, 100)}
	for i := range source.snapshots {
		source.snapshots[i] = emptySupportDecisionPublisherSnapshot()
	}
	publisher := newSupportDecisionPublisherTestSubject(generation, source, &supportDecisionPublisherStoreStub{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 100 {
			_, _ = publisher.Publish(context.Background())
		}
	}()
	go func() {
		defer wg.Done()
		for range 1000 {
			s := publisher.Snapshot()
			require.LessOrEqual(t, s.SuccessfulActivations, s.Attempts)
		}
	}()
	wg.Wait()
}

func corruptSupportDecisionProfileForReview(profile *supportDecisionProfile) {
	for i := range profile.SupportBits {
		profile.SupportBits[i] = 0
	}
	for i := range profile.EligibleBits {
		profile.EligibleBits[i] = 0xff
	}
}

func cloneSupportDecisionTableForReview(t *testing.T, table *SupportDecisionTable) *SupportDecisionTable {
	t.Helper()
	payload, err := EncodeSupportDecisionDocument(table)
	require.NoError(t, err)
	cloned, err := DecodeSupportDecisionDocument(payload, table.Generation)
	require.NoError(t, err)
	return cloned
}

func supportDecisionScopeForReview(t *testing.T, table *SupportDecisionTable, platform string, groupID int64) *supportDecisionScopeTable {
	t.Helper()
	index, ok := table.scopeIndex[supportDecisionScopeKey{Platform: platform, GroupID: groupID}]
	require.True(t, ok)
	return &table.runtimeScopes[index]
}

func BenchmarkSupportDecisionShadowRepresentative(b *testing.B) {
	const modelCount = 4096
	snapshot := supportDecisionTestSnapshot([]Account{{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{"openai_passthrough": true}}}, PlatformOpenAI, nil)
	options := SupportDecisionBuildOptions{Generation: 5, OpenAIWS: config.GatewayOpenAIWSConfig{}}
	table := buildSupportDecisionTestTable(b, snapshot, options)
	key := supportDecisionScopeKey{Platform: PlatformOpenAI, GroupID: 42}
	index := table.scopeIndex[key]
	runtimeScope := &table.runtimeScopes[index]
	byteCount := (supportDecisionOpenAICoordinateCount + 7) / 8
	for i := 0; i < modelCount; i++ {
		model := fmt.Sprintf("model-%04d", i)
		table.runtimeStrings = append(table.runtimeStrings, model)
		modelID, targetID := uint32(len(table.runtimeStrings)-1), uint32(0)
		profile := supportDecisionProfile{SupportBits: make([]byte, byteCount), EligibleBits: make([]byte, byteCount)}
		for coordinate := 0; coordinate < supportDecisionOpenAICoordinateCount; coordinate++ {
			setBit(profile.SupportBits, coordinate)
		}
		runtimeScope.ExactWire = append(runtimeScope.ExactWire, supportDecisionExactEntry{StringID: modelID, TargetID: targetID, Profile: profile})
		runtimeScope.Exact[model] = supportDecisionExactValue{TargetID: targetID, Profile: profile}
	}
	var operations uint64
	ctx := context.WithValue(context.Background(), supportDecisionShadowOperationCounterKey{}, &operations)
	checks, err := VerifySupportDecisionShadow(ctx, snapshot, options, table)
	require.NoError(b, err)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = VerifySupportDecisionShadow(context.Background(), snapshot, options, table)
	}
	b.StopTimer()
	b.ReportMetric(float64(checks), "checks/run")
	b.ReportMetric(float64(operations), "operations/run")
}

func BenchmarkSupportDecisionAtomicReaderLookup(b *testing.B) {
	table := buildSupportDecisionTestTable(b, supportDecisionTestSnapshot([]Account{{Platform: PlatformAnthropic}}, PlatformAnthropic, []string{"hot"}))
	reader := NewSupportDecisionAtomicReader(30 * time.Second)
	now := time.Unix(1, 0)
	reader.now = func() time.Time { return now }
	reader.Install(table, now)
	query := SupportDecisionQuery{Scope: SupportDecisionScope{Platform: PlatformAnthropic, GroupID: 42}, RequestedModel: "hot"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = reader.Lookup(query)
	}
}

func BenchmarkSupportDecisionAtomicReaderLookupParallel(b *testing.B) {
	table := buildSupportDecisionTestTable(b, supportDecisionTestSnapshot([]Account{{Platform: PlatformAnthropic}}, PlatformAnthropic, []string{"hot"}))
	reader := NewSupportDecisionAtomicReader(30 * time.Second)
	now := time.Unix(1, 0)
	reader.now = func() time.Time { return now }
	reader.Install(table, now)
	query := SupportDecisionQuery{Scope: SupportDecisionScope{Platform: PlatformAnthropic, GroupID: 42}, RequestedModel: "hot"}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = reader.Lookup(query)
		}
	})
}
