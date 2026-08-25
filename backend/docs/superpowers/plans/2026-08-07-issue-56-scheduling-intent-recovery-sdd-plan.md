# Issue #56: Preserve Scheduling Intent Across Error Recovery — SDD Plan

> **Issue:** an automatically blocked account can recover to `status=active` but remain unavailable to scheduling because entering the error state overwrote `schedulable=true` with `false`.
>
> **Boundary:** separate transient account health from durable administrator scheduling intent. Do not add a migration, infer destroyed historical intent, or unconditionally reschedule accounts during recovery.

## Objective

An account has two independent scheduling inputs:

```text
status       = runtime health state
schedulable  = durable administrator intent
```

Effective scheduling continues to require both:

```text
status == active && schedulable == true && no runtime scheduling block
```

Entering an error state must preserve `schedulable`. The error status itself blocks effective scheduling. Clearing the error restores `status=active` and therefore restores effective scheduling only when the preserved administrator intent is `true`.

## Scope and non-goals

### Included

- Preserve the stored `schedulable` value when `AccountRepository.SetError` marks an account errored.
- Preserve the supplied `schedulable` value when the generic account update path stores `status=error`.
- Keep error accounts effectively unavailable through existing status checks and scheduler queries.
- Keep `ClearError` limited to clearing health state and publishing the recovered snapshot.
- Cover both administrator intents (`true` and `false`) through error entry and recovery.
- Preserve scheduler outbox and immediate snapshot synchronization behavior.

### Excluded

- Unconditionally setting `schedulable=true` in `ClearError`, `RecoverAccountState`, scheduled-test recovery, manual account tests, `/recover-state`, or custom error-code handlers.
- Adding `schedulable_before_error`, provenance flags, schema changes, or migrations.
- Guessing whether an existing `status=error, schedulable=false` row was manually disabled or was corrupted by the old behavior.
- Broad scheduler, recovery-service, or handler refactoring.
- Changing rate-limit, overload, temporary-unschedulable, expiration, or quota semantics.

## Invariants

1. `status=error` always makes `Account.IsSchedulable()` false, regardless of the stored intent.
2. Repository schedulable queries continue to require `status=active` and `schedulable=true`.
3. Setting an error changes runtime health and the error message, not administrator scheduling intent.
4. A generic full account update stores the caller's explicit `Schedulable` value even when the new status is `error`.
5. Clearing an error changes `status` to `active` and clears `error_message`; it does not write `schedulable`.
6. Recovery makes a formerly enabled account effectively schedulable again, subject to the other existing runtime gates.
7. Recovery leaves a manually disabled account effectively unschedulable.
8. Error entry and recovery retain existing scheduler outbox and cache-snapshot publication.
9. Historical rows whose previous intent was already destroyed remain unchanged; administrators must explicitly re-enable them.

## SDD scenario matrix

Production changes begin only after the relevant scenario fails for the intended reason.

| ID | Initial state | Operation | Stored result | Effective scheduling / cache result |
|---|---|---|---|---|
| S1 | active, `schedulable=true` | `SetError` | error, intent remains `true` | unavailable while errored |
| S2 | active, `schedulable=false` | `SetError` | error, intent remains `false` | unavailable while errored |
| S3 | active, `schedulable=true` | full `Update` to error | error, intent remains `true` | unavailable while errored |
| S4 | active, `schedulable=false` | full `Update` to error | error, intent remains `false` | unavailable while errored |
| S5 | error, `schedulable=true` | `ClearError` | active, intent remains `true` | stored and published snapshot are schedulable |
| S6 | error, `schedulable=false` | `ClearError` | active, intent remains `false` | stored and published snapshot remain disabled |
| S7 | active and healthy | `ClearError` | no row change | no redundant outbox/cache effect |
| S8 | custom error policy | `SetError`, then successful recovery | same S1/S2 and S5/S6 semantics | no handler-specific rescheduling needed |

## Verified RED checkpoint

The focused repository integration regressions were added before production edits:

```bash
go test -tags=integration ./internal/repository \
  -run 'TestAccountRepoSuite/(TestSetErrorPreservesSchedulingIntent|TestUpdateErrorStatusPreservesSchedulingIntent)' \
  -count=1
```

Expected failures were observed only for the `schedulable=true` cases:

```text
TestSetErrorPreservesSchedulingIntent/true: expected true, actual false
TestUpdateErrorStatusPreservesSchedulingIntent/true: expected true, actual false
```

The `false` cases passed, isolating the defect to destructive loss of enabled administrator intent.

## Root cause and bounded design

The regression was introduced by commit `202aab8e6` in two repository writes:

1. `SetError` explicitly wrote `schedulable=false`.
2. `updateAccountRow` replaced the account's scheduling value with `false` whenever `status=error`.

Those writes are unnecessary because both the domain model and scheduler queries already require active status for effective scheduling.

The minimum implementation is therefore:

- remove the status-based `schedulable` override from `updateAccountRow`;
- have `SetError` update only `status` and `error_message`;
- leave `ClearError`, recovery services, handlers, outbox emission, and snapshot synchronization unchanged.

A concise repository comment should document why error transitions preserve the field, preventing a future reintroduction of the lossy coupling.

## Implementation order

1. Keep S1–S4 regression tests and their verified RED evidence.
2. Extend recovery snapshot coverage for S5–S6 before production changes; verify the enabled case fails under the old destructive error transition.
3. Remove the two destructive repository writes and add the maintenance comment.
4. Run the focused repository integration scenarios until green.
5. Run relevant recovery and custom-error service tests to confirm no caller adds unconditional rescheduling.
6. Run the repository package integration suite or the narrowest stable broader repository gate.
7. Run formatting, `git diff --check`, and final diff review.

## Verification commands

```bash
go test -tags=integration ./internal/repository \
  -run 'TestAccountRepoSuite/(TestSetErrorPreservesSchedulingIntent|TestUpdateErrorStatusPreservesSchedulingIntent|TestClearError_SyncSchedulerSnapshotOnRecovery)' \
  -count=1

go test -tags=unit ./internal/service \
  -run 'TestRateLimitService_(RecoverAccountAfterSuccessfulTest_.*|RecoverAccountState_.*)' \
  -count=1

go test -tags=integration ./internal/service \
  -run 'Test.*ErrorPolicy.*' \
  -count=1

go test -tags=integration ./internal/repository -count=1

gofmt -w internal/repository/account_repo.go internal/repository/account_repo_integration_test.go
git diff --check
```

If a broad suite has an environmental or pre-existing failure, rerun the failing test in isolation and compare it with the unchanged base before classifying it as a regression.

## Review checklist

- [ ] Error state still blocks effective scheduling.
- [ ] `SetError` preserves both `schedulable=true` and `schedulable=false`.
- [ ] Full update to error preserves both scheduling intents.
- [ ] `ClearError` does not write `schedulable`.
- [ ] Recovery publishes the preserved intent to the scheduler cache.
- [ ] No recovery caller unconditionally enables scheduling.
- [ ] Existing outbox and snapshot synchronization remain in place.
- [ ] No schema/migration or unsafe historical inference is introduced.
- [ ] Focused and relevant broader tests pass.
- [ ] `git diff --check` passes.
