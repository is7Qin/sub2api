package service

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

// OpenAIOAuthService handles OpenAI OAuth authentication flows
type OpenAIOAuthService struct {
	sessionStore         openAIOAuthSessionStore
	proxyRepo            ProxyRepository
	accountRepo          OpenAICodexFingerprintAccountRepository
	oauthClient          OpenAIOAuthClient
	userAgentProvider    OpenAICodexUserAgentProvider
	privacyClientFactory PrivacyClientFactory // 用于调用 chatgpt.com/backend-api（ImpersonateChrome）
}

// NewOpenAIOAuthService creates a new OpenAI OAuth service with in-memory pending session storage.
func NewOpenAIOAuthService(proxyRepo ProxyRepository, oauthClient OpenAIOAuthClient) *OpenAIOAuthService {
	return newOpenAIOAuthServiceWithSessionStore(proxyRepo, oauthClient, newOpenAIOAuthMemorySessionStore())
}

func newOpenAIOAuthServiceWithSessionStore(proxyRepo ProxyRepository, oauthClient OpenAIOAuthClient, sessionStore openAIOAuthSessionStore) *OpenAIOAuthService {
	if sessionStore == nil {
		sessionStore = newOpenAIOAuthMemorySessionStore()
	}
	return &OpenAIOAuthService{
		sessionStore: sessionStore,
		proxyRepo:    proxyRepo,
		oauthClient:  oauthClient,
	}
}

func (s *OpenAIOAuthService) SetCodexFingerprintDependencies(accountRepo OpenAICodexFingerprintAccountRepository, userAgentProvider OpenAICodexUserAgentProvider) {
	if s == nil {
		return
	}
	s.accountRepo = accountRepo
	s.userAgentProvider = userAgentProvider
}

// SetPrivacyClientFactory 注入 ImpersonateChrome 客户端工厂，
// 用于调用 chatgpt.com/backend-api 获取账号信息（plan_type 等）。
func (s *OpenAIOAuthService) SetPrivacyClientFactory(factory PrivacyClientFactory) {
	s.privacyClientFactory = factory
}

func (s *OpenAIOAuthService) openAIOAuthPendingFingerprint(ctx context.Context, account *Account, now time.Time) (OpenAICodexFingerprint, error) {
	defaultProfile := s.defaultOpenAICodexUAProfile(ctx)
	if account != nil {
		return ensureOpenAICodexFingerprintWithProfile(ctx, account, s.accountRepo, defaultProfile, now)
	}
	fingerprint, _ := NormalizeOpenAICodexFingerprint(nil, defaultProfile, now)
	return fingerprint, nil
}

func (s *OpenAIOAuthService) defaultOpenAICodexUAProfile(ctx context.Context) OpenAICodexUAProfile {
	ua := DefaultOpenAICodexUserAgent
	if s != nil && s.userAgentProvider != nil {
		if configured := strings.TrimSpace(s.userAgentProvider.GetOpenAICodexUserAgent(ctx)); configured != "" {
			ua = configured
		}
	}
	return ParseOpenAICodexUAProfile(ua)
}

// OpenAIAuthURLResult contains the authorization URL and session info
type OpenAIAuthURLResult struct {
	AuthURL   string `json:"auth_url"`
	SessionID string `json:"session_id"`
}

type OpenAIAuthURLInput struct {
	ProxyID     *int64
	RedirectURI string
	Platform    string
	Account     *Account
}

// GenerateAuthURL generates an OpenAI OAuth authorization URL
func (s *OpenAIOAuthService) GenerateAuthURL(ctx context.Context, proxyID *int64, redirectURI, platform string) (*OpenAIAuthURLResult, error) {
	return s.GenerateAuthURLWithInput(ctx, OpenAIAuthURLInput{ProxyID: proxyID, RedirectURI: redirectURI, Platform: platform})
}

