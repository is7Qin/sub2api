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

// Runtime coordinates the lifecycle of registered worker components.
type Runtime struct {
	registry *Registry

	mu       sync.Mutex
	started  bool
	stopping bool
	stopped  bool
	root     context.Context
	cancel   context.CancelFunc
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

// StartAll freezes registration and starts pools before periodic components.
func (r *Runtime) StartAll(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return nil
	}
	if r.stopping || r.stopped {
		return errors.New("runtime has stopped")
	}
	r.registry.Freeze()
	components := startOrder(r.registry.componentCopies())
	root, cancel := context.WithCancel(ctx)

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
			return errors.Join(rollbackErrs...)
		}
		started = append(started, registered)
	}

	r.started = true
	r.root = root
	r.cancel = cancel
	return nil
}

// StopAll cancels the root context and stops periodic components before pools.
func (r *Runtime) StopAll(ctx context.Context) ([]StopResult, error) {
	r.mu.Lock()
	if !r.started || r.stopping || r.stopped {
		r.mu.Unlock()
		return nil, nil
	}
	r.stopping = true
	r.stopped = true
	cancel := r.cancel
	r.mu.Unlock()

	// Cancel workers before invoking their shutdown routines so their run loops exit.
	cancel()
	components := stopOrder(r.registry.componentCopies())
	results := make([]StopResult, 0, len(components))
	errs := make([]error, 0)
	deadlineReported := false
	for _, registered := range components {
		result := stopComponent(ctx, registered)
		results = append(results, result)
		if result.Outcome == StopError {
			errs = append(errs, result.Err)
		}
		if result.Outcome == StopTimedOut && !deadlineReported {
			errs = append(errs, ctx.Err())
			deadlineReported = true
		}
	}
	return results, errors.Join(errs...)
}

func stopComponent(ctx context.Context, registered registeredComponent) StopResult {
	done := make(chan error, 1)
	go func() { done <- registered.component.Stop(ctx) }()

	select {
	case err := <-done:
		if ctx.Err() != nil {
			return StopResult{Name: registered.descriptor.Name, Outcome: StopTimedOut, Err: ctx.Err(), StillRunning: true}
		}
		if err != nil {
			return StopResult{Name: registered.descriptor.Name, Outcome: StopError, Err: err}
		}
		return StopResult{Name: registered.descriptor.Name, Outcome: StopCompleted}
	case <-ctx.Done():
		return StopResult{Name: registered.descriptor.Name, Outcome: StopTimedOut, Err: ctx.Err(), StillRunning: true}
	}
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
