package apicompat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// ResponsesNamespaceName shares the bridge's namespace identity and flattening contract.
type ResponsesNamespaceName = NamespacedToolName

// FlattenResponsesNamespaces flattens supported namespace declarations and
// rewrites matching direct input calls and tool choice selections.
func FlattenResponsesNamespaces(req map[string]any) (map[string]ResponsesNamespaceName, bool, error) {
	return FlattenResponsesNamespacesExcept(req, nil)
}

// FlattenResponsesNamespacesExcept leaves service-owned namespaces untouched.
func FlattenResponsesNamespacesExcept(req map[string]any, preserved map[string]bool) (map[string]ResponsesNamespaceName, bool, error) {
	if req == nil {
		return nil, false, nil
	}
	tools, ok := req["tools"].([]any)
	if !ok || len(tools) == 0 {
		return nil, false, nil
	}

	topLevel := make(map[string]bool)
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typ := strings.TrimSpace(responsesNamespaceString(tool["type"]))
		name := strings.TrimSpace(responsesNamespaceString(tool["name"]))
		if (typ == "function" || typ == "custom") && name != "" {
			topLevel[name] = true
		}
	}

	names := make(map[string]ResponsesNamespaceName)
	definitions := make(map[string][]byte)
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok || strings.TrimSpace(responsesNamespaceString(tool["type"])) != "namespace" {
			continue
		}
		namespace := strings.TrimSpace(responsesNamespaceString(tool["name"]))
		if namespace == "" || preserved[namespace] {
			continue
		}
		for _, rawChild := range responsesNamespaceChildren(tool) {
			child, ok := rawChild.(map[string]any)
			if !ok || strings.TrimSpace(responsesNamespaceString(child["type"])) != "function" {
				continue
			}
			name := strings.TrimSpace(responsesNamespaceString(child["name"]))
			if name == "" {
				continue
			}
			flat := flattenNamespaceToolName(namespace, name)
			entry := ResponsesNamespaceName{Namespace: namespace, Name: name}
			definition, err := marshalResponsesNamespaceToolDefinition(child)
			if err != nil {
				return nil, false, fmt.Errorf("encode namespace tool definition: %w", err)
			}
			if topLevel[flat] {
				return nil, false, fmt.Errorf("namespace tool has a flattened-name conflict with a top-level tool")
			}
			if previous, exists := names[flat]; exists {
				if previous == entry && bytes.Equal(definitions[flat], definition) {
					continue
				}
				return nil, false, fmt.Errorf("namespace tools have a flattened-name conflict")
			}
			names[flat] = entry
			definitions[flat] = definition
		}
	}
	if len(names) == 0 {
		return nil, false, nil
	}

	flattened := make([]any, 0, len(tools)+len(names))
	seen := make(map[string]bool)
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok || strings.TrimSpace(responsesNamespaceString(tool["type"])) != "namespace" {
			flattened = append(flattened, raw)
			continue
		}
		namespace := strings.TrimSpace(responsesNamespaceString(tool["name"]))
		if namespace == "" || preserved[namespace] {
			flattened = append(flattened, raw)
			continue
		}
		for _, rawChild := range responsesNamespaceChildren(tool) {
			child, ok := rawChild.(map[string]any)
			if !ok || strings.TrimSpace(responsesNamespaceString(child["type"])) != "function" {
				continue
			}
			name := strings.TrimSpace(responsesNamespaceString(child["name"]))
			flat := flattenNamespaceToolName(namespace, name)
			if name == "" || seen[flat] {
				continue
			}
			seen[flat] = true
			flatChild := make(map[string]any, len(child))
			for key, value := range child {
				flatChild[key] = value
			}
			flatChild["name"] = flat
			flattened = append(flattened, flatChild)
		}
	}
	req["tools"] = flattened
	rewriteResponsesNamespaceInputCalls(req["input"], names)
	if choice, ok := req["tool_choice"].(map[string]any); ok {
		rewriteResponsesNamespaceToolChoice(choice, names)
	}
	return names, true, nil
}

// RestoreResponsesNamespaceCalls restores verified function-call items in a
// Responses payload without modifying message, metadata, or result namespaces.
func RestoreResponsesNamespaceCalls(payload []byte, names map[string]ResponsesNamespaceName) ([]byte, bool, error) {
	if len(payload) == 0 || len(names) == 0 {
		return payload, false, nil
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return payload, false, err
	}
	// SSE data is normally an object. A valid non-object payload is unrelated
	// to Responses call identity and must pass through rather than aborting a
	// stream before its first client-visible event.
	root, ok := value.(map[string]any)
	if !ok {
		return payload, false, nil
	}
	changed := restoreResponsesNamespacePayloadRoot(root, names)
	if !changed {
		return payload, false, nil
	}
	var rebuilt bytes.Buffer
	encoder := json.NewEncoder(&rebuilt)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(root); err != nil {
		return payload, false, err
	}
	return bytes.TrimSuffix(rebuilt.Bytes(), []byte("\n")), true, nil
}

