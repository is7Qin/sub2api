# Issue #24 PR6 Legacy Error Boundary Cleanup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Complete Issue #24 by routing the remaining Generic HTTP, Anthropic SSE, ops-rule, and failover-exhaustion boundaries through bounded facts and safe final presentations.

**Architecture:** Add one service-level final-presentation resolver that accepts only `UpstreamErrorFact`, built-in policy, and the existing unknown-rule service. Move Generic HTTP recognition before health mutation, remove Anthropic SSE’s synthetic 403, and carry the selected candidate’s fact through exhaustion so each existing protocol renderer receives one finalized presentation.

**Tech Stack:** Go 1.24, Gin, `net/http`, `gjson`, Testify, existing `ErrorPassthroughService`, existing Issue #24 fact/policy/recovery types.

## Global Constraints

- Work on `fix/issue-24-legacy-boundary-cleanup`, based on `origin/dev` commit `63bb740a25fe33b87e1db9dbd6e99e695dcf8134`.
- Follow `docs/superpowers/specs/2026-07-31-issue-24-pr6-legacy-boundary-cleanup-design.md`.
- Use test-first development: add one failing behavior test, verify the expected failure, then make the minimum production change.
- Never expose a raw upstream body, unknown provider message, credential, URL, stack trace, session/thread/installation ID, or unbounded metadata to clients.
- Unknown unmatched real upstream 4xx keeps its real 4xx status with a fixed safe message.
- Unknown unmatched upstream 5xx, status-unknown SSE/WS, transport/read, malformed, and missing facts return generic 502.
- Recognized direct errors run before account health, failover, account exclusion, and database rules.
- Database rules and `skip_monitoring` apply only to unknown bounded facts.
- Keep existing protocol renderers and successful streaming hot paths; do not add parsing to successful frames.
- Preserve the private `SanitizedClientResponse` boundary; do not widen it to arbitrary upstream payloads.
- Do not change billing, usage submission, pricing, or unrelated scheduler behavior.
- Do not remove or migrate seeded database rows in this PR.
- Do not push without explicit user instruction.

---

## File Structure

### New files

- `internal/service/upstream_error_final_presentation.go` — owns safe final resolution for recognized, structured recoverable, rule-matched unknown, and unmatched unknown facts.
- `internal/service/upstream_error_final_presentation_test.go` — table tests for status policy, rule precedence, bounded messages, and secret-bearing facts.
- `internal/service/ops_upstream_context_fact_test.go` — focused tests for unknown-only `skip_monitoring` behavior.
- `internal/handler/upstream_error_exhaustion_test.go` — handler-level tests for candidate precedence and protocol-specific exhaustion output.

### Modified files

- `internal/service/upstream_error_policy.go` — retain fact data in `UpstreamErrorCandidate`; do not turn an unknown candidate into an approved public message.
- `internal/service/error_passthrough_runtime.go` — make legacy rule presentation use `fact.SafeMessage`, never `ExtractUpstreamErrorMessage(responseBody)`.
- `internal/service/error_passthrough_runtime_test.go` — characterize bounded matched-rule output and safe Generic HTTP unknown fallbacks.
- `internal/service/gateway_service.go` — move Generic HTTP recognition before health handling; remove raw 400 output and Anthropic SSE synthetic 403.
- `internal/service/upstream_error_fact_test.go` — replace the synthetic-403 characterization with the final status-unknown contract.
- `internal/service/ops_upstream_context.go` — attach/derive bounded facts for ops matching and call `MatchUnknownRule` only.
- `internal/handler/failover_loop.go` — retain the bounded fact inside candidates and expose the selected candidate unchanged.
- `internal/handler/failover_loop_test.go` — assert candidate facts, rank, tie ordering, and success clearing.
- `internal/handler/gateway_handler.go` — resolve retained candidates safely on both error exhaustion and selection exhaustion.
- `internal/handler/gateway_handler_chat_completions.go` — replace generic exhaustion text with candidate resolution when an upstream candidate exists.
- `internal/handler/gateway_handler_responses.go` — use recovery candidate resolution and emit JSON/`response.failed` through existing writers.
- `internal/handler/openai_gateway_handler.go` — ensure status-only candidates pass through final resolution rather than direct rendering.
- `internal/handler/gemini_v1beta_handler.go` — use selected candidate for both failure-exhaustion and selection-exhaustion paths.

---

### Task 1: Add the bounded final-presentation resolver

**Files:**
- Create: `internal/service/upstream_error_final_presentation.go`
- Create: `internal/service/upstream_error_final_presentation_test.go`
- Modify: `internal/service/upstream_error_policy.go:50-81`
- Verify all `UpstreamErrorCandidate{...}` literals with `rg -n 'UpstreamErrorCandidate\\s*\\{' internal` and update every production/test literal to populate `Fact` where the fact is available.

**Interfaces:**
- Consumes: `UpstreamErrorFact`, `ResolveUpstreamRecoveryPolicy(UpstreamErrorFact) (UpstreamRecoveryPolicy, bool)`, `ErrorPassthroughService.MatchUnknownRule(UpstreamErrorFact)`.
- Produces:
  - `type FinalUpstreamErrorResolution struct { Presentation UpstreamClientPresentation; RuleMatched bool; SkipMonitoring bool }`
  - `func ResolveFinalUpstreamError(fact UpstreamErrorFact, rules *ErrorPassthroughService) FinalUpstreamErrorResolution`
  - `UpstreamErrorCandidate.Fact UpstreamErrorFact`
- `ResolveFinalUpstreamError` is the only final rule-resolution entry point. Callers must not call `MatchUnknownRule` and then call the resolver again for the same fact.

- [ ] **Step 1: Write failing resolver tests**

Create `internal/service/upstream_error_final_presentation_test.go` with table coverage equivalent to:

