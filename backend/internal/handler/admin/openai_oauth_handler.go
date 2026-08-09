package admin

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// OpenAIOAuthHandler handles OpenAI OAuth-related operations
type openAIQuotaService interface {
	QueryUsage(ctx context.Context, accountID int64) (*service.OpenAIQuotaUsage, error)
	ResetCredit(ctx context.Context, accountID int64) (*service.OpenAIQuotaResetResult, error)
}

type openAIAccountStateRecoverer interface {
	RecoverAccountState(ctx context.Context, accountID int64, options service.AccountRecoveryOptions) (*service.SuccessfulTestRecoveryResult, error)
}

type OpenAIOAuthHandler struct {
	openaiOAuthService *service.OpenAIOAuthService
	adminService       service.AdminService
	quotaService       openAIQuotaService
	rateLimitService   openAIAccountStateRecoverer
}

const (
	openAIQuotaResetWarningAccountRecoveryFailed = "account_state_recovery_failed"
	openAIQuotaResetWarningAccountRefreshFailed  = "account_state_refresh_failed"
	openAIQuotaResetWarningCacheRefreshFailed    = "reset_credit_cache_refresh_failed"
	openAIQuotaResetPostProcessTimeout           = 8 * time.Second
)

type openAIQuotaResetResponse struct {
	service.OpenAIQuotaResetResult
	Quota                 *service.OpenAIQuotaUsage `json:"quota,omitempty"`
	Account               *dto.Account              `json:"account,omitempty"`
	AccountStateRecovered bool                      `json:"account_state_recovered"`
	WarningCode           string                    `json:"warning_code,omitempty"`
}

func openAIQuotaResetPostProcessContext(ctx context.Context) (context.Context, context.CancelFunc) {
	base := context.Background()
	if ctx != nil {
		base = context.WithoutCancel(ctx)
	}
	return context.WithTimeout(base, openAIQuotaResetPostProcessTimeout)
}

func oauthPlatformFromPath(c *gin.Context) string {
	return service.PlatformOpenAI
}

var openAIOAuthServerOwnedCredentialKeys = map[string]struct{}{
	"client_id":               {},
	"email":                   {},
	"chatgpt_account_id":      {},
	"chatgpt_user_id":         {},
	"organization_id":         {},
	"plan_type":               {},
	"subscription_expires_at": {},
}

var openAIOAuthCreateCredentialOptionKeys = map[string]struct{}{
	"compact_model_mapping":        {},
	"custom_error_codes":           {},
	"custom_error_codes_enabled":   {},
	"intercept_warmup_requests":    {},
	"model_mapping":                {},
	"pool_mode":                    {},
	"pool_mode_retry_count":        {},
	"pool_mode_retry_status_codes": {},
	"temp_unschedulable_enabled":   {},
	"temp_unschedulable_rules":     {},
}

func mergeOpenAIOAuthCreateCredentialOptions(base, incoming map[string]any) map[string]any {
	if base == nil {
		base = map[string]any{}
	}
	for key, value := range incoming {
		if key == "" || service.IsSensitiveCredentialKey(key) {
			continue
		}
		if _, serverOwned := openAIOAuthServerOwnedCredentialKeys[key]; serverOwned {
			continue
		}
		if _, allowed := openAIOAuthCreateCredentialOptionKeys[key]; !allowed {
			continue
		}
		base[key] = value
	}
	return base
}

type openAIOAuthTokenInfoResponse struct {
	AccessToken           string `json:"access_token"`
	RefreshToken          string `json:"refresh_token"`
	IDToken               string `json:"id_token,omitempty"`
	ExpiresIn             int64  `json:"expires_in"`
	ExpiresAt             int64  `json:"expires_at"`
	ClientID              string `json:"client_id,omitempty"`
	Email                 string `json:"email,omitempty"`
	ChatGPTAccountID      string `json:"chatgpt_account_id,omitempty"`
	ChatGPTUserID         string `json:"chatgpt_user_id,omitempty"`
	OrganizationID        string `json:"organization_id,omitempty"`
	PlanType              string `json:"plan_type,omitempty"`
	SubscriptionExpiresAt string `json:"subscription_expires_at,omitempty"`
	PrivacyMode           string `json:"privacy_mode,omitempty"`
}

