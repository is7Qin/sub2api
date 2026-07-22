package handler

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// TestWrapReleaseOnDone_NoGoroutineLeak 验证 wrapReleaseOnDone 修复后不会泄露 goroutine
func TestWrapReleaseOnDone_NoGoroutineLeak(t *testing.T) {
	// 记录测试开始时的 goroutine 数量
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	initialGoroutines := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var releaseCount int32
	release := wrapReleaseOnDone(ctx, func() {
		atomic.AddInt32(&releaseCount, 1)
	})

	// 正常释放
	release()

	// 等待足够时间确保 goroutine 退出
	time.Sleep(200 * time.Millisecond)

	// 验证只释放一次
	if count := atomic.LoadInt32(&releaseCount); count != 1 {
		t.Errorf("expected release count to be 1, got %d", count)
	}

	// 强制 GC，清理已退出的 goroutine
	runtime.GC()
	time.Sleep(100 * time.Millisecond)

	// 验证 goroutine 数量没有增加（允许±2的误差，考虑到测试框架本身可能创建的 goroutine）
	finalGoroutines := runtime.NumGoroutine()
	if finalGoroutines > initialGoroutines+2 {
		t.Errorf("goroutine leak detected: initial=%d, final=%d, leaked=%d",
			initialGoroutines, finalGoroutines, finalGoroutines-initialGoroutines)
	}
}

// TestWrapReleaseOnDone_ContextCancellation 验证 context 取消时也能正确释放
func TestWrapReleaseOnDone_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var releaseCount int32
	_ = wrapReleaseOnDone(ctx, func() {
		atomic.AddInt32(&releaseCount, 1)
	})

	// 取消 context，应该触发释放
	cancel()

	// 等待释放完成
	time.Sleep(100 * time.Millisecond)

	// 验证释放被调用
	if count := atomic.LoadInt32(&releaseCount); count != 1 {
		t.Errorf("expected release count to be 1, got %d", count)
	}
}

// TestWrapReleaseOnDone_MultipleCallsOnlyReleaseOnce 验证多次调用 release 只释放一次
func TestWrapReleaseOnDone_MultipleCallsOnlyReleaseOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var releaseCount int32
	release := wrapReleaseOnDone(ctx, func() {
		atomic.AddInt32(&releaseCount, 1)
	})

	// 调用多次
	release()
	release()
	release()

	// 等待执行完成
	time.Sleep(100 * time.Millisecond)

	// 验证只释放一次
	if count := atomic.LoadInt32(&releaseCount); count != 1 {
		t.Errorf("expected release count to be 1, got %d", count)
	}
}

// TestWrapReleaseOnDone_NilReleaseFunc 验证 nil releaseFunc 不会 panic
func TestWrapReleaseOnDone_NilReleaseFunc(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	release := wrapReleaseOnDone(ctx, nil)

	if release != nil {
		t.Error("expected nil release function when releaseFunc is nil")
	}
}

// TestWrapReleaseOnDone_ConcurrentCalls 验证并发调用的安全性
func TestWrapReleaseOnDone_ConcurrentCalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var releaseCount int32
	release := wrapReleaseOnDone(ctx, func() {
		atomic.AddInt32(&releaseCount, 1)
	})

	// 并发调用 release
	const numGoroutines = 10
	for i := 0; i < numGoroutines; i++ {
		go release()
	}

	// 等待所有 goroutine 完成
	time.Sleep(200 * time.Millisecond)

	// 验证只释放一次
	if count := atomic.LoadInt32(&releaseCount); count != 1 {
		t.Errorf("expected release count to be 1, got %d", count)
	}
}

// BenchmarkWrapReleaseOnDone 性能基准测试
func TestHTTPAttemptReleaseSetCancellationBeforeTransfer(
	t *testing.T,
) {
	ctx, cancel := context.WithCancel(context.Background())
	released := make(chan struct{}, 1)
	set := newHTTPAttemptReleaseSet(ctx)
	release := set.Add(func() { released <- struct{}{} })

	cancel()

	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("pending release was not reclaimed")
	}
	release()
	select {
	case <-released:
		t.Fatal("release ran more than once")
	default:
	}
}

func TestHTTPAttemptReleaseSetTransferHoldsUntilCompletion(
	t *testing.T,
) {
	ctx, cancel := context.WithCancel(context.Background())
	released := make(chan struct{}, 1)
	releaseStarted := make(chan struct{})
	allowRelease := make(chan struct{})
	var releaseOnce sync.Once
	set := newHTTPAttemptReleaseSet(ctx)
	release := set.Add(func() {
		releaseOnce.Do(func() { close(releaseStarted) })
		<-allowRelease
		released <- struct{}{}
	})
	if !set.Transfer() {
		t.Fatal("release ownership transfer failed")
	}
	if set.Transfer() {
		t.Fatal("release ownership transferred more than once")
	}

	cancel()
	select {
	case <-releaseStarted:
		t.Fatal("transferred release started on client cancellation")
	default:
	}

	go release()
	select {
	case <-releaseStarted:
	case <-time.After(time.Second):
		t.Fatal("transferred release did not start at completion")
	}
	close(allowRelease)
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("transferred release did not run at completion")
	}
}

