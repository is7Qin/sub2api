package handler

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type openAIWSTurnConcurrencyProbe struct {
	cache   *concurrencyCacheMock
	mu      sync.Mutex
	order   []string
	keyOK   bool
	userOK  bool
	acctOK  bool
	keyErr  error
	userErr error
	acctErr error
}

func newOpenAIWSTurnConcurrencyProbe() *openAIWSTurnConcurrencyProbe {
	p := &openAIWSTurnConcurrencyProbe{keyOK: true, userOK: true, acctOK: true}
	p.cache = &concurrencyCacheMock{
		acquireAPIKeySlotFn: func(context.Context, int64, int, string) (bool, error) {
			p.record("key")
			return p.keyOK, p.keyErr
		},
		acquireUserSlotFn: func(context.Context, int64, int, string) (bool, error) {
			p.record("user")
			return p.userOK, p.userErr
		},
		acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) {
			p.record("account")
			return p.acctOK, p.acctErr
		},
	}
	return p
}

func (p *openAIWSTurnConcurrencyProbe) record(slot string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.order = append(p.order, slot)
}

func (p *openAIWSTurnConcurrencyProbe) helper() *ConcurrencyHelper {
	return NewConcurrencyHelper(service.NewConcurrencyService(p.cache), SSEPingFormatNone, time.Second)
}

func requireOpenAIWSClose(t *testing.T, err error, status coderws.StatusCode) {
	t.Helper()
	var closeErr *service.OpenAIWSClientCloseError
	require.ErrorAs(t, err, &closeErr)
	require.Equal(t, status, closeErr.StatusCode())
}

func TestOpenAIWSTurnSlots_KeyFullClosesTryAgainLaterBeforeDownstreamAdmission(t *testing.T) {
	p := newOpenAIWSTurnConcurrencyProbe()
	p.keyOK = false
	slots := newOpenAIWSTurnSlots(p.helper(), context.Background(), 101, 1, 201, 1)

	err := slots.acquireClient()

	requireOpenAIWSClose(t, err, coderws.StatusTryAgainLater)
	require.Equal(t, []string{"key"}, p.order)
	require.Zero(t, atomic.LoadInt32(&p.cache.releaseAPIKeyCalled))
}

func TestOpenAIResponsesWebSocket_KeyFullUsesTryAgainLaterClose(t *testing.T) {
	p := newOpenAIWSTurnConcurrencyProbe()
	p.keyOK = false
	h := newOpenAIHandlerForPreviousResponseIDValidation(t, p.cache)
	wsServer := newOpenAIWSHandlerTestServerWithKeyConcurrency(t, h, 1)
	defer wsServer.Close()

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
	clientConn, _, err := coderws.Dial(dialCtx, "ws"+strings.TrimPrefix(wsServer.URL, "http")+"/openai/v1/responses", nil)
	cancelDial()
	require.NoError(t, err)
	defer func() { _ = clientConn.CloseNow() }()

	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 3*time.Second)
	err = clientConn.Write(writeCtx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","stream":false}`))
	cancelWrite()
	require.NoError(t, err)

	readCtx, cancelRead := context.WithTimeout(context.Background(), 3*time.Second)
	_, _, err = clientConn.Read(readCtx)
	cancelRead()
	require.Error(t, err)
	var closeErr coderws.CloseError
	require.ErrorAs(t, err, &closeErr)
	require.Equal(t, coderws.StatusTryAgainLater, closeErr.Code)
	require.Equal(t, []string{"key"}, p.order)
}

func TestOpenAIWSTurnSlots_UserFullReleasesKey(t *testing.T) {
	p := newOpenAIWSTurnConcurrencyProbe()
	p.userOK = false
	slots := newOpenAIWSTurnSlots(p.helper(), context.Background(), 101, 1, 201, 1)

	err := slots.acquireClient()

	requireOpenAIWSClose(t, err, coderws.StatusTryAgainLater)
	require.Equal(t, []string{"key", "user"}, p.order)
	require.Equal(t, int32(1), atomic.LoadInt32(&p.cache.releaseAPIKeyCalled))
	require.Zero(t, atomic.LoadInt32(&p.cache.releaseUserCalled))
}

