//go:build unit

package service

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func newSMTPTestCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, pool
}

type smtpConnectionTestServer struct {
	listener          net.Listener
	tlsConfig         *tls.Config
	advertiseStartTLS bool

	mu       sync.Mutex
	commands []string
	wg       sync.WaitGroup
}

func startSMTPConnectionTestServer(t *testing.T, implicitTLS, advertiseStartTLS bool) (*smtpConnectionTestServer, int) {
	t.Helper()
	cert, pool := newSMTPTestCert(t)
	previousPool := smtpTestRootCAs
	smtpTestRootCAs = pool
	t.Cleanup(func() { smtpTestRootCAs = previousPool })

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &smtpConnectionTestServer{
		listener:          listener,
		tlsConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		advertiseStartTLS: advertiseStartTLS,
	}
	if implicitTLS {
		server.listener = tls.NewListener(listener, server.tlsConfig)
	}
	t.Cleanup(func() {
		_ = server.listener.Close()
		server.wg.Wait()
	})

	server.wg.Add(1)
	go func() {
		defer server.wg.Done()
		for {
			conn, err := server.listener.Accept()
			if err != nil {
				return
			}
			server.wg.Add(1)
			go func() {
				defer server.wg.Done()
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				server.serve(conn, server.advertiseStartTLS)
			}()
		}
	}()

	return server, listener.Addr().(*net.TCPAddr).Port
}

func (s *smtpConnectionTestServer) record(command string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, command)
}

func (s *smtpConnectionTestServer) sawCommand(prefix string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, command := range s.commands {
		if strings.HasPrefix(strings.ToUpper(command), prefix) {
			return true
		}
	}
	return false
}

func (s *smtpConnectionTestServer) serve(conn net.Conn, allowStartTLS bool) {
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	writeLine := func(line string) bool {
		if _, err := writer.WriteString(line + "\r\n"); err != nil {
			return false
		}
		return writer.Flush() == nil
	}
	if !writeLine("220 fake.test ESMTP ready") {
		return
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		command := strings.TrimSpace(line)
		s.record(command)
		switch upper := strings.ToUpper(command); {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			ok := writeLine("250-fake.test")
			if allowStartTLS {
				ok = ok && writeLine("250-STARTTLS")
			}
			if !(ok && writeLine("250-AUTH PLAIN") && writeLine("250 OK")) {
				return
			}
		case upper == "STARTTLS" && allowStartTLS:
			if !writeLine("220 ready to start TLS") {
				return
			}
			tlsConn := tls.Server(conn, s.tlsConfig)
			if err := tlsConn.Handshake(); err != nil {
				return
			}
			s.serveUpgraded(tlsConn)
			return
		case strings.HasPrefix(upper, "AUTH"):
			if !writeLine("235 authenticated") {
				return
			}
		case upper == "QUIT":
			_ = writeLine("221 bye")
			return
		default:
			if !writeLine("250 OK") {
				return
			}
		}
	}
}

func (s *smtpConnectionTestServer) serveUpgraded(conn net.Conn) {
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		command := strings.TrimSpace(line)
		s.record(command)
		upper := strings.ToUpper(command)
		var response string
		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			response = "250-fake.test\r\n250-AUTH PLAIN\r\n250 OK\r\n"
		case strings.HasPrefix(upper, "AUTH"):
			response = "235 authenticated\r\n"
		case upper == "QUIT":
			response = "221 bye\r\n"
		default:
			response = "250 OK\r\n"
		}
		if _, err := writer.WriteString(response); err != nil || writer.Flush() != nil {
			return
		}
		if upper == "QUIT" {
			return
		}
	}
}

func testSMTPConfig(port int, useTLS bool) *SMTPConfig {
	return &SMTPConfig{Host: "127.0.0.1", Port: port, Username: "user", Password: "pass", From: "from@example.com", UseTLS: useTLS}
}

func TestSMTPConnectionImplicitTLS(t *testing.T) {
	server, port := startSMTPConnectionTestServer(t, true, false)
	err := (&EmailService{}).TestSMTPConnectionWithConfig(testSMTPConfig(port, true))
	if err != nil {
		t.Fatalf("TestSMTPConnectionWithConfig: %v", err)
	}
	if !server.sawCommand("AUTH") {
		t.Fatal("expected authenticated implicit TLS connection")
	}
}

func TestSMTPConnectionStartTLSFallbackWhenTLSEnabled(t *testing.T) {
	server, port := startSMTPConnectionTestServer(t, false, true)
	err := (&EmailService{}).TestSMTPConnectionWithConfig(testSMTPConfig(port, true))
	if err != nil {
		t.Fatalf("TestSMTPConnectionWithConfig: %v", err)
	}
	if !server.sawCommand("STARTTLS") || !server.sawCommand("AUTH") {
		t.Fatal("expected mandatory STARTTLS fallback and authentication")
	}
}

func TestSMTPConnectionMandatoryStartTLSRefusesPlaintext(t *testing.T) {
	server, port := startSMTPConnectionTestServer(t, false, false)
	err := (&EmailService{}).TestSMTPConnectionWithConfig(testSMTPConfig(port, true))
	if err == nil || !strings.Contains(err.Error(), "does not support STARTTLS") {
		t.Fatalf("error = %v, want mandatory STARTTLS failure", err)
	}
	if server.sawCommand("AUTH") {
		t.Fatal("credentials must not be sent over plaintext")
	}
}

func TestSMTPConnectionOpportunisticStartTLSWhenTLSDisabled(t *testing.T) {
	server, port := startSMTPConnectionTestServer(t, false, true)
	err := (&EmailService{}).TestSMTPConnectionWithConfig(testSMTPConfig(port, false))
	if err != nil {
		t.Fatalf("TestSMTPConnectionWithConfig: %v", err)
	}
	if !server.sawCommand("STARTTLS") || !server.sawCommand("AUTH") {
		t.Fatal("expected opportunistic STARTTLS and authentication")
	}
}

func TestSMTPConnectionPlainWhenNoStartTLS(t *testing.T) {
	server, port := startSMTPConnectionTestServer(t, false, false)
	err := (&EmailService{}).TestSMTPConnectionWithConfig(testSMTPConfig(port, false))
	if err != nil {
		t.Fatalf("TestSMTPConnectionWithConfig: %v", err)
	}
	if server.sawCommand("STARTTLS") || !server.sawCommand("AUTH") {
		t.Fatal("expected configured plaintext authentication without STARTTLS")
	}
}
