package service

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/Wei-Shaw/sub2api/internal/util/urlvalidator"
	"github.com/cespare/xxhash/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
)

const (
	// ChatGPT internal API for OAuth accounts
	chatgptCodexURL = "https://chatgpt.com/backend-api/codex/responses"
	// OpenAI Platform API for API Key accounts (fallback)
	openaiPlatformAPIURL    = "https://api.openai.com/v1/responses"
	openaiStickySessionTTL  = time.Hour // 粘性会话TTL
	codexCLIVersion         = "0.144.1"
	codexOSFingerprint      = "Mac OS 26.5.0; arm64"
	codexTerminalName       = "Apple_Terminal/470.2"
	codexOfficialOriginator = "codex-tui"
	codexCLIUserAgent       = codexOfficialOriginator + "/" + codexCLIVersion + " (" + codexOSFingerprint + ") " + codexTerminalName + " (" + codexOfficialOriginator + "; " + codexCLIVersion + ")"
	// codex_cli_only 拒绝时单个请求头日志长度上限（字符）
	codexCLIOnlyHeaderValueMaxBytes = 256

	// OpenAI WS Mode 失败后的重连次数上限（不含首次尝试）。
	// 与 Codex 客户端保持一致：失败后最多重连 5 次。
	openAIWSReconnectRetryLimit = 5
	// 上游错误体只需要提取错误 JSON/日志摘要，默认 512KiB 避免错误风暴叠加大请求体。
	openAIUpstreamErrorBodyReadLimit int64 = 512 << 10
	// APIKey retry classification has its own fixed, configuration-independent work bounds.
	openAIErrorClassificationMaxBytes  = 64 << 10
	openAIErrorClassificationMaxDepth  = 32
	openAIErrorClassificationMaxTokens = 4096
	// OpenAI WS Mode 重连退避默认值（可由配置覆盖）。
	openAIWSRetryBackoffInitialDefault = 120 * time.Millisecond
	openAIWSRetryBackoffMaxDefault     = 2 * time.Second
	openAIWSRetryJitterRatioDefault    = 0.2
	openAICompactSessionSeedKey        = "openai_compact_session_seed"
	openAICompactClientStreamKey       = "openai_compact_client_stream"
	openAICompactSSEKeepaliveKey       = "openai_compact_sse_keepalive"
	// Codex 限额快照仅用于后台展示/诊断，不需要每个成功请求都立即落库。
	openAICodexSnapshotPersistMinInterval = 30 * time.Second
	// 配额自动暂停时，超过该时长仍未刷新的 used% 快照视为陈旧，不再据此暂停账号。
	// 被暂停的账号收不到流量，其快照永远不会从上游响应头刷新；该兜底让账号在快照
	// 陈旧时放行一次请求，从而通过正常响应头自愈，而无需等待整个窗口（5h/7d）重置。
	openAICodexAutoPauseStaleAfter = 2 * time.Hour

	// 计费前硬拦截不可信 usage：阈值高于正常单次响应，避免异常 64 位 token
	// 直接造成巨额扣费，或后续写入 usage_logs int 列时溢出。
	openAIUsageBillingMaxTokenCount = 10_000_000
	openAIUsageBillingMaxActualCost = 100.0
)

const (
	openAICodexSessionIDHeader            = "session-id"
	openAICodexThreadIDHeader             = "thread-id"
	openAICodexClientRequestIDHeader      = "x-client-request-id"
	openAICodexInstallationIDHeader       = "x-codex-installation-id"
	openAICodexWindowIDHeader             = "x-codex-window-id"
	openAICodexParentThreadIDHeader       = "x-codex-parent-thread-id"
	openAICodexBetaFeaturesHeader         = "x-codex-beta-features"
	openAICodexTurnStateHeader            = "x-codex-turn-state"
	openAICodexTurnMetadataHeader         = "x-codex-turn-metadata"
	openAICodexSubagentHeader             = "x-openai-subagent"
	openAICodexMemgenRequestHeader        = "x-openai-memgen-request"
	openAICodexAttestationHeader          = "x-oai-attestation"
	openAICodexIncludeTimingMetricsHeader = "x-responsesapi-include-timing-metrics"
	openAICodexPrimaryUsedPercentHeader   = "x-codex-primary-used-percent"
	openAICodexPrimaryResetSecondsHeader  = "x-codex-primary-reset-after-seconds"
	openAICodexPrimaryWindowMinutesHeader = "x-codex-primary-window-minutes"
	openAICodexSecondUsedPercentHeader    = "x-codex-secondary-used-percent"
	openAICodexSecondResetSecondsHeader   = "x-codex-secondary-reset-after-seconds"
	openAICodexSecondWindowMinutesHeader  = "x-codex-secondary-window-minutes"
	openAICodexPrimaryOverSecondHeader    = "x-codex-primary-over-secondary-limit-percent"
	openAITraceparentHeader               = "traceparent"
	openAITracestateHeader                = "tracestate"
)

// OpenAI allowed headers whitelist (for non-passthrough).
var openaiAllowedHeaders = map[string]bool{
	"accept-language":                     true,
	"content-type":                        true,
	"conversation_id":                     true,
	"user-agent":                          true,
	"originator":                          true,
	"session_id":                          true,
	"version":                             true,
	openAICodexSessionIDHeader:            true,
	openAICodexThreadIDHeader:             true,
	openAICodexClientRequestIDHeader:      true,
	openAICodexInstallationIDHeader:       true,
	openAICodexWindowIDHeader:             true,
	openAICodexParentThreadIDHeader:       true,
	openAICodexSubagentHeader:             true,
	openAICodexMemgenRequestHeader:        true,
	openAICodexAttestationHeader:          true,
	openAICodexIncludeTimingMetricsHeader: true,
	openAITraceparentHeader:               true,
	openAITracestateHeader:                true,
	openAICodexTurnStateHeader:            true,
	openAICodexTurnMetadataHeader:         true,
}

// OpenAI passthrough allowed headers whitelist.
// 透传模式下仅放行这些低风险请求头，避免将非标准/环境噪声头传给上游触发风控。
var openaiPassthroughAllowedHeaders = map[string]bool{
	"accept":                              true,
	"accept-language":                     true,
	"content-type":                        true,
	"conversation_id":                     true,
	"openai-beta":                         true,
	"user-agent":                          true,
	"originator":                          true,
	"session_id":                          true,
	"version":                             true,
	openAICodexSessionIDHeader:            true,
	openAICodexThreadIDHeader:             true,
	openAICodexClientRequestIDHeader:      true,
	openAICodexInstallationIDHeader:       true,
	openAICodexWindowIDHeader:             true,
	openAICodexParentThreadIDHeader:       true,
	openAICodexSubagentHeader:             true,
	openAICodexMemgenRequestHeader:        true,
	openAICodexAttestationHeader:          true,
	openAICodexIncludeTimingMetricsHeader: true,
	openAITraceparentHeader:               true,
	openAITracestateHeader:                true,
	openAICodexTurnStateHeader:            true,
	openAICodexTurnMetadataHeader:         true,
}

// codex_cli_only 拒绝时记录的请求头白名单（仅用于诊断日志，不参与上游透传）
var codexCLIOnlyDebugHeaderWhitelist = []string{
	"User-Agent",
	"Content-Type",
	"Accept",
	"Accept-Language",
	"OpenAI-Beta",
	"Originator",
	"Session_ID",
	"Conversation_ID",
	"X-Request-ID",
	"X-Client-Request-ID",
	"X-Forwarded-For",
	"X-Real-IP",
}

// OpenAICodexUsageSnapshot represents Codex API usage limits from response headers
type OpenAICodexUsageSnapshot struct {
	PrimaryUsedPercent          *float64 `json:"primary_used_percent,omitempty"`
	PrimaryResetAfterSeconds    *int     `json:"primary_reset_after_seconds,omitempty"`
	PrimaryWindowMinutes        *int     `json:"primary_window_minutes,omitempty"`
	SecondaryUsedPercent        *float64 `json:"secondary_used_percent,omitempty"`
	SecondaryResetAfterSeconds  *int     `json:"secondary_reset_after_seconds,omitempty"`
	SecondaryWindowMinutes      *int     `json:"secondary_window_minutes,omitempty"`
	PrimaryOverSecondaryPercent *float64 `json:"primary_over_secondary_percent,omitempty"`
	UpdatedAt                   string   `json:"updated_at,omitempty"`
}

// NormalizedCodexLimits contains normalized 5h/7d rate limit data
type NormalizedCodexLimits struct {
	Used5hPercent   *float64
	Reset5hSeconds  *int
	Window5hMinutes *int
	Used7dPercent   *float64
	Reset7dSeconds  *int
	Window7dMinutes *int
}

// Normalize converts primary/secondary fields to canonical 5h/7d fields.
// Strategy: Compare window_minutes to determine which is 5h vs 7d.
// Returns nil if snapshot is nil or has no useful data.
func (s *OpenAICodexUsageSnapshot) Normalize() *NormalizedCodexLimits {
	if s == nil {
		return nil
	}

	result := &NormalizedCodexLimits{}

	primaryMins := 0
	secondaryMins := 0
	hasPrimaryWindow := false
	hasSecondaryWindow := false

	if s.PrimaryWindowMinutes != nil {
		primaryMins = *s.PrimaryWindowMinutes
		hasPrimaryWindow = true
	}
	if s.SecondaryWindowMinutes != nil {
		secondaryMins = *s.SecondaryWindowMinutes
		hasSecondaryWindow = true
	}

	// Determine mapping based on window_minutes
	use5hFromPrimary := false
	use7dFromPrimary := false

	if hasPrimaryWindow && hasSecondaryWindow {
		// Both known: smaller window is 5h, larger is 7d
		if primaryMins < secondaryMins {
			use5hFromPrimary = true
		} else {
			use7dFromPrimary = true
		}
	} else if hasPrimaryWindow {
		// Only primary known: classify by threshold (<=360 min = 6h -> 5h window)
		if primaryMins <= 360 {
			use5hFromPrimary = true
		} else {
			use7dFromPrimary = true
		}
	} else if hasSecondaryWindow {
		// Only secondary known: classify by threshold
		if secondaryMins <= 360 {
			// 5h from secondary, so primary (if any data) is 7d
			use7dFromPrimary = true
		} else {
			// 7d from secondary, so primary (if any data) is 5h
			use5hFromPrimary = true
		}
	} else {
		// No window_minutes: fall back to legacy assumption (primary=7d, secondary=5h)
		use7dFromPrimary = true
	}

	// Assign values
	if use5hFromPrimary {
		result.Used5hPercent = s.PrimaryUsedPercent
		result.Reset5hSeconds = s.PrimaryResetAfterSeconds
		result.Window5hMinutes = s.PrimaryWindowMinutes
		result.Used7dPercent = s.SecondaryUsedPercent
		result.Reset7dSeconds = s.SecondaryResetAfterSeconds
		result.Window7dMinutes = s.SecondaryWindowMinutes
	} else if use7dFromPrimary {
		result.Used7dPercent = s.PrimaryUsedPercent
		result.Reset7dSeconds = s.PrimaryResetAfterSeconds
		result.Window7dMinutes = s.PrimaryWindowMinutes
		result.Used5hPercent = s.SecondaryUsedPercent
		result.Reset5hSeconds = s.SecondaryResetAfterSeconds
		result.Window5hMinutes = s.SecondaryWindowMinutes
	}

	return result
}

// OpenAIUsage represents OpenAI API response usage
type OpenAIUsage struct {
	InputTokens              int `json:"input_tokens"`
	ImageInputTokens         int `json:"image_input_tokens,omitempty"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	ImageOutputTokens        int `json:"image_output_tokens,omitempty"`
}

// OpenAIForwardResult represents the result of forwarding
type OpenAIForwardResult struct {
	RequestID string
	// AttemptID identifies the admitted physical upstream request. It scopes
	// durable billing replay independently from the logical request correlation ID.
	AttemptID  string
	ResponseID string
	Usage      OpenAIUsage
	Model      string // 原始模型（用于响应和日志显示）
	// BillingModel is the model used for cost calculation.
	// When non-empty, CalculateCost uses this instead of Model.
	// This is set by the Anthropic Messages conversion path where
	// the mapped upstream model differs from the client-facing model.
	BillingModel string
	// UpstreamModel is the actual model sent to the upstream provider after mapping.
	// Empty when no mapping was applied (requested model was used as-is).
	UpstreamModel string
	// UpstreamEndpoint records the actual upstream API path when a service path
	// intentionally differs from the default endpoint derived by the handler.
	UpstreamEndpoint string
	// ServiceTier records the OpenAI Responses API service tier, e.g. "priority" / "flex".
	// Nil means the request did not specify a recognized tier.
	ServiceTier *string
	// ReasoningEffort is extracted from request body (reasoning.effort) or derived from model suffix.
	// Stored for usage records display; nil means not provided / not applicable.
	ReasoningEffort    *string
	Stream             bool
	OpenAIWSMode       bool
	ResponseHeaders    http.Header
	Duration           time.Duration
	FirstTokenMs       *int
	ClientDisconnect   bool
	ImageCount         int
	ImageSize          string
	ImageInputSize     string
	ImageOutputSize    string
	ImageOutputSizes   []string
	ImageSizeSource    string
	ImageSizeBreakdown map[string]int
	upstreamFact       *UpstreamErrorFact

	wsReplayInput       []json.RawMessage
	wsReplayInputExists bool
}

// UpstreamFact returns the observational terminal error fact without exposing
// the mutable attachment retained by the WebSocket forwarding path.
func (r *OpenAIForwardResult) UpstreamFact() (UpstreamErrorFact, bool) {
	if r == nil || r.upstreamFact == nil {
		return UpstreamErrorFact{}, false
	}
	return *r.upstreamFact, true
}

type OpenAIWSRetryMetricsSnapshot struct {
	RetryAttemptsTotal            int64 `json:"retry_attempts_total"`
	RetryBackoffMsTotal           int64 `json:"retry_backoff_ms_total"`
	RetryExhaustedTotal           int64 `json:"retry_exhausted_total"`
	NonRetryableFastFallbackTotal int64 `json:"non_retryable_fast_fallback_total"`
}

type OpenAICompatibilityFallbackMetricsSnapshot struct {
	SessionHashLegacyReadFallbackTotal int64   `json:"session_hash_legacy_read_fallback_total"`
	SessionHashLegacyReadFallbackHit   int64   `json:"session_hash_legacy_read_fallback_hit"`
	SessionHashLegacyDualWriteTotal    int64   `json:"session_hash_legacy_dual_write_total"`
	SessionHashLegacyReadHitRate       float64 `json:"session_hash_legacy_read_hit_rate"`

	MetadataLegacyFallbackIsMaxTokensOneHaikuTotal int64 `json:"metadata_legacy_fallback_is_max_tokens_one_haiku_total"`
	MetadataLegacyFallbackThinkingEnabledTotal     int64 `json:"metadata_legacy_fallback_thinking_enabled_total"`
	MetadataLegacyFallbackPrefetchedStickyAccount  int64 `json:"metadata_legacy_fallback_prefetched_sticky_account_total"`
	MetadataLegacyFallbackPrefetchedStickyGroup    int64 `json:"metadata_legacy_fallback_prefetched_sticky_group_total"`
	MetadataLegacyFallbackSingleAccountRetryTotal  int64 `json:"metadata_legacy_fallback_single_account_retry_total"`
	MetadataLegacyFallbackAccountSwitchCountTotal  int64 `json:"metadata_legacy_fallback_account_switch_count_total"`
	MetadataLegacyFallbackTotal                    int64 `json:"metadata_legacy_fallback_total"`
}

type openAIWSRetryMetrics struct {
	retryAttempts            atomic.Int64
	retryBackoffMs           atomic.Int64
	retryExhausted           atomic.Int64
	nonRetryableFastFallback atomic.Int64
}

type accountWriteThrottle struct {
	minInterval time.Duration
	mu          sync.Mutex
	lastByID    map[int64]time.Time
}

func newAccountWriteThrottle(minInterval time.Duration) *accountWriteThrottle {
	return &accountWriteThrottle{
		minInterval: minInterval,
		lastByID:    make(map[int64]time.Time),
	}
}

func (t *accountWriteThrottle) Allow(id int64, now time.Time) bool {
	if t == nil || id <= 0 || t.minInterval <= 0 {
		return true
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if last, ok := t.lastByID[id]; ok && now.Sub(last) < t.minInterval {
		return false
	}
	t.lastByID[id] = now

	if len(t.lastByID) > 4096 {
		cutoff := now.Add(-4 * t.minInterval)
		for accountID, writtenAt := range t.lastByID {
			if writtenAt.Before(cutoff) {
				delete(t.lastByID, accountID)
			}
		}
	}

	return true
}

var defaultOpenAICodexSnapshotPersistThrottle = newAccountWriteThrottle(openAICodexSnapshotPersistMinInterval)

// ErrNoAvailableCompactAccounts indicates the request needs /responses/compact
// support but no compatible account is available.
var ErrNoAvailableCompactAccounts = errors.New("no available OpenAI accounts support /responses/compact")

// OpenAIGatewayService handles OpenAI API gateway operations
type OpenAIGatewayService struct {
	accountRepo             AccountRepository
	usageLogRepo            UsageLogRepository
	usageBillingRepo        UsageBillingRepository
	userRepo                UserRepository
	userSubRepo             UserSubscriptionRepository
	cache                   GatewayCache
	cfg                     *config.Config
	codexDetector           CodexClientRestrictionDetector
	schedulerSnapshot       *SchedulerSnapshotService
	concurrencyService      *ConcurrencyService
	billingService          *BillingService
	rateLimitService        *RateLimitService
	billingCacheService     *BillingCacheService
	userGroupRateResolver   *userGroupRateResolver
	httpUpstream            HTTPUpstream
	deferredService         *DeferredService
	openAITokenProvider     *OpenAITokenProvider
	toolCorrector           *CodexToolCorrector
	openaiWSResolver        OpenAIWSProtocolResolver
	resolver                *ModelPricingResolver
	channelService          *ChannelService
	balanceNotifyService    *BalanceNotifyService
	settingService          *SettingService
	codexFingerprintService *OpenAICodexFingerprintService
	userPlatformQuotaRepo   UserPlatformQuotaRepository
	billingOutboxRepo       BillingOutboxRepository

	agentIdentityTaskMu           sync.Mutex
	openaiWSPoolOnce              sync.Once
	openaiWSStateStoreOnce        sync.Once
	openaiSchedulerOnce           sync.Once
	openaiWSPassthroughDialerOnce sync.Once
	openaiWSPool                  *openAIWSConnPool
	openaiWSStateStore            OpenAIWSStateStore
	openaiScheduler               OpenAIAccountScheduler
	openaiWSPassthroughDialer     openAIWSClientDialer
	openaiAccountStats            *openAIAccountRuntimeStats

	openaiWSFallbackUntil               sync.Map // key: int64(accountID), value: time.Time
	openaiAccountRuntimeBlockUntil      sync.Map // key: int64(accountID), value: time.Time
	openaiOAuth429WindowStartUnixNano   atomic.Int64
	openaiOAuth429WindowCount           atomic.Int64
	openaiWSRetryMetrics                openAIWSRetryMetrics
	responseHeaderFilter                *responseheaders.CompiledHeaderFilter
	codexSnapshotThrottle               *accountWriteThrottle
	openaiCompatSessionResponses        sync.Map
	openaiCompatAnthropicDigestSessions sync.Map
	classifyContextWindowError          func([]byte) openAIContextWindowMatch
}

// NewOpenAIGatewayService creates a new OpenAIGatewayService
func NewOpenAIGatewayService(
	accountRepo AccountRepository,
	usageLogRepo UsageLogRepository,
	usageBillingRepo UsageBillingRepository,
	userRepo UserRepository,
	userSubRepo UserSubscriptionRepository,
	userGroupRateRepo UserGroupRateRepository,
	cache GatewayCache,
	cfg *config.Config,
	schedulerSnapshot *SchedulerSnapshotService,
	concurrencyService *ConcurrencyService,
	billingService *BillingService,
	rateLimitService *RateLimitService,
	billingCacheService *BillingCacheService,
	httpUpstream HTTPUpstream,
	deferredService *DeferredService,
	openAITokenProvider *OpenAITokenProvider,
	resolver *ModelPricingResolver,
	channelService *ChannelService,
	balanceNotifyService *BalanceNotifyService,
	settingService *SettingService,
	userPlatformQuotaRepo UserPlatformQuotaRepository,
) *OpenAIGatewayService {
	svc := &OpenAIGatewayService{
		accountRepo:         accountRepo,
		usageLogRepo:        usageLogRepo,
		usageBillingRepo:    usageBillingRepo,
		userRepo:            userRepo,
		userSubRepo:         userSubRepo,
		cache:               cache,
		cfg:                 cfg,
		codexDetector:       NewOpenAICodexClientRestrictionDetector(cfg),
		schedulerSnapshot:   schedulerSnapshot,
		concurrencyService:  concurrencyService,
		billingService:      billingService,
		rateLimitService:    rateLimitService,
		billingCacheService: billingCacheService,
		userGroupRateResolver: newUserGroupRateResolver(
			userGroupRateRepo,
			nil,
			resolveUserGroupRateCacheTTL(cfg),
			nil,
			"service.openai_gateway",
		),
		httpUpstream:            httpUpstream,
		deferredService:         deferredService,
		openAITokenProvider:     openAITokenProvider,
		toolCorrector:           NewCodexToolCorrector(),
		openaiWSResolver:        NewOpenAIWSProtocolResolver(cfg),
		resolver:                resolver,
		channelService:          channelService,
		balanceNotifyService:    balanceNotifyService,
		settingService:          settingService,
		codexFingerprintService: NewOpenAICodexFingerprintService(accountRepo, settingService),
		userPlatformQuotaRepo:   userPlatformQuotaRepo,
		billingOutboxRepo:       nil,
		responseHeaderFilter:    compileResponseHeaderFilter(cfg),
		codexSnapshotThrottle:   newAccountWriteThrottle(openAICodexSnapshotPersistMinInterval),
	}
	if rateLimitService != nil {
		rateLimitService.SetAccountRuntimeBlocker(svc)
	}
	if openAITokenProvider != nil {
		openAITokenProvider.SetAccountRuntimeBlocker(svc)
	}
	svc.logOpenAIWSModeBootstrap()
	return svc
}

// SetBillingOutboxRepository injects durable billing command storage after the
// gateway's existing constructor has completed.
func (s *OpenAIGatewayService) SetBillingOutboxRepository(repo BillingOutboxRepository) {
	if s != nil {
		s.billingOutboxRepo = repo
	}
}

// ResolveChannelMapping 解析渠道级模型映射（代理到 ChannelService）
func (s *OpenAIGatewayService) ResolveChannelMapping(ctx context.Context, groupID int64, model string) ChannelMappingResult {
	if s.channelService == nil {
		return ChannelMappingResult{MappedModel: model}
	}
	return s.channelService.ResolveChannelMapping(ctx, groupID, model)
}

// IsModelRestricted 检查模型是否被渠道限制（代理到 ChannelService）
func (s *OpenAIGatewayService) IsModelRestricted(ctx context.Context, groupID int64, model string) bool {
	if s.channelService == nil {
		return false
	}
	return s.channelService.IsModelRestricted(ctx, groupID, model)
}

// ResolveChannelMappingAndRestrict 解析渠道映射。
// 模型限制检查已移至调度阶段，restricted 始终返回 false。
func (s *OpenAIGatewayService) ResolveChannelMappingAndRestrict(ctx context.Context, groupID *int64, model string) (ChannelMappingResult, bool) {
	if s.channelService == nil {
		return ChannelMappingResult{MappedModel: model}, false
	}
	return s.channelService.ResolveChannelMappingAndRestrict(ctx, groupID, model)
}

func (s *OpenAIGatewayService) isCodexImageGenerationBridgeEnabled(ctx context.Context, account *Account, apiKey *APIKey) bool {
	if override := account.CodexImageGenerationBridgeOverride(); override != nil {
		return *override
	}
	if s != nil && s.channelService != nil && apiKey != nil && apiKey.GroupID != nil {
		ch, err := s.channelService.GetChannelForGroup(ctx, *apiKey.GroupID)
		if err != nil {
			slog.Warn("failed to resolve codex image generation bridge channel override", "group_id", *apiKey.GroupID, "error", err)
		} else if override := ch.CodexImageGenerationBridgeOverride(PlatformOpenAI); override != nil {
			return *override
		}
	}
	return s != nil && s.cfg != nil && s.cfg.Gateway.CodexImageGenerationBridgeEnabled
}

func (s *OpenAIGatewayService) checkChannelPricingRestriction(ctx context.Context, groupID *int64, requestedModel string) bool {
	if groupID == nil || s.channelService == nil || requestedModel == "" {
		return false
	}
	mapping := s.channelService.ResolveChannelMapping(ctx, *groupID, requestedModel)
	billingModel := billingModelForRestriction(mapping.BillingModelSource, requestedModel, mapping.MappedModel)
	if billingModel == "" {
		return false
	}
	return s.channelService.IsModelRestricted(ctx, *groupID, billingModel)
}

func (s *OpenAIGatewayService) isUpstreamModelRestrictedByChannel(ctx context.Context, groupID int64, account *Account, requestedModel string, requireCompact bool) bool {
	if s.channelService == nil {
		return false
	}
	upstreamModel := resolveOpenAIAccountUpstreamModelForRequest(account, requestedModel, requireCompact)
	if upstreamModel == "" {
		return false
	}
	return s.channelService.IsModelRestricted(ctx, groupID, upstreamModel)
}

func (s *OpenAIGatewayService) needsUpstreamChannelRestrictionCheck(ctx context.Context, groupID *int64) bool {
	if groupID == nil || s.channelService == nil {
		return false
	}
	ch, err := s.channelService.GetChannelForGroup(ctx, *groupID)
	if err != nil {
		slog.Warn("failed to check openai channel upstream restriction", "group_id", *groupID, "error", err)
		return false
	}
	if ch == nil || !ch.RestrictModels {
		return false
	}
	return ch.BillingModelSource == BillingModelSourceUpstream
}

// ReplaceModelInBody 替换请求体中的 JSON model 字段（通用 gjson/sjson 实现）。
func (s *OpenAIGatewayService) ReplaceModelInBody(body []byte, newModel string) []byte {
	return ReplaceModelInBody(body, newModel)
}

func (s *OpenAIGatewayService) getCodexSnapshotThrottle() *accountWriteThrottle {
	if s != nil && s.codexSnapshotThrottle != nil {
		return s.codexSnapshotThrottle
	}
	return defaultOpenAICodexSnapshotPersistThrottle
}

func (s *OpenAIGatewayService) billingDeps() *billingDeps {
	return &billingDeps{
		accountRepo:           s.accountRepo,
		userRepo:              s.userRepo,
		userSubRepo:           s.userSubRepo,
		billingCacheService:   s.billingCacheService,
		deferredService:       s.deferredService,
		balanceNotifyService:  s.balanceNotifyService,
		userPlatformQuotaRepo: s.userPlatformQuotaRepo,
	}
}

// CloseOpenAIWSPool 关闭 OpenAI WebSocket 连接池的后台 worker 和空闲连接。
// 应在应用优雅关闭时调用。
func (s *OpenAIGatewayService) CloseOpenAIWSPool() {
	if s != nil && s.openaiWSPool != nil {
		s.openaiWSPool.Close()
	}
}

func (s *OpenAIGatewayService) logOpenAIWSModeBootstrap() {
	if s == nil || s.cfg == nil {
		return
	}
	wsCfg := s.cfg.Gateway.OpenAIWS
	logOpenAIWSModeInfo(
		"bootstrap enabled=%v oauth_enabled=%v apikey_enabled=%v force_http=%v responses_websockets_v2=%v responses_websockets=%v payload_log_sample_rate=%.3f event_flush_batch_size=%d event_flush_interval_ms=%d prewarm_cooldown_ms=%d retry_backoff_initial_ms=%d retry_backoff_max_ms=%d retry_jitter_ratio=%.3f retry_total_budget_ms=%d ws_read_limit_bytes=%d",
		wsCfg.Enabled,
		wsCfg.OAuthEnabled,
		wsCfg.APIKeyEnabled,
		wsCfg.ForceHTTP,
		wsCfg.ResponsesWebsocketsV2,
		wsCfg.ResponsesWebsockets,
		wsCfg.PayloadLogSampleRate,
		wsCfg.EventFlushBatchSize,
		wsCfg.EventFlushIntervalMS,
		wsCfg.PrewarmCooldownMS,
		wsCfg.RetryBackoffInitialMS,
		wsCfg.RetryBackoffMaxMS,
		wsCfg.RetryJitterRatio,
		wsCfg.RetryTotalBudgetMS,
		openAIWSMessageReadLimitBytes,
	)
}

func (s *OpenAIGatewayService) getCodexClientRestrictionDetector() CodexClientRestrictionDetector {
	if s != nil && s.codexDetector != nil {
		return s.codexDetector
	}
	var cfg *config.Config
	if s != nil {
		cfg = s.cfg
	}
	return NewOpenAICodexClientRestrictionDetector(cfg)
}

func (s *OpenAIGatewayService) getOpenAIWSProtocolResolver() OpenAIWSProtocolResolver {
	if s != nil && s.openaiWSResolver != nil {
		return s.openaiWSResolver
	}
	var cfg *config.Config
	if s != nil {
		cfg = s.cfg
	}
	return NewOpenAIWSProtocolResolver(cfg)
}

func classifyOpenAIWSReconnectReason(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	var fallbackErr *openAIWSFallbackError
	if !errors.As(err, &fallbackErr) || fallbackErr == nil {
		return "", false
	}
	reason := strings.TrimSpace(fallbackErr.Reason)
	if reason == "" {
		return "", false
	}

	baseReason := strings.TrimPrefix(reason, "prewarm_")

	switch baseReason {
	case "policy_violation",
		"message_too_big",
		"upgrade_required",
		"ws_unsupported",
		"auth_failed",
		"invalid_encrypted_content",
		"previous_response_not_found":
		return reason, false
	}

	switch baseReason {
	case "read_event",
		"write_request",
		"write",
		"acquire_timeout",
		"acquire_conn",
		"conn_queue_full",
		"dial_failed",
		"upstream_5xx",
		"event_error",
		"error_event",
		"upstream_error_event",
		"ws_connection_limit_reached",
		"missing_final_response":
		return reason, true
	default:
		return reason, false
	}
}

func resolveOpenAIWSFallbackErrorResponse(err error) (statusCode int, errType string, clientMessage string, upstreamMessage string, ok bool) {
	if err == nil {
		return 0, "", "", "", false
	}
	var fallbackErr *openAIWSFallbackError
	if !errors.As(err, &fallbackErr) || fallbackErr == nil {
		return 0, "", "", "", false
	}

	reason := strings.TrimSpace(fallbackErr.Reason)
	reason = strings.TrimPrefix(reason, "prewarm_")
	if reason == "" {
		return 0, "", "", "", false
	}

	var dialErr *openAIWSDialError
	if fallbackErr.Err != nil && errors.As(fallbackErr.Err, &dialErr) && dialErr != nil {
		if dialErr.StatusCode > 0 {
			statusCode = dialErr.StatusCode
		}
		if dialErr.Err != nil {
			upstreamMessage = sanitizeOpenAIUpstreamDiagnosticText(strings.TrimSpace(dialErr.Err.Error()))
		}
	}

	switch reason {
	case "invalid_encrypted_content":
		if statusCode == 0 {
			statusCode = http.StatusBadRequest
		}
		errType = "invalid_request_error"
		if upstreamMessage == "" {
			upstreamMessage = "encrypted content could not be verified"
		}
	case "previous_response_not_found":
		if statusCode == 0 {
			statusCode = http.StatusBadRequest
		}
		errType = "invalid_request_error"
		if upstreamMessage == "" {
			upstreamMessage = "previous response not found"
		}
	case "upgrade_required":
		if statusCode == 0 {
			statusCode = http.StatusUpgradeRequired
		}
	case "ws_unsupported":
		if statusCode == 0 {
			statusCode = http.StatusBadRequest
		}
	case "auth_failed":
		if statusCode == 0 {
			statusCode = http.StatusUnauthorized
		}
	case "upstream_rate_limited":
		if statusCode == 0 {
			statusCode = http.StatusTooManyRequests
		}
	default:
		if statusCode == 0 {
			return 0, "", "", "", false
		}
	}

	if upstreamMessage == "" && fallbackErr.Err != nil {
		upstreamMessage = sanitizeOpenAIUpstreamDiagnosticText(strings.TrimSpace(fallbackErr.Err.Error()))
	}
	if upstreamMessage == "" {
		switch reason {
		case "upgrade_required":
			upstreamMessage = "upstream websocket upgrade required"
		case "ws_unsupported":
			upstreamMessage = "upstream websocket not supported"
		case "auth_failed":
			upstreamMessage = "upstream authentication failed"
		case "upstream_rate_limited":
			upstreamMessage = "upstream rate limit exceeded, please retry later"
		default:
			upstreamMessage = "Upstream request failed"
		}
	}

	if errType == "" {
		if statusCode == http.StatusTooManyRequests {
			errType = "rate_limit_error"
		} else {
			errType = "upstream_error"
		}
	}
	clientMessage = upstreamMessage
	return statusCode, errType, clientMessage, upstreamMessage, true
}

func (s *OpenAIGatewayService) writeOpenAIWSFallbackErrorResponse(c *gin.Context, account *Account, wsErr error) bool {
	if c == nil || c.Writer == nil || c.Writer.Written() {
		return false
	}
	statusCode, errType, clientMessage, upstreamMessage, ok := resolveOpenAIWSFallbackErrorResponse(wsErr)
	if !ok {
		return false
	}
	if strings.TrimSpace(clientMessage) == "" {
		clientMessage = "Upstream request failed"
	}
	if strings.TrimSpace(upstreamMessage) == "" {
		upstreamMessage = clientMessage
	}

	upstreamURL := ""
	upstreamEndpoint := ""
	var fallbackErr *openAIWSFallbackError
	if errors.As(wsErr, &fallbackErr) && fallbackErr != nil {
		upstreamURL = safeUpstreamURL(fallbackErr.UpstreamURL)
		upstreamEndpoint = strings.TrimSpace(fallbackErr.UpstreamEndpoint)
	}

	setOpsUpstreamError(c, statusCode, upstreamMessage, "")
	if account != nil {
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: statusCode,
			UpstreamURL:        upstreamURL,
			UpstreamEndpoint:   upstreamEndpoint,
			Kind:               "ws_error",
			Message:            upstreamMessage,
		})
	}
	c.JSON(statusCode, gin.H{
		"error": gin.H{
			"type":    errType,
			"message": clientMessage,
		},
	})
	return true
}

func (s *OpenAIGatewayService) openAIWSRetryBackoff(attempt int) time.Duration {
	if attempt <= 0 {
		return 0
	}

	initial := openAIWSRetryBackoffInitialDefault
	maxBackoff := openAIWSRetryBackoffMaxDefault
	jitterRatio := openAIWSRetryJitterRatioDefault
	if s != nil && s.cfg != nil {
		wsCfg := s.cfg.Gateway.OpenAIWS
		if wsCfg.RetryBackoffInitialMS > 0 {
			initial = time.Duration(wsCfg.RetryBackoffInitialMS) * time.Millisecond
		}
		if wsCfg.RetryBackoffMaxMS > 0 {
			maxBackoff = time.Duration(wsCfg.RetryBackoffMaxMS) * time.Millisecond
		}
		if wsCfg.RetryJitterRatio >= 0 {
			jitterRatio = wsCfg.RetryJitterRatio
		}
	}
	if initial <= 0 {
		return 0
	}
	if maxBackoff <= 0 {
		maxBackoff = initial
	}
	if maxBackoff < initial {
		maxBackoff = initial
	}
	if jitterRatio < 0 {
		jitterRatio = 0
	}
	if jitterRatio > 1 {
		jitterRatio = 1
	}

	shift := attempt - 1
	if shift < 0 {
		shift = 0
	}
	backoff := initial
	if shift > 0 {
		backoff = initial * time.Duration(1<<shift)
	}
	if backoff > maxBackoff {
		backoff = maxBackoff
	}
	if jitterRatio <= 0 {
		return backoff
	}
	jitter := time.Duration(float64(backoff) * jitterRatio)
	if jitter <= 0 {
		return backoff
	}
	delta := time.Duration(rand.Int63n(int64(jitter)*2+1)) - jitter
	withJitter := backoff + delta
	if withJitter < 0 {
		return 0
	}
	return withJitter
}

func (s *OpenAIGatewayService) openAIWSRetryTotalBudget() time.Duration {
	if s != nil && s.cfg != nil {
		ms := s.cfg.Gateway.OpenAIWS.RetryTotalBudgetMS
		if ms <= 0 {
			return 0
		}
		return time.Duration(ms) * time.Millisecond
	}
	return 0
}

func (s *OpenAIGatewayService) recordOpenAIWSRetryAttempt(backoff time.Duration) {
	if s == nil {
		return
	}
	s.openaiWSRetryMetrics.retryAttempts.Add(1)
	if backoff > 0 {
		s.openaiWSRetryMetrics.retryBackoffMs.Add(backoff.Milliseconds())
	}
}

func (s *OpenAIGatewayService) recordOpenAIWSRetryExhausted() {
	if s == nil {
		return
	}
	s.openaiWSRetryMetrics.retryExhausted.Add(1)
}

func (s *OpenAIGatewayService) recordOpenAIWSNonRetryableFastFallback() {
	if s == nil {
		return
	}
	s.openaiWSRetryMetrics.nonRetryableFastFallback.Add(1)
}

func (s *OpenAIGatewayService) SnapshotOpenAIWSRetryMetrics() OpenAIWSRetryMetricsSnapshot {
	if s == nil {
		return OpenAIWSRetryMetricsSnapshot{}
	}
	return OpenAIWSRetryMetricsSnapshot{
		RetryAttemptsTotal:            s.openaiWSRetryMetrics.retryAttempts.Load(),
		RetryBackoffMsTotal:           s.openaiWSRetryMetrics.retryBackoffMs.Load(),
		RetryExhaustedTotal:           s.openaiWSRetryMetrics.retryExhausted.Load(),
		NonRetryableFastFallbackTotal: s.openaiWSRetryMetrics.nonRetryableFastFallback.Load(),
	}
}

func SnapshotOpenAICompatibilityFallbackMetrics() OpenAICompatibilityFallbackMetricsSnapshot {
	legacyReadFallbackTotal, legacyReadFallbackHit, legacyDualWriteTotal := openAIStickyCompatStats()
	isMaxTokensOneHaiku, thinkingEnabled, prefetchedStickyAccount, prefetchedStickyGroup, singleAccountRetry, accountSwitchCount := RequestMetadataFallbackStats()

	readHitRate := float64(0)
	if legacyReadFallbackTotal > 0 {
		readHitRate = float64(legacyReadFallbackHit) / float64(legacyReadFallbackTotal)
	}
	metadataFallbackTotal := isMaxTokensOneHaiku + thinkingEnabled + prefetchedStickyAccount + prefetchedStickyGroup + singleAccountRetry + accountSwitchCount

	return OpenAICompatibilityFallbackMetricsSnapshot{
		SessionHashLegacyReadFallbackTotal: legacyReadFallbackTotal,
		SessionHashLegacyReadFallbackHit:   legacyReadFallbackHit,
		SessionHashLegacyDualWriteTotal:    legacyDualWriteTotal,
		SessionHashLegacyReadHitRate:       readHitRate,

		MetadataLegacyFallbackIsMaxTokensOneHaikuTotal: isMaxTokensOneHaiku,
		MetadataLegacyFallbackThinkingEnabledTotal:     thinkingEnabled,
		MetadataLegacyFallbackPrefetchedStickyAccount:  prefetchedStickyAccount,
		MetadataLegacyFallbackPrefetchedStickyGroup:    prefetchedStickyGroup,
		MetadataLegacyFallbackSingleAccountRetryTotal:  singleAccountRetry,
		MetadataLegacyFallbackAccountSwitchCountTotal:  accountSwitchCount,
		MetadataLegacyFallbackTotal:                    metadataFallbackTotal,
	}
}

func (s *OpenAIGatewayService) detectCodexClientRestriction(c *gin.Context, account *Account) CodexClientRestrictionDetectionResult {
	var globalAllowedClients []string
	if account != nil && account.IsCodexCLIOnlyEnabled() && s != nil && s.settingService != nil {
		ctx := context.Background()
		if c != nil && c.Request != nil {
			ctx = c.Request.Context()
		}
		if s.settingService.IsOpenAIAllowClaudeCodeCodexPluginEnabled(ctx) {
			globalAllowedClients = []string{openai.AllowedClientClaudeCode}
		}
	}
	return s.getCodexClientRestrictionDetector().Detect(c, account, globalAllowedClients)
}

func getAPIKeyIDFromContext(c *gin.Context) int64 {
	if c == nil {
		return 0
	}
	v, exists := c.Get("api_key")
	if !exists {
		return 0
	}
	apiKey, ok := v.(*APIKey)
	if !ok || apiKey == nil {
		return 0
	}
	return apiKey.ID
}

// isolateOpenAISessionID 将 apiKeyID 混入 legacy session 标识符。
// OAuth-like Codex upstream identity must use isolateOpenAICodexOAuthSessionID instead,
// because upstream session/thread ids need to stay UUID-shaped.
func isolateOpenAISessionID(apiKeyID int64, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	h := xxhash.New()
	_, _ = fmt.Fprintf(h, "k%d:", apiKeyID)
	_, _ = h.WriteString(raw)
	return fmt.Sprintf("%016x", h.Sum64())
}

func isolateOpenAICodexOAuthSessionID(apiKeyID int64, raw string, purpose string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	purpose = safeOpenAICodexUAComponent(purpose, "session")
	return generateOpenAICodexDeterministicUUIDV7(fmt.Sprintf("openai-codex-oauth:%s:%d:%s", purpose, apiKeyID, raw))
}

func generateOpenAICodexDeterministicUUIDV7(seed string) string {
	if strings.TrimSpace(seed) == "" {
		return newOpenAICodexUUID()
	}
	hash := sha256.Sum256([]byte(seed))
	bytes := hash[:16]
	bytes[6] = (bytes[6] & 0x0f) | 0x70
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
}

func newOpenAICodexUUID() string {
	id, err := uuid.NewV7()
	if err == nil {
		return id.String()
	}
	return uuid.NewString()
}

type openAICodexRequestIdentity struct {
	SessionID       string
	ThreadID        string
	ClientRequestID string
	InstallationID  string
	WindowID        string
}

func applyOpenAICodexHTTPRequestAlignment(req *http.Request, c *gin.Context, account *Account, body []byte, promptCacheKey string, allowLegacyConversationID bool) ([]byte, openAICodexRequestIdentity) {
	return applyOpenAICodexHTTPRequestAlignmentWithBody(req, c, account, body, promptCacheKey, allowLegacyConversationID, true)
}

func applyOpenAICodexHTTPRequestAlignmentWithBody(req *http.Request, c *gin.Context, account *Account, body []byte, promptCacheKey string, allowLegacyConversationID bool, mutateBody bool) ([]byte, openAICodexRequestIdentity) {
	return applyOpenAICodexHTTPRequestAlignmentWithBodyOptions(req, c, account, body, promptCacheKey, allowLegacyConversationID, mutateBody, mutateBody)
}

func applyOpenAICodexHTTPRequestAlignmentWithBodyOptions(req *http.Request, c *gin.Context, account *Account, body []byte, promptCacheKey string, allowLegacyConversationID bool, mutatePromptCacheKey bool, mutateClientMetadata bool) ([]byte, openAICodexRequestIdentity) {
	identity := resolveOpenAICodexRequestIdentity(req, c, account, body, promptCacheKey)
	applyOpenAICodexIdentityHeaders(req, c, identity, allowLegacyConversationID)
	if mutatePromptCacheKey {
		body = setOpenAICodexHTTPPromptCacheKey(body, identity)
	}
	if mutateClientMetadata {
		body = setOpenAICodexHTTPClientMetadata(body, identity)
	}
	resetHTTPRequestBody(req, body)
	return body, identity
}

func resolveOpenAICodexRequestIdentity(req *http.Request, c *gin.Context, account *Account, body []byte, promptCacheKey string) openAICodexRequestIdentity {
	apiKeyID := getAPIKeyIDFromContext(c)
	codexSessionID := openAIHeaderValue(req, c, openAICodexSessionIDHeader)
	codexThreadID := openAIHeaderValue(req, c, openAICodexThreadIDHeader)
	legacySessionID := openAIHeaderValue(req, c, "session_id")
	legacyConversationID := openAIHeaderValue(req, c, "conversation_id")
	rawSessionID := firstNonEmptyOpenAICodexString(codexSessionID, legacySessionID, legacyConversationID, promptCacheKey, bodyPromptCacheKey(body))
	if rawSessionID == "" {
		rawSessionID = fallbackOpenAICodexSessionID(c, account, body)
	}
	rawThreadID := firstNonEmptyOpenAICodexString(codexThreadID, legacyConversationID, rawSessionID)

	sessionID := rawSessionID
	threadID := rawThreadID
	if account != nil && account.IsOpenAIOAuthLike() {
		sessionID = isolateOpenAICodexOAuthSessionID(apiKeyID, sessionID, "session")
		threadID = isolateOpenAICodexOAuthSessionID(apiKeyID, threadID, "thread")
	}
	if threadID == "" {
		threadID = sessionID
	}

	clientRequestID := firstNonEmptyOpenAICodexString(openAIHeaderValue(req, c, openAICodexClientRequestIDHeader), threadID, sessionID)
	if account != nil && account.IsOpenAIOAuthLike() {
		clientRequestID = threadID
	}
	if clientRequestID == "" {
		clientRequestID = newOpenAICodexUUID()
	}
	installationID := firstNonEmptyOpenAICodexString(openAIHeaderValue(req, c, openAICodexInstallationIDHeader), deterministicOpenAICodexInstallationID(c, account))
	if account != nil && account.IsOpenAIOAuthLike() {
		installationID = ""
		if fp, ok := coerceOpenAICodexFingerprint(account.Extra[OpenAICodexFingerprintExtraKey]); ok {
			installationID, _ = canonicalOpenAICodexInstallationID(fp.InstallationID)
		}
		if installationID == "" && req != nil {
			if fp, ok := openAICodexFingerprintFromContext(req.Context()); ok {
				installationID, _ = canonicalOpenAICodexInstallationID(fp.InstallationID)
			}
		}
	}
	windowGeneration := resolveOpenAICodexWindowGeneration(req, c, body, account != nil && account.IsOpenAIOAuthLike())
	windowID := openAIHeaderValue(req, c, openAICodexWindowIDHeader)
	if account != nil && account.IsOpenAIOAuthLike() {
		windowID = ""
	}
	if threadID != "" && (windowID == "" || account != nil && account.IsOpenAIOAuthLike()) {
		windowID = threadID + ":" + windowGeneration
	}

	return openAICodexRequestIdentity{
		SessionID:       sessionID,
		ThreadID:        threadID,
		ClientRequestID: clientRequestID,
		InstallationID:  installationID,
		WindowID:        windowID,
	}
}

func applyOpenAICodexIdentityHeaders(req *http.Request, c *gin.Context, identity openAICodexRequestIdentity, allowLegacyConversationID bool) {
	if req == nil {
		return
	}
	copyOpenAICodexOptionalHeaders(req.Header, c)
	if identity.SessionID != "" {
		req.Header.Set(openAICodexSessionIDHeader, identity.SessionID)
		req.Header.Set("session_id", identity.SessionID)
	}
	if identity.ThreadID != "" {
		req.Header.Set(openAICodexThreadIDHeader, identity.ThreadID)
		if allowLegacyConversationID {
			req.Header.Set("conversation_id", identity.ThreadID)
		}
	}
	if identity.ClientRequestID != "" {
		req.Header.Set(openAICodexClientRequestIDHeader, identity.ClientRequestID)
	}
	if identity.InstallationID != "" {
		req.Header.Set(openAICodexInstallationIDHeader, identity.InstallationID)
	}
	if identity.WindowID != "" {
		req.Header.Set(openAICodexWindowIDHeader, identity.WindowID)
	}
}

func setOpenAICodexHTTPPromptCacheKey(body []byte, identity openAICodexRequestIdentity) []byte {
	if len(bytes.TrimSpace(body)) == 0 || identity.ThreadID == "" {
		return body
	}
	next, err := sjson.SetBytes(body, "prompt_cache_key", identity.ThreadID)
	if err != nil {
		return body
	}
	return next
}

func setOpenAICodexHTTPClientMetadata(body []byte, identity openAICodexRequestIdentity) []byte {
	// 对齐 Codex build_responses_request：HTTP 仅注入 x-codex-installation-id。
	if len(bytes.TrimSpace(body)) == 0 {
		return body
	}
	updated, err := stripOpenAICodexClientMetadataIdentityRaw(body)
	if err != nil {
		return body
	}
	if identity.InstallationID == "" {
		return updated
	}
	next, err := sjson.SetBytes(updated, openAICodexClientMetadataPath(openAICodexInstallationIDHeader), identity.InstallationID)
	if err != nil {
		return body
	}
	return next
}

func openAICodexClientMetadataIdentityKeys() []string {
	return []string{
		openAICodexInstallationIDHeader,
		openAICodexWindowIDHeader,
		openAICodexSessionIDHeader,
		openAICodexThreadIDHeader,
		openAICodexClientRequestIDHeader,
		openAICodexParentThreadIDHeader,
		openAICodexTurnStateHeader,
		openAICodexTurnMetadataHeader,
		openAICodexSubagentHeader,
		openAICodexBetaFeaturesHeader,
		openAICodexMemgenRequestHeader,
		openAICodexAttestationHeader,
		openAICodexIncludeTimingMetricsHeader,
		openAITraceparentHeader,
		openAITracestateHeader,
		openAICodexWSTraceparentMetadataKey,
		openAICodexWSTracestateMetadataKey,
		openAICodexWSStreamRequestStartMSKey,
		"session_id",
		"conversation_id",
		"prompt_cache_key",
	}
}

func openAICodexClientMetadataIdentityKeyPrefixes() []string {
	return []string{"ws_request_header_"}
}

func stripOpenAICodexClientMetadataIdentity(metadata map[string]any) {
	if metadata == nil {
		return
	}
	for _, key := range openAICodexClientMetadataIdentityKeys() {
		delete(metadata, key)
	}
	for key := range metadata {
		for _, prefix := range openAICodexClientMetadataIdentityKeyPrefixes() {
			if strings.HasPrefix(key, prefix) {
				delete(metadata, key)
				break
			}
		}
	}
}

func stripOpenAICodexClientMetadataIdentityRaw(payload []byte) ([]byte, error) {
	metadata := gjson.GetBytes(payload, "client_metadata")
	if !metadata.IsObject() {
		return payload, nil
	}
	updated := payload
	for key := range metadata.Map() {
		if !isOpenAICodexClientMetadataIdentityKey(key) {
			continue
		}
		next, err := sjson.DeleteBytes(updated, openAICodexClientMetadataPath(key))
		if err != nil {
			return payload, err
		}
		updated = next
	}
	return updated, nil
}

func isOpenAICodexClientMetadataIdentityKey(key string) bool {
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}
	for _, identityKey := range openAICodexClientMetadataIdentityKeys() {
		if key == identityKey {
			return true
		}
	}
	for _, prefix := range openAICodexClientMetadataIdentityKeyPrefixes() {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

func openAICodexClientMetadataPath(key string) string {
	return "client_metadata." + strings.ReplaceAll(strings.TrimSpace(key), ".", "\\.")
}

func resetHTTPRequestBody(req *http.Request, body []byte) {
	if req == nil {
		return
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
}

func copyOpenAICodexOptionalHeaders(dst http.Header, c *gin.Context) {
	if dst == nil {
		return
	}
	for _, key := range []string{
		openAICodexParentThreadIDHeader,
		openAICodexBetaFeaturesHeader,
		openAICodexTurnStateHeader,
		openAICodexTurnMetadataHeader,
		openAICodexSubagentHeader,
		openAICodexMemgenRequestHeader,
		openAICodexAttestationHeader,
		openAICodexIncludeTimingMetricsHeader,
		openAITraceparentHeader,
		openAITracestateHeader,
	} {
		if dst.Get(key) != "" {
			continue
		}
		if c == nil {
			continue
		}
		if value := strings.TrimSpace(c.GetHeader(key)); value != "" {
			dst.Set(key, value)
		}
	}
}

func openAIHeaderValue(req *http.Request, c *gin.Context, key string) string {
	if req != nil {
		if value := strings.TrimSpace(req.Header.Get(key)); value != "" {
			return value
		}
	}
	if c != nil {
		return strings.TrimSpace(c.GetHeader(key))
	}
	return ""
}

func firstNonEmptyOpenAICodexString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func bodyPromptCacheKey(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	return strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String())
}

func resolveOpenAICodexWindowGeneration(req *http.Request, c *gin.Context, body []byte, preferBody bool) string {
	if preferBody && len(body) > 0 {
		if generation, ok := parseOpenAICodexWindowGeneration(gjson.GetBytes(body, "client_metadata."+openAICodexWindowIDHeader).String()); ok {
			return generation
		}
	}
	if preferBody && isOpenAIWSHTTPBridgeContext(c) {
		return "0"
	}
	if generation, ok := parseOpenAICodexWindowGeneration(openAIHeaderValue(req, c, openAICodexWindowIDHeader)); ok {
		return generation
	}
	if !preferBody && len(body) > 0 {
		if generation, ok := parseOpenAICodexWindowGeneration(gjson.GetBytes(body, "client_metadata."+openAICodexWindowIDHeader).String()); ok {
			return generation
		}
	}
	return "0"
}

func isOpenAIWSHTTPBridgeContext(c *gin.Context) bool {
	if c == nil {
		return false
	}
	value, ok := c.Get("openai_ws_http_bridge")
	if !ok {
		return false
	}
	enabled, _ := value.(bool)
	return enabled
}

func parseOpenAICodexWindowGeneration(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	idx := strings.LastIndex(raw, ":")
	if idx < 0 || idx == len(raw)-1 {
		return "", false
	}
	generation := strings.TrimSpace(raw[idx+1:])
	if generation == "" || len(generation) > 20 {
		return "", false
	}
	parsed, err := strconv.ParseUint(generation, 10, 64)
	if err != nil {
		return "", false
	}
	return strconv.FormatUint(parsed, 10), true
}

func fallbackOpenAICodexSessionID(c *gin.Context, account *Account, body []byte) string {
	seed := ""
	if len(body) > 0 {
		seed = deriveOpenAIContentSessionSeed(body)
	}
	accountID := int64(0)
	if account != nil {
		accountID = account.ID
	}
	return generateSessionUUID(fmt.Sprintf("openai-codex-session:%d:%d:%s", accountID, getAPIKeyIDFromContext(c), seed))
}

func deterministicOpenAICodexInstallationID(c *gin.Context, account *Account) string {
	accountID := int64(0)
	if account != nil {
		accountID = account.ID
	}
	return generateSessionUUID(fmt.Sprintf("openai-codex-installation:%d:%d", accountID, getAPIKeyIDFromContext(c)))
}

func logCodexCLIOnlyDetection(ctx context.Context, c *gin.Context, account *Account, apiKeyID int64, result CodexClientRestrictionDetectionResult, body []byte) {
	if !result.Enabled {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	accountID := int64(0)
	if account != nil {
		accountID = account.ID
	}
	fields := []zap.Field{
		zap.String("component", "service.openai_gateway"),
		zap.Int64("account_id", accountID),
		zap.Bool("codex_cli_only_enabled", result.Enabled),
		zap.Bool("codex_official_client_match", result.Matched),
		zap.String("reject_reason", result.Reason),
	}
	if apiKeyID > 0 {
		fields = append(fields, zap.Int64("api_key_id", apiKeyID))
	}
	if !result.Matched {
		fields = appendCodexCLIOnlyRejectedRequestFields(fields, c, body)
	}
	log := logger.FromContext(ctx).With(fields...)
	if result.Matched {
		log.Info("OpenAI codex_cli_only 放行请求")
		return
	}
	log.Warn("OpenAI codex_cli_only 拒绝非官方客户端请求")
}

func appendCodexCLIOnlyRejectedRequestFields(fields []zap.Field, c *gin.Context, body []byte) []zap.Field {
	if c == nil || c.Request == nil {
		return fields
	}

	req := c.Request
	requestModel, requestStream, promptCacheKey := extractOpenAIRequestMetaFromBody(body)
	requestUserAgent := strings.TrimSpace(req.Header.Get("User-Agent"))
	fields = append(fields,
		zap.String("request_method", strings.TrimSpace(req.Method)),
		zap.String("request_path", strings.TrimSpace(req.URL.Path)),
		zap.String("request_query", sanitizeOpenAIRequestQueryForLog(req.URL.RawQuery)),
		zap.String("request_host", strings.TrimSpace(req.Host)),
		zap.String("request_client_ip", strings.TrimSpace(ip.GetClientIP(c))),
		zap.String("request_remote_addr", strings.TrimSpace(req.RemoteAddr)),
		zap.Bool("request_user_agent_present", requestUserAgent != ""),
		zap.String("request_user_agent_sha256", hashSensitiveValueForLog(requestUserAgent)),
		zap.String("request_content_type", strings.TrimSpace(req.Header.Get("Content-Type"))),
		zap.Int64("request_content_length", req.ContentLength),
		zap.Bool("request_stream", requestStream),
	)
	if requestModel != "" {
		fields = append(fields, zap.String("request_model", requestModel))
	}
	if promptCacheKey != "" {
		fields = append(fields, zap.String("request_prompt_cache_key_sha256", hashSensitiveValueForLog(promptCacheKey)))
	}

	if headers := snapshotCodexCLIOnlyHeaders(req.Header); len(headers) > 0 {
		fields = append(fields, zap.Any("request_headers", headers))
	}
	fields = append(fields, zap.Int("request_body_size", len(body)))
	return fields
}

func sanitizeOpenAIRequestQueryForLog(rawQuery string) string {
	rawQuery = strings.TrimSpace(rawQuery)
	if rawQuery == "" {
		return ""
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return sanitizeOpenAIUpstreamDiagnosticText(rawQuery)
	}
	for key, vals := range values {
		if isOpenAIQueryParamSensitiveForLog(key) {
			for i := range vals {
				vals[i] = "[redacted]"
			}
			continue
		}
		for i, value := range vals {
			vals[i] = sanitizeOpenAIUpstreamDiagnosticText(value)
		}
	}
	return values.Encode()
}

func isOpenAIQueryParamSensitiveForLog(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "authorization",
		"access_token",
		"refresh_token",
		"api_key",
		"prompt_cache_key",
		"session_id",
		"conversation_id",
		"installation_id",
		"thread_id",
		"window_id",
		"raw_user_agent",
		"user_agent",
		"user-agent":
		return true
	default:
		return isOpenAIIdentityHeaderSensitiveForLog(key)
	}
}

func snapshotCodexCLIOnlyHeaders(header http.Header) map[string]string {
	if len(header) == 0 {
		return nil
	}
	result := make(map[string]string, len(codexCLIOnlyDebugHeaderWhitelist))
	for _, key := range codexCLIOnlyDebugHeaderWhitelist {
		value := strings.TrimSpace(header.Get(key))
		if value == "" {
			continue
		}
		lowerKey := strings.ToLower(key)
		if isOpenAIIdentityHeaderSensitiveForLog(lowerKey) {
			result[lowerKey+"_sha256"] = hashSensitiveValueForLog(value)
			continue
		}
		result[lowerKey] = truncateString(value, codexCLIOnlyHeaderValueMaxBytes)
	}
	return result
}

func isOpenAIIdentityHeaderSensitiveForLog(lowerKey string) bool {
	switch strings.ToLower(strings.TrimSpace(lowerKey)) {
	case "user-agent",
		"session_id",
		"conversation_id",
		openAICodexSessionIDHeader,
		openAICodexThreadIDHeader,
		openAICodexClientRequestIDHeader,
		openAICodexInstallationIDHeader,
		openAICodexWindowIDHeader:
		return true
	default:
		return false
	}
}

func hashSensitiveValueForLog(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}

func logOpenAIInstructionsRequiredDebug(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	upstreamStatusCode int,
	upstreamMsg string,
	requestBody []byte,
	upstreamBody []byte,
) {
	msg := strings.TrimSpace(upstreamMsg)
	if !isOpenAIInstructionsRequiredError(upstreamStatusCode, msg, upstreamBody) {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	accountID := int64(0)
	accountName := ""
	if account != nil {
		accountID = account.ID
		accountName = strings.TrimSpace(account.Name)
	}

	userAgent := ""
	originator := ""
	if c != nil {
		userAgent = strings.TrimSpace(c.GetHeader("User-Agent"))
		originator = strings.TrimSpace(c.GetHeader("originator"))
	}

	fields := []zap.Field{
		zap.String("component", "service.openai_gateway"),
		zap.Int64("account_id", accountID),
		zap.String("account_name", accountName),
		zap.Int("upstream_status_code", upstreamStatusCode),
		zap.String("upstream_error_message", msg),
		zap.Bool("request_user_agent_present", userAgent != ""),
		zap.String("request_user_agent_sha256", hashSensitiveValueForLog(userAgent)),
		zap.Bool("codex_official_client_match", openai.IsCodexOfficialClientByHeaders(userAgent, originator)),
	}
	fields = appendCodexCLIOnlyRejectedRequestFields(fields, c, requestBody)

	logger.FromContext(ctx).With(fields...).Warn("OpenAI 上游返回 Instructions are required，已记录请求详情用于排查")
}

func isOpenAIInstructionsRequiredError(upstreamStatusCode int, upstreamMsg string, upstreamBody []byte) bool {
	if upstreamStatusCode != http.StatusBadRequest {
		return false
	}

	hasInstructionRequired := func(text string) bool {
		lower := strings.ToLower(strings.TrimSpace(text))
		if lower == "" {
			return false
		}
		if strings.Contains(lower, "instructions are required") {
			return true
		}
		if strings.Contains(lower, "required parameter: 'instructions'") {
			return true
		}
		if strings.Contains(lower, "required parameter: instructions") {
			return true
		}
		if strings.Contains(lower, "missing required parameter") && strings.Contains(lower, "instructions") {
			return true
		}
		return strings.Contains(lower, "instruction") && strings.Contains(lower, "required")
	}

	if hasInstructionRequired(upstreamMsg) {
		return true
	}
	if len(upstreamBody) == 0 {
		return false
	}

	errMsg := gjson.GetBytes(upstreamBody, "error.message").String()
	errMsgLower := strings.ToLower(strings.TrimSpace(errMsg))
	errCode := strings.ToLower(strings.TrimSpace(gjson.GetBytes(upstreamBody, "error.code").String()))
	errParam := strings.ToLower(strings.TrimSpace(gjson.GetBytes(upstreamBody, "error.param").String()))
	errType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(upstreamBody, "error.type").String()))

	if errParam == "instructions" {
		return true
	}
	if hasInstructionRequired(errMsg) {
		return true
	}
	if strings.Contains(errCode, "missing_required_parameter") && strings.Contains(errMsgLower, "instructions") {
		return true
	}
	if strings.Contains(errType, "invalid_request") && strings.Contains(errMsgLower, "instructions") && strings.Contains(errMsgLower, "required") {
		return true
	}

	return false
}

func isOpenAITransientProcessingError(upstreamStatusCode int, upstreamMsg string, upstreamBody []byte) bool {
	if upstreamStatusCode != http.StatusBadRequest && upstreamStatusCode != http.StatusServiceUnavailable {
		return false
	}

	hasOverloadedCode := func(payload []byte) bool {
		code := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "error.code").String()))
		if code == "" {
			code = strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "response.error.code").String()))
		}
		return code == "server_is_overloaded" || code == "slow_down"
	}
	if len(upstreamBody) > 0 && hasOverloadedCode(upstreamBody) {
		return true
	}
	if upstreamStatusCode != http.StatusBadRequest {
		return false
	}

	match := func(text string) bool {
		lower := strings.ToLower(strings.TrimSpace(text))
		if lower == "" {
			return false
		}
		if strings.Contains(lower, "an error occurred while processing your request") {
			return true
		}
		if strings.Contains(lower, "selected model is at capacity") {
			return true
		}
		return strings.Contains(lower, "you can retry your request") &&
			strings.Contains(lower, "help.openai.com") &&
			strings.Contains(lower, "request id")
	}
	if match(upstreamMsg) {
		return true
	}
	if len(upstreamBody) == 0 {
		return false
	}
	if match(gjson.GetBytes(upstreamBody, "error.message").String()) {
		return true
	}
	return match(string(upstreamBody))
}

// ExtractSessionID extracts the raw session ID from headers or body without hashing.
// Used by ForwardAsAnthropic to pass as prompt_cache_key for upstream cache.
func (s *OpenAIGatewayService) ExtractSessionID(c *gin.Context, body []byte) string {
	if c == nil {
		return ""
	}
	sessionID := strings.TrimSpace(c.GetHeader(openAICodexSessionIDHeader))
	if sessionID == "" {
		sessionID = strings.TrimSpace(c.GetHeader("session_id"))
	}
	if sessionID == "" {
		sessionID = strings.TrimSpace(c.GetHeader(openAICodexThreadIDHeader))
	}
	if sessionID == "" {
		sessionID = strings.TrimSpace(c.GetHeader("conversation_id"))
	}
	if sessionID == "" && len(body) > 0 {
		sessionID = strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String())
	}
	return sessionID
}

func explicitOpenAISessionID(c *gin.Context, body []byte) string {
	if c == nil {
		return ""
	}

	sessionID := strings.TrimSpace(c.GetHeader(openAICodexSessionIDHeader))
	if sessionID == "" {
		sessionID = strings.TrimSpace(c.GetHeader("session_id"))
	}
	if sessionID == "" {
		sessionID = strings.TrimSpace(c.GetHeader(openAICodexThreadIDHeader))
	}
	if sessionID == "" {
		sessionID = strings.TrimSpace(c.GetHeader("conversation_id"))
	}
	if sessionID == "" && len(body) > 0 {
		sessionID = strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String())
	}
	return sessionID
}

// GenerateExplicitSessionHash generates a sticky-session hash only from explicit
// client session signals. It intentionally skips content-derived fallback and is
// used by stateless endpoints such as /v1/images.
func (s *OpenAIGatewayService) GenerateExplicitSessionHash(c *gin.Context, body []byte) string {
	sessionID := explicitOpenAISessionID(c, body)
	if sessionID == "" {
		return ""
	}

	currentHash, legacyHash := deriveOpenAISessionHashes(sessionID)
	attachOpenAILegacySessionHashToGin(c, legacyHash)
	return currentHash
}

// GenerateSessionHash generates a sticky-session hash for OpenAI requests.
//
// Priority:
//  1. Header: session_id
//  2. Header: conversation_id
//  3. Body:   prompt_cache_key (opencode)
//  4. Body:   content-based fallback (model + system + tools + first user message)
func (s *OpenAIGatewayService) GenerateSessionHash(c *gin.Context, body []byte) string {
	if c == nil {
		return ""
	}

	sessionID := explicitOpenAISessionID(c, body)
	if sessionID == "" && len(body) > 0 {
		sessionID = deriveOpenAIContentSessionSeed(body)
	}
	if sessionID == "" {
		return ""
	}

	currentHash, legacyHash := deriveOpenAISessionHashes(sessionID)
	attachOpenAILegacySessionHashToGin(c, legacyHash)
	return currentHash
}

// GenerateSessionHashWithFallback 先按常规信号生成会话哈希；
// 当未携带 session_id/conversation_id/prompt_cache_key 时，使用 fallbackSeed 生成稳定哈希。
// 该方法用于 WS ingress，避免会话信号缺失时发生跨账号漂移。
func (s *OpenAIGatewayService) GenerateSessionHashWithFallback(c *gin.Context, body []byte, fallbackSeed string) string {
	sessionHash := s.GenerateSessionHash(c, body)
	if sessionHash != "" {
		return sessionHash
	}

	seed := strings.TrimSpace(fallbackSeed)
	if seed == "" {
		return ""
	}

	currentHash, legacyHash := deriveOpenAISessionHashes(seed)
	attachOpenAILegacySessionHashToGin(c, legacyHash)
	return currentHash
}

func resolveOpenAIUpstreamOriginator(c *gin.Context, isOfficialClient bool) string {
	if c != nil {
		if originator := strings.TrimSpace(c.GetHeader("originator")); originator != "" && openai.IsCodexOfficialClientByHeaders(c.GetHeader("User-Agent"), originator) {
			return originator
		}
	}
	if isOfficialClient {
		return codexOfficialOriginator
	}
	return codexOfficialOriginator
}

// BindStickySession sets session -> account binding with standard TTL.
func (s *OpenAIGatewayService) BindStickySession(ctx context.Context, groupID *int64, sessionHash string, accountID int64) error {
	if sessionHash == "" || accountID <= 0 {
		return nil
	}
	ttl := openaiStickySessionTTL
	if s != nil && s.cfg != nil && s.cfg.Gateway.OpenAIWS.StickySessionTTLSeconds > 0 {
		ttl = time.Duration(s.cfg.Gateway.OpenAIWS.StickySessionTTLSeconds) * time.Second
	}
	return s.setStickySessionAccountID(ctx, groupID, sessionHash, accountID, ttl)
}

// SelectAccount selects an OpenAI account with sticky session support
func (s *OpenAIGatewayService) SelectAccount(ctx context.Context, groupID *int64, sessionHash string) (*Account, error) {
	return s.SelectAccountForModel(ctx, groupID, sessionHash, "")
}

// SelectAccountForModel selects an account supporting the requested model
func (s *OpenAIGatewayService) SelectAccountForModel(ctx context.Context, groupID *int64, sessionHash string, requestedModel string) (*Account, error) {
	return s.SelectAccountForModelWithExclusions(ctx, groupID, sessionHash, requestedModel, nil)
}

// SelectAccountForModelWithExclusions selects an account supporting the requested model while excluding specified accounts.
// SelectAccountForModelWithExclusions 选择支持指定模型的账号，同时排除指定的账号。
func (s *OpenAIGatewayService) SelectAccountForModelWithExclusions(ctx context.Context, groupID *int64, sessionHash string, requestedModel string, excludedIDs map[int64]struct{}) (*Account, error) {
	return s.selectAccountForModelWithExclusions(s.withOpenAIQuotaAutoPauseContext(ctx), groupID, sessionHash, requestedModel, excludedIDs, false, 0, "")
}

// noAvailableOpenAISelectionError builds the standard "no account available" error
// while preserving the compact-specific error when applicable.
func noAvailableOpenAISelectionError(requestedModel string, compactBlocked bool) error {
	if compactBlocked {
		return ErrNoAvailableCompactAccounts
	}
	if strings.TrimSpace(requestedModel) != "" {
		return noAvailableOpenAIAccountsError(fmt.Sprintf("no available OpenAI accounts supporting model: %s", requestedModel))
	}
	return noAvailableOpenAIAccountsError("no available OpenAI accounts")
}

func noAvailableOpenAISelectionCapacityError(requestedModel string) error {
	if strings.TrimSpace(requestedModel) != "" {
		return noAvailableOpenAIAccountsError(fmt.Sprintf("no available OpenAI accounts supporting model: %s", requestedModel))
	}
	return ErrNoAvailableAccounts
}

type noAvailableOpenAIAccountsError string

func (e noAvailableOpenAIAccountsError) Error() string {
	return string(e)
}

func (e noAvailableOpenAIAccountsError) Is(target error) bool {
	return target == ErrNoAvailableAccounts
}

func noAvailableOpenAISelectionErrorForAccounts(ctx context.Context, service *OpenAIGatewayService, groupID *int64, accounts []Account, requestedModel string, excludedIDs map[int64]struct{}, requireCompact bool, requiredCapability OpenAIEndpointCapability, requiredImageCapability OpenAIImagesCapability, requiredTransport OpenAIUpstreamTransport, schedGroup *Group, compactBlocked bool) error {
	if compactBlocked {
		return ErrNoAvailableCompactAccounts
	}
	if isPureOpenAIModelSupportMiss(ctx, service, groupID, accounts, requestedModel, excludedIDs, requireCompact, requiredCapability, requiredImageCapability, requiredTransport, schedGroup) {
		return newModelNotSupportedByAccountsError(requestedModel)
	}
	return noAvailableOpenAISelectionError(requestedModel, false)
}

func isPureOpenAIModelSupportMiss(ctx context.Context, service *OpenAIGatewayService, groupID *int64, accounts []Account, requestedModel string, excludedIDs map[int64]struct{}, requireCompact bool, requiredCapability OpenAIEndpointCapability, requiredImageCapability OpenAIImagesCapability, requiredTransport OpenAIUpstreamTransport, schedGroup *Group) bool {
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" || !publicModelSupportMiss404Enabled(ctx) || len(excludedIDs) > 0 || service == nil {
		return false
	}
	if candidateRepo, ok := service.accountRepo.(ModelAvailabilityCandidateRepository); ok {
		includeGrouped := groupID == nil && service.cfg != nil && service.cfg.RunMode == config.RunModeSimple
		configuredAccounts, err := candidateRepo.ListModelAvailabilityCandidates(ctx, groupID, []string{PlatformOpenAI}, includeGrouped)
		if err != nil {
			return false
		}
		accounts = configuredAccounts
	}
	if len(accounts) == 0 {
		return false
	}
	needsUpstreamCheck := service.needsUpstreamChannelRestrictionCheck(ctx, groupID)

	otherwiseEligible := 0
	for i := range accounts {
		account := &accounts[i]
		if account == nil || !account.IsOpenAI() {
			continue
		}
		if account.IsModelSupported(requestedModel) {
			return false
		}
		if shouldBlockAccountForPrivacyRequirement(account, schedGroup) {
			continue
		}
		if needsUpstreamCheck && service.isUpstreamModelRestrictedByChannel(ctx, *groupID, account, requestedModel, requireCompact) {
			continue
		}
		if !account.SupportsOpenAIEndpointCapability(requiredCapability) {
			continue
		}
		if !account.SupportsOpenAIImageCapability(requiredImageCapability) {
			continue
		}
		if requireCompact && openAICompactSupportTier(account) == 0 {
			continue
		}
		if service != nil && !service.isOpenAIAccountTransportCompatible(account, requiredTransport) {
			continue
		}
		otherwiseEligible++
	}
	return otherwiseEligible > 0
}

// openAICompactSupportTier classifies an OpenAI account by compact capability.
// 0 = explicitly unsupported, 1 = unknown / not yet probed, 2 = explicitly supported.
func openAICompactSupportTier(account *Account) int {
	if account == nil || !account.IsOpenAI() {
		return 0
	}
	supported, known := account.OpenAICompactSupportKnown()
	if !known {
		return 1
	}
	if supported {
		return 2
	}
	return 0
}

// isOpenAIAccountEligibleForRequest centralises the schedulable / OpenAI / model /
// compact-support checks used during account selection.
func isOpenAIAccountEligibleForRequest(ctx context.Context, account *Account, requestedModel string, requireCompact bool, requiredCapability OpenAIEndpointCapability) bool {
	if account == nil || !account.IsOpenAI() || !account.IsSchedulableForModelWithContext(ctx, requestedModel) {
		return false
	}
	if paused, reason := shouldAutoPauseOpenAIAccountByQuota(ctx, account); paused {
		// Debug level: this fires per-candidate on the scheduling hot path, so Info
		// would amplify into log spam once several accounts cross the threshold.
		slog.Debug("account_auto_paused_by_quota",
			"account_id", account.ID,
			"window", reason.window,
			"threshold", reason.threshold,
			"utilization", reason.utilization,
		)
		return false
	}
	if requestedModel != "" && !account.IsModelSupported(requestedModel) {
		return false
	}
	if !account.SupportsOpenAIEndpointCapability(requiredCapability) {
		return false
	}
	if requireCompact && openAICompactSupportTier(account) == 0 {
		return false
	}
	return true
}

func (s *OpenAIGatewayService) resolveOpenAISchedulingGroup(ctx context.Context, groupID *int64) *Group {
	if groupID == nil {
		return nil
	}
	if group, ok := ctx.Value(ctxkey.Group).(*Group); ok && group != nil && group.ID == *groupID {
		return group
	}
	if group, ok := ctx.Value(ctxkey.Group).(Group); ok && group.ID == *groupID {
		groupCopy := group
		return &groupCopy
	}
	if s != nil && s.schedulerSnapshot != nil {
		group, err := s.schedulerSnapshot.GetGroupByID(ctx, *groupID)
		if err == nil {
			return group
		}
		// 若无法确认分组配置，privacy gate 必须 fail closed，避免缺失上下文时放行未设置 privacy 的 OAuth 账号。
		slog.Warn("failed to resolve OpenAI scheduling group; enforcing privacy requirement", "group_id", *groupID, "error", err)
		return &Group{ID: *groupID, Name: fmt.Sprintf("group:%d", *groupID), Platform: PlatformOpenAI, RequirePrivacySet: true}
	}
	return nil
}

func (s *OpenAIGatewayService) resolveOpenAIAccountForPrivacyRequirement(ctx context.Context, account *Account, group *Group) (*Account, bool) {
	if account == nil {
		return nil, false
	}
	if group == nil || !group.RequirePrivacySet {
		return account, true
	}
	if account.Platform == PlatformOpenAI && account.Type != AccountTypeOAuth {
		return account, true
	}
	if s != nil && s.schedulerSnapshot != nil && s.accountRepo != nil {
		// Snapshot/cache data can lag behind privacy updates; verify against DB before
		// mutating runtime/account error state so fresh privacy-set accounts are not poisoned.
		latest, err := s.accountRepo.GetByID(ctx, account.ID)
		if err != nil || latest == nil {
			return nil, false
		}
		account = latest
	}
	return s.resolveFreshOpenAIAccountForPrivacyRequirement(ctx, account, group)
}

// resolveFreshOpenAIAccountForPrivacyRequirement applies privacy gating to an account the caller already refreshed from DB.
func (s *OpenAIGatewayService) resolveFreshOpenAIAccountForPrivacyRequirement(ctx context.Context, account *Account, group *Group) (*Account, bool) {
	if account == nil {
		return nil, false
	}
	if group == nil || !group.RequirePrivacySet {
		return account, true
	}
	if account.Platform == PlatformOpenAI && account.Type != AccountTypeOAuth {
		return account, true
	}
	if s.blockOpenAIAccountForPrivacyRequirement(ctx, account, group) {
		return nil, false
	}
	return account, true
}

func (s *OpenAIGatewayService) blockOpenAIAccountForPrivacyRequirement(ctx context.Context, account *Account, group *Group) bool {
	if !shouldBlockAccountForPrivacyRequirement(account, group) {
		return false
	}
	if s != nil {
		if s.isOpenAIAccountRuntimeBlocked(account) {
			return true
		}
		// 将 privacy gate 同步到运行时隔离，防止 sticky/previous/fallback 路径重复命中。
		s.BlockAccountScheduling(account, time.Time{}, "privacy_not_set")
		if s.accountRepo != nil {
			_ = s.accountRepo.SetError(ctx, account.ID,
				fmt.Sprintf("Privacy not set, required by group [%s]", group.Name))
		}
	}
	return true
}

type openAIQuotaAutoPauseDecision struct {
	window      string
	threshold   float64
	utilization float64
}

func shouldAutoPauseOpenAIAccountByQuota(ctx context.Context, account *Account) (bool, openAIQuotaAutoPauseDecision) {
	if account == nil || !account.IsOpenAI() {
		return false, openAIQuotaAutoPauseDecision{}
	}
	// Per-account explicit-disable flags must take precedence over the global default.
	// Without these, leaving the account threshold blank means "use global default",
	// so an admin has no way to exempt a single account from auto-pause once a global
	// default exists. The disable flag is per-window so an account can opt out of
	// only 5h or only 7d auto-pause.
	disabled5h := resolveAccountExtraBool(account.Extra, "auto_pause_5h_disabled")
	disabled7d := resolveAccountExtraBool(account.Extra, "auto_pause_7d_disabled")
	threshold5h, threshold7d := resolveOpenAIQuotaAutoPauseThresholds(ctx, account)
	now := time.Now()
	if !disabled5h && threshold5h > 0 {
		if utilization, ok := resolveOpenAIQuotaUtilization(account.Extra, "5h", now); ok && utilization >= threshold5h {
			return true, openAIQuotaAutoPauseDecision{window: "5h", threshold: threshold5h, utilization: utilization}
		}
	}
	if !disabled7d && threshold7d > 0 {
		if utilization, ok := resolveOpenAIQuotaUtilization(account.Extra, "7d", now); ok && utilization >= threshold7d {
			return true, openAIQuotaAutoPauseDecision{window: "7d", threshold: threshold7d, utilization: utilization}
		}
	}
	return false, openAIQuotaAutoPauseDecision{}
}

// resolveAccountExtraBool reads a bool-like value from account extra, tolerating
// the few shapes JSON unmarshalling may produce (real bool, "true"/"false"
// strings, 0/1 numbers).
func resolveAccountExtraBool(extra map[string]any, key string) bool {
	if len(extra) == 0 {
		return false
	}
	value, ok := extra[key]
	if !ok || value == nil {
		return false
	}
	switch v := value.(type) {
	case bool:
		return v
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(v))
		return err == nil && parsed
	case float64:
		return v != 0
	case float32:
		return v != 0
	case int:
		return v != 0
	case int64:
		return v != 0
	case json.Number:
		if i, err := v.Int64(); err == nil {
			return i != 0
		}
	}
	return false
}

func resolveOpenAIQuotaAutoPauseThresholds(ctx context.Context, account *Account) (float64, float64) {
	threshold5h, _ := resolveAccountExtraNumber(account.Extra, "auto_pause_5h_threshold")
	threshold7d, _ := resolveAccountExtraNumber(account.Extra, "auto_pause_7d_threshold")
	threshold5h = clamp01(threshold5h)
	threshold7d = clamp01(threshold7d)
	if threshold5h > 0 && threshold7d > 0 {
		return threshold5h, threshold7d
	}
	settings := openAIQuotaAutoPauseSettingsFromContext(ctx)
	if threshold5h <= 0 {
		threshold5h = clamp01(settings.DefaultThreshold5h)
	}
	if threshold7d <= 0 {
		threshold7d = clamp01(settings.DefaultThreshold7d)
	}
	return threshold5h, threshold7d
}

func resolveAccountExtraNumber(extra map[string]any, keys ...string) (float64, bool) {
	if len(extra) == 0 {
		return 0, false
	}
	for _, key := range keys {
		value, ok := extra[key]
		if !ok || value == nil {
			continue
		}
		switch v := value.(type) {
		case float64:
			return v, true
		case float32:
			return float64(v), true
		case int:
			return float64(v), true
		case int64:
			return float64(v), true
		case json.Number:
			parsed, err := v.Float64()
			if err == nil {
				return parsed, true
			}
		case string:
			parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err == nil {
				return parsed, true
			}
		}
	}
	return 0, false
}

// resolveOpenAIQuotaUtilization returns the current utilization ratio (0..1) for the
// given Codex usage window. ok=false means there is no usable signal to pause on:
// either no snapshot exists, or the window has already rolled over so the cached
// percentage is stale. The stale guard matters because a paused account stops
// receiving requests, so its snapshot is never refreshed from upstream headers —
// without this check an old used_percent would keep the account paused forever even
// after the real window reset.
func resolveOpenAIQuotaUtilization(extra map[string]any, window string, now time.Time) (float64, bool) {
	usedPercent := readOpenAIQuotaUsedPercent(extra, window)
	if usedPercent <= 0 {
		return 0, false
	}
	if openAIQuotaWindowReset(extra, window, now) {
		return 0, false
	}
	// 快照过于陈旧（账号长期未收到流量刷新）时，不再据此暂停。放行后下一次响应头
	// 会刷新快照实现自愈，避免账号在错误/过期的 used% 上被永久跳过（issue #2994）。
	if openAICodexSnapshotStaleForPause(extra, now) {
		return 0, false
	}
	return usedPercent / 100, true
}

// openAICodexSnapshotStaleForPause reports whether the Codex usage snapshot is stale
// enough that it should no longer keep an account auto-paused. It anchors on
// codex_usage_updated_at (always written by buildCodexUsageExtraUpdates). A missing or
// unparseable timestamp returns false (treated as fresh, so the account stays paused) —
// this is deliberate: it prevents any snapshot without a write time from silently escaping
// auto-pause, and a genuinely-exhausted account that is actively served refreshes the
// timestamp on every response so it never crosses the staleness bound.
func openAICodexSnapshotStaleForPause(extra map[string]any, now time.Time) bool {
	if len(extra) == 0 {
		return false
	}
	updatedRaw, ok := extra["codex_usage_updated_at"]
	if !ok {
		return false
	}
	updatedAt, err := parseTime(fmt.Sprint(updatedRaw))
	if err != nil {
		return false
	}
	return now.Sub(updatedAt) >= openAICodexAutoPauseStaleAfter
}

// openAIQuotaWindowReset reports whether the Codex usage window's reset time has
// already passed relative to now. It prefers the absolute codex_<window>_reset_at
// timestamp and falls back to codex_<window>_reset_after_seconds anchored at
// codex_usage_updated_at, mirroring AccountUsageService's window-progress logic.
func openAIQuotaWindowReset(extra map[string]any, window string, now time.Time) bool {
	if len(extra) == 0 {
		return false
	}
	if resetAtRaw, ok := extra["codex_"+window+"_reset_at"]; ok {
		if resetAt, err := parseTime(fmt.Sprint(resetAtRaw)); err == nil {
			return !now.Before(resetAt)
		}
	}
	resetAfter := parseExtraInt(extra["codex_"+window+"_reset_after_seconds"])
	if resetAfter <= 0 {
		return false
	}
	base := now
	if updatedRaw, ok := extra["codex_usage_updated_at"]; ok {
		if updatedAt, err := parseTime(fmt.Sprint(updatedRaw)); err == nil {
			base = updatedAt
		}
	}
	resetAt := base.Add(time.Duration(resetAfter) * time.Second)
	return !now.Before(resetAt)
}

func readOpenAIQuotaUsedPercent(extra map[string]any, window string) float64 {
	if len(extra) == 0 {
		return 0
	}
	if value, ok := resolveAccountExtraNumber(extra, "codex_"+window+"_used_percent"); ok {
		return value
	}
	return 0
}

type openAIQuotaAutoPauseCtxKey struct{}

func withOpenAIQuotaAutoPauseSettings(ctx context.Context, settings OpsOpenAIAccountQuotaAutoPauseSettings) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, openAIQuotaAutoPauseCtxKey{}, settings)
}

func openAIQuotaAutoPauseSettingsFromContext(ctx context.Context) OpsOpenAIAccountQuotaAutoPauseSettings {
	if ctx == nil {
		return OpsOpenAIAccountQuotaAutoPauseSettings{}
	}
	settings, _ := ctx.Value(openAIQuotaAutoPauseCtxKey{}).(OpsOpenAIAccountQuotaAutoPauseSettings)
	return settings
}

func (s *OpenAIGatewayService) withOpenAIQuotaAutoPauseContext(ctx context.Context) context.Context {
	if s == nil || s.settingService == nil {
		return ctx
	}
	return withOpenAIQuotaAutoPauseSettings(ctx, s.settingService.GetOpenAIQuotaAutoPauseSettings(ctx))
}

// prioritizeOpenAICompactAccounts re-orders a slice so that accounts with known
// compact support are tried first, followed by unknown, then explicitly unsupported.
// The relative order within each tier is preserved.
func prioritizeOpenAICompactAccounts(accounts []*Account) []*Account {
	if len(accounts) == 0 {
		return nil
	}
	supported := make([]*Account, 0, len(accounts))
	unknown := make([]*Account, 0, len(accounts))
	unsupported := make([]*Account, 0, len(accounts))
	for _, account := range accounts {
		switch openAICompactSupportTier(account) {
		case 2:
			supported = append(supported, account)
		case 1:
			unknown = append(unknown, account)
		default:
			unsupported = append(unsupported, account)
		}
	}
	out := make([]*Account, 0, len(accounts))
	out = append(out, supported...)
	out = append(out, unknown...)
	out = append(out, unsupported...)
	return out
}

// resolveOpenAIAccountUpstreamModelForRequest resolves the upstream model that
// would be sent for a given request, honouring compact-only mappings when the
// caller is on the /responses/compact path.
func resolveOpenAIAccountUpstreamModelForRequest(account *Account, requestedModel string, requireCompact bool) string {
	upstreamModel := resolveOpenAIForwardModel(account, requestedModel)
	if upstreamModel == "" {
		return ""
	}
	if requireCompact {
		return resolveOpenAICompactForwardModel(account, upstreamModel)
	}
	return upstreamModel
}

func (s *OpenAIGatewayService) selectAccountForModelWithExclusions(ctx context.Context, groupID *int64, sessionHash string, requestedModel string, excludedIDs map[int64]struct{}, requireCompact bool, stickyAccountID int64, requiredCapability OpenAIEndpointCapability) (*Account, error) {
	if s.checkChannelPricingRestriction(ctx, groupID, requestedModel) {
		slog.Warn("channel pricing restriction blocked request",
			"group_id", derefGroupID(groupID),
			"model", requestedModel)
		return nil, fmt.Errorf("%w supporting model: %s (channel pricing restriction)", ErrNoAvailableAccounts, requestedModel)
	}

	// 1. 尝试粘性会话命中
	// Try sticky session hit
	if account := s.tryStickySessionHit(ctx, groupID, sessionHash, requestedModel, excludedIDs, requireCompact, stickyAccountID, requiredCapability); account != nil {
		return account, nil
	}

	// 2. 获取可调度的 OpenAI 账号
	// Get schedulable OpenAI accounts
	accounts, err := s.listSchedulableAccounts(ctx, groupID)
	if err != nil {
		return nil, fmt.Errorf("query accounts failed: %w", err)
	}

	// 3. 按优先级 + LRU 选择最佳账号
	// Select by priority + LRU
	selected, compactBlocked := s.selectBestAccount(ctx, groupID, accounts, requestedModel, excludedIDs, requireCompact, requiredCapability)

	if selected == nil {
		schedGroup := s.resolveOpenAISchedulingGroup(ctx, groupID)
		return nil, noAvailableOpenAISelectionErrorForAccounts(ctx, s, groupID, accounts, requestedModel, excludedIDs, requireCompact, requiredCapability, "", OpenAIUpstreamTransportAny, schedGroup, compactBlocked)
	}

	hydrated, err := s.hydrateSelectedAccount(ctx, selected)
	if err != nil {
		return nil, err
	}

	// 4. 设置粘性会话绑定
	// Set sticky session binding
	if sessionHash != "" {
		_ = s.setStickySessionAccountID(ctx, groupID, sessionHash, selected.ID, openaiStickySessionTTL)
	}

	return hydrated, nil
}

// tryStickySessionHit 尝试从粘性会话获取账号。
// 如果命中且账号可用则返回账号；如果账号不可用则清理会话并返回 nil。
//
// tryStickySessionHit attempts to get account from sticky session.
// Returns account if hit and usable; clears session and returns nil if account is unavailable.
func (s *OpenAIGatewayService) tryStickySessionHit(ctx context.Context, groupID *int64, sessionHash, requestedModel string, excludedIDs map[int64]struct{}, requireCompact bool, stickyAccountID int64, requiredCapability OpenAIEndpointCapability) *Account {
	if sessionHash == "" {
		return nil
	}

	accountID := stickyAccountID
	if accountID <= 0 {
		var err error
		accountID, err = s.getStickySessionAccountID(ctx, groupID, sessionHash)
		if err != nil || accountID <= 0 {
			return nil
		}
	}

	if _, excluded := excludedIDs[accountID]; excluded {
		return nil
	}

	account, err := s.getSchedulableAccount(ctx, accountID)
	if err != nil {
		return nil
	}
	account = s.refreshSelectedOpenAIAccountFromDB(ctx, account)
	if account == nil || !s.openAIStickyAccountMatchesSchedulingGroup(account, groupID) {
		_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
		return nil
	}

	// 检查账号是否需要清理粘性会话
	// Check if sticky session should be cleared
	if shouldClearStickySession(account, requestedModel) {
		_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
		return nil
	}

	// 验证账号是否可用于当前请求
	// Verify account is usable for current request
	if !isOpenAIAccountEligibleForRequest(ctx, account, requestedModel, false, requiredCapability) {
		return nil
	}
	schedGroup := s.resolveOpenAISchedulingGroup(ctx, groupID)
	var ok bool
	account, ok = s.resolveFreshOpenAIAccountForPrivacyRequirement(ctx, account, schedGroup)
	if !ok {
		_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
		return nil
	}
	if account = s.recheckOpenAIAccountEligibility(ctx, account, requestedModel, requireCompact, requiredCapability); account == nil {
		_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
		return nil
	}
	account, ok = s.resolveFreshOpenAIAccountForPrivacyRequirement(ctx, account, schedGroup)
	if !ok {
		_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
		return nil
	}
	if groupID != nil && s.needsUpstreamChannelRestrictionCheck(ctx, groupID) &&
		s.isUpstreamModelRestrictedByChannel(ctx, *groupID, account, requestedModel, requireCompact) {
		_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
		return nil
	}

	// 刷新会话 TTL 并返回账号
	// Refresh session TTL and return account
	_ = s.refreshStickySessionTTL(ctx, groupID, sessionHash, openaiStickySessionTTL)
	return account
}

// selectBestAccount 从候选账号中选择最佳账号（优先级 + LRU）。
// 返回 nil 表示无可用账号。
//
// selectBestAccount selects the best account from candidates (priority + LRU).
// Returns nil if no available account. The second return reports whether at
// least one candidate was filtered out solely because it lacks compact support
// (only meaningful when requireCompact=true).
func (s *OpenAIGatewayService) selectBestAccount(ctx context.Context, groupID *int64, accounts []Account, requestedModel string, excludedIDs map[int64]struct{}, requireCompact bool, requiredCapability OpenAIEndpointCapability) (*Account, bool) {
	var selected *Account
	selectedCompactTier := -1
	compactBlocked := false
	needsUpstreamCheck := s.needsUpstreamChannelRestrictionCheck(ctx, groupID)
	schedGroup := s.resolveOpenAISchedulingGroup(ctx, groupID)

	for i := range accounts {
		acc := &accounts[i]

		// 跳过被排除的账号
		// Skip excluded accounts
		if _, excluded := excludedIDs[acc.ID]; excluded {
			continue
		}

		fresh := s.resolveFreshSchedulableOpenAIAccount(ctx, acc, requestedModel, false, requiredCapability)
		if fresh == nil {
			continue
		}
		fresh = s.recheckSelectedOpenAIAccountFromDB(ctx, fresh, requestedModel, false, requiredCapability)
		if fresh == nil {
			continue
		}
		var ok bool
		fresh, ok = s.resolveOpenAIAccountForPrivacyRequirement(ctx, fresh, schedGroup)
		if !ok {
			continue
		}
		if needsUpstreamCheck && s.isUpstreamModelRestrictedByChannel(ctx, *groupID, fresh, requestedModel, requireCompact) {
			continue
		}
		compactTier := 0
		if requireCompact {
			compactTier = openAICompactSupportTier(fresh)
			if compactTier == 0 {
				compactBlocked = true
				continue
			}
		}

		// 选择优先级最高且最久未使用的账号
		// Select highest priority and least recently used
		if selected == nil {
			selected = fresh
			selectedCompactTier = compactTier
			continue
		}

		// compact 模式下高 tier 优先；同 tier 内才比较 priority/LRU。
		if requireCompact && compactTier != selectedCompactTier {
			if compactTier > selectedCompactTier {
				selected = fresh
				selectedCompactTier = compactTier
			}
			continue
		}

		if s.isBetterAccount(fresh, selected) {
			selected = fresh
			selectedCompactTier = compactTier
		}
	}

	return selected, compactBlocked
}

// isBetterAccount 判断 candidate 是否比 current 更优。
// 规则：优先级更高（数值更小）优先；同优先级时，未使用过的优先，其次是最久未使用的。
//
// isBetterAccount checks if candidate is better than current.
// Rules: higher priority (lower value) wins; same priority: never used > least recently used.
func (s *OpenAIGatewayService) isBetterAccount(candidate, current *Account) bool {
	// 优先级更高（数值更小）
	// Higher priority (lower value)
	if candidate.Priority < current.Priority {
		return true
	}
	if candidate.Priority > current.Priority {
		return false
	}

	// 同优先级，比较最后使用时间
	// Same priority, compare last used time
	switch {
	case candidate.LastUsedAt == nil && current.LastUsedAt != nil:
		// candidate 从未使用，优先
		return true
	case candidate.LastUsedAt != nil && current.LastUsedAt == nil:
		// current 从未使用，保持
		return false
	case candidate.LastUsedAt == nil && current.LastUsedAt == nil:
		// 都未使用，保持
		return false
	default:
		// 都使用过，选择最久未使用的
		return candidate.LastUsedAt.Before(*current.LastUsedAt)
	}
}

// SelectAccountWithLoadAwareness selects an account with load-awareness and wait plan.
func (s *OpenAIGatewayService) SelectAccountWithLoadAwareness(ctx context.Context, groupID *int64, sessionHash string, requestedModel string, excludedIDs map[int64]struct{}) (*AccountSelectionResult, error) {
	return s.selectAccountWithLoadAwareness(s.withOpenAIQuotaAutoPauseContext(ctx), groupID, sessionHash, requestedModel, excludedIDs, false, "")
}

func (s *OpenAIGatewayService) selectAccountWithLoadAwareness(ctx context.Context, groupID *int64, sessionHash string, requestedModel string, excludedIDs map[int64]struct{}, requireCompact bool, requiredCapability OpenAIEndpointCapability) (*AccountSelectionResult, error) {
	if s.checkChannelPricingRestriction(ctx, groupID, requestedModel) {
		slog.Warn("channel pricing restriction blocked request",
			"group_id", derefGroupID(groupID),
			"model", requestedModel)
		return nil, fmt.Errorf("%w supporting model: %s (channel pricing restriction)", ErrNoAvailableAccounts, requestedModel)
	}

	cfg := s.schedulingConfig()
	needsUpstreamCheck := s.needsUpstreamChannelRestrictionCheck(ctx, groupID)
	schedGroup := s.resolveOpenAISchedulingGroup(ctx, groupID)
	var stickyAccountID int64
	if sessionHash != "" && s.cache != nil {
		if accountID, err := s.getStickySessionAccountID(ctx, groupID, sessionHash); err == nil {
			stickyAccountID = accountID
		}
	}
	if s.concurrencyService == nil || !cfg.LoadBatchEnabled {
		account, err := s.selectAccountForModelWithExclusions(ctx, groupID, sessionHash, requestedModel, excludedIDs, requireCompact, stickyAccountID, requiredCapability)
		if err != nil {
			return nil, err
		}
		result, err := s.tryAcquireAccountSlot(ctx, account.ID, account.Concurrency)
		if err == nil && result != nil && result.Acquired {
			return s.newAcquiredSelectionResult(ctx, account, result.ReleaseFunc)
		}
		if stickyAccountID > 0 && stickyAccountID == account.ID && s.concurrencyService != nil {
			waitingCount, _ := s.concurrencyService.GetAccountWaitingCount(ctx, account.ID)
			if waitingCount < cfg.StickySessionMaxWaiting {
				return s.newSelectionResult(ctx, account, false, nil, &AccountWaitPlan{
					AccountID:      account.ID,
					MaxConcurrency: account.Concurrency,
					Timeout:        cfg.StickySessionWaitTimeout,
					MaxWaiting:     cfg.StickySessionMaxWaiting,
				})
			}
		}
		return s.newSelectionResult(ctx, account, false, nil, &AccountWaitPlan{
			AccountID:      account.ID,
			MaxConcurrency: account.Concurrency,
			Timeout:        cfg.FallbackWaitTimeout,
			MaxWaiting:     cfg.FallbackMaxWaiting,
		})
	}

	accounts, err := s.listSchedulableAccounts(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if len(accounts) == 0 {
		if isPureOpenAIModelSupportMiss(ctx, s, groupID, nil, requestedModel, excludedIDs, requireCompact, requiredCapability, "", OpenAIUpstreamTransportAny, schedGroup) {
			return nil, newModelNotSupportedByAccountsError(requestedModel)
		}
		return nil, noAvailableOpenAISelectionError(requestedModel, false)
	}

	isExcluded := func(accountID int64) bool {
		if excludedIDs == nil {
			return false
		}
		_, excluded := excludedIDs[accountID]
		return excluded
	}

	// ============ Layer 1: Sticky session ============
	if sessionHash != "" {
		accountID := stickyAccountID
		if accountID > 0 && !isExcluded(accountID) {
			account, err := s.getSchedulableAccount(ctx, accountID)
			if err == nil {
				account = s.refreshSelectedOpenAIAccountFromDB(ctx, account)
				if account == nil || !s.openAIStickyAccountMatchesSchedulingGroup(account, groupID) {
					_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
				} else {
					clearSticky := shouldClearStickySession(account, requestedModel)
					if clearSticky {
						_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
					}
					if !clearSticky && isOpenAIAccountEligibleForRequest(ctx, account, requestedModel, false, requiredCapability) {
						account = s.recheckOpenAIAccountEligibility(ctx, account, requestedModel, requireCompact, requiredCapability)
						if account == nil {
							_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
						} else if resolved, ok := s.resolveFreshOpenAIAccountForPrivacyRequirement(ctx, account, schedGroup); !ok {
							_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
						} else if account = resolved; !s.openAIStickyAccountMatchesSchedulingGroup(account, groupID) {
							_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
						} else if s.isOpenAIAccountRuntimeBlocked(account) {
							_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
						} else if needsUpstreamCheck && s.isUpstreamModelRestrictedByChannel(ctx, *groupID, account, requestedModel, requireCompact) {
							_ = s.deleteStickySessionAccountID(ctx, groupID, sessionHash)
						} else {
							result, err := s.tryAcquireAccountSlot(ctx, accountID, account.Concurrency)
							if err == nil && result != nil && result.Acquired {
								selection, selectErr := s.newAcquiredSelectionResult(ctx, account, result.ReleaseFunc)
								if selectErr != nil {
									return nil, selectErr
								}
								_ = s.refreshStickySessionTTL(ctx, groupID, sessionHash, openaiStickySessionTTL)
								return selection, nil
							}

							waitingCount, _ := s.concurrencyService.GetAccountWaitingCount(ctx, accountID)
							if waitingCount < cfg.StickySessionMaxWaiting {
								return s.newSelectionResult(ctx, account, false, nil, &AccountWaitPlan{
									AccountID:      accountID,
									MaxConcurrency: account.Concurrency,
									Timeout:        cfg.StickySessionWaitTimeout,
									MaxWaiting:     cfg.StickySessionMaxWaiting,
								})
							}
						}
					}
				}
			}
		}
	}

	// ============ Layer 2: Load-aware selection ============
	baseCandidateCount := 0
	candidates := make([]*Account, 0, len(accounts))
	for i := range accounts {
		acc := &accounts[i]
		if isExcluded(acc.ID) {
			continue
		}
		// Scheduler snapshots can be temporarily stale (bucket rebuild is throttled);
		// re-check schedulability here so recently rate-limited/overloaded accounts
		// are not selected again before the bucket is rebuilt.
		if !isOpenAIAccountEligibleForRequest(ctx, acc, requestedModel, false, requiredCapability) {
			continue
		}
		var ok bool
		acc, ok = s.resolveOpenAIAccountForPrivacyRequirement(ctx, acc, schedGroup)
		if !ok {
			continue
		}
		if s.isOpenAIAccountRuntimeBlocked(acc) {
			continue
		}
		if needsUpstreamCheck && s.isUpstreamModelRestrictedByChannel(ctx, *groupID, acc, requestedModel, requireCompact) {
			continue
		}
		baseCandidateCount++
		candidates = append(candidates, acc)
	}

	if len(candidates) == 0 {
		if isPureOpenAIModelSupportMiss(ctx, s, groupID, accounts, requestedModel, excludedIDs, requireCompact, requiredCapability, "", OpenAIUpstreamTransportAny, schedGroup) {
			return nil, newModelNotSupportedByAccountsError(requestedModel)
		}
		return nil, noAvailableOpenAISelectionCapacityError(requestedModel)
	}

	accountLoads := make([]AccountWithConcurrency, 0, len(candidates))
	for _, acc := range candidates {
		accountLoads = append(accountLoads, AccountWithConcurrency{
			ID:             acc.ID,
			MaxConcurrency: acc.EffectiveLoadFactor(),
		})
	}

	tryAcquireFromLoadMap := func(loadMap map[int64]*AccountLoadInfo) (*AccountSelectionResult, bool, error) {
		var available []accountWithLoad
		for _, acc := range candidates {
			loadInfo := loadMap[acc.ID]
			if loadInfo == nil {
				loadInfo = &AccountLoadInfo{AccountID: acc.ID}
			}
			if loadInfo.LoadRate < 100 {
				available = append(available, accountWithLoad{
					account:  acc,
					loadInfo: loadInfo,
				})
			}
		}

		if len(available) == 0 {
			return nil, false, nil
		}

		sort.SliceStable(available, func(i, j int) bool {
			a, b := available[i], available[j]
			if a.account.Priority != b.account.Priority {
				return a.account.Priority < b.account.Priority
			}
			if a.loadInfo.LoadRate != b.loadInfo.LoadRate {
				return a.loadInfo.LoadRate < b.loadInfo.LoadRate
			}
			switch {
			case a.account.LastUsedAt == nil && b.account.LastUsedAt != nil:
				return true
			case a.account.LastUsedAt != nil && b.account.LastUsedAt == nil:
				return false
			case a.account.LastUsedAt == nil && b.account.LastUsedAt == nil:
				return false
			default:
				return a.account.LastUsedAt.Before(*b.account.LastUsedAt)
			}
		})
		shuffleWithinSortGroups(available)

		selectionOrder := make([]accountWithLoad, 0, len(available))
		if requireCompact {
			appendTier := func(out []accountWithLoad, tier int) []accountWithLoad {
				for _, item := range available {
					if openAICompactSupportTier(item.account) == tier {
						out = append(out, item)
					}
				}
				return out
			}
			selectionOrder = appendTier(selectionOrder, 2)
			selectionOrder = appendTier(selectionOrder, 1)
			// tier 0 候选作为兜底追加：DB recheck 时若发现 cache tier 0 实际
			// 已升级为 1/2（探测刚跑完，cache 尚未刷新），仍可正常命中。
			selectionOrder = appendTier(selectionOrder, 0)
		} else {
			selectionOrder = append(selectionOrder, available...)
		}

		for _, item := range selectionOrder {
			fresh := s.resolveFreshSchedulableOpenAIAccount(ctx, item.account, requestedModel, false, requiredCapability)
			if fresh == nil {
				continue
			}
			fresh = s.recheckSelectedOpenAIAccountFromDB(ctx, fresh, requestedModel, requireCompact, requiredCapability)
			if fresh == nil {
				continue
			}
			var ok bool
			fresh, ok = s.resolveOpenAIAccountForPrivacyRequirement(ctx, fresh, schedGroup)
			if !ok {
				continue
			}
			if needsUpstreamCheck && s.isUpstreamModelRestrictedByChannel(ctx, *groupID, fresh, requestedModel, requireCompact) {
				continue
			}
			result, err := s.tryAcquireAccountSlot(ctx, fresh.ID, fresh.Concurrency)
			if err == nil && result != nil && result.Acquired {
				selection, selectErr := s.newAcquiredSelectionResult(ctx, fresh, result.ReleaseFunc)
				if selectErr != nil {
					return nil, true, selectErr
				}
				if sessionHash != "" {
					_ = s.setStickySessionAccountID(ctx, groupID, sessionHash, fresh.ID, openaiStickySessionTTL)
				}
				return selection, true, nil
			}
		}
		return nil, true, nil
	}

	loadMap, err := s.concurrencyService.GetAccountsLoadBatch(ctx, accountLoads)
	if err != nil {
		ordered := append([]*Account(nil), candidates...)
		sortAccountsByPriorityAndLastUsed(ordered, false)
		if requireCompact {
			ordered = prioritizeOpenAICompactAccounts(ordered)
		}
		for _, acc := range ordered {
			fresh := s.resolveFreshSchedulableOpenAIAccount(ctx, acc, requestedModel, false, requiredCapability)
			if fresh == nil {
				continue
			}
			fresh = s.recheckSelectedOpenAIAccountFromDB(ctx, fresh, requestedModel, requireCompact, requiredCapability)
			if fresh == nil {
				continue
			}
			var ok bool
			fresh, ok = s.resolveOpenAIAccountForPrivacyRequirement(ctx, fresh, schedGroup)
			if !ok {
				continue
			}
			if needsUpstreamCheck && s.isUpstreamModelRestrictedByChannel(ctx, *groupID, fresh, requestedModel, requireCompact) {
				continue
			}
			result, err := s.tryAcquireAccountSlot(ctx, fresh.ID, fresh.Concurrency)
			if err == nil && result != nil && result.Acquired {
				selection, selectErr := s.newAcquiredSelectionResult(ctx, fresh, result.ReleaseFunc)
				if selectErr != nil {
					return nil, selectErr
				}
				if sessionHash != "" {
					_ = s.setStickySessionAccountID(ctx, groupID, sessionHash, fresh.ID, openaiStickySessionTTL)
				}
				return selection, nil
			}
		}
	} else {
		if selection, attempted, selectErr := tryAcquireFromLoadMap(loadMap); selectErr != nil {
			return nil, selectErr
		} else if selection != nil {
			return selection, nil
		} else if attempted {
			if freshLoadMap, loadErr := s.concurrencyService.GetAccountsLoadBatchFresh(ctx, accountLoads); loadErr == nil {
				if selection, _, selectErr := tryAcquireFromLoadMap(freshLoadMap); selectErr != nil {
					return nil, selectErr
				} else if selection != nil {
					return selection, nil
				}
			}
		}
	}

	// ============ Layer 3: Fallback wait ============
	sortAccountsByPriorityAndLastUsed(candidates, false)
	if requireCompact {
		candidates = prioritizeOpenAICompactAccounts(candidates)
	}
	for _, acc := range candidates {
		fresh := s.resolveFreshSchedulableOpenAIAccount(ctx, acc, requestedModel, false, requiredCapability)
		if fresh == nil {
			continue
		}
		fresh = s.recheckSelectedOpenAIAccountFromDB(ctx, fresh, requestedModel, requireCompact, requiredCapability)
		if fresh == nil {
			continue
		}
		var ok bool
		fresh, ok = s.resolveOpenAIAccountForPrivacyRequirement(ctx, fresh, schedGroup)
		if !ok {
			continue
		}
		if needsUpstreamCheck && s.isUpstreamModelRestrictedByChannel(ctx, *groupID, fresh, requestedModel, requireCompact) {
			continue
		}
		return s.newSelectionResult(ctx, fresh, false, nil, &AccountWaitPlan{
			AccountID:      fresh.ID,
			MaxConcurrency: fresh.Concurrency,
			Timeout:        cfg.FallbackWaitTimeout,
			MaxWaiting:     cfg.FallbackMaxWaiting,
		})
	}

	if requireCompact && baseCandidateCount > 0 {
		return nil, ErrNoAvailableCompactAccounts
	}
	return nil, ErrNoAvailableAccounts
}

func (s *OpenAIGatewayService) listSchedulableAccounts(ctx context.Context, groupID *int64) ([]Account, error) {
	if s.schedulerSnapshot != nil {
		accounts, _, err := s.schedulerSnapshot.ListSchedulableAccounts(ctx, groupID, PlatformOpenAI, false)
		return accounts, err
	}
	var accounts []Account
	var err error
	if s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
		accounts, err = s.accountRepo.ListSchedulableByPlatform(ctx, PlatformOpenAI)
	} else if groupID != nil {
		accounts, err = s.accountRepo.ListSchedulableByGroupIDAndPlatform(ctx, *groupID, PlatformOpenAI)
	} else {
		accounts, err = s.accountRepo.ListSchedulableUngroupedByPlatform(ctx, PlatformOpenAI)
	}
	if err != nil {
		return nil, fmt.Errorf("query accounts failed: %w", err)
	}
	return accounts, nil
}

func (s *OpenAIGatewayService) tryAcquireAccountSlot(ctx context.Context, accountID int64, maxConcurrency int) (*AcquireResult, error) {
	if s.concurrencyService == nil {
		return &AcquireResult{Acquired: true, ReleaseFunc: func() {}}, nil
	}
	return s.concurrencyService.AcquireAccountSlot(ctx, accountID, maxConcurrency)
}

func (s *OpenAIGatewayService) resolveFreshSchedulableOpenAIAccount(ctx context.Context, account *Account, requestedModel string, requireCompact bool, requiredCapability OpenAIEndpointCapability) *Account {
	if account == nil {
		return nil
	}

	fresh := account
	if s.schedulerSnapshot != nil {
		current, err := s.getSchedulableAccount(ctx, account.ID)
		if err != nil || current == nil {
			return nil
		}
		fresh = current
	}

	if !isOpenAIAccountEligibleForRequest(ctx, fresh, requestedModel, requireCompact, requiredCapability) {
		return nil
	}
	if s.isOpenAIAccountRuntimeBlocked(fresh) {
		return nil
	}
	return fresh
}

func (s *OpenAIGatewayService) refreshSelectedOpenAIAccountFromDB(ctx context.Context, account *Account) *Account {
	if account == nil {
		return nil
	}
	if s.schedulerSnapshot == nil || s.accountRepo == nil {
		return account
	}
	latest, err := s.accountRepo.GetByID(ctx, account.ID)
	if err != nil || latest == nil {
		return nil
	}
	return latest
}

func (s *OpenAIGatewayService) recheckOpenAIAccountEligibility(ctx context.Context, account *Account, requestedModel string, requireCompact bool, requiredCapability OpenAIEndpointCapability) *Account {
	if account == nil {
		return nil
	}
	if !isOpenAIAccountEligibleForRequest(ctx, account, requestedModel, requireCompact, requiredCapability) {
		return nil
	}
	if s.isOpenAIAccountRuntimeBlocked(account) {
		return nil
	}
	return account
}

func (s *OpenAIGatewayService) recheckSelectedOpenAIAccountFromDB(ctx context.Context, account *Account, requestedModel string, requireCompact bool, requiredCapability OpenAIEndpointCapability) *Account {
	return s.recheckOpenAIAccountEligibility(ctx, s.refreshSelectedOpenAIAccountFromDB(ctx, account), requestedModel, requireCompact, requiredCapability)
}

func (s *OpenAIGatewayService) openAIStickyAccountMatchesSchedulingGroup(account *Account, groupID *int64) bool {
	if account == nil {
		return false
	}
	if s != nil && s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
		return true
	}
	return openAIStickyAccountMatchesGroup(account, groupID)
}

func (s *OpenAIGatewayService) getSchedulableAccount(ctx context.Context, accountID int64) (*Account, error) {
	var (
		account *Account
		err     error
	)
	if s.schedulerSnapshot != nil {
		account, err = s.schedulerSnapshot.GetAccount(ctx, accountID)
	} else {
		account, err = s.accountRepo.GetByID(ctx, accountID)
	}
	if err != nil || account == nil {
		return account, err
	}
	return account, nil
}

func (s *OpenAIGatewayService) hydrateSelectedAccount(ctx context.Context, account *Account) (*Account, error) {
	if account == nil || s.schedulerSnapshot == nil {
		return account, nil
	}
	hydrated, err := s.schedulerSnapshot.GetAccount(ctx, account.ID)
	if err != nil {
		return nil, err
	}
	if hydrated == nil {
		return nil, fmt.Errorf("selected openai account %d not found during hydration", account.ID)
	}
	return hydrated, nil
}

func (s *OpenAIGatewayService) newSelectionResult(ctx context.Context, account *Account, acquired bool, release func(), waitPlan *AccountWaitPlan) (*AccountSelectionResult, error) {
	hydrated, err := s.hydrateSelectedAccount(ctx, account)
	if err != nil {
		return nil, err
	}
	return &AccountSelectionResult{
		Account:     hydrated,
		Acquired:    acquired,
		ReleaseFunc: release,
		WaitPlan:    waitPlan,
	}, nil
}

func (s *OpenAIGatewayService) newAcquiredSelectionResult(ctx context.Context, account *Account, release func()) (*AccountSelectionResult, error) {
	selection, err := s.newSelectionResult(ctx, account, true, release, nil)
	if err != nil && release != nil {
		release()
	}
	return selection, err
}

func (s *OpenAIGatewayService) schedulingConfig() config.GatewaySchedulingConfig {
	if s.cfg != nil {
		return s.cfg.Gateway.Scheduling
	}
	return config.GatewaySchedulingConfig{
		StickySessionMaxWaiting:  3,
		StickySessionWaitTimeout: 45 * time.Second,
		FallbackWaitTimeout:      30 * time.Second,
		FallbackMaxWaiting:       100,
		LoadBatchEnabled:         true,
		SlotCleanupInterval:      30 * time.Second,
	}
}

// GetAccessToken gets the access token for an OpenAI account
func (s *OpenAIGatewayService) GetAccessToken(ctx context.Context, account *Account) (string, string, error) {
	switch account.Type {
	case AccountTypeOAuth, AccountTypeSetupToken:
		if account.IsOpenAIAgentIdentity() {
			return "", "oauth", nil
		}
		// 使用 TokenProvider 获取缓存的 token；setup-token 无 refresh_token 时会直接读取 access_token。
		if s.openAITokenProvider != nil {
			accessToken, err := s.openAITokenProvider.GetAccessToken(ctx, account)
			if err != nil {
				return "", "", err
			}
			return accessToken, "oauth", nil
		}
		// 降级：TokenProvider 未配置时只在最终 Codex 请求边界读取 bearer。
		accessToken := account.GetOpenAICodexBearerToken()
		if accessToken == "" {
			return "", "", errors.New("access_token not found in credentials")
		}
		return accessToken, "oauth", nil
	case AccountTypeAPIKey:
		apiKey := account.GetOpenAIApiKey()
		if apiKey == "" {
			return "", "", errors.New("api_key not found in credentials")
		}
		return apiKey, "apikey", nil
	default:
		return "", "", fmt.Errorf("unsupported account type: %s", account.Type)
	}
}

func (s *OpenAIGatewayService) shouldFailoverUpstreamError(statusCode int) bool {
	switch statusCode {
	case 401, 402, 403, 429, 529:
		return true
	default:
		return statusCode >= 500
	}
}

func (s *OpenAIGatewayService) shouldFailoverOpenAIUpstreamResponse(statusCode int, upstreamMsg string, upstreamBody []byte) bool {
	if s.shouldFailoverUpstreamError(statusCode) {
		return true
	}
	return isOpenAITransientProcessingError(statusCode, upstreamMsg, upstreamBody)
}

func marshalOpenAIUpstreamJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	if len(out) > 0 && out[len(out)-1] == '\n' {
		out = out[:len(out)-1]
	}
	return out, nil
}

func openAIUpstreamErrorBodyReadLimitForConfig(cfg *config.Config) int64 {
	return openAIUpstreamErrorBodyReadLimit
}

func (s *OpenAIGatewayService) readUpstreamErrorBody(resp *http.Response) []byte {
	if resp == nil || resp.Body == nil {
		return nil
	}
	cfg := (*config.Config)(nil)
	if s != nil {
		cfg = s.cfg
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, openAIUpstreamErrorBodyReadLimitForConfig(cfg)))
	return body
}

func (s *OpenAIGatewayService) handleFailoverSideEffects(ctx context.Context, resp *http.Response, account *Account, responseBody []byte, requestedModel ...string) {
	if len(requestedModel) > 0 {
		s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, responseBody, requestedModel[0])
		return
	}
	s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, responseBody)
}

// Forward forwards request to OpenAI API
func (s *OpenAIGatewayService) Forward(ctx context.Context, c *gin.Context, account *Account, body []byte) (*OpenAIForwardResult, error) {
	if GetOpenAIClientTransport(c) != OpenAIClientTransportWS {
		ctx = withHTTPAttemptAuthority(ctx)
	}
	startTime := time.Now()
	// A Gin context spans scheduler failover attempts, including OAuth to API-key
	// transitions. Never let a prior attempt's response mapping leak forward.
	clearOpenAIResponsesNamespaceNames(c)

	restrictionResult := s.detectCodexClientRestriction(c, account)
	apiKeyID := getAPIKeyIDFromContext(c)
	logCodexCLIOnlyDetection(ctx, c, account, apiKeyID, restrictionResult, body)
	if restrictionResult.Enabled && !restrictionResult.Matched {
		MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalPolicyDenied)
		c.JSON(http.StatusForbidden, gin.H{
			"error": gin.H{
				"type":    "forbidden_error",
				"message": "This account only allows Codex official clients",
			},
		})
		return nil, errors.New("codex_cli_only restriction: only codex official clients are allowed")
	}

	normalizedBody, normalized, err := normalizeOpenAICodexCompactReasoningEffortForAccount(c, account, body)
	if err != nil {
		return nil, err
	}
	if normalized {
		body = normalizedBody
	}

	originalBody := body
	requestView := newOpenAIRequestView(body)
	reqModel, reqStream, promptCacheKey := requestView.Model, requestView.Stream, requestView.PromptCacheKey
	originalModel := reqModel
	isCompactRequest := isOpenAIResponsesCompactPath(c)

	compatMessagesBridge := isOpenAICompatMessagesBridgeBody(body)
	setOpenAICompatMessagesBridgeContext(c, compatMessagesBridge)

	isCodexCLI := openai.IsCodexOfficialClientByHeaders(c.GetHeader("User-Agent"), c.GetHeader("originator")) || (s.cfg != nil && s.cfg.Gateway.ForceCodexCLI)
	wsDecision := s.getOpenAIWSProtocolResolver().Resolve(account)
	clientTransport := GetOpenAIClientTransport(c)
	// 仅允许 WS 入站请求走 WS 上游，避免出现 HTTP -> WS 协议混用。
	wsDecision = resolveOpenAIWSDecisionByClientTransport(wsDecision, clientTransport)
	if c != nil {
		c.Set("openai_ws_transport_decision", string(wsDecision.Transport))
		c.Set("openai_ws_transport_reason", wsDecision.Reason)
	}
	if wsDecision.Transport == OpenAIUpstreamTransportResponsesWebsocketV2 {
		logOpenAIWSModeDebug(
			"selected account_id=%d account_type=%s transport=%s reason=%s model=%s stream=%v",
			account.ID,
			account.Type,
			normalizeOpenAIWSLogValue(string(wsDecision.Transport)),
			normalizeOpenAIWSLogValue(wsDecision.Reason),
			reqModel,
			reqStream,
		)
	}
	// 当前仅支持 WSv2；WSv1 命中时直接返回错误，避免出现“配置可开但行为不确定”。
	if wsDecision.Transport == OpenAIUpstreamTransportResponsesWebsocket {
		if c != nil {
			MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalFeatureGate)
			c.JSON(http.StatusBadRequest, gin.H{
				"error": gin.H{
					"type":    "invalid_request_error",
					"message": "OpenAI WSv1 is temporarily unsupported. Please enable responses_websockets_v2.",
				},
			})
		}
		return nil, errors.New("openai ws v1 is temporarily unsupported; use ws v2")
	}
	if account.IsOpenAIOAuthLike() {
		// OAuth-like 请求必须先校验原始 body，避免 sjson patch 把 trailing tokens 清洗成合法 JSON。
		if _, err := decodeOpenAIRequestBodyMapUseNumber(originalBody); err != nil {
			return nil, err
		}
	}
	passthroughEnabled := account.IsOpenAIPassthroughEnabled()
	if passthroughEnabled {
		// API-key passthrough remains on the native Responses wire boundary, so
		// apply the same upstream item-ID contract before forwarding it.
		if account.Platform == PlatformOpenAI && account.Type == AccountTypeAPIKey {
			sanitizedBody, changed, sanitizeErr := sanitizeOpenAIResponsesInputItemIDs(body)
			if sanitizeErr != nil {
				return nil, fmt.Errorf("sanitize OpenAI Responses input item IDs: %w", sanitizeErr)
			}
			if changed {
				body = sanitizedBody
				originalBody = sanitizedBody
			}
		}
		// 透传分支只需要轻量提取字段，避免热路径全量 Unmarshal。
		mappedModel := account.GetMappedModel(reqModel)
		reasoningEffort := extractOpenAIReasoningEffortFromBody(body, reqModel, mappedModel)
		reasoningEffort = ApplyThinkingEnabledFallback(reasoningEffort, body, mappedModel)
		return s.forwardOpenAIPassthrough(ctx, c, account, originalBody, reqModel, reasoningEffort, reqStream, startTime)
	}

	bodyModified := false
	var reqBody map[string]any
	ensureReqBody := func() (map[string]any, error) {
		if requestView.HasPatches() {
			patchedBody, patchErr := requestView.ApplyPatches()
			if patchErr != nil {
				return nil, patchErr
			}
			body = patchedBody
			requestView = newOpenAIRequestView(body)
			reqBody = nil
			bodyModified = false
		}
		if reqBody != nil {
			return reqBody, nil
		}
		var decoded map[string]any
		var decodeErr error
		// OAuth 末端 allowlist 会复制允许字段的 raw JSON；UseNumber 避免中途改写时大整数先被 float64 舍入。
		if account != nil && account.IsOpenAIOAuthLike() {
			decoded, decodeErr = requestView.DecodeUseNumber(c)
		} else {
			decoded, decodeErr = requestView.Decode(c)
		}
		if decodeErr != nil {
			return nil, decodeErr
		}
		reqBody = decoded
		return reqBody, nil
	}
	markPatchSet := func(path string, value any) {
		bodyModified = true
		if requestView.patchesDisabled {
			if reqBody != nil {
				setOpenAIRequestMapPath(reqBody, path, value)
			}
			return
		}
		requestView.MarkPatchSet(path, value)
	}
	markPatchDelete := func(path string) {
		bodyModified = true
		if requestView.patchesDisabled {
			if reqBody != nil {
				deleteOpenAIRequestMapPath(reqBody, path)
			}
			return
		}
		requestView.MarkPatchDelete(path)
	}
	disablePatch := func() {
		requestView.DisablePatches()
	}
	markDecodedModified := func() {
		bodyModified = true
		disablePatch()
	}

	apiKey := getAPIKeyFromContext(c)
	imageGenerationAllowed := GroupAllowsImageGeneration(nil)
	if apiKey != nil {
		imageGenerationAllowed = GroupAllowsImageGeneration(apiKey.Group)
	}
	codexImageGenerationExplicitToolPolicy := codexImageGenerationExplicitToolPolicyAllow
	if isCodexCLI {
		codexImageGenerationExplicitToolPolicy = account.CodexImageGenerationExplicitToolPolicy()
	}
	codexImageGenerationBridgeEnabled := isCodexCLI && !isCompactRequest && imageGenerationAllowed && codexImageGenerationExplicitToolPolicy != codexImageGenerationExplicitToolPolicyStrip && s.isCodexImageGenerationBridgeEnabled(ctx, account, apiKey)
	imageIntent := false
	if isCodexCLI && codexImageGenerationExplicitToolPolicy == codexImageGenerationExplicitToolPolicyStrip {
		decoded, decodeErr := ensureReqBody()
		if decodeErr != nil {
			return nil, decodeErr
		}
		if stripOpenAIImageGenerationTools(decoded) {
			markDecodedModified()
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Stripped /responses image_generation tool for Codex client by account policy")
		}
	}

	instructions := gjson.GetBytes(body, "instructions")
	instructionsEmpty := !instructions.Exists() || instructions.Type != gjson.String || strings.TrimSpace(instructions.String()) == ""
	if instructionsEmpty && !compatMessagesBridge && !account.IsOpenAIOAuthLike() {
		markPatchSet("instructions", "You are a helpful coding assistant.")
	}

	billingModel := account.GetMappedModel(reqModel)
	if billingModel != reqModel {
		logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Model mapping applied: %s -> %s (account: %s, isCodexCLI: %v)", reqModel, billingModel, account.Name, isCodexCLI)
		reqModel = billingModel
		markPatchSet("model", billingModel)
	}
	upstreamModel := billingModel
	if account.IsOpenAIOAuthLike() && isCompactRequest {
		// Compact terminal policy omits stream, so response handling must follow the normalized body.
		reqStream = false
	}
	compactMapped := false
	if isCompactRequest {
		compactMappedModel := resolveOpenAICompactForwardModel(account, billingModel)
		if compactMappedModel != "" && compactMappedModel != billingModel {
			compactMapped = true
			billingModel = compactMappedModel
			upstreamModel = compactMappedModel
			reqModel = compactMappedModel
			markPatchSet("model", compactMappedModel)
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Compact model mapping applied: %s -> %s (account: %s, isCodexCLI: %v)", billingModel, compactMappedModel, account.Name, isCodexCLI)
		}
	}
	if !compactMapped {
		modelForNormalize := reqModel
		if modelForNormalize == "" {
			modelForNormalize = requestView.Model
		}
		upstreamModel = normalizeOpenAIModelForUpstream(account, modelForNormalize)
		if upstreamModel != "" && upstreamModel != modelForNormalize {
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Upstream model resolved: %s -> %s (account: %s, type: %s, isCodexCLI: %v)", modelForNormalize, upstreamModel, account.Name, account.Type, isCodexCLI)
			reqModel = upstreamModel
			markPatchSet("model", upstreamModel)
		}
	}
	if strings.TrimSpace(gjson.GetBytes(body, "reasoning.effort").String()) == "minimal" {
		markPatchSet("reasoning.effort", "none")
		logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Normalized reasoning.effort: minimal -> none (account: %s)", account.Name)
	}

	if isCodexSparkModel(upstreamModel) && openAIRequestBodyHasImageGenerationTooling(body) {
		decoded, decodeErr := ensureReqBody()
		if decodeErr != nil {
			return nil, decodeErr
		}
		if stripCodexSparkImageGenerationTooling(decoded, upstreamModel) {
			markDecodedModified()
		}
	}

	imageIntent = classifyOpenAIForwardImageIntent(reqModel, upstreamModel, body, reqBody)
	if imageIntent && !imageGenerationAllowed {
		MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalFeatureGate)
		c.JSON(http.StatusForbidden, gin.H{"error": gin.H{"type": "permission_error", "message": ImageGenerationPermissionMessage()}})
		return nil, errors.New("image generation disabled for group")
	}

	if imageGenerationAllowed && (codexImageGenerationBridgeEnabled || isOpenAIImageGenerationModel(requestView.Model) || openAIRequestBodyImageGenerationToolNeedsNormalization(body) || isOpenAIImageGenerationModel(upstreamModel)) {
		decoded, decodeErr := ensureReqBody()
		if decodeErr != nil {
			return nil, decodeErr
		}
		if codexImageGenerationBridgeEnabled && ensureOpenAIResponsesImageGenerationTool(decoded) {
			markDecodedModified()
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Injected /responses image_generation tool for Codex client")
		}
		if normalizeOpenAIResponsesImageGenerationTools(decoded) {
			markDecodedModified()
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Normalized /responses image_generation tool payload")
		}
		if normalizeOpenAIResponsesImageOnlyModel(decoded) {
			markDecodedModified()
			if model, ok := decoded["model"].(string); ok {
				upstreamModel = strings.TrimSpace(model)
			}
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Normalized /responses image-only model request inbound_model=%s image_model=%s upstream_model=%s", requestView.Model, billingModel, upstreamModel)
		}
		if err := validateOpenAIResponsesImageModel(decoded, upstreamModel); err != nil {
			setOpsUpstreamError(c, http.StatusBadRequest, err.Error(), "")
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "message": err.Error(), "param": "model"}})
			return nil, err
		}
		if hasOpenAIImageGenerationTool(decoded) {
			imageIntent = true
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI] /responses image_generation request inbound_model=%s mapped_model=%s account_type=%s", requestView.Model, upstreamModel, account.Type)
		}
		if codexImageGenerationBridgeEnabled && applyCodexImageGenerationBridgeInstructions(decoded) {
			markDecodedModified()
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Added Codex image_generation bridge instructions")
		}
	} else if imageGenerationAllowed && imageIntent && openAIRequestBodyHasImageGenerationTool(body) {
		// 完整 image_generation tool 只做 raw 计费读取，校验/桥接/旧字段迁移命中时才展开大 input map。
		logger.LegacyPrintf("service.openai_gateway", "[OpenAI] /responses image_generation request inbound_model=%s mapped_model=%s account_type=%s", requestView.Model, upstreamModel, account.Type)
	}

	if isCodexSparkModel(upstreamModel) && openAIRequestBodyMayContainImageInput(body) {
		decoded, decodeErr := ensureReqBody()
		if decodeErr != nil {
			return nil, decodeErr
		}
		if err := validateCodexSparkInput(decoded, upstreamModel); err != nil {
			setOpsUpstreamError(c, http.StatusBadRequest, err.Error(), "")
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "message": err.Error(), "param": "input"}})
			return nil, err
		}
	}

	if account.IsOpenAIOAuthLike() {
		decoded, decodeErr := ensureReqBody()
		if decodeErr != nil {
			return nil, decodeErr
		}
		if shouldNormalizeOpenAIResponsesNamespaces(account, wsDecision.Transport, clientTransport) {
			changed, namespaceErr := normalizeOpenAIResponsesNamespaces(c, decoded)
			if namespaceErr != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
					"type": "invalid_request_error", "message": namespaceErr.Error(), "param": "tools",
				}})
				return nil, namespaceErr
			}
			if changed {
				markDecodedModified()
			}
		}
		codexResult := codexTransformResult{}
		if compatMessagesBridge {
			codexResult = applyCodexOAuthTransformWithOptions(decoded, codexOAuthTransformOptions{IsCodexCLI: isCodexCLI, IsCompact: isCompactRequest, SkipDefaultInstructions: true, PreserveToolCallIDs: true})
			ensureCodexOAuthInstructionsField(decoded)
			markDecodedModified()
		} else {
			codexResult = applyCodexOAuthTransform(decoded, isCodexCLI, isCompactRequest)
		}
		if codexResult.Modified {
			markDecodedModified()
		}
		if codexResult.NormalizedModel != "" {
			upstreamModel = codexResult.NormalizedModel
		}
		if codexResult.PromptCacheKey != "" {
			promptCacheKey = codexResult.PromptCacheKey
		}
	}

	if !SupportsVerbosity(upstreamModel) && gjson.GetBytes(body, "text.verbosity").Exists() {
		markPatchDelete("text.verbosity")
	}

	if !isCodexCLI {
		maxOutputTokens := gjson.GetBytes(body, "max_output_tokens")
		if maxOutputTokens.Exists() {
			switch account.Platform {
			case PlatformOpenAI:
				// Preserve Responses-native output limits. Compatible upstreams that
				// explicitly reject the field get one bounded normalization retry.
			case PlatformAnthropic:
				decoded, decodeErr := ensureReqBody()
				if decodeErr != nil {
					return nil, decodeErr
				}
				delete(decoded, "max_output_tokens")
				if _, hasMaxTokens := decoded["max_tokens"]; !hasMaxTokens {
					decoded["max_tokens"] = maxOutputTokens.Value()
				}
				markDecodedModified()
			case PlatformGemini:
				markPatchDelete("max_output_tokens")
			default:
				markPatchDelete("max_output_tokens")
			}
		}
		if gjson.GetBytes(body, "max_completion_tokens").Exists() && (account.Type == AccountTypeAPIKey || account.Platform != PlatformOpenAI) {
			markPatchDelete("max_completion_tokens")
		}
		if !account.IsOpenAIOAuthLike() {
			for _, unsupportedField := range []string{"prompt_cache_retention", "safety_identifier"} {
				if gjson.GetBytes(body, unsupportedField).Exists() {
					markPatchDelete(unsupportedField)
				}
			}
		}
	}
	if account.Platform == PlatformOpenAI && account.Type == AccountTypeAPIKey && gjson.GetBytes(body, "metadata").Exists() {
		markPatchDelete("metadata")
	}
	if !account.IsOpenAIOAuthLike() && wsDecision.Transport != OpenAIUpstreamTransportResponsesWebsocketV2 {
		for _, field := range []string{"previous_response_id", "generate", "type"} {
			if gjson.GetBytes(body, field).Exists() {
				markPatchDelete(field)
			}
		}
	}
	if openAIRequestBodyMayContainEmptyBase64InputImage(body) {
		decoded, decodeErr := ensureReqBody()
		if decodeErr != nil {
			return nil, decodeErr
		}
		if sanitizeEmptyBase64InputImagesInOpenAIRequestBodyMap(decoded) {
			markDecodedModified()
		}
	}

	rawTier := requestView.ServiceTier
	// Per-key override: force service_tier=priority before fast policy.
	if shouldForceOpenAIPriorityTier(apiKey) && rawTier != "priority" {
		rawTier = "priority"
		markPatchSet("service_tier", rawTier)
	}
	if rawTier != "" {
		if normTier := normalizedOpenAIServiceTierValue(rawTier); normTier != "" {
			action, errMsg := s.evaluateOpenAIFastPolicy(ctx, account, upstreamModel, normTier)
			switch action {
			case BetaPolicyActionBlock:
				msg := errMsg
				if msg == "" {
					msg = fmt.Sprintf("openai service_tier=%s is not allowed for model %s", normTier, upstreamModel)
				}
				blocked := &OpenAIFastBlockedError{Message: msg}
				writeOpenAIFastPolicyBlockedResponse(c, blocked)
				return nil, blocked
			case BetaPolicyActionFilter:
				markPatchDelete("service_tier")
			default:
				if normTier != rawTier {
					markPatchSet("service_tier", normTier)
				}
			}
		}
	}

	if bodyModified {
		if requestView.HasPatches() {
			if patchedBody, patchErr := requestView.ApplyPatches(); patchErr == nil {
				body = patchedBody
				requestView = newOpenAIRequestView(body)
				reqBody = nil
				bodyModified = false
			}
		}
		if bodyModified {
			decoded, decodeErr := ensureReqBody()
			if decodeErr != nil {
				return nil, decodeErr
			}
			var marshalErr error
			body, marshalErr = marshalOpenAIUpstreamJSON(decoded)
			if marshalErr != nil {
				return nil, fmt.Errorf("serialize request body: %w", marshalErr)
			}
			requestView = newOpenAIRequestView(body)
		}
	}
	// API-key native Responses accepts only persisted upstream item IDs. Apply
	// this protocol-specific cleanup only when the request stays on Responses;
	// raw Chat fallback continues to consume originalBody unchanged below.
	if account.Platform == PlatformOpenAI && account.Type == AccountTypeAPIKey &&
		openai_compat.ShouldUseResponsesAPIForModel(account.Extra, upstreamModel) {
		sanitizedBody, changed, sanitizeErr := sanitizeOpenAIResponsesInputItemIDs(body)
		if sanitizeErr != nil {
			return nil, fmt.Errorf("sanitize OpenAI Responses input item IDs: %w", sanitizeErr)
		}
		if changed {
			body = sanitizedBody
			requestView = newOpenAIRequestView(body)
			reqBody = nil
		}
	}
	// Capability checks must follow model normalization (for example image-only
	// requests rewriting gpt-image-* to the Responses-capable text model), but
	// raw Chat fallback must convert the original Responses body before APIKey
	// field stripping drops fields such as max_output_tokens.
	if account.Type == AccountTypeAPIKey && !openai_compat.ShouldUseResponsesAPIForModel(account.Extra, upstreamModel) {
		return s.forwardResponsesViaRawChatCompletions(ctx, c, account, originalBody, originalModel, billingModel, upstreamModel)
	}
	imageBillingModel := ""
	imageSizeTier := ""
	imageInputSize := ""
	if imageIntent {
		var imageCfg OpenAIResponsesImageBillingConfig
		var imageCfgErr error
		if reqBody != nil {
			imageCfg, imageCfgErr = resolveOpenAIResponsesImageBillingConfigDetailed(reqBody, billingModel)
		} else {
			imageCfg, imageCfgErr = resolveOpenAIResponsesImageBillingConfigDetailedFromBody(body, billingModel)
		}
		if imageCfgErr != nil {
			setOpsUpstreamError(c, http.StatusBadRequest, imageCfgErr.Error(), "")
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"type": "invalid_request_error", "message": imageCfgErr.Error(), "param": "size"}})
			return nil, imageCfgErr
		}
		imageBillingModel = imageCfg.Model
		imageSizeTier = imageCfg.SizeTier
		imageInputSize = imageCfg.InputSize
	}

	// Get access token
	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}

	// 命中 WS 时仅走 WebSocket Mode；不再自动回退 HTTP。
	if wsDecision.Transport == OpenAIUpstreamTransportResponsesWebsocketV2 {
		// WS 分支需要结构化 payload 与重连恢复，命中后再触发 full-map decode。
		wsReqBody, err := ensureReqBody()
		if err != nil {
			return nil, err
		}
		if account.IsOpenAIOAuthLike() {
			wsReqBody["store"] = false
			wsReqBody["stream"] = true
		}
		_, hasPreviousResponseID := wsReqBody["previous_response_id"]
		logOpenAIWSModeDebug(
			"forward_start account_id=%d account_type=%s model=%s stream=%v has_previous_response_id=%v",
			account.ID,
			account.Type,
			upstreamModel,
			reqStream,
			hasPreviousResponseID,
		)
		maxAttempts := openAIWSReconnectRetryLimit + 1
		wsAttempts := 0
		var wsResult *OpenAIForwardResult
		var wsErr error
		wsLastFailureReason := ""
		wsPrevResponseRecoveryTried := false
		wsInvalidEncryptedContentRecoveryTried := false
		recoverPrevResponseNotFound := func(attempt int) bool {
			if wsPrevResponseRecoveryTried {
				return false
			}
			previousResponseID := openAIWSPayloadString(wsReqBody, "previous_response_id")
			if previousResponseID == "" {
				logOpenAIWSModeInfo(
					"reconnect_prev_response_recovery_skip account_id=%d attempt=%d reason=missing_previous_response_id previous_response_id_present=false",
					account.ID,
					attempt,
				)
				return false
			}
			if HasFunctionCallOutput(wsReqBody) {
				logOpenAIWSModeInfo(
					"reconnect_prev_response_recovery_skip account_id=%d attempt=%d reason=has_function_call_output previous_response_id_present=true",
					account.ID,
					attempt,
				)
				return false
			}
			delete(wsReqBody, "previous_response_id")
			wsPrevResponseRecoveryTried = true
			logOpenAIWSModeInfo(
				"reconnect_prev_response_recovery account_id=%d attempt=%d action=drop_previous_response_id retry=1 previous_response_id=%s previous_response_id_kind=%s",
				account.ID,
				attempt,
				truncateOpenAIWSLogValue(previousResponseID, openAIWSIDValueMaxLen),
				normalizeOpenAIWSLogValue(ClassifyOpenAIPreviousResponseIDKind(previousResponseID)),
			)
			return true
		}
		recoverInvalidEncryptedContent := func(attempt int) bool {
			if wsInvalidEncryptedContentRecoveryTried {
				return false
			}
			removedReasoningItems := trimOpenAIEncryptedReasoningItems(wsReqBody)
			if !removedReasoningItems {
				logOpenAIWSModeInfo(
					"reconnect_invalid_encrypted_content_recovery_skip account_id=%d attempt=%d reason=missing_encrypted_reasoning_items",
					account.ID,
					attempt,
				)
				return false
			}
			previousResponseID := openAIWSPayloadString(wsReqBody, "previous_response_id")
			hasFunctionCallOutput := HasFunctionCallOutput(wsReqBody)
			if previousResponseID != "" && !hasFunctionCallOutput {
				delete(wsReqBody, "previous_response_id")
			}
			wsInvalidEncryptedContentRecoveryTried = true
			logOpenAIWSModeInfo(
				"reconnect_invalid_encrypted_content_recovery account_id=%d attempt=%d action=drop_encrypted_reasoning_items retry=1 previous_response_id_present=%v previous_response_id=%s previous_response_id_kind=%s has_function_call_output=%v dropped_previous_response_id=%v",
				account.ID,
				attempt,
				previousResponseID != "",
				truncateOpenAIWSLogValue(previousResponseID, openAIWSIDValueMaxLen),
				normalizeOpenAIWSLogValue(ClassifyOpenAIPreviousResponseIDKind(previousResponseID)),
				hasFunctionCallOutput,
				previousResponseID != "" && !hasFunctionCallOutput,
			)
			return true
		}
		retryBudget := s.openAIWSRetryTotalBudget()
		retryStartedAt := time.Now()
	wsRetryLoop:
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			wsAttempts = attempt
			wsResult, wsErr = s.forwardOpenAIWSV2(
				ctx,
				c,
				account,
				wsReqBody,
				token,
				wsDecision,
				isCodexCLI,
				reqStream,
				originalModel,
				upstreamModel,
				startTime,
				attempt,
				wsLastFailureReason,
			)
			if wsErr == nil {
				break
			}
			if c != nil && c.Writer != nil && c.Writer.Written() {
				break
			}

			reason, retryable := classifyOpenAIWSReconnectReason(wsErr)
			if reason != "" {
				wsLastFailureReason = reason
			}
			// previous_response_not_found 说明续链锚点不可用：
			// 对非 function_call_output 场景，允许一次“去掉 previous_response_id 后重放”。
			if reason == "previous_response_not_found" && recoverPrevResponseNotFound(attempt) {
				continue
			}
			if reason == "invalid_encrypted_content" && recoverInvalidEncryptedContent(attempt) {
				continue
			}
			if retryable && attempt < maxAttempts {
				backoff := s.openAIWSRetryBackoff(attempt)
				if retryBudget > 0 && time.Since(retryStartedAt)+backoff > retryBudget {
					s.recordOpenAIWSRetryExhausted()
					logOpenAIWSModeInfo(
						"reconnect_budget_exhausted account_id=%d attempts=%d max_retries=%d reason=%s elapsed_ms=%d budget_ms=%d",
						account.ID,
						attempt,
						openAIWSReconnectRetryLimit,
						normalizeOpenAIWSLogValue(reason),
						time.Since(retryStartedAt).Milliseconds(),
						retryBudget.Milliseconds(),
					)
					break
				}
				s.recordOpenAIWSRetryAttempt(backoff)
				logOpenAIWSModeInfo(
					"reconnect_retry account_id=%d retry=%d max_retries=%d reason=%s backoff_ms=%d",
					account.ID,
					attempt,
					openAIWSReconnectRetryLimit,
					normalizeOpenAIWSLogValue(reason),
					backoff.Milliseconds(),
				)
				if backoff > 0 {
					timer := time.NewTimer(backoff)
					select {
					case <-ctx.Done():
						if !timer.Stop() {
							<-timer.C
						}
						wsErr = wrapOpenAIWSFallback("retry_backoff_canceled", ctx.Err())
						break wsRetryLoop
					case <-timer.C:
					}
				}
				continue
			}
			if retryable {
				s.recordOpenAIWSRetryExhausted()
				logOpenAIWSModeInfo(
					"reconnect_exhausted account_id=%d attempts=%d max_retries=%d reason=%s",
					account.ID,
					attempt,
					openAIWSReconnectRetryLimit,
					normalizeOpenAIWSLogValue(reason),
				)
			} else if reason != "" {
				s.recordOpenAIWSNonRetryableFastFallback()
				logOpenAIWSModeInfo(
					"reconnect_stop account_id=%d attempt=%d reason=%s",
					account.ID,
					attempt,
					normalizeOpenAIWSLogValue(reason),
				)
			}
			break
		}
		var requestErr *OpenAIUpstreamRequestError
		if errors.As(wsErr, &requestErr) {
			if wsResult != nil {
				wsResult.UpstreamModel = upstreamModel
				wsResult.BillingModel = billingModel
				if wsResult.ImageCount > 0 {
					wsResult.ImageSize = imageSizeTier
					wsResult.ImageInputSize = imageInputSize
					wsResult.BillingModel = imageBillingModel
				}
			}
			return wsResult, requestErr
		}
		if wsErr == nil {
			firstTokenMs := int64(0)
			hasFirstTokenMs := wsResult != nil && wsResult.FirstTokenMs != nil
			if hasFirstTokenMs {
				firstTokenMs = int64(*wsResult.FirstTokenMs)
			}
			requestID := ""
			if wsResult != nil {
				requestID = strings.TrimSpace(wsResult.RequestID)
			}
			logOpenAIWSModeDebug(
				"forward_succeeded account_id=%d request_id=%s stream=%v has_first_token_ms=%v first_token_ms=%d ws_attempts=%d",
				account.ID,
				requestID,
				reqStream,
				hasFirstTokenMs,
				firstTokenMs,
				wsAttempts,
			)
			wsResult.UpstreamModel = upstreamModel
			wsResult.BillingModel = billingModel
			if wsResult.ImageCount > 0 {
				wsResult.ImageSize = imageSizeTier
				wsResult.ImageInputSize = imageInputSize
				wsResult.BillingModel = imageBillingModel
			}
			return wsResult, nil
		}
		s.writeOpenAIWSFallbackErrorResponse(c, account, wsErr)
		return nil, wsErr
	}

	httpInvalidEncryptedContentRetryTried := false
	agentIdentityTaskRecoveryTried := false
	rejectedFieldRetryState := newOpenAIResponsesRejectedFieldRetryState(body)
	var completedError completedResponseSnapshot
	for {
		// Build upstream request
		upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
		upstreamReq, err := s.buildUpstreamRequest(upstreamCtx, c, account, body, token, reqStream, promptCacheKey, isCodexCLI)
		releaseUpstreamCtx()
		if err != nil {
			return nil, err
		}

		// Get proxy URL
		proxyURL := ""
		if account.ProxyID != nil && account.Proxy != nil {
			proxyURL = account.Proxy.URL()
		}

		// Send request
		upstreamStart := time.Now()
		resp, err := doHTTPUpstream(ctx, s.httpUpstream, upstreamReq, proxyURL, account.ID, account.Concurrency)
		SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(upstreamStart).Milliseconds())
		if err != nil {
			if restored, notAdmitted := completedError.ifRetryNotAdmitted(err); notAdmitted {
				resp = restored
			} else {
				return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false)
			}
		}

		// Handle error response
		if resp.StatusCode >= 400 {
			respBody := s.readUpstreamErrorBody(resp)
			_ = resp.Body.Close()
			resp.Body = io.NopCloser(bytes.NewReader(respBody))
			completedError = snapshotCompletedResponse(resp, respBody)

			if !agentIdentityTaskRecoveryTried && account.IsOpenAIAgentIdentity() && isAgentIdentityTaskInvalidHTTPResponse(resp.StatusCode, respBody) {
				expectedTaskID := strings.TrimSpace(account.GetCredential("task_id"))
				agentIdentityTaskRecoveryTried = true
				if recoverErr := s.recoverAgentIdentityTask(ctx, account, expectedTaskID); recoverErr != nil {
					return nil, fmt.Errorf("recover agent identity task: %w", recoverErr)
				}
				if restored, canceled := completedError.ifRetryCanceled(ctx); canceled {
					resp = restored
					respBody = completedError.body
				} else {
					continue
				}
			}
			respBody = s.redactAgentIdentitySensitiveBody(ctx, account, respBody)
			resp.Body = io.NopCloser(bytes.NewReader(respBody))
			completedError = snapshotCompletedResponse(resp, respBody)
			upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(respBody))
			upstreamMsg = sanitizeOpenAIUpstreamDiagnosticText(upstreamMsg)
			upstreamCode := extractUpstreamErrorCode(respBody)
			if !httpInvalidEncryptedContentRetryTried && resp.StatusCode == http.StatusBadRequest && isOpenAIEncryptedContextErrorCode(upstreamCode) {
				decoded, decodeErr := ensureReqBody()
				if decodeErr != nil {
					return nil, decodeErr
				}
				if trimOpenAIEncryptedReasoningItems(decoded) {
					body, err = marshalOpenAIUpstreamJSON(decoded)
					if err != nil {
						return nil, fmt.Errorf("serialize encrypted context retry body: %w", err)
					}
					httpInvalidEncryptedContentRetryTried = true
					rejectedFieldRetryState.remember(body)
					logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Retrying non-WSv2 request once after encrypted context error code=%s (account: %s)", upstreamCode, account.Name)
					if restored, canceled := completedError.ifRetryCanceled(ctx); canceled {
						resp = restored
						respBody = completedError.body
					} else {
						continue
					}
				}
				logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Skip non-WSv2 encrypted context retry because stale reasoning/compaction items are missing (account: %s)", account.Name)
			}
			if retryBody, reason, changed, retryErr := normalizeOpenAIResponsesRejectedFieldRetryBody(resp.StatusCode, body, respBody); retryErr != nil {
				return nil, fmt.Errorf("normalize rejected Responses field retry body: %w", retryErr)
			} else if changed && rejectedFieldRetryState.Allow(retryBody) {
				body = retryBody
				reqBody = nil
				logger.LegacyPrintf("service.openai_gateway", "[OpenAI] Retrying non-WSv2 request after %s (account: %s)", reason, account.Name)
				if restored, canceled := completedError.ifRetryCanceled(ctx); canceled {
					resp = restored
					respBody = completedError.body
				} else {
					continue
				}
			}
			if policy, ok := RecognizeUpstreamErrorFact(ParseHTTPUpstreamErrorFact(PlatformOpenAI, resp, respBody)); ok {
				writeRecognizedOpenAIHTTPError(c, policy.Presentation)
				return nil, newRecognizedOpenAIUpstreamRequestErrorForPolicy(policy, resp.Header.Get("x-request-id"))
			}
			if s.shouldFailoverOpenAIUpstreamResponse(resp.StatusCode, upstreamMsg, respBody) {
				upstreamDetail := ""
				if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
					maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
					if maxBytes <= 0 {
						maxBytes = 2048
					}
					upstreamDetail = sanitizeOpenAIUpstreamDiagnosticBodyForLog(respBody, maxBytes)
				}
				appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
					Platform:           account.Platform,
					AccountID:          account.ID,
					AccountName:        account.Name,
					UpstreamStatusCode: resp.StatusCode,
					UpstreamRequestID:  resp.Header.Get("x-request-id"),
					Kind:               "failover",
					Message:            upstreamMsg,
					Detail:             upstreamDetail,
				})

				s.handleFailoverSideEffects(ctx, resp, account, respBody, upstreamModel)
				return nil, newOpenAIHTTPFailoverError(
					resp,
					respBody,
					account.IsPoolMode() && (account.IsPoolModeRetryableStatus(resp.StatusCode) || isOpenAITransientProcessingError(resp.StatusCode, upstreamMsg, respBody)),
				)
			}
			return s.handleErrorResponse(ctx, resp, c, account, body, upstreamModel)
		}
		defer func() { _ = resp.Body.Close() }()

		reasoningEffort := ApplyThinkingEnabledFallback(extractOpenAIReasoningEffortFromBody(body, upstreamModel, billingModel, originalModel), body, reqModel)
		serviceTier := extractOpenAIServiceTierFromBody(body)
		// 上游接受后只保留计费需要的标量，避免响应处理期间继续保活完整 input/tools map。
		reqBody = nil

		// Handle normal response
		var usage *OpenAIUsage
		var firstTokenMs *int
		responseID := ""
		imageCount := 0
		var imageOutputSizes []string
		if reqStream {
			streamResult, err := s.handleStreamingResponse(ctx, resp, c, account, startTime, originalModel, upstreamModel)
			if err != nil {
				var failoverErr *UpstreamFailoverError
				if errors.As(err, &failoverErr) {
					return nil, err
				}
				var requestErr *OpenAIUpstreamRequestError
				if errors.As(err, &requestErr) {
					return openAIForwardResultFromStreamingResult(streamResult, resp, startTime, originalModel, billingModel, upstreamModel, serviceTier, reasoningEffort, reqStream, imageBillingModel, imageSizeTier, imageInputSize), err
				}
				if !openAIStreamingResultShouldExposeOnError(streamResult) {
					return nil, err
				}
				return openAIForwardResultFromStreamingResult(streamResult, resp, startTime, originalModel, billingModel, upstreamModel, serviceTier, reasoningEffort, reqStream, imageBillingModel, imageSizeTier, imageInputSize), err
			}
			usage = streamResult.usage
			firstTokenMs = streamResult.firstTokenMs
			responseID = strings.TrimSpace(streamResult.responseID)
			imageCount = streamResult.imageCount
			imageOutputSizes = streamResult.imageOutputSizes
		} else {
			nonStreamResult, err := s.handleNonStreamingResponse(ctx, resp, c, account, originalModel, upstreamModel)
			if err != nil {
				var requestErr *OpenAIUpstreamRequestError
				if errors.As(err, &requestErr) {
					usage := nonStreamResult.OpenAIUsage
					if usage == nil {
						usage = nonStreamResult.usage
					}
					streamResult := &openaiStreamingResult{usage: usage, responseID: nonStreamResult.responseID, imageCount: nonStreamResult.imageCount, imageOutputSizes: nonStreamResult.imageOutputSizes}
					return openAIForwardResultFromStreamingResult(streamResult, resp, startTime, originalModel, billingModel, upstreamModel, serviceTier, reasoningEffort, reqStream, imageBillingModel, imageSizeTier, imageInputSize), err
				}
				return nil, err
			}
			usage = nonStreamResult.usage
			responseID = strings.TrimSpace(nonStreamResult.responseID)
			imageCount = nonStreamResult.imageCount
			imageOutputSizes = nonStreamResult.imageOutputSizes
		}
		s.bindHTTPResponseAccount(ctx, c, account, responseID)

		// Extract and save Codex usage snapshot from response headers for full OAuth only.
		if account.IsOpenAIOAuth() {
			if snapshot := ParseCodexRateLimitHeaders(resp.Header); snapshot != nil {
				s.updateCodexUsageSnapshot(ctx, account.ID, snapshot)
			}
		}

		if usage == nil {
			usage = &OpenAIUsage{}
		}

		forwardResult := &OpenAIForwardResult{
			RequestID:       resp.Header.Get("x-request-id"),
			AttemptID:       forwardResultAttemptID(resp),
			ResponseID:      responseID,
			Usage:           *usage,
			Model:           originalModel,
			BillingModel:    billingModel,
			UpstreamModel:   upstreamModel,
			ServiceTier:     serviceTier,
			ReasoningEffort: reasoningEffort,
			Stream:          reqStream,
			OpenAIWSMode:    false,
			Duration:        time.Since(startTime),
			FirstTokenMs:    firstTokenMs,
		}
		if imageCount > 0 {
			forwardResult.ImageCount = imageCount
			forwardResult.ImageSize = imageSizeTier
			forwardResult.ImageInputSize = imageInputSize
			forwardResult.ImageOutputSizes = imageOutputSizes
			forwardResult.BillingModel = imageBillingModel
		}
		return forwardResult, nil
	}
}

func openAIForwardResultFromStreamingResult(
	streamResult *openaiStreamingResult,
	resp *http.Response,
	startTime time.Time,
	originalModel string,
	billingModel string,
	upstreamModel string,
	serviceTier *string,
	reasoningEffort *string,
	stream bool,
	imageBillingModel string,
	imageSizeTier string,
	imageInputSize string,
) *OpenAIForwardResult {
	if streamResult == nil {
		return nil
	}
	usage := streamResult.usage
	if usage == nil {
		usage = &OpenAIUsage{}
	}
	requestID := ""
	var headers http.Header
	if resp != nil {
		requestID = resp.Header.Get("x-request-id")
		headers = resp.Header.Clone()
	}
	result := &OpenAIForwardResult{
		RequestID:        requestID,
		AttemptID:        forwardResultAttemptID(resp),
		ResponseID:       strings.TrimSpace(streamResult.responseID),
		Usage:            *usage,
		Model:            originalModel,
		BillingModel:     billingModel,
		UpstreamModel:    upstreamModel,
		ServiceTier:      serviceTier,
		ReasoningEffort:  reasoningEffort,
		Stream:           stream,
		OpenAIWSMode:     false,
		ResponseHeaders:  headers,
		Duration:         time.Since(startTime),
		FirstTokenMs:     streamResult.firstTokenMs,
		ImageCount:       streamResult.imageCount,
		ImageOutputSizes: streamResult.imageOutputSizes,
	}
	if streamResult.imageCount > 0 {
		result.ImageSize = imageSizeTier
		result.ImageInputSize = imageInputSize
		result.BillingModel = imageBillingModel
	}
	return result
}

func (s *OpenAIGatewayService) forwardOpenAIPassthrough(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	reqModel string,
	reasoningEffort *string,
	reqStream bool,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	if account == nil {
		return nil, errors.New("account is nil")
	}
	if !account.IsOpenAIApiKey() {
		return nil, fmt.Errorf("openai native passthrough requires APIKey account, got=%s", account.Type)
	}

	upstreamPassthroughModel := ""
	billingModel := ""
	if isOpenAIResponsesCompactPath(c) {
		compactMappedModel := resolveOpenAICompactForwardModel(account, reqModel)
		billingModel = strings.TrimSpace(compactMappedModel)
		if compactMappedModel != "" && compactMappedModel != reqModel {
			nextBody, setErr := sjson.SetBytes(body, "model", compactMappedModel)
			if setErr != nil {
				return nil, fmt.Errorf("set compact passthrough model: %w", setErr)
			}
			body = nextBody
			upstreamPassthroughModel = compactMappedModel
		}
	}

	sanitizedBody, sanitized, err := sanitizeEmptyBase64InputImagesInOpenAIBody(body)
	if err != nil {
		return nil, err
	}
	if sanitized {
		body = sanitizedBody
	}

	// Apply OpenAI fast policy to the passthrough body (filter/block by service_tier).
	// 统一使用 upstream 视角的 model：APIKey 透传路径下 body 已经过 compact 映射，
	// body 中的 model 字段即上游真正会看到的 slug。
	// 这样可以与 chat-completions / messages / native /responses 入口的
	// upstreamModel 保持一致，避免 whitelist 命中差异。当 body 中没有
	// model 字段时退回 reqModel。
	policyModel := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if policyModel == "" {
		policyModel = reqModel
	}
	apiKey := getAPIKeyFromContext(c)
	forcedBody, forceErr := forceOpenAIPriorityTierInBody(apiKey, body)
	if forceErr != nil {
		return nil, forceErr
	}
	body = forcedBody
	updatedBody, policyErr := s.applyOpenAIFastPolicyToBody(ctx, account, policyModel, body)
	if policyErr != nil {
		var blocked *OpenAIFastBlockedError
		if errors.As(policyErr, &blocked) {
			writeOpenAIFastPolicyBlockedResponse(c, blocked)
		}
		return nil, policyErr
	}
	body = updatedBody

	if IsImageGenerationIntent(openAIResponsesEndpoint, reqModel, body) && !GroupAllowsImageGeneration(apiKeyGroup(apiKey)) {
		MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalFeatureGate)
		c.JSON(http.StatusForbidden, gin.H{
			"error": gin.H{
				"type":    "permission_error",
				"message": ImageGenerationPermissionMessage(),
			},
		})
		return nil, errors.New("image generation disabled for group")
	}
	imageBillingModel := ""
	imageSizeTier := ""
	imageInputSize := ""
	if IsImageGenerationIntent(openAIResponsesEndpoint, reqModel, body) {
		var imageCfgErr error
		imageCfg, imageCfgErr := resolveOpenAIResponsesImageBillingConfigDetailedFromBody(body, reqModel)
		if imageCfgErr != nil {
			setOpsUpstreamError(c, http.StatusBadRequest, imageCfgErr.Error(), "")
			c.JSON(http.StatusBadRequest, gin.H{
				"error": gin.H{
					"type":    "invalid_request_error",
					"message": imageCfgErr.Error(),
					"param":   "size",
				},
			})
			return nil, imageCfgErr
		}
		imageBillingModel = imageCfg.Model
		imageSizeTier = imageCfg.SizeTier
		imageInputSize = imageCfg.InputSize
	}

	logger.LegacyPrintf("service.openai_gateway",
		"[OpenAI 自动透传] 命中自动透传分支: account=%d name=%s type=%s model=%s stream=%v",
		account.ID,
		account.Name,
		account.Type,
		reqModel,
		reqStream,
	)
	if reqStream && c != nil && c.Request != nil {
		if timeoutHeaders := collectOpenAIPassthroughTimeoutHeaders(c.Request.Header); len(timeoutHeaders) > 0 {
			streamWarnLogger := logger.FromContext(ctx).With(
				zap.String("component", "service.openai_gateway"),
				zap.Int64("account_id", account.ID),
				zap.Strings("timeout_headers", timeoutHeaders),
			)
			if s.isOpenAIPassthroughTimeoutHeadersAllowed() {
				streamWarnLogger.Warn("OpenAI passthrough 透传请求包含超时相关请求头，且当前配置为放行，可能导致上游提前断流")
			} else {
				streamWarnLogger.Warn("OpenAI passthrough 检测到超时相关请求头，将按配置过滤以降低断流风险")
			}
		}
	}

	// Get access token
	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}

	upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
	upstreamReq, err := s.buildUpstreamRequestOpenAIPassthrough(upstreamCtx, c, account, body, token)
	releaseUpstreamCtx()
	if err != nil {
		return nil, err
	}

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	if c != nil {
		c.Set("openai_passthrough", true)
	}

	upstreamStart := time.Now()
	resp, err := doHTTPUpstream(ctx, s.httpUpstream, upstreamReq, proxyURL, account.ID, account.Concurrency)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(upstreamStart).Milliseconds())
	if err != nil {
		return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, true)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		responseBody := s.readUpstreamErrorBody(resp)
		// Preserve the bounded API-key context classification side effect before
		// applying the built-in direct policy; the policy still wins before failover.
		contextMatch := s.classifyOpenAIContextWindowErrorForAccount(account, responseBody)
		if policy, ok := RecognizeUpstreamErrorFact(ParseHTTPUpstreamErrorFact(PlatformOpenAI, resp, responseBody)); ok {
			writeRecognizedOpenAIHTTPError(c, policy.Presentation)
			return nil, newRecognizedOpenAIUpstreamRequestErrorForPolicy(policy, resp.Header.Get("x-request-id"))
		}
		// 透传模式默认保持原样代理；但 429/529 属于网关必须兜底的
		// 上游容量类错误，应先触发多账号 failover 以维持基础 SLA。
		if shouldFailoverOpenAIPassthroughResponse(resp.StatusCode, contextMatch, account != nil && account.Type == AccountTypeAPIKey) {
			return nil, s.handleFailoverErrorResponsePassthrough(ctx, resp, c, account, body, responseBody)
		}
		return nil, s.handleErrorResponsePassthrough(ctx, resp, c, account, body, responseBody, contextMatch)
	}

	serviceTier := extractOpenAIServiceTierFromBody(body)

	var usage *OpenAIUsage
	var firstTokenMs *int
	responseID := ""
	imageCount := 0
	var imageOutputSizes []string
	if reqStream {
		result, err := s.handleStreamingResponsePassthrough(ctx, resp, c, account, startTime, reqModel, upstreamPassthroughModel)
		if err != nil {
			var failoverErr *UpstreamFailoverError
			if errors.As(err, &failoverErr) {
				return nil, err
			}
			var requestErr *OpenAIUpstreamRequestError
			if errors.As(err, &requestErr) {
				return openAIForwardResultFromPassthroughStreamingResult(result, resp, startTime, reqModel, billingModel, upstreamPassthroughModel, serviceTier, reasoningEffort, imageBillingModel, imageSizeTier, imageInputSize), err
			}
			if !openAIStreamingPassthroughResultShouldExposeOnError(result) {
				return nil, err
			}
			return openAIForwardResultFromPassthroughStreamingResult(result, resp, startTime, reqModel, billingModel, upstreamPassthroughModel, serviceTier, reasoningEffort, imageBillingModel, imageSizeTier, imageInputSize), err
		}
		usage = result.usage
		firstTokenMs = result.firstTokenMs
		responseID = strings.TrimSpace(result.responseID)
		imageCount = result.imageCount
		imageOutputSizes = result.imageOutputSizes
	} else {
		result, err := s.handleNonStreamingResponsePassthrough(ctx, resp, c, reqModel, upstreamPassthroughModel)
		if err != nil {
			var requestErr *OpenAIUpstreamRequestError
			if errors.As(err, &requestErr) {
				return openAIForwardResultFromPassthroughNonStreamingResult(result, resp, startTime, reqModel, billingModel, upstreamPassthroughModel, serviceTier, reasoningEffort, imageBillingModel, imageSizeTier, imageInputSize), err
			}
			return nil, err
		}
		usage = result.usage
		responseID = strings.TrimSpace(result.responseID)
		imageCount = result.imageCount
		imageOutputSizes = result.imageOutputSizes
	}
	s.bindHTTPResponseAccount(ctx, c, account, responseID)

	if snapshot := ParseCodexRateLimitHeaders(resp.Header); snapshot != nil {
		s.updateCodexUsageSnapshot(ctx, account.ID, snapshot)
	}

	if usage == nil {
		usage = &OpenAIUsage{}
	}

	forwardResult := &OpenAIForwardResult{
		RequestID:       resp.Header.Get("x-request-id"),
		AttemptID:       forwardResultAttemptID(resp),
		ResponseID:      responseID,
		Usage:           *usage,
		Model:           reqModel,
		BillingModel:    billingModel,
		UpstreamModel:   upstreamPassthroughModel,
		ServiceTier:     serviceTier,
		ReasoningEffort: reasoningEffort,
		Stream:          reqStream,
		OpenAIWSMode:    false,
		Duration:        time.Since(startTime),
		FirstTokenMs:    firstTokenMs,
	}
	if imageCount > 0 {
		forwardResult.ImageCount = imageCount
		forwardResult.ImageSize = imageSizeTier
		forwardResult.ImageInputSize = imageInputSize
		forwardResult.ImageOutputSizes = imageOutputSizes
		forwardResult.BillingModel = imageBillingModel
	}
	return forwardResult, nil
}

func openAIForwardResultFromPassthroughNonStreamingResult(
	result *openaiNonStreamingResultPassthrough,
	resp *http.Response,
	startTime time.Time,
	originalModel string,
	billingModel string,
	upstreamModel string,
	serviceTier *string,
	reasoningEffort *string,
	imageBillingModel string,
	imageSizeTier string,
	imageInputSize string,
) *OpenAIForwardResult {
	if result == nil {
		return nil
	}
	usage := result.usage
	if usage == nil {
		usage = result.OpenAIUsage
	}
	if usage == nil {
		usage = &OpenAIUsage{}
	}
	requestID := ""
	var headers http.Header
	if resp != nil {
		requestID = resp.Header.Get("x-request-id")
		headers = resp.Header.Clone()
	}
	forwardResult := &OpenAIForwardResult{
		RequestID:        requestID,
		AttemptID:        forwardResultAttemptID(resp),
		ResponseID:       strings.TrimSpace(result.responseID),
		Usage:            *usage,
		Model:            originalModel,
		BillingModel:     billingModel,
		UpstreamModel:    upstreamModel,
		ServiceTier:      serviceTier,
		ReasoningEffort:  reasoningEffort,
		Stream:           false,
		OpenAIWSMode:     false,
		ResponseHeaders:  headers,
		Duration:         time.Since(startTime),
		ImageCount:       result.imageCount,
		ImageOutputSizes: result.imageOutputSizes,
	}
	if result.imageCount > 0 {
		forwardResult.ImageSize = imageSizeTier
		forwardResult.ImageInputSize = imageInputSize
		forwardResult.BillingModel = imageBillingModel
	}
	return forwardResult
}

func openAIForwardResultFromPassthroughStreamingResult(
	streamResult *openaiStreamingResultPassthrough,
	resp *http.Response,
	startTime time.Time,
	originalModel string,
	billingModel string,
	upstreamModel string,
	serviceTier *string,
	reasoningEffort *string,
	imageBillingModel string,
	imageSizeTier string,
	imageInputSize string,
) *OpenAIForwardResult {
	if streamResult == nil {
		return nil
	}
	usage := streamResult.usage
	if usage == nil {
		usage = &OpenAIUsage{}
	}
	requestID := ""
	var headers http.Header
	if resp != nil {
		requestID = resp.Header.Get("x-request-id")
		headers = resp.Header.Clone()
	}
	result := &OpenAIForwardResult{
		RequestID:        requestID,
		AttemptID:        forwardResultAttemptID(resp),
		ResponseID:       strings.TrimSpace(streamResult.responseID),
		Usage:            *usage,
		Model:            originalModel,
		BillingModel:     billingModel,
		UpstreamModel:    upstreamModel,
		ServiceTier:      serviceTier,
		ReasoningEffort:  reasoningEffort,
		Stream:           true,
		OpenAIWSMode:     false,
		ResponseHeaders:  headers,
		Duration:         time.Since(startTime),
		FirstTokenMs:     streamResult.firstTokenMs,
		ImageCount:       streamResult.imageCount,
		ImageOutputSizes: streamResult.imageOutputSizes,
	}
	if streamResult.imageCount > 0 {
		result.ImageSize = imageSizeTier
		result.ImageInputSize = imageInputSize
		result.BillingModel = imageBillingModel
	}
	return result
}

func (s *OpenAIGatewayService) buildUpstreamRequestOpenAIPassthrough(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	token string,
) (*http.Request, error) {
	if account == nil {
		return nil, errors.New("account is nil")
	}
	if !account.IsOpenAIApiKey() {
		return nil, fmt.Errorf("openai native passthrough requires APIKey account, got=%s", account.Type)
	}

	targetURL := openaiPlatformAPIURL
	baseURL := account.GetOpenAIBaseURL()
	if baseURL != "" {
		validatedURL, err := s.validateUpstreamBaseURL(baseURL)
		if err != nil {
			return nil, err
		}
		targetURL = buildOpenAIResponsesURL(validatedURL)
	}
	targetURL = appendOpenAIResponsesRequestPathSuffix(targetURL, openAIResponsesRequestPathSuffix(c))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))

	// APIKey 透传保留客户端请求头安全白名单。
	allowTimeoutHeaders := s.isOpenAIPassthroughTimeoutHeadersAllowed()
	if c != nil && c.Request != nil {
		for key, values := range c.Request.Header {
			lower := strings.ToLower(strings.TrimSpace(key))
			if !isOpenAIPassthroughAllowedRequestHeader(lower, allowTimeoutHeaders) {
				continue
			}
			for _, v := range values {
				req.Header.Add(key, v)
			}
		}
	}

	// 覆盖入站鉴权残留，并注入上游认证。
	req.Header.Del("authorization")
	req.Header.Del("x-api-key")
	req.Header.Del("x-goog-api-key")
	req.Header.Set("authorization", "Bearer "+token)

	// APIKey 透传模式支持账户自定义 User-Agent 与 ForceCodexCLI 兜底。
	customUA := account.GetOpenAIUserAgent()
	if customUA != "" {
		req.Header.Set("user-agent", customUA)
	}
	if s.cfg != nil && s.cfg.Gateway.ForceCodexCLI {
		req.Header.Set("user-agent", s.resolveOpenAICodexUserAgent(ctx))
	}

	if req.Header.Get("content-type") == "" {
		req.Header.Set("content-type", "application/json")
	}

	return req, nil
}

func (s *OpenAIGatewayService) buildUpstreamRequestOpenAIOAuthAdapter(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	token string,
	promptCacheKey string,
	isCodexCLI bool,
) (*http.Request, error) {
	if account == nil {
		return nil, errors.New("account is nil")
	}
	if !account.IsOpenAIOAuthLike() {
		return nil, fmt.Errorf("openai OAuth adapter requires OAuth account, got=%s", account.Type)
	}

	targetURL := appendOpenAIResponsesRequestPathSuffix(chatgptCodexURL, openAIResponsesRequestPathSuffix(c))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))

	if c != nil && c.Request != nil {
		for key, values := range c.Request.Header {
			lower := strings.ToLower(strings.TrimSpace(key))
			if !openaiAllowedHeaders[lower] {
				continue
			}
			for _, v := range values {
				req.Header.Add(key, v)
			}
		}
	}

	authHeaders, authErr := s.buildOpenAIAuthenticationHeaders(ctx, account, token)
	if authErr != nil {
		return nil, authErr
	}
	setHeaderRaw(req.Header, "Authorization", authHeaders.Get("Authorization"))

	// OAuth adapter 目标是 ChatGPT internal API，需要补齐 Codex/ChatGPT 请求头。
	if promptCacheKey == "" {
		promptCacheKey = strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String())
	}
	req.Host = "chatgpt.com"
	if chatgptAccountID := account.GetChatGPTAccountID(); chatgptAccountID != "" {
		setHeaderRaw(req.Header, "ChatGPT-Account-ID", chatgptAccountID)
	}
	if account.IsOpenAIChatGPTFedRAMPAccount() {
		setHeaderRaw(req.Header, "X-OpenAI-Fedramp", "true")
	}
	isCompactRequest := isOpenAIResponsesCompactPath(c)
	if isCompactRequest {
		setHeaderRaw(req.Header, "Accept", "application/json")
		if req.Header.Get("version") == "" {
			req.Header.Set("version", codexCLIVersion)
		}
		if req.Header.Get("session_id") == "" && req.Header.Get(openAICodexSessionIDHeader) == "" {
			req.Header.Set(openAICodexSessionIDHeader, resolveOpenAICompactSessionID(c))
		}
	} else if getHeaderRaw(req.Header, "Accept") == "" {
		setHeaderRaw(req.Header, "Accept", "text/event-stream")
	}
	compatMessagesBridge := isOpenAICompatMessagesBridgeContext(c) || isOpenAICompatMessagesBridgeBody(body)
	clientConversationID := strings.TrimSpace(req.Header.Get("conversation_id"))
	req.Header.Del("conversation_id")
	req.Header.Del("session_id")

	fingerprint, err := s.ensureOpenAICodexFingerprint(ctx, account)
	if err != nil {
		return nil, err
	}
	applyOpenAICodexFingerprintHeaders(req, fingerprint)
	*req = *req.WithContext(openAICodexFingerprintContext(req.Context(), fingerprint))

	mutatePromptCacheKey := !compatMessagesBridge
	mutateClientMetadata := !isCompactRequest
	body, _ = applyOpenAICodexHTTPRequestAlignmentWithBodyOptions(req, c, account, body, promptCacheKey, !compatMessagesBridge || clientConversationID != "", mutatePromptCacheKey, mutateClientMetadata)
	if _, err := normalizeOpenAIOAuthHTTPUpstreamRequestBody(req, c, account, body); err != nil {
		return nil, err
	}

	codexUA := s.resolveOpenAICodexUserAgent(ctx)
	if ua := fingerprint.UAProfile.UserAgent(); ua != "" {
		codexUA = ua
	}
	setHeaderRaw(req.Header, "User-Agent", codexUA)

	if getHeaderRaw(req.Header, "Content-Type") == "" {
		setHeaderRaw(req.Header, "Content-Type", "application/json")
	}

	return req, nil
}

func shouldFailoverOpenAIPassthroughResponse(statusCode int, contextMatch openAIContextWindowMatch, apiKey bool) bool {
	if !apiKey || !contextMatch.valid || contextMatch.overflow {
		return false
	}
	if contextMatch.matched() || contextMatch.rejectedRequest {
		return false
	}
	switch statusCode {
	case http.StatusTooManyRequests, 529:
		return true
	case 500, 502, 503, 504, 520, 521, 522, 523, 524:
		return true
	default:
		return false
	}
}

type openAIContextWindowMatch struct {
	field           string
	message         string
	errorType       string
	rejectedRequest bool
	valid           bool
	overflow        bool
}

func (m openAIContextWindowMatch) matched() bool { return m.field != "" }

func (s *OpenAIGatewayService) classifyOpenAIContextWindowError(body []byte) openAIContextWindowMatch {
	if s != nil && s.classifyContextWindowError != nil {
		return s.classifyContextWindowError(body)
	}
	return classifyOpenAIContextWindowError(body)
}

func (s *OpenAIGatewayService) classifyOpenAIContextWindowErrorForAccount(account *Account, body []byte) openAIContextWindowMatch {
	if account == nil || account.Type != AccountTypeAPIKey {
		return openAIContextWindowMatch{}
	}
	return s.classifyOpenAIContextWindowError(body)
}

// classifyOpenAIContextWindowError only trusts exact string values at the
// documented paths. Ambiguous trusted-path members and invalid JSON fail closed.
func classifyOpenAIContextWindowError(body []byte) openAIContextWindowMatch {
	return classifyOpenAIContextWindowErrorFields(body, []string{
		"error.code", "response.error.code", "code",
		"error.message", "response.error.message", "message",
	})
}

// Failed-terminal events only trust structured error envelopes. Root fields are
// retained solely for legacy HTTP error classification.
func classifyOpenAIFailedTerminalContextWindowError(body []byte) openAIContextWindowMatch {
	return classifyOpenAIContextWindowErrorFields(body, []string{
		"error.code", "response.error.code",
		"error.message", "response.error.message",
	})
}

func classifyOpenAIContextWindowErrorFields(body []byte, fields []string) openAIContextWindowMatch {
	if len(body) > openAIErrorClassificationMaxBytes {
		return openAIContextWindowMatch{overflow: true}
	}
	if len(body) == 0 || !utf8.Valid(body) {
		return openAIContextWindowMatch{}
	}
	p := openAIErrorClassifier{decoder: json.NewDecoder(bytes.NewReader(body)), values: make(map[string]string, 6)}
	first, err := p.next()
	if err != nil || first != json.Delim('{') {
		return openAIContextWindowMatch{}
	}
	if err := p.consume(first, openAIErrorPathRoot, 1); err != nil {
		return openAIContextWindowMatch{overflow: errors.Is(err, errOpenAIErrorClassificationOverflow)}
	}
	if p.ambiguous {
		return openAIContextWindowMatch{}
	}
	if token, err := p.next(); err != io.EOF || token != nil {
		return openAIContextWindowMatch{}
	}
	if openAITrustedErrorEnvelopePresent(p.values, "error.") && openAITrustedErrorEnvelopePresent(p.values, "response.error.") &&
		!openAITrustedErrorEnvelopesAgree(p.values, "error.", "response.error.") {
		return openAIContextWindowMatch{}
	}
	for _, field := range fields {
		if matchesOpenAIContextWindow(p.values[field]) {
			prefix := strings.TrimSuffix(field, "code")
			prefix = strings.TrimSuffix(prefix, "message")
			return openAIContextWindowMatch{
				field:           field,
				message:         p.values[prefix+"message"],
				errorType:       p.values[prefix+"type"],
				rejectedRequest: anyRejectedOpenAITrustedValue(p.values, matchesOpenAIRejectedRequest),
				valid:           true,
			}
		}
	}
	return openAIContextWindowMatch{rejectedRequest: anyRejectedOpenAITrustedValue(p.values, matchesOpenAIRejectedRequest), valid: true}
}

func openAITrustedErrorEnvelopePresent(values map[string]string, prefix string) bool {
	return values[prefix+"code"] != "" || values[prefix+"message"] != "" || values[prefix+"type"] != ""
}

func openAITrustedErrorEnvelopesAgree(values map[string]string, leftPrefix, rightPrefix string) bool {
	for _, field := range []string{"code", "message", "type"} {
		if strings.TrimSpace(values[leftPrefix+field]) != strings.TrimSpace(values[rightPrefix+field]) {
			return false
		}
	}
	return true
}

var errOpenAIErrorClassificationOverflow = errors.New("OpenAI error classification bound exceeded")

type openAIErrorPath uint8

const (
	openAIErrorPathOther openAIErrorPath = iota
	openAIErrorPathRoot
	openAIErrorPathError
	openAIErrorPathResponse
	openAIErrorPathResponseError
)

type openAIErrorClassifier struct {
	decoder   *json.Decoder
	values    map[string]string
	tokens    int
	ambiguous bool
}

func (p *openAIErrorClassifier) next() (json.Token, error) {
	if p.tokens >= openAIErrorClassificationMaxTokens {
		return nil, errOpenAIErrorClassificationOverflow
	}
	p.tokens++
	return p.decoder.Token()
}

func (p *openAIErrorClassifier) consume(token json.Token, path openAIErrorPath, depth int) error {
	if depth > openAIErrorClassificationMaxDepth {
		return errOpenAIErrorClassificationOverflow
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{}, 6)
		for p.decoder.More() {
			keyToken, err := p.next()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("OpenAI error classification object key is not a string")
			}
			lowerKey := strings.ToLower(key)
			field, childPath, object, leaf := openAITrustedErrorField(path, lowerKey)
			if field != "" {
				if key != lowerKey {
					p.ambiguous = true
				}
				if _, exists := seen[lowerKey]; exists {
					p.ambiguous = true
				}
			}
			seen[lowerKey] = struct{}{}
			value, err := p.next()
			if err != nil {
				return err
			}
			if leaf {
				if text, ok := value.(string); ok {
					p.values[field] = text
				} else {
					p.ambiguous = true
					if err := p.consume(value, openAIErrorPathOther, depth+1); err != nil {
						return err
					}
				}
				continue
			}
			if object && value != json.Delim('{') {
				p.ambiguous = true
			}
			if err := p.consume(value, childPath, depth+1); err != nil {
				return err
			}
		}
		end, err := p.next()
		if err != nil || end != json.Delim('}') {
			return errors.New("OpenAI error classification object is incomplete")
		}
		return nil
	case '[':
		if path != openAIErrorPathOther {
			p.ambiguous = true
		}
		for p.decoder.More() {
			value, err := p.next()
			if err != nil {
				return err
			}
			if err := p.consume(value, openAIErrorPathOther, depth+1); err != nil {
				return err
			}
		}
		end, err := p.next()
		if err != nil || end != json.Delim(']') {
			return errors.New("OpenAI error classification array is incomplete")
		}
		return nil
	default:
		return errors.New("unexpected OpenAI error JSON delimiter")
	}
}

func openAITrustedErrorField(path openAIErrorPath, key string) (string, openAIErrorPath, bool, bool) {
	switch path {
	case openAIErrorPathRoot:
		switch key {
		case "error":
			return "error", openAIErrorPathError, true, false
		case "response":
			return "response", openAIErrorPathResponse, true, false
		case "code", "message", "type", "param":
			return key, openAIErrorPathOther, false, true
		}
	case openAIErrorPathError:
		if key == "code" || key == "message" || key == "type" || key == "param" {
			return "error." + key, openAIErrorPathOther, false, true
		}
	case openAIErrorPathResponse:
		if key == "error" {
			return "response.error", openAIErrorPathResponseError, true, false
		}
	case openAIErrorPathResponseError:
		if key == "code" || key == "message" || key == "type" || key == "param" {
			return "response.error." + key, openAIErrorPathOther, false, true
		}
	}
	return "", openAIErrorPathOther, false, false
}

func matchesOpenAIContextWindow(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return false
	}
	if strings.Contains(lower, "context_too_large") || strings.Contains(lower, "context_length_exceeded") ||
		strings.Contains(lower, "maximum context length") || strings.Contains(lower, "max context length") {
		return true
	}
	exceeded := strings.Contains(lower, "exceed") || strings.Contains(lower, "too large") || strings.Contains(lower, "too long")
	return exceeded && (strings.Contains(lower, "context window") || strings.Contains(lower, "context length") ||
		(strings.Contains(lower, "token limit") && strings.Contains(lower, "context")))
}

func matchesOpenAIRejectedRequest(field, text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return false
	}
	if strings.HasSuffix(field, "type") || strings.HasSuffix(field, "code") {
		if strings.HasPrefix(lower, "invalid_request") || lower == "invalid_parameter" || lower == "invalid_field" ||
			lower == "unknown_field" || lower == "unknown_parameter" || lower == "unsupported_parameter" ||
			lower == "unrecognized_parameter" || lower == "rejected_parameter" || lower == "rejected_field" {
			return true
		}
	}
	return lower == "invalid parameter" || strings.HasPrefix(lower, "invalid parameter:") ||
		lower == "invalid field" || strings.HasPrefix(lower, "invalid field:") || strings.Contains(lower, "unknown field") ||
		strings.Contains(lower, "unknown parameter") || strings.Contains(lower, "unsupported parameter") ||
		strings.Contains(lower, "unrecognized parameter") || strings.Contains(lower, "rejected parameter") || strings.Contains(lower, "rejected field")
}

func anyRejectedOpenAITrustedValue(values map[string]string, match func(string, string) bool) bool {
	for field, value := range values {
		if match(field, value) {
			return true
		}
	}
	return false
}

func openAIContextWindowClientMessage() string {
	return "The request exceeds the model context window"
}

func sanitizedOpenAIPassthroughError(statusCode int, upstreamHeaders http.Header, message string) ([]byte, http.Header) {
	body, _ := json.Marshal(gin.H{"error": gin.H{"type": "upstream_error", "message": message}})
	headers := make(http.Header, 3)
	headers.Set("Content-Type", "application/json; charset=utf-8")
	headers.Set("Cache-Control", "no-store")
	if retryAfter := strings.TrimSpace(upstreamHeaders.Get("Retry-After")); validOpenAIPassthroughRetryAfter(retryAfter, time.Now()) {
		headers.Set("Retry-After", retryAfter)
	}
	return body, headers
}

func validOpenAIPassthroughRetryAfter(raw string, now time.Time) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	allDigits := true
	for i := 0; i < len(raw); i++ {
		if raw[i] < '0' || raw[i] > '9' {
			allDigits = false
			break
		}
	}
	if allDigits {
		seconds, err := strconv.ParseUint(raw, 10, 64)
		return err == nil && seconds > 0
	}
	parsed, err := http.ParseTime(raw)
	return err == nil && parsed.After(now)
}

func openAIPassthroughSafeMessage(statusCode int, contextMessage string) (int, string) {
	if contextMessage != "" {
		return statusCode, contextMessage
	}
	switch statusCode {
	case http.StatusUnauthorized:
		return http.StatusBadGateway, "Upstream authentication failed"
	case http.StatusForbidden:
		return http.StatusBadGateway, "Upstream access denied"
	default:
		if statusCode >= http.StatusInternalServerError {
			return statusCode, "Upstream service temporarily unavailable"
		}
		return statusCode, "Upstream request failed"
	}
}

func (s *OpenAIGatewayService) handleFailoverErrorResponsePassthrough(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
	requestBody []byte,
	responseBody []byte,
) error {
	body := responseBody

	upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(body))
	upstreamMsg = sanitizeOpenAIUpstreamDiagnosticText(upstreamMsg)
	upstreamDetail := ""
	if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
		if maxBytes <= 0 {
			maxBytes = 2048
		}
		upstreamDetail = sanitizeOpenAIUpstreamDiagnosticBodyForLog(body, maxBytes)
	}
	setOpsUpstreamError(c, resp.StatusCode, upstreamMsg, upstreamDetail)
	logOpenAIInstructionsRequiredDebug(ctx, c, account, resp.StatusCode, upstreamMsg, requestBody, body)
	reqModel, _, _ := extractOpenAIRequestMetaFromBody(requestBody)
	_ = s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, body, reqModel)
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:             account.Platform,
		AccountID:            account.ID,
		AccountName:          account.Name,
		UpstreamStatusCode:   resp.StatusCode,
		UpstreamRequestID:    resp.Header.Get("x-request-id"),
		Passthrough:          true,
		Kind:                 "failover",
		Message:              upstreamMsg,
		Detail:               upstreamDetail,
		UpstreamResponseBody: upstreamDetail,
	})
	if account == nil || account.Type != AccountTypeAPIKey {
		failoverErr := newOpenAIHTTPFailoverError(resp, body, false)
		failoverErr.ResponseHeaders = resp.Header.Clone()
		return failoverErr
	}
	statusCode, safeMessage := openAIPassthroughSafeMessage(resp.StatusCode, "")
	safeBody, safeHeaders := sanitizedOpenAIPassthroughError(statusCode, resp.Header, safeMessage)
	fact := ParseHTTPUpstreamErrorFact(PlatformOpenAI, resp, body)
	return newSanitizedUpstreamFailoverErrorWithFact(
		statusCode,
		safeBody,
		safeHeaders,
		account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
		&fact,
	)
}

func (s *OpenAIGatewayService) handleErrorResponsePassthrough(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
	requestBody []byte,
	responseBody []byte,
	contextMatch openAIContextWindowMatch,
) error {
	body := responseBody

	upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(body))
	upstreamMsg = sanitizeOpenAIUpstreamDiagnosticText(upstreamMsg)
	upstreamDetail := ""
	if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
		if maxBytes <= 0 {
			maxBytes = 2048
		}
		upstreamDetail = sanitizeOpenAIUpstreamDiagnosticBodyForLog(body, maxBytes)
	}
	setOpsUpstreamError(c, resp.StatusCode, upstreamMsg, upstreamDetail)
	logOpenAIInstructionsRequiredDebug(ctx, c, account, resp.StatusCode, upstreamMsg, requestBody, body)
	if policy, ok := RecognizeUpstreamErrorFact(ParseHTTPUpstreamErrorFact(PlatformOpenAI, resp, body)); ok {
		writeRecognizedOpenAIHTTPError(c, policy.Presentation)
		return newRecognizedOpenAIUpstreamRequestErrorForPolicy(policy, resp.Header.Get("x-request-id"))
	}
	// 透传模式保留原始上游错误响应，但运行态账号状态仍需更新，
	// 避免粘性路由继续复用刚被限流的账号。
	reqModel, _, _ := extractOpenAIRequestMetaFromBody(requestBody)
	_ = s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, body, reqModel)
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:             account.Platform,
		AccountID:            account.ID,
		AccountName:          account.Name,
		UpstreamStatusCode:   resp.StatusCode,
		UpstreamRequestID:    resp.Header.Get("x-request-id"),
		Passthrough:          true,
		Kind:                 "http_error",
		Message:              upstreamMsg,
		Detail:               upstreamDetail,
		UpstreamResponseBody: upstreamDetail,
	})

	if account != nil && account.Type == AccountTypeAPIKey {
		contextMessage := ""
		if contextMatch.matched() {
			contextMessage = openAIContextWindowClientMessage()
		}
		statusCode, safeMessage := openAIPassthroughSafeMessage(resp.StatusCode, contextMessage)
		safeBody, safeHeaders := sanitizedOpenAIPassthroughError(statusCode, resp.Header, safeMessage)
		for key := range c.Writer.Header() {
			c.Writer.Header().Del(key)
		}
		for key, values := range safeHeaders {
			c.Writer.Header()[key] = append([]string(nil), values...)
		}
		MarkResponseCommitted(c)
		c.Data(statusCode, safeHeaders.Get("Content-Type"), safeBody)
		return fmt.Errorf("upstream error: %d (client response sanitized)", resp.StatusCode)
	}

	writeOpenAIPassthroughResponseHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	MarkResponseCommitted(c)
	c.Data(resp.StatusCode, contentType, body)

	if upstreamMsg == "" {
		return fmt.Errorf("upstream error: %d", resp.StatusCode)
	}
	return fmt.Errorf("upstream error: %d message=%s", resp.StatusCode, upstreamMsg)
}

func isOpenAIPassthroughAllowedRequestHeader(lowerKey string, allowTimeoutHeaders bool) bool {
	if lowerKey == "" {
		return false
	}
	if isOpenAIPassthroughTimeoutHeader(lowerKey) {
		return allowTimeoutHeaders
	}
	return openaiPassthroughAllowedHeaders[lowerKey]
}

func isOpenAIPassthroughTimeoutHeader(lowerKey string) bool {
	switch lowerKey {
	case "x-stainless-timeout", "x-stainless-read-timeout", "x-stainless-connect-timeout", "x-request-timeout", "request-timeout", "grpc-timeout":
		return true
	default:
		return false
	}
}

func (s *OpenAIGatewayService) isOpenAIPassthroughTimeoutHeadersAllowed() bool {
	return s != nil && s.cfg != nil && s.cfg.Gateway.OpenAIPassthroughAllowTimeoutHeaders
}

func collectOpenAIPassthroughTimeoutHeaders(h http.Header) []string {
	if h == nil {
		return nil
	}
	var matched []string
	for key, values := range h {
		lowerKey := strings.ToLower(strings.TrimSpace(key))
		if isOpenAIPassthroughTimeoutHeader(lowerKey) {
			entry := lowerKey
			if len(values) > 0 {
				entry = fmt.Sprintf("%s=%s", lowerKey, strings.Join(values, "|"))
			}
			matched = append(matched, entry)
		}
	}
	sort.Strings(matched)
	return matched
}

type openaiStreamingResultPassthrough struct {
	usage             *OpenAIUsage
	firstTokenMs      *int
	responseID        string
	imageCount        int
	imageOutputSizes  []string
	responseFailed    bool
	realOutputStarted bool
}

type openaiNonStreamingResultPassthrough struct {
	*OpenAIUsage
	usage            *OpenAIUsage
	responseID       string
	imageCount       int
	imageOutputSizes []string
}

func openAIStreamClientOutputStarted(c *gin.Context, localStarted bool) bool {
	if localStarted {
		return true
	}
	return c != nil && c.Writer != nil && c.Writer.Written()
}

func openAIStreamEventIsPreamble(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "response.created", "response.in_progress":
		return true
	default:
		return false
	}
}

func openAIStreamDataStartsClientOutput(data, eventType string) bool {
	trimmed := strings.TrimSpace(data)
	if trimmed == "" {
		return false
	}
	if strings.TrimSpace(eventType) == "response.failed" {
		return false
	}
	return !openAIStreamEventIsPreamble(eventType)
}

func openAIStreamDataStartsRealClientOutput(data, eventType string) bool {
	return strings.TrimSpace(data) != "[DONE]" && openAIStreamDataStartsClientOutput(data, eventType)
}

func openAIStreamingPassthroughResultShouldExposeOnError(result *openaiStreamingResultPassthrough) bool {
	if result == nil {
		return false
	}
	if result.responseFailed && !result.realOutputStarted {
		return false
	}
	return true
}

func openAIStreamFailedEventShouldFailover(payload []byte, message string) bool {
	if isOpenAITransientProcessingError(http.StatusBadRequest, message, payload) {
		return true
	}
	code := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "response.error.code").String()))
	if code == "" {
		code = strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "error.code").String()))
	}
	errType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "response.error.type").String()))
	if errType == "" {
		errType = strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, "error.type").String()))
	}
	combined := strings.ToLower(strings.TrimSpace(message + " " + code + " " + errType))
	if combined == "" {
		return true
	}
	nonRetryableMarkers := []string{
		"invalid_request",
		"content_policy",
		"policy",
		"safety",
		"high-risk cyber",
		"not allowed",
		"violat",
	}
	for _, marker := range nonRetryableMarkers {
		if strings.Contains(combined, marker) {
			return false
		}
	}
	return true
}

func (s *OpenAIGatewayService) newOpenAIStreamFailoverError(
	c *gin.Context,
	account *Account,
	passthrough bool,
	upstreamRequestID string,
	payload []byte,
	message string,
) *UpstreamFailoverError {
	message = sanitizeOpenAIUpstreamDiagnosticText(strings.TrimSpace(message))
	if message == "" {
		message = "OpenAI stream disconnected before completion"
	}
	diagnosticPayload := sanitizeOpenAIStreamFailoverDiagnosticPayload(payload)
	detail := ""
	if len(diagnosticPayload) > 0 && s != nil && s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
		if maxBytes <= 0 {
			maxBytes = 2048
		}
		detail = sanitizeOpenAIUpstreamDiagnosticBodyForLog(diagnosticPayload, maxBytes)
	}
	if c != nil {
		setOpsUpstreamError(c, http.StatusBadGateway, message, detail)
		event := OpsUpstreamErrorEvent{
			Platform:           PlatformOpenAI,
			UpstreamStatusCode: http.StatusBadGateway,
			UpstreamRequestID:  strings.TrimSpace(upstreamRequestID),
			Passthrough:        passthrough,
			Kind:               "failover",
			Message:            message,
			Detail:             detail,
		}
		if account != nil {
			event.Platform = account.Platform
			event.AccountID = account.ID
			event.AccountName = account.Name
		}
		appendOpsUpstreamError(c, event)
	}
	body, _ := json.Marshal(gin.H{
		"error": gin.H{
			"type":    "upstream_error",
			"message": message,
		},
	})
	retryableOnSameAccount := false
	if account != nil && account.IsPoolMode() {
		// Stream failover errors are synthesized as 502s; same-account retry should
		// still honor the pool-mode status-code policy, including an explicit empty list.
		retryableOnSameAccount = account.IsPoolModeRetryableStatus(http.StatusBadGateway)
	}
	fact := ParseOpenAIJSONErrorFact(PlatformOpenAI, UpstreamErrorSourceSSE, payload, upstreamRequestID)
	if len(payload) == 0 {
		fact.Source = UpstreamErrorSourceStreamTermination
		fact.SafeMessage = boundedUpstreamErrorFactScalar(message, upstreamErrorFactMaxScalarBytes)
		fact.InternalMatchText = buildUpstreamErrorFactMatchText(fact)
	}
	return &UpstreamFailoverError{
		StatusCode:             http.StatusBadGateway,
		ResponseBody:           body,
		RetryableOnSameAccount: retryableOnSameAccount,
		upstreamFact:           &fact,
	}
}

func (s *OpenAIGatewayService) handleStreamingResponsePassthrough(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
	startTime time.Time,
	originalModel string,
	mappedModel string,
) (*openaiStreamingResultPassthrough, error) {
	writeOpenAIPassthroughResponseHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)

	// SSE headers
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")
	if v := resp.Header.Get("x-request-id"); v != "" {
		c.Header("x-request-id", v)
	}

	w := c.Writer
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, errors.New("streaming not supported")
	}

	usage := &OpenAIUsage{}
	imageCounter := newOpenAIImageOutputCounter()
	var firstTokenMs *int
	responseID := ""
	clientDisconnected := false
	sawDone := false
	sawTerminalEvent := false
	sawFailedEvent := false
	failedMessage := ""
	clientOutputStarted := false
	realOutputStarted := false
	upstreamRequestID := strings.TrimSpace(resp.Header.Get("x-request-id"))
	var streamRequestErr *OpenAIUpstreamRequestError
	pendingLines := make([]string, 0, 8)
	flushPending := false
	flushPendingOutput := func() {
		if !clientDisconnected && flushPending {
			flusher.Flush()
			flushPending = false
		}
	}
	writePendingLines := func() bool {
		for _, pending := range pendingLines {
			if _, err := fmt.Fprintln(w, pending); err != nil {
				clientDisconnected = true
				logger.LegacyPrintf("service.openai_gateway", "[OpenAI passthrough] Client disconnected during streaming, continue draining upstream for usage: account=%d", account.ID)
				return false
			}
		}
		pendingLines = pendingLines[:0]
		return true
	}

	scanner := bufio.NewScanner(resp.Body)
	maxLineSize := defaultMaxLineSize
	if s.cfg != nil && s.cfg.Gateway.MaxLineSize > 0 {
		maxLineSize = s.cfg.Gateway.MaxLineSize
	}
	scanBuf := getSSEScannerBuf64K()
	scanner.Buffer(scanBuf[:0], maxLineSize)
	defer putSSEScannerBuf64K(scanBuf)
	documentScanner := newOpenAISSEJSONDocumentScanner(scanner)

	needModelReplace := strings.TrimSpace(originalModel) != "" && strings.TrimSpace(mappedModel) != "" && strings.TrimSpace(originalModel) != strings.TrimSpace(mappedModel)
	resultWithUsage := func() *openaiStreamingResultPassthrough {
		return &openaiStreamingResultPassthrough{
			usage:             usage,
			firstTokenMs:      firstTokenMs,
			responseID:        responseID,
			imageCount:        imageCounter.Count(),
			imageOutputSizes:  imageCounter.Sizes(),
			responseFailed:    sawFailedEvent,
			realOutputStarted: realOutputStarted,
		}
	}

	currentEventType := ""
	suppressTerminalFrame := false
	// Keep the preamble buffered for newly recognized direct errors so the
	// handler can still render a pre-output HTTP response.
	suppressClientOutput := false
	for documentScanner.Scan() {
		line := documentScanner.Text()
		suppressLine := suppressTerminalFrame
		lineStartsClientOutput := false
		lineStartsRealOutput := false
		forceFlushFailedEvent := false
		if parsedEventType, ok := extractOpenAISSEEventLine(line); ok {
			currentEventType = parsedEventType
			if sawTerminalEvent && openAIResponseStreamEventTypeIsTerminal(parsedEventType) {
				suppressLine = true
				suppressTerminalFrame = true
			}
		} else if strings.TrimSpace(line) == "" {
			currentEventType = ""
			suppressTerminalFrame = false
		}
		if data, ok := extractOpenAISSEDataLine(line); ok {
			acceptTerminalState := !sawTerminalEvent
			dataBytes := []byte(data)
			trimmedData := strings.TrimSpace(data)
			if responseID == "" {
				responseID = extractOpenAIResponseIDFromJSONBytes(dataBytes)
			}
			if needModelReplace && strings.Contains(data, mappedModel) {
				line = s.replaceModelInSSELine(line, mappedModel, originalModel)
				if replacedData, replaced := extractOpenAISSEDataLine(line); replaced {
					dataBytes = []byte(replacedData)
					trimmedData = strings.TrimSpace(replacedData)
				}
			}
			if normalizedData, normalized := normalizeOpenAIResponsesFunctionCallArguments(dataBytes); normalized {
				dataBytes = normalizedData
				trimmedData = strings.TrimSpace(string(normalizedData))
				line = "data: " + string(normalizedData)
			}
			eventType := classifyOpenAIResponseSSEEvent(dataBytes, currentEventType)
			if !acceptTerminalState && openAIResponseStreamEventTypeIsTerminal(eventType) {
				suppressLine = true
				suppressTerminalFrame = true
			}
			if suppressLine {
				continue
			}
			if acceptTerminalState && (eventType == "response.completed" || eventType == "response.done") {
				SetOpenAIRemoteCompactionSemanticOutcome(c, OpenAIRemoteCompactionSemanticOutcomeSucceeded)
				if normalizedData, normalized := normalizeCompletedImageGenerationStatusForEvent(dataBytes, eventType); normalized {
					dataBytes = normalizedData
					trimmedData = strings.TrimSpace(string(normalizedData))
					line = "data: " + string(normalizedData)
				}
			}
			if acceptTerminalState && openAIResponseStreamTerminalIsFailure(eventType) {
				SetOpenAIRemoteCompactionSemanticOutcome(c, OpenAIRemoteCompactionSemanticOutcomeFailed)
			}
			if acceptTerminalState && eventType == "response.failed" {
				failedMessage = extractOpenAISSEErrorMessage(dataBytes)
				s.parseSSEUsageBytesForEvent(dataBytes, usage, eventType)
				fact := ParseOpenAIJSONErrorFact(PlatformOpenAI, UpstreamErrorSourceSSE, dataBytes, upstreamRequestID)
				requestErr := newRecognizedOpenAIUpstreamRequestError(fact)
				if requestErr == nil {
					requestErr = newOpenAIUpstreamRequestError(dataBytes, upstreamRequestID)
				}
				outputStarted := realOutputStarted || openAIStreamClientOutputStarted(c, clientOutputStarted)
				if requestErr != nil {
					requestErr.observeTerminal(*usage, outputStarted)
					streamRequestErr = requestErr
					suppressClientOutput = isNewRecognizedDirectOpenAIError(fact) && !outputStarted
					forceFlushFailedEvent = !suppressClientOutput
				} else if !outputStarted && openAIStreamFailedEventShouldFailover(dataBytes, failedMessage) {
					return resultWithUsage(),
						s.newOpenAIStreamFailoverError(c, account, true, upstreamRequestID, openAIResponseFailedPayloadForDiagnostic(dataBytes, eventType), failedMessage)
				} else {
					forceFlushFailedEvent = true
				}
				sawFailedEvent = true
			}
			if trimmedData == "[DONE]" {
				sawDone = true
			}
			if acceptTerminalState && openAIStreamEventIsTerminalWithType(trimmedData, eventType) {
				sawTerminalEvent = true
			}
			imageCounter.AddSSEData(dataBytes)
			if eventType == "response.failed" {
				if sanitizedData, sanitized := sanitizeOpenAIResponseFailedEventForClient(dataBytes, eventType); sanitized {
					dataBytes = sanitizedData
					trimmedData = strings.TrimSpace(string(sanitizedData))
					line = "data: " + string(sanitizedData)
				}
			}
			lineStartsRealOutput = openAIStreamDataStartsRealClientOutput(trimmedData, eventType)
			if lineStartsRealOutput {
				realOutputStarted = true
			}
			lineStartsClientOutput = forceFlushFailedEvent || lineStartsRealOutput
			if suppressClientOutput {
				continue
			}
			if firstTokenMs == nil && lineStartsRealOutput {
				ms := int(time.Since(startTime).Milliseconds())
				firstTokenMs = &ms
			}
			if acceptTerminalState {
				s.parseSSEUsageBytesForEvent(dataBytes, usage, eventType)
			}
		}

		if suppressLine {
			continue
		}
		if !clientDisconnected {
			if !clientOutputStarted && !lineStartsClientOutput {
				pendingLines = append(pendingLines, line)
				continue
			}
			if !clientOutputStarted && len(pendingLines) > 0 {
				if !writePendingLines() {
					continue
				}
			}
			if _, err := fmt.Fprintln(w, line); err != nil {
				clientDisconnected = true
				logger.LegacyPrintf("service.openai_gateway", "[OpenAI passthrough] Client disconnected during streaming, continue draining upstream for usage: account=%d", account.ID)
			} else {
				clientOutputStarted = true
				if lineStartsRealOutput {
					realOutputStarted = true
				}
				flushPending = true
				if line == "" {
					flushPendingOutput()
				}
			}
		}
	}
	flushPendingOutput()
	if err := documentScanner.Err(); err != nil {
		if !sawTerminalEvent {
			SetOpenAIRemoteCompactionSemanticOutcome(c, OpenAIRemoteCompactionSemanticOutcomeFailed)
		}
		if sawTerminalEvent && !sawFailedEvent {
			return resultWithUsage(), nil
		}
		if sawFailedEvent {
			if streamRequestErr != nil {
				return resultWithUsage(), streamRequestErr
			}
			return resultWithUsage(), fmt.Errorf("upstream response failed: %s", failedMessage)
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return resultWithUsage(), fmt.Errorf("stream usage incomplete: %w", err)
		}
		if errors.Is(err, bufio.ErrTooLong) {
			logger.LegacyPrintf("service.openai_gateway", "[OpenAI passthrough] SSE line too long: account=%d max_size=%d error=%v", account.ID, maxLineSize, err)
			return resultWithUsage(), err
		}
		if !openAIStreamClientOutputStarted(c, clientOutputStarted) {
			msg := "OpenAI stream disconnected before completion"
			if errText := strings.TrimSpace(err.Error()); errText != "" {
				msg += ": " + errText
			}
			return resultWithUsage(),
				s.newOpenAIStreamFailoverError(c, account, true, upstreamRequestID, nil, msg)
		}
		if clientDisconnected {
			return resultWithUsage(), fmt.Errorf("stream usage incomplete after disconnect: %w", err)
		}
		logger.LegacyPrintf("service.openai_gateway",
			"[OpenAI passthrough] 流读取异常中断: account=%d request_id=%s err=%v",
			account.ID,
			upstreamRequestID,
			err,
		)
		return resultWithUsage(), fmt.Errorf("stream read error: %w", err)
	}
	if sawFailedEvent {
		if streamRequestErr != nil {
			return resultWithUsage(), streamRequestErr
		}
		return resultWithUsage(), fmt.Errorf("upstream response failed: %s", failedMessage)
	}
	if !sawTerminalEvent {
		SetOpenAIRemoteCompactionSemanticOutcome(c, OpenAIRemoteCompactionSemanticOutcomeFailed)
	}
	if !clientDisconnected && !sawDone && !sawTerminalEvent && ctx.Err() == nil {
		logger.FromContext(ctx).With(
			zap.String("component", "service.openai_gateway"),
			zap.Int64("account_id", account.ID),
			zap.String("upstream_request_id", upstreamRequestID),
		).Info("OpenAI passthrough 上游流在未收到 [DONE] 时结束，疑似断流")
		if !openAIStreamClientOutputStarted(c, clientOutputStarted) {
			return resultWithUsage(),
				s.newOpenAIStreamFailoverError(c, account, true, upstreamRequestID, nil, "OpenAI stream ended before a terminal event")
		}
		return resultWithUsage(), errors.New("stream usage incomplete: missing terminal event")
	}

	return resultWithUsage(), nil
}

func (s *OpenAIGatewayService) handleNonStreamingResponsePassthrough(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	originalModel string,
	mappedModel string,
) (*openaiNonStreamingResultPassthrough, error) {
	body, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
	if err != nil {
		return nil, err
	}

	// Detect SSE responses from upstream and convert to JSON.
	// Some upstreams (e.g. other sub2api instances) may return SSE even when
	// stream=false was requested. Without this conversion the client would
	// receive raw SSE text or a terminal event with empty output.
	if isEventStreamResponse(resp.Header) {
		return s.handlePassthroughSSEToJSON(resp, c, body, originalModel, mappedModel)
	}

	usage := &OpenAIUsage{}
	usageParsed := false
	if len(body) > 0 {
		if parsedUsage, ok := extractValidatedOpenAIResponsesUsageFromJSONBytes(body); ok {
			*usage = parsedUsage
			usageParsed = true
		}
	}
	if !usageParsed {
		if bodyLooksLikeSSE := bodyHasSSEFraming(body); bodyLooksLikeSSE {
			// 兜底：兼容被错误标记为 JSON 的 SSE 响应。
			if finalResponse, ok := extractCodexFinalResponse(string(body)); ok {
				if parsedUsage, ok := extractValidatedOpenAIResponsesUsageFromJSONBytes(finalResponse); ok {
					*usage = parsedUsage
					usageParsed = true
				}
			}
		}
	}
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices && !usageParsed {
		return nil, s.writeOpenAINonStreamingProtocolError(resp, c, "Upstream returned invalid usage")
	}
	responseID := extractOpenAIResponseIDFromJSONBytes(body)

	writeOpenAIPassthroughResponseHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	if originalModel != "" && mappedModel != "" && originalModel != mappedModel {
		body = s.replaceModelInResponseBody(body, mappedModel, originalModel)
	}
	if normalized, normalizedChanged := normalizeOpenAIResponsesFunctionCallOutputArguments(body); normalizedChanged {
		body = normalized
	}
	if !writeOpenAICompactSSEBridge(c, resp.StatusCode, body) {
		c.Data(resp.StatusCode, contentType, body)
	}
	setOpenAIRemoteCompactionNonStreamingOutcome(c, body)
	return &openaiNonStreamingResultPassthrough{
		OpenAIUsage:      usage,
		usage:            usage,
		responseID:       responseID,
		imageCount:       countOpenAIResponseImageOutputsFromJSONBytes(body),
		imageOutputSizes: collectOpenAIResponseImageOutputSizesFromJSONBytes(body),
	}, nil
}

// handlePassthroughSSEToJSON converts an SSE response body into a JSON
// response for the passthrough path. It mirrors handleSSEToJSON while
// preserving passthrough payloads, except compact-only model remapping may
// rewrite model fields back to the original requested model.
func (s *OpenAIGatewayService) handlePassthroughSSEToJSON(resp *http.Response, c *gin.Context, body []byte, originalModel string, mappedModel string) (*openaiNonStreamingResultPassthrough, error) {
	bodyText := string(body)
	finalResponse, ok := extractCodexFinalResponse(bodyText)

	usage := &OpenAIUsage{}
	if ok {
		parsedUsage, parsed := extractValidatedOpenAIResponsesUsageFromJSONBytes(finalResponse)
		if !parsed {
			return nil, s.writeOpenAINonStreamingProtocolError(resp, c, "Upstream returned invalid usage")
		}
		*usage = parsedUsage
		// When the terminal event has an empty output array, reconstruct
		// output from accumulated delta events so the client gets full content.
		if len(gjson.GetBytes(finalResponse, "output").Array()) == 0 {
			if outputJSON, reconstructed := reconstructResponseOutputFromSSE(bodyText); reconstructed {
				if patched, err := sjson.SetRawBytes(finalResponse, "output", outputJSON); err == nil {
					finalResponse = patched
				}
			}
		}
		body = finalResponse
		if originalModel != "" && mappedModel != "" && originalModel != mappedModel {
			body = s.replaceModelInResponseBody(body, mappedModel, originalModel)
		}
		// Correct tool calls in final response
		body = s.correctToolCallsInResponseBody(body)
	} else {
		terminalType, terminalPayload, terminalOK := extractOpenAISSETerminalEvent(bodyText)
		if terminalOK {
			if terminalType == "response.failed" {
				s.parseSSEUsageBytesForEvent(terminalPayload, usage, terminalType)
				fact := ParseOpenAIJSONErrorFact(PlatformOpenAI, UpstreamErrorSourceSSE, terminalPayload, resp.Header.Get("x-request-id"))
				requestErr := newRecognizedOpenAIUpstreamRequestError(fact)
				if requestErr == nil {
					requestErr = newOpenAIUpstreamRequestError(terminalPayload, resp.Header.Get("x-request-id"))
				}
				if requestErr != nil {
					requestErr.attachUsage(*usage)
					if strings.EqualFold(fact.ProviderCode, "context_length_exceeded") {
						clientPayload, _ := sanitizeOpenAIResponseFailedEventForClient(terminalPayload, terminalType)
						c.Data(requestErr.StatusCode, "application/json; charset=utf-8", clientPayload)
					} else {
						c.JSON(requestErr.StatusCode, gin.H{"error": gin.H{
							"code":    requestErr.Code,
							"type":    requestErr.Type,
							"message": requestErr.Message,
						}})
					}
					return &openaiNonStreamingResultPassthrough{OpenAIUsage: usage, usage: usage}, requestErr
				}
				diagnosticPayload := openAIResponseFailedPayloadForDiagnostic(terminalPayload, terminalType)
				sanitizedPayload := sanitizeOpenAIStreamFailoverDiagnosticPayload(diagnosticPayload)
				msg := extractOpenAISSEErrorMessage(terminalPayload)
				if msg == "" {
					msg = "Upstream compact response failed"
				}
				return nil, s.writeOpenAINonStreamingProtocolError(resp, c, msg, sanitizedPayload)
			}
			return nil, s.writeOpenAINonStreamingProtocolError(resp, c, "Upstream compact response returned conflicting terminal events", terminalPayload)
		}
		return nil, s.writeOpenAINonStreamingProtocolError(resp, c, "Upstream returned invalid usage")
	}

	writeOpenAIPassthroughResponseHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)

	contentType := "application/json; charset=utf-8"
	if !ok {
		contentType = resp.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "text/event-stream"
		}
	}
	if !writeOpenAICompactSSEBridge(c, resp.StatusCode, body) {
		c.Data(resp.StatusCode, contentType, body)
	}
	if ok {
		setOpenAIRemoteCompactionNonStreamingOutcome(c, body)
	}

	return &openaiNonStreamingResultPassthrough{
		OpenAIUsage:      usage,
		usage:            usage,
		responseID:       extractOpenAIResponseIDFromJSONBytes(body),
		imageCount:       countOpenAIImageOutputsFromSSEBody(bodyText),
		imageOutputSizes: collectOpenAIImageOutputSizesFromSSEBody(bodyText),
	}, nil
}

func writeOpenAIPassthroughResponseHeaders(dst http.Header, src http.Header, filter *responseheaders.CompiledHeaderFilter) {
	if dst == nil || src == nil {
		return
	}
	if filter != nil {
		responseheaders.WriteFilteredHeaders(dst, src, filter)
	} else {
		// 兜底：尽量保留最基础的 content-type
		if v := strings.TrimSpace(src.Get("Content-Type")); v != "" {
			dst.Set("Content-Type", v)
		}
	}
	// 透传模式强制放行 x-codex-* 响应头（若上游返回）。
	// 注意：真实 http.Response.Header 的 key 一般会被 canonicalize；但为了兼容测试/自建响应，
	// 这里用 EqualFold 做一次大小写不敏感的查找。
	getCaseInsensitiveValues := func(h http.Header, want string) []string {
		if h == nil {
			return nil
		}
		for k, vals := range h {
			if strings.EqualFold(k, want) {
				return vals
			}
		}
		return nil
	}

	for _, rawKey := range []string{
		openAICodexPrimaryUsedPercentHeader,
		openAICodexPrimaryResetSecondsHeader,
		openAICodexPrimaryWindowMinutesHeader,
		openAICodexSecondUsedPercentHeader,
		openAICodexSecondResetSecondsHeader,
		openAICodexSecondWindowMinutesHeader,
		openAICodexPrimaryOverSecondHeader,
	} {
		vals := getCaseInsensitiveValues(src, rawKey)
		if len(vals) == 0 {
			continue
		}
		key := http.CanonicalHeaderKey(rawKey)
		dst.Del(key)
		for _, v := range vals {
			dst.Add(key, v)
		}
	}
}

func (s *OpenAIGatewayService) buildUpstreamRequest(ctx context.Context, c *gin.Context, account *Account, body []byte, token string, isStream bool, promptCacheKey string, isCodexCLI bool) (*http.Request, error) {
	if account != nil && account.IsOpenAIOAuthLike() {
		return s.buildUpstreamRequestOpenAIOAuthAdapter(ctx, c, account, body, token, promptCacheKey, isCodexCLI)
	}

	// Remaining runtime path is APIKey/native OpenAI; OAuth-like accounts already
	// delegated to the adapter and must not fall back to legacy passthrough logic.
	var targetURL string
	switch account.Type {
	case AccountTypeAPIKey:
		// API Key accounts use Platform API or custom base URL
		baseURL := account.GetOpenAIBaseURL()
		if baseURL == "" {
			targetURL = openaiPlatformAPIURL
		} else {
			validatedURL, err := s.validateUpstreamBaseURL(baseURL)
			if err != nil {
				return nil, err
			}
			targetURL = buildOpenAIResponsesURL(validatedURL)
		}
	default:
		targetURL = openaiPlatformAPIURL
	}
	targetURL = appendOpenAIResponsesRequestPathSuffix(targetURL, openAIResponsesRequestPathSuffix(c))

	req, err := http.NewRequestWithContext(ctx, "POST", targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))

	// Set authentication header
	req.Header.Set("authorization", "Bearer "+token)

	// Whitelist native OpenAI request headers for APIKey/custom-base-url calls.
	for key, values := range c.Request.Header {
		lowerKey := strings.ToLower(key)
		if openaiAllowedHeaders[lowerKey] {
			for _, v := range values {
				req.Header.Add(key, v)
			}
		}
	}

	// Apply custom User-Agent if configured
	customUA := account.GetOpenAIUserAgent()
	if customUA != "" {
		req.Header.Set("user-agent", customUA)
	}

	// 若开启 ForceCodexCLI，则强制将上游 User-Agent 伪装为 Codex CLI。
	// 用于网关未透传/改写 User-Agent 时，仍能命中 Codex 侧识别逻辑。
	if s.cfg != nil && s.cfg.Gateway.ForceCodexCLI {
		req.Header.Set("user-agent", s.resolveOpenAICodexUserAgent(ctx))
	}
	// Ensure required headers exist
	if req.Header.Get("content-type") == "" {
		req.Header.Set("content-type", "application/json")
	}

	return req, nil
}

func (s *OpenAIGatewayService) ensureOpenAICodexFingerprint(ctx context.Context, account *Account) (OpenAICodexFingerprint, error) {
	if account == nil || !account.IsOpenAIOAuthLike() {
		return OpenAICodexFingerprint{}, nil
	}
	if s == nil || s.codexFingerprintService == nil {
		defaultUA := DefaultOpenAICodexUserAgent
		var accountRepo OpenAICodexFingerprintAccountRepository
		if s != nil {
			defaultUA = s.resolveOpenAICodexUserAgent(ctx)
			accountRepo = s.accountRepo
		}
		return ensureOpenAICodexFingerprintWithProfile(ctx, account, accountRepo, ParseOpenAICodexUAProfile(defaultUA), time.Now().UTC())
	}
	return s.codexFingerprintService.Ensure(ctx, account)
}

func (s *OpenAIGatewayService) resolveOpenAICodexUserAgent(ctx context.Context) string {
	codexUA := DefaultOpenAICodexUserAgent
	if s != nil && s.settingService != nil {
		if v := strings.TrimSpace(s.settingService.GetOpenAICodexUserAgent(ctx)); v != "" {
			codexUA = v
		}
	}
	return codexUA
}

func (s *OpenAIGatewayService) handleErrorResponse(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
	requestBody []byte,
	requestedModel ...string,
) (*OpenAIForwardResult, error) {
	body := s.readUpstreamErrorBody(resp)

	upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(body))
	upstreamMsg = sanitizeOpenAIUpstreamDiagnosticText(upstreamMsg)
	upstreamDetail := ""
	if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
		if maxBytes <= 0 {
			maxBytes = 2048
		}
		upstreamDetail = sanitizeOpenAIUpstreamDiagnosticBodyForLog(body, maxBytes)
	}
	setOpsUpstreamError(c, resp.StatusCode, upstreamMsg, upstreamDetail)
	logOpenAIInstructionsRequiredDebug(ctx, c, account, resp.StatusCode, upstreamMsg, requestBody, body)

	if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		logger.LegacyPrintf("service.openai_gateway",
			"OpenAI upstream error %d (account=%d platform=%s type=%s): %s",
			resp.StatusCode,
			account.ID,
			account.Platform,
			account.Type,
			sanitizeOpenAIUpstreamDiagnosticBodyForLog(body, s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes),
		)
	}

	if policy, ok := RecognizeUpstreamErrorFact(ParseHTTPUpstreamErrorFact(PlatformOpenAI, resp, body)); ok {
		writeRecognizedOpenAIHTTPError(c, policy.Presentation)
		return nil, newRecognizedOpenAIUpstreamRequestErrorForPolicy(policy, resp.Header.Get("x-request-id"))
	}

	if status, errType, errMsg, matched := applyErrorPassthroughRule(
		c,
		PlatformOpenAI,
		resp.StatusCode,
		body,
		http.StatusBadGateway,
		"upstream_error",
		"Upstream request failed",
	); matched {
		errMsg = sanitizeOpenAIUpstreamDiagnosticText(errMsg)
		MarkResponseCommitted(c)
		c.JSON(status, gin.H{
			"error": gin.H{
				"type":    errType,
				"message": errMsg,
			},
		})
		if upstreamMsg == "" {
			upstreamMsg = errMsg
		}
		if upstreamMsg == "" {
			return nil, fmt.Errorf("upstream error: %d (passthrough rule matched)", resp.StatusCode)
		}
		return nil, fmt.Errorf("upstream error: %d (passthrough rule matched) message=%s", resp.StatusCode, upstreamMsg)
	}

	// Check custom error codes
	if !account.ShouldHandleErrorCode(resp.StatusCode) {
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: resp.StatusCode,
			UpstreamRequestID:  resp.Header.Get("x-request-id"),
			Kind:               "http_error",
			Message:            upstreamMsg,
			Detail:             upstreamDetail,
		})
		MarkResponseCommitted(c)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{
				"type":    "upstream_error",
				"message": "Upstream gateway error",
			},
		})
		if upstreamMsg == "" {
			return nil, fmt.Errorf("upstream error: %d (not in custom error codes)", resp.StatusCode)
		}
		return nil, fmt.Errorf("upstream error: %d (not in custom error codes) message=%s", resp.StatusCode, upstreamMsg)
	}

	// Handle upstream error (mark account status)
	var reqModel string
	if len(requestedModel) > 0 {
		reqModel = strings.TrimSpace(requestedModel[0])
	}
	if reqModel == "" {
		reqModel, _, _ = extractOpenAIRequestMetaFromBody(requestBody)
	}
	shouldDisable := s.handleOpenAIAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, body, reqModel)
	kind := "http_error"
	if shouldDisable {
		kind = "failover"
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: resp.StatusCode,
		UpstreamRequestID:  resp.Header.Get("x-request-id"),
		Kind:               kind,
		Message:            upstreamMsg,
		Detail:             upstreamDetail,
	})
	if shouldDisable {
		return nil, newOpenAIHTTPFailoverError(
			resp,
			body,
			account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
		)
	}

	// Return appropriate error response
	var errType, errMsg string
	var statusCode int

	switch resp.StatusCode {
	case 401:
		statusCode = http.StatusBadGateway
		errType = "upstream_error"
		errMsg = "Upstream authentication failed, please contact administrator"
	case 402:
		statusCode = http.StatusBadGateway
		errType = "upstream_error"
		errMsg = "Upstream payment required: insufficient balance or billing issue"
	case 403:
		statusCode = http.StatusBadGateway
		errType = "upstream_error"
		errMsg = "Upstream access forbidden, please contact administrator"
	case 429:
		statusCode = http.StatusTooManyRequests
		errType = "rate_limit_error"
		errMsg = "Upstream rate limit exceeded, please retry later"
	default:
		statusCode = http.StatusBadGateway
		errType = "upstream_error"
		errMsg = "Upstream request failed"
	}

	MarkResponseCommitted(c)
	c.JSON(statusCode, gin.H{
		"error": gin.H{
			"type":    errType,
			"message": errMsg,
		},
	})

	if upstreamMsg == "" {
		return nil, fmt.Errorf("upstream error: %d", resp.StatusCode)
	}
	return nil, fmt.Errorf("upstream error: %d message=%s", resp.StatusCode, upstreamMsg)
}

// compatErrorWriter is the signature for format-specific error writers used by
// the compat paths (Chat Completions and Anthropic Messages).
type compatErrorWriter func(c *gin.Context, statusCode int, errType, message string)

// handleCompatErrorResponse is the shared non-failover error handler for the
// Chat Completions and Anthropic Messages compat paths. It mirrors the logic of
// handleErrorResponse (passthrough rules, ShouldHandleErrorCode, rate-limit
// tracking, secondary failover) but delegates the final error write to the
// format-specific writer function.
func (s *OpenAIGatewayService) handleCompatErrorResponse(
	resp *http.Response,
	c *gin.Context,
	account *Account,
	writeError compatErrorWriter,
	requestedModel ...string,
) (*OpenAIForwardResult, error) {
	body := s.readUpstreamErrorBody(resp)

	upstreamMsg := strings.TrimSpace(extractUpstreamErrorMessage(body))
	if upstreamMsg == "" {
		upstreamMsg = fmt.Sprintf("Upstream error: %d", resp.StatusCode)
	}
	upstreamMsg = sanitizeOpenAIUpstreamDiagnosticText(upstreamMsg)

	upstreamDetail := ""
	if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
		if maxBytes <= 0 {
			maxBytes = 2048
		}
		upstreamDetail = sanitizeOpenAIUpstreamDiagnosticBodyForLog(body, maxBytes)
	}
	setOpsUpstreamError(c, resp.StatusCode, upstreamMsg, upstreamDetail)

	if policy, ok := RecognizeUpstreamErrorFact(ParseHTTPUpstreamErrorFact(PlatformOpenAI, resp, body)); ok {
		requestErr := newRecognizedOpenAIUpstreamRequestErrorForPolicy(policy, resp.Header.Get("x-request-id"))
		if writeError == nil {
			return nil, requestErr
		}
		if c == nil || c.Writer == nil {
			return nil, requestErr
		}
		MarkResponseCommitted(c)
		c.Data(requestErr.StatusCode, "application/json; charset=utf-8", requestErr.ChatErrorBody())
		return nil, requestErr
	}

	// Apply error passthrough rules
	if status, errType, errMsg, matched := applyErrorPassthroughRule(
		c, account.Platform, resp.StatusCode, body,
		http.StatusBadGateway, "api_error", "Upstream request failed",
	); matched {
		errMsg = sanitizeOpenAIUpstreamDiagnosticText(errMsg)
		writeError(c, status, errType, errMsg)
		if upstreamMsg == "" {
			upstreamMsg = errMsg
		}
		if upstreamMsg == "" {
			return nil, fmt.Errorf("upstream error: %d (passthrough rule matched)", resp.StatusCode)
		}
		return nil, fmt.Errorf("upstream error: %d (passthrough rule matched) message=%s", resp.StatusCode, upstreamMsg)
	}

	// Check custom error codes — if the account does not handle this status,
	// return a generic error without exposing upstream details.
	if !account.ShouldHandleErrorCode(resp.StatusCode) {
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: resp.StatusCode,
			UpstreamRequestID:  resp.Header.Get("x-request-id"),
			Kind:               "http_error",
			Message:            upstreamMsg,
			Detail:             upstreamDetail,
		})
		writeError(c, http.StatusInternalServerError, "api_error", "Upstream gateway error")
		if upstreamMsg == "" {
			return nil, fmt.Errorf("upstream error: %d (not in custom error codes)", resp.StatusCode)
		}
		return nil, fmt.Errorf("upstream error: %d (not in custom error codes) message=%s", resp.StatusCode, upstreamMsg)
	}

	// Track rate limits and decide whether to trigger secondary failover.
	var modelForCooldown string
	if len(requestedModel) > 0 {
		modelForCooldown = requestedModel[0]
	}
	shouldDisable := s.handleOpenAIAccountUpstreamError(
		c.Request.Context(), account, resp.StatusCode, resp.Header, body, modelForCooldown,
	)
	kind := "http_error"
	if shouldDisable {
		kind = "failover"
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: resp.StatusCode,
		UpstreamRequestID:  resp.Header.Get("x-request-id"),
		Kind:               kind,
		Message:            upstreamMsg,
		Detail:             upstreamDetail,
	})
	if shouldDisable {
		return nil, newOpenAIHTTPFailoverError(
			resp,
			body,
			account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
		)
	}

	// Map status code to error type and write response
	errType := "api_error"
	switch {
	case resp.StatusCode == 400:
		errType = "invalid_request_error"
	case resp.StatusCode == 404:
		errType = "not_found_error"
	case resp.StatusCode == 429:
		errType = "rate_limit_error"
	case resp.StatusCode >= 500:
		errType = "api_error"
	}

	writeError(c, resp.StatusCode, errType, upstreamMsg)
	return nil, fmt.Errorf("upstream error: %d %s", resp.StatusCode, upstreamMsg)
}

// openaiStreamingResult streaming response result
type openaiStreamingResult struct {
	usage             *OpenAIUsage
	firstTokenMs      *int
	responseID        string
	imageCount        int
	imageOutputSizes  []string
	responseFailed    bool
	realOutputStarted bool
}

type openaiNonStreamingResult struct {
	*OpenAIUsage
	usage            *OpenAIUsage
	responseID       string
	imageCount       int
	imageOutputSizes []string
}

func openAIStreamingResultShouldExposeOnError(result *openaiStreamingResult) bool {
	if result == nil {
		return false
	}
	if result.responseFailed && !result.realOutputStarted {
		return false
	}
	return true
}

func (s *OpenAIGatewayService) handleStreamingResponse(ctx context.Context, resp *http.Response, c *gin.Context, account *Account, startTime time.Time, originalModel, mappedModel string) (*openaiStreamingResult, error) {
	return s.handleStreamingResponseWithNamespaceRestorer(ctx, resp, c, account, startTime, originalModel, mappedModel, restoreOpenAIResponsesNamespacePayload)
}

func (s *OpenAIGatewayService) handleStreamingResponseWithNamespaceRestorer(ctx context.Context, resp *http.Response, c *gin.Context, account *Account, startTime time.Time, originalModel, mappedModel string, restoreNamespace func(*gin.Context, []byte) ([]byte, error)) (*openaiStreamingResult, error) {
	if s.responseHeaderFilter != nil {
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	}

	// Set SSE response headers
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	// Pass through other headers
	if v := resp.Header.Get("x-request-id"); v != "" {
		c.Header("x-request-id", v)
	}

	w := c.Writer
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, errors.New("streaming not supported")
	}
	bufferedWriter := bufio.NewWriterSize(w, 4*1024)
	flushBuffered := func() error {
		if err := bufferedWriter.Flush(); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	usage := &OpenAIUsage{}
	imageCounter := newOpenAIImageOutputCounter()
	var firstTokenMs *int
	responseID := ""
	scanner := bufio.NewScanner(resp.Body)
	maxLineSize := defaultMaxLineSize
	if s.cfg != nil && s.cfg.Gateway.MaxLineSize > 0 {
		maxLineSize = s.cfg.Gateway.MaxLineSize
	}
	scanBuf := getSSEScannerBuf64K()
	scanner.Buffer(scanBuf[:0], maxLineSize)
	documentScanner := newOpenAISSEJSONDocumentScanner(scanner)

	streamInterval := time.Duration(0)
	if s.cfg != nil && s.cfg.Gateway.StreamDataIntervalTimeout > 0 {
		streamInterval = time.Duration(s.cfg.Gateway.StreamDataIntervalTimeout) * time.Second
	}
	// 仅监控上游数据间隔超时，不被下游写入阻塞影响
	var intervalTicker *time.Ticker
	if streamInterval > 0 {
		intervalTicker = time.NewTicker(streamInterval)
		defer intervalTicker.Stop()
	}
	var intervalCh <-chan time.Time
	if intervalTicker != nil {
		intervalCh = intervalTicker.C
	}

	keepaliveInterval := time.Duration(0)
	if s.cfg != nil && s.cfg.Gateway.StreamKeepaliveInterval > 0 {
		keepaliveInterval = time.Duration(s.cfg.Gateway.StreamKeepaliveInterval) * time.Second
	}
	// 下游 keepalive 仅用于防止代理空闲断开
	var keepaliveTicker *time.Ticker
	if keepaliveInterval > 0 {
		keepaliveTicker = time.NewTicker(keepaliveInterval)
		defer keepaliveTicker.Stop()
	}
	var keepaliveCh <-chan time.Time
	if keepaliveTicker != nil {
		keepaliveCh = keepaliveTicker.C
	}
	// Track downstream writes separately from upstream reads: pre-output failover
	// can buffer response.created / response.in_progress, so keepalive must be
	// based on downstream idle time.
	lastDownstreamWriteAt := time.Now()

	// 仅发送一次错误事件，避免多次写入导致协议混乱。
	// 注意：OpenAI `/v1/responses` streaming 事件必须符合 OpenAI Responses schema；
	// 否则下游 SDK（例如 OpenCode）会因为类型校验失败而报错。
	errorEventSent := false
	clientDisconnected := false // 客户端断开后继续 drain 上游以收集 usage
	sawTerminalEvent := false
	sawFailedEvent := false
	failedMessage := ""
	clientOutputStarted := false
	realOutputStarted := false
	upstreamRequestID := strings.TrimSpace(resp.Header.Get("x-request-id"))
	var streamFailoverErr error
	var streamRequestErr *OpenAIUpstreamRequestError
	eventShouldFlush := false
	eventInProgress := false
	sendErrorEvent := func(reason string) {
		if errorEventSent || clientDisconnected {
			return
		}
		errorEventSent = true
		payload := `{"type":"error","sequence_number":0,"error":{"type":"upstream_error","message":` + strconv.Quote(reason) + `,"code":` + strconv.Quote(reason) + `}}`
		if err := flushBuffered(); err != nil {
			clientDisconnected = true
			return
		}
		if _, err := bufferedWriter.WriteString("data: " + payload + "\n\n"); err != nil {
			clientDisconnected = true
			return
		}
		if err := flushBuffered(); err != nil {
			clientDisconnected = true
			return
		}
		clientOutputStarted = true
		lastDownstreamWriteAt = time.Now()
	}

	needModelReplace := originalModel != mappedModel
	streamOutputAccumulator := apicompat.NewBufferedResponseAccumulator()
	streamImageOutputs := make([]json.RawMessage, 0, 1)
	streamSeenImages := make(map[string]struct{})
	currentEventType := ""
	suppressTerminalFrame := false
	// Only PR2's newly recognized direct errors suppress the buffered preamble so
	// the handler can render a pre-output HTTP error. Existing context failures
	// retain their response.failed framing contract.
	suppressClientOutput := false
	resultWithUsage := func() *openaiStreamingResult {
		return &openaiStreamingResult{
			usage:             usage,
			firstTokenMs:      firstTokenMs,
			responseID:        responseID,
			imageCount:        imageCounter.Count(),
			imageOutputSizes:  imageCounter.Sizes(),
			responseFailed:    sawFailedEvent,
			realOutputStarted: realOutputStarted,
		}
	}
	flushBufferedFailedEvent := func(reason string) {
		if clientDisconnected || bufferedWriter.Buffered() == 0 {
			return
		}
		if err := flushBuffered(); err != nil {
			clientDisconnected = true
			logger.LegacyPrintf("service.openai_gateway", "Client disconnected during response.failed flush (%s), continuing to drain upstream for billing", reason)
			return
		}
		clientOutputStarted = true
		lastDownstreamWriteAt = time.Now()
	}
	finalizeStream := func() (*openaiStreamingResult, error) {
		if !sawTerminalEvent {
			SetOpenAIRemoteCompactionSemanticOutcome(c, OpenAIRemoteCompactionSemanticOutcomeFailed)
			if !openAIStreamClientOutputStarted(c, clientOutputStarted) {
				return resultWithUsage(), s.newOpenAIStreamFailoverError(
					c,
					account,
					false,
					upstreamRequestID,
					nil,
					"OpenAI stream ended before a terminal event",
				)
			}
			return resultWithUsage(), fmt.Errorf("stream usage incomplete: missing terminal event")
		}
		if sawFailedEvent {
			if !suppressClientOutput {
				flushBufferedFailedEvent("finalize")
			}
			if streamRequestErr != nil {
				return resultWithUsage(), streamRequestErr
			}
			return resultWithUsage(), fmt.Errorf("upstream response failed: %s", failedMessage)
		}
		if !clientDisconnected {
			hadBufferedData := bufferedWriter.Buffered() > 0
			if err := flushBuffered(); err != nil {
				clientDisconnected = true
				logger.LegacyPrintf("service.openai_gateway", "Client disconnected during final flush, returning collected usage")
			} else if hadBufferedData {
				clientOutputStarted = true
				lastDownstreamWriteAt = time.Now()
				eventInProgress = false
			}
		}
		return resultWithUsage(), nil
	}
	handleScanErr := func(scanErr error) (*openaiStreamingResult, error, bool) {
		if scanErr == nil {
			return nil, nil, false
		}
		if !sawTerminalEvent {
			SetOpenAIRemoteCompactionSemanticOutcome(c, OpenAIRemoteCompactionSemanticOutcomeFailed)
		}
		if sawTerminalEvent && !sawFailedEvent {
			logger.LegacyPrintf("service.openai_gateway", "Upstream scan ended after terminal event: %v", scanErr)
			return resultWithUsage(), nil, true
		}
		if sawFailedEvent {
			if !suppressClientOutput {
				flushBufferedFailedEvent("scan_error")
			}
			if streamRequestErr != nil {
				return resultWithUsage(), streamRequestErr, true
			}
			return resultWithUsage(), fmt.Errorf("upstream response failed: %s", failedMessage), true
		}
		// 客户端断开/取消请求时，上游读取往往会返回 context canceled。
		// /v1/responses 的 SSE 事件必须符合 OpenAI 协议；这里不注入自定义 error event，避免下游 SDK 解析失败。
		if errors.Is(scanErr, context.Canceled) || errors.Is(scanErr, context.DeadlineExceeded) {
			return resultWithUsage(), fmt.Errorf("stream usage incomplete: %w", scanErr), true
		}
		if errors.Is(scanErr, bufio.ErrTooLong) {
			logger.LegacyPrintf("service.openai_gateway", "SSE line too long: account=%d max_size=%d error=%v", account.ID, maxLineSize, scanErr)
			sendErrorEvent("response_too_large")
			return resultWithUsage(), scanErr, true
		}
		if !openAIStreamClientOutputStarted(c, clientOutputStarted) {
			msg := "OpenAI stream disconnected before completion"
			if errText := strings.TrimSpace(scanErr.Error()); errText != "" {
				msg += ": " + errText
			}
			return resultWithUsage(), s.newOpenAIStreamFailoverError(c, account, false, upstreamRequestID, nil, msg), true
		}
		// 客户端已断开时，上游出错仅影响体验，不影响计费；返回已收集 usage
		if clientDisconnected {
			return resultWithUsage(), fmt.Errorf("stream usage incomplete after disconnect: %w", scanErr), true
		}
		sendErrorEvent("stream_read_error")
		return resultWithUsage(), fmt.Errorf("stream read error: %w", scanErr), true
	}
	processSSELine := func(line string, queueDrained bool) {
		if streamFailoverErr != nil || suppressClientOutput {
			return
		}
		suppressLine := suppressTerminalFrame
		if parsedEventType, ok := extractOpenAISSEEventLine(line); ok {
			currentEventType = parsedEventType
			if sawTerminalEvent && openAIResponseStreamEventTypeIsTerminal(parsedEventType) {
				suppressLine = true
				suppressTerminalFrame = true
			}
		} else if strings.TrimSpace(line) == "" {
			currentEventType = ""
			suppressTerminalFrame = false
		}
		// Extract data from SSE line (supports both "data: " and "data:" formats)
		if data, ok := extractOpenAISSEDataLine(line); ok {
			acceptTerminalState := !sawTerminalEvent
			dataBytes := []byte(data)
			if responseID == "" {
				responseID = extractOpenAIResponseIDFromJSONBytes(dataBytes)
			}
			eventType := classifyOpenAIResponseSSEEvent(dataBytes, currentEventType)
			if !acceptTerminalState && openAIResponseStreamEventTypeIsTerminal(eventType) {
				suppressTerminalFrame = true
				return
			}
			if acceptTerminalState && openAIStreamEventIsTerminalWithType(data, eventType) {
				sawTerminalEvent = true
			}
			forceFlushFailedEvent := false
			if acceptTerminalState && openAIResponseStreamTerminalIsFailure(eventType) {
				SetOpenAIRemoteCompactionSemanticOutcome(c, OpenAIRemoteCompactionSemanticOutcomeFailed)
			}
			if acceptTerminalState && eventType == "response.failed" {
				failedMessage = extractOpenAISSEErrorMessage(dataBytes)
				s.parseSSEUsageBytesForEvent(dataBytes, usage, eventType)
				fact := ParseOpenAIJSONErrorFact(PlatformOpenAI, UpstreamErrorSourceSSE, dataBytes, upstreamRequestID)
				requestErr := newRecognizedOpenAIUpstreamRequestError(fact)
				if requestErr == nil {
					requestErr = newOpenAIUpstreamRequestError(dataBytes, upstreamRequestID)
				}
				outputStarted := realOutputStarted || openAIStreamClientOutputStarted(c, clientOutputStarted)
				if requestErr != nil {
					requestErr.observeTerminal(*usage, outputStarted)
					streamRequestErr = requestErr
					suppressClientOutput = isNewRecognizedDirectOpenAIError(fact) && !outputStarted
					forceFlushFailedEvent = !suppressClientOutput
					sawFailedEvent = true
				} else if !outputStarted && openAIStreamFailedEventShouldFailover(dataBytes, failedMessage) {
					sawFailedEvent = true
					streamFailoverErr = s.newOpenAIStreamFailoverError(c, account, false, upstreamRequestID, openAIResponseFailedPayloadForDiagnostic(dataBytes, eventType), failedMessage)
					return
				} else {
					forceFlushFailedEvent = true
					sawFailedEvent = true
				}
			}
			imageCounter.AddSSEData(dataBytes)

			// Correct Codex tool calls if needed (apply_patch -> edit, etc.)
			if s.toolCorrector != nil {
				if correctedData, corrected := s.toolCorrector.CorrectToolCallsInSSEBytes(dataBytes); corrected {
					dataBytes = correctedData
					data = string(correctedData)
					line = "data: " + data
					eventType = classifyOpenAIResponseSSEEvent(dataBytes, currentEventType)
				}
			}
			if normalizedData, normalized := normalizeOpenAIResponsesFunctionCallArguments(dataBytes); normalized {
				dataBytes = normalizedData
				data = string(normalizedData)
				line = "data: " + data
				eventType = classifyOpenAIResponseSSEEvent(dataBytes, currentEventType)
			}
			restoredData, restoreErr := restoreNamespace(c, dataBytes)
			if restoreErr != nil {
				// A local best-effort presentation conversion must never trigger a
				// second upstream request after the stream is visible to the client.
				if openAIStreamClientOutputStarted(c, clientOutputStarted) {
					restoredData = dataBytes
				} else {
					streamFailoverErr = fmt.Errorf("restore OpenAI namespace response: %w", restoreErr)
					return
				}
			}
			if !bytes.Equal(restoredData, dataBytes) {
				dataBytes = restoredData
				data = string(restoredData)
				line = "data: " + data
				eventType = classifyOpenAIResponseSSEEvent(dataBytes, currentEventType)
			}
			if imageOutput, ok := extractImageGenerationOutputFromSSEData(dataBytes, eventType, streamSeenImages); ok {
				streamImageOutputs = append(streamImageOutputs, imageOutput)
			}
			if responsesStreamEventMayContributeToOutput(eventType) {
				var streamEvent apicompat.ResponsesStreamEvent
				if err := json.Unmarshal(dataBytes, &streamEvent); err == nil {
					streamOutputAccumulator.ProcessEvent(&streamEvent)
				}
			}
			if normalizedData, normalized := normalizeResponsesStreamingTerminalOutput(dataBytes, streamOutputAccumulator, streamImageOutputs); normalized {
				dataBytes = normalizedData
				data = string(normalizedData)
				line = "data: " + data
				eventType = classifyOpenAIResponseSSEEvent(dataBytes, currentEventType)
			}
			if acceptTerminalState && (eventType == "response.completed" || eventType == "response.done") {
				SetOpenAIRemoteCompactionSemanticOutcome(c, OpenAIRemoteCompactionSemanticOutcomeSucceeded)
				if normalizedData, normalized := normalizeCompletedImageGenerationStatusForEvent(dataBytes, eventType); normalized {
					dataBytes = normalizedData
					data = string(normalizedData)
					line = "data: " + data
				}
			}
			if eventType == "response.failed" {
				if sanitizedData, sanitized := sanitizeOpenAIResponseFailedEventForClient(dataBytes, eventType); sanitized {
					dataBytes = sanitizedData
					data = string(sanitizedData)
					line = "data: " + data
				}
			}
			// Replace model in response if needed.
			// Fast path: most events do not contain model field values.
			if needModelReplace && mappedModel != "" && strings.Contains(line, mappedModel) {
				line = s.replaceModelInSSELine(line, mappedModel, originalModel)
			}
			startsRealOutput := openAIStreamDataStartsRealClientOutput(data, eventType)
			if startsRealOutput {
				realOutputStarted = true
			}
			startsClientOutput := forceFlushFailedEvent || startsRealOutput

			// Pre-output recognized request errors are rendered by the handler as HTTP JSON;
			// do not commit the buffered SSE preamble or terminal event.
			if suppressClientOutput {
				return
			}

			// 写入客户端（客户端断开后继续 drain 上游）
			if !clientDisconnected {
				shouldFlush := queueDrained && (clientOutputStarted || startsClientOutput)
				if forceFlushFailedEvent {
					shouldFlush = true
				}
				if firstTokenMs == nil && startsRealOutput {
					// 保证首个 token 事件尽快出站，避免影响 TTFT。
					shouldFlush = true
				}
				eventShouldFlush = eventShouldFlush || shouldFlush
				if _, err := bufferedWriter.WriteString(line); err != nil {
					clientDisconnected = true
					logger.LegacyPrintf("service.openai_gateway", "Client disconnected during streaming, continuing to drain upstream for billing")
				} else if _, err := bufferedWriter.WriteString("\n"); err != nil {
					clientDisconnected = true
					logger.LegacyPrintf("service.openai_gateway", "Client disconnected during streaming, continuing to drain upstream for billing")
				} else {
					eventInProgress = true
				}
			}

			// Record first token time
			if firstTokenMs == nil && startsRealOutput {
				ms := int(time.Since(startTime).Milliseconds())
				firstTokenMs = &ms
			}
			if acceptTerminalState {
				s.parseSSEUsageBytesForEvent(dataBytes, usage, eventType)
			}
			return
		}

		if suppressLine {
			return
		}
		shouldFlush := line == "" && (eventShouldFlush || (queueDrained && clientOutputStarted))
		if line == "" {
			eventShouldFlush = false
		}
		if !clientDisconnected {
			if _, err := bufferedWriter.WriteString(line); err != nil {
				clientDisconnected = true
				logger.LegacyPrintf("service.openai_gateway", "Client disconnected during streaming, continuing to drain upstream for billing")
			} else if _, err := bufferedWriter.WriteString("\n"); err != nil {
				clientDisconnected = true
				logger.LegacyPrintf("service.openai_gateway", "Client disconnected during streaming, continuing to drain upstream for billing")
			} else if shouldFlush {
				if err := flushBuffered(); err != nil {
					clientDisconnected = true
					logger.LegacyPrintf("service.openai_gateway", "Client disconnected during streaming flush, continuing to drain upstream for billing")
				} else {
					clientOutputStarted = true
					lastDownstreamWriteAt = time.Now()
					eventInProgress = false
				}
			} else {
				eventInProgress = line != ""
			}
		}
	}

	// 无超时/无 keepalive 的常见路径走同步扫描，减少 goroutine 与 channel 开销。
	if streamInterval <= 0 && keepaliveInterval <= 0 {
		defer putSSEScannerBuf64K(scanBuf)
		for documentScanner.Scan() {
			processSSELine(documentScanner.Text(), true)
			if streamFailoverErr != nil {
				return resultWithUsage(), streamFailoverErr
			}
		}
		if result, err, done := handleScanErr(documentScanner.Err()); done {
			return result, err
		}
		return finalizeStream()
	}

	type scanEvent struct {
		line string
		err  error
	}
	// 独立 goroutine 读取上游，避免读取阻塞影响 keepalive/超时处理
	events := make(chan scanEvent, 16)
	done := make(chan struct{})
	sendEvent := func(ev scanEvent) bool {
		select {
		case events <- ev:
			return true
		case <-done:
			return false
		}
	}
	var lastReadAt int64
	atomic.StoreInt64(&lastReadAt, time.Now().UnixNano())
	go func(scanBuf *sseScannerBuf64K) {
		defer putSSEScannerBuf64K(scanBuf)
		defer close(events)
		for documentScanner.Scan() {
			atomic.StoreInt64(&lastReadAt, time.Now().UnixNano())
			if !sendEvent(scanEvent{line: documentScanner.Text()}) {
				return
			}
		}
		if err := documentScanner.Err(); err != nil {
			_ = sendEvent(scanEvent{err: err})
		}
	}(scanBuf)
	defer close(done)

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return finalizeStream()
			}
			if result, err, done := handleScanErr(ev.err); done {
				return result, err
			}
			processSSELine(ev.line, len(events) == 0)
			if streamFailoverErr != nil {
				return resultWithUsage(), streamFailoverErr
			}

		case <-intervalCh:
			lastRead := time.Unix(0, atomic.LoadInt64(&lastReadAt))
			if time.Since(lastRead) < streamInterval {
				continue
			}
			SetOpenAIRemoteCompactionSemanticOutcome(c, OpenAIRemoteCompactionSemanticOutcomeFailed)
			if clientDisconnected {
				return resultWithUsage(), fmt.Errorf("stream usage incomplete after timeout")
			}
			logger.LegacyPrintf("service.openai_gateway", "Stream data interval timeout: account=%d model=%s interval=%s", account.ID, originalModel, streamInterval)
			// 处理流超时，可能标记账户为临时不可调度或错误状态
			if s.rateLimitService != nil {
				s.rateLimitService.HandleStreamTimeout(ctx, account, originalModel)
			}
			sendErrorEvent("stream_timeout")
			return resultWithUsage(), fmt.Errorf("stream data interval timeout")

		case <-keepaliveCh:
			if clientDisconnected {
				continue
			}
			if eventInProgress {
				continue
			}
			if time.Since(lastDownstreamWriteAt) < keepaliveInterval {
				continue
			}
			if _, err := bufferedWriter.WriteString(":\n\n"); err != nil {
				clientDisconnected = true
				logger.LegacyPrintf("service.openai_gateway", "Client disconnected during streaming, continuing to drain upstream for billing")
				continue
			}
			if err := flushBuffered(); err != nil {
				clientDisconnected = true
				logger.LegacyPrintf("service.openai_gateway", "Client disconnected during keepalive flush, continuing to drain upstream for billing")
			} else {
				lastDownstreamWriteAt = time.Now()
			}
		}
	}

}

// extractOpenAISSEDataLine 低开销提取 SSE `data:` 行内容。
// 兼容 `data: xxx` 与 `data:xxx` 两种格式。
func extractOpenAISSEDataLine(line string) (string, bool) {
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	start := len("data:")
	for start < len(line) {
		if line[start] != ' ' && line[start] != '	' {
			break
		}
		start++
	}
	return line[start:], true
}

func extractOpenAISSEEventLine(line string) (string, bool) {
	if !strings.HasPrefix(line, "event:") {
		return "", false
	}
	start := len("event:")
	for start < len(line) {
		if line[start] != ' ' && line[start] != '	' {
			break
		}
		start++
	}
	return strings.TrimSpace(line[start:]), true
}

type openAICompatSSEFrame struct {
	EventType string
	Data      string
}

type openAICompatSSEFrameParser struct {
	eventType string
	dataLines []string
}

func forEachOpenAISSEFrame(body string, fn func(openAICompatSSEFrame)) {
	if fn == nil || strings.TrimSpace(body) == "" {
		return
	}
	eventType := ""
	dataLines := make([]string, 0, 4)
	flush := func() {
		emitOpenAISSEFrames(eventType, dataLines, fn)
		eventType = ""
		dataLines = dataLines[:0]
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if parsedEventType, ok := extractOpenAISSEEventLine(line); ok {
			eventType = parsedEventType
			continue
		}
		if data, ok := extractOpenAISSEDataLine(line); ok {
			dataLines = append(dataLines, data)
		}
	}
	flush()
}

func emitOpenAISSEFrames(eventType string, dataLines []string, fn func(openAICompatSSEFrame)) {
	if fn == nil || len(dataLines) == 0 {
		return
	}
	if len(dataLines) == 1 {
		fn(openAICompatSSEFrame{EventType: eventType, Data: dataLines[0]})
		return
	}
	joined := strings.Join(dataLines, "\n")
	if gjson.Valid(joined) {
		fn(openAICompatSSEFrame{EventType: eventType, Data: joined})
		return
	}
	for _, data := range dataLines {
		fn(openAICompatSSEFrame{EventType: eventType, Data: data})
	}
}

func (p *openAICompatSSEFrameParser) AddLine(line string) (openAICompatSSEFrame, bool) {
	if line == "" {
		return p.dispatch()
	}
	if strings.HasPrefix(line, ":") {
		return openAICompatSSEFrame{}, false
	}
	if eventType, ok := extractOpenAISSEEventLine(line); ok {
		p.eventType = eventType
		return openAICompatSSEFrame{}, false
	}
	if data, ok := extractOpenAISSEDataLine(line); ok {
		p.dataLines = append(p.dataLines, data)
	}
	return openAICompatSSEFrame{}, false
}

func (p *openAICompatSSEFrameParser) Finish() (openAICompatSSEFrame, bool) {
	return p.dispatch()
}

func (p *openAICompatSSEFrameParser) dispatch() (openAICompatSSEFrame, bool) {
	frame := openAICompatSSEFrame{
		EventType: p.eventType,
		Data:      strings.Join(p.dataLines, "\n"),
	}
	p.eventType = ""
	p.dataLines = nil
	return frame, frame.Data != ""
}

func openAICompatPayloadWithEventType(payload, eventType string) string {
	eventType = strings.TrimSpace(eventType)
	if eventType == "" || strings.TrimSpace(payload) == "" || strings.TrimSpace(payload) == "[DONE]" {
		return payload
	}
	if gjson.Get(payload, "type").Exists() {
		return payload
	}
	patched, err := sjson.Set(payload, "type", eventType)
	if err != nil {
		return payload
	}
	return patched
}

func (s *OpenAIGatewayService) replaceModelInSSELine(line, fromModel, toModel string) string {
	data, ok := extractOpenAISSEDataLine(line)
	if !ok {
		return line
	}
	if data == "" || data == "[DONE]" {
		return line
	}

	// 使用 gjson 精确检查 model 字段，避免全量 JSON 反序列化
	if m := gjson.Get(data, "model"); m.Exists() && m.Str == fromModel {
		newData, err := sjson.Set(data, "model", toModel)
		if err != nil {
			return line
		}
		return "data: " + newData
	}

	// 检查嵌套的 response.model 字段
	if m := gjson.Get(data, "response.model"); m.Exists() && m.Str == fromModel {
		newData, err := sjson.Set(data, "response.model", toModel)
		if err != nil {
			return line
		}
		return "data: " + newData
	}

	return line
}

// correctToolCallsInResponseBody 修正响应体中的工具调用
func (s *OpenAIGatewayService) correctToolCallsInResponseBody(body []byte) []byte {
	if len(body) == 0 {
		return body
	}

	updated := body
	changed := false
	if s != nil && s.toolCorrector != nil {
		if corrected, correctedChanged := s.toolCorrector.CorrectToolCallsInSSEBytes(updated); correctedChanged {
			updated = corrected
			changed = true
		}
	}
	if normalized, normalizedChanged := normalizeOpenAIResponsesFunctionCallArguments(updated); normalizedChanged {
		updated = normalized
		changed = true
	}
	if changed {
		return updated
	}
	return body
}

func normalizeOpenAIResponsesFunctionCallArguments(payload []byte) ([]byte, bool) {
	if len(bytes.TrimSpace(payload)) == 0 ||
		!bytes.Contains(payload, []byte(`"arguments"`)) ||
		(!bytes.Contains(payload, []byte(`"function_call"`)) &&
			!bytes.Contains(payload, []byte(`function_call_arguments`)) &&
			!bytes.Contains(payload, []byte(`"custom_tool_call"`))) {
		return payload, false
	}
	if !gjson.ValidBytes(payload) {
		return payload, false
	}

	updated := payload
	changed := false
	normalizeAtPath := func(path string) {
		value := gjson.GetBytes(updated, path)
		if !value.Exists() || value.Type != gjson.String {
			return
		}
		deduped, dedupedChanged := dedupeRepeatedJSONArgumentString(value.Str)
		if !dedupedChanged {
			return
		}
		next, err := sjson.SetBytes(updated, path, deduped)
		if err != nil {
			return
		}
		updated = next
		changed = true
	}

	if strings.TrimSpace(gjson.GetBytes(updated, "type").String()) == "response.function_call_arguments.done" {
		normalizeAtPath("arguments")
		normalizeAtPath("response.function_call_arguments.done.arguments")
	}

	if item := gjson.GetBytes(updated, "item"); item.Exists() && item.IsObject() {
		if openAIResponsesFunctionCallItemTypeAllowsArgumentsDedupe(item.Get("type").String()) {
			normalizeAtPath("item.arguments")
		}
	}

	if outputNormalized, outputChanged := normalizeOpenAIResponsesFunctionCallOutputArguments(updated); outputChanged {
		updated = outputNormalized
		changed = true
	}

	return updated, changed
}

func normalizeOpenAIResponsesFunctionCallOutputArguments(payload []byte) ([]byte, bool) {
	if len(bytes.TrimSpace(payload)) == 0 ||
		!bytes.Contains(payload, []byte(`"arguments"`)) ||
		!bytes.Contains(payload, []byte(`"output"`)) ||
		(!bytes.Contains(payload, []byte(`"function_call"`)) &&
			!bytes.Contains(payload, []byte(`"custom_tool_call"`))) {
		return payload, false
	}
	if !gjson.ValidBytes(payload) {
		return payload, false
	}

	updated := payload
	changed := false
	for _, root := range []string{"response.output", "output"} {
		count := int(gjson.GetBytes(updated, root+".#").Int())
		for i := 0; i < count; i++ {
			itemPath := root + "." + strconv.Itoa(i)
			if !openAIResponsesFunctionCallItemTypeAllowsArgumentsDedupe(gjson.GetBytes(updated, itemPath+".type").String()) {
				continue
			}
			path := itemPath + ".arguments"
			value := gjson.GetBytes(updated, path)
			if !value.Exists() || value.Type != gjson.String {
				continue
			}
			deduped, dedupedChanged := dedupeRepeatedJSONArgumentString(value.Str)
			if !dedupedChanged {
				continue
			}
			next, err := sjson.SetBytes(updated, path, deduped)
			if err != nil {
				continue
			}
			updated = next
			changed = true
		}
	}
	return updated, changed
}

func openAIResponsesFunctionCallItemTypeAllowsArgumentsDedupe(itemType string) bool {
	switch strings.TrimSpace(itemType) {
	case "function_call", "custom_tool_call":
		return true
	default:
		return false
	}
}

func dedupeRepeatedJSONArgumentString(argument string) (string, bool) {
	argument = strings.TrimSpace(argument)
	if len(argument) < 4 {
		return argument, false
	}
	firstRaw, firstEnd, ok := decodeFirstJSONRaw(argument)
	if !ok || !openAIResponsesArgumentJSONCanBeDeduped(firstRaw) {
		return argument, false
	}
	rest := strings.TrimSpace(argument[firstEnd:])
	if rest == "" {
		return argument, false
	}
	secondRaw, secondEnd, ok := decodeFirstJSONRaw(rest)
	if !ok || strings.TrimSpace(rest[secondEnd:]) != "" {
		return argument, false
	}
	firstRaw = bytes.TrimSpace(firstRaw)
	secondRaw = bytes.TrimSpace(secondRaw)
	if !bytes.Equal(firstRaw, secondRaw) {
		return argument, false
	}
	return string(firstRaw), true
}

func decodeFirstJSONRaw(input string) ([]byte, int, bool) {
	decoder := json.NewDecoder(strings.NewReader(input))
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return nil, 0, false
	}
	offset := decoder.InputOffset()
	if offset < 0 || offset > int64(len(input)) {
		return nil, 0, false
	}
	return raw, int(offset), true
}

func openAIResponsesArgumentJSONCanBeDeduped(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return false
	}
	return raw[0] == '{' || raw[0] == '['
}

func (s *OpenAIGatewayService) parseSSEUsage(data string, usage *OpenAIUsage) {
	s.parseSSEUsageBytes([]byte(data), usage)
}

func (s *OpenAIGatewayService) parseSSEUsageBytes(data []byte, usage *OpenAIUsage) {
	s.parseSSEUsageBytesForEvent(data, usage, strings.TrimSpace(gjson.GetBytes(data, "type").String()))
}

func (s *OpenAIGatewayService) parseSSEUsageBytesForEvent(data []byte, usage *OpenAIUsage, eventType string) {
	if usage == nil || len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return
	}
	// 选择性解析：仅在数据中包含终止事件标识时才进入字段提取。
	if len(data) < 72 {
		return
	}
	eventType = strings.TrimSpace(eventType)
	if eventType != "response.completed" && eventType != "response.done" && eventType != "response.failed" &&
		eventType != "response.incomplete" && eventType != "response.cancelled" && eventType != "response.canceled" {
		return
	}

	if parsedUsage, ok := extractOpenAIResponsesUsageFromJSONBytes(data); ok {
		*usage = parsedUsage
	}
}

func classifyOpenAIResponseSSEEvent(payload []byte, eventLineType string) string {
	eventLineType = strings.TrimSpace(eventLineType)
	eventType := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
	if eventLineType == "response.failed" || eventType == "response.failed" {
		return "response.failed"
	}
	if eventType != "" {
		return eventType
	}
	if isOpenAIResponseFailedPayload(payload) {
		return "response.failed"
	}
	return eventLineType
}

func openAIResponseFailedPayloadForDiagnostic(payload []byte, eventType string) []byte {
	if strings.TrimSpace(eventType) != "response.failed" || len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload
	}
	if strings.TrimSpace(gjson.GetBytes(payload, "type").String()) == "response.failed" {
		return payload
	}
	updated, err := sjson.SetBytes(payload, "type", "response.failed")
	if err != nil {
		return payload
	}
	return updated
}

func isOpenAIResponseFailedPayload(payload []byte) bool {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return false
	}
	status := strings.TrimSpace(gjson.GetBytes(payload, "response.status").String())
	if status == "" {
		status = strings.TrimSpace(gjson.GetBytes(payload, "status").String())
	}
	if status == "failed" {
		return true
	}
	return gjson.GetBytes(payload, "response.error").Exists() || gjson.GetBytes(payload, "error").Exists()
}

func openAIResponseStreamEventTypeIsTerminal(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
		return true
	default:
		return false
	}
}

func openAIResponseStreamTerminalIsFailure(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
		return true
	default:
		return false
	}
}

func extractOpenAIUsageFromJSONBytes(body []byte) (OpenAIUsage, bool) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return OpenAIUsage{}, false
	}
	if usage, ok := openAIUsageFromGJSON(gjson.GetBytes(body, "usage")); ok {
		return usage, true
	}
	return openAIUsageFromGJSON(gjson.GetBytes(body, "response.usage"))
}

// extractValidatedOpenAIResponsesUsageFromJSONBytes accepts only explicit,
// structurally usable token accounting for successful non-stream responses.
func extractValidatedOpenAIResponsesUsageFromJSONBytes(body []byte) (OpenAIUsage, bool) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return OpenAIUsage{}, false
	}
	usage := gjson.GetBytes(body, "usage")
	imageGen := gjson.GetBytes(body, "tool_usage.image_gen")
	if !usage.IsObject() {
		usage = gjson.GetBytes(body, "response.usage")
		imageGen = gjson.GetBytes(body, "response.tool_usage.image_gen")
	}
	if !usage.IsObject() {
		return OpenAIUsage{}, false
	}
	for _, field := range []string{
		"input_tokens_details",
		"prompt_tokens_details",
		"output_tokens_details",
		"completion_tokens_details",
	} {
		value := usage.Get(field)
		if value.Exists() && !value.IsObject() {
			return OpenAIUsage{}, false
		}
	}
	if imageGen.Exists() && !imageGen.IsObject() {
		return OpenAIUsage{}, false
	}
	for _, field := range []string{"input_tokens_details", "output_tokens_details"} {
		value := imageGen.Get(field)
		if value.Exists() && !value.IsObject() {
			return OpenAIUsage{}, false
		}
	}

	hasInputTokens := false
	for _, field := range []string{
		"input_tokens",
		"prompt_tokens",
		"output_tokens",
		"completion_tokens",
		"input_tokens_details.cached_tokens",
		"prompt_tokens_details.cached_tokens",
		"cache_creation_input_tokens",
		"cache_write_input_tokens",
		"cache_creation_tokens",
		"cache_write_tokens",
		"input_tokens_details.cache_write_tokens",
		"prompt_tokens_details.cache_write_tokens",
		"input_tokens_details.cache_creation_tokens",
		"prompt_tokens_details.cache_creation_tokens",
		"input_tokens_details.image_tokens",
		"prompt_tokens_details.image_tokens",
		"output_tokens_details.image_tokens",
		"completion_tokens_details.image_tokens",
	} {
		value := usage.Get(field)
		if !value.Exists() {
			continue
		}
		if !isNonNegativeGJSONInteger(value) {
			return OpenAIUsage{}, false
		}
		if field == "input_tokens" || field == "prompt_tokens" {
			hasInputTokens = true
		}
	}
	if !hasInputTokens {
		return OpenAIUsage{}, false
	}
	for _, field := range []string{
		"input_tokens_details.image_tokens",
		"output_tokens_details.image_tokens",
	} {
		value := imageGen.Get(field)
		if value.Exists() && !isNonNegativeGJSONInteger(value) {
			return OpenAIUsage{}, false
		}
	}
	parsedUsage, ok := openAIUsageFromGJSON(usage)
	if !ok {
		return OpenAIUsage{}, false
	}
	mergeHostedImageGenerationUsage(imageGen, &parsedUsage)
	return parsedUsage, true
}

func isNonNegativeGJSONInteger(value gjson.Result) bool {
	if value.Type != gjson.Number {
		return false
	}
	_, err := strconv.ParseUint(value.Raw, 10, strconv.IntSize)
	return err == nil
}

func extractOpenAIResponsesUsageFromJSONBytes(body []byte) (OpenAIUsage, bool) {
	usage, ok := extractOpenAIUsageFromJSONBytes(body)
	if !ok {
		return OpenAIUsage{}, false
	}
	imageGen := gjson.GetBytes(body, "tool_usage.image_gen")
	if !gjson.GetBytes(body, "usage").IsObject() {
		imageGen = gjson.GetBytes(body, "response.tool_usage.image_gen")
	}
	mergeHostedImageGenerationUsage(imageGen, &usage)
	return usage, true
}

func mergeHostedImageGenerationUsage(imageGen gjson.Result, usage *OpenAIUsage) {
	if usage == nil || !imageGen.Exists() || !imageGen.IsObject() {
		return
	}
	if usage.ImageInputTokens == 0 {
		if tokens, ok := positiveJSONInt(imageGen.Get("input_tokens_details.image_tokens")); ok {
			usage.ImageInputTokens = tokens
		}
	}
	if usage.ImageOutputTokens == 0 {
		if tokens, ok := positiveJSONInt(imageGen.Get("output_tokens_details.image_tokens")); ok {
			usage.ImageOutputTokens = tokens
		}
	}
}

func positiveJSONInt(value gjson.Result) (int, bool) {
	if !value.Exists() || value.Type != gjson.Number {
		return 0, false
	}
	tokens, err := strconv.ParseUint(value.Raw, 10, strconv.IntSize)
	if err != nil || tokens == 0 {
		return 0, false
	}
	return int(tokens), true
}

func extractOpenAIResponseIDFromJSONBytes(body []byte) string {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return ""
	}
	if id := strings.TrimSpace(gjson.GetBytes(body, "id").String()); id != "" {
		return id
	}
	return strings.TrimSpace(gjson.GetBytes(body, "response.id").String())
}

func (s *OpenAIGatewayService) bindHTTPResponseAccount(ctx context.Context, c *gin.Context, account *Account, responseID string) {
	if s == nil || account == nil || account.ID <= 0 {
		return
	}
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return
	}
	store := s.getOpenAIWSStateStore()
	if store == nil {
		return
	}
	groupID := getOpenAIGroupIDFromContext(c)
	logOpenAIWSBindResponseAccountWarn(groupID, account.ID, responseID, store.BindResponseAccount(ctx, groupID, responseID, account.ID, s.openAIWSResponseStickyTTL()))
}

func openAIUsageFromGJSON(value gjson.Result) (OpenAIUsage, bool) {
	if !value.Exists() || !value.IsObject() {
		return OpenAIUsage{}, false
	}
	inputTokens := value.Get("input_tokens").Int()
	if inputTokens == 0 {
		inputTokens = value.Get("prompt_tokens").Int()
	}
	outputTokens := value.Get("output_tokens").Int()
	if outputTokens == 0 {
		outputTokens = value.Get("completion_tokens").Int()
	}
	cacheReadTokens := value.Get("input_tokens_details.cached_tokens").Int()
	if cacheReadTokens == 0 {
		cacheReadTokens = value.Get("prompt_tokens_details.cached_tokens").Int()
	}
	cacheCreationTokens := int64(0)
	for _, path := range []string{
		"cache_creation_input_tokens",
		"cache_write_input_tokens",
		"cache_creation_tokens",
		"cache_write_tokens",
	} {
		if candidate := value.Get(path); candidate.Int() > 0 {
			cacheCreationTokens = candidate.Int()
			break
		}
	}
	for _, path := range []string{
		"input_tokens_details.cache_write_tokens",
		"prompt_tokens_details.cache_write_tokens",
		"input_tokens_details.cache_creation_tokens",
		"prompt_tokens_details.cache_creation_tokens",
	} {
		if candidate := value.Get(path); candidate.Exists() {
			// Official nested details are authoritative by field presence.
			cacheCreationTokens = candidate.Int()
			if cacheCreationTokens < 0 {
				cacheCreationTokens = 0
			}
			break
		}
	}
	imageInputTokens := value.Get("input_tokens_details.image_tokens").Int()
	if imageInputTokens == 0 {
		imageInputTokens = value.Get("prompt_tokens_details.image_tokens").Int()
	}
	imageOutputTokens := value.Get("output_tokens_details.image_tokens").Int()
	if imageOutputTokens == 0 {
		imageOutputTokens = value.Get("completion_tokens_details.image_tokens").Int()
	}
	return OpenAIUsage{
		InputTokens:              int(inputTokens),
		ImageInputTokens:         int(imageInputTokens),
		OutputTokens:             int(outputTokens),
		CacheCreationInputTokens: int(cacheCreationTokens),
		CacheReadInputTokens:     int(cacheReadTokens),
		ImageOutputTokens:        int(imageOutputTokens),
	}, true
}

func (s *OpenAIGatewayService) handleNonStreamingResponse(ctx context.Context, resp *http.Response, c *gin.Context, account *Account, originalModel, mappedModel string) (*openaiNonStreamingResult, error) {
	body, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
	if err != nil {
		return nil, err
	}

	// Detect SSE responses for ALL account types via Content-Type header.
	// Some OpenAI-compatible upstreams (including other sub2api instances)
	// may return SSE even when stream=false was requested.
	if isEventStreamResponse(resp.Header) {
		return s.handleSSEToJSON(resp, c, body, originalModel, mappedModel)
	}
	bodyLooksLikeSSE := bodyHasSSEFraming(body)

	// For OAuth accounts, also fall back to a body-content heuristic because
	// the upstream may omit the Content-Type header while still sending SSE.
	// This heuristic is NOT applied to API-key accounts to avoid false
	// positives on JSON responses that coincidentally contain "data:" or
	// "event:" in their text content.
	if account.IsOpenAIOAuthLike() && bodyLooksLikeSSE {
		return s.handleSSEToJSON(resp, c, body, originalModel, mappedModel)
	}

	usageValue, usageOK := extractValidatedOpenAIResponsesUsageFromJSONBytes(body)
	if !usageOK {
		if bodyLooksLikeSSE {
			return s.handleSSEToJSON(resp, c, body, originalModel, mappedModel)
		}
		return nil, s.writeOpenAINonStreamingProtocolError(resp, c, "Upstream returned invalid usage")
	}
	usage := &usageValue
	responseID := extractOpenAIResponseIDFromJSONBytes(body)

	// Replace model in response if needed
	if originalModel != mappedModel {
		body = s.replaceModelInResponseBody(body, mappedModel, originalModel)
	}
	body, err = restoreOpenAIResponsesNamespacePayload(c, body)
	if err != nil {
		return nil, fmt.Errorf("restore OpenAI namespace response: %w", err)
	}

	responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)

	contentType := "application/json"
	if s.cfg != nil && !s.cfg.Security.ResponseHeaders.Enabled {
		if upstreamType := resp.Header.Get("Content-Type"); upstreamType != "" {
			contentType = upstreamType
		}
	}

	if !writeOpenAICompactSSEBridge(c, resp.StatusCode, body) {
		c.Data(resp.StatusCode, contentType, body)
	}
	setOpenAIRemoteCompactionNonStreamingOutcome(c, body)

	return &openaiNonStreamingResult{
		OpenAIUsage:      usage,
		usage:            usage,
		responseID:       responseID,
		imageCount:       countOpenAIResponseImageOutputsFromJSONBytes(body),
		imageOutputSizes: collectOpenAIResponseImageOutputSizesFromJSONBytes(body),
	}, nil
}

func isEventStreamResponse(header http.Header) bool {
	contentType := strings.ToLower(header.Get("Content-Type"))
	return strings.Contains(contentType, "text/event-stream")
}

func bodyHasSSEFraming(body []byte) bool {
	for len(body) > 0 {
		line := body
		if newline := bytes.IndexByte(body, '\n'); newline >= 0 {
			line = body[:newline]
			body = body[newline+1:]
		} else {
			body = nil
		}
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		if bytes.HasPrefix(line, []byte("data:")) || bytes.HasPrefix(line, []byte("event:")) {
			return true
		}
	}
	return false
}

func (s *OpenAIGatewayService) handleSSEToJSON(resp *http.Response, c *gin.Context, body []byte, originalModel, mappedModel string) (*openaiNonStreamingResult, error) {
	bodyText := string(body)
	finalResponse, ok := extractCodexFinalResponse(bodyText)

	usage := &OpenAIUsage{}
	if ok {
		parsedUsage, parsed := extractValidatedOpenAIResponsesUsageFromJSONBytes(finalResponse)
		if !parsed {
			return nil, s.writeOpenAINonStreamingProtocolError(resp, c, "Upstream returned invalid usage")
		}
		*usage = parsedUsage
		// When the terminal event has an empty output array, reconstruct
		// output from accumulated delta events so the client gets full content.
		// gjson Array() returns empty slice for null, missing, or empty arrays.
		if len(gjson.GetBytes(finalResponse, "output").Array()) == 0 {
			if outputJSON, reconstructed := reconstructResponseOutputFromSSE(bodyText); reconstructed {
				if patched, err := sjson.SetRawBytes(finalResponse, "output", outputJSON); err == nil {
					finalResponse = patched
				}
			}
		}
		body = finalResponse
		if originalModel != mappedModel {
			body = s.replaceModelInResponseBody(body, mappedModel, originalModel)
		}
		// Correct tool calls in final response
		body = s.correctToolCallsInResponseBody(body)
		restoredBody, restoreErr := restoreOpenAIResponsesNamespacePayload(c, body)
		if restoreErr != nil {
			return nil, fmt.Errorf("restore OpenAI namespace response: %w", restoreErr)
		}
		body = restoredBody
	} else {
		terminalType, terminalPayload, terminalOK := extractOpenAISSETerminalEvent(bodyText)
		if terminalOK {
			if terminalType == "response.failed" {
				s.parseSSEUsageBytesForEvent(terminalPayload, usage, terminalType)
				fact := ParseOpenAIJSONErrorFact(PlatformOpenAI, UpstreamErrorSourceSSE, terminalPayload, resp.Header.Get("x-request-id"))
				requestErr := newRecognizedOpenAIUpstreamRequestError(fact)
				if requestErr == nil {
					requestErr = newOpenAIUpstreamRequestError(terminalPayload, resp.Header.Get("x-request-id"))
				}
				if requestErr != nil {
					requestErr.attachUsage(*usage)
					if strings.EqualFold(fact.ProviderCode, "context_length_exceeded") {
						clientPayload, _ := sanitizeOpenAIResponseFailedEventForClient(terminalPayload, terminalType)
						c.Data(requestErr.StatusCode, "application/json; charset=utf-8", clientPayload)
					} else {
						c.JSON(requestErr.StatusCode, gin.H{"error": gin.H{
							"code":    requestErr.Code,
							"type":    requestErr.Type,
							"message": requestErr.Message,
						}})
					}
					return &openaiNonStreamingResult{OpenAIUsage: usage, usage: usage}, requestErr
				}
				diagnosticPayload := openAIResponseFailedPayloadForDiagnostic(terminalPayload, terminalType)
				sanitizedPayload := sanitizeOpenAIStreamFailoverDiagnosticPayload(diagnosticPayload)
				msg := extractOpenAISSEErrorMessage(terminalPayload)
				if msg == "" {
					msg = "Upstream compact response failed"
				}
				return nil, s.writeOpenAINonStreamingProtocolError(resp, c, msg, sanitizedPayload)
			}
			return nil, s.writeOpenAINonStreamingProtocolError(resp, c, "Upstream compact response returned conflicting terminal events", terminalPayload)
		}
		return nil, s.writeOpenAINonStreamingProtocolError(resp, c, "Upstream returned invalid usage")
	}

	responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)

	contentType := "application/json; charset=utf-8"
	if !ok {
		contentType = resp.Header.Get("Content-Type")
		if contentType == "" {
			contentType = "text/event-stream"
		}
	}
	if !writeOpenAICompactSSEBridge(c, resp.StatusCode, body) {
		c.Data(resp.StatusCode, contentType, body)
	}
	if ok {
		setOpenAIRemoteCompactionNonStreamingOutcome(c, body)
	}

	return &openaiNonStreamingResult{
		OpenAIUsage:      usage,
		usage:            usage,
		responseID:       extractOpenAIResponseIDFromJSONBytes(body),
		imageCount:       countOpenAIImageOutputsFromSSEBody(bodyText),
		imageOutputSizes: collectOpenAIImageOutputSizesFromSSEBody(bodyText),
	}, nil
}

func extractOpenAISSETerminalEvent(body string) (string, []byte, bool) {
	var terminalType string
	var terminalPayload []byte
	forEachOpenAISSEFrame(body, func(frame openAICompatSSEFrame) {
		if terminalPayload != nil {
			return
		}
		data := []byte(strings.TrimSpace(frame.Data))
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			return
		}
		eventType := classifyOpenAIResponseSSEEvent(data, frame.EventType)
		if openAIResponseStreamEventTypeIsTerminal(eventType) {
			terminalType = eventType
			terminalPayload = append([]byte(nil), data...)
		}
	})
	if terminalPayload != nil {
		return terminalType, terminalPayload, true
	}
	return "", nil, false
}

func extractOpenAISSEErrorMessage(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	for _, path := range []string{"response.error.message", "error.message", "message"} {
		if msg := strings.TrimSpace(gjson.GetBytes(payload, path).String()); msg != "" {
			return sanitizeOpenAIUpstreamDiagnosticText(msg)
		}
	}
	return sanitizeOpenAIUpstreamDiagnosticText(strings.TrimSpace(extractUpstreamErrorMessage(payload)))
}

func sanitizeOpenAIResponseFailedEventForClient(payload []byte, eventType string) ([]byte, bool) {
	if eventType = strings.TrimSpace(eventType); eventType == "" {
		eventType = classifyOpenAIResponseSSEEvent(payload, "")
	}
	if eventType != "response.failed" || len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload, false
	}
	fact := ParseOpenAIJSONErrorFact(PlatformOpenAI, UpstreamErrorSourceSSE, payload, "")
	if requestErr := newRecognizedOpenAIUpstreamRequestError(fact); requestErr != nil {
		response := gin.H{
			"status": "failed",
			"error":  gin.H{"code": requestErr.Code, "type": requestErr.Type, "message": requestErr.Message},
		}
		if responseID := extractOpenAIResponseIDFromJSONBytes(payload); responseID != "" {
			response["id"] = responseID
		}
		sanitized, _ := json.Marshal(gin.H{"type": "response.failed", "response": response})
		return sanitized, true
	}
	if requestErr := newOpenAIUpstreamRequestError(payload, ""); requestErr != nil {
		response := gin.H{
			"status": "failed",
			"error":  gin.H{"code": requestErr.Code, "type": requestErr.Type, "message": requestErr.Message},
		}
		if responseID := extractOpenAIResponseIDFromJSONBytes(payload); responseID != "" {
			response["id"] = responseID
		}
		sanitized, _ := json.Marshal(gin.H{"type": "response.failed", "response": response})
		return sanitized, true
	}
	updated, changed, ok := deleteOpenAIResponseFailedVerboseFields(payload)
	if !ok {
		return payload, false
	}
	return updated, changed
}

func deleteOpenAIResponseFailedVerboseFields(payload []byte) ([]byte, bool, bool) {
	updated := payload
	for _, path := range []string{
		"instructions",
		"input",
		"output",
		"usage",
		"metadata",
		"reasoning",
		"tools",
		"tool_choice",
		"parallel_tool_calls",
		"prompt_cache_key",
		"previous_response_id",
		"text",
		"truncation",
		"max_output_tokens",
		"incomplete_details",
	} {
		for _, prefix := range []string{"", "response."} {
			next, err := sjson.DeleteBytes(updated, prefix+path)
			if err != nil {
				return payload, false, false
			}
			updated = next
		}
	}
	return updated, !bytes.Equal(updated, payload), true
}

func sanitizeOpenAIStreamFailoverDiagnosticPayload(payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}
	eventType := classifyOpenAIResponseSSEEvent(payload, "")
	if eventType != "response.failed" {
		return payload
	}
	if sanitized, changed := sanitizeOpenAIResponseFailedEventForClient(payload, eventType); changed {
		return sanitized
	}
	return payload
}

func (s *OpenAIGatewayService) writeOpenAINonStreamingProtocolError(resp *http.Response, c *gin.Context, message string, diagnosticPayload ...[]byte) error {
	message = sanitizeOpenAIUpstreamDiagnosticText(strings.TrimSpace(message))
	if message == "" {
		message = "Upstream returned an invalid non-streaming response"
	}
	detail := ""
	if len(diagnosticPayload) > 0 && len(diagnosticPayload[0]) > 0 && s != nil && s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
		maxBytes := s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes
		if maxBytes <= 0 {
			maxBytes = 2048
		}
		detail = sanitizeOpenAIUpstreamDiagnosticBodyForLog(diagnosticPayload[0], maxBytes)
	}
	setOpsUpstreamError(c, http.StatusBadGateway, message, detail)
	responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	c.Writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	c.JSON(http.StatusBadGateway, gin.H{
		"error": gin.H{
			"type":    "upstream_error",
			"message": message,
		},
	})
	return fmt.Errorf("non-streaming openai protocol error: %s", message)
}

func extractCodexFinalResponse(body string) ([]byte, bool) {
	var finalResponse []byte
	terminalFailure := false
	forEachOpenAISSEFrame(body, func(frame openAICompatSSEFrame) {
		data := []byte(strings.TrimSpace(frame.Data))
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			return
		}
		eventType := classifyOpenAIResponseSSEEvent(data, frame.EventType)
		if !openAIResponseStreamEventTypeIsTerminal(eventType) {
			return
		}
		if eventType != "response.done" && eventType != "response.completed" {
			terminalFailure = true
			return
		}
		if responseTerminalPayloadIsFailure(data) {
			terminalFailure = true
			return
		}
		if finalResponse != nil {
			terminalFailure = true
			return
		}
		if normalized, changed := normalizeCompletedImageGenerationStatusForEvent(data, eventType); changed {
			data = normalized
		}
		if response := gjson.GetBytes(data, "response"); response.Exists() && response.Type == gjson.JSON && response.Raw != "" {
			finalResponse = []byte(response.Raw)
		}
	})
	if finalResponse != nil && !terminalFailure {
		return finalResponse, true
	}
	return nil, false
}

func responseTerminalPayloadIsFailure(data []byte) bool {
	if len(data) == 0 || !gjson.ValidBytes(data) {
		return true
	}
	for _, path := range []string{"response.status", "status"} {
		status := strings.ToLower(strings.TrimSpace(gjson.GetBytes(data, path).String()))
		switch status {
		case "", "completed":
		case "failed", "incomplete", "cancelled", "canceled":
			return true
		default:
			return true
		}
	}
	for _, path := range []string{"response.error", "error"} {
		errorValue := gjson.GetBytes(data, path)
		if errorValue.Exists() && errorValue.Type != gjson.Null {
			return true
		}
	}
	return false
}

func normalizeCompletedImageGenerationStatus(data []byte) ([]byte, bool) {
	eventType := strings.TrimSpace(gjson.GetBytes(data, "type").String())
	return normalizeCompletedImageGenerationStatusForEvent(data, eventType)
}

func normalizeCompletedImageGenerationStatusForEvent(data []byte, eventType string) ([]byte, bool) {
	switch eventType {
	case "response.output_item.done", "response.completed", "response.done":
	default:
		return data, false
	}
	if len(data) == 0 || !gjson.ValidBytes(data) {
		return data, false
	}

	shouldNormalize := func(item gjson.Result) bool {
		if !item.Exists() || !item.IsObject() || strings.TrimSpace(item.Get("type").String()) != "image_generation_call" {
			return false
		}
		result := item.Get("result")
		if result.Type != gjson.String || strings.TrimSpace(result.String()) == "" {
			return false
		}
		switch strings.TrimSpace(item.Get("status").String()) {
		case "generating", "in_progress":
			return true
		default:
			return false
		}
	}

	switch eventType {
	case "response.output_item.done":
		if !shouldNormalize(gjson.GetBytes(data, "item")) {
			return data, false
		}
		updated, err := sjson.SetBytes(data, "item.status", "completed")
		if err != nil {
			return data, false
		}
		return updated, true
	case "response.completed", "response.done":
		response := gjson.GetBytes(data, "response")
		status := strings.ToLower(strings.TrimSpace(response.Get("status").String()))
		switch status {
		case "failed", "incomplete", "cancelled", "canceled":
			return data, false
		case "":
			// Some Codex-compatible terminals omit response.status; retain that
			// compatibility while rejecting explicit non-success states below.
		case "completed":
		default:
			return data, false
		}
		rootStatus := strings.ToLower(strings.TrimSpace(gjson.GetBytes(data, "status").String()))
		switch rootStatus {
		case "failed", "incomplete", "cancelled", "canceled":
			return data, false
		case "", "completed":
		default:
			return data, false
		}
		if responseError := response.Get("error"); responseError.Exists() && responseError.Type != gjson.Null {
			return data, false
		}
		if rootError := gjson.GetBytes(data, "error"); rootError.Exists() && rootError.Type != gjson.Null {
			return data, false
		}
		output := gjson.GetBytes(data, "response.output")
		if !output.Exists() || !output.IsArray() {
			return data, false
		}
		type replacement struct {
			start int
			end   int
		}
		replacements := make([]replacement, 0, 1)
		newLen := len(data)
		for _, item := range output.Array() {
			if !shouldNormalize(item) {
				continue
			}
			status := item.Get("status")
			if status.Index < 0 || status.Index+len(status.Raw) > len(data) {
				return data, false
			}
			replacements = append(replacements, replacement{
				start: status.Index,
				end:   status.Index + len(status.Raw),
			})
			newLen += len(`"completed"`) - len(status.Raw)
		}
		if len(replacements) == 0 {
			return data, false
		}
		updated := make([]byte, 0, newLen)
		previous := 0
		for _, replacement := range replacements {
			updated = append(updated, data[previous:replacement.start]...)
			updated = append(updated, `"completed"`...)
			previous = replacement.end
		}
		updated = append(updated, data[previous:]...)
		return updated, true
	default:
		return data, false
	}
}

func normalizeResponsesStreamingTerminalOutput(data []byte, acc *apicompat.BufferedResponseAccumulator, imageOutputs []json.RawMessage) ([]byte, bool) {
	eventType := strings.TrimSpace(gjson.GetBytes(data, "type").String())
	switch eventType {
	case "response.completed", "response.done", "response.incomplete", "response.cancelled", "response.canceled":
	default:
		return data, false
	}

	output := gjson.GetBytes(data, "response.output")
	hasAccumulatedOutput := (acc != nil && acc.HasContent()) || len(imageOutputs) > 0
	if output.Exists() && output.IsArray() {
		if len(output.Array()) > 0 || !hasAccumulatedOutput {
			return data, false
		}
	}

	outputJSON := []byte("[]")
	if reconstructed, ok := buildResponsesOutputJSON(acc, imageOutputs); ok {
		outputJSON = reconstructed
	}
	updated, err := sjson.SetRawBytes(data, "response.output", outputJSON)
	if err != nil {
		return data, false
	}
	return updated, true
}

func responsesStreamEventMayContributeToOutput(eventType string) bool {
	switch eventType {
	case "response.output_text.delta",
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.reasoning_summary_text.delta":
		return true
	default:
		return false
	}
}

// reconstructResponseOutputFromSSE scans raw SSE body text for delta events and
// returns a JSON-encoded output array reconstructed from accumulated deltas.
// Returns (nil, false) if no content was found in deltas.
func reconstructResponseOutputFromSSE(bodyText string) ([]byte, bool) {
	acc := apicompat.NewBufferedResponseAccumulator()
	imageOutputs := make([]json.RawMessage, 0, 1)
	seenImages := make(map[string]struct{})
	forEachOpenAISSEFrame(bodyText, func(frame openAICompatSSEFrame) {
		data := []byte(strings.TrimSpace(frame.Data))
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			return
		}
		eventType := classifyOpenAIResponseSSEEvent(data, frame.EventType)
		if eventType == "response.output_item.done" {
			if normalized, changed := normalizeCompletedImageGenerationStatusForEvent(data, eventType); changed {
				data = normalized
			}
		}
		if imageOutput, ok := extractImageGenerationOutputFromSSEData(data, eventType, seenImages); ok {
			imageOutputs = append(imageOutputs, imageOutput)
		}
		if responsesStreamEventMayContributeToOutput(eventType) {
			var event apicompat.ResponsesStreamEvent
			if err := json.Unmarshal([]byte(openAICompatPayloadWithEventType(string(data), eventType)), &event); err == nil {
				acc.ProcessEvent(&event)
			}
		}
	})
	return buildResponsesOutputJSON(acc, imageOutputs)
}

func buildResponsesOutputJSON(acc *apicompat.BufferedResponseAccumulator, imageOutputs []json.RawMessage) ([]byte, bool) {
	if (acc == nil || !acc.HasContent()) && len(imageOutputs) == 0 {
		return nil, false
	}
	var output []json.RawMessage
	if acc != nil && acc.HasContent() {
		outputJSON, err := json.Marshal(acc.BuildOutput())
		if err == nil {
			_ = json.Unmarshal(outputJSON, &output)
		}
	}
	output = append(output, imageOutputs...)
	if len(output) == 0 {
		return nil, false
	}

	outputJSON, err := json.Marshal(output)
	if err != nil {
		return nil, false
	}
	return outputJSON, true
}

func extractImageGenerationOutputFromSSEData(data []byte, eventType string, seen map[string]struct{}) (json.RawMessage, bool) {
	if len(data) == 0 || !gjson.ValidBytes(data) {
		return nil, false
	}
	if strings.TrimSpace(eventType) != "response.output_item.done" {
		return nil, false
	}
	item := gjson.GetBytes(data, "item")
	if !item.Exists() || !item.IsObject() || item.Get("type").String() != "image_generation_call" {
		return nil, false
	}
	if strings.TrimSpace(item.Get("result").String()) == "" {
		return nil, false
	}
	key := strings.TrimSpace(item.Get("id").String())
	if key == "" {
		key = strings.TrimSpace(item.Get("output_format").String()) + "|" + strings.TrimSpace(item.Get("result").String())
	}
	if key != "" && seen != nil {
		if _, exists := seen[key]; exists {
			return nil, false
		}
		seen[key] = struct{}{}
	}
	return json.RawMessage(item.Raw), true
}

func (s *OpenAIGatewayService) parseSSEUsageFromBody(body string) *OpenAIUsage {
	usage := &OpenAIUsage{}
	forEachOpenAISSEFrame(body, func(frame openAICompatSSEFrame) {
		data := []byte(strings.TrimSpace(frame.Data))
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			return
		}
		s.parseSSEUsageBytesForEvent(data, usage, classifyOpenAIResponseSSEEvent(data, frame.EventType))
	})
	return usage
}

func (s *OpenAIGatewayService) replaceModelInSSEBody(body, fromModel, toModel string) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if _, ok := extractOpenAISSEDataLine(line); !ok {
			continue
		}
		lines[i] = s.replaceModelInSSELine(line, fromModel, toModel)
	}
	return strings.Join(lines, "\n")
}

func (s *OpenAIGatewayService) validateUpstreamBaseURL(raw string) (string, error) {
	if _, err := normalizedAppendableUpstreamBaseURL(raw); err != nil {
		return "", fmt.Errorf("invalid base_url: %w", err)
	}
	if s.cfg != nil && !s.cfg.Security.URLAllowlist.Enabled {
		normalized, err := urlvalidator.ValidateURLFormat(raw, s.cfg.Security.URLAllowlist.AllowInsecureHTTP)
		if err != nil {
			return "", fmt.Errorf("invalid base_url: %w", err)
		}
		return normalized, nil
	}
	normalized, err := urlvalidator.ValidateHTTPSURL(raw, urlvalidator.ValidationOptions{
		AllowedHosts:     s.cfg.Security.URLAllowlist.UpstreamHosts,
		RequireAllowlist: true,
		AllowPrivate:     s.cfg.Security.URLAllowlist.AllowPrivateHosts,
	})
	if err != nil {
		return "", fmt.Errorf("invalid base_url: %w", err)
	}
	return normalized, nil
}

// buildOpenAIResponsesURL 组装 OpenAI Responses 端点。
// - base 以 /v1 结尾：追加 /responses
// - base 以其他版本段结尾（如 /v4）：追加 /responses
// - base 已是 /responses：原样返回
// - 其他情况：追加 /v1/responses
func buildOpenAIResponsesURL(base string) string {
	return buildOpenAIEndpointURL(base, "/v1/responses")
}

func trimOpenAIEncryptedReasoningItems(reqBody map[string]any) bool {
	if len(reqBody) == 0 {
		return false
	}

	inputValue, has := reqBody["input"]
	if !has {
		return false
	}

	switch input := inputValue.(type) {
	case []any:
		filtered := input[:0]
		changed := false
		for _, item := range input {
			nextItem, itemChanged, keep := sanitizeEncryptedReasoningInputItem(item)
			if itemChanged {
				changed = true
			}
			if !keep {
				continue
			}
			filtered = append(filtered, nextItem)
		}
		if !changed {
			return false
		}
		if len(filtered) == 0 {
			delete(reqBody, "input")
			return true
		}
		reqBody["input"] = filtered
		return true
	case []map[string]any:
		filtered := input[:0]
		changed := false
		for _, item := range input {
			nextItem, itemChanged, keep := sanitizeEncryptedReasoningInputItem(item)
			if itemChanged {
				changed = true
			}
			if !keep {
				continue
			}
			nextMap, ok := nextItem.(map[string]any)
			if !ok {
				filtered = append(filtered, item)
				continue
			}
			filtered = append(filtered, nextMap)
		}
		if !changed {
			return false
		}
		if len(filtered) == 0 {
			delete(reqBody, "input")
			return true
		}
		reqBody["input"] = filtered
		return true
	case map[string]any:
		nextItem, changed, keep := sanitizeEncryptedReasoningInputItem(input)
		if !changed {
			return false
		}
		if !keep {
			delete(reqBody, "input")
			return true
		}
		nextMap, ok := nextItem.(map[string]any)
		if !ok {
			return false
		}
		reqBody["input"] = nextMap
		return true
	default:
		return false
	}
}

func isOpenAIEncryptedContextErrorCode(code string) bool {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "invalid_encrypted_content", "thinking_signature_invalid":
		return true
	default:
		return false
	}
}

func sanitizeEncryptedReasoningInputItem(item any) (next any, changed bool, keep bool) {
	inputItem, ok := item.(map[string]any)
	if !ok {
		return item, false, true
	}

	itemType, _ := inputItem["type"].(string)
	switch strings.TrimSpace(itemType) {
	case "reasoning", "compaction":
		return nil, true, false
	default:
		return item, false, true
	}
}

func IsOpenAIResponsesCompactPathForTest(c *gin.Context) bool {
	return isOpenAIResponsesCompactPath(c)
}

func OpenAICompactSessionSeedKeyForTest() string {
	return openAICompactSessionSeedKey
}

func NormalizeOpenAICompactRequestBodyForTest(body []byte) ([]byte, bool, error) {
	return normalizeOpenAICompactRequestBody(body)
}

func isOpenAIResponsesCompactPath(c *gin.Context) bool {
	suffix := openAIResponsesRequestPathSuffix(c)
	return suffix == "/compact" || strings.HasPrefix(suffix, "/compact/")
}

// IsForwardableOpenAIResponsesRequestPath reports whether the decoded wildcard
// suffix can safely be appended to an upstream Responses URL.
func IsForwardableOpenAIResponsesRequestPath(c *gin.Context) bool {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return false
	}
	_, ok := sanitizedUpstreamPathSuffix(openAIResponsesRequestPathSuffixRaw(c))
	return ok
}

func normalizeOpenAICodexCompactReasoningEffortForAccount(c *gin.Context, account *Account, body []byte) ([]byte, bool, error) {
	if account == nil || !account.IsOpenAIOAuth() || !isOpenAIResponsesCompactPath(c) {
		return body, false, nil
	}

	requestedModel := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	effectiveModel := account.GetMappedModel(requestedModel)
	return normalizeOpenAICodexCompactReasoningEffort(body, effectiveModel)
}

func normalizeOpenAICodexCompactReasoningEffort(body []byte, effectiveModel string) ([]byte, bool, error) {
	if !isOpenAIGPT56Model(effectiveModel) ||
		!strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, "reasoning.effort").String()), "max") {
		return body, false, nil
	}

	// ChatGPT's compact endpoint currently accepts xhigh, not GPT-5.6's max.
	normalized, err := sjson.SetBytes(body, "reasoning.effort", "xhigh")
	if err != nil {
		return body, false, fmt.Errorf("normalize codex compact reasoning effort: %w", err)
	}
	return normalized, true, nil
}

func normalizeOpenAICompactRequestBody(body []byte) ([]byte, bool, error) {
	if len(body) == 0 {
		return body, false, nil
	}
	if !json.Valid(body) {
		return body, false, fmt.Errorf("normalize compact body: invalid JSON")
	}

	normalized := []byte(`{}`)
	// 对齐 Codex CompactionInput schema（9 字段），丢弃上游不接受的字段。
	for _, field := range []string{
		"model",
		"input",
		"instructions",
		"tools",
		"parallel_tool_calls",
		"reasoning",
		"service_tier",
		"prompt_cache_key",
		"text",
	} {
		value := gjson.GetBytes(body, field)
		if !value.Exists() {
			continue
		}
		next, err := sjson.SetRawBytes(normalized, field, []byte(value.Raw))
		if err != nil {
			return body, false, fmt.Errorf("normalize compact body %s: %w", field, err)
		}
		normalized = next
	}

	if bytes.Equal(bytes.TrimSpace(body), bytes.TrimSpace(normalized)) {
		return body, false, nil
	}
	return normalized, true, nil
}

func resolveOpenAICompactSessionID(c *gin.Context) string {
	if c != nil {
		if sessionID := strings.TrimSpace(c.GetHeader(openAICodexSessionIDHeader)); sessionID != "" {
			return sessionID
		}
		if sessionID := strings.TrimSpace(c.GetHeader("session_id")); sessionID != "" {
			return sessionID
		}
		if threadID := strings.TrimSpace(c.GetHeader(openAICodexThreadIDHeader)); threadID != "" {
			return threadID
		}
		if conversationID := strings.TrimSpace(c.GetHeader("conversation_id")); conversationID != "" {
			return conversationID
		}
		if seed, ok := c.Get(openAICompactSessionSeedKey); ok {
			if seedStr, ok := seed.(string); ok && strings.TrimSpace(seedStr) != "" {
				return strings.TrimSpace(seedStr)
			}
		}
	}
	return uuid.NewString()
}

func openAIResponsesRequestPathSuffix(c *gin.Context) string {
	suffix, ok := sanitizedUpstreamPathSuffix(openAIResponsesRequestPathSuffixRaw(c))
	if !ok {
		return ""
	}
	return suffix
}

func openAIResponsesRequestPathSuffixRaw(c *gin.Context) string {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return ""
	}
	if wildcard, ok := c.Params.Get("subpath"); ok {
		return wildcard
	}

	// Direct service tests do not pass through Gin's router. Accept only the
	// known route prefixes so an embedded later "/responses" cannot become the
	// validation anchor for a different suffix.
	path := c.Request.URL.Path
	for _, prefix := range []string{"/backend-api/codex/responses", "/openai/v1/responses", "/v1/responses", "/responses"} {
		if path == prefix {
			return ""
		}
		if strings.HasPrefix(path, prefix+"/") {
			return path[len(prefix):]
		}
	}
	return ""
}

func appendOpenAIResponsesRequestPathSuffix(baseURL, suffix string) string {
	trimmedBase := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	validatedSuffix, ok := sanitizedUpstreamPathSuffix(suffix)
	if trimmedBase == "" || !ok || validatedSuffix == "" {
		return trimmedBase
	}
	return trimmedBase + validatedSuffix
}

func (s *OpenAIGatewayService) replaceModelInResponseBody(body []byte, fromModel, toModel string) []byte {
	// 使用 gjson/sjson 精确替换 model 字段，避免全量 JSON 反序列化
	if m := gjson.GetBytes(body, "model"); m.Exists() && m.Str == fromModel {
		newBody, err := sjson.SetBytes(body, "model", toModel)
		if err != nil {
			return body
		}
		return newBody
	}
	return body
}

// OpenAIRecordUsageInput input for recording usage
type OpenAIRecordUsageInput struct {
	Result             *OpenAIForwardResult
	APIKey             *APIKey
	User               *User
	Account            *Account
	Subscription       *UserSubscription
	InboundEndpoint    string
	UpstreamEndpoint   string
	UserAgent          string // 请求的 User-Agent
	IPAddress          string // 请求的客户端 IP 地址
	RequestPayloadHash string
	APIKeyService      APIKeyQuotaUpdater
	QuotaPlatform      string // user×platform quota platform resolved before async billing.
	// PreserveAccountHealth prevents billable partial output from being mistaken for an upstream success.
	PreserveAccountHealth bool
	ChannelUsageFields
}

func validateOpenAIUsageForBilling(usage OpenAIUsage, imageCount int) error {
	checks := [...]struct {
		name  string
		value int
	}{
		{name: "input_tokens", value: usage.InputTokens},
		{name: "image_input_tokens", value: usage.ImageInputTokens},
		{name: "output_tokens", value: usage.OutputTokens},
		{name: "cache_creation_input_tokens", value: usage.CacheCreationInputTokens},
		{name: "cache_read_input_tokens", value: usage.CacheReadInputTokens},
		{name: "image_output_tokens", value: usage.ImageOutputTokens},
	}
	for _, check := range checks {
		if check.value < 0 {
			return fmt.Errorf("%s is negative: %d", check.name, check.value)
		}
		if check.value > openAIUsageBillingMaxTokenCount {
			return fmt.Errorf("%s exceeds billing guard: %d > %d", check.name, check.value, openAIUsageBillingMaxTokenCount)
		}
	}
	return validateImageCountForBilling(imageCount)
}

func validateOpenAIUsageCostForBilling(cost *CostBreakdown) error {
	if cost == nil {
		return nil
	}
	if math.IsNaN(cost.ActualCost) || math.IsInf(cost.ActualCost, 0) {
		return fmt.Errorf("actual_cost is not finite: %v", cost.ActualCost)
	}
	if cost.ActualCost < 0 {
		return fmt.Errorf("actual_cost is negative: %f", cost.ActualCost)
	}
	if cost.ActualCost > openAIUsageBillingMaxActualCost {
		return fmt.Errorf("actual_cost exceeds billing guard: %f > %f", cost.ActualCost, openAIUsageBillingMaxActualCost)
	}
	return nil
}

func logOpenAIUsageBillingGuard(reason string, result *OpenAIForwardResult, input *OpenAIRecordUsageInput, extra map[string]any, err error) {
	fields := []zap.Field{
		zap.String("component", "service.openai_gateway"),
		zap.String("reason", reason),
		zap.Error(err),
	}
	if result != nil {
		fields = append(fields,
			zap.String("request_id", result.RequestID),
			zap.String("model", result.Model),
			zap.String("upstream_model", result.UpstreamModel),
			zap.Int("input_tokens", result.Usage.InputTokens),
			zap.Int("output_tokens", result.Usage.OutputTokens),
			zap.Int("cache_creation_input_tokens", result.Usage.CacheCreationInputTokens),
			zap.Int("cache_read_input_tokens", result.Usage.CacheReadInputTokens),
			zap.Int("image_input_tokens", result.Usage.ImageInputTokens),
			zap.Int("image_output_tokens", result.Usage.ImageOutputTokens),
		)
	}
	if input != nil {
		if input.APIKey != nil {
			fields = append(fields, zap.Int64("api_key_id", input.APIKey.ID))
		}
		if input.User != nil {
			fields = append(fields, zap.Int64("user_id", input.User.ID))
		}
		if input.Account != nil {
			fields = append(fields, zap.Int64("account_id", input.Account.ID))
		}
	}
	if len(extra) > 0 {
		for key, value := range extra {
			fields = append(fields, zap.Any(key, value))
		}
	}
	logger.L().Warn("openai_usage.billing_guard_skip", fields...)
}

// RecordUsage records usage and deducts balance
// ResolveUserGroupRateMultiplier exposes the multiplier resolver used by OpenAI usage billing.
func (s *OpenAIGatewayService) ResolveUserGroupRateMultiplier(ctx context.Context, userID, groupID int64, groupDefaultMultiplier float64) float64 {
	if s == nil {
		return groupDefaultMultiplier
	}
	resolver := s.userGroupRateResolver
	if resolver == nil {
		resolver = newUserGroupRateResolver(nil, nil, resolveUserGroupRateCacheTTL(s.cfg), nil, "service.openai_gateway")
	}
	return resolver.Resolve(ctx, userID, groupID, groupDefaultMultiplier)
}

func (s *OpenAIGatewayService) RecordUsage(ctx context.Context, input *OpenAIRecordUsageInput) error {
	if input == nil {
		return errors.New("openai usage input is nil")
	}
	if input.Result == nil {
		return errors.New("openai usage result is nil")
	}
	resultCopy := *input.Result
	result := &resultCopy
	apiKey := input.APIKey
	user := input.User
	account := input.Account
	subscription := input.Subscription
	if err := validateOpenAIUsageForBilling(result.Usage, result.ImageCount); err != nil {
		logOpenAIUsageBillingGuard("usage", result, input, nil, err)
		return err
	}
	ApplyOpenAIImageBillingResolution(result)
	if err := validateImageCountForBilling(result.ImageCount); err != nil {
		logOpenAIUsageBillingGuard("usage", result, input, nil, err)
		return err
	}

	// 计算实际的新输入token（减去缓存读取的token）
	// 因为 input_tokens 包含了 cache_read_tokens，而缓存读取的token不应按输入价格计费
	actualInputTokens := result.Usage.InputTokens - result.Usage.CacheReadInputTokens
	if actualInputTokens < 0 {
		actualInputTokens = 0
	}

	// Calculate cost
	tokens := UsageTokens{
		InputTokens:         actualInputTokens,
		ImageInputTokens:    result.Usage.ImageInputTokens,
		OutputTokens:        result.Usage.OutputTokens,
		CacheCreationTokens: result.Usage.CacheCreationInputTokens,
		CacheReadTokens:     result.Usage.CacheReadInputTokens,
		ImageOutputTokens:   result.Usage.ImageOutputTokens,
	}

	// Get rate multiplier
	multiplier := 1.0
	if s.cfg != nil {
		multiplier = s.cfg.Default.RateMultiplier
	}
	if apiKey.GroupID != nil && apiKey.Group != nil {
		multiplier = s.ResolveUserGroupRateMultiplier(ctx, user.ID, *apiKey.GroupID, apiKey.Group.RateMultiplier)
	}
	imageMultiplier := resolveImageRateMultiplier(apiKey, multiplier)

	var cost *CostBreakdown
	var err error
	billingModel := forwardResultBillingModel(result.Model, result.UpstreamModel)
	if result.BillingModel != "" {
		billingModel = strings.TrimSpace(result.BillingModel)
	}
	if input.BillingModelSource == BillingModelSourceChannelMapped && input.ChannelMappedModel != "" && input.ChannelMappedModel != input.OriginalModel {
		billingModel = input.ChannelMappedModel
	}
	if input.BillingModelSource == BillingModelSourceRequested && input.OriginalModel != "" {
		billingModel = input.OriginalModel
	}
	billingModels := usageBillingModelCandidates(
		billingModel,
		result.BillingModel,
		input.ChannelMappedModel,
		input.OriginalModel,
		result.UpstreamModel,
		result.Model,
	)
	serviceTier := ""
	if result.ServiceTier != nil {
		serviceTier = strings.TrimSpace(*result.ServiceTier)
	}
	cost, err = s.calculateOpenAIRecordUsageCost(ctx, result, apiKey, billingModels, multiplier, imageMultiplier, tokens, serviceTier)
	if err != nil {
		if !isUsagePricingUnavailableError(err) {
			return err
		}
		logger.L().With(
			zap.String("component", "service.openai_gateway"),
			zap.Strings("billing_models", billingModels),
			zap.String("requested_model", input.OriginalModel),
			zap.String("mapped_model", input.ChannelMappedModel),
			zap.String("upstream_model", result.UpstreamModel),
			zap.Int64("api_key_id", apiKey.ID),
			zap.Int64("account_id", account.ID),
		).Warn("openai_usage.pricing_missing_record_zero_cost", zap.Error(err))
		cost = &CostBreakdown{BillingMode: string(BillingModeToken)}
	}
	if err := validateOpenAIUsageCostForBilling(cost); err != nil {
		actualCost := 0.0
		if cost != nil {
			actualCost = cost.ActualCost
		}
		logOpenAIUsageBillingGuard("cost", result, input, map[string]any{"actual_cost": actualCost}, err)
		return err
	}

	// Determine billing type
	isSubscriptionBilling := subscription != nil && apiKey.Group != nil && apiKey.Group.IsSubscriptionType()
	billingType := BillingTypeBalance
	if isSubscriptionBilling {
		billingType = BillingTypeSubscription
	}

	// Create usage log
	durationMs := int(result.Duration.Milliseconds())
	accountRateMultiplier := account.BillingRateMultiplier()
	requestID := resolveUsageBillingRequestID(ctx, result.RequestID, result.AttemptID)
	if result.OpenAIWSMode {
		if upstreamRequestID := strings.TrimSpace(result.RequestID); upstreamRequestID != "" {
			requestID = upstreamRequestID
		}
	}

	// 确定 RequestedModel（渠道映射前的原始模型）
	requestedModel := result.Model
	if input.OriginalModel != "" {
		requestedModel = input.OriginalModel
	}

	usageLog := &UsageLog{
		UserID:              user.ID,
		APIKeyID:            apiKey.ID,
		AccountID:           &account.ID,
		RequestID:           requestID,
		Model:               result.Model,
		RequestedModel:      requestedModel,
		UpstreamModel:       optionalNonEqualStringPtr(result.UpstreamModel, result.Model),
		ServiceTier:         result.ServiceTier,
		ReasoningEffort:     result.ReasoningEffort,
		InboundEndpoint:     optionalTrimmedStringPtr(input.InboundEndpoint),
		UpstreamEndpoint:    optionalTrimmedStringPtr(input.UpstreamEndpoint),
		InputTokens:         actualInputTokens,
		OutputTokens:        result.Usage.OutputTokens,
		CacheCreationTokens: result.Usage.CacheCreationInputTokens,
		CacheReadTokens:     result.Usage.CacheReadInputTokens,
		ImageOutputTokens:   result.Usage.ImageOutputTokens,
		ImageCount:          result.ImageCount,
		ImageSize:           optionalTrimmedStringPtr(result.ImageSize),
		ImageInputSize:      optionalTrimmedStringPtr(result.ImageInputSize),
		ImageOutputSize:     optionalTrimmedStringPtr(result.ImageOutputSize),
		ImageSizeSource:     optionalTrimmedStringPtr(result.ImageSizeSource),
		ImageSizeBreakdown:  result.ImageSizeBreakdown,
	}
	if cost != nil {
		usageLog.InputCost = cost.InputCost
		usageLog.OutputCost = cost.OutputCost
		usageLog.ImageOutputCost = cost.ImageOutputCost
		usageLog.CacheCreationCost = cost.CacheCreationCost
		usageLog.CacheReadCost = cost.CacheReadCost
		usageLog.TotalCost = cost.TotalCost
		usageLog.ActualCost = cost.ActualCost
	}
	if result.ImageCount > 0 && (cost == nil || cost.BillingMode != string(BillingModeToken)) {
		usageLog.RateMultiplier = imageMultiplier
	} else {
		usageLog.RateMultiplier = multiplier
	}
	usageLog.AccountRateMultiplier = &accountRateMultiplier
	usageLog.BillingType = billingType
	usageLog.Stream = result.Stream
	usageLog.OpenAIWSMode = result.OpenAIWSMode
	usageLog.DurationMs = &durationMs
	usageLog.FirstTokenMs = result.FirstTokenMs
	usageLog.CreatedAt = time.Now()
	// 设置渠道信息
	usageLog.ChannelID = optionalInt64Ptr(input.ChannelID)
	usageLog.ModelMappingChain = optionalTrimmedStringPtr(input.ModelMappingChain)
	// 设置计费模式
	if cost != nil && cost.BillingMode != "" {
		billingMode := cost.BillingMode
		usageLog.BillingMode = &billingMode
	} else if result.ImageCount > 0 {
		billingMode := string(BillingModeImage)
		usageLog.BillingMode = &billingMode
	} else {
		billingMode := string(BillingModeToken)
		usageLog.BillingMode = &billingMode
	}
	// 添加 UserAgent
	if input.UserAgent != "" {
		usageLog.UserAgent = &input.UserAgent
	}

	// 添加 IPAddress
	if input.IPAddress != "" {
		usageLog.IPAddress = &input.IPAddress
	}

	if apiKey.GroupID != nil {
		usageLog.GroupID = apiKey.GroupID
	}
	if subscription != nil {
		usageLog.SubscriptionID = &subscription.ID
	}

	// 计算账号统计定价费用（使用最终上游模型匹配自定义规则）
	if apiKey.GroupID != nil {
		applyAccountStatsCost(ctx, usageLog, s.channelService, s.billingService,
			account.ID, *apiKey.GroupID, result.UpstreamModel, result.Model,
			tokens, cost.TotalCost,
		)
	}

	if s.cfg != nil && s.cfg.RunMode == config.RunModeSimple {
		writeUsageLogBestEffort(ctx, s.usageLogRepo, usageLog, "service.openai_gateway")
		if !input.PreserveAccountHealth && s.rateLimitService != nil && account != nil && account.Platform == PlatformOpenAI {
			s.rateLimitService.ResetOpenAI403Counter(ctx, account.ID)
		}
		logger.LegacyPrintf("service.openai_gateway", "[SIMPLE MODE] Usage recorded (not billed): user=%d, tokens=%d", usageLog.UserID, usageLog.TotalTokens())
		if !input.PreserveAccountHealth {
			s.deferredService.ScheduleLastUsedUpdate(account.ID)
		}
		return nil
	}

	quotaPlatform := input.QuotaPlatform
	if quotaPlatform == "" {
		quotaPlatform = PlatformFromAPIKey(apiKey)
	}

	usageLogPersisted, billingErr := func() (bool, error) {
		applied, err := applyUsageBilling(ctx, requestID, usageLog, &postUsageBillingParams{
			Cost:                  cost,
			User:                  user,
			APIKey:                apiKey,
			Account:               account,
			Subscription:          subscription,
			RequestPayloadHash:    resolveUsageBillingPayloadFingerprint(ctx, input.RequestPayloadHash),
			IsSubscriptionBill:    isSubscriptionBilling,
			AccountRateMultiplier: accountRateMultiplier,
			APIKeyService:         input.APIKeyService,
			Platform:              quotaPlatform,
			PreserveAccountHealth: input.PreserveAccountHealth,
		}, s.billingDeps(), s.usageBillingRepo, s.billingOutboxRepo)
		return applied, err
	}()

	if billingErr != nil {
		if s.billingOutboxRepo != nil {
			// Preserve the immutable usage audit record when durable enqueue is
			// unavailable; direct Apply failures retain their all-or-nothing path.
			writeUsageLogBestEffort(ctx, s.usageLogRepo, usageLog, "service.openai_gateway")
		}
		return billingErr
	}
	if !usageLogPersisted {
		writeUsageLogBestEffort(ctx, s.usageLogRepo, usageLog, "service.openai_gateway")
	}
	if !input.PreserveAccountHealth && s.rateLimitService != nil && account != nil && account.Platform == PlatformOpenAI {
		s.rateLimitService.ResetOpenAI403Counter(ctx, account.ID)
	}

	return nil
}

func (s *OpenAIGatewayService) calculateOpenAIRecordUsageCost(
	ctx context.Context,
	result *OpenAIForwardResult,
	apiKey *APIKey,
	billingModels []string,
	multiplier float64,
	imageMultiplier float64,
	tokens UsageTokens,
	serviceTier string,
) (*CostBreakdown, error) {
	billingModel := firstUsageBillingModel(billingModels)
	if result != nil && result.ImageCount > 0 {
		// 渠道定价为 token 计费时走 token 路径，否则走图片计费
		if resolved := s.resolveOpenAIChannelPricing(ctx, billingModel, apiKey); resolved == nil || resolved.Mode != BillingModeToken {
			return s.calculateOpenAIImageCost(ctx, billingModel, apiKey, result, imageMultiplier)
		}
	}
	if len(billingModels) == 0 || billingModel == "" {
		return nil, errors.New("openai usage billing model is empty")
	}
	var lastErr error
	for _, candidate := range billingModels {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		cost, err := s.calculateOpenAIRecordUsageTokenCost(ctx, apiKey, candidate, multiplier, tokens, serviceTier)
		if err == nil {
			return cost, nil
		}
		if !isUsagePricingUnavailableError(err) {
			return nil, fmt.Errorf("calculate OpenAI usage cost failed for billing model %s: %w", candidate, err)
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no non-empty billing model candidates")
	}
	return nil, fmt.Errorf("calculate OpenAI usage cost failed for billing models %s: %w", strings.Join(billingModels, ","), lastErr)
}

func isUsagePricingUnavailableError(err error) bool {
	return errors.Is(err, ErrModelPricingUnavailable)
}

func (s *OpenAIGatewayService) calculateOpenAIRecordUsageTokenCost(
	ctx context.Context,
	apiKey *APIKey,
	billingModel string,
	multiplier float64,
	tokens UsageTokens,
	serviceTier string,
) (*CostBreakdown, error) {
	if s.resolver != nil && apiKey.Group != nil {
		gid := apiKey.Group.ID
		return s.billingService.CalculateCostUnified(CostInput{
			Ctx:                             ctx,
			Model:                           billingModel,
			GroupID:                         &gid,
			Tokens:                          tokens,
			RequestCount:                    1,
			RateMultiplier:                  multiplier,
			ServiceTier:                     serviceTier,
			Resolver:                        s.resolver,
			OpenAILongContextBillingEnabled: apiKey.Group.OpenAILongContextBillingEnabled,
		})
	}
	applyLongContext := apiKey != nil && apiKey.Group != nil && apiKey.Group.OpenAILongContextBillingEnabled
	return s.billingService.calculateCostInternalWithPolicy(billingModel, tokens, multiplier, serviceTier, nil, applyLongContext)
}

func (s *OpenAIGatewayService) calculateOpenAIImageCost(
	ctx context.Context,
	billingModel string,
	apiKey *APIKey,
	result *OpenAIForwardResult,
	multiplier float64,
) (*CostBreakdown, error) {
	sizeTier := NormalizeImageBillingTierOrDefault(result.ImageSize)
	if resolved := s.resolveOpenAIChannelPricing(ctx, billingModel, apiKey); resolved != nil &&
		(resolved.Mode == BillingModePerRequest || resolved.Mode == BillingModeImage) {
		gid := apiKey.Group.ID
		cost, err := s.billingService.CalculateCostUnified(CostInput{
			Ctx:            ctx,
			Model:          billingModel,
			GroupID:        &gid,
			RequestCount:   result.ImageCount,
			SizeTier:       sizeTier,
			RateMultiplier: multiplier,
			Resolver:       s.resolver,
			Resolved:       resolved,
		})
		if err == nil {
			return cost, nil
		}
		logger.LegacyPrintf("service.openai_gateway", "Calculate image channel cost failed: %v", err)
		return nil, err
	}

	var groupConfig *ImagePriceConfig
	if apiKey != nil && apiKey.Group != nil {
		groupConfig = &ImagePriceConfig{
			Price1K: apiKey.Group.ImagePrice1K,
			Price2K: apiKey.Group.ImagePrice2K,
			Price4K: apiKey.Group.ImagePrice4K,
		}
	}
	cost := s.billingService.CalculateImageCost(billingModel, sizeTier, result.ImageCount, groupConfig, multiplier)
	if err := validateCostBreakdownForBilling(cost); err != nil {
		return nil, err
	}
	return cost, nil
}

func (s *OpenAIGatewayService) resolveOpenAIChannelPricing(ctx context.Context, billingModel string, apiKey *APIKey) *ResolvedPricing {
	if s.resolver == nil || apiKey == nil || apiKey.Group == nil {
		return nil
	}
	gid := apiKey.Group.ID
	resolved := s.resolver.Resolve(ctx, PricingInput{Model: billingModel, GroupID: &gid})
	if resolved.Source == PricingSourceChannel {
		return resolved
	}
	return nil
}

// ParseCodexRateLimitHeaders extracts Codex usage limits from response headers.
// Exported for use in ratelimit_service when handling OpenAI 429 responses.
func ParseCodexRateLimitHeaders(headers http.Header) *OpenAICodexUsageSnapshot {
	snapshot := &OpenAICodexUsageSnapshot{}
	hasData := false

	// Helper to parse float64 from header
	parseFloat := func(key string) *float64 {
		if v := headers.Get(key); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return &f
			}
		}
		return nil
	}

	// Helper to parse int from header
	parseInt := func(key string) *int {
		if v := headers.Get(key); v != "" {
			if i, err := strconv.Atoi(v); err == nil {
				return &i
			}
		}
		return nil
	}

	// Primary (weekly) limits
	if v := parseFloat(openAICodexPrimaryUsedPercentHeader); v != nil {
		snapshot.PrimaryUsedPercent = v
		hasData = true
	}
	if v := parseInt(openAICodexPrimaryResetSecondsHeader); v != nil {
		snapshot.PrimaryResetAfterSeconds = v
		hasData = true
	}
	if v := parseInt(openAICodexPrimaryWindowMinutesHeader); v != nil {
		snapshot.PrimaryWindowMinutes = v
		hasData = true
	}

	// Secondary (5h) limits
	if v := parseFloat(openAICodexSecondUsedPercentHeader); v != nil {
		snapshot.SecondaryUsedPercent = v
		hasData = true
	}
	if v := parseInt(openAICodexSecondResetSecondsHeader); v != nil {
		snapshot.SecondaryResetAfterSeconds = v
		hasData = true
	}
	if v := parseInt(openAICodexSecondWindowMinutesHeader); v != nil {
		snapshot.SecondaryWindowMinutes = v
		hasData = true
	}

	// Overflow ratio
	if v := parseFloat(openAICodexPrimaryOverSecondHeader); v != nil {
		snapshot.PrimaryOverSecondaryPercent = v
		hasData = true
	}

	if !hasData {
		return nil
	}

	snapshot.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return snapshot
}

func codexSnapshotBaseTime(snapshot *OpenAICodexUsageSnapshot, fallback time.Time) time.Time {
	if snapshot == nil {
		return fallback
	}
	if snapshot.UpdatedAt == "" {
		return fallback
	}
	base, err := time.Parse(time.RFC3339, snapshot.UpdatedAt)
	if err != nil {
		return fallback
	}
	return base
}

func codexResetAtRFC3339(base time.Time, resetAfterSeconds *int) *string {
	if resetAfterSeconds == nil {
		return nil
	}
	sec := *resetAfterSeconds
	if sec < 0 {
		sec = 0
	}
	resetAt := base.Add(time.Duration(sec) * time.Second).Format(time.RFC3339)
	return &resetAt
}

func buildCodexUsageExtraUpdates(snapshot *OpenAICodexUsageSnapshot, fallbackNow time.Time) map[string]any {
	if snapshot == nil {
		return nil
	}

	baseTime := codexSnapshotBaseTime(snapshot, fallbackNow)
	updates := make(map[string]any)

	// 保存原始 primary/secondary 字段，便于排查问题
	if snapshot.PrimaryUsedPercent != nil {
		updates["codex_primary_used_percent"] = *snapshot.PrimaryUsedPercent
	}
	if snapshot.PrimaryResetAfterSeconds != nil {
		updates["codex_primary_reset_after_seconds"] = *snapshot.PrimaryResetAfterSeconds
	}
	if snapshot.PrimaryWindowMinutes != nil {
		updates["codex_primary_window_minutes"] = *snapshot.PrimaryWindowMinutes
	}
	if snapshot.SecondaryUsedPercent != nil {
		updates["codex_secondary_used_percent"] = *snapshot.SecondaryUsedPercent
	}
	if snapshot.SecondaryResetAfterSeconds != nil {
		updates["codex_secondary_reset_after_seconds"] = *snapshot.SecondaryResetAfterSeconds
	}
	if snapshot.SecondaryWindowMinutes != nil {
		updates["codex_secondary_window_minutes"] = *snapshot.SecondaryWindowMinutes
	}
	if snapshot.PrimaryOverSecondaryPercent != nil {
		updates["codex_primary_over_secondary_percent"] = *snapshot.PrimaryOverSecondaryPercent
	}
	updates["codex_usage_updated_at"] = baseTime.UTC().Format(time.RFC3339Nano)

	// 归一化到 5h/7d 规范字段
	if normalized := snapshot.Normalize(); normalized != nil {
		if normalized.Used5hPercent != nil {
			updates["codex_5h_used_percent"] = *normalized.Used5hPercent
		}
		if normalized.Reset5hSeconds != nil {
			updates["codex_5h_reset_after_seconds"] = *normalized.Reset5hSeconds
		}
		if normalized.Window5hMinutes != nil {
			updates["codex_5h_window_minutes"] = *normalized.Window5hMinutes
		}
		if normalized.Used7dPercent != nil {
			updates["codex_7d_used_percent"] = *normalized.Used7dPercent
		}
		if normalized.Reset7dSeconds != nil {
			updates["codex_7d_reset_after_seconds"] = *normalized.Reset7dSeconds
		}
		if normalized.Window7dMinutes != nil {
			updates["codex_7d_window_minutes"] = *normalized.Window7dMinutes
		}
		if reset5hAt := codexResetAtRFC3339(baseTime, normalized.Reset5hSeconds); reset5hAt != nil {
			updates["codex_5h_reset_at"] = *reset5hAt
		}
		if reset7dAt := codexResetAtRFC3339(baseTime, normalized.Reset7dSeconds); reset7dAt != nil {
			updates["codex_7d_reset_at"] = *reset7dAt
		}
	}

	return updates
}

// updateCodexUsageSnapshot saves the Codex usage snapshot to account's Extra field
func (s *OpenAIGatewayService) updateCodexUsageSnapshot(ctx context.Context, accountID int64, snapshot *OpenAICodexUsageSnapshot) {
	if snapshot == nil {
		return
	}
	if s == nil || s.accountRepo == nil {
		return
	}

	now := time.Now()
	updates := buildCodexUsageExtraUpdates(snapshot, now)
	if len(updates) == 0 {
		return
	}
	if !s.getCodexSnapshotThrottle().Allow(accountID, now) {
		return
	}

	go func() {
		updateCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		observedAt, err := runtimeExtraObservedAt(updates, "codex_usage_updated_at")
		if err != nil {
			return
		}
		updated, err := updateRuntimeExtra(updateCtx, s.accountRepo, accountID, updates, "codex_usage_updated_at", observedAt)
		if err != nil || !updated {
			return
		}
		syncCodexFiveHourSessionWindowEnd(updateCtx, s.accountRepo, accountID, updates, "response_headers")
	}()
}

func (s *OpenAIGatewayService) UpdateCodexUsageSnapshotFromHeaders(ctx context.Context, accountID int64, headers http.Header) {
	if accountID <= 0 || headers == nil {
		return
	}
	if snapshot := ParseCodexRateLimitHeaders(headers); snapshot != nil {
		s.updateCodexUsageSnapshot(ctx, accountID, snapshot)
	}
}

func getOpenAIReasoningEffortFromReqBody(reqBody map[string]any, requestedModel string) (value string, present bool) {
	if reqBody == nil {
		return "", false
	}

	// Primary: reasoning.effort
	if reasoning, ok := reqBody["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok {
			return normalizeOpenAIReasoningEffortForModel(effort, requestedModel), true
		}
	}

	// Fallback: some clients may use a flat field.
	if effort, ok := reqBody["reasoning_effort"].(string); ok {
		return normalizeOpenAIReasoningEffortForModel(effort, requestedModel), true
	}

	return "", false
}

func deriveOpenAIReasoningEffortFromModel(model string) string {
	if strings.TrimSpace(model) == "" {
		return ""
	}

	modelID := strings.TrimSpace(model)
	if strings.Contains(modelID, "/") {
		parts := strings.Split(modelID, "/")
		modelID = parts[len(parts)-1]
	}

	parts := strings.FieldsFunc(strings.ToLower(modelID), func(r rune) bool {
		switch r {
		case '-', '_', ' ':
			return true
		default:
			return false
		}
	})
	if len(parts) == 0 {
		return ""
	}

	return normalizeOpenAIReasoningEffortForModel(parts[len(parts)-1], modelID)
}

type openAIRequestView struct {
	body               []byte
	Model              string
	Stream             bool
	PromptCacheKey     string
	PreviousResponseID string
	ServiceTier        string
	ReasoningEffort    string
	patches            []openAIRequestPatch
	patchesDisabled    bool
}

type openAIRequestPatch struct {
	path   string
	delete bool
	value  any
}

func newOpenAIRequestView(body []byte) openAIRequestView {
	if len(body) == 0 {
		return openAIRequestView{}
	}
	return openAIRequestView{
		body:               body,
		Model:              strings.TrimSpace(gjson.GetBytes(body, "model").String()),
		Stream:             gjson.GetBytes(body, "stream").Bool(),
		PromptCacheKey:     strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String()),
		PreviousResponseID: strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String()),
		ServiceTier:        strings.TrimSpace(gjson.GetBytes(body, "service_tier").String()),
		ReasoningEffort:    strings.TrimSpace(gjson.GetBytes(body, "reasoning.effort").String()),
	}
}

// Decode 保留阶段一既有 full-map 行为；后续阶段会把调用点下沉到复杂分支。
func (v openAIRequestView) Decode(c *gin.Context) (map[string]any, error) {
	return getOpenAIRequestBodyMap(c, v.body)
}

// DecodeUseNumber 保留 JSON 数字文本，供 OAuth allowlist 终端路径避免大整数被 float64 舍入。
func (v openAIRequestView) DecodeUseNumber(_ *gin.Context) (map[string]any, error) {
	return decodeOpenAIRequestBodyMapUseNumber(v.body)
}

func (v *openAIRequestView) MarkPatchSet(path string, value any) {
	if v == nil || v.patchesDisabled {
		return
	}
	path = strings.TrimSpace(path)
	if !isSimpleOpenAIRequestPatchPath(path) {
		v.DisablePatches()
		return
	}
	v.patches = append(v.patches, openAIRequestPatch{path: path, value: value})
}

func (v *openAIRequestView) MarkPatchDelete(path string) {
	if v == nil || v.patchesDisabled {
		return
	}
	path = strings.TrimSpace(path)
	if !isSimpleOpenAIRequestPatchPath(path) {
		v.DisablePatches()
		return
	}
	v.patches = append(v.patches, openAIRequestPatch{path: path, delete: true})
}

func isSimpleOpenAIRequestPatchPath(path string) bool {
	if path == "" || strings.ContainsRune(path, '\\') {
		return false
	}
	for _, part := range strings.Split(path, ".") {
		if strings.TrimSpace(part) == "" {
			return false
		}
	}
	return true
}

func (v *openAIRequestView) DisablePatches() {
	if v == nil {
		return
	}
	v.patchesDisabled = true
	v.patches = nil
}

func (v openAIRequestView) HasPatches() bool {
	return !v.patchesDisabled && len(v.patches) > 0
}

func (v openAIRequestView) ApplyPatches() ([]byte, error) {
	if v.patchesDisabled || len(v.patches) == 0 {
		return nil, errors.New("openai request patches disabled")
	}
	body := v.body
	for _, patch := range v.patches {
		var err error
		if patch.delete {
			body, err = sjson.DeleteBytes(body, patch.path)
		} else {
			body, err = sjson.SetBytes(body, patch.path, patch.value)
		}
		if err != nil {
			return nil, err
		}
	}
	return body, nil
}

func setOpenAIRequestMapPath(reqBody map[string]any, path string, value any) {
	path = strings.TrimSpace(path)
	if reqBody == nil || path == "" {
		return
	}
	parts := strings.Split(path, ".")
	current := reqBody
	for _, part := range parts[:len(parts)-1] {
		part = strings.TrimSpace(part)
		if part == "" {
			return
		}
		next, _ := current[part].(map[string]any)
		if next == nil {
			next = map[string]any{}
			current[part] = next
		}
		current = next
	}
	last := strings.TrimSpace(parts[len(parts)-1])
	if last != "" {
		current[last] = value
	}
}

func deleteOpenAIRequestMapPath(reqBody map[string]any, path string) {
	path = strings.TrimSpace(path)
	if reqBody == nil || path == "" {
		return
	}
	parts := strings.Split(path, ".")
	current := reqBody
	for _, part := range parts[:len(parts)-1] {
		part = strings.TrimSpace(part)
		if part == "" {
			return
		}
		next, _ := current[part].(map[string]any)
		if next == nil {
			return
		}
		current = next
	}
	last := strings.TrimSpace(parts[len(parts)-1])
	if last != "" {
		delete(current, last)
	}
}

func extractOpenAIRequestMetaFromBody(body []byte) (model string, stream bool, promptCacheKey string) {
	view := newOpenAIRequestView(body)
	return view.Model, view.Stream, view.PromptCacheKey
}

func normalizeOpenAIOAuthHTTPUpstreamRequestBody(req *http.Request, c *gin.Context, account *Account, body []byte) ([]byte, error) {
	if account == nil || !account.IsOpenAIOAuthLike() {
		return body, nil
	}

	normalized, _, err := normalizeOpenAICodexCompactReasoningEffortForAccount(c, account, body)
	if err != nil {
		return body, err
	}
	normalized, _, err = normalizeOpenAIOAuthHTTPBody(normalized, isOpenAIResponsesCompactPath(c))
	if err != nil {
		return body, fmt.Errorf("normalize oauth body: %w", err)
	}
	if shouldStripOpenAIResponsesInputNamespaces(c, account) {
		normalized, err = stripOpenAIResponsesInputNamespaces(normalized)
		if err != nil {
			return body, fmt.Errorf("normalize oauth input namespaces: %w", err)
		}
	}
	resetHTTPRequestBody(req, normalized)
	return normalized, nil
}

// normalizeOpenAIOAuthHTTPBody 将 OpenAI OAuth HTTP 请求体收敛到 Codex CLI 实际发送的字段集合：
// 1) /responses/compact → Codex CompactionInput allowlist（9 字段）
// 2) 普通 HTTP /responses → Codex ResponsesApiRequest allowlist（14 字段）+ force store=false, stream=true
func normalizeOpenAIOAuthHTTPBody(body []byte, compact bool) ([]byte, bool, error) {
	if len(body) == 0 {
		return body, false, nil
	}

	if compact {
		return applyOpenAIOAuthCompactAllowlist(body)
	}

	return applyOpenAIOAuthHTTPAllowlist(body)
}

func extractOpenAIReasoningEffortFromBody(body []byte, modelCandidates ...string) *string {
	reasoningEffort := strings.TrimSpace(gjson.GetBytes(body, "reasoning.effort").String())
	if reasoningEffort == "" {
		reasoningEffort = strings.TrimSpace(gjson.GetBytes(body, "reasoning_effort").String())
	}
	if reasoningEffort != "" {
		normalized := normalizeOpenAIReasoningEffortForModel(reasoningEffort, firstNonEmpty(modelCandidates...))
		if normalized == "" {
			return nil
		}
		return &normalized
	}

	value := deriveOpenAIReasoningEffortFromModelCandidates(modelCandidates)
	if value == "" {
		return nil
	}
	return &value
}

func extractOpenAIServiceTier(reqBody map[string]any) *string {
	if reqBody == nil {
		return nil
	}
	raw, ok := reqBody["service_tier"].(string)
	if !ok {
		return nil
	}
	return normalizeOpenAIServiceTier(raw)
}

func extractOpenAIServiceTierFromBody(body []byte) *string {
	if len(body) == 0 {
		return nil
	}
	return normalizeOpenAIServiceTier(gjson.GetBytes(body, "service_tier").String())
}

func normalizeOpenAIServiceTier(raw string) *string {
	value := strings.ToLower(strings.TrimSpace(raw))
	if value == "" {
		return nil
	}
	if value == "fast" {
		value = "priority"
	}
	// 放过 OpenAI 官方文档定义的所有合法 tier 值：priority/flex/auto/default/scale。
	// 对 Codex 客户端零影响（Codex 只发 priority 或 flex，见 codex-rs/core/src/client.rs），
	// 但能让直连 OpenAI SDK 的用户透传 auto/default/scale 以便抓包/调试。
	// 真未知值仍返回 nil，由 normalizeResponsesBodyServiceTier 从 body 中删除。
	switch value {
	case "priority", "flex", "auto", "default", "scale":
		return &value
	default:
		return nil
	}
}

// OpenAIFastBlockedError indicates a request was rejected by the OpenAI fast
// policy (action=block). Mirrors BetaBlockedError on the Claude side.
type OpenAIFastBlockedError struct {
	Message string
}

func (e *OpenAIFastBlockedError) Error() string { return e.Message }

// evaluateOpenAIFastPolicy returns the action and error message that should be
// applied for a request with the given account/model/service_tier. When the
// policy service is unavailable or no rule matches, it returns
// (BetaPolicyActionPass, "") so callers can short-circuit safely.
//
// Matching rules:
//   - Scope filters by account type (all / oauth / apikey / bedrock)
//   - ServiceTier must be empty (= any), "all", or equal the normalized tier
//   - ModelWhitelist narrows the rule to specific models; FallbackAction
//     handles the non-matching case (default: pass)
//
// 与 Claude BetaPolicy 的差异（保留首条匹配 short-circuit）：
//   - BetaPolicy 处理的是 anthropic-beta header 中的 token 集合，不同
//     规则可能针对不同 token，filter 需要累加成 set；block 则 first-match。
//   - OpenAI fast policy 操作的是单个字段 service_tier：filter 即删字段，
//     没有可累加的对象。一次请求只携带一个 service_tier，规则的 tier
//     维度天然互斥；同一 (scope, tier) 下若多条规则的 model whitelist
//     发生重叠，admin 可通过规则顺序明确意图。因此采用 first-match 而
//     非 BetaPolicy 那样的"block 覆盖 filter 覆盖 pass"语义。
func (s *OpenAIGatewayService) evaluateOpenAIFastPolicy(ctx context.Context, account *Account, model, serviceTier string) (action, errMsg string) {
	if s == nil || s.settingService == nil {
		return BetaPolicyActionPass, ""
	}
	tier := strings.ToLower(strings.TrimSpace(serviceTier))
	if tier == "" {
		return BetaPolicyActionPass, ""
	}
	settings := openAIFastPolicySettingsFromContext(ctx)
	if settings == nil {
		fetched, err := s.settingService.GetOpenAIFastPolicySettings(ctx)
		if err != nil || fetched == nil {
			return BetaPolicyActionPass, ""
		}
		settings = fetched
	}
	return evaluateOpenAIFastPolicyWithSettings(settings, account, model, tier)
}

// evaluateOpenAIFastPolicyWithSettings is the pure-function core extracted so
// long-lived sessions (e.g. WS) can prefetch settings once and avoid hitting
// the settingService on every frame. See WSSession entry and
// openAIFastPolicySettingsFromContext for the caching glue.
func evaluateOpenAIFastPolicyWithSettings(settings *OpenAIFastPolicySettings, account *Account, model, tier string) (action, errMsg string) {
	if settings == nil {
		return BetaPolicyActionPass, ""
	}
	isOAuth := account != nil && account.IsOAuth()
	isBedrock := account != nil && account.IsBedrock()
	for _, rule := range settings.Rules {
		if !betaPolicyScopeMatches(rule.Scope, isOAuth, isBedrock) {
			continue
		}
		ruleTier := strings.ToLower(strings.TrimSpace(rule.ServiceTier))
		if ruleTier != "" && ruleTier != OpenAIFastTierAny && ruleTier != tier {
			continue
		}
		eff := BetaPolicyRule{
			Action:               rule.Action,
			ErrorMessage:         rule.ErrorMessage,
			ModelWhitelist:       rule.ModelWhitelist,
			FallbackAction:       rule.FallbackAction,
			FallbackErrorMessage: rule.FallbackErrorMessage,
		}
		return resolveRuleAction(eff, model)
	}
	return BetaPolicyActionPass, ""
}

// openAIFastPolicyCtxKey 是 context 中预取的 OpenAIFastPolicySettings 缓存
// 键，仅用于 WebSocket 长会话内多帧复用同一份策略快照，避免每帧 DB 命中。
//
// Trade-off：策略变更不会影响当前 WS session（只影响新 session）。这是
// 有意为之 —— 对长会话来说，"策略一致性"比"立刻生效"更重要，且 Claude
// BetaPolicy 的 gin.Context 缓存也是同样取舍。需要 hot-reload 时管理员
// 可以通过踢断 session 强制刷新。
type openAIFastPolicyCtxKeyType struct{}

var openAIFastPolicyCtxKey = openAIFastPolicyCtxKeyType{}

// withOpenAIFastPolicyContext 将一份 settings 快照绑定到 context，供该 ctx
// 衍生 goroutine 中的 evaluateOpenAIFastPolicy 复用。
func withOpenAIFastPolicyContext(ctx context.Context, settings *OpenAIFastPolicySettings) context.Context {
	if ctx == nil || settings == nil {
		return ctx
	}
	return context.WithValue(ctx, openAIFastPolicyCtxKey, settings)
}

func openAIFastPolicySettingsFromContext(ctx context.Context) *OpenAIFastPolicySettings {
	if ctx == nil {
		return nil
	}
	if v, ok := ctx.Value(openAIFastPolicyCtxKey).(*OpenAIFastPolicySettings); ok {
		return v
	}
	return nil
}

// shouldForceOpenAIPriorityTier reports whether the per-key override forces
// service_tier=priority for this API key.
func shouldForceOpenAIPriorityTier(apiKey *APIKey) bool {
	return apiKey != nil && apiKey.OpenAIForcePriorityTier
}

// forceOpenAIPriorityTierInBody returns body with service_tier="priority"
// injected; no-op when the override is off or the field already matches.
func forceOpenAIPriorityTierInBody(apiKey *APIKey, body []byte) ([]byte, error) {
	if !shouldForceOpenAIPriorityTier(apiKey) || len(body) == 0 {
		return body, nil
	}
	if gjson.GetBytes(body, "service_tier").String() == "priority" {
		return body, nil
	}
	updated, err := sjson.SetBytes(body, "service_tier", "priority")
	if err != nil {
		return body, fmt.Errorf("force service_tier=priority: %w", err)
	}
	return updated, nil
}

// applyOpenAIFastPolicyToBody applies the OpenAI fast policy to a raw request
// body. When action=filter it removes the service_tier field; when
// action=block it returns (body, *OpenAIFastBlockedError). On pass it
// normalizes the service_tier value (e.g. client alias "fast" → "priority"),
// rewriting the body so the upstream receives a slug it recognizes.
//
// Rationale for normalize-on-pass: chat-completions / messages 入口在调用本
// 函数之前已经通过 normalizeResponsesBodyServiceTier 把 service_tier 归一化
// 到了上游可识别值；passthrough（OpenAI 自动透传） / native /responses 等
// 入口没有这一前置步骤，pass 路径下若不在此处归一化，"fast" 就会被原样
// 透传到 OpenAI 上游导致 400/拒绝。把归一化收敛到本函数，所有入口行为一致。
func (s *OpenAIGatewayService) applyOpenAIFastPolicyToBody(ctx context.Context, account *Account, model string, body []byte) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}
	rawTier := gjson.GetBytes(body, "service_tier").String()
	if rawTier == "" {
		return body, nil
	}
	normTier := normalizedOpenAIServiceTierValue(rawTier)
	if normTier == "" {
		return body, nil
	}
	action, errMsg := s.evaluateOpenAIFastPolicy(ctx, account, model, normTier)
	switch action {
	case BetaPolicyActionBlock:
		msg := errMsg
		if msg == "" {
			msg = fmt.Sprintf("openai service_tier=%s is not allowed for model %s", normTier, model)
		}
		return body, &OpenAIFastBlockedError{Message: msg}
	case BetaPolicyActionFilter:
		trimmed, err := sjson.DeleteBytes(body, "service_tier")
		if err != nil {
			return body, fmt.Errorf("strip service_tier from body: %w", err)
		}
		return trimmed, nil
	default:
		// pass：把别名（如 "fast"）写回为规范值（"priority"）。
		if normTier == rawTier {
			return body, nil
		}
		updated, err := sjson.SetBytes(body, "service_tier", normTier)
		if err != nil {
			return body, fmt.Errorf("normalize service_tier on pass: %w", err)
		}
		return updated, nil
	}
}

// writeOpenAIFastPolicyBlockedResponse writes a 403 JSON response for a
// request blocked by the OpenAI fast policy.
func writeOpenAIFastPolicyBlockedResponse(c *gin.Context, err *OpenAIFastBlockedError) {
	if c == nil || err == nil {
		return
	}
	MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalPolicyDenied)
	c.JSON(http.StatusForbidden, gin.H{
		"error": gin.H{
			"type":    "permission_error",
			"message": err.Message,
		},
	})
}

// applyOpenAIFastPolicyToWSResponseCreate evaluates the OpenAI fast policy
// against a single client→upstream WebSocket frame whose top-level
// "type"=="response.create". It mirrors the HTTP-side
// applyOpenAIFastPolicyToBody contract but operates on a Realtime/Responses
// WS payload:
//
//   - pass: keeps service_tier, normalizing aliases such as "fast" to "priority"
//   - filter: returns a copy with top-level service_tier removed
//   - block: returns (frame, *OpenAIFastBlockedError)
//
// Only frames whose "type" field strictly equals "response.create" are
// inspected/mutated. Any other frame type — including the empty string —
// passes through untouched. The OpenAI Realtime client-event spec requires
// "type" to be set, so an empty type is treated as a malformed frame we do
// not police; the upstream is the source of truth for rejecting it.
//
// service_tier lives at the top level of response.create — same as the
// Responses HTTP body shape (see openai_gateway_chat_completions.go:304 +
// extractOpenAIServiceTierFromBody at line 5593, and the test fixture at
// openai_ws_forwarder_ingress_session_test.go:402). We therefore only need
// to inspect / strip the top-level field; there is no nested form in the
// schema today.
//
// The caller is responsible for choosing the upstream model passed in —
// this helper does not re-derive it.
func (s *OpenAIGatewayService) applyOpenAIFastPolicyToWSResponseCreate(
	ctx context.Context,
	account *Account,
	model string,
	frame []byte,
) ([]byte, *OpenAIFastBlockedError, error) {
	if len(frame) == 0 {
		return frame, nil, nil
	}
	if !gjson.ValidBytes(frame) {
		return frame, nil, nil
	}
	frameType := strings.TrimSpace(gjson.GetBytes(frame, "type").String())
	// Strict match: only response.create is policy-checked. Empty / other
	// types pass through untouched so we never accidentally strip fields
	// from response.cancel, conversation.item.create, or any future
	// client-event the spec adds. The Realtime spec requires "type" on
	// every client event, so an empty type is malformed input — let the
	// upstream reject it rather than guessing at our layer.
	if frameType != "response.create" {
		return frame, nil, nil
	}
	rawTier := gjson.GetBytes(frame, "service_tier").String()
	if rawTier == "" {
		return frame, nil, nil
	}
	normTier := normalizedOpenAIServiceTierValue(rawTier)
	if normTier == "" {
		return frame, nil, nil
	}
	action, errMsg := s.evaluateOpenAIFastPolicy(ctx, account, model, normTier)
	switch action {
	case BetaPolicyActionBlock:
		msg := errMsg
		if msg == "" {
			msg = fmt.Sprintf("openai service_tier=%s is not allowed for model %s", normTier, model)
		}
		return frame, &OpenAIFastBlockedError{Message: msg}, nil
	case BetaPolicyActionFilter:
		trimmed, err := sjson.DeleteBytes(frame, "service_tier")
		if err != nil {
			return frame, nil, fmt.Errorf("strip service_tier from ws frame: %w", err)
		}
		return trimmed, nil, nil
	default:
		if normTier == rawTier {
			return frame, nil, nil
		}
		updated, err := sjson.SetBytes(frame, "service_tier", normTier)
		if err != nil {
			return frame, nil, fmt.Errorf("normalize service_tier in ws frame: %w", err)
		}
		return updated, nil, nil
	}
}

// newOpenAIFastPolicyWSEventID returns a Realtime-style event_id for a
// server-emitted error event. Matches the loose "evt_<rand>" convention used
// by upstream Realtime servers; the exact value is not load-bearing and is
// only required for client-side log correlation. We reuse the existing
// google/uuid dependency rather than pulling a new one.
func newOpenAIFastPolicyWSEventID() string {
	id, err := uuid.NewRandom()
	if err != nil {
		// Extremely unlikely; fall back to a fixed prefix so the field is
		// still non-empty and the schema stays self-consistent.
		return "evt_openai_fast_policy"
	}
	// Strip dashes so it visually matches "evt_<hex>" rather than UUID v4
	// canonical form, mirroring what real Realtime traces look like.
	return "evt_" + strings.ReplaceAll(id.String(), "-", "")
}

// buildOpenAIFastPolicyBlockedWSEvent renders an OpenAI Realtime/Responses
// style "error" event payload for a request blocked by the OpenAI fast
// policy. The shape mirrors Realtime error events as observed in upstream
// traces and per the spec's server "error" event:
//
//	{
//	  "event_id": "evt_<random>",
//	  "type": "error",
//	  "error": {
//	    "type": "invalid_request_error",
//	    "code": "policy_violation",
//	    "message": "..."
//	  }
//	}
//
// event_id lets clients correlate the rejection in their logs; "code" gives
// programmatic clients a stable identifier (HTTP-side equivalent is the
// 403 permission_error JSON body).
func buildOpenAIFastPolicyBlockedWSEvent(err *OpenAIFastBlockedError) []byte {
	if err == nil {
		return nil
	}
	eventID := newOpenAIFastPolicyWSEventID()
	payload, mErr := json.Marshal(map[string]any{
		"event_id": eventID,
		"type":     "error",
		"error": map[string]any{
			"type":    "invalid_request_error",
			"code":    "policy_violation",
			"message": err.Message,
		},
	})
	if mErr != nil {
		// Fallback to a minimal hand-rolled payload; Marshal of the literal
		// shape above should never fail in practice.
		return []byte(`{"event_id":"` + eventID + `","type":"error","error":{"type":"invalid_request_error","code":"policy_violation","message":"openai fast policy blocked this request"}}`)
	}
	return payload
}

func openAIRequestBodyMayContainImageInput(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	input := gjson.GetBytes(body, "input")
	messages := gjson.GetBytes(body, "messages.#-1")
	return openAIJSONValueMayContainImageInput(input) || openAIJSONValueMayContainImageInput(messages)
}

func openAIJSONValueMayContainImageInput(value gjson.Result) bool {
	if !value.Exists() {
		return false
	}
	if value.IsArray() {
		found := false
		value.ForEach(func(_, item gjson.Result) bool {
			if openAIJSONValueMayContainImageInput(item) {
				found = true
				return false
			}
			return true
		})
		return found
	}
	if value.IsObject() {
		if strings.TrimSpace(value.Get("type").String()) == "input_image" || value.Get("image_url").Exists() {
			return true
		}
		return openAIJSONValueMayContainImageInput(value.Get("content"))
	}
	return false
}

func openAIRequestBodyMayContainEmptyBase64InputImage(body []byte) bool {
	if len(body) == 0 || !openAIRequestBodyMayContainInputImageToken(body) {
		return false
	}
	input := gjson.GetBytes(body, "input")
	if !input.Exists() {
		return false
	}
	return openAIJSONValueMayContainEmptyBase64InputImage(input)
}

func openAIRequestBodyMayContainInputImageToken(body []byte) bool {
	if bytes.Contains(body, []byte("input_image")) {
		return true
	}
	// JSON 字符串任意字符都可能被 unicode escape，遇到 \u 时交给 gjson 解码后的结构扫描兜底。
	return bytes.Contains(body, []byte("\\u"))
}

func openAIJSONValueMayContainEmptyBase64InputImage(value gjson.Result) bool {
	if !value.Exists() {
		return false
	}
	if value.IsArray() {
		found := false
		value.ForEach(func(_, item gjson.Result) bool {
			if openAIJSONValueMayContainEmptyBase64InputImage(item) {
				found = true
				return false
			}
			return true
		})
		return found
	}
	if value.IsObject() {
		if strings.TrimSpace(value.Get("type").String()) == "input_image" && isEmptyBase64DataURI(value.Get("image_url").String()) {
			return true
		}
		return openAIJSONValueMayContainEmptyBase64InputImage(value.Get("content"))
	}
	return false
}

func sanitizeEmptyBase64InputImagesInOpenAIBody(body []byte) ([]byte, bool, error) {
	if !openAIRequestBodyMayContainEmptyBase64InputImage(body) {
		return body, false, nil
	}

	var reqBody map[string]any
	if err := json.Unmarshal(body, &reqBody); err != nil {
		return body, false, fmt.Errorf("sanitize request body: %w", err)
	}
	if !sanitizeEmptyBase64InputImagesInOpenAIRequestBodyMap(reqBody) {
		return body, false, nil
	}
	normalized, err := marshalOpenAIUpstreamJSON(reqBody)
	if err != nil {
		return body, false, fmt.Errorf("serialize sanitized request body: %w", err)
	}
	return normalized, true, nil
}

func sanitizeEmptyBase64InputImagesInOpenAIRequestBodyMap(reqBody map[string]any) bool {
	if reqBody == nil {
		return false
	}
	input, ok := reqBody["input"]
	if !ok {
		return false
	}
	normalizedInput, changed := sanitizeEmptyBase64InputImagesInOpenAIInput(input)
	if !changed {
		return false
	}
	reqBody["input"] = normalizedInput
	return true
}

func sanitizeEmptyBase64InputImagesInOpenAIInput(input any) (any, bool) {
	items, ok := input.([]any)
	if !ok {
		return input, false
	}

	normalizedItems := make([]any, 0, len(items))
	changed := false
	for _, item := range items {
		itemMap, ok := item.(map[string]any)
		if !ok {
			normalizedItems = append(normalizedItems, item)
			continue
		}
		if shouldDropEmptyBase64InputImagePart(itemMap) {
			changed = true
			continue
		}
		content, ok := itemMap["content"]
		if !ok {
			normalizedItems = append(normalizedItems, itemMap)
			continue
		}
		parts, ok := content.([]any)
		if !ok {
			normalizedItems = append(normalizedItems, itemMap)
			continue
		}

		normalizedParts := make([]any, 0, len(parts))
		itemChanged := false
		for _, part := range parts {
			if shouldDropEmptyBase64InputImagePart(part) {
				changed = true
				itemChanged = true
				continue
			}
			normalizedParts = append(normalizedParts, part)
		}
		if itemChanged {
			if len(normalizedParts) == 0 {
				continue
			}
			itemMap["content"] = normalizedParts
		}
		normalizedItems = append(normalizedItems, itemMap)
	}
	if !changed {
		return input, false
	}
	return normalizedItems, true
}

func shouldDropEmptyBase64InputImagePart(part any) bool {
	partMap, ok := part.(map[string]any)
	if !ok {
		return false
	}
	typeValue, _ := partMap["type"].(string)
	if strings.TrimSpace(typeValue) != "input_image" {
		return false
	}
	imageURL, _ := partMap["image_url"].(string)
	return isEmptyBase64DataURI(imageURL)
}

func isEmptyBase64DataURI(raw string) bool {
	if !strings.HasPrefix(raw, "data:") {
		return false
	}
	rest := strings.TrimPrefix(raw, "data:")
	semicolonIdx := strings.Index(rest, ";")
	if semicolonIdx < 0 {
		return false
	}
	rest = rest[semicolonIdx+1:]
	if !strings.HasPrefix(rest, "base64,") {
		return false
	}
	return strings.TrimSpace(strings.TrimPrefix(rest, "base64,")) == ""
}

func getOpenAIRequestBodyMap(_ *gin.Context, body []byte) (map[string]any, error) {
	var reqBody map[string]any
	if err := json.Unmarshal(body, &reqBody); err != nil {
		return nil, fmt.Errorf("parse request: %w", err)
	}
	return reqBody, nil
}

func decodeOpenAIRequestBodyMapUseNumber(body []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var reqBody map[string]any
	if err := decoder.Decode(&reqBody); err != nil {
		return nil, fmt.Errorf("parse request: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("parse request: unexpected trailing JSON")
		}
		return nil, fmt.Errorf("parse request: %w", err)
	}
	return reqBody, nil
}

func extractOpenAIReasoningEffort(reqBody map[string]any, modelCandidates ...string) *string {
	if value, present := getOpenAIReasoningEffortFromReqBody(reqBody, firstNonEmpty(modelCandidates...)); present {
		if value == "" {
			return nil
		}
		return &value
	}

	value := deriveOpenAIReasoningEffortFromModelCandidates(modelCandidates)
	if value == "" {
		return nil
	}
	return &value
}

func deriveOpenAIReasoningEffortFromModelCandidates(models []string) string {
	for _, model := range models {
		if value := deriveOpenAIReasoningEffortFromModel(model); value != "" {
			return value
		}
	}
	return ""
}

func normalizeOpenAIReasoningEffortForModel(raw, model string) string {
	if strings.EqualFold(strings.TrimSpace(raw), "max") && isOpenAIGPT56Model(model) {
		return "max"
	}
	return normalizeOpenAIReasoningEffort(raw)
}

func normalizeOpenAIReasoningEffort(raw string) string {
	value := strings.ToLower(strings.TrimSpace(raw))
	if value == "" {
		return ""
	}

	// Normalize separators for "x-high"/"x_high" variants.
	value = strings.NewReplacer("-", "", "_", "", " ", "").Replace(value)

	switch value {
	case "none", "minimal":
		return ""
	case "low", "medium", "high":
		return value
	case "xhigh", "extrahigh", "max":
		return "xhigh"
	default:
		// Only store known effort levels for now to keep UI consistent.
		return ""
	}
}

func openAICompactClientWantsStream(c *gin.Context) bool {
	if c == nil {
		return false
	}
	value, ok := c.Get(openAICompactClientStreamKey)
	wants, _ := value.(bool)
	return ok && wants
}

func writeOpenAICompactSSEBridge(c *gin.Context, statusCode int, finalResponse []byte) bool {
	if c == nil || !openAICompactClientWantsStream(c) || statusCode < 200 || statusCode >= 300 {
		return false
	}
	payload, ok := buildOpenAICompactSSEPayload(finalResponse)
	if !ok {
		return false
	}
	h := c.Writer.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(statusCode)
	_, _ = c.Writer.Write(payload)
	c.Writer.Flush()
	return true
}

func buildOpenAICompactSSEPayload(finalResponse []byte) ([]byte, bool) {
	if len(finalResponse) == 0 || !gjson.ValidBytes(finalResponse) || !gjson.ParseBytes(finalResponse).IsObject() {
		return nil, false
	}
	var compacted bytes.Buffer
	if err := json.Compact(&compacted, finalResponse); err != nil {
		return nil, false
	}
	response := compacted.Bytes()
	if strings.TrimSpace(gjson.GetBytes(response, "id").String()) == "" {
		var err error
		response, err = sjson.SetBytes(response, "id", "resp_"+strings.ReplaceAll(uuid.NewString(), "-", ""))
		if err != nil {
			return nil, false
		}
	}
	if usage := gjson.GetBytes(response, "usage"); usage.Exists() && !openAICompactUsageParsableByCodex(usage) {
		var err error
		response, err = sjson.DeleteBytes(response, "usage")
		if err != nil {
			return nil, false
		}
	}
	var buf bytes.Buffer
	appendEvent := func(eventType string, data []byte) {
		_, _ = buf.WriteString("event: " + eventType + "\ndata: ")
		_, _ = buf.Write(data)
		_, _ = buf.WriteString("\n\n")
	}
	outputIndex := 0
	for _, item := range gjson.GetBytes(response, "output").Array() {
		if !item.IsObject() {
			continue
		}
		event, err := sjson.SetBytes([]byte(`{"type":"response.output_item.done"}`), "output_index", outputIndex)
		if err != nil {
			return nil, false
		}
		event, err = sjson.SetRawBytes(event, "item", []byte(item.Raw))
		if err != nil {
			return nil, false
		}
		appendEvent("response.output_item.done", event)
		outputIndex++
	}
	completed, err := sjson.SetRawBytes([]byte(`{"type":"response.completed"}`), "response", response)
	if err != nil {
		return nil, false
	}
	appendEvent("response.completed", completed)
	return buf.Bytes(), true
}

func openAICompactUsageParsableByCodex(usage gjson.Result) bool {
	if !usage.IsObject() {
		return false
	}
	for _, field := range []string{"input_tokens", "output_tokens", "total_tokens"} {
		if usage.Get(field).Type != gjson.Number {
			return false
		}
	}
	return true
}

type openAICompactSSEKeepalive struct {
	mu               sync.Mutex
	writer           gin.ResponseWriter
	started, stopped bool
	bytes            int
	stop             chan struct{}
}

func startOpenAICompactSSEKeepalive(c *gin.Context, interval time.Duration) func() {
	if c == nil || c.Writer == nil || interval <= 0 || !openAICompactClientWantsStream(c) {
		return func() {}
	}
	k := &openAICompactSSEKeepalive{writer: c.Writer, stop: make(chan struct{})}
	c.Set(openAICompactSSEKeepaliveKey, k)
	w := &openAICompactKeepaliveWriter{ResponseWriter: c.Writer, k: k}
	c.Writer = w
	var done <-chan struct{}
	if c.Request != nil {
		done = c.Request.Context().Done()
	}
	go func() {
		timer := time.NewTimer(interval)
		defer timer.Stop()
		for {
			select {
			case <-k.stop:
				return
			case <-done:
				return
			case <-timer.C:
			}
			if !k.beat() {
				return
			}
			timer.Reset(interval)
		}
	}()
	return func() {
		k.Stop()
		// The keepalive wrapper is request-scoped. Do not leave it retaining an
		// inner writer (which may itself be pooled) after this owner is done.
		if c.Writer == w {
			c.Writer = w.ResponseWriter
		}
		if current, ok := c.Get(openAICompactSSEKeepaliveKey); ok && current == k {
			delete(c.Keys, openAICompactSSEKeepaliveKey)
		}
	}
}
func (k *openAICompactSSEKeepalive) beat() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.stopped {
		return false
	}
	if !k.started {
		h := k.writer.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		h.Set("X-Accel-Buffering", "no")
		k.writer.WriteHeader(http.StatusOK)
		k.started = true
	}
	n, err := k.writer.Write([]byte(": keepalive\n\n"))
	k.bytes += n
	if err != nil {
		k.markStoppedLocked()
		return false
	}
	k.writer.Flush()
	return true
}
func (k *openAICompactSSEKeepalive) markStoppedLocked() {
	if !k.stopped {
		k.stopped = true
		close(k.stop)
	}
}
func (k *openAICompactSSEKeepalive) Stop() { k.mu.Lock(); k.markStoppedLocked(); k.mu.Unlock() }

// StopOpenAICompactSSEKeepaliveCommitted stops request keepalives and reports
// whether they already committed a 200 response. On return no later heartbeat
// can write, so the handler can safely inspect or take over the writer.
func StopOpenAICompactSSEKeepaliveCommitted(c *gin.Context) bool {
	if c == nil {
		return false
	}
	v, ok := c.Get(openAICompactSSEKeepaliveKey)
	if !ok {
		return false
	}
	k, ok := v.(*openAICompactSSEKeepalive)
	if !ok || k == nil {
		return false
	}
	k.mu.Lock()
	k.markStoppedLocked()
	committed := k.started
	k.mu.Unlock()
	return committed
}

func openAICompactKeepaliveAdjustedWrittenSize(c *gin.Context) int {
	if c == nil || c.Writer == nil {
		return -1
	}
	v, ok := c.Get(openAICompactSSEKeepaliveKey)
	if !ok {
		return c.Writer.Size()
	}
	k, ok := v.(*openAICompactSSEKeepalive)
	if !ok || k == nil {
		return c.Writer.Size()
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	size := k.writer.Size()
	if size < 0 {
		return size
	}
	if real := size - k.bytes; real > 0 {
		return real
	}
	return -1
}

type openAICompactKeepaliveWriter struct {
	gin.ResponseWriter
	k *openAICompactSSEKeepalive
}

func (w *openAICompactKeepaliveWriter) suspend() { w.k.Stop() }
func (w *openAICompactKeepaliveWriter) Header() http.Header {
	w.suspend()
	return w.ResponseWriter.Header()
}
func (w *openAICompactKeepaliveWriter) Write(b []byte) (int, error) {
	w.suspend()
	return w.ResponseWriter.Write(b)
}
func (w *openAICompactKeepaliveWriter) WriteString(v string) (int, error) {
	w.suspend()
	return w.ResponseWriter.WriteString(v)
}
func (w *openAICompactKeepaliveWriter) WriteHeader(v int) {
	w.suspend()
	w.ResponseWriter.WriteHeader(v)
}
func (w *openAICompactKeepaliveWriter) WriteHeaderNow() {
	w.suspend()
	w.ResponseWriter.WriteHeaderNow()
}
func (w *openAICompactKeepaliveWriter) Flush() { w.suspend(); w.ResponseWriter.Flush() }
func (w *openAICompactKeepaliveWriter) Status() int {
	w.k.mu.Lock()
	defer w.k.mu.Unlock()
	return w.ResponseWriter.Status()
}
func (w *openAICompactKeepaliveWriter) Size() int {
	w.k.mu.Lock()
	defer w.k.mu.Unlock()
	return w.ResponseWriter.Size()
}
func (w *openAICompactKeepaliveWriter) Written() bool {
	w.k.mu.Lock()
	defer w.k.mu.Unlock()
	return w.ResponseWriter.Written()
}

func mergeOpenAICompactTerminalOutput(finalResponse []byte, bodyText string) []byte {
	if len(finalResponse) == 0 || !gjson.ValidBytes(finalResponse) {
		return finalResponse
	}
	output := gjson.GetBytes(finalResponse, "output").Array()
	for _, existing := range output {
		if isOpenAICompactionOutputType(existing.Get("type").String()) {
			return finalResponse
		}
	}
	var raw []byte
	forEachOpenAISSEFrame(bodyText, func(frame openAICompatSSEFrame) {
		if raw != nil {
			return
		}
		data := []byte(strings.TrimSpace(frame.Data))
		if classifyOpenAIResponseSSEEvent(data, frame.EventType) != "response.output_item.done" {
			return
		}
		item := gjson.GetBytes(data, "item")
		if item.IsObject() && isOpenAICompactionOutputType(item.Get("type").String()) {
			raw = append([]byte(nil), item.Raw...)
		}
	})
	if raw == nil {
		return finalResponse
	}
	path := "output.-1"
	if len(output) == 0 {
		path = "output.0"
	}
	updated, err := sjson.SetRawBytes(finalResponse, path, raw)
	if err != nil {
		return finalResponse
	}
	return updated
}

func isOpenAICompactionOutputType(itemType string) bool {
	switch strings.TrimSpace(itemType) {
	case "compaction", "compaction_summary":
		return true
	default:
		return false
	}
}
