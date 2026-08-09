package service

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/workerruntime"
)

const supportDecisionReplicaPollInterval = time.Second

type supportDecisionReplicaTicker interface {
	Chan() <-chan time.Time
	Stop()
}

type supportDecisionTimeTicker struct{ ticker *time.Ticker }

func (t supportDecisionTimeTicker) Chan() <-chan time.Time { return t.ticker.C }
func (t supportDecisionTimeTicker) Stop()                  { t.ticker.Stop() }

type supportDecisionReplicaDependencies struct {
	now             func() time.Time
	newTicker       func(time.Duration) supportDecisionReplicaTicker
	afterRefresh    func(error)
	afterHint       func()
	afterStopMarked func()
	beforeFinish    func()
}

type supportDecisionReplicaPhase uint8

const (
	supportDecisionReplicaIdle supportDecisionReplicaPhase = iota
	supportDecisionReplicaStarting
	supportDecisionReplicaRunning
	supportDecisionReplicaStopping
)

// SupportDecisionReplica verifies Redis publication state in the background and
// replaces the process-local immutable table as one atomic operation.
type SupportDecisionReplica struct {
	store  SupportDecisionPublicationStore
	reader *SupportDecisionAtomicReader
	deps   supportDecisionReplicaDependencies

	mu           sync.Mutex
	phase        supportDecisionReplicaPhase
	ctx          context.Context
	cancel       context.CancelFunc
	subscription SupportDecisionWakeupSubscription
	done         chan struct{}
	metrics      supportDecisionReplicaMetrics
}

func NewSupportDecisionReplica(store SupportDecisionPublicationStore, reader *SupportDecisionAtomicReader) *SupportDecisionReplica {
	return newSupportDecisionReplica(store, reader, supportDecisionReplicaDependencies{
		now: time.Now,
		newTicker: func(interval time.Duration) supportDecisionReplicaTicker {
			return supportDecisionTimeTicker{ticker: time.NewTicker(interval)}
		},
	})
}

func newSupportDecisionReplica(store SupportDecisionPublicationStore, reader *SupportDecisionAtomicReader, deps supportDecisionReplicaDependencies) *SupportDecisionReplica {
	return &SupportDecisionReplica{store: store, reader: reader, deps: deps}
}

// Start publishes cancellation and completion state before any blocking store
// call, then reports success only after the initial verified load.
func (r *SupportDecisionReplica) Start(ctx context.Context) error {
	if r == nil || r.store == nil || r.reader == nil {
		return errors.New("support decision replica dependencies are incomplete")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	r.mu.Lock()
	switch r.phase {
	case supportDecisionReplicaRunning:
		if r.ctx != nil && r.ctx.Err() == nil {
			r.mu.Unlock()
			return nil
		}
		r.mu.Unlock()
		return errors.New("support decision replica run is terminating")
	case supportDecisionReplicaStarting:
		r.mu.Unlock()
		return errors.New("support decision replica start already in progress")
	case supportDecisionReplicaStopping:
		r.mu.Unlock()
		return errors.New("support decision replica stop in progress")
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	r.phase = supportDecisionReplicaStarting
	r.ctx = runCtx
	r.cancel = cancel
	r.done = done
	r.mu.Unlock()

	subscriptionStarted := r.deps.now()
	subscription, err := r.store.SubscribeWakeups(runCtx)
	if err != nil {
		completed := r.deps.now()
		r.metrics.completed(subscriptionStarted, completed, workerruntime.OutcomeError, func() {
			r.metrics.subscriptionFailures.Add(1)
		})
		cancel()
		r.finishLifecycle(done)
		if errors.Is(err, context.Canceled) {
			return context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return context.DeadlineExceeded
		}
		return errors.New("subscribe to support decision wakeups: class=operation")
	}

	r.mu.Lock()
	if r.done != done || r.phase != supportDecisionReplicaStarting || runCtx.Err() != nil {
		r.mu.Unlock()
		cancel()
		_ = subscription.Close()
		r.finishLifecycle(done)
		return context.Canceled
	}
	r.subscription = subscription
	r.mu.Unlock()

	if err := r.refresh(runCtx); err != nil {
		cancel()
		_ = subscription.Close()
		r.finishLifecycle(done)
		if errors.Is(err, context.Canceled) {
			return context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return context.DeadlineExceeded
		}
		if errors.Is(err, ErrSupportDecisionActiveGenerationNotFound) {
			return ErrSupportDecisionActiveGenerationNotFound
		}
		return errors.New("load active support decision generation: class=operation")
	}

	r.mu.Lock()
	if r.done != done || r.phase != supportDecisionReplicaStarting || runCtx.Err() != nil {
		r.mu.Unlock()
		cancel()
		_ = subscription.Close()
		r.finishLifecycle(done)
		return context.Canceled
	}
	r.phase = supportDecisionReplicaRunning
	r.mu.Unlock()

	go r.run(runCtx, subscription, done)
	return nil
}

func (r *SupportDecisionReplica) run(ctx context.Context, subscription SupportDecisionWakeupSubscription, done chan struct{}) {
	receiverDone := make(chan struct{})
	defer func() {
		_ = subscription.Close()
		<-receiverDone
		r.finishLifecycle(done)
	}()
	ticker := r.deps.newTicker(supportDecisionReplicaPollInterval)
	defer ticker.Stop()

	wakeups := make(chan uint64)
	go func() {
		defer close(receiverDone)
		r.receiveWakeups(ctx, subscription, wakeups)
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.Chan():
			r.refreshInBackground(ctx)
		case hint, ok := <-wakeups:
			if !ok {
				wakeups = nil
				continue
			}
			r.metrics.wakeups.Add(1)
			// Hints only accelerate an authoritative active-generation poll.
			if hint > r.reader.generation() {
				r.refreshInBackground(ctx)
			}
			if r.deps.afterHint != nil {
				r.deps.afterHint()
			}
		}
	}
}

func (r *SupportDecisionReplica) finishLifecycle(done chan struct{}) {
	if r.deps.beforeFinish != nil {
		r.deps.beforeFinish()
	}
	r.mu.Lock()
	if r.done == done {
		close(done)
		r.phase = supportDecisionReplicaIdle
		r.ctx = nil
		r.cancel = nil
		r.subscription = nil
		r.done = nil
	}
	r.mu.Unlock()
}

func (r *SupportDecisionReplica) lifecycleDone() <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done != nil {
		return r.done
	}
	done := make(chan struct{})
	close(done)
	return done
}