```go
package service

import (
    "net/http"
    "strings"
    "testing"

    "github.com/Wei-Shaw/sub2api/internal/model"
    "github.com/stretchr/testify/require"
)

func TestResolveFinalUpstreamError(t *testing.T) {
    responseCode := http.StatusTeapot
    customMessage := "administrator-approved message"
    rules := &ErrorPassthroughService{}
    rules.setLocalCache([]*model.ErrorPassthroughRule{{
        Enabled:         true,
        Priority:        1,
        Platforms:       []string{PlatformOpenAI},
        Keywords:        []string{"vendor_failure"},
        MatchMode:       model.MatchModeAny,
        PassthroughCode: false,
        ResponseCode:    &responseCode,
        PassthroughBody: false,
        CustomMessage:   &customMessage,
        SkipMonitoring:  true,
    }})

    tests := []struct {
        name       string
        fact       UpstreamErrorFact
        rules      *ErrorPassthroughService
        wantStatus int
        wantCode   string
        wantType   string
        wantMsg    string
        wantSkip   bool
    }{
        {
            name: "recognized overload bypasses conflicting rules",
            fact: UpstreamErrorFact{
                Provider: PlatformOpenAI, Source: UpstreamErrorSourceHTTP,
                HTTPStatusKnown: true, HTTPStatus: http.StatusServiceUnavailable,
                ProviderCode: "server_is_overloaded", ProviderType: "service_unavailable_error",
                SafeMessage: "retry later", InternalMatchText: "openai server_is_overloaded retry later",
            },
            rules: rules, wantStatus: http.StatusServiceUnavailable,
            wantCode: "server_is_overloaded", wantType: "service_unavailable_error", wantMsg: "retry later",
        },
        {
            name: "structured rate limit remains authoritative",
            fact: UpstreamErrorFact{
                Provider: PlatformOpenAI, Source: UpstreamErrorSourceHTTP,
                HTTPStatusKnown: true, HTTPStatus: http.StatusTooManyRequests,
                ProviderCode: "rate_limit_exceeded", ProviderType: "rate_limit_error",
                SafeMessage: "quota exceeded", InternalMatchText: "openai rate_limit_exceeded quota exceeded",
            },
            wantStatus: http.StatusTooManyRequests, wantCode: "rate_limit_exceeded",
            wantType: "rate_limit_error", wantMsg: "quota exceeded",
        },
        {
            name: "unknown matched custom-code rule uses approved presentation",
            fact: UpstreamErrorFact{
                Provider: PlatformOpenAI, Source: UpstreamErrorSourceHTTP,
                HTTPStatusKnown: true, HTTPStatus: http.StatusBadGateway,
                ProviderCode: "vendor_failure", SafeMessage: "bounded vendor text",
                InternalMatchText: "openai vendor_failure bounded vendor text",
            },
            rules: rules, wantStatus: http.StatusTeapot, wantType: "upstream_error",
            wantMsg: customMessage, wantSkip: true,
        },
        {
            name: "unknown real 400 keeps safe 400",
            fact: UpstreamErrorFact{
                Provider: PlatformAnthropic, Source: UpstreamErrorSourceHTTP,
                HTTPStatusKnown: true, HTTPStatus: http.StatusBadRequest,
                SafeMessage: "secret upstream detail",
            },
            wantStatus: http.StatusBadRequest, wantType: "upstream_error", wantMsg: "Upstream request failed",
        },
        {
            name: "unknown real 503 becomes 502",
            fact: UpstreamErrorFact{
                Provider: PlatformOpenAI, Source: UpstreamErrorSourceHTTP,
                HTTPStatusKnown: true, HTTPStatus: http.StatusServiceUnavailable,
                SafeMessage: "unknown provider failure",
            },
            wantStatus: http.StatusBadGateway, wantType: "upstream_error", wantMsg: "Upstream request failed",
        },
        {
            name: "status unknown sse becomes 502",
            fact: UpstreamErrorFact{
                Provider: PlatformAnthropic, Source: UpstreamErrorSourceSSE,
                SafeMessage: "unknown stream failure",
            },
            wantStatus: http.StatusBadGateway, wantType: "upstream_error", wantMsg: "Upstream request failed",
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            got := ResolveFinalUpstreamError(tt.fact, tt.rules)
            require.Equal(t, tt.wantStatus, got.Presentation.HTTPStatus)
            require.Equal(t, tt.wantCode, got.Presentation.ErrorCode)
            require.Equal(t, tt.wantType, got.Presentation.ErrorType)
            require.Equal(t, tt.wantMsg, got.Presentation.Message)
            require.Equal(t, tt.wantSkip, got.SkipMonitoring)
            wantRule := tt.name == "unknown matched custom-code rule uses approved presentation"
            require.Equal(t, wantRule, got.RuleMatched)
        })
    }
}

func TestResolveFinalUpstreamErrorMatchedPassthroughCodeUsesRealStatusOnly(t *testing.T) {
    customMessage := "administrator-approved message"
    rules := &ErrorPassthroughService{}
    rules.setLocalCache([]*model.ErrorPassthroughRule{{
        Enabled: true, Priority: 1, Platforms: []string{PlatformOpenAI},
        Keywords: []string{"vendor_failure"}, MatchMode: model.MatchModeAny,
        PassthroughCode: true, PassthroughBody: false,
        CustomMessage: &customMessage,
    }})

    known := ResolveFinalUpstreamError(UpstreamErrorFact{
        Provider: PlatformOpenAI, Source: UpstreamErrorSourceHTTP,
        HTTPStatusKnown: true, HTTPStatus: http.StatusTooManyRequests,
        ProviderCode: "vendor_failure", InternalMatchText: "vendor_failure",
    }, rules)
    require.Equal(t, http.StatusTooManyRequests, known.Presentation.HTTPStatus)
    require.True(t, known.RuleMatched)

    unknown := ResolveFinalUpstreamError(UpstreamErrorFact{
        Provider: PlatformOpenAI, Source: UpstreamErrorSourceSSE,
        ProviderCode: "vendor_failure", InternalMatchText: "vendor_failure",
    }, rules)
    require.Equal(t, http.StatusBadGateway, unknown.Presentation.HTTPStatus)
    require.True(t, unknown.RuleMatched)
}

func TestResolveFinalUpstreamErrorMatchedPassthroughBodyUsesBoundedFactMessage(t *testing.T) {
    rules := &ErrorPassthroughService{}
    rules.setLocalCache([]*model.ErrorPassthroughRule{{
        Enabled: true, Priority: 1, Platforms: []string{PlatformOpenAI},
        Keywords: []string{"vendor_failure"}, MatchMode: model.MatchModeAny,
        PassthroughCode: true, PassthroughBody: true,
    }})
    fact := ParseOpenAIJSONErrorFact(PlatformOpenAI, UpstreamErrorSourceHTTP, []byte(
        `{"error":{"code":"vendor_failure","message":"Bearer sk-secret https://internal.example/ `+
            strings.Repeat("x", upstreamErrorFactMaxScalarBytes*2)+`"}}`,
    ), "")
    fact.HTTPStatusKnown = true
    fact.HTTPStatus = http.StatusBadRequest

    got := ResolveFinalUpstreamError(fact, rules)

    require.Equal(t, http.StatusBadRequest, got.Presentation.HTTPStatus)
    require.LessOrEqual(t, len(got.Presentation.Message), upstreamErrorFactMaxScalarBytes)
    require.NotContains(t, got.Presentation.Message, "sk-secret")
    require.NotContains(t, got.Presentation.Message, "internal.example")
}
```

- [ ] **Step 2: Run the new tests and verify RED**

Run:

```bash
go test ./internal/service -run 'TestResolveFinalUpstreamError' -count=1
```

Expected: build failure because `FinalUpstreamErrorResolution` and `ResolveFinalUpstreamError` do not exist.

- [ ] **Step 3: Implement the resolver**

Create `internal/service/upstream_error_final_presentation.go`. The resolver must first return recognized direct semantics, then query `MatchUnknownRule` exactly once for an unknown fact, set `RuleMatched: true` only when that query returns a rule, and otherwise return `safeUnknownUpstreamPresentation`.

```go
package service

import "net/http"

const genericUpstreamFailureMessage = "Upstream request failed"

type FinalUpstreamErrorResolution struct {
    Presentation  UpstreamClientPresentation
    RuleMatched   bool
    SkipMonitoring bool
}

