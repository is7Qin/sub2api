# Issue #24 PR6: Legacy Error Boundary Cleanup Design

**Date:** 2026-07-31  
**Base:** merged PR5 on `origin/dev` (`63bb740a25fe33b87e1db9dbd6e99e695dcf8134`)  
**Status:** Approved for implementation planning

## Purpose

Close the remaining legacy boundaries that bypass the Issue #24 fact-to-policy-to-presentation design. PR1–PR5 established bounded error facts, built-in direct semantics, WebSocket terminal handling, targeted recovery, and unknown-only database rules. This PR completes the migration at the known HTTP, SSE, ops-monitoring, and failover-exhaustion boundaries without rewriting provider adapters or successful forwarding paths.

## Decisions

1. Built-in direct errors are recognized before account-health, failover, or database-rule side effects.
2. An Anthropic error event carried by upstream HTTP 200 never acquires a synthetic upstream HTTP status.
3. Database rules, including `skip_monitoring`, are consulted only for unknown bounded facts.
4. Client presentation never re-extracts a message from a raw upstream body.
5. An unknown, unmatched error with a real upstream 4xx keeps that 4xx status but receives a fixed safe generic message.
6. An unknown, unmatched upstream 5xx, an error without a real HTTP status, or a transport/read failure receives a generic 502.
7. A retained recovery candidate is semantic input to final presentation, not permission to expose an unknown provider message.
8. Every failover-exhaustion path uses the same deterministic candidate precedence.

Decision 5 intentionally refines the earlier broad “unknown unmatched => generic 502” statement. A real upstream 4xx represents a client request rejection rather than a gateway failure. PR6 preserves that status while withholding unrecognized upstream content.

## Non-goals

- Replacing all existing gateway services or protocol renderers.
- Removing `UpstreamFailoverError` throughout the repository.
- Changing successful HTTP, SSE, or WebSocket hot paths.
- Redesigning provider-local request-repair loops.
- Changing billing, usage accounting, pricing, or scheduler semantics outside the direct-error ordering defect.
- Removing seeded passthrough-rule data. Runtime irrelevance and data cleanup remain separate concerns.
- Adding database schema or admin API fields.

## Architecture

PR6 adds two narrow semantic boundaries while retaining existing provider adapters and renderers.

### Attempt boundary

A provider error is parsed into a bounded `UpstreamErrorFact` before scheduling or account-health side effects:

```text
upstream error
  -> bounded UpstreamErrorFact
  -> built-in recognition
       DirectReturn -> safe presentation -> protocol renderer
       otherwise    -> explicit existing recovery/health processing
```

A direct-return fact does not:

- enter `FailoverState`;
- increment account-switch counters;
- exclude an account;
- call database passthrough matching;
- temporarily unschedule, permanently disable, or rate-limit an account.

The Generic Gateway HTTP path must follow the same ordering already established in the OpenAI-specific HTTP paths.

### Final presentation boundary

A small service-level resolver consumes a bounded final fact and optionally the existing rule service. It returns a finalized safe presentation plus the operational `skip_monitoring` flag:

```text
final bounded fact
  -> built-in recognition
  -> unknown-only database rule
  -> safe unknown fallback
  -> existing protocol renderer
```

Conceptually:

```go
type FinalUpstreamErrorPresentation struct {
    Presentation  UpstreamClientPresentation
    SkipMonitoring bool
}
```

The exact Go name may follow surrounding conventions. The resolver must not receive or return a raw body.

## Unknown fallback policy

For an unknown fact with no usable database rule:

| Fact | Client status | Client message |
|---|---:|---|
| Real upstream 4xx | Preserve the real 4xx | Fixed safe generic request-failure message |
| Real upstream 5xx | 502 | Fixed safe upstream-failure message |
| SSE/WS semantic error with `HTTPStatusKnown=false` | 502 | Fixed safe upstream-failure message |
| Transport/read/stream-termination error | 502 | Fixed safe upstream-failure message |
| Missing or malformed fact | 502 | Fixed safe upstream-failure message |

Unknown provider code, type, message, or metadata is not copied into the no-rule response. Redaction makes a field safer to process; it does not establish that an unknown provider field is suitable for public presentation.

For an unknown fact with a matching rule:

- matching uses platform, a real HTTP status only when `HTTPStatusKnown=true`, and bounded `InternalMatchText`;
- an explicit configured response code may override the default status;
- a configured custom message may override the message;
- otherwise any allowed provider-derived message comes only from bounded `fact.SafeMessage`;
- `skip_monitoring` affects ops logging only.

## Generic HTTP ordering

`GatewayService.handleErrorResponse` parses an HTTP fact immediately after its bounded body read. Built-in recognition occurs before `rateLimitService.HandleUpstreamError` and before creation of any failover carrier.

