package service

import (
	"bytes"
	"context"
	"errors"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/workerruntime"
	"github.com/stretchr/testify/require"
)

type schedulerSupportPublisherTestProcessor struct {
	mu           sync.Mutex
	results      []SchedulerDirtyWorkResult
	calls        [][]SchedulerDirtyWork
	block        <-chan struct{}
	ignoreCancel bool
}

func (p *schedulerSupportPublisherTestProcessor) ApplyDirtyWorkBatch(ctx context.Context, work []SchedulerDirtyWork) []SchedulerDirtyWorkResult {
	p.mu.Lock()
	p.calls = append(p.calls, append([]SchedulerDirtyWork(nil), work...))
	results := append([]SchedulerDirtyWorkResult(nil), p.results...)
	block, ignoreCancel := p.block, p.ignoreCancel
	p.mu.Unlock()
	if block != nil {
		if ignoreCancel {
			<-block
		} else {
			select {
			case <-block:
			case <-ctx.Done():
				results = make([]SchedulerDirtyWorkResult, len(work))
				for i := range work {
					results[i] = SchedulerDirtyWorkResult{Work: work[i], Err: ctx.Err()}
				}
			}
		}
	}
	if results == nil {
		results = make([]SchedulerDirtyWorkResult, len(work))
		for i := range work {
			results[i].Work = work[i]
		}
	}
	return results
}

type schedulerSupportPublisherTestPublisher struct {
	mu      sync.Mutex
	calls   int
	gen     uint64
	err     error
	entered chan struct{}
	block   <-chan struct{}
}

func (p *schedulerSupportPublisherTestPublisher) Publish(ctx context.Context) (uint64, error) {
	p.mu.Lock()
	p.calls++
	if p.entered != nil {
		select {
		case <-p.entered:
		default:
			close(p.entered)
		}
	}
	gen, err, block := p.gen, p.err, p.block
	p.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	if err != nil {
		return 0, err
	}
	if gen == 0 {
		return 0, ErrSupportDecisionNotPublished
	}
	return gen, nil
}

type schedulerSupportPublisherTestOwnership struct {
	ctx    context.Context
	cancel context.CancelFunc
	lost   chan struct{}
	mu     sync.Mutex
	closes int
}

func newSchedulerSupportPublisherTestOwnership() *schedulerSupportPublisherTestOwnership {
	ctx, cancel := context.WithCancel(context.Background())
	return &schedulerSupportPublisherTestOwnership{ctx: ctx, cancel: cancel, lost: make(chan struct{})}
}
func (o *schedulerSupportPublisherTestOwnership) Epoch() int64             { return 1 }
func (o *schedulerSupportPublisherTestOwnership) Context() context.Context { return o.ctx }
func (o *schedulerSupportPublisherTestOwnership) Lost() <-chan struct{}    { return o.lost }
func (o *schedulerSupportPublisherTestOwnership) Err() error               { return o.ctx.Err() }
func (o *schedulerSupportPublisherTestOwnership) Close() error {
	o.mu.Lock()
	o.closes++
	o.mu.Unlock()
	o.cancel()
	return nil
}

type schedulerSupportPublisherTestOwnershipRepo struct {
	owner    SchedulerOwnership
	acquired bool
	calls    int
}

func (r *schedulerSupportPublisherTestOwnershipRepo) TryAcquire(ctx context.Context) (SchedulerOwnership, bool, error) {
	r.calls++
	if r.acquired {
		return nil, false, nil
	}
	r.acquired = true
	return r.owner, true, nil
}

type schedulerSupportPublisherTestRepo struct {
	mu           sync.Mutex
	work         []SchedulerDirtyWork
	lists        [][]SchedulerDirtyWork
	listCalls    int
	promoteCalls int
	acks         []SchedulerDirtyWork
	failures     []SchedulerDirtyWork
	failureErrs  []error
	ackHook      func(SchedulerDirtyWork)
	promoteHook  func(int)
}

