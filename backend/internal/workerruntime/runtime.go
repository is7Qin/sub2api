package workerruntime

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

const rollbackTimeout = 30 * time.Second

// StopOutcome describes how a component's stop request finished.
type StopOutcome string

const (
	StopCompleted StopOutcome = "completed"
	StopTimedOut  StopOutcome = "timed_out"
	StopError     StopOutcome = "error"
)

// StopResult records the shutdown outcome for one component.
type StopResult struct {
	Name         string
	Outcome      StopOutcome
	Err          error
	StillRunning bool
}

// StopInitiationNotifier is optionally implemented by components that can
// publish when their Stop method has entered. Component deliberately remains
// minimal; Runtime falls back to launch-and-proceed for other components.
type StopInitiationNotifier interface {
	StopInitiated() <-chan struct{}
}

type stopCall struct {
	registered registeredComponent
	once       sync.Once
	done       chan struct{}
	err        error
}

func (c *stopCall) start(ctx context.Context) (started <-chan struct{}) {
	c.once.Do(func() {
		if notifier, ok := c.registered.component.(StopInitiationNotifier); ok {
			started = notifier.StopInitiated()
		}
		go func() {
			c.err = c.registered.component.Stop(ctx)
			close(c.done)
		}()
	})
	return started
}

// Runtime coordinates the lifecycle of registered worker components.
type Runtime struct {
	registry *Registry

	mu             sync.Mutex
	starting       bool
	started        bool
	stopping       bool
	stopped        bool
	root           context.Context
	cancel         context.CancelFunc
	startDone      chan struct{}
	stopInitiated  bool
	stopCallsReady chan struct{}
	stopCalls      []*stopCall
}

// NewRuntime creates a runtime backed by registry.
func NewRuntime(registry *Registry) *Runtime {
	if registry == nil {
		registry = NewRegistry()
	}
	return &Runtime{registry: registry}
}

// Register adds a component before the runtime begins.
func (r *Runtime) Register(component Component) error {
	return r.registry.Register(component)
}

// Snapshot returns name-sorted, detached snapshots of all managed components.
func (r *Runtime) Snapshot() []Snapshot {
	if r == nil {
		return nil
	}
	return r.registry.Snapshot()
}

// StartAll freezes registration and starts pools before periodic components.
func (r *Runtime) StartAll(ctx context.Context) error {
	r.mu.Lock()
	if r.started || r.starting {
		r.mu.Unlock()
		return nil
	}
	if r.stopping || r.stopped {
		r.mu.Unlock()
		return errors.New("runtime has stopped")
	}
	r.registry.Freeze()
	components := startOrder(r.registry.componentCopies())
	root, cancel := context.WithCancel(ctx)
	r.starting = true
	r.root = root
	r.cancel = cancel
	r.startDone = make(chan struct{})
	r.mu.Unlock()

	started := make([]registeredComponent, 0, len(components))
	for _, registered := range components {
		if err := registered.component.Start(root); err != nil {
			cancel()
			rollbackErrs := []error{err}
			rollbackCtx, rollbackCancel := context.WithTimeout(ctx, rollbackTimeout)
			for i := len(started) - 1; i >= 0; i-- {
				if rollbackErr := started[i].component.Stop(rollbackCtx); rollbackErr != nil {
					rollbackErrs = append(rollbackErrs, rollbackErr)
				}
			}
			rollbackCancel()
			r.finishStart(false)
			return errors.Join(rollbackErrs...)
		}
		started = append(started, registered)
	}

	r.finishStart(true)
	return nil
}

func (r *Runtime) finishStart(succeeded bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starting = false
	r.started = succeeded
	close(r.startDone)
}

// StopAll cancels the root context and stops periodic components before pools.
func (r *Runtime) StopAll(ctx context.Context) ([]StopResult, error) {
	if err := r.waitForStart(ctx); err != nil {
		return nil, err
	}

	calls, err := r.ensureStopCalls(ctx)
	if err != nil || calls == nil {
		return nil, err
	}

	results, allCompleted := collectStopResults(ctx, calls)
	if allCompleted {
		r.mu.Lock()
		r.stopped = true
		r.stopping = false
		r.mu.Unlock()
	}
	return results, joinStopErrors(results)
}

