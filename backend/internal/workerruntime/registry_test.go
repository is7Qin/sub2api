//go:build unit

package workerruntime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type stubComponent struct {
	mu         sync.RWMutex
	descriptor Descriptor
	lifecycle  LifecycleSnapshot
	status     Status
}

func newStubComponent(name string, kind Kind) *stubComponent {
	return &stubComponent{
		descriptor: Descriptor{
			Name:             name,
			Kind:             kind,
			CoordinationMode: CoordinationPerInstance,
			Tags:             []string{"initial"},
		},
		lifecycle: LifecycleSnapshot{State: LifecycleStopped},
		status:    statusForKind(kind),
	}
}

func statusForKind(kind Kind) Status {
	switch kind {
	case KindPeriodic:
		return PeriodicStatus{LastRunAt: time.Unix(1, 0).UTC()}
	case KindPool:
		return PoolStatus{MaxConcurrency: 2, RunningWorkers: 1}
	default:
		return nil
	}
}

func (c *stubComponent) Descriptor() Descriptor {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.descriptor
}

func (c *stubComponent) Start(context.Context) error { return nil }

func (c *stubComponent) Stop(context.Context) error { return nil }

func (c *stubComponent) Snapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return Snapshot{
		Descriptor: c.descriptor,
		Lifecycle:  c.lifecycle,
		Status:     c.status,
	}
}

func (c *stubComponent) setState(state LifecycleState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lifecycle.State = state
}

func (c *stubComponent) setTag(tag string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.descriptor.Tags[0] = tag
}

func TestRegistryRegisterRejectsDuplicateName(t *testing.T) {
	registry := NewRegistry()
	require.NoError(t, registry.Register(newStubComponent("account-expiry", KindPeriodic)))
	err := registry.Register(newStubComponent("account-expiry", KindPeriodic))
	require.ErrorContains(t, err, "duplicate component name: account-expiry")
}

func TestRegistrySnapshotIsSortedAndDetached(t *testing.T) {
	registry := NewRegistry()
	require.NoError(t, registry.Register(newStubComponent("usage-record-pool", KindPool)))
	require.NoError(t, registry.Register(newStubComponent("account-expiry", KindPeriodic)))

	snapshots := registry.Snapshot()
	require.Equal(t, []string{"account-expiry", "usage-record-pool"}, []string{
		snapshots[0].Descriptor.Name,
		snapshots[1].Descriptor.Name,
	})
	snapshots[0].Descriptor.Name = "mutated"
	snapshots[0].Descriptor.Tags[0] = "mutated"
	stored, ok := registry.Get("account-expiry")
	require.True(t, ok)
	require.Equal(t, "account-expiry", stored.Descriptor.Name)
	require.Equal(t, []string{"initial"}, stored.Descriptor.Tags)
}

func TestRegistryFreezeRejectsNewRegistration(t *testing.T) {
	registry := NewRegistry()
	registry.Freeze()
	require.ErrorContains(t, registry.Register(newStubComponent("account-expiry", KindPeriodic)), "registry is frozen")
}

func TestRegistryRetainsDetachedDescriptor(t *testing.T) {
	registry := NewRegistry()
	component := newStubComponent("account-expiry", KindPeriodic)
	require.NoError(t, registry.Register(component))

	component.setTag("mutated")
	stored, ok := registry.Get("account-expiry")
	require.True(t, ok)
	require.Equal(t, []string{"initial"}, stored.Descriptor.Tags)
}

func TestRegistryRegisterRejectsNilComponent(t *testing.T) {
	registry := NewRegistry()
	require.ErrorContains(t, registry.Register(nil), "component is nil")
}

func TestRegistryRegisterRejectsEmptyName(t *testing.T) {
	registry := NewRegistry()
	require.ErrorContains(t, registry.Register(newStubComponent(" ", KindPeriodic)), "component name is required")
}

func TestRegistryRegisterRejectsUnknownKind(t *testing.T) {
	registry := NewRegistry()
	require.ErrorContains(t, registry.Register(newStubComponent("account-expiry", Kind("unknown"))), "unknown component kind: unknown")
}

func TestRegistryRegisterRejectsUnknownCoordinationMode(t *testing.T) {
	registry := NewRegistry()
	component := newStubComponent("account-expiry", KindPeriodic)
	component.descriptor.CoordinationMode = CoordinationMode("unknown")

	require.ErrorContains(t, registry.Register(component), "unknown coordination mode: unknown")
}

func TestRegistryRegisterRejectsMismatchedTypedStatus(t *testing.T) {
	registry := NewRegistry()
	component := newStubComponent("account-expiry", KindPeriodic)
	component.status = PoolStatus{MaxConcurrency: 1}

	err := registry.Register(component)
	require.ErrorContains(t, err, "snapshot status kind mismatch for account-expiry")
}

func TestRegistryRegisterRejectsTypedNilStatusPointer(t *testing.T) {
	registry := NewRegistry()
	component := newStubComponent("account-expiry", KindPeriodic)
	component.status = (*PeriodicStatus)(nil)

	require.ErrorContains(t, registry.Register(component), "snapshot status kind mismatch for account-expiry")
}

func TestRegistryGetDetachesPointerPeriodicStatus(t *testing.T) {
	registry := NewRegistry()
	component := newStubComponent("account-expiry", KindPeriodic)
	component.status = &PeriodicStatus{RunCount: 1}
	require.NoError(t, registry.Register(component))

	first, ok := registry.Get("account-expiry")
	require.True(t, ok)
	first.Status.(*PeriodicStatus).RunCount = 99

	second, ok := registry.Get("account-expiry")
	require.True(t, ok)
	require.Equal(t, uint64(1), second.Status.(*PeriodicStatus).RunCount)
}

func TestRegistrySnapshotDetachesPointerPoolStatus(t *testing.T) {
	registry := NewRegistry()
	component := newStubComponent("usage-record-pool", KindPool)
	component.status = &PoolStatus{MaxConcurrency: 2}
	require.NoError(t, registry.Register(component))

	first := registry.Snapshot()
	first[0].Status.(*PoolStatus).MaxConcurrency = 99

	second := registry.Snapshot()
	require.Equal(t, 2, second[0].Status.(*PoolStatus).MaxConcurrency)
}

func TestRegistrySnapshotReadsWhileComponentStateChanges(t *testing.T) {
	registry := NewRegistry()
	component := newStubComponent("account-expiry", KindPeriodic)
	require.NoError(t, registry.Register(component))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			default:
				snapshots := registry.Snapshot()
				require.Len(t, snapshots, 1)
				require.Equal(t, "account-expiry", snapshots[0].Descriptor.Name)
			}
		}
	}()

	for i := 0; i < 1000; i++ {
		if i%2 == 0 {
			component.setState(LifecycleRunning)
		} else {
			component.setState(LifecycleStopped)
		}
	}
	cancel()
	wg.Wait()
}
