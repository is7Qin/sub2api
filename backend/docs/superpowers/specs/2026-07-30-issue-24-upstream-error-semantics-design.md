# Issue #24: Upstream Error Semantics Design

**Date:** 2026-07-30  
**Status:** Proposed and approved for planning; implementation remains gated by per-PR SDD

## Purpose

Make gateway handling of upstream failures semantically correct, bounded, safe, and consistent across HTTP, SSE, and Responses WebSocket transports.

The current code uses `UpstreamFailoverError` as an overloaded carrier for upstream facts, retry/failover signals, account-health inputs, and client-facing error bodies. This causes semantic information to be lost before presentation. Two concrete failures are:

- Anthropic `HTTP 200 + SSE event:error` is represented as a fake upstream HTTP 403.
- OpenAI `response.failed` with `code=server_is_overloaded` can become synthetic `502 upstream_error`, then end as generic 502 or `All available accounts exhausted`.

This design introduces a fact-to-policy-to-presentation flow without widening successful streaming hot paths.

## Non-goals

- Replacing all provider adapters or rewriting all handlers in one change.
- Forwarding arbitrary raw upstream error bodies.
- Making database rules a retry, failover, account-health, or scheduler DSL.
- Changing unrelated admin/domain error behavior.
- Removing every provider-local retry in the first implementation pass.

## Invariants

1. An upstream HTTP status is recorded only when the upstream response actually had that status. A semantic stream error carried in an HTTP 200 response has `HTTPStatusKnown=false`.
2. Client presentation status is a derived value and never overwrites the upstream fact.
3. A direct-return error never enters `FailoverState`, increments a switch counter, excludes an account, or affects account health unless its explicit policy says so.
4. A real client output start prevents silent account switching. At most one protocol-valid terminal error may be written.
5. Built-in recognized semantic policy takes precedence over database fallback. Only unknown errors may query `error_passthrough_rules`.
6. Unrecognized errors that have no matching database rule return a bounded generic 502.
7. `skip_monitoring` affects ops monitoring only. It never suppresses billing, scheduler, account-health, or rate-limit transitions.
8. A later success suppresses all prior error candidates for the logical request.
9. Error extraction, matching, diagnostics, and client output are bounded and redacted. They never expose credentials, tokens, cookies, Authorization headers, internal URLs, stack traces, request bodies, tool definitions, session IDs, installation IDs, or unbounded upstream metadata.
10. Successful streaming paths retain their current low-overhead forwarding behavior. Parsing is performed only on error/terminal paths and is bounded.

## Error fact contract

Provider boundaries parse an immutable bounded `UpstreamErrorFact`. The final Go names may follow repository conventions, but the data contract is:

```text
Provider
Source: HTTP | SSE | WebSocket | Transport | StreamTermination
HTTPStatusKnown
HTTPStatus
ProviderCode
ProviderType
SafeMessage
RequestID
RetryAfter
ScopeHint: Request | Account | Model | Provider | Unknown
InternalMatchText (bounded, redacted; never client output)
```

`HTTPStatusKnown` is required. HTTP status is absent for transport errors and semantic stream errors received over HTTP 200.

The parser may preserve safe scalar values only after bounded extraction and redaction. `InternalMatchText` is for known-error recognition and unknown-rule matching; it is bounded and is not a raw body passthrough mechanism.

Existing `UpstreamFailoverError` remains behind a compatibility bridge during migration. It is no longer the source of truth for error identity or client presentation.

## Policy outputs

Recognition and recovery are separate. A recognized semantic error resolves to three independent outputs:

```text
AttemptDisposition
AccountHealthAction
ClientPresentation
```

### Attempt disposition

The disposition is mutually exclusive:

```text
DirectReturn
RetrySameAccount(budget, exhaustion action)
Failover(account-transition budget)
GenericAbort
```

A transition budget is semantic policy, not a handler default. The existing `max_account_switches` remains a global hard cap:

```text
actual transitions = min(semantic transition budget, configured hard cap)
```

### Account health action

