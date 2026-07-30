# Issue #24 PR 3: Responses WebSocket Recognized Error Semantics — SDD Plan

> **Base:** merged PR 2 on `origin/dev` (`c09e84598`).
>
> **PR boundary:** migrate recognized OpenAI Responses WebSocket terminal and error frames to the fact/policy/presentation contract established by PRs 1–2. Preserve existing rate-limit failover, bounded transport retry, previous-response repair, disconnect drain, accounting, and unknown-error compatibility. Do not introduce PR 4 recovery budgets, PR 5 database fallback, or PR 6 cleanup.

## Scope and non-goals

PR 3 covers the three existing Responses WebSocket boundaries:

1. `forwardOpenAIWSV2`: an HTTP Responses request whose upstream transport is WebSocket v2;
2. `ProxyResponsesWebSocketFromClient`: managed client WebSocket ingress using the pooled upstream connection;
3. `proxyResponsesWebSocketV2Passthrough`: native API-key WebSocket ingress using the low-level passthrough relay.

Included behavior:

- parse `response.failed` and `type:error` frames with `ParseOpenAIWebSocketErrorFact`;
- apply the existing transport-independent `RecognizeUpstreamErrorFact` policy;
- convert recognized facts to `OpenAIUpstreamRequestError` before any generic fallback/failover path;
- render recognized frames from bounded `UpstreamClientPresentation` fields rather than arbitrary upstream JSON;
- retain terminal usage and request ID;
- base `OutputStarted` on actual successful downstream event delivery, not `FirstTokenMs`;
- ensure one semantic terminal owner per turn and prevent account switching/account-health mutation for recognized direct errors;
- retain the existing context-window direct behavior, including its stricter ambiguous-envelope validation, as a compatibility guard.

This PR must not:

- redesign the number of account transitions or introduce semantic retry budgets;
- retain or rank final semantic candidates across recovery attempts;
- move unknown WebSocket errors to database rule matching or generic 502 handling;
- broadly reclassify handshake statuses without real upstream fixtures;
- remove the legacy `UpstreamFailoverError` or synthetic-status bridges still used by unknown/recoverable cases;
- change successful frame forwarding, model/tool rewriting, sticky-session behavior, or usage pricing;
- expose those fields from any newly reconstructed recognized frame: authorization data, cookies, API keys, tokens, session/thread/installation identifiers, internal URLs, stack traces, request bodies, tool definitions, model output, verbose response objects, or unbounded metadata. Unknown-path hardening beyond the current sanitizer remains PR 6 scope and must not regress in PR 3.

## Existing compatibility behavior to preserve

The recognized-error migration is deliberately ordered around these existing paths:

- a pre-output WebSocket rate-limit/quota `type:error` persists rate-limit state and may produce `UpstreamFailoverError`;
- a handshake HTTP 429 retains the actual upstream status and current rate-limit handling;
- a generic managed-ingress read/write transport failure can retry the same logical turn at most once before any client event is delivered;
- `previous_response_not_found` can repair/replay once before output by removing invalid previous-response state;
- after client disconnect, downstream writes remain disabled while the admitted upstream turn drains for terminal usage;
- unknown, malformed, and compatibility-only error frames retain their current behavior in PR 3.

Previous-response repair remains first where applicable. For `type:error`, parse the bounded fact before rate-limit classification and reserve built-in `server_is_overloaded` and `cyber_policy` for direct policy. Every remaining unrecognized frame continues through the existing `isOpenAIWSRateLimitError` classifier and persistence/failover branch; PR 3 does not infer or populate a new `ScopeHint` for this decision. This prevents contradictory rate-limit wording/type fields from overriding the authoritative built-in provider code without changing ordinary rate-limit compatibility.

## Contracts

### Recognition at WebSocket boundaries

Every migrated semantic boundary parses one bounded fact, but `response.failed` first obtains a strict compatibility eligibility result from the established trusted-envelope parser:

