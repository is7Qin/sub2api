package service

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
)

var (
	openAISensitiveDiagnosticFieldNames = []string{
		"x-codex-installation-id",
		"x-codex-window-id",
		"session-id",
		"thread-id",
		"x-client-request-id",
		"installation_id",
		"thread_id",
		"window_id",
		"prompt_cache_key",
		"session_id",
		"conversation_id",
		"raw_user_agent",
		"user_agent",
		"user-agent",
		"authorization",
		"access_token",
		"refresh_token",
		"id_token",
		"session_token",
		"api_key",
		"apikey",
		"token",
		"personal_access_token",
		"email",
		"chatgpt_user_id",
		"chatgpt_account_id",
		"chatgpt-account-id",
		"chatgpt_plan_type",
		"chatgpt_account_is_fedramp",
		"x-openai-fedramp",
	}
	openAISensitiveDiagnosticFieldSet         = buildOpenAISensitiveDiagnosticFieldSet(openAISensitiveDiagnosticFieldNames)
	openAISensitiveDiagnosticFieldPattern     = strings.Join(openAISensitiveDiagnosticFieldNames, "|")
	openAISensitiveDiagnosticJSONFieldRe      = regexp.MustCompile(`(?i)("(?:` + openAISensitiveDiagnosticFieldPattern + `)"\s*:\s*)(?:"(?:\\.|[^"\\])*"|true|false|null|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?)(\s*(?:[,}\]]|$))`)
	openAISensitiveDiagnosticJSONKeyRe        = regexp.MustCompile(`(?i)"(?:` + openAISensitiveDiagnosticFieldPattern + `)"\s*:\s*`)
	openAISensitiveDiagnosticEscapedJSONKeyRe = regexp.MustCompile(`(?i)\\"(?:` + openAISensitiveDiagnosticFieldPattern + `)\\"\s*:\s*`)
	openAISensitiveDiagnosticBearerRe         = regexp.MustCompile(`(?i)\bBearer\s+[^\s,;"}]+`)
	openAISensitiveDiagnosticUUIDRe           = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	openAISensitiveDiagnosticCodexUARe        = regexp.MustCompile(`(?i)codex-tui/[^\s";]+\s+\([^"\)]*\)\s+[^\s"]+\s+\(codex-tui;\s*[^\)]*\)`)
)

const (
	openAISensitiveDiagnosticJSONCompositeMaxScan = 8192
	openAISensitiveDiagnosticJSONKeyMaxScan       = 512
	openAISensitiveDiagnosticKVValueMaxScan       = 8192
	openAISensitiveDiagnosticJSONMaxDepth         = 16
	openAISensitiveDiagnosticJSONBodyMaxParse     = 512 << 10
	openAISensitiveDiagnosticEmbeddedJSONMaxParse = 64 << 10
)

const openAIAuthenticationDiagnosticFallback = "account authentication failed"

// sanitizeOpenAIAccountDiagnosticText applies structural redaction first, then
// removes known account credential values that an upstream may echo as plain text.
// Short credentials are too ambiguous for value replacement, so an echoed one
// fails closed to a fixed diagnostic instead of corrupting unrelated prose.
func sanitizeOpenAIAccountDiagnosticText(account *Account, text string) string {
	text = sanitizeOpenAIUpstreamDiagnosticText(text)
	if account == nil || text == "" {
		return text
	}

	credentialKeys := make([]string, 0, len(SensitiveCredentialKeys)+1)
	credentialKeys = append(credentialKeys, SensitiveCredentialKeys...)
	credentialKeys = append(credentialKeys, "setup_token")
	values := make([]string, 0, len(credentialKeys))
	for _, key := range credentialKeys {
		value := strings.TrimSpace(account.GetCredential(key))
		if value == "" {
			continue
		}
		if len(value) < 4 {
			if containsOpenAIShortCredentialToken(text, value) {
				return openAIAuthenticationDiagnosticFallback
			}
			continue
		}
		values = append(values, value)
	}
	// Replace longer overlapping values first so no credential suffix survives.
	slices.SortFunc(values, func(a, b string) int { return len(b) - len(a) })
	for _, value := range values {
		text = strings.ReplaceAll(text, value, "[redacted]")
	}
	return text
}

