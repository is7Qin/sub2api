package proxyutil

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestConfigureTransportProxySOCKS5UsesBoundedForwardDialer(t *testing.T) {
	for _, scheme := range []string{"socks5", "socks5h"} {
		t.Run(scheme, func(t *testing.T) {
			proxyURL, err := url.Parse(scheme + "://127.0.0.1:1080")
			require.NoError(t, err)

			transport := &http.Transport{}
			require.NoError(t, ConfigureTransportProxy(transport, proxyURL))
			require.NotNil(t, transport.DialContext)
			require.Nil(t, transport.Proxy)
		})
	}
}

func TestConfigureTransportProxyHTTPPreservesDialContext(t *testing.T) {
	proxyURL, err := url.Parse("http://127.0.0.1:8080")
	require.NoError(t, err)

	called := false
	transport := &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			called = true
			return nil, net.ErrClosed
		},
	}
	require.NoError(t, ConfigureTransportProxy(transport, proxyURL))
	require.NotNil(t, transport.Proxy)
	_, _ = transport.DialContext(context.Background(), "tcp", "127.0.0.1:1")
	require.True(t, called)
}

func TestSOCKS5ForwardDialerHasBoundedTimeout(t *testing.T) {
	require.Greater(t, socks5ForwardDialer.Timeout, time.Duration(0))
	require.Equal(t, socks5DialTimeout, socks5ForwardDialer.Timeout)
}