Account-health actions remain distinct from client response generation:

```text
None
RecordOnly
ApplyRateLimit
TemporarilyUnschedule
PermanentlyDisable
ApplyModelRateLimit
```

The existing `ErrorPolicyResult` remains an account scheduling/health mechanism. It must not become the new client semantic policy abstraction.

### Client presentation

The presentation contains only safe fields:

```text
HTTPStatus
ErrorCode
ErrorType
Message
Protocol terminal shape
```

Renderers receive finalized presentation only. They do not inspect raw upstream bodies or make retry/failover decisions.

## Confirmed recovery budgets

| Semantic class | Same-account retry | Account transitions | Account health | Presentation |
|---|---:|---:|---|---|
| Request parameters, context window, prompt too long | 0 | 0 | None | Direct safe client error |
| `cyber_policy`, safety, content/request policy rejection | 0 | 0 | None | Direct safe client error |
| Provider-wide `server_is_overloaded` | 0 | 0 | None | Direct 503 preserving safe code/type/message |
| Provider/model-wide capacity exhaustion | Explicit provider-local bounded retry only | 0 | Model-level cooldown/rate limit when appropriate | Direct safe 503 after retry budget |
| Explicit account token invalid/revoked | 0 | 1 | Temporary unschedule or permanent disable | Safe auth error if recovery exhausts |
| Explicit account billing/workspace unavailable | 0 | 1 | Permanent disable or configured account state | Safe billing/upstream error if recovery exhausts |
| Account-specific rate limit/quota | At most one only when provider retry-after semantics support it | 1 | Rate-limit / temporary scheduling state | Safe rate-limit result if recovery exhausts |
| Stable project configuration error | At most 1 | 1 | Temporary unschedule after exhaustion when appropriate | Safe configuration/upstream error |
| Generic provider 5xx | 0 | 1 | Record only by default | Safe temporary-unavailable error |
| Persistent proxy/DNS/TCP error | 0 | 1 | Temporary unschedule only for durable fault classes | Safe generic 502 if recovery exhausts |
| Unknown | 0 | 0 | Record only | Database matched presentation or generic 502 |

Provider-local request repair remains allowed only where it changes the request deterministically and is explicitly bounded. Existing Gemini signature downgrade, Antigravity prompt fallback, and OpenAI request repair are separate from account recovery.

## Built-in recognition precedence

Recognition is provider- and protocol-scoped. The precedence is:

```text
built-in recognized semantic policy
  > database fallback for unknown errors
  > generic 502 for unknown/unmatched/unsafe errors
```

Examples of built-in recognized errors include:

- OpenAI `cyber_policy` and stable policy/safety request rejections;
- OpenAI `server_is_overloaded`;
- OpenAI context-window terminal errors;
- Anthropic/native stream semantic errors;
- Antigravity prompt-too-long and model-capacity errors;
- stable Gemini/Antigravity project-configuration errors.

Recognition does not imply direct return. Account-scoped authorization, quota, or project-configuration errors can still have the targeted recovery budget in the table above.

## Unknown database fallback

`error_passthrough_rules` remains an administrator extension mechanism for errors the gateway does not recognize.

For an unknown fact only:

1. match platform, actual upstream HTTP status if known, and bounded redacted internal match text;
2. if a rule matches, build a safe direct client presentation;
3. if none matches, build the generic 502 presentation.

Rules do not affect recovery budgets, account transitions, account health, billing, scheduling, or rate-limit state.

The existing `OpenAI cyber_policy fallback` seed becomes runtime-irrelevant once built-in recognition ships. Cleanup is separate: only an exact canonical seeded row may be removed in a later migration; modified or administrator-created rules are retained.

## Attempt and final-candidate flow

```text
provider HTTP/SSE/WebSocket/transport failure
  -> bounded UpstreamErrorFact parser
  -> built-in recognition/classification
  -> policy resolution
       DirectReturn
       RetrySameAccount
       Failover
       GenericAbort
  -> execute explicit account-health action
  -> if recovery attempted, retain safe semantic candidate
  -> later success clears candidates and returns success
  -> recovery exhausts: choose deterministic final candidate
  -> unknown final candidate only: optional database fallback
  -> safe presentation
  -> protocol renderer
```

