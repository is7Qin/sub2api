package admin

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

const (
	codexImportClockSkewSeconds int64 = 120
	codexImportFormatHCPA             = "hcpa"
)

type CodexSessionImportRequest struct {
	Content                 string         `json:"content"`
	Contents                []string       `json:"contents"`
	Name                    string         `json:"name"`
	Notes                   *string        `json:"notes"`
	GroupIDs                []int64        `json:"group_ids"`
	ProxyID                 *int64         `json:"proxy_id"`
	Concurrency             *int           `json:"concurrency"`
	Priority                *int           `json:"priority"`
	RateMultiplier          *float64       `json:"rate_multiplier"`
	LoadFactor              *int           `json:"load_factor"`
	ExpiresAt               *int64         `json:"expires_at"`
	AutoPauseOnExpired      *bool          `json:"auto_pause_on_expired"`
	CredentialExtras        map[string]any `json:"credential_extras"`
	Extra                   map[string]any `json:"extra"`
	UpdateExisting          *bool          `json:"update_existing"`
	SkipDefaultGroupBind    *bool          `json:"skip_default_group_bind"`
	ConfirmMixedChannelRisk *bool          `json:"confirm_mixed_channel_risk"`
}

type CodexSessionImportResult struct {
	Total    int                         `json:"total"`
	Created  int                         `json:"created"`
	Updated  int                         `json:"updated"`
	Skipped  int                         `json:"skipped"`
	Failed   int                         `json:"failed"`
	Items    []CodexSessionImportItem    `json:"items,omitempty"`
	Warnings []CodexSessionImportMessage `json:"warnings,omitempty"`
	Errors   []CodexSessionImportMessage `json:"errors,omitempty"`
}

type CodexSessionImportItem struct {
	Index     int    `json:"index"`
	Name      string `json:"name,omitempty"`
	Action    string `json:"action"`
	AccountID int64  `json:"account_id,omitempty"`
	Message   string `json:"message,omitempty"`
}

type CodexSessionImportMessage struct {
	Index   int    `json:"index"`
	Name    string `json:"name,omitempty"`
	Message string `json:"message"`
}

type codexImportEntry struct {
	Index int
	Value any
}

type codexImportAccount struct {
	Name                string
	AccessToken         string
	PersonalAccessToken string
	RefreshToken        string
	IDToken             string
	Email               string
	AccountID           string
	UserID              string
	PlanType            string
	Organization        string
	AgentRuntimeID      string
	AgentPrivateKey     string
	AgentTaskID         string
	AgentFedRAMP        bool
	IsAgentIdentity     bool
	Credentials         map[string]any
	Extra               map[string]any
	TokenExpiresAt      *time.Time
	AccountExpiresAt    *time.Time
	IdentityKeys        []string
	WarningTexts        []string
	Status              string
	ImportFormat        string
}

type codexJWTClaims struct {
	Sub        string                `json:"sub"`
	Email      string                `json:"email"`
	Exp        int64                 `json:"exp"`
	Iat        int64                 `json:"iat"`
	OpenAIAuth *codexJWTOpenAIClaims `json:"https://api.openai.com/auth,omitempty"`
}

type codexJWTOpenAIClaims struct {
	ChatGPTAccountID string                     `json:"chatgpt_account_id"`
	ChatGPTUserID    string                     `json:"chatgpt_user_id"`
	ChatGPTPlanType  string                     `json:"chatgpt_plan_type"`
	UserID           string                     `json:"user_id"`
	POID             string                     `json:"poid"`
	Organizations    []openai.OrganizationClaim `json:"organizations"`
}

type codexAccountIndex struct {
	accountsByKey map[string]service.Account
}

func (h *AccountHandler) ImportCodexSession(c *gin.Context) {
	var req CodexSessionImportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if req.Concurrency != nil && *req.Concurrency < 0 {
		response.BadRequest(c, "concurrency must be >= 0")
		return
	}
	if req.Priority != nil && *req.Priority < 0 {
		response.BadRequest(c, "priority must be >= 0")
		return
	}
	if req.RateMultiplier != nil && *req.RateMultiplier < 0 {
		response.BadRequest(c, "rate_multiplier must be >= 0")
		return
	}
	if req.LoadFactor != nil && *req.LoadFactor > 10000 {
		response.BadRequest(c, "load_factor must be <= 10000")
		return
	}

	entries, err := parseCodexSessionImportEntries(req)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if len(entries) == 0 {
		response.BadRequest(c, "请输入 accessToken 或 Codex session JSON")
		return
	}

	executeAdminIdempotentJSON(c, "admin.accounts.import_codex_session", req, service.DefaultWriteIdempotencyTTL(), func(ctx context.Context) (any, error) {
		return h.importCodexSessions(ctx, req, entries)
	})
}

