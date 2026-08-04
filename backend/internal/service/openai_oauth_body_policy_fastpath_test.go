package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// referenceApplyOpenAIOAuthHTTPAllowlist 是旧实现（gjson 收集 + sjson 逐字段重写）
// 的逐字节参考，用于差分测试证明新实现输出完全一致。
func referenceApplyOpenAIOAuthHTTPAllowlist(body []byte) ([]byte, bool, error) {
	if len(body) == 0 {
		return body, false, nil
	}
	if !json.Valid(body) {
		return body, false, fmt.Errorf("apply openai oauth http allowlist: invalid JSON")
	}

	normalized, err := referenceCopyOpenAIOAuthAllowedRawFields(body, openAIOAuthHTTPAllowlistFields, "http")
	if err != nil {
		return body, false, err
	}

	if store := gjson.GetBytes(normalized, "store"); !store.Exists() || store.Type != gjson.False {
		next, err := sjson.SetBytes(normalized, "store", false)
		if err != nil {
			return body, false, fmt.Errorf("apply openai oauth http allowlist store=false: %w", err)
		}
		normalized = next
	}
	if stream := gjson.GetBytes(normalized, "stream"); !stream.Exists() || stream.Type != gjson.True {
		next, err := sjson.SetBytes(normalized, "stream", true)
		if err != nil {
			return body, false, fmt.Errorf("apply openai oauth http allowlist stream=true: %w", err)
		}
		normalized = next
	}

	return changedOpenAIOAuthBody(body, normalized)
}

func referenceApplyOpenAIOAuthCompactAllowlist(body []byte) ([]byte, bool, error) {
	if len(body) == 0 {
		return body, false, nil
	}
	if !json.Valid(body) {
		return body, false, fmt.Errorf("apply openai oauth compact allowlist: invalid JSON")
	}

	normalized, err := referenceCopyOpenAIOAuthAllowedRawFields(body, openAIOAuthCompactAllowlistFields, "compact")
	if err != nil {
		return body, false, err
	}
	return changedOpenAIOAuthBody(body, normalized)
}

func referenceCopyOpenAIOAuthAllowedRawFields(body []byte, fields []string, policy string) ([]byte, error) {
	normalized := []byte(`{}`)
	for _, field := range fields {
		value := gjson.GetBytes(body, field)
		if !value.Exists() {
			continue
		}
		next, err := sjson.SetRawBytes(normalized, field, []byte(value.Raw))
		if err != nil {
			return nil, fmt.Errorf("apply openai oauth %s allowlist %s: %w", policy, field, err)
		}
		normalized = next
	}
	return normalized, nil
}

