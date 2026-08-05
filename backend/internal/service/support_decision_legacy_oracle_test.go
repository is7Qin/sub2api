//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestLegacyPureModelSupportMissSemanticMatrix(t *testing.T) {
	const requestedModel = "claude-sonnet-4-5"
	unsupported := Account{
		ID:          1,
		Platform:    PlatformAnthropic,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{"model_mapping": map[string]any{"claude-haiku-*": "claude-haiku-3-5"}},
	}
	supportingExact := unsupported
	supportingExact.ID = 2
	supportingExact.Credentials = map[string]any{"model_mapping": map[string]any{requestedModel: requestedModel}}
	supportingWildcard := unsupported
	supportingWildcard.ID = 3
	supportingWildcard.Credentials = map[string]any{"model_mapping": map[string]any{"claude-sonnet-*": requestedModel}}
	wrongPlatform := unsupported
	wrongPlatform.ID = 4
	wrongPlatform.Platform = PlatformGemini
	privacyBlocked := unsupported
	privacyBlocked.ID = 5
	privacyBlocked.Platform = PlatformAntigravity
	privacyBlocked.Extra = map[string]any{"mixed_scheduling": true}

	tests := []struct {
		name              string
		accounts          []Account
		platform          string
		allowMixed        bool
		requirePrivacy    bool
		channelRestricted func(*Account, string) bool
		want              bool
	}{
		{name: "unsupported eligible witness", accounts: []Account{unsupported}, platform: PlatformAnthropic, want: true},
		{name: "no accounts", platform: PlatformAnthropic, want: false},
		{name: "wrong platform has no eligible witness", accounts: []Account{wrongPlatform}, platform: PlatformAnthropic, want: false},
		{name: "exact mapping supports", accounts: []Account{unsupported, supportingExact}, platform: PlatformAnthropic, want: false},
		{name: "wildcard mapping supports", accounts: []Account{unsupported, supportingWildcard}, platform: PlatformAnthropic, want: false},
		{
			name:           "generic privacy restriction",
			accounts:       []Account{privacyBlocked},
			platform:       PlatformAnthropic,
			allowMixed:     true,
			requirePrivacy: true,
			want:           false,
		},
		{
			name:              "generic channel restriction",
			accounts:          []Account{unsupported},
			platform:          PlatformAnthropic,
			channelRestricted: func(*Account, string) bool { return true },
			want:              false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := legacyPureModelSupportMiss(legacyModelSupportMissInput{
				Accounts:             tt.accounts,
				RequestedModel:       requestedModel,
				Platform:             tt.platform,
				AllowMixedScheduling: tt.allowMixed,
				RequirePrivacy:       tt.requirePrivacy,
				ModelSupported: func(account *Account, model string, _ bool) bool {
					return account.IsModelSupported(model)
				},
				UpstreamRestricted: tt.channelRestricted,
			})
			require.Equal(t, tt.want, got)
		})
	}

	t.Run("caller guards", func(t *testing.T) {
		repo := &mockAccountRepoForPlatform{accountsByID: map[int64]*Account{}, accounts: []Account{unsupported}}
		svc := &GatewayService{accountRepo: repo, cfg: testConfig()}
		enabled := WithPublicModelSupportMiss404(context.Background())

		require.False(t, svc.isPureModelSupportMiss(enabled, nil, "  ", PlatformAnthropic, nil, false, nil, nil))
		require.False(t, svc.isPureModelSupportMiss(context.Background(), nil, requestedModel, PlatformAnthropic, nil, false, nil, nil))
		require.False(t, svc.isPureModelSupportMiss(enabled, nil, requestedModel, PlatformAnthropic, map[int64]struct{}{999: {}}, false, nil, nil))
	})

	t.Run("persistent scope replaces caller scope", func(t *testing.T) {
		repo := &mockAccountRepoForPlatform{
			accountsByID: map[int64]*Account{},
			listModelAvailabilityCandidates: func(context.Context, *int64, []string, bool) ([]Account, error) {
				return []Account{unsupported}, nil
			},
		}
		svc := &GatewayService{accountRepo: repo, cfg: testConfig()}
		got := svc.isPureModelSupportMiss(WithPublicModelSupportMiss404(context.Background()), []Account{supportingExact}, requestedModel, PlatformAnthropic, nil, false, nil, nil)
		require.True(t, got)
	})

	t.Run("mixed scheduling ungrouped scope", func(t *testing.T) {
		var gotPlatforms []string
		var gotIncludeGrouped bool
		repo := &mockAccountRepoForPlatform{
			accountsByID: map[int64]*Account{},
			listModelAvailabilityCandidates: func(_ context.Context, groupID *int64, platforms []string, includeGrouped bool) ([]Account, error) {
				require.Nil(t, groupID)
				gotPlatforms = append([]string(nil), platforms...)
				gotIncludeGrouped = includeGrouped
				mixed := privacyBlocked
				mixed.Extra = map[string]any{"mixed_scheduling": true, "privacy_mode": AntigravityPrivacySet}
				return []Account{mixed}, nil
			},
		}
		svc := &GatewayService{accountRepo: repo, cfg: &config.Config{RunMode: config.RunModeSimple}}
		got := svc.isPureModelSupportMiss(WithPublicModelSupportMiss404(context.Background()), nil, requestedModel, PlatformAnthropic, nil, true, nil, nil)
		require.True(t, got)
		require.Equal(t, []string{PlatformAnthropic, PlatformAntigravity}, gotPlatforms)
		require.True(t, gotIncludeGrouped)
	})
}