func (r *schedulerSupportPublisherTestRepo) Promote(context.Context, SchedulerOwnership, int) (int, error) {
	r.mu.Lock()
	r.promoteCalls++
	call := r.promoteCalls
	hook := r.promoteHook
	r.mu.Unlock()
	if hook != nil {
		hook(call)
	}
	return 0, nil
}
func (r *schedulerSupportPublisherTestRepo) RequestFullRebuild(context.Context) error { return nil }
func (r *schedulerSupportPublisherTestRepo) List(context.Context, int) ([]SchedulerDirtyWork, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []SchedulerDirtyWork
	if r.listCalls < len(r.lists) {
		out = r.lists[r.listCalls]
	} else {
		out = r.work
	}
	r.listCalls++
	return append([]SchedulerDirtyWork(nil), out...), nil
}
func (r *schedulerSupportPublisherTestRepo) RecordFailure(_ context.Context, _ SchedulerOwnership, w SchedulerDirtyWork, failure error) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures = append(r.failures, w)
	r.failureErrs = append(r.failureErrs, failure)
	return true, nil
}
func (r *schedulerSupportPublisherTestRepo) Acknowledge(ctx context.Context, _ SchedulerOwnership, w SchedulerDirtyWork) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ackHook != nil {
		r.ackHook(w)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	r.acks = append(r.acks, w)
	return true, nil
}
func (r *schedulerSupportPublisherTestRepo) PendingStats(context.Context) (SchedulerDirtyWorkStats, error) {
	return SchedulerDirtyWorkStats{}, nil
}

type schedulerSupportPublisherTestTimer struct {
	ch      chan time.Time
	stopped bool
}

func (t *schedulerSupportPublisherTestTimer) Chan() <-chan time.Time { return t.ch }
func (t *schedulerSupportPublisherTestTimer) Stop()                  { t.stopped = true }

type schedulerSupportPublisherTestClock struct {
	mu        sync.Mutex
	now       time.Time
	durations []time.Duration
	timers    []*schedulerSupportPublisherTestTimer
}

func (c *schedulerSupportPublisherTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *schedulerSupportPublisherTestClock) NewTimer(d time.Duration) schedulerSupportPublisherTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &schedulerSupportPublisherTestTimer{ch: make(chan time.Time, 1)}
	c.durations = append(c.durations, d)
	c.timers = append(c.timers, t)
	return t
}
func (c *schedulerSupportPublisherTestClock) fire(i int, advance time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(advance)
	t := c.timers[i]
	now := c.now
	c.mu.Unlock()
	t.ch <- now
}
func (c *schedulerSupportPublisherTestClock) timerCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}
func (c *schedulerSupportPublisherTestClock) timerDuration(i int) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.durations[i]
}

func newSchedulerSupportPublisherTestWorker(repo SchedulerDirtyWorkRepository, ownerRepo SchedulerOwnershipRepository, processor SchedulerSnapshotDirtyProcessor, publisher supportDecisionBatchPublisher, clock schedulerSupportPublisherClock) *SchedulerSupportPublisherWorker {
	return newSchedulerSupportPublisherWorker(repo, ownerRepo, processor, publisher, time.Hour, clock)
}

func TestSchedulerSupportPublisherWorkerIsSoleDirtyWorkOwner(t *testing.T) {
	svc := newSchedulerSnapshotService(&outboxPollCache{}, nil, &schedulerSupportPublisherTestRepo{}, &schedulerSupportPublisherTestOwnershipRepo{}, nil, nil, nil)
	svc.Start()
	svc.Stop()
	require.NotNil(t, svc)
}