// TestOpenAIOAuthAllowlist_DifferentialAgainstReference 证明新实现与旧 sjson 链
// 对同一 body 输出逐字节一致（含 changed 标志与错误行为）。
func TestOpenAIOAuthAllowlist_DifferentialAgainstReference(t *testing.T) {
	bodies := []string{
		// 已是收敛形态（http 需 store=false/stream=true）
		`{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":"hello"}],"instructions":"be helpful","tools":[{"type":"function","name":"search"}],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":{"effort":"medium"},"store":false,"stream":true,"include":["reasoning.encrypted_content"],"service_tier":"priority","prompt_cache_key":"cache_abc","text":{"verbosity":"low"},"client_metadata":{"x-codex-installation-id":"inst-123"}}`,
		`{"model":"x","input":[],"store":false,"stream":true}`,
		`{"model":"x","store":false,"stream":true}`,
		`{"model":"x","store":false,"stream":true}` + "\n",
		`{"model":"x","input":{"a":{"b":[1,{"c":"d"}]}},"store":false,"stream":true}`,
		`{"model":"x","input":[{"a":"}"},{"b":"{","c":"\"","d":"\\"}],"store":false,"stream":true}`,
		`{"model":"x","input":"你好","store":false,"stream":true}`,
		`{"model":"x","input":9007199254740993,"store":false,"stream":true}`,
		`{"model":"x","input":1.50e+3,"store":false,"stream":true}`,
		`{"model":"x","input":-0.5,"store":false,"stream":true}`,
		`{"model":"x","input":null,"store":false,"stream":true}`,
		`{"model":true,"input":false,"store":false,"stream":true}`,
		// 需要重写：多余字段 / 缺 store/stream / 值不正确
		`{"model":"gpt-5.4","max_output_tokens":1024,"temperature":0.5,"metadata":{"drop":true},"user":"drop","stream_options":{"include_usage":true},"unknown_field":"drop"}`,
		`{"model":"x","input":"hello"}`,
		`{"model":"x","input":"hello","store":true,"stream":true}`,
		`{"model":"x","input":"hello","store":"false","stream":"true"}`,
		`{"model":"x","input":"hello","store":0,"stream":1}`,
		`{"model":"x","input":"hello","stream":true}`,
		`{"model":"x","input":"hello","store":false}`,
		`{"model":"x","store":false,"stream":true,"input":"y"}`,
		`{"store":false,"model":"x","stream":true}`,
		`{"model":"x","bad":1,"store":false,"stream":true}`,
		`{"bad":1}`,
		`{}`,
		`{"model":"a","model":"b","store":false,"stream":true}`,
		`{"input":[1],"input":[2],"store":false,"stream":true}`,
		// 空白布局（重写输出仍为紧凑形态）
		"{ \"model\" : \"x\" , \"input\" : \"hello\" }",
		"{\n\t\"model\": \"x\",\n\t\"store\": false,\n\t\"stream\": true\n}",
		`{"model":"x","store":false,"stream":true }`,
		`{"model":"x","store": false,"stream":true}`,
		`{"model":"x","input": [1, 2],"store":false,"stream":true}`,
		` {"model":"x","store":false,"stream":true} `,
		// 键名带转义（解码后为 model，gjson 按解码键名匹配）
		`{"` + `\` + `u006dodel":"x","store":false,"stream":true}`,
		`{"` + `\` + `u006dodel":"x","input":"y"}`,
		// 顶层非对象
		`[]`,
		`[{"model":"x"}]`,
		`null`,
		`"str"`,
		// 非法 JSON（应走错误分支且行为一致）
		`{"model":"gpt-5.4","input":"hello"} trailing`,
		`{"model":"gpt-5.4","input":"hello"`,
		`{"model":01,"store":false,"stream":true}`,
		`{"model":"x",}`,
		``,
		`   `,
		// compact 语义差异：store/stream 被丢弃
		`{"model":"gpt-5.4","input":"hello","user":"user_123","metadata":{"user_id":"user_123"},"stream":true,"store":true}`,
		`{"text":{"trace":9007199254740995},"model":"gpt-5.4"}`,
		`{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":"compact me"}],"instructions":"compact-test","tools":[],"parallel_tool_calls":true,"reasoning":{"effort":"high"},"service_tier":"priority","prompt_cache_key":"cache_abc","text":{"trace":9007199254740995}}`,
	}

	for _, policy := range []string{"http", "compact"} {
		for i, raw := range bodies {
			t.Run(fmt.Sprintf("%s/%d", policy, i), func(t *testing.T) {
				body := []byte(raw)
				var want []byte
				var wantChanged bool
				var wantErr error
				if policy == "http" {
					want, wantChanged, wantErr = referenceApplyOpenAIOAuthHTTPAllowlist(body)
				} else {
					want, wantChanged, wantErr = referenceApplyOpenAIOAuthCompactAllowlist(body)
				}

				var got []byte
				var gotChanged bool
				var gotErr error
				if policy == "http" {
					got, gotChanged, gotErr = applyOpenAIOAuthHTTPAllowlist(body)
				} else {
					got, gotChanged, gotErr = applyOpenAIOAuthCompactAllowlist(body)
				}

				require.Equal(t, wantErr == nil, gotErr == nil, "error presence mismatch: want=%v got=%v", wantErr, gotErr)
				if wantErr != nil {
					require.EqualError(t, gotErr, wantErr.Error())
					return
				}
				require.Equal(t, wantChanged, gotChanged)
				require.Equal(t, string(want), string(got), "new implementation must be byte-identical to the sjson reference")
			})
		}
	}
}