func (h *AccountHandler) importCodexSessions(ctx context.Context, req CodexSessionImportRequest, entries []codexImportEntry) (CodexSessionImportResult, error) {
	result := CodexSessionImportResult{
		Total: len(entries),
		Items: make([]CodexSessionImportItem, 0, len(entries)),
	}

	existingAccounts, err := h.listAccountsFiltered(ctx, service.PlatformOpenAI, service.AccountTypeOAuth, "", "", 0, "", "", "created_at", "desc")
	if err != nil {
		return result, err
	}
	index := buildCodexAccountIndex(existingAccounts)

	updateExisting := true
	if req.UpdateExisting != nil {
		updateExisting = *req.UpdateExisting
	}
	concurrency := 3
	if req.Concurrency != nil {
		concurrency = *req.Concurrency
	}
	priority := 50
	if req.Priority != nil {
		priority = *req.Priority
	}
	credentialExtras := sanitizeCodexImportCredentialExtras(req.CredentialExtras)
	skipDefaultGroupBind := false
	if req.SkipDefaultGroupBind != nil {
		skipDefaultGroupBind = *req.SkipDefaultGroupBind
	}
	skipMixedChannelCheck := req.ConfirmMixedChannelRisk != nil && *req.ConfirmMixedChannelRisk

	seenIdentity := map[string]int{}
	for _, entry := range entries {
		item, err := normalizeCodexImportEntry(entry)
		if err != nil {
			result.Failed++
			result.Items = append(result.Items, CodexSessionImportItem{
				Index:   entry.Index,
				Action:  "failed",
				Message: err.Error(),
			})
			result.Errors = append(result.Errors, CodexSessionImportMessage{
				Index:   entry.Index,
				Message: err.Error(),
			})
			continue
		}
		accountName := buildCodexCreateAccountName(req.Name, item, entry.Index, len(entries))
		effectiveExpiresAt, credentialExpiresAt, autoPauseOnExpired, expiryWarnings, expiryErr := resolveCodexImportExpiry(req, item)
		if expiryErr != nil {
			result.Failed++
			result.Items = append(result.Items, CodexSessionImportItem{
				Index:   entry.Index,
				Name:    accountName,
				Action:  "failed",
				Message: expiryErr.Error(),
			})
			result.Errors = append(result.Errors, CodexSessionImportMessage{
				Index:   entry.Index,
				Name:    accountName,
				Message: expiryErr.Error(),
			})
			continue
		}
		item.WarningTexts = append(item.WarningTexts, expiryWarnings...)
		if credentialExpiresAt != nil {
			item.Credentials["expires_at"] = credentialExpiresAt.Format(time.RFC3339)
		}
		if err := h.hydrateCodexImportPersonalAccessToken(ctx, req.ProxyID, item); err != nil {
			result.Failed++
			result.Items = append(result.Items, CodexSessionImportItem{
				Index:   entry.Index,
				Name:    accountName,
				Action:  "failed",
				Message: err.Error(),
			})
			result.Errors = append(result.Errors, CodexSessionImportMessage{
				Index:   entry.Index,
				Name:    accountName,
				Message: err.Error(),
			})
			continue
		}
		accountName = buildCodexCreateAccountName(req.Name, item, entry.Index, len(entries))
		credentials := mergeCodexImportMap(item.Credentials, credentialExtras)
		extra := mergeCodexImportExtra(req.Extra, item.Extra)
		for _, warning := range item.WarningTexts {
			result.Warnings = append(result.Warnings, CodexSessionImportMessage{
				Index:   entry.Index,
				Name:    accountName,
				Message: warning,
			})
		}

		if duplicateIndex, ok := firstSeenCodexIdentity(seenIdentity, item.IdentityKeys); ok {
			message := fmt.Sprintf("与第 %d 条导入项重复，已跳过", duplicateIndex)
			result.Skipped++
			result.Items = append(result.Items, CodexSessionImportItem{
				Index:   entry.Index,
				Name:    accountName,
				Action:  "skipped",
				Message: message,
			})
			result.Warnings = append(result.Warnings, CodexSessionImportMessage{
				Index:   entry.Index,
				Name:    accountName,
				Message: message,
			})
			continue
		}
		markCodexIdentitySeen(seenIdentity, item.IdentityKeys, entry.Index)

		if existing := index.Find(item.IdentityKeys); existing != nil && updateExisting {
			preserveExistingRefresh := item.RefreshToken == "" &&
				codexCredentialString(existing.Credentials, "refresh_token") != ""
			if preserveExistingRefresh {
				result.Warnings = append(result.Warnings, CodexSessionImportMessage{
					Index:   entry.Index,
					Name:    accountName,
					Message: "已有账号包含 refresh_token，本次 accessToken-only 导入已保留自动续期凭据",
				})
				effectiveExpiresAt = nil
				autoPauseOnExpired = nil
			}
			mergedCredentials := mergeCodexImportCredentials(existing.Credentials, credentials, item)
			sensitiveDeletes := codexImportClearedSensitiveCredentials(existing.Credentials, mergedCredentials)
			mergedExtra := mergeCodexImportExtra(existing.Extra, extra)
			updateInput := &service.UpdateAccountInput{
				Credentials:        mergedCredentials,
				SensitiveDeletes:   sensitiveDeletes,
				Extra:              mergedExtra,
				Concurrency:        req.Concurrency,
				Priority:           req.Priority,
				RateMultiplier:     req.RateMultiplier,
				LoadFactor:         req.LoadFactor,
				Status:             item.Status,
				ExpiresAt:          effectiveExpiresAt,
				AutoPauseOnExpired: autoPauseOnExpired,
			}
			if req.ProxyID != nil {
				updateInput.ProxyID = req.ProxyID
			}
			if len(req.GroupIDs) > 0 {
				groupIDs := append([]int64(nil), req.GroupIDs...)
				updateInput.GroupIDs = &groupIDs
				updateInput.SkipMixedChannelCheck = skipMixedChannelCheck
			}
			updated, updateErr := h.adminService.UpdateAccount(ctx, existing.ID, updateInput)
			if updateErr != nil {
				result.Failed++
				result.Items = append(result.Items, CodexSessionImportItem{
					Index:   entry.Index,
					Name:    accountName,
					Action:  "failed",
					Message: updateErr.Error(),
				})
				result.Errors = append(result.Errors, CodexSessionImportMessage{
					Index:   entry.Index,
					Name:    accountName,
					Message: updateErr.Error(),
				})
				continue
			}
			if h.tokenCacheInvalidator != nil && updated != nil {
				_ = h.tokenCacheInvalidator.InvalidateToken(ctx, updated)
			}
			result.Updated++
			accountID := existing.ID
			if updated != nil {
				accountID = updated.ID
				index.Add(*updated)
			}
			result.Items = append(result.Items, CodexSessionImportItem{
				Index:     entry.Index,
				Name:      accountName,
				Action:    "updated",
				AccountID: accountID,
			})
			continue
		}

		account, createErr := h.adminService.CreateAccount(ctx, &service.CreateAccountInput{
			Name:                  accountName,
			Notes:                 req.Notes,
			Platform:              service.PlatformOpenAI,
			Type:                  service.AccountTypeOAuth,
			Credentials:           credentials,
			Extra:                 extra,
			ProxyID:               req.ProxyID,
			Concurrency:           concurrency,
			Priority:              priority,
			RateMultiplier:        req.RateMultiplier,
			LoadFactor:            req.LoadFactor,
			GroupIDs:              req.GroupIDs,
			Status:                item.Status,
			ExpiresAt:             effectiveExpiresAt,
			AutoPauseOnExpired:    autoPauseOnExpired,
			SkipDefaultGroupBind:  skipDefaultGroupBind,
			SkipMixedChannelCheck: skipMixedChannelCheck,
		})
		if createErr != nil {
			result.Failed++
			result.Items = append(result.Items, CodexSessionImportItem{
				Index:   entry.Index,
				Name:    accountName,
				Action:  "failed",
				Message: createErr.Error(),
			})
			result.Errors = append(result.Errors, CodexSessionImportMessage{
				Index:   entry.Index,
				Name:    accountName,
				Message: createErr.Error(),
			})
			continue
		}
		if account != nil {
			index.Add(*account)
		}
		result.Created++
		accountID := int64(0)
		if account != nil {
			accountID = account.ID
		}
		result.Items = append(result.Items, CodexSessionImportItem{
			Index:     entry.Index,
			Name:      accountName,
			Action:    "created",
			AccountID: accountID,
		})
	}

	return result, nil
}

