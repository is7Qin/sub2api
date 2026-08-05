//go:build unit

package service

import (
	"fmt"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func supportDecisionTestSnapshot(accounts []Account, platform string, hot []string) *SupportDecisionConstructionSnapshot {
	groupID := int64(42)
	memberships := make([]SupportDecisionMembership, 0, len(accounts))
	for i := range accounts {
		if accounts[i].ID == 0 {
			accounts[i].ID = int64(i + 1)
		}
		memberships = append(memberships, SupportDecisionMembership{AccountID: accounts[i].ID, GroupID: groupID})
	}
	return &SupportDecisionConstructionSnapshot{
		Accounts:    accounts,
		Memberships: memberships,
		Groups: []SupportDecisionGroup{{
			ID:       groupID,
			Platform: platform,
			ModelsListConfig: GroupModelsListConfig{
				Enabled: true,
				Models:  hot,
			},
		}},
	}
}

func buildSupportDecisionTestTable(t testing.TB, snapshot *SupportDecisionConstructionSnapshot, options ...SupportDecisionBuildOptions) *SupportDecisionTable {
	t.Helper()
	option := SupportDecisionBuildOptions{Generation: 7}
	if len(options) > 0 {
		option = options[0]
	}
	table, err := BuildSupportDecisionTable(snapshot, option)
	require.NoError(t, err)
	return table
}

func TestSupportDecisionBuilderMatchesLegacyGenericOracle(t *testing.T) {
	const model = "claude-sonnet-4-5"
	accounts := []Account{
		{Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": map[string]any{"claude-haiku-*": "claude-haiku-3-5"}}},
		{Platform: PlatformAntigravity, Credentials: map[string]any{"model_mapping": map[string]any{model: model}}, Extra: map[string]any{"mixed_scheduling": true}},
	}
	table := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot(accounts, PlatformAnthropic, []string{model}))
	for _, thinking := range []bool{false, true} {
		legacy := legacyPureModelSupportMiss(legacyModelSupportMissInput{
			Accounts:             accounts,
			RequestedModel:       model,
			Platform:             PlatformAnthropic,
			AllowMixedScheduling: true,
			ThinkingEnabled:      thinking,
			ModelSupported: func(account *Account, model string, thinking bool) bool {
				return supportDecisionGenericModelSupported(account, model, thinking)
			},
		})
		got := table.Lookup(SupportDecisionQuery{Scope: SupportDecisionScope{Platform: PlatformAnthropic, GroupID: 42, AllowMixedScheduling: true}, RequestedModel: model, ThinkingEnabled: thinking})
		require.Equal(t, legacy, got == SupportDecisionPureMiss)
	}
}

func TestSupportDecisionBuilderMatchesLegacyGenericOracleAcrossAccountOrder(t *testing.T) {
	const model = "external-model"
	unsupported := Account{ID: 1, Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": map[string]any{"other": "other"}}}
	supporting := Account{ID: 2, Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": map[string]any{model: model}}, Extra: map[string]any{"privacy_mode": "default"}}
	for _, accounts := range [][]Account{{unsupported, supporting}, {supporting, unsupported}} {
		table := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot(accounts, PlatformAnthropic, []string{model}))
		got := table.Lookup(SupportDecisionQuery{Scope: SupportDecisionScope{Platform: PlatformAnthropic, GroupID: 42}, RequestedModel: model, RequiresPrivacy: true})
		require.Equal(t, SupportDecisionNotPureMiss, got)
	}
}

func TestSupportDecisionBuilderMatchesLegacyOpenAIOracle(t *testing.T) {
	const model = "gpt-external"
	account := Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"model_mapping": map[string]any{"gpt-allowed": "upstream"}}}
	wsConfig := config.GatewayOpenAIWSConfig{Enabled: true, APIKeyEnabled: true, ResponsesWebsocketsV2: true}
	table := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot([]Account{account}, PlatformOpenAI, []string{model}), SupportDecisionBuildOptions{Generation: 8, OpenAIWS: wsConfig})

	for _, query := range []SupportDecisionQuery{
		{RequestedModel: model},
		{RequestedModel: model, RequiresPrivacy: true},
		{RequestedModel: model, EndpointCapability: OpenAIEndpointCapabilityEmbeddings},
		{RequestedModel: model, ImageCapability: OpenAIImagesCapabilityBasic},
		{RequestedModel: model, RequireCompact: true},
		{RequestedModel: model, Transport: OpenAIUpstreamTransportResponsesWebsocketV2},
	} {
		legacy := legacyPureOpenAIModelSupportMiss(legacyOpenAIModelSupportMissInput{
			Accounts:           []Account{account},
			RequestedModel:     model,
			RequirePrivacy:     query.RequiresPrivacy,
			EndpointCapability: query.EndpointCapability,
			ImageCapability:    query.ImageCapability,
			RequireCompact:     query.RequireCompact,
			Transport:          query.Transport,
			TransportCompatible: func(account *Account, required OpenAIUpstreamTransport) bool {
				return supportDecisionTransportCompatible(account, required, wsConfig)
			},
		})
		query.Scope = SupportDecisionScope{Platform: PlatformOpenAI, GroupID: 42}
		require.Equal(t, legacy, table.Lookup(query) == SupportDecisionPureMiss)
	}
}