The new ordering is:

```text
read bounded body
  -> ParseHTTPUpstreamErrorFact
  -> recognize direct semantic
       yes -> set bounded ops context and render directly
       no  -> existing account-health/recovery policy
  -> final presentation resolver where this is a terminal boundary
```

The unmatched HTTP 400 branch must no longer call `c.Data(..., body)`. It returns a protocol-compatible safe 400 envelope with no raw upstream fields.

## Anthropic SSE status semantics

For upstream HTTP 200 plus Anthropic `event:error`:

- the attached fact remains `Source=SSE` and `HTTPStatusKnown=false`;
- an ops event does not record `403`, `502`, or any other invented upstream status;
- the legacy carrier uses status zero/unknown rather than `http.StatusForbidden`;
- recognized semantics retain their existing direct-return behavior;
- unknown semantics may use only recovery explicitly supported without a fabricated status;
- final unknown/no-rule presentation is generic 502.

Compatibility logic that previously keyed on the synthetic 403 must consume the fact or an explicit semantic decision instead.

## Ops monitoring and `skip_monitoring`

`appendOpsUpstreamError` must not perform legacy raw `MatchRule` matching over `Detail` or `Message`.

The rule side effect follows the same precedence as final presentation:

```text
bounded fact available
  -> recognized: do not query rules
  -> unknown: MatchUnknownRule(fact)
  -> matched + SkipMonitoring: set OpsSkipPassthroughKey
```

If a legacy ops event cannot supply enough bounded fact data, construct a fact only from already sanitized and bounded scalar fields. Do not treat an unbounded diagnostic body as `InternalMatchText`.

This change does not alter billing, usage submission, rate-limit actions, scheduler state, or account health.

## Raw-body isolation

Client output must not be derived from:

- `c.Data(..., responseBody)` at generic error boundaries;
- `ExtractUpstreamErrorMessage(responseBody)` after a rule match;
- `UpstreamFailoverError.ResponseBody` in a final renderer;
- an unknown candidate’s provider message without an administrator rule.

`applyErrorPassthroughRule` must use the bounded fact and `fact.SafeMessage` for an allowed matched-rule provider message. Unknown/no-rule output uses the fixed fallback. Raw bodies remain limited to existing internal diagnostic paths and must continue to obey configured truncation and sanitization.

The existing private `SanitizedClientResponse` carrier is not removed in this PR. Its current producers generate explicitly sanitized payloads. It remains a reviewed safe compatibility boundary and must not accept arbitrary upstream bodies.

## Recovery candidates

Recovery state keeps the existing rank order:

```text
structured semantic candidate
  > status-only candidate
  > generic transport candidate
  > no candidate
```

Ties continue to prefer the most recent candidate. A later success clears all candidates.

The final rendering contract changes as follows:

- a structured candidate may supply recognized or recoverable semantic fields;
- a status-only candidate supplies status facts only;
- a generic transport candidate supplies failure class only;
- every candidate still passes through final presentation resolution;
- an unknown status-only candidate does not authorize returning its provider message;
- no safe candidate produces the unknown fallback policy above.

All Generic Gateway, Gemini, Anthropic-compatible Responses, and OpenAI Responses exhaustion paths must consult the retained best candidate rather than unconditionally rendering `LastFailoverErr` or `All available accounts exhausted`.

## Protocol lifecycle

The resolver produces semantic presentation only. Existing protocol writers retain wire ownership:

- uncommitted Anthropic JSON uses the existing Anthropic envelope;
- committed Anthropic SSE writes one `event: error`;
- uncommitted OpenAI/Responses uses the existing JSON envelope;
- committed Responses SSE writes one `response.failed`;
- Chat Completions uses its existing valid SSE error form;
- WebSocket lifecycle remains under the PR3 writer and close-state logic.

No final resolver writes directly to `gin.Context`. No handler may append a second terminal event after an existing writer owns terminal output.

## Components and expected changes

### `internal/service/upstream_error_policy.go`

- Define or host the safe unknown fallback status policy.
- Keep recognition and unknown fallback separate.
- Preserve provider code/type/message only for recognized or explicitly rule-approved output.

### `internal/service/error_passthrough_runtime.go`

- Resolve from a bounded fact.
- Replace raw `ExtractUpstreamErrorMessage(responseBody)` client output with `fact.SafeMessage` for matched rules.
- Return the safe unknown fallback when the caller is migrated to the final resolver.

### `internal/service/gateway_service.go`

- Recognize Generic HTTP facts before account-health handling.
- Remove raw HTTP 400 body output.
- Remove Anthropic SSE synthetic 403 from the carrier and ops event.

### `internal/service/ops_upstream_context.go`

