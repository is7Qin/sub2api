package openai_ws_v2

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type FrameConn interface {
	ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error)
	WriteFrame(ctx context.Context, msgType coderws.MessageType, payload []byte) error
	Close() error
}

type Usage struct {
	InputTokens              int
	OutputTokens             int
	CacheCreationInputTokens int
	CacheReadInputTokens     int
	ImageOutputTokens        int
}

type RelayResult struct {
	RequestModel            string
	Usage                   Usage
	RequestID               string
	TerminalEventType       string
	FirstTokenMs            *int
	Duration                time.Duration
	ClientToUpstreamFrames  int64
	UpstreamToClientFrames  int64
	DroppedDownstreamFrames int64
}

type RelaySemanticTerminal struct {
	Terminal  bool
	StopRelay bool
}

type RelayTurnResult struct {
	RequestModel         string
	Usage                Usage
	RequestID            string
	TerminalEventType    string
	TerminalPayload      []byte
	Duration             time.Duration
	FirstTokenMs         *int
	ClientEventDelivered bool
}

type RelayExit struct {
	Stage           string
	Err             error
	WroteDownstream bool
}

type RelayOptions struct {
	WriteTimeout                    time.Duration
	IdleTimeout                     time.Duration
	UpstreamDrainTimeout            time.Duration
	FirstMessageType                coderws.MessageType
	FirstMessageSent                bool
	StartClientAfterFirstDownstream bool
	OnUsageParseFailure             func(eventType string, usageRaw string)
	OnTurnComplete                  func(turn RelayTurnResult)
	ClassifySemanticTerminal        func(msgType coderws.MessageType, payload []byte, eventType string) RelaySemanticTerminal
	BeforeWriteClient               func(msgType coderws.MessageType, payload []byte, wroteDownstream bool) error
	TransformWriteClient            func(msgType coderws.MessageType, payload []byte, eventType string) ([]byte, error)
	ReadClientFrame                 func(ctx context.Context, clientConn FrameConn, markActivity func()) (coderws.MessageType, []byte, error)
	OnTrace                         func(event RelayTraceEvent)
	Now                             func() time.Time
}

type RelayTraceEvent struct {
	Stage           string
	Direction       string
	MessageType     string
	PayloadBytes    int
	Graceful        bool
	WroteDownstream bool
	Error           string
}

type relayState struct {
	usage             Usage
	requestModel      string
	lastResponseID    string
	terminalEventType string
	firstTokenMs      *int
	turnTimingByID    map[string]*relayTurnTiming
	// Kept for the full connection: a replay can arrive after its turn timing
	// is released, so removing IDs with active-turn state would re-enable billing.
	completedResponseIDs     map[string]struct{}
	activeTurn               *relayTurnTiming
	classifySemanticTerminal func(msgType coderws.MessageType, payload []byte, eventType string) RelaySemanticTerminal

	// ID-less terminal events have no response ID to key replay suppression on.
	// The client turn sequence provides a bounded per-turn key without
	// conflating two distinct ID-less response.create requests.
	idlessTurnGeneration     atomic.Uint64
	idlessTerminalGeneration atomic.Uint64
	completedTurnGeneration  atomic.Uint64
}

type relayExitSignal struct {
	stage           string
	err             error
	graceful        bool
	wroteDownstream bool
}

type downstreamWriteGate struct {
	mu   sync.Mutex
	drop atomic.Bool
}

func (g *downstreamWriteGate) disable() {
	if g == nil {
		return
	}
	// Publishing sink loss must not wait for an already admitted transport
	// write; that write holds the mutex until it returns. The atomic marker
	// prevents every later admission while allowing terminal drain to start.
	g.drop.Store(true)
}