func parseCodexSessionImportEntries(req CodexSessionImportRequest) ([]codexImportEntry, error) {
	contents := make([]string, 0, 1+len(req.Contents))
	if strings.TrimSpace(req.Content) != "" {
		contents = append(contents, req.Content)
	}
	for _, content := range req.Contents {
		if strings.TrimSpace(content) != "" {
			contents = append(contents, content)
		}
	}

	var entries []codexImportEntry
	for _, content := range contents {
		values, err := parseCodexSessionImportContent(content)
		if err != nil {
			return nil, err
		}
		for _, value := range values {
			entries = append(entries, codexImportEntry{
				Index: len(entries) + 1,
				Value: value,
			})
		}
	}
	return entries, nil
}

func parseCodexSessionImportContent(content string) ([]any, error) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return nil, nil
	}

	if looksLikeJSON(trimmed) {
		values, err := decodeCodexJSONStream(trimmed)
		if err != nil {
			if strings.Contains(trimmed, "\n") {
				if lineValues, lineErr := parseCodexSessionImportLines(trimmed); lineErr == nil {
					return lineValues, nil
				}
			}
			return nil, fmt.Errorf("JSON 解析失败: %w", err)
		}
		return flattenCodexImportValues(values), nil
	}

	return parseCodexSessionImportLines(trimmed)
}

func parseCodexSessionImportLines(content string) ([]any, error) {
	values := make([]any, 0)
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if looksLikeJSON(line) {
			lineValues, err := decodeCodexJSONStream(line)
			if err != nil {
				return nil, fmt.Errorf("第 %d 行 JSON 解析失败: %w", len(values)+1, err)
			}
			values = append(values, flattenCodexImportValues(lineValues)...)
			continue
		}
		values = append(values, line)
	}
	return values, nil
}