func (r *Runtime) waitForStart(ctx context.Context) error {
	r.mu.Lock()
	if !r.starting {
		r.mu.Unlock()
		return nil
	}
	r.stopping = true
	cancel := r.cancel
	done := r.startDone
	r.mu.Unlock()

	// A shutdown requested during startup must unblock components immediately.
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Runtime) ensureStopCalls(ctx context.Context) ([]*stopCall, error) {
	r.mu.Lock()
	if r.stopped || !r.started {
		r.mu.Unlock()
		return nil, nil
	}
	if r.stopInitiated {
		ready := r.stopCallsReady
		r.mu.Unlock()
		select {
		case <-ready:
			return r.stopCallsSnapshot(), nil
		default:
		}
		select {
		case <-ready:
			return r.stopCallsSnapshot(), nil
		case <-ctx.Done():
			// Stop calls may have become available concurrently with cancellation.
			// Prefer inspecting their completed state over reporting a stale timeout.
			select {
			case <-ready:
				return r.stopCallsSnapshot(), nil
			default:
				return nil, ctx.Err()
			}
		}
	}
	r.stopping = true
	r.stopInitiated = true
	r.stopCallsReady = make(chan struct{})
	ready := r.stopCallsReady
	cancel := r.cancel
	r.mu.Unlock()

	// Cancel workers before invoking their shutdown routines so their run loops exit.
	cancel()
	components := stopOrder(r.registry.componentCopies())
	calls := make([]*stopCall, 0, len(components))
	for _, registered := range components {
		calls = append(calls, &stopCall{registered: registered, done: make(chan struct{})})
	}

	r.mu.Lock()
	r.stopCalls = calls
	close(ready)
	r.mu.Unlock()

	// Start every stop call before collecting results. Stop calls are issued in the
	// established periodic-before-pool order, but each runs independently so a
	// blocking periodic stop cannot delay a pool from rejecting new work.
	for _, call := range calls {
		started := call.start(context.Background())
		if started != nil {
			// Preserve actual Stop-entry ordering while it is observable, but do
			// not let a broken external notifier bypass the caller's deadline.
			select {
			case <-started:
				continue
			default:
			}
			select {
			case <-started:
			case <-ctx.Done():
				// An entry observed with cancellation still establishes ordering.
				select {
				case <-started:
				default:
				}
			}
		}
	}
	return calls, nil
}

func (r *Runtime) stopCallsSnapshot() []*stopCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopCalls
}

func collectStopResults(ctx context.Context, calls []*stopCall) ([]StopResult, bool) {
	results := make([]StopResult, 0, len(calls))
	allCompleted := true
	for _, call := range calls {
		if result, completed := completedStopResult(call); completed {
			results = append(results, result)
			continue
		}

		select {
		case <-call.done:
			results = append(results, stopResultForCompletedCall(call))
		case <-ctx.Done():
			// Completion wins if it raced with the caller deadline. This second
			// non-blocking observation also handles already-expired retry contexts.
			if result, completed := completedStopResult(call); completed {
				results = append(results, result)
				continue
			}
			allCompleted = false
			results = append(results, StopResult{Name: call.registered.descriptor.Name, Outcome: StopTimedOut, Err: ctx.Err(), StillRunning: true})
		}
	}
	return results, allCompleted
}

func completedStopResult(call *stopCall) (StopResult, bool) {
	select {
	case <-call.done:
		return stopResultForCompletedCall(call), true
	default:
		return StopResult{}, false
	}
}

func stopResultForCompletedCall(call *stopCall) StopResult {
	if call.err != nil {
		return StopResult{Name: call.registered.descriptor.Name, Outcome: StopError, Err: call.err}
	}
	return StopResult{Name: call.registered.descriptor.Name, Outcome: StopCompleted}
}

func joinStopErrors(results []StopResult) error {
	errs := make([]error, 0)
	deadlineReported := false
	for _, result := range results {
		switch result.Outcome {
		case StopError:
			errs = append(errs, result.Err)
		case StopTimedOut:
			if !deadlineReported {
				errs = append(errs, result.Err)
				deadlineReported = true
			}
		}
	}
	return errors.Join(errs...)
}

func startOrder(components []registeredComponent) []registeredComponent {
	return orderedComponents(components, KindPool, KindPeriodic)
}

func stopOrder(components []registeredComponent) []registeredComponent {
	return orderedComponents(components, KindPeriodic, KindPool)
}

func orderedComponents(components []registeredComponent, first, second Kind) []registeredComponent {
	ordered := make([]registeredComponent, 0, len(components))
	for _, kind := range []Kind{first, second} {
		for _, registered := range components {
			if registered.descriptor.Kind == kind {
				ordered = append(ordered, registered)
			}
		}
	}
	sort.Slice(ordered, func(i, j int) bool {
		left, right := ordered[i], ordered[j]
		if left.descriptor.Kind != right.descriptor.Kind {
			return left.descriptor.Kind == first
		}
		return left.descriptor.Name < right.descriptor.Name
	})
	return ordered
}
