//go:build unit

package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type supportDecisionReplicaFakeStore struct {
	mu               sync.Mutex
	active           uint64
	activeErr        error
	documents        map[uint64][]byte
	documentErr      error
	activeCalls      int
	documentCalls    int
	activeCalled     chan struct{}
	subscription     *supportDecisionReplicaFakeSubscription
	subscribeStarted chan struct{}
	subscribeRelease <-chan struct{}
	subscribeCalls   int
}

func newSupportDecisionReplicaFakeStore() *supportDecisionReplicaFakeStore {
	return &supportDecisionReplicaFakeStore{
		documents:    make(map[uint64][]byte),
		activeCalled: make(chan struct{}, 32),
		subscription: newSupportDecisionReplicaFakeSubscription(),
	}
}

func (s *supportDecisionReplicaFakeStore) PutDocument(context.Context, uint64, []byte, time.Duration) error {
	panic("unexpected publisher call")
}
func (s *supportDecisionReplicaFakeStore) Activate(context.Context, uint64) (bool, error) {
	panic("unexpected publisher call")
}
func (s *supportDecisionReplicaFakeStore) PublishWakeup(context.Context, uint64) error {
	panic("unexpected publisher call")
}
func (s *supportDecisionReplicaFakeStore) SubscribeWakeups(ctx context.Context) (SupportDecisionWakeupSubscription, error) {
	s.mu.Lock()
	s.subscribeCalls++
	started, release := s.subscribeStarted, s.subscribeRelease
	s.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if release != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
		}
	}
	return s.subscription, nil
}
func (s *supportDecisionReplicaFakeStore) ActiveGeneration(context.Context) (uint64, error) {
	s.mu.Lock()
	s.activeCalls++
	generation, err := s.active, s.activeErr
	s.mu.Unlock()
	select {
	case s.activeCalled <- struct{}{}:
	default:
	}
	return generation, err
}
func (s *supportDecisionReplicaFakeStore) GetDocument(_ context.Context, generation uint64) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.documentCalls++
	if s.documentErr != nil {
		return nil, s.documentErr
	}
	payload, ok := s.documents[generation]
	if !ok {
		return nil, ErrSupportDecisionDocumentNotFound
	}
	return append([]byte(nil), payload...), nil
}
func (s *supportDecisionReplicaFakeStore) setActive(generation uint64, err error) {
	s.mu.Lock()
	s.active, s.activeErr = generation, err
	s.mu.Unlock()
}
func (s *supportDecisionReplicaFakeStore) put(generation uint64, payload []byte) {
	s.mu.Lock()
	s.documents[generation] = append([]byte(nil), payload...)
	s.mu.Unlock()
}
func (s *supportDecisionReplicaFakeStore) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeCalls, s.documentCalls
}

type supportDecisionReplicaFakeSubscription struct {
	hints     chan uint64
	closed    chan struct{}
	closeOnce sync.Once
}

func newSupportDecisionReplicaFakeSubscription() *supportDecisionReplicaFakeSubscription {
	return &supportDecisionReplicaFakeSubscription{hints: make(chan uint64, 16), closed: make(chan struct{})}
}
func (s *supportDecisionReplicaFakeSubscription) Receive(ctx context.Context) (uint64, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-s.closed:
		return 0, errors.New("closed")
	case generation := <-s.hints:
		return generation, nil
	}
}
func (s *supportDecisionReplicaFakeSubscription) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	return nil
}

type supportDecisionReplicaFakeTicker struct {
	ch      chan time.Time
	stopped chan struct{}
	stop    sync.Once
}

func newSupportDecisionReplicaFakeTicker() *supportDecisionReplicaFakeTicker {
	return &supportDecisionReplicaFakeTicker{ch: make(chan time.Time, 16), stopped: make(chan struct{})}
}
func (t *supportDecisionReplicaFakeTicker) Chan() <-chan time.Time { return t.ch }
func (t *supportDecisionReplicaFakeTicker) Stop() {
	t.stop.Do(func() { close(t.stopped) })
}

type supportDecisionReplicaHarness struct {
	replica      *SupportDecisionReplica
	reader       *SupportDecisionAtomicReader
	store        *supportDecisionReplicaFakeStore
	ticker       *supportDecisionReplicaFakeTicker
	refreshed    chan struct{}
	hintsHandled chan struct{}
	now          time.Time
	query        SupportDecisionQuery
}

