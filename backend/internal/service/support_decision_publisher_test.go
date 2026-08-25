//go:build unit

package service

import (
	"bytes"
	"context"
	"errors"
	"log"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type supportDecisionPublisherGenerationStub struct {
	generations []uint64
	errs        []error
	calls       int
	events      *[]string
}

func (s *supportDecisionPublisherGenerationStub) NextSupportDecisionGeneration(ctx context.Context) (uint64, error) {
	s.calls++
	if s.events != nil {
		*s.events = append(*s.events, "allocate")
	}
	index := s.calls - 1
	var generation uint64
	if index < len(s.generations) {
		generation = s.generations[index]
	}
	if index < len(s.errs) {
		return generation, s.errs[index]
	}
	return generation, nil
}

type supportDecisionPublisherSourceStub struct {
	snapshots []*SupportDecisionConstructionSnapshot
	errs      []error
	calls     int
	events    *[]string
}

func (s *supportDecisionPublisherSourceStub) Load(ctx context.Context) (*SupportDecisionConstructionSnapshot, error) {
	s.calls++
	if s.events != nil {
		*s.events = append(*s.events, "load")
	}
	index := s.calls - 1
	var snapshot *SupportDecisionConstructionSnapshot
	if index < len(s.snapshots) {
		snapshot = s.snapshots[index]
	}
	if index < len(s.errs) {
		return snapshot, s.errs[index]
	}
	return snapshot, nil
}

type supportDecisionPublisherStoreStub struct {
	putFn      func(context.Context, uint64, []byte, time.Duration) error
	activateFn func(context.Context, uint64) (bool, error)
	wakeupFn   func(context.Context, uint64) error
	putCalls   int
	activates  int
	wakeups    int
}

func (s *supportDecisionPublisherStoreStub) PutDocument(ctx context.Context, generation uint64, payload []byte, ttl time.Duration) error {
	s.putCalls++
	if s.putFn != nil {
		return s.putFn(ctx, generation, payload, ttl)
	}
	return nil
}

func (s *supportDecisionPublisherStoreStub) Activate(ctx context.Context, generation uint64) (bool, error) {
	s.activates++
	if s.activateFn != nil {
		return s.activateFn(ctx, generation)
	}
	return true, nil
}

func (s *supportDecisionPublisherStoreStub) PublishWakeup(ctx context.Context, generation uint64) error {
	s.wakeups++
	if s.wakeupFn != nil {
		return s.wakeupFn(ctx, generation)
	}
	return nil
}

func (*supportDecisionPublisherStoreStub) ActiveGeneration(context.Context) (uint64, error) {
	panic("unexpected ActiveGeneration call")
}
func (*supportDecisionPublisherStoreStub) GetDocument(context.Context, uint64) ([]byte, error) {
	panic("unexpected GetDocument call")
}
func (*supportDecisionPublisherStoreStub) SubscribeWakeups(context.Context) (SupportDecisionWakeupSubscription, error) {
	panic("unexpected SubscribeWakeups call")
}

func newSupportDecisionPublisherTestSubject(
	generation SupportDecisionGenerationRepository,
	source SupportDecisionSource,
	store SupportDecisionPublicationStore,
) *SupportDecisionPublisher {
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.SupportDecisionHotModels.OpenAI = []string{"configured-hot-model"}
	cfg.Gateway.OpenAIWS.Enabled = true
	return NewSupportDecisionPublisher(generation, source, store, cfg)
}

func emptySupportDecisionPublisherSnapshot() *SupportDecisionConstructionSnapshot {
	return &SupportDecisionConstructionSnapshot{}
}

func TestSupportDecisionPublisherUsesDedicatedGeneration(t *testing.T) {
	generation := &supportDecisionPublisherGenerationStub{generations: []uint64{73}}
	source := &supportDecisionPublisherSourceStub{snapshots: []*SupportDecisionConstructionSnapshot{emptySupportDecisionPublisherSnapshot()}}
	store := &supportDecisionPublisherStoreStub{}
	publisher := newSupportDecisionPublisherTestSubject(generation, source, store)

	var builtOptions SupportDecisionBuildOptions
	publisher.build = func(snapshot *SupportDecisionConstructionSnapshot, options SupportDecisionBuildOptions) (*SupportDecisionTable, error) {
		builtOptions = options
		return BuildSupportDecisionTable(snapshot, options)
	}
	store.putFn = func(ctx context.Context, gotGeneration uint64, payload []byte, ttl time.Duration) error {
		require.Equal(t, context.Background(), ctx)
		require.Equal(t, uint64(73), gotGeneration)
		require.Positive(t, ttl)
		require.Zero(t, ttl%time.Millisecond)
		table, err := DecodeSupportDecisionDocument(payload, gotGeneration)
		require.NoError(t, err)
		require.Equal(t, gotGeneration, table.Generation)
		return nil
	}
	store.activateFn = func(ctx context.Context, gotGeneration uint64) (bool, error) {
		require.Equal(t, context.Background(), ctx)
		require.Equal(t, uint64(73), gotGeneration)
		return true, nil
	}

	got, err := publisher.Publish(context.Background())
	require.NoError(t, err)
	require.Equal(t, uint64(73), got)
	require.Equal(t, uint64(73), builtOptions.Generation)
	require.Equal(t, []string{"configured-hot-model"}, builtOptions.HotModels.OpenAI)
	require.True(t, builtOptions.OpenAIWS.Enabled)
	require.Equal(t, 1, generation.calls)
	require.Equal(t, 1, source.calls)
}

func TestSupportDecisionPublisherWritesBeforeActivating(t *testing.T) {
	events := make([]string, 0, 5)
	generation := &supportDecisionPublisherGenerationStub{generations: []uint64{9}, events: &events}
	source := &supportDecisionPublisherSourceStub{snapshots: []*SupportDecisionConstructionSnapshot{emptySupportDecisionPublisherSnapshot()}, events: &events}
	store := &supportDecisionPublisherStoreStub{
		putFn: func(context.Context, uint64, []byte, time.Duration) error {
			events = append(events, "put")
			return nil
		},
		activateFn: func(context.Context, uint64) (bool, error) {
			events = append(events, "activate")
			return true, nil
		},
		wakeupFn: func(context.Context, uint64) error {
			events = append(events, "wakeup")
			return nil
		},
	}
	publisher := newSupportDecisionPublisherTestSubject(generation, source, store)
	originalBuild := publisher.build
	publisher.build = func(snapshot *SupportDecisionConstructionSnapshot, options SupportDecisionBuildOptions) (*SupportDecisionTable, error) {
		events = append(events, "build")
		return originalBuild(snapshot, options)
	}

	got, err := publisher.Publish(context.Background())
	require.NoError(t, err)
	require.Equal(t, uint64(9), got)
	require.Equal(t, []string{"allocate", "load", "build", "put", "activate", "wakeup"}, events)
}

func TestSupportDecisionPublisherDoesNotActivateFailedBuild(t *testing.T) {
	generation := &supportDecisionPublisherGenerationStub{generations: []uint64{10}}
	source := &supportDecisionPublisherSourceStub{snapshots: []*SupportDecisionConstructionSnapshot{emptySupportDecisionPublisherSnapshot()}}
	store := &supportDecisionPublisherStoreStub{}
	publisher := newSupportDecisionPublisherTestSubject(generation, source, store)
	buildCalls := 0
	publisher.build = func(*SupportDecisionConstructionSnapshot, SupportDecisionBuildOptions) (*SupportDecisionTable, error) {
		buildCalls++
		return nil, errors.New("build failed")
	}

	got, err := publisher.Publish(context.Background())
	require.ErrorContains(t, err, "build support decision document")
	require.Zero(t, got)
	require.Equal(t, 1, buildCalls)
	require.Equal(t, 1, source.calls)
	require.Zero(t, store.putCalls)
	require.Zero(t, store.activates)
	require.Zero(t, store.wakeups)
}

func TestSupportDecisionPublisherDoesNotActivateOversizedDocument(t *testing.T) {
	generation := &supportDecisionPublisherGenerationStub{generations: []uint64{11}}
	source := &supportDecisionPublisherSourceStub{snapshots: []*SupportDecisionConstructionSnapshot{emptySupportDecisionPublisherSnapshot()}}
	store := &supportDecisionPublisherStoreStub{}
	publisher := newSupportDecisionPublisherTestSubject(generation, source, store)
	publisher.encode = func(*SupportDecisionTable) ([]byte, error) {
		return make([]byte, SupportDecisionMaxDocumentSize+1), nil
	}

	got, err := publisher.Publish(context.Background())
	require.ErrorContains(t, err, "exceeds")
	require.Zero(t, got)
	require.Zero(t, store.putCalls)
	require.Zero(t, store.activates)
	require.Zero(t, store.wakeups)
}

func TestSupportDecisionPublisherDoesNotActivateAfterOwnershipLoss(t *testing.T) {
	t.Run("before document write", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		generation := &supportDecisionPublisherGenerationStub{generations: []uint64{12}}
		source := &supportDecisionPublisherSourceStub{snapshots: []*SupportDecisionConstructionSnapshot{emptySupportDecisionPublisherSnapshot()}}
		store := &supportDecisionPublisherStoreStub{}
		publisher := newSupportDecisionPublisherTestSubject(generation, source, store)
		originalEncode := publisher.encode
		publisher.encode = func(table *SupportDecisionTable) ([]byte, error) {
			payload, err := originalEncode(table)
			cancel()
			return payload, err
		}

		got, err := publisher.Publish(ctx)
		require.ErrorIs(t, err, context.Canceled)
		require.Zero(t, got)
		require.Zero(t, store.putCalls)
		require.Zero(t, store.activates)
		require.Zero(t, store.wakeups)
	})

	t.Run("between document write and activation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		generation := &supportDecisionPublisherGenerationStub{generations: []uint64{13}}
		source := &supportDecisionPublisherSourceStub{snapshots: []*SupportDecisionConstructionSnapshot{emptySupportDecisionPublisherSnapshot()}}
		store := &supportDecisionPublisherStoreStub{
			putFn: func(context.Context, uint64, []byte, time.Duration) error {
				cancel()
				return nil
			},
		}
		publisher := newSupportDecisionPublisherTestSubject(generation, source, store)

		got, err := publisher.Publish(ctx)
		require.ErrorIs(t, err, context.Canceled)
		require.Zero(t, got)
		require.Equal(t, 1, store.putCalls)
		require.Zero(t, store.activates)
		require.Zero(t, store.wakeups)
	})
}

