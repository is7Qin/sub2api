package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/workerruntime"
)

const (
	schedulerSupportPublisherDebounce    = 100 * time.Millisecond
	schedulerSupportPublisherCoalesceCap = time.Second
)

type supportDecisionBatchPublisher interface {
	Publish(context.Context) (uint64, error)
}

type supportDecisionActiveGenerationReader interface {
	ActiveGeneration(context.Context) (uint64, error)
}

type schedulerDirtyWorkRecovery interface {
	RecordDirtyWorkListFailure(context.Context)
	ClearDirtyWorkListFailure()
	CheckDirtyWorkLag(context.Context)
}

type schedulerSupportPublisherTimer interface {
	Chan() <-chan time.Time
	Stop()
}

type schedulerSupportPublisherClock interface {
	Now() time.Time
	NewTimer(time.Duration) schedulerSupportPublisherTimer
}

type schedulerSupportPublisherTimeClock struct{}
type schedulerSupportPublisherTimeTimer struct{ timer *time.Timer }

func (schedulerSupportPublisherTimeClock) Now() time.Time { return time.Now() }
func (schedulerSupportPublisherTimeClock) NewTimer(d time.Duration) schedulerSupportPublisherTimer {
	return schedulerSupportPublisherTimeTimer{timer: time.NewTimer(d)}
}
func (t schedulerSupportPublisherTimeTimer) Chan() <-chan time.Time { return t.timer.C }
func (t schedulerSupportPublisherTimeTimer) Stop()                  { t.timer.Stop() }

// immediateSchedulerSupportPublisherClock is used by focused tests that do not
// exercise timing; production always uses schedulerSupportPublisherTimeClock.
type immediateSchedulerSupportPublisherClock struct{}
type immediateSchedulerSupportPublisherTimer struct{ ch chan time.Time }

func (immediateSchedulerSupportPublisherClock) Now() time.Time { return time.Now() }
func (immediateSchedulerSupportPublisherClock) NewTimer(time.Duration) schedulerSupportPublisherTimer {
	ch := make(chan time.Time, 1)
	ch <- time.Now()
	return immediateSchedulerSupportPublisherTimer{ch: ch}
}
func (t immediateSchedulerSupportPublisherTimer) Chan() <-chan time.Time { return t.ch }
func (immediateSchedulerSupportPublisherTimer) Stop()                    {}

// SchedulerSupportPublisherWorker owns scheduler dirty work and the activation
// barrier for one PostgreSQL ownership term.
type SchedulerSupportPublisherWorker struct {
	dirty        SchedulerDirtyWorkRepository
	ownership    SchedulerOwnershipRepository
	processor    SchedulerSnapshotDirtyProcessor
	publisher    supportDecisionBatchPublisher
	active       supportDecisionActiveGenerationReader
	recovery     schedulerDirtyWorkRecovery
	pollInterval time.Duration
	clock        schedulerSupportPublisherClock
	descriptor   workerruntime.Descriptor

	mu        sync.Mutex
	lifecycle workerruntime.LifecycleSnapshot
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
}

func NewSchedulerSupportPublisherWorker(dirty SchedulerDirtyWorkRepository, ownership SchedulerOwnershipRepository, processor SchedulerSnapshotDirtyProcessor, publisher *SupportDecisionPublisher, store SupportDecisionPublicationStore, cfg *config.Config) *SchedulerSupportPublisherWorker {
	poll := time.Second
	if cfg != nil && cfg.Gateway.Scheduling.OutboxPollIntervalSeconds > 0 {
		poll = time.Duration(cfg.Gateway.Scheduling.OutboxPollIntervalSeconds) * time.Second
	}
	worker := newSchedulerSupportPublisherWorker(dirty, ownership, processor, publisher, poll, schedulerSupportPublisherTimeClock{})
	worker.active = store
	if recovered, ok := processor.(schedulerDirtyWorkRecovery); ok {
		worker.recovery = recovered
	}
	return worker
}

func newSchedulerSupportPublisherWorker(dirty SchedulerDirtyWorkRepository, ownership SchedulerOwnershipRepository, processor SchedulerSnapshotDirtyProcessor, publisher supportDecisionBatchPublisher, poll time.Duration, clock schedulerSupportPublisherClock) *SchedulerSupportPublisherWorker {
	if poll <= 0 {
		poll = time.Second
	}
	if clock == nil {
		clock = schedulerSupportPublisherTimeClock{}
	}
	return &SchedulerSupportPublisherWorker{dirty: dirty, ownership: ownership, processor: processor, publisher: publisher, pollInterval: poll, clock: clock,
		descriptor: workerruntime.Descriptor{Name: "scheduler-support-publisher", Kind: workerruntime.KindPeriodic, Group: "scheduler", CoordinationMode: workerruntime.CoordinationSingletonRun, Description: "Publishes scheduler snapshots and support decisions behind one activation barrier", Tags: []string{"scheduler", "support-decision", "dirty-work"}},
		lifecycle:  workerruntime.LifecycleSnapshot{State: workerruntime.LifecycleStopped, UpdatedAt: time.Now()}}
}

