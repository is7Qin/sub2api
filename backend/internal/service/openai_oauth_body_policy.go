package service

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var openAIOAuthHTTPAllowlistFields = []string{
	"model",
	"input",
	"instructions",
	"tools",
	"tool_choice",
	"parallel_tool_calls",
	"reasoning",
	"store",
	"stream",
	"include",
	"service_tier",
	"prompt_cache_key",
	"text",
	"client_metadata",
}

var openAIOAuthCompactAllowlistFields = []string{
	"model",
	"input",
	"instructions",
	"tools",
	"parallel_tool_calls",
	"reasoning",
	"service_tier",
	"prompt_cache_key",
	"text",
}

// openAIOAuthAllowlistFieldsMax 是两套 allowlist 字段数的上限，决定扫描器
// 栈上收集数组的大小（http=14、compact=9，均小于该值）。
const openAIOAuthAllowlistFieldsMax = 14

var openAIOAuthHTTPAllowlistIndex = buildOpenAIOAuthAllowlistIndex(openAIOAuthHTTPAllowlistFields)
var openAIOAuthCompactAllowlistIndex = buildOpenAIOAuthAllowlistIndex(openAIOAuthCompactAllowlistFields)

var openAIOAuthFalseRaw = []byte("false")
var openAIOAuthTrueRaw = []byte("true")

func buildOpenAIOAuthAllowlistIndex(fields []string) map[string]int {
	index := make(map[string]int, len(fields))
	for i, field := range fields {
		index[field] = i
	}
	return index
}

// openAIOAuthAllowlistScan 是对顶层对象单次扫描的产物：
//   - canonical：body 是否已是 allowlist 收敛后的精确字节形态（键集合/键序/紧凑、
//     以及 http 路径的 store=false、stream=true），是则整个 copy-rewrite 链可跳过；
//   - escapedKey：顶层出现带转义的键名（gjson 按解码后键名匹配，扫描器按原始字节
//     匹配），出现时需要回退 gjson 收集以保证输出逐字节一致；
//   - values：各 allowlist 字段 raw 值在 body 中的 [start,end) 偏移，供单遍重建使用。
//     重复键只记录首个出现，与 gjson.GetBytes 的"首个匹配"语义一致。
type openAIOAuthAllowlistScan struct {
	canonical  bool
	escapedKey bool
	storeSeen  bool
	streamSeen bool
	values     [openAIOAuthAllowlistFieldsMax][2]int
}

// applyOpenAIOAuthHTTPAllowlist applies the terminal OAuth /responses body policy.
// It copies allowed fields with raw JSON so large numeric literals are not rounded.
func applyOpenAIOAuthHTTPAllowlist(body []byte) ([]byte, bool, error) {
	if len(body) == 0 {
		return body, false, nil
	}
	if !json.Valid(body) {
		return body, false, fmt.Errorf("apply openai oauth http allowlist: invalid JSON")
	}

	// 已是 Codex 收敛形态（仅含允许字段、键序一致、紧凑、store=false/stream=true）时
	// 原样透传：一次扫描 + 零分配，跳过整个 parse-copy-rewrite-marshal 链。
	trimmed := bytes.TrimSpace(body)
	scan := openAIOAuthAllowlistScanBody(trimmed, openAIOAuthHTTPAllowlistIndex, true, true)
	if scan.canonical {
		return body, false, nil
	}
	if scan.escapedKey {
		// 键名带转义时扫描器无法精确复刻 gjson 的解码匹配，回退旧实现。
		return applyOpenAIOAuthHTTPAllowlistSlow(body)
	}
	normalized := buildOpenAIOAuthAllowlistBody(trimmed, openAIOAuthHTTPAllowlistFields, scan, true, true)
	return changedOpenAIOAuthBody(body, normalized)
}

