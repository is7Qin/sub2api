//go:build unit

package workerruntime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type lifecycleStub struct {
	mu         sync.RWMutex
	descriptor Descriptor
	lifecycle  LifecycleSnapshot
	events     *[]string
	startErr   error
	stopErr    error
	startCtx   context.Context
	startHook  func()
	stopHook   func()
}

func newLifecycleStub(name string, kind Kind, events *[]string) *lifecycleStub {
	return &lifecycleStub{descriptor: Descriptor{Name: name, Kind: kind, CoordinationMode: CoordinationPerInstance}, lifecycle: LifecycleSnapshot{State: LifecycleStopped}, events: events}
}

func (c *lifecycleStub) Descriptor() Descriptor { return c.descriptor }

func (c *lifecycleStub) Start(ctx context.Context) error {
	if c.startHook != nil {
		c.startHook()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.startCtx = ctx
	if c.events != nil {
		*c.events = append(*c.events, "start:"+c.descriptor.Name)
	}
	if c.startErr != nil {
		c.lifecycle.State = StateStartFailed
		return c.startErr
	}
	c.lifecycle.State = LifecycleRunning
	return nil
}

func (c *lifecycleStub) Stop(context.Context) error {
	if c.stopHook != nil {
		c.stopHook()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.events != nil {
		*c.events = append(*c.events, "stop:"+c.descriptor.Name)
	}
	if c.stopErr != nil {
		return c.stopErr
	}
	c.lifecycle.State = LifecycleStopped
	return nil
}

func (c *lifecycleStub) Snapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return Snapshot{Descriptor: c.descriptor, Lifecycle: c.lifecycle, Status: statusForKind(c.descriptor.Kind)}
}

type blockingStopStub struct {
	*lifecycleStub
	release chan struct{}
}

func newBlockingStopStub(name string) *blockingStopStub {
	return &blockingStopStub{lifecycleStub: newLifecycleStub(name, KindPool, nil), release: make(chan struct{})}
}

func (c *blockingStopStub) Stop(context.Context) error {
	c.mu.Lock()
	c.lifecycle.State = StateStopping
	c.mu.Unlock()
	<-c.release
	c.mu.Lock()
	c.lifecycle.State = LifecycleStopped
	c.mu.Unlock()
	return nil
}

func TestRuntimeStartsPoolsBeforePeriodicJobs(t *testing.T) {
	events := make([]string, 0, 4)
	runtime := NewRuntime(NewRegistry())
	require.NoError(t, runtime.Register(newLifecycleStub("periodic", KindPeriodic, &events)))
	require.NoError(t, runtime.Register(newLifecycleStub("pool", KindPool, &events)))
	require.NoError(t, runtime.StartAll(context.Background()))
	require.Equal(t, []string{"start:pool", "start:periodic"}, events)
}

func TestRuntimeStartFailureRollsBackReverseOrderAndJoinsErrors(t *testing.T) {
	runtime := NewRuntime(NewRegistry())
	first := newLifecycleStub("pool", KindPool, nil)
	second := newLifecycleStub("periodic", KindPeriodic, nil)
	second.startErr = errors.New("start periodic")
	first.stopErr = errors.New("rollback pool")
	require.NoError(t, runtime.Register(first))
	require.NoError(t, runtime.Register(second))
	err := runtime.StartAll(context.Background())
	require.ErrorIs(t, err, second.startErr)
	require.ErrorIs(t, err, first.stopErr)
	require.Equal(t, StateStartFailed, second.Snapshot().Lifecycle.State)
}

func TestRuntimeStopReportsDeadlineWithoutClaimingStopped(t *testing.T) {
	runtime := NewRuntime(NewRegistry())
	component := newBlockingStopStub("pool")
	require.NoError(t, runtime.Register(component))
	require.NoError(t, runtime.StartAll(context.Background()))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	results, err := runtime.StopAll(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, StopTimedOut, results[0].Outcome)
	require.True(t, results[0].StillRunning)
	require.Equal(t, StateStopping, component.Snapshot().Lifecycle.State)
	close(component.release)
	require.Eventually(t, func() bool { return component.Snapshot().Lifecycle.State == LifecycleStopped }, time.Second, time.Millisecond)
}

func TestRuntimeStartAllIsIdempotent(t *testing.T) {
	events := make([]string, 0, 1)
	runtime := NewRuntime(NewRegistry())
	require.NoError(t, runtime.Register(newLifecycleStub("pool", KindPool, &events)))
	require.NoError(t, runtime.StartAll(context.Background()))
	require.NoError(t, runtime.StartAll(context.Background()))
	require.Equal(t, []string{"start:pool"}, events)
}

func TestRuntimeStopAllStopsPeriodicBeforePoolsInDeterministicOrder(t *testing.T) {
	events := make([]string, 0, 8)
	runtime := NewRuntime(NewRegistry())
	for _, component := range []*lifecycleStub{
		newLifecycleStub("z-pool", KindPool, &events), newLifecycleStub("a-periodic", KindPeriodic, &events),
		newLifecycleStub("a-pool", KindPool, &events), newLifecycleStub("z-periodic", KindPeriodic, &events),
	} {
		require.NoError(t, runtime.Register(component))
	}
	require.NoError(t, runtime.StartAll(context.Background()))
	results, err := runtime.StopAll(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"stop:a-periodic", "stop:z-periodic", "stop:a-pool", "stop:z-pool"}, events[4:])
	require.Equal(t, []string{"a-periodic", "z-periodic", "a-pool", "z-pool"}, stopResultNames(results))
}

