package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type attemptRecordingUpstream struct {
	mu         sync.Mutex
	contexts   []context.Context
	attemptIDs []string
	calls      int
	onDo       func(int)
	response   *http.Response
	err        error
}

func (u *attemptRecordingUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.mu.Lock()
	u.calls++
	call := u.calls
	if req != nil {
		u.contexts = append(u.contexts, req.Context())
		u.attemptIDs = append(u.attemptIDs, HTTPAttemptID(req.Context()))
	}
	onDo := u.onDo
	resp := u.response
	err := u.err
	u.mu.Unlock()
	if onDo != nil {
		onDo(call)
	}
	if resp != nil || err != nil {
		return resp, err
	}
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
}

func (u *attemptRecordingUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

func TestHTTPAttemptAuthorityRejectsCancellationBeforeFirstPhysicalRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ctx = withHTTPAttemptAuthority(WithHTTPAttemptAdmissionHook(ctx, cancel))
	upstream := &attemptRecordingUpstream{}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.com", nil)
	require.NoError(t, err)

	_, err = doHTTPUpstream(ctx, upstream, req, "", 1, 1)

	require.True(t, IsHTTPUpstreamAttemptNotAdmitted(err))
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, upstream.calls)
}

func TestHTTPAttemptAuthorityAdmittedRequestSurvivesClientCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ctx = withHTTPAttemptAuthority(ctx)
	upstream := &attemptRecordingUpstream{onDo: func(int) { cancel() }}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.com", nil)
	require.NoError(t, err)

	_, err = doHTTPUpstream(ctx, upstream, req, "", 1, 1)

	require.NoError(t, err)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.Len(t, upstream.contexts, 1)
	require.False(t, errors.Is(upstream.contexts[0].Err(), context.Canceled))
}

func TestHTTPAttemptAuthorityPreservesRequestContextValues(t *testing.T) {
	type requestValueKey struct{}
	ctx := context.WithValue(
		context.Background(),
		requestValueKey{},
		"request-value",
	)
	ctx = withHTTPAttemptAuthority(ctx)
	upstream := &attemptRecordingUpstream{}
	requestCtx := WithHTTPUpstreamProfile(
		context.WithValue(
			ctx,
			requestValueKey{},
			"request-value",
		),
		HTTPUpstreamProfileOpenAI,
	)
	req, err := http.NewRequestWithContext(
		requestCtx,
		http.MethodPost,
		"https://example.com",
		nil,
	)
	require.NoError(t, err)

	_, err = doHTTPUpstream(ctx, upstream, req, "", 1, 1)

	require.NoError(t, err)
	require.Len(t, upstream.contexts, 1)
	require.Equal(
		t,
		"request-value",
		upstream.contexts[0].Value(requestValueKey{}),
	)
	require.Equal(
		t,
		HTTPUpstreamProfileOpenAI,
		HTTPUpstreamProfileFromContext(upstream.contexts[0]),
	)
}

func TestHTTPAttemptAuthorityTransfersOwnershipOnlyOnFirstPhysicalRequest(t *testing.T) {
	calls := 0
	ctx := WithHTTPAttemptAuthority(
		context.Background(),
		func() bool {
			calls++
			return calls == 1
		},
		nil,
	)
	upstream := &attemptRecordingUpstream{}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"https://example.com",
		nil,
	)
	require.NoError(t, err)

	_, err = doHTTPUpstream(ctx, upstream, req, "", 1, 1)
	require.NoError(t, err)
	_, err = doHTTPUpstream(ctx, upstream, req, "", 1, 1)

	require.NoError(t, err)
	require.Equal(t, 1, calls)
	require.Equal(t, 2, upstream.calls)
}

func TestHTTPAttemptAuthorityRejectsCancellationDuringFirstTransfer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	admittedSideEffects := 0
	ctx = WithHTTPAttemptAuthority(
		ctx,
		func() bool {
			cancel()
			return true
		},
		func() { admittedSideEffects++ },
	)
	upstream := &attemptRecordingUpstream{}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"https://example.com",
		nil,
	)
	require.NoError(t, err)

	_, err = doHTTPUpstream(ctx, upstream, req, "", 1, 1)

	require.True(t, IsHTTPUpstreamAttemptNotAdmitted(err))
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, upstream.calls)
	require.Zero(t, admittedSideEffects)
}

