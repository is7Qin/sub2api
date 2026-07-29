package openai_ws_v2

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

type passthroughTestFrame struct {
	msgType coderws.MessageType
	payload []byte
}

type passthroughTestFrameConn struct {
	mu     sync.Mutex
	writes []passthroughTestFrame
	readCh chan passthroughTestFrame
	once   sync.Once
}

type delayedReadFrameConn struct {
	base       FrameConn
	firstDelay time.Duration
	once       sync.Once
}

type closeSpyFrameConn struct {
	closeCalls atomic.Int32
}

func newPassthroughTestFrameConn(frames []passthroughTestFrame, autoClose bool) *passthroughTestFrameConn {
	c := &passthroughTestFrameConn{
		readCh: make(chan passthroughTestFrame, len(frames)+1),
	}
	for _, frame := range frames {
		copied := passthroughTestFrame{msgType: frame.msgType, payload: append([]byte(nil), frame.payload...)}
		c.readCh <- copied
	}
	if autoClose {
		close(c.readCh)
	}
	return c
}

func (c *passthroughTestFrameConn) ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return coderws.MessageText, nil, ctx.Err()
	case frame, ok := <-c.readCh:
		if !ok {
			return coderws.MessageText, nil, io.EOF
		}
		return frame.msgType, append([]byte(nil), frame.payload...), nil
	}
}

func (c *passthroughTestFrameConn) WriteFrame(ctx context.Context, msgType coderws.MessageType, payload []byte) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes = append(c.writes, passthroughTestFrame{msgType: msgType, payload: append([]byte(nil), payload...)})
	return nil
}

func (c *passthroughTestFrameConn) Close() error {
	c.once.Do(func() {
		defer func() { _ = recover() }()
		close(c.readCh)
	})
	return nil
}

func (c *passthroughTestFrameConn) Writes() []passthroughTestFrame {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]passthroughTestFrame, len(c.writes))
	copy(out, c.writes)
	return out
}

func (c *delayedReadFrameConn) ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error) {
	if c == nil || c.base == nil {
		return coderws.MessageText, nil, io.EOF
	}
	c.once.Do(func() {
		if c.firstDelay > 0 {
			timer := time.NewTimer(c.firstDelay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
			case <-timer.C:
			}
		}
	})
	return c.base.ReadFrame(ctx)
}

func (c *delayedReadFrameConn) WriteFrame(ctx context.Context, msgType coderws.MessageType, payload []byte) error {
	if c == nil || c.base == nil {
		return io.EOF
	}
	return c.base.WriteFrame(ctx, msgType, payload)
}

func (c *delayedReadFrameConn) Close() error {
	if c == nil || c.base == nil {
		return nil
	}
	return c.base.Close()
}

func (c *closeSpyFrameConn) ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	<-ctx.Done()
	return coderws.MessageText, nil, ctx.Err()
}

func (c *closeSpyFrameConn) WriteFrame(ctx context.Context, _ coderws.MessageType, _ []byte) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func (c *closeSpyFrameConn) Close() error {
	if c != nil {
		c.closeCalls.Add(1)
	}
	return nil
}

func (c *closeSpyFrameConn) CloseCalls() int32 {
	if c == nil {
		return 0
	}
	return c.closeCalls.Load()
}