func TestSchedulerSnapshotStartDoesNotLaunchDirtyOwner(t *testing.T) {
	repo := &schedulerSupportPublisherTestOwnershipRepo{}
	svc := newSchedulerSnapshotService(&outboxPollCache{}, nil, &schedulerSupportPublisherTestRepo{}, repo, nil, nil, nil)
	svc.Start()
	svc.Stop()
	require.Zero(t, repo.calls)
}

func TestSchedulerSupportPublisherDebouncesFromFirstItemFor100Milliseconds(t *testing.T) {
	clock := &schedulerSupportPublisherTestClock{now: time.Unix(1, 0)}
	work := []SchedulerDirtyWork{{Kind: 1, EntityID: 1, Generation: 1}}
	repo := &schedulerSupportPublisherTestRepo{work: work}
	w := newSchedulerSupportPublisherTestWorker(repo, nil, &schedulerSupportPublisherTestProcessor{}, &schedulerSupportPublisherTestPublisher{gen: 1}, clock)
	done := make(chan struct{})
	go func() {
		w.processAvailable(context.Background(), newSchedulerSupportPublisherTestOwnership())
		close(done)
	}()
	require.Eventually(t, func() bool { return clock.timerCount() == 1 }, time.Second, time.Millisecond)
	require.Equal(t, 100*time.Millisecond, clock.timerDuration(0))
	clock.fire(0, 100*time.Millisecond)
	<-done
}

func TestSchedulerSupportPublisherStaticFullPagePublishesAfter100Milliseconds(t *testing.T) {
	clock := &schedulerSupportPublisherTestClock{now: time.Unix(1, 0)}
	batch := make([]SchedulerDirtyWork, dirtyWorkBatchSize)
	for i := range batch {
		batch[i] = SchedulerDirtyWork{Kind: 1, EntityID: int64(i + 1), Generation: 1}
	}
	repo := &schedulerSupportPublisherTestRepo{work: batch}
	processor := &schedulerSupportPublisherTestProcessor{}
	w := newSchedulerSupportPublisherTestWorker(repo, nil, processor, &schedulerSupportPublisherTestPublisher{gen: 1}, clock)
	done := make(chan struct{})
	go func() {
		w.processAvailable(context.Background(), newSchedulerSupportPublisherTestOwnership())
		close(done)
	}()
	require.Eventually(t, func() bool { return clock.timerCount() == 1 }, time.Second, time.Millisecond)
	clock.fire(0, 100*time.Millisecond)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("unchanged full page did not publish after first debounce")
	}
	require.Equal(t, 100*time.Millisecond, clock.Now().Sub(time.Unix(1, 0)))
}

func TestSchedulerSupportPublisherCapsContinuousCoalescingAtOneSecond(t *testing.T) {
	clock := &schedulerSupportPublisherTestClock{now: time.Unix(1, 0)}
	initial := []SchedulerDirtyWork{{Kind: 1, EntityID: 1, Generation: 1}}
	lists := make([][]SchedulerDirtyWork, 11)
	lists[0] = initial
	for i := 1; i < len(lists); i++ {
		lists[i] = []SchedulerDirtyWork{{Kind: 1, EntityID: 1, Generation: int64(i + 1)}}
	}
	repo := &schedulerSupportPublisherTestRepo{lists: lists}
	processor := &schedulerSupportPublisherTestProcessor{}
	w := newSchedulerSupportPublisherTestWorker(repo, nil, processor, &schedulerSupportPublisherTestPublisher{gen: 1}, clock)
	done := make(chan struct{})
	go func() {
		w.processAvailable(context.Background(), newSchedulerSupportPublisherTestOwnership())
		close(done)
	}()
	for i := 0; i < 10; i++ {
		require.Eventually(t, func() bool { return clock.timerCount() > i }, time.Second, time.Millisecond)
		clock.fire(i, 100*time.Millisecond)
	}
	<-done
	require.Equal(t, time.Second, clock.Now().Sub(time.Unix(1, 0)))
	require.Len(t, processor.calls, 1)
	require.Equal(t, int64(11), processor.calls[0][0].Generation)
}

