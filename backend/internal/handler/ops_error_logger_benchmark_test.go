package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
)

var benchmarkOpsContextsSink []*gin.Context

func BenchmarkOpsRequestContextRetainedHeap(b *testing.B) {
	gin.SetMode(gin.TestMode)

	const (
		// Keep the active context count modest because the legacy case retains
		// the full 50 MB body per context.
		contextCount = 8
		bodyBytes    = 50 << 20
	)

	cases := []struct {
		name string
		set  func(*gin.Context, string, bool, []byte)
	}{
		{
			name: "legacy_full_body_context",
			set:  setOpsRequestContextLegacyForBenchmark,
		},
		{
			name: "snapshot_context",
			set:  setOpsRequestContext,
		},
	}

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()

			var retainedSum uint64
			for i := 0; i < b.N; i++ {
				benchmarkOpsContextsSink = nil
				runtime.GC()
				before := heapAllocForBenchmark()

				contexts := make([]*gin.Context, 0, contextCount)
				for j := 0; j < contextCount; j++ {
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
					body := newOpsBenchmarkBody(bodyBytes, i*contextCount+j)
					tc.set(c, "gpt-5", true, body)
					contexts = append(contexts, c)
				}

				benchmarkOpsContextsSink = contexts
				runtime.GC()
				after := heapAllocForBenchmark()
				if after > before {
					retainedSum += after - before
				}
			}

			retainedPerRun := float64(retainedSum) / float64(b.N)
			b.ReportMetric(retainedPerRun/1024/1024, "retained_MB/run")
			b.ReportMetric(retainedPerRun/contextCount, "retained_B/context")
		})
	}

	benchmarkOpsContextsSink = nil
	runtime.GC()
}

func setOpsRequestContextLegacyForBenchmark(c *gin.Context, model string, stream bool, requestBody []byte) {
	if c == nil {
		return
	}
	model = strings.TrimSpace(model)
	c.Set(opsModelKey, model)
	c.Set(opsStreamKey, stream)
	if len(requestBody) > 0 {
		c.Set(opsRequestBodyKey, requestBody)
	}
	if c.Request != nil && model != "" {
		ctx := context.WithValue(c.Request.Context(), ctxkey.Model, model)
		c.Request = c.Request.WithContext(ctx)
	}
}

func heapAllocForBenchmark() uint64 {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

func newOpsBenchmarkBody(size int, seed int) []byte {
	const prefix = `{"model":"gpt-5","input":"`
	const suffix = `"}`
	if size < len(prefix)+len(suffix) {
		size = len(prefix) + len(suffix)
	}
	body := make([]byte, size)
	copy(body, prefix)
	end := size - len(suffix)
	for i := len(prefix); i < end; i++ {
		body[i] = byte('a' + (seed+i)%26)
	}
	copy(body[end:], suffix)
	return body
}