- Replace legacy `MatchRule` monitoring side effects with bounded fact-aware unknown-only matching.

### `internal/handler/failover_loop.go`

- Preserve candidate rank, but retain enough bounded fact semantics for final resolution.
- Do not treat a status-only candidate as finalized public presentation.

### Handler exhaustion paths

Migrate the known Generic Gateway, Gemini, Anthropic-compatible Responses, and OpenAI Responses branches to:

```text
best retained candidate/fact
  -> final presentation resolver
  -> existing protocol renderer
```

Remove `All available accounts exhausted` only where an upstream-error candidate or fact exists. Pure routing failure before any upstream attempt may retain its existing no-available-account response.

## Test-first scenarios

### Generic HTTP direct errors

1. Recognized `server_is_overloaded` returns safe 503 before any health mutation, failover, or database query.
2. Recognized `cyber_policy` and context-length errors return safe 400 with no account-state mutation.
3. An unknown HTTP 400 without a rule returns safe 400 and does not include the raw body, secret fields, or debug metadata.
4. An unknown HTTP 503 without a rule returns generic 502 and does not expose the provider message.
5. An unknown HTTP error with a matching rule uses only the rule-approved safe presentation.

### Anthropic SSE

1. HTTP 200 plus unknown `event:error` produces a fact with `HTTPStatusKnown=false`.
2. Its ops event does not contain a synthetic 403 or 502.
3. Its legacy carrier does not claim a real HTTP status.
4. Unknown/no-rule exhaustion returns generic 502.
5. Recognized overload remains direct and emits exactly one valid terminal event when output is committed.

### Ops rules

1. A recognized overload plus a conflicting `skip_monitoring=true` rule does not query or apply the rule.
2. An unknown bounded fact can still set `OpsSkipPassthroughKey` through a matching rule.
3. `skip_monitoring` does not suppress usage, billing, health, scheduler, or rate-limit side effects.
4. Rule keyword matching never receives a raw diagnostic body.

### Raw-body safety

1. Unknown HTTP 400 containing access tokens, URLs, stack data, session/thread/installation IDs, or unrelated metadata exposes none of them.
2. Matched `PassthroughBody` output uses bounded/redacted `fact.SafeMessage`.
3. An oversized message is bounded before presentation.
4. A malformed body produces a fixed safe fallback.

### Candidate and exhaustion behavior

1. A structured recoverable candidate followed by a weaker transport error remains the best final candidate.
2. An unknown status-only candidate without a rule cannot bypass the safe fallback policy.
3. Error exhaustion and account-selection exhaustion produce the same candidate choice.
4. Generic Gateway, Gemini, Anthropic-compatible Responses, and OpenAI Responses use the shared final-resolution semantics.
5. A later successful attempt clears earlier candidates.
6. A pure routing failure with no upstream attempt retains the existing no-available-account behavior.

### Protocol lifecycle

1. Uncommitted errors use the correct JSON status and envelope.
2. Committed Anthropic SSE emits one `event: error`.
3. Committed Responses SSE emits one `response.failed`.
4. Existing terminal ownership prevents duplicate generic fallback output.
5. Successful streaming events do not enter the error parser or final resolver.

## Verification

Run focused tests first, then relevant packages:

```bash
go test ./internal/service -run 'Test.*(UpstreamError|Passthrough|AnthropicSSE|GatewayHandleError|OpsSkip)'
go test ./internal/handler -run 'Test.*(Failover|Candidate|Responses|Gemini|StreamingAware)'
go test ./internal/service ./internal/handler -count=1
go test -race ./internal/service ./internal/handler -run 'Test.*(UpstreamError|Passthrough|Failover|Candidate)'
go vet ./internal/service ./internal/handler
gofmt -w <changed-go-files>
git diff --check
```

Known pre-existing `go vet` lock-copy warnings in `openai_ws_protocol_resolver_test.go` must be reported separately unless the baseline changes.

## Acceptance criteria

- No supported path records or consumes a synthetic upstream stream status.
- Generic HTTP recognized direct errors bypass health mutation, failover, and database rules.
- No unknown/no-rule path returns a raw upstream body.
- Unknown real 4xx errors preserve the real status with a fixed safe message.
- Unknown 5xx, status-unknown, transport, and malformed errors use generic 502.
- `skip_monitoring` rules apply only to unknown bounded facts and affect ops monitoring only.
- Matched-rule provider messages come only from bounded/redacted facts.
- Every migrated exhaustion path uses deterministic candidate precedence and final resolution.
- Structured candidates are not replaced by weaker later failures.
- Unknown status-only candidates do not bypass safe fallback.
- Protocol renderers emit at most one legal terminal error.
- Successful request and streaming hot paths gain no broad parsing or copying.
- Focused and relevant package tests pass with no new baseline regression.
