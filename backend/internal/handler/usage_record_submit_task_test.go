package handler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func newUsageRecordTestPool(t *testing.T) *service.UsageRecordWorkerPool {
	t.Helper()
	pool := service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{
		WorkerCount:           1,
		QueueSize:             8,
		TaskTimeout:           time.Second,
		OverflowPolicy:        "drop",
		OverflowSamplePercent: 0,
		AutoScaleEnabled:      false,
	})
	t.Cleanup(pool.Stop)
	return pool
}

func TestWrapUsageRecordTaskContextSnapshotsRequestIDs(t *testing.T) {
	parent := &mutableUsageRecordValueContext{
		Context: context.Background(),
		values: map[any]any{
			ctxkey.ClientRequestID: " client-before ",
			ctxkey.RequestID:       " request-before ",
		},
	}
	var clientRequestID, requestID string
	wrapped := wrapUsageRecordTaskContext(parent, func(ctx context.Context) {
		clientRequestID, _ = ctx.Value(ctxkey.ClientRequestID).(string)
		requestID, _ = ctx.Value(ctxkey.RequestID).(string)
	})
	parent.values[ctxkey.ClientRequestID] = "client-after"
	parent.values[ctxkey.RequestID] = "request-after"

	wrapped(context.Background())

	require.Equal(t, "client-before", clientRequestID)
	require.Equal(t, "request-before", requestID)
}

type mutableUsageRecordValueContext struct {
	context.Context
	values map[any]any
}

func (c *mutableUsageRecordValueContext) Value(key any) any {
	return c.values[key]
}

func TestGatewayHandlerSubmitUsageRecordTask_WithPool(t *testing.T) {
	pool := newUsageRecordTestPool(t)
	h := &GatewayHandler{usageRecordWorkerPool: pool}

	done := make(chan struct{})
	h.submitUsageRecordTask(context.Background(), func(ctx context.Context) {
		close(done)
	})

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("task not executed")
	}
}

func TestGatewayHandlerSubmitUsageRecordTask_WithoutPoolSyncFallback(t *testing.T) {
	h := &GatewayHandler{}
	var called atomic.Bool

	h.submitUsageRecordTask(context.Background(), func(ctx context.Context) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("expected deadline in fallback context")
		}
		called.Store(true)
	})

	require.True(t, called.Load())
}

func TestGatewayHandlerSubmitUsageRecordTask_NilTask(t *testing.T) {
	h := &GatewayHandler{}
	require.NotPanics(t, func() {
		h.submitUsageRecordTask(context.Background(), nil)
	})
}

func TestGatewayHandlerSubmitUsageRecordTask_WithoutPool_TaskPanicRecovered(t *testing.T) {
	h := &GatewayHandler{}
	var called atomic.Bool

	require.NotPanics(t, func() {
		h.submitUsageRecordTask(context.Background(), func(ctx context.Context) {
			panic("usage task panic")
		})
	})

	h.submitUsageRecordTask(context.Background(), func(ctx context.Context) {
		called.Store(true)
	})
	require.True(t, called.Load(), "panic 后后续任务应仍可执行")
}

func TestGatewayHandlerSubmitUsageRecordTask_StoppedPoolFallsBackExactlyOnce(t *testing.T) {
	pool := newUsageRecordTestPool(t)
	pool.Stop()
	h := &GatewayHandler{usageRecordWorkerPool: pool}
	var calls atomic.Int32

	h.submitUsageRecordTask(context.Background(), func(ctx context.Context) {
		calls.Add(1)
	})

	require.Equal(t, int32(1), calls.Load())
}

func TestOpenAIGatewayHandlerSubmitUsageRecordTask_WithPool(t *testing.T) {
	pool := newUsageRecordTestPool(t)
	h := &OpenAIGatewayHandler{usageRecordWorkerPool: pool}

	done := make(chan struct{})
	h.submitUsageRecordTask(context.Background(), func(ctx context.Context) {
		close(done)
	})

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("task not executed")
	}
}

func TestOpenAIGatewayHandlerSubmitUsageRecordTask_StoppedPoolFallsBackExactlyOnce(t *testing.T) {
	pool := newUsageRecordTestPool(t)
	pool.Stop()
	h := &OpenAIGatewayHandler{usageRecordWorkerPool: pool}
	var calls atomic.Int32

	h.submitUsageRecordTask(context.Background(), func(ctx context.Context) {
		calls.Add(1)
	})

	require.Equal(t, int32(1), calls.Load())
}

func TestOpenAIGatewayHandlerSubmitUsageRecordTask_WithoutPoolSyncFallback(t *testing.T) {
	h := &OpenAIGatewayHandler{}
	var called atomic.Bool

	h.submitUsageRecordTask(context.Background(), func(ctx context.Context) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("expected deadline in fallback context")
		}
		called.Store(true)
	})

	require.True(t, called.Load())
}

func TestOpenAIGatewayHandlerSubmitUsageRecordTask_NilTask(t *testing.T) {
	h := &OpenAIGatewayHandler{}
	require.NotPanics(t, func() {
		h.submitUsageRecordTask(context.Background(), nil)
	})
}

func TestOpenAIGatewayHandlerSubmitUsageRecordTask_WithoutPool_TaskPanicRecovered(t *testing.T) {
	h := &OpenAIGatewayHandler{}
	var called atomic.Bool

	require.NotPanics(t, func() {
		h.submitUsageRecordTask(context.Background(), func(ctx context.Context) {
			panic("usage task panic")
		})
	})

	h.submitUsageRecordTask(context.Background(), func(ctx context.Context) {
		called.Store(true)
	})
	require.True(t, called.Load(), "panic 后后续任务应仍可执行")
}

