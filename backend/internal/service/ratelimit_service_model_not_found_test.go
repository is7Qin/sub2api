//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRateLimitService_HandleUpstreamError_OpenAIOAuthPlanGatedModel(t *testing.T) {
	message := "The 'gpt-5.4' model is not supported when using Codex with a ChatGPT account."
	tests := []struct {
		name      string
		account   *Account
		status    int
		body      string
		requested string
		want      bool
		wantScope string
	}{
		{name: "detail", account: planGatedAccount(PlatformOpenAI, AccountTypeOAuth), status: 400, body: `{"detail":"` + message + `"}`, requested: "gpt-5.4", want: true, wantScope: "gpt-5.4"},
		{name: "error message", account: planGatedAccount(PlatformOpenAI, AccountTypeOAuth), status: 400, body: `{"error":{"message":"` + message + `"}}`, requested: "gpt-5.4", want: true, wantScope: "gpt-5.4"},
		{name: "already mapped once", account: planGatedMappedAccount(), status: 400, body: `{"detail":"The 'gpt-5.4' model is not supported when using Codex with a ChatGPT account."}`, requested: "gpt-5.4", want: true, wantScope: "gpt-5.4"},
		{name: "unmapped alias rejected", account: planGatedMappedAccount(), status: 400, body: `{"detail":"The 'gpt-5.4' model is not supported when using Codex with a ChatGPT account."}`, requested: "alias"},
		{name: "mismatched model", account: planGatedAccount(PlatformOpenAI, AccountTypeOAuth), status: 400, body: `{"detail":"` + message + `"}`, requested: "gpt-5.3"},
		{name: "api key", account: planGatedAccount(PlatformOpenAI, AccountTypeAPIKey), status: 400, body: `{"detail":"` + message + `"}`, requested: "gpt-5.4"},
		{name: "setup token", account: planGatedAccount(PlatformOpenAI, AccountTypeSetupToken), status: 400, body: `{"detail":"` + message + `"}`, requested: "gpt-5.4"},
		{name: "non OpenAI", account: planGatedAccount(PlatformAnthropic, AccountTypeOAuth), status: 400, body: `{"detail":"` + message + `"}`, requested: "gpt-5.4"},
		{name: "wrong status", account: planGatedAccount(PlatformOpenAI, AccountTypeOAuth), status: http.StatusTeapot, body: `{"detail":"` + message + `"}`, requested: "gpt-5.4"},
		{name: "unrelated 400", account: planGatedAccount(PlatformOpenAI, AccountTypeOAuth), status: 400, body: `{"error":{"message":"invalid request"}}`, requested: "gpt-5.4"},
		{name: "malformed", account: planGatedAccount(PlatformOpenAI, AccountTypeOAuth), status: 400, body: `{"detail":`, requested: "gpt-5.4"},
		{name: "trailing", account: planGatedAccount(PlatformOpenAI, AccountTypeOAuth), status: 400, body: `{"detail":"` + message + `"} true`, requested: "gpt-5.4"},
		{name: "duplicate", account: planGatedAccount(PlatformOpenAI, AccountTypeOAuth), status: 400, body: `{"detail":"` + message + `","detail":"` + message + `"}`, requested: "gpt-5.4"},
		{name: "case collision", account: planGatedAccount(PlatformOpenAI, AccountTypeOAuth), status: 400, body: `{"detail":"` + message + `","Detail":"` + message + `"}`, requested: "gpt-5.4"},
		{name: "wrong type", account: planGatedAccount(PlatformOpenAI, AccountTypeOAuth), status: 400, body: `{"detail":42}`, requested: "gpt-5.4"},
		{name: "wrong error type", account: planGatedAccount(PlatformOpenAI, AccountTypeOAuth), status: 400, body: `{"error":"` + message + `"}`, requested: "gpt-5.4"},
		{name: "wrong message type", account: planGatedAccount(PlatformOpenAI, AccountTypeOAuth), status: 400, body: `{"error":{"message":42}}`, requested: "gpt-5.4"},
		{name: "nested case collision", account: planGatedAccount(PlatformOpenAI, AccountTypeOAuth), status: 400, body: `{"error":{"message":"` + message + `","Message":"` + message + `"}}`, requested: "gpt-5.4"},
		{name: "spoofed nested", account: planGatedAccount(PlatformOpenAI, AccountTypeOAuth), status: 400, body: `{"wrapper":{"detail":"` + message + `"}}`, requested: "gpt-5.4"},
		{name: "array", account: planGatedAccount(PlatformOpenAI, AccountTypeOAuth), status: 400, body: `[{"detail":"` + message + `"}]`, requested: "gpt-5.4"},
		{name: "scalar", account: planGatedAccount(PlatformOpenAI, AccountTypeOAuth), status: 400, body: `"` + message + `"`, requested: "gpt-5.4"},
		{name: "oversized", account: planGatedAccount(PlatformOpenAI, AccountTypeOAuth), status: 400, body: `{"detail":"` + message + `","padding":"` + strings.Repeat("x", openAIPlanGatedBodyMaxBytes) + `"}`, requested: "gpt-5.4"},
		{name: "both locations", account: planGatedAccount(PlatformOpenAI, AccountTypeOAuth), status: 400, body: `{"detail":"` + message + `","error":{"message":"` + message + `"}}`, requested: "gpt-5.4"},
		{name: "invalid UTF-8", account: planGatedAccount(PlatformOpenAI, AccountTypeOAuth), status: 400, body: string(append([]byte(`{"detail":"`+message), 0xff)), requested: "gpt-5.4"},
		{name: "depth bound", account: planGatedAccount(PlatformOpenAI, AccountTypeOAuth), status: 400, body: `{"detail":"` + message + `","padding":` + strings.Repeat(`[`, openAIPlanGatedMaxDepth) + `0` + strings.Repeat(`]`, openAIPlanGatedMaxDepth) + `}`, requested: "gpt-5.4"},
		{name: "token bound", account: planGatedAccount(PlatformOpenAI, AccountTypeOAuth), status: 400, body: `{"detail":"` + message + `","padding":[` + strings.Repeat(`0,`, openAIPlanGatedMaxTokens) + `0]}`, requested: "gpt-5.4"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &modelNotFoundAccountRepoStub{}
			svc := &RateLimitService{accountRepo: repo}
			got := svc.HandleUpstreamError(context.Background(), tt.account, tt.status, http.Header{}, []byte(tt.body), tt.requested)
			require.Equal(t, tt.want, got)
			if tt.want {
				require.Len(t, repo.modelRateLimitCalls, 1)
				require.Equal(t, tt.wantScope, repo.modelRateLimitCalls[0].scope)
				require.Equal(t, openAIPlanGatedModelReason, repo.modelRateLimitCalls[0].reason)
			} else {
				require.Empty(t, repo.modelRateLimitCalls)
			}
		})
	}
}