func TestLegacyPureModelSupportMissAntigravityThinkingCoordinate(t *testing.T) {
	const model = "claude-sonnet-4-5"
	account := Account{
		Platform: PlatformAntigravity,
		Credentials: map[string]any{
			"model_mapping": map[string]any{model: model},
		},
		Extra: map[string]any{"mixed_scheduling": true},
	}
	modelSupported := func(account *Account, requestedModel string, thinkingEnabled bool) bool {
		mapped := mapAntigravityModel(account, requestedModel)
		if mapped == "" {
			return false
		}
		finalModel := applyThinkingModelSuffix(mapped, thinkingEnabled)
		return finalModel == mapped || account.IsModelSupported(finalModel)
	}

	for _, tc := range []struct {
		name            string
		thinkingEnabled bool
		want            bool
	}{
		{name: "thinking disabled supports base mapping", want: false},
		{name: "thinking enabled requires suffixed mapping", thinkingEnabled: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := legacyPureModelSupportMiss(legacyModelSupportMissInput{
				Accounts:             []Account{account},
				RequestedModel:       model,
				Platform:             PlatformAnthropic,
				AllowMixedScheduling: true,
				ThinkingEnabled:      tc.thinkingEnabled,
				ModelSupported:       modelSupported,
			})
			require.Equal(t, tc.want, got)
		})
	}

	query := SupportDecisionQuery{ThinkingEnabled: true}
	require.True(t, query.ThinkingEnabled)
}