func openAIOAuthTokenResponse(tokenInfo *service.OpenAITokenInfo) *openAIOAuthTokenInfoResponse {
	if tokenInfo == nil {
		return nil
	}
	return &openAIOAuthTokenInfoResponse{
		AccessToken:           tokenInfo.AccessToken,
		RefreshToken:          tokenInfo.RefreshToken,
		IDToken:               tokenInfo.IDToken,
		ExpiresIn:             tokenInfo.ExpiresIn,
		ExpiresAt:             tokenInfo.ExpiresAt,
		ClientID:              tokenInfo.ClientID,
		Email:                 tokenInfo.Email,
		ChatGPTAccountID:      tokenInfo.ChatGPTAccountID,
		ChatGPTUserID:         tokenInfo.ChatGPTUserID,
		OrganizationID:        tokenInfo.OrganizationID,
		PlanType:              tokenInfo.PlanType,
		SubscriptionExpiresAt: tokenInfo.SubscriptionExpiresAt,
		PrivacyMode:           tokenInfo.PrivacyMode,
	}
}

func openAIOAuthProxyURL(ctx context.Context, adminService service.AdminService, proxyID *int64) string {
	if proxyID == nil {
		return ""
	}
	proxy, err := adminService.GetProxy(ctx, *proxyID)
	if err != nil || proxy == nil {
		return ""
	}
	return proxy.URL()
}

type openAIOAuthCreateAccountParams struct {
	Name                    string
	Notes                   *string
	Credentials             map[string]any
	Extra                   map[string]any
	ProxyID                 *int64
	Concurrency             int
	LoadFactor              *int
	Priority                int
	RateMultiplier          *float64
	GroupIDs                []int64
	ExpiresAt               *int64
	AutoPauseOnExpired      *bool
	ConfirmMixedChannelRisk *bool
}

