package service

import (
	"net/http/httptest"
	"runtime"
	"testing"

	"github.com/gin-gonic/gin"
)

var benchmarkOpsUpstreamContextsSink []*gin.Context

func BenchmarkOpsUpstreamRequestBodyRetainedHeap(b *testing.B) {
	gin.SetMode(gin.TestMode)

	const (
		// Keep the active context count modest because the legacy case retains
		// the full 50 MB body per context.
		contextCount = 8
		bodyBytes    = 50 << 20
	)

	cases := []struct {
		name string
		set  func(*gin.Context, []byte)
	}{
		{
			name: "legacy_full_body_context",
			set:  setOpsUpstreamRequestBodyLegacyForBenchmark,
		},
		{
			name: "snapshot_context",
			set:  setOpsUpstreamRequestBody,
		},
	}

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()

			var retainedSum uint64
			for i := 0; i < b.N; i++ {
				benchmarkOpsUpstreamContextsSink = nil
				runtime.GC()
				before := heapAllocForBenchmark()

				contexts := make([]*gin.Context, 0, contextCount)
				for j := 0; j < contextCount; j++ {
					rec := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(rec)
					body := newOpsBenchmarkBody(bodyBytes, i*contextCount+j)
					tc.set(c, body)
					contexts = append(contexts, c)
				}

				benchmarkOpsUpstreamContextsSink = contexts
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

	benchmarkOpsUpstreamContextsSink = nil
	runtime.GC()
}

func setOpsUpstreamRequestBodyLegacyForBenchmark(c *gin.Context, body []byte) {
	if c == nil || len(body) == 0 {
		return
	}
	c.Set(OpsUpstreamRequestBodyKey, body)
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
