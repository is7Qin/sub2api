# Issue #24 PR 2: HTTP/SSE Recognized Direct Errors — SDD Plan

> **Base:** merged PR 1 on `origin/dev` (`e69f19d58`).
>
> **PR boundary:** add a small policy/presentation contract and migrate only stable request-scoped and provider-wide HTTP/SSE semantics to direct return. Keep WebSocket, targeted account recovery, final-candidate retention, and unknown database fallback for later PRs.

## Scope and non-goals

PR 2 consumes the bounded `UpstreamErrorFact` added in PR 1. Recognized direct errors do not enter `FailoverState`, do not retry the same account, do not exclude or switch accounts, and do not invoke account-health transitions.

Included semantic classes:

- OpenAI `server_is_overloaded`;
- OpenAI `cyber_policy` and stable request/content/safety policy rejection;
- existing OpenAI context-window request rejection;
- Anthropic native SSE request-scoped error types that can be safely rendered directly;
- existing prompt-too-long direct-return paths, characterized to prevent regression.

This PR must not:

- alter Responses WebSocket behavior;
- introduce account-scoped retry/failover budgets or final-candidate retention;
- move unknown error matching to the new fact contract (PR 5);
- delete seeded database rows or legacy helpers unrelated to migrated recognized paths;
- forward arbitrary upstream bodies, verbose response objects, headers, URLs, credentials, metadata, request bodies, tool definitions, or model output;
- infer an upstream HTTP status for semantic errors carried over HTTP 200;
- change billing/usage submission for admitted attempts.

## Contracts

### Attempt disposition

```go
type UpstreamAttemptDisposition string

const (
    UpstreamAttemptDirectReturn UpstreamAttemptDisposition = "direct_return"
    UpstreamAttemptRetrySameAccount UpstreamAttemptDisposition = "retry_same_account"
    UpstreamAttemptFailover UpstreamAttemptDisposition = "failover"
    UpstreamAttemptGenericAbort UpstreamAttemptDisposition = "generic_abort"
)
```

Only `DirectReturn` is executed by newly migrated PR 2 paths. The remaining values make the result mutually exclusive and reserve the policy boundary for later migration without changing current recovery behavior.

### Safe presentation

```go
type UpstreamClientPresentation struct {
    HTTPStatus int
    ErrorCode  string
    ErrorType  string
    Message    string
}

type RecognizedUpstreamErrorPolicy struct {
    Disposition  UpstreamAttemptDisposition
    Presentation UpstreamClientPresentation
}
```

Recognition returns `(policy, true)` only for built-in recognized semantics. Unknown facts return `false` and continue through the existing compatibility flow in PR 2.

All presentation scalars are copied from bounded/redacted fact fields or fixed gateway strings. Rendering never reads the raw upstream body.

## Recognition precedence

Within migrated HTTP/SSE boundaries:

```text
built-in recognized direct policy
  > current compatibility behavior
```

For `cyber_policy` and other migrated recognized semantics, database passthrough rules are not consulted. Unknown semantics keep their existing PR 1 behavior until PR 5.

Initial built-in policy:

| Semantic | Disposition | Presentation | Account health |
|---|---|---|---|
| `server_is_overloaded` | DirectReturn | 503; preserve safe code/type/message | None |
| `cyber_policy` / stable policy rejection | DirectReturn | 400; preserve safe code/type/message | None |
| context-window rejection | DirectReturn | existing 400 `context_length_exceeded` presentation | None |
| Anthropic `invalid_request_error` over SSE | DirectReturn | 400 Anthropic error | None |
| Anthropic recognized overload over SSE | DirectReturn | 503 Anthropic error | None |

Missing safe messages use bounded fixed fallbacks. Missing provider type/code never causes raw-body fallback.

## Renderer boundaries

Renderers accept only `UpstreamClientPresentation`:

- OpenAI HTTP/non-streaming: `{ "error": { "code", "type", "message" } }`;
- Responses SSE before real output: return an HTTP JSON error before committing streaming output;
- Responses SSE after real output: write exactly one sanitized `response.failed` terminal frame and no retry/failover;
- Anthropic HTTP/non-streaming: existing Anthropic error envelope;
- Anthropic SSE before real output: return the direct error without the synthetic `UpstreamFailoverError{StatusCode:403}` bridge;
- Anthropic SSE after real output: write exactly one `event: error` frame and return a non-failover direct error.

`HTTPStatusKnown` remains false for HTTP 200 stream errors. `Presentation.HTTPStatus` is derived and must not be copied back into the fact.

## SDD scenario matrix

| ID | Input/boundary | Expected behavior | Compatibility guard |
|---|---|---|---|
| D1 | OpenAI HTTP structured `server_is_overloaded` | safe 503 direct return | zero failover state mutation; no account-health call |
| D2 | OpenAI Responses SSE `response.failed`, overload before output | safe 503 JSON/direct error | no synthetic 502; no account switch |
| D3 | Same overload after real output | one sanitized `response.failed` terminal | no duplicate terminal; no switch |
| D4 | OpenAI `cyber_policy`, no bound rule service | safe 400 direct return | does not require seeded DB rule |
| D5 | `cyber_policy` with conflicting matching rule | built-in 400 presentation wins | rule matcher not consulted; no `skip_monitoring` side effect |
| D6 | Context-window HTTP/SSE rejection | existing direct 400 retained | no failover/account-health regression |
| D7 | Anthropic HTTP 200 + SSE `invalid_request_error` before output | direct Anthropic 400 | no synthetic upstream 403 fact/status |
| D8 | Anthropic recognized SSE error after output | exactly one Anthropic `event:error` | no failover after output |
| D9 | Unknown OpenAI/Anthropic error | current compatibility path | no premature PR 5 behavior |
| D10 | Secret-bearing/oversized recognized message | bounded redacted presentation | no raw body/metadata leakage |
| D11 | Successful HTTP/SSE | unchanged output | no policy/parser work on success hot path |
| D12 | Admitted request with terminal direct error and usage | partial usage remains available | no billing suppression |