func (s *OpenAIOAuthService) GenerateAuthURLWithInput(ctx context.Context, input OpenAIAuthURLInput) (*OpenAIAuthURLResult, error) {
	// Generate PKCE values
	state, err := openai.GenerateState()
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "OPENAI_OAUTH_STATE_FAILED", "failed to generate state: %v", err)
	}

	codeVerifier, err := openai.GenerateCodeVerifier()
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "OPENAI_OAUTH_VERIFIER_FAILED", "failed to generate code verifier: %v", err)
	}

	codeChallenge := openai.GenerateCodeChallenge(codeVerifier)

	// Generate session ID
	sessionID, err := openai.GenerateSessionID()
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "OPENAI_OAUTH_SESSION_FAILED", "failed to generate session ID: %v", err)
	}

	// Get proxy URL if specified
	var proxyURL string
	if input.ProxyID != nil {
		proxy, err := s.proxyRepo.GetByID(ctx, *input.ProxyID)
		if err != nil {
			return nil, infraerrors.Newf(http.StatusBadRequest, "OPENAI_OAUTH_PROXY_NOT_FOUND", "proxy not found: %v", err)
		}
		if proxy != nil {
			proxyURL = proxy.URL()
		}
	}

	// Use default redirect URI if not specified
	redirectURI := input.RedirectURI
	if redirectURI == "" {
		redirectURI = openai.DefaultRedirectURI
	}
	normalizedPlatform := normalizeOpenAIOAuthPlatform(input.Platform)
	clientID, _ := openai.OAuthClientConfigByPlatform(normalizedPlatform)
	createdAt := time.Now()
	fingerprint, err := s.openAIOAuthPendingFingerprint(ctx, input.Account, createdAt)
	if err != nil {
		return nil, err
	}

	// Store session
	session := &openAIOAuthPendingSession{
		OAuthSession: openai.OAuthSession{
			State:        state,
			CodeVerifier: codeVerifier,
			ClientID:     clientID,
			RedirectURI:  redirectURI,
			ProxyURL:     proxyURL,
			CreatedAt:    createdAt,
		},
		CodexFingerprint: fingerprint,
	}
	if err := s.sessionStore.Set(ctx, sessionID, session); err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "OPENAI_OAUTH_SESSION_STORE_FAILED", "failed to store oauth session: %v", err)
	}

	// Build authorization URL with the same profile originator used for token exchange.
	authURL := openai.BuildAuthorizationURLForPlatformWithOriginator(state, codeChallenge, redirectURI, normalizedPlatform, fingerprint.UAProfile.Originator)

	return &OpenAIAuthURLResult{
		AuthURL:   authURL,
		SessionID: sessionID,
	}, nil
}

// OpenAIExchangeCodeInput represents the input for code exchange
type OpenAIExchangeCodeInput struct {
	SessionID   string
	Code        string
	State       string
	RedirectURI string
	ProxyID     *int64
}

// OpenAITokenInfo represents the token information for OpenAI
type OpenAITokenInfo struct {
	AccessToken           string                  `json:"access_token"`
	RefreshToken          string                  `json:"refresh_token"`
	IDToken               string                  `json:"id_token,omitempty"`
	ExpiresIn             int64                   `json:"expires_in"`
	ExpiresAt             int64                   `json:"expires_at"`
	ClientID              string                  `json:"client_id,omitempty"`
	Email                 string                  `json:"email,omitempty"`
	ChatGPTAccountID      string                  `json:"chatgpt_account_id,omitempty"`
	ChatGPTUserID         string                  `json:"chatgpt_user_id,omitempty"`
	OrganizationID        string                  `json:"organization_id,omitempty"`
	PlanType              string                  `json:"plan_type,omitempty"`
	SubscriptionExpiresAt string                  `json:"subscription_expires_at,omitempty"`
	PrivacyMode           string                  `json:"privacy_mode,omitempty"`
	CodexFingerprint      *OpenAICodexFingerprint `json:"-"`
}