// containsOpenAIShortCredentialToken rejects only a credibly echoed short
// credential. ASCII letters, digits, underscores, and hyphens make a larger
// credential-like token, so matching within ordinary prose or identifiers is
// deliberately not enough to discard the entire diagnostic.
func containsOpenAIShortCredentialToken(text, credential string) bool {
	start := 0
	for {
		index := strings.Index(text[start:], credential)
		if index < 0 {
			return false
		}
		index += start
		end := index + len(credential)
		if (index == 0 || !isOpenAIShortCredentialTokenChar(text[index-1])) &&
			(end == len(text) || !isOpenAIShortCredentialTokenChar(text[end])) {
			return true
		}
		start = end
	}
}

func isOpenAIShortCredentialTokenChar(ch byte) bool {
	return openAISensitiveDiagnosticIsRegexWordChar(ch) || ch == '-'
}

func sanitizeOpenAIUpstreamDiagnosticText(text string) string {
	if text == "" {
		return text
	}
	text = redactOpenAISensitiveDiagnosticEscapedJSONFields(text)
	text = redactOpenAISensitiveDiagnosticKVFields(text)
	text = sanitizeUpstreamErrorMessage(text)
	text = redactOpenAISensitiveDiagnosticEscapedJSONFields(text)
	text = redactOpenAISensitiveDiagnosticNormalizedJSONFields(text)
	text = openAISensitiveDiagnosticJSONFieldRe.ReplaceAllString(text, `$1"[redacted]"$2`)
	text = redactOpenAISensitiveDiagnosticJSONCompositeFields(text)
	text = redactOpenAISensitiveDiagnosticJSONRemainderFields(text)
	text = redactOpenAISensitiveDiagnosticEscapedJSONFields(text)
	text = redactOpenAISensitiveDiagnosticNormalizedJSONFields(text)
	text = openAISensitiveDiagnosticBearerRe.ReplaceAllString(text, "Bearer [redacted]")
	text = openAISensitiveDiagnosticCodexUARe.ReplaceAllString(text, "[codex-user-agent-redacted]")
	text = redactOpenAISensitiveDiagnosticKVFields(text)
	text = openAISensitiveDiagnosticUUIDRe.ReplaceAllString(text, "[uuid-redacted]")
	return text
}