func decodeCodexJSONStream(content string) ([]any, error) {
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.UseNumber()
	values := make([]any, 0, 1)
	for {
		var value any
		err := decoder.Decode(&value)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	if len(values) == 0 {
		return nil, errors.New("空 JSON 内容")
	}
	return values, nil
}

func flattenCodexImportValues(values []any) []any {
	out := make([]any, 0, len(values))
	var appendValue func(any)
	appendValue = func(value any) {
		if arr, ok := value.([]any); ok {
			for _, item := range arr {
				appendValue(item)
			}
			return
		}
		out = append(out, value)
	}
	for _, value := range values {
		appendValue(value)
	}
	return out
}

func normalizeCodexImportEntry(entry codexImportEntry) (*codexImportAccount, error) {
	now := time.Now().UTC()
	item := &codexImportAccount{
		Credentials: map[string]any{},
		Extra:       map[string]any{},
	}

	switch raw := entry.Value.(type) {
	case string:
		trimmed := strings.TrimSpace(raw)
		if service.ValidateOpenAIPersonalAccessToken(trimmed) == nil {
			item.PersonalAccessToken = trimmed
		} else {
			item.AccessToken = trimmed
		}
	case map[string]any:
		if agentIdentity, ok := firstCodexMap(raw, []string{"agent_identity"}, []string{"agentIdentity"}); ok || strings.EqualFold(firstCodexString(raw, []string{"auth_mode"}, []string{"authMode"}), service.OpenAIAuthModeAgentIdentity) {
			if !ok {
				agentIdentity = raw
			}
			item.IsAgentIdentity = true
			item.AgentRuntimeID = firstCodexString(agentIdentity, []string{"agent_runtime_id"}, []string{"agentRuntimeId"})
			item.AgentPrivateKey = firstCodexString(agentIdentity, []string{"agent_private_key"}, []string{"agentPrivateKey"})
			item.AgentTaskID = firstCodexString(agentIdentity, []string{"task_id"}, []string{"taskId"})
			item.AccountID = firstCodexString(agentIdentity, []string{"account_id"}, []string{"accountId"})
			item.UserID = firstCodexString(agentIdentity, []string{"chatgpt_user_id"}, []string{"chatgptUserId"})
			item.Email = firstCodexString(agentIdentity, []string{"email"})
			item.PlanType = firstCodexString(agentIdentity, []string{"plan_type"}, []string{"planType"})
			item.AgentFedRAMP, _ = firstCodexBool(agentIdentity, []string{"chatgpt_account_is_fedramp"}, []string{"chatgptAccountIsFedramp"})
			if item.AgentRuntimeID == "" || item.AgentPrivateKey == "" || item.AccountID == "" || item.UserID == "" {
				return nil, errors.New("agent identity 缺少必要字段")
			}
			if err := service.ValidateOpenAIAgentIdentityPrivateKey(item.AgentPrivateKey); err != nil {
				return nil, errors.New("agent identity private key 格式无效")
			}
			item.Credentials["auth_mode"] = service.OpenAIAuthModeAgentIdentity
			item.Credentials["agent_runtime_id"] = item.AgentRuntimeID
			item.Credentials["agent_private_key"] = item.AgentPrivateKey
			item.Credentials["chatgpt_account_id"] = item.AccountID
			item.Credentials["chatgpt_user_id"] = item.UserID
			item.Credentials["chatgpt_account_is_fedramp"] = item.AgentFedRAMP
			setCodexCredentialIfNotEmpty(item.Credentials, "task_id", item.AgentTaskID)
			setCodexCredentialIfNotEmpty(item.Credentials, "email", item.Email)
			setCodexCredentialIfNotEmpty(item.Credentials, "plan_type", item.PlanType)
			if item.AgentTaskID == "" {
				item.WarningTexts = append(item.WarningTexts, "未包含 task_id，首次请求会使用现有 runtime 注册新 task")
			}
			item.IdentityKeys = buildCodexAgentIdentityKeys(item.AccountID, item.UserID)
			item.Name = buildCodexImportAccountName(item, entry.Index)
			return item, nil
		}
		if handled, err := normalizeHCPAImportAccount(item, raw, now); handled {
			if err != nil {
				return nil, err
			}
			break
		}
		item.PersonalAccessToken = firstCodexString(raw,
			[]string{"personal_access_token"},
			[]string{"personalAccessToken"},
		)
		item.AccessToken = firstCodexString(raw,
			[]string{"tokens", "access_token"},
			[]string{"tokens", "accessToken"},
			[]string{"access_token"},
			[]string{"accessToken"},
			[]string{"token"},
		)
		item.RefreshToken = firstCodexString(raw,
			[]string{"tokens", "refresh_token"},
			[]string{"tokens", "refreshToken"},
			[]string{"refresh_token"},
			[]string{"refreshToken"},
		)
		item.IDToken = firstCodexString(raw,
			[]string{"tokens", "id_token"},
			[]string{"tokens", "idToken"},
			[]string{"id_token"},
			[]string{"idToken"},
		)
		item.Email = firstCodexString(raw, []string{"email"}, []string{"user", "email"})
		item.AccountID = firstCodexString(raw,
			[]string{"chatgpt_account_id"},
			[]string{"chatgptAccountId"},
			[]string{"account_id"},
			[]string{"accountId"},
			[]string{"account", "id"},
			[]string{"account", "account_id"},
			[]string{"account", "chatgpt_account_id"},
		)
		item.UserID = firstCodexString(raw,
			[]string{"chatgpt_user_id"},
			[]string{"chatgptUserId"},
			[]string{"user_id"},
			[]string{"userId"},
			[]string{"user", "id"},
		)
		item.PlanType = firstCodexString(raw,
			[]string{"chatgpt_plan_type"},
			[]string{"chatgptPlanType"},
			[]string{"plan_type"},
			[]string{"planType"},
			[]string{"account", "plan_type"},
			[]string{"account", "planType"},
		)
		if fedramp, ok := firstCodexBool(raw,
			[]string{"chatgpt_account_is_fedramp"},
			[]string{"chatgptAccountIsFedramp"},
			[]string{"chatgptAccountIsFedRAMP"},
			[]string{"account", "chatgpt_account_is_fedramp"},
			[]string{"account", "isFedramp"},
		); ok {
			item.Credentials["chatgpt_account_is_fedramp"] = fedramp
		}
		item.Organization = firstCodexString(raw,
			[]string{"organization_id"},
			[]string{"organizationId"},
			[]string{"org_id"},
			[]string{"orgId"},
		)
		item.Name = firstCodexString(raw, []string{"name"}, []string{"user", "name"})
		authProvider := firstCodexString(raw, []string{"auth_provider"}, []string{"authProvider"})
		if authProvider != "" {
			item.Extra["auth_provider"] = authProvider
		}
		if sessionToken := firstCodexString(raw, []string{"session_token"}, []string{"sessionToken"}); sessionToken != "" {
			item.Extra["session_token_present"] = true
			item.WarningTexts = append(item.WarningTexts, "sessionToken 已忽略，不会作为 OAuth refresh_token 存储")
		}
		if sessionExpiresAt, ok := codexTimeAt(raw, []string{"expires"}); ok {
			item.Extra["session_expires_at"] = sessionExpiresAt.Format(time.RFC3339)
		}
		if tokenExpiresAt, ok := firstCodexTime(raw,
			[]string{"tokens", "expires_at"},
			[]string{"tokens", "expiresAt"},
			[]string{"expires_at"},
			[]string{"expiresAt"},
		); ok && item.PersonalAccessToken == "" {
			if tokenExpiresAt.Unix() <= now.Unix()-codexImportClockSkewSeconds {
				return nil, fmt.Errorf("access_token 已过期: %s", tokenExpiresAt.Format(time.RFC3339))
			}
			item.TokenExpiresAt = &tokenExpiresAt
			item.Credentials["expires_at"] = tokenExpiresAt.Format(time.RFC3339)
		}
		copyCodexExtraString(raw, item.Extra, "user_image", []string{"user", "image"})
		copyCodexExtraString(raw, item.Extra, "user_picture", []string{"user", "picture"})
		copyCodexExtraString(raw, item.Extra, "account_structure", []string{"account", "structure"})
		copyCodexExtraString(raw, item.Extra, "account_residency_region", []string{"account", "residencyRegion"})
		copyCodexExtraString(raw, item.Extra, "compute_residency", []string{"account", "computeResidency"})
	default:
		return nil, fmt.Errorf("第 %d 条格式不支持", entry.Index)
	}

	if item.PersonalAccessToken != "" {
		item.Credentials["personal_access_token"] = item.PersonalAccessToken
		// PAT is distinct from OAuth AT/RT. Only the HCPA format deliberately
		// carries both credential families; generic PAT imports keep OAuth tokens out
		// so stale access_token values cannot be mistaken for PAT metadata.
		if item.ImportFormat == codexImportFormatHCPA {
			setCodexCredentialIfNotEmpty(item.Credentials, "access_token", item.AccessToken)
			if item.RefreshToken != "" {
				item.Credentials["refresh_token"] = item.RefreshToken
				item.Credentials["client_id"] = openai.ClientID
			}
			setCodexCredentialIfNotEmpty(item.Credentials, "id_token", item.IDToken)
			if item.TokenExpiresAt != nil {
				item.Credentials["expires_at"] = item.TokenExpiresAt.Format(time.RFC3339)
			}
		}
		item.WarningTexts = append(item.WarningTexts, "personal_access_token 不会自动刷新，请确保其生命周期由 Codex/Auth 控制")
	} else {
		if item.AccessToken == "" {
			return nil, errors.New("缺少 accessToken/access_token 或 personal_access_token")
		}
		item.Credentials["access_token"] = item.AccessToken
		if item.RefreshToken != "" {
			item.Credentials["refresh_token"] = item.RefreshToken
			item.Credentials["client_id"] = openai.ClientID
		}
		if item.IDToken != "" {
			item.Credentials["id_token"] = item.IDToken
			_ = enrichCodexImportAccountFromJWT(item, item.IDToken, false, now)
		}
		if err := enrichCodexImportAccountFromJWT(item, item.AccessToken, true, now); err != nil {
			return nil, err
		}
		if _, ok := item.Credentials["expires_at"]; !ok {
			item.WarningTexts = append(item.WarningTexts, "无法从 accessToken 解析过期时间，导入后需自行确认令牌有效性")
		}
		if item.RefreshToken == "" {
			item.WarningTexts = append(item.WarningTexts, "未包含 refresh_token，accessToken 过期后无法自动续期")
		}
	}

	setCodexCredentialIfNotEmpty(item.Credentials, "email", item.Email)
	setCodexCredentialIfNotEmpty(item.Credentials, "chatgpt_account_id", item.AccountID)
	setCodexCredentialIfNotEmpty(item.Credentials, "chatgpt_user_id", item.UserID)
	setCodexCredentialIfNotEmpty(item.Credentials, "organization_id", item.Organization)
	setCodexCredentialIfNotEmpty(item.Credentials, "plan_type", item.PlanType)

	identityToken := item.AccessToken
	if item.PersonalAccessToken != "" {
		identityToken = item.PersonalAccessToken
	} else {
		item.Extra["access_token_sha256"] = codexTokenFingerprint(item.AccessToken)
	}
	item.IdentityKeys = buildCodexImportIdentityKeys(item.AccountID, item.UserID, item.Email, identityToken, item.RefreshToken)
	item.Name = buildCodexImportAccountName(item, entry.Index)

	return item, nil
}

func (h *AccountHandler) hydrateCodexImportPersonalAccessToken(ctx context.Context, proxyID *int64, item *codexImportAccount) error {
	if item == nil || strings.TrimSpace(item.PersonalAccessToken) == "" {
		return nil
	}
	if h.openaiOAuthService == nil {
		return errors.New("OpenAI personal_access_token whoami hydration is not configured")
	}
	metadata, err := h.openaiOAuthService.HydratePersonalAccessToken(ctx, item.PersonalAccessToken, proxyID)
	if err != nil {
		return err
	}
	item.Credentials = service.ApplyOpenAIPersonalAccessTokenMetadata(item.Credentials, item.PersonalAccessToken, metadata)
	item.Email = strings.TrimSpace(metadata.Email)
	item.UserID = strings.TrimSpace(metadata.ChatGPTUserID)
	item.AccountID = strings.TrimSpace(metadata.ChatGPTAccountID)
	item.PlanType = strings.TrimSpace(metadata.ChatGPTPlanType)
	item.IdentityKeys = buildCodexIdentityKeys(item.AccountID, item.UserID, item.Email, item.PersonalAccessToken)
	item.Name = buildCodexImportAccountName(item, 0)
	return nil
}

func normalizeHCPAImportAccount(item *codexImportAccount, raw map[string]any, now time.Time) (bool, error) {
	delete(raw, "type")

	authorization, ok := hcpaAuthorizationHeader(raw)
	if !ok {
		return false, nil
	}
	personalAccessToken := codexBearerAuthorizationToken(authorization)
	if personalAccessToken == "" {
		return true, errors.New("HCPA headers.authorization 必须是 Bearer token")
	}

	item.ImportFormat = codexImportFormatHCPA
	item.PersonalAccessToken = personalAccessToken
	item.AccessToken = firstCodexString(raw,
		[]string{"access_token"},
		[]string{"accessToken"},
		[]string{"at"},
		[]string{"AT"},
	)
	item.RefreshToken = firstCodexString(raw,
		[]string{"refresh_token"},
		[]string{"refreshToken"},
		[]string{"rt"},
		[]string{"RT"},
	)
	item.IDToken = firstCodexString(raw,
		[]string{"id_token"},
		[]string{"idToken"},
	)
	item.Email = firstCodexString(raw, []string{"email"}, []string{"user", "email"})
	item.AccountID = firstCodexString(raw,
		[]string{"account_id"},
		[]string{"accountId"},
		[]string{"chatgpt_account_id"},
		[]string{"chatgptAccountId"},
	)
	item.UserID = firstCodexString(raw,
		[]string{"user_id"},
		[]string{"userId"},
		[]string{"chatgpt_user_id"},
		[]string{"chatgptUserId"},
	)
	item.PlanType = firstCodexString(raw,
		[]string{"plan_type"},
		[]string{"planType"},
		[]string{"chatgpt_plan_type"},
		[]string{"chatgptPlanType"},
	)
	item.Organization = firstCodexString(raw,
		[]string{"organization_id"},
		[]string{"organizationId"},
		[]string{"org_id"},
		[]string{"orgId"},
	)
	item.Name = firstCodexString(raw, []string{"name"})

	if disabled, ok := firstCodexBool(raw, []string{"disabled"}, []string{"is_disabled"}, []string{"isDisabled"}); ok {
		if disabled {
			item.Status = service.StatusDisabled
		} else {
			item.Status = service.StatusActive
		}
	}
	if accountExpiresAt, ok := firstCodexTime(raw,
		[]string{"expired"},
		[]string{"expires"},
		[]string{"expires_at"},
		[]string{"expiresAt"},
	); ok {
		item.AccountExpiresAt = &accountExpiresAt
	}

	_ = enrichCodexImportAccountFromJWT(item, item.IDToken, false, now)
	_ = enrichCodexImportAccountFromJWT(item, item.AccessToken, false, now)
	setCodexTokenExpiresAtIfValidJWT(item, item.AccessToken, now)
	return true, nil
}

func hcpaAuthorizationHeader(raw map[string]any) (any, bool) {
	for key, value := range raw {
		if !strings.EqualFold(strings.TrimSpace(key), "headers") {
			continue
		}
		headers, ok := value.(map[string]any)
		if !ok {
			return nil, true
		}
		for headerKey, headerValue := range headers {
			if strings.EqualFold(strings.TrimSpace(headerKey), "authorization") {
				return headerValue, true
			}
		}
	}
	return nil, false
}

func codexBearerAuthorizationToken(value any) string {
	header := codexStringValue(value)
	if header == "" {
		return ""
	}
	fields := strings.Fields(header)
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
		return ""
	}
	return fields[1]
}