func (w *SchedulerSupportPublisherWorker) Descriptor() workerruntime.Descriptor {
	if w == nil {
		return workerruntime.Descriptor{}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.descriptor
}
func (w *SchedulerSupportPublisherWorker) Snapshot() workerruntime.Snapshot {
	if w == nil {
		return workerruntime.Snapshot{}
	}
	w.mu.Lock()
	descriptor, lifecycle := w.descriptor, w.lifecycle
	w.mu.Unlock()
	status := workerruntime.PeriodicStatus{StillRunning: lifecycle.State == workerruntime.LifecycleRunning || lifecycle.State == workerruntime.LifecycleStopping}
	if publisher, ok := w.publisher.(interface {
		Snapshot() SupportDecisionPublisherSnapshot
	}); ok {
		published := publisher.Snapshot()
		status.RunCount, status.SuccessCount = published.Attempts, published.SuccessfulActivations
		if published.Attempts >= published.SuccessfulActivations {
			status.ErrorCount = published.Attempts - published.SuccessfulActivations
		}
		status.LastRunAt, status.LastDuration, status.LastOutcome = published.LastCompletedAt, published.LastDuration, published.LastOutcome
		status.StillRunning = status.StillRunning || published.Active
	}
	return workerruntime.Snapshot{Descriptor: descriptor, Lifecycle: lifecycle, Status: status}
}

func (w *SchedulerSupportPublisherWorker) Start(ctx context.Context) error {
	if w == nil || w.dirty == nil || w.ownership == nil || w.processor == nil || w.publisher == nil {
		return errors.New("scheduler support publisher dependencies are incomplete")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	if w.done != nil {
		if w.ctx != nil && w.ctx.Err() == nil {
			w.mu.Unlock()
			return nil
		}
		w.mu.Unlock()
		return errors.New("scheduler support publisher lifecycle transition in progress")
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	w.ctx, w.cancel, w.done = runCtx, cancel, done
	w.lifecycle = workerruntime.LifecycleSnapshot{State: workerruntime.LifecycleRunning, UpdatedAt: time.Now()}
	w.mu.Unlock()

	if w.active != nil {
		if err := w.ensureBootstrap(runCtx); err != nil {
			cancel()
			w.finishRun(done)
			return err
		}
	}
	go w.run(runCtx, done)
	return nil
}

func (w *SchedulerSupportPublisherWorker) Stop(ctx context.Context) error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	cancel, done := w.cancel, w.done
	if done == nil {
		w.mu.Unlock()
		return nil
	}
	w.lifecycle = workerruntime.LifecycleSnapshot{State: workerruntime.LifecycleStopping, UpdatedAt: time.Now()}
	w.mu.Unlock()
	cancel()
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *SchedulerSupportPublisherWorker) finishRun(done chan struct{}) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done != done {
		return
	}
	// Publish completion before clearing done so a new run cannot overlap the
	// tail of the previous run's lifecycle transition.
	close(done)
	w.ctx = nil
	w.cancel = nil
	w.done = nil
	w.lifecycle = workerruntime.LifecycleSnapshot{State: workerruntime.LifecycleStopped, UpdatedAt: time.Now()}
}

func (w *SchedulerSupportPublisherWorker) ensureBootstrap(ctx context.Context) error {
	for {
		generation, err := w.active.ActiveGeneration(ctx)
		if err == nil && generation > 0 {
			return nil
		}
		owner, acquired, acquireErr := w.ownership.TryAcquire(ctx)
		if acquireErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !w.wait(ctx, w.pollInterval) {
				return ctx.Err()
			}
			continue
		}
		if acquired && owner != nil {
			termCtx, cancel := context.WithCancel(owner.Context())
			stop := context.AfterFunc(ctx, cancel)
			_, publishErr := w.publisher.Publish(termCtx)
			stop()
			cancel()
			closeErr := owner.Close()
			if publishErr != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if !w.wait(ctx, w.pollInterval) {
					return ctx.Err()
				}
				continue
			}
			if closeErr != nil {
				return errors.New("release scheduler ownership after support bootstrap")
			}
			return nil
		}
		if !w.wait(ctx, w.pollInterval) {
			return ctx.Err()
		}
	}
}