// ExchangeCode exchanges authorization code for tokens
func (s *OpenAIOAuthService) ExchangeCode(ctx context.Context, input *OpenAIExchangeCodeInput) (*OpenAITokenInfo, error) {
	// Get session
	session, ok := s.sessionStore.Get(ctx, input.SessionID)
	if !ok {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_OAUTH_SESSION_NOT_FOUND", "session not found or expired")
	}
	if input.State == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_OAUTH_STATE_REQUIRED", "oauth state is required")
	}
	if subtle.ConstantTimeCompare([]byte(input.State), []byte(session.State)) != 1 {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_OAUTH_INVALID_STATE", "invalid oauth state")
	}

	// Get proxy URL: prefer input.ProxyID, fallback to session.ProxyURL
	proxyURL := session.ProxyURL
	if input.ProxyID != nil {
		proxy, err := s.proxyRepo.GetByID(ctx, *input.ProxyID)
		if err != nil {
			return nil, infraerrors.Newf(http.StatusBadRequest, "OPENAI_OAUTH_PROXY_NOT_FOUND", "proxy not found: %v", err)
		}
		if proxy != nil {
			proxyURL = proxy.URL()
		}
	}

	// Use redirect URI from session or input
	redirectURI := session.RedirectURI
	if input.RedirectURI != "" {
		redirectURI = input.RedirectURI
	}
	clientID := strings.TrimSpace(session.ClientID)
	if clientID == "" {
		clientID = openai.ClientID
	}
	fingerprint, _ := NormalizeOpenAICodexFingerprint(session.CodexFingerprint, s.defaultOpenAICodexUAProfile(ctx), time.Now())

	// Exchange code with the pending account fingerprint captured when the auth URL was generated.
	tokenResp, err := s.oauthClient.ExchangeCode(ctx, input.Code, session.CodeVerifier, redirectURI, proxyURL, OpenAIOAuthTokenOptions{
		ClientID:  clientID,
		UAProfile: fingerprint.UAProfile,
	})
	if err != nil {
		return nil, err
	}

	// Parse ID token to get user info
	var userInfo *openai.UserInfo
	if tokenResp.IDToken != "" {
		claims, parseErr := openai.ParseIDToken(tokenResp.IDToken)
		if parseErr != nil {
			slog.Warn("openai_oauth_id_token_parse_failed", "error", parseErr)
		} else {
			userInfo = claims.GetUserInfo()
		}
	}

	// Delete session after successful exchange
	s.sessionStore.Delete(ctx, input.SessionID)

	tokenInfo := &OpenAITokenInfo{
		AccessToken:      tokenResp.AccessToken,
		RefreshToken:     tokenResp.RefreshToken,
		IDToken:          tokenResp.IDToken,
		ExpiresIn:        int64(tokenResp.ExpiresIn),
		ExpiresAt:        time.Now().Unix() + int64(tokenResp.ExpiresIn),
		ClientID:         clientID,
		CodexFingerprint: &fingerprint,
	}

	if userInfo != nil {
		tokenInfo.Email = userInfo.Email
		tokenInfo.ChatGPTAccountID = userInfo.ChatGPTAccountID
		tokenInfo.ChatGPTUserID = userInfo.ChatGPTUserID
		tokenInfo.OrganizationID = userInfo.OrganizationID
		tokenInfo.PlanType = userInfo.PlanType
	}

	s.enrichTokenInfo(ctx, tokenInfo, proxyURL)

	return tokenInfo, nil
}

// RefreshToken refreshes an OpenAI OAuth token.
func (s *OpenAIOAuthService) RefreshToken(ctx context.Context, refreshToken string, proxyURL string) (*OpenAITokenInfo, error) {
	return s.RefreshTokenWithOptions(ctx, refreshToken, proxyURL, OpenAIOAuthTokenOptions{})
}

// RefreshTokenWithClientID refreshes an OpenAI OAuth token with optional client_id.
func (s *OpenAIOAuthService) RefreshTokenWithClientID(ctx context.Context, refreshToken string, proxyURL string, clientID string) (*OpenAITokenInfo, error) {
	return s.RefreshTokenWithOptions(ctx, refreshToken, proxyURL, OpenAIOAuthTokenOptions{ClientID: clientID})
}

func (s *OpenAIOAuthService) RefreshTokenWithOptions(ctx context.Context, refreshToken string, proxyURL string, opts OpenAIOAuthTokenOptions) (*OpenAITokenInfo, error) {
	tokenResp, err := s.oauthClient.RefreshTokenWithOptions(ctx, refreshToken, proxyURL, opts)
	if err != nil {
		return nil, err
	}

	// Parse ID token to get user info
	var userInfo *openai.UserInfo
	if tokenResp.IDToken != "" {
		claims, parseErr := openai.ParseIDToken(tokenResp.IDToken)
		if parseErr != nil {
			slog.Warn("openai_oauth_id_token_parse_failed", "error", parseErr)
		} else {
			userInfo = claims.GetUserInfo()
		}
	}

	tokenInfo := &OpenAITokenInfo{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		IDToken:      tokenResp.IDToken,
		ExpiresIn:    int64(tokenResp.ExpiresIn),
		ExpiresAt:    time.Now().Unix() + int64(tokenResp.ExpiresIn),
	}
	if trimmed := strings.TrimSpace(opts.ClientID); trimmed != "" {
		tokenInfo.ClientID = trimmed
	}

	if userInfo != nil {
		tokenInfo.Email = userInfo.Email
		tokenInfo.ChatGPTAccountID = userInfo.ChatGPTAccountID
		tokenInfo.ChatGPTUserID = userInfo.ChatGPTUserID
		tokenInfo.OrganizationID = userInfo.OrganizationID
		tokenInfo.PlanType = userInfo.PlanType
	}

	s.enrichTokenInfo(ctx, tokenInfo, proxyURL)

	return tokenInfo, nil
}