```go
type failedTerminalCompatibilityClass int
const (
    failedTerminalNotContextCandidate failedTerminalCompatibilityClass = iota
    failedTerminalValidContext
    failedTerminalRejectedContextCandidate
)

fact := ParseOpenAIWebSocketErrorFact(PlatformOpenAI, payload, requestID)
compatibility := classifyOpenAIFailedTerminalCompatibility(payload)
switch compatibility {
case failedTerminalValidContext:
    requestErr = newOpenAIUpstreamRequestError(payload, requestID)
case failedTerminalRejectedContextCandidate:
    // Retain existing unknown/compatibility behavior. Do not broadly recognize.
default:
    requestErr = newRecognizedOpenAIUpstreamRequestError(fact)
}
```

`classifyOpenAIFailedTerminalCompatibility` is a small extension/adaptor around the current strict parser, not a second policy table. It distinguishes a valid context rejection from a context candidate rejected for malformed/duplicate/conflicting trusted-envelope members. If either trusted envelope mentions a context code/message but the envelopes disagree (for example `error.code=context_length_exceeded` and `response.error.code=server_is_overloaded`), it returns `RejectedContextCandidate` and blocks **all** broad recognition, irrespective of the fact parser's selected code.

This keeps the legacy context constructor's ambiguity checks authoritative. Non-context built-ins use the shared recognized policy. A recognized error never becomes `UpstreamFailoverError`, never excludes an account, never increments account-switch count, and never reports a failed scheduler/account-health result.

`HTTPStatusKnown` remains false for semantic frames received over an established WebSocket. `OpenAIUpstreamRequestError.StatusCode` is a client presentation status and must not be copied into the fact as an observed upstream HTTP status.

### Safe WebSocket presentation

Add focused render helpers that accept only the recognized presentation plus bounded correlation fields:

- recognized `response.failed` → a minimal protocol-valid frame containing `type`, optional safe response ID, `response.status`, and `response.error.{code,type,message}`;
- recognized `type:error` → a minimal protocol-valid frame containing `type:error` and `error.{code,type,message}`; retain an event ID only if already bounded and safe;
- no renderer copies unknown sibling fields from the raw payload;
- terminal usage is retained internally for accounting, but is not copied into the safe public error frame unless an existing protocol contract explicitly requires it.

Existing sanitization remains the fallback for context and unknown frames. Direct passthrough means safe semantic preservation, never raw body passthrough.

### Client-delivery lifecycle

Per logical turn track two independent facts:

```text
ClientEventDelivered
TerminalOwned
```

`ClientEventDelivered` means that a frame belonging to the current turn was successfully written **before the current semantic terminal/error frame was admitted**. The terminal's own write never changes the `OutputStarted` value captured for that terminal. It is not inferred from token classification or `FirstTokenMs`.

Consequences:

- a recognized terminal before any prior successful client write has `OutputStarted=false`, even if writing that terminal succeeds;
- a successfully delivered `response.created` followed by `response.failed` has `OutputStarted=true`, even when `FirstTokenMs` is nil;
- a token delta followed by a recognized terminal also has `OutputStarted=true`;
- a failed write does not mark the event delivered;
- state is per turn, not connection-global, so an earlier completed turn cannot make a later terminal-only turn look started.

`FirstTokenMs` remains a timing metric only.

`TerminalOwned` is set when a recognized terminal/error is admitted for that turn. The owner is responsible for exactly one safe error/terminal delivery attempt and the normal connection close/return path. Generic recovery and fallback must not emit a second error after ownership is established.

### Low-level passthrough relay boundary

The low-level `openai_ws_v2` package remains provider-policy agnostic. Extend only the relay metadata/hook boundary needed by the service adapter:

```go
type RelaySemanticTerminal struct {
    Terminal bool
    StopRelay bool
}

type RelayOptions struct {
    ClassifySemanticTerminal func(msgType coderws.MessageType, payload []byte, eventType string) RelaySemanticTerminal
    // existing hooks...
}

type RelayTurnResult struct {
    // existing fields...
    ClientEventDelivered bool
}
```

The classifier runs during text-frame observation, before response-ID fallback, duplicate-terminal suppression, usage completion, and `OnTurnComplete`. It does not expose provider semantics to the relay; it only answers whether the already parsed frame terminates the logical turn and whether the relay must stop after the single delivery attempt.

