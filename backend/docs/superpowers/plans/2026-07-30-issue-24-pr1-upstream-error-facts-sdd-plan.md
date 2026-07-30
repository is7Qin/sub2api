# Issue #24 PR 1: Upstream Error Facts and Characterization — SDD Plan

> **PR boundary:** establish a bounded, redacted upstream-error fact contract and attach it observationally at selected error boundaries. Preserve current recovery, account-health, database-rule, and client-rendering behavior.
>
> **Prerequisite:** approved design: [`2026-07-30-issue-24-upstream-error-semantics-design.md`](../specs/2026-07-30-issue-24-upstream-error-semantics-design.md).

## Scope and non-goals

This PR introduces an `UpstreamErrorFact` that describes what the upstream actually produced. It creates no new direct-return policy and makes no retry/failover decision from the fact.

It must not:

- remove the current Anthropic SSE synthetic 403 bridge;
- remove OpenAI stream synthetic 502 bridge;
- change `UpstreamFailoverError.StatusCode`, `ResponseBody`, `RetryableOnSameAccount`, or handler behavior;
- alter account health, scheduler, billing, database passthrough, or final rendering;
- parse successful SSE or WebSocket frames.

The exception is observational correctness: a fact attached to a stream error must record `HTTPStatusKnown=false`, even while the legacy bridge retains its synthetic compatibility status.

## PR-local SDD scenario matrix

All scenarios below are written as focused tests before production changes. Tests expressly distinguish the **fact** from the compatibility carrier.

| ID | Boundary / input | Required fact | Compatibility requirement |
|---|---|---|---|
| F1 | HTTP 401/403/429/500 with error JSON | source HTTP; `HTTPStatusKnown=true`; actual HTTP status retained | Existing failover error status/body unchanged |
| F2 | Anthropic HTTP 200 plus `event:error` | source SSE; `HTTPStatusKnown=false`; safe error scalar extraction | Existing synthetic 403 remains unchanged |
| F3 | OpenAI HTTP 200 `response.failed` | source SSE; `HTTPStatusKnown=false`; code/type/message extracted | Existing synthetic 502 failover behavior unchanged |
| F4 | OpenAI HTTP 200 `type:error` | source SSE; `HTTPStatusKnown=false`; safe fields extracted | Existing output/fallback behavior unchanged |
| F5 | OpenAI WS `response.failed`/`type:error` | source WebSocket; `HTTPStatusKnown=false` | Existing close/fallback behavior unchanged |
| F6 | Failed OpenAI WS handshake with HTTP response | source WebSocket; actual handshake status known when available | Existing handling unchanged |
| F7 | DNS/TCP/TLS/proxy transport error | source Transport; status unknown; message bounded/redacted | Existing transport failover behavior unchanged |
| F8 | Malformed body/event | safe unknown fact, no panic | Existing generic behavior unchanged |
| F9 | Oversized body/event | all fact fields bounded; no raw body retained | Existing stream/body limits unchanged |
| F10 | Secret-bearing body/event/error text | no credentials, cookies, auth values, URLs with credentials, session/install IDs, or opaque metadata in any fact field | Existing client output unchanged |
| F11 | Successful SSE / WS frames | fact parser not invoked | forwarded bytes unchanged |
| F12 | Terminal ordering fixtures | current ordering behavior captured, not reinterpreted | no terminal policy changes |

### Required test-first checkpoints

1. **Parser contract tests fail** because no fact type/parser exists.
2. **Boundary characterization tests fail** because facts are not attached where the existing compatibility errors are created.
3. Each production edit is the minimum needed to make the corresponding focused test pass.
4. Existing behavior tests must continue to pass before moving to the next boundary.

## Proposed data contract

Create `internal/service/upstream_error_fact.go`:

```go
type UpstreamErrorSource string

const (
    UpstreamErrorSourceHTTP              UpstreamErrorSource = "http"
    UpstreamErrorSourceSSE               UpstreamErrorSource = "sse"
    UpstreamErrorSourceWebSocket         UpstreamErrorSource = "websocket"
    UpstreamErrorSourceTransport         UpstreamErrorSource = "transport"
    UpstreamErrorSourceStreamTermination UpstreamErrorSource = "stream_termination"
)

type UpstreamErrorScope string

const (
    UpstreamErrorScopeRequest  UpstreamErrorScope = "request"
    UpstreamErrorScopeAccount  UpstreamErrorScope = "account"
    UpstreamErrorScopeModel    UpstreamErrorScope = "model"
    UpstreamErrorScopeProvider UpstreamErrorScope = "provider"
    UpstreamErrorScopeUnknown  UpstreamErrorScope = "unknown"
)

type UpstreamErrorFact struct {
    Provider          string
    Source            UpstreamErrorSource
    HTTPStatusKnown   bool
    HTTPStatus        int
    ProviderCode      string
    ProviderType      string
    SafeMessage       string
    RequestID         string
    RetryAfter        string
    ScopeHint         UpstreamErrorScope
    InternalMatchText string
}
```

The fact has no raw body, response headers, retry disposition, account-health action, or client presentation. PR 1 defaults `ScopeHint` to `unknown`.

Use bounded scalar and body limits. Initial values are implementation details, but all inputs must be copied/sliced to a small fact-parser limit before parsing; every stored scalar and match text is capped. `InternalMatchText` is a derived bounded concatenation of safe scalar fields, never a raw upstream body.

Provide narrow parser functions:

```go
func ParseHTTPUpstreamErrorFact(provider string, resp *http.Response, body []byte) UpstreamErrorFact
func ParseAnthropicSSEErrorFact(provider string, data []byte, requestID string) UpstreamErrorFact
func ParseOpenAIJSONErrorFact(provider string, source UpstreamErrorSource, payload []byte, requestID string) UpstreamErrorFact
func ParseOpenAIWebSocketErrorFact(provider string, payload []byte, requestID string) UpstreamErrorFact
func ParseTransportErrorFact(provider string, err error) UpstreamErrorFact
```

For PR 1, names may be refined to match nearby Go idiom, but every parser must preserve the matrix invariants.

## Detailed implementation steps

### Step 1 — Parser-local tests and fact primitives

**Files**

- Create `internal/service/upstream_error_fact.go`
- Create `internal/service/upstream_error_fact_test.go`

**Tests to write first**

- actual HTTP status is known and preserved;
- Anthropic SSE fact has no HTTP status;
- OpenAI `response.failed` extracts known scalar fields;
- OpenAI `type:error` extracts known scalar fields;
- malformed JSON produces an unknown fact without panic;
- oversized payload does not exceed fact limits;
- secret-bearing input is redacted;
- transport fact has no HTTP status.

**Implementation**

- Reuse existing sanitizers where safe and add fact-specific redaction only when existing helpers lack a required rule.
- Extract only known JSON scalar paths:
  - Anthropic: `error.type`, `error.message`;
  - OpenAI Responses: `response.error.code`, `response.error.type`, `response.error.message`;
  - OpenAI error: `error.code`, `error.type`, `error.message`;
  - fallback top-level known scalar values only.
- Read `Retry-After` only from an HTTP response header and bound it.
- Do not infer status from semantic code, type, or message.

**Focused command**

```bash
go test ./internal/service -run 'Test(UpstreamErrorFact|Parse.*UpstreamErrorFact)'
```

### Step 2 — Attach fact to legacy `UpstreamFailoverError`

**Files**

- Modify `internal/service/gateway_service.go`
- Extend `internal/service/gateway_streaming_test.go`
- Extend `internal/handler/failover_loop_test.go` only if a compatibility assertion cannot be made at service level

**Implementation**

- Add a private `upstreamFact *UpstreamErrorFact` attachment to `UpstreamFailoverError`.
- If external/internal consumers need access, provide a defensive value-returning accessor, not a mutable pointer.
- Do not read the attachment in handler recovery or rendering code.
- Ensure legacy fields remain authoritative for all existing code during PR 1.

**Tests to write first**

- fact attachment does not overwrite HTTP compatibility status;
- SSE fact status unknown does not overwrite Anthropic synthetic 403;
- OpenAI terminal fact status unknown does not overwrite synthetic 502;
- legacy `FailoverState` continues to use only existing status/retry fields.

### Step 3 — Anthropic/native SSE boundary characterization

**Files**

- Modify `internal/service/gateway_service.go` around `sseStreamErrorEventError` and `handleStreamingResponse`
- Extend `internal/service/gateway_streaming_test.go`

**Tests to write first**