func TestRelay_BasicRelayAndUsage(t *testing.T) {
	t.Parallel()

	clientConn := newPassthroughTestFrameConn(nil, false)
	upstreamConn := newPassthroughTestFrameConn([]passthroughTestFrame{
		{
			msgType: coderws.MessageText,
			payload: []byte(`{"type":"response.completed","response":{"id":"resp_123","usage":{"input_tokens":7,"output_tokens":3,"input_tokens_details":{"cached_tokens":2}}}}`),
		},
	}, true)

	firstPayload := []byte(`{"type":"response.create","model":"gpt-5.3-codex","input":[{"type":"input_text","text":"hello"}]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, relayExit := Relay(ctx, clientConn, upstreamConn, firstPayload, RelayOptions{})
	require.Nil(t, relayExit)
	require.Equal(t, "gpt-5.3-codex", result.RequestModel)
	require.Equal(t, "resp_123", result.RequestID)
	require.Equal(t, "response.completed", result.TerminalEventType)
	require.Equal(t, 7, result.Usage.InputTokens)
	require.Equal(t, 3, result.Usage.OutputTokens)
	require.Equal(t, 2, result.Usage.CacheReadInputTokens)
	require.Nil(t, result.FirstTokenMs, "a terminal-only response must not produce a TTFT sample")
	require.Equal(t, int64(1), result.ClientToUpstreamFrames)
	require.Equal(t, int64(1), result.UpstreamToClientFrames)
	require.Equal(t, int64(0), result.DroppedDownstreamFrames)

	upstreamWrites := upstreamConn.Writes()
	require.Len(t, upstreamWrites, 1)
	require.Equal(t, coderws.MessageText, upstreamWrites[0].msgType)
	require.JSONEq(t, string(firstPayload), string(upstreamWrites[0].payload))

	clientWrites := clientConn.Writes()
	require.Len(t, clientWrites, 1)
	require.Equal(t, coderws.MessageText, clientWrites[0].msgType)
	require.JSONEq(t, `{"type":"response.completed","response":{"id":"resp_123","usage":{"input_tokens":7,"output_tokens":3,"input_tokens_details":{"cached_tokens":2}}}}`, string(clientWrites[0].payload))
}

func TestRelay_TerminalOnlyTurnHasNoFirstToken(t *testing.T) {
	t.Parallel()

	for _, terminalEventType := range []string{
		"response.completed",
		"response.done",
		"response.failed",
		"response.incomplete",
		"response.cancelled",
		"response.canceled",
	} {
		terminalEventType := terminalEventType
		t.Run(terminalEventType, func(t *testing.T) {
			t.Parallel()

			clientConn := newPassthroughTestFrameConn(nil, false)
			upstreamConn := newPassthroughTestFrameConn([]passthroughTestFrame{
				{msgType: coderws.MessageText, payload: []byte(`{"type":"response.created","response":{"id":"resp_terminal_only"}}`)},
				{msgType: coderws.MessageText, payload: []byte(`{"type":"` + terminalEventType + `","response":{"id":"resp_terminal_only","usage":{"input_tokens":1,"output_tokens":0}}}`)},
			}, true)

			base := time.Unix(0, 0)
			var ticks atomic.Int64
			nowFn := func() time.Time {
				return base.Add(time.Duration(ticks.Add(1)) * 10 * time.Millisecond)
			}
			var turn RelayTurnResult
			result, relayExit := Relay(context.Background(), clientConn, upstreamConn, []byte(`{"type":"response.create","model":"gpt-5.3-codex","input":[]}`), RelayOptions{
				Now: nowFn,
				OnTurnComplete: func(current RelayTurnResult) {
					turn = current
				},
			})

			require.Nil(t, relayExit)
			require.Equal(t, terminalEventType, turn.TerminalEventType)
			require.Greater(t, turn.Duration, time.Duration(0))
			require.Nil(t, turn.FirstTokenMs)
			require.Nil(t, result.FirstTokenMs)
		})
	}
}

func TestRelay_OutputDeltaSetsFirstTokenBeforeCompletion(t *testing.T) {
	t.Parallel()

	clientConn := newPassthroughTestFrameConn(nil, false)
	upstreamConn := newPassthroughTestFrameConn([]passthroughTestFrame{
		{msgType: coderws.MessageText, payload: []byte(`{"type":"response.created","response":{"id":"resp_delta"}}`)},
		{msgType: coderws.MessageText, payload: []byte(`{"type":"response.output_text.delta","response_id":"resp_delta","delta":"hi"}`)},
		{msgType: coderws.MessageText, payload: []byte(`{"type":"response.completed","response":{"id":"resp_delta","usage":{"input_tokens":1,"output_tokens":1}}}`)},
	}, true)

	base := time.Unix(0, 0)
	var ticks atomic.Int64
	nowFn := func() time.Time {
		return base.Add(time.Duration(ticks.Add(1)) * 10 * time.Millisecond)
	}
	var turn RelayTurnResult
	_, relayExit := Relay(context.Background(), clientConn, upstreamConn, []byte(`{"type":"response.create","model":"gpt-5.3-codex","input":[]}`), RelayOptions{
		Now: nowFn,
		OnTurnComplete: func(current RelayTurnResult) {
			turn = current
		},
	})

	require.Nil(t, relayExit)
	require.NotNil(t, turn.FirstTokenMs)
	require.Greater(t, *turn.FirstTokenMs, 0)
	require.Less(t, int64(*turn.FirstTokenMs), turn.Duration.Milliseconds())
}

func TestRelay_FunctionCallOutputBytesPreserved(t *testing.T) {
	t.Parallel()

	clientConn := newPassthroughTestFrameConn(nil, false)
	upstreamConn := newPassthroughTestFrameConn([]passthroughTestFrame{
		{
			msgType: coderws.MessageText,
			payload: []byte(`{"type":"response.completed","response":{"id":"resp_func","usage":{"input_tokens":1,"output_tokens":1}}}`),
		},
	}, true)

	firstPayload := []byte(`{"type":"response.create","model":"gpt-5.3-codex","input":[{"type":"function_call_output","call_id":"call_abc123","output":"{\"ok\":true}"}]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, relayExit := Relay(ctx, clientConn, upstreamConn, firstPayload, RelayOptions{})
	require.Nil(t, relayExit)

	upstreamWrites := upstreamConn.Writes()
	require.Len(t, upstreamWrites, 1)
	require.Equal(t, coderws.MessageText, upstreamWrites[0].msgType)
	require.Equal(t, firstPayload, upstreamWrites[0].payload)
}

func TestRelay_UpstreamDisconnect(t *testing.T) {
	t.Parallel()

	// 上游立即关闭（EOF），客户端不发送额外帧
	clientConn := newPassthroughTestFrameConn(nil, false)
	upstreamConn := newPassthroughTestFrameConn(nil, true) // 立即 close -> EOF

	firstPayload := []byte(`{"type":"response.create","model":"gpt-4o","input":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, relayExit := Relay(ctx, clientConn, upstreamConn, firstPayload, RelayOptions{})
	// 上游 EOF 属于 disconnect，标记为 graceful
	require.Nil(t, relayExit, "上游 EOF 应被视为 graceful disconnect")
	require.Equal(t, "gpt-4o", result.RequestModel)
}

func TestRelay_ClientDisconnect(t *testing.T) {
	t.Parallel()

	// 客户端立即关闭（EOF），上游阻塞读取直到 context 取消
	clientConn := newPassthroughTestFrameConn(nil, true) // 立即 close -> EOF
	upstreamConn := newPassthroughTestFrameConn(nil, false)

	firstPayload := []byte(`{"type":"response.create","model":"gpt-4o","input":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, relayExit := Relay(ctx, clientConn, upstreamConn, firstPayload, RelayOptions{})
	require.NotNil(t, relayExit, "客户端 EOF 应返回可观测的中断状态")
	require.Equal(t, "client_disconnected", relayExit.Stage)
	require.Equal(t, "gpt-4o", result.RequestModel)
}

func TestRelay_ClientDisconnect_DrainCapturesLateUsage(t *testing.T) {
	t.Parallel()

	clientConn := newPassthroughTestFrameConn(nil, true)
	upstreamBase := newPassthroughTestFrameConn([]passthroughTestFrame{
		{
			msgType: coderws.MessageText,
			payload: []byte(`{"type":"response.completed","response":{"id":"resp_drain","usage":{"input_tokens":6,"output_tokens":4,"input_tokens_details":{"cached_tokens":1}}}}`),
		},
	}, true)
	upstreamConn := &delayedReadFrameConn{
		base:       upstreamBase,
		firstDelay: 80 * time.Millisecond,
	}

	firstPayload := []byte(`{"type":"response.create","model":"gpt-4o","input":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, relayExit := Relay(ctx, clientConn, upstreamConn, firstPayload, RelayOptions{
		UpstreamDrainTimeout: 400 * time.Millisecond,
	})
	require.NotNil(t, relayExit)
	require.Equal(t, "client_disconnected", relayExit.Stage)
	require.Equal(t, "resp_drain", result.RequestID)
	require.Equal(t, "response.completed", result.TerminalEventType)
	require.Equal(t, 6, result.Usage.InputTokens)
	require.Equal(t, 4, result.Usage.OutputTokens)
	require.Equal(t, 1, result.Usage.CacheReadInputTokens)
	require.Equal(t, int64(1), result.ClientToUpstreamFrames)
	require.Equal(t, int64(0), result.UpstreamToClientFrames)
	require.Equal(t, int64(1), result.DroppedDownstreamFrames)
}

func TestRelay_IdleTimeout(t *testing.T) {
	t.Parallel()

	// 客户端和上游都不发送帧，idle timeout 应触发
	clientConn := newPassthroughTestFrameConn(nil, false)
	upstreamConn := newPassthroughTestFrameConn(nil, false)

	firstPayload := []byte(`{"type":"response.create","model":"gpt-4o","input":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 使用快进时间来加速 idle timeout
	now := time.Now()
	callCount := 0
	nowFn := func() time.Time {
		callCount++
		// 前几次调用返回正常时间（初始化阶段），之后快进
		if callCount <= 5 {
			return now
		}
		return now.Add(time.Hour) // 快进到超时
	}

	result, relayExit := Relay(ctx, clientConn, upstreamConn, firstPayload, RelayOptions{
		IdleTimeout: 2 * time.Second,
		Now:         nowFn,
	})
	require.NotNil(t, relayExit, "应因 idle timeout 退出")
	require.Equal(t, "idle_timeout", relayExit.Stage)
	require.Equal(t, "gpt-4o", result.RequestModel)
}

func TestRelay_IdleTimeoutDoesNotCloseClientOnError(t *testing.T) {
	t.Parallel()

	clientConn := &closeSpyFrameConn{}
	upstreamConn := &closeSpyFrameConn{}

	firstPayload := []byte(`{"type":"response.create","model":"gpt-4o","input":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	now := time.Now()
	callCount := 0
	nowFn := func() time.Time {
		callCount++
		if callCount <= 5 {
			return now
		}
		return now.Add(time.Hour)
	}

	_, relayExit := Relay(ctx, clientConn, upstreamConn, firstPayload, RelayOptions{
		IdleTimeout: 2 * time.Second,
		Now:         nowFn,
	})
	require.NotNil(t, relayExit, "应因 idle timeout 退出")
	require.Equal(t, "idle_timeout", relayExit.Stage)
	require.Zero(t, clientConn.CloseCalls(), "错误路径不应提前关闭客户端连接，交给上层决定 close code")
	require.GreaterOrEqual(t, upstreamConn.CloseCalls(), int32(1))
}

func TestRelay_NilConnections(t *testing.T) {
	t.Parallel()

	firstPayload := []byte(`{"type":"response.create","model":"gpt-4o","input":[]}`)
	ctx := context.Background()

	t.Run("nil client conn", func(t *testing.T) {
		upstreamConn := newPassthroughTestFrameConn(nil, true)
		_, relayExit := Relay(ctx, nil, upstreamConn, firstPayload, RelayOptions{})
		require.NotNil(t, relayExit)
		require.Equal(t, "relay_init", relayExit.Stage)
		require.Contains(t, relayExit.Err.Error(), "nil")
	})

	t.Run("nil upstream conn", func(t *testing.T) {
		clientConn := newPassthroughTestFrameConn(nil, true)
		_, relayExit := Relay(ctx, clientConn, nil, firstPayload, RelayOptions{})
		require.NotNil(t, relayExit)
		require.Equal(t, "relay_init", relayExit.Stage)
		require.Contains(t, relayExit.Err.Error(), "nil")
	})
}

func TestRelay_MultipleUpstreamMessages(t *testing.T) {
	t.Parallel()

	// 上游发送多个事件（delta + completed），验证多帧中继和 usage 聚合
	clientConn := newPassthroughTestFrameConn(nil, false)
	upstreamConn := newPassthroughTestFrameConn([]passthroughTestFrame{
		{
			msgType: coderws.MessageText,
			payload: []byte(`{"type":"response.output_text.delta","delta":"Hello"}`),
		},
		{
			msgType: coderws.MessageText,
			payload: []byte(`{"type":"response.output_text.delta","delta":" world"}`),
		},
		{
			msgType: coderws.MessageText,
			payload: []byte(`{"type":"response.completed","response":{"id":"resp_multi","usage":{"input_tokens":10,"output_tokens":5,"input_tokens_details":{"cached_tokens":3}}}}`),
		},
	}, true)

	firstPayload := []byte(`{"type":"response.create","model":"gpt-4o","input":[{"type":"input_text","text":"hi"}]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, relayExit := Relay(ctx, clientConn, upstreamConn, firstPayload, RelayOptions{})
	require.Nil(t, relayExit)
	require.Equal(t, "resp_multi", result.RequestID)
	require.Equal(t, "response.completed", result.TerminalEventType)
	require.Equal(t, 10, result.Usage.InputTokens)
	require.Equal(t, 5, result.Usage.OutputTokens)
	require.Equal(t, 3, result.Usage.CacheReadInputTokens)
	require.NotNil(t, result.FirstTokenMs)

	// 验证所有 3 个上游帧都转发给了客户端
	clientWrites := clientConn.Writes()
	require.Len(t, clientWrites, 3)
}

func TestRelay_OnTurnComplete_PerTerminalEvent(t *testing.T) {
	t.Parallel()

	clientConn := newPassthroughTestFrameConn(nil, false)
	upstreamConn := newPassthroughTestFrameConn([]passthroughTestFrame{
		{
			msgType: coderws.MessageText,
			payload: []byte(`{"type":"response.completed","response":{"id":"resp_turn_1","usage":{"input_tokens":2,"output_tokens":1}}}`),
		},
		{
			msgType: coderws.MessageText,
			payload: []byte(`{"type":"response.failed","response":{"id":"resp_turn_2","usage":{"input_tokens":3,"output_tokens":4}}}`),
		},
	}, true)

	firstPayload := []byte(`{"type":"response.create","model":"gpt-5.3-codex","input":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	turns := make([]RelayTurnResult, 0, 2)
	result, relayExit := Relay(ctx, clientConn, upstreamConn, firstPayload, RelayOptions{
		OnTurnComplete: func(turn RelayTurnResult) {
			turns = append(turns, turn)
		},
	})
	require.Nil(t, relayExit)
	require.Len(t, turns, 2)
	require.Equal(t, "resp_turn_1", turns[0].RequestID)
	require.Equal(t, "response.completed", turns[0].TerminalEventType)
	require.Equal(t, 2, turns[0].Usage.InputTokens)
	require.Equal(t, 1, turns[0].Usage.OutputTokens)
	require.Equal(t, "resp_turn_2", turns[1].RequestID)
	require.Equal(t, "response.failed", turns[1].TerminalEventType)
	require.Equal(t, 3, turns[1].Usage.InputTokens)
	require.Equal(t, 4, turns[1].Usage.OutputTokens)
	require.Equal(t, 5, result.Usage.InputTokens)
	require.Equal(t, 5, result.Usage.OutputTokens)
}

func TestRelay_OnTurnComplete_DuplicateTerminalForResponseEmitsOnce(t *testing.T) {
	t.Parallel()

	clientConn := newPassthroughTestFrameConn(nil, false)
	upstreamConn := newPassthroughTestFrameConn([]passthroughTestFrame{
		{msgType: coderws.MessageText, payload: []byte(`{"type":"response.completed","response":{"id":"resp_duplicate_terminal","usage":{"input_tokens":2,"output_tokens":1}}}`)},
		{msgType: coderws.MessageText, payload: []byte(`{"type":"response.completed","response":{"id":"resp_duplicate_terminal","usage":{"input_tokens":2,"output_tokens":1}}}`)},
	}, true)

	var turns []RelayTurnResult
	_, relayExit := Relay(context.Background(), clientConn, upstreamConn, []byte(`{"type":"response.create","model":"gpt-5.3-codex","input":[]}`), RelayOptions{
		OnTurnComplete: func(turn RelayTurnResult) {
			turns = append(turns, turn)
		},
	})

	require.Nil(t, relayExit)
	require.Len(t, turns, 1, "a response terminal event must release its turn exactly once")
	require.Equal(t, "resp_duplicate_terminal", turns[0].RequestID)
}

func TestRelay_DirectClientFramesKeepIdleWatchdogAliveUntilTheyStop(t *testing.T) {
	// The watchdog has a one-second minimum polling interval, so this bounded
	// test leaves enough margin for a full post-activity poll.
	clientConn := newPassthroughTestFrameConn(nil, false)
	upstreamConn := newPassthroughTestFrameConn(nil, false)
	defer clientConn.Close()

	go func() {
		for range 12 {
			clientConn.readCh <- passthroughTestFrame{msgType: coderws.MessageText, payload: []byte(`{"type":"session.update"}`)}
			time.Sleep(100 * time.Millisecond)
		}
	}()

	startedAt := time.Now()
	_, relayExit := Relay(context.Background(), clientConn, upstreamConn, []byte(`{"type":"response.create","model":"gpt-5.3-codex","input":[]}`), RelayOptions{
		IdleTimeout: 500 * time.Millisecond,
		ReadClientFrame: func(ctx context.Context, conn FrameConn, markActivity func()) (coderws.MessageType, []byte, error) {
			for {
				msgType, payload, err := conn.ReadFrame(ctx)
				if err != nil {
					return msgType, payload, err
				}
				if err := upstreamConn.WriteFrame(ctx, msgType, payload); err != nil {
					return msgType, payload, err
				}
				markActivity()
			}
		},
	})

	require.NotNil(t, relayExit)
	require.Equal(t, "idle_timeout", relayExit.Stage)
	require.GreaterOrEqual(t, time.Since(startedAt), 1500*time.Millisecond, "valid direct client frames must defer the watchdog until the frames stop")
	require.Len(t, upstreamConn.Writes(), 13, "first response.create plus every direct client frame must reach upstream")
}

func TestRelay_DuplicateTerminalDoesNotDoubleCountOrBlockOtherResponse(t *testing.T) {
	t.Parallel()

	clientConn := newPassthroughTestFrameConn(nil, false)
	upstreamConn := newPassthroughTestFrameConn([]passthroughTestFrame{
		{msgType: coderws.MessageText, payload: []byte(`{"type":"response.completed","response":{"id":"resp_terminal_a","usage":{"input_tokens":2,"output_tokens":1}}}`)},
		{msgType: coderws.MessageText, payload: []byte(`{"type":"response.failed","response":{"id":"resp_terminal_a","usage":{"input_tokens":20,"output_tokens":10}}}`)},
		{msgType: coderws.MessageText, payload: []byte(`{"type":"response.completed","response":{"id":"resp_terminal_b","usage":{"input_tokens":3,"output_tokens":4}}}`)},
	}, true)

	var turns []RelayTurnResult
	result, relayExit := Relay(context.Background(), clientConn, upstreamConn, []byte(`{"type":"response.create","model":"gpt-5.3-codex","input":[]}`), RelayOptions{
		OnTurnComplete: func(turn RelayTurnResult) {
			turns = append(turns, turn)
		},
	})

	require.Nil(t, relayExit)
	require.Equal(t, Usage{InputTokens: 5, OutputTokens: 5}, result.Usage)
	require.Len(t, turns, 2)
	require.Equal(t, "resp_terminal_a", turns[0].RequestID)
	require.Equal(t, "response.completed", turns[0].TerminalEventType)
	require.Equal(t, "resp_terminal_b", turns[1].RequestID)
	require.Equal(t, "response.completed", turns[1].TerminalEventType)
}

func TestRelay_LaterTurnFirstSemanticOutputUsesOwnTiming(t *testing.T) {
	t.Parallel()

	clientConn := newPassthroughTestFrameConn(nil, false)
	upstreamConn := newPassthroughTestFrameConn([]passthroughTestFrame{
		{msgType: coderws.MessageText, payload: []byte(`{"type":"response.output_text.delta","response_id":"resp_later_1","delta":"first"}`)},
		{msgType: coderws.MessageText, payload: []byte(`{"type":"response.completed","response":{"id":"resp_later_1","usage":{"input_tokens":1,"output_tokens":1}}}`)},
		{msgType: coderws.MessageText, payload: []byte(`{"type":"response.created","response":{"id":"resp_later_2"}}`)},
		{msgType: coderws.MessageText, payload: []byte(`{"type":"response.output_text.delta","response_id":"resp_later_2","delta":"second"}`)},
		{msgType: coderws.MessageText, payload: []byte(`{"type":"response.completed","response":{"id":"resp_later_2","usage":{"input_tokens":1,"output_tokens":1}}}`)},
	}, true)

	base := time.Unix(0, 0)
	var ticks atomic.Int64
	nowFn := func() time.Time {
		return base.Add(time.Duration(ticks.Add(1)) * 10 * time.Millisecond)
	}
	var turns []RelayTurnResult
	_, relayExit := Relay(context.Background(), clientConn, upstreamConn, []byte(`{"type":"response.create","model":"gpt-5.3-codex","input":[]}`), RelayOptions{
		Now: nowFn,
		OnTurnComplete: func(turn RelayTurnResult) {
			turns = append(turns, turn)
		},
	})

	require.Nil(t, relayExit)
	require.Len(t, turns, 2)
	require.NotNil(t, turns[0].FirstTokenMs)
	require.NotNil(t, turns[1].FirstTokenMs)
	require.Greater(t, *turns[1].FirstTokenMs, 0, "later turn must start timing from its own first semantic output")
	require.Less(t, *turns[1].FirstTokenMs, int(turns[1].Duration.Milliseconds()))
}

func TestRelay_OverlappingResponsesDoNotStealFirstTokenTiming(t *testing.T) {
	t.Parallel()

	clientConn := newPassthroughTestFrameConn(nil, false)
	upstreamConn := newPassthroughTestFrameConn([]passthroughTestFrame{
		{msgType: coderws.MessageText, payload: []byte(`{"type":"response.created","response":{"id":"resp_overlap_a"}}`)},
		{msgType: coderws.MessageText, payload: []byte(`{"type":"response.created","response":{"id":"resp_overlap_b"}}`)},
		{msgType: coderws.MessageText, payload: []byte(`{"type":"response.output_text.delta","response_id":"resp_overlap_a","delta":"A"}`)},
		{msgType: coderws.MessageText, payload: []byte(`{"type":"response.output_text.delta","response_id":"resp_overlap_b","delta":"B"}`)},
		{msgType: coderws.MessageText, payload: []byte(`{"type":"response.completed","response":{"id":"resp_overlap_b","usage":{"input_tokens":2,"output_tokens":1}}}`)},
		{msgType: coderws.MessageText, payload: []byte(`{"type":"response.failed","response":{"id":"resp_overlap_a","usage":{"input_tokens":3,"output_tokens":2}}}`)},
	}, true)

	base := time.Unix(0, 0)
	var ticks atomic.Int64
	nowFn := func() time.Time {
		return base.Add(time.Duration(ticks.Add(1)) * 10 * time.Millisecond)
	}
	var turns []RelayTurnResult
	_, relayExit := Relay(context.Background(), clientConn, upstreamConn, []byte(`{"type":"response.create","model":"gpt-5.3-codex","input":[]}`), RelayOptions{
		Now: nowFn,
		OnTurnComplete: func(turn RelayTurnResult) {
			turns = append(turns, turn)
		},
	})

	require.Nil(t, relayExit)
	require.Len(t, turns, 2)
	require.Equal(t, "resp_overlap_b", turns[0].RequestID)
	require.Equal(t, "response.completed", turns[0].TerminalEventType)
	require.Equal(t, Usage{InputTokens: 2, OutputTokens: 1}, turns[0].Usage)
	require.NotNil(t, turns[0].FirstTokenMs)
	require.Equal(t, 60, *turns[0].FirstTokenMs, "B first-token timing must come from B's own semantic output")
	require.Equal(t, "resp_overlap_a", turns[1].RequestID)
	require.Equal(t, "response.failed", turns[1].TerminalEventType)
	require.Equal(t, Usage{InputTokens: 3, OutputTokens: 2}, turns[1].Usage)
	require.NotNil(t, turns[1].FirstTokenMs)
	require.Equal(t, 60, *turns[1].FirstTokenMs, "A terminal result must retain A's own first-token timing")
}

func TestRelay_ResponseFailedTextSanitizesClientPayloadAndKeepsUsage(t *testing.T) {
	t.Parallel()

	failedPayload := []byte(`{"type":"response.failed","instructions":"top-secret instructions","input":[{"role":"user","content":"top secret prompt"}],"output":[{"type":"message","content":"top secret output"}],"usage":{"input_tokens":99,"output_tokens":88},"metadata":{"top":"top-secret"},"reasoning":{"effort":"high"},"tools":[{"type":"function","name":"top_secret_tool"}],"tool_choice":"auto","parallel_tool_calls":true,"prompt_cache_key":"top-secret-cache","previous_response_id":"resp_prev_top","text":{"verbosity":"high"},"truncation":"auto","max_output_tokens":8192,"incomplete_details":{"reason":"max"},"response":{"id":"resp_failed","status":"failed","instructions":"secret instructions","input":[{"role":"user","content":"sensitive prompt"}],"output":[{"type":"message","content":"secret output"}],"usage":{"input_tokens":11,"output_tokens":13,"input_tokens_details":{"cached_tokens":2},"cache_creation_input_tokens":3,"output_tokens_details":{"image_tokens":4}},"metadata":{"tenant":"tenant-secret"},"reasoning":{"effort":"high"},"tools":[{"type":"function","name":"secret_tool"}],"tool_choice":"auto","parallel_tool_calls":true,"prompt_cache_key":"secret-cache","previous_response_id":"resp_prev_inner","text":{"verbosity":"high"},"truncation":"auto","max_output_tokens":4096,"incomplete_details":{"reason":"max_output_tokens"},"error":{"code":"context_length_exceeded","message":"too long"}}}`)
	clientConn := newPassthroughTestFrameConn(nil, false)
	upstreamConn := newPassthroughTestFrameConn([]passthroughTestFrame{
		{msgType: coderws.MessageText, payload: failedPayload},
	}, true)

	firstPayload := []byte(`{"type":"response.create","model":"gpt-5.3-codex","input":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	turns := make([]RelayTurnResult, 0, 1)
	result, relayExit := Relay(ctx, clientConn, upstreamConn, firstPayload, RelayOptions{
		OnTurnComplete: func(turn RelayTurnResult) {
			turns = append(turns, turn)
		},
	})
	require.Nil(t, relayExit)
	require.Equal(t, "resp_failed", result.RequestID)
	require.Equal(t, "response.failed", result.TerminalEventType)
	require.Equal(t, 11, result.Usage.InputTokens)
	require.Equal(t, 13, result.Usage.OutputTokens)
	require.Equal(t, 2, result.Usage.CacheReadInputTokens)
	require.Equal(t, 3, result.Usage.CacheCreationInputTokens)
	require.Equal(t, 4, result.Usage.ImageOutputTokens)
	require.Len(t, turns, 1)
	require.Equal(t, "resp_failed", turns[0].RequestID)
	require.Equal(t, "response.failed", turns[0].TerminalEventType)
	require.Equal(t, result.Usage, turns[0].Usage)

	clientWrites := clientConn.Writes()
	require.Len(t, clientWrites, 1)
	require.Equal(t, coderws.MessageText, clientWrites[0].msgType)
	got := string(clientWrites[0].payload)
	require.JSONEq(t, `{"type":"response.failed","response":{"id":"resp_failed","status":"failed","error":{"code":"context_length_exceeded","message":"too long"}}}`, got)
	for _, sensitive := range []string{
		`"instructions"`,
		`"input"`,
		`"output"`,
		`"usage"`,
		`"metadata"`,
		`"reasoning"`,
		`"tools"`,
		`"tool_choice"`,
		`"parallel_tool_calls"`,
		`"prompt_cache_key"`,
		`"previous_response_id"`,
		`"text"`,
		`"truncation"`,
		`"max_output_tokens"`,
		`"incomplete_details"`,
		"sensitive prompt",
		"secret_tool",
		"tenant-secret",
	} {
		require.NotContains(t, got, sensitive)
	}
}

func TestRelay_BinaryResponseFailedFrameSanitizedAndNotObserved(t *testing.T) {
	t.Parallel()

	binaryPayload := []byte(`{"type":"response.failed","instructions":"top-secret instructions","input":[{"role":"user","content":"top secret prompt"}],"output":[{"type":"message","content":"top secret output"}],"usage":{"input_tokens":99,"output_tokens":88},"metadata":{"top":"top-secret"},"reasoning":{"effort":"high"},"tools":[{"type":"function","name":"top_secret_tool"}],"tool_choice":"auto","parallel_tool_calls":true,"prompt_cache_key":"top-secret-cache","previous_response_id":"resp_prev_top","text":{"verbosity":"high"},"truncation":"auto","max_output_tokens":8192,"incomplete_details":{"reason":"max"},"response":{"id":"resp_binary_failed","status":"failed","instructions":"secret instructions","input":[{"role":"user","content":"sensitive prompt"}],"output":[{"type":"message","content":"secret output"}],"usage":{"input_tokens":7,"output_tokens":3},"metadata":{"tenant":"secret"},"reasoning":{"effort":"high"},"tools":[{"type":"function","name":"secret_tool"}],"tool_choice":"auto","parallel_tool_calls":true,"prompt_cache_key":"secret-cache","previous_response_id":"resp_prev_inner","text":{"verbosity":"high"},"truncation":"auto","max_output_tokens":4096,"incomplete_details":{"reason":"max_output_tokens"},"error":{"code":"bad"}}}`)
	clientConn := newPassthroughTestFrameConn(nil, false)
	upstreamConn := newPassthroughTestFrameConn([]passthroughTestFrame{
		{msgType: coderws.MessageBinary, payload: binaryPayload},
	}, true)

	firstPayload := []byte(`{"type":"response.create","model":"gpt-4o","input":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	turns := make([]RelayTurnResult, 0, 1)
	result, relayExit := Relay(ctx, clientConn, upstreamConn, firstPayload, RelayOptions{
		OnTurnComplete: func(turn RelayTurnResult) {
			turns = append(turns, turn)
		},
	})
	require.Nil(t, relayExit)
	require.Equal(t, 0, result.Usage.InputTokens)
	require.Equal(t, "", result.RequestID)
	require.Equal(t, "", result.TerminalEventType)
	require.Empty(t, turns)

	clientWrites := clientConn.Writes()
	require.Len(t, clientWrites, 1)
	require.Equal(t, coderws.MessageBinary, clientWrites[0].msgType)
	require.JSONEq(t, `{"type":"response.failed","response":{"id":"resp_binary_failed","status":"failed","error":{"code":"bad"}}}`, string(clientWrites[0].payload))
	require.NotContains(t, string(clientWrites[0].payload), "tenant")
	require.NotContains(t, string(clientWrites[0].payload), "usage")
}

func TestRelay_NonJSONTextFramePreserved(t *testing.T) {
	t.Parallel()

	payload := []byte(`not-json {"type":"response.failed","response":{"usage":{"input_tokens":7,"output_tokens":3}}}`)
	clientConn := newPassthroughTestFrameConn(nil, false)
	upstreamConn := newPassthroughTestFrameConn([]passthroughTestFrame{
		{msgType: coderws.MessageText, payload: payload},
	}, true)

	firstPayload := []byte(`{"type":"response.create","model":"gpt-4o","input":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, relayExit := Relay(ctx, clientConn, upstreamConn, firstPayload, RelayOptions{})
	require.Nil(t, relayExit)

	clientWrites := clientConn.Writes()
	require.Len(t, clientWrites, 1)
	require.Equal(t, coderws.MessageText, clientWrites[0].msgType)
	require.Equal(t, payload, clientWrites[0].payload)
}

func TestRelay_NonFailedJSONFramesPreserved(t *testing.T) {
	t.Parallel()

	deltaPayload := []byte(`{"type":"response.output_text.delta","delta":"hi","metadata":{"tenant":"keep"},"tools":[{"name":"keep_tool"}]}`)
	completedPayload := []byte(`{"type":"response.completed","instructions":"keep instructions","output":[{"type":"message","content":"keep output"}],"metadata":{"tenant":"keep"},"tools":[{"name":"keep_tool"}],"response":{"id":"resp_completed","output":[{"type":"message","content":"keep output"}],"usage":{"input_tokens":2,"output_tokens":1},"metadata":{"tenant":"keep"}}}`)
	clientConn := newPassthroughTestFrameConn(nil, false)
	upstreamConn := newPassthroughTestFrameConn([]passthroughTestFrame{
		{msgType: coderws.MessageText, payload: deltaPayload},
		{msgType: coderws.MessageText, payload: completedPayload},
	}, true)

	firstPayload := []byte(`{"type":"response.create","model":"gpt-4o","input":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, relayExit := Relay(ctx, clientConn, upstreamConn, firstPayload, RelayOptions{})
	require.Nil(t, relayExit)
	require.Equal(t, "resp_completed", result.RequestID)
	require.Equal(t, "response.completed", result.TerminalEventType)
	require.Equal(t, 2, result.Usage.InputTokens)
	require.Equal(t, 1, result.Usage.OutputTokens)

	clientWrites := clientConn.Writes()
	require.Len(t, clientWrites, 2)
	require.Equal(t, coderws.MessageText, clientWrites[0].msgType)
	require.Equal(t, deltaPayload, clientWrites[0].payload)
	require.Equal(t, coderws.MessageText, clientWrites[1].msgType)
	require.Equal(t, completedPayload, clientWrites[1].payload)
}
func TestRelay_OnTurnComplete_ProvidesTurnMetrics(t *testing.T) {
	t.Parallel()

	clientConn := newPassthroughTestFrameConn(nil, false)
	upstreamConn := newPassthroughTestFrameConn([]passthroughTestFrame{
		{
			msgType: coderws.MessageText,
			payload: []byte(`{"type":"response.output_text.delta","response_id":"resp_metric","delta":"hi"}`),
		},
		{
			msgType: coderws.MessageText,
			payload: []byte(`{"type":"response.completed","response":{"id":"resp_metric","usage":{"input_tokens":2,"output_tokens":1}}}`),
		},
	}, true)

	firstPayload := []byte(`{"type":"response.create","model":"gpt-5.3-codex","input":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	base := time.Unix(0, 0)
	var nowTick atomic.Int64
	nowFn := func() time.Time {
		step := nowTick.Add(1)
		return base.Add(time.Duration(step) * 5 * time.Millisecond)
	}

	var turn RelayTurnResult
	result, relayExit := Relay(ctx, clientConn, upstreamConn, firstPayload, RelayOptions{
		Now: nowFn,
		OnTurnComplete: func(current RelayTurnResult) {
			turn = current
		},
	})
	require.Nil(t, relayExit)
	require.Equal(t, "resp_metric", turn.RequestID)
	require.Equal(t, "response.completed", turn.TerminalEventType)
	require.NotNil(t, turn.FirstTokenMs)
	require.GreaterOrEqual(t, *turn.FirstTokenMs, 0)
	require.Greater(t, turn.Duration.Milliseconds(), int64(0))
	require.NotNil(t, result.FirstTokenMs)
	require.Greater(t, result.Duration.Milliseconds(), int64(0))
}

func TestRelay_BinaryFramePassthrough(t *testing.T) {
	t.Parallel()

	// 验证 binary frame 被透传但不进行 usage 解析
	binaryPayload := []byte{0x00, 0x01, 0x02, 0x03}
	clientConn := newPassthroughTestFrameConn(nil, false)
	upstreamConn := newPassthroughTestFrameConn([]passthroughTestFrame{
		{
			msgType: coderws.MessageBinary,
			payload: binaryPayload,
		},
	}, true)

	firstPayload := []byte(`{"type":"response.create","model":"gpt-4o","input":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, relayExit := Relay(ctx, clientConn, upstreamConn, firstPayload, RelayOptions{})
	require.Nil(t, relayExit)
	// binary frame 不解析 usage
	require.Equal(t, 0, result.Usage.InputTokens)

	clientWrites := clientConn.Writes()
	require.Len(t, clientWrites, 1)
	require.Equal(t, coderws.MessageBinary, clientWrites[0].msgType)
	require.Equal(t, binaryPayload, clientWrites[0].payload)
}

func TestRelay_BinaryJSONFrameSkipsObservation(t *testing.T) {
	t.Parallel()

	clientConn := newPassthroughTestFrameConn(nil, false)
	upstreamConn := newPassthroughTestFrameConn([]passthroughTestFrame{
		{
			msgType: coderws.MessageBinary,
			payload: []byte(`{"type":"response.completed","response":{"id":"resp_binary","usage":{"input_tokens":7,"output_tokens":3}}}`),
		},
	}, true)

	firstPayload := []byte(`{"type":"response.create","model":"gpt-4o","input":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, relayExit := Relay(ctx, clientConn, upstreamConn, firstPayload, RelayOptions{})
	require.Nil(t, relayExit)
	require.Equal(t, 0, result.Usage.InputTokens)
	require.Equal(t, "", result.RequestID)
	require.Equal(t, "", result.TerminalEventType)

	clientWrites := clientConn.Writes()
	require.Len(t, clientWrites, 1)
	require.Equal(t, coderws.MessageBinary, clientWrites[0].msgType)
}

func TestRelay_UpstreamErrorEventPassthroughRaw(t *testing.T) {
	t.Parallel()

	clientConn := newPassthroughTestFrameConn(nil, false)
	errorEvent := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"No tool call found"}}`)
	upstreamConn := newPassthroughTestFrameConn([]passthroughTestFrame{
		{
			msgType: coderws.MessageText,
			payload: errorEvent,
		},
	}, true)

	firstPayload := []byte(`{"type":"response.create","model":"gpt-4o","input":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, relayExit := Relay(ctx, clientConn, upstreamConn, firstPayload, RelayOptions{})
	require.Nil(t, relayExit)
	require.Equal(t, 0, result.Usage.InputTokens)
	require.Equal(t, "", result.RequestID)
	require.Equal(t, "", result.TerminalEventType)

	clientWrites := clientConn.Writes()
	require.Len(t, clientWrites, 1)
	require.Equal(t, coderws.MessageText, clientWrites[0].msgType)
	require.Equal(t, errorEvent, clientWrites[0].payload)
}

func TestRelay_PreservesFirstMessageType(t *testing.T) {
	t.Parallel()

	clientConn := newPassthroughTestFrameConn(nil, false)
	upstreamConn := newPassthroughTestFrameConn(nil, true)

	firstPayload := []byte(`{"type":"response.create","model":"gpt-4o","input":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, relayExit := Relay(ctx, clientConn, upstreamConn, firstPayload, RelayOptions{
		FirstMessageType: coderws.MessageBinary,
	})
	require.Nil(t, relayExit)

	upstreamWrites := upstreamConn.Writes()
	require.Len(t, upstreamWrites, 1)
	require.Equal(t, coderws.MessageBinary, upstreamWrites[0].msgType)
	require.Equal(t, firstPayload, upstreamWrites[0].payload)
}

func TestRelay_UsageParseFailureDoesNotBlockRelay(t *testing.T) {
	baseline := SnapshotMetrics().UsageParseFailureTotal

	// 上游发送无效 JSON（非 usage 格式），不应影响透传
	clientConn := newPassthroughTestFrameConn(nil, false)
	upstreamConn := newPassthroughTestFrameConn([]passthroughTestFrame{
		{
			msgType: coderws.MessageText,
			payload: []byte(`{"type":"response.completed","response":{"id":"resp_bad","usage":"not_an_object"}}`),
		},
	}, true)

	firstPayload := []byte(`{"type":"response.create","model":"gpt-4o","input":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result, relayExit := Relay(ctx, clientConn, upstreamConn, firstPayload, RelayOptions{})
	require.Nil(t, relayExit)
	// usage 解析失败，值为 0 但不影响透传
	require.Equal(t, 0, result.Usage.InputTokens)
	require.Equal(t, "response.completed", result.TerminalEventType)

	// 帧仍然被转发
	clientWrites := clientConn.Writes()
	require.Len(t, clientWrites, 1)
	require.GreaterOrEqual(t, SnapshotMetrics().UsageParseFailureTotal, baseline+1)
}

func TestRelay_WriteUpstreamFirstMessageFails(t *testing.T) {
	t.Parallel()

	// 上游连接立即关闭，首包写入失败
	upstreamConn := newPassthroughTestFrameConn(nil, true)
	_ = upstreamConn.Close()

	// 覆盖 WriteFrame 使其返回错误
	errConn := &errorOnWriteFrameConn{}
	clientConn := newPassthroughTestFrameConn(nil, false)

	firstPayload := []byte(`{"type":"response.create","model":"gpt-4o","input":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, relayExit := Relay(ctx, clientConn, errConn, firstPayload, RelayOptions{})
	require.NotNil(t, relayExit)
	require.Equal(t, "write_upstream", relayExit.Stage)
}

func TestRelay_ContextCanceled(t *testing.T) {
	t.Parallel()

	clientConn := newPassthroughTestFrameConn(nil, false)
	upstreamConn := newPassthroughTestFrameConn(nil, false)

	firstPayload := []byte(`{"type":"response.create","model":"gpt-4o","input":[]}`)

	// 立即取消 context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, relayExit := Relay(ctx, clientConn, upstreamConn, firstPayload, RelayOptions{})
	// context 取消导致写首包失败
	require.NotNil(t, relayExit)
}

func TestRelay_TraceEvents_ContainsLifecycleStages(t *testing.T) {
	t.Parallel()

	clientConn := newPassthroughTestFrameConn(nil, false)
	upstreamConn := newPassthroughTestFrameConn([]passthroughTestFrame{
		{
			msgType: coderws.MessageText,
			payload: []byte(`{"type":"response.completed","response":{"id":"resp_trace","usage":{"input_tokens":1,"output_tokens":1}}}`),
		},
	}, true)

	firstPayload := []byte(`{"type":"response.create","model":"gpt-4o","input":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	stages := make([]string, 0, 8)
	var stagesMu sync.Mutex
	_, relayExit := Relay(ctx, clientConn, upstreamConn, firstPayload, RelayOptions{
		OnTrace: func(event RelayTraceEvent) {
			stagesMu.Lock()
			stages = append(stages, event.Stage)
			stagesMu.Unlock()
		},
	})
	require.Nil(t, relayExit)
	stagesMu.Lock()
	capturedStages := append([]string(nil), stages...)
	stagesMu.Unlock()
	require.Contains(t, capturedStages, "relay_start")
	require.Contains(t, capturedStages, "write_first_message_ok")
	require.Contains(t, capturedStages, "first_exit")
	require.Contains(t, capturedStages, "relay_complete")
}

func TestRelay_TraceEvents_IdleTimeout(t *testing.T) {
	t.Parallel()

	clientConn := newPassthroughTestFrameConn(nil, false)
	upstreamConn := newPassthroughTestFrameConn(nil, false)

	firstPayload := []byte(`{"type":"response.create","model":"gpt-4o","input":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	now := time.Now()
	callCount := 0
	nowFn := func() time.Time {
		callCount++
		if callCount <= 5 {
			return now
		}
		return now.Add(time.Hour)
	}

	stages := make([]string, 0, 8)
	var stagesMu sync.Mutex
	_, relayExit := Relay(ctx, clientConn, upstreamConn, firstPayload, RelayOptions{
		IdleTimeout: 2 * time.Second,
		Now:         nowFn,
		OnTrace: func(event RelayTraceEvent) {
			stagesMu.Lock()
			stages = append(stages, event.Stage)
			stagesMu.Unlock()
		},
	})
	require.NotNil(t, relayExit)
	require.Equal(t, "idle_timeout", relayExit.Stage)
	stagesMu.Lock()
	capturedStages := append([]string(nil), stages...)
	stagesMu.Unlock()
	require.Contains(t, capturedStages, "idle_timeout_triggered")
	require.Contains(t, capturedStages, "relay_exit")
}

// errorOnWriteFrameConn 是一个写入总是失败的 FrameConn 实现，用于测试首包写入失败。
type errorOnWriteFrameConn struct{}

func (c *errorOnWriteFrameConn) ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error) {
	<-ctx.Done()
	return coderws.MessageText, nil, ctx.Err()
}

func (c *errorOnWriteFrameConn) WriteFrame(_ context.Context, _ coderws.MessageType, _ []byte) error {
	return errors.New("write failed: connection refused")
}

func (c *errorOnWriteFrameConn) Close() error {
	return nil
}

func TestRelay_OnTurnComplete_RealOpenAIStream_FirstTokenMs(t *testing.T) {
	t.Parallel()

	clientConn := newPassthroughTestFrameConn(nil, false)
	upstreamConn := newPassthroughTestFrameConn([]passthroughTestFrame{
		{
			msgType: coderws.MessageText,
			payload: []byte(`{"type":"response.created","response":{"id":"resp_real"}}`),
		},
		{
			msgType: coderws.MessageText,
			payload: []byte(`{"type":"response.output_text.delta","delta":"He"}`),
		},
		{
			msgType: coderws.MessageText,
			payload: []byte(`{"type":"response.output_text.delta","delta":"llo"}`),
		},
		{
			msgType: coderws.MessageText,
			payload: []byte(`{"type":"response.output_text.delta","delta":" world"}`),
		},
		{
			msgType: coderws.MessageText,
			payload: []byte(`{"type":"response.completed","response":{"id":"resp_real","usage":{"input_tokens":2,"output_tokens":3}}}`),
		},
	}, true)

	firstPayload := []byte(`{"type":"response.create","model":"gpt-5.3-codex","input":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	base := time.Unix(0, 0)
	var nowTick atomic.Int64
	nowFn := func() time.Time {
		step := nowTick.Add(1)
		return base.Add(time.Duration(step) * 10 * time.Millisecond)
	}

	var turn RelayTurnResult
	result, relayExit := Relay(ctx, clientConn, upstreamConn, firstPayload, RelayOptions{
		Now: nowFn,
		OnTurnComplete: func(current RelayTurnResult) {
			turn = current
		},
	})
	require.Nil(t, relayExit)
	require.Equal(t, "resp_real", turn.RequestID)
	require.Equal(t, "response.completed", turn.TerminalEventType)

	require.NotNil(t, turn.FirstTokenMs, "per-turn FirstTokenMs must be captured for real OpenAI streams")
	require.Greater(t, turn.Duration.Milliseconds(), int64(0))

	require.Less(t,
		int64(*turn.FirstTokenMs),
		turn.Duration.Milliseconds(),
		"per-turn FirstTokenMs (%dms) should be strictly less than Duration (%dms); "+
			"equality indicates the bug where first_token is mistakenly stamped on the terminal event",
		*turn.FirstTokenMs, turn.Duration.Milliseconds(),
	)

	require.NotNil(t, result.FirstTokenMs)
	require.Greater(t, *result.FirstTokenMs, 0)
}
