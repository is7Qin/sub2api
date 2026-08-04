package service

import (
	"strings"
	"unicode"
)

// openAIModelAliasSpellingReplacements 是 canonicalize 的纠错替换表。
// 包级共享：快速路径的恒等判定与慢速路径的替换必须使用同一份字面量。
var openAIModelAliasSpellingReplacements = []struct {
	from string
	to   string
}{
	{"gpt-5.4mini", "gpt-5.4-mini"},
	{"gpt-5.4nano", "gpt-5.4-nano"},
	{"gpt-5.3-codexspark", "gpt-5.3-codex-spark"},
	{"gpt-5.3codexspark", "gpt-5.3-codex-spark"},
	{"gpt-5.3codex", "gpt-5.3-codex"},
}

func lastOpenAIModelSegment(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	if strings.Contains(model, "/") {
		parts := strings.Split(model, "/")
		model = parts[len(parts)-1]
	}
	return strings.TrimSpace(model)
}

func canonicalizeOpenAIModelAliasSpelling(model string) string {
	// 快速路径：输入已是规范拼写时原样返回。热路径（IsModelSupported 的每请求
	// 判别、计费模型候选）调用频繁，慢速路径的 ToLower/Fields/Join 会产生
	// 线性分配；isCanonicalOpenAIModelAliasSpelling 保证恒等判定与慢速路径
	// 输出完全一致（见其注释的逐项对应）。
	if isCanonicalOpenAIModelAliasSpelling(model) {
		return model
	}
	model = strings.ToLower(lastOpenAIModelSegment(model))
	if model == "" {
		return ""
	}

	normalized := strings.ReplaceAll(model, "_", "-")
	normalized = strings.Join(strings.Fields(normalized), "-")
	for strings.Contains(normalized, "--") {
		normalized = strings.ReplaceAll(normalized, "--", "-")
	}

	if strings.HasPrefix(normalized, "gpt5") {
		normalized = "gpt-5" + strings.TrimPrefix(normalized, "gpt5")
	}
	if !strings.HasPrefix(normalized, "gpt-") && !strings.Contains(normalized, "codex") {
		return ""
	}

	for _, replacement := range openAIModelAliasSpellingReplacements {
		normalized = strings.ReplaceAll(normalized, replacement.from, replacement.to)
	}
	return normalized
}

// isCanonicalOpenAIModelAliasSpelling 判断 canonicalizeOpenAIModelAliasSpelling
// 的输出即输入本身（恒等输入），逐项对应慢速路径的每个变换，全程零分配：
//   - 无首尾空白、无 '/'：lastOpenAIModelSegment 恒等
//   - 全小写（逐 rune 对照 unicode.ToLower）：ToLower 恒等
//   - 无下划线、无空白：ReplaceAll("_","-") 与 Fields/Join 恒等
//   - 无连续连字符："--" 折叠恒等
//   - 无 gpt5 连写、不含纠错替换字面量：gpt5 重写与替换表恒等
//   - 通过 gpt-/codex 前缀门：与慢速路径的判空门一致
func isCanonicalOpenAIModelAliasSpelling(model string) bool {
	if model == "" || strings.Contains(model, "/") || strings.ContainsAny(model, "_ \t\n\r\v\f") {
		return false
	}
	for _, r := range model {
		if unicode.IsSpace(r) {
			return false
		}
		if unicode.ToLower(r) != r {
			return false
		}
	}
	if strings.Contains(model, "--") || strings.HasPrefix(model, "gpt5") {
		return false
	}
	for _, replacement := range openAIModelAliasSpellingReplacements {
		if strings.Contains(model, replacement.from) {
			return false
		}
	}
	return strings.HasPrefix(model, "gpt-") || strings.Contains(model, "codex")
}