func redactOpenAISensitiveDiagnosticKVFields(text string) string {
	var b strings.Builder
	last := 0
	changed := false

	for i := 0; i < len(text); i++ {
		if !openAISensitiveDiagnosticCanStartKVKey(text, i) {
			continue
		}
		keyEnd, ok := openAISensitiveDiagnosticMatchKVKey(text, i)
		if !ok {
			continue
		}
		if openAISensitiveDiagnosticShouldSkipKVQuotedJSONKey(text, i, keyEnd) {
			continue
		}
		valueStart, safePrefixEnd, failClosed, ok := openAISensitiveDiagnosticKVValueStart(text, keyEnd)
		if failClosed {
			b.WriteString(text[last:safePrefixEnd])
			b.WriteString("[redacted]")
			return b.String()
		}
		if !ok {
			continue
		}

		b.WriteString(text[last:valueStart])
		b.WriteString("[redacted]")
		changed = true

		valueEnd, ok := openAISensitiveDiagnosticKVValueEnd(text, valueStart)
		if !ok {
			return b.String()
		}
		last = valueEnd
		i = valueEnd - 1
	}

	if !changed {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}

func openAISensitiveDiagnosticCanStartKVKey(text string, index int) bool {
	if index >= len(text) {
		return false
	}
	ch := text[index]
	if !openAISensitiveDiagnosticIsASCIIAlpha(ch) {
		return false
	}
	return index == 0 || !openAISensitiveDiagnosticIsRegexWordChar(text[index-1])
}

func openAISensitiveDiagnosticMatchKVKey(text string, start int) (int, bool) {
	for _, key := range openAISensitiveDiagnosticFieldNames {
		end := start + len(key)
		if end > len(text) {
			continue
		}
		if strings.EqualFold(text[start:end], key) && (end == len(text) || !openAISensitiveDiagnosticIsRegexWordChar(text[end])) {
			return end, true
		}
	}
	return 0, false
}

func openAISensitiveDiagnosticShouldSkipKVQuotedJSONKey(text string, start, keyEnd int) bool {
	if start > 0 && text[start-1] == '"' && keyEnd < len(text) && text[keyEnd] == '"' {
		i, ok := openAISensitiveDiagnosticKVSkipWhitespaceBounded(text, keyEnd+1)
		if !ok {
			return false
		}
		return i < len(text) && text[i] == ':'
	}

	if start > 1 && text[start-1] == '"' && openAISensitiveDiagnosticIsEscapedJSONQuote(text, start-1) &&
		keyEnd+1 < len(text) && text[keyEnd] == '\\' && text[keyEnd+1] == '"' &&
		openAISensitiveDiagnosticIsEscapedJSONQuote(text, keyEnd+1) {
		i, ok := openAISensitiveDiagnosticKVSkipWhitespaceBounded(text, keyEnd+2)
		if !ok {
			return false
		}
		return i < len(text) && text[i] == ':'
	}

	return false
}

func openAISensitiveDiagnosticKVValueStart(text string, keyEnd int) (valueStart, safePrefixEnd int, failClosed, ok bool) {
	i := keyEnd
	safePrefixEnd = keyEnd
	if i < len(text) && (text[i] == '"' || text[i] == '\'') {
		i++
		safePrefixEnd = i
	} else {
		quoteEnd, _, quoteOK, quoteUnsafe := openAISensitiveDiagnosticKVEscapedQuoteEnd(text, i)
		if quoteUnsafe {
			return 0, safePrefixEnd, true, false
		}
		if quoteOK {
			i = quoteEnd
			safePrefixEnd = i
		}
	}
	separatorStart, ok := openAISensitiveDiagnosticKVSkipWhitespaceBounded(text, i)
	if !ok {
		return 0, safePrefixEnd, true, false
	}
	if separatorStart >= len(text) || (text[separatorStart] != '=' && text[separatorStart] != ':') {
		return 0, 0, false, false
	}
	return separatorStart + 1, 0, false, true
}

func openAISensitiveDiagnosticKVValueEnd(text string, start int) (int, bool) {
	valueStart, ok := openAISensitiveDiagnosticKVSkipValueWhitespace(text, start)
	if !ok {
		return 0, false
	}
	if valueStart >= len(text) {
		return valueStart, true
	}
	if bearerStart, matched, ok := openAISensitiveDiagnosticKVBearerValueStart(text, valueStart); matched {
		if !ok {
			return 0, false
		}
		valueStart = bearerStart
	}
	if valueStart >= len(text) {
		return valueStart, true
	}

	switch {
	case text[valueStart] == '"' || text[valueStart] == '\'':
		return openAISensitiveDiagnosticKVQuotedStringEnd(text, valueStart, text[valueStart])
	case openAISensitiveDiagnosticStartsKVEscapedQuote(text, valueStart):
		return openAISensitiveDiagnosticKVEscapedQuotedStringEnd(text, valueStart)
	default:
		return openAISensitiveDiagnosticKVUnquotedValueEnd(text, valueStart)
	}
}

func openAISensitiveDiagnosticKVSkipValueWhitespace(text string, start int) (int, bool) {
	return openAISensitiveDiagnosticKVSkipWhitespaceBounded(text, start)
}

func openAISensitiveDiagnosticKVSkipWhitespaceBounded(text string, start int) (int, bool) {
	i := start
	for i < len(text) && openAISensitiveDiagnosticIsWhitespace(text[i]) {
		if i-start >= openAISensitiveDiagnosticKVValueMaxScan {
			return 0, false
		}
		i++
	}
	return i, true
}

func openAISensitiveDiagnosticKVBearerValueStart(text string, start int) (int, bool, bool) {
	const bearer = "Bearer"
	end := start + len(bearer)
	if end > len(text) || !strings.EqualFold(text[start:end], bearer) {
		return 0, false, true
	}
	if end >= len(text) || !openAISensitiveDiagnosticIsWhitespace(text[end]) {
		return 0, false, true
	}
	valueStart, ok := openAISensitiveDiagnosticKVSkipValueWhitespace(text, end)
	if !ok {
		return 0, true, false
	}
	return valueStart, true, true
}

func openAISensitiveDiagnosticKVQuotedStringEnd(text string, start int, quote byte) (int, bool) {
	escaped := false
	for i := start + 1; i < len(text) && i-start <= openAISensitiveDiagnosticKVValueMaxScan; i++ {
		switch {
		case escaped:
			escaped = false
		case text[i] == '\\':
			escaped = true
		case text[i] == quote:
			if openAISensitiveDiagnosticKVHasQuotedValueBoundary(text, i+1) {
				return i + 1, true
			}
		}
	}
	return 0, false
}

func openAISensitiveDiagnosticKVEscapedQuotedStringEnd(text string, start int) (int, bool) {
	openQuoteEnd, quote, ok, _ := openAISensitiveDiagnosticKVEscapedQuoteEnd(text, start)
	if !ok {
		return 0, false
	}
	openBackslashes := openAISensitiveDiagnosticKVBackslashRunBefore(text, openQuoteEnd-1)
	for i := openQuoteEnd; i < len(text) && i-start <= openAISensitiveDiagnosticKVValueMaxScan; i++ {
		if text[i] != quote || !openAISensitiveDiagnosticKVHasQuotedValueBoundary(text, i+1) {
			continue
		}
		// Match the quote layer that opened the value; longer runs are escaped content.
		if openAISensitiveDiagnosticKVBackslashRunBefore(text, i) == openBackslashes {
			return i + 1, true
		}
	}
	return 0, false
}

func openAISensitiveDiagnosticKVUnquotedValueEnd(text string, start int) (int, bool) {
	for i := start; i < len(text) && i-start <= openAISensitiveDiagnosticKVValueMaxScan; i++ {
		switch text[i] {
		case ' ', '\t', '\r', '\n', '\f', '\v', ',', ';', '&', '"', '}':
			return i, true
		}
	}
	if len(text)-start > openAISensitiveDiagnosticKVValueMaxScan {
		return 0, false
	}
	return len(text), true
}

func openAISensitiveDiagnosticKVHasQuotedValueBoundary(text string, index int) bool {
	if index >= len(text) {
		return true
	}
	return !openAISensitiveDiagnosticIsRegexWordChar(text[index])
}

func openAISensitiveDiagnosticStartsKVEscapedQuote(text string, start int) bool {
	_, _, ok, _ := openAISensitiveDiagnosticKVEscapedQuoteEnd(text, start)
	return ok
}

func openAISensitiveDiagnosticKVEscapedQuoteEnd(text string, start int) (end int, quote byte, ok bool, unsafe bool) {
	if start >= len(text) || text[start] != '\\' {
		return 0, 0, false, false
	}
	i := start
	for i < len(text) && text[i] == '\\' {
		if i-start >= openAISensitiveDiagnosticKVValueMaxScan {
			return 0, 0, false, true
		}
		i++
	}
	if i >= len(text) || (text[i] != '"' && text[i] != '\'') {
		return 0, 0, false, false
	}
	return i + 1, text[i], true, false
}

func openAISensitiveDiagnosticIsKVEscapedQuote(text string, quoteIndex int) bool {
	return openAISensitiveDiagnosticKVBackslashRunBefore(text, quoteIndex) > 0
}

func openAISensitiveDiagnosticKVBackslashRunBefore(text string, index int) int {
	backslashes := 0
	for i := index - 1; i >= 0 && text[i] == '\\'; i-- {
		backslashes++
	}
	return backslashes
}

func openAISensitiveDiagnosticIsASCIIAlpha(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z')
}

func openAISensitiveDiagnosticIsRegexWordChar(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_'
}

func openAISensitiveDiagnosticIsWhitespace(ch byte) bool {
	switch ch {
	case ' ', '\t', '\r', '\n', '\f', '\v':
		return true
	default:
		return false
	}
}

func redactOpenAISensitiveDiagnosticNormalizedJSONFields(text string) string {
	var b strings.Builder
	last := 0
	changed := false

	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '"':
		case '\\':
			runEnd := openAISensitiveDiagnosticBackslashRunEnd(text, i, openAISensitiveDiagnosticJSONKeyMaxScan)
			if runEnd >= len(text) || text[runEnd] != '"' {
				i = runEnd - 1
				continue
			}
		default:
			continue
		}
		key, keyEnd, escapedKey, ok := openAISensitiveDiagnosticNormalizedJSONKey(text, i)
		if !ok || !isOpenAISensitiveDiagnosticField(key) {
			continue
		}
		valueStart, ok := openAISensitiveDiagnosticJSONFieldValueStart(text, keyEnd)
		if !ok {
			continue
		}
		if valueStart < last {
			continue
		}

		b.WriteString(text[last:valueStart])
		changed = true

		valueEnd, ok := 0, false
		if escapedKey || openAISensitiveDiagnosticStartsEscapedJSONValue(text, valueStart) {
			b.WriteString(`\"[redacted]\"`)
			valueEnd, ok = openAISensitiveDiagnosticEscapedJSONValueEnd(text, valueStart)
		} else {
			b.WriteString(`"[redacted]"`)
			valueEnd, ok = openAISensitiveDiagnosticJSONValueEnd(text, valueStart)
		}
		if !ok {
			return b.String()
		}
		last = valueEnd
		i = valueEnd - 1
	}

	if !changed {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}

