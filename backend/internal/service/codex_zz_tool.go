package service

const featureKeyCodexInjectZZTool = "codex_inject_zz_tool"

// IsCodexInjectZZToolEnabled returns whether the OpenAI OAuth account should
// inject the extra zz function tool into Codex/ChatGPT tool lists.
func (a *Account) IsCodexInjectZZToolEnabled() bool {
	if a == nil || !a.IsOpenAIOAuth() || a.Extra == nil {
		return false
	}
	if enabled, ok := a.Extra[featureKeyCodexInjectZZTool].(bool); ok {
		return enabled
	}
	if enabled, ok := a.Extra["codex_inject_zz_tool_enabled"].(bool); ok {
		return enabled
	}
	openaiConfig, _ := a.Extra[PlatformOpenAI].(map[string]any)
	if openaiConfig == nil {
		return false
	}
	if enabled, ok := openaiConfig[featureKeyCodexInjectZZTool].(bool); ok {
		return enabled
	}
	if enabled, ok := openaiConfig["codex_inject_zz_tool_enabled"].(bool); ok {
		return enabled
	}
	return false
}