func TestSupportDecisionPublisherTreatsStaleCASAsNotPublished(t *testing.T) {
	generation := &supportDecisionPublisherGenerationStub{generations: []uint64{14}}
	source := &supportDecisionPublisherSourceStub{snapshots: []*SupportDecisionConstructionSnapshot{emptySupportDecisionPublisherSnapshot()}}
	store := &supportDecisionPublisherStoreStub{activateFn: func(context.Context, uint64) (bool, error) { return false, nil }}
	publisher := newSupportDecisionPublisherTestSubject(generation, source, store)

	got, err := publisher.Publish(context.Background())
	require.ErrorIs(t, err, ErrSupportDecisionNotPublished)
	require.Zero(t, got)
	require.Equal(t, 1, store.putCalls)
	require.Equal(t, 1, store.activates)
	require.Zero(t, store.wakeups)
}

func TestSupportDecisionPublisherAcceptsGenerationGaps(t *testing.T) {
	generation := &supportDecisionPublisherGenerationStub{generations: []uint64{20, 22}}
	source := &supportDecisionPublisherSourceStub{
		snapshots: []*SupportDecisionConstructionSnapshot{nil, emptySupportDecisionPublisherSnapshot()},
		errs:      []error{errors.New("temporary source failure"), nil},
	}
	store := &supportDecisionPublisherStoreStub{}
	publisher := newSupportDecisionPublisherTestSubject(generation, source, store)

	first, err := publisher.Publish(context.Background())
	require.Error(t, err)
	require.Zero(t, first)
	second, err := publisher.Publish(context.Background())
	require.NoError(t, err)
	require.Equal(t, uint64(22), second)
	require.Equal(t, 2, generation.calls)
	require.Equal(t, 2, source.calls)
}