func responsesNamespaceChildren(tool map[string]any) []any {
	if children, ok := tool["tools"].([]any); ok && len(children) > 0 {
		return children
	}
	children, _ := tool["children"].([]any)
	return children
}

func rewriteResponsesNamespaceInputCalls(input any, names map[string]ResponsesNamespaceName) {
	items, ok := input.([]any)
	if !ok {
		return
	}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok || !responsesNamespaceInputCallType(responsesNamespaceString(item["type"])) {
			continue
		}
		rewriteResponsesNamespaceQualifiedCall(item, names)
	}
}

func rewriteResponsesNamespaceToolChoice(choice map[string]any, names map[string]ResponsesNamespaceName) bool {
	choiceType := strings.TrimSpace(responsesNamespaceString(choice["type"]))
	if choiceType != "function" && choiceType != "namespace" {
		return false
	}

	namespace := strings.TrimSpace(responsesNamespaceString(choice["namespace"]))
	name := strings.TrimSpace(responsesNamespaceString(choice["name"]))
	function, _ := choice["function"].(map[string]any)
	functionName := strings.TrimSpace(responsesNamespaceString(function["name"]))
	if name == "" {
		name = functionName
	}
	if choiceType == "namespace" && namespace == "" {
		namespace = strings.TrimSpace(responsesNamespaceString(choice["name"]))
		name = functionName
	}
	if namespace == "" || name == "" {
		return false
	}

	flat := flattenNamespaceToolName(namespace, name)
	entry, ok := names[flat]
	if !ok || entry.Namespace != namespace || entry.Name != name {
		return false
	}
	choice["type"] = "function"
	choice["name"] = flat
	delete(choice, "function")
	delete(choice, "namespace")
	return true
}

func rewriteResponsesNamespaceQualifiedCall(item map[string]any, names map[string]ResponsesNamespaceName) bool {
	namespace := strings.TrimSpace(responsesNamespaceString(item["namespace"]))
	name := strings.TrimSpace(responsesNamespaceString(item["name"]))
	if namespace == "" || name == "" {
		return false
	}
	flat := flattenNamespaceToolName(namespace, name)
	entry, ok := names[flat]
	if !ok || entry.Namespace != namespace || entry.Name != name {
		return false
	}
	item["name"] = flat
	delete(item, "namespace")
	return true
}

func restoreResponsesNamespacePayloadRoot(root map[string]any, names map[string]ResponsesNamespaceName) bool {
	changed := restoreResponsesNamespaceCallItem(root, names)
	if item, ok := root["item"].(map[string]any); ok {
		changed = restoreResponsesNamespaceCallItem(item, names) || changed
	}
	if response, ok := root["response"].(map[string]any); ok {
		changed = restoreResponsesNamespaceOutput(response["output"], names) || changed
	}
	changed = restoreResponsesNamespaceOutput(root["output"], names) || changed
	return changed
}

func restoreResponsesNamespaceOutput(value any, names map[string]ResponsesNamespaceName) bool {
	items, ok := value.([]any)
	if !ok {
		return false
	}
	changed := false
	for _, raw := range items {
		if item, ok := raw.(map[string]any); ok {
			changed = restoreResponsesNamespaceCallItem(item, names) || changed
		}
	}
	return changed
}

func restoreResponsesNamespaceCallItem(item map[string]any, names map[string]ResponsesNamespaceName) bool {
	if item == nil || !responsesNamespaceRestorableCallType(responsesNamespaceString(item["type"])) {
		return false
	}
	entry, ok := names[strings.TrimSpace(responsesNamespaceString(item["name"]))]
	if !ok {
		return false
	}
	item["name"] = entry.Name
	item["namespace"] = entry.Namespace
	return true
}

func responsesNamespaceInputCallType(itemType string) bool {
	switch strings.TrimSpace(itemType) {
	case "function_call", "tool_call", "custom_tool_call", "mcp_tool_call", "local_shell_call", "tool_search_call":
		return true
	default:
		return false
	}
}

func responsesNamespaceRestorableCallType(itemType string) bool {
	switch strings.TrimSpace(itemType) {
	case "function_call", "tool_call", "custom_tool_call", "mcp_tool_call", "local_shell_call", "tool_search_call":
		return true
	default:
		return false
	}
}

func marshalResponsesNamespaceToolDefinition(tool map[string]any) ([]byte, error) {
	return json.Marshal(tool)
}

func responsesNamespaceString(value any) string {
	text, _ := value.(string)
	return text
}