// TestOpenAIOAuthAllowlist_FastPathCanonicalBodyPassesThrough 验证已收敛的 body
// 原样透传，且快速路径近乎零分配（旧实现每次约 30+ 次 body 级分配）。
func TestOpenAIOAuthAllowlist_FastPathCanonicalBodyPassesThrough(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":"hello"}],"instructions":"be helpful","tools":[{"type":"function","name":"search"}],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":{"effort":"medium"},"store":false,"stream":true,"include":["reasoning.encrypted_content"],"service_tier":"priority","prompt_cache_key":"cache_abc","text":{"verbosity":"low"},"client_metadata":{"x-codex-installation-id":"inst-123"}}`)

	normalized, changed, err := applyOpenAIOAuthHTTPAllowlist(body)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, body, normalized)

	allocs := testing.AllocsPerRun(200, func() {
		_, _, _ = applyOpenAIOAuthHTTPAllowlist(body)
	})
	require.Less(t, allocs, float64(5), "canonical fast path should allocate ~0, old path allocates 30+")
}

func TestOpenAIOAuthCompactAllowlist_FastPathCanonicalBodyPassesThrough(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":"hi","instructions":"compact","tools":[],"parallel_tool_calls":true,"reasoning":{"effort":"high"},"service_tier":"priority","prompt_cache_key":"k","text":{"verbosity":"low"}}`)

	normalized, changed, err := applyOpenAIOAuthCompactAllowlist(body)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, body, normalized)

	// 空对象在 compact 策略下同样命中快速路径（http 策略则需补 store/stream）
	empty := []byte(`{}`)
	normalized, changed, err = applyOpenAIOAuthCompactAllowlist(empty)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, empty, normalized)

	// 子集字段、键序一致也命中快速路径
	subset := []byte(`{"model":"x","input":"hi"}`)
	normalized, changed, err = applyOpenAIOAuthCompactAllowlist(subset)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, subset, normalized)
}

// TestOpenAIOAuthAllowlist_FastPathDoesNotFireWhenRewriteNeeded 覆盖快速路径
// 不能触发的关键边界：任一条件不满足都必须走重写。
func TestOpenAIOAuthAllowlist_FastPathDoesNotFireWhenRewriteNeeded(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantModel  string
		wantStore  string // 期望的 store raw；"-" 表示不存在
		wantStream string
	}{
		{name: "store true forced false", body: `{"model":"x","store":true,"stream":true}`, wantModel: "x", wantStore: "false", wantStream: "true"},
		{name: "stream false forced true", body: `{"model":"x","store":false,"stream":false}`, wantModel: "x", wantStore: "false", wantStream: "true"},
		{name: "store missing appended", body: `{"model":"x","input":"hello"}`, wantModel: "x", wantStore: "false", wantStream: "true"},
		{name: "extra field dropped", body: `{"model":"x","temperature":0.5,"store":false,"stream":true}`, wantModel: "x", wantStore: "false", wantStream: "true"},
		{name: "reordered keys", body: `{"stream":true,"model":"x","store":false}`, wantModel: "x", wantStore: "false", wantStream: "true"},
		{name: "duplicate keys first wins", body: `{"model":"a","model":"b","store":false,"stream":true}`, wantModel: "a", wantStore: "false", wantStream: "true"},
		{name: "whitespace body compacted", body: "{\n  \"model\": \"x\",\n  \"store\": false,\n  \"stream\": true\n}", wantModel: "x", wantStore: "false", wantStream: "true"},
		{name: "string store not canonical", body: `{"model":"x","store":"false","stream":true}`, wantModel: "x", wantStore: "false", wantStream: "true"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			normalized, changed, err := applyOpenAIOAuthHTTPAllowlist([]byte(tt.body))
			require.NoError(t, err)
			require.True(t, changed)
			require.Equal(t, tt.wantModel, gjson.GetBytes(normalized, "model").String())
			require.Equal(t, tt.wantStore, gjson.GetBytes(normalized, "store").Raw)
			require.Equal(t, tt.wantStream, gjson.GetBytes(normalized, "stream").Raw)
		})
	}
}