func TestSupportDecisionPublisherWakeupIsBestEffort(t *testing.T) {
	generation := &supportDecisionPublisherGenerationStub{generations: []uint64{30}}
	source := &supportDecisionPublisherSourceStub{snapshots: []*SupportDecisionConstructionSnapshot{emptySupportDecisionPublisherSnapshot()}}
	store := &supportDecisionPublisherStoreStub{wakeupFn: func(context.Context, uint64) error { return errors.New("pubsub unavailable") }}
	publisher := newSupportDecisionPublisherTestSubject(generation, source, store)

	got, err := publisher.Publish(context.Background())
	require.NoError(t, err)
	require.Equal(t, uint64(30), got)
	require.Equal(t, 1, store.activates)
	require.Equal(t, 1, store.wakeups)
}

func TestSupportDecisionPublisherDoesNotLogSourceIdentifiers(t *testing.T) {
	var logs bytes.Buffer
	originalWriter := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(originalWriter) })

	generation := &supportDecisionPublisherGenerationStub{generations: []uint64{31}}
	source := &supportDecisionPublisherSourceStub{
		snapshots: []*SupportDecisionConstructionSnapshot{{
			Accounts: []Account{{ID: 987654, Platform: PlatformOpenAI, Credentials: map[string]any{"secret": "do-not-log"}}},
		}},
	}
	store := &supportDecisionPublisherStoreStub{}
	publisher := newSupportDecisionPublisherTestSubject(generation, source, store)

	_, err := publisher.Publish(context.Background())
	require.NoError(t, err)
	require.Empty(t, logs.String())
}