func TestHTTPAttemptReleaseSetLogicalTransferSurvivesMultipleAttempts(
	t *testing.T,
) {
	set := newHTTPAttemptReleaseSet(context.Background())
	if !set.transferLogical() {
		t.Fatal("first logical transfer failed")
	}
	if !set.transferLogical() {
		t.Fatal("later logical transfer was rejected")
	}
	set.finish()
	if set.transferLogical() {
		t.Fatal("completed logical lease transferred again")
	}
}

func TestHTTPAttemptReleaseSetRejectsLateReleaseWithoutLeak(
	t *testing.T,
) {
	ctx := context.Background()
	set := newHTTPAttemptReleaseSet(ctx)
	if !set.Transfer() {
		t.Fatal("release ownership transfer failed")
	}
	released := make(chan struct{}, 1)
	release := set.Add(func() { released <- struct{}{} })

	select {
	case <-released:
	default:
		t.Fatal("late release was not reclaimed")
	}
	release()
	select {
	case <-released:
		t.Fatal("late release ran more than once")
	default:
	}
}

func TestLogicalClientLeaseLifecycle(t *testing.T) {
	acquireLogical := func(t *testing.T, ctx context.Context) (*httpAttemptReleaseSet, *concurrencyCacheMock) {
		t.Helper()
		cache := &concurrencyCacheMock{
			acquireAPIKeySlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil },
			acquireUserSlotFn:   func(context.Context, int64, int, string) (bool, error) { return true, nil },
		}
		helper := NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, time.Second)
		c, _ := newHelperTestContext("POST", "/v1/test")
		c.Request = c.Request.WithContext(ctx)
		streamStarted := false
		clientRelease, err := helper.AcquireClientSlotsWithWait(c, 77, 1, 101, 1, false, &streamStarted)
		require.NoError(t, err)
		logical := newHTTPAttemptReleaseSet(ctx)
		logical.Add(clientRelease)
		return logical, cache
	}
	assertClientReleasedOnce := func(t *testing.T, cache *concurrencyCacheMock) {
		t.Helper()
		require.Equal(t, int32(1), atomic.LoadInt32(&cache.releaseAPIKeyCalled))
		require.Equal(t, int32(1), atomic.LoadInt32(&cache.releaseUserCalled))
	}

	t.Run("billing auth and scheduling failures release both client slots", func(t *testing.T) {
		for _, failure := range []string{"billing", "auth", "scheduling"} {
			t.Run(failure, func(t *testing.T) {
				logical, cache := acquireLogical(t, context.Background())
				logical.finish()
				logical.finish()
				assertClientReleasedOnce(t, cache)
			})
		}
	})

	t.Run("cancellation releases both client slots", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		logical, cache := acquireLogical(t, ctx)
		cancel()
		require.Eventually(t, func() bool {
			return atomic.LoadInt32(&cache.releaseAPIKeyCalled) == 1 && atomic.LoadInt32(&cache.releaseUserCalled) == 1
		}, time.Second, time.Millisecond)
		logical.finish()
		assertClientReleasedOnce(t, cache)
	})

	t.Run("streaming completion releases transferred client slots", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		logical, cache := acquireLogical(t, ctx)
		require.True(t, logical.transferLogical())

		cancel()
		time.Sleep(10 * time.Millisecond)
		require.Zero(t, atomic.LoadInt32(&cache.releaseAPIKeyCalled))
		require.Zero(t, atomic.LoadInt32(&cache.releaseUserCalled))

		logical.finish()
		logical.finish()
		assertClientReleasedOnce(t, cache)
	})

	t.Run("failover keeps client slots logical and accounts per attempt", func(t *testing.T) {
		logical, cache := acquireLogical(t, context.Background())
		var accountReleases atomic.Int32
		for range 2 {
			attempt := newHTTPAttemptReleaseSet(context.Background())
			attempt.Add(func() { accountReleases.Add(1) })
			require.True(t, attempt.Transfer())
			require.True(t, logical.transferLogical())
			attempt.finish()
		}
		logical.finish()

		assertClientReleasedOnce(t, cache)
		require.Equal(t, int32(2), accountReleases.Load())
	})
}

func BenchmarkWrapReleaseOnDone(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		release := wrapReleaseOnDone(ctx, func() {})
		release()
	}
}
