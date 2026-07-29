package service

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

type httpAttemptAuthorityKey struct{}
type httpAttemptIDKey struct{}

// physicalHTTPAttemptMaxLifetime is a total headers-plus-body safety fuse for
// admitted work whose caller supplied no deadline. Client cancellation still
// cannot interrupt draining/accounting after admission.
const physicalHTTPAttemptMaxLifetime = 30 * time.Minute

type httpAttemptAuthority struct {
	clientCtx context.Context

	mu              sync.Mutex
	admitted        uint64
	canceled        error
	onFirstTransfer func() bool
	afterFirstAdmit func()
}

func withHTTPAttemptAuthority(ctx context.Context) context.Context {
	return withHTTPAttemptAuthorityOnFirst(ctx, nil, nil)
}

// WithHTTPAttemptAuthority installs one handler-attempt authority. The transfer
// callback runs while first admission is still cancelable; afterFirstAdmit runs
// only after admission commits. Later service retries still re-check client
// cancellation without retransferring handler-owned resources.
func WithHTTPAttemptAuthority(
	ctx context.Context,
	onFirstTransfer func() bool,
	afterFirstAdmit func(),
) context.Context {
	return withHTTPAttemptAuthorityOnFirst(
		ctx,
		onFirstTransfer,
		afterFirstAdmit,
	)
}

func withHTTPAttemptAuthorityOnFirst(
	ctx context.Context,
	onFirstTransfer func() bool,
	afterFirstAdmit func(),
) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Value(
		httpAttemptAuthorityKey{},
	).(*httpAttemptAuthority); ok {
		return ctx
	}
	authority := &httpAttemptAuthority{
		clientCtx:       ctx,
		onFirstTransfer: onFirstTransfer,
		afterFirstAdmit: afterFirstAdmit,
	}
	return context.WithValue(
		ctx,
		httpAttemptAuthorityKey{},
		authority,
	)
}

func httpAttemptAuthorityFromContext(ctx context.Context) *httpAttemptAuthority {
	if ctx == nil {
		return &httpAttemptAuthority{clientCtx: context.Background()}
	}
	if authority, ok := ctx.Value(httpAttemptAuthorityKey{}).(*httpAttemptAuthority); ok && authority != nil {
		return authority
	}
	return &httpAttemptAuthority{clientCtx: ctx}
}

// HTTPAttemptID returns the identity of the physical upstream request carried
// by ctx. It is absent until admission and differs across service retries.
func HTTPAttemptID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	attemptID, _ := ctx.Value(httpAttemptIDKey{}).(string)
	return attemptID
}