func ResolveFinalUpstreamError(fact UpstreamErrorFact, rules *ErrorPassthroughService) FinalUpstreamErrorResolution {
    if policy, ok := ResolveUpstreamRecoveryPolicy(fact); ok && policy.CandidateRank == UpstreamCandidateStructured {
        return FinalUpstreamErrorResolution{Presentation: policy.Presentation}
    }

    fallback := safeUnknownUpstreamPresentation(fact)
    if rules == nil {
        return FinalUpstreamErrorResolution{Presentation: fallback}
    }
    rule := rules.MatchUnknownRule(fact)
    if rule == nil {
        return FinalUpstreamErrorResolution{Presentation: fallback}
    }

    presentation := fallback
    if rule.PassthroughCode {
        // A rule can preserve only a real upstream HTTP status. Status-unknown
        // SSE/WS/transport facts retain the safe 502 fallback.
        if fact.HTTPStatusKnown && fact.HTTPStatus >= 400 && fact.HTTPStatus <= 599 {
            presentation.HTTPStatus = fact.HTTPStatus
        }
    } else if rule.ResponseCode != nil {
        presentation.HTTPStatus = *rule.ResponseCode
    }
    if rule.PassthroughBody && fact.SafeMessage != "" {
        presentation.Message = sanitizeUpstreamErrorFactScalar(fact.SafeMessage, upstreamErrorFactMaxScalarBytes)
    } else if !rule.PassthroughBody && rule.CustomMessage != nil {
        presentation.Message = sanitizeUpstreamErrorFactScalar(*rule.CustomMessage, upstreamErrorFactMaxScalarBytes)
    }
    if presentation.Message == "" {
        presentation.Message = genericUpstreamFailureMessage
    }
    return FinalUpstreamErrorResolution{
        Presentation:  presentation,
        RuleMatched:   true,
        SkipMonitoring: rule.SkipMonitoring,
    }
}

func safeUnknownUpstreamPresentation(fact UpstreamErrorFact) UpstreamClientPresentation {
    status := http.StatusBadGateway
    if fact.HTTPStatusKnown && fact.HTTPStatus >= http.StatusBadRequest && fact.HTTPStatus < http.StatusInternalServerError {
        status = fact.HTTPStatus
    }
    return UpstreamClientPresentation{
        HTTPStatus: status,
        ErrorType:  "upstream_error",
        Message:    genericUpstreamFailureMessage,
    }
}
```

Modify `UpstreamErrorCandidate` and its constructor in `internal/service/upstream_error_policy.go`:

```go
type UpstreamErrorCandidate struct {
    Fact         UpstreamErrorFact
    Presentation UpstreamClientPresentation
    Rank         UpstreamCandidateRank
}

func NewUpstreamErrorCandidate(fact UpstreamErrorFact, rank UpstreamCandidateRank) *UpstreamErrorCandidate {
    presentation := normalizedUpstreamClientPresentation(fact)
    if presentation.HTTPStatus == 0 {
        if fact.HTTPStatusKnown && fact.HTTPStatus > 0 {
            presentation.HTTPStatus = fact.HTTPStatus
        } else {
            presentation.HTTPStatus = http.StatusBadGateway
        }
    }
    if presentation.ErrorType == "" {
        presentation.ErrorType = "api_error"
    }
    if presentation.Message == "" {
        presentation.Message = genericUpstreamFailureMessage
    }
    return &UpstreamErrorCandidate{Fact: fact, Presentation: presentation, Rank: rank}
}
```

- [ ] **Step 4: Run resolver and policy tests and verify GREEN**

Run:

```bash
go test ./internal/service -run 'Test(ResolveFinalUpstreamError|RecognizeUpstreamErrorFact|ResolveUpstreamRecoveryPolicy|UpstreamErrorCandidate)' -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit Task 1**

Before committing, run `rg -n 'UpstreamErrorCandidate\\s*\\{' internal` and include every file containing a direct literal in the commit. The constructor itself remains the preferred creation path.

```bash
git add internal/service/upstream_error_final_presentation.go internal/service/upstream_error_final_presentation_test.go internal/service/upstream_error_policy.go internal/handler/failover_loop_test.go
git commit -m "refactor(gateway): resolve final upstream presentations"
```

---

### Task 2: Remove raw-body client presentation from service runtime

**Files:**
- Modify: `internal/service/error_passthrough_runtime.go:30-97`
- Modify: `internal/service/error_passthrough_runtime_test.go:20-417`

**Interfaces:**
- Consumes: `ResolveFinalUpstreamError(UpstreamErrorFact, *ErrorPassthroughService)` from Task 1.
- Produces: legacy `applyErrorPassthroughRule` behavior that never calls `ExtractUpstreamErrorMessage` for client output.

- [ ] **Step 1: Add failing tests for bounded matched output and unknown fallback**

Append tests that assert:

```go
func TestApplyErrorPassthroughRule_PassthroughBodyUsesFactSafeMessage(t *testing.T) {
    gin.SetMode(gin.TestMode)
    rec := httptest.NewRecorder()
    c, _ := gin.CreateTestContext(rec)
    ruleSvc := &ErrorPassthroughService{}
    ruleSvc.setLocalCache([]*model.ErrorPassthroughRule{{
        Enabled: true, Priority: 1, Platforms: []string{PlatformOpenAI},
        Keywords: []string{"vendor_failure"}, MatchMode: model.MatchModeAny,
        PassthroughCode: true, PassthroughBody: true,
    }})
    BindErrorPassthroughService(c, ruleSvc)
    body := []byte(`{"error":{"code":"vendor_failure","message":"Authorization: Bearer sk-private https://internal.example/path"}}`)

    status, errType, message, matched := applyErrorPassthroughRule(
        c, PlatformOpenAI, http.StatusBadRequest, body,
        http.StatusBadGateway, "upstream_error", "Upstream request failed",
    )

    require.True(t, matched)
    require.Equal(t, http.StatusBadRequest, status)
    require.Equal(t, "upstream_error", errType)
    require.NotContains(t, message, "sk-private")
    require.NotContains(t, message, "internal.example")
    require.LessOrEqual(t, len(message), upstreamErrorFactMaxScalarBytes)
}
```

Update existing expectations only where they encode raw provider message output rather than a configured custom message.

- [ ] **Step 2: Run the focused test and verify RED**

Run:

```bash
go test ./internal/service -run 'TestApplyErrorPassthroughRule_PassthroughBodyUsesFactSafeMessage' -count=1
```

Expected: FAIL because the legacy helper returns `ExtractUpstreamErrorMessage(responseBody)` and exposes unredacted content.

- [ ] **Step 3: Route the legacy helper through bounded resolution**

In `applyErrorPassthroughRule`, keep the existing return contract for callers but use exactly one match operation. Preserve the caller-provided default when neither a recognized semantic nor an unknown rule applies; terminal boundaries migrate to `ResolveFinalUpstreamError` in Tasks 3 and 5.