func TestSchedulerSupportPublisherPromotesSourceArrivalsDuringCoalescing(t *testing.T) {
	clock := &schedulerSupportPublisherTestClock{now: time.Unix(1, 0)}
	first := SchedulerDirtyWork{Kind: 1, EntityID: 1, Generation: 1}
	arrival := SchedulerDirtyWork{Kind: 2, EntityID: 9, Generation: 3}
	repo := &schedulerSupportPublisherTestRepo{lists: [][]SchedulerDirtyWork{{first}, {first, arrival}, {first, arrival}}}
	processor := &schedulerSupportPublisherTestProcessor{}
	w := newSchedulerSupportPublisherTestWorker(repo, nil, processor, &schedulerSupportPublisherTestPublisher{gen: 1}, clock)
	done := make(chan struct{})
	go func() {
		w.processAvailable(context.Background(), newSchedulerSupportPublisherTestOwnership())
		close(done)
	}()
	for i := 0; i < 2; i++ {
		require.Eventually(t, func() bool { return clock.timerCount() > i }, time.Second, time.Millisecond)
		clock.fire(i, 100*time.Millisecond)
	}
	<-done
	require.GreaterOrEqual(t, repo.promoteCalls, 3)
	require.Equal(t, []SchedulerDirtyWork{first, arrival}, processor.calls[0])
}

func TestSchedulerSupportPublisherAcknowledgesOnlyAfterActivation(t *testing.T) {
	work := []SchedulerDirtyWork{{Kind: 1, EntityID: 1, Generation: 2}}
	repo := &schedulerSupportPublisherTestRepo{work: work}
	published := false
	repo.ackHook = func(SchedulerDirtyWork) { require.True(t, published) }
	publisher := &schedulerSupportPublisherTestPublisher{gen: 1}
	processor := &schedulerSupportPublisherTestProcessor{}
	w := newSchedulerSupportPublisherTestWorker(repo, nil, processor, publisher, immediateSchedulerSupportPublisherClock{})
	publisher.entered = make(chan struct{})
	repo.ackHook = func(SchedulerDirtyWork) {
		select {
		case <-publisher.entered:
			published = true
		default:
		}
		require.True(t, published)
	}
	require.NoError(t, w.processAvailable(context.Background(), newSchedulerSupportPublisherTestOwnership()))
	require.Equal(t, work, repo.acks)
}

func TestSchedulerSupportPublisherLeavesBatchPendingWhenPublicationFails(t *testing.T) {
	work := []SchedulerDirtyWork{{Kind: 1, EntityID: 1, Generation: 2}}
	repo := &schedulerSupportPublisherTestRepo{work: work}
	w := newSchedulerSupportPublisherTestWorker(repo, nil, &schedulerSupportPublisherTestProcessor{}, &schedulerSupportPublisherTestPublisher{err: errors.New("publish")}, immediateSchedulerSupportPublisherClock{})
	require.Error(t, w.processAvailable(context.Background(), newSchedulerSupportPublisherTestOwnership()))
	require.Empty(t, repo.acks)
	require.Equal(t, work, repo.failures)
}

func TestSchedulerSupportPublisherPreservesPerItemSnapshotFailureIsolation(t *testing.T) {
	work := []SchedulerDirtyWork{{Kind: 1, EntityID: 1, Generation: 1}, {Kind: 2, EntityID: 2, Generation: 4}}
	failed := errors.New("snapshot")
	processor := &schedulerSupportPublisherTestProcessor{results: []SchedulerDirtyWorkResult{{Work: work[0]}, {Work: work[1], Err: failed}}}
	repo := &schedulerSupportPublisherTestRepo{work: work}
	w := newSchedulerSupportPublisherTestWorker(repo, nil, processor, &schedulerSupportPublisherTestPublisher{gen: 2}, immediateSchedulerSupportPublisherClock{})
	require.NoError(t, w.processAvailable(context.Background(), newSchedulerSupportPublisherTestOwnership()))
	require.Equal(t, []SchedulerDirtyWork{work[0]}, repo.acks)
	require.Equal(t, []SchedulerDirtyWork{work[1]}, repo.failures)
}

