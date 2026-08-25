package service

// anthropicSSEFieldValue accepts the optional single space after an SSE field
// colon. Some compatible upstreams omit it in otherwise valid event/data pairs.
func anthropicSSEFieldValue(line, field string) (string, bool) {
	if len(line) <= len(field) || line[:len(field)] != field || line[len(field)] != ':' {
		return "", false
	}
	value := line[len(field)+1:]
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	return value, true
}