func newSupportDecisionReplicaHarness(t *testing.T) *supportDecisionReplicaHarness {
	t.Helper()
	h := &supportDecisionReplicaHarness{
		reader: NewSupportDecisionAtomicReader(30 * time.Second),
		store:  newSupportDecisionReplicaFakeStore(),
		ticker: newSupportDecisionReplicaFakeTicker(),
		now:    time.Unix(100, 0),
		query: SupportDecisionQuery{
			Scope:          SupportDecisionScope{Platform: PlatformAnthropic, GroupID: 42},
			RequestedModel: "hot",
		},
	}
	h.reader.now = func() time.Time { return h.now }
	refreshed := make(chan struct{}, 32)
	hintsHandled := make(chan struct{}, 32)
	h.replica = newSupportDecisionReplica(h.store, h.reader, supportDecisionReplicaDependencies{
		now: func() time.Time { return h.now },
		newTicker: func(interval time.Duration) supportDecisionReplicaTicker {
			require.Equal(t, time.Second, interval)
			return h.ticker
		},
		afterRefresh: func(error) { refreshed <- struct{}{} },
		afterHint:    func() { hintsHandled <- struct{}{} },
	})
	h.refreshed = refreshed
	h.hintsHandled = hintsHandled
	return h
}

func (h *supportDecisionReplicaHarness) addDocument(t *testing.T, generation uint64, accounts []Account) {
	t.Helper()
	table := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot(accounts, PlatformAnthropic, []string{"hot"}), SupportDecisionBuildOptions{Generation: generation})
	payload, err := EncodeSupportDecisionDocument(table)
	require.NoError(t, err)
	h.store.put(generation, payload)
}

func (h *supportDecisionReplicaHarness) start(t *testing.T) {
	t.Helper()
	require.NoError(t, h.replica.Start(context.Background()))
	t.Cleanup(h.replica.Stop)
}

func (h *supportDecisionReplicaHarness) tickAndWait(t *testing.T) {
	t.Helper()
	h.ticker.ch <- h.now
	select {
	case <-h.refreshed:
	case <-time.After(time.Second):
		t.Fatal("refresh did not complete")
	}
}

func TestSupportDecisionReplicaLoadsActiveGenerationAtStartup(t *testing.T) {
	h := newSupportDecisionReplicaHarness(t)
	h.addDocument(t, 1, []Account{{Platform: PlatformAnthropic}})
	h.store.setActive(1, nil)
	h.start(t)
	require.Equal(t, SupportDecisionNotPureMiss, h.reader.Lookup(h.query))
	require.Equal(t, uint64(1), h.reader.generation())
}

func TestSupportDecisionReplicaReturnsUnknownBeforeInitialLoad(t *testing.T) {
	h := newSupportDecisionReplicaHarness(t)
	require.Equal(t, SupportDecisionUnknown, h.reader.Lookup(h.query))
}

func TestSupportDecisionReplicaPollsWithoutWakeups(t *testing.T) {
	h := newSupportDecisionReplicaHarness(t)
	h.addDocument(t, 1, []Account{{Platform: PlatformAnthropic}})
	h.addDocument(t, 2, nil)
	h.store.setActive(1, nil)
	h.start(t)
	h.store.setActive(2, nil)
	h.tickAndWait(t)
	require.Equal(t, uint64(2), h.reader.generation())
}

func TestSupportDecisionReplicaWakeupAcceleratesPolling(t *testing.T) {
	h := newSupportDecisionReplicaHarness(t)
	h.addDocument(t, 1, []Account{{Platform: PlatformAnthropic}})
	h.addDocument(t, 2, nil)
	h.store.setActive(1, nil)
	h.start(t)
	h.store.setActive(2, nil)
	before, _ := h.store.counts()
	h.store.subscription.hints <- 2
	select {
	case <-h.hintsHandled:
	case <-time.After(time.Second):
		t.Fatal("hint was not handled")
	}
	after, _ := h.store.counts()
	require.Greater(t, after, before)
	require.Equal(t, uint64(2), h.reader.generation())
}

func TestSupportDecisionReplicaIgnoresOlderGeneration(t *testing.T) {
	h := newSupportDecisionReplicaHarness(t)
	h.addDocument(t, 2, []Account{{Platform: PlatformAnthropic}})
	h.store.setActive(2, nil)
	h.start(t)
	before, _ := h.store.counts()
	h.store.subscription.hints <- 1
	select {
	case <-h.hintsHandled:
	case <-time.After(time.Second):
		t.Fatal("hint was not handled")
	}
	after, _ := h.store.counts()
	require.Equal(t, before, after)
	require.Equal(t, uint64(2), h.reader.generation())
}

