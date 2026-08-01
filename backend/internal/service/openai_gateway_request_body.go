package service

// SanitizeOpenAICrossModeFailoverReasoning removes top-level encrypted reasoning
// items for a later non-passthrough Responses attempt without mutating the canonical body.
func SanitizeOpenAICrossModeFailoverReasoning(body []byte) ([]byte, bool, error) {
	decoded, err := decodeOpenAIRequestBodyMapUseNumber(body)
	if err != nil {
		return body, false, err
	}
	inputValue, has := decoded["input"]
	if !has {
		return body, false, nil
	}

	var filtered []any
	changed := false
	switch input := inputValue.(type) {
	case []any:
		filtered = make([]any, 0, len(input))
		for _, item := range input {
			if isOpenAIEncryptedReasoningInputItem(item) {
				changed = true
				continue
			}
			filtered = append(filtered, item)
		}
		if changed {
			decoded["input"] = filtered
		}
	case map[string]any:
		if isOpenAIEncryptedReasoningInputItem(input) {
			delete(decoded, "input")
			changed = true
		}
	default:
		return body, false, nil
	}
	if !changed {
		return body, false, nil
	}
	sanitized, err := marshalOpenAIUpstreamJSON(decoded)
	if err != nil {
		return body, false, err
	}
	return sanitized, true, nil
}

func isOpenAIEncryptedReasoningInputItem(item any) bool {
	inputItem, ok := item.(map[string]any)
	if !ok {
		return false
	}
	itemType, ok := inputItem["type"].(string)
	if !ok || itemType != "reasoning" {
		return false
	}
	_, hasEncryptedContent := inputItem["encrypted_content"]
	return hasEncryptedContent
}
