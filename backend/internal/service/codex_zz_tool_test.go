package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAccount_IsCodexInjectZZToolEnabled(t *testing.T) {
	t.Run("openai oauth enabled", func(t *testing.T) {
		account := &Account{
			Platform: PlatformOpenAI,
			Type:     AccountTypeOAuth,
			Extra: map[string]any{
				featureKeyCodexInjectZZTool: true,
			},
		}
		require.True(t, account.IsCodexInjectZZToolEnabled())
	})

	t.Run("legacy nested flag is supported", func(t *testing.T) {
		account := &Account{
			Platform: PlatformOpenAI,
			Type:     AccountTypeOAuth,
			Extra: map[string]any{
				PlatformOpenAI: map[string]any{
					"codex_inject_zz_tool_enabled": true,
				},
			},
		}
		require.True(t, account.IsCodexInjectZZToolEnabled())
	})

	t.Run("missing or invalid value defaults to false", func(t *testing.T) {
		account := &Account{
			Platform: PlatformOpenAI,
			Type:     AccountTypeOAuth,
			Extra: map[string]any{
				featureKeyCodexInjectZZTool: "true",
			},
		}
		require.False(t, account.IsCodexInjectZZToolEnabled())
	})

	t.Run("non oauth or non openai accounts stay disabled", func(t *testing.T) {
		apiKeyAccount := &Account{
			Platform: PlatformOpenAI,
			Type:     AccountTypeAPIKey,
			Extra: map[string]any{
				featureKeyCodexInjectZZTool: true,
			},
		}
		require.False(t, apiKeyAccount.IsCodexInjectZZToolEnabled())

		otherPlatform := &Account{
			Platform: PlatformAnthropic,
			Type:     AccountTypeOAuth,
			Extra: map[string]any{
				featureKeyCodexInjectZZTool: true,
			},
		}
		require.False(t, otherPlatform.IsCodexInjectZZToolEnabled())
	})
}