// TestOpenAIOAuthAllowlist_EscapedKeyFallsBackToGJSON 验证键名带转义时回退旧
// sjson 链（gjson 按解码后的键名匹配，输出与参考实现一致）。
func TestOpenAIOAuthAllowlist_EscapedKeyFallsBackToGJSON(t *testing.T) {
	// JSON 文本中的键是 model（m 用转义书写），gjson 解码后按 model 匹配
	body := []byte(`{"` + `\` + `u006dodel":"x","store":false,"stream":true}`)

	normalized, changed, err := applyOpenAIOAuthHTTPAllowlist(body)
	require.NoError(t, err)
	require.True(t, changed)

	ref, refChanged, refErr := referenceApplyOpenAIOAuthHTTPAllowlist(body)
	require.NoError(t, refErr)
	require.Equal(t, refChanged, changed)
	require.Equal(t, string(ref), string(normalized))
	require.Equal(t, "x", gjson.GetBytes(normalized, "model").String())
}

// TestOpenAIOAuthAllowlistScanBody 直接验证扫描器的 canonical/收集语义。
func TestOpenAIOAuthAllowlistScanBody(t *testing.T) {
	httpIndex := openAIOAuthHTTPAllowlistIndex
	tests := []struct {
		name      string
		body      string
		compact   bool // 用 compact 字段集/无 store-stream 强制扫描
		canonical bool
	}{
		{name: "http canonical subset", body: `{"model":"x","store":false,"stream":true}`, canonical: true},
		{name: "http canonical all fields", body: `{"model":"x","input":[],"instructions":"i","tools":[],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":{},"store":false,"stream":true,"include":[],"service_tier":"priority","prompt_cache_key":"k","text":{},"client_metadata":{}}`, canonical: true},
		{name: "store true", body: `{"model":"x","store":true,"stream":true}`, canonical: false},
		{name: "store missing", body: `{"model":"x","stream":true}`, canonical: false},
		{name: "stream false", body: `{"model":"x","store":false,"stream":false}`, canonical: false},
		{name: "extra field", body: `{"model":"x","bad":1,"store":false,"stream":true}`, canonical: false},
		{name: "wrong order", body: `{"store":false,"model":"x","stream":true}`, canonical: false},
		{name: "space after comma", body: `{"model":"x", "store":false,"stream":true}`, canonical: false},
		{name: "space around colon", body: `{"model" : "x","store":false,"stream":true}`, canonical: false},
		{name: "duplicate key", body: `{"model":"a","model":"b","store":false,"stream":true}`, canonical: false},
		{name: "http empty", body: `{}`, canonical: false},
		{name: "array root", body: `[]`, canonical: false},
		{name: "leading whitespace trimmed", body: ` {"model":"x","store":false,"stream":true}`, canonical: true},
		{name: "compact canonical subset", body: `{"model":"x","input":"hi"}`, compact: true, canonical: true},
		{name: "compact empty", body: `{}`, compact: true, canonical: true},
		{name: "compact drops store", body: `{"model":"x","store":true}`, compact: true, canonical: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fieldIndex, forceStore, forceStream := httpIndex, true, true
			if tt.compact {
				fieldIndex, forceStore, forceStream = openAIOAuthCompactAllowlistIndex, false, false
			}
			scan := openAIOAuthAllowlistScanBody(bytes.TrimSpace([]byte(tt.body)), fieldIndex, forceStore, forceStream)
			require.Equal(t, tt.canonical, scan.canonical, "body: %s", tt.body)
		})
	}

	// compact 策略（无 store/stream 强制）下复用同一扫描器
	compact := openAIOAuthAllowlistScanBody([]byte(`{}`), openAIOAuthCompactAllowlistIndex, false, false)
	require.True(t, compact.canonical)
	compact = openAIOAuthAllowlistScanBody([]byte(`{"store":true}`), openAIOAuthCompactAllowlistIndex, false, false)
	require.False(t, compact.canonical)
	compact = openAIOAuthAllowlistScanBody([]byte(`{"model":"x"}`), openAIOAuthCompactAllowlistIndex, false, false)
	require.True(t, compact.canonical)
}

