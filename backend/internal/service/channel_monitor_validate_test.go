package service

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGeminiMonitorUsesValidatedModelActionURL(t *testing.T) {
	original := monitorHTTPClient
	t.Cleanup(func() { monitorHTTPClient = original })

	var requests atomic.Int32
	var requestedURL string
	monitorHTTPClient = &http.Client{Transport: channelMonitorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		requestedURL = req.URL.String()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`)),
		}, nil
	})}

	_, _, _, err := callProvider(context.Background(), MonitorProviderGemini, "https://generativelanguage.googleapis.com", "test-key", "../unsafe", "prompt", nil)
	if err == nil {
		t.Fatal("unsafe Gemini monitor model was accepted")
	}
	if requests.Load() != 0 {
		t.Fatalf("unsafe model sent %d upstream requests", requests.Load())
	}

	_, _, status, err := callProvider(context.Background(), MonitorProviderGemini, "https://generativelanguage.googleapis.com", "test-key", "gemini-2.5-flash", "prompt", nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("valid Gemini monitor call status=%d error=%v", status, err)
	}
	want := "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent"
	if requestedURL != want {
		t.Fatalf("Gemini monitor URL = %q, want %q", requestedURL, want)
	}
}

func TestChannelMonitorValidateEndpointOrigin(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		wantErr  error
	}{
		{name: "origin", endpoint: "https://1.1.1.1"},
		{name: "origin with slash", endpoint: "https://1.1.1.1/"},
		{name: "empty hostname with port", endpoint: "https://:443", wantErr: ErrChannelMonitorInvalidEndpoint},
		{name: "empty port", endpoint: "https://example.com:", wantErr: ErrChannelMonitorInvalidEndpoint},
		{name: "invalid port", endpoint: "https://example.com:https", wantErr: ErrChannelMonitorInvalidEndpoint},
		{name: "zero port", endpoint: "https://example.com:0", wantErr: ErrChannelMonitorInvalidEndpoint},
		{name: "negative port", endpoint: "https://example.com:-1", wantErr: ErrChannelMonitorInvalidEndpoint},
		{name: "out of range port", endpoint: "https://example.com:65536", wantErr: ErrChannelMonitorInvalidEndpoint},
		{name: "ipv6 zone", endpoint: "https://[2001:4860:4860::8888%25eth0]", wantErr: ErrChannelMonitorInvalidEndpoint},
		{name: "bare query delimiter", endpoint: "https://example.com?", wantErr: ErrChannelMonitorEndpointPath},
		{name: "query", endpoint: "https://example.com?key=value", wantErr: ErrChannelMonitorEndpointPath},
		{name: "bare fragment delimiter", endpoint: "https://example.com#", wantErr: ErrChannelMonitorEndpointPath},
		{name: "fragment", endpoint: "https://example.com#fragment", wantErr: ErrChannelMonitorEndpointPath},
		{name: "userinfo", endpoint: "https://user:secret@example.com", wantErr: ErrChannelMonitorEndpointPath},
		{name: "non https", endpoint: "http://example.com", wantErr: ErrChannelMonitorEndpointScheme},
		{name: "path", endpoint: "https://example.com/v1", wantErr: ErrChannelMonitorEndpointPath},
		{name: "encoded path separator", endpoint: "https://example.com/%2F", wantErr: ErrChannelMonitorEndpointPath},
		{name: "encoded query separator", endpoint: "https://example.com/%3F", wantErr: ErrChannelMonitorEndpointPath},
		{name: "encoded hash in path", endpoint: "https://example.com/%23", wantErr: ErrChannelMonitorEndpointPath},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateEndpoint(tt.endpoint); err != tt.wantErr {
				t.Fatalf("validateEndpoint(%q) error = %v, want %v", tt.endpoint, err, tt.wantErr)
			}
		})
	}
}

func TestChannelMonitorRejectsNonGlobalAddressesAtValidationAndDial(t *testing.T) {
	tests := []string{
		"0.0.0.1", "10.0.0.1", "100.64.0.1", "127.0.0.1", "169.254.169.254",
		"172.16.0.1", "192.0.0.1", "192.0.0.8", "192.0.0.170", "192.168.0.1",
		"224.0.0.1", "239.255.255.250", "240.0.0.1", "255.255.255.255",
		"192.0.2.1", "192.88.99.1", "192.88.99.2",
		"198.18.0.1", "198.51.100.1", "203.0.113.1",
		"::", "::1", "ff02::1", "64:ff9b:1::1", "100::1", "100:0:0:1::1", "2001::1",
		"2001:2::1", "2001:10::1", "2001:db8::1",
		"2002::1", "3fff::1", "5f00::1", "fc00::1", "fe80::1",
	}
	for _, raw := range tests {
		raw := raw
		t.Run(raw, func(t *testing.T) {
			ip := net.ParseIP(raw)
			if !isPrivateIP(ip) {
				t.Fatalf("isPrivateIP(%q) = false", raw)
			}
			endpointHost := raw
			if strings.Contains(raw, ":") {
				endpointHost = "[" + raw + "]"
			}
			if err := validateEndpoint("https://" + endpointHost); err != ErrChannelMonitorEndpointPrivate {
				t.Fatalf("validateEndpoint(%q) error = %v, want private-host rejection", raw, err)
			}
			address := net.JoinHostPort(raw, "443")
			_, err := safeDialContext(context.Background(), "tcp", address)
			if !errors.Is(err, errMonitorDialPolicy) {
				t.Fatalf("safeDialContext(%q) error = %v, want SSRF rejection", address, err)
			}
		})
	}
}

func TestChannelMonitorAcceptsGlobalAssignmentsAtValidationAndDial(t *testing.T) {
	tests := []string{
		"192.0.0.9", "192.0.0.10", "192.31.196.1", "192.52.193.1", "192.175.48.1",
		"64:ff9b::1", "2001:1::1", "2001:1::2", "2001:1::3", "2001:3::1",
		"2001:4:112::1", "2001:20::1", "2001:30::1", "2620:4f:8000::1",
	}
	originalDial := monitorDialContext
	monitorDialContext = func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("dial reached") }
	t.Cleanup(func() { monitorDialContext = originalDial })
	for _, raw := range tests {
		if isPrivateIP(net.ParseIP(raw)) {
			t.Fatalf("isPrivateIP(%q) = true, want public", raw)
		}
		host := raw
		if strings.Contains(raw, ":") {
			host = "[" + raw + "]"
		}
		if err := validateEndpoint("https://" + host); err != nil {
			t.Fatalf("validateEndpoint(%q) error = %v, want accepted", raw, err)
		}
		_, err := safeDialContext(context.Background(), "tcp", net.JoinHostPort(raw, "443"))
		if err == nil || err.Error() != "dial reached" {
			t.Fatalf("safeDialContext(%q) error = %v, want dial attempt", raw, err)
		}
	}
}

func TestChannelMonitorDialRejectsInvalidAuthorityAndIPv6Zones(t *testing.T) {
	addresses := []string{
		"example.com:", "example.com:0", "example.com:-1", "example.com:65536",
		"[2001:4860:4860::8888%eth0]:443",
	}
	for _, address := range addresses {
		if _, err := safeDialContext(context.Background(), "tcp", address); !errors.Is(err, errMonitorDialPolicy) {
			t.Fatalf("safeDialContext(%q) error = %v, want policy rejection", address, err)
		}
	}
}

type channelMonitorRoundTripFunc func(*http.Request) (*http.Response, error)

func (f channelMonitorRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestPostRawJSONDoesNotReturnRawURL(t *testing.T) {
	malformedURL := "https://user:secret@example.com/%zz?api_key=top-secret"
	_, _, err := postRawJSON(context.Background(), malformedURL, []byte(`{}`), nil)
	if err == nil || err.Error() != "build request failed" || strings.Contains(err.Error(), malformedURL) {
		t.Fatalf("postRawJSON() malformed URL error = %q, want sanitized build error", err)
	}

	original := monitorHTTPClient
	t.Cleanup(func() { monitorHTTPClient = original })
	monitorHTTPClient = &http.Client{
		Timeout: time.Second,
		Transport: channelMonitorRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("dial tcp 203.0.113.1:443: test failure")
		}),
	}

	rawURL := "https://user:secret@example.com/v1?api_key=top-secret"
	_, _, err = postRawJSON(context.Background(), rawURL, []byte(`{}`), nil)
	if err == nil {
		t.Fatal("postRawJSON() error = nil")
	}
	if got := err.Error(); got != "connection failed" || strings.Contains(got, rawURL) || strings.Contains(got, "203.0.113.1") {
		t.Fatalf("postRawJSON() error = %q, want sanitized error", got)
	}
}

func TestClassifyMonitorRequestErrorCategoriesAndSecrecy(t *testing.T) {
	const secret = "credential-secret.example"
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "canceled", err: fmt.Errorf("%s: %w", secret, context.Canceled), want: "request canceled"},
		{name: "timeout", err: fmt.Errorf("%s: %w", secret, context.DeadlineExceeded), want: "request timeout"},
		{name: "dns", err: &net.DNSError{Err: secret, Name: secret}, want: "DNS lookup failed"},
		{name: "tls record", err: tls.RecordHeaderError{Msg: secret}, want: "TLS handshake failed"},
		{name: "tls certificate", err: x509.UnknownAuthorityError{}, want: "TLS handshake failed"},
		{name: "connect", err: &net.OpError{Op: "dial", Err: errors.New(secret)}, want: "connection failed"},
		{name: "policy", err: fmt.Errorf("%s: %w", secret, errMonitorDialPolicy), want: "request blocked by policy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyMonitorRequestError(tt.err).Error()
			if got != tt.want || strings.Contains(got, secret) {
				t.Fatalf("category = %q, want %q without secret", got, tt.want)
			}
		})
	}
}

type failingReadCloser struct{ err error }

func (f failingReadCloser) Read([]byte) (int, error) { return 0, f.err }
func (f failingReadCloser) Close() error             { return nil }

func TestPostRawJSONResponseReadCategoryAndSecrecy(t *testing.T) {
	const secret = "body-read-secret.example"
	original := monitorHTTPClient
	monitorHTTPClient = &http.Client{Transport: channelMonitorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: failingReadCloser{err: errors.New(secret)}, Request: req}, nil
	})}
	t.Cleanup(func() { monitorHTTPClient = original })
	_, status, err := postRawJSON(context.Background(), "https://user:password@example.com/path", nil, nil)
	if status != http.StatusOK || err == nil || err.Error() != "response read failed" || strings.Contains(err.Error(), secret) {
		t.Fatalf("status = %d, error = %v", status, err)
	}
}

func TestPostRawJSONRejectsOversizedResponses(t *testing.T) {
	original := monitorHTTPClient
	t.Cleanup(func() { monitorHTTPClient = original })

	for _, statusCode := range []int{http.StatusOK, http.StatusBadGateway} {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			monitorHTTPClient = &http.Client{Transport: channelMonitorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				body := strings.Repeat("x", monitorResponseMaxBytes+1)
				return &http.Response{StatusCode: statusCode, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
			})}
			body, status, err := postRawJSON(context.Background(), "https://example.com/path", nil, nil)
			if body != nil || status != statusCode || err == nil || err.Error() != "response body too large" {
				t.Fatalf("body/status/error = %v/%d/%v", body, status, err)
			}
		})
	}
}

type countingReadCloser struct {
	reader io.Reader
	read   int
}

func (r *countingReadCloser) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.read += n
	return n, err
}

func (*countingReadCloser) Close() error { return nil }

func TestPostRawJSONReadsAtMostOverflowProbe(t *testing.T) {
	original := monitorHTTPClient
	t.Cleanup(func() { monitorHTTPClient = original })
	body := &countingReadCloser{reader: strings.NewReader(strings.Repeat("x", monitorResponseMaxBytes*2))}
	monitorHTTPClient = &http.Client{Transport: channelMonitorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: body, Request: req}, nil
	})}

	_, _, err := postRawJSON(context.Background(), "https://example.com/path", nil, nil)
	if err == nil || err.Error() != "response body too large" || body.read != monitorResponseMaxBytes+1 {
		t.Fatalf("error/read = %v/%d, want oversized/%d", err, body.read, monitorResponseMaxBytes+1)
	}
}

func TestPostRawJSONReusesConnectionForBoundedResponses(t *testing.T) {
	var newConnections int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			newConnections++
		}
	}
	defer server.Close()

	original := monitorHTTPClient
	monitorHTTPClient = server.Client()
	t.Cleanup(func() { monitorHTTPClient = original })
	for range 2 {
		if _, _, err := postRawJSON(context.Background(), server.URL, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if newConnections != 1 {
		t.Fatalf("new connections = %d, want 1", newConnections)
	}
}

func TestSafeDialContextRacesEligibleAddresses(t *testing.T) {
	originalLookup, originalDial, originalDelay := monitorLookupIPAddr, monitorDialContext, monitorDialFallbackDelay
	monitorLookupIPAddr = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("2606:4700:4700::1111")}, {IP: net.ParseIP("1.1.1.1")}}, nil
	}
	monitorDialFallbackDelay = 10 * time.Millisecond
	monitorDialContext = func(ctx context.Context, _, address string) (net.Conn, error) {
		if strings.HasPrefix(address, "[") {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		client, peer := net.Pipe()
		go func() { _ = peer.Close() }()
		return client, nil
	}
	t.Cleanup(func() {
		monitorLookupIPAddr, monitorDialContext, monitorDialFallbackDelay = originalLookup, originalDial, originalDelay
	})

	start := time.Now()
	conn, err := safeDialContext(context.Background(), "tcp", "example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("dial took %v, want staggered fallback", elapsed)
	}
}

func TestDialMonitorAddressesRejectsZeroAddresses(t *testing.T) {
	originalDial := monitorDialContext
	dialed := false
	monitorDialContext = func(context.Context, string, string) (net.Conn, error) {
		dialed = true
		return nil, errors.New("unexpected dial")
	}
	t.Cleanup(func() { monitorDialContext = originalDial })

	conn, err := dialMonitorAddresses(context.Background(), "tcp", "443", nil)
	if conn != nil || !errors.Is(err, errMonitorDialPolicy) || dialed {
		t.Fatalf("connection/error/dialed = %v/%v/%v, want nil/policy rejection/false", conn, err, dialed)
	}
}

func TestSafeDialContextProgressesPastTwoBlackholesAndCleansLosers(t *testing.T) {
	originalLookup, originalDial, originalDelay := monitorLookupIPAddr, monitorDialContext, monitorDialFallbackDelay
	monitorLookupIPAddr = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{
			{IP: net.ParseIP("1.1.1.1")},
			{IP: net.ParseIP("8.8.8.8")},
			{IP: net.ParseIP("9.9.9.9")},
		}, nil
	}
	monitorDialFallbackDelay = time.Millisecond
	t.Cleanup(func() {
		monitorLookupIPAddr, monitorDialContext, monitorDialFallbackDelay = originalLookup, originalDial, originalDelay
	})

	for iteration := range 50 {
		var active, maximum atomic.Int32
		loserClosed := make(chan struct{})
		monitorDialContext = func(ctx context.Context, _, address string) (net.Conn, error) {
			current := active.Add(1)
			defer active.Add(-1)
			for {
				observed := maximum.Load()
				if current <= observed || maximum.CompareAndSwap(observed, current) {
					break
				}
			}
			host, _, _ := net.SplitHostPort(address)
			switch host {
			case "1.1.1.1":
				<-ctx.Done()
				return nil, ctx.Err()
			case "8.8.8.8":
				<-ctx.Done()
				conn, peer := net.Pipe()
				go func() {
					defer peer.Close()
					var b [1]byte
					_, _ = peer.Read(b[:])
					close(loserClosed)
				}()
				return conn, nil
			default:
				conn, peer := net.Pipe()
				go func() { _ = peer.Close() }()
				return conn, nil
			}
		}

		conn, err := safeDialContext(context.Background(), "tcp", "example.com:443")
		if err != nil {
			t.Fatalf("iteration %d: %v", iteration, err)
		}
		_ = conn.Close()
		if got := maximum.Load(); got > 2 {
			t.Fatalf("iteration %d: maximum live dials = %d, want <= 2", iteration, got)
		}
		if got := active.Load(); got != 0 {
			t.Fatalf("iteration %d: live dials after return = %d", iteration, got)
		}
		select {
		case <-loserClosed:
		case <-time.After(time.Second):
			t.Fatalf("iteration %d: canceled loser's connection was not closed", iteration)
		}
	}
}

func TestSafeDialContextRejectsMixedPublicAndSpecialAnswersBeforeDial(t *testing.T) {
	originalLookup, originalDial := monitorLookupIPAddr, monitorDialContext
	monitorLookupIPAddr = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}, {IP: net.ParseIP("127.0.0.1")}}, nil
	}
	dialed := false
	monitorDialContext = func(context.Context, string, string) (net.Conn, error) {
		dialed = true
		return nil, errors.New("unexpected dial")
	}
	t.Cleanup(func() { monitorLookupIPAddr, monitorDialContext = originalLookup, originalDial })

	_, err := safeDialContext(context.Background(), "tcp", "example.com:443")
	if !errors.Is(err, errMonitorDialPolicy) || dialed {
		t.Fatalf("error/dialed = %v/%v, want policy rejection before dial", err, dialed)
	}
}

func TestSafeDialContextCancellationCancelsAttempts(t *testing.T) {
	originalLookup, originalDial, originalDelay := monitorLookupIPAddr, monitorDialContext, monitorDialFallbackDelay
	monitorLookupIPAddr = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}, {IP: net.ParseIP("8.8.8.8")}}, nil
	}
	monitorDialFallbackDelay = time.Millisecond
	done := make(chan struct{}, 2)
	monitorDialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		done <- struct{}{}
		return nil, ctx.Err()
	}
	t.Cleanup(func() {
		monitorLookupIPAddr, monitorDialContext, monitorDialFallbackDelay = originalLookup, originalDial, originalDelay
	})

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := safeDialContext(ctx, "tcp", "example.com:443")
		result <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want canceled", err)
	}
	for range 2 {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("dial attempt did not exit after cancellation")
		}
	}
}

func TestChannelMonitorClientDoesNotFollowRedirectOrDiscloseAuthorization(t *testing.T) {
	const secret = "Bearer channel-monitor-secret"
	var requests int
	client := newSSRFSafeHTTPClient(time.Second)
	client.Transport = channelMonitorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		if requests > 1 {
			t.Fatalf("redirect followed to %q with authorization %q", req.URL.String(), req.Header.Get("Authorization"))
		}
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": []string{"http://192.0.2.1/collect"}},
			Body:       io.NopCloser(strings.NewReader("redirect")),
			Request:    req,
		}, nil
	})

	req, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", secret)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound || requests != 1 {
		t.Fatalf("Do() status = %d, requests = %d; want 302 and one request", resp.StatusCode, requests)
	}
}

func TestChannelMonitorPersistedMessagesExcludeUpstreamControlledData(t *testing.T) {
	const hostile = `https://user:password@evil.example/path 203.0.113.7 Authorization: Bearer arbitrary-secret credential=also-secret`
	original := monitorHTTPClient
	t.Cleanup(func() { monitorHTTPClient = original })

	tests := []struct {
		name       string
		statusCode int
		wantStatus string
		wantMsg    string
	}{
		{name: "error body", statusCode: http.StatusUnauthorized, wantStatus: MonitorStatusError, wantMsg: "upstream HTTP 401"},
		{name: "challenge body", statusCode: http.StatusOK, wantStatus: MonitorStatusFailed, wantMsg: "challenge response mismatch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			monitorHTTPClient = &http.Client{Transport: channelMonitorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tt.statusCode, Body: io.NopCloser(strings.NewReader(hostile)), Header: make(http.Header), Request: req}, nil
			})}
			got := runCheckForModel(context.Background(), MonitorProviderOpenAI, "https://example.com", "api-secret", "model", nil)
			if got.Status != tt.wantStatus || got.Message != tt.wantMsg || strings.Contains(got.Message, hostile) {
				t.Fatalf("result status/message = %q/%q, want %q/%q", got.Status, got.Message, tt.wantStatus, tt.wantMsg)
			}
		})
	}
}
