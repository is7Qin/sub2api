# Task 3 — Scheduler cache-only verification cleanup

## Scope completed

- Renamed stale scheduler cache refresh helpers:
  - `refreshSelectedOpenAIAccountFromSchedulerCache`
  - `refreshOpenAICandidatesFromSchedulerCache`
  - `recheckSelectedOpenAIAccountFromSchedulerCache`
- Updated every call site and removed comments that described request-path database refresh/fallback.
- Removed obsolete request DB-fallback/singleflight tests and replaced them with cache-only behavior tests:
  - a snapshot miss returns `ErrSchedulerCacheNotReady` without invoking the panic-on-DB repository;
  - a published snapshot returns accounts without DB access;
  - a canceled caller context does not create detached DB fallback work.
- Updated scheduler hydration cache test fixture to implement the required batch candidate metadata API, preserving selected-account hydration from `GetAccount`.
- Fixed scheduler rebuild-backoff test fixture for default bucket merging by implementing the mixed-platform ungrouped repository query and asserting the actual 10 default buckets (11 when a registered group bucket is also merged).
- Removed the obsolete scheduler DB fallback configuration fields/defaults/validation and unused fallback-context helpers. Updated the integration fixture accordingly.

## TDD evidence

### RED

Before migrating stale assertions:

```text
go test -tags=unit ./internal/service -count=1 -run 'TestSchedulerSnapshotFallback|TestOpenAISelectAccountWithLoadAwareness_HydratesSelectedAccountFromSchedulerSnapshot|TestSchedulerCheckOutboxLagBacksOffAfterFailedRebuild'
```

Failed as expected because cache misses now return `scheduler cache not ready`; legacy tests still expected DB singleflight fallback. The hydration test failed with no available account because its cache did not implement batch candidate metadata reads. The backoff test panicked in the embedded mock's unimplemented `ListSchedulableUngroupedByPlatforms` after default bucket merging.

### GREEN

```text
go test -tags=unit ./internal/service -count=1 -run 'TestSchedulerSnapshot(CacheMissReturnsUnavailableWithoutDB|CacheMissUsesCallerContext|ReturnsPublishedAccountsWithoutDB)|TestOpenAISelectAccountWithLoadAwareness_HydratesSelectedAccountFromSchedulerSnapshot|TestRefreshOpenAICandidates_(ReadsFromSnapshotBatch|SnapshotErrorDoesNotQueryDB|NoBatchReaderDoesNotQueryDB|NoSnapshotReturnsNil)'
```

Passed.

## Verification

Passed:

```text
go test -tags=unit ./internal/service -count=1 -run 'TestSchedulerSnapshot(CacheMissReturnsUnavailableWithoutDB|CacheMissUsesCallerContext|ReturnsPublishedAccountsWithoutDB)|TestOpenAISelectAccountWithLoadAwareness_HydratesSelectedAccountFromSchedulerSnapshot|TestSchedulerCheckOutboxLagBacksOffAfterFailedRebuild|TestSchedulerCheckOutboxLagBacklogBacksOffAfterFailedRebuild|TestSchedulerHandleDirtyWork(GlobalBacksOffAfterFailedRebuild|GroupNotBlockedByFullRebuildBackoff)'
go test -tags=unit ./internal/service -count=1 -run 'Scheduler|scheduler'
go test ./cmd/server -count=1
git diff --check
```

The isolated unrelated usage-log timing test passed five consecutive runs:

```text
go test -tags=unit ./internal/service -count=1 -run '^TestWriteUsageLogBestEffort_CreateErrorLoggedNotPropagated$'
```

The complete service suite was run multiple times but remains red on unrelated nondeterministic tests. Latest run failures:

- `TestRunOne_DecryptFailureCanBeRescheduledAfterRepair`: expected repaired monitor to run.
- `TestGatewayService_ClaudeCodeKeepaliveUsesNoopContentDeltaInsideBlock`: expected synthetic empty content delta missing from stream.
- `TestWriteUsageLogBestEffort_CreateErrorLoggedNotPropagated`: condition never satisfied despite the expected worker error log.

No changes were made to those unrelated tests or production paths.

## Self-review

- Confirmed no `DbFallback`, `db_fallback`, `fallbackQueryContext`, or `withFallbackTimeout` references remain in Go source.
- Confirmed stale `*FromDB` scheduler refresh helper names and `TestSchedulerSnapshotFallback*` tests are absent.
- Confirmed `git diff --check` passes.