func (g *downstreamWriteGate) withWrite(write func() error) (bool, error) {
	if g == nil {
		return true, write()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.drop.Load() {
		return false, nil
	}
	return true, write()
}

func (g *downstreamWriteGate) disabled() bool {
	return g != nil && g.drop.Load()
}

type observedUpstreamEvent struct {
	terminal             bool
	eventType            string
	responseID           string
	payload              []byte
	usage                Usage
	duration             time.Duration
	firstToken           *int
	clientEventDelivered bool
	stopRelay            bool
}

type relayTurnTiming struct {
	startAt      time.Time
	firstTokenMs *int
	generation   uint64
}

func Relay(
	ctx context.Context,
	clientConn FrameConn,
	upstreamConn FrameConn,
	firstClientMessage []byte,
	options RelayOptions,
) (RelayResult, *RelayExit) {
	result := RelayResult{RequestModel: strings.TrimSpace(gjson.GetBytes(firstClientMessage, "model").String())}
	if clientConn == nil || upstreamConn == nil {
		return result, &RelayExit{Stage: "relay_init", Err: errors.New("relay connection is nil")}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	nowFn := options.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	writeTimeout := options.WriteTimeout
	if writeTimeout <= 0 {
		writeTimeout = 2 * time.Minute
	}
	drainTimeout := options.UpstreamDrainTimeout
	if drainTimeout <= 0 {
		// This is a hard safety deadline only. Normal accounting ends earlier on
		// the terminal event or upstream close, so a short write window cannot
		// become the accounting boundary.
		drainTimeout = 30 * time.Second
	}
	firstMessageType := options.FirstMessageType
	if firstMessageType != coderws.MessageBinary {
		firstMessageType = coderws.MessageText
	}
	startAt := nowFn()
	state := &relayState{
		requestModel:             result.RequestModel,
		classifySemanticTerminal: options.ClassifySemanticTerminal,
	}
	state.idlessTurnGeneration.Store(1)
	onTrace := options.OnTrace

	relayCtx, relayCancel := context.WithCancel(ctx)
	defer relayCancel()

	lastActivity := atomic.Int64{}
	lastActivity.Store(nowFn().UnixNano())
	markActivity := func() {
		lastActivity.Store(nowFn().UnixNano())
	}

	writeUpstream := func(msgType coderws.MessageType, payload []byte) error {
		writeCtx, cancel := context.WithTimeout(relayCtx, writeTimeout)
		defer cancel()
		return upstreamConn.WriteFrame(writeCtx, msgType, payload)
	}
	writeClient := func(msgType coderws.MessageType, payload []byte) error {
		writeCtx, cancel := context.WithTimeout(relayCtx, writeTimeout)
		defer cancel()
		return clientConn.WriteFrame(writeCtx, msgType, payload)
	}

	clientToUpstreamFrames := &atomic.Int64{}
	upstreamToClientFrames := &atomic.Int64{}
	droppedDownstreamFrames := &atomic.Int64{}
	emitRelayTrace(onTrace, RelayTraceEvent{
		Stage:        "relay_start",
		PayloadBytes: len(firstClientMessage),
		MessageType:  relayMessageTypeString(firstMessageType),
	})

	if options.FirstMessageSent {
		emitRelayTrace(onTrace, RelayTraceEvent{
			Stage:        "write_first_message_skipped",
			Direction:    "client_to_upstream",
			MessageType:  relayMessageTypeString(firstMessageType),
			PayloadBytes: len(firstClientMessage),
		})
	} else {
		if err := writeUpstream(firstMessageType, firstClientMessage); err != nil {
			result.Duration = nowFn().Sub(startAt)
			emitRelayTrace(onTrace, RelayTraceEvent{
				Stage:        "write_first_message_failed",
				Direction:    "client_to_upstream",
				MessageType:  relayMessageTypeString(firstMessageType),
				PayloadBytes: len(firstClientMessage),
				Error:        err.Error(),
			})
			return result, &RelayExit{Stage: "write_upstream", Err: err}
		}
		emitRelayTrace(onTrace, RelayTraceEvent{
			Stage:        "write_first_message_ok",
			Direction:    "client_to_upstream",
			MessageType:  relayMessageTypeString(firstMessageType),
			PayloadBytes: len(firstClientMessage),
		})
	}
	clientToUpstreamFrames.Add(1)
	markActivity()

	exitCh := make(chan relayExitSignal, 8)
	downstreamWrites := &downstreamWriteGate{}
	clientReaderStarted := atomic.Bool{}
	clientReaderDone := make(chan struct{})
	upstreamDone := make(chan struct{})
	watchdogDone := make(chan struct{})
	startClientReader := func() {
		if !clientReaderStarted.CompareAndSwap(false, true) {
			return
		}
		go func() {
			defer close(clientReaderDone)
			runClientToUpstream(relayCtx, clientConn, options.ReadClientFrame, writeUpstream, markActivity, clientToUpstreamFrames, onTrace, state, downstreamWrites, exitCh)
		}()
	}
	if !options.StartClientAfterFirstDownstream {
		startClientReader()
	}
	go func() {
		defer close(upstreamDone)
		runUpstreamToClient(
			relayCtx,
			upstreamConn,
			writeClient,
			startAt,
			nowFn,
			state,
			options.OnUsageParseFailure,
			options.OnTurnComplete,
			options.BeforeWriteClient,
			options.TransformWriteClient,
			func() {
				if options.StartClientAfterFirstDownstream {
					startClientReader()
				}
			},
			downstreamWrites,
			upstreamToClientFrames,
			droppedDownstreamFrames,
			markActivity,
			onTrace,
			exitCh,
		)
	}()
	go func() {
		defer close(watchdogDone)
		runIdleWatchdog(relayCtx, nowFn, options.IdleTimeout, &lastActivity, onTrace, exitCh)
	}()

	firstExit, _ := waitRelayExitContext(ctx, exitCh)
	// A disconnect may already be visible to the client reader when the idle
	// watchdog publishes concurrently. Prefer the disconnect drain boundary.
	if firstExit.stage == "idle_timeout" && downstreamWrites.disabled() {
		firstExit, _ = waitRelayExitContext(ctx, exitCh)
	}
	emitRelayTrace(onTrace, RelayTraceEvent{
		Stage:           "first_exit",
		Direction:       relayDirectionFromStage(firstExit.stage),
		Graceful:        firstExit.graceful,
		WroteDownstream: firstExit.wroteDownstream,
		Error:           relayErrorString(firstExit.err),
	})
	combinedWroteDownstream := firstExit.wroteDownstream
	secondExit := relayExitSignal{graceful: true}
	hasSecondExit := false

	// Once the client disconnects or a downstream write fails, continue
	// observing the admitted upstream turn until terminal, close, or deadline.
	if (firstExit.stage == "read_client" && firstExit.graceful) || firstExit.stage == "write_client" {
		downstreamWrites.disable()
		if state.completedTurnGeneration.Load() < state.idlessTurnGeneration.Load() {
			secondExit, hasSecondExit = waitTerminalDrainExit(ctx, exitCh, drainTimeout)
		}
	} else {
		relayCancel()
		_ = upstreamConn.Close()
		if clientReaderStarted.Load() {
			secondExit, hasSecondExit = waitRelayExit(exitCh, 200*time.Millisecond)
		}
	}
	if hasSecondExit {
		combinedWroteDownstream = combinedWroteDownstream || secondExit.wroteDownstream
		emitRelayTrace(onTrace, RelayTraceEvent{
			Stage:           "second_exit",
			Direction:       relayDirectionFromStage(secondExit.stage),
			Graceful:        secondExit.graceful,
			WroteDownstream: secondExit.wroteDownstream,
			Error:           relayErrorString(secondExit.err),
		})
	}

	relayCancel()
	_ = upstreamConn.Close()
	// Close/cancel unblocks transport reads; join every worker before returning
	// so no late callback or downstream write can outlive the relay.
	<-upstreamDone
	if clientReaderStarted.Load() {
		<-clientReaderDone
	}
	<-watchdogDone

	enrichResult(&result, state, nowFn().Sub(startAt))
	result.ClientToUpstreamFrames = clientToUpstreamFrames.Load()
	result.UpstreamToClientFrames = upstreamToClientFrames.Load()
	result.DroppedDownstreamFrames = droppedDownstreamFrames.Load()
	if options.FirstMessageSent && firstExit.stage == "read_client" && firstExit.graceful {
		emitRelayTrace(onTrace, RelayTraceEvent{
			Stage:           "relay_client_closed",
			Graceful:        true,
			WroteDownstream: combinedWroteDownstream,
		})
		return result, nil
	}
	if firstExit.stage == "read_client" && firstExit.graceful {
		stage := "client_disconnected"
		exitErr := firstExit.err
		if hasSecondExit && !secondExit.graceful && secondExit.stage != "read_upstream" {
			stage = secondExit.stage
			exitErr = secondExit.err
		}
		if exitErr == nil {
			exitErr = io.EOF
		}
		emitRelayTrace(onTrace, RelayTraceEvent{
			Stage:           "relay_exit",
			Direction:       relayDirectionFromStage(stage),
			Graceful:        false,
			WroteDownstream: combinedWroteDownstream,
			Error:           relayErrorString(exitErr),
		})
		return result, &RelayExit{
			Stage:           stage,
			Err:             exitErr,
			WroteDownstream: combinedWroteDownstream,
		}
	}
	if firstExit.graceful && (!hasSecondExit || secondExit.graceful) {
		emitRelayTrace(onTrace, RelayTraceEvent{
			Stage:           "relay_complete",
			Graceful:        true,
			WroteDownstream: combinedWroteDownstream,
		})
		_ = clientConn.Close()
		return result, nil
	}
	if !firstExit.graceful {
		emitRelayTrace(onTrace, RelayTraceEvent{
			Stage:           "relay_exit",
			Direction:       relayDirectionFromStage(firstExit.stage),
			Graceful:        false,
			WroteDownstream: combinedWroteDownstream,
			Error:           relayErrorString(firstExit.err),
		})
		return result, &RelayExit{
			Stage:           firstExit.stage,
			Err:             firstExit.err,
			WroteDownstream: combinedWroteDownstream,
		}
	}
	if hasSecondExit && !secondExit.graceful {
		emitRelayTrace(onTrace, RelayTraceEvent{
			Stage:           "relay_exit",
			Direction:       relayDirectionFromStage(secondExit.stage),
			Graceful:        false,
			WroteDownstream: combinedWroteDownstream,
			Error:           relayErrorString(secondExit.err),
		})
		return result, &RelayExit{
			Stage:           secondExit.stage,
			Err:             secondExit.err,
			WroteDownstream: combinedWroteDownstream,
		}
	}
	if options.FirstMessageSent {
		emitRelayTrace(onTrace, RelayTraceEvent{
			Stage:           "relay_client_closed",
			Graceful:        true,
			WroteDownstream: combinedWroteDownstream,
		})
		return result, nil
	}
	emitRelayTrace(onTrace, RelayTraceEvent{
		Stage:           "relay_complete",
		Graceful:        true,
		WroteDownstream: combinedWroteDownstream,
	})
	_ = clientConn.Close()
	return result, nil
}

func runClientToUpstream(
	ctx context.Context,
	clientConn FrameConn,
	readClientFrame func(context.Context, FrameConn, func()) (coderws.MessageType, []byte, error),
	writeUpstream func(msgType coderws.MessageType, payload []byte) error,
	markActivity func(),
	forwardedFrames *atomic.Int64,
	onTrace func(event RelayTraceEvent),
	state *relayState,
	downstreamWrites *downstreamWriteGate,
	exitCh chan<- relayExitSignal,
) {
	if readClientFrame == nil {
		readClientFrame = func(ctx context.Context, conn FrameConn, _ func()) (coderws.MessageType, []byte, error) {
			return conn.ReadFrame(ctx)
		}
	}
	for {
		msgType, payload, err := readClientFrame(ctx, clientConn, markActivity)
		if err != nil {
			graceful := isDisconnectError(err)
			// Publish sink loss before notifying the coordinator so the upstream
			// reader cannot race one more downstream write through.
			if graceful && downstreamWrites != nil {
				downstreamWrites.disable()
			}
			emitRelayTrace(onTrace, RelayTraceEvent{
				Stage:     "read_client_failed",
				Direction: "client_to_upstream",
				Error:     err.Error(),
				Graceful:  graceful,
			})
			publishRelayExit(ctx, exitCh, relayExitSignal{stage: "read_client", err: err, graceful: graceful})
			return
		}
		isResponseCreate := msgType == coderws.MessageText && strings.TrimSpace(gjson.GetBytes(payload, "type").String()) == "response.create"
		var turnGeneration uint64
		if isResponseCreate && state != nil {
			turnGeneration = state.idlessTurnGeneration.Add(1)
		}
		if err := writeUpstream(msgType, payload); err != nil {
			if turnGeneration != 0 {
				state.idlessTurnGeneration.CompareAndSwap(turnGeneration, turnGeneration-1)
			}
			emitRelayTrace(onTrace, RelayTraceEvent{
				Stage:        "write_upstream_failed",
				Direction:    "client_to_upstream",
				MessageType:  relayMessageTypeString(msgType),
				PayloadBytes: len(payload),
				Error:        err.Error(),
			})
			publishRelayExit(ctx, exitCh, relayExitSignal{stage: "write_upstream", err: err})
			return
		}
		if forwardedFrames != nil {
			forwardedFrames.Add(1)
		}
		markActivity()
	}
}

func runUpstreamToClient(
	ctx context.Context,
	upstreamConn FrameConn,
	writeClient func(msgType coderws.MessageType, payload []byte) error,
	startAt time.Time,
	nowFn func() time.Time,
	state *relayState,
	onUsageParseFailure func(eventType string, usageRaw string),
	onTurnComplete func(turn RelayTurnResult),
	beforeWriteClient func(msgType coderws.MessageType, payload []byte, wroteDownstream bool) error,
	transformWriteClient func(msgType coderws.MessageType, payload []byte, eventType string) ([]byte, error),
	afterWriteClient func(),
	downstreamWrites *downstreamWriteGate,
	forwardedFrames *atomic.Int64,
	droppedFrames *atomic.Int64,
	markActivity func(),
	onTrace func(event RelayTraceEvent),
	exitCh chan<- relayExitSignal,
) {
	wroteDownstream := false
	for {
		msgType, payload, err := upstreamConn.ReadFrame(ctx)
		if err != nil {
			emitRelayTrace(onTrace, RelayTraceEvent{
				Stage:           "read_upstream_failed",
				Direction:       "upstream_to_client",
				Error:           err.Error(),
				Graceful:        isDisconnectError(err),
				WroteDownstream: wroteDownstream,
			})
			publishRelayExit(ctx, exitCh, relayExitSignal{
				stage:           "read_upstream",
				err:             err,
				graceful:        isDisconnectError(err),
				wroteDownstream: wroteDownstream,
			})
			return
		}
		markActivity()
		observedEvent := observedUpstreamEvent{}
		switch msgType {
		case coderws.MessageText:
			eventType := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
			semantic := RelaySemanticTerminal{}
			if state != nil && state.classifySemanticTerminal != nil {
				semantic = state.classifySemanticTerminal(msgType, payload, eventType)
			}
			observedEvent = observeUpstreamMessage(state, payload, startAt, nowFn, onUsageParseFailure, semantic.Terminal)
			observedEvent.stopRelay = semantic.StopRelay
		case coderws.MessageBinary:
			// binary frame 直接透传，不进入 JSON 观测路径（避免无效解析开销）。
		}
		// The terminal's own write is deliberately excluded: only an earlier
		// successful downstream frame establishes client-visible output.
		observedEvent.clientEventDelivered = wroteDownstream
		emitTurnComplete(onTurnComplete, state, observedEvent)
		if downstreamWrites != nil && downstreamWrites.disabled() {
			if droppedFrames != nil {
				droppedFrames.Add(1)
			}
			emitRelayTrace(onTrace, RelayTraceEvent{
				Stage:           "drop_downstream_frame",
				Direction:       "upstream_to_client",
				MessageType:     relayMessageTypeString(msgType),
				PayloadBytes:    len(payload),
				WroteDownstream: wroteDownstream,
			})
			if observedEvent.terminal {
				publishRelayExit(ctx, exitCh, relayExitSignal{
					stage:           "drain_terminal",
					graceful:        true,
					wroteDownstream: wroteDownstream,
				})
				return
			}
			markActivity()
			continue
		}
		if beforeWriteClient != nil {
			if err := beforeWriteClient(msgType, payload, wroteDownstream); err != nil {
				emitRelayTrace(onTrace, RelayTraceEvent{
					Stage:           "upstream_message_rejected",
					Direction:       "upstream_to_client",
					MessageType:     relayMessageTypeString(msgType),
					PayloadBytes:    len(payload),
					WroteDownstream: wroteDownstream,
					Error:           err.Error(),
				})
				if downstreamWrites.disabled() {
					if droppedFrames != nil {
						droppedFrames.Add(1)
					}
					if observedEvent.terminal {
						publishRelayExit(ctx, exitCh, relayExitSignal{stage: "drain_terminal", graceful: true, wroteDownstream: wroteDownstream})
						return
					}
					continue
				}
				publishRelayExit(ctx, exitCh, relayExitSignal{
					stage:           "upstream_message",
					err:             err,
					wroteDownstream: wroteDownstream,
				})
				return
			}
		}
		if (msgType == coderws.MessageText && observedEvent.eventType == "response.failed") || msgType == coderws.MessageBinary {
			payload, _ = sanitizeResponseFailedMessageForClient(payload)
		}
		if transformWriteClient != nil {
			transformed, transformErr := transformWriteClient(msgType, payload, observedEvent.eventType)
			if transformErr != nil {
				if downstreamWrites.disabled() {
					if droppedFrames != nil {
						droppedFrames.Add(1)
					}
					if observedEvent.terminal {
						publishRelayExit(ctx, exitCh, relayExitSignal{stage: "drain_terminal", graceful: true, wroteDownstream: wroteDownstream})
						return
					}
					continue
				}
				publishRelayExit(ctx, exitCh, relayExitSignal{
					stage:           "upstream_message",
					err:             transformErr,
					wroteDownstream: wroteDownstream,
				})
				return
			}
			payload = transformed
		}
		writeStarted, writeErr := downstreamWrites.withWrite(func() error {
			return writeClient(msgType, payload)
		})
		if !writeStarted {
			if droppedFrames != nil {
				droppedFrames.Add(1)
			}
			if observedEvent.terminal {
				publishRelayExit(ctx, exitCh, relayExitSignal{stage: "drain_terminal", graceful: true, wroteDownstream: wroteDownstream})
				return
			}
			continue
		}
		if observedEvent.stopRelay && writeErr != nil {
			if downstreamWrites != nil {
				downstreamWrites.disable()
			}
			publishRelayExit(ctx, exitCh, relayExitSignal{stage: "write_client", err: writeErr, wroteDownstream: wroteDownstream})
			return
		}
		if observedEvent.stopRelay && writeErr == nil {
			wroteDownstream = true
			if afterWriteClient != nil {
				afterWriteClient()
			}
			if forwardedFrames != nil {
				forwardedFrames.Add(1)
			}
			publishRelayExit(ctx, exitCh, relayExitSignal{stage: "semantic_terminal", graceful: true, wroteDownstream: wroteDownstream})
			return
		}
		if writeErr != nil {
			emitRelayTrace(onTrace, RelayTraceEvent{
				Stage:           "write_client_failed",
				Direction:       "upstream_to_client",
				MessageType:     relayMessageTypeString(msgType),
				PayloadBytes:    len(payload),
				WroteDownstream: wroteDownstream,
				Error:           writeErr.Error(),
			})
			// The client is no longer a usable sink, but the admitted upstream
			// turn still needs terminal observation for accounting.
			if downstreamWrites != nil {
				downstreamWrites.disable()
			}
			if afterWriteClient == nil {
				publishRelayExit(ctx, exitCh, relayExitSignal{stage: "write_client", err: writeErr, wroteDownstream: wroteDownstream})
				return
			}
			publishRelayExit(ctx, exitCh, relayExitSignal{stage: "write_client", err: writeErr, wroteDownstream: wroteDownstream})
			if observedEvent.terminal {
				publishRelayExit(ctx, exitCh, relayExitSignal{stage: "drain_terminal", graceful: true, wroteDownstream: wroteDownstream})
				return
			}
			continue
		}
		wroteDownstream = true
		if afterWriteClient != nil {
			afterWriteClient()
		}
		if forwardedFrames != nil {
			forwardedFrames.Add(1)
		}
		markActivity()
	}
}

func runIdleWatchdog(
	ctx context.Context,
	nowFn func() time.Time,
	idleTimeout time.Duration,
	lastActivity *atomic.Int64,
	onTrace func(event RelayTraceEvent),
	exitCh chan<- relayExitSignal,
) {
	if idleTimeout <= 0 {
		return
	}
	checkInterval := minDuration(idleTimeout/4, 5*time.Second)
	if checkInterval < time.Second {
		checkInterval = time.Second
	}
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			last := time.Unix(0, lastActivity.Load())
			if nowFn().Sub(last) < idleTimeout {
				continue
			}
			emitRelayTrace(onTrace, RelayTraceEvent{
				Stage:     "idle_timeout_triggered",
				Direction: "watchdog",
				Error:     context.DeadlineExceeded.Error(),
			})
			publishRelayExit(ctx, exitCh, relayExitSignal{stage: "idle_timeout", err: context.DeadlineExceeded})
			return
		}
	}
}

