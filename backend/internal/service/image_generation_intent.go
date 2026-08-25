package service

import (
	"bytes"
	"strings"

	"github.com/tidwall/gjson"
)

const (
	openAIResponsesEndpoint          = "/v1/responses"
	openAIResponsesCompactEndpoint   = "/v1/responses/compact"
	imageGenerationPermissionMessage = "Image generation is not enabled for this group"
)

// ImageGenerationPermissionMessage returns the stable end-user error text for disabled groups.
func ImageGenerationPermissionMessage() string {
	return imageGenerationPermissionMessage
}

// GroupAllowsImageGeneration preserves ungrouped-key behavior and enforces the flag when a group is present.
func GroupAllowsImageGeneration(group *Group) bool {
	return group == nil || group.AllowImageGeneration
}

// IsImageGenerationIntent classifies requests that can produce generated images.
func IsImageGenerationIntent(endpoint string, requestedModel string, body []byte) bool {
	if IsImageGenerationEndpoint(endpoint) {
		return true
	}
	if isOpenAIImageGenerationModel(requestedModel) {
		return true
	}
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return false
	}
	if model := strings.TrimSpace(gjson.GetBytes(body, "model").String()); isOpenAIImageGenerationModel(model) {
		return true
	}
	if openAIJSONToolsContainImageGeneration(gjson.GetBytes(body, "tools")) {
		return true
	}
	if openAIRequestBodyMayContainAdditionalImageTooling(body) &&
		openAIJSONInputContainsImageGenerationTooling(gjson.GetBytes(body, "input")) {
		return true
	}
	return openAIJSONToolChoiceSelectsImageGeneration(gjson.GetBytes(body, "tool_choice"))
}

// IsImageGenerationIntentMap is the map-backed variant used after service-side request mutation.
func IsImageGenerationIntentMap(endpoint string, requestedModel string, reqBody map[string]any) bool {
	if IsImageGenerationEndpoint(endpoint) {
		return true
	}
	if isOpenAIImageGenerationModel(requestedModel) {
		return true
	}
	if reqBody == nil {
		return false
	}
	if isOpenAIImageGenerationModel(firstNonEmptyString(reqBody["model"])) {
		return true
	}
	if hasOpenAIImageGenerationTool(reqBody) {
		return true
	}
	return openAIAnyToolChoiceSelectsImageGeneration(reqBody["tool_choice"])
}

// classifyOpenAIForwardImageIntent preserves Forward's raw view until an earlier mutation requires a map.
func classifyOpenAIForwardImageIntent(requestedModel string, upstreamModel string, body []byte, reqBody map[string]any) bool {
	if reqBody != nil {
		return IsImageGenerationIntentMap(openAIResponsesEndpoint, requestedModel, reqBody) || isOpenAIImageGenerationModel(upstreamModel)
	}
	return IsImageGenerationIntent(openAIResponsesEndpoint, requestedModel, body) || isOpenAIImageGenerationModel(upstreamModel)
}

// IsImageGenerationEndpoint identifies dedicated generated-image endpoints.
func IsImageGenerationEndpoint(endpoint string) bool {
	switch normalizeImageGenerationEndpoint(endpoint) {
	case "/v1/images/generations", "/v1/images/edits", "/images/generations", "/images/edits":
		return true
	default:
		return false
	}
}

func normalizeImageGenerationEndpoint(endpoint string) string {
	endpoint = strings.TrimSpace(strings.ToLower(endpoint))
	if endpoint == "" {
		return ""
	}
	endpoint = strings.TrimPrefix(endpoint, "https://api.openai.com")
	if idx := strings.IndexByte(endpoint, '?'); idx >= 0 {
		endpoint = endpoint[:idx]
	}
	return strings.TrimRight(endpoint, "/")
}

func openAIJSONToolsContainImageGeneration(tools gjson.Result) bool {
	if !tools.IsArray() {
		return false
	}
	found := false
	tools.ForEach(func(_, item gjson.Result) bool {
		if openAIJSONString(item.Get("type")) == "image_generation" || openAIJSONToolIsImageGenNamespace(item) {
			found = true
			return false
		}
		return true
	})
	return found
}

func openAIJSONToolIsImageGenNamespace(tool gjson.Result) bool {
	return openAIJSONString(tool.Get("type")) == "namespace" &&
		openAIJSONString(tool.Get("name")) == "image_gen"
}

func openAIJSONInputContainsImageGenerationTooling(input gjson.Result) bool {
	if !input.IsArray() {
		return false
	}
	found := false
	input.ForEach(func(_, item gjson.Result) bool {
		if openAIJSONString(item.Get("type")) != "additional_tools" {
			return true
		}
		tools := item.Get("tools")
		if !tools.IsArray() {
			return true
		}
		tools.ForEach(func(_, tool gjson.Result) bool {
			if openAIJSONString(tool.Get("type")) == "image_generation" || openAIJSONToolIsImageGenNamespace(tool) {
				found = true
				return false
			}
			return true
		})
		return !found
	})
	return found
}