```go
fact := NewLegacyUpstreamErrorFact(platform, upstreamStatus, responseBody)
if policy, recognized := RecognizeUpstreamErrorFact(fact); recognized {
    presentation := policy.Presentation
    if presentation.HTTPStatus > 0 {
        status = presentation.HTTPStatus
    }
    if presentation.ErrorType != "" {
        errType = presentation.ErrorType
    }
    if presentation.Message != "" {
        errMsg = presentation.Message
    }
    return status, errType, errMsg, true
}

resolved := ResolveFinalUpstreamError(fact, svc)
if !resolved.RuleMatched {
    return status, errType, errMsg, false
}
if resolved.SkipMonitoring {
    c.Set(OpsSkipPassthroughKey, true)
}
presentation := resolved.Presentation
return presentation.HTTPStatus, presentation.ErrorType, presentation.Message, true
```

The resolver performs the single unknown-rule query and reports it through `RuleMatched`. Do not call `MatchUnknownRule` separately in this helper or add a new raw-body extraction path.

Delete the client-facing call to:

```go
ExtractUpstreamErrorMessage(responseBody)
```

Do not change absent-service semantics in this task; final unknown fallback is applied by migrated terminal boundaries in Tasks 3 and 5.

- [ ] **Step 4: Run runtime tests and verify GREEN**

Run:

```bash
go test ./internal/service -run 'Test(ErrorPassthroughService_MatchUnknownRule|ApplyErrorPassthroughRule|GatewayHandleErrorResponse|OpenAIHandleErrorResponse|GeminiWriteGeminiMappedError)' -count=1
```

Expected: PASS with no raw secret or URL in client messages.

- [ ] **Step 5: Commit Task 2**

```bash
git add internal/service/error_passthrough_runtime.go internal/service/error_passthrough_runtime_test.go
git commit -m "fix(gateway): bound legacy passthrough messages"
```

---

### Task 3: Move Generic HTTP recognition before health mutation and remove raw HTTP 400

**Files:**
- Modify: `internal/service/gateway_service.go:8039-8192`
- Modify: `internal/service/ops_upstream_context.go:122-227` to introduce the fact-aware ops event contract before Generic HTTP starts attaching facts.
- Modify: `internal/service/ops_upstream_context_fact_test.go` for recognized/unknown rule precedence.
- Modify: `internal/service/error_passthrough_runtime_test.go` for untagged response-safety cases.
- Modify: `internal/service/gateway_multiplatform_test.go` for the health-ordering case using its complete `mockAccountRepoForPlatform` fixture (`//go:build unit`).

**Interfaces:**
- Consumes: `ResolveFinalUpstreamError`, `ParseHTTPUpstreamErrorFact`, `appendOpsUpstreamError`, and the existing Generic Gateway JSON envelope.
- Produces: Generic HTTP direct return before `RateLimitService.HandleUpstreamError`, while still recording one bounded fact-aware ops event, and safe unknown terminal output.
- Testing strategy: do not alter the production `GatewayService.rateLimitService *RateLimitService` field solely for injection. Put the health-ordering regression in the existing unit-tagged fixture, configure the real `RateLimitService` with `mockAccountRepoForPlatform`, and assert its account mutation counters/state remain unchanged. Keep the unknown 400/503 renderer tests untagged because they use `GatewayService{rateLimitService:nil}` and exercise no health dependency.

- [ ] **Step 1: Add failing Generic HTTP boundary tests**

Add tests for these concrete inputs:

```go
// Add these fields to mockAccountRepoForPlatform in gateway_multiplatform_test.go:
//
//     setErrorCalls       int
//     setSchedulableCalls int
//
// Then increment them in the existing methods:
//
//     func (m *mockAccountRepoForPlatform) SetError(context.Context, int64, string) error {
//         m.setErrorCalls++
//         return nil
//     }
//
//     func (m *mockAccountRepoForPlatform) SetSchedulable(context.Context, int64, bool) error {
//         m.setSchedulableCalls++
//         return nil
//     }
//
// Construct a real RateLimitService with this complete unit-test repository.
func TestGatewayHandleErrorResponse_RecognizedDirectBeforeHealthAndRules(t *testing.T) {
    gin.SetMode(gin.TestMode)
    rec := httptest.NewRecorder()
    c, _ := gin.CreateTestContext(rec)
    c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
    body := []byte(`{"error":{"code":"cyber_policy","type":"invalid_request_error","message":"policy rejected"}}`)
    resp := &http.Response{
        StatusCode: http.StatusUnauthorized,
        Header: http.Header{"x-request-id": []string{"req-policy"}},
        Body: io.NopCloser(bytes.NewReader(body)),
    }
    conflictingCode := http.StatusTeapot
    conflictingMessage := "database must not win"
    rules := &ErrorPassthroughService{}
    rules.setLocalCache([]*model.ErrorPassthroughRule{{
        Enabled: true, Priority: 1, Platforms: []string{PlatformAnthropic},
        Keywords: []string{"cyber_policy"}, MatchMode: model.MatchModeAny,
        ResponseCode: &conflictingCode, CustomMessage: &conflictingMessage, SkipMonitoring: true,
    }})
    BindErrorPassthroughService(c, rules)
    account := &Account{ID: 901, Platform: PlatformAnthropic, Type: AccountTypeAPIKey}
    accountRepo := &mockAccountRepoForPlatform{
        accountsByID: map[int64]*Account{account.ID: account},
    }
    rateLimits := NewRateLimitService(accountRepo, nil, testConfig(), nil, nil)
    svc := &GatewayService{rateLimitService: rateLimits}

    _, err := svc.handleErrorResponse(context.Background(), resp, c, account)

    require.Error(t, err)
    require.NotErrorAs(t, err, new(*UpstreamFailoverError))
    require.Zero(t, accountRepo.setErrorCalls)
    require.Zero(t, accountRepo.setSchedulableCalls)
    require.Equal(t, http.StatusBadRequest, rec.Code)
    require.Equal(t, "invalid_request_error", gjson.GetBytes(rec.Body.Bytes(), "error.type").String())
    require.Equal(t, "policy rejected", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
    require.NotContains(t, rec.Body.String(), conflictingMessage)
    _, skip := c.Get(OpsSkipPassthroughKey)
    require.False(t, skip)
    rawEvents, exists := c.Get(OpsUpstreamErrorsKey)
    require.True(t, exists)
    events := rawEvents.([]*OpsUpstreamErrorEvent)
    require.Len(t, events, 1)
    require.NotNil(t, events[0].UpstreamFact)
    require.Equal(t, "cyber_policy", events[0].UpstreamFact.ProviderCode)
    require.True(t, IsResponseCommitted(c))
}

func TestGatewayHandleErrorResponse_Unknown400UsesSafeEnvelope(t *testing.T) {
    gin.SetMode(gin.TestMode)
    rec := httptest.NewRecorder()
    c, _ := gin.CreateTestContext(rec)
    body := []byte(`{"error":{"message":"bad request"},"access_token":"secret-token","debug":"internal"}`)
    resp := &http.Response{
        StatusCode: http.StatusBadRequest,
        Header: http.Header{},
        Body: io.NopCloser(bytes.NewReader(body)),
    }

    _, err := (&GatewayService{}).handleErrorResponse(
        context.Background(), resp, c,
        &Account{ID: 902, Platform: PlatformAnthropic, Type: AccountTypeAPIKey},
    )

    require.Error(t, err)
    require.Equal(t, http.StatusBadRequest, rec.Code)
    require.Equal(t, "Upstream request failed", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
    require.NotContains(t, rec.Body.String(), "secret-token")
    require.NotContains(t, rec.Body.String(), "debug")
}

func TestGatewayHandleErrorResponse_Unknown503UsesGeneric502(t *testing.T) {
    gin.SetMode(gin.TestMode)
    rec := httptest.NewRecorder()
    c, _ := gin.CreateTestContext(rec)
    body := []byte(`{"error":{"code":"vendor_failure","message":"private vendor detail"}}`)
    resp := &http.Response{
        StatusCode: http.StatusServiceUnavailable,
        Header: http.Header{},
        Body: io.NopCloser(bytes.NewReader(body)),
    }

    _, err := (&GatewayService{}).handleErrorResponse(
        context.Background(), resp, c,
        &Account{ID: 903, Platform: PlatformAnthropic, Type: AccountTypeAPIKey},
    )

    require.Error(t, err)
    require.Equal(t, http.StatusBadGateway, rec.Code)
    require.Equal(t, "Upstream request failed", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
    require.NotContains(t, rec.Body.String(), "private vendor detail")
}
```