func publishRelayExit(ctx context.Context, exitCh chan<- relayExitSignal, signal relayExitSignal) {
	select {
	case exitCh <- signal:
	case <-ctx.Done():
	}
}

func emitRelayTrace(onTrace func(event RelayTraceEvent), event RelayTraceEvent) {
	if onTrace == nil {
		return
	}
	onTrace(event)
}

func relayMessageTypeString(msgType coderws.MessageType) string {
	switch msgType {
	case coderws.MessageText:
		return "text"
	case coderws.MessageBinary:
		return "binary"
	default:
		return "unknown(" + strconv.Itoa(int(msgType)) + ")"
	}
}

func relayDirectionFromStage(stage string) string {
	switch stage {
	case "read_client", "write_upstream":
		return "client_to_upstream"
	case "read_upstream", "write_client", "drain_terminal":
		return "upstream_to_client"
	case "idle_timeout":
		return "watchdog"
	default:
		return ""
	}
}

func relayErrorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func observeUpstreamMessage(
	state *relayState,
	message []byte,
	startAt time.Time,
	nowFn func() time.Time,
	onUsageParseFailure func(eventType string, usageRaw string),
	semanticTerminal ...bool,
) observedUpstreamEvent {
	if state == nil || len(message) == 0 {
		return observedUpstreamEvent{}
	}
	semantic := len(semanticTerminal) > 0 && semanticTerminal[0]
	values := gjson.GetManyBytes(message, "type", "response.id", "response_id", "id")
	eventType := strings.TrimSpace(values[0].String())
	if eventType == "" {
		return observedUpstreamEvent{}
	}
	responseID := strings.TrimSpace(values[1].String())
	if responseID == "" {
		responseID = strings.TrimSpace(values[2].String())
	}
	// Only native response terminals may fall back to a top-level ID. A
	// classifier-only semantic terminal can be an event envelope, not a turn.
	if responseID == "" && isTerminalEvent(eventType) && !semantic {
		responseID = strings.TrimSpace(values[3].String())
	}
	now := nowFn()
	// An upstream may replay a terminal frame while closing. Ignore duplicate
	// bookkeeping so it cannot double-charge or release a later turn's slots.
	effectiveTerminal := isTerminalEvent(eventType) || semantic
	if effectiveTerminal {
		if responseID != "" && !openAIWSRelayMarkResponseCompleted(state, responseID) {
			return observedUpstreamEvent{eventType: eventType, responseID: responseID}
		}
		if responseID == "" && !openAIWSRelayMarkIDLessTerminal(state) {
			return observedUpstreamEvent{eventType: eventType}
		}
	}

	if state.firstTokenMs == nil && isTokenEvent(eventType) {
		ms := int(now.Sub(startAt).Milliseconds())
		if ms >= 0 {
			state.firstTokenMs = &ms
		}
		// Identified events exclusively time their own response. Only ID-less
		// semantic output may fall back to the active turn.
		if responseID == "" && state.activeTurn != nil && state.activeTurn.firstTokenMs == nil {
			tms := int(now.Sub(state.activeTurn.startAt).Milliseconds())
			if tms >= 0 {
				state.activeTurn.firstTokenMs = &tms
			}
		}
	}
	parsedUsage := parseUsageAndAccumulate(state, message, eventType, onUsageParseFailure)
	observed := observedUpstreamEvent{
		eventType:  eventType,
		responseID: responseID,
		usage:      parsedUsage,
	}
	if responseID != "" {
		turnTiming := openAIWSRelayGetOrInitTurnTiming(state, responseID, now)
		if turnTiming != nil && turnTiming.firstTokenMs == nil && isTokenEvent(eventType) {
			ms := int(now.Sub(turnTiming.startAt).Milliseconds())
			if ms >= 0 {
				turnTiming.firstTokenMs = &ms
			}
		}
	}
	if !effectiveTerminal {
		return observed
	}
	observed.terminal = true
	observed.payload = append([]byte(nil), message...)
	state.terminalEventType = eventType
	terminalGeneration := state.idlessTurnGeneration.Load()
	if terminalGeneration == 0 {
		terminalGeneration = 1
	}
	if responseID != "" {
		state.lastResponseID = responseID
		if turnTiming, ok := openAIWSRelayDeleteTurnTiming(state, responseID); ok {
			terminalGeneration = turnTiming.generation
			duration := now.Sub(turnTiming.startAt)
			if duration < 0 {
				duration = 0
			}
			observed.duration = duration
			observed.firstToken = openAIWSRelayCloneIntPtr(turnTiming.firstTokenMs)
		}
	}
	openAIWSRelayMarkTurnCompleted(state, terminalGeneration)
	return observed
}