For an effective terminal, the relay snapshots prior delivery for the **same response ID or id-less generation** into `RelayTurnResult.ClientEventDelivered`, applies existing duplicate suppression, emits `OnTurnComplete` once, attempts the transformed client write once, and publishes a graceful semantic-terminal exit when `StopRelay` is true. A write failure still preserves the callback/terminal owner, while client disconnect continues the established drain path.

Classifier-only `type:error` frames do not use their top-level `id` as a response/request ID: that field can be an event ID. Unless the frame contains a trusted response ID in `response.id` or `response_id`, it uses the current id-less turn generation for delivery association and replay suppression. Per-response delivery state is retained independently until that response terminal completes; completing response A must not clear response B's delivery state. Tests include interleaved A/B output followed by reversed terminals.

The service adapter performs fact parsing, policy recognition, safe transformation, typed-error construction, and chooses `Terminal/StopRelay`. The relay only tracks provider-neutral delivery, terminal, duplicate, and exit lifecycle.

## Boundary behavior

### HTTP request with upstream WSv2

- streaming recognized `response.failed` before any buffered event is flushed: suppress buffered preamble and terminal bytes, return typed direct error so the HTTP handler can render the safe 400/503 response;
- non-streaming recognized failure before output: do not write the current `200` final response; return the typed error so the handler owns the safe 400/503 JSON response;
- recognized terminal after successful downstream SSE delivery: emit exactly one sanitized terminal frame, return typed direct error with `OutputStarted=true`, and do not emit handler fallback;
- recognized `type:error`: use the same direct/safe split rather than the generic error/fallback path;
- context-window terminal retains its existing direct 400 framing compatibility;
- if a pre-output direct terminal contains billable usage, the HTTP handler must submit that result before returning while setting `PreserveAccountHealth=true`; absence of client output is not permission to drop admitted usage.

### Managed client WebSocket ingress

- preserve pre-output previous-response repair and rate-limit failover precedence;
- for recognized `response.failed` or recognized non-rate-limit `type:error`, write one safe frame if the client is connected, attach usage, return typed direct error, and end the account attempt without switching;
- derive `OutputStarted` from the turn-local `wroteDownstream` value before the terminal/error write;
- if the client is disconnected, do not attempt later recovery output; continue the existing terminal drain/accounting path.

### Native API-key passthrough ingress

- `TransformWriteClient` reconstructs recognized `response.failed`/`type:error` frames from safe presentation fields;
- the relay reports per-turn client delivery to `OnTurnComplete`;
- `OnTurnComplete` applies the context compatibility guard first, then creates other recognized typed errors, and attaches terminal usage plus the relay's pre-terminal delivery snapshot;
- `ClassifySemanticTerminal` reserves built-in overload/cyber for direct handling; `BeforeWriteClient` retains existing failover behavior only for remaining unrecognized pre-output rate-limit `type:error` frames;
- once recognized terminal ownership is established, the adapter returns the typed request error even if the terminal write or subsequent close fails.

## SDD scenario matrix

| ID | Input/boundary | Expected behavior | Compatibility guard |
|---|---|---|---|
| W1 | HTTP→WSv2 streaming overload as first `response.failed` | direct safe 503; no committed SSE bytes | no failover/account mutation |
| W1b | HTTP→WSv2 non-streaming overload as first `response.failed` | direct safe 503 JSON; no prior HTTP 200 body | handler owns presentation |
| W1c | HTTP→WSv2 pre-output recognized failure with usage | usage submitted once; `PreserveAccountHealth=true` | no accounting loss on early return |
| W2 | Managed ingress overload as first `response.failed` | one safe terminal; typed 503; `OutputStarted=false` | no account switch |
| W3 | Managed ingress `response.created` then overload | one safe terminal; `OutputStarted=true` with nil TTFT allowed | event delivery, not token timing |
| W4 | Managed ingress token delta then overload | partial output plus one terminal | no retry/switch after output |
| W5 | Managed ingress `cyber_policy` terminal | safe typed 400 | no database dependency |
| W6 | Context-window `response.failed` | existing direct 400 retained | existing sanitized shape retained |
| W7 | Native passthrough overload terminal | safe terminal and typed 503 | no failover/account-health action |
| W8 | Terminal client write failure | recognized typed error still owns turn once | no duplicate fallback/close error event |
| W9 | Recognized `type:error` before prior client event | one safe error terminal and typed direct error | raw siblings/secrets absent |
| W10 | Recognized `type:error` after prior client event | one safe error terminal; `OutputStarted=true` | no later recovery output or switch |
| W11 | Pre-output unrecognized usage/rate-limit `type:error` | existing rate-limit persistence and failover | built-in overload/cyber code wins over contradictory wording/type |
| W12 | Handshake HTTP 429 | actual status retained; existing rate-limit handling | no synthetic semantic status |
| W13 | Generic transport failure before output | existing single bounded retry/fallback | no PR 4 budget redesign |
| W14 | `previous_response_not_found` before output | existing one repair/replay | recognized migration does not intercept |
| W15 | Client disconnect before terminal usage | drain upstream; retain usage; write nothing further | no recovery output after disconnect |
| W16 | Secret-bearing/oversized recognized frame | bounded safe code/type/message only | no raw metadata or identifiers |
| W17 | Unknown or malformed `response.failed`/`type:error` | current compatibility behavior | no PR 5 fallback semantics |
| W18 | Successful multi-turn WebSocket | unchanged frames and accounting | delivery state resets per turn; no success hot-path policy parse |
| W19 | Duplicate/replayed terminal for one response | one turn completion and one semantic owner | no duplicate billing/release |
| W20 | Handshake 401/403/426 and existing HTTP fallback | current close/fallback behavior | broad handshake redesign deferred |