func (r *SupportDecisionReplica) refreshInBackground(ctx context.Context) {
	err := r.refresh(ctx)
	if r.deps.afterRefresh != nil {
		r.deps.afterRefresh(err)
	}
}

func (r *SupportDecisionReplica) receiveWakeups(ctx context.Context, subscription SupportDecisionWakeupSubscription, wakeups chan<- uint64) {
	defer close(wakeups)
	for {
		generation, err := subscription.Receive(ctx)
		if err != nil {
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				completed := r.deps.now()
				r.metrics.completed(completed, completed, workerruntime.OutcomeError, func() {
					r.metrics.subscriptionFailures.Add(1)
				})
			}
			return
		}
		select {
		case wakeups <- generation:
		case <-ctx.Done():
			return
		}
	}
}

func (r *SupportDecisionReplica) refresh(ctx context.Context) (resultErr error) {
	started := r.deps.now()
	var completedMutations []func()
	defer func() {
		completed := r.deps.now()
		outcome := workerruntime.OutcomeSuccess
		if resultErr != nil {
			outcome = workerruntime.OutcomeError
		}
		r.metrics.completed(started, completed, outcome, func() {
			r.metrics.polls.Add(1)
			for _, mutate := range completedMutations {
				mutate()
			}
		})
	}()
	failure := func(stage SupportDecisionReplicaStage, class SupportDecisionErrorClass) {
		completedMutations = append(completedMutations, func() { r.metrics.failures[stage][class].Add(1) })
	}
	if err := ctx.Err(); err != nil {
		failure(SupportDecisionReplicaStageActive, supportDecisionErrorClass(err))
		return err
	}
	generation, err := r.store.ActiveGeneration(ctx)
	if err != nil || generation == 0 {
		failure(SupportDecisionReplicaStageActive, supportDecisionErrorClass(err))
		if errors.Is(err, context.Canceled) {
			return context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return context.DeadlineExceeded
		}
		if errors.Is(err, ErrSupportDecisionActiveGenerationNotFound) || generation == 0 {
			return ErrSupportDecisionActiveGenerationNotFound
		}
		return errors.New("support decision replica stage=active class=operation")
	}
	completedMutations = append(completedMutations, func() { r.metrics.activeGeneration.Store(generation) })

	localGeneration := r.reader.generation()
	if generation < localGeneration {
		failure(SupportDecisionReplicaStageInstall, SupportDecisionErrorOperation)
		return errors.New("support decision replica stage=install class=operation")
	}
	verifiedAt := r.deps.now()
	if generation == localGeneration {
		if !r.reader.verifyGeneration(generation, verifiedAt) {
			failure(SupportDecisionReplicaStageInstall, SupportDecisionErrorOperation)
			return errors.New("support decision replica stage=install class=operation")
		}
		completedMutations = append(completedMutations, func() { r.metrics.verifications.Add(1) })
		return nil
	}

	payload, err := r.store.GetDocument(ctx, generation)
	if err != nil {
		failure(SupportDecisionReplicaStageFetch, supportDecisionErrorClass(err))
		if errors.Is(err, context.Canceled) {
			return context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return context.DeadlineExceeded
		}
		return errors.New("support decision replica stage=fetch class=operation")
	}
	documentBytes := uint64(len(payload))
	completedMutations = append(completedMutations, func() { r.metrics.documentBytes.Store(documentBytes) })
	decodeStarted := r.deps.now()
	table, err := DecodeSupportDecisionDocument(payload, generation)
	decodeDuration := int64(r.deps.now().Sub(decodeStarted))
	completedMutations = append(completedMutations, func() { r.metrics.decodeDurationNanos.Store(decodeDuration) })
	if err != nil {
		failure(SupportDecisionReplicaStageDecode, supportDecisionErrorClass(err))
		return errors.New("support decision replica stage=decode class=operation")
	}
	installStarted := r.deps.now()
	installed := r.reader.installNewer(table, verifiedAt)
	installDuration := int64(r.deps.now().Sub(installStarted))
	completedMutations = append(completedMutations, func() { r.metrics.installDurationNanos.Store(installDuration) })
	if !installed {
		failure(SupportDecisionReplicaStageInstall, SupportDecisionErrorOperation)
		return errors.New("support decision replica stage=install class=operation")
	}
	completedMutations = append(completedMutations, func() { r.metrics.installs.Add(1) })
	return nil
}