func emitTurnComplete(
	onTurnComplete func(turn RelayTurnResult),
	state *relayState,
	observed observedUpstreamEvent,
) {
	if onTurnComplete == nil || !observed.terminal {
		return
	}
	onTurnComplete(relayTurnResult(state, observed))
}

func relayTurnResult(state *relayState, observed observedUpstreamEvent) RelayTurnResult {
	requestModel := ""
	if state != nil {
		requestModel = state.requestModel
	}
	return RelayTurnResult{
		RequestModel:         requestModel,
		Usage:                observed.usage,
		RequestID:            strings.TrimSpace(observed.responseID),
		TerminalEventType:    observed.eventType,
		TerminalPayload:      append([]byte(nil), observed.payload...),
		Duration:             observed.duration,
		FirstTokenMs:         openAIWSRelayCloneIntPtr(observed.firstToken),
		ClientEventDelivered: observed.clientEventDelivered,
	}
}

func openAIWSRelayGetOrInitTurnTiming(state *relayState, responseID string, now time.Time) *relayTurnTiming {
	if state == nil {
		return nil
	}
	if state.turnTimingByID == nil {
		state.turnTimingByID = make(map[string]*relayTurnTiming, 8)
	}
	timing, ok := state.turnTimingByID[responseID]
	if !ok || timing == nil || timing.startAt.IsZero() {
		generation := state.idlessTurnGeneration.Load()
		if generation == 0 {
			generation = 1
		}
		timing = &relayTurnTiming{startAt: now, generation: generation}
		state.turnTimingByID[responseID] = timing
		state.activeTurn = timing
		return timing
	}
	return timing
}