## Test-first checkpoints

### Step 1 — Safe recognized WebSocket frame rendering

**Files**

- extend `internal/service/upstream_error_policy_test.go` or add a focused WebSocket policy/render test;
- modify a focused service helper file only if needed.

**RED tests**

- recognized overload `response.failed` renders only safe response/error fields;
- recognized cyber `type:error` renders only safe error fields;
- secret-bearing siblings and oversized metadata are absent;
- malformed/unknown input is not reconstructed as recognized.

Implement pure helpers first. No connection or scheduler dependencies.

### Step 2 — HTTP→upstream WSv2 terminal migration

**Files**

- modify `internal/service/openai_ws_forwarder.go`;
- extend `internal/service/openai_ws_forwarder_success_test.go` and focused protocol tests.

**RED tests**

- streaming first-frame overload returns `OpenAIUpstreamRequestError` 503 without committed SSE output;
- non-streaming first-frame overload returns typed 503 without committing the current HTTP 200 response body;
- overload after delivered output emits one safe terminal and sets `OutputStarted=true`;
- cyber policy returns typed 400;
- context-window tests, including ambiguous/conflicting-envelope rejection that blocks all broad recognition, remain green;
- unknown/fallback and successful forwarding tests remain green.

### Step 3 — Managed client ingress migration

**Files**

- modify the `sendAndRelay` boundary in `internal/service/openai_ws_forwarder.go`;
- extend `internal/service/openai_ws_forwarder_ingress_session_test.go`;
- retain `internal/service/openai_ws_ratelimit_signal_test.go` as a compatibility gate.

**RED tests**

- overload first terminal is safe, typed, and non-failover;
- delivered `response.created` with no token makes `OutputStarted=true`;
- cyber terminal is direct;
- recognized `type:error` is safe and owns the turn;
- rate-limit error still returns failover and persists state;
- previous-response repair, one transport retry, terminal write failure ownership, and disconnect drain remain green.

### Step 4 — Native passthrough lifecycle metadata

**Files**

- minimally modify `internal/service/openai_ws_v2/passthrough_relay.go`;
- extend `internal/service/openai_ws_v2/passthrough_relay_test.go` and `passthrough_relay_internal_test.go`.

**RED tests**

- `RelayTurnResult.ClientEventDelivered` reports a prior successfully delivered non-token event but excludes the current terminal write;
- it remains false when the prior write fails or the terminal is the first frame;
- delivery state is isolated across multiple turns;
- the provider-neutral classifier marks an adapter-recognized `type:error` terminal before response-ID and duplicate bookkeeping;
- a classifier-only error's top-level event ID is never promoted to a request/response ID;
- interleaved response IDs retain isolated prior-delivery snapshots through reversed terminal order;
- `StopRelay=true` exits after one transformed delivery attempt and one callback;
- duplicate terminal suppression and disconnect drain remain unchanged.