func TestSchedulerSupportPublisherPreservesSnapshotErrorWhenPublicationAlsoFails(t *testing.T) {
	work := []SchedulerDirtyWork{{Kind: 1, EntityID: 1, Generation: 1}, {Kind: 2, EntityID: 2, Generation: 4}}
	snapshotErr := errors.New("snapshot failure")
	publishErr := errors.New("publication failure")
	processor := &schedulerSupportPublisherTestProcessor{results: []SchedulerDirtyWorkResult{{Work: work[0], Err: snapshotErr}, {Work: work[1]}}}
	repo := &schedulerSupportPublisherTestRepo{work: work}
	w := newSchedulerSupportPublisherTestWorker(repo, nil, processor, &schedulerSupportPublisherTestPublisher{err: publishErr}, immediateSchedulerSupportPublisherClock{})
	require.ErrorIs(t, w.processAvailable(context.Background(), newSchedulerSupportPublisherTestOwnership()), publishErr)
	require.Empty(t, repo.acks)
	require.Equal(t, work, repo.failures)
	require.ErrorIs(t, repo.failureErrs[0], snapshotErr)
	require.ErrorIs(t, repo.failureErrs[1], publishErr)
}

func TestSchedulerSupportPublisherUsesExactDirtyGenerationForAck(t *testing.T) {
	observed := SchedulerDirtyWork{Kind: 1, EntityID: 8, Generation: 7}
	repo := &schedulerSupportPublisherTestRepo{work: []SchedulerDirtyWork{observed}}
	w := newSchedulerSupportPublisherTestWorker(repo, nil, &schedulerSupportPublisherTestProcessor{}, &schedulerSupportPublisherTestPublisher{gen: 1}, immediateSchedulerSupportPublisherClock{})
	require.NoError(t, w.processAvailable(context.Background(), newSchedulerSupportPublisherTestOwnership()))
	require.Equal(t, int64(7), repo.acks[0].Generation)
}

func TestSchedulerSupportPublisherStopsOnOwnershipLoss(t *testing.T) {
	owner := newSchedulerSupportPublisherTestOwnership()
	close(owner.lost)
	owner.cancel()
	repo := &schedulerSupportPublisherTestRepo{work: []SchedulerDirtyWork{{Kind: 1, EntityID: 1, Generation: 1}}}
	w := newSchedulerSupportPublisherTestWorker(repo, nil, &schedulerSupportPublisherTestProcessor{}, &schedulerSupportPublisherTestPublisher{gen: 1}, immediateSchedulerSupportPublisherClock{})
	require.ErrorIs(t, w.processAvailable(owner.Context(), owner), context.Canceled)
	require.Empty(t, repo.acks)
}