func openAIWSRelayMarkResponseCompleted(state *relayState, responseID string) bool {
	if state == nil || responseID == "" {
		return true
	}
	if state.completedResponseIDs == nil {
		state.completedResponseIDs = make(map[string]struct{}, 8)
	}
	if _, exists := state.completedResponseIDs[responseID]; exists {
		return false
	}
	state.completedResponseIDs[responseID] = struct{}{}
	return true
}

func openAIWSRelayMarkTurnCompleted(state *relayState, generation uint64) {
	if state == nil || generation == 0 {
		return
	}
	for {
		completed := state.completedTurnGeneration.Load()
		if completed >= generation || state.completedTurnGeneration.CompareAndSwap(completed, generation) {
			return
		}
	}
}

func openAIWSRelayMarkIDLessTerminal(state *relayState) bool {
	if state == nil {
		return true
	}
	generation := state.idlessTurnGeneration.Load()
	if generation == 0 {
		generation = 1
	}
	for {
		completed := state.idlessTerminalGeneration.Load()
		if completed >= generation {
			return false
		}
		if state.idlessTerminalGeneration.CompareAndSwap(completed, generation) {
			return true
		}
	}
}

func openAIWSRelayDeleteTurnTiming(state *relayState, responseID string) (relayTurnTiming, bool) {
	if state == nil || state.turnTimingByID == nil {
		return relayTurnTiming{}, false
	}
	timing, ok := state.turnTimingByID[responseID]
	if !ok || timing == nil {
		return relayTurnTiming{}, false
	}
	delete(state.turnTimingByID, responseID)
	if state.activeTurn == timing {
		state.activeTurn = nil
	}
	return *timing, true
}

