# Worker Runtime Inventory

This inventory records all process-local background activity as of Issue #9 Phase 1.
`managed` means the component is registered in `workerruntime.Runtime` and appears in
`GET /api/v1/admin/ops/workers/status`; `unmanaged` components retain their existing
startup and shutdown behavior until a later phase. The status endpoint intentionally
reports no process identifier, credentials, payloads, stack traces, or raw upstream data.

| Component | Managed | Startup location | Scheduler / executor | Lifecycle owner | Stop wait guarantee | Coordination mode | Health / status surface | Target phase |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Account expiry | Yes | `cmd/server/provideWorkerRuntime` | `workerruntime.PeriodicJob` | Server runtime | `Runtime.StopAll` waits to deadline and reports timeout | per-instance | Ops worker status | Phase 1 |
| Idempotency cleanup | Yes | `cmd/server/provideWorkerRuntime` | `workerruntime.PeriodicJob` | Server runtime | `Runtime.StopAll` waits to deadline and reports timeout | per-instance | Ops worker status | Phase 1 |
| Usage record pool | Yes | `cmd/server/provideWorkerRuntime` | `workerruntime` pool adapter | Server runtime | `Runtime.StopAll` waits to deadline and reports timeout | per-instance | Ops worker status | Phase 1 |
| TimingWheel | No | `service.ProvideTimingWheelService` | go-zero timing wheel | Service provider / cleanup | `TimingWheelService.Stop` has no explicit external wait contract | per-process | Logs only | Later migration |
| Deferred | No | `service.ProvideDeferredService` | TimingWheel recurring callback | Service provider | Cancels timer then flushes synchronously; no global deadline contract | per-instance | Logs only | Later migration |
| Concurrency cleanup | No | `service.ProvideConcurrencyService` | ticker goroutine | Service provider | No stop method; process exit | per-instance | Ops concurrency endpoint | Later migration |
| UserMessageQueue cleanup | No | `service.ProvideUserMessageQueueService` | ticker goroutine | Service provider | `Stop` cancels; no join guarantee | per-instance | None | Later migration |
| DashboardAggregation | No | `service.ProvideDashboardAggregationService` | TimingWheel recurring callback plus async backfill | Service provider | No dedicated stop/cancel for scheduled work | singleton per run when leader lock configured | Admin dashboard / logs | Later migration |
| UsageCleanup | No | `service.ProvideUsageCleanupService` | TimingWheel recurring callback | Service provider | Cancels worker context and timer; no explicit join guarantee | per-instance | Admin usage cleanup tasks / logs | Later migration |
| Ops jobs (metrics, aggregation, alerts, cleanup, reports, system-log sink) | No | service providers and server cleanup | service-specific tickers / cron / queues | Service providers | Cleanup invokes `Stop`; guarantee is component-specific | mixed; some singleton via locks | Ops endpoints and logs | Later migration |
| BillingCache pool | No | `service.ProvideBillingCacheService` / lazy pool use | bounded async work pool | Service provider | `BillingCacheService.Stop` closes pool; component-specific wait | per-instance | Cache metrics / logs | Later migration |
| Billing outbox | No | `service.ProvideBillingOutboxWorker` | ticker poller with durable claims | Service provider | `Stop` cancels and waits for its `WaitGroup` | durable claim | `GET /api/v1/admin/ops/billing-outbox/health` | Later migration |
| Auth-cache outbox | No | `service.ProvideAuthCacheInvalidationWorker` | ticker poller with durable claims | Service provider | `Stop` cancels and waits for its `WaitGroup` | durable claim | Logs only | Later migration |

## Process-local status contract

The authenticated, monitoring-gated worker endpoint is
`GET /api/v1/admin/ops/workers/status`. Its successful `data` value is exactly
`{"scope":"process","workers":[...]}`. The worker list is a point-in-time snapshot
of the local runtime registry, not a fleet view; no instance identity is emitted until a
stable non-secret process identity exists.