func TestSupportDecisionBuilderMatchesOpenAIMappedTargetFallbackSemantics(t *testing.T) {
	channel := SupportDecisionChannel{
		ID:                 919191919,
		Status:             StatusActive,
		GroupIDs:           []int64{42},
		RestrictModels:     true,
		BillingModelSource: BillingModelSourceUpstream,
		PricingModels: []SupportDecisionPricingModels{{
			Platform: PlatformOpenAI,
			Models:   []string{"gpt-5.4"},
		}},
	}
	accounts := []Account{
		{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"model_mapping": map[string]any{"known-*": "gpt-5.4", "unknown-*": "custom-upstream", "exact-known": "gpt-5.4", "exact-unknown": "custom-upstream"}}},
	}
	snapshot := supportDecisionTestSnapshot(accounts, PlatformOpenAI, []string{"hot"})
	snapshot.Channels = []SupportDecisionChannel{channel}
	table := buildSupportDecisionTestTable(t, snapshot)

	for _, model := range []string{"known-alias", "unknown-alias", "exact-known", "exact-unknown"} {
		legacy := legacyPureOpenAIModelSupportMiss(legacyOpenAIModelSupportMissInput{
			Accounts:       accounts,
			RequestedModel: model,
			UpstreamRestricted: func(account *Account, requestedModel string, requireCompact bool) bool {
				return supportDecisionUpstreamRestricted(&channel, PlatformOpenAI, account, requestedModel, requireCompact)
			},
		})
		got := table.Lookup(SupportDecisionQuery{Scope: SupportDecisionScope{Platform: PlatformOpenAI, GroupID: 42}, RequestedModel: model})
		require.Equalf(t, legacy, got == SupportDecisionPureMiss, "model %q", model)
	}
}

func TestSupportDecisionBuilderPreservesPureMissWhenUnseenProbeWouldMatchMapping(t *testing.T) {
	account := Account{
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"__support_decision_*": "upstream"},
		},
	}
	const requested = "gpt-99"
	table := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot([]Account{account}, PlatformAnthropic, []string{"hot"}))
	legacy := legacyPureModelSupportMiss(legacyModelSupportMissInput{
		Accounts:       []Account{account},
		RequestedModel: requested,
		Platform:       PlatformAnthropic,
		ModelSupported: func(account *Account, model string, thinking bool) bool {
			return supportDecisionGenericModelSupported(account, model, thinking)
		},
	})
	require.True(t, legacy)
	require.Equal(t, SupportDecisionPureMiss, table.Lookup(SupportDecisionQuery{
		Scope:          SupportDecisionScope{Platform: PlatformAnthropic, GroupID: 42},
		RequestedModel: requested,
	}))
}

func TestSupportDecisionBuilderSupportsCatchAllMappingWithExactPrecedence(t *testing.T) {
	account := Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"*": "custom-upstream", "exact": "gpt-5.4"},
		},
	}
	table := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot([]Account{account}, PlatformOpenAI, []string{"hot"}))
	for model, want := range map[string]SupportDecisionResult{
		"exact":     SupportDecisionNotPureMiss,
		"arbitrary": SupportDecisionPureMiss,
	} {
		require.Equalf(t, want, table.Lookup(SupportDecisionQuery{
			Scope:          SupportDecisionScope{Platform: PlatformOpenAI, GroupID: 42},
			RequestedModel: model,
		}), "model %q", model)
	}
}

func TestSupportDecisionBuilderMatchesLegacyUnseenChannelPolicy(t *testing.T) {
	channel := SupportDecisionChannel{
		Status:             StatusActive,
		GroupIDs:           []int64{42},
		RestrictModels:     true,
		BillingModelSource: BillingModelSourceUpstream,
		PricingModels: []SupportDecisionPricingModels{{
			Platform: PlatformAnthropic,
			Models:   []string{"allowed-exact", "allowed-prefix-*"},
		}},
	}
	account := Account{
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"configured": "configured"},
		},
	}
	snapshot := supportDecisionTestSnapshot([]Account{account}, PlatformAnthropic, []string{"hot"})
	snapshot.Channels = []SupportDecisionChannel{channel}
	table := buildSupportDecisionTestTable(t, snapshot)

	for _, model := range []string{"allowed-exact", "allowed-prefix-model", "denied-unseen"} {
		legacy := legacyPureModelSupportMiss(legacyModelSupportMissInput{
			Accounts:       []Account{account},
			RequestedModel: model,
			Platform:       PlatformAnthropic,
			ModelSupported: func(account *Account, requested string, thinking bool) bool {
				return supportDecisionGenericModelSupported(account, requested, thinking)
			},
			UpstreamRestricted: func(account *Account, requested string) bool {
				return supportDecisionUpstreamRestricted(&channel, PlatformAnthropic, account, requested, false)
			},
		})
		got := table.Lookup(SupportDecisionQuery{
			Scope:          SupportDecisionScope{Platform: PlatformAnthropic, GroupID: 42},
			RequestedModel: model,
		})
		require.Equalf(t, legacy, got == SupportDecisionPureMiss, "model %q", model)
	}
}