func openAIWSRelayCloneIntPtr(v *int) *int {
	if v == nil {
		return nil
	}
	cloned := *v
	return &cloned
}

func parseUsageAndAccumulate(
	state *relayState,
	message []byte,
	eventType string,
	onParseFailure func(eventType string, usageRaw string),
) Usage {
	if state == nil || len(message) == 0 || !shouldParseUsage(eventType) {
		return Usage{}
	}
	usageResult := gjson.GetBytes(message, "response.usage")
	if !usageResult.Exists() {
		return Usage{}
	}
	usageRaw := strings.TrimSpace(usageResult.Raw)
	if usageRaw == "" || !strings.HasPrefix(usageRaw, "{") {
		recordUsageParseFailure()
		if onParseFailure != nil {
			onParseFailure(eventType, usageRaw)
		}
		return Usage{}
	}

	inputResult := gjson.GetBytes(message, "response.usage.input_tokens")
	if !inputResult.Exists() {
		inputResult = gjson.GetBytes(message, "response.usage.prompt_tokens")
	}
	outputResult := gjson.GetBytes(message, "response.usage.output_tokens")
	if !outputResult.Exists() {
		outputResult = gjson.GetBytes(message, "response.usage.completion_tokens")
	}
	cachedResult := gjson.GetBytes(message, "response.usage.input_tokens_details.cached_tokens")
	if !cachedResult.Exists() {
		cachedResult = gjson.GetBytes(message, "response.usage.prompt_tokens_details.cached_tokens")
	}
	imageTokens := usageResult.Get("output_tokens_details.image_tokens").Int()
	if imageTokens == 0 {
		imageTokens = usageResult.Get("completion_tokens_details.image_tokens").Int()
	}

	inputTokens, inputOK := parseUsageIntField(inputResult, true)
	outputTokens, outputOK := parseUsageIntField(outputResult, true)
	cachedTokens, cachedOK := parseUsageIntField(cachedResult, false)
	if !inputOK || !outputOK || !cachedOK {
		recordUsageParseFailure()
		if onParseFailure != nil {
			onParseFailure(eventType, usageRaw)
		}
		// 解析失败时不做部分字段累加，避免计费 usage 出现“半有效”状态。
		return Usage{}
	}
	parsedUsage := Usage{
		InputTokens:              inputTokens,
		OutputTokens:             outputTokens,
		CacheCreationInputTokens: openAICacheCreationTokensFromUsage(usageResult),
		CacheReadInputTokens:     cachedTokens,
		ImageOutputTokens:        int(imageTokens),
	}

	state.usage.InputTokens += parsedUsage.InputTokens
	state.usage.OutputTokens += parsedUsage.OutputTokens
	state.usage.CacheCreationInputTokens += parsedUsage.CacheCreationInputTokens
	state.usage.CacheReadInputTokens += parsedUsage.CacheReadInputTokens
	state.usage.ImageOutputTokens += parsedUsage.ImageOutputTokens
	return parsedUsage
}

