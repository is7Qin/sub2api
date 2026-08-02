package workerruntime

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

const maxPeriodicErrorMessageLength = 512

// PeriodicJobSpec configures a fixed-delay periodic component.
type PeriodicJobSpec struct {
	Descriptor     Descriptor
	Interval       time.Duration
	Timeout        time.Duration
	RunImmediately bool
	Run            func(context.Context) error
}

// PeriodicJob runs one callback at a time on a fixed delay.
type PeriodicJob struct {
	descriptor     Descriptor
	interval       time.Duration
	timeout        time.Duration
	runImmediately bool
	run            func(context.Context) error

	mu           sync.RWMutex
	lifecycle    LifecycleSnapshot
	status       PeriodicStatus
	started      bool
	stopping     bool
	stopDone     chan struct{}
	rootCancel   context.CancelFunc
	activeCancel context.CancelFunc
	stopStarted  chan struct{}
}

// NewPeriodicJob validates spec and returns a periodic component.
func NewPeriodicJob(spec PeriodicJobSpec) (*PeriodicJob, error) {
	descriptor := cloneDescriptor(spec.Descriptor)
	if err := validateDescriptor(descriptor); err != nil {
		return nil, err
	}
	if descriptor.Kind != KindPeriodic {
		return nil, fmt.Errorf("periodic job kind must be %s", KindPeriodic)
	}
	if spec.Interval <= 0 {
		return nil, fmt.Errorf("periodic interval must be positive")
	}
	if spec.Timeout <= 0 {
		return nil, fmt.Errorf("periodic timeout must be positive")
	}
	if spec.Run == nil {
		return nil, fmt.Errorf("periodic callback is required")
	}
	return &PeriodicJob{
		descriptor:     descriptor,
		interval:       spec.Interval,
		timeout:        spec.Timeout,
		runImmediately: spec.RunImmediately,
		run:            spec.Run,
		lifecycle:      LifecycleSnapshot{State: LifecycleStopped, UpdatedAt: time.Now()},
	}, nil
}

func (j *PeriodicJob) Descriptor() Descriptor {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return cloneDescriptor(j.descriptor)
}

func (j *PeriodicJob) Start(ctx context.Context) error {
	j.mu.Lock()
	if j.started {
		j.mu.Unlock()
		return nil
	}
	if j.stopping || j.lifecycle.State == LifecycleStopping {
		j.mu.Unlock()
		return fmt.Errorf("periodic job is stopping")
	}
	root, cancel := context.WithCancel(ctx)
	j.started = true
	j.stopDone = make(chan struct{})
	j.rootCancel = cancel
	j.lifecycle = LifecycleSnapshot{State: LifecycleRunning, UpdatedAt: time.Now()}
	j.mu.Unlock()

	go j.loop(root)
	return nil
}

// StopInitiated returns a channel closed when Stop has entered.
func (j *PeriodicJob) StopInitiated() <-chan struct{} {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.stopStarted == nil {
		j.stopStarted = make(chan struct{})
	}
	return j.stopStarted
}

func (j *PeriodicJob) Stop(ctx context.Context) error {
	j.mu.Lock()
	if j.stopStarted == nil {
		j.stopStarted = make(chan struct{})
	}
	close(j.stopStarted)
	j.stopStarted = nil
	if !j.started {
		j.lifecycle = LifecycleSnapshot{State: LifecycleStopped, UpdatedAt: time.Now()}
		j.mu.Unlock()
		return nil
	}
	if !j.stopping {
		j.stopping = true
		j.lifecycle = LifecycleSnapshot{State: LifecycleStopping, UpdatedAt: time.Now()}
		if j.rootCancel != nil {
			j.rootCancel()
		}
		if j.activeCancel != nil {
			j.activeCancel()
		}
	}
	done := j.stopDone
	j.mu.Unlock()

	if periodicStopCompleted(ctx, done) {
		return nil
	}
	j.mu.Lock()
	select {
	case <-done:
		j.mu.Unlock()
		return nil
	default:
		// The scheduler still owns the completion signal, so this stop timed out truthfully.
		j.lifecycle = LifecycleSnapshot{State: StateStopping, UpdatedAt: time.Now(), LastError: ctx.Err().Error()}
		j.status.StillRunning = true
		j.mu.Unlock()
		return ctx.Err()
	}
}