func TestLegacyPureOpenAIModelSupportMissSemanticMatrix(t *testing.T) {
	const requestedModel = "gpt-external"
	unsupportedAPIKey := Account{
		ID:          10,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{"model_mapping": map[string]any{"gpt-allowed": "gpt-allowed"}},
	}

	baseInput := func(account Account) legacyOpenAIModelSupportMissInput {
		return legacyOpenAIModelSupportMissInput{
			Accounts:       []Account{account},
			RequestedModel: requestedModel,
			TransportCompatible: func(*Account, OpenAIUpstreamTransport) bool {
				return true
			},
		}
	}

	tests := []struct {
		name   string
		mutate func(*legacyOpenAIModelSupportMissInput, *Account)
		want   bool
	}{
		{name: "unsupported eligible witness", want: true},
		{
			name: "privacy restriction",
			mutate: func(input *legacyOpenAIModelSupportMissInput, account *Account) {
				account.Type = AccountTypeOAuth
				account.Extra = map[string]any{"privacy_mode": "default"}
				account.Credentials = map[string]any{"model_mapping": map[string]any{"gpt-allowed": "gpt-5.4"}}
				input.RequirePrivacy = true
			},
			want: false,
		},
		{
			name: "channel restriction",
			mutate: func(input *legacyOpenAIModelSupportMissInput, _ *Account) {
				input.UpstreamRestricted = func(*Account, string, bool) bool { return true }
			},
			want: false,
		},
		{
			name: "endpoint restriction",
			mutate: func(input *legacyOpenAIModelSupportMissInput, account *Account) {
				account.Type = AccountTypeOAuth
				input.EndpointCapability = OpenAIEndpointCapabilityEmbeddings
			},
			want: false,
		},
		{
			name: "image restriction",
			mutate: func(input *legacyOpenAIModelSupportMissInput, account *Account) {
				account.Type = AccountTypeSetupToken
				input.ImageCapability = OpenAIImagesCapabilityBasic
			},
			want: false,
		},
		{
			name: "compact restriction",
			mutate: func(input *legacyOpenAIModelSupportMissInput, account *Account) {
				account.Extra = map[string]any{"openai_compact_mode": OpenAICompactModeForceOff}
				input.RequireCompact = true
			},
			want: false,
		},
		{
			name: "transport restriction",
			mutate: func(input *legacyOpenAIModelSupportMissInput, _ *Account) {
				input.Transport = OpenAIUpstreamTransportResponsesWebsocketV2
				input.TransportCompatible = func(*Account, OpenAIUpstreamTransport) bool { return false }
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := unsupportedAPIKey
			input := baseInput(account)
			if tt.mutate != nil {
				tt.mutate(&input, &account)
				input.Accounts = []Account{account}
			}
			require.Equal(t, tt.want, legacyPureOpenAIModelSupportMiss(input))
		})
	}

	t.Run("support mappings and OpenAI authentication semantics", func(t *testing.T) {
		cases := []struct {
			name    string
			account Account
			model   string
			want    bool
		}{
			{
				name: "API-key passthrough allow-all despite stale mapping",
				account: Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
					Extra:       map[string]any{"openai_passthrough": true},
					Credentials: map[string]any{"model_mapping": map[string]any{"stale": "stale"}}},
				model: requestedModel,
				want:  false,
			},
			{
				name:    "OAuth normalizes known Codex alias",
				account: Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth},
				model:   "openai/gpt-5-codex",
				want:    false,
			},
			{
				name: "OAuth legacy passthrough does not allow unknown model",
				account: Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth,
					Extra: map[string]any{"openai_passthrough": true}},
				model: requestedModel,
				want:  true,
			},
			{
				name: "exact mapping",
				account: Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
					Credentials: map[string]any{"model_mapping": map[string]any{requestedModel: "upstream-model"}}},
				model: requestedModel,
				want:  false,
			},
			{
				name: "wildcard mapping",
				account: Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
					Credentials: map[string]any{"model_mapping": map[string]any{"gpt-*": "upstream-model"}}},
				model: requestedModel,
				want:  false,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				input := baseInput(tc.account)
				input.RequestedModel = tc.model
				require.Equal(t, tc.want, legacyPureOpenAIModelSupportMiss(input))
			})
		}
	})

	t.Run("caller guards and persistent replacement", func(t *testing.T) {
		repo := &mockAccountRepoForPlatform{
			accountsByID: map[int64]*Account{},
			listModelAvailabilityCandidates: func(context.Context, *int64, []string, bool) ([]Account, error) {
				return []Account{unsupportedAPIKey}, nil
			},
		}
		service := &OpenAIGatewayService{accountRepo: repo, cfg: testConfig()}
		enabled := WithPublicModelSupportMiss404(context.Background())
		supporting := unsupportedAPIKey
		supporting.Credentials = nil

		require.False(t, isPureOpenAIModelSupportMiss(enabled, service, nil, nil, " ", nil, false, "", "", OpenAIUpstreamTransportAny, nil))
		require.False(t, isPureOpenAIModelSupportMiss(context.Background(), service, nil, nil, requestedModel, nil, false, "", "", OpenAIUpstreamTransportAny, nil))
		require.False(t, isPureOpenAIModelSupportMiss(enabled, service, nil, nil, requestedModel, map[int64]struct{}{10: {}}, false, "", "", OpenAIUpstreamTransportAny, nil))
		require.True(t, isPureOpenAIModelSupportMiss(enabled, service, nil, []Account{supporting}, requestedModel, nil, false, "", "", OpenAIUpstreamTransportAny, nil))
	})
}

func TestLegacyPureModelSupportMissSupportingAccountWins(t *testing.T) {
	const model = "claude-sonnet-4-5"

	tests := []struct {
		name               string
		account            Account
		requirePrivacy     bool
		upstreamRestricted func(account *Account, requestedModel string) bool
		assertBlocked      func(t *testing.T, account *Account)
	}{
		{
			name: "privacy",
			account: Account{
				Platform: PlatformAntigravity,
				Credentials: map[string]any{
					"model_mapping": map[string]any{model: model},
				},
				Extra: map[string]any{
					"mixed_scheduling": true,
					"privacy_mode":     "default",
				},
			},
			requirePrivacy: true,
			assertBlocked: func(t *testing.T, account *Account) {
				require.True(t, shouldBlockAccountForPrivacyRequirement(account, &Group{RequirePrivacySet: true}))
			},
		},
		{
			name: "upstream channel",
			account: Account{
				Platform: PlatformAnthropic,
				Credentials: map[string]any{
					"model_mapping": map[string]any{model: model},
				},
			},
			upstreamRestricted: func(account *Account, requestedModel string) bool {
				return resolveAccountUpstreamModel(account, requestedModel) == model
			},
			assertBlocked: func(t *testing.T, account *Account) {
				require.Equal(t, model, resolveAccountUpstreamModel(account, model))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.True(t, tt.account.IsModelSupported(model), "fixture must support the requested model")
			tt.assertBlocked(t, &tt.account)

			got := legacyPureModelSupportMiss(legacyModelSupportMissInput{
				Accounts:             []Account{tt.account},
				RequestedModel:       model,
				Platform:             PlatformAnthropic,
				AllowMixedScheduling: tt.account.Platform == PlatformAntigravity,
				RequirePrivacy:       tt.requirePrivacy,
				ModelSupported: func(account *Account, model string, _ bool) bool {
					return account.IsModelSupported(model)
				},
				UpstreamRestricted: tt.upstreamRestricted,
			})
			require.False(t, got)
		})
	}
}