- [ ] **Step 2: Run the three tests and verify RED**

Run the untagged renderer cases and the unit-tagged health-ordering case separately:

```bash
go test ./internal/service -run 'TestGatewayHandleErrorResponse_(Unknown400UsesSafeEnvelope|Unknown503UsesGeneric502)' -count=1
go test -tags=unit ./internal/service -run 'TestGatewayHandleErrorResponse_RecognizedDirectBeforeHealthAndRules' -count=1
```

Expected failures:
- recognized 401 can reach health/failover before recognition;
- unknown 400 returns the raw body;
- unknown 503 follows legacy status mapping instead of the final resolver.

- [ ] **Step 3: Implement early recognition and safe terminal resolution**

Immediately after the bounded body read in `handleErrorResponse`, construct the fact. Build the sanitized diagnostic detail as today, then append one ops event with `UpstreamFact: &fact` before branching. Fact-aware ops matching will decline database rules for recognized semantics.

```go
fact := ParseHTTPUpstreamErrorFact(account.Platform, resp, body)
setOpsUpstreamError(c, resp.StatusCode, fact.SafeMessage, upstreamDetail)
appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
    Platform:           account.Platform,
    AccountID:          account.ID,
    AccountName:        account.Name,
    UpstreamStatusCode: resp.StatusCode,
    UpstreamRequestID:  fact.RequestID,
    Kind:               "http_error",
    Message:            fact.SafeMessage,
    Detail:             upstreamDetail,
    UpstreamFact:       &fact,
})
if policy, recognized := RecognizeUpstreamErrorFact(fact); recognized {
    presentation := policy.Presentation
    MarkResponseCommitted(c)
    c.JSON(presentation.HTTPStatus, gin.H{
        "type": "error",
        "error": gin.H{
            "type":    presentation.ErrorType,
            "message": presentation.Message,
        },
    })
    return nil, fmt.Errorf("recognized upstream error: %s", presentation.ErrorCode)
}
```

The recognition branch must appear after the single bounded ops append but before `rateLimitService.HandleUpstreamError`, `shouldDisable`, failover-carrier creation, and terminal database resolution. The existing Generic Gateway writer owns this non-streaming HTTP boundary; mark `ResponseCommittedKey` before returning so the handler does not append a second response.

At the terminal non-failover boundary, replace the raw 400 and status switch with the final resolver. Since this is the final response boundary, `RuleMatched` does not alter control flow; it exists for compatibility helpers such as Task 2.

```go
resolved := ResolveFinalUpstreamError(fact, getBoundErrorPassthroughService(c))
if resolved.SkipMonitoring {
    c.Set(OpsSkipPassthroughKey, true)
}
presentation := resolved.Presentation
MarkResponseCommitted(c)
c.JSON(presentation.HTTPStatus, gin.H{
    "type": "error",
    "error": gin.H{
        "type":    presentation.ErrorType,
        "message": presentation.Message,
    },
})
```

Keep account-health and failover decisions before this terminal block only for non-direct facts. Remove `c.Data(http.StatusBadRequest, "application/json", body)`. Do not append another ops event in the terminal block; the fact-aware event was already recorded once before health handling. Add an assertion using `gatewayForwardErrorAlreadyCommunicated` or the existing handler fallback test fixture to prove `ResponseCommittedKey` prevents a second JSON/SSE fallback after `handleErrorResponse` returns its logging error.

- [ ] **Step 4: Run Generic HTTP and service tests and verify GREEN**

Run:

```bash
go test ./internal/service -run 'TestGatewayHandleErrorResponse' -count=1
go test -tags=unit ./internal/service -run 'TestGatewayHandleErrorResponse_RecognizedDirectBeforeHealthAndRules' -count=1
go test ./internal/service -run 'Test.*(Recognized|Passthrough|UpstreamError)' -count=1
```

Expected: PASS; unknown 400 is safe 400, unknown 503 is generic 502, recognized direct errors bypass the conflicting rule and failover carrier.

- [ ] **Step 5: Commit Task 3**

```bash
git add internal/service/gateway_service.go internal/service/ops_upstream_context.go internal/service/ops_upstream_context_fact_test.go internal/service/error_passthrough_runtime_test.go internal/service/gateway_multiplatform_test.go
git commit -m "fix(gateway): recognize direct HTTP errors before health"
```

---

### Task 4: Remove the Anthropic SSE synthetic 403 and attach its bounded ops fact

**Files:**
- Modify: `internal/service/gateway_service.go:649-670, 5667-5715`
- Modify: `internal/service/upstream_error_fact_test.go:118-140`
- Reuse: `internal/service/ops_upstream_context.go` and `internal/service/ops_upstream_context_fact_test.go` fact-aware contract introduced in Task 3.

**Interfaces:**
- Consumes: `UpstreamErrorFact` and `OpsUpstreamErrorEvent.UpstreamFact *UpstreamErrorFact` with `json:"-"` from Task 3.
- Produces: `newAnthropicSSEFailoverError` whose compatibility status is zero, and an unknown SSE ops event with no fabricated status and one attached bounded fact.

- [ ] **Step 1: Replace the synthetic-403 characterization with failing final-contract tests**

Replace `TestNewAnthropicSSEFailoverErrorKeepsLegacySynthetic403` with:

```go
func TestNewAnthropicSSEFailoverErrorKeepsStatusUnknown(t *testing.T) {
    body := []byte(`{"error":{"type":"vendor_stream_error","message":"stream failed"}}`)
    err := newAnthropicSSEFailoverError(&Account{Platform: PlatformAnthropic}, body, "req_123")

    attached, ok := err.UpstreamFact()
    require.True(t, ok)
    require.Zero(t, err.StatusCode)
    require.False(t, attached.HTTPStatusKnown)
    require.Zero(t, attached.HTTPStatus)
    require.Equal(t, UpstreamErrorSourceSSE, attached.Source)
}
```

The two ops tests already added in Task 3 remain the contract for recognized/unknown precedence. Add one Anthropic SSE-specific assertion that the stored event has `UpstreamStatusCode == 0`, `UpstreamFact.Source == UpstreamErrorSourceSSE`, and `UpstreamFact.HTTPStatusKnown == false`.

