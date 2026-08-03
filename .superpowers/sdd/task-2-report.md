# Task 2 implementation report

## Status
Completed locally; no push performed.

## TDD evidence

### RED
Required command was run first:

```text
cd backend && go test ./internal/repository ./internal/service -run 'TestScheduler(Cache_EmptyPublishedSnapshotIsAHit|Cache_StaticStateReadersShareActiveVersion|SnapshotService_RebuildPublishesPersistentSupport|.*DefaultBuckets)' -count=1
```

It could not reach the new test failures because the baseline service package fails to compile from unrelated missing symbols:

```text
internal/service/gemini_messages_compat_service_test.go:1960:11: undefined: geminiErrorPolicyRepo
internal/service/openai_gateway_service_test.go:1707:17: undefined: newMockSettingRepo
```

A narrowed RED command then verified the intended missing-feature failure before production implementation:

```text
cd backend && go test -tags unit ./internal/repository -run 'TestSchedulerCache(EmptyPublishedSnapshotIsAHit|StaticStateReadersShareActiveVersion|IncompleteStaticStateDoesNotHitPersistentSupport)' -count=1
```

Expected failure observed:

```text
cache.SetStaticState undefined
cache.GetPersistentSupport undefined
```

The first implementation run exposed a real empty-state defect (`TestSchedulerCacheEmptyPublishedSnapshotIsAHit`: expected hit=true); it was fixed by using the version-coherent support-state marker to distinguish an intentionally published empty static state from legacy empty SetSnapshot behavior.

## Follow-up fixes from Task 2 re-review

### RED
Added focused regressions before fixes:

- `TestSchedulerSnapshotService_MixedRebuildFiltersPersistentSupport` failed with disabled Antigravity account ID 3 retained in support.
- `TestSchedulerSnapshotService_DefaultBucketsMergeRegistryWithoutGroupRepository` failed because nonempty registry returned alone and omitted global OpenAI default.
- `TestSchedulerCache_StaticStatePartialWriteKeepsPreviousVersion` was strengthened to reuse account ID 20 with distinguishable old/new names and model mappings, failing under mutable global payload publication.
- `TestSchedulerCache_StaticStateActivationUsesNewPayloadAndExpiresOldPayload` initially failed because old version payload had no grace TTL.

### Fixes
- `loadPersistentSupportForRebuild` now applies the same `IsMixedSchedulingEnabled()` filter to Antigravity rows as candidate loading for mixed buckets.
- `rebuildBucketsForStartup` always merges registry with `defaultBuckets`; active groups are still added by `defaultBuckets` when a group repository exists, and discovery errors still propagate.
- Static-state account and metadata payloads are written under bucket/version-qualified keys. Static readers resolve payloads through the active version, while legacy `SetSnapshot` continues using mutable global account keys and its existing behavior. Activation applies the 60-second reader-grace TTL to old version payload keys along with old candidate/support ZSETs.

This is additive schema migration: existing global `sched:acc:{id}` and `sched:meta:{id}` keys remain for legacy snapshots and single-account APIs; only static-state publication/read paths use the versioned keys. No migration job is required, and old readers continue seeing legacy keys until upgraded.

## GREEN
Focused unit suite:

```text
cd backend && go test -tags unit ./internal/repository ./internal/service -run 'TestScheduler(Cache|SnapshotService|.*DefaultBuckets)' -count=1
ok github.com/Wei-Shaw/sub2api/internal/repository
ok github.com/Wei-Shaw/sub2api/internal/service
```

Race suite:

```text
cd backend && go test -race -tags unit ./internal/repository ./internal/service -run 'TestScheduler(Cache|SnapshotService)' -count=1
ok github.com/Wei-Shaw/sub2api/internal/repository
ok github.com/Wei-Shaw/sub2api/internal/service
```

`git diff --check` passed.

## Changed files

