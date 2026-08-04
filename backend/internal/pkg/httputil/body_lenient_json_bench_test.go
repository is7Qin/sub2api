package httputil

import (
	"bytes"
	"testing"
)

var benchNormalizeSink []byte

func makeNormalizeBenchBodyValid(n int) []byte {
	chunk := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello world this is a test message body with some padding text to make the chunk long enough"}],"temperature":0.7}`)
	body := make([]byte, 0, n)
	for len(body) < n {
		body = append(body, chunk...)
	}
	return body[:n]
}

func makeNormalizeBenchBodyCtrl(n int) []byte {
	body := makeNormalizeBenchBodyValid(n)
	// 在 content 字符串内注入一个原始控制字节，触发转义路径
	i := bytes.Index(body, []byte("test "))
	body[i+4] = 0x01
	return body
}

// BenchmarkNormalizeLenientJSONRequestBody 覆盖两个典型场景：
// valid——严格合法 JSON（生产主路径，应零拷贝零分配）；
// control-byte——字符串内混入原始控制字节（触发 \uXXXX 转义）。
func BenchmarkNormalizeLenientJSONRequestBody(b *testing.B) {
	valid := makeNormalizeBenchBodyValid(4096)
	ctrl := makeNormalizeBenchBodyCtrl(4096)
	b.Run("valid", func(b *testing.B) {
		b.SetBytes(int64(len(valid)))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			out, err := NormalizeLenientJSONRequestBody(valid, 1<<30)
			if err != nil {
				b.Fatal(err)
			}
			benchNormalizeSink = out
		}
	})
	b.Run("control-byte", func(b *testing.B) {
		b.SetBytes(int64(len(ctrl)))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			out, err := NormalizeLenientJSONRequestBody(ctrl, 1<<30)
			if err != nil {
				b.Fatal(err)
			}
			benchNormalizeSink = out
		}
	})
}