// TestOpenAIOAuthAllowlistScan_CollectsRawOffsets 验证扫描器收集的 raw 偏移
// 可直接用于重建（含空白容忍与重复键首现语义）。
func TestOpenAIOAuthAllowlistScan_CollectsRawOffsets(t *testing.T) {
	httpIndex := openAIOAuthHTTPAllowlistIndex
	body := []byte(`{"model" : "x", "input":[1, 2],"model":"dup","store":false,"stream":true}`)
	scan := openAIOAuthAllowlistScanBody(body, httpIndex, true, true)
	require.False(t, scan.canonical)
	require.True(t, scan.storeSeen)
	require.True(t, scan.streamSeen)
	// model 首现：值为 "x"
	modelSpan := scan.values[0]
	require.Equal(t, `"x"`, string(body[modelSpan[0]:modelSpan[1]]))
	// input 值带内部空白仍被完整收集
	inputSpan := scan.values[1]
	require.Equal(t, `[1, 2]`, string(body[inputSpan[0]:inputSpan[1]]))

	// 键名带转义 → escapedKey 置位，且不会被当作 allowlist 键收集
	esc := openAIOAuthAllowlistScanBody([]byte(`{"`+`\`+`u006dodel":"x","store":false,"stream":true}`), httpIndex, true, true)
	require.True(t, esc.escapedKey)
	require.False(t, esc.canonical)
	require.Equal(t, [2]int{}, esc.values[0])
}

// TestOpenAIOAuthAllowlist_BuildMatchesSJSONFormat 验证重建输出与 sjson 链的
// 拼接格式完全一致（紧凑、allowlist 序、store/stream 末尾追加顺序）。
func TestOpenAIOAuthAllowlist_BuildMatchesSJSONFormat(t *testing.T) {
	body := []byte(`{"input":"hello","model":"x"}`)
	trimmed := bytes.TrimSpace(body)
	scan := openAIOAuthAllowlistScanBody(trimmed, openAIOAuthHTTPAllowlistIndex, true, true)
	require.False(t, scan.canonical)
	got := buildOpenAIOAuthAllowlistBody(trimmed, openAIOAuthHTTPAllowlistFields, scan, true, true)
	require.Equal(t, `{"model":"x","input":"hello","store":false,"stream":true}`, string(got))
}

// TestOpenAIOAuthAllowlist_FieldCountFitsStackArray 防止新增字段后超出栈上收集数组。
func TestOpenAIOAuthAllowlist_FieldCountFitsStackArray(t *testing.T) {
	require.LessOrEqual(t, len(openAIOAuthHTTPAllowlistFields), openAIOAuthAllowlistFieldsMax)
	require.LessOrEqual(t, len(openAIOAuthCompactAllowlistFields), openAIOAuthAllowlistFieldsMax)
}

// TestInputContainsText 覆盖递归字符串扫描的命中/未命中与提前退出。
func TestInputContainsText(t *testing.T) {
	marker := openAICompatClaudeCodeTodoGuardMarker
	require.True(t, inputContainsText([]any{map[string]any{
		"type":    "message",
		"content": []any{map[string]any{"type": "input_text", "text": "keep it simple " + marker}},
	}}, marker))
	require.True(t, inputContainsText([]any{[]any{map[string]any{"a": "…" + marker + "…"}}}, marker))
	require.True(t, inputContainsText([]any{map[string]any{marker: "in-key"}}, marker))
	require.True(t, inputContainsText([]any{"plain " + marker}, marker))
	require.False(t, inputContainsText([]any{map[string]any{"type": "message", "content": "plain"}}, marker))
	require.False(t, inputContainsText(nil, marker))
	require.False(t, inputContainsText([]any{"x"}, "  "))
	require.False(t, inputContainsText([]any{1.5, true, nil}, marker))
}

// TestAppendOpenAICompatClaudeCodeTodoGuardToRequestBody_SkipsWhenMarkerPresent
// 验证 input 已含 guard marker 时不再重复注入（旧实现因 marshal HTML 转义
// 永远匹配不到，会在多轮会话里重复注入）。
func TestAppendOpenAICompatClaudeCodeTodoGuardToRequestBody_SkipsWhenMarkerPresent(t *testing.T) {
	reqBody := map[string]any{
		"input": []any{
			map[string]any{
				"type":    "message",
				"role":    "developer",
				"content": []any{map[string]any{"type": "input_text", "text": openAICompatClaudeCodeTodoGuardText}},
			},
		},
	}
	require.False(t, appendOpenAICompatClaudeCodeTodoGuardToRequestBody(reqBody))

	// 无 marker 时正常注入
	plain := map[string]any{
		"input": []any{map[string]any{"type": "message", "role": "user", "content": "hi"}},
	}
	require.True(t, appendOpenAICompatClaudeCodeTodoGuardToRequestBody(plain))
}