func (w *SchedulerSupportPublisherWorker) run(ctx context.Context, done chan struct{}) {
	defer w.finishRun(done)
	for ctx.Err() == nil {
		owner, acquired, err := w.ownership.TryAcquire(ctx)
		if err != nil && ctx.Err() == nil {
			logger.LegacyPrintf("service.scheduler_support_publisher", "[Scheduler] ownership acquisition failed class=%s", schedulerSupportErrorClass(err))
		}
		if acquired && owner != nil {
			w.runOwnershipTerm(ctx, owner)
			// Ownership Context is canceled by lease loss as well as Close. Closing the
			// session-backed owner is still mandatory and occurs exactly once per term.
			if err := owner.Close(); err != nil && ctx.Err() == nil {
				logger.LegacyPrintf("service.scheduler_support_publisher", "[Scheduler] ownership release failed")
			}
		}
		if !w.wait(ctx, w.pollInterval) {
			return
		}
	}
}

func (w *SchedulerSupportPublisherWorker) runOwnershipTerm(parent context.Context, owner SchedulerOwnership) {
	ctx, cancel := context.WithCancel(owner.Context())
	stop := context.AfterFunc(parent, cancel)
	defer func() { stop(); cancel() }()
	for ctx.Err() == nil {
		if err := w.processAvailable(ctx, owner); err != nil && ctx.Err() == nil {
			logger.LegacyPrintf("service.scheduler_support_publisher", "[Scheduler] support batch failed class=%s", schedulerSupportErrorClass(err))
		}
		if !w.wait(ctx, w.pollInterval) {
			return
		}
	}
}

func schedulerSupportErrorClass(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, ErrSupportDecisionNotPublished):
		return "not_published"
	default:
		return "operation_failed"
	}
}

func (w *SchedulerSupportPublisherWorker) wait(ctx context.Context, d time.Duration) bool {
	t := w.clock.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.Chan():
		return true
	case <-ctx.Done():
		return false
	}
}

func (w *SchedulerSupportPublisherWorker) processAvailable(ctx context.Context, owner SchedulerOwnership) error {
	_, err := w.processAvailableBatch(ctx, owner)
	return err
}

func (w *SchedulerSupportPublisherWorker) processAvailableBatch(ctx context.Context, owner SchedulerOwnership) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	for range dirtyWorkBatchSize {
		n, err := w.dirty.Promote(ctx, owner, dirtyWorkBatchSize)
		if err != nil {
			return false, fmt.Errorf("promote scheduler dirty sources: %w", err)
		}
		if n == 0 {
			break
		}
	}
	work, err := w.listDirtyWork(ctx)
	if err != nil {
		return false, err
	}
	if len(work) == 0 {
		if w.recovery != nil {
			w.recovery.CheckDirtyWorkLag(ctx)
		}
		return false, nil
	}
	coalesced, err := w.coalesce(ctx, owner, work)
	if err != nil {
		return true, err
	}
	if err := ctx.Err(); err != nil {
		return true, err
	}
	results := w.processor.ApplyDirtyWorkBatch(ctx, coalesced)
	if err := ctx.Err(); err != nil {
		return true, err
	}
	if len(results) != len(coalesced) {
		return true, w.failBatch(ctx, owner, coalesced, errors.New("snapshot processor returned invalid result count"))
	}
	for i, item := range coalesced {
		result := results[i]
		if result.Work.Kind != item.Kind || result.Work.EntityID != item.EntityID || result.Work.Generation != item.Generation {
			return true, w.failBatch(ctx, owner, coalesced, errors.New("snapshot processor changed dirty work identity"))
		}
	}
	generation, publishErr := w.publisher.Publish(ctx)
	if publishErr != nil || generation == 0 {
		if publishErr == nil {
			publishErr = ErrSupportDecisionNotPublished
		}
		if failureErr := w.failResults(ctx, owner, results, publishErr); failureErr != nil && !errors.Is(failureErr, publishErr) {
			return true, errors.Join(publishErr, failureErr)
		}
		return true, publishErr
	}
	for i, item := range coalesced {
		if err := ctx.Err(); err != nil {
			return true, err
		}
		result := results[i]
		if result.Err != nil {
			if _, err := w.dirty.RecordFailure(ctx, owner, item, result.Err); err != nil {
				return true, err
			}
			continue
		}
		if _, err := w.dirty.Acknowledge(ctx, owner, item); err != nil {
			return true, err
		}
		if err := ctx.Err(); err != nil {
			return true, err
		}
	}
	if w.recovery != nil {
		w.recovery.CheckDirtyWorkLag(ctx)
	}
	return true, nil
}