// openAIRequestBodyMayContainAdditionalImageTooling 用 bytes.Contains（内部 SIMD 子串匹配）替换
// 逐字节 hasMarkerAt 扫描：命中条件不变——additional_tools 与任意 image 类标记都出现才返回 true。
func openAIRequestBodyMayContainAdditionalImageTooling(body []byte) bool {
	const (
		additionalToolsMarker = "additional_tools"
		imageGenerationMarker = "image_generation"
		imageGenMarker        = "image_gen"
		namespaceMarker       = "namespace"
	)

	if !bytes.Contains(body, []byte(additionalToolsMarker)) {
		return false
	}
	return bytes.Contains(body, []byte(imageGenerationMarker)) ||
		bytes.Contains(body, []byte(imageGenMarker)) ||
		bytes.Contains(body, []byte(namespaceMarker))
}

func hasMarkerAt(body []byte, offset int, marker string) bool {
	if offset+len(marker) > len(body) {
		return false
	}
	for i := 0; i < len(marker); i++ {
		if body[offset+i] != marker[i] {
			return false
		}
	}
	return true
}

func openAIRequestBodyHasImageGenerationTool(body []byte) bool {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return false
	}
	return openAIJSONToolsContainImageGeneration(gjson.GetBytes(body, "tools"))
}

func openAIRequestBodyHasImageGenerationTooling(body []byte) bool {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return false
	}
	return openAIJSONToolsContainImageGeneration(gjson.GetBytes(body, "tools")) ||
		(openAIRequestBodyMayContainAdditionalImageTooling(body) &&
			openAIJSONInputContainsImageGenerationTooling(gjson.GetBytes(body, "input"))) ||
		openAIJSONToolChoiceSelectsImageGeneration(gjson.GetBytes(body, "tool_choice"))
}

func openAIRequestBodyImageGenerationToolNeedsNormalization(body []byte) bool {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return false
	}
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return false
	}
	needsNormalization := false
	tools.ForEach(func(_, item gjson.Result) bool {
		if openAIJSONString(item.Get("type")) != "image_generation" {
			return true
		}
		// 只有旧字段需要迁移时才进入 map 修改，纯计费读取保持 raw 路径。
		if item.Get("format").Exists() || item.Get("compression").Exists() {
			needsNormalization = true
			return false
		}
		return true
	})
	return needsNormalization
}

func openAIJSONToolChoiceSelectsImageGeneration(choice gjson.Result) bool {
	if !choice.Exists() {
		return false
	}
	if choice.Type == gjson.String {
		return strings.TrimSpace(choice.String()) == "image_generation"
	}
	if !choice.IsObject() {
		return false
	}
	if strings.TrimSpace(choice.Get("type").String()) == "image_generation" {
		return true
	}
	if strings.TrimSpace(choice.Get("tool.type").String()) == "image_generation" {
		return true
	}
	if strings.TrimSpace(choice.Get("function.name").String()) == "image_generation" {
		return true
	}
	return false
}

func openAIAnyToolChoiceSelectsImageGeneration(choice any) bool {
	switch v := choice.(type) {
	case string:
		return strings.TrimSpace(v) == "image_generation"
	case map[string]any:
		if strings.TrimSpace(firstNonEmptyString(v["type"])) == "image_generation" {
			return true
		}
		if tool, ok := v["tool"].(map[string]any); ok && strings.TrimSpace(firstNonEmptyString(tool["type"])) == "image_generation" {
			return true
		}
		if fn, ok := v["function"].(map[string]any); ok && strings.TrimSpace(firstNonEmptyString(fn["name"])) == "image_generation" {
			return true
		}
	}
	return false
}

func getAPIKeyFromContext(c interface{ Get(string) (any, bool) }) *APIKey {
	if c == nil {
		return nil
	}
	v, exists := c.Get("api_key")
	if !exists {
		return nil
	}
	apiKey, _ := v.(*APIKey)
	return apiKey
}

func apiKeyGroup(apiKey *APIKey) *Group {
	if apiKey == nil {
		return nil
	}
	return apiKey.Group
}

type OpenAIResponsesImageBillingConfig struct {
	Model     string
	SizeTier  string
	InputSize string
}

func resolveOpenAIResponsesImageBillingConfigDetailed(reqBody map[string]any, fallbackModel string) (OpenAIResponsesImageBillingConfig, error) {
	imageModel := ""
	imageSize := ""
	hasImageTool := false
	if reqBody != nil {
		if toolMap, ok := firstOpenAIImageGenerationToolMap(reqBody); ok {
			hasImageTool = true
			imageModel = strings.TrimSpace(firstNonEmptyString(toolMap["model"]))
			imageSize = strings.TrimSpace(firstNonEmptyString(toolMap["size"]))
		}
		if imageSize == "" {
			imageSize = strings.TrimSpace(firstNonEmptyString(reqBody["size"]))
		}
	}
	if imageModel == "" && reqBody != nil {
		bodyModel := strings.TrimSpace(firstNonEmptyString(reqBody["model"]))
		if isOpenAIImageBillingModelAlias(bodyModel) || !hasImageTool {
			imageModel = bodyModel
		}
	}
	if imageModel == "" && hasImageTool {
		imageModel = "gpt-image-2"
	}
	if imageModel == "" {
		imageModel = strings.TrimSpace(fallbackModel)
	}
	sizeTier := normalizeOpenAIImageSizeTier(imageSize)
	return OpenAIResponsesImageBillingConfig{
		Model:     imageModel,
		SizeTier:  sizeTier,
		InputSize: imageSize,
	}, nil
}

