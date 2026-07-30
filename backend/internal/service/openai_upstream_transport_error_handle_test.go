//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type openaiTransportAccountRepoStub struct {
	AccountRepository
	tempUnschedCalls []tempUnschedCall
}

func (r *openaiTransportAccountRepoStub) SetTempUnschedulable(_ context.Context, id int64, until time.Time, reason string) error {
	r.tempUnschedCalls = append(r.tempUnschedCalls, tempUnschedCall{accountID: id, until: until, reason: reason})
	return nil
}

func newOpenAITransportErrTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	return c, rec
}

func TestHandleOpenAIUpstreamTransportError_PersistentEvictsAndFailsOver(t *testing.T) {
	repo := &openaiTransportAccountRepoStub{}
	svc := &OpenAIGatewayService{accountRepo: repo}
	account := &Account{ID: 4627, Name: "proxy-expired", Platform: PlatformOpenAI}
	c, rec := newOpenAITransportErrTestContext()

	before := time.Now()
	retErr := svc.handleOpenAIUpstreamTransportError(context.Background(), c, account,
		errors.New(`Post "https://chatgpt.com/backend-api/codex/responses": socks connect tcp 85.255.176.68:12324->chatgpt.com:443: username/password authentication failed`), false)
	after := time.Now()

	var fo *UpstreamFailoverError
	require.True(t, errors.As(retErr, &fo), "persistent error must return *UpstreamFailoverError")
	require.Equal(t, http.StatusBadGateway, fo.StatusCode)
	fact, ok := fo.UpstreamFact()
	require.True(t, ok)
	require.Equal(t, UpstreamErrorSourceTransport, fact.Source)
	require.False(t, fact.HTTPStatusKnown)
	require.Zero(t, fact.HTTPStatus)
	require.NotContains(t, fact.SafeMessage, "chatgpt.com")
	require.NotContains(t, fact.SafeMessage, "username/password")

	require.Len(t, repo.tempUnschedCalls, 1)
	require.Equal(t, int64(4627), repo.tempUnschedCalls[0].accountID)
	require.Contains(t, repo.tempUnschedCalls[0].reason, "authentication failed")
	require.True(t, repo.tempUnschedCalls[0].until.After(before.Add(openAITransportErrorTempUnschedDuration-time.Second)))
	require.True(t, repo.tempUnschedCalls[0].until.Before(after.Add(openAITransportErrorTempUnschedDuration+time.Second)))
	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
	require.Equal(t, 0, rec.Body.Len())
}

func TestHandleOpenAIUpstreamTransportError_TransientFailsOverWithoutEviction(t *testing.T) {
	repo := &openaiTransportAccountRepoStub{}
	svc := &OpenAIGatewayService{accountRepo: repo}
	account := &Account{ID: 99, Name: "flaky", Platform: PlatformOpenAI}
	c, rec := newOpenAITransportErrTestContext()

	err := svc.handleOpenAIUpstreamTransportError(context.Background(), c, account,
		errors.New(`Post "https://chatgpt.com/...": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`), false)

	var fo *UpstreamFailoverError
	require.True(t, errors.As(err, &fo), "transient error must return *UpstreamFailoverError")
	require.Equal(t, http.StatusBadGateway, fo.StatusCode)
	require.Empty(t, repo.tempUnschedCalls)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
	require.Equal(t, 0, rec.Body.Len())
}

func TestHandleOpenAIUpstreamTransportError_ContextCanceled_NoFailoverNoEviction(t *testing.T) {
	repo := &openaiTransportAccountRepoStub{}
	svc := &OpenAIGatewayService{accountRepo: repo}
	account := &Account{ID: 77, Name: "healthy", Platform: PlatformOpenAI}
	c, rec := newOpenAITransportErrTestContext()

	err := svc.handleOpenAIUpstreamTransportError(context.Background(), c, account, context.Canceled, false)

	var fo *UpstreamFailoverError
	require.False(t, errors.As(err, &fo), "context.Canceled must NOT return *UpstreamFailoverError")
	require.NotNil(t, err)
	require.Empty(t, repo.tempUnschedCalls)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
	require.Equal(t, 0, rec.Body.Len())
}

func TestHandleOpenAIUpstreamTransportError_WrappedContextCanceled_NoFailover(t *testing.T) {
	repo := &openaiTransportAccountRepoStub{}
	svc := &OpenAIGatewayService{accountRepo: repo}
	account := &Account{ID: 78, Name: "healthy2", Platform: PlatformOpenAI}
	c, _ := newOpenAITransportErrTestContext()

	err := svc.handleOpenAIUpstreamTransportError(context.Background(), c, account, fmt.Errorf("http request failed: %w", context.Canceled), false)

	var fo *UpstreamFailoverError
	require.False(t, errors.As(err, &fo), "wrapped context.Canceled must NOT return *UpstreamFailoverError")
	require.Empty(t, repo.tempUnschedCalls)
	require.False(t, svc.isOpenAIAccountRuntimeBlocked(account))
}

func TestTempUnscheduleOpenAITransportError_NilAccountRepo_InMemoryBlockOnly(t *testing.T) {
	svc := &OpenAIGatewayService{accountRepo: nil}
	account := &Account{ID: 55, Name: "no-db", Platform: PlatformOpenAI}

	svc.tempUnscheduleOpenAITransportError(context.Background(), account, "proxy refused")

	require.True(t, svc.isOpenAIAccountRuntimeBlocked(account))
}

func TestHandleOpenAIUpstreamTransportError_DeadlineExceeded_StillFailsOver(t *testing.T) {
	repo := &openaiTransportAccountRepoStub{}
	svc := &OpenAIGatewayService{accountRepo: repo}
	account := &Account{ID: 79, Name: "slow", Platform: PlatformOpenAI}
	c, _ := newOpenAITransportErrTestContext()

	err := svc.handleOpenAIUpstreamTransportError(context.Background(), c, account, context.DeadlineExceeded, false)

	var fo *UpstreamFailoverError
	require.True(t, errors.As(err, &fo), "context.DeadlineExceeded must still return *UpstreamFailoverError")
}

func TestForwardAsRawChatCompletions_TransportErrorFailsOver(t *testing.T) {
	repo := &openaiTransportAccountRepoStub{}
	upstream := &httpUpstreamRecorder{
		err: errors.New(`Post "https://opencode.ai/zen/v1/chat/completions": EOF`),
	}
	svc := &OpenAIGatewayService{
		accountRepo:  repo,
		httpUpstream: upstream,
		cfg: &config.Config{
			Security: config.SecurityConfig{
				URLAllowlist: config.URLAllowlistConfig{Enabled: false},
			},
		},
	}
	account := &Account{
		ID:       81,
		Name:     "oc-20053",
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "sk-test",
			"base_url": "https://opencode.ai/zen/v1",
		},
	}
	body := []byte(`{"model":"deepseek-v4-flash-free","messages":[{"role":"user","content":"hello"}]}`)
	c, rec := newOpenAITransportErrTestContext()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	_, err := svc.forwardAsRawChatCompletions(context.Background(), c, account, body)

	require.Len(t, upstream.requests, 1)
	var fo *UpstreamFailoverError
	require.True(t, errors.As(err, &fo), "transport error must trigger account failover")
	require.Equal(t, http.StatusBadGateway, fo.StatusCode)
	require.Empty(t, repo.tempUnschedCalls, "plain EOF is transient: fail over but do not evict")
	require.Equal(t, 0, rec.Body.Len(), "service must not write a hard 502 before handler can fail over")
}