func TestSupportDecisionReplicaRejectsCorruptOrOversizedDocument(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload []byte
	}{
		{name: "corrupt", payload: []byte("not-json")},
		{name: "oversized", payload: make([]byte, SupportDecisionMaxDocumentSize+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newSupportDecisionReplicaHarness(t)
			h.addDocument(t, 1, []Account{{Platform: PlatformAnthropic}})
			h.store.setActive(1, nil)
			h.start(t)
			verifiedAt := h.reader.verifiedAt()
			h.store.put(2, test.payload)
			h.store.setActive(2, nil)
			h.tickAndWait(t)
			require.Equal(t, uint64(1), h.reader.generation())
			require.Equal(t, verifiedAt, h.reader.verifiedAt())
		})
	}
}

func TestSupportDecisionReplicaReplacesTableAtomically(t *testing.T) {
	h := newSupportDecisionReplicaHarness(t)
	h.addDocument(t, 1, []Account{{Platform: PlatformAnthropic}})
	h.addDocument(t, 2, []Account{{Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": map[string]any{"other": "other"}}}})
	h.store.setActive(1, nil)
	h.start(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 10000; i++ {
			got := h.reader.Lookup(h.query)
			if got != SupportDecisionNotPureMiss && got != SupportDecisionPureMiss {
				t.Errorf("observed partial table result %v", got)
				return
			}
		}
	}()
	h.store.setActive(2, nil)
	h.tickAndWait(t)
	<-done
	require.Equal(t, SupportDecisionPureMiss, h.reader.Lookup(h.query))
}

func TestSupportDecisionReplicaUsesVerifiedTableDuringShortRedisOutage(t *testing.T) {
	h := newSupportDecisionReplicaHarness(t)
	h.addDocument(t, 1, []Account{{Platform: PlatformAnthropic}})
	h.store.setActive(1, nil)
	h.start(t)
	h.store.setActive(0, errors.New("redis unavailable"))
	h.now = h.now.Add(29 * time.Second)
	h.tickAndWait(t)
	require.Equal(t, SupportDecisionNotPureMiss, h.reader.Lookup(h.query))
}

func TestSupportDecisionReplicaReturnsUnknownAfterThirtySecondsUnverified(t *testing.T) {
	h := newSupportDecisionReplicaHarness(t)
	h.addDocument(t, 1, []Account{{Platform: PlatformAnthropic}})
	h.store.setActive(1, nil)
	h.start(t)
	h.store.setActive(0, errors.New("redis unavailable"))
	h.now = h.now.Add(30*time.Second + time.Nanosecond)
	h.tickAndWait(t)
	require.Equal(t, SupportDecisionUnknown, h.reader.Lookup(h.query))
	require.Equal(t, uint64(1), h.reader.generation())
}

func TestSupportDecisionReplicaRecoversAfterRedisReturns(t *testing.T) {
	h := newSupportDecisionReplicaHarness(t)
	h.addDocument(t, 1, []Account{{Platform: PlatformAnthropic}})
	h.store.setActive(1, nil)
	h.start(t)
	h.store.setActive(0, errors.New("redis unavailable"))
	h.now = h.now.Add(31 * time.Second)
	h.tickAndWait(t)
	require.Equal(t, SupportDecisionUnknown, h.reader.Lookup(h.query))
	h.store.setActive(1, nil)
	h.tickAndWait(t)
	require.Equal(t, SupportDecisionNotPureMiss, h.reader.Lookup(h.query))
}

func TestSupportDecisionReplicaStopCancelsPollAndSubscription(t *testing.T) {
	h := newSupportDecisionReplicaHarness(t)
	h.addDocument(t, 1, []Account{{Platform: PlatformAnthropic}})
	h.store.setActive(1, nil)
	require.NoError(t, h.replica.Start(context.Background()))
	h.replica.Stop()
	h.replica.Stop()
	select {
	case <-h.store.subscription.closed:
	case <-time.After(time.Second):
		t.Fatal("subscription was not closed")
	}
	select {
	case <-h.ticker.stopped:
	case <-time.After(time.Second):
		t.Fatal("ticker was not stopped")
	}
}

func TestSupportDecisionReplicaSameGenerationVerifiesWithoutRefetch(t *testing.T) {
	h := newSupportDecisionReplicaHarness(t)
	h.addDocument(t, 1, []Account{{Platform: PlatformAnthropic}})
	h.store.setActive(1, nil)
	h.start(t)
	_, fetches := h.store.counts()
	h.now = h.now.Add(20 * time.Second)
	h.tickAndWait(t)
	_, afterFetches := h.store.counts()
	require.Equal(t, fetches, afterFetches)
	require.Equal(t, h.now, h.reader.verifiedAt())
}

func TestSupportDecisionReplicaStartupRequiresActiveGeneration(t *testing.T) {
	h := newSupportDecisionReplicaHarness(t)
	h.store.setActive(0, ErrSupportDecisionActiveGenerationNotFound)
	require.ErrorIs(t, h.replica.Start(context.Background()), ErrSupportDecisionActiveGenerationNotFound)
	require.Equal(t, SupportDecisionUnknown, h.reader.Lookup(h.query))
	select {
	case <-h.store.subscription.closed:
	case <-time.After(time.Second):
		t.Fatal("failed startup did not close subscription")
	}
}

func TestSupportDecisionReplicaPreventsActiveGenerationRollback(t *testing.T) {
	h := newSupportDecisionReplicaHarness(t)
	h.addDocument(t, 2, []Account{{Platform: PlatformAnthropic}})
	h.store.setActive(2, nil)
	h.start(t)
	verifiedAt := h.reader.verifiedAt()
	h.now = h.now.Add(10 * time.Second)
	h.store.setActive(1, nil)
	h.tickAndWait(t)
	require.Equal(t, uint64(2), h.reader.generation())
	require.Equal(t, verifiedAt, h.reader.verifiedAt())
}

func TestSupportDecisionReplicaRejectsMissingOrMismatchedDocument(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*supportDecisionReplicaHarness, *testing.T)
	}{
		{name: "missing"},
		{name: "mismatched", setup: func(h *supportDecisionReplicaHarness, t *testing.T) {
			h.addDocument(t, 3, nil)
			h.store.mu.Lock()
			h.store.documents[2] = append([]byte(nil), h.store.documents[3]...)
			h.store.mu.Unlock()
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newSupportDecisionReplicaHarness(t)
			h.addDocument(t, 1, []Account{{Platform: PlatformAnthropic}})
			h.store.setActive(1, nil)
			h.start(t)
			if test.setup != nil {
				test.setup(h, t)
			}
			verifiedAt := h.reader.verifiedAt()
			h.now = h.now.Add(5 * time.Second)
			h.store.setActive(2, nil)
			h.tickAndWait(t)
			require.Equal(t, uint64(1), h.reader.generation())
			require.Equal(t, verifiedAt, h.reader.verifiedAt())
		})
	}
}

func TestSupportDecisionReplicaExactlyThirtySecondsRemainsUsable(t *testing.T) {
	h := newSupportDecisionReplicaHarness(t)
	h.addDocument(t, 1, []Account{{Platform: PlatformAnthropic}})
	h.store.setActive(1, nil)
	h.start(t)
	h.store.setActive(0, errors.New("redis unavailable"))
	h.now = h.now.Add(30 * time.Second)
	h.tickAndWait(t)
	require.Equal(t, SupportDecisionNotPureMiss, h.reader.Lookup(h.query))
}

func TestSupportDecisionReplicaFailedVerificationPreservesTimestamp(t *testing.T) {
	h := newSupportDecisionReplicaHarness(t)
	h.addDocument(t, 1, []Account{{Platform: PlatformAnthropic}})
	h.store.setActive(1, nil)
	h.start(t)
	verifiedAt := h.reader.verifiedAt()
	h.now = h.now.Add(10 * time.Second)
	h.store.setActive(0, errors.New("redis unavailable"))
	h.tickAndWait(t)
	require.Equal(t, verifiedAt, h.reader.verifiedAt())
}

func TestSupportDecisionReplicaStopDuringStartCancelsAndWaits(t *testing.T) {
	h := newSupportDecisionReplicaHarness(t)
	h.addDocument(t, 1, []Account{{Platform: PlatformAnthropic}})
	h.store.setActive(1, nil)
	started, release := make(chan struct{}, 1), make(chan struct{})
	h.store.subscribeStarted, h.store.subscribeRelease = started, release
	startResult := make(chan error, 1)
	go func() { startResult <- h.replica.Start(context.Background()) }()
	<-started
	stopDone := make(chan struct{})
	go func() { h.replica.Stop(); close(stopDone) }()
	require.ErrorIs(t, <-startResult, context.Canceled)
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("stop did not wait for startup cancellation")
	}
	close(release)
}