func TestHTTPAttemptAuthorityAssignsIdentityOnlyAfterAdmission(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ctx = withHTTPAttemptAuthority(WithHTTPAttemptAdmissionHook(ctx, cancel))
	upstream := &attemptRecordingUpstream{}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.com", nil)
	require.NoError(t, err)

	require.Empty(t, HTTPAttemptID(ctx))
	_, err = doHTTPUpstream(ctx, upstream, req, "", 1, 1)

	require.True(t, IsHTTPUpstreamAttemptNotAdmitted(err))
	require.Empty(t, HTTPAttemptID(ctx))
	require.Empty(t, upstream.attemptIDs)
}

func TestHTTPAttemptAuthorityAssignsDistinctIdentityToEachServiceRetry(t *testing.T) {
	ctx := withHTTPAttemptAuthority(context.Background())
	upstream := &attemptRecordingUpstream{}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.com", nil)
	require.NoError(t, err)

	resp, err := doHTTPUpstream(ctx, upstream, req, "", 1, 1)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	resp, err = doHTTPUpstream(ctx, upstream, req, "", 1, 1)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Len(t, upstream.attemptIDs, 2)
	require.NotEmpty(t, upstream.attemptIDs[0])
	require.NotEmpty(t, upstream.attemptIDs[1])
	require.NotEqual(t, upstream.attemptIDs[0], upstream.attemptIDs[1])
}

func TestHTTPAttemptAuthorityRunsSideEffectsAfterFirstAdmission(t *testing.T) {
	transfers := 0
	admittedSideEffects := 0
	ctx := WithHTTPAttemptAuthority(
		context.Background(),
		func() bool {
			transfers++
			return true
		},
		func() { admittedSideEffects++ },
	)
	upstream := &attemptRecordingUpstream{}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"https://example.com",
		nil,
	)
	require.NoError(t, err)

	resp, err := doHTTPUpstream(ctx, upstream, req, "", 1, 1)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	resp, err = doHTTPUpstream(ctx, upstream, req, "", 1, 1)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Equal(t, 1, transfers)
	require.Equal(t, 1, admittedSideEffects)
	require.Equal(t, 2, upstream.calls)
}

func TestHTTPAttemptAuthorityRejectsFailedOwnershipTransfer(t *testing.T) {
	calls := 0
	ctx := WithHTTPAttemptAuthority(
		context.Background(),
		func() bool {
			calls++
			return false
		},
		nil,
	)
	upstream := &attemptRecordingUpstream{}
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"https://example.com",
		nil,
	)
	require.NoError(t, err)

	_, err = doHTTPUpstream(ctx, upstream, req, "", 1, 1)

	require.True(t, IsHTTPUpstreamAttemptNotAdmitted(err))
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, upstream.calls)

	_, err = doHTTPUpstream(ctx, upstream, req, "", 1, 1)
	require.True(t, IsHTTPUpstreamAttemptNotAdmitted(err))
	require.Equal(t, 2, calls)
	require.Zero(t, upstream.calls)
}

type attemptBodyProbe struct {
	ctx         context.Context
	closed      chan struct{}
	contextDone chan struct{}
	closeOnce   sync.Once
	doneOnce    sync.Once
}

func newAttemptBodyProbe(ctx context.Context) *attemptBodyProbe {
	probe := &attemptBodyProbe{
		ctx:         ctx,
		closed:      make(chan struct{}),
		contextDone: make(chan struct{}),
	}
	go func() {
		<-ctx.Done()
		probe.doneOnce.Do(func() { close(probe.contextDone) })
	}()
	return probe
}

func (b *attemptBodyProbe) Read([]byte) (int, error) { return 0, io.EOF }