// applyOpenAIOAuthHTTPAllowlistSlow 是旧实现（gjson 收集 + sjson 逐字段重写），
// 仅用于键名带转义等扫描器无法保证与 gjson 逐字节一致的罕见场景。
func applyOpenAIOAuthHTTPAllowlistSlow(body []byte) ([]byte, bool, error) {
	normalized, err := copyOpenAIOAuthAllowedRawFields(body, openAIOAuthHTTPAllowlistFields, "http")
	if err != nil {
		return body, false, err
	}

	// Codex CLI fixed values for non-compact /responses.
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

// applyOpenAIOAuthCompactAllowlist applies the terminal OAuth /responses/compact body policy.
// Compact follows Codex CompactionInput fields and intentionally omits store/stream/client_metadata.
func applyOpenAIOAuthCompactAllowlist(body []byte) ([]byte, bool, error) {
	if len(body) == 0 {
		return body, false, nil
	}
	if !json.Valid(body) {
		return body, false, fmt.Errorf("apply openai oauth compact allowlist: invalid JSON")
	}

	trimmed := bytes.TrimSpace(body)
	scan := openAIOAuthAllowlistScanBody(trimmed, openAIOAuthCompactAllowlistIndex, false, false)
	if scan.canonical {
		return body, false, nil
	}
	if scan.escapedKey {
		return applyOpenAIOAuthCompactAllowlistSlow(body)
	}
	normalized := buildOpenAIOAuthAllowlistBody(trimmed, openAIOAuthCompactAllowlistFields, scan, false, false)
	return changedOpenAIOAuthBody(body, normalized)
}

// applyOpenAIOAuthCompactAllowlistSlow 是旧实现，用途同 HTTP 版本。
func applyOpenAIOAuthCompactAllowlistSlow(body []byte) ([]byte, bool, error) {
	normalized, err := copyOpenAIOAuthAllowedRawFields(body, openAIOAuthCompactAllowlistFields, "compact")
	if err != nil {
		return body, false, err
	}
	return changedOpenAIOAuthBody(body, normalized)
}

func copyOpenAIOAuthAllowedRawFields(body []byte, fields []string, policy string) ([]byte, error) {
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

func changedOpenAIOAuthBody(original, normalized []byte) ([]byte, bool, error) {
	if bytes.Equal(bytes.TrimSpace(original), bytes.TrimSpace(normalized)) {
		return original, false, nil
	}
	return normalized, true, nil
}

// openAIOAuthAllowlistScanBody 单次扫描顶层对象：既判定 body 是否已处于 allowlist
// 收敛形态（canonical），又在需要重写时收集各字段 raw 值偏移。对合法 JSON 必然走完
// 整个对象；空白等非紧凑 token 布局只会把 canonical 置为 false，不影响收集。
func openAIOAuthAllowlistScanBody(trimmed []byte, fieldIndex map[string]int, forceStoreFalse, forceStreamTrue bool) openAIOAuthAllowlistScan {
	scan := openAIOAuthAllowlistScan{canonical: true}
	n := len(trimmed)
	if n == 0 || trimmed[0] != '{' {
		scan.canonical = false
		return scan
	}
	prevIndex := -1
	i := 1 // 已消费 '{'
	for i < n {
		// 键前空白：严格形态不允许，但继续扫描以收集其余字段
		for i < n && trimmed[i] <= ' ' {
			scan.canonical = false
			i++
		}
		if i >= n {
			scan.canonical = false
			return scan
		}
		if trimmed[i] == '}' {
			if i != n-1 {
				scan.canonical = false
			}
			break
		}
		if trimmed[i] != '"' {
			scan.canonical = false
			return scan
		}
		// 读取 key 原始字节（含转义）；带转义的键原样命中不了 allowlist
		j := i + 1
		for j < n && trimmed[j] != '"' {
			if trimmed[j] == '\\' {
				scan.escapedKey = true
				j++
			}
			j++
		}
		if j >= n {
			scan.canonical = false
			return scan
		}
		key := trimmed[i+1 : j]
		fieldIdx, inFields := fieldIndex[string(key)]
		if !inFields || fieldIdx <= prevIndex {
			scan.canonical = false
		}
		prevIndex = fieldIdx
		// 冒号必须存在；其前后允许空白（非 canonical，但收集不受影响）
		i = j + 1
		for i < n && trimmed[i] <= ' ' {
			scan.canonical = false
			i++
		}
		if i >= n || trimmed[i] != ':' {
			scan.canonical = false
			return scan
		}
		i++
		for i < n && trimmed[i] <= ' ' {
			scan.canonical = false
			i++
		}
		if i >= n {
			scan.canonical = false
			return scan
		}
		valueStart := i
		valueEnd, ok := openAIOAuthAllowlistValueEnd(trimmed, valueStart)
		if !ok {
			scan.canonical = false
			return scan
		}
		// store/stream 必须已是强制字面值（http 路径）
		switch {
		case forceStoreFalse && string(key) == "store":
			if string(trimmed[valueStart:valueEnd]) == "false" {
				scan.storeSeen = true
			}
		case forceStreamTrue && string(key) == "stream":
			if string(trimmed[valueStart:valueEnd]) == "true" {
				scan.streamSeen = true
			}
		}
		// 收集 raw 偏移：只记录首个出现（与 gjson.GetBytes 的重复键语义一致）
		if inFields && scan.values[fieldIdx] == [2]int{} {
			scan.values[fieldIdx] = [2]int{valueStart, valueEnd}
		}
		// 值后必须是 ',' 或 '}'；其余（空白等）为非 canonical，跳过空白继续收集
		i = valueEnd
		for i < n && trimmed[i] <= ' ' {
			scan.canonical = false
			i++
		}
		if i >= n {
			scan.canonical = false
			return scan
		}
		switch trimmed[i] {
		case ',':
			i++
		case '}':
			if i != n-1 {
				scan.canonical = false
			}
			i = n
		default:
			scan.canonical = false
			return scan
		}
	}
	if forceStoreFalse && !scan.storeSeen {
		scan.canonical = false
	}
	if forceStreamTrue && !scan.streamSeen {
		scan.canonical = false
	}
	return scan
}

// openAIOAuthAllowlistValueEnd 返回从 start 开始的 JSON 值结束位置（不含）。
// 值类型由起始字节分发；调用方保证 body 已通过 json.Valid，合法输入必然返回 ok。
func openAIOAuthAllowlistValueEnd(trimmed []byte, start int) (int, bool) {
	n := len(trimmed)
	switch trimmed[start] {
	case '"':
		for k := start + 1; k < n; k++ {
			if trimmed[k] == '\\' {
				k++
				continue
			}
			if trimmed[k] == '"' {
				return k + 1, true
			}
		}
	case '{', '[':
		depth := 1
		for k := start + 1; k < n; k++ {
			switch trimmed[k] {
			case '"':
				k++
				for k < n && trimmed[k] != '"' {
					if trimmed[k] == '\\' {
						k++
					}
					k++
				}
				if k >= n {
					return 0, false
				}
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return k + 1, true
				}
			}
		}
	case 't':
		if start+4 <= n && string(trimmed[start:start+4]) == "true" {
			return start + 4, true
		}
	case 'f':
		if start+5 <= n && string(trimmed[start:start+5]) == "false" {
			return start + 5, true
		}
	case 'n':
		if start+4 <= n && string(trimmed[start:start+4]) == "null" {
			return start + 4, true
		}
	default:
		if trimmed[start] == '-' || (trimmed[start] >= '0' && trimmed[start] <= '9') {
			k := start + 1
			for k < n && (trimmed[k] >= '0' && trimmed[k] <= '9' ||
				trimmed[k] == '.' || trimmed[k] == 'e' || trimmed[k] == 'E' ||
				trimmed[k] == '+' || trimmed[k] == '-') {
				k++
			}
			return k, true
		}
	}
	return 0, false
}

// buildOpenAIOAuthAllowlistBody 依据扫描收集的 raw 偏移一次性重建收敛后的 body。
// 输出与旧 sjson.SetRawBytes 链逐字节一致：紧凑 `{"k":raw,...}`、字段按 allowlist
// 序、store/stream 原位写强制值、缺失时按 store→stream 顺序末尾追加。
func buildOpenAIOAuthAllowlistBody(trimmed []byte, fields []string, scan openAIOAuthAllowlistScan, forceStoreFalse, forceStreamTrue bool) []byte {
	storeIdx, streamIdx := -1, -1
	if forceStoreFalse {
		for i, field := range fields {
			if field == "store" {
				storeIdx = i
				break
			}
		}
	}
	if forceStreamTrue {
		for i, field := range fields {
			if field == "stream" {
				streamIdx = i
				break
			}
		}
	}
	// 精确计算输出长度，单次分配
	need := 2 // 花括号
	entries := 0
	for i, field := range fields {
		span := scan.values[i]
		if span == [2]int{} {
			continue
		}
		entries++
		valueLen := span[1] - span[0]
		if i == storeIdx {
			valueLen = len(openAIOAuthFalseRaw)
		} else if i == streamIdx {
			valueLen = len(openAIOAuthTrueRaw)
		}
		need += len(field) + 3 + valueLen
	}
	if storeIdx >= 0 && scan.values[storeIdx] == [2]int{} {
		entries++
		need += len("store") + 3 + len(openAIOAuthFalseRaw)
	}
	if streamIdx >= 0 && scan.values[streamIdx] == [2]int{} {
		entries++
		need += len("stream") + 3 + len(openAIOAuthTrueRaw)
	}
	if entries > 1 {
		need += entries - 1 // 字段间逗号
	}
	buf := make([]byte, 0, need)
	buf = append(buf, '{')
	first := true
	appendEntry := func(field string, raw []byte) {
		if !first {
			buf = append(buf, ',')
		}
		first = false
		buf = append(buf, '"')
		buf = append(buf, field...)
		buf = append(buf, `":`...)
		buf = append(buf, raw...)
	}
	for i, field := range fields {
		span := scan.values[i]
		if span == [2]int{} {
			continue
		}
		switch i {
		case storeIdx:
			appendEntry(field, openAIOAuthFalseRaw)
		case streamIdx:
			appendEntry(field, openAIOAuthTrueRaw)
		default:
			appendEntry(field, trimmed[span[0]:span[1]])
		}
	}
	if storeIdx >= 0 && scan.values[storeIdx] == [2]int{} {
		appendEntry("store", openAIOAuthFalseRaw)
	}
	if streamIdx >= 0 && scan.values[streamIdx] == [2]int{} {
		appendEntry("stream", openAIOAuthTrueRaw)
	}
	buf = append(buf, '}')
	return buf
}
