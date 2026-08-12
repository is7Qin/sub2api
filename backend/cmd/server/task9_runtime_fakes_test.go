package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type serverSchedulerDirtyRepo struct{}

func (*serverSchedulerDirtyRepo) Promote(context.Context, service.SchedulerOwnership, int) (int, error) {
	return 0, nil
}
func (*serverSchedulerDirtyRepo) RequestFullRebuild(context.Context) error { return nil }
func (*serverSchedulerDirtyRepo) List(context.Context, int) ([]service.SchedulerDirtyWork, error) {
	return nil, nil
}
func (*serverSchedulerDirtyRepo) RecordFailure(context.Context, service.SchedulerOwnership, service.SchedulerDirtyWork, error) (bool, error) {
	return true, nil
}
func (*serverSchedulerDirtyRepo) Acknowledge(context.Context, service.SchedulerOwnership, service.SchedulerDirtyWork) (bool, error) {
	return true, nil
}
func (*serverSchedulerDirtyRepo) PendingStats(context.Context) (service.SchedulerDirtyWorkStats, error) {
	return service.SchedulerDirtyWorkStats{}, nil
}

type serverSchedulerOwnershipRepo struct{}

func (*serverSchedulerOwnershipRepo) TryAcquire(context.Context) (service.SchedulerOwnership, bool, error) {
	return nil, false, nil
}

type serverSchedulerDirtyProcessor struct{}

func (serverSchedulerDirtyProcessor) ApplyDirtyWorkBatch(_ context.Context, work []service.SchedulerDirtyWork) []service.SchedulerDirtyWorkResult {
	results := make([]service.SchedulerDirtyWorkResult, len(work))
	for i, item := range work {
		results[i] = service.SchedulerDirtyWorkResult{Work: item}
	}
	return results
}

type serverSupportDecisionGeneration struct{}

func (serverSupportDecisionGeneration) NextSupportDecisionGeneration(context.Context) (uint64, error) {
	return 2, nil
}

type serverSupportDecisionSource struct{}

func (serverSupportDecisionSource) Load(context.Context) (*service.SupportDecisionConstructionSnapshot, error) {
	return &service.SupportDecisionConstructionSnapshot{}, nil
}

type serverSupportDecisionStore struct {
	mu            sync.Mutex
	documents     map[uint64][]byte
	active        uint64
	subscriptions []*serverSupportDecisionSubscription
}

func newServerSupportDecisionStore(t testing.TB) *serverSupportDecisionStore {
	t.Helper()
	table, err := service.BuildSupportDecisionTable(&service.SupportDecisionConstructionSnapshot{}, service.SupportDecisionBuildOptions{Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := service.EncodeSupportDecisionDocument(table)
	if err != nil {
		t.Fatal(err)
	}
	return &serverSupportDecisionStore{documents: map[uint64][]byte{1: payload}, active: 1}
}

func (*serverSupportDecisionStore) PutDocument(context.Context, uint64, []byte, time.Duration) error {
	return nil
}
func (*serverSupportDecisionStore) Activate(context.Context, uint64) (bool, error) { return true, nil }
func (s *serverSupportDecisionStore) ActiveGeneration(context.Context) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == 0 {
		return 0, service.ErrSupportDecisionActiveGenerationNotFound
	}
	return s.active, nil
}
func (s *serverSupportDecisionStore) GetDocument(_ context.Context, generation uint64) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.documents[generation]...), nil
}
func (s *serverSupportDecisionStore) PublishWakeup(_ context.Context, generation uint64) error {
	s.mu.Lock()
	subscriptions := append([]*serverSupportDecisionSubscription(nil), s.subscriptions...)
	s.mu.Unlock()
	for _, subscription := range subscriptions {
		select {
		case subscription.hints <- generation:
		default:
		}
	}
	return nil
}
func (s *serverSupportDecisionStore) publish(generation uint64, payload []byte) {
	s.mu.Lock()
	s.documents[generation] = append([]byte(nil), payload...)
	s.active = generation
	s.mu.Unlock()
	_ = s.PublishWakeup(context.Background(), generation)
}
func (s *serverSupportDecisionStore) SubscribeWakeups(context.Context) (service.SupportDecisionWakeupSubscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	subscription := newServerSupportDecisionSubscription()
	s.subscriptions = append(s.subscriptions, subscription)
	return subscription, nil
}

type serverSupportDecisionSubscription struct {
	hints     chan uint64
	closed    chan struct{}
	closeOnce sync.Once
}

func newServerSupportDecisionSubscription() *serverSupportDecisionSubscription {
	return &serverSupportDecisionSubscription{hints: make(chan uint64, 1), closed: make(chan struct{})}
}
func (s *serverSupportDecisionSubscription) Receive(ctx context.Context) (uint64, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-s.closed:
		return 0, context.Canceled
	case generation := <-s.hints:
		return generation, nil
	}
}
func (s *serverSupportDecisionSubscription) Close() error {
	s.closeOnce.Do(func() { close(s.closed) })
	return nil
}