func setCodexTokenExpiresAtIfValidJWT(item *codexImportAccount, token string, now time.Time) {
	claims, err := decodeCodexJWTClaims(token)
	if err != nil || claims.Exp <= 0 {
		return
	}
	if now.Unix() > claims.Exp+codexImportClockSkewSeconds {
		return
	}
	expiresAt := time.Unix(claims.Exp, 0).UTC()
	item.TokenExpiresAt = &expiresAt
}

func enrichCodexImportAccountFromJWT(item *codexImportAccount, token string, validateExpiry bool, now time.Time) error {
	claims, err := decodeCodexJWTClaims(token)
	if err != nil {
		if validateExpiry {
			item.WarningTexts = append(item.WarningTexts, "accessToken 不是可解析 JWT，无法校验过期时间和账号身份")
		}
		return nil
	}
	if validateExpiry && claims.Exp > 0 {
		if now.Unix() > claims.Exp+codexImportClockSkewSeconds {
			return fmt.Errorf("access_token 已过期: %s", time.Unix(claims.Exp, 0).UTC().Format(time.RFC3339))
		}
		expiresAt := time.Unix(claims.Exp, 0).UTC()
		item.TokenExpiresAt = &expiresAt
		item.Credentials["expires_at"] = expiresAt.Format(time.RFC3339)
	}
	if item.Email == "" {
		item.Email = strings.TrimSpace(claims.Email)
	}
	if claims.OpenAIAuth == nil {
		if item.UserID == "" {
			item.UserID = strings.TrimSpace(claims.Sub)
		}
		return nil
	}
	if item.AccountID == "" {
		item.AccountID = strings.TrimSpace(claims.OpenAIAuth.ChatGPTAccountID)
	}
	if item.UserID == "" {
		item.UserID = strings.TrimSpace(claims.OpenAIAuth.ChatGPTUserID)
	}
	if item.UserID == "" {
		item.UserID = strings.TrimSpace(claims.OpenAIAuth.UserID)
	}
	if item.PlanType == "" {
		item.PlanType = strings.TrimSpace(claims.OpenAIAuth.ChatGPTPlanType)
	}
	if item.Organization == "" {
		item.Organization = strings.TrimSpace(claims.OpenAIAuth.POID)
	}
	if item.Organization == "" {
		for _, org := range claims.OpenAIAuth.Organizations {
			if org.IsDefault {
				item.Organization = org.ID
				break
			}
		}
	}
	if item.Organization == "" && len(claims.OpenAIAuth.Organizations) > 0 {
		item.Organization = claims.OpenAIAuth.Organizations[0].ID
	}
	if item.UserID == "" {
		item.UserID = strings.TrimSpace(claims.Sub)
	}
	return nil
}