func TestSupportDecisionBuilderMatchesClaudeRevisionAliasForChannelWildcard(t *testing.T) {
	channel := SupportDecisionChannel{
		Status:             StatusActive,
		GroupIDs:           []int64{42},
		RestrictModels:     true,
		BillingModelSource: BillingModelSourceUpstream,
		PricingModels: []SupportDecisionPricingModels{{
			Platform: PlatformAnthropic,
			Models:   []string{"CLAUDE-OPUS-4-*"},
		}},
	}
	account := Account{
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"configured": "configured"},
		},
	}
	snapshot := supportDecisionTestSnapshot([]Account{account}, PlatformAnthropic, []string{"hot"})
	snapshot.Channels = []SupportDecisionChannel{channel}
	table := buildSupportDecisionTestTable(t, snapshot)
	for _, requested := range []string{"claude-opus-4.8", "CLAUDE-OPUS-4.8"} {
		legacy := legacyPureModelSupportMiss(legacyModelSupportMissInput{
			Accounts:       []Account{account},
			RequestedModel: requested,
			Platform:       PlatformAnthropic,
			ModelSupported: func(account *Account, model string, thinking bool) bool {
				return supportDecisionGenericModelSupported(account, model, thinking)
			},
			UpstreamRestricted: func(account *Account, model string) bool {
				return !supportDecisionChannelAllowsModel(&channel, PlatformAnthropic, model)
			},
		})
		require.Equalf(t, legacy, table.Lookup(SupportDecisionQuery{
			Scope:          SupportDecisionScope{Platform: PlatformAnthropic, GroupID: 42},
			RequestedModel: requested,
		}) == SupportDecisionPureMiss, "model %q", requested)
	}
}

func TestSupportDecisionBuilderMatchesLegacyCaseInsensitiveChannelPolicy(t *testing.T) {
	channel := SupportDecisionChannel{
		Status:             StatusActive,
		GroupIDs:           []int64{42},
		RestrictModels:     true,
		BillingModelSource: BillingModelSourceUpstream,
		PricingModels: []SupportDecisionPricingModels{{
			Platform: PlatformAnthropic,
			Models:   []string{"ALLOWED-EXACT", "Allowed-Prefix-*", "claude-opus-4-8"},
		}},
	}
	account := Account{
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"configured": "configured"},
		},
	}
	snapshot := supportDecisionTestSnapshot([]Account{account}, PlatformAnthropic, []string{"hot"})
	snapshot.Channels = []SupportDecisionChannel{channel}
	table := buildSupportDecisionTestTable(t, snapshot)

	for _, model := range []string{"allowed-exact", "ALLOWED-EXACT", "allowed-prefix-model", "ALLOWED-PREFIX-MODEL", "claude-opus-4.8"} {
		legacy := legacyPureModelSupportMiss(legacyModelSupportMissInput{
			Accounts:       []Account{account},
			RequestedModel: model,
			Platform:       PlatformAnthropic,
			ModelSupported: func(account *Account, requested string, thinking bool) bool {
				return supportDecisionGenericModelSupported(account, requested, thinking)
			},
			UpstreamRestricted: func(account *Account, requested string) bool {
				return !supportDecisionChannelAllowsModel(&channel, PlatformAnthropic, requested)
			},
		})
		got := table.Lookup(SupportDecisionQuery{
			Scope:          SupportDecisionScope{Platform: PlatformAnthropic, GroupID: 42},
			RequestedModel: model,
		})
		require.Equalf(t, legacy, got == SupportDecisionPureMiss, "model %q", model)
	}
}

func TestSupportDecisionBuilderDoesNotGeneralizeExplicitOAuthMappingToCodexFamily(t *testing.T) {
	account := Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"gpt-5-codex": "gpt-5-codex"},
		},
	}
	const requested = "openai/gpt-5-codex"
	table := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot([]Account{account}, PlatformOpenAI, []string{"hot"}))
	legacy := legacyPureOpenAIModelSupportMiss(legacyOpenAIModelSupportMissInput{
		Accounts:       []Account{account},
		RequestedModel: requested,
	})
	require.True(t, legacy)
	require.Equal(t, SupportDecisionPureMiss, table.Lookup(SupportDecisionQuery{
		Scope:          SupportDecisionScope{Platform: PlatformOpenAI, GroupID: 42},
		RequestedModel: requested,
	}))
}

