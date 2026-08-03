//go:build unit

package service

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// gatewayUsageLogRepoStub 记录 Create 调用，可选择性阻塞并注入错误。
type gatewayUsageLogRepoStub struct {
	UsageLogRepository

	err        error
	calls      atomic.Int32
	lastLog    *UsageLog
	lastCtxErr error
	entered    chan struct{} // 可选：Create 进入时发信号（缓冲 1，非阻塞）
	block      chan struct{} // 可选：Create 进入后阻塞直到关闭
}

func (s *gatewayUsageLogRepoStub) Create(ctx context.Context, usageLog *UsageLog) (bool, error) {
	s.calls.Add(1)
	s.lastLog = usageLog
	s.lastCtxErr = ctx.Err()
	if s.entered != nil {
		select {
		case s.entered <- struct{}{}:
		default:
		}
	}
	if s.block != nil {
		<-s.block
	}
	return false, s.err
}

// newGatewayUsageLogPoolForTest 构建 1 worker 的同步溢流策略池（与生产默认一致）。
func newGatewayUsageLogPoolForTest(t *testing.T) *UsageRecordWorkerPool {
	t.Helper()
	pool := NewUsageRecordWorkerPoolWithOptions(UsageRecordWorkerPoolOptions{
		WorkerCount:           1,
		QueueSize:             8,
		TaskTimeout:           time.Second,
		OverflowPolicy:        config.UsageRecordOverflowPolicySync,
		OverflowSamplePercent: 0,
		AutoScaleEnabled:      false,
	})
	require.NoError(t, pool.Start())
	t.Cleanup(pool.Stop)
	return pool
}

// blockGatewayUsageLogPoolWorker 占用池的唯一 worker，返回放行信号。
func blockGatewayUsageLogPoolWorker(t *testing.T, pool *UsageRecordWorkerPool) chan struct{} {
	t.Helper()
	started := make(chan struct{})
	release := make(chan struct{})
	require.Equal(t, UsageRecordSubmitModeEnqueued, pool.Submit(func(context.Context) {
		close(started)
		<-release
	}))
	<-started
	return release
}

// newSaturatedGatewayUsageLogPoolForTest 构建队列已满、worker 被占用的池。
func newSaturatedGatewayUsageLogPoolForTest(t *testing.T) (*UsageRecordWorkerPool, chan struct{}) {
	t.Helper()
	pool := NewUsageRecordWorkerPoolWithOptions(UsageRecordWorkerPoolOptions{
		WorkerCount:           1,
		QueueSize:             1,
		TaskTimeout:           time.Second,
		OverflowPolicy:        config.UsageRecordOverflowPolicySync,
		OverflowSamplePercent: 0,
		AutoScaleEnabled:      false,
	})
	require.NoError(t, pool.Start())
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
	require.Equal(t, UsageRecordSubmitModeEnqueued, pool.Submit(func(context.Context) {
		close(started)
		<-release
	}))
	<-started
	require.Equal(t, UsageRecordSubmitModeEnqueued, pool.Submit(func(context.Context) {}))
	return pool, release
}

func TestWriteUsageLogBestEffort_EnqueuedWriteRunsAsync(t *testing.T) {
	pool := newGatewayUsageLogPoolForTest(t)
	workerRelease := blockGatewayUsageLogPoolWorker(t, pool)
	repo := &gatewayUsageLogRepoStub{entered: make(chan struct{}, 1)}
	usageLog := &UsageLog{RequestID: "req-async", UserID: 1}

	writeUsageLogBestEffort(context.Background(), pool, repo, usageLog, "test.gateway_usage_log")

	// worker 被占用：任务已入队，请求路径不得同步调用 Create。
	select {
	case <-repo.entered:
		t.Fatal("repo.Create called synchronously; expected async enqueue")
	default:
	}

	close(workerRelease)
	select {
	case <-repo.entered:
	case <-time.After(time.Second):
		t.Fatal("async usage log write never executed")
	}
	require.Equal(t, int32(1), repo.calls.Load())
	require.Same(t, usageLog, repo.lastLog)
}