func normalizeKnownOpenAICodexModel(model string) string {
	normalized := canonicalizeOpenAIModelAliasSpelling(model)
	if normalized == "" {
		return ""
	}

	if mapped := getNormalizedCodexModel(normalized); mapped != "" {
		return mapped
	}
	if strings.HasSuffix(normalized, "-openai-compact") {
		if mapped := getNormalizedCodexModel(strings.TrimSuffix(normalized, "-openai-compact")); mapped != "" {
			return mapped
		}
	}

	switch {
	case strings.Contains(normalized, "gpt-5.6-sol"):
		return "gpt-5.6-sol"
	case strings.Contains(normalized, "gpt-5.6-terra"):
		return "gpt-5.6-terra"
	case strings.Contains(normalized, "gpt-5.6-luna"):
		return "gpt-5.6-luna"
	case normalized == "gpt-5.6":
		return "gpt-5.6-sol"
	case strings.HasPrefix(normalized, "gpt-5.6-"):
		suffix := strings.TrimPrefix(normalized, "gpt-5.6-")
		if suffix == "max" || isKnownCodexModelSuffix(suffix) {
			return "gpt-5.6-sol"
		}
		return ""
	case strings.Contains(normalized, "gpt-5.5-pro"):
		return "gpt-5.5-pro"
	case strings.Contains(normalized, "gpt-5.5"):
		return "gpt-5.5"
	case strings.Contains(normalized, "gpt-5.4-mini"):
		return "gpt-5.4-mini"
	case strings.Contains(normalized, "gpt-5.4-nano"):
		return "gpt-5.4-nano"
	case strings.Contains(normalized, "gpt-5.4"):
		return "gpt-5.4"
	case strings.Contains(normalized, "gpt-5.2"):
		return "gpt-5.2"
	case strings.Contains(normalized, "gpt-5.3-codex-spark"):
		return "gpt-5.3-codex-spark"
	case strings.Contains(normalized, "gpt-5.3-codex"):
		return "gpt-5.3-codex"
	case strings.Contains(normalized, "gpt-5.3"):
		return "gpt-5.3-codex"
	case strings.Contains(normalized, "codex"):
		return "gpt-5.3-codex"
	case strings.Contains(normalized, "gpt-5"):
		return "gpt-5.4"
	default:
		return ""
	}
}

// isOpenAIGPT56Model 判断是否 GPT-5.6 系列模型；入参可为原始模型名
// （含大小写/路径/后缀变体）或已归一化的基名，两者均能正确识别。
func isOpenAIGPT56Model(model string) bool {
	normalized := canonicalizeOpenAIModelAliasSpelling(model)
	if normalized == "gpt-5.6" {
		return true
	}
	if suffix, ok := strings.CutPrefix(normalized, "gpt-5.6-"); ok && (suffix == "max" || isKnownCodexModelSuffix(suffix)) {
		return true
	}
	for _, prefix := range []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"} {
		if normalized == prefix || strings.HasPrefix(normalized, prefix+"-") {
			return true
		}
	}
	return false
}

func appendUsageBillingModelCandidate(candidates []string, seen map[string]struct{}, model string) []string {
	trimmed := strings.TrimSpace(model)
	if trimmed == "" {
		return candidates
	}
	add := func(candidate string) {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			return
		}
		key := strings.ToLower(candidate)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		candidates = append(candidates, candidate)
	}

	add(trimmed)
	if canonical := canonicalizeOpenAIModelAliasSpelling(trimmed); canonical != "" {
		add(canonical)
	}
	if normalized := normalizeKnownOpenAICodexModel(trimmed); normalized != "" {
		add(normalized)
	}
	return candidates
}

func usageBillingModelCandidates(primary string, alternates ...string) []string {
	seen := make(map[string]struct{}, 1+len(alternates))
	candidates := appendUsageBillingModelCandidate(nil, seen, primary)
	for _, alternate := range alternates {
		candidates = appendUsageBillingModelCandidate(candidates, seen, alternate)
	}
	return candidates
}

func firstUsageBillingModel(candidates []string) string {
	for _, candidate := range candidates {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
