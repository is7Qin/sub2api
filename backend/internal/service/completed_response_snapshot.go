package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
)

// completedResponseSnapshot retains a bounded physical response after its body
// has been consumed and closed, so cancellation cannot erase completed work.
type completedResponseSnapshot struct {
	valid      bool
	statusCode int
	header     http.Header
	body       []byte
	request    *http.Request
}

func snapshotCompletedResponse(resp *http.Response, body []byte) completedResponseSnapshot {
	if resp == nil {
		return completedResponseSnapshot{}
	}
	return completedResponseSnapshot{
		valid:      true,
		statusCode: resp.StatusCode,
		header:     resp.Header.Clone(),
		body:       bytes.Clone(body),
		request:    resp.Request,
	}
}

func (s completedResponseSnapshot) ifRetryCanceled(ctx context.Context) (*http.Response, bool) {
	if !s.valid || ctx == nil || ctx.Err() == nil {
		return nil, false
	}
	return s.response(), true
}

// ifRetryNotAdmitted restores completed work only when the current physical
// retry was rejected before admission, never for an admitted transport error.
func (s completedResponseSnapshot) ifRetryNotAdmitted(err error) (*http.Response, bool) {
	if !s.valid || (!IsHTTPUpstreamAttemptNotAdmitted(err) && !isHTTPUpstreamRetryNotAdmitted(err)) {
		return nil, false
	}
	return s.response(), true
}

func (s completedResponseSnapshot) response() *http.Response {
	return &http.Response{
		StatusCode: s.statusCode,
		Header:     s.header.Clone(),
		Body:       io.NopCloser(bytes.NewReader(s.body)),
		Request:    s.request,
	}
}