func decodeCodexJWTClaims(token string) (*codexJWTClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid JWT format")
	}
	payload, err := decodeCodexJWTSegment(parts[1])
	if err != nil {
		return nil, err
	}
	var claims codexJWTClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, err
	}
	return &claims, nil
}

func decodeCodexJWTSegment(segment string) ([]byte, error) {
	if decoded, err := base64.RawURLEncoding.DecodeString(segment); err == nil {
		return decoded, nil
	}
	if decoded, err := base64.RawStdEncoding.DecodeString(segment); err == nil {
		return decoded, nil
	}
	padded := segment
	if rem := len(padded) % 4; rem > 0 {
		padded += strings.Repeat("=", 4-rem)
	}
	if decoded, err := base64.URLEncoding.DecodeString(padded); err == nil {
		return decoded, nil
	}
	return base64.StdEncoding.DecodeString(padded)
}

func buildCodexImportAccountName(item *codexImportAccount, index int) string {
	for _, candidate := range []string{item.Name, item.Email, item.AccountID, item.UserID} {
		candidate = strings.TrimSpace(candidate)
		if candidate != "" {
			return candidate
		}
	}
	return fmt.Sprintf("Codex 导入账号 %d", index)
}

func buildCodexCreateAccountName(base string, item *codexImportAccount, index, total int) string {
	base = strings.TrimSpace(base)
	if base == "" {
		if item == nil {
			return fmt.Sprintf("Codex 导入账号 %d", index)
		}
		return item.Name
	}
	if total > 1 {
		return fmt.Sprintf("%s #%d", base, index)
	}
	return base
}