func TestSupportDecisionBuilderNormalizesAntigravityWildcardModelsPrefix(t *testing.T) {
	account := Account{
		Platform: PlatformAntigravity,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"vendor-*": "vendor-target"},
		},
	}
	const requested = "models/vendor-model"
	table := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot([]Account{account}, PlatformAntigravity, []string{"hot"}))
	legacy := legacyPureModelSupportMiss(legacyModelSupportMissInput{
		Accounts:       []Account{account},
		RequestedModel: requested,
		Platform:       PlatformAntigravity,
		ModelSupported: func(account *Account, requested string, thinking bool) bool {
			return supportDecisionGenericModelSupported(account, requested, thinking)
		},
	})
	require.False(t, legacy)
	require.Equal(t, SupportDecisionNotPureMiss, table.Lookup(SupportDecisionQuery{
		Scope:          SupportDecisionScope{Platform: PlatformAntigravity, GroupID: 42},
		RequestedModel: requested,
	}))
}

func TestSupportDecisionBuilderIncludesMixedAntigravityInPlatformScopes(t *testing.T) {
	account := Account{
		ID:       1,
		Platform: PlatformAntigravity,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"configured": "configured"},
		},
		Extra: map[string]any{"mixed_scheduling": true},
	}
	for _, tt := range []struct {
		name           string
		memberships    []SupportDecisionMembership
		includeGrouped bool
	}{
		{name: "ungrouped", includeGrouped: false},
		{name: "simple mode includes grouped", memberships: []SupportDecisionMembership{{AccountID: 1, GroupID: 99}}, includeGrouped: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			table := buildSupportDecisionTestTable(t, &SupportDecisionConstructionSnapshot{
				Accounts:    []Account{account},
				Memberships: tt.memberships,
			})
			require.Equal(t, SupportDecisionPureMiss, table.Lookup(SupportDecisionQuery{
				Scope: SupportDecisionScope{
					Platform:             PlatformAnthropic,
					IncludeGrouped:       tt.includeGrouped,
					AllowMixedScheduling: true,
				},
				RequestedModel: "missing",
			}))
		})
	}
}

