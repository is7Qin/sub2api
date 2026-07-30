# Issue #24 PR4: Targeted Recovery and Final Candidate Retention — SDD Plan

> **Base:** merged PR3 on `origin/dev` (`5b05dada6`).
>
> **PR boundary:** add bounded account-recovery policy, one logical-request account-transition budget, and safe final-error candidate retention for existing failover paths. Preserve PR1–PR3 direct-return semantics and all existing provider-local retry, accounting, sticky-session, scheduling, and disconnect behavior.

## Objective

The current failover loops retain only `LastFailoverErr`. Each later attempt overwrites it, so a weaker transport/status error can replace an earlier structured account error. When account selection is finally exhausted, the client may receive generic exhaustion or a weaker error instead of the most authoritative safe semantic candidate.

PR4 makes recovery state explicit:

```text
bounded upstream fact / compatibility error
  -> recovery policy (retry, transition budget, health action)
  -> retain safe candidate
  -> bounded recovery attempt
  -> later success clears candidate
  -> exhaustion renders best candidate, otherwise generic fallback
```

The existing configured `max_account_switches` remains a global hard cap. It is not a semantic policy multiplier or default recovery budget.

## Scope and non-goals

### Included

- Add a service-neutral safe `UpstreamErrorCandidate` containing only bounded client presentation fields and a precedence rank.
- Add a request-scoped recovery state that tracks candidate precedence and a semantic account-transition budget.
- Keep same-account retry counters separate from account-transition counters.
- Apply one transition as the normal maximum for account-scoped recoverable failures.
- Preserve structured candidates over later status-only or generic transport candidates.
- Clear candidates after a successful logical request.
- Keep account-health actions separate from client presentation and avoid duplicate health mutation.
- Migrate the generic gateway failover loop and the OpenAI HTTP Responses/Messages failover loops to the shared candidate/budget behavior where their existing error contracts permit it.
- Keep PR3 Responses WebSocket terminal ownership/lifecycle unchanged; only consume the established typed direct-error contract for generic recovery paths that still reach failover.

### Excluded

- PR5 unknown-only database passthrough fallback or rule-service precedence changes.
- PR6 cleanup of legacy synthetic status/body bridges.
- Removal of `UpstreamFailoverError`.
- A provider-wide retry/failover rewrite.
- New schema or database migrations.
- Changes to successful streaming hot paths, billing/usage pricing, sticky-session binding, client-disconnect drain, or provider-local deterministic request repair.
- Treating every HTTP 5xx/503 as account-scoped; built-in provider-wide overload remains direct return with zero recovery budget.

## Contracts

### Safe candidate

Add to `internal/service`:

```go
type UpstreamCandidateRank uint8

const (
    UpstreamCandidateGenericTransport UpstreamCandidateRank = iota
    UpstreamCandidateStatusOnly
    UpstreamCandidateStructured
)

type UpstreamErrorCandidate struct {
    Presentation UpstreamClientPresentation
    Rank         UpstreamCandidateRank
}
```

A candidate contains no raw response body, headers, request body, credentials, cookies, URLs, stack traces, or unbounded metadata. It is created from an attached bounded `UpstreamErrorFact` when available. A legacy `UpstreamFailoverError` without a fact may produce only a status-only candidate from its observed `StatusCode`; its `ResponseBody` is never used as final client output.

Precedence is deterministic:

1. structured semantic candidate;
2. actual status-only candidate;
3. generic transport candidate;
4. generic 502 only when no safe candidate exists.

Equal-rank candidates use the most recent candidate. The candidate presentation is copied so later mutation of a compatibility error cannot alter retained state.

### Recovery policy

Do not repurpose `ErrorPolicyResult`; it remains the account scheduling/health result. Add a separate recovery policy contract:

```go
type UpstreamAccountHealthAction string

const (
    UpstreamHealthNone                  UpstreamAccountHealthAction = "none"
    UpstreamHealthRecordOnly            UpstreamAccountHealthAction = "record_only"
    UpstreamHealthApplyRateLimit        UpstreamAccountHealthAction = "apply_rate_limit"
    UpstreamHealthTemporarilyUnschedule UpstreamAccountHealthAction = "temporarily_unschedule"
    UpstreamHealthPermanentlyDisable    UpstreamAccountHealthAction = "permanently_disable"
    UpstreamHealthApplyModelRateLimit   UpstreamAccountHealthAction = "apply_model_rate_limit"
)

type UpstreamRecoveryPolicy struct {
    Disposition             UpstreamAttemptDisposition
    SameAccountRetryBudget  int
    AccountTransitionBudget int
    AccountHealthAction     UpstreamAccountHealthAction
    CandidateRank           UpstreamCandidateRank
    Presentation             UpstreamClientPresentation
}
```

The existing `RecognizedUpstreamErrorPolicy` remains compatible for direct-return callers. Built-in recognized direct policies continue to have zero retry/transition budget and no health action. Unknown database matching remains outside this resolver and is unchanged in PR4.