// admit linearizes client cancellation against one physical HTTP request. The
// original client context remains the authority for any later retry.
func (a *httpAttemptAuthority) admit() error {
	if a == nil || a.clientCtx == nil {
		return nil
	}
	if hook, _ := a.clientCtx.Value(httpAttemptAdmissionHookKey{}).(func()); hook != nil {
		hook()
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.canceled == nil {
		a.canceled = a.clientCtx.Err()
	}
	if a.canceled != nil {
		if a.admitted == 0 {
			return &HTTPUpstreamAttemptNotAdmittedError{cause: a.canceled}
		}
		return &httpUpstreamRetryNotAdmittedError{cause: a.canceled}
	}
	first := a.admitted == 0
	if first && a.onFirstTransfer != nil {
		if !a.onFirstTransfer() {
			cause := a.clientCtx.Err()
			if cause == nil {
				cause = context.Canceled
			}
			return &HTTPUpstreamAttemptNotAdmittedError{cause: cause}
		}
		if cause := a.clientCtx.Err(); cause != nil {
			a.canceled = cause
			return &HTTPUpstreamAttemptNotAdmittedError{cause: cause}
		}
	}
	a.admitted++
	if first && a.afterFirstAdmit != nil {
		a.afterFirstAdmit()
	}
	return nil
}

func doHTTPUpstream(
	ctx context.Context,
	upstream HTTPUpstream,
	req *http.Request,
	proxyURL string,
	accountID int64,
	accountConcurrency int,
) (*http.Response, error) {
	authority := httpAttemptAuthorityFromContext(ctx)
	if err := authority.admit(); err != nil {
		return nil, err
	}
	attemptID := generateRequestID()
	ownedReq, releaseAttempt := ownedHTTPAttemptRequest(req, authority.clientCtx, attemptID)
	resp, err := upstream.Do(ownedReq, proxyURL, accountID, accountConcurrency)
	return ownHTTPAttemptResponse(resp, err, releaseAttempt, ownedReq)
}

func doHTTPUpstreamWithTLS(
	ctx context.Context,
	upstream HTTPUpstream,
	req *http.Request,
	proxyURL string,
	accountID int64,
	accountConcurrency int,
	profile *tlsfingerprint.Profile,
) (*http.Response, error) {
	authority := httpAttemptAuthorityFromContext(ctx)
	if err := authority.admit(); err != nil {
		return nil, err
	}
	attemptID := generateRequestID()
	ownedReq, releaseAttempt := ownedHTTPAttemptRequest(req, authority.clientCtx, attemptID)
	resp, err := upstream.DoWithTLS(ownedReq, proxyURL, accountID, accountConcurrency, profile)
	return ownHTTPAttemptResponse(resp, err, releaseAttempt, ownedReq)
}

func ownedHTTPAttemptRequest(
	req *http.Request,
	logicalCtx context.Context,
	attemptID string,
) (*http.Request, context.CancelFunc) {
	requestCtx := logicalCtx
	if req != nil && req.Context() != nil {
		requestCtx = req.Context()
	}
	requestCtx = context.WithValue(requestCtx, httpAttemptIDKey{}, attemptID)
	attemptCtx, releaseAttempt := ownedHTTPAttemptContext(requestCtx, logicalCtx)
	if req == nil {
		return nil, releaseAttempt
	}
	return req.Clone(attemptCtx), releaseAttempt
}

func ownedHTTPAttemptContext(
	requestCtx context.Context,
	logicalCtx context.Context,
) (context.Context, context.CancelFunc) {
	if requestCtx == nil {
		requestCtx = context.Background()
	}
	deadline := time.Now().Add(physicalHTTPAttemptMaxLifetime)
	if requestDeadline, ok := requestCtx.Deadline(); ok && requestDeadline.Before(deadline) {
		deadline = requestDeadline
	}
	if logicalCtx != nil {
		if logicalDeadline, ok := logicalCtx.Deadline(); ok && logicalDeadline.Before(deadline) {
			deadline = logicalDeadline
		}
	}
	return context.WithDeadline(context.WithoutCancel(requestCtx), deadline)
}

func ownHTTPAttemptResponse(
	resp *http.Response,
	err error,
	releaseAttempt context.CancelFunc,
	ownedReq *http.Request,
) (*http.Response, error) {
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
			resp.Body = http.NoBody
		}
		releaseAttempt()
		return resp, err
	}
	if resp != nil && ownedReq != nil {
		if resp.Request == nil {
			resp.Request = ownedReq
		} else {
			attemptID := HTTPAttemptID(ownedReq.Context())
			resp.Request = resp.Request.WithContext(context.WithValue(resp.Request.Context(), httpAttemptIDKey{}, attemptID))
		}
	}
	if resp == nil || resp.Body == nil {
		releaseAttempt()
		return resp, nil
	}
	resp.Body = &httpAttemptBody{
		ReadCloser:     resp.Body,
		releaseAttempt: releaseAttempt,
	}
	return resp, nil
}

type httpAttemptBody struct {
	io.ReadCloser
	releaseOnce    sync.Once
	releaseAttempt context.CancelFunc
}

func (b *httpAttemptBody) Close() error {
	if b == nil {
		return nil
	}
	err := b.ReadCloser.Close()
	b.releaseOnce.Do(b.releaseAttempt)
	return err
}
