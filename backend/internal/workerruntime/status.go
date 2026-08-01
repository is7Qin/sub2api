package workerruntime

import (
	"context"
	"time"
)

// Kind identifies the scheduling model used by a component.
type Kind string

const (
	KindPeriodic Kind = "periodic"
	KindPool     Kind = "pool"
)

// CoordinationMode identifies how a component coordinates across instances.
type CoordinationMode string

const (
	CoordinationPerInstance  CoordinationMode = "per_instance"
	CoordinationSingletonRun CoordinationMode = "singleton_per_run"
	CoordinationDurableClaim CoordinationMode = "durable_claim"
)

// Descriptor is immutable registry metadata for a component.
type Descriptor struct {
	Name             string
	Kind             Kind
	Group            string
	CoordinationMode CoordinationMode
	Description      string
	Tags             []string
}

// LifecycleState describes a component's current lifecycle phase.
type LifecycleState string

const (
	LifecycleStopped  LifecycleState = "stopped"
	LifecycleStarting LifecycleState = "starting"
	LifecycleRunning  LifecycleState = "running"
	LifecycleStopping LifecycleState = "stopping"
	LifecycleFailed   LifecycleState = "failed"

	// StateStopping and StateStartFailed are lifecycle aliases used by runtime clients.
	StateStopping    = LifecycleStopping
	StateStartFailed = LifecycleFailed
)

// LifecycleSnapshot describes lifecycle state that applies to every component kind.
type LifecycleSnapshot struct {
	State     LifecycleState
	UpdatedAt time.Time
	LastError string
}

// Status is the kind-specific portion of a component snapshot.
type Status interface {
	statusKind() Kind
}

// Outcome identifies the result of a periodic invocation.
type Outcome string

const (
	OutcomeSuccess Outcome = "success"
	OutcomeError   Outcome = "error"
	OutcomePanic   Outcome = "panic"
	OutcomeTimeout Outcome = "timeout"
)

// PeriodicStatus is the status reported by periodic components.
type PeriodicStatus struct {
	LastRunAt    time.Time
	NextRunAt    time.Time
	LastDuration time.Duration
	LastOutcome  Outcome
	LastError    string
	RunCount     uint64
	SuccessCount uint64
	ErrorCount   uint64
	PanicCount   uint64
	TimeoutCount uint64
	StillRunning bool
}

func (PeriodicStatus) statusKind() Kind { return KindPeriodic }

// PoolStatus is the status reported by worker-pool components.
type PoolStatus struct {
	Accepting          bool
	StillRunning       bool
	MaxConcurrency     int
	RunningWorkers     int64
	WaitingTasks       uint64
	SubmittedTasks     uint64
	CompletedTasks     uint64
	SuccessfulTasks    uint64
	FailedTasks        uint64
	DroppedTasks       uint64
	DroppedQueueFull   uint64
	DroppedPoolStopped uint64
	SyncFallbackTasks  uint64
}

func (PoolStatus) statusKind() Kind { return KindPool }

// Snapshot is a detached point-in-time view of a component.
type Snapshot struct {
	Descriptor Descriptor
	Lifecycle  LifecycleSnapshot
	Status     Status
}

// Component is a managed runtime worker.
type Component interface {
	Descriptor() Descriptor
	Start(context.Context) error
	Stop(context.Context) error
	Snapshot() Snapshot
}