func TestSupportDecisionPublisherFailureFences(t *testing.T) {
	t.Run("entry cancellation does not allocate", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		generation := &supportDecisionPublisherGenerationStub{generations: []uint64{40}}
		source := &supportDecisionPublisherSourceStub{}
		store := &supportDecisionPublisherStoreStub{}
		publisher := newSupportDecisionPublisherTestSubject(generation, source, store)

		got, err := publisher.Publish(ctx)
		require.ErrorIs(t, err, context.Canceled)
		require.Zero(t, got)
		require.Zero(t, generation.calls)
		require.Zero(t, source.calls)
		require.Zero(t, store.putCalls)
	})

	t.Run("source load failure has no Redis side effects", func(t *testing.T) {
		generation := &supportDecisionPublisherGenerationStub{generations: []uint64{41}}
		source := &supportDecisionPublisherSourceStub{errs: []error{errors.New("source unavailable")}}
		store := &supportDecisionPublisherStoreStub{}
		publisher := newSupportDecisionPublisherTestSubject(generation, source, store)
		buildCalls := 0
		publisher.build = func(*SupportDecisionConstructionSnapshot, SupportDecisionBuildOptions) (*SupportDecisionTable, error) {
			buildCalls++
			return nil, nil
		}

		got, err := publisher.Publish(context.Background())
		require.ErrorContains(t, err, "load support decision source")
		require.Zero(t, got)
		require.Equal(t, 1, source.calls)
		require.Zero(t, buildCalls)
		require.Zero(t, store.putCalls)
		require.Zero(t, store.activates)
	})

	t.Run("write failure prevents activation", func(t *testing.T) {
		generation := &supportDecisionPublisherGenerationStub{generations: []uint64{42}}
		source := &supportDecisionPublisherSourceStub{snapshots: []*SupportDecisionConstructionSnapshot{emptySupportDecisionPublisherSnapshot()}}
		store := &supportDecisionPublisherStoreStub{putFn: func(context.Context, uint64, []byte, time.Duration) error { return errors.New("write failed") }}
		publisher := newSupportDecisionPublisherTestSubject(generation, source, store)

		got, err := publisher.Publish(context.Background())
		require.ErrorContains(t, err, "put support decision document")
		require.Zero(t, got)
		require.Equal(t, 1, store.putCalls)
		require.Zero(t, store.activates)
		require.Zero(t, store.wakeups)
	})

	t.Run("activation error prevents wakeup", func(t *testing.T) {
		generation := &supportDecisionPublisherGenerationStub{generations: []uint64{43}}
		source := &supportDecisionPublisherSourceStub{snapshots: []*SupportDecisionConstructionSnapshot{emptySupportDecisionPublisherSnapshot()}}
		store := &supportDecisionPublisherStoreStub{activateFn: func(context.Context, uint64) (bool, error) { return false, errors.New("ambiguous activation") }}
		publisher := newSupportDecisionPublisherTestSubject(generation, source, store)

		got, err := publisher.Publish(context.Background())
		require.ErrorContains(t, err, "activate support decision document")
		require.Zero(t, got)
		require.Equal(t, 1, store.activates)
		require.Zero(t, store.wakeups)
	})

	t.Run("codec error prevents write", func(t *testing.T) {
		generation := &supportDecisionPublisherGenerationStub{generations: []uint64{44}}
		source := &supportDecisionPublisherSourceStub{snapshots: []*SupportDecisionConstructionSnapshot{emptySupportDecisionPublisherSnapshot()}}
		store := &supportDecisionPublisherStoreStub{}
		publisher := newSupportDecisionPublisherTestSubject(generation, source, store)
		publisher.encode = func(*SupportDecisionTable) ([]byte, error) { return nil, errors.New("codec failed") }

		got, err := publisher.Publish(context.Background())
		require.ErrorContains(t, err, "encode support decision document")
		require.Zero(t, got)
		require.Zero(t, store.putCalls)
	})
}