- HTTP 200 + `event:error` attaches an SSE fact with unknown HTTP status;
- same event still produces legacy `UpstreamFailoverError.StatusCode == 403`;
- malformed/oversized/secret-bearing SSE error payloads produce safe facts;
- normal Anthropic `message_start`, content delta, and `message_stop` do not invoke error parsing;
- characterize error/terminal ordering without changing output.

**Implementation**

- Parse only after the existing SSE processor identifies `sseStreamErrorEventError`.
- Keep `sseStreamErrorEventError` and all client output paths intact.
- Attach the parsed fact only where the synthetic failover bridge is constructed.

### Step 4 — OpenAI HTTP/SSE terminal boundary characterization

**Files**

- Modify `internal/service/openai_gateway_service.go` in existing `response.failed` and `type:error` branches
- Extend existing OpenAI response-failed and hot-path tests

**Tests to write first**

- `response.failed` over HTTP 200 produces SSE fact with unknown HTTP status;
- `type:error` extracts safe fields;
- actual OpenAI HTTP error produces known actual HTTP status;
- current `newOpenAIStreamFailoverError` remains 502 in compatibility mode;
- context-window typed direct error behavior remains unchanged;
- successful normal response events and `response.completed` do not invoke parser;
- malformed, oversized, secret-bearing, and terminal-order cases are safe and non-policy-changing.

**Implementation**

- Call parsers only in the existing terminal/error branch.
- Preserve `openAIStreamFailedEventShouldFailover`, `newOpenAIStreamFailoverError`, and `OpenAIUpstreamRequestError` decisions exactly.
- Do not query database rules or create new direct-return behavior in this PR.

### Step 5 — Responses WebSocket and transport characterization

**Files**

- Modify `internal/service/openai_ws_forwarder.go`
- Modify `internal/service/openai_upstream_transport_error.go`
- Extend `internal/service/openai_ws_forwarder_test.go`
- Extend `internal/service/openai_ws_forwarder_hotpath_optimization_test.go`
- Extend `internal/service/openai_upstream_transport_error_test.go`

**Tests to write first**

- WS `response.failed` and WS `type:error` have status unknown;
- known failed handshake status is preserved only when a handshake HTTP response supplied it;
- transport fact has status unknown and strips/redacts network details;
- normal successful WS frames never invoke the error parser;
- benchmark or existing hot-path test demonstrates no parse call on success.

**Implementation**

- Guard parsing behind existing event-type/error/terminal detection.
- Preserve raw forwarding, fallback, close, accounting, and `wroteDownstream` behavior.
- Use the existing sanitized OpenAI diagnostic helper before assigning transport message fields.

### Step 6 — Verification, baseline, and review

**Targeted tests**

```bash
go test ./internal/service -run 'Test(UpstreamErrorFact|Parse.*UpstreamErrorFact|.*Fact)'
go test ./internal/service -run 'TestHandleStreamingResponse_SSEErrorEvent|Test.*Streaming.*Failover'
go test ./internal/service -run 'TestOpenAI.*ResponseFailed|TestOpenAI.*Transport'
go test ./internal/service -run 'TestOpenAIWS.*Fact|TestOpenAIWSError|TestOpenAIWSSuccessful'
go test ./internal/handler -run 'Test.*Failover|Test.*StreamingAwareError'
```

**Benchmark**

```bash
go test ./internal/service -run '^$' -bench 'BenchmarkOpenAIWS(ForwarderHotPath|EventEnvelopeParse)' -benchmem
```

**Broader checks**

```bash
go test ./internal/service
go test ./internal/handler
go test ./...
golangci-lint run ./internal/service/... ./internal/handler/...
```

Run `gofmt` only after implementation edits. Compare broad package failures to the pre-recorded WSv2 baseline and report only proven new failures as PR regressions.

## Review checklist

- [ ] No policy code reads `UpstreamErrorFact` in PR 1.
- [ ] No status inference writes an invented value into `HTTPStatus`.
- [ ] Every fact field is bounded and redacted.
- [ ] No raw error body, request body, or headers are retained in a fact.
- [ ] Existing synthetic 403 and 502 compatibility bridges still behave exactly as before.
- [ ] Parser invocation is guarded out of normal SSE/WS forwarding paths.
- [ ] Context-window direct error and current account-health behavior remain unchanged.
- [ ] Targeted suites pass and broader results are compared against baseline.