Policy resolution is pure: it may classify bounded facts and existing compatibility metadata, but it must not query the database or mutate account state. Provider-wide `server_is_overloaded`, cyber policy, context-window, and other PR2/PR3 direct semantics must resolve before legacy failover handling.

### Recovery state and budget

Extend `FailoverState` or introduce a small shared handler state used by both generic and OpenAI loops. The state must live for the whole logical request, not for an account-selection iteration:

```go
type UpstreamRecoveryState struct {
    TransitionBudget int
    TransitionsUsed  int
    BestCandidate    *service.UpstreamErrorCandidate
}
```

Required operations:

```go
RetainCandidate(candidate *service.UpstreamErrorCandidate)
AdoptPolicy(policy service.UpstreamRecoveryPolicy)
CanTransition(configuredHardCap int) bool
RecordTransition()
ClearOnSuccess()
FinalCandidate() (*service.UpstreamErrorCandidate, bool)
```

Rules:

- A semantic budget is request-wide and never resets on same-account retry, account selection, another failed account, or Messages fallback-group entry.
- A later policy may narrow remaining budget but may not add budget. Two account-scoped failures therefore still allow at most one account transition.
- `max_account_switches` is an independent hard cap: actual transitions are bounded by both budgets.
- Same-account retries do not consume transition budget.
- An actual account transition increments both semantic usage and the existing switch metric/count exactly once.
- A successful logical request clears the candidate before returning success.
- A real downstream output or an already-owned terminal prevents account switching; candidate retention must not create a second protocol terminal.
- Provider-wide overload and other recognized direct errors never enter this state.

### Initial policy classes

PR4 only adds recovery semantics for explicit, bounded account-scoped/provider-scoped signals already available at a service boundary:

| Class | Same-account retry | Account transitions | Health action | Candidate |
|---|---:|---:|---|---|
| Request/context/policy rejection | 0 | 0 | none | direct safe presentation |
| Provider-wide `server_is_overloaded` | 0 | 0 | none | direct safe 503 |
| Explicit account token invalid/revoked | 0 | 1 | existing auth unschedule/disable action | structured auth |
| Account billing/workspace unavailable | 0 | 1 | existing configured account action | structured billing |
| Account quota/rate limit with explicit provider signal | at most 1 where retry-after semantics support it | 1 | existing rate-limit action | structured quota |
| Stable project/configuration error | at most 1 only where existing provider code marks it | 1 | existing provider action | structured configuration |
| Generic provider 5xx | 0 | 1 | record-only by default | status-only |
| Durable proxy/DNS/TCP fault | 0 | 1 | existing service-side temporary unschedule only | generic/status |
| Unknown | existing compatibility behavior | no new PR4 database semantics | existing behavior | no new semantic policy |

The resolver must be conservative when scope is unknown. It must not infer account scope solely from a generic status code. Existing provider-local request repair remains separate and does not consume account-transition budget.

## Data flow and integration boundaries

### Generic gateway

Files:

- `internal/handler/failover_loop.go`
- `internal/handler/gateway_handler.go`

Keep `FailedAccountIDs`, per-account retry counters, `ForceCacheBilling`, cancellation checks, temporary unscheduling, and Antigravity single-account backoff. Add candidate retention and request-wide budget checks around the existing failover action. `LastFailoverErr` remains a compatibility fallback for unmigrated callers only; it is not the semantic final authority once a candidate exists.

At selection exhaustion, render the retained candidate through the existing safe handler presentation path. Never render from `ResponseBody`. Preserve the existing stream-written guard and protocol terminal behavior.

### OpenAI HTTP Responses and Messages

File:

- `internal/handler/openai_gateway_handler.go`

Use shared candidate/budget state for both duplicated loops while retaining local account exclusion maps and OpenAI-specific behavior:

- `ReportOpenAIAccountScheduleResult`;
- account pool same-account retry counters;
- OAuth 429 storm protection;
- partial usage and image-result handling;
- sticky-session binding and cache billing;
- fallback-group routing behavior.

The semantic state is request-wide. Group-local account exclusions must not be confused with it. A success calls `ClearOnSuccess`; a later generic failure cannot replace an earlier structured candidate.

### Responses WebSocket

Do not redesign PR3 relay/terminal lifecycle. Preserve direct typed errors, delivery state, terminal ownership, duplicate suppression, disconnect drain, and usage behavior. Only generic recoverable errors that still enter the existing failover loop may retain a candidate or consume the request transition budget.

### Service/account health

Files to inspect and minimally adapt:

- `internal/service/gateway_service.go`
- `internal/service/upstream_error_policy.go`
- `internal/service/openai_gateway_service.go`
- `internal/service/openai_upstream_transport_error.go`
- `internal/service/antigravity_gateway_service.go`

Use attached facts where already available. Keep durable OpenAI transport unscheduling in its current service path and prevent handler-level duplicate unscheduling. `skip_monitoring` and existing `ErrorPolicyResult` precedence remain unchanged.