func (h *OpenAIOAuthHandler) createOpenAIOAuthAccount(c *gin.Context, tokenInfo *service.OpenAITokenInfo, params openAIOAuthCreateAccountParams) {
	// Build credentials from token info and merge only non-sensitive create options from the request.
	credentials := mergeOpenAIOAuthCreateCredentialOptions(h.openaiOAuthService.BuildAccountCredentials(tokenInfo), params.Credentials)

	name := params.Name
	if name == "" && tokenInfo.Email != "" {
		name = tokenInfo.Email
	}
	if name == "" {
		name = "OpenAI OAuth Account"
	}

	if params.RateMultiplier != nil && *params.RateMultiplier < 0 {
		response.BadRequest(c, "rate_multiplier must be >= 0")
		return
	}
	if params.LoadFactor != nil && *params.LoadFactor > 10000 {
		response.BadRequest(c, "load_factor must be <= 10000")
		return
	}
	if tokenInfo.CodexFingerprint == nil {
		response.BadRequest(c, "OpenAI OAuth session fingerprint is missing")
		return
	}

	// Create account. The Codex fingerprint is passed separately so generic Extra
	// normalization still strips client-supplied fingerprint placeholders.
	account, err := h.adminService.CreateAccount(c.Request.Context(), &service.CreateAccountInput{
		Name:                   name,
		Notes:                  params.Notes,
		Platform:               oauthPlatformFromPath(c),
		Type:                   "oauth",
		Credentials:            credentials,
		Extra:                  params.Extra,
		ProxyID:                params.ProxyID,
		Concurrency:            params.Concurrency,
		Priority:               params.Priority,
		RateMultiplier:         params.RateMultiplier,
		LoadFactor:             params.LoadFactor,
		GroupIDs:               params.GroupIDs,
		ExpiresAt:              params.ExpiresAt,
		AutoPauseOnExpired:     params.AutoPauseOnExpired,
		SkipMixedChannelCheck:  params.ConfirmMixedChannelRisk != nil && *params.ConfirmMixedChannelRisk,
		OpenAICodexFingerprint: tokenInfo.CodexFingerprint,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, dto.AccountFromService(account))
}

// NewOpenAIOAuthHandler creates a new OpenAI OAuth handler
func NewOpenAIOAuthHandler(
	openaiOAuthService *service.OpenAIOAuthService,
	adminService service.AdminService,
	quotaService *service.OpenAIQuotaService,
	rateLimitService *service.RateLimitService,
) *OpenAIOAuthHandler {
	h := &OpenAIOAuthHandler{
		openaiOAuthService: openaiOAuthService,
		adminService:       adminService,
	}
	if quotaService != nil {
		h.quotaService = quotaService
	}
	if rateLimitService != nil {
		h.rateLimitService = rateLimitService
	}
	return h
}

// OpenAIGenerateAuthURLRequest represents the request for generating OpenAI auth URL
type OpenAIGenerateAuthURLRequest struct {
	ProxyID     *int64 `json:"proxy_id"`
	RedirectURI string `json:"redirect_uri"`
	AccountID   *int64 `json:"account_id"`
}

// GenerateAuthURL generates OpenAI OAuth authorization URL
// POST /api/v1/admin/openai/generate-auth-url
func (h *OpenAIOAuthHandler) GenerateAuthURL(c *gin.Context) {
	var req OpenAIGenerateAuthURLRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		// Allow empty body
		req = OpenAIGenerateAuthURLRequest{}
	}

	var account *service.Account
	if req.AccountID != nil {
		loaded, err := h.adminService.GetAccount(c.Request.Context(), *req.AccountID)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		platform := oauthPlatformFromPath(c)
		if loaded.Platform != platform || !loaded.IsOpenAIOAuthLike() {
			response.BadRequest(c, "Account platform or type does not match OpenAI OAuth endpoint")
			return
		}
		account = loaded
		if req.ProxyID == nil {
			req.ProxyID = loaded.ProxyID
		}
	}

	result, err := h.openaiOAuthService.GenerateAuthURLWithInput(c.Request.Context(), service.OpenAIAuthURLInput{
		ProxyID:     req.ProxyID,
		RedirectURI: req.RedirectURI,
		Platform:    oauthPlatformFromPath(c),
		Account:     account,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, result)
}

// OpenAIExchangeCodeRequest represents the request for exchanging OpenAI auth code
type OpenAIExchangeCodeRequest struct {
	SessionID   string `json:"session_id" binding:"required"`
	Code        string `json:"code" binding:"required"`
	State       string `json:"state" binding:"required"`
	RedirectURI string `json:"redirect_uri"`
	ProxyID     *int64 `json:"proxy_id"`
}

// ExchangeCode exchanges OpenAI authorization code for tokens
// POST /api/v1/admin/openai/exchange-code
func (h *OpenAIOAuthHandler) ExchangeCode(c *gin.Context) {
	var req OpenAIExchangeCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	tokenInfo, err := h.openaiOAuthService.ExchangeCode(c.Request.Context(), &service.OpenAIExchangeCodeInput{
		SessionID:   req.SessionID,
		Code:        req.Code,
		State:       req.State,
		RedirectURI: req.RedirectURI,
		ProxyID:     req.ProxyID,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, openAIOAuthTokenResponse(tokenInfo))
}

// OpenAIRefreshTokenRequest represents the request for refreshing OpenAI token
type OpenAIRefreshTokenRequest struct {
	RefreshToken string `json:"refresh_token"`
	RT           string `json:"rt"`
	ClientID     string `json:"client_id"`
	ProxyID      *int64 `json:"proxy_id"`
}

// RefreshToken refreshes an OpenAI OAuth token
// POST /api/v1/admin/openai/refresh-token
func (h *OpenAIOAuthHandler) RefreshToken(c *gin.Context) {
	var req OpenAIRefreshTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	refreshToken := strings.TrimSpace(req.RefreshToken)
	if refreshToken == "" {
		refreshToken = strings.TrimSpace(req.RT)
	}
	if refreshToken == "" {
		response.BadRequest(c, "refresh_token is required")
		return
	}

	proxyURL := openAIOAuthProxyURL(c.Request.Context(), h.adminService, req.ProxyID)

	// 未指定 client_id 时，根据请求路径平台自动设置默认值，避免 repository 层盲猜
	clientID := strings.TrimSpace(req.ClientID)
	if clientID == "" {
		platform := oauthPlatformFromPath(c)
		clientID, _ = openai.OAuthClientConfigByPlatform(platform)
	}

	tokenInfo, err := h.openaiOAuthService.RefreshTokenForNewAccount(c.Request.Context(), refreshToken, proxyURL, clientID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, openAIOAuthTokenResponse(tokenInfo))
}

// OpenAICreateAccountFromRefreshTokenRequest creates an OpenAI OAuth account from a manually supplied refresh token.
type OpenAICreateAccountFromRefreshTokenRequest struct {
	RefreshToken            string         `json:"refresh_token"`
	RT                      string         `json:"rt"`
	ClientID                string         `json:"client_id"`
	ProxyID                 *int64         `json:"proxy_id"`
	Name                    string         `json:"name"`
	Notes                   *string        `json:"notes"`
	Credentials             map[string]any `json:"credentials"`
	Extra                   map[string]any `json:"extra"`
	Concurrency             int            `json:"concurrency"`
	LoadFactor              *int           `json:"load_factor"`
	Priority                int            `json:"priority"`
	RateMultiplier          *float64       `json:"rate_multiplier"`
	GroupIDs                []int64        `json:"group_ids"`
	ExpiresAt               *int64         `json:"expires_at"`
	AutoPauseOnExpired      *bool          `json:"auto_pause_on_expired"`
	ConfirmMixedChannelRisk *bool          `json:"confirm_mixed_channel_risk"`
}

// CreateAccountFromRefreshToken creates a new OpenAI OAuth account from a refresh token.
// POST /api/v1/admin/openai/create-from-refresh-token
func (h *OpenAIOAuthHandler) CreateAccountFromRefreshToken(c *gin.Context) {
	var req OpenAICreateAccountFromRefreshTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	refreshToken := strings.TrimSpace(req.RefreshToken)
	if refreshToken == "" {
		refreshToken = strings.TrimSpace(req.RT)
	}
	if refreshToken == "" {
		response.BadRequest(c, "refresh_token is required")
		return
	}

	clientID := strings.TrimSpace(req.ClientID)
	if clientID == "" {
		clientID, _ = openai.OAuthClientConfigByPlatform(oauthPlatformFromPath(c))
	}
	tokenInfo, err := h.openaiOAuthService.RefreshTokenForNewAccount(c.Request.Context(), refreshToken, openAIOAuthProxyURL(c.Request.Context(), h.adminService, req.ProxyID), clientID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	h.createOpenAIOAuthAccount(c, tokenInfo, openAIOAuthCreateAccountParams{
		Name:                    req.Name,
		Notes:                   req.Notes,
		Credentials:             req.Credentials,
		Extra:                   req.Extra,
		ProxyID:                 req.ProxyID,
		Concurrency:             req.Concurrency,
		LoadFactor:              req.LoadFactor,
		Priority:                req.Priority,
		RateMultiplier:          req.RateMultiplier,
		GroupIDs:                req.GroupIDs,
		ExpiresAt:               req.ExpiresAt,
		AutoPauseOnExpired:      req.AutoPauseOnExpired,
		ConfirmMixedChannelRisk: req.ConfirmMixedChannelRisk,
	})
}

// RefreshAccountToken refreshes token for a specific OpenAI account
// POST /api/v1/admin/openai/accounts/:id/refresh
func (h *OpenAIOAuthHandler) RefreshAccountToken(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}

	// Get account
	account, err := h.adminService.GetAccount(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	platform := oauthPlatformFromPath(c)
	if account.Platform != platform {
		response.BadRequest(c, "Account platform does not match OAuth endpoint")
		return
	}

	// Only refresh OAuth-like accounts; setup-token reuses access-token metadata when no RT exists.
	if !account.IsOAuth() {
		response.BadRequest(c, "Cannot refresh non-OAuth account credentials")
		return
	}

	// Use OpenAI OAuth service to refresh token
	tokenInfo, err := h.openaiOAuthService.RefreshAccountToken(c.Request.Context(), account)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	// Build new credentials from token info
	newCredentials := h.openaiOAuthService.BuildAccountCredentials(tokenInfo)

	// Preserve non-token settings from existing credentials
	for k, v := range account.Credentials {
		if _, exists := newCredentials[k]; !exists {
			newCredentials[k] = v
		}
	}

	updatedAccount, err := h.adminService.UpdateAccount(c.Request.Context(), accountID, &service.UpdateAccountInput{
		Credentials: newCredentials,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	response.Success(c, dto.AccountFromService(updatedAccount))
}

// CreateAccountFromOAuth creates a new OpenAI OAuth account from token info
// POST /api/v1/admin/openai/create-from-oauth
func (h *OpenAIOAuthHandler) CreateAccountFromOAuth(c *gin.Context) {
	var req struct {
		SessionID               string         `json:"session_id" binding:"required"`
		Code                    string         `json:"code" binding:"required"`
		State                   string         `json:"state" binding:"required"`
		RedirectURI             string         `json:"redirect_uri"`
		ProxyID                 *int64         `json:"proxy_id"`
		Name                    string         `json:"name"`
		Notes                   *string        `json:"notes"`
		Credentials             map[string]any `json:"credentials"`
		Extra                   map[string]any `json:"extra"`
		Concurrency             int            `json:"concurrency"`
		LoadFactor              *int           `json:"load_factor"`
		Priority                int            `json:"priority"`
		RateMultiplier          *float64       `json:"rate_multiplier"`
		GroupIDs                []int64        `json:"group_ids"`
		ExpiresAt               *int64         `json:"expires_at"`
		AutoPauseOnExpired      *bool          `json:"auto_pause_on_expired"`
		ConfirmMixedChannelRisk *bool          `json:"confirm_mixed_channel_risk"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	// Exchange code for tokens
	tokenInfo, err := h.openaiOAuthService.ExchangeCode(c.Request.Context(), &service.OpenAIExchangeCodeInput{
		SessionID:   req.SessionID,
		Code:        req.Code,
		State:       req.State,
		RedirectURI: req.RedirectURI,
		ProxyID:     req.ProxyID,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	h.createOpenAIOAuthAccount(c, tokenInfo, openAIOAuthCreateAccountParams{
		Name:                    req.Name,
		Notes:                   req.Notes,
		Credentials:             req.Credentials,
		Extra:                   req.Extra,
		ProxyID:                 req.ProxyID,
		Concurrency:             req.Concurrency,
		LoadFactor:              req.LoadFactor,
		Priority:                req.Priority,
		RateMultiplier:          req.RateMultiplier,
		GroupIDs:                req.GroupIDs,
		ExpiresAt:               req.ExpiresAt,
		AutoPauseOnExpired:      req.AutoPauseOnExpired,
		ConfirmMixedChannelRisk: req.ConfirmMixedChannelRisk,
	})
}

// QueryQuota queries the rate-limit / quota usage for an OpenAI account.
// GET /api/v1/admin/openai/accounts/:id/quota
func (h *OpenAIOAuthHandler) QueryQuota(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	if h.quotaService == nil {
		response.BadRequest(c, "openai quota service is not enabled")
		return
	}
	usage, err := h.quotaService.QueryUsage(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, usage)
}

// ResetQuota consumes one rate-limit reset credit for an OpenAI account.
// POST /api/v1/admin/openai/accounts/:id/reset-quota
func (h *OpenAIOAuthHandler) ResetQuota(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	if h.quotaService == nil {
		response.BadRequest(c, "openai quota service is not enabled")
		return
	}
	result, err := h.quotaService.ResetCredit(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if result == nil {
		response.Error(c, http.StatusInternalServerError, "openai quota reset returned an empty result")
		return
	}

	resetResponse := openAIQuotaResetResponse{OpenAIQuotaResetResult: *result}
	postCtx, cancel := openAIQuotaResetPostProcessContext(c.Request.Context())
	defer cancel()
	if h.rateLimitService == nil {
		resetResponse.WarningCode = openAIQuotaResetWarningAccountRecoveryFailed
		response.Success(c, resetResponse)
		return
	}
	if _, recoverErr := h.rateLimitService.RecoverAccountState(postCtx, accountID, service.AccountRecoveryOptions{InvalidateToken: true}); recoverErr != nil {
		slog.Warn("openai_quota_reset_account_recovery_failed", "account_id", accountID, "error", recoverErr)
		resetResponse.WarningCode = openAIQuotaResetWarningAccountRecoveryFailed
		response.Success(c, resetResponse)
		return
	}
	resetResponse.AccountStateRecovered = true

	usage, usageErr := h.quotaService.QueryUsage(postCtx, accountID)
	if usageErr != nil || usage == nil {
		slog.Warn("openai_quota_reset_cache_refresh_failed", "account_id", accountID, "error", usageErr)
		resetResponse.WarningCode = openAIQuotaResetWarningCacheRefreshFailed
	} else {
		resetResponse.Quota = usage
	}
	account, accountErr := h.adminService.GetAccount(postCtx, accountID)
	if accountErr != nil {
		slog.Warn("openai_quota_reset_account_refresh_failed", "account_id", accountID, "error", accountErr)
		if resetResponse.WarningCode == "" {
			resetResponse.WarningCode = openAIQuotaResetWarningAccountRefreshFailed
		}
	} else {
		resetResponse.Account = dto.AccountFromService(account)
	}
	response.Success(c, resetResponse)
}
