package httputil

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/klauspost/compress/zstd"
)

const (
	requestBodyReadInitCap    = 512
	requestBodyReadMaxInitCap = 1 << 20
	// maxDecompressedBodySize limits the decompressed request body to 64 MB
	// to prevent decompression bomb attacks.
	maxDecompressedBodySize = 64 << 20
)

var errRawJSONControlAfterEscape = errors.New("raw JSON control character after escape")

// ReadRequestBodyWithPrealloc reads request body with preallocated buffer based
// on content length, transparently decoding any Content-Encoding the upstream
// client used to compress the body (zstd, gzip, deflate).
func ReadRequestBodyWithPrealloc(req *http.Request) ([]byte, error) {
	return readRequestBodyWithPrealloc(req, maxDecompressedBodySize)
}

// ReadLenientJSONRequestBodyWithPrealloc reads a JSON request and escapes raw
// control bytes inside strings. Strictly valid JSON is returned without a copy.
func ReadLenientJSONRequestBodyWithPrealloc(req *http.Request, decodedLimit int64) ([]byte, error) {
	limit := effectiveDecodedLimit(decodedLimit)
	body, err := readRequestBodyWithPrealloc(req, limit)
	if err != nil {
		return nil, err
	}
	return NormalizeLenientJSONRequestBody(body, limit)
}

func readRequestBodyWithPrealloc(req *http.Request, decodedLimit int64) ([]byte, error) {
	if req == nil || req.Body == nil {
		return nil, nil
	}

	capHint := requestBodyReadInitCap
	if req.ContentLength > 0 {
		switch {
		case req.ContentLength < int64(requestBodyReadInitCap):
			capHint = requestBodyReadInitCap
		case req.ContentLength > int64(requestBodyReadMaxInitCap):
			capHint = requestBodyReadMaxInitCap
		default:
			capHint = int(req.ContentLength)
		}
	}

	enc := strings.ToLower(strings.TrimSpace(req.Header.Get("Content-Encoding")))
	var source io.Reader = req.Body
	if enc == "" || enc == "identity" {
		source = io.LimitReader(req.Body, decodedLimit+1)
	}
	buf := bytes.NewBuffer(make([]byte, 0, capHint))
	if _, err := io.Copy(buf, source); err != nil {
		return nil, err
	}
	raw := buf.Bytes()

	if enc == "" || enc == "identity" {
		if int64(len(raw)) > decodedLimit {
			return nil, &http.MaxBytesError{Limit: decodedLimit}
		}
		return raw, nil
	}

	decoded, err := decompressRequestBodyLimited(enc, raw, decodedLimit)
	if err != nil {
		return nil, fmt.Errorf("decode Content-Encoding %q: %w", enc, err)
	}

	req.Header.Del("Content-Encoding")
	req.Header.Del("Content-Length")
	req.ContentLength = int64(len(decoded))

	return decoded, nil
}

func decompressRequestBody(encoding string, raw []byte) ([]byte, error) {
	return decompressRequestBodyLimited(encoding, raw, maxDecompressedBodySize)
}

func decompressRequestBodyLimited(encoding string, raw []byte, decodedLimit int64) ([]byte, error) {
	readLimited := func(r io.Reader) ([]byte, error) {
		decoded, err := io.ReadAll(io.LimitReader(r, decodedLimit+1))
		if err != nil {
			return nil, err
		}
		if int64(len(decoded)) > decodedLimit {
			return nil, &http.MaxBytesError{Limit: decodedLimit}
		}
		return decoded, nil
	}

	switch encoding {
	case "zstd":
		dec, err := zstd.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		defer dec.Close()
		return readLimited(dec)
	case "gzip", "x-gzip":
		gr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		defer func() { _ = gr.Close() }()
		return readLimited(gr)
	case "deflate":
		zr, err := zlib.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		defer func() { _ = zr.Close() }()
		return readLimited(zr)
	default:
		return nil, errors.New("unsupported Content-Encoding")
	}
}

func effectiveDecodedLimit(configured int64) int64 {
	if configured > 0 && configured < maxDecompressedBodySize {
		return configured
	}
	return maxDecompressedBodySize
}

// NormalizeLenientJSONRequestBody repairs only raw control bytes in JSON
// strings. It intentionally leaves all other malformed JSON for strict parsing.
func NormalizeLenientJSONRequestBody(body []byte, limit int64) ([]byte, error) {
	if int64(len(body)) > limit {
		return nil, &http.MaxBytesError{Limit: limit}
	}

	// 快路径：整段不存在任何原始控制字节（<0x20）时状态机必然空转，直接返回
	// 原切片（零拷贝）。词级预扫描会误报字符串外的控制字节，但仅作为存在性
	// 预筛——命中即回退到下面的逐字节状态机按字符串边界精确处理，语义与旧实现
	// 完全一致。
	if !hasRawControlByte(body) {
		return body, nil
	}

	var out []byte
	inString, escaped := false, false
	for i, b := range body {
		if inString && b < 0x20 {
			if escaped {
				return nil, errRawJSONControlAfterEscape
			}
			if out == nil {
				out = make([]byte, 0, len(body))
				out = append(out, body[:i]...)
			}
			if int64(len(out)+6+len(body)-i-1) > limit {
				return nil, &http.MaxBytesError{Limit: limit}
			}
			const hex = "0123456789abcdef"
			out = append(out, '\\', 'u', '0', '0', hex[b>>4], hex[b&0xf])
			continue
		}

		switch {
		case escaped:
			escaped = false
		case inString && b == '\\':
			escaped = true
		case b == '"':
			inString = !inString
		}
		if out != nil {
			out = append(out, b)
		}
	}
	if out != nil {
		return out, nil
	}
	return body, nil
}

// hasRawControlByte 按 8 字节一组快速检测是否存在原始控制字节（<0x20）。
// 经典 SWAR 技巧：对每 8 字节 v，(v-0x2020202020202020)&^v&0x8080808080808080
// 非零当且仅当存在字节 <0x20（&^v 用于排除 >=0x80 字节的减法借位误报）。
func hasRawControlByte(b []byte) bool {
	const (
		less = 0x2020202020202020 // 每字节减 0x20，小于 0x20 时向高位借位
		high = 0x8080808080808080 // 借位结果高位字节为 1 的掩码
	)
	for len(b) >= 8 {
		v := binary.LittleEndian.Uint64(b)
		if (v-less)&^v&high != 0 {
			return true
		}
		b = b[8:]
	}
	for _, c := range b {
		if c < 0x20 {
			return true
		}
	}
	return false
}