## RED test matrix

Production changes begin only after the corresponding tests fail for the intended reason.

### 1. Pure policy tests (`internal/service/upstream_error_policy_test.go`)

- Provider-wide overload is direct, zero retry/transition, no health action, safe 503.
- Account-scoped quota is recoverable with at most one transition and its explicit health action.
- Explicit revoked-token/auth and billing/configuration signals allow one transition with their existing health action.
- Generic provider 5xx allows one transition with record-only health.
- Durable transport allows one transition while preserving service-side temporary unschedule ownership.
- Unknown facts remain outside new database semantics.
- Existing `ErrorPolicyResult` behavior is unchanged.

### 2. Candidate and state tests (`internal/handler/failover_loop_test.go` or focused recovery-state test)

- Structured candidate beats later generic transport.
- Structured candidate beats later status-only candidate.
- Status-only candidate beats generic transport.
- Equal-rank candidates use the latest candidate.
- Success clears all candidates.
- Candidate stores only safe presentation fields and cannot expose raw body/headers/secrets.
- Same-account retry does not consume transition budget.
- Two account-scoped failures permit at most one transition.
- Configured hard cap zero prevents transition.
- Semantic budget is not multiplied by failed-account count.
- Provider-wide overload does not mutate failover state.

### 3. Generic gateway integration

Targets:

- `internal/handler/gateway_handler_stream_failover_test.go`
- existing gateway failover tests

Scenarios:

- account quota switches once and final response preserves quota semantics;
- durable transport unschedules only the failed account and switches at most once;
- later success suppresses earlier candidate;
- later generic transport does not replace earlier structured candidate;
- real output prevents switching and still emits only one valid terminal.

### 4. OpenAI HTTP integration

Target: `internal/handler/openai_gateway_handler_test.go` and relevant OpenAI transport tests.

Scenarios:

- Responses account quota switches once and succeeds on the second account;
- Messages exhaustion preserves structured quota semantics;
- provider-wide overload remains one attempt with zero switch/health mutation;
- structured candidate survives later generic transport failure;
- durable transport switches at most once without duplicate unschedule;
- Messages fallback-group entry does not reset request budget.

### 5. Compatibility gates

Existing tests must remain green for:

- PR2/PR3 recognized direct errors;
- generic WebSocket failover and terminal ownership;
- request repair and provider-local retries;
- sticky-session/cache billing;
- usage submission on partial/pre-output failures;
- cancellation and downstream disconnect;
- Antigravity single-account backoff;
- OpenAI OAuth 429 protection;
- unknown passthrough behavior.

## Implementation order

1. Add pure candidate/rank and recovery-policy contracts; add RED unit tests.
2. Add request-scoped recovery state and candidate ranking/clearing; run focused RED/GREEN tests.
3. Add compatibility candidate extraction from bounded facts/status-only errors without raw-body rendering.
4. Integrate generic gateway failover state while preserving existing account-health and retry actions.
5. Integrate OpenAI Responses/Messages loops, preserving duplicated provider-specific behavior and request-wide state.
6. Add targeted integration regressions for one transition, candidate precedence, later success, and no-switch-after-output.
7. Run formatting, targeted packages, race tests where relevant, and broader baseline comparisons.

## Verification commands

```bash
go test ./internal/service -run 'Test(ResolveUpstream|RecognizeUpstream|.*Transport.*Recovery)'
go test ./internal/handler -run 'Test(UpstreamRecovery|FailoverState|Gateway.*Failover|OpenAI.*(Recovery|Failover))'
go test ./internal/service/openai_ws_v2
go test -race ./internal/handler -run 'Test(UpstreamRecovery|FailoverState)'
go test ./internal/handler
go test ./internal/service
go vet ./internal/service ./internal/handler
gofmt -w <changed-go-files>
git diff --check
```

The known broader `internal/service` WSv2 baseline failures must be compared against `origin/dev`; they are not PR4 regressions without before/after evidence.

## Review checklist

- [ ] Direct recognized policy still bypasses failover, account exclusion, health mutation, and candidate retention.
- [ ] Provider-wide overload never consumes a transition.
- [ ] Candidate contains safe presentation only; raw body/header data is never rendered.
- [ ] Structured > status-only > generic precedence is explicit and equal ranks are latest-wins.
- [ ] Success clears candidate state.
- [ ] Semantic transition budget is request-wide and never multiplied by account or loop count.
- [ ] `max_account_switches` remains an independent hard cap.
- [ ] Same-account retries remain separately bounded and do not consume transitions.
- [ ] Real output/terminal ownership still prevents switching and duplicate protocol errors.
- [ ] Account-health actions remain separate from client presentation and are not applied twice.
- [ ] Unknown database fallback remains PR5 scope.
- [ ] PR2/PR3, usage, billing, sticky-session, disconnect, and provider-local retry compatibility tests remain green.