func (r *SupportDecisionReplica) Snapshot() SupportDecisionReplicaSnapshot {
	if r == nil {
		return SupportDecisionReplicaSnapshot{Unknown: true}
	}
	for {
		before := r.metrics.sequence.Load()
		if before&1 != 0 {
			continue
		}
		reader := r.reader.Snapshot()
		s := SupportDecisionReplicaSnapshot{
			Polls: r.metrics.polls.Load(), Wakeups: r.metrics.wakeups.Load(), SuccessfulInstalls: r.metrics.installs.Load(), SubscriptionFailures: r.metrics.subscriptionFailures.Load(),
			VerificationRefreshes: r.metrics.verifications.Load(), ActiveGeneration: r.metrics.activeGeneration.Load(),
			InstalledGeneration: reader.Generation, DocumentBytes: r.metrics.documentBytes.Load(), LastVerifiedAt: reader.LastVerifiedAt,
			VerificationAge: reader.VerificationAge, Stale: reader.Stale, Unknown: reader.Unknown,
			DecodeDuration: time.Duration(r.metrics.decodeDurationNanos.Load()), InstallDuration: time.Duration(r.metrics.installDurationNanos.Load()), LastDuration: time.Duration(r.metrics.lastDurationNanos.Load()),
		}
		if unixNano := r.metrics.lastCompletedUnixNano.Load(); unixNano != 0 {
			s.LastCompletedAt = time.Unix(0, unixNano)
		}
		switch r.metrics.lastOutcome.Load() {
		case 1:
			s.LastOutcome = workerruntime.OutcomeSuccess
		case 2:
			s.LastOutcome = workerruntime.OutcomeError
		}
		for stage := range s.Failures {
			for class := range s.Failures[stage] {
				s.Failures[stage][class] = r.metrics.failures[stage][class].Load()
				if stage == int(SupportDecisionReplicaStageActive) {
					s.ActiveGenerationFailures += s.Failures[stage][class]
				}
			}
		}
		if r.metrics.sequence.Load() == before {
			return s
		}
	}
}

// Stop cancels and waits for either startup or running work and is idempotent.
func (r *SupportDecisionReplica) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.phase == supportDecisionReplicaIdle {
		r.mu.Unlock()
		return
	}
	r.phase = supportDecisionReplicaStopping
	cancel, subscription, done := r.cancel, r.subscription, r.done
	r.mu.Unlock()
	if r.deps.afterStopMarked != nil {
		r.deps.afterStopMarked()
	}

	cancel()
	if subscription != nil {
		_ = subscription.Close()
	}
	<-done
}
