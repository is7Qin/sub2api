package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeKnownOpenAICodexModel_BareGPT56RoutesToSol(t *testing.T) {
	tests := map[string]string{
		"gpt-5.6":            "gpt-5.6-sol",
		"openai/gpt-5.6":     "gpt-5.6-sol",
		"gpt5.6":             "gpt-5.6-sol",
		"gpt-5.6-high":       "gpt-5.6-sol",
		"gpt-5.6-max":        "gpt-5.6-sol",
		"gpt-5.6-2026-07-09": "gpt-5.6-sol",
		"openai/gpt-5.6-max": "gpt-5.6-sol",
	}

	for input, expected := range tests {
		t.Run(input, func(t *testing.T) {
			require.Equal(t, expected, normalizeKnownOpenAICodexModel(input))
		})
	}
}

func TestUsageBillingModelCandidates_BareGPT56IncludesSol(t *testing.T) {
	require.Equal(t,
		[]string{"gpt-5.6", "gpt-5.6-sol"},
		usageBillingModelCandidates("gpt-5.6"),
	)
	require.Equal(t,
		[]string{"openai/gpt-5.6", "gpt-5.6", "gpt-5.6-sol"},
		usageBillingModelCandidates("openai/gpt-5.6"),
	)
}

// TestCanonicalizeOpenAIModelAliasSpelling_FastPathIdentity 验证规范拼写走
// 快速路径时输出即输入（零分配路径），变体拼写仍由慢速路径纠错：两路径
// 结果均与 canonicalize 的历史行为一致。
func TestCanonicalizeOpenAIModelAliasSpelling_FastPathIdentity(t *testing.T) {
	canonical := []string{
		"gpt-5", "gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex", "gpt-5.3-codex-spark",
		"codex", "codex-mini-latest", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna",
	}
	for _, m := range canonical {
		require.Equal(t, m, canonicalizeOpenAIModelAliasSpelling(m), "规范拼写应走快速路径恒等返回")
	}
	variants := map[string]string{
		"GPT-5.4":            "gpt-5.4",
		"gpt-5.4mini":        "gpt-5.4-mini",
		"gpt-5.4_nano":       "gpt-5.4-nano",
		"gpt5.4":             "gpt-5.4",
		"gpt-5.3codex":       "gpt-5.3-codex",
		"gpt-5.3-codexspark": "gpt-5.3-codex-spark",
		"models/gpt-5.4":     "gpt-5.4",
		"gpt-5.4 ":           "gpt-5.4",
		"foo":                "",
		"claude-sonnet":      "",
	}
	for from, want := range variants {
		require.Equal(t, want, canonicalizeOpenAIModelAliasSpelling(from), "变体拼写应由慢速路径纠错")
	}
}
