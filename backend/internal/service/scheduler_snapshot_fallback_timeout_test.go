//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// fallbackTimeoutTestService 构造回源上下文测试用的服务实例，指定受控回源超时（秒）。
func fallbackTimeoutTestService(timeoutSeconds int) *SchedulerSnapshotService {
	return &SchedulerSnapshotService{
		cfg: &config.Config{Gateway: config.GatewayConfig{
			Scheduling: config.GatewaySchedulingConfig{DbFallbackTimeoutSeconds: timeoutSeconds},
		}},
	}
}

// TestSchedulerSnapshotFallbackContext_HardCapWhenTimeoutUnset 验证
// db_fallback_timeout_seconds 未配置（生产默认 0）时回源查询上下文仍带硬性
// 执行上限：否则 DB 挂起会让 singleflight leader 无限阻塞，整桶等待者一起卡死。
func TestSchedulerSnapshotFallbackContext_HardCapWhenTimeoutUnset(t *testing.T) {
	svc := fallbackTimeoutTestService(0)
	fallbackCtx, cancel := svc.fallbackQueryContext(context.Background())
	defer cancel()

	deadline, ok := fallbackCtx.Deadline()
	require.True(t, ok, "db_fallback_timeout_seconds=0 时回源上下文也必须带硬性上限")
	require.WithinDuration(t, time.Now().Add(schedulerBucketRebuildLimit), deadline, 2*time.Second)
}

// TestSchedulerSnapshotFallbackContext_ConfigTimeoutWinsWhenShorter 验证配置了
// 受控回源超时时以其为准（比硬性上限更短），保持既有收紧语义。
func TestSchedulerSnapshotFallbackContext_ConfigTimeoutWinsWhenShorter(t *testing.T) {
	svc := fallbackTimeoutTestService(5)
	fallbackCtx, cancel := svc.fallbackQueryContext(context.Background())
	defer cancel()

	deadline, ok := fallbackCtx.Deadline()
	require.True(t, ok)
	require.WithinDuration(t, time.Now().Add(5*time.Second), deadline, 2*time.Second)
}

// TestSchedulerSnapshotFallbackContext_DetachedFromCallerCancel 验证回源上下文
// 脱离调用方取消：首个请求断连不得中断 leader 的 DB 工作（否则 singleflight
// 等待者共享一个瞬时错误）。
func TestSchedulerSnapshotFallbackContext_DetachedFromCallerCancel(t *testing.T) {
	svc := fallbackTimeoutTestService(0)
	parent, cancel := context.WithCancel(context.Background())
	fallbackCtx, cancel2 := svc.fallbackQueryContext(parent)
	defer cancel2()

	cancel()
	require.NoError(t, fallbackCtx.Err(), "回源上下文必须脱离调用方取消")
}
