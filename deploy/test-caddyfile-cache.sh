#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
caddyfile="$repo_root/deploy/Caddyfile"

reject_cache_control_override() {
	awk '
	function ascii_lower(value,    upper, lower, i, c, position, result) {
		upper = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
		lower = "abcdefghijklmnopqrstuvwxyz"
		for (i = 1; i <= length(value); i++) {
			c = substr(value, i, 1)
			position = index(upper, c)
			result = result (position ? substr(lower, position, 1) : c)
		}
		return result
	}
	function unquote_token(token,    quote, i, c, escaped, result) {
		if (length(token) < 2) return token
		quote = substr(token, 1, 1)
		if ((quote != "\"" && quote != "`") || substr(token, length(token), 1) != quote) return token
		if (quote == "`") return substr(token, 2, length(token) - 2)
		for (i = 2; i < length(token); i++) {
			c = substr(token, i, 1)
			if (escaped) { result = result c; escaped = 0; continue }
			if (c == "\\") { escaped = 1; continue }
			if (c == "\"") return token
			result = result c
		}
		return escaped ? token : result
	}
	function tokenize(line,    i, c, token, quote, escaped, count) {
		delete tokens
		for (i = 1; i <= length(line); i++) {
			c = substr(line, i, 1)
			if (quote != "") {
				token = token c
				if (quote == "\"" && escaped) { escaped = 0; continue }
				if (quote == "\"" && c == "\\") { escaped = 1; continue }
				if (c == quote) quote = ""
				continue
			}
			if (c == "\"" || c == "`") { quote = c; token = token c; continue }
			if (c ~ /[[:space:]]/) {
				if (token != "") { tokens[++count] = token; token = "" }
				continue
			}
			token = token c
		}
		if (token != "") tokens[++count] = token
		if (quote != "" || escaped) line_malformed = 1
		return count
	}
	function is_cache_control(token) {
		token = unquote_token(token)
		sub(/^[+?>-]*/, "", token)
		token = unquote_token(token)
		return ascii_lower(token) == "cache-control"
	}
	function has_inline_cache_control(line, directive,    count, i) {
		count = tokenize(line)
		if (ascii_lower(unquote_token(tokens[1])) != directive) return 0
		for (i = 2; i <= count; i++) {
			if (is_cache_control(tokens[i])) return 1
		}
		return 0
	}
	function is_header_block(line,    count, matcher) {
		count = tokenize(line)
		if (ascii_lower(unquote_token(tokens[1])) != "header" || tokens[count] != "{") return 0
		if (count == 2) return 1
		if (ascii_lower(unquote_token(tokens[count - 1])) == "defer") count--
		if (count != 3) return 0
		matcher = unquote_token(tokens[2])
		return matcher == "*" || matcher ~ /^\// || matcher ~ /^@[[:alnum:]_-]+$/
	}
	function is_named_matcher_block(line,    count) {
		count = tokenize(line)
		return unquote_token(tokens[1]) ~ /^@[[:alnum:]_-]+$/ && tokens[count] == "{"
	}
	function is_import_directive(line) {
		tokenize(line)
		return ascii_lower(unquote_token(tokens[1])) == "import"
	}
	function scan_line(line,    i, c, quote, escaped, out) {
		brace_delta = 0
		line_malformed = 0
		for (i = 1; i <= length(line); i++) {
			c = substr(line, i, 1)
			if (escaped) { out = out c; escaped = 0; continue }
			if (c == "\\" && quote == "\"") { out = out c; escaped = 1; continue }
			if ((c == "\"" || c == "`") && quote == "") { quote = c; out = out c; continue }
			if (c == quote) { quote = ""; out = out c; continue }
			if (c == "#" && quote == "") break
			if (quote == "" && c == "{") brace_delta++
			if (quote == "" && c == "}") brace_delta--
			out = out c
		}
		if (quote != "" || escaped) line_malformed = 1
		return out
	}
	{
		line = scan_line($0)
		trimmed = line
		sub(/^[[:space:]]+/, "", trimmed)
		sub(/[[:space:]]+$/, "", trimmed)
		if (trimmed == "") next
		if (line_malformed) malformed = 1
		if (is_import_directive(trimmed)) found = 1

		if (header_depth && depth >= header_depth) {
			tokenize(trimmed)
			if (is_cache_control(tokens[1])) found = 1
		}
		if (!matcher_depth && has_inline_cache_control(trimmed, "header_down")) found = 1
		if (!matcher_depth && has_inline_cache_control(trimmed, "header")) found = 1

		if (!matcher_depth && is_named_matcher_block(trimmed)) matcher_depth = depth + 1
		if (!matcher_depth && is_header_block(trimmed)) header_depth = depth + 1
		depth += brace_delta
		if (depth < 0) malformed = 1
		if (header_depth && depth < header_depth) header_depth = 0
		if (matcher_depth && depth < matcher_depth) matcher_depth = 0
	}
	END {
		if (depth != 0 || header_depth || matcher_depth) malformed = 1
		exit (found || malformed) ? 1 : 0
	}
	' "$1"
}

