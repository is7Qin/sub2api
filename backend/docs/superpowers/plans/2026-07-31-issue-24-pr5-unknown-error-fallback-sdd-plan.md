# Issue #24 PR5: Unknown-Only Database Error Fallback — SDD Plan

> **Base:** merged PR4 on `origin/dev` (`5ca76c576`).
>
> **Boundary:** preserve PR1–PR4 recognized direct semantics and bounded recovery; change only the precedence of legacy database passthrough matching and the unknown-error fallback.

## Objective

Every upstream error follows one explicit order:

```text
bounded upstream fact
  -> built-in recognized semantic policy
  -> direct safe presentation (no database lookup, retry, failover, or health mutation)
  -> unknown-only database passthrough rule
  -> safe rule-defined presentation
  -> sanitized 502 when no rule or lookup is unavailable
```

Database rules are not a second classifier for built-in semantics. Unknown matching remains bounded by the existing rule service and must fail closed.

## Contracts

Add a small response-boundary adapter that derives a bounded `UpstreamErrorFact` from a platform, observed status, and body only where an existing legacy body-only boundary has no fact. It returns the existing `RecognizedUpstreamErrorPolicy`; it never exposes raw body data.

`applyErrorPassthroughRule` and handler-level `MatchRule` calls must consult the built-in recognizer first. Recognized facts bypass the rule service entirely. Unknown facts alone may match rules. Rule miss, malformed rule data, absent service, or DB/cache refresh failure all return the caller's sanitized generic fallback; raw upstream bodies are never used as a client fallback.

No schema or migration changes are needed. `MatchRule` remains backward-compatible; nil continues to mean no usable match and is treated fail-closed at runtime.

## Integration boundaries

- `internal/service/error_passthrough_runtime.go`: central unknown-only gate for service callers.
- `internal/service/upstream_error_policy.go`: bounded response recognition adapter.
- `internal/handler/gateway_handler.go`, `openai_gateway_handler.go`, and Gemini failover fallback: recognize before direct `MatchRule`; render the safe policy when available.
- Existing service-specific recognized paths remain authoritative and are not refactored broadly.
- Streaming and WebSocket committed paths retain their current terminal renderers; the gate must not create a second terminal.

## Required behavior

| Input | Result |
|---|---|
| recognized cyber/overload/context/Anthropic direct error + conflicting rule | built-in safe status/type/message; rule service not consulted |
| unknown error + matching enabled rule | configured safe status/message and skip-monitoring behavior |
| unknown error + no match | sanitized 502/generic existing fallback |
| rule service absent or lookup failure | sanitized 502/generic existing fallback; no raw body |
| malformed rule response/custom fields | fail closed; no raw body or unsafe status/message |
| committed SSE/WebSocket output | one existing protocol terminal; no HTTP rewrite or second terminal |

## Test-first matrix

1. Runtime helper: recognized response does not call/apply a conflicting rule; unknown matching still works.
2. Generic and OpenAI failover exhaustion: recognized fact wins over a matching rule; unknown rule match remains safe; unknown miss is generic 502.
3. Gemini fallback: unknown rule behavior remains; recognized built-ins never enter rule matching.
4. Service HTTP/SSE boundaries: recognized direct semantics remain before passthrough and account-health handling.
5. Lookup failure/empty cache/malformed rule: fail closed and secrets/raw body are absent.
6. Streaming committed and uncommitted paths: safe direct response or exactly one terminal event.

## Non-goals

- Rewriting the rule repository/cache state model.
- Removing `UpstreamFailoverError` or legacy synthetic bridges.
- Changing retry/failover budgets, account health, billing, usage, or successful streaming hot paths.
- Broad cleanup of body-only legacy error handling (PR6).

## Verification

```bash
go test ./internal/service -run 'Test(ApplyErrorPassthrough|.*Unknown.*|.*Recognized.*Passthrough)'
go test ./internal/handler -run 'Test.*(Failover|Passthrough|Recognized|Unknown)'
go test ./internal/service ./internal/handler -count=1
go test -race ./internal/service ./internal/handler -run 'Test.*(Passthrough|Failover|Recognized)'
go vet ./internal/service ./internal/handler
gofmt -w <changed-go-files>
git diff --check
```