func openAISensitiveDiagnosticBackslashRunEnd(text string, start, max int) int {
	i := start
	for i < len(text) && text[i] == '\\' && i-start <= max {
		i++
	}
	return i
}

func openAISensitiveDiagnosticNormalizedJSONKey(text string, start int) (key string, end int, escaped bool, ok bool) {
	if start >= len(text) {
		return "", 0, false, false
	}
	if text[start] == '"' && !openAISensitiveDiagnosticIsKVEscapedQuote(text, start) {
		key, end, ok = openAISensitiveDiagnosticNormalizedJSONStringKeyEnd(text, start+1, false)
		return key, end, false, ok
	}
	if text[start] != '\\' {
		return "", 0, false, false
	}
	openEnd, quote, quoteOK, quoteUnsafe := openAISensitiveDiagnosticKVEscapedQuoteEnd(text, start)
	if quoteUnsafe || !quoteOK || quote != '"' {
		return "", 0, false, false
	}
	key, end, ok = openAISensitiveDiagnosticNormalizedJSONStringKeyEnd(text, openEnd, true)
	return key, end, true, ok
}

func openAISensitiveDiagnosticNormalizedJSONStringKeyEnd(text string, start int, escapedDelimiter bool) (string, int, bool) {
	var key strings.Builder
	for i := start; i < len(text) && i-start <= openAISensitiveDiagnosticJSONKeyMaxScan; {
		if escapedDelimiter {
			if closeEnd, quote, ok, unsafe := openAISensitiveDiagnosticKVEscapedQuoteEnd(text, i); unsafe {
				return "", 0, false
			} else if ok && quote == '"' {
				return key.String(), closeEnd, true
			}
		} else if text[i] == '"' {
			return key.String(), i + 1, true
		}

		if text[i] == '\\' {
			next, ok := openAISensitiveDiagnosticAppendNormalizedJSONKeyEscape(&key, text, i)
			if !ok {
				return "", 0, false
			}
			i = next
			continue
		}
		if text[i] >= 0x80 {
			return "", 0, false
		}
		key.WriteByte(text[i])
		i++
	}
	return "", 0, false
}