func TestSupportDecisionReplicaStartDuringStopDoesNotCrossLifecycle(t *testing.T) {
	h := newSupportDecisionReplicaHarness(t)
	h.addDocument(t, 1, []Account{{Platform: PlatformAnthropic}})
	h.store.setActive(1, nil)
	require.NoError(t, h.replica.Start(context.Background()))
	stopMarked := make(chan struct{}, 1)
	h.replica.deps.afterStopMarked = func() { stopMarked <- struct{}{} }
	stopDone := make(chan struct{})
	go func() { h.replica.Stop(); close(stopDone) }()
	<-stopMarked
	require.Error(t, h.replica.Start(context.Background()))
	<-stopDone
}

func TestSupportDecisionReplicaDuplicateConcurrentStartUsesSingleStartup(t *testing.T) {
	h := newSupportDecisionReplicaHarness(t)
	h.addDocument(t, 1, []Account{{Platform: PlatformAnthropic}})
	h.store.setActive(1, nil)
	started, release := make(chan struct{}, 1), make(chan struct{})
	h.store.subscribeStarted, h.store.subscribeRelease = started, release
	first := make(chan error, 1)
	go func() { first <- h.replica.Start(context.Background()) }()
	<-started
	require.Error(t, h.replica.Start(context.Background()))
	close(release)
	require.NoError(t, <-first)
	h.replica.Stop()
	h.store.mu.Lock()
	require.Equal(t, 1, h.store.subscribeCalls)
	h.store.mu.Unlock()
}