validate_sse_compression_policy() {
	awk '
	function trim(value) {
		sub(/^[[:space:]]+/, "", value)
		sub(/[[:space:]]+$/, "", value)
		return value
	}
	function brace_count(value, token,    copy, count) {
		copy = value
		while ((position = index(copy, token)) > 0) {
			count++
			copy = substr(copy, position + 1)
		}
		return count
	}
	BEGIN {
		required["text/css*"] = 1
		required["text/csv*"] = 1
		required["text/html*"] = 1
		required["text/javascript*"] = 1
		required["text/markdown*"] = 1
		required["text/plain*"] = 1
		required["text/xml*"] = 1
		required["application/json*"] = 1
		required["application/javascript*"] = 1
		required["application/xml*"] = 1
		required["application/rss+xml*"] = 1
		required["image/svg+xml*"] = 1
	}
	{
		line = $0
		sub(/[[:space:]]*#.*/, "", line)
		line = trim(line)
		if (line == "") next

		split(line, fields, /[[:space:]]+/)
		if (fields[1] == "flush_interval") invalid = 1
		if (fields[1] == "encode") {
			encode_count++
			if (in_encode || index(line, "{") == 0) invalid = 1
			in_encode = 1
			encode_depth = brace_count(line, "{") - brace_count(line, "}")
			next
		}
		if (!in_encode) next

		if (fields[1] == "header" && fields[2] == "Content-Type") {
			mime = fields[3]
			if (!(mime in required) || mime == "text/*" || mime ~ /^text\/event-stream/) invalid = 1
			seen[mime]++
		}
		encode_depth += brace_count(line, "{") - brace_count(line, "}")
		if (encode_depth <= 0) in_encode = 0
	}
	END {
		for (mime in required) if (seen[mime] != 1) invalid = 1
		if (encode_count != 1 || in_encode) invalid = 1
		exit invalid ? 1 : 0
	}
	' "$1"
}

run_compression_self_tests() {
	tmp_dir=$(mktemp -d)
	trap 'rm -rf "$tmp_dir"' EXIT HUP INT TERM
	cat >"$tmp_dir/valid" <<'EOF'
example.com {
	encode {
		zstd
		gzip 6
		minimum_length 256
		match {
			header Content-Type text/css*
			header Content-Type text/csv*
			header Content-Type text/html*
			header Content-Type text/javascript*
			header Content-Type text/markdown*
			header Content-Type text/plain*
			header Content-Type text/xml*
			header Content-Type application/json*
			header Content-Type application/javascript*
			header Content-Type application/xml*
			header Content-Type application/rss+xml*
			header Content-Type image/svg+xml*
		}
	}
}
EOF
	validate_sse_compression_policy "$tmp_dir/valid" || {
		echo "Caddy compression guard rejected the canonical non-SSE policy" >&2
		exit 1
	}

	sed 's#text/plain\*#text/*#' "$tmp_dir/valid" >"$tmp_dir/invalid"
	if validate_sse_compression_policy "$tmp_dir/invalid"; then
		echo "Caddy compression guard accepted text/*" >&2
		exit 1
	fi
	sed '/text\/plain\*/a\			header Content-Type text/event-stream*' "$tmp_dir/valid" >"$tmp_dir/invalid"
	if validate_sse_compression_policy "$tmp_dir/invalid"; then
		echo "Caddy compression guard accepted text/event-stream" >&2
		exit 1
	fi
	sed '/application\/json\*/d' "$tmp_dir/valid" >"$tmp_dir/invalid"
	if validate_sse_compression_policy "$tmp_dir/invalid"; then
		echo "Caddy compression guard accepted a reduced compression allowlist" >&2
		exit 1
	fi
	cp "$tmp_dir/valid" "$tmp_dir/invalid"
	printf 'flush_interval -1\n' >>"$tmp_dir/invalid"
	if validate_sse_compression_policy "$tmp_dir/invalid"; then
		echo "Caddy compression guard accepted a forced flush interval" >&2
		exit 1
	fi

	rm -rf "$tmp_dir"
	trap - EXIT HUP INT TERM
}

run_self_tests() {
	tmp_dir=$(mktemp -d)
	trap 'rm -rf "$tmp_dir"' EXIT HUP INT TERM

	cat >"$tmp_dir/allowed" <<'EOF'
# header Cache-Control "no-store"
example.com {
	@api {
		header Cache-Control no-cache
		header X-Open "{"
		header X-Close "}"
		header X-Escaped "\{\}"
	}
	# Comments with unmatched braces are ignored: { { }
	header X-Cache-Control "documentation # retained"
	header X-Literal "{ }"
	reverse_proxy localhost:8080 {
		header_up Cache-Control "request directive is unrelated"
		header_up `Cache-Control` `request { value } # retained`
	}
	header X-Raw `literal { } # not a comment`
}
EOF
	reject_cache_control_override "$tmp_dir/allowed" || {
		echo "Caddy cache guard rejected legitimate unrelated configuration" >&2
		exit 1
	}

	for directive in \
		'header Cache-Control "no-store"' \
		'header "Cache-Control" no-store' \
		'HeAdEr "cAcHe-CoNtRoL" no-store' \
		'"header" "Cache-Control" no-store' \
		'`header` `Cache-Control` `no-store`' \
		'header `Cache-Control` "no-store"' \
		'`header` "Cache-Control" `no-store`' \
		'header ?"Cache-Control" no-store' \
		'header "+cAcHe-CoNtRoL" no-store' \
		'HeAdEr cache-control "no-store"' \
		'HEADER ?cAcHe-CoNtRoL "no-store"' \
		'header @assets Cache-Control "public, max-age=60"' \
		'hEaDeR @assets +cache-control "public, max-age=60"' \
		'header_down Cache-Control "no-cache"' \
		'header_down "Cache-Control" no-store' \
		'HeAdEr_DoWn "-cAcHe-CoNtRoL" no-store' \
		'"header_down" +"Cache-Control" no-store' \
		'`header_down` -`Cache-Control` `no-store`' \
		'HeAdEr_DoWn -cAcHe-CoNtRoL "no-cache"'
	do
		printf 'example.com {\n\t%s\n}\n' "$directive" >"$tmp_dir/rejected"
		if reject_cache_control_override "$tmp_dir/rejected"; then
			echo "Caddy cache guard accepted response override: $directive" >&2
			exit 1
		fi
	done

	for opener in \
		'header {' \
		'"header" {' \
		'HeAdEr {' \
		'header /assets/* {' \
		'HEADER /assets/* {' \
		'header * {' \
		'header @assets {' \
		'`header` `@assets` {' \
		'header `/assets/*` `defer` {' \
		'hEaDeR @assets DeFeR {' \
		'header @expression defer {' \
		'header /assets/* defer {'
	do
		cat >"$tmp_dir/rejected" <<EOF
example.com {
	$opener
		"?cAcHe-CoNtRoL" "public, max-age=31536000, immutable"
	}
}
EOF
		if reject_cache_control_override "$tmp_dir/rejected"; then
			echo "Caddy cache guard accepted a matched header-block override: $opener" >&2
			exit 1
		fi
	done

	for opener in \
		'header "/assets/*" {' \
		'header "*" {' \
		'header "@assets" {' \
		'HeAdEr "/assets/*" "DeFeR" {'
	do
		cat >"$tmp_dir/rejected" <<EOF
example.com {
	$opener
		"Cache-Control" "public, max-age=31536000, immutable"
	}
}
EOF
		if reject_cache_control_override "$tmp_dir/rejected"; then
			echo "Caddy cache guard accepted a quoted matched header-block override: $opener" >&2
			exit 1
		fi
	done

	cat >"$tmp_dir/allowed" <<'EOF'