func resolveCodexImportExpiry(req CodexSessionImportRequest, item *codexImportAccount) (*int64, *time.Time, *bool, []string, error) {
	if item == nil {
		return nil, nil, nil, nil, errors.New("导入项为空")
	}

	if item.IsAgentIdentity {
		return nil, nil, req.AutoPauseOnExpired, nil, nil
	}

	var requestExpiresAt *time.Time
	if req.ExpiresAt != nil && *req.ExpiresAt > 0 {
		t := time.Unix(*req.ExpiresAt, 0).UTC()
		requestExpiresAt = &t
	}

	var accountExpiresAt *time.Time
	var credentialExpiresAt *time.Time
	warnings := make([]string, 0, 2)
	if strings.TrimSpace(item.PersonalAccessToken) != "" {
		accountExpiresAt := item.AccountExpiresAt
		if requestExpiresAt != nil {
			accountExpiresAt = requestExpiresAt
		}
		if accountExpiresAt != nil {
			v := accountExpiresAt.Unix()
			return &v, nil, req.AutoPauseOnExpired, warnings, nil
		}
		return nil, nil, req.AutoPauseOnExpired, warnings, nil
	}
	if item.RefreshToken == "" {
		if item.TokenExpiresAt != nil {
			tokenExpiresAt := item.TokenExpiresAt.UTC()
			accountExpiresAt = &tokenExpiresAt
			credentialExpiresAt = &tokenExpiresAt
		}
		if requestExpiresAt != nil {
			accountExpiresAt = earlierCodexTime(accountExpiresAt, requestExpiresAt)
			credentialExpiresAt = earlierCodexTime(credentialExpiresAt, requestExpiresAt)
		}
		if accountExpiresAt == nil {
			return nil, nil, nil, nil, errors.New("未包含 refresh_token，且无法解析 accessToken 过期时间；请在第一步设置过期时间后再导入")
		}
		if accountExpiresAt.Unix() <= time.Now().UTC().Unix()-codexImportClockSkewSeconds {
			return nil, nil, nil, nil, fmt.Errorf("过期时间已过期: %s", accountExpiresAt.Format(time.RFC3339))
		}
		warnings = append(warnings, "未包含 refresh_token，已按 accessToken/账号过期时间设置自动停止调度")
		if req.AutoPauseOnExpired != nil && !*req.AutoPauseOnExpired {
			warnings = append(warnings, "未包含 refresh_token，已强制开启过期自动暂停")
		}
		autoPause := true
		expiresAtUnix := accountExpiresAt.Unix()
		return &expiresAtUnix, credentialExpiresAt, &autoPause, warnings, nil
	}

	if requestExpiresAt != nil {
		accountExpiresAt = requestExpiresAt
	}
	if item.TokenExpiresAt != nil {
		tokenExpiresAt := item.TokenExpiresAt.UTC()
		credentialExpiresAt = &tokenExpiresAt
	}
	var expiresAtUnix *int64
	if accountExpiresAt != nil {
		v := accountExpiresAt.Unix()
		expiresAtUnix = &v
	}
	return expiresAtUnix, credentialExpiresAt, req.AutoPauseOnExpired, warnings, nil
}

func earlierCodexTime(current, candidate *time.Time) *time.Time {
	if candidate == nil {
		return current
	}
	if current == nil || candidate.Before(*current) {
		t := candidate.UTC()
		return &t
	}
	t := current.UTC()
	return &t
}

