//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsModelSupported_OpenAIOAuthEmptyMappingUsesCodexCapabilities(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	require.True(t, account.IsModelSupported("gpt-5.4-high"))
	require.True(t, account.IsModelSupported("codex-mini-latest"))
	require.False(t, account.IsModelSupported("deepseek-v4"))
	require.False(t, account.IsModelSupported("future-vendor-model"))
}

func TestIsModelSupported_OpenAIOAuthExplicitMappingRemainsAuthoritative(t *testing.T) {
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"deepseek-v4": "gpt-5.4"},
		},
	}

	require.True(t, account.IsModelSupported("deepseek-v4"))
	require.False(t, account.IsModelSupported("future-vendor-model"))

	account.Credentials["model_mapping"] = map[string]any{"deepseek-v4": "future-vendor-model"}
	require.False(t, account.IsModelSupported("deepseek-v4"))

	account.Credentials["model_mapping"] = map[string]any{"custom": "gpt-5.4-high"}
	require.True(t, account.IsModelSupported("custom"))
}

func TestIsModelSupported_OpenAIAPIKeyRemainsIsolated(t *testing.T) {
	apiKey := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	require.True(t, apiKey.IsModelSupported("deepseek-v4"))
}

func TestIsModelSupported_OpenAIAPIKeyPassthroughIgnoresLeftoverMapping(t *testing.T) {
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Extra:    map[string]any{"openai_passthrough": true},
		Credentials: map[string]any{
			"model_mapping": map[string]any{"gpt-5.4": "gpt-5.4"},
		},
	}

	require.True(t, account.IsModelSupported("gpt-5.6-sol"))
	require.True(t, account.IsModelSupported("deepseek-v4"))
}

func TestIsModelSupported_OpenAIOAuthLegacyPassthroughDoesNotBypassMapping(t *testing.T) {
	account := &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra:    map[string]any{"openai_passthrough": true},
		Credentials: map[string]any{
			"model_mapping": map[string]any{"gpt-5.4": "gpt-5.4"},
		},
	}

	require.False(t, account.IsModelSupported("deepseek-v4"))
}