- [ ] **Step 2: Run the new tests and verify RED**

Run:

```bash
go test ./internal/service -run 'Test(NewAnthropicSSEFailoverErrorKeepsStatusUnknown|AppendOpsUpstreamError_)' -count=1
```

Expected: FAIL because the carrier and its unknown SSE ops event still report synthetic 403; the `UpstreamFact` field itself already exists from Task 3.

- [ ] **Step 3: Remove the synthetic status**

Change `newAnthropicSSEFailoverError` to:

```go
func newAnthropicSSEFailoverError(account *Account, body []byte, requestID string) *UpstreamFailoverError {
    provider := PlatformAnthropic
    if account != nil && account.Platform != "" {
        provider = account.Platform
    }
    fact := ParseAnthropicSSEErrorFact(provider, body, requestID)
    return &UpstreamFailoverError{
        StatusCode:   0,
        ResponseBody: body,
        upstreamFact: &fact,
    }
}
```

When appending the unknown SSE ops event, remove `UpstreamStatusCode: 403` and attach the fact:

```go
fact := ParseAnthropicSSEErrorFact(account.Platform, body, resp.Header.Get("x-request-id"))
appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
    Platform:          account.Platform,
    AccountID:         account.ID,
    AccountName:       account.Name,
    UpstreamRequestID: resp.Header.Get("x-request-id"),
    Kind:              "stream_error",
    Message:           fact.SafeMessage,
    Detail:            upstreamDetail,
    UpstreamFact:      &fact,
})
```

- [ ] **Step 4: Verify the fact-aware ops contract introduced in Task 3**

Confirm `OpsUpstreamErrorEvent` already contains:

```go
UpstreamFact *UpstreamErrorFact `json:"-"`
```

and that `checkSkipMonitoringForUpstreamEvent` calls `boundedOpsUpstreamErrorFact` followed by `MatchUnknownRule`. The helper may use the already-existing package-private `newUpstreamErrorFact`, `sanitizeUpstreamErrorFactScalar`, `boundedUpstreamErrorFactScalar`, and `nonEmptyStrings` functions from `internal/service/upstream_error_fact.go`; no new undefined helper is introduced. Do not pass `UpstreamResponseBody` or an unbounded diagnostic into matching.

- [ ] **Step 5: Run SSE, ops, and fact tests and verify GREEN**

Run:

```bash
go test ./internal/service -run 'Test(NewAnthropicSSEFailoverError|ParseAnthropicSSE|AppendOpsUpstreamError|GatewayStreaming.*Error)' -count=1
```

Expected: PASS; no test or ops event observes a fabricated 403.

- [ ] **Step 6: Commit Task 4**

```bash
git add internal/service/gateway_service.go internal/service/upstream_error_fact_test.go internal/service/ops_upstream_context_fact_test.go
git commit -m "fix(gateway): remove synthetic Anthropic stream status"
```

---

### Task 5: Route retained candidates through safe final resolution on every exhaustion path

**Files:**
- Modify: `internal/handler/failover_loop.go:44-143`
- Modify: `internal/handler/failover_loop_test.go:922-967`
- Modify: `internal/handler/gateway_handler.go:332-338, 501-503, 965-967, 1572-1643`
- Modify: `internal/handler/gateway_handler_chat_completions.go:307-323, 383-398`
- Modify: `internal/handler/gateway_handler_responses.go:187-200, 275-290, 343-368`
- Modify: `internal/handler/openai_gateway_handler.go:2116-2136`
- Modify: `internal/handler/gemini_v1beta_handler.go:584-591, 684-734`
- Create: `internal/handler/upstream_error_exhaustion_test.go`

**Interfaces:**
- Consumes: `UpstreamErrorCandidate.Fact`, `ResolveFinalUpstreamError`, and each handler's existing `errorPassthroughService *service.ErrorPassthroughService` field. Both `GatewayHandler` and `OpenAIGatewayHandler` already own this field; do not add a context lookup or new constructor dependency.
- Produces protocol-specific helper methods that receive `*UpstreamErrorCandidate` and render only the resolver result.
- Renderer ownership: Generic/Responses/Chat/Gemini helpers live on `GatewayHandler`; OpenAI Responses and Anthropic-native helpers live on `OpenAIGatewayHandler`. Committed Responses must flow through `GatewayHandler.handleStreamingAwareError`, whose existing `inboundIsResponses`/`writeResponsesFailedSSE` branch emits exactly one `response.failed`. Committed Anthropic-native output must keep using `anthropicStreamingAwareError`.

- [ ] **Step 1: Add failing candidate-safety and precedence tests**

Extend `TestUpstreamRecoveryState_CandidatePrecedenceAndSuccess` with fact assertions:

```go
candidate, ok := state.FinalCandidate()
require.True(t, ok)
require.Equal(t, "rate_limit_exceeded", candidate.Fact.ProviderCode)
require.Equal(t, structured.Presentation, candidate.Presentation)
```

The existing test already constructs candidates through `service.NewUpstreamErrorCandidate`; no additional candidate literal is required there. Before editing, run `rg -n 'UpstreamErrorCandidate\\s*\\{' internal` and update every literal found outside this constructor to carry its corresponding `Fact`.

Add a same-rank recency test:

```go
func TestUpstreamRecoveryState_SameRankUsesMostRecentCandidate(t *testing.T) {
    state := NewUpstreamRecoveryState()
    state.RetainCandidate(service.NewUpstreamErrorCandidate(service.UpstreamErrorFact{
        ProviderCode: "first", SafeMessage: "first",
    }, service.UpstreamCandidateStructured))
    state.RetainCandidate(service.NewUpstreamErrorCandidate(service.UpstreamErrorFact{
        ProviderCode: "second", SafeMessage: "second",
    }, service.UpstreamCandidateStructured))

    candidate, ok := state.FinalCandidate()
    require.True(t, ok)
    require.Equal(t, "second", candidate.Fact.ProviderCode)
}
```

Create `internal/handler/upstream_error_exhaustion_test.go` with focused helper tests:

```go
package handler

import (
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"

    "github.com/Wei-Shaw/sub2api/internal/service"
    "github.com/gin-gonic/gin"
    "github.com/stretchr/testify/require"
    "github.com/tidwall/gjson"
)

func TestOpenAIHandleUpstreamCandidate_Unknown503UsesGeneric502(t *testing.T) {
    gin.SetMode(gin.TestMode)
    rec := httptest.NewRecorder()
    c, _ := gin.CreateTestContext(rec)
    candidate := service.NewUpstreamErrorCandidate(service.UpstreamErrorFact{
        Provider: service.PlatformOpenAI, Source: service.UpstreamErrorSourceHTTP,
        HTTPStatusKnown: true, HTTPStatus: http.StatusServiceUnavailable,
        ProviderCode: "vendor_failure", SafeMessage: "private vendor detail",
    }, service.UpstreamCandidateStatusOnly)

    (&OpenAIGatewayHandler{}).handleUpstreamCandidate(c, candidate, false)

    require.Equal(t, http.StatusBadGateway, rec.Code)
    require.Equal(t, "Upstream request failed", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
    require.NotContains(t, rec.Body.String(), "private vendor detail")
}

func TestGatewayResponsesCandidatePreservesStructuredRateLimit(t *testing.T) {
    gin.SetMode(gin.TestMode)
    rec := httptest.NewRecorder()
    c, _ := gin.CreateTestContext(rec)
    c.Request = httptest.NewRequest(http.MethodPost, EndpointResponses, nil)
    candidate := service.NewUpstreamErrorCandidate(service.UpstreamErrorFact{
        Provider: service.PlatformOpenAI, Source: service.UpstreamErrorSourceHTTP,
        HTTPStatusKnown: true, HTTPStatus: http.StatusTooManyRequests,
        ProviderCode: "rate_limit_exceeded", ProviderType: "rate_limit_error",
        SafeMessage: "quota exceeded",
    }, service.UpstreamCandidateStructured)

    (&GatewayHandler{}).handleResponsesCandidate(c, candidate, false)

    require.Equal(t, http.StatusTooManyRequests, rec.Code)
    require.Equal(t, "rate_limit_exceeded", gjson.GetBytes(rec.Body.Bytes(), "error.code").String())
    require.Equal(t, "quota exceeded", gjson.GetBytes(rec.Body.Bytes(), "error.message").String())
}

func TestGatewayResponsesCandidateAfterCommitEmitsOneResponseFailed(t *testing.T) {
    gin.SetMode(gin.TestMode)
    rec := httptest.NewRecorder()
    c, _ := gin.CreateTestContext(rec)
    c.Request = httptest.NewRequest(http.MethodPost, EndpointResponses, nil)
    _, _ = c.Writer.WriteString(":\n\n")
    candidate := service.NewUpstreamErrorCandidate(service.UpstreamErrorFact{
        Provider: service.PlatformOpenAI, Source: service.UpstreamErrorSourceHTTP,
        HTTPStatusKnown: true, HTTPStatus: http.StatusTooManyRequests,
        ProviderCode: "rate_limit_exceeded", ProviderType: "rate_limit_error",
        SafeMessage: "quota exceeded",
    }, service.UpstreamCandidateStructured)

    (&GatewayHandler{}).handleResponsesCandidate(c, candidate, true)

    require.Equal(t, 1, strings.Count(rec.Body.String(), "event: response.failed\n"))
    require.Equal(t, 1, strings.Count(rec.Body.String(), `"type":"response.failed"`))
    require.NotContains(t, rec.Body.String(), `data: {"type":"error"`)
}
```

- [ ] **Step 2: Run candidate tests and verify RED**

Run:

```bash
go test ./internal/handler -run 'Test(UpstreamRecoveryState_|OpenAIHandleUpstreamCandidate_|GatewayResponsesCandidate)' -count=1
```

Expected failures:
- current OpenAI candidate rendering exposes the unknown presentation directly;
- `handleResponsesCandidate` does not exist.

- [ ] **Step 3: Resolve candidates inside handler helpers**

Update OpenAI’s helper:

```go
func (h *OpenAIGatewayHandler) handleUpstreamCandidate(c *gin.Context, candidate *service.UpstreamErrorCandidate, streamStarted bool) {
    if candidate == nil {
        h.handleFailoverExhaustedSimple(c, http.StatusBadGateway, streamStarted)
        return
    }
    resolved := service.ResolveFinalUpstreamError(candidate.Fact, h.errorPassthroughService)
    if resolved.SkipMonitoring {
        c.Set(service.OpsSkipPassthroughKey, true)
    }
    presentation := resolved.Presentation
    service.SetOpsUpstreamError(c, presentation.HTTPStatus, presentation.Message, "")
    h.handleStreamingAwareErrorWithCode(
        c, presentation.HTTPStatus, presentation.ErrorType,
        presentation.ErrorCode, presentation.Message, streamStarted,
    )
}
```

Update Generic Gateway’s `handleUpstreamCandidate` identically except it calls the existing `handleStreamingAwareError` without an OpenAI code argument.

Add Responses-specific rendering:

```go
func (h *GatewayHandler) handleResponsesCandidate(c *gin.Context, candidate *service.UpstreamErrorCandidate, streamStarted bool) {
    if candidate == nil {
        h.handleStreamingAwareError(c, http.StatusBadGateway, "upstream_error", "Upstream request failed", streamStarted)
        return
    }
    resolved := service.ResolveFinalUpstreamError(candidate.Fact, h.errorPassthroughService)
    if resolved.SkipMonitoring {
        c.Set(service.OpsSkipPassthroughKey, true)
    }
    presentation := resolved.Presentation
    service.SetOpsUpstreamError(c, presentation.HTTPStatus, presentation.Message, "")
    if streamStarted {
        // This helper must use the Responses terminal writer, not the generic
        // SSE error event; handleStreamingAwareError selects response.failed.
        h.handleStreamingAwareError(c, presentation.HTTPStatus, presentation.ErrorType, presentation.Message, true)
        return
    }
    code := presentation.ErrorCode
    if code == "" {
        code = presentation.ErrorType
    }
    h.responsesErrorResponse(c, presentation.HTTPStatus, code, presentation.Message)
}
```

Add equivalent thin Chat Completions and Gemini candidate helpers using `chatCompletionsErrorResponse` and `googleError` respectively. Each helper must call `ResolveFinalUpstreamError` and must not read `candidate.Presentation.Message` directly. For committed Chat Completions, keep the existing valid SSE error writer rather than returning silently. Gemini has no committed-SSE candidate path in this task.

Add/update the Anthropic-native helper separately:

```go
func (h *OpenAIGatewayHandler) handleAnthropicCandidate(c *gin.Context, candidate *service.UpstreamErrorCandidate, streamStarted bool) {
    if candidate == nil {
        h.anthropicStreamingAwareError(c, http.StatusBadGateway, "api_error", "Upstream request failed", streamStarted)
        return
    }
    resolved := service.ResolveFinalUpstreamError(candidate.Fact, h.errorPassthroughService)
    if resolved.SkipMonitoring {
        c.Set(service.OpsSkipPassthroughKey, true)
    }
    presentation := resolved.Presentation
    service.SetOpsUpstreamError(c, presentation.HTTPStatus, presentation.Message, "")
    h.anthropicStreamingAwareError(c, presentation.HTTPStatus, presentation.ErrorType, presentation.Message, streamStarted)
}
```

This preserves exactly one Anthropic `event: error` after commitment.

- [ ] **Step 4: Use candidates on error exhaustion and selection exhaustion**

At every `FailoverExhausted` switch branch found by `rg -n 'case FailoverExhausted|handle(CC|Responses|Gemini)FailoverExhausted' internal/handler`, prefer `fs.FinalCandidate()`:

```go
case FailoverExhausted:
    if candidate, ok := fs.FinalCandidate(); ok {
        h.handleUpstreamCandidate(c, candidate, streamStarted)
    } else {
        h.handleFailoverExhausted(c, fs.LastFailoverErr, account.Platform, streamStarted)
    }
    return
```

Use the protocol-specific candidate helper in Chat Completions, Responses, and Gemini. Change `handleResponsesFailoverExhausted` to accept `recovery *UpstreamRecoveryState` or call it only after the caller has checked `fs.FinalCandidate()`; do not leave a path where an existing candidate becomes `All available accounts exhausted`.

For selection failure before any upstream attempt, retain the existing “No available accounts” behavior. If selection fails after one or more upstream attempts and `fs.FinalCandidate()` exists, render that candidate instead of the selection error. Only replace `All available accounts exhausted` after a prior upstream candidate exists. Add one handler-level test that seeds `fs.Recovery` with a structured candidate, invokes the selection-exhaustion branch, and asserts the structured code/message are rendered; add a companion test with no candidate that asserts the existing no-available-account response remains unchanged.

- [ ] **Step 5: Ensure legacy final handlers delegate to bounded facts**

Apply this step to both `GatewayHandler` and `OpenAIGatewayHandler` legacy helpers. The OpenAI helper must preserve its existing private `SanitizedClientResponse` branch exactly as a fixed, explicitly sanitized compatibility path; only its non-sanitized fallback is migrated to the resolver.

For `handleFailoverExhausted`, obtain or construct the fact and call the resolver once:

```go
fact, ok := failoverErr.UpstreamFact()
if !ok {
    fact = service.NewLegacyUpstreamErrorFact(platform, failoverErr.StatusCode, failoverErr.ResponseBody)
}
resolved := service.ResolveFinalUpstreamError(fact, h.errorPassthroughService)
```

Preserve the existing silent-refusal and private `SanitizedClientResponse` checks before this generic resolution. After those explicit safe boundaries, remove duplicated built-in recognition, `MatchUnknownRule`, and raw-body message extraction from the handler.

- [ ] **Step 6: Run handler tests and verify GREEN**

Run:

```bash
go test ./internal/handler -run 'Test.*(Failover|Candidate|Responses|Gemini|ChatCompletions|StreamingAware)' -count=1
```

Expected: PASS; structured candidates survive weaker failures, unknown status-only candidates become safe fallback, selection exhaustion preserves a seeded candidate while pure no-account routing retains its existing response, and protocol-specific responses remain valid.

- [ ] **Step 7: Commit Task 5**

```bash
git add internal/handler/failover_loop.go internal/handler/failover_loop_test.go internal/handler/gateway_handler.go internal/handler/gateway_handler_chat_completions.go internal/handler/gateway_handler_responses.go internal/handler/openai_gateway_handler.go internal/handler/gemini_v1beta_handler.go internal/handler/upstream_error_exhaustion_test.go
git commit -m "fix(gateway): resolve retained errors consistently"
```

---

### Task 6: Complete boundary regression coverage and verify the PR

**Files:**
- Modify only failing tests that encode superseded synthetic-status, raw-body, or generic-exhaustion behavior.
- Production files are not expected to change in this task; if a newly failing acceptance test identifies an uncovered PR6 boundary, stop, add one failing test, and make one bounded fix before continuing.

**Interfaces:**
- Consumes all Task 1–5 contracts.
- Produces a clean, independently reviewable PR6 branch.

- [ ] **Step 1: Run focused Issue #24 service tests**

```bash
go test ./internal/service -run 'Test.*(UpstreamError|Passthrough|AnthropicSSE|GatewayHandleError|AppendOpsUpstreamError|OpsSkip)' -count=1
go test -tags=unit ./internal/service -run 'TestGatewayHandleErrorResponse_RecognizedDirectBeforeHealthAndRules' -count=1
```

Expected: PASS.

- [ ] **Step 2: Run focused Issue #24 handler tests**

```bash
go test ./internal/handler -run 'Test.*(Failover|Candidate|Responses|Gemini|ChatCompletions|StreamingAware)' -count=1
```

Expected: PASS.

- [ ] **Step 3: Run relevant package suites**

```bash
go test ./internal/service ./internal/handler -count=1
go test -tags=unit ./internal/service -run 'TestGatewayHandleErrorResponse_RecognizedDirectBeforeHealthAndRules' -count=1
```

Expected: both packages PASS. If a test fails because it expects synthetic 403, raw HTTP 400, unbounded passthrough text, or `All available accounts exhausted` despite a retained candidate, update that test to the approved PR6 behavior. Do not loosen unrelated assertions. Run the unit-tagged Generic HTTP health-ordering test separately as shown in Task 3.

- [ ] **Step 4: Run race-focused tests**

```bash
go test -race ./internal/service ./internal/handler -run 'Test.*(UpstreamError|Passthrough|Failover|Candidate|AppendOpsUpstreamError)' -count=1
go test -race -tags=unit ./internal/service -run 'TestGatewayHandleErrorResponse_RecognizedDirectBeforeHealthAndRules' -count=1
```

Expected: PASS with no race report.

- [ ] **Step 5: Format and inspect changes**

```bash
gofmt -w internal/service/upstream_error_final_presentation.go internal/service/upstream_error_final_presentation_test.go internal/service/upstream_error_policy.go internal/service/error_passthrough_runtime.go internal/service/error_passthrough_runtime_test.go internal/service/gateway_service.go internal/service/upstream_error_fact_test.go internal/service/ops_upstream_context.go internal/service/ops_upstream_context_fact_test.go internal/handler/failover_loop.go internal/handler/failover_loop_test.go internal/handler/gateway_handler.go internal/handler/gateway_handler_chat_completions.go internal/handler/gateway_handler_responses.go internal/handler/openai_gateway_handler.go internal/handler/gemini_v1beta_handler.go internal/handler/upstream_error_exhaustion_test.go
git diff --check
git status --short
git diff --stat
```

Expected: no formatting or whitespace errors; only planned Issue #24 files are modified.

- [ ] **Step 6: Run vet and record the known baseline separately**

```bash
go vet ./internal/service ./internal/handler
```

Expected: either PASS or only the pre-existing lock-copy warnings in `internal/service/openai_ws_protocol_resolver_test.go`. Any new warning in a PR6 file must be fixed before completion.

- [ ] **Step 7: Review security invariants directly**

Run searches:

```bash
rg -n 'c\.Data\([^\n]*body|ExtractUpstreamErrorMessage\(responseBody\)|StatusCode:\s*http\.StatusForbidden|\.MatchRule\(' internal/service internal/handler
```

Expected:
- no Generic HTTP unknown branch returns raw `body`;
- no final passthrough path derives client message from `responseBody`;
- `newAnthropicSSEFailoverError` does not use 403;
- production rule calls are either inside `MatchUnknownRule` or non-runtime administrative/test compatibility code.

- [ ] **Step 8: Commit final test expectation updates**

```bash
git add internal/service/upstream_error_final_presentation_test.go internal/service/error_passthrough_runtime_test.go internal/service/upstream_error_fact_test.go internal/service/ops_upstream_context_fact_test.go internal/service/gateway_multiplatform_test.go internal/handler/failover_loop_test.go internal/handler/upstream_error_exhaustion_test.go
git commit -m "test(gateway): cover legacy error boundary cleanup"
```

If the working tree is already clean because no expectations needed updates, skip this commit rather than creating an empty commit.

- [ ] **Step 9: Final branch summary**

```bash
git status --short --branch
git log --oneline origin/dev..HEAD
```

Expected:
- clean working tree;
- design commit plus small test-first implementation commits;
- no push, PR creation, or merge until explicitly requested.