func TestSupportDecisionReplicaParentCancellationAllowsRestart(t *testing.T) {
	h := newSupportDecisionReplicaHarness(t)
	h.addDocument(t, 1, []Account{{Platform: PlatformAnthropic}})
	h.store.setActive(1, nil)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, h.replica.Start(ctx))
	cancel()
	select {
	case <-h.replica.lifecycleDone():
	case <-time.After(time.Second):
		t.Fatal("replica did not terminate after parent cancellation")
	}
	h.store.mu.Lock()
	h.store.subscription = newSupportDecisionReplicaFakeSubscription()
	h.store.mu.Unlock()
	h.ticker = newSupportDecisionReplicaFakeTicker()
	h.replica.deps.newTicker = func(time.Duration) supportDecisionReplicaTicker { return h.ticker }
	require.NoError(t, h.replica.Start(context.Background()))
	h.replica.Stop()
}

func TestSupportDecisionReplicaStartRejectsCanceledRunBeforeTeardown(t *testing.T) {
	h := newSupportDecisionReplicaHarness(t)
	h.addDocument(t, 1, []Account{{Platform: PlatformAnthropic}})
	h.store.setActive(1, nil)
	teardownReached, releaseTeardown := make(chan struct{}), make(chan struct{})
	h.replica.deps.beforeFinish = func() {
		close(teardownReached)
		<-releaseTeardown
	}
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, h.replica.Start(ctx))
	oldDone := h.replica.lifecycleDone()
	cancel()
	<-teardownReached

	require.Error(t, h.replica.Start(context.Background()))
	close(releaseTeardown)
	<-oldDone

	h.store.mu.Lock()
	h.store.subscription = newSupportDecisionReplicaFakeSubscription()
	h.store.mu.Unlock()
	h.ticker = newSupportDecisionReplicaFakeTicker()
	h.replica.deps.newTicker = func(time.Duration) supportDecisionReplicaTicker { return h.ticker }
	h.replica.deps.beforeFinish = nil
	require.NoError(t, h.replica.Start(context.Background()))
	h.replica.Stop()
}

func TestSupportDecisionAtomicReaderRetainsMonotonicVerificationTime(t *testing.T) {
	start := time.Now()
	table := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot([]Account{{Platform: PlatformAnthropic}}, PlatformAnthropic, []string{"hot"}))
	reader := NewSupportDecisionAtomicReader(30 * time.Second)
	reader.now = func() time.Time { return start.Add(30*time.Second + time.Nanosecond) }
	require.True(t, reader.Install(table, start))
	require.Equal(t, start, reader.verifiedAt())
	// == compares the hidden monotonic reading too; Equal alone compares instants.
	require.True(t, reader.state.Load().verifiedAt == start)
}

func TestSupportDecisionAtomicReaderLookupRemainsAllocationFree(t *testing.T) {
	table := buildSupportDecisionTestTable(t, supportDecisionTestSnapshot([]Account{{Platform: PlatformAnthropic}}, PlatformAnthropic, []string{"hot"}))
	reader := NewSupportDecisionAtomicReader(30 * time.Second)
	now := time.Now()
	reader.now = func() time.Time { return now }
	require.True(t, reader.Install(table, now))
	query := SupportDecisionQuery{Scope: SupportDecisionScope{Platform: PlatformAnthropic, GroupID: 42}, RequestedModel: "hot"}
	require.Zero(t, testing.AllocsPerRun(1000, func() { _ = reader.Lookup(query) }))
}