// periodicStopCompleted gives a completed scheduler precedence over caller cancellation.
func periodicStopCompleted(ctx context.Context, done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
	}
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

func (j *PeriodicJob) Snapshot() Snapshot {
	j.mu.RLock()
	defer j.mu.RUnlock()
	return Snapshot{
		Descriptor: cloneDescriptor(j.descriptor),
		Lifecycle:  j.lifecycle,
		Status:     j.status,
	}
}

func (j *PeriodicJob) loop(root context.Context) {
	defer func() {
		j.mu.Lock()
		j.started = false
		j.stopping = false
		j.rootCancel = nil
		j.activeCancel = nil
		j.lifecycle = LifecycleSnapshot{State: LifecycleStopped, UpdatedAt: time.Now()}
		j.status.StillRunning = false
		done := j.stopDone
		j.mu.Unlock()
		close(done)
	}()

	if !j.runImmediately {
		if !j.wait(root, j.interval) {
			return
		}
	}

	for {
		if root.Err() != nil {
			return
		}
		j.invoke(root)
		if root.Err() != nil {
			return
		}
		if !j.wait(root, j.interval) {
			return
		}
	}
}

func (j *PeriodicJob) wait(root context.Context, delay time.Duration) bool {
	next := time.Now().Add(delay)
	j.mu.Lock()
	j.status.NextRunAt = next
	j.mu.Unlock()
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-root.Done():
		return false
	}
}

func (j *PeriodicJob) invoke(root context.Context) {
	startedAt := time.Now()
	ctx, cancel := context.WithTimeout(root, j.timeout)
	j.mu.Lock()
	j.activeCancel = cancel
	j.status.StillRunning = true
	j.mu.Unlock()

	done := make(chan invocationResult, 1)
	go func() { done <- j.call(ctx) }()

	select {
	case result := <-done:
		result = classifyInvocationResult(root, ctx, result)
		cancel()
		j.recordInvocation(startedAt, result, false)
	case <-ctx.Done():
		if root.Err() != nil {
			result := <-done
			cancel()
			j.recordInvocation(startedAt, result, false)
			return
		}
		result := invocationResult{outcome: OutcomeTimeout, err: ctx.Err()}
		j.recordInvocation(startedAt, result, true)
		<-done
		cancel()
		j.recordCallbackFinished()
	}
}

type invocationResult struct {
	outcome Outcome
	err     error
}

func (j *PeriodicJob) call(ctx context.Context) (result invocationResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = invocationResult{outcome: OutcomePanic, err: fmt.Errorf("panic: %v", recovered)}
		}
	}()
	if err := j.run(ctx); err != nil {
		return invocationResult{outcome: OutcomeError, err: err}
	}
	return invocationResult{outcome: OutcomeSuccess}
}

// A callback can return its deadline error before the scheduler observes ctx.Done.
// Preserve stop/root cancellation as an error, but record per-run expiry as timeout.
func classifyInvocationResult(root, invocation context.Context, result invocationResult) invocationResult {
	if result.outcome == OutcomeError && root.Err() == nil && invocation.Err() == context.DeadlineExceeded {
		return invocationResult{outcome: OutcomeTimeout, err: context.DeadlineExceeded}
	}
	return result
}

func (j *PeriodicJob) recordInvocation(startedAt time.Time, result invocationResult, stillRunning bool) {
	now := time.Now()
	if !now.After(startedAt) {
		now = startedAt.Add(time.Nanosecond)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.status.LastRunAt = startedAt
	j.status.LastDuration = now.Sub(startedAt)
	j.status.LastOutcome = result.outcome
	j.status.LastError = boundedPeriodicError(result.err)
	j.status.RunCount++
	j.status.StillRunning = stillRunning
	if !stillRunning {
		j.activeCancel = nil
	}
	switch result.outcome {
	case OutcomeSuccess:
		j.status.SuccessCount++
	case OutcomeError:
		j.status.ErrorCount++
	case OutcomePanic:
		j.status.PanicCount++
	case OutcomeTimeout:
		j.status.TimeoutCount++
	}
}

func (j *PeriodicJob) recordCallbackFinished() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.status.StillRunning = false
	j.activeCancel = nil
}

func boundedPeriodicError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Error())
	if len(message) <= maxPeriodicErrorMessageLength {
		return message
	}
	return message[:maxPeriodicErrorMessageLength]
}