func TestOpenAIWSTurnSlots_AccountFailureReleasesClientSlots(t *testing.T) {
	p := newOpenAIWSTurnConcurrencyProbe()
	p.acctErr = errors.New("account cache unavailable")
	slots := newOpenAIWSTurnSlots(p.helper(), context.Background(), 101, 1, 201, 1)
	require.NoError(t, slots.acquireClient())

	err := slots.acquireAccount(301, 1)

	requireOpenAIWSClose(t, err, coderws.StatusInternalError)
	require.Equal(t, []string{"key", "user", "account"}, p.order)
	require.Equal(t, int32(1), atomic.LoadInt32(&p.cache.releaseAPIKeyCalled))
	require.Equal(t, int32(1), atomic.LoadInt32(&p.cache.releaseUserCalled))
	require.Zero(t, atomic.LoadInt32(&p.cache.releaseAccountCalled))
}

func TestOpenAIWSTurnSlots_TurnExitReleasesAllExactlyOnce(t *testing.T) {
	for _, exit := range []string{"normal", "error", "disconnect"} {
		t.Run(exit, func(t *testing.T) {
			p := newOpenAIWSTurnConcurrencyProbe()
			slots := newOpenAIWSTurnSlots(p.helper(), context.Background(), 101, 1, 201, 1)
			require.NoError(t, slots.acquireClient())
			require.NoError(t, slots.acquireAccount(301, 1))

			slots.releaseTurn()
			slots.releaseTurn()

			require.Equal(t, int32(1), atomic.LoadInt32(&p.cache.releaseAPIKeyCalled))
			require.Equal(t, int32(1), atomic.LoadInt32(&p.cache.releaseUserCalled))
			require.Equal(t, int32(1), atomic.LoadInt32(&p.cache.releaseAccountCalled))
		})
	}
}

func TestOpenAIWSTurnSlots_IdleConnectionHoldsNoSlots(t *testing.T) {
	p := newOpenAIWSTurnConcurrencyProbe()
	slots := newOpenAIWSTurnSlots(p.helper(), context.Background(), 101, 1, 201, 1)
	require.NoError(t, slots.acquireClient())
	require.NoError(t, slots.acquireAccount(301, 1))

	slots.releaseTurn()

	require.False(t, slots.clientHeld())
	require.False(t, slots.accountHeld())
	require.Equal(t, int32(1), atomic.LoadInt32(&p.cache.releaseAPIKeyCalled))
	require.Equal(t, int32(1), atomic.LoadInt32(&p.cache.releaseUserCalled))
	require.Equal(t, int32(1), atomic.LoadInt32(&p.cache.releaseAccountCalled))
}

func TestOpenAIWSTurnSlots_FailoverReleasesOnlyAttemptAccount(t *testing.T) {
	p := newOpenAIWSTurnConcurrencyProbe()
	slots := newOpenAIWSTurnSlots(p.helper(), context.Background(), 101, 1, 201, 1)
	require.NoError(t, slots.acquireClient())
	require.NoError(t, slots.acquireAccount(301, 1))

	slots.releaseAccount()
	require.True(t, slots.clientHeld())
	require.NoError(t, slots.acquireClient())
	require.NoError(t, slots.acquireAccount(302, 1))
	slots.releaseTurn()

	require.Equal(t, []string{"key", "user", "account", "account"}, p.order)
	require.Equal(t, int32(1), atomic.LoadInt32(&p.cache.releaseAPIKeyCalled))
	require.Equal(t, int32(1), atomic.LoadInt32(&p.cache.releaseUserCalled))
	require.Equal(t, int32(2), atomic.LoadInt32(&p.cache.releaseAccountCalled))
}

func newOpenAIWSHandlerTestServerWithKeyConcurrency(t *testing.T, h *OpenAIGatewayHandler, keyConcurrency int) *httptest.Server {
	t.Helper()
	groupID := int64(2)
	apiKey := &service.APIKey{
		ID:          101,
		GroupID:     &groupID,
		Concurrency: keyConcurrency,
		User:        &service.User{ID: 201},
	}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), apiKey)
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 201, Concurrency: 1})
		c.Next()
	})
	router.GET("/openai/v1/responses", h.ResponsesWebSocket)
	return httptest.NewServer(router)
}

func TestOpenAIWSTurnSlots_DisabledKeyConcurrencySkipsKeyCache(t *testing.T) {
	p := newOpenAIWSTurnConcurrencyProbe()
	slots := newOpenAIWSTurnSlots(p.helper(), context.Background(), 101, 0, 201, 1)

	require.NoError(t, slots.acquireClient())
	slots.releaseTurn()

	require.Equal(t, []string{"user"}, p.order)
	require.Zero(t, atomic.LoadInt32(&p.cache.releaseAPIKeyCalled))
	require.Equal(t, int32(1), atomic.LoadInt32(&p.cache.releaseUserCalled))
}
