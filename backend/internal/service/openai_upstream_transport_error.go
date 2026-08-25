package service

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// openAITransportErrorTempUnschedDuration matches token refresh cooldown: long
// enough to stop proxy hammering, short enough for operator-side recovery.
const openAITransportErrorTempUnschedDuration = tokenRefreshTempUnschedDuration

var openAITransportFailoverBody = []byte(`{"error":{"type":"upstream_error","message":"Upstream request failed"}}`)

type openAITransportErrorClass struct {
	Persistent bool
}

// openAIPersistentTransportErrorMarkers are durable proxy/network fault reasons,
// not operations: a proxy timeout should fail over without evicting the account.
var openAIPersistentTransportErrorMarkers = []string{
	"authentication failed",         // SOCKS5 proxy credentials rejected.
	"proxy authentication required", // HTTP proxy 407.
	"connection refused",
	"no route to host",
	"network is unreachable",
	"no such host",
}

func classifyOpenAITransportError(err error) openAITransportErrorClass {
	if err == nil {
		return openAITransportErrorClass{}
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH) {
		return openAITransportErrorClass{Persistent: true}
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return openAITransportErrorClass{Persistent: true}
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range openAIPersistentTransportErrorMarkers {
		if strings.Contains(msg, marker) {
			return openAITransportErrorClass{Persistent: true}
		}
	}
	return openAITransportErrorClass{}
}

// handleOpenAIUpstreamTransportError handles non-HTTP upstream failures
// (proxy/DNS/TCP/TLS). It records ops diagnostics, fails over instead of writing
// a hard 502, and temporarily unschedules only durable proxy/network faults.
func (s *OpenAIGatewayService) handleOpenAIUpstreamTransportError(ctx context.Context, c *gin.Context, account *Account, err error, passthrough bool) error {
	if IsHTTPUpstreamAttemptNotAdmitted(err) || errors.Is(err, context.Canceled) {
		return err
	}
	if downstreamErr := downstreamRequestContextErr(c); downstreamErr != nil {
		return downstreamErr
	}
	safeErr := sanitizeOpenAIUpstreamDiagnosticText(err.Error())
	setOpsUpstreamError(c, 0, safeErr, "")
	if account != nil {
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: 0,
			Passthrough:        passthrough,
			Kind:               "request_error",
			Message:            safeErr,
		})
	}

	if classifyOpenAITransportError(err).Persistent {
		s.tempUnscheduleOpenAITransportError(ctx, account, safeErr)
	}
	fact := ParseTransportErrorFact(PlatformOpenAI, err)
	return &UpstreamFailoverError{
		StatusCode:   http.StatusBadGateway,
		ResponseBody: openAITransportFailoverBody,
		upstreamFact: &fact,
	}
}

func (s *OpenAIGatewayService) tempUnscheduleOpenAITransportError(ctx context.Context, account *Account, safeErr string) {
	if s == nil || account == nil {
		return
	}
	until := time.Now().Add(openAITransportErrorTempUnschedDuration)
	reason := "upstream transport error (proxy/network): " + safeErr

	// Runtime block is immediate; DB/cache propagation can lag behind the request
	// that discovered the durable proxy fault.
	s.BlockAccountScheduling(account, until, "transport_error")

	if s.accountRepo == nil {
		logger.L().With(zap.String("component", "service.openai_gateway")).Warn(
			"openai.account_temp_unscheduled_transport_memory_only",
			zap.Int64("account_id", account.ID),
			zap.String("account_name", account.Name),
			zap.String("platform", account.Platform),
			zap.Time("until", until),
			zap.String("reason", reason),
		)
		return
	}

	bgCtx, cancel := openAIAccountStateContext(ctx)
	defer cancel()
	if err := s.accountRepo.SetTempUnschedulable(bgCtx, account.ID, until, reason); err != nil {
		logger.L().With(zap.String("component", "service.openai_gateway")).Warn(
			"openai.account_temp_unscheduled_transport_failed",
			zap.Int64("account_id", account.ID),
			zap.Error(err),
		)
		return
	}
	logger.L().With(zap.String("component", "service.openai_gateway")).Warn(
		"openai.account_temp_unscheduled_transport",
		zap.Int64("account_id", account.ID),
		zap.String("account_name", account.Name),
		zap.String("platform", account.Platform),
		zap.Time("until", until),
		zap.String("reason", reason),
	)
}