func TestSupportDecisionBuilderMatchesLegacyGenericScopeAndChannelMatrix(t *testing.T) {
	antigravity := Account{ID: 1, Platform: PlatformAntigravity, Credentials: map[string]any{"model_mapping": map[string]any{"claude-sonnet-4-5": "claude-sonnet-4-5", "claude-sonnet-4-5-thinking": "claude-sonnet-4-5-thinking"}}, Extra: map[string]any{"mixed_scheduling": true}}
	native := Account{ID: 2, Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": map[string]any{"other": "other"}}}
	accounts := []Account{antigravity, native}
	table := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot(accounts, PlatformAnthropic, []string{"claude-sonnet-4-5"}))
	for _, mixed := range []bool{false, true} {
		for _, thinking := range []bool{false, true} {
			legacy := legacyPureModelSupportMiss(legacyModelSupportMissInput{
				Accounts:             accounts,
				RequestedModel:       "claude-sonnet-4-5",
				Platform:             PlatformAnthropic,
				AllowMixedScheduling: mixed,
				ThinkingEnabled:      thinking,
				ModelSupported: func(account *Account, requested string, enabled bool) bool {
					return supportDecisionGenericModelSupported(account, requested, enabled)
				},
			})
			got := table.Lookup(SupportDecisionQuery{Scope: SupportDecisionScope{Platform: PlatformAnthropic, GroupID: 42, AllowMixedScheduling: mixed}, RequestedModel: "claude-sonnet-4-5", ThinkingEnabled: thinking})
			require.Equal(t, legacy, got == SupportDecisionPureMiss)
		}
	}

	channel := SupportDecisionChannel{Status: StatusActive, GroupIDs: []int64{42}, RestrictModels: true, BillingModelSource: BillingModelSourceUpstream, PricingModels: []SupportDecisionPricingModels{{Platform: PlatformAnthropic, Models: []string{"allowed-*"}}}}
	restrictedAccount := Account{ID: 3, Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": map[string]any{"known": "allowed-upstream"}}}
	snapshot := supportDecisionTestSnapshot([]Account{restrictedAccount}, PlatformAnthropic, []string{"missing"})
	snapshot.Channels = []SupportDecisionChannel{channel}
	restrictedTable := buildSupportDecisionTestTable(t, snapshot)
	legacy := legacyPureModelSupportMiss(legacyModelSupportMissInput{
		Accounts:       []Account{restrictedAccount},
		RequestedModel: "missing",
		Platform:       PlatformAnthropic,
		ModelSupported: func(account *Account, requested string, thinking bool) bool {
			return supportDecisionGenericModelSupported(account, requested, thinking)
		},
		UpstreamRestricted: func(account *Account, requested string) bool {
			return supportDecisionUpstreamRestricted(&channel, PlatformAnthropic, account, requested, false)
		},
	})
	got := restrictedTable.Lookup(SupportDecisionQuery{Scope: SupportDecisionScope{Platform: PlatformAnthropic, GroupID: 42}, RequestedModel: "missing"})
	require.Equal(t, legacy, got == SupportDecisionPureMiss)
}

func TestSupportDecisionBuilderMatchesLegacyGenericNormalizationAndWildcardMatrix(t *testing.T) {
	tests := []struct {
		name     string
		platform string
		account  Account
		models   []string
		thinking bool
	}{
		{name: "bedrock alias and model id family", platform: PlatformAnthropic, account: Account{Platform: PlatformAnthropic, Type: AccountTypeBedrock, Credentials: map[string]any{}}, models: []string{"claude-sonnet-4-5", "anthropic.claude-sonnet-4-5-20250929-v1:0"}},
		{name: "anthropic oauth normalization", platform: PlatformAnthropic, account: Account{Platform: PlatformAnthropic, Type: AccountTypeOAuth, Credentials: map[string]any{"model_mapping": map[string]any{"claude-sonnet-4-5-20250929": "upstream"}}}, models: []string{"claude-sonnet-4-5"}},
		{name: "anthropic vertex normalization", platform: PlatformAnthropic, account: Account{Platform: PlatformAnthropic, Type: AccountTypeServiceAccount, Credentials: map[string]any{"model_mapping": map[string]any{"claude-sonnet-4-5@20250929": "upstream"}}}, models: []string{"claude-sonnet-4-5"}},
		{name: "antigravity models prefix", platform: PlatformAntigravity, account: Account{Platform: PlatformAntigravity, Credentials: map[string]any{"model_mapping": map[string]any{"claude-sonnet-4-5": "claude-sonnet-4-5", "claude-sonnet-4-5-thinking": "claude-sonnet-4-5-thinking"}}}, models: []string{"models/claude-sonnet-4-5"}},
		{name: "exact beats wildcard", platform: PlatformAnthropic, account: Account{Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": map[string]any{"vendor-model": "exact", "vendor-*": "wild"}}}, models: []string{"vendor-model"}},
		{name: "longest wildcard", platform: PlatformAnthropic, account: Account{Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": map[string]any{"vendor-*": "broad", "vendor-special-*": "narrow"}}}, models: []string{"vendor-special-model"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			table := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot([]Account{tt.account}, tt.platform, []string{"hot"}))
			for _, model := range tt.models {
				legacy := legacyPureModelSupportMiss(legacyModelSupportMissInput{
					Accounts:        []Account{tt.account},
					RequestedModel:  model,
					Platform:        tt.platform,
					ThinkingEnabled: tt.thinking,
					ModelSupported: func(account *Account, requested string, thinking bool) bool {
						return supportDecisionGenericModelSupported(account, requested, thinking)
					},
				})
				got := table.Lookup(SupportDecisionQuery{Scope: SupportDecisionScope{Platform: tt.platform, GroupID: 42}, RequestedModel: model, ThinkingEnabled: tt.thinking})
				require.Equal(t, legacy, got == SupportDecisionPureMiss)
			}
		})
	}
}

func TestSupportDecisionBuilderMatchesLegacyOpenAIFiniteMatrix(t *testing.T) {
	wsConfig := config.GatewayOpenAIWSConfig{Enabled: true, APIKeyEnabled: true, ResponsesWebsocketsV2: true}
	tests := []struct {
		name    string
		account Account
		model   string
		query   SupportDecisionQuery
	}{
		{name: "api key allow all", account: Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, model: "custom-model"},
		{name: "oauth known default", account: Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}, model: "openai/gpt-5-codex"},
		{name: "oauth unknown default", account: Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}, model: "custom-model"},
		{name: "oauth known exact target", account: Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"model_mapping": map[string]any{"alias": "gpt-5.4"}}}, model: "alias"},
		{name: "oauth unknown exact target", account: Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"model_mapping": map[string]any{"alias": "custom"}}}, model: "alias"},
		{name: "privacy blocked", account: Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"model_mapping": map[string]any{"other": "gpt-5.4"}}}, model: "missing", query: SupportDecisionQuery{RequiresPrivacy: true}},
		{name: "endpoint blocked", account: Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"model_mapping": map[string]any{"other": "upstream"}}}, model: "missing", query: SupportDecisionQuery{EndpointCapability: OpenAIEndpointCapabilityOAuthCompactBodySignal}},
		{name: "image blocked", account: Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"model_mapping": map[string]any{"other": "gpt-5.4"}}}, model: "missing", query: SupportDecisionQuery{ImageCapability: OpenAIImagesCapabilityNative}},
		{name: "compact blocked", account: Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"model_mapping": map[string]any{"other": "gpt-5.4"}}, Extra: map[string]any{"openai_compact_mode": OpenAICompactModeForceOff}}, model: "missing", query: SupportDecisionQuery{RequireCompact: true}},
		{name: "transport blocked", account: Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1, Credentials: map[string]any{"model_mapping": map[string]any{"other": "upstream"}}}, model: "missing", query: SupportDecisionQuery{Transport: OpenAIUpstreamTransportResponsesWebsocketV2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			table := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot([]Account{tt.account}, PlatformOpenAI, []string{"hot"}), SupportDecisionBuildOptions{Generation: 77, OpenAIWS: wsConfig})
			legacy := legacyPureOpenAIModelSupportMiss(legacyOpenAIModelSupportMissInput{
				Accounts:           []Account{tt.account},
				RequestedModel:     tt.model,
				RequirePrivacy:     tt.query.RequiresPrivacy,
				EndpointCapability: tt.query.EndpointCapability,
				ImageCapability:    tt.query.ImageCapability,
				RequireCompact:     tt.query.RequireCompact,
				Transport:          tt.query.Transport,
				TransportCompatible: func(account *Account, required OpenAIUpstreamTransport) bool {
					return supportDecisionTransportCompatible(account, required, wsConfig)
				},
			})
			tt.query.Scope = SupportDecisionScope{Platform: PlatformOpenAI, GroupID: 42}
			tt.query.RequestedModel = tt.model
			require.Equal(t, legacy, table.Lookup(tt.query) == SupportDecisionPureMiss)
		})
	}
}