func mergeCodexImportExtra(existing, incoming map[string]any) map[string]any {
	out := mergeCodexImportMap(existing, incoming)
	stripInternalHCPAAccountExtra(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

func sanitizeCodexImportCredentialExtras(input map[string]any) map[string]any {
	if len(input) == 0 {
		return nil
	}
	protected := map[string]struct{}{
		"access_token":               {},
		"personal_access_token":      {},
		"refresh_token":              {},
		"id_token":                   {},
		"expires_at":                 {},
		"email":                      {},
		"auth_mode":                  {},
		"openai_auth_mode":           {},
		"token_type":                 {},
		"chatgpt_account_id":         {},
		"chatgpt_user_id":            {},
		"chatgpt_account_is_fedramp": {},
		"chatgpt_plan_type":          {},
		"organization_id":            {},
		"plan_type":                  {},
		"client_id":                  {},
	}
	out := make(map[string]any, len(input))
	for key, value := range input {
		normalizedKey := strings.TrimSpace(key)
		if normalizedKey == "" {
			continue
		}
		if _, ok := protected[strings.ToLower(normalizedKey)]; ok {
			continue
		}
		out[normalizedKey] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func buildCodexIdentityKeys(accountID, userID, email, accessToken string) []string {
	keys := make([]string, 0, 4)
	accountID = strings.TrimSpace(accountID)
	userID = strings.TrimSpace(userID)
	email = strings.ToLower(strings.TrimSpace(email))
	if accountID != "" && userID != "" {
		keys = append(keys, "account_user:"+accountID+":"+userID)
	}
	if accountID != "" && email != "" {
		keys = append(keys, "account_email:"+accountID+":"+email)
	}
	if accountID == "" && userID != "" {
		keys = append(keys, "user:"+userID)
	}
	if accountID == "" && userID == "" && email != "" {
		keys = append(keys, "email:"+email)
	}
	if accessToken = strings.TrimSpace(accessToken); accessToken != "" {
		keys = append(keys, "access:"+codexTokenFingerprint(accessToken))
	}
	return keys
}

func buildCodexAgentIdentityKeys(accountID, userID string) []string {
	accountID = strings.TrimSpace(accountID)
	userID = strings.TrimSpace(userID)
	if accountID == "" || userID == "" {
		return nil
	}
	return []string{"agent_account_user:" + accountID + ":" + userID}
}

func buildCodexImportIdentityKeys(accountID, userID, email, accessToken, refreshToken string) []string {
	accessToken = strings.TrimSpace(accessToken)
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" && accessToken != "" {
		return []string{"access:" + codexTokenFingerprint(accessToken)}
	}
	return buildCodexIdentityKeys(accountID, userID, email, accessToken)
}

func buildCodexAccountIndex(accounts []service.Account) *codexAccountIndex {
	index := &codexAccountIndex{accountsByKey: map[string]service.Account{}}
	for _, account := range accounts {
		index.Add(account)
	}
	return index
}

func (i *codexAccountIndex) Add(account service.Account) {
	if i == nil {
		return
	}
	if i.accountsByKey == nil {
		i.accountsByKey = map[string]service.Account{}
	}
	identityToken := codexCredentialString(account.Credentials, "personal_access_token")
	if identityToken == "" {
		identityToken = codexCredentialString(account.Credentials, "access_token")
	}
	keys := buildCodexIdentityKeys(
		codexCredentialString(account.Credentials, "chatgpt_account_id"),
		codexCredentialString(account.Credentials, "chatgpt_user_id"),
		codexCredentialString(account.Credentials, "email"),
		identityToken,
	)
	for _, key := range keys {
		i.accountsByKey[key] = account
	}
}

func (i *codexAccountIndex) Find(keys []string) *service.Account {
	if i == nil {
		return nil
	}
	for _, key := range keys {
		if account, ok := i.accountsByKey[key]; ok {
			return &account
		}
	}
	return nil
}

func firstSeenCodexIdentity(seen map[string]int, keys []string) (int, bool) {
	for _, key := range keys {
		if index, ok := seen[key]; ok {
			return index, true
		}
	}
	return 0, false
}

func markCodexIdentitySeen(seen map[string]int, keys []string, index int) {
	for _, key := range keys {
		seen[key] = index
	}
}

func mergeCodexImportMap(existing, incoming map[string]any) map[string]any {
	out := make(map[string]any, len(existing)+len(incoming))
	for k, v := range existing {
		out[k] = v
	}
	for k, v := range incoming {
		out[k] = v
	}
	return out
}

func codexImportClearedSensitiveCredentials(existing, merged map[string]any) []string {
	deletes := make([]string, 0, len(service.SensitiveCredentialKeys))
	for _, key := range service.SensitiveCredentialKeys {
		if _, hadExisting := existing[key]; !hadExisting {
			continue
		}
		if _, kept := merged[key]; !kept {
			deletes = append(deletes, key)
		}
	}
	return deletes
}

func mergeCodexImportCredentials(existing, incoming map[string]any, item *codexImportAccount) map[string]any {
	out := mergeCodexImportMap(existing, incoming)
	if item == nil {
		return out
	}
	if strings.TrimSpace(item.PersonalAccessToken) != "" {
		if strings.TrimSpace(item.AccessToken) == "" {
			delete(out, "access_token")
		}
		if item.TokenExpiresAt == nil {
			delete(out, "expires_at")
		}
		if strings.TrimSpace(item.RefreshToken) == "" {
			delete(out, "refresh_token")
			delete(out, "client_id")
		}
		if strings.TrimSpace(item.IDToken) == "" {
			delete(out, "id_token")
		}
		return out
	}
	// 缺少 personal_access_token 表示本次导入未提供该字段；保留旧值避免导入旧备份误清 PAT。
	if strings.TrimSpace(item.RefreshToken) == "" {
		preserveRefresh := codexCredentialString(existing, "refresh_token") != "" &&
			codexCredentialString(existing, "personal_access_token") == ""
		if !preserveRefresh {
			delete(out, "refresh_token")
			delete(out, "client_id")
		} else {
			out["refresh_token"] = existing["refresh_token"]
			if clientID, ok := existing["client_id"]; ok {
				out["client_id"] = clientID
			}
		}
	}
	if strings.TrimSpace(item.IDToken) == "" {
		delete(out, "id_token")
	}
	return out
}

func codexCredentialString(credentials map[string]any, key string) string {
	if credentials == nil {
		return ""
	}
	return codexStringValue(credentials[key])
}

func codexTokenFingerprint(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

func looksLikeJSON(content string) bool {
	if content == "" {
		return false
	}
	switch content[0] {
	case '{', '[':
		return true
	default:
		return false
	}
}

func firstCodexMap(obj map[string]any, paths ...[]string) (map[string]any, bool) {
	for _, path := range paths {
		if value, ok := codexPathValue(obj, path); ok {
			if nested, ok := value.(map[string]any); ok {
				return nested, true
			}
		}
	}
	return nil, false
}

func firstCodexString(obj map[string]any, paths ...[]string) string {
	for _, path := range paths {
		if value, ok := codexPathValue(obj, path); ok {
			if str := codexStringValue(value); str != "" {
				return str
			}
		}
	}
	return ""
}

func firstCodexBool(obj map[string]any, paths ...[]string) (bool, bool) {
	for _, path := range paths {
		if value, ok := codexPathValue(obj, path); ok {
			parsed, parsedOK := codexBoolValue(value)
			if parsedOK {
				return parsed, true
			}
		}
	}
	return false, false
}

func copyCodexExtraString(obj map[string]any, extra map[string]any, key string, path []string) {
	value := firstCodexString(obj, path)
	if value != "" {
		extra[key] = value
	}
}

func firstCodexTime(obj map[string]any, paths ...[]string) (time.Time, bool) {
	for _, path := range paths {
		if value, ok := codexTimeAt(obj, path); ok {
			return value, true
		}
	}
	return time.Time{}, false
}

func codexTimeAt(obj map[string]any, path []string) (time.Time, bool) {
	value, ok := codexPathValue(obj, path)
	if !ok {
		return time.Time{}, false
	}
	return parseCodexTimeValue(value)
}

func codexPathValue(obj map[string]any, path []string) (any, bool) {
	var current any = obj
	for _, key := range path {
		currentObj, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		value, ok := currentObj[key]
		if !ok {
			return nil, false
		}
		current = value
	}
	return current, true
}

func codexStringValue(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return strings.TrimSpace(v.String())
	case float64:
		return strings.TrimSpace(strconv.FormatFloat(v, 'f', -1, 64))
	case float32:
		return strings.TrimSpace(strconv.FormatFloat(float64(v), 'f', -1, 32))
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case int32:
		return strconv.FormatInt(int64(v), 10)
	default:
		return ""
	}
}

func codexBoolValue(value any) (bool, bool) {
	switch v := value.(type) {
	case bool:
		return v, true
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(v))
		return parsed, err == nil
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return i != 0, true
		}
		if f, err := strconv.ParseFloat(v.String(), 64); err == nil {
			return f != 0, true
		}
	case float64:
		return v != 0, true
	case float32:
		return v != 0, true
	case int:
		return v != 0, true
	case int64:
		return v != 0, true
	case int32:
		return v != 0, true
	}
	return false, false
}

func setCodexCredentialIfNotEmpty(credentials map[string]any, key, value string) {
	value = strings.TrimSpace(value)
	if value != "" {
		credentials[key] = value
	}
}

func parseCodexTimeValue(value any) (time.Time, bool) {
	switch v := value.(type) {
	case string:
		v = strings.TrimSpace(v)
		if v == "" {
			return time.Time{}, false
		}
		if parsed, err := time.Parse(time.RFC3339Nano, v); err == nil {
			return parsed.UTC(), true
		}
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return codexUnixTime(n), true
		}
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return codexUnixTime(n), true
		}
		if f, err := v.Float64(); err == nil {
			return codexUnixTime(int64(f)), true
		}
	case float64:
		return codexUnixTime(int64(v)), true
	case int:
		return codexUnixTime(int64(v)), true
	case int64:
		return codexUnixTime(v), true
	}
	return time.Time{}, false
}

func codexUnixTime(value int64) time.Time {
	if value > 1_000_000_000_000 {
		return time.UnixMilli(value).UTC()
	}
	return time.Unix(value, 0).UTC()
}