func TestRuntimeStopAllIsIdempotent(t *testing.T) {
	events := make([]string, 0, 2)
	runtime := NewRuntime(NewRegistry())
	require.NoError(t, runtime.Register(newLifecycleStub("pool", KindPool, &events)))
	require.NoError(t, runtime.StartAll(context.Background()))
	first, err := runtime.StopAll(context.Background())
	require.NoError(t, err)
	second, err := runtime.StopAll(context.Background())
	require.NoError(t, err)
	require.Len(t, first, 1)
	require.Empty(t, second)
	require.Equal(t, []string{"start:pool", "stop:pool"}, events)
}

func TestRuntimeStopReturnsEveryResultWhenOneComponentFails(t *testing.T) {
	runtime := NewRuntime(NewRegistry())
	periodic := newLifecycleStub("periodic", KindPeriodic, nil)
	periodic.stopErr = errors.New("stop periodic")
	pool := newLifecycleStub("pool", KindPool, nil)
	require.NoError(t, runtime.Register(periodic))
	require.NoError(t, runtime.Register(pool))
	require.NoError(t, runtime.StartAll(context.Background()))
	results, err := runtime.StopAll(context.Background())
	require.ErrorIs(t, err, periodic.stopErr)
	require.Equal(t, []string{"periodic", "pool"}, stopResultNames(results))
	require.Equal(t, StopError, results[0].Outcome)
	require.Equal(t, StopCompleted, results[1].Outcome)
}

func TestRuntimeStopCancelsRootContextBeforeStoppingComponents(t *testing.T) {
	runtime := NewRuntime(NewRegistry())
	component := newLifecycleStub("pool", KindPool, nil)
	require.NoError(t, runtime.Register(component))
	require.NoError(t, runtime.StartAll(context.Background()))
	_, err := runtime.StopAll(context.Background())
	require.NoError(t, err)
	require.ErrorIs(t, component.startCtx.Err(), context.Canceled)
}

func TestRuntimeDoesNotHoldRegistryMutexWhileCallingComponents(t *testing.T) {
	registry := NewRegistry()
	runtime := NewRuntime(registry)
	component := newLifecycleStub("pool", KindPool, nil)
	component.startHook = func() { registry.Snapshot() }
	component.stopHook = func() { registry.Snapshot() }
	require.NoError(t, runtime.Register(component))
	startDone := make(chan error, 1)
	go func() { startDone <- runtime.StartAll(context.Background()) }()
	select {
	case err := <-startDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("StartAll called component while registry was locked")
	}
	stopDone := make(chan error, 1)
	go func() { _, err := runtime.StopAll(context.Background()); stopDone <- err }()
	select {
	case err := <-stopDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("StopAll called component while registry was locked")
	}
}

func stopResultNames(results []StopResult) []string {
	names := make([]string, 0, len(results))
	for _, result := range results {
		names = append(names, result.Name)
	}
	return names
}