func TestWriteUsageLogBestEffort_OverflowSyncBlocksAndNeverDrops(t *testing.T) {
	pool, workerRelease := newSaturatedGatewayUsageLogPoolForTest(t)
	repo := &gatewayUsageLogRepoStub{entered: make(chan struct{}, 1)}
	usageLog := &UsageLog{RequestID: "req-overflow", UserID: 2}

	callerDone := make(chan struct{})
	go func() {
		writeUsageLogBestEffort(context.Background(), pool, repo, usageLog, "test.gateway_usage_log")
		close(callerDone)
	}()

	// 队列已满 + sync 溢流策略：调用方阻塞直至池接管任务，Create 不得被丢弃。
	select {
	case <-callerDone:
		t.Fatal("sync overflow expected to block until pool accepts the task")
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case <-repo.entered:
		t.Fatal("repo.Create must not run while the pool worker is occupied")
	default:
	}

	close(workerRelease)
	select {
	case <-callerDone:
	case <-time.After(time.Second):
		t.Fatal("sync overflow submission never completed")
	}
	select {
	case <-repo.entered:
	case <-time.After(time.Second):
		t.Fatal("overflow usage log write never executed")
	}
	require.Equal(t, int32(1), repo.calls.Load())
	require.Same(t, usageLog, repo.lastLog)
}

func TestWriteUsageLogBestEffort_StoppedPoolFallsBackSynchronously(t *testing.T) {
	pool := newGatewayUsageLogPoolForTest(t)
	pool.Stop()
	repo := &gatewayUsageLogRepoStub{}
	usageLog := &UsageLog{RequestID: "req-stopped", UserID: 3}

	writeUsageLogBestEffort(context.Background(), pool, repo, usageLog, "test.gateway_usage_log")

	// 池已停止：Submit 返回 dropped，须同步兜底写入，保证计费记录不丢。
	require.Equal(t, int32(1), repo.calls.Load())
	require.Same(t, usageLog, repo.lastLog)
}

func TestWriteUsageLogBestEffort_NilPoolWritesSynchronously(t *testing.T) {
	repo := &gatewayUsageLogRepoStub{}
	usageLog := &UsageLog{RequestID: "req-nil-pool", UserID: 4}

	writeUsageLogBestEffort(context.Background(), nil, repo, usageLog, "test.gateway_usage_log")

	require.Equal(t, int32(1), repo.calls.Load())
	require.Same(t, usageLog, repo.lastLog)
}

// lockedLogBuffer 供 worker 协程与断言协程安全共享的日志缓冲。
type lockedLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestWriteUsageLogBestEffort_CreateErrorLoggedNotPropagated(t *testing.T) {
	pool := newGatewayUsageLogPoolForTest(t)
	repo := &gatewayUsageLogRepoStub{entered: make(chan struct{}, 1), err: errors.New("db down")}

	logBuf := &lockedLogBuffer{}
	origLogOutput := log.Writer()
	log.SetOutput(logBuf)
	t.Cleanup(func() { log.SetOutput(origLogOutput) })

	require.NotPanics(t, func() {
		// LegacyPrintf 在 logger 未初始化时回退到标准库 log；此处通过 log 输出断言错误被记录。
		writeUsageLogBestEffort(context.Background(), pool, repo, &UsageLog{RequestID: "req-err"}, "test.gateway_usage_log")
	})

	select {
	case <-repo.entered:
	case <-time.After(time.Second):
		t.Fatal("usage log write with error never executed")
	}
	require.Equal(t, int32(1), repo.calls.Load())
	// Create 返回后任务内才打印日志，需等待异步任务完成。
	require.Eventually(t, func() bool {
		return strings.Contains(logBuf.String(), "Create usage log failed")
	}, time.Second, 10*time.Millisecond)
	require.Contains(t, logBuf.String(), "db down")
}