- `backend/internal/service/scheduler_snapshot_service.go`
  - Filters Antigravity persistent-support rows in mixed buckets using the same mixed scheduling gate as candidate loading.
  - Merges registered buckets with global defaults even without `groupRepo`.
- `backend/internal/repository/scheduler_cache.go`
  - Adds version-qualified static full/meta payload keys and resolves static candidate/support reads through active bucket version.
  - Applies bounded grace TTL to old version payload keys and membership keys after activation.
  - Preserves legacy SetSnapshot global payload semantics.
- `backend/internal/repository/scheduler_cache_unit_test.go`
  - Adds same-ID failed replacement immutability regression and successful activation/grace cleanup regression.
- `backend/internal/service/scheduler_snapshot_static_state_test.go`
  - Adds mixed persistent-support filtering and registry-plus-global-default discovery regressions.

## Atomicity and compatibility

SetStaticState materializes candidate/support memberships and version-qualified payloads before one activation transaction. A failed support membership write cannot alter the active version or payloads. Readers use active version membership and matching version payloads, retaining old state for reader grace. Legacy SetSnapshot remains intentionally separate: it writes global payload keys and preserves its prior semantics for third-party/legacy paths.

## Final static-payload cleanup review fix

### RED

Added focused repository regressions before the production fix:

- `TestSchedulerCache_StaticStateAmbiguousPayloadPipelineFailureCleansUnpublishedPayloads` uses a Redis pipeline hook that returns an error only after Redis has accepted the first payload pipeline. Before the fix, version `2` full/meta payloads for IDs `41` and `42` remained with no TTL while active state correctly remained at version `1`.
- `TestSchedulerCache_StaticStateStaleActivationCleansUnpublishedPayloads` causes a separate cache instance to publish complete version `2` state after the first writer has materialized version `1`, but before it activates. Before the fix, the Lua script returned `0`, `SetStaticState` incorrectly returned success, and version `1` candidate/support ZSET and full/meta payload keys remained indefinitely.

RED command and observed failures:

```text
cd backend && go test -tags unit ./internal/repository -run 'TestSchedulerCache_StaticState(AmbiguousPayloadPipelineFailureCleansUnpublishedPayloads|StaleActivationCleansUnpublishedPayloads)' -count=1

--- FAIL: TestSchedulerCache_StaticStateAmbiguousPayloadPipelineFailureCleansUnpublishedPayloads
    scheduler_cache_unit_test.go:593: Should be zero, but was 2
--- FAIL: TestSchedulerCache_StaticStateStaleActivationCleansUnpublishedPayloads
    scheduler_cache_unit_test.go:612: An error is expected but got nil.
```

### GREEN

- `SetStaticState` now computes the complete candidate/support payload ID cleanup set before any writes. On every unsuccessful path (payload pipeline, candidate ZSET, support ZSET, stale activation result, or activation error), it removes the unpublished candidate/support ZSETs and all version-qualified full/meta payloads that may have been written.
- New versioned full/meta payload writes begin with a five-minute bounded TTL. This is a defensive fallback if Redis has processed writes but an error makes the outcome ambiguous and best-effort cleanup cannot complete. A successful static-state activation atomically `PERSIST`s the active version's payloads, so the active static state retains its existing indefinite lifetime.
- `activateStaticStateVersion` now returns the Lua integer result. A stale result (`0`) is treated as failed publication, causes cleanup, and returns `static state version was superseded` without changing the newer active state.
- Cleanup checks whether the candidate version became active before deleting. This preserves a state that was successfully activated even if the client lost the activation response after the Lua transaction committed.
- Legacy/global `sched:acc:{id}` and `sched:meta:{id}` behavior is unchanged; only `sched:acc:v:*` and `sched:meta:v:*` static-state payloads use the new safety TTL and cleanup path.

### Verification