Candidate rules:

1. Direct-return facts terminate immediately and are not retained as failover candidates.
2. Structured recognized semantics outrank status-only facts.
3. Status-only facts outrank generic transport failures.
4. Ties resolve to the most recent candidate.
5. A success clears all candidates.
6. No safe candidate yields generic 502.

This prevents a structured overload or policy error from being replaced by `All available accounts exhausted` or a later weaker transport failure.

## Downstream lifecycle

Attempt policy receives lifecycle state independently from upstream facts:

```text
HeadersCommitted
RealOutputStarted
TerminalWritten
```

Responses WebSocket has equivalent event/close state:

```text
ClientEventDelivered
TerminalOrCloseWritten
```

Rules:

- Keepalive/preamble output is not real client output.
- After real client output starts, the current logical response cannot switch accounts.
- After terminal output or close has been written, no further error is emitted.
- HTTP direct errors render as JSON before headers commit; otherwise as the protocol-valid terminal event.
- Responses SSE uses `response.failed`.
- Anthropic SSE uses `event: error`.
- Compatibility Chat Completions uses its valid SSE error form.
- WebSocket writes one valid error event or closes once, according to its protocol lifecycle.

## SDD delivery plan

Every PR in this design follows the repository SDD process. No production behavior is introduced in a PR before that PR has a written, reviewed scenario set and failing characterization/acceptance tests for its stated scope.

Each PR uses this cycle:

1. write the PR-local scenario matrix and invariants;
2. add targeted failing tests or characterization tests;
3. review the test boundary against this design and current provider behavior;
4. implement the minimum change that makes only those scenarios pass;
5. run the targeted suite and the relevant package suite;
6. compare broader-suite failures with the pre-existing baseline and record any unrelated failures;
7. conduct implementation review before moving to the next PR.

The full `internal/service` baseline currently has unrelated WSv2 failures. Targeted issue #24 suites are the regression gate; full-package results are reported as baseline deltas rather than silently treated as clean.

### PR 1: Facts and characterization

Scope:

- Add `UpstreamErrorFact` and bounded provider-boundary parsers.
- Cover HTTP non-2xx, Anthropic `HTTP 200 + SSE event:error`, OpenAI `type:error`, `response.failed`, Responses WebSocket terminal shapes, and transport failures.
- Distinguish actual HTTP status from inferred client status.
- Add compatibility bridge to existing `UpstreamFailoverError` callers.
- Add malformed, oversized, secret-bearing, and terminal-order fixtures.
- Characterize existing synthetic 403 and synthetic 502 behavior without changing policy.

SDD acceptance scenarios:

- stream error over HTTP 200 has `HTTPStatusKnown=false`;
- HTTP error preserves actual status;
- parser bounds and redacts unsafe data;
- malformed payload produces a safe unknown fact;
- successful stream forwarding does not invoke parsing;
- compatibility callers retain existing behavior until PR 2 migration.

### PR 2: HTTP/SSE recognized direct return

Scope:

- Add mutually exclusive attempt disposition.
- Migrate stable request-scoped policy, context, prompt-length, and structured provider-overload semantics.
- Remove fake Anthropic stream 403 and OpenAI overload synthetic 502.
- Add finalized safe presentation and thin HTTP/Anthropic SSE/Responses SSE renderers.
- Enforce no recovery after real output begins.
- Make recognized behavior independent of database rules.

SDD acceptance scenarios:

- `server_is_overloaded` before output returns safe 503 with zero switch and no account-health action;
- the same error after output writes exactly one valid terminal event and does not switch;
- `cyber_policy` direct returns even with no database rule;
- context window and prompt-too-long retain their direct-return semantics;
- Anthropic HTTP 200 SSE error never records fake upstream 403;
- seeded cyber rule is not consulted for a recognized error.