// listDirtyWork keeps the failure latch paired with every canonical list attempt,
// including coalescing samples that discover newly promoted source work.
func (w *SchedulerSupportPublisherWorker) listDirtyWork(ctx context.Context) ([]SchedulerDirtyWork, error) {
	work, err := w.dirty.List(ctx, dirtyWorkBatchSize)
	if err != nil {
		if w.recovery != nil && ctx.Err() == nil {
			w.recovery.RecordDirtyWorkListFailure(ctx)
		}
		return nil, fmt.Errorf("list scheduler dirty work: %w", err)
	}
	if w.recovery != nil {
		w.recovery.ClearDirtyWorkListFailure()
	}
	return work, nil
}

func (w *SchedulerSupportPublisherWorker) failBatch(ctx context.Context, owner SchedulerOwnership, work []SchedulerDirtyWork, failure error) error {
	for _, item := range work {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := w.dirty.RecordFailure(ctx, owner, item, failure); err != nil {
			return err
		}
	}
	return failure
}

func (w *SchedulerSupportPublisherWorker) failResults(ctx context.Context, owner SchedulerOwnership, results []SchedulerDirtyWorkResult, publicationErr error) error {
	for _, result := range results {
		if err := ctx.Err(); err != nil {
			return err
		}
		failure := publicationErr
		if result.Err != nil {
			failure = result.Err
		}
		if _, err := w.dirty.RecordFailure(ctx, owner, result.Work, failure); err != nil {
			return err
		}
	}
	return publicationErr
}

