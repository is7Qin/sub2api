# Task 1 Report: Runtime Types and Registry

## Scope

Implemented Task 1 only in the isolated worktree:

- `backend/internal/workerruntime/status.go`
  - Runtime domain types: `Kind`, `CoordinationMode`, `Descriptor`, `LifecycleState`, `LifecycleSnapshot`, typed `Status`, `PeriodicStatus`, `PoolStatus`, `Snapshot`, and `Component`.
- `backend/internal/workerruntime/registry.go`
  - `Registry`, `NewRegistry`, `Register`, `Freeze`, `Snapshot`, and `Get`.
  - Registration validates nil components, trimmed names, component kinds, coordination modes, snapshot descriptor consistency, and kind-specific status consistency.
  - Registry stores components in a private name-indexed map. Descriptor metadata is cloned at registration and replaced on returned snapshots, so caller mutation of component descriptor slices cannot alter registry metadata.
  - Component methods are invoked only after releasing registry locks. Snapshot results are cloned and sorted by component name.
- `backend/internal/workerruntime/registry_test.go`
  - Required duplicate, sort/detachment, freeze, validation, typed-status, and concurrent-snapshot tests.

No later runtime lifecycle, periodic-runtime, service-migration, wiring, or status-endpoint work was performed.

## TDD Evidence

### RED

Initial required RED command, after creating the tests and before production code:

```text
go -C backend test -tags=unit ./internal/workerruntime -run 'TestRegistry' -count=1
```

Result: failed as expected because the new package types and functions did not exist. Representative compiler output:

```text
undefined: Descriptor
undefined: LifecycleSnapshot
undefined: Kind
undefined: CoordinationPerInstance
undefined: Snapshot
```

During self-review, I added a test proving registration must reject an unknown coordination mode. Its focused RED command was:

```text
go -C backend test -tags=unit ./internal/workerruntime -run 'TestRegistryRegisterRejectsUnknownCoordinationMode' -count=1
```

Result: failed as expected with `An error is expected but got nil.` The minimal validation branch was then added.

### GREEN

After implementation and formatting:

```text
go -C backend test -tags=unit ./internal/workerruntime -run 'TestRegistry' -count=1
```

Result:

```text
ok github.com/Wei-Shaw/sub2api/internal/workerruntime
```

Required race command:

```text
go -C backend test -race -tags=unit ./internal/workerruntime -run 'TestRegistry' -count=1
```

Result:

```text
ok github.com/Wei-Shaw/sub2api/internal/workerruntime
```

Additional focused package run:

```text
go -C backend test -tags=unit ./internal/workerruntime -count=1
```

Result:

```text
ok github.com/Wei-Shaw/sub2api/internal/workerruntime
```

## Self-Review

- Confirmed registration rejects duplicate names and rejects all registrations after `Freeze`.
- Confirmed snapshots are name-sorted and detached from both returned-slice mutation and registered component descriptor-tag mutation.
- Confirmed a periodic component cannot register with a pool status.
- Confirmed a goroutine can continuously call `Snapshot` while the component changes lifecycle state; the required race run reports no data race.
- Confirmed registry locks protect map/freeze state only; calls to `Descriptor` and `Snapshot` are made outside the lock to avoid lock inversion or blocking registration reads.
- No runtime endpoint or application entrypoint currently consumes this new internal registry, so end-to-end runtime-surface verification is not applicable until later tasks wire it into the application.

## Concern

The status payload fields are intentionally minimal Task 1 foundations. Later component-adapter tasks must populate their real operational fields through `PeriodicStatus` and `PoolStatus` without exposing mutable reference data in snapshots.
