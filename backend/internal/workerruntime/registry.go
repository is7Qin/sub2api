package workerruntime

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
)

type registeredComponent struct {
	component  Component
	descriptor Descriptor
}

// Registry stores runtime components by name.
type Registry struct {
	mu         sync.RWMutex
	components map[string]registeredComponent
	frozen     bool
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{components: make(map[string]registeredComponent)}
}

// Register validates and stores a component.
func (r *Registry) Register(component Component) error {
	if isNilComponent(component) {
		return fmt.Errorf("component is nil")
	}

	descriptor := cloneDescriptor(component.Descriptor())
	if err := validateDescriptor(descriptor); err != nil {
		return err
	}

	snapshot := cloneSnapshot(component.Snapshot())
	if snapshot.Descriptor.Name != descriptor.Name || snapshot.Descriptor.Kind != descriptor.Kind {
		return fmt.Errorf("snapshot descriptor mismatch for %s", descriptor.Name)
	}
	if snapshot.Status == nil || snapshot.Status.statusKind() != descriptor.Kind {
		return fmt.Errorf("snapshot status kind mismatch for %s", descriptor.Name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		return fmt.Errorf("registry is frozen")
	}
	if _, exists := r.components[descriptor.Name]; exists {
		return fmt.Errorf("duplicate component name: %s", descriptor.Name)
	}
	if r.components == nil {
		r.components = make(map[string]registeredComponent)
	}
	r.components[descriptor.Name] = registeredComponent{
		component:  component,
		descriptor: descriptor,
	}
	return nil
}

// Freeze prevents subsequent registrations.
func (r *Registry) Freeze() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frozen = true
}

// Snapshot returns name-sorted, detached component snapshots.
func (r *Registry) Snapshot() []Snapshot {
	components := r.componentCopies()
	snapshots := make([]Snapshot, 0, len(components))
	for _, registered := range components {
		snapshot := cloneSnapshot(registered.component.Snapshot())
		snapshot.Descriptor = cloneDescriptor(registered.descriptor)
		snapshots = append(snapshots, snapshot)
	}
	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].Descriptor.Name < snapshots[j].Descriptor.Name
	})
	return snapshots
}

// Get returns a detached snapshot for the named component.
func (r *Registry) Get(name string) (Snapshot, bool) {
	r.mu.RLock()
	registered, ok := r.components[name]
	r.mu.RUnlock()
	if !ok {
		return Snapshot{}, false
	}
	snapshot := cloneSnapshot(registered.component.Snapshot())
	snapshot.Descriptor = cloneDescriptor(registered.descriptor)
	return snapshot, true
}

func (r *Registry) componentCopies() []registeredComponent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	components := make([]registeredComponent, 0, len(r.components))
	for _, registered := range r.components {
		registered.descriptor = cloneDescriptor(registered.descriptor)
		components = append(components, registered)
	}
	return components
}

func validateDescriptor(descriptor Descriptor) error {
	if strings.TrimSpace(descriptor.Name) == "" {
		return fmt.Errorf("component name is required")
	}
	switch descriptor.Kind {
	case KindPeriodic, KindPool:
	default:
		return fmt.Errorf("unknown component kind: %s", descriptor.Kind)
	}
	switch descriptor.CoordinationMode {
	case CoordinationPerInstance, CoordinationSingletonRun, CoordinationDurableClaim:
		return nil
	default:
		return fmt.Errorf("unknown coordination mode: %s", descriptor.CoordinationMode)
	}
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	snapshot.Descriptor = cloneDescriptor(snapshot.Descriptor)
	return snapshot
}

func cloneDescriptor(descriptor Descriptor) Descriptor {
	descriptor.Tags = append([]string(nil), descriptor.Tags...)
	return descriptor
}

func isNilComponent(component Component) bool {
	if component == nil {
		return true
	}
	value := reflect.ValueOf(component)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