func TestSchedulerSupportPublisherStopCancelsBlockedBuild(t *testing.T) {
	block := make(chan struct{})
	entered := make(chan struct{})
	publisher := &schedulerSupportPublisherTestPublisher{gen: 1, entered: entered, block: block}
	owner := newSchedulerSupportPublisherTestOwnership()
	repo := &schedulerSupportPublisherTestRepo{work: []SchedulerDirtyWork{{Kind: 1, EntityID: 1, Generation: 1}}}
	ownerRepo := &schedulerSupportPublisherTestOwnershipRepo{owner: owner}
	worker := newSchedulerSupportPublisherTestWorker(repo, ownerRepo, &schedulerSupportPublisherTestProcessor{}, publisher, immediateSchedulerSupportPublisherClock{})
	require.NoError(t, worker.Start(context.Background()))
	require.Eventually(t, func() bool {
		select {
		case <-entered:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond)
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, worker.Stop(stopCtx))
	owner.mu.Lock()
	closes := owner.closes
	owner.mu.Unlock()
	require.Equal(t, 1, closes)
}

func TestSchedulerSupportPublisherCancelsDuringDebounce(t *testing.T) {
	clock := &schedulerSupportPublisherTestClock{now: time.Unix(1, 0)}
	owner := newSchedulerSupportPublisherTestOwnership()
	repo := &schedulerSupportPublisherTestRepo{work: []SchedulerDirtyWork{{Kind: 1, EntityID: 1, Generation: 1}}}
	worker := newSchedulerSupportPublisherTestWorker(repo, nil, &schedulerSupportPublisherTestProcessor{}, &schedulerSupportPublisherTestPublisher{gen: 1}, clock)
	ctx, cancel := context.WithCancel(owner.Context())
	done := make(chan error, 1)
	go func() { done <- worker.processAvailable(ctx, owner) }()
	require.Eventually(t, func() bool { return clock.timerCount() == 1 }, time.Second, time.Millisecond)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	require.Empty(t, repo.acks)
}

func TestSchedulerSupportPublisherCancelsBeforeAck(t *testing.T) {
	owner := newSchedulerSupportPublisherTestOwnership()
	repo := &schedulerSupportPublisherTestRepo{work: []SchedulerDirtyWork{{Kind: 1, EntityID: 1, Generation: 1}}}
	publisher := &schedulerSupportPublisherTestPublisher{gen: 1}
	repo.ackHook = func(SchedulerDirtyWork) { owner.cancel(); <-owner.Context().Done() }
	worker := newSchedulerSupportPublisherTestWorker(repo, nil, &schedulerSupportPublisherTestProcessor{}, publisher, immediateSchedulerSupportPublisherClock{})
	require.ErrorIs(t, worker.processAvailable(owner.Context(), owner), context.Canceled)
	require.Empty(t, repo.acks)
}

func TestSchedulerSupportPublisherRuntimeStopHonorsDeadline(t *testing.T) {
	block := make(chan struct{})
	processor := &schedulerSupportPublisherTestProcessor{block: block, ignoreCancel: true}
	owner := newSchedulerSupportPublisherTestOwnership()
	repo := &schedulerSupportPublisherTestRepo{work: []SchedulerDirtyWork{{Kind: 1, EntityID: 1, Generation: 1}}}
	worker := newSchedulerSupportPublisherTestWorker(repo, &schedulerSupportPublisherTestOwnershipRepo{owner: owner}, processor, &schedulerSupportPublisherTestPublisher{gen: 1}, immediateSchedulerSupportPublisherClock{})
	require.NoError(t, worker.Start(context.Background()))
	require.Eventually(t, func() bool { processor.mu.Lock(); defer processor.mu.Unlock(); return len(processor.calls) > 0 }, time.Second, time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, worker.Stop(ctx), context.DeadlineExceeded)
	close(block)
	require.Eventually(t, func() bool { return worker.Snapshot().Lifecycle.State == workerruntime.LifecycleStopped }, time.Second, time.Millisecond)
}

func TestSchedulerSupportPublisherDoesNotLogRawErrors(t *testing.T) {
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(old) })
	owner := newSchedulerSupportPublisherTestOwnership()
	repo := &schedulerSupportPublisherTestRepo{work: []SchedulerDirtyWork{{Kind: 1, EntityID: 987654, Generation: 1}}}
	marker := "credential=secret source=scope-987654"
	worker := newSchedulerSupportPublisherTestWorker(repo, nil, &schedulerSupportPublisherTestProcessor{}, &schedulerSupportPublisherTestPublisher{err: errors.New(marker)}, immediateSchedulerSupportPublisherClock{})
	require.Error(t, worker.processAvailable(owner.Context(), owner))
	require.NotContains(t, logs.String(), marker)
}