func (w *SchedulerSupportPublisherWorker) coalesce(ctx context.Context, owner SchedulerOwnership, initial []SchedulerDirtyWork) ([]SchedulerDirtyWork, error) {
	first := w.clock.Now()
	deadline := first.Add(schedulerSupportPublisherCoalesceCap)
	items := dedupeSchedulerDirtyWork(initial)
	for {
		remaining := deadline.Sub(w.clock.Now())
		if remaining <= 0 {
			return items, nil
		}
		wait := schedulerSupportPublisherDebounce
		if remaining < wait {
			wait = remaining
		}
		if !w.wait(ctx, wait) {
			return nil, ctx.Err()
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for range dirtyWorkBatchSize {
			n, err := w.dirty.Promote(ctx, owner, dirtyWorkBatchSize)
			if err != nil {
				return nil, fmt.Errorf("promote scheduler dirty sources while coalescing: %w", err)
			}
			if n == 0 {
				break
			}
		}
		more, err := w.listDirtyWork(ctx)
		if err != nil {
			return nil, err
		}
		merged := mergeSchedulerDirtyWork(items, more)
		if schedulerDirtyWorkEqual(items, merged) {
			return items, nil
		}
		items = merged
	}
}

type schedulerDirtyWorkKey struct {
	kind     int16
	entityID int64
}

func dedupeSchedulerDirtyWork(work []SchedulerDirtyWork) []SchedulerDirtyWork {
	return mergeSchedulerDirtyWork(nil, work)
}

func schedulerDirtyWorkEqual(left, right []SchedulerDirtyWork) bool {
	if len(left) != len(right) {
		return false
	}
	byKey := make(map[schedulerDirtyWorkKey]int64, len(left))
	for _, item := range left {
		byKey[schedulerDirtyWorkKey{item.Kind, item.EntityID}] = item.Generation
	}
	for _, item := range right {
		if byKey[schedulerDirtyWorkKey{item.Kind, item.EntityID}] != item.Generation {
			return false
		}
	}
	return true
}

// SupportDecisionReplicaWorker adapts the per-process replica to workerruntime.
type SupportDecisionReplicaWorker struct {
	replica     *SupportDecisionReplica
	descriptor  workerruntime.Descriptor
	mu          sync.Mutex
	lifecycle   workerruntime.LifecycleSnapshot
	stopDone    chan struct{}
	stopStarted bool
}

func NewSupportDecisionReplicaWorker(replica *SupportDecisionReplica) *SupportDecisionReplicaWorker {
	return &SupportDecisionReplicaWorker{
		replica: replica,
		descriptor: workerruntime.Descriptor{
			Name:             "scheduler-support-replica",
			Kind:             workerruntime.KindPeriodic,
			Group:            "scheduler",
			CoordinationMode: workerruntime.CoordinationPerInstance,
			Description:      "Maintains the process-local verified scheduler support table",
			Tags:             []string{"scheduler", "support-decision", "replica"},
		},
		lifecycle: workerruntime.LifecycleSnapshot{State: workerruntime.LifecycleStopped, UpdatedAt: time.Now()},
	}
}

func (w *SupportDecisionReplicaWorker) Descriptor() workerruntime.Descriptor {
	if w == nil {
		return workerruntime.Descriptor{}
	}
	return w.descriptor
}

func (w *SupportDecisionReplicaWorker) Snapshot() workerruntime.Snapshot {
	if w == nil {
		return workerruntime.Snapshot{}
	}
	w.mu.Lock()
	descriptor, lifecycle := w.descriptor, w.lifecycle
	w.mu.Unlock()
	status := workerruntime.PeriodicStatus{StillRunning: lifecycle.State == workerruntime.LifecycleRunning || lifecycle.State == workerruntime.LifecycleStopping}
	if w.replica != nil {
		replica := w.replica.Snapshot()
		status.RunCount = replica.Polls
		status.SuccessCount = replica.SuccessfulInstalls + replica.VerificationRefreshes
		for _, classes := range replica.Failures {
			for _, count := range classes {
				status.ErrorCount += count
			}
		}
		status.ErrorCount += replica.SubscriptionFailures
		status.LastRunAt, status.LastDuration, status.LastOutcome = replica.LastCompletedAt, replica.LastDuration, replica.LastOutcome
	}
	return workerruntime.Snapshot{Descriptor: descriptor, Lifecycle: lifecycle, Status: status}
}

func (w *SupportDecisionReplicaWorker) Start(ctx context.Context) error {
	if w == nil || w.replica == nil {
		return errors.New("support decision replica is required")
	}
	w.mu.Lock()
	if w.lifecycle.State == workerruntime.LifecycleRunning {
		w.mu.Unlock()
		return nil
	}
	if w.lifecycle.State == workerruntime.LifecycleStarting || w.lifecycle.State == workerruntime.LifecycleStopping {
		w.mu.Unlock()
		return errors.New("support decision replica lifecycle transition in progress")
	}
	w.lifecycle = workerruntime.LifecycleSnapshot{State: workerruntime.LifecycleStarting, UpdatedAt: time.Now()}
	w.stopDone = nil
	w.stopStarted = false
	w.mu.Unlock()
	if err := w.replica.Start(ctx); err != nil {
		w.mu.Lock()
		w.lifecycle = workerruntime.LifecycleSnapshot{State: workerruntime.LifecycleFailed, UpdatedAt: time.Now(), LastError: err.Error()}
		w.mu.Unlock()
		return err
	}
	w.mu.Lock()
	w.lifecycle = workerruntime.LifecycleSnapshot{State: workerruntime.LifecycleRunning, UpdatedAt: time.Now()}
	w.mu.Unlock()
	return nil
}

func (w *SupportDecisionReplicaWorker) Stop(ctx context.Context) error {
	if w == nil || w.replica == nil {
		return nil
	}
	w.mu.Lock()
	if w.lifecycle.State == workerruntime.LifecycleStopped {
		w.mu.Unlock()
		return nil
	}
	if !w.stopStarted {
		w.lifecycle = workerruntime.LifecycleSnapshot{State: workerruntime.LifecycleStopping, UpdatedAt: time.Now()}
		w.stopStarted = true
		w.stopDone = make(chan struct{})
		done := w.stopDone
		replica := w.replica
		w.mu.Unlock()
		go func() {
			replica.Stop()
			w.mu.Lock()
			w.lifecycle = workerruntime.LifecycleSnapshot{State: workerruntime.LifecycleStopped, UpdatedAt: time.Now()}
			close(done)
			w.mu.Unlock()
		}()
	} else {
		w.mu.Unlock()
	}
	w.mu.Lock()
	done := w.stopDone
	w.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func mergeSchedulerDirtyWork(base, more []SchedulerDirtyWork) []SchedulerDirtyWork {
	out := append([]SchedulerDirtyWork(nil), base...)
	index := make(map[schedulerDirtyWorkKey]int, len(out))
	for i, item := range out {
		index[schedulerDirtyWorkKey{item.Kind, item.EntityID}] = i
	}
	for _, item := range more {
		key := schedulerDirtyWorkKey{item.Kind, item.EntityID}
		if i, ok := index[key]; ok {
			if item.Generation >= out[i].Generation {
				out[i] = item
			}
			continue
		}
		index[key] = len(out)
		out = append(out, item)
	}
	return out
}