func TestSupportDecisionBuilderSupportingAccountWinsOverAllLaterPredicates(t *testing.T) {
	const model = "gpt-supported"
	supporting := Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"model_mapping": map[string]any{model: "gpt-5.4"}}, Extra: map[string]any{"privacy_mode": "default", "openai_compact_mode": OpenAICompactModeForceOff}}
	table := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot([]Account{supporting}, PlatformOpenAI, []string{model}))
	got := table.Lookup(SupportDecisionQuery{
		Scope:              SupportDecisionScope{Platform: PlatformOpenAI, GroupID: 42},
		RequestedModel:     model,
		RequiresPrivacy:    true,
		EndpointCapability: OpenAIEndpointCapabilityEmbeddings,
		RequireCompact:     true,
		Transport:          OpenAIUpstreamTransportResponsesWebsocketV2,
	})
	require.Equal(t, SupportDecisionNotPureMiss, got)
}

func TestSupportDecisionBuilderUsesConfiguredConcurrencyAndOpenAIWSConfig(t *testing.T) {
	const model = "gpt-external"
	base := Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"model_mapping": map[string]any{"gpt-other": "upstream"}}, Extra: map[string]any{"openai_apikey_responses_websockets_v2_mode": OpenAIWSIngressModeCtxPool}}
	wsConfig := config.GatewayOpenAIWSConfig{Enabled: true, APIKeyEnabled: true, ModeRouterV2Enabled: true, ResponsesWebsocketsV2: true, IngressModeDefault: OpenAIWSIngressModeOff}
	query := SupportDecisionQuery{Scope: SupportDecisionScope{Platform: PlatformOpenAI, GroupID: 42}, RequestedModel: model, Transport: OpenAIUpstreamTransportResponsesWebsocketV2}

	base.Concurrency = 0
	zero := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot([]Account{base}, PlatformOpenAI, []string{model}), SupportDecisionBuildOptions{Generation: 9, OpenAIWS: wsConfig})
	require.Equal(t, SupportDecisionNotPureMiss, zero.Lookup(query))

	base.Concurrency = 1
	positive := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot([]Account{base}, PlatformOpenAI, []string{model}), SupportDecisionBuildOptions{Generation: 10, OpenAIWS: wsConfig})
	require.Equal(t, SupportDecisionPureMiss, positive.Lookup(query))

	wsConfig.Enabled = false
	disabled := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot([]Account{base}, PlatformOpenAI, []string{model}), SupportDecisionBuildOptions{Generation: 11, OpenAIWS: wsConfig})
	require.Equal(t, SupportDecisionNotPureMiss, disabled.Lookup(query))
}

func TestSupportDecisionBuilderAcceptsExactBoundary4096(t *testing.T) {
	table, err := BuildSupportDecisionTable(supportDecisionBoundarySnapshot(SupportDecisionExactLimit, 0), SupportDecisionBuildOptions{Generation: 1})
	require.NoError(t, err)
	require.NotNil(t, table)
}

func TestSupportDecisionBuilderRejectsExactBoundary4097(t *testing.T) {
	_, err := BuildSupportDecisionTable(supportDecisionBoundarySnapshot(SupportDecisionExactLimit+1, 0), SupportDecisionBuildOptions{Generation: 1})
	require.ErrorContains(t, err, "exact keys")
}

func TestSupportDecisionBuilderAcceptsWildcardBoundary256(t *testing.T) {
	_, err := BuildSupportDecisionTable(supportDecisionBoundarySnapshot(0, SupportDecisionWildcardLimit), SupportDecisionBuildOptions{Generation: 1})
	require.NoError(t, err)
}

func TestSupportDecisionBuilderRejectsWildcardBoundary257(t *testing.T) {
	_, err := BuildSupportDecisionTable(supportDecisionBoundarySnapshot(0, SupportDecisionWildcardLimit+1), SupportDecisionBuildOptions{Generation: 1})
	require.ErrorContains(t, err, "wildcard rules")
}

