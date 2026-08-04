package httputil

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"math/rand"
	"net/http"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestNormalizeLenientJSONRequestBody(t *testing.T) {
	valid := []byte(`{"value":"valid\\njson"}`)
	got, err := NormalizeLenientJSONRequestBody(valid, 1024)
	if err != nil || &got[0] != &valid[0] {
		t.Fatalf("valid JSON must be zero-copy: got %q, err %v", got, err)
	}

	tests := []struct {
		name    string
		body    string
		want    string
		wantErr error
	}{
		{"even slashes permit repair", "{\"v\":\"\\\\\x01\"}", "{\"v\":\"\\\\\\u0001\"}", nil},
		{"odd slash leaves escape pending", "{\"v\":\"\\\x01\"}", "", errRawJSONControlAfterEscape},
		{"three slashes leave escape pending", "{\"v\":\"\\\\\\\x01\"}", "", errRawJSONControlAfterEscape},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeLenientJSONRequestBody([]byte(tt.body), 1024)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if err == nil && string(got) != tt.want {
				t.Fatalf("body = %q, want %q", got, tt.want)
			}
		})
	}
}

// normalizeLenientJSONRequestBodyLegacy 是预扫描优化前的逐字节状态机实现，
// 仅作为差分测试的参照基准，验证快路径与原实现对所有输入行为一致。
func normalizeLenientJSONRequestBodyLegacy(body []byte, limit int64) ([]byte, error) {
	if int64(len(body)) > limit {
		return nil, &http.MaxBytesError{Limit: limit}
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

func assertSameNormalizeResult(t *testing.T, got, want []byte, gotErr, wantErr error) {
	t.Helper()
	if (gotErr == nil) != (wantErr == nil) {
		t.Fatalf("error 不一致: got %v, want %v", gotErr, wantErr)
	}
	if gotErr != nil {
		var gotMax, wantMax *http.MaxBytesError
		switch {
		case errors.As(gotErr, &gotMax) && errors.As(wantErr, &wantMax):
			if gotMax.Limit != wantMax.Limit {
				t.Fatalf("MaxBytesError limit 不一致: got %d, want %d", gotMax.Limit, wantMax.Limit)
			}
		case errors.Is(gotErr, errRawJSONControlAfterEscape) && errors.Is(wantErr, errRawJSONControlAfterEscape):
		default:
			t.Fatalf("error 类型不一致: got %v, want %v", gotErr, wantErr)
		}
		return
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("body 不一致: got %q, want %q", got, want)
	}
}

func TestNormalizeLenientJSONRequestBodyDifferential(t *testing.T) {
	// 手工构造的语料：覆盖字符串边界、转义序列、Unicode、字符串外控制字节等
	corpus := []struct {
		name string
		body []byte
	}{
		{"valid-json", []byte(`{"a":"b","n":1}`)},
		{"valid-with-backslash", []byte(`{"v":"a\\b\"c"}`)},
		{"control-in-string", []byte("{\"v\":\"\x01\"}")},
		{"control-in-string-0a", []byte("{\"v\":\"\n\"}")},
		{"control-at-string-start", []byte("{\"v\":\"\x01x\"}")},
		{"control-at-string-end", []byte("{\"v\":\"x\x01\"}")},
		{"control-only-in-string", []byte("\"\x00\"")},
		{"control-outside-string", []byte("{\"v\":\"x\"}\x01")},
		{"control-before-string", []byte("\x01{\"v\":\"x\"}")},
		{"even-slashes-repair", []byte("{\"v\":\"\\\\\x01\"}")},
		{"odd-slashes-error", []byte("{\"v\":\"\\\x01\"}")},
		{"three-slashes-error", []byte("{\"v\":\"\\\\\\\x01\"}")},
		{"escape-then-control", []byte("{\"v\":\"\\n\x01\"}")},
		{"escaped-quote-then-control", []byte("{\"v\":\"\\\"\x01\"}")},
		{"unicode-utf8", []byte("{\"v\":\"你好é\"}")},
		{"high-bytes-invalid-utf8", []byte("{\"v\":\"\x80\xff\"}")},
		{"high-bytes-plus-control", []byte("{\"v\":\"\xc3\xa9\x01\"}")},
		{"empty-string", []byte(`""`)},
		{"empty-object", []byte(`{}`)},
		{"empty-body", []byte{}},
		{"only-control", []byte{0x00}},
		{"control-at-body-end", []byte("{\"v\":\"x\x01")},
		{"escaped-control-json-then-raw", []byte("{\"v\":\"\\u0001\x01\"}")},
		{"control-between-escapes", []byte("{\"v\":\"\\\\\x01\\\\\x02\"}")},
		{"control-at-end-of-long-body", []byte(strings.Repeat(`{"a":"b"},`, 900) + "{\"v\":\"x\x01\"}")},
		{"control-at-start-of-long-body", []byte("\x01" + strings.Repeat(`{"a":"b"},`, 900))},
	}
	for _, tt := range corpus {
		t.Run(tt.name, func(t *testing.T) {
			for _, limit := range []int64{1 << 30, int64(len(tt.body)), int64(len(tt.body)) + 3, int64(len(tt.body)) - 1} {
				got, gotErr := NormalizeLenientJSONRequestBody(tt.body, limit)
				want, wantErr := normalizeLenientJSONRequestBodyLegacy(tt.body, limit)
				assertSameNormalizeResult(t, got, want, gotErr, wantErr)
			}
		})
	}

	// 随机语料：混合 ASCII、引号、反斜杠、控制字节、非法 UTF-8 与多字节字符，
	// 覆盖 8 字节词扫描的各类对齐与尾部情况
	rng := rand.New(rand.NewSource(42))
	alphabet := []byte("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 {}[]:\",\\-._/@\t\n\x00\x01\x0b\x0c\x1f\x1a\x7f\x80\xc3\xa9\xe4\xbd\xa0")
	for n := 0; n < 4000; n++ {
		body := make([]byte, rng.Intn(96))
		for i := range body {
			body[i] = alphabet[rng.Intn(len(alphabet))]
		}
		if n%50 == 0 {
			body = append(body, bytes.Repeat([]byte(`{"x":"y"}`), rng.Intn(64))...)
		}
		for _, limit := range []int64{1 << 30, int64(len(body)), int64(len(body)) + 3, int64(len(body)) - 1} {
			got, gotErr := NormalizeLenientJSONRequestBody(body, limit)
			want, wantErr := normalizeLenientJSONRequestBodyLegacy(body, limit)
			assertSameNormalizeResult(t, got, want, gotErr, wantErr)
		}
	}
}

func TestReadLenientJSONRequestBodyCompressedLimit(t *testing.T) {
	payload := []byte(`{"value":"0123456789"}`)
	encoders := map[string]func(*bytes.Buffer) error{
		"gzip": func(dst *bytes.Buffer) error {
			w := gzip.NewWriter(dst)
			_, _ = w.Write(payload)
			return w.Close()
		},
		"deflate": func(dst *bytes.Buffer) error {
			w := zlib.NewWriter(dst)
			_, _ = w.Write(payload)
			return w.Close()
		},
		"zstd": func(dst *bytes.Buffer) error {
			w, err := zstd.NewWriter(dst)
			if err != nil {
				return err
			}
			_, _ = w.Write(payload)
			return w.Close()
		},
	}
	for encoding, encode := range encoders {
		t.Run(encoding, func(t *testing.T) {
			var compressed bytes.Buffer
			if err := encode(&compressed); err != nil {
				t.Fatal(err)
			}
			req, err := http.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(compressed.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Encoding", encoding)
			_, err = ReadLenientJSONRequestBodyWithPrealloc(req, int64(len(payload)-1))
			var maxErr *http.MaxBytesError
			if !errors.As(err, &maxErr) || maxErr.Limit != int64(len(payload)-1) {
				t.Fatalf("error = %#v, want MaxBytesError limit %d", err, len(payload)-1)
			}
		})
	}
}