### PR 3: Responses WebSocket semantic handling

Scope:

- Parse WebSocket terminal/error frames into the fact contract.
- Apply direct-return semantics to WebSocket turns.
- Preserve accounting and usage behavior.
- Enforce one error event or close and prohibit account switching after client output.
- Stop relying solely on synthetic status to create close reasons.

SDD acceptance scenarios:

- overload before client event direct-terminates without account switch;
- overload after event produces one valid terminal/close sequence;
- no duplicate error/close write;
- context terminal preserves direct-return behavior;
- generic failover retains existing bounded WebSocket account switching;
- client disconnect does not trigger additional recovery output.

### PR 4: Targeted recovery and final candidate retention

Scope:

- Introduce explicit semantic retry/switch budgets and exhaustion actions.
- Make one account transition the normal maximum for recoverable account-scoped semantics.
- Retain safe semantic candidates across recovery attempts.
- Separate account-health actions from client presentation.
- Migrate account auth, billing, quota, project-config, provider 5xx, and transport classes incrementally.
- Constrain provider-local loops through declared semantic budgets without deleting justified provider/model recovery.

SDD acceptance scenarios:

- account-scoped quota can switch once and final output preserves quota semantics;
- provider-wide overload never switches;
- durable proxy/DNS failures temporarily unschedule only the failed account and switch at most once;
- later success clears earlier candidates;
- a later generic transport failure does not displace an earlier authoritative structured candidate;
- global config remains a hard cap, not a default policy multiplier.

### PR 5: Unknown-only database fallback

Scope:

- Route all database passthrough matching through finalized unknown facts.
- Remove database matching from recognized-error paths.
- Keep existing valid status/keyword/platform compatibility for unknown facts.
- Keep `skip_monitoring` strictly operational.
- Return bounded generic 502 for unmatched unknowns.

SDD acceptance scenarios:

- recognized `cyber_policy` and overload do not query rule service;
- unknown matching a rule receives its safe rule presentation;
- unknown unmatched returns generic 502;
- rule cannot alter account health, retry, or account switch behavior;
- `skip_monitoring` does not suppress usage, billing, rate-limit, or scheduler action;
- matching uses bounded redacted text.

### PR 6: Legacy cleanup

Scope:

- Remove obsolete synthetic status bridges and duplicate mappers after equivalent coverage exists.
- Remove final `All available accounts exhausted` replacement where a semantic candidate exists.
- Consolidate duplicated safe rendering/sanitization paths.
- Make seeded cyber fallback cleanup conservative and separate from semantic behavior.

SDD acceptance scenarios:

- no supported path still creates fake upstream stream status;
- no supported recognized terminal error becomes generic exhaustion;
- all protocol renderers consume final presentation rather than raw upstream body;
- canonical seed cleanup does not delete modified/admin-owned rules;
- targeted legacy and new suites pass with no new broader baseline regressions.

## Test strategy

Targeted suites cover:

- HTTP JSON error facts;
- SSE event/error and terminal parsing;
- OpenAI `response.failed` and `type:error`;
- Responses WebSocket terminal/error lifecycle;
- actual versus inferred status;
- malformed and oversized payloads;
- redaction and secret-bearing payloads;
- keepalive versus real output;
- exactly-once terminal writing;
- direct-return bypass of failover and account health;
- bounded recovery and final-candidate precedence;
- unknown database matched and unmatched cases.

Each PR also runs the narrow relevant packages. Broader failures are compared with the established baseline; no unrelated WSv2 failure is reclassified as an issue #24 regression without evidence.

## Compatibility and rollout

- Preserve public endpoint protocol shapes throughout migration.
- Retain legacy `UpstreamFailoverError` bridge until all target call sites are migrated.
- Do not remove existing administrator rules during semantic-policy rollout.
- Avoid schema changes unless later cleanup needs an exact system-managed identifier; current behavior can become correct without one.
- Deliver independently reviewable PRs. PR N depends only on the tested contracts established by preceding PRs.