func TestSupportDecisionBuilderEnforcesPerScopeHotBudget(t *testing.T) {
	snapshot := supportDecisionTestSnapshot([]Account{{Platform: PlatformOpenAI}}, PlatformOpenAI, makeHotModels(SupportDecisionHotModelLimit))
	_, err := BuildSupportDecisionTable(snapshot, SupportDecisionBuildOptions{Generation: 1})
	require.NoError(t, err)
}

func TestSupportDecisionBuilderEnforcesPerScopeFallbackBudget(t *testing.T) {
	_, err := BuildSupportDecisionTable(supportDecisionBoundarySnapshot(SupportDecisionExactLimit, SupportDecisionWildcardLimit), SupportDecisionBuildOptions{Generation: 1})
	require.NoError(t, err)
}

func TestSupportDecisionBuilderRejectsPerScopeFallbackAboveBudget(t *testing.T) {
	mapping := make(map[string]any, SupportDecisionExactLimit)
	for i := 0; i < SupportDecisionExactLimit; i++ {
		model := fmt.Sprintf("exact-%04d-%s", i, stringsOfLength(120))
		mapping[model] = "target"
	}
	snapshot := supportDecisionTestSnapshot([]Account{{
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"model_mapping": mapping,
		},
	}}, PlatformAnthropic, []string{"hot"})
	_, err := BuildSupportDecisionTable(snapshot, SupportDecisionBuildOptions{Generation: 1})
	require.ErrorContains(t, err, "fallback exceeds")
}

func TestSupportDecisionBuilderMeasuresSerializedPerScopeFallbackBudget(t *testing.T) {
	under := supportDecisionSerializedFallbackSnapshot(1800, 48)
	table, err := BuildSupportDecisionTable(under, SupportDecisionBuildOptions{Generation: 1})
	require.NoError(t, err)
	require.NotNil(t, table)

	over := supportDecisionSerializedFallbackSnapshot(SupportDecisionExactLimit, 96)
	_, err = BuildSupportDecisionTable(over, SupportDecisionBuildOptions{Generation: 1})
	require.ErrorContains(t, err, "fallback exceeds")
}

func TestSupportDecisionBuilderEnforcesCombinedWildcardBoundary(t *testing.T) {
	accepted := supportDecisionCombinedRuleSnapshot(254, true, 1, 0, 0)
	_, err := BuildSupportDecisionTable(accepted, SupportDecisionBuildOptions{Generation: 1})
	require.NoError(t, err)

	rejected := supportDecisionCombinedRuleSnapshot(254, true, 2, 0, 0)
	_, err = BuildSupportDecisionTable(rejected, SupportDecisionBuildOptions{Generation: 1})
	require.ErrorContains(t, err, "wildcard rules")
}

func TestSupportDecisionBuilderEnforcesCombinedExactBoundary(t *testing.T) {
	accepted := supportDecisionCombinedRuleSnapshot(0, false, 0, SupportDecisionExactLimit-1, 1)
	_, err := BuildSupportDecisionTable(accepted, SupportDecisionBuildOptions{Generation: 1})
	require.NoError(t, err)

	rejected := supportDecisionCombinedRuleSnapshot(0, false, 0, SupportDecisionExactLimit, 1)
	_, err = BuildSupportDecisionTable(rejected, SupportDecisionBuildOptions{Generation: 1})
	require.ErrorContains(t, err, "exact keys")
}

func TestSupportDecisionBuilderPreservesHotOverflowInFallback(t *testing.T) {
	models := makeHotModels(SupportDecisionHotModelLimit + 3)
	snapshot := supportDecisionTestSnapshot([]Account{{
		Platform: PlatformAnthropic,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"configured": "configured"},
		},
	}}, PlatformAnthropic, models)
	table := buildSupportDecisionTestTable(t, snapshot)
	scope := table.Scopes[table.scopeIndex[supportDecisionScopeKey{Platform: PlatformAnthropic, GroupID: 42}]]
	require.Len(t, scope.Hot, SupportDecisionHotModelLimit)
	for _, model := range models[SupportDecisionHotModelLimit:] {
		require.Contains(t, scope.Exact, model)
		require.Equal(t, SupportDecisionPureMiss, table.Lookup(SupportDecisionQuery{
			Scope:          SupportDecisionScope{Platform: PlatformAnthropic, GroupID: 42},
			RequestedModel: model,
		}))
	}
}

func TestSupportDecisionBuilderInternsStringsAcrossScopes(t *testing.T) {
	const shared = "shared-model"
	snapshot := &SupportDecisionConstructionSnapshot{
		Accounts: []Account{
			{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Credentials: map[string]any{"model_mapping": map[string]any{shared: shared}}},
			{ID: 2, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Credentials: map[string]any{"model_mapping": map[string]any{shared: shared}}},
		},
		Memberships: []SupportDecisionMembership{
			{AccountID: 1, GroupID: 41},
			{AccountID: 2, GroupID: 42},
		},
		Groups: []SupportDecisionGroup{
			{ID: 41, Platform: PlatformAnthropic, ModelsListConfig: GroupModelsListConfig{Enabled: true, Models: []string{"hot-a"}}},
			{ID: 42, Platform: PlatformAnthropic, ModelsListConfig: GroupModelsListConfig{Enabled: true, Models: []string{"hot-b"}}},
		},
	}
	table := buildSupportDecisionTestTable(t, snapshot)
	count := 0
	for _, value := range table.Strings {
		if value == shared {
			count++
		}
	}
	require.Equal(t, 1, count)
}