// RefreshTokenForNewAccount validates a manually supplied RT with a fresh server-owned
// Codex fingerprint, then returns the same fingerprint for account creation.
func (s *OpenAIOAuthService) RefreshTokenForNewAccount(ctx context.Context, refreshToken string, proxyURL string, clientID string) (*OpenAITokenInfo, error) {
	fingerprint, _ := NormalizeOpenAICodexFingerprint(nil, s.defaultOpenAICodexUAProfile(ctx), time.Now())
	tokenInfo, err := s.RefreshTokenWithOptions(ctx, refreshToken, proxyURL, OpenAIOAuthTokenOptions{
		ClientID:  clientID,
		UAProfile: fingerprint.UAProfile,
	})
	if err != nil {
		return nil, err
	}
	tokenInfo.CodexFingerprint = &fingerprint
	return tokenInfo, nil
}

// enrichTokenInfo 通过 ChatGPT backend-api 补全 tokenInfo 并设置隐私（best-effort）。
// 从 accounts/check 获取最新 plan_type、subscription_expires_at、email，
// 然后尝试关闭训练数据共享。适用于所有获取/刷新 token 的路径。
func (s *OpenAIOAuthService) enrichTokenInfo(ctx context.Context, tokenInfo *OpenAITokenInfo, proxyURL string) {
	if tokenInfo.AccessToken == "" || s.privacyClientFactory == nil {
		return
	}

	// 从 access_token JWT 中提取 orgID（poid），用于匹配正确的账号
	orgID := tokenInfo.OrganizationID
	if orgID == "" {
		if atClaims, err := openai.DecodeIDToken(tokenInfo.AccessToken); err == nil && atClaims.OpenAIAuth != nil {
			orgID = atClaims.OpenAIAuth.POID
		}
	}
	if info := fetchChatGPTAccountInfo(ctx, s.privacyClientFactory, tokenInfo.AccessToken, proxyURL, orgID); info != nil {
		if info.PlanType != "" {
			tokenInfo.PlanType = info.PlanType
		}
		if info.SubscriptionExpiresAt != "" {
			tokenInfo.SubscriptionExpiresAt = info.SubscriptionExpiresAt
		}
		if tokenInfo.Email == "" && info.Email != "" {
			tokenInfo.Email = info.Email
		}
	}
	if strings.TrimSpace(tokenInfo.SubscriptionExpiresAt) == "" {
		if expiresAt := fetchChatGPTSubscriptionExpiresAt(ctx, s.privacyClientFactory, tokenInfo.AccessToken, proxyURL, resolveChatGPTSubscriptionAccountID(tokenInfo, orgID)); expiresAt != "" {
			tokenInfo.SubscriptionExpiresAt = expiresAt
		}
	}

	// 尝试设置隐私（关闭训练数据共享），best-effort
	tokenInfo.PrivacyMode = disableOpenAITraining(ctx, s.privacyClientFactory, tokenInfo.AccessToken, proxyURL)
}