func TestLegacyPureOpenAIModelSupportMissSupportingAccountWins(t *testing.T) {
	const model = "gpt-external"

	oauthAccount := func() Account {
		return Account{
			Platform: PlatformOpenAI,
			Type:     AccountTypeOAuth,
			Credentials: map[string]any{
				"model_mapping": map[string]any{model: "gpt-5.4"},
			},
		}
	}
	apiKeyAccount := func() Account {
		return Account{
			Platform: PlatformOpenAI,
			Type:     AccountTypeAPIKey,
			Credentials: map[string]any{
				"model_mapping": map[string]any{model: "upstream"},
			},
		}
	}

	tests := []struct {
		name                string
		account             Account
		requirePrivacy      bool
		endpointCapability  OpenAIEndpointCapability
		imageCapability     OpenAIImagesCapability
		requireCompact      bool
		transport           OpenAIUpstreamTransport
		upstreamRestricted  bool
		transportCompatible bool
		assertBlocked       func(t *testing.T, account *Account)
	}{
		{
			name:           "privacy",
			account:        oauthAccount(),
			requirePrivacy: true,
			assertBlocked: func(t *testing.T, account *Account) {
				require.True(t, shouldBlockAccountForPrivacyRequirement(account, &Group{RequirePrivacySet: true}))
			},
		},
		{
			name:               "upstream channel",
			account:            apiKeyAccount(),
			upstreamRestricted: true,
			assertBlocked: func(t *testing.T, account *Account) {
				require.NotEmpty(t, resolveOpenAIAccountUpstreamModelForRequest(account, model, false))
			},
		},
		{
			name:               "endpoint",
			account:            oauthAccount(),
			endpointCapability: OpenAIEndpointCapabilityEmbeddings,
			assertBlocked: func(t *testing.T, account *Account) {
				require.False(t, account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityEmbeddings))
			},
		},
		{
			name:            "image",
			account:         func() Account { account := oauthAccount(); account.Type = AccountTypeSetupToken; return account }(),
			imageCapability: OpenAIImagesCapabilityBasic,
			assertBlocked: func(t *testing.T, account *Account) {
				require.False(t, account.SupportsOpenAIImageCapability(OpenAIImagesCapabilityBasic))
			},
		},
		{
			name: "compact",
			account: func() Account {
				account := apiKeyAccount()
				account.Extra = map[string]any{"openai_compact_mode": OpenAICompactModeForceOff}
				return account
			}(),
			requireCompact: true,
			assertBlocked: func(t *testing.T, account *Account) {
				require.Zero(t, openAICompactSupportTier(account))
			},
		},
		{
			name:                "transport",
			account:             apiKeyAccount(),
			transport:           OpenAIUpstreamTransportResponsesWebsocketV2,
			transportCompatible: false,
			assertBlocked: func(t *testing.T, _ *Account) {
				require.NotEqual(t, OpenAIUpstreamTransportAny, OpenAIUpstreamTransportResponsesWebsocketV2)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.True(t, tt.account.IsModelSupported(model), "fixture must support the requested model")
			tt.assertBlocked(t, &tt.account)

			got := legacyPureOpenAIModelSupportMiss(legacyOpenAIModelSupportMissInput{
				Accounts:           []Account{tt.account},
				RequestedModel:     model,
				RequirePrivacy:     tt.requirePrivacy,
				EndpointCapability: tt.endpointCapability,
				ImageCapability:    tt.imageCapability,
				RequireCompact:     tt.requireCompact,
				Transport:          tt.transport,
				UpstreamRestricted: func(*Account, string, bool) bool {
					return tt.upstreamRestricted
				},
				TransportCompatible: func(*Account, OpenAIUpstreamTransport) bool {
					return tt.transportCompatible
				},
			})
			require.False(t, got)
		})
	}
}
