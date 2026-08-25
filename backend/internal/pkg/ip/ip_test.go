//go:build unit

package ip

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestIsPrivateIP(t *testing.T) {
	tests := []struct {
		name     string
		ip       string
		expected bool
	}{
		// 私有 IPv4
		{"10.x 私有地址", "10.0.0.1", true},
		{"10.x 私有地址段末", "10.255.255.255", true},
		{"172.16.x 私有地址", "172.16.0.1", true},
		{"172.31.x 私有地址", "172.31.255.255", true},
		{"192.168.x 私有地址", "192.168.1.1", true},
		{"127.0.0.1 本地回环", "127.0.0.1", true},
		{"127.x 回环段", "127.255.255.255", true},

		// 公网 IPv4
		{"8.8.8.8 公网 DNS", "8.8.8.8", false},
		{"1.1.1.1 公网", "1.1.1.1", false},
		{"172.15.255.255 非私有", "172.15.255.255", false},
		{"172.32.0.0 非私有", "172.32.0.0", false},
		{"11.0.0.1 公网", "11.0.0.1", false},

		// IPv6
		{"::1 IPv6 回环", "::1", true},
		{"fc00:: IPv6 私有", "fc00::1", true},
		{"fd00:: IPv6 私有", "fd00::1", true},
		{"2001:db8::1 IPv6 公网", "2001:db8::1", false},

		// 无效输入
		{"空字符串", "", false},
		{"非法字符串", "not-an-ip", false},
		{"不完整 IP", "192.168", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isPrivateIP(tc.ip)
			require.Equal(t, tc.expected, got, "isPrivateIP(%q)", tc.ip)
		})
	}
}

func TestRequestClientIPMiddlewareSnapshotsModeAndParsesCustomHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)

	settings := RequestSettings{TrustForwardedIP: true, Headers: []string{"X-Cdn-IP", "True-Client-IP"}}
	r := gin.New()
	require.NoError(t, r.SetTrustedProxies([]string{"10.0.0.1"}))
	r.Use(RequestMiddleware(func() RequestSettings { return settings }))
	r.GET("/t", func(c *gin.Context) {
		// A runtime update must affect only the next request.
		settings = RequestSettings{TrustForwardedIP: false}
		c.String(200, GetClientIP(c))
	})

	req := httptest.NewRequest("GET", "/t", nil)
	req.RemoteAddr = "10.0.0.1:12345"
	req.Header.Add("X-Cdn-IP", "bad, 203.0.113.7")
	req.Header.Add("X-Cdn-IP", "198.51.100.8")
	req.Header.Set("True-Client-IP", "192.0.2.9")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, "203.0.113.7", w.Body.String())

	req = httptest.NewRequest("GET", "/t", nil)
	req.RemoteAddr = "10.0.0.1:12345"
	req.Header.Set("X-Cdn-IP", "203.0.113.7")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, "10.0.0.1", w.Body.String())
}

func TestGetClientIPWithoutRequestMiddlewareIgnoresRawForwardingHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	require.NoError(t, r.SetTrustedProxies(nil))
	r.GET("/t", func(c *gin.Context) { c.String(200, GetClientIP(c)) })

	req := httptest.NewRequest("GET", "/t", nil)
	req.RemoteAddr = "9.9.9.9:12345"
	for _, header := range []string{"CF-Connecting-IP", "X-Real-IP", "X-Forwarded-For", "X-Cdn-IP"} {
		req.Header.Set(header, "1.2.3.4")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, "9.9.9.9", w.Body.String())
}

func TestGetSecurityClientIPUsesOnlyGinTrustedProxyResolution(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, tt := range []struct {
		name           string
		trustedProxies []string
		want           string
	}{
		{name: "untrusted direct client ignores raw headers", trustedProxies: nil, want: "9.9.9.9"},
		{name: "trusted proxy uses Gin forwarded chain", trustedProxies: []string{"9.9.9.9"}, want: "1.2.3.4"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := gin.New()
			require.NoError(t, r.SetTrustedProxies(tt.trustedProxies))
			r.Use(RequestMiddleware(func() RequestSettings {
				return RequestSettings{TrustForwardedIP: true, Headers: []string{"X-Cdn-IP"}}
			}))
			r.GET("/t", func(c *gin.Context) { c.String(200, GetSecurityClientIP(c)) })

			req := httptest.NewRequest("GET", "/t", nil)
			req.RemoteAddr = "9.9.9.9:12345"
			for _, header := range []string{"CF-Connecting-IP", "X-Real-IP", "X-Forwarded-For", "X-Cdn-IP"} {
				req.Header.Set(header, "1.2.3.4")
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			require.Equal(t, tt.want, w.Body.String())
		})
	}
}

func TestResolveLegacyMetadataClientIPUsesOrderedRepeatedAndCommaValues(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Add("X-First", "invalid, 203.0.113.4")
	req.Header.Add("X-First", "198.51.100.5")
	req.Header.Set("X-Second", "192.0.2.6")
	require.Equal(t, "203.0.113.4", ResolveLegacyMetadataClientIP(req.Header, []string{"X-First", "X-Second"}))
}

func TestResolveLegacyMetadataClientIPPrefersPublicAcrossRepeatedAndCommaValues(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Add("X-Cdn-IP", "172.18.0.4, 10.0.0.2")
	req.Header.Add("X-Cdn-IP", "invalid, 203.0.113.9")
	require.Equal(t, "203.0.113.9", ResolveLegacyMetadataClientIP(req.Header, []string{"X-Cdn-IP"}))
}

func TestResolveLegacyMetadataClientIPFallsBackToFirstValidPrivateCandidate(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Add("X-Cdn-IP", "invalid, 172.18.0.4")
	req.Header.Add("X-Cdn-IP", "10.0.0.2")
	require.Equal(t, "172.18.0.4", ResolveLegacyMetadataClientIP(req.Header, []string{"X-Cdn-IP"}))
}

func TestGetTrustedClientIPUsesGinClientIP(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	require.NoError(t, r.SetTrustedProxies(nil))

	r.GET("/t", func(c *gin.Context) {
		c.String(200, GetTrustedClientIP(c))
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/t", nil)
	req.RemoteAddr = "9.9.9.9:12345"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.Header.Set("X-Real-IP", "1.2.3.4")
	req.Header.Set("CF-Connecting-IP", "1.2.3.4")
	r.ServeHTTP(w, req)

	require.Equal(t, 200, w.Code)
	require.Equal(t, "9.9.9.9", w.Body.String())
}

func TestCheckIPRestrictionWithCompiledRules(t *testing.T) {
	whitelist := CompileIPRules([]string{"10.0.0.0/8", "192.168.1.2"})
	blacklist := CompileIPRules([]string{"10.1.1.1"})

	allowed, reason := CheckIPRestrictionWithCompiledRules("10.2.3.4", whitelist, blacklist)
	require.True(t, allowed)
	require.Equal(t, "", reason)

	allowed, reason = CheckIPRestrictionWithCompiledRules("10.1.1.1", whitelist, blacklist)
	require.False(t, allowed)
	require.Equal(t, "access denied", reason)
}

func TestCheckIPRestrictionWithCompiledRules_InvalidWhitelistStillDenies(t *testing.T) {
	// 与旧实现保持一致：白名单有配置但全无效时，最终应拒绝访问。
	invalidWhitelist := CompileIPRules([]string{"not-a-valid-pattern"})
	allowed, reason := CheckIPRestrictionWithCompiledRules("8.8.8.8", invalidWhitelist, nil)
	require.False(t, allowed)
	require.Equal(t, "access denied", reason)
}
