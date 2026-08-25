package admin

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func TestParseCodexSessionImportEntriesSupportsRawTokenJSONAndArray(t *testing.T) {
	token1 := "raw-access-token-1"
	token2 := buildCodexImportTestJWT(t, time.Now().Add(time.Hour), map[string]any{
		"email": "json@example.com",
	})
	token3 := "raw-access-token-3"

	req := CodexSessionImportRequest{
		Content: fmt.Sprintf("%s\n{\"accessToken\":%q}\n[%q]", token1, token2, token3),
	}

	entries, err := parseCodexSessionImportEntries(req)
	if err != nil {
		t.Fatalf("parseCodexSessionImportEntries error = %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("len(entries) = %d, want 3", len(entries))
	}

	first, err := normalizeCodexImportEntry(entries[0])
	if err != nil {
		t.Fatalf("normalize raw token error = %v", err)
	}
	if first.Credentials["access_token"] != token1 {
		t.Fatalf("raw token access_token = %v, want %s", first.Credentials["access_token"], token1)
	}

	second, err := normalizeCodexImportEntry(entries[1])
	if err != nil {
		t.Fatalf("normalize json token error = %v", err)
	}
	if second.Email != "json@example.com" {
		t.Fatalf("email = %q, want json@example.com", second.Email)
	}

	third, err := normalizeCodexImportEntry(entries[2])
	if err != nil {
		t.Fatalf("normalize array token error = %v", err)
	}
	if third.Credentials["access_token"] != token3 {
		t.Fatalf("array token access_token = %v, want %s", third.Credentials["access_token"], token3)
	}
}

func TestParseCodexSessionImportEntriesFallsBackToLineModeForMixedJSONAndToken(t *testing.T) {
	req := CodexSessionImportRequest{
		Content: "{\"accessToken\":\"json-line-token\"}\nraw-line-token",
	}

	entries, err := parseCodexSessionImportEntries(req)
	if err != nil {
		t.Fatalf("parseCodexSessionImportEntries error = %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(entries))
	}

	first, err := normalizeCodexImportEntry(entries[0])
	if err != nil {
		t.Fatalf("normalize json line error = %v", err)
	}
	if first.Credentials["access_token"] != "json-line-token" {
		t.Fatalf("json line access_token = %v, want json-line-token", first.Credentials["access_token"])
	}

	second, err := normalizeCodexImportEntry(entries[1])
	if err != nil {
		t.Fatalf("normalize raw line error = %v", err)
	}
	if second.Credentials["access_token"] != "raw-line-token" {
		t.Fatalf("raw line access_token = %v, want raw-line-token", second.Credentials["access_token"])
	}
}

func TestNormalizeCodexSessionJSONExtractsCredentialsAndIgnoresSessionToken(t *testing.T) {
	accessToken := buildCodexImportTestJWT(t, time.Now().Add(time.Hour), map[string]any{
		"email": "claim@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct-from-claim",
			"chatgpt_user_id":    "user-from-claim",
			"chatgpt_plan_type":  "plus",
			"poid":               "org-from-claim",
		},
	})
	raw := map[string]any{
		"user": map[string]any{
			"id":    "user-from-json",
			"name":  "Sup OO",
			"email": "json@example.com",
			"image": "https://example.com/avatar.png",
		},
		"account": map[string]any{
			"id":       "acct-from-json",
			"planType": "free",
		},
		"accessToken":  accessToken,
		"sessionToken": "secret-session-token",
		"expires":      "2026-08-05T13:40:42.836Z",
	}

	item, err := normalizeCodexImportEntry(codexImportEntry{Index: 1, Value: raw})
	if err != nil {
		t.Fatalf("normalizeCodexImportEntry error = %v", err)
	}
	if item.Credentials["access_token"] != accessToken {
		t.Fatalf("access_token not stored")
	}
	if item.Credentials["email"] != "json@example.com" {
		t.Fatalf("email = %v, want json@example.com", item.Credentials["email"])
	}
	if item.Credentials["chatgpt_account_id"] != "acct-from-json" {
		t.Fatalf("chatgpt_account_id = %v, want acct-from-json", item.Credentials["chatgpt_account_id"])
	}
	if item.Credentials["chatgpt_user_id"] != "user-from-json" {
		t.Fatalf("chatgpt_user_id = %v, want user-from-json", item.Credentials["chatgpt_user_id"])
	}
	if item.Credentials["plan_type"] != "free" {
		t.Fatalf("plan_type = %v, want free", item.Credentials["plan_type"])
	}
	if _, ok := item.Credentials["session_token"]; ok {
		t.Fatalf("session_token should not be written to credentials")
	}
	if item.Extra["session_token_present"] != true {
		t.Fatalf("session_token_present = %v, want true", item.Extra["session_token_present"])
	}
	if item.Extra["session_expires_at"] != "2026-08-05T13:40:42Z" {
		t.Fatalf("session_expires_at = %v", item.Extra["session_expires_at"])
	}
	if item.TokenExpiresAt == nil {
		t.Fatalf("TokenExpiresAt should be parsed from accessToken")
	}
}

func TestMergeCodexImportCredentialsClearsStaleRefreshFieldsWhenIncomingHasNoRefreshToken(t *testing.T) {
	existing := map[string]any{
		"access_token":       "old-access-token",
		"id_token":           "old-id-token",
		"model_mapping":      map[string]any{"from": "existing"},
		"chatgpt_account_id": "acct-old",
		"unrelated_existing": "keep",
	}
	incoming := map[string]any{
		"access_token":       "new-access-token",
		"expires_at":         "2026-08-05T13:40:42Z",
		"chatgpt_account_id": "acct-new",
	}
	item := &codexImportAccount{
		AccessToken: "new-access-token",
	}

	merged := mergeCodexImportCredentials(existing, incoming, item)

	if merged["access_token"] != "new-access-token" {
		t.Fatalf("access_token = %v, want new-access-token", merged["access_token"])
	}
	if merged["chatgpt_account_id"] != "acct-new" {
		t.Fatalf("chatgpt_account_id = %v, want acct-new", merged["chatgpt_account_id"])
	}
	if _, ok := merged["refresh_token"]; ok {
		t.Fatalf("refresh_token should be cleared")
	}
	if _, ok := merged["client_id"]; ok {
		t.Fatalf("client_id should be cleared")
	}
	if _, ok := merged["id_token"]; ok {
		t.Fatalf("id_token should be cleared")
	}
	if merged["unrelated_existing"] != "keep" {
		t.Fatalf("unrelated_existing = %v, want keep", merged["unrelated_existing"])
	}
	if _, ok := merged["model_mapping"]; !ok {
		t.Fatalf("model_mapping should be preserved")
	}
}

func TestMergeCodexImportCredentialsPreservesExistingRefreshFieldsWhenAccessOnlyImportMatches(t *testing.T) {
	existing := map[string]any{
		"access_token":  "old-access-token",
		"refresh_token": "old-refresh-token",
		"client_id":     "old-client-id",
		"id_token":      "old-id-token",
	}
	incoming := map[string]any{
		"access_token": "new-access-token",
		"expires_at":   "2026-08-05T13:40:42Z",
	}
	item := &codexImportAccount{
		AccessToken: "new-access-token",
	}

	merged := mergeCodexImportCredentials(existing, incoming, item)

	if merged["access_token"] != "new-access-token" {
		t.Fatalf("access_token = %v, want new-access-token", merged["access_token"])
	}
	if merged["refresh_token"] != "old-refresh-token" {
		t.Fatalf("refresh_token = %v, want old-refresh-token", merged["refresh_token"])
	}
	if merged["client_id"] != "old-client-id" {
		t.Fatalf("client_id = %v, want old-client-id", merged["client_id"])
	}
	if _, ok := merged["id_token"]; ok {
		t.Fatalf("id_token should be cleared when incoming has no id_token")
	}
}

func TestMergeCodexImportCredentialsKeepsRefreshFieldsWhenIncomingHasRefreshToken(t *testing.T) {
	existing := map[string]any{
		"refresh_token": "old-refresh-token",
		"client_id":     "old-client-id",
		"id_token":      "old-id-token",
	}
	incoming := map[string]any{
		"access_token":  "new-access-token",
		"refresh_token": "new-refresh-token",
		"client_id":     "new-client-id",
		"id_token":      "new-id-token",
	}
	item := &codexImportAccount{
		AccessToken:  "new-access-token",
		RefreshToken: "new-refresh-token",
		IDToken:      "new-id-token",
	}

	merged := mergeCodexImportCredentials(existing, incoming, item)

	if merged["refresh_token"] != "new-refresh-token" {
		t.Fatalf("refresh_token = %v, want new-refresh-token", merged["refresh_token"])
	}
	if merged["client_id"] != "new-client-id" {
		t.Fatalf("client_id = %v, want new-client-id", merged["client_id"])
	}
	if merged["id_token"] != "new-id-token" {
		t.Fatalf("id_token = %v, want new-id-token", merged["id_token"])
	}
}

func TestNormalizeCodexImportRejectsExpiredAccessToken(t *testing.T) {
	expiredToken := buildCodexImportTestJWT(t, time.Now().Add(-time.Hour), map[string]any{})

	_, err := normalizeCodexImportEntry(codexImportEntry{Index: 1, Value: expiredToken})
	if err == nil {
		t.Fatal("normalizeCodexImportEntry error = nil, want expired token error")
	}
	if !strings.Contains(err.Error(), "已过期") {
		t.Fatalf("error = %v, want expired token message", err)
	}
}

func TestResolveCodexImportExpiryForNoRefreshTokenUsesTokenExpiry(t *testing.T) {
	tokenExpiresAt := time.Now().Add(time.Hour).UTC()
	item := &codexImportAccount{
		AccessToken:    "access-token",
		Credentials:    map[string]any{"access_token": "access-token"},
		TokenExpiresAt: &tokenExpiresAt,
		WarningTexts:   []string{},
	}
	disabled := false
	req := CodexSessionImportRequest{AutoPauseOnExpired: &disabled}

	accountExpiresAt, credentialExpiresAt, autoPause, warnings, err := resolveCodexImportExpiry(req, item)
	if err != nil {
		t.Fatalf("resolveCodexImportExpiry error = %v", err)
	}
	if accountExpiresAt == nil || *accountExpiresAt != tokenExpiresAt.Unix() {
		t.Fatalf("account expires_at = %v, want %d", accountExpiresAt, tokenExpiresAt.Unix())
	}
	if credentialExpiresAt == nil || credentialExpiresAt.Unix() != tokenExpiresAt.Unix() {
		t.Fatalf("credential expires_at = %v, want %s", credentialExpiresAt, tokenExpiresAt)
	}
	if autoPause == nil || !*autoPause {
		t.Fatalf("autoPause = %v, want true", autoPause)
	}
	if len(warnings) == 0 {
		t.Fatalf("warnings should not be empty")
	}
}

func TestResolveCodexImportExpiryForNoRefreshTokenRequiresExpiry(t *testing.T) {
	item := &codexImportAccount{
		AccessToken:  "opaque-access-token",
		Credentials:  map[string]any{"access_token": "opaque-access-token"},
		WarningTexts: []string{},
	}

	_, _, _, _, err := resolveCodexImportExpiry(CodexSessionImportRequest{}, item)
	if err == nil {
		t.Fatal("resolveCodexImportExpiry error = nil, want missing expiry error")
	}
	if !strings.Contains(err.Error(), "无法解析 accessToken 过期时间") {
		t.Fatalf("error = %v, want missing expiry message", err)
	}
}

func TestResolveCodexImportExpiryForNoRefreshTokenUsesEarlierRequestExpiry(t *testing.T) {
	tokenExpiresAt := time.Now().Add(2 * time.Hour).UTC()
	requestExpiresAt := time.Now().Add(time.Hour).UTC()
	item := &codexImportAccount{
		AccessToken:    "access-token",
		Credentials:    map[string]any{"access_token": "access-token"},
		TokenExpiresAt: &tokenExpiresAt,
		WarningTexts:   []string{},
	}
	reqUnix := requestExpiresAt.Unix()
	req := CodexSessionImportRequest{ExpiresAt: &reqUnix}

	accountExpiresAt, credentialExpiresAt, _, _, err := resolveCodexImportExpiry(req, item)
	if err != nil {
		t.Fatalf("resolveCodexImportExpiry error = %v", err)
	}
	if accountExpiresAt == nil || *accountExpiresAt != requestExpiresAt.Unix() {
		t.Fatalf("account expires_at = %v, want %d", accountExpiresAt, requestExpiresAt.Unix())
	}
	if credentialExpiresAt == nil || credentialExpiresAt.Unix() != requestExpiresAt.Unix() {
		t.Fatalf("credential expires_at = %v, want %s", credentialExpiresAt, requestExpiresAt)
	}
}

func TestCodexIdentityKeysPreferStrongIdentifiers(t *testing.T) {
	keys := buildCodexIdentityKeys("acct-1", "user-1", "same@example.com", "token")
	for _, key := range keys {
		if key == "account:acct-1" || key == "email:same@example.com" || key == "user:user-1" {
			t.Fatalf("strong identity should not include broad fallback key %q: %v", key, keys)
		}
	}
	for _, want := range []string{
		"account_user:acct-1:user-1",
		"account_email:acct-1:same@example.com",
	} {
		if !codexTestHasIdentityKey(keys, want) {
			t.Fatalf("identity keys missing %q: %v", want, keys)
		}
	}

	keys = buildCodexIdentityKeys("", "", "same@example.com", "token")
	if !codexTestHasIdentityKey(keys, "email:same@example.com") {
		t.Fatalf("weak identity should include email fallback: %v", keys)
	}
}

func TestCodexImportIdentityKeysUseAccessOnlyIdentityWhenRefreshTokenMissing(t *testing.T) {
	keys := buildCodexImportIdentityKeys("acct-1", "user-1", "same@example.com", "access-token", "")
	if len(keys) != 1 {
		t.Fatalf("access-only import keys = %v, want exactly one access fingerprint key", keys)
	}
	if want := "access:" + codexTokenFingerprint("access-token"); keys[0] != want {
		t.Fatalf("access-only import key = %q, want %q", keys[0], want)
	}
}

func TestCodexImportIdentityKeysUseStrongIdentityWhenRefreshTokenPresent(t *testing.T) {
	keys := buildCodexImportIdentityKeys("acct-1", "user-1", "same@example.com", "access-token", "refresh-token")
	if !codexTestHasIdentityKey(keys, "account_user:acct-1:user-1") {
		t.Fatalf("refreshable import should include account_user key: %v", keys)
	}
	if !codexTestHasIdentityKey(keys, "account_email:acct-1:same@example.com") {
		t.Fatalf("refreshable import should include account_email key: %v", keys)
	}
}

func TestCodexAccountIndexDoesNotMatchSameWorkspaceDifferentUser(t *testing.T) {
	index := buildCodexAccountIndex([]service.Account{{
		ID:   1,
		Name: "existing-user-1",
		Credentials: map[string]any{
			"chatgpt_account_id": "acct-1",
			"chatgpt_user_id":    "user-1",
			"email":              "user1@example.com",
			"access_token":       "token-1",
		},
	}})

	keys := buildCodexIdentityKeys("acct-1", "user-2", "user2@example.com", "token-2")
	if existing := index.Find(keys); existing != nil {
		t.Fatalf("same workspace with different user matched account %d", existing.ID)
	}
}

func TestCodexAccountIndexMatchesSameWorkspaceAndUser(t *testing.T) {
	index := buildCodexAccountIndex([]service.Account{{
		ID:   1,
		Name: "existing-user-1",
		Credentials: map[string]any{
			"chatgpt_account_id": "acct-1",
			"chatgpt_user_id":    "user-1",
			"email":              "user1@example.com",
			"access_token":       "token-1",
		},
	}})

	keys := buildCodexIdentityKeys("acct-1", "user-1", "other@example.com", "token-2")
	existing := index.Find(keys)
	if existing == nil || existing.ID != 1 {
		t.Fatalf("same workspace and user should match account 1, got %#v", existing)
	}
}

func TestCodexAccountIndexMatchesRepeatedAgentIdentityImport(t *testing.T) {
	index := buildCodexAccountIndex([]service.Account{{
		ID: 1,
		Credentials: map[string]any{
			"auth_mode":          service.OpenAIAuthModeAgentIdentity,
			"chatgpt_account_id": "acct-agent",
			"chatgpt_user_id":    "user-agent",
			"agent_runtime_id":   "runtime-agent",
		},
	}})

	keys := buildCodexAgentIdentityKeys("acct-agent", "user-agent")
	if existing := index.Find(keys); existing == nil || existing.ID != 1 {
		t.Fatalf("repeated agent identity import did not match account 1: %#v", existing)
	}
}

func TestCodexAccountIndexUpdateRemovesStaleIdentityKeys(t *testing.T) {
	index := buildCodexAccountIndex([]service.Account{{
		ID: 1,
		Credentials: map[string]any{
			"chatgpt_account_id": "acct-old",
			"chatgpt_user_id":    "user-old",
			"email":              "old@example.com",
			"access_token":       "token-old",
		},
	}})

	index.Add(service.Account{
		ID: 1,
		Credentials: map[string]any{
			"chatgpt_account_id": "acct-new",
			"chatgpt_user_id":    "user-new",
			"email":              "new@example.com",
			"access_token":       "token-new",
		},
	})

	for _, key := range buildCodexIdentityKeys("acct-old", "user-old", "old@example.com", "token-old") {
		if existing := index.Find([]string{key}); existing != nil {
			t.Fatalf("stale identity key %q matched account %d after update", key, existing.ID)
		}
	}
	for _, key := range buildCodexIdentityKeys("acct-new", "user-new", "new@example.com", "token-new") {
		if existing := index.Find([]string{key}); existing == nil || existing.ID != 1 {
			t.Fatalf("updated identity key %q did not match account 1: %#v", key, existing)
		}
	}
}

func codexTestHasIdentityKey(keys []string, want string) bool {
	for _, key := range keys {
		if key == want {
			return true
		}
	}
	return false
}

func TestNormalizeCodexImportPersonalAccessTokenStoresPATMetadataAndFedRAMP(t *testing.T) {
	raw := map[string]any{
		"personalAccessToken": "pat-token",
		"email":               "pat@example.com",
		"chatgptAccountId":    "acct-pat",
		"chatgptUserId":       "user-pat",
		"chatgptPlanType":     "team",
		"account": map[string]any{
			"isFedramp": true,
		},
	}

	item, err := normalizeCodexImportEntry(codexImportEntry{Index: 1, Value: raw})
	if err != nil {
		t.Fatalf("normalizeCodexImportEntry error = %v", err)
	}
	if item.Credentials["personal_access_token"] != "pat-token" {
		t.Fatalf("personal_access_token = %v, want pat-token", item.Credentials["personal_access_token"])
	}
	if _, ok := item.Credentials["access_token"]; ok {
		t.Fatalf("access_token should not be stored for PAT import")
	}
	if item.Credentials["email"] != "pat@example.com" {
		t.Fatalf("email = %v, want pat@example.com", item.Credentials["email"])
	}
	if item.Credentials["chatgpt_account_id"] != "acct-pat" {
		t.Fatalf("chatgpt_account_id = %v, want acct-pat", item.Credentials["chatgpt_account_id"])
	}
	if item.Credentials["chatgpt_user_id"] != "user-pat" {
		t.Fatalf("chatgpt_user_id = %v, want user-pat", item.Credentials["chatgpt_user_id"])
	}
	if item.Credentials["plan_type"] != "team" {
		t.Fatalf("plan_type = %v, want team", item.Credentials["plan_type"])
	}
	if item.Credentials["chatgpt_account_is_fedramp"] != true {
		t.Fatalf("chatgpt_account_is_fedramp = %v, want true", item.Credentials["chatgpt_account_is_fedramp"])
	}
	if _, ok := item.Extra["personal_access_token_sha256"]; ok {
		t.Fatalf("personal_access_token_sha256 should not be recorded for PAT import")
	}
	if _, ok := item.Extra["access_token_sha256"]; ok {
		t.Fatalf("access_token_sha256 should not be recorded for PAT import")
	}
	if item.TokenExpiresAt != nil {
		t.Fatalf("TokenExpiresAt = %v, want nil for PAT import", item.TokenExpiresAt)
	}
}

func TestSanitizeCodexImportCredentialExtrasProtectsHydratedAuthMetadata(t *testing.T) {
	credentials := service.ApplyOpenAIPersonalAccessTokenMetadata(map[string]any{
		"auth_mode":        "personal_access_token",
		"openai_auth_mode": "codex_pat",
		"token_type":       "Bearer",
	}, "at-server", &service.OpenAIPersonalAccessTokenMetadata{
		Email:                   "server@example.com",
		ChatGPTUserID:           "user-server",
		ChatGPTAccountID:        "acct-server",
		ChatGPTPlanType:         "team",
		ChatGPTAccountIsFedRAMP: true,
	})
	extras := sanitizeCodexImportCredentialExtras(map[string]any{
		"auth_mode":                   "oauth",
		"openai_auth_mode":            "oauth",
		"token_type":                  "service_account",
		"chatgpt_account_is_fedramp":  false,
		"chatgpt_plan_type":           "free",
		"plan_type":                   "free",
		"custom_passthrough_metadata": "kept",
	})

	merged := mergeCodexImportMap(credentials, extras)

	if merged["auth_mode"] != "personal_access_token" {
		t.Fatalf("auth_mode = %v, want personal_access_token", merged["auth_mode"])
	}
	if merged["openai_auth_mode"] != "codex_pat" {
		t.Fatalf("openai_auth_mode = %v, want codex_pat", merged["openai_auth_mode"])
	}
	if merged["token_type"] != "Bearer" {
		t.Fatalf("token_type = %v, want Bearer", merged["token_type"])
	}
	if merged["chatgpt_account_is_fedramp"] != true {
		t.Fatalf("chatgpt_account_is_fedramp = %v, want true", merged["chatgpt_account_is_fedramp"])
	}
	if merged["chatgpt_plan_type"] != "team" {
		t.Fatalf("chatgpt_plan_type = %v, want team", merged["chatgpt_plan_type"])
	}
	if merged["plan_type"] != "team" {
		t.Fatalf("plan_type = %v, want team", merged["plan_type"])
	}
	if merged["custom_passthrough_metadata"] != "kept" {
		t.Fatalf("custom_passthrough_metadata = %v, want kept", merged["custom_passthrough_metadata"])
	}
}

func TestNormalizeCodexImportPersonalAccessTokenIgnoresExpiredAccessToken(t *testing.T) {
	raw := map[string]any{
		"personal_access_token": "pat-token",
		"accessToken":           buildCodexImportTestJWT(t, time.Now().Add(-time.Hour), nil),
		"expiresAt":             time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	}

	item, err := normalizeCodexImportEntry(codexImportEntry{Index: 1, Value: raw})
	if err != nil {
		t.Fatalf("normalizeCodexImportEntry error = %v", err)
	}
	if item.Credentials["personal_access_token"] != "pat-token" {
		t.Fatalf("personal_access_token = %v, want pat-token", item.Credentials["personal_access_token"])
	}
	if _, ok := item.Credentials["access_token"]; ok {
		t.Fatalf("expired access_token should be ignored for PAT import")
	}
	if _, ok := item.Credentials["expires_at"]; ok {
		t.Fatalf("expires_at should be ignored for PAT import")
	}
}

func TestMergeCodexImportExtraStripsInternalHCPAFields(t *testing.T) {
	extra := mergeCodexImportExtra(
		map[string]any{
			"existing":             "value",
			"hcpa_disabled":        true,
			"hcpa_expired_at":      "2026-01-01T00:00:00Z",
			"hcpa_last_refresh_at": "2026-01-01T00:00:00Z",
		},
		map[string]any{
			"keep":                 "value",
			"import_source":        "hcpa",
			"import_format":        "hcpa",
			"imported_at":          "2026-06-23T14:06:08+08:00",
			"hcpa_disabled":        false,
			"hcpa_expired_at":      "2026-12-31T10:00:00+08:00",
			"hcpa_last_refresh_at": "2026-06-23T14:06:08+08:00",
		},
	)

	if extra["existing"] != "value" {
		t.Fatalf("existing = %v, want value", extra["existing"])
	}
	if extra["keep"] != "value" {
		t.Fatalf("keep = %v, want value", extra["keep"])
	}
	for _, key := range internalHCPAAccountExtraKeys() {
		if _, ok := extra[key]; ok {
			t.Fatalf("internal HCPA import field %q should not be retained in Extra: %v", key, extra)
		}
	}
}

func TestNormalizeCodexImportHCPAFormatExtractsAllFields(t *testing.T) {
	accountExpiry := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	lastRefresh := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	raw := map[string]any{
		"at":           "hcpa-access-token",
		"rt":           "hcpa-refresh-token",
		"id_token":     "hcpa-id-token",
		"account_id":   "acct-hcpa",
		"email":        "hcpa@example.com",
		"expired":      accountExpiry.Format(time.RFC3339),
		"disabled":     true,
		"last_refresh": lastRefresh.Format(time.RFC3339),
		"type":         "codex",
		"headers": map[string]any{
			"Authorization": "Bearer hcpa-pat-token",
		},
	}

	item, err := normalizeCodexImportEntry(codexImportEntry{Index: 1, Value: raw})
	if err != nil {
		t.Fatalf("normalizeCodexImportEntry error = %v", err)
	}
	if item.ImportFormat != codexImportFormatHCPA {
		t.Fatalf("ImportFormat = %q, want %q", item.ImportFormat, codexImportFormatHCPA)
	}
	if item.Credentials["personal_access_token"] != "hcpa-pat-token" {
		t.Fatalf("personal_access_token = %v, want hcpa-pat-token", item.Credentials["personal_access_token"])
	}
	if item.Credentials["access_token"] != "hcpa-access-token" {
		t.Fatalf("access_token = %v, want hcpa-access-token", item.Credentials["access_token"])
	}
	if item.Credentials["refresh_token"] != "hcpa-refresh-token" {
		t.Fatalf("refresh_token = %v, want hcpa-refresh-token", item.Credentials["refresh_token"])
	}
	if item.Credentials["id_token"] != "hcpa-id-token" {
		t.Fatalf("id_token = %v, want hcpa-id-token", item.Credentials["id_token"])
	}
	if item.Credentials["client_id"] == "" {
		t.Fatalf("client_id should be stored when HCPA rt is present")
	}
	if item.Credentials["chatgpt_account_id"] != "acct-hcpa" {
		t.Fatalf("chatgpt_account_id = %v, want acct-hcpa", item.Credentials["chatgpt_account_id"])
	}
	if item.Credentials["email"] != "hcpa@example.com" {
		t.Fatalf("email = %v, want hcpa@example.com", item.Credentials["email"])
	}
	if item.Status != "disabled" {
		t.Fatalf("Status = %q, want disabled", item.Status)
	}
	if item.AccountExpiresAt == nil || item.AccountExpiresAt.Unix() != accountExpiry.Unix() {
		t.Fatalf("AccountExpiresAt = %v, want %s", item.AccountExpiresAt, accountExpiry)
	}
	for _, key := range []string{"import_source", "import_format", "imported_at", "hcpa_disabled", "hcpa_expired_at", "hcpa_last_refresh_at"} {
		if _, ok := item.Extra[key]; ok {
			t.Fatalf("internal HCPA import field %q should not be stored in Extra: %v", key, item.Extra)
		}
	}
	if _, ok := item.Extra["personal_access_token_sha256"]; ok {
		t.Fatalf("personal_access_token_sha256 should not be recorded for HCPA PAT import")
	}
	if _, ok := item.Extra["access_token_sha256"]; ok {
		t.Fatalf("access_token_sha256 should not be recorded for HCPA PAT import")
	}

	accountExpiresAt, credentialExpiresAt, _, _, err := resolveCodexImportExpiry(CodexSessionImportRequest{}, item)
	if err != nil {
		t.Fatalf("resolveCodexImportExpiry error = %v", err)
	}
	if accountExpiresAt == nil || *accountExpiresAt != accountExpiry.Unix() {
		t.Fatalf("account expires_at = %v, want %d", accountExpiresAt, accountExpiry.Unix())
	}
	if credentialExpiresAt != nil {
		t.Fatalf("credential expires_at = %v, want nil for opaque HCPA at", credentialExpiresAt)
	}
}

func TestResolveCodexImportExpiryForPersonalAccessTokenDoesNotRequireExpiry(t *testing.T) {
	item := &codexImportAccount{
		PersonalAccessToken: "pat-token",
		Credentials:         map[string]any{"personal_access_token": "pat-token"},
		WarningTexts:        []string{},
	}

	accountExpiresAt, credentialExpiresAt, autoPause, warnings, err := resolveCodexImportExpiry(CodexSessionImportRequest{}, item)
	if err != nil {
		t.Fatalf("resolveCodexImportExpiry error = %v", err)
	}
	if accountExpiresAt != nil || credentialExpiresAt != nil || autoPause != nil {
		t.Fatalf("PAT without requested expiry should not force expiry: account=%v credential=%v auto=%v", accountExpiresAt, credentialExpiresAt, autoPause)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}

	requestExpiresAt := time.Now().Add(time.Hour).UTC().Unix()
	autoPauseInput := false
	accountExpiresAt, credentialExpiresAt, autoPause, _, err = resolveCodexImportExpiry(CodexSessionImportRequest{
		ExpiresAt:          &requestExpiresAt,
		AutoPauseOnExpired: &autoPauseInput,
	}, item)
	if err != nil {
		t.Fatalf("resolveCodexImportExpiry with request expiry error = %v", err)
	}
	if accountExpiresAt == nil || *accountExpiresAt != requestExpiresAt {
		t.Fatalf("account expires_at = %v, want %d", accountExpiresAt, requestExpiresAt)
	}
	if credentialExpiresAt != nil {
		t.Fatalf("credential expires_at = %v, want nil for PAT", credentialExpiresAt)
	}
	if autoPause == nil || *autoPause != false {
		t.Fatalf("autoPause = %v, want false", autoPause)
	}
}

func TestMergeCodexImportCredentialsPersonalAccessTokenClearsStaleOAuthFields(t *testing.T) {
	existing := map[string]any{
		"access_token":       "old-access-token",
		"refresh_token":      "old-refresh-token",
		"client_id":          "old-client-id",
		"id_token":           "old-id-token",
		"expires_at":         "2026-08-05T13:40:42Z",
		"unrelated_existing": "keep",
	}
	incoming := map[string]any{
		"personal_access_token": "pat-new",
		"chatgpt_account_id":    "acct-new",
	}
	item := &codexImportAccount{PersonalAccessToken: "pat-new"}

	merged := mergeCodexImportCredentials(existing, incoming, item)

	if merged["personal_access_token"] != "pat-new" {
		t.Fatalf("personal_access_token = %v, want pat-new", merged["personal_access_token"])
	}
	for _, key := range []string{"access_token", "refresh_token", "client_id", "id_token", "expires_at"} {
		if _, ok := merged[key]; ok {
			t.Fatalf("%s should be cleared on PAT import", key)
		}
	}
	if merged["unrelated_existing"] != "keep" {
		t.Fatalf("unrelated_existing = %v, want keep", merged["unrelated_existing"])
	}
}

func TestMergeCodexImportCredentialsMissingPersonalAccessTokenPreservesExistingPAT(t *testing.T) {
	existing := map[string]any{
		"personal_access_token": "pat-existing",
		"access_token":          "old-access-token",
		"refresh_token":         "old-refresh-token",
		"client_id":             "old-client-id",
	}
	incoming := map[string]any{
		"access_token": "new-access-token",
		"expires_at":   "2026-08-05T13:40:42Z",
	}
	item := &codexImportAccount{AccessToken: "new-access-token"}

	merged := mergeCodexImportCredentials(existing, incoming, item)

	if merged["personal_access_token"] != "pat-existing" {
		t.Fatalf("personal_access_token = %v, want preserved PAT", merged["personal_access_token"])
	}
	if merged["access_token"] != "new-access-token" {
		t.Fatalf("access_token = %v, want new-access-token", merged["access_token"])
	}
	if _, ok := merged["refresh_token"]; ok {
		t.Fatalf("refresh_token should be cleared when incoming has no refresh token")
	}
	if _, ok := merged["client_id"]; ok {
		t.Fatalf("client_id should be cleared when incoming has no refresh token")
	}
}

func buildCodexImportTestJWT(t *testing.T, exp time.Time, extraClaims map[string]any) string {
	t.Helper()
	header := map[string]any{
		"alg": "none",
		"typ": "JWT",
	}
	claims := map[string]any{
		"sub": "user-from-sub",
		"exp": exp.Unix(),
		"iat": time.Now().Unix(),
	}
	for k, v := range extraClaims {
		claims[k] = v
	}
	headerBytes, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	claimBytes, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(headerBytes) + "." + base64.RawURLEncoding.EncodeToString(claimBytes) + "."
}