func parseUsageIntField(value gjson.Result, required bool) (int, bool) {
	if !value.Exists() {
		return 0, !required
	}
	if value.Type != gjson.Number {
		return 0, false
	}
	return int(value.Int()), true
}

func openAICacheCreationTokensFromUsage(value gjson.Result) int {
	cacheCreationTokens := 0
	for _, field := range []string{
		"cache_creation_input_tokens",
		"cache_write_input_tokens",
		"cache_creation_tokens",
		"cache_write_tokens",
	} {
		if tokens := int(value.Get(field).Int()); tokens > 0 {
			cacheCreationTokens = tokens
			break
		}
	}
	for _, field := range []string{
		"input_tokens_details.cache_write_tokens",
		"prompt_tokens_details.cache_write_tokens",
		"input_tokens_details.cache_creation_tokens",
		"prompt_tokens_details.cache_creation_tokens",
	} {
		if result := value.Get(field); result.Exists() {
			return max(int(result.Int()), 0)
		}
	}
	return cacheCreationTokens
}

func enrichResult(result *RelayResult, state *relayState, duration time.Duration) {
	if result == nil {
		return
	}
	result.Duration = duration
	if state == nil {
		return
	}
	result.RequestModel = state.requestModel
	result.Usage = state.usage
	result.RequestID = state.lastResponseID
	result.TerminalEventType = state.terminalEventType
	result.FirstTokenMs = state.firstTokenMs
}