## Test-first checkpoints

### Step 1 — Pure recognition and presentation contract

**Files**

- create `internal/service/upstream_error_policy.go`;
- create `internal/service/upstream_error_policy_test.go`.

**RED tests**

- overload fact resolves to `DirectReturn`, safe 503, preserved safe code/type/message;
- cyber-policy fact resolves to safe 400;
- context-window fact resolves to existing safe 400 semantics;
- unknown fact is not recognized;
- recognized facts never expose unbounded or secret-bearing scalars;
- recognition does not mutate the input fact.

Implement only pure recognition and scalar normalization. No database/service/handler dependencies.

### Step 2 — OpenAI HTTP direct recognition

**Files**

- modify existing HTTP error branches in `internal/service/openai_gateway_service.go`;
- extend focused OpenAI HTTP error tests.

**RED tests**

- structured overload HTTP response returns 503 directly;
- account-health repository/service spy receives no transition;
- handler-level two-account fixture performs one upstream request only;
- conflicting passthrough rule is not consulted for recognized cyber/overload;
- unknown HTTP error retains current behavior.

Run recognition before `applyErrorPassthroughRule`, `ShouldHandleErrorCode`, and `handleOpenAIAccountUpstreamError`, but only when it returns a built-in policy.

### Step 3 — OpenAI Responses SSE direct recognition

**Files**

- modify `handleStreamingResponse`, `handleStreamingResponsePassthrough`, `handleSSEToJSON`, and `handlePassthroughSSEToJSON` only where existing terminal detection already occurs;
- extend `openai_gateway_response_failed_passthrough_test.go` and response-failed characterization tests.

**RED tests**

- pre-output overload returns direct 503 and never constructs `UpstreamFailoverError`;
- post-output overload emits one sanitized terminal event;
- cyber-policy works without a bound rule service;
- a bound conflicting cyber rule is not consulted;
- context-window terminal remains 400;
- usage attached before the terminal remains present in the returned result/error path;
- verbose `response.failed` fields and secrets are absent from client output.

Parsing remains behind existing `response.failed` detection. Successful SSE frames incur no policy work.

### Step 4 — Anthropic native SSE direct recognition

**Files**

- modify the native Anthropic SSE error bridge in `internal/service/gateway_service.go`;
- extend `gateway_streaming_test.go` and handler failover characterization.

**RED tests**

- HTTP 200 + `event:error` recognized as request-scoped returns direct 400, not `UpstreamFailoverError`;
- the attached/returned fact still has `HTTPStatusKnown=false` and status zero;
- pre-output direct errors do not enter `FailoverState`;
- post-output errors emit one Anthropic terminal frame and do not switch;
- malformed/unknown Anthropic errors retain compatibility fallback in PR 2;
- partial explicit usage remains billable.

### Step 5 — Prompt/context regression characterization

No broad redesign. Add or extend tests proving existing `PromptTooLongError`, `OpenAIUpstreamRequestError`, and local beta-policy direct handling remain non-failover and continue using their current status/envelopes.

### Step 6 — Verification

Targeted commands:

```bash
go test ./internal/service -run 'TestRecognize.*Upstream|Test.*ServerIsOverloaded|Test.*CyberPolicy|Test.*ResponseFailed|Test.*Anthropic.*SSEError'
go test ./internal/handler -run 'Test.*DirectReturn|Test.*Failover.*Recognized|Test.*PromptTooLong|Test.*Context'
go test -race ./internal/service -run 'Test.*ResponseFailed|Test.*SSEError'
go test ./internal/service -run '^$' -bench 'BenchmarkOpenAIWS(ForwarderHotPath|EventEnvelopeParse)' -benchmem
```

Broader checks:

```bash
go test ./internal/handler
go test ./internal/service
go vet ./internal/service ./internal/handler
git diff --check
```

Compare full `internal/service` and vet failures against the recorded PR 1 baseline. Do not classify existing WSv2 fixture or lock-copy failures as PR 2 regressions without before/after evidence.

## Review checklist

- [ ] Every migrated recognized error resolves to exactly one disposition.
- [ ] Direct-return errors never construct or enter `UpstreamFailoverError`/`FailoverState`.
- [ ] Recognized errors bypass database rules.
- [ ] Unknown errors retain PR 1 compatibility behavior.
- [ ] No migrated direct error calls account-health mutation code.
- [ ] Stream facts retain `HTTPStatusKnown=false` under HTTP 200.
- [ ] Derived presentation status never mutates the fact.
- [ ] Renderers consume only bounded safe presentation fields.
- [ ] Exactly one protocol-valid terminal event is written after output.
- [ ] Successful streaming hot paths remain unchanged.
- [ ] Usage/billing behavior for admitted attempts is preserved.