func (b *attemptBodyProbe) Close() error {
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

type orderedAttemptBody struct {
	ctx              context.Context
	closedBeforeDone bool
}

func (b *orderedAttemptBody) Read([]byte) (int, error) { return 0, io.EOF }

func (b *orderedAttemptBody) Close() error {
	b.closedBeforeDone = b.ctx.Err() == nil
	return nil
}

type errorResponseAttemptUpstream struct {
	body *orderedAttemptBody
}

func (u *errorResponseAttemptUpstream) Do(
	req *http.Request,
	_ string,
	_ int64,
	_ int,
) (*http.Response, error) {
	u.body = &orderedAttemptBody{ctx: req.Context()}
	return &http.Response{
		StatusCode: http.StatusBadGateway,
		Body:       u.body,
	}, errors.New("transport failed with response")
}

func (u *errorResponseAttemptUpstream) DoWithTLS(
	req *http.Request,
	proxyURL string,
	accountID int64,
	concurrency int,
	_ *tlsfingerprint.Profile,
) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

func TestHTTPAttemptAuthorityClosesErrorResponseBeforeCancel(t *testing.T) {
	for _, withTLS := range []bool{false, true} {
		t.Run(fmt.Sprintf("tls=%t", withTLS), func(t *testing.T) {
			ctx := withHTTPAttemptAuthority(context.Background())
			upstream := &errorResponseAttemptUpstream{}
			req, err := http.NewRequestWithContext(
				ctx,
				http.MethodPost,
				"https://example.com",
				nil,
			)
			require.NoError(t, err)

			var resp *http.Response
			if withTLS {
				resp, err = doHTTPUpstreamWithTLS(ctx, upstream, req, "", 1, 1, nil)
			} else {
				resp, err = doHTTPUpstream(ctx, upstream, req, "", 1, 1)
			}

			require.EqualError(t, err, "transport failed with response")
			require.NotNil(t, resp)
			require.Equal(t, http.NoBody, resp.Body)
			require.NoError(t, resp.Body.Close())
			require.NotNil(t, upstream.body)
			require.True(t, upstream.body.closedBeforeDone)
			require.Eventually(t, func() bool {
				return upstream.body.ctx.Err() != nil
			}, time.Second, time.Millisecond)
		})
	}
}

func TestHTTPAttemptAuthorityOwnsContextUntilBodyClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ctx = withHTTPAttemptAuthority(ctx)
	var body *attemptBodyProbe

	requestCtx, releaseRequest := context.WithTimeout(ctx, time.Minute)
	defer releaseRequest()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, "https://example.com", nil)
	require.NoError(t, err)

	// Use a local upstream whose body observes the admitted request context.
	owned := &attemptBodyUpstream{}
	resp, err := doHTTPUpstream(ctx, owned, req, "", 1, 1)
	require.NoError(t, err)
	body = owned.body
	cancel()
	require.NoError(t, body.ctx.Err())

	require.NoError(t, resp.Body.Close())
	<-body.closed
	<-body.contextDone
	require.ErrorIs(t, body.ctx.Err(), context.Canceled)
}

type attemptBodyUpstream struct {
	body *attemptBodyProbe
}

func (u *attemptBodyUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.body = newAttemptBodyProbe(req.Context())
	return &http.Response{StatusCode: http.StatusOK, Body: u.body}, nil
}

func (u *attemptBodyUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, concurrency)
}

func TestHTTPAttemptAuthorityRestoresLogicalDeadline(t *testing.T) {
	logicalCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	logicalCtx = withHTTPAttemptAuthority(logicalCtx)
	requestCtx := context.WithoutCancel(logicalCtx)
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, "https://example.com", nil)
	require.NoError(t, err)
	upstream := &attemptRecordingUpstream{}

	resp, err := doHTTPUpstream(logicalCtx, upstream, req, "", 1, 1)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Len(t, upstream.contexts, 1)
	got, ok := upstream.contexts[0].Deadline()
	require.True(t, ok)
	want, ok := logicalCtx.Deadline()
	require.True(t, ok)
	require.WithinDuration(t, want, got, time.Millisecond)
}

func TestHTTPAttemptAuthorityBoundsDeadlineFreeAttempt(t *testing.T) {
	ctx := withHTTPAttemptAuthority(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.com", nil)
	require.NoError(t, err)
	upstream := &attemptRecordingUpstream{}
	started := time.Now()

	resp, err := doHTTPUpstream(ctx, upstream, req, "", 1, 1)
	require.NoError(t, err)
	defer resp.Body.Close()
	got, ok := upstream.contexts[0].Deadline()
	require.True(t, ok)
	require.WithinDuration(t, started.Add(physicalHTTPAttemptMaxLifetime), got, time.Second)
}

func TestHTTPAttemptAuthoritySuppressesRetryAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ctx = withHTTPAttemptAuthority(ctx)
	upstream := &attemptRecordingUpstream{onDo: func(call int) {
		if call == 1 {
			cancel()
		}
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.com", nil)
	require.NoError(t, err)

	_, err = doHTTPUpstreamWithTLS(ctx, upstream, req, "", 1, 1, nil)
	require.NoError(t, err)
	_, err = doHTTPUpstreamWithTLS(ctx, upstream, req, "", 1, 1, nil)

	require.ErrorIs(t, err, context.Canceled)
	require.False(t, IsHTTPUpstreamAttemptNotAdmitted(err))
	require.True(t, isHTTPUpstreamRetryNotAdmitted(err))
	require.Equal(t, 1, upstream.calls)
}