func TestWriteUsageLogBestEffort_DetachesParentCancellation(t *testing.T) {
	pool := newGatewayUsageLogPoolForTest(t)
	workerRelease := blockGatewayUsageLogPoolWorker(t, pool)
	repo := &gatewayUsageLogRepoStub{entered: make(chan struct{}, 1)}

	parentCtx, cancel := context.WithCancel(context.Background())
	writeUsageLogBestEffort(parentCtx, pool, repo, &UsageLog{RequestID: "req-detached"}, "test.gateway_usage_log")
	cancel() // 任务执行前父 ctx 已取消：写入仍须完成（detached context）

	close(workerRelease)
	select {
	case <-repo.entered:
	case <-time.After(time.Second):
		t.Fatal("detached usage log write never executed")
	}
	require.Equal(t, int32(1), repo.calls.Load())
	require.NoError(t, repo.lastCtxErr, "usage log write must not inherit parent cancellation")
}

func TestGatewayServiceRecordUsage_SimpleModeUsageLogAsync(t *testing.T) {
	pool := newGatewayUsageLogPoolForTest(t)
	workerRelease := blockGatewayUsageLogPoolWorker(t, pool)
	repo := &gatewayUsageLogRepoStub{entered: make(chan struct{}, 1)}
	svc := newGatewayRecordUsageServiceForTest(repo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{})
	svc.cfg.RunMode = config.RunModeSimple
	svc.usageRecordWorkerPool = pool

	err := svc.RecordUsage(context.Background(), &RecordUsageInput{
		Result: &ForwardResult{RequestID: "simple-async", Usage: ClaudeUsage{InputTokens: 10, OutputTokens: 6}, Model: "claude-sonnet-4"},
		APIKey: &APIKey{ID: 501, Quota: 100}, User: &User{ID: 601}, Account: &Account{ID: 701},
	})
	require.NoError(t, err)

	// SIMPLE 模式：请求返回后 usage log 仍在后台异步落库。
	select {
	case <-repo.entered:
		t.Fatal("GatewayService SIMPLE mode wrote usage log synchronously")
	default:
	}
	require.Equal(t, int32(0), repo.calls.Load())

	close(workerRelease)
	select {
	case <-repo.entered:
	case <-time.After(time.Second):
		t.Fatal("GatewayService SIMPLE mode async usage log never written")
	}
	require.Equal(t, int32(1), repo.calls.Load())
	require.NotNil(t, repo.lastLog)
	require.Equal(t, "simple-async", repo.lastLog.RequestID)
}

func TestOpenAIGatewayServiceRecordUsage_SimpleModeUsageLogAsync(t *testing.T) {
	pool := newGatewayUsageLogPoolForTest(t)
	workerRelease := blockGatewayUsageLogPoolWorker(t, pool)
	repo := &gatewayUsageLogRepoStub{entered: make(chan struct{}, 1)}
	svc := newOpenAIRecordUsageServiceForTest(repo, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
	svc.cfg.RunMode = config.RunModeSimple
	svc.usageRecordWorkerPool = pool

	err := svc.RecordUsage(context.Background(), &OpenAIRecordUsageInput{
		Result: &OpenAIForwardResult{RequestID: "simple-async-openai", Usage: OpenAIUsage{InputTokens: 10, OutputTokens: 5}, Model: "gpt-5.1", Duration: time.Second},
		APIKey: &APIKey{ID: 1000}, User: &User{ID: 2000}, Account: &Account{ID: 3000},
	})
	require.NoError(t, err)

	select {
	case <-repo.entered:
		t.Fatal("OpenAIGatewayService SIMPLE mode wrote usage log synchronously")
	default:
	}
	require.Equal(t, int32(0), repo.calls.Load())

	close(workerRelease)
	select {
	case <-repo.entered:
	case <-time.After(time.Second):
		t.Fatal("OpenAIGatewayService SIMPLE mode async usage log never written")
	}
	require.Equal(t, int32(1), repo.calls.Load())
	require.NotNil(t, repo.lastLog)
	require.Equal(t, "simple-async-openai", repo.lastLog.RequestID)
}