```text
cd backend && go test -tags unit ./internal/repository -run 'TestSchedulerCache_StaticState(AmbiguousPayloadPipelineFailureCleansUnpublishedPayloads|StaleActivationCleansUnpublishedPayloads|PartialWriteKeepsPreviousVersion|ActivationUsesNewPayloadAndExpiresOldPayload)' -count=1
ok github.com/Wei-Shaw/sub2api/internal/repository

cd backend && go test -tags unit ./internal/repository ./internal/service -run 'TestScheduler(Cache|SnapshotService|.*DefaultBuckets)' -count=1
ok github.com/Wei-Shaw/sub2api/internal/repository
ok github.com/Wei-Shaw/sub2api/internal/service

cd backend && go test -race -tags unit ./internal/repository ./internal/service -run 'TestScheduler(Cache|SnapshotService)' -count=1
ok github.com/Wei-Shaw/sub2api/internal/repository
ok github.com/Wei-Shaw/sub2api/internal/service
```

`git diff --check` passed.

## Activation response-loss cleanup race fix

### RED

Added `TestSchedulerCache_StaticStateLostActivationResponsePreservesReaderGrace` before changing production code. The hook returns an error only after V1's `EVAL` activation has committed; while handling that response loss it publishes V2 using a second client. This reproduces the hazardous ordering: V2's activation gives committed V1 its reader grace, then V1's caller runs error cleanup while `active=V2`.

Initial focused RED command:

```text
cd backend && go test -tags unit ./internal/repository -run 'TestSchedulerCache_StaticStateLostActivationResponsePreservesReaderGrace' -count=1
```

Observed expected failure before the fix:

```text
scheduler_cache_unit_test.go:637: expected 1m0s, actual -2ns
```

V1's candidate/support ZSET and version-qualified full/meta payload keys had been deleted by error cleanup even though V2 had already switched away from V1.

### GREEN

- `SetStaticState` now distinguishes a definite stale Lua result (`published == false`) from an activation transport error. The former remains destructively cleaned because Redis definitively rejected the version.
- On an activation transport error, `boundAmbiguousStaticState` never deletes version artifacts. If the uncertain version is still active, it leaves its durable lifetime unchanged. If it is no longer active, it applies the existing five-minute unpublished-artifact TTL to candidate/support ZSETs and every version-qualified payload.
- The five-minute bound is longer than the 60-second reader grace, so it cannot shorten TTLs granted by a later successful V2 activation. It also bounds a truly unpublished activation whose client cannot determine whether the Lua transaction executed.
- The regression asserts V2 reads correctly and every V1 candidate/support/payload artifact has TTL between the 60-second reader grace and the five-minute bounded-cleanup window.

### Verification

```text
cd backend && go test -tags unit ./internal/repository -run 'TestSchedulerCache_StaticState(AmbiguousPayloadPipelineFailureCleansUnpublishedPayloads|StaleActivationCleansUnpublishedPayloads|PartialWriteKeepsPreviousVersion|ActivationUsesNewPayloadAndExpiresOldPayload|LostActivationResponsePreservesReaderGrace)' -count=1
ok github.com/Wei-Shaw/sub2api/internal/repository

cd backend && go test -tags unit ./internal/repository ./internal/service -run 'TestScheduler(Cache|SnapshotService|.*DefaultBuckets)' -count=1
ok github.com/Wei-Shaw/sub2api/internal/repository
ok github.com/Wei-Shaw/sub2api/internal/service

cd backend && go test -race -tags unit ./internal/repository ./internal/service -run 'TestScheduler(Cache|SnapshotService)' -count=1
ok github.com/Wei-Shaw/sub2api/internal/repository
ok github.com/Wei-Shaw/sub2api/internal/service
```

## Candidate/support payload namespace isolation review fix

### RED

Added `TestSchedulerCache_StaticStateSameIDReadersUseIndependentPayloads` before changing production code. It publishes candidate and persistent-support records with the same account ID, but only the candidate has a future `RateLimitResetAt`.