func isDisconnectError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) {
		return true
	}
	switch coderws.CloseStatus(err) {
	case coderws.StatusNormalClosure, coderws.StatusGoingAway, coderws.StatusNoStatusRcvd, coderws.StatusAbnormalClosure:
		return true
	}
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	if message == "" {
		return false
	}
	return strings.Contains(message, "failed to read frame header: eof") ||
		strings.Contains(message, "unexpected eof") ||
		strings.Contains(message, "use of closed network connection") ||
		strings.Contains(message, "connection reset by peer") ||
		strings.Contains(message, "broken pipe")
}

func isTerminalEvent(eventType string) bool {
	switch eventType {
	case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
		return true
	default:
		return false
	}
}

func shouldParseUsage(eventType string) bool {
	switch eventType {
	case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
		return true
	default:
		return false
	}
}

func sanitizeResponseFailedMessageForClient(payload []byte) ([]byte, bool) {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload, false
	}
	if strings.TrimSpace(gjson.GetBytes(payload, "type").String()) != "response.failed" {
		return payload, false
	}

	updated := payload
	for _, path := range []string{
		"instructions",
		"input",
		"output",
		"usage",
		"metadata",
		"reasoning",
		"tools",
		"tool_choice",
		"parallel_tool_calls",
		"prompt_cache_key",
		"previous_response_id",
		"text",
		"truncation",
		"max_output_tokens",
		"incomplete_details",
	} {
		for _, prefix := range []string{"", "response."} {
			next, err := sjson.DeleteBytes(updated, prefix+path)
			if err != nil {
				return payload, false
			}
			updated = next
		}
	}
	return updated, !bytes.Equal(updated, payload)
}

func isTokenEvent(eventType string) bool {
	eventType = strings.TrimSpace(eventType)
	if eventType == "" || isTerminalEvent(eventType) {
		return false
	}
	switch eventType {
	case "response.created", "response.in_progress", "response.output_item.added", "response.output_item.done":
		return false
	}
	if strings.Contains(eventType, ".delta") {
		return true
	}
	if strings.HasPrefix(eventType, "response.output_text") {
		return true
	}
	if strings.HasPrefix(eventType, "response.output") {
		return true
	}
	return false
}

func minDuration(a, b time.Duration) time.Duration {
	if a <= 0 {
		return b
	}
	if b <= 0 {
		return a
	}
	if a < b {
		return a
	}
	return b
}

func waitRelayExitContext(ctx context.Context, exitCh <-chan relayExitSignal) (relayExitSignal, bool) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return relayExitSignal{stage: "context_canceled", err: err}, false
		}
	}
	select {
	case signal := <-exitCh:
		return signal, true
	case <-ctx.Done():
		return relayExitSignal{stage: "context_canceled", err: ctx.Err()}, false
	}
}

func waitRelayExit(exitCh <-chan relayExitSignal, timeout time.Duration) (relayExitSignal, bool) {
	if timeout <= 0 {
		timeout = 200 * time.Millisecond
	}
	select {
	case sig := <-exitCh:
		return sig, true
	case <-time.After(timeout):
		return relayExitSignal{}, false
	}
}

func waitTerminalDrainExit(ctx context.Context, exitCh <-chan relayExitSignal, timeout time.Duration) (relayExitSignal, bool) {
	if timeout <= 0 {
		timeout = 200 * time.Millisecond
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case sig := <-exitCh:
			switch sig.stage {
			case "drain_terminal", "read_upstream":
				return sig, true
			default:
				continue
			}
		case <-ctx.Done():
			return relayExitSignal{stage: "context_canceled", err: ctx.Err()}, false
		case <-timer.C:
			return relayExitSignal{}, false
		}
	}
}