func TestSupportDecisionBuilderWarnsAboveTypicalGlobalTarget(t *testing.T) {
	table := &SupportDecisionTable{SchemaVersion: SupportDecisionSchemaVersion, Generation: 1, Strings: []string{stringsOfLength(SupportDecisionTypicalDocumentSize + 1)}}
	table.verified = true
	payload, err := encodeSupportDecisionDocumentUnchecked(table)
	require.NoError(t, err)
	require.Greater(t, len(payload), SupportDecisionTypicalDocumentSize)
	require.Less(t, len(payload), SupportDecisionMaxDocumentSize)
}

func TestSupportDecisionBuilderRejectsAboveHardGlobalLimit(t *testing.T) {
	table := &SupportDecisionTable{SchemaVersion: SupportDecisionSchemaVersion, Generation: 1, Strings: []string{stringsOfLength(SupportDecisionMaxDocumentSize + 1)}, verified: true}
	_, err := EncodeSupportDecisionDocument(table)
	require.ErrorContains(t, err, "exceeds")
}

func TestSupportDecisionBuilderRejectsOverlongModelOrPrefix(t *testing.T) {
	long := stringsOfLength(SupportDecisionMaxModelBytes + 1)
	for _, mapping := range []map[string]any{{long: "target"}, {long + "*": "target"}} {
		snapshot := supportDecisionTestSnapshot([]Account{{Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": mapping}}}, PlatformAnthropic, []string{"hot"})
		_, err := BuildSupportDecisionTable(snapshot, SupportDecisionBuildOptions{Generation: 1})
		require.ErrorContains(t, err, "exceeds")
	}
}

func supportDecisionSerializedFallbackSnapshot(exact, modelPadding int) *SupportDecisionConstructionSnapshot {
	mapping := make(map[string]any, exact)
	for i := 0; i < exact; i++ {
		mapping[fmt.Sprintf("exact-%04d-%s", i, stringsOfLength(modelPadding))] = "target"
	}
	return supportDecisionTestSnapshot([]Account{{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"model_mapping": mapping,
		},
	}}, PlatformOpenAI, []string{"hot"})
}

func supportDecisionCombinedRuleSnapshot(wildcard int, catchAll bool, channelWildcard, exact, channelExact int) *SupportDecisionConstructionSnapshot {
	mapping := make(map[string]any, wildcard+exact+1)
	for i := 0; i < wildcard; i++ {
		mapping[fmt.Sprintf("wild-%04d-*", i)] = "target"
	}
	if catchAll {
		mapping["*"] = "target"
	}
	for i := 0; i < exact; i++ {
		mapping[fmt.Sprintf("exact-%04d", i)] = "target"
	}
	pricing := make([]string, 0, channelWildcard+channelExact)
	for i := 0; i < channelWildcard; i++ {
		pricing = append(pricing, fmt.Sprintf("channel-wild-%04d-*", i))
	}
	for i := 0; i < channelExact; i++ {
		pricing = append(pricing, fmt.Sprintf("channel-exact-%04d", i))
	}
	snapshot := supportDecisionTestSnapshot([]Account{{Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Credentials: map[string]any{"model_mapping": mapping}}}, PlatformAnthropic, []string{"hot"})
	if len(pricing) > 0 {
		snapshot.Channels = []SupportDecisionChannel{{
			Status:             StatusActive,
			GroupIDs:           []int64{42},
			RestrictModels:     true,
			BillingModelSource: BillingModelSourceUpstream,
			PricingModels:      []SupportDecisionPricingModels{{Platform: PlatformAnthropic, Models: pricing}},
		}}
	}
	return snapshot
}

func supportDecisionBoundarySnapshot(exact, wildcard int) *SupportDecisionConstructionSnapshot {
	mapping := make(map[string]any, exact+wildcard)
	for i := 0; i < exact; i++ {
		mapping[fmt.Sprintf("exact-%04d", i)] = "target"
	}
	for i := 0; i < wildcard; i++ {
		mapping[fmt.Sprintf("wild-%04d-*", i)] = "target"
	}
	return supportDecisionTestSnapshot([]Account{{Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Credentials: map[string]any{"model_mapping": mapping}}}, PlatformAnthropic, []string{"hot"})
}

func makeHotModels(count int) []string {
	models := make([]string, count)
	for i := range models {
		models[i] = fmt.Sprintf("hot-%d", i)
	}
	return models
}

func stringsOfLength(length int) string {
	value := make([]byte, length)
	for i := range value {
		value[i] = 'x'
	}
	return string(value)
}