### Step 5 — Native passthrough policy adapter

**Files**

- modify `internal/service/openai_ws_v2_passthrough_adapter.go`;
- add/extend focused passthrough adapter tests.

**RED tests**

- overload and cyber terminals become typed recognized errors;
- `OutputStarted` uses relay delivery metadata, not `FirstTokenMs`;
- recognized `type:error` is safely transformed and returned directly;
- terminal write failure preserves recognized ownership;
- rate-limit error remains `UpstreamFailoverError`;
- unknown error remains compatible.

### Step 6 — Handler/account-health characterization

**Files**

- modify the pre-output `OpenAIUpstreamRequestError` return boundary in `internal/handler/openai_gateway_handler.go` so a non-empty admitted result is recorded before return;
- extend focused HTTP and WebSocket handler tests only where service-level evidence cannot prove scheduling/accounting effects.

**RED tests**

- recognized typed error performs one account acquisition, zero account switch, and no failed schedule result;
- pre-output terminal usage from HTTP→WSv2 is submitted once with `PreserveAccountHealth=true` before the handler returns;
- managed/native WebSocket terminal usage remains submitted with `PreserveAccountHealth=true`;
- failover fixture still switches under its existing bounded policy.

## Verification

Targeted commands:

```bash
go test ./internal/service -run 'Test.*WS.*(Recognized|Overload|Cyber|ContextFailed|ResponseFailed|ErrorEvent|RateLimit|PreviousResponse|ClientDisconnect|WriteFail)'
go test ./internal/service/openai_ws_v2 -run 'TestRelay.*(TurnComplete|ResponseFailed|Error|Downstream|Disconnect|Duplicate)'
go test ./internal/handler -run 'TestOpenAIWS.*(Recognized|Failover|TurnSlots)'
go test -race ./internal/service/openai_ws_v2 -run 'TestRelay.*(TurnComplete|ResponseFailed|Disconnect|Duplicate)'
go test -race ./internal/service -run 'TestOpenAIGatewayService_.*WS.*(ResponseFailed|ErrorEvent|ClientDisconnect)'
```

Hot-path guard:

```bash
go test ./internal/service -run '^$' -bench 'BenchmarkOpenAIWS(ForwarderHotPath|EventEnvelopeParse|ErrorEventFieldReuse)' -benchmem
```

Broader checks:

```bash
go test ./internal/handler
go test ./internal/service/openai_ws_v2
go test ./internal/service
go vet ./internal/handler
go vet ./internal/service ./internal/handler
git diff --check
```

Compare broad `internal/service` and combined vet failures against the recorded `origin/dev` baseline. Existing WSv2 fixture failures and lock-copy warnings are not PR 3 regressions without before/after evidence.

## Review checklist

- [ ] Recognized WebSocket facts use the shared PR 2 policy; no second WS policy table exists.
- [ ] Previous-response repair stays first; built-in overload/cyber recognition stays ahead of generic rate-limit classification.
- [ ] Legacy context ambiguity validation remains authoritative before broad recognized policy, including conflicting context/non-context envelopes.
- [ ] Classifier-only error event IDs stay separate from response/request IDs and per-response delivery state survives interleaving.
- [ ] Recognized direct errors never construct/enter `UpstreamFailoverError` or failed scheduler/account-health handling.
- [ ] `OutputStarted` reflects prior successful downstream event delivery, excludes the terminal's own write, and resets per turn.
- [ ] `FirstTokenMs` remains metric-only.
- [ ] Every recognized turn has one terminal owner and at most one safe terminal/error frame.
- [ ] Terminal write failure or disconnect cannot trigger a second recovery output.
- [ ] Safe renderers consume bounded presentation fields, not raw sibling metadata.
- [ ] WebSocket semantic facts retain `HTTPStatusKnown=false`.
- [ ] Context-window, rate-limit, transport retry, previous-response repair, disconnect drain, and unknown behavior remain compatible.
- [ ] Pre-output and post-output terminal usage/accounting remains available, is submitted once, and preserves account health.
- [ ] Non-streaming recognized failure does not commit an HTTP 200 body before the handler's direct response.
- [ ] Successful forwarding avoids new policy parsing and preserves existing frame shapes.
