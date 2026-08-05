package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
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

	subscription, err := r.store.SubscribeWakeups(runCtx)
	if err != nil {
		cancel()
		r.finishLifecycle(done)
		return fmt.Errorf("subscribe to support decision wakeups: %w", err)
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
		return fmt.Errorf("load active support decision generation: %w", err)
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
			return
		}
		select {
		case wakeups <- generation:
		case <-ctx.Done():
			return
		}
	}
}

func (r *SupportDecisionReplica) refresh(ctx context.Context) error {
	generation, err := r.store.ActiveGeneration(ctx)
	if err != nil {
		return err
	}
	if generation == 0 {
		return ErrSupportDecisionActiveGenerationNotFound
	}

	localGeneration := r.reader.generation()
	if generation < localGeneration {
		return fmt.Errorf("active support decision generation %d is older than local generation %d", generation, localGeneration)
	}
	verifiedAt := r.deps.now()
	if generation == localGeneration {
		if !r.reader.verifyGeneration(generation, verifiedAt) {
			return errors.New("support decision generation changed during verification")
		}
		return nil
	}

	payload, err := r.store.GetDocument(ctx, generation)
	if err != nil {
		return fmt.Errorf("get support decision generation %d: %w", generation, err)
	}
	table, err := DecodeSupportDecisionDocument(payload, generation)
	if err != nil {
		return fmt.Errorf("decode support decision generation %d: %w", generation, err)
	}
	if !r.reader.installNewer(table, verifiedAt) {
		return fmt.Errorf("support decision generation %d was superseded before installation", generation)
	}
	return nil
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