```text
cd backend && go test -tags unit ./internal/repository -run 'TestSchedulerCache_StaticStateSameIDReadersUseIndependentPayloads' -count=1
```

Before the fix, the candidate payload was overwritten by the persistent-support projection. The assertion dereferenced a nil candidate `RateLimitResetAt`, producing the expected failure:

```text
panic: runtime error: invalid memory address or nil pointer dereference
```

### GREEN

- Candidate full/meta payloads remain under `sched:acc:v:*` and `sched:meta:v:*`; persistent-support full/meta payloads now use independent `sched:support:acc:v:*` and `sched:support:meta:v:*` namespaces.
- `GetSnapshot` continues resolving candidate membership through candidate payload keys. `GetPersistentSupport` resolves support membership only through support payload keys. Both still share the one atomic active version.
- The activation Lua transaction now persists and applies reader-grace TTL separately for each namespace. Unpublished cleanup and ambiguous activation bounds enumerate both candidate and support key families, including overlap IDs without cross-namespace deletion.
- Expanded activation, response-loss, partial-pipeline, and stale-activation cleanup assertions so both namespaces are grace-safe and bounded.
- Legacy global `sched:acc:{id}` / `sched:meta:{id}` publication and readers remain unchanged.

### Verification

```text
cd backend && go test -tags unit ./internal/repository -run 'TestSchedulerCache_StaticState' -count=1
ok github.com/Wei-Shaw/sub2api/internal/repository

cd backend && go test -tags unit ./internal/repository ./internal/service -run 'TestScheduler(Cache|SnapshotService|.*DefaultBuckets)' -count=1
ok github.com/Wei-Shaw/sub2api/internal/repository
ok github.com/Wei-Shaw/sub2api/internal/service

cd backend && go test -race -tags unit ./internal/repository ./internal/service -run 'TestScheduler(Cache|SnapshotService)' -count=1
ok github.com/Wei-Shaw/sub2api/internal/repository
ok github.com/Wei-Shaw/sub2api/internal/service

git diff --check
# passed
```

## Static marker transition reader-race fix

### RED

Added `TestSchedulerCache_GetSnapshotRetriesStaticStateTransitionBeforeChoosingPayload` before changing `GetSnapshot`. A deterministic Redis `GET` hook reads V1 as active, publishes V2 before the support-state marker is read, and provides a stale legacy global payload for V1's member ID.

```text
cd backend && go test -tags=unit ./internal/repository -run '^TestSchedulerCache_GetSnapshotRetriesStaticStateTransitionBeforeChoosingPayload$' -count=1
```

The test failed as intended before the fix: the reader returned V1 membership (`70`) after choosing the global payload path, instead of coherently retrying to V2 (`71`).

### GREEN

- `GetSnapshot` now reads the active version and support-state marker as a bounded coherent decision (two attempts).
- A missing marker remains the legacy `SetSnapshot` case and continues to use global payload keys, including historical empty-snapshot behavior.
- A present-but-mismatched marker means a static activation transition was crossed. The reader retries; if it cannot obtain a coherent pair, it fails closed as a cache miss rather than treating a grace-period static version as legacy data.
- A matching pair continues to use version-qualified candidate payloads, preserving immutable old-reader grace and empty static snapshot hits.

### Verification

```text
cd backend && go test -tags=unit ./internal/repository -run '^TestSchedulerCache_GetSnapshotRetriesStaticStateTransitionBeforeChoosingPayload$' -count=1
ok github.com/Wei-Shaw/sub2api/internal/repository

cd backend && go test -tags=unit ./internal/repository -count=1
ok github.com/Wei-Shaw/sub2api/internal/repository

cd backend && go test -race -tags=unit ./internal/repository -run 'TestSchedulerCache_(GetSnapshotRetriesStaticStateTransitionBeforeChoosingPayload|StaticState)' -count=1
ok github.com/Wei-Shaw/sub2api/internal/repository
```