func TestRateLimitService_HandleUpstreamError_ModelNotFoundHonorsErrorCodePolicy(t *testing.T) {
	account := planGatedAccount(PlatformOpenAI, AccountTypeAPIKey)
	account.Credentials = map[string]any{"custom_error_codes_enabled": true, "custom_error_codes": []any{float64(404)}}
	repo := &modelNotFoundAccountRepoStub{}
	svc := &RateLimitService{accountRepo: repo}

	handled := svc.HandleUpstreamError(context.Background(), account, 400, http.Header{}, []byte(`{"error":{"message":"model not found"}}`), "gpt-5.4")

	require.False(t, handled)
	require.Empty(t, repo.modelRateLimitCalls)
}

func TestRateLimitService_HandleUpstreamError_OpenAIOAuthPlanGatedRepositoryFailureKeepsFailover(t *testing.T) {
	repo := &modelNotFoundAccountRepoStub{modelRateLimitErr: errors.New("write failed")}
	svc := &RateLimitService{accountRepo: repo}
	account := planGatedAccount(PlatformOpenAI, AccountTypeOAuth)

	handled := svc.HandleUpstreamError(context.Background(), account, 400, http.Header{}, []byte(`{"detail":"The 'gpt-5.4' model is not supported when using Codex with a ChatGPT account."}`), "gpt-5.4")

	require.True(t, handled)
	require.Len(t, repo.modelRateLimitCalls, 1)
}

func planGatedAccount(platform, accountType string) *Account {
	return &Account{ID: 202, Platform: platform, Type: accountType, Status: StatusActive, Schedulable: true}
}

func planGatedMappedAccount() *Account {
	account := planGatedAccount(PlatformOpenAI, AccountTypeOAuth)
	account.Credentials = map[string]any{"model_mapping": map[string]any{"alias": "gpt-5.4", "gpt-5.4": "must-not-map-twice"}}
	return account
}