func openAISensitiveDiagnosticAppendNormalizedJSONKeyEscape(b *strings.Builder, text string, slash int) (int, bool) {
	if slash+1 >= len(text) {
		return 0, false
	}
	switch text[slash+1] {
	case '"', '\\', '/':
		b.WriteByte(text[slash+1])
		return slash + 2, true
	case 'b':
		b.WriteByte('\b')
		return slash + 2, true
	case 'f':
		b.WriteByte('\f')
		return slash + 2, true
	case 'n':
		b.WriteByte('\n')
		return slash + 2, true
	case 'r':
		b.WriteByte('\r')
		return slash + 2, true
	case 't':
		b.WriteByte('\t')
		return slash + 2, true
	case 'u':
		if slash+6 > len(text) {
			return 0, false
		}
		r, ok := openAISensitiveDiagnosticHex4(text[slash+2 : slash+6])
		if !ok || r > 0x7f {
			return 0, false
		}
		b.WriteByte(byte(r))
		return slash + 6, true
	default:
		return 0, false
	}
}

func openAISensitiveDiagnosticHex4(text string) (rune, bool) {
	if len(text) != 4 {
		return 0, false
	}
	var value rune
	for i := 0; i < len(text); i++ {
		ch := text[i]
		value <<= 4
		switch {
		case ch >= '0' && ch <= '9':
			value += rune(ch - '0')
		case ch >= 'a' && ch <= 'f':
			value += rune(ch-'a') + 10
		case ch >= 'A' && ch <= 'F':
			value += rune(ch-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

func openAISensitiveDiagnosticJSONFieldValueStart(text string, keyEnd int) (int, bool) {
	colon, ok := openAISensitiveDiagnosticKVSkipWhitespaceBounded(text, keyEnd)
	if !ok {
		return 0, false
	}
	if colon >= len(text) || text[colon] != ':' {
		return 0, false
	}
	valueStart, ok := openAISensitiveDiagnosticKVSkipWhitespaceBounded(text, colon+1)
	if !ok {
		return 0, false
	}
	return valueStart, true
}

func openAISensitiveDiagnosticStartsEscapedJSONValue(text string, start int) bool {
	return start+1 < len(text) && text[start] == '\\' && text[start+1] == '"'
}

func redactOpenAISensitiveDiagnosticJSONCompositeFields(text string) string {
	matches := openAISensitiveDiagnosticJSONKeyRe.FindAllStringIndex(text, -1)
	if len(matches) == 0 {
		return text
	}

	var b strings.Builder
	last := 0
	changed := false
	for _, match := range matches {
		if match[0] < last || match[1] >= len(text) {
			continue
		}
		if openAISensitiveDiagnosticIsEscapedJSONQuote(text, match[0]) {
			continue
		}
		if text[match[1]] != '[' && text[match[1]] != '{' {
			continue
		}
		valueEnd, ok := openAISensitiveDiagnosticJSONCompositeEnd(text, match[1])
		b.WriteString(text[last:match[1]])
		b.WriteString(`"[redacted]"`)
		changed = true
		if !ok {
			return b.String()
		}
		last = valueEnd
	}
	if !changed {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}

func openAISensitiveDiagnosticJSONCompositeEnd(text string, start int) (int, bool) {
	stack := make([]byte, 0, openAISensitiveDiagnosticJSONMaxDepth)
	inString := false
	escaped := false
	for i := start; i < len(text) && i-start <= openAISensitiveDiagnosticJSONCompositeMaxScan; i++ {
		if inString {
			switch {
			case escaped:
				escaped = false
			case text[i] == '\\':
				escaped = true
			case text[i] == '"':
				inString = false
			}
			continue
		}

		switch text[i] {
		case '"':
			inString = true
		case '{':
			if len(stack) >= openAISensitiveDiagnosticJSONMaxDepth {
				return 0, false
			}
			stack = append(stack, '}')
		case '[':
			if len(stack) >= openAISensitiveDiagnosticJSONMaxDepth {
				return 0, false
			}
			stack = append(stack, ']')
		case '}', ']':
			if len(stack) == 0 || stack[len(stack)-1] != text[i] {
				return 0, false
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return i + 1, true
			}
		}
	}
	return 0, false
}

func sanitizeOpenAIUpstreamDiagnosticBodyForLog(body []byte, maxBytes int) string {
	if len(body) == 0 {
		return ""
	}
	if maxBytes <= 0 {
		maxBytes = 2048
	}
	text, ok := sanitizeOpenAIUpstreamDiagnosticJSONBodyForLog(body)
	if !ok {
		text = sanitizeOpenAIUpstreamDiagnosticText(string(body))
	}
	// Keep diagnostic bodies single-line for logs/support details after redaction.
	text = strings.ReplaceAll(text, "\r", " ")
	text = strings.ReplaceAll(text, "\n", " ")
	return truncateString(text, maxBytes)
}

func redactOpenAISensitiveDiagnosticJSONRemainderFields(text string) string {
	matches := openAISensitiveDiagnosticJSONKeyRe.FindAllStringIndex(text, -1)
	if len(matches) == 0 {
		return text
	}

	var b strings.Builder
	last := 0
	changed := false
	for _, match := range matches {
		if match[0] < last {
			continue
		}
		if openAISensitiveDiagnosticIsEscapedJSONQuote(text, match[0]) {
			continue
		}
		valueStart := match[1]
		b.WriteString(text[last:valueStart])
		b.WriteString(`"[redacted]"`)
		changed = true
		valueEnd, ok := openAISensitiveDiagnosticJSONValueEnd(text, valueStart)
		if !ok {
			return b.String()
		}
		last = valueEnd
	}
	if !changed {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}

func redactOpenAISensitiveDiagnosticEscapedJSONFields(text string) string {
	matches := openAISensitiveDiagnosticEscapedJSONKeyRe.FindAllStringIndex(text, -1)
	if len(matches) == 0 {
		return text
	}

	var b strings.Builder
	last := 0
	changed := false
	for _, match := range matches {
		if match[0] < last {
			continue
		}
		valueStart := match[1]
		b.WriteString(text[last:valueStart])
		b.WriteString(`\"[redacted]\"`)
		changed = true
		valueEnd, ok := openAISensitiveDiagnosticEscapedJSONValueEnd(text, valueStart)
		if !ok {
			return b.String()
		}
		last = valueEnd
	}
	if !changed {
		return text
	}
	b.WriteString(text[last:])
	return b.String()
}

func openAISensitiveDiagnosticJSONValueEnd(text string, start int) (int, bool) {
	if start >= len(text) {
		return 0, false
	}

	var (
		valueEnd int
		ok       bool
	)
	switch text[start] {
	case '"':
		valueEnd, ok = openAISensitiveDiagnosticQuotedStringEnd(text, start)
	case '\\':
		if start+1 < len(text) && text[start+1] == '"' {
			valueEnd, ok = openAISensitiveDiagnosticEscapedQuotedStringEnd(text, start)
		}
	case '{', '[':
		valueEnd, ok = openAISensitiveDiagnosticJSONCompositeEnd(text, start)
	default:
		valueEnd, ok = openAISensitiveDiagnosticUnknownScalarEnd(text, start)
	}
	if !ok {
		return 0, false
	}
	if !openAISensitiveDiagnosticValueHasJSONDelimiter(text, valueEnd) {
		return 0, false
	}
	return valueEnd, true
}

func openAISensitiveDiagnosticQuotedStringEnd(text string, start int) (int, bool) {
	escaped := false
	for i := start + 1; i < len(text); i++ {
		switch {
		case escaped:
			escaped = false
		case text[i] == '\\':
			escaped = true
		case text[i] == '"':
			return i + 1, true
		}
	}
	return 0, false
}

func openAISensitiveDiagnosticEscapedQuotedStringEnd(text string, start int) (int, bool) {
	for i := start + 2; i < len(text); i++ {
		if text[i] == '"' && openAISensitiveDiagnosticIsEscapedJSONQuote(text, i) {
			return i + 1, true
		}
	}
	return 0, false
}

func openAISensitiveDiagnosticUnknownScalarEnd(text string, start int) (int, bool) {
	for i := start; i < len(text); i++ {
		switch text[i] {
		case ',', '}', ']':
			return i, true
		}
	}
	return len(text), true
}

func openAISensitiveDiagnosticEscapedJSONValueEnd(text string, start int) (int, bool) {
	if start >= len(text) {
		return 0, false
	}

	var (
		valueEnd int
		ok       bool
	)
	switch text[start] {
	case '\\':
		if start+1 < len(text) && text[start+1] == '"' {
			valueEnd, ok = openAISensitiveDiagnosticEscapedQuotedStringEnd(text, start)
		}
	case '{', '[':
		valueEnd, ok = openAISensitiveDiagnosticEscapedJSONCompositeEnd(text, start)
	default:
		valueEnd, ok = openAISensitiveDiagnosticUnknownScalarEnd(text, start)
	}
	if !ok {
		return 0, false
	}
	if !openAISensitiveDiagnosticValueHasJSONDelimiter(text, valueEnd) {
		return 0, false
	}
	return valueEnd, true
}

func openAISensitiveDiagnosticValueHasJSONDelimiter(text string, valueEnd int) bool {
	for i := valueEnd; i < len(text); i++ {
		switch text[i] {
		case ' ', '\t', '\r', '\n':
			continue
		case ',', '}', ']':
			return true
		default:
			return false
		}
	}
	return true
}

func openAISensitiveDiagnosticEscapedJSONCompositeEnd(text string, start int) (int, bool) {
	stack := make([]byte, 0, openAISensitiveDiagnosticJSONMaxDepth)
	inString := false
	for i := start; i < len(text) && i-start <= openAISensitiveDiagnosticJSONCompositeMaxScan; i++ {
		if inString {
			if text[i] == '"' && openAISensitiveDiagnosticIsEscapedJSONQuote(text, i) {
				inString = false
			}
			continue
		}

		if text[i] == '"' && openAISensitiveDiagnosticIsEscapedJSONQuote(text, i) {
			inString = true
			continue
		}

		switch text[i] {
		case '{':
			if len(stack) >= openAISensitiveDiagnosticJSONMaxDepth {
				return 0, false
			}
			stack = append(stack, '}')
		case '[':
			if len(stack) >= openAISensitiveDiagnosticJSONMaxDepth {
				return 0, false
			}
			stack = append(stack, ']')
		case '}', ']':
			if len(stack) == 0 || stack[len(stack)-1] != text[i] {
				return 0, false
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return i + 1, true
			}
		}
	}
	return 0, false
}

func openAISensitiveDiagnosticIsEscapedJSONQuote(text string, quoteIndex int) bool {
	backslashes := 0
	for i := quoteIndex - 1; i >= 0 && text[i] == '\\'; i-- {
		backslashes++
	}
	return backslashes%4 == 1
}

func sanitizeOpenAIUpstreamDiagnosticJSONBodyForLog(body []byte) (string, bool) {
	if len(body) > openAISensitiveDiagnosticJSONBodyMaxParse {
		return "", false
	}
	if !json.Valid(body) {
		return "", false
	}

	var value any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "", false
	}

	redacted := redactOpenAISensitiveDiagnosticJSONValue(value, 0)
	encoded, err := json.Marshal(redacted)
	if err != nil {
		return "", false
	}
	return string(encoded), true
}

func redactOpenAISensitiveDiagnosticJSONValue(value any, depth int) any {
	if depth > openAISensitiveDiagnosticJSONMaxDepth {
		return "[redacted]"
	}

	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, val := range v {
			if isOpenAISensitiveDiagnosticField(key) {
				out[key] = "[redacted]"
				continue
			}
			out[key] = redactOpenAISensitiveDiagnosticJSONValue(val, depth+1)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = redactOpenAISensitiveDiagnosticJSONValue(item, depth+1)
		}
		return out
	case string:
		return sanitizeOpenAISensitiveDiagnosticJSONString(v, depth)
	default:
		return value
	}
}

func sanitizeOpenAISensitiveDiagnosticJSONString(value string, depth int) string {
	if depth >= openAISensitiveDiagnosticJSONMaxDepth {
		return sanitizeOpenAIUpstreamDiagnosticText(value)
	}

	trimmed := strings.TrimSpace(value)
	if len(trimmed) > 0 && len(trimmed) <= openAISensitiveDiagnosticEmbeddedJSONMaxParse && (trimmed[0] == '{' || trimmed[0] == '[') && json.Valid([]byte(trimmed)) {
		var embedded any
		decoder := json.NewDecoder(strings.NewReader(trimmed))
		decoder.UseNumber()
		if err := decoder.Decode(&embedded); err == nil {
			redacted := redactOpenAISensitiveDiagnosticJSONValue(embedded, depth+1)
			if encoded, err := json.Marshal(redacted); err == nil {
				return string(encoded)
			}
		}
	}

	return sanitizeOpenAIUpstreamDiagnosticText(value)
}

func isOpenAISensitiveDiagnosticField(key string) bool {
	_, ok := openAISensitiveDiagnosticFieldSet[strings.ToLower(strings.TrimSpace(key))]
	return ok
}

func buildOpenAISensitiveDiagnosticFieldSet(keys []string) map[string]struct{} {
	set := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		normalized := strings.ToLower(strings.TrimSpace(key))
		if normalized == "" {
			continue
		}
		set[normalized] = struct{}{}
	}
	return set
}
