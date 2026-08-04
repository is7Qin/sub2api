package service

import (
	"fmt"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/modelcatalog"
	"github.com/stretchr/testify/require"
)

func TestResolveSupportDecisionHotModelsPrecedence(t *testing.T) {
	configured := config.SupportDecisionHotModelsConfig{
		OpenAI: []string{"configured-openai"},
	}

	t.Run("enabled group custom list", func(t *testing.T) {
		group := &Group{ModelsListConfig: GroupModelsListConfig{
			Enabled: true,
			Models:  []string{"group-first", "group-second"},
		}}
		got := ResolveSupportDecisionHotModels(PlatformOpenAI, group, configured)
		require.Equal(t, []string{"group-first", "group-second"}, got.HotModels)
		require.Empty(t, got.FallbackModels)
	})

	t.Run("enabled but empty group list is not a custom list", func(t *testing.T) {
		group := &Group{ModelsListConfig: GroupModelsListConfig{Enabled: true}}
		got := ResolveSupportDecisionHotModels(PlatformOpenAI, group, configured)
		require.Equal(t, []string{"configured-openai"}, got.HotModels)
		require.Empty(t, got.FallbackModels)
	})

	t.Run("configured platform list", func(t *testing.T) {
		group := &Group{ModelsListConfig: GroupModelsListConfig{
			Enabled: false,
			Models:  []string{"disabled-group-model"},
		}}
		got := ResolveSupportDecisionHotModels(PlatformOpenAI, group, configured)
		require.Equal(t, []string{"configured-openai"}, got.HotModels)
		require.Empty(t, got.FallbackModels)
	})

	t.Run("default platform catalog", func(t *testing.T) {
		got := ResolveSupportDecisionHotModels(PlatformAnthropic, nil, configured)
		require.Equal(t, modelcatalog.DefaultModelIDs(PlatformAnthropic), got.HotModels)
		require.Empty(t, got.FallbackModels)
	})
}

func TestResolveSupportDecisionHotModelsTrimsDeduplicatesAndPreservesOrder(t *testing.T) {
	configured := config.SupportDecisionHotModelsConfig{
		Gemini: []string{" model-b ", "", "model-a", "model-b", "  ", "model-c", "model-a"},
	}

	got := ResolveSupportDecisionHotModels(PlatformGemini, nil, configured)

	require.Equal(t, []string{"model-b", "model-a", "model-c"}, got.HotModels)
	require.Empty(t, got.FallbackModels)
}

func TestResolveSupportDecisionHotModelsCapsHotSetWithoutDroppingFallbackSemantics(t *testing.T) {
	models := make([]string, SupportDecisionHotModelLimit+3)
	for i := range models {
		models[i] = fmt.Sprintf("model-%02d", i)
	}
	configured := config.SupportDecisionHotModelsConfig{Antigravity: models}

	got := ResolveSupportDecisionHotModels(PlatformAntigravity, nil, configured)

	require.Len(t, got.HotModels, SupportDecisionHotModelLimit)
	require.Equal(t, models[:SupportDecisionHotModelLimit], got.HotModels)
	require.Equal(t, models[SupportDecisionHotModelLimit:], got.FallbackModels)
}