func resolveChatGPTSubscriptionAccountID(tokenInfo *OpenAITokenInfo, orgID string) string {
	for _, candidate := range []string{
		tokenInfo.ChatGPTAccountID,
		tokenInfo.OrganizationID,
		orgID,
	} {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// RefreshAccountToken refreshes token for an OpenAI OAuth account.
func (s *OpenAIOAuthService) RefreshAccountToken(ctx context.Context, account *Account) (*OpenAITokenInfo, error) {
	if account == nil || account.Platform != PlatformOpenAI {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_OAUTH_INVALID_ACCOUNT", "account is not an OpenAI account")
	}
	if !account.IsOpenAIOAuthLike() {
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_OAUTH_INVALID_ACCOUNT_TYPE", "account is not an OpenAI OAuth/setup-token account")
	}

	fingerprint, err := ensureOpenAICodexFingerprintWithProfile(ctx, account, s.accountRepo, s.defaultOpenAICodexUAProfile(ctx), time.Now())
	if err != nil {
		return nil, err
	}

	var proxyURL string
	if account.ProxyID != nil {
		proxy, err := s.proxyRepo.GetByID(ctx, *account.ProxyID)
		if err == nil && proxy != nil {
			proxyURL = proxy.URL()
		}
	}

	refreshToken := account.GetCredential("refresh_token")
	if refreshToken == "" {
		accessToken := account.GetCredential("access_token")
		if accessToken != "" {
			tokenInfo := &OpenAITokenInfo{
				AccessToken:           accessToken,
				RefreshToken:          "",
				IDToken:               account.GetCredential("id_token"),
				ClientID:              account.GetCredential("client_id"),
				Email:                 account.GetCredential("email"),
				ChatGPTAccountID:      account.GetCredential("chatgpt_account_id"),
				ChatGPTUserID:         account.GetCredential("chatgpt_user_id"),
				OrganizationID:        account.GetCredential("organization_id"),
				PlanType:              account.GetCredential("plan_type"),
				SubscriptionExpiresAt: account.GetCredential("subscription_expires_at"),
			}
			if expiresAt := account.GetCredentialAsTime("expires_at"); expiresAt != nil {
				tokenInfo.ExpiresAt = expiresAt.Unix()
				tokenInfo.ExpiresIn = int64(time.Until(*expiresAt).Seconds())
			}
			tokenInfo.CodexFingerprint = &fingerprint
			s.enrichTokenInfo(ctx, tokenInfo, proxyURL)
			return tokenInfo, nil
		}
		return nil, infraerrors.New(http.StatusBadRequest, "OPENAI_OAUTH_NO_REFRESH_TOKEN", "no refresh token available")
	}

	clientID := account.GetCredential("client_id")
	tokenInfo, err := s.RefreshTokenWithOptions(ctx, refreshToken, proxyURL, OpenAIOAuthTokenOptions{
		ClientID:  clientID,
		UAProfile: fingerprint.UAProfile,
	})
	if err != nil {
		return nil, err
	}
	tokenInfo.CodexFingerprint = &fingerprint
	return tokenInfo, nil
}

// BuildAccountCredentials builds credentials map from token info
func (s *OpenAIOAuthService) BuildAccountCredentials(tokenInfo *OpenAITokenInfo) map[string]any {
	expiresAt := time.Unix(tokenInfo.ExpiresAt, 0).Format(time.RFC3339)

	creds := map[string]any{
		"access_token": tokenInfo.AccessToken,
		"expires_at":   expiresAt,
	}
	// 仅在刷新响应返回了新的 refresh_token 时才更新，防止用空值覆盖已有令牌
	if strings.TrimSpace(tokenInfo.RefreshToken) != "" {
		creds["refresh_token"] = tokenInfo.RefreshToken
	}

	if tokenInfo.IDToken != "" {
		creds["id_token"] = tokenInfo.IDToken
	}
	if tokenInfo.Email != "" {
		creds["email"] = tokenInfo.Email
	}
	if tokenInfo.ChatGPTAccountID != "" {
		creds["chatgpt_account_id"] = tokenInfo.ChatGPTAccountID
	}
	if tokenInfo.ChatGPTUserID != "" {
		creds["chatgpt_user_id"] = tokenInfo.ChatGPTUserID
	}
	if tokenInfo.OrganizationID != "" {
		creds["organization_id"] = tokenInfo.OrganizationID
	}
	if tokenInfo.PlanType != "" {
		creds["plan_type"] = tokenInfo.PlanType
	}
	if tokenInfo.SubscriptionExpiresAt != "" {
		creds["subscription_expires_at"] = tokenInfo.SubscriptionExpiresAt
	}
	if strings.TrimSpace(tokenInfo.ClientID) != "" {
		creds["client_id"] = strings.TrimSpace(tokenInfo.ClientID)
	}

	return creds
}

// CleanupSessions removes expired pending sessions from the request-serving store.
func (s *OpenAIOAuthService) CleanupSessions(ctx context.Context) error {
	if s == nil || s.sessionStore == nil {
		return nil
	}
	return s.sessionStore.CleanupExpiredSessions(ctx)
}

// hasRedisSetFailureCleanup reports whether the request-serving store has marker cleanup work.
func (s *OpenAIOAuthService) hasRedisSetFailureCleanup() bool {
	if s == nil || s.sessionStore == nil {
		return false
	}
	_, ok := s.sessionStore.(openAIOAuthRedisSetFailureCleaner)
	return ok
}

// CleanupRedisSetFailures removes expired process-local Redis write-failure markers.
func (s *OpenAIOAuthService) CleanupRedisSetFailures(ctx context.Context) error {
	if s == nil || s.sessionStore == nil {
		return nil
	}
	cleaner, ok := s.sessionStore.(openAIOAuthRedisSetFailureCleaner)
	if !ok {
		return nil
	}
	return cleaner.CleanupExpiredRedisSetFailures(ctx)
}

func normalizeOpenAIOAuthPlatform(platform string) string {
	return openai.OAuthPlatformOpenAI
}