func TestRateLimitService_HandleUpstreamError_ModelNotFoundUsesModelRateLimit(t *testing.T) {
	repo := &modelNotFoundAccountRepoStub{}
	svc := &RateLimitService{accountRepo: repo}
	account := openAIModelNotFoundTempAccount()

	handled := svc.HandleUpstreamError(
		context.Background(),
		account,
		http.StatusNotFound,
		http.Header{},
		[]byte(`{"error":{"code":"model_not_found","message":"model not found"}}`),
		"gpt-5.4",
	)

	require.True(t, handled)
	require.Zero(t, repo.tempCalls)
	require.Len(t, repo.modelRateLimitCalls, 1)
	call := repo.modelRateLimitCalls[0]
	require.Equal(t, account.ID, call.accountID)
	require.Equal(t, "gpt-5.4", call.scope)
	require.Equal(t, upstreamModelNotFoundReason, call.reason)
	require.WithinDuration(t, time.Now().Add(upstreamModelNotFoundCooldown), call.resetAt, 5*time.Second)
}

func TestRateLimitService_HandleUpstreamError_ModelNotFoundWriteFailureDoesNotTempUnschedule(t *testing.T) {
	repo := &modelNotFoundAccountRepoStub{modelRateLimitErr: errors.New("write failed")}
	svc := &RateLimitService{accountRepo: repo}
	account := openAIModelNotFoundTempAccount()

	handled := svc.HandleUpstreamError(
		context.Background(),
		account,
		http.StatusNotFound,
		http.Header{},
		[]byte(`{"error":{"code":"model_not_found","message":"model not found"}}`),
		"gpt-5.4",
	)

	require.True(t, handled)
	require.Zero(t, repo.tempCalls)
	require.Len(t, repo.modelRateLimitCalls, 1)
}

func TestRateLimitService_HandleUpstreamError_Bare404UsesModelScopedTempUnschedulable(t *testing.T) {
	repo := &modelNotFoundAccountRepoStub{}
	svc := &RateLimitService{accountRepo: repo}
	account := openAIModelNotFoundTempAccount()

	handled := svc.HandleUpstreamError(
		context.Background(),
		account,
		http.StatusNotFound,
		http.Header{},
		[]byte(`{"error":{"message":"endpoint not found"}}`),
		"gpt-5.4",
	)

	require.True(t, handled)
	require.Zero(t, repo.tempCalls)
	require.Len(t, repo.modelRateLimitCalls, 1)
	require.Equal(t, "gpt-5.4", repo.modelRateLimitCalls[0].scope)
}

func TestRateLimitService_HandleUpstreamError_UnknownModelKeepsAccountScopedTempUnschedulable(t *testing.T) {
	repo := &modelNotFoundAccountRepoStub{}
	svc := &RateLimitService{accountRepo: repo}
	account := openAIModelNotFoundTempAccount()

	handled := svc.HandleUpstreamError(
		context.Background(),
		account,
		http.StatusNotFound,
		http.Header{},
		[]byte(`{"error":{"message":"endpoint not found"}}`),
	)

	require.True(t, handled)
	require.Equal(t, 1, repo.tempCalls)
	require.Empty(t, repo.modelRateLimitCalls)
}

func TestRateLimitService_HandleUpstreamError_ModelScopedTempWriteFailureDoesNotWidenScope(t *testing.T) {
	repo := &modelNotFoundAccountRepoStub{modelRateLimitErr: errors.New("write failed")}
	svc := &RateLimitService{accountRepo: repo}
	account := openAIModelNotFoundTempAccount()

	handled := svc.HandleUpstreamError(
		context.Background(),
		account,
		http.StatusNotFound,
		http.Header{},
		[]byte(`{"error":{"message":"endpoint not found"}}`),
		"gpt-5.4",
	)

	require.True(t, handled)
	require.Zero(t, repo.tempCalls)
	require.Len(t, repo.modelRateLimitCalls, 1)
}

func openAIModelNotFoundTempAccount() *Account {
	return &Account{
		ID:          101,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Credentials: map[string]any{
			"temp_unschedulable_enabled": true,
			"temp_unschedulable_rules": []any{
				map[string]any{
					"error_code":       float64(http.StatusNotFound),
					"keywords":         []any{"not found"},
					"duration_minutes": float64(10),
				},
			},
		},
	}
}