func resolveOpenAIResponsesImageBillingConfigFromBody(body []byte, fallbackModel string) (string, string, error) {
	cfg, err := resolveOpenAIResponsesImageBillingConfigDetailedFromBody(body, fallbackModel)
	if err != nil {
		return "", "", err
	}
	return cfg.Model, cfg.SizeTier, nil
}

func resolveOpenAIResponsesImageBillingConfigDetailedFromBody(body []byte, fallbackModel string) (OpenAIResponsesImageBillingConfig, error) {
	imageModel := ""
	imageSize := ""
	hasImageTool := false
	if len(body) > 0 && gjson.ValidBytes(body) {
		if tool, ok := firstOpenAIJSONImageGenerationTool(body); ok {
			hasImageTool = true
			imageModel = openAIJSONString(tool.Get("model"))
			imageSize = openAIJSONString(tool.Get("size"))
		}
		if imageSize == "" {
			imageSize = openAIJSONString(gjson.GetBytes(body, "size"))
		}
		if imageModel == "" {
			bodyModel := openAIJSONString(gjson.GetBytes(body, "model"))
			if isOpenAIImageBillingModelAlias(bodyModel) || !hasImageTool {
				imageModel = bodyModel
			}
		}
	}
	if imageModel == "" && hasImageTool {
		imageModel = "gpt-image-2"
	}
	if imageModel == "" {
		imageModel = strings.TrimSpace(fallbackModel)
	}
	return OpenAIResponsesImageBillingConfig{
		Model:     imageModel,
		SizeTier:  normalizeOpenAIImageSizeTier(imageSize),
		InputSize: imageSize,
	}, nil
}

func firstOpenAIJSONImageGenerationTool(body []byte) (gjson.Result, bool) {
	if tool, ok := firstOpenAIJSONImageGenerationToolInTools(gjson.GetBytes(body, "tools")); ok {
		return tool, true
	}
	if !openAIRequestBodyMayContainAdditionalImageTooling(body) {
		return gjson.Result{}, false
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return gjson.Result{}, false
	}
	var found gjson.Result
	ok := false
	input.ForEach(func(_, item gjson.Result) bool {
		if openAIJSONString(item.Get("type")) != "additional_tools" {
			return true
		}
		if tool, toolOK := firstOpenAIJSONImageGenerationToolInTools(item.Get("tools")); toolOK {
			found = tool
			ok = true
			return false
		}
		return true
	})
	return found, ok
}

func firstOpenAIJSONImageGenerationToolInTools(tools gjson.Result) (gjson.Result, bool) {
	if !tools.IsArray() {
		return gjson.Result{}, false
	}
	var found gjson.Result
	ok := false
	tools.ForEach(func(_, item gjson.Result) bool {
		if openAIJSONString(item.Get("type")) != "image_generation" && !openAIJSONToolIsImageGenNamespace(item) {
			return true
		}
		found = item
		ok = true
		return false
	})
	return found, ok
}

func firstOpenAIImageGenerationToolMap(reqBody map[string]any) (map[string]any, bool) {
	if tool, ok := firstOpenAIImageGenerationToolMapInTools(reqBody["tools"]); ok {
		return tool, true
	}
	input, ok := reqBody["input"].([]any)
	if !ok {
		return nil, false
	}
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok || strings.TrimSpace(firstNonEmptyString(item["type"])) != "additional_tools" {
			continue
		}
		if tool, ok := firstOpenAIImageGenerationToolMapInTools(item["tools"]); ok {
			return tool, true
		}
	}
	return nil, false
}

func firstOpenAIImageGenerationToolMapInTools(rawTools any) (map[string]any, bool) {
	tools, ok := rawTools.([]any)
	if !ok {
		return nil, false
	}
	for _, rawTool := range tools {
		toolMap, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		if strings.TrimSpace(firstNonEmptyString(toolMap["type"])) != "image_generation" &&
			!openAIAnyToolIsImageGenNamespace(toolMap) {
			continue
		}
		return toolMap, true
	}
	return nil, false
}

func isOpenAIImageBillingModelAlias(model string) bool {
	normalized := strings.ToLower(strings.TrimSpace(model))
	if normalized == "" {
		return false
	}
	return isOpenAIImageGenerationModel(normalized) || strings.Contains(normalized, "image")
}

func openAIJSONString(value gjson.Result) string {
	if value.Type != gjson.String {
		return ""
	}
	return strings.TrimSpace(value.String())
}