func newSaturatedUsageRecordTestPool(t *testing.T, policy string, samplePercent int) (*service.UsageRecordWorkerPool, chan struct{}) {
	t.Helper()
	pool := service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{
		WorkerCount:           1,
		QueueSize:             1,
		TaskTimeout:           time.Second,
		OverflowPolicy:        policy,
		OverflowSamplePercent: samplePercent,
		AutoScaleEnabled:      false,
	})
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		pool.Stop()
	})
	started := make(chan struct{})
	require.Equal(t, service.UsageRecordSubmitModeEnqueued, pool.Submit(func(ctx context.Context) {
		close(started)
		<-release
	}))
	<-started
	require.Equal(t, service.UsageRecordSubmitModeEnqueued, pool.Submit(func(ctx context.Context) {}))
	return pool, release
}

func TestMandatoryUsageRecordTask_QueueFullDropFallsBackExactlyOnce(t *testing.T) {
	pool, release := newSaturatedUsageRecordTestPool(t, config.UsageRecordOverflowPolicyDrop, 0)
	h := &GatewayHandler{usageRecordWorkerPool: pool}
	var calls atomic.Int32

	h.submitUsageRecordTask(context.Background(), func(ctx context.Context) {
		calls.Add(1)
	})
	close(release)

	require.Equal(t, int32(1), calls.Load())
}

func TestMandatoryUsageRecordTask_EnqueuedExecutesExactlyOnce(t *testing.T) {
	pool := newUsageRecordTestPool(t)
	h := &GatewayHandler{usageRecordWorkerPool: pool}
	var calls atomic.Int32
	done := make(chan struct{})

	h.submitUsageRecordTask(context.Background(), func(ctx context.Context) {
		if calls.Add(1) == 1 {
			close(done)
		}
	})
	<-done
	pool.Stop()

	require.Equal(t, int32(1), calls.Load())
}

func TestMandatoryUsageRecordTask_FallbackUsesPoolTimeoutAndPanicIsolation(t *testing.T) {
	pool := service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{
		WorkerCount: 1, QueueSize: 1, TaskTimeout: 20 * time.Millisecond,
		OverflowPolicy: config.UsageRecordOverflowPolicyDrop, AutoScaleEnabled: false,
	})
	pool.Stop()
	h := &GatewayHandler{usageRecordWorkerPool: pool}

	require.NotPanics(t, func() {
		h.submitUsageRecordTask(context.Background(), func(ctx context.Context) { panic("boom") })
	})
	timedOut := make(chan struct{})
	h.submitUsageRecordTask(context.Background(), func(ctx context.Context) {
		<-ctx.Done()
		close(timedOut)
	})
	select {
	case <-timedOut:
	default:
		t.Fatal("fallback did not use the configured pool timeout")
	}
}

func TestOpenAIGatewayHandlerSubmitMandatoryUsageRecordTask_DroppedTaskSyncFallback(t *testing.T) {
	pool := service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{
		WorkerCount:           1,
		QueueSize:             1,
		TaskTimeout:           time.Second,
		OverflowPolicy:        "drop",
		OverflowSamplePercent: 0,
		AutoScaleEnabled:      false,
	})
	t.Cleanup(pool.Stop)
	h := &OpenAIGatewayHandler{usageRecordWorkerPool: pool}

	block := make(chan struct{})
	release := make(chan struct{})
	pool.Submit(func(ctx context.Context) {
		close(block)
		<-release
	})
	<-block
	pool.Submit(func(ctx context.Context) {})

	var called atomic.Bool
	h.submitMandatoryUsageRecordTask(context.Background(), func(ctx context.Context) {
		called.Store(true)
	})
	close(release)

	require.True(t, called.Load(), "mandatory usage task must run synchronously when async submit is dropped")
}

func TestOpenAIGatewayHandlerSubmitOpenAIUsageRecordTask_WebSocketTokenResultUsesMandatoryFallback(t *testing.T) {
	pool := newUsageRecordTestPool(t)
	pool.Stop()
	h := &OpenAIGatewayHandler{usageRecordWorkerPool: pool}
	var calls atomic.Int32

	h.submitOpenAIUsageRecordTask(context.Background(), &service.OpenAIForwardResult{}, func(ctx context.Context) {
		calls.Add(1)
	})

	require.Equal(t, int32(1), calls.Load())
	require.Equal(t, uint64(1), pool.Stats().DroppedPoolStopped)
}

func TestOpenAIGatewayHandlerSubmitOpenAIUsageRecordTask_ImageResultUsesMandatoryFallback(t *testing.T) {
	pool := service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{
		WorkerCount:           1,
		QueueSize:             1,
		TaskTimeout:           time.Second,
		OverflowPolicy:        "drop",
		OverflowSamplePercent: 0,
		AutoScaleEnabled:      false,
	})
	t.Cleanup(pool.Stop)
	h := &OpenAIGatewayHandler{usageRecordWorkerPool: pool}

	block := make(chan struct{})
	release := make(chan struct{})
	pool.Submit(func(ctx context.Context) {
		close(block)
		<-release
	})
	<-block
	pool.Submit(func(ctx context.Context) {})

	var called atomic.Bool
	h.submitOpenAIUsageRecordTask(context.Background(), &service.OpenAIForwardResult{ImageCount: 1}, func(ctx context.Context) {
		called.Store(true)
	})
	close(release)

	require.True(t, called.Load(), "image usage task must be mandatory when async submit is dropped")
}