# import response_headers.caddy
example.com {
	header X-Directive "import"
	header X-Documentation "use import response_headers.caddy"
	respond "literal import response_headers.caddy"
}
EOF
	reject_cache_control_override "$tmp_dir/allowed" || {
		echo "Caddy cache guard rejected comments or quoted literal import values" >&2
		exit 1
	}

	for directive in \
		'import response_headers.caddy' \
		'ImPoRt response_headers.caddy' \
		'"import" response_headers.caddy' \
		'"ImPoRt" "response_headers.caddy"' \
		'`import` `*.caddy`' \
		'`ImPoRt` "response_headers.caddy"'
	do
		printf '%s\n' "$directive" >"$tmp_dir/rejected"
		if reject_cache_control_override "$tmp_dir/rejected"; then
			echo "Caddy cache guard accepted a top-level import: $directive" >&2
			exit 1
		fi

		printf 'example.com {\n\troute {\n\t\t%s\n\t}\n}\n' "$directive" >"$tmp_dir/rejected"
		if reject_cache_control_override "$tmp_dir/rejected"; then
			echo "Caddy cache guard accepted a nested import: $directive" >&2
			exit 1
		fi
	done

	cat >"$tmp_dir/allowed" <<'EOF'
example.com {
	@request {
		header cache-control request-matcher-value
	}
	reverse_proxy localhost:8080 {
		HeAdEr_Up CaChE-CoNtRoL "request header is unrelated"
		"header_up" "Cache-Control" "quoted request directive is unrelated"
		`header_up` `Cache-Control` `raw request directive # unrelated { }`
		HeAdEr_DoWn X-Cache-Control "unrelated response header"
	}
	@quoted_request {
		"header" "Cache-Control" request-matcher-value
	}
	`@raw_request` {
		`header` `Cache-Control` `raw matcher value`
	}
	header X-Cache-Control "unrelated inline response header"
	header /assets/* {
		X-Cache-Control "unrelated block response header"
	}
}
EOF
	reject_cache_control_override "$tmp_dir/allowed" || {
		echo "Caddy cache guard rejected case-insensitive false-positive controls" >&2
		exit 1
	}

	for quoted_braces in '"{"' '"}"' '"\{\}"'
	do
		cat >"$tmp_dir/rejected" <<EOF
example.com {
	@literal {
		header X-Literal $quoted_braces
	}
	header Cache-Control "no-store"
}
EOF
		if reject_cache_control_override "$tmp_dir/rejected"; then
			echo "Caddy cache guard accepted an inline override hidden by quoted braces: $quoted_braces" >&2
			exit 1
		fi

		cat >"$tmp_dir/rejected" <<EOF
example.com {
	@literal {
		header X-Literal $quoted_braces
	}
	header {
		Cache-Control "no-store"
	}
}
EOF
		if reject_cache_control_override "$tmp_dir/rejected"; then
			echo "Caddy cache guard accepted a header-block override hidden by quoted braces: $quoted_braces" >&2
			exit 1
		fi

		cat >"$tmp_dir/rejected" <<EOF
example.com {
	@literal {
		header X-Literal $quoted_braces
	}
	reverse_proxy localhost:8080 {
		header_down Cache-Control "no-store"
	}
}
EOF
		if reject_cache_control_override "$tmp_dir/rejected"; then
			echo "Caddy cache guard accepted a header_down override hidden by quoted braces: $quoted_braces" >&2
			exit 1
		fi
	done

	cat >"$tmp_dir/rejected" <<'EOF'
example.com {
	header {
		"Cache-Control" no-store
	}
}
EOF
	if reject_cache_control_override "$tmp_dir/rejected"; then
		echo "Caddy cache guard accepted a header-block Cache-Control override" >&2
		exit 1
	fi

	printf 'example.com {\r\n\theader_down "Cache-Control" no-store\r\n}\r\n' >"$tmp_dir/rejected"
	if reject_cache_control_override "$tmp_dir/rejected"; then
		echo "Caddy cache guard accepted a quoted CRLF response override" >&2
		exit 1
	fi

	printf 'example.com {\r\n\t`header` `Cache-Control` `no-store`\r\n}\r\n' >"$tmp_dir/rejected"
	if reject_cache_control_override "$tmp_dir/rejected"; then
		echo "Caddy cache guard accepted a raw-string CRLF response override" >&2
		exit 1
	fi

	cat >"$tmp_dir/rejected" <<'EOF'
example.com {
	@request {
		header X-Request value
	# Deliberately unclosed: later response directives must not be suppressed.
	header Cache-Control "no-store"
}
EOF
	if reject_cache_control_override "$tmp_dir/rejected"; then
		echo "Caddy cache guard accepted an unclosed matcher that suppresses later detection" >&2
		exit 1
	fi

	cat >"$tmp_dir/rejected" <<'EOF'
example.com {
	header /assets/* {
		X-Literal "quoted braces: { } # still quoted"
	# Deliberately unclosed header block must fail closed.
}
EOF
	if reject_cache_control_override "$tmp_dir/rejected"; then
		echo "Caddy cache guard accepted an unclosed header block" >&2
		exit 1
	fi

	printf 'example.com {\n\theader X-Literal "unterminated import\n}\n' >"$tmp_dir/rejected"
	if reject_cache_control_override "$tmp_dir/rejected"; then
		echo "Caddy cache guard accepted malformed quote state" >&2
		exit 1
	fi

	for malformed_raw in \
		'header X-Literal `unterminated # { }' \
		'`header Cache-Control no-store' \
		'header `Cache-Control no-store'
	do
		printf 'example.com {\n\t%s\n}\n' "$malformed_raw" >"$tmp_dir/rejected"
		if reject_cache_control_override "$tmp_dir/rejected"; then
			echo "Caddy cache guard accepted malformed raw-string state: $malformed_raw" >&2
			exit 1
		fi
	done
}

run_caddy_adaptation_tests() {
	command -v caddy >/dev/null 2>&1 || return 0

	tmp_dir=$(mktemp -d)
	trap 'rm -rf "$tmp_dir"' EXIT HUP INT TERM
	for matcher in '"/assets/*"' '"*"' '"@assets"'
	do
		cat >"$tmp_dir/Caddyfile" <<EOF
:8080 {
	@assets path /assets/*
	header $matcher {
		Cache-Control "public, max-age=31536000, immutable"
	}
	respond "ok"
}
EOF
		caddy adapt --config "$tmp_dir/Caddyfile" --adapter caddyfile >/dev/null
	done
	cat >"$tmp_dir/Caddyfile" <<'EOF'
:8080 {
	`header` `Cache-Control` `no-store`
	respond `ok # { }`
}
EOF
	caddy adapt --config "$tmp_dir/Caddyfile" --adapter caddyfile >"$tmp_dir/adapted.json"
	grep -q 'Cache-Control' "$tmp_dir/adapted.json"
	grep -q 'no-store' "$tmp_dir/adapted.json"

	cat >"$tmp_dir/response_headers.caddy" <<'EOF'
header Cache-Control "public, max-age=31536000, immutable"
EOF
	cat >"$tmp_dir/Caddyfile" <<'EOF'
:8080 {
	import `*.caddy`
	respond "ok"
}
EOF
	caddy adapt --config "$tmp_dir/Caddyfile" --adapter caddyfile >/dev/null
	rm -rf "$tmp_dir"
	trap - EXIT HUP INT TERM
}

run_compression_self_tests
run_self_tests
run_caddy_adaptation_tests

if ! validate_sse_compression_policy "$caddyfile"; then
	echo "Caddyfile compression must use the non-SSE text MIME allowlist without a forced flush interval" >&2
	exit 1
fi

if ! reject_cache_control_override "$caddyfile"; then
	echo "Caddyfile must not override Cache-Control response headers; the backend owns asset cache policy" >&2
	exit 1
fi

active_config=$(sed 's/[[:space:]]*#.*$//' "$caddyfile")

if ! printf '%s\n' "$active_config" | grep -Eq '^[[:space:]]*reverse_proxy[[:space:]]+localhost:8080'; then
	echo "Caddyfile must continue proxying all application routes to localhost:8080" >&2
	exit 1
fi

echo "Caddyfile preserves backend cache policy, SSE streaming, and reverse_proxy routing"
