package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const openAIResponsesNamespaceNamesContextKey = "openai_responses_namespace_names"

func shouldNormalizeOpenAIResponsesNamespaces(account *Account, transport OpenAIUpstreamTransport, clientTransport OpenAIClientTransport, compactPath bool) bool {
	if account == nil || !account.IsOpenAIOAuth() || clientTransport != OpenAIClientTransportHTTP || transport != OpenAIUpstreamTransportHTTPSSE {
		return false
	}
	return compactPath || account.IsOpenAIResponsesFlattenNamespacesEnabled()
}

// shouldStripOpenAIResponsesInputNamespaces is the terminal counterpart to
// request normalization. It is deliberately narrowed to the same native OAuth
// HTTP path so setup-token and WebSocket payloads keep their native contract.
func shouldStripOpenAIResponsesInputNamespaces(c *gin.Context, account *Account) bool {
	if c == nil {
		return false
	}
	transport, _ := c.Get("openai_ws_transport_decision")
	return shouldNormalizeOpenAIResponsesNamespaces(
		account,
		OpenAIUpstreamTransport(strings.TrimSpace(fmt.Sprint(transport))),
		GetOpenAIClientTransport(c),
		isOpenAIResponsesCompactPath(c),
	)
}

func normalizeOpenAIResponsesNamespaces(c *gin.Context, requestBody map[string]any) (bool, error) {
	if requestBody == nil {
		return false, nil
	}
	// image_gen declarations are removed by the established Codex transform;
	// flattening them here would instead expose invalid client-owned functions.
	names, changed, err := apicompat.FlattenResponsesNamespacesExcept(requestBody, map[string]bool{"image_gen": true})
	if err != nil {
		return false, err
	}
	if changed {
		setOpenAIResponsesNamespaceNames(c, names)
	}
	return changed, nil
}

// stripOpenAIResponsesInputNamespaces removes namespace only from direct input
// call items accepted by the Codex input adapter. Raw item JSON is retained so
// unrelated metadata and number spelling do not pass through float64.
func stripOpenAIResponsesInputNamespaces(body []byte) ([]byte, error) {
	if !bytes.Contains(body, []byte(`"namespace"`)) {
		return body, nil
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, nil
	}

	var rebuilt bytes.Buffer
	rebuilt.Grow(len(input.Raw))
	_ = rebuilt.WriteByte('[')
	changed := false
	first := true
	var stripErr error
	input.ForEach(func(_, item gjson.Result) bool {
		if !first {
			_ = rebuilt.WriteByte(',')
		}
		first = false
		itemBody := []byte(item.Raw)
		if item.IsObject() && item.Get("namespace").Exists() &&
			isCodexToolCallInputType(strings.TrimSpace(item.Get("type").String())) {
			itemBody, stripErr = sjson.DeleteBytes(itemBody, "namespace")
			if stripErr != nil {
				return false
			}
			changed = true
		}
		_, _ = rebuilt.Write(itemBody)
		return true
	})
	if stripErr != nil {
		return body, fmt.Errorf("delete OpenAI input namespace: %w", stripErr)
	}
	if !changed {
		return body, nil
	}
	_ = rebuilt.WriteByte(']')
	stripped, err := sjson.SetRawBytes(body, "input", rebuilt.Bytes())
	if err != nil {
		return body, fmt.Errorf("replace OpenAI input after namespace deletion: %w", err)
	}
	return stripped, nil
}

func setOpenAIResponsesNamespaceNames(c *gin.Context, names map[string]apicompat.ResponsesNamespaceName) {
	if c != nil && len(names) > 0 {
		c.Set(openAIResponsesNamespaceNamesContextKey, names)
	}
}

func clearOpenAIResponsesNamespaceNames(c *gin.Context) {
	if c != nil {
		// One Gin context can span sequential account failover attempts. Clear the
		// prior attempt's mapping before account-specific request normalization.
		c.Set(openAIResponsesNamespaceNamesContextKey, nil)
	}
}

func openAIResponsesNamespaceNames(c *gin.Context) map[string]apicompat.ResponsesNamespaceName {
	if c == nil {
		return nil
	}
	value, ok := c.Get(openAIResponsesNamespaceNamesContextKey)
	if !ok {
		return nil
	}
	names, _ := value.(map[string]apicompat.ResponsesNamespaceName)
	return names
}

func restoreOpenAIResponsesNamespacePayload(c *gin.Context, payload []byte) ([]byte, error) {
	names := openAIResponsesNamespaceNames(c)
	if len(names) == 0 || !bytes.Contains(payload, []byte(`"name"`)) || !json.Valid(payload) {
		return payload, nil
	}
	restored, changed, err := apicompat.RestoreResponsesNamespaceCalls(payload, names)
	if err != nil {
		return payload, err
	}
	if changed {
		return restored, nil
	}
	return payload, nil
}
