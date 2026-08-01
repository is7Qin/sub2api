package service

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestTrimOpenAIEncryptedReasoningItems_Compaction(t *testing.T) {
	tests := []struct {
		name      string
		itemType  string
		encrypted bool
		changed   bool
	}{
		{name: "encrypted compaction", itemType: "compaction", encrypted: true, changed: true},
		{name: "encrypted compaction summary", itemType: "compaction_summary", encrypted: true, changed: true},
		{name: "unencrypted compaction", itemType: "compaction", encrypted: false, changed: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := map[string]any{"type": tt.itemType, "id": "cmp_stale"}
			if tt.encrypted {
				item["encrypted_content"] = "gAAA"
			}
			reqBody := map[string]any{"input": []any{
				item,
				map[string]any{"type": "message", "content": "hi"},
			}}

			changed := trimOpenAIEncryptedReasoningItems(reqBody)
			require.Equal(t, tt.changed, changed)
			input, ok := reqBody["input"].([]any)
			require.True(t, ok)
			if tt.changed {
				require.Len(t, input, 1)
				require.Equal(t, "message", input[0].(map[string]any)["type"])
				return
			}
			require.Len(t, input, 2)
			require.Equal(t, tt.itemType, input[0].(map[string]any)["type"])
		})
	}
}

func TestSanitizeOpenAICrossModeFailoverReasoning(t *testing.T) {
	t.Run("removes only encrypted reasoning array items", func(t *testing.T) {
		body := []byte(`{"model":"gpt-5","input":[{"type":"reasoning","encrypted_content":null,"summary":[{"type":"summary_text","text":"secret"}]},{"type":"message","role":"user","content":"hello <world>"},{"type":"compaction","encrypted_content":"keep-compaction"},{"type":"reasoning","summary":"keep-reasoning"},{"type":"function_call","name":"lookup","arguments":"{}"}],"metadata":{"sequence":90071992547409931234567890}}`)
		original := append([]byte(nil), body...)

		sanitized, changed, err := SanitizeOpenAICrossModeFailoverReasoning(body)

		require.NoError(t, err)
		require.True(t, changed)
		require.Equal(t, original, body, "canonical body must remain immutable")
		require.Len(t, gjson.GetBytes(sanitized, "input").Array(), 4)
		require.Equal(t, "message", gjson.GetBytes(sanitized, "input.0.type").String())
		require.Equal(t, "hello <world>", gjson.GetBytes(sanitized, "input.0.content").String())
		require.Contains(t, string(sanitized), `<world>`, "changed bodies use upstream JSON encoding")
		require.Equal(t, "compaction", gjson.GetBytes(sanitized, "input.1.type").String())
		require.Equal(t, "reasoning", gjson.GetBytes(sanitized, "input.2.type").String())
		require.Equal(t, "function_call", gjson.GetBytes(sanitized, "input.3.type").String())
		require.Equal(t, "90071992547409931234567890", gjson.GetBytes(sanitized, "metadata.sequence").Raw)
	})

	t.Run("removes a matching single object input", func(t *testing.T) {
		body := []byte(`{"model":"gpt-5","input":{"type":"reasoning","encrypted_content":"foreign","summary":"secret"},"tools":[{"type":"function","name":"lookup"}]}`)

		sanitized, changed, err := SanitizeOpenAICrossModeFailoverReasoning(body)

		require.NoError(t, err)
		require.True(t, changed)
		require.False(t, gjson.GetBytes(sanitized, "input").Exists())
		require.Equal(t, "lookup", gjson.GetBytes(sanitized, "tools.0.name").String())
	})

	t.Run("leaves nonmatching bodies byte exact", func(t *testing.T) {
		for _, body := range [][]byte{
			[]byte(` { "model" : "gpt-5" , "input" : [ { "type" : "reasoning", "summary" : "keep" }, { "type" : "compaction", "encrypted_content" : null } ] } `),
			[]byte(`{"model":"gpt-5","input":{"type":"message","content":"hello"}}`),
			[]byte(`{"model":"gpt-5","input":"hello"}`),
			[]byte(`{"model":"gpt-5","metadata":{"sequence":90071992547409931234567890}}`),
			[]byte(`{"model":"gpt-5","input":[{"type":"reasoning ","encrypted_content":"keep"}]}`),
		} {
			sanitized, changed, err := SanitizeOpenAICrossModeFailoverReasoning(body)
			require.NoError(t, err)
			require.False(t, changed)
			require.Equal(t, body, sanitized)
		}
	})

	t.Run("returns an error without mutation for malformed JSON", func(t *testing.T) {
		body := []byte(`{"model":"gpt-5","input":[{"type":"reasoning","encrypted_content":"foreign"}]`)

		sanitized, changed, err := SanitizeOpenAICrossModeFailoverReasoning(body)

		require.Error(t, err)
		require.False(t, changed)
		require.True(t, bytes.Equal(body, sanitized))
	})
}
