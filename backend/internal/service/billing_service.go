package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// APIKeyRateLimitCacheData holds rate limit usage data cached in Redis.
type APIKeyRateLimitCacheData struct {
	Usage5h  float64 `json:"usage_5h"`
	Usage1d  float64 `json:"usage_1d"`
	Usage7d  float64 `json:"usage_7d"`
	Window5h int64   `json:"window_5h"` // unix timestamp, 0 = not started
	Window1d int64   `json:"window_1d"`
	Window7d int64   `json:"window_7d"`
}

// UserPlatformQuotaKey 标识一个 user×platform，用于脏集出入与批量读。
type UserPlatformQuotaKey struct {
	UserID   int64
	Platform string
}

// UserPlatformQuotaCacheEntry Redis hash 反序列化结果。
//
// SchemaVersion 用于向后兼容：
//   - 0（旧 entry，无 SchemaVersion 字段）→ 视为 cache MISS，强制 refresh
//   - 1（当前版本）→ 包含 limits 和 window_start，可免 DB 查询
//
// limit 字段为 nil 表示"无限额"（DB 中对应列为 NULL）。
const UserPlatformQuotaCacheSchemaV1 = int64(1)

type UserPlatformQuotaCacheEntry struct {
	DailyUsageUSD   float64
	WeeklyUsageUSD  float64
	MonthlyUsageUSD float64
	Version         int64
	SchemaVersion   int64

	// 以下字段仅在 SchemaVersion >= 1 时有效
	DailyLimitUSD   *float64
	WeeklyLimitUSD  *float64
	MonthlyLimitUSD *float64

	DailyWindowStart   *time.Time
	WeeklyWindowStart  *time.Time
	MonthlyWindowStart *time.Time
}

// BillingCache defines cache operations for billing service
type BillingCache interface {
	// Balance operations
	GetUserBalance(ctx context.Context, userID int64) (float64, error)
	SetUserBalance(ctx context.Context, userID int64, balance float64) error
	DeductUserBalance(ctx context.Context, userID int64, amount float64) error
	InvalidateUserBalance(ctx context.Context, userID int64) error

	// Subscription operations
	GetSubscriptionCache(ctx context.Context, userID, groupID int64) (*SubscriptionCacheData, error)
	SetSubscriptionCache(ctx context.Context, userID, groupID int64, data *SubscriptionCacheData) error
	UpdateSubscriptionUsage(ctx context.Context, userID, groupID int64, cost float64) error
	InvalidateSubscriptionCache(ctx context.Context, userID, groupID int64) error

	// API Key rate limit operations
	GetAPIKeyRateLimit(ctx context.Context, keyID int64) (*APIKeyRateLimitCacheData, error)
	SetAPIKeyRateLimit(ctx context.Context, keyID int64, data *APIKeyRateLimitCacheData) error
	UpdateAPIKeyRateLimitUsage(ctx context.Context, keyID int64, cost float64) error
	InvalidateAPIKeyRateLimit(ctx context.Context, keyID int64) error

	// user × platform quota 缓存
	GetUserPlatformQuotaCache(ctx context.Context, userID int64, platform string) (*UserPlatformQuotaCacheEntry, bool, error)
	SetUserPlatformQuotaCache(ctx context.Context, userID int64, platform string, entry *UserPlatformQuotaCacheEntry, ttl time.Duration) error
	DeleteUserPlatformQuotaCache(ctx context.Context, userID int64, platform string) error
	// IncrUserPlatformQuotaUsageCache 在缓存命中时累加用量；缓存未命中（key 不存在）静默返回 nil。
	// markDirty=true 时将该 key 的 member 写入 Redis 脏集，供 flusher 批量回写 DB。
	IncrUserPlatformQuotaUsageCache(ctx context.Context, userID int64, platform string, cost float64, ttl time.Duration, markDirty bool) error

	// 脏集读写，供 flusher 使用。
	PopDirtyUserPlatformQuotaKeys(ctx context.Context, n int) ([]UserPlatformQuotaKey, error)
	ReaddDirtyUserPlatformQuotaKeys(ctx context.Context, keys []UserPlatformQuotaKey) error
	BatchGetUserPlatformQuotaCache(ctx context.Context, keys []UserPlatformQuotaKey) ([]*UserPlatformQuotaCacheEntry, error)
}

// ModelPricing 模型价格配置（per-token价格，与LiteLLM格式一致）
type ModelPricing struct {
	InputPricePerToken                 float64 // 每token输入价格 (USD)
	InputPricePerTokenPriority         float64 // priority service tier 下每token输入价格 (USD)
	ImageInputPricePerToken            float64 // 图片输入 token 价格 (USD)，为 0 时回退到输入价格
	OutputPricePerToken                float64 // 每token输出价格 (USD)
	OutputPricePerTokenPriority        float64 // priority service tier 下每token输出价格 (USD)
	CacheCreationPricePerToken         float64 // 缓存创建每token价格 (USD)
	CacheCreationPricePerTokenPriority float64 // priority service tier 下缓存创建每token价格 (USD)
	CacheCreationPriceExplicit         bool    // 是否由渠道/区间定价显式设定（为 true 时即使 == 0 也不回退）
	CacheReadPricePerToken             float64 // 缓存读取每token价格 (USD)
	CacheReadPricePerTokenPriority     float64 // priority service tier 下缓存读取每token价格 (USD)
	CacheCreation5mPrice               float64 // 5分钟缓存创建每token价格 (USD)
	CacheCreation1hPrice               float64 // 1小时缓存创建每token价格 (USD)
	SupportsCacheBreakdown             bool    // 是否支持详细的缓存分类
	LongContextInputThreshold          int     // 超过阈值后按整次会话提升输入价格
	LongContextInputMultiplier         float64 // 长上下文整次会话输入倍率
	LongContextOutputMultiplier        float64 // 长上下文整次会话输出倍率
	ImageOutputPricePerToken           float64 // 图片输出 token 价格 (USD)
	ImageOutputPriceExplicit           bool    // 是否由渠道定价显式设定（为 true 时即使 == 0 也不回退）
}

const (
	openAIGPT54LongContextInputThreshold   = 272000
	openAIGPT54LongContextInputMultiplier  = 2.0
	openAIGPT54LongContextOutputMultiplier = 1.5
)

func normalizeBillingServiceTier(serviceTier string) string {
	return strings.ToLower(strings.TrimSpace(serviceTier))
}

func usePriorityServiceTierPricing(serviceTier string, pricing *ModelPricing) bool {
	if pricing == nil || normalizeBillingServiceTier(serviceTier) != "priority" {
		return false
	}
	return pricing.InputPricePerTokenPriority > 0 || pricing.OutputPricePerTokenPriority > 0 ||
		pricing.CacheCreationPricePerTokenPriority > 0 || pricing.CacheReadPricePerTokenPriority > 0
}

func serviceTierCostMultiplier(serviceTier string) float64 {
	switch normalizeBillingServiceTier(serviceTier) {
	case "priority":
		return 2.0
	case "flex":
		return 0.5
	default:
		return 1.0
	}
}

// UsageTokens 使用的token数量
type UsageTokens struct {
	InputTokens           int
	ImageInputTokens      int
	OutputTokens          int
	CacheCreationTokens   int
	CacheReadTokens       int
	CacheCreation5mTokens int
	CacheCreation1hTokens int
	ImageOutputTokens     int
}

// CostBreakdown 费用明细
type CostBreakdown struct {
	InputCost         float64
	OutputCost        float64
	ImageOutputCost   float64
	CacheCreationCost float64
	CacheReadCost     float64
	TotalCost         float64
	ActualCost        float64 // 应用倍率后的实际费用
	BillingMode       string  // 计费模式（"token"/"per_request"/"image"），由 CalculateCostUnified 填充
}

// ErrModelPricingUnavailable indicates that none of the configured pricing
// sources can price the requested model.
var ErrModelPricingUnavailable = errors.New("pricing not found")

// BillingService 计费服务
type BillingService struct {
	cfg              *config.Config
	pricingService   *PricingService
	fallbackPrices   map[string]*ModelPricing // 硬编码回退价格
	fallbackWarnSeen sync.Map
}

// NewBillingService 创建计费服务实例
func NewBillingService(cfg *config.Config, pricingService *PricingService) *BillingService {
	s := &BillingService{
		cfg:            cfg,
		pricingService: pricingService,
		fallbackPrices: make(map[string]*ModelPricing),
	}

	// 初始化硬编码回退价格（当动态价格不可用时使用）
	s.initFallbackPricing()

	return s
}

// initFallbackPricing 初始化硬编码回退价格（当动态价格不可用时使用）
// 价格单位：USD per token（与LiteLLM格式一致）
func (s *BillingService) initFallbackPricing() {
	// Claude 4.5 Opus
	s.fallbackPrices["claude-opus-4.5"] = &ModelPricing{
		InputPricePerToken:         5e-6,    // $5 per MTok
		OutputPricePerToken:        25e-6,   // $25 per MTok
		CacheCreationPricePerToken: 6.25e-6, // $6.25 per MTok
		CacheReadPricePerToken:     0.5e-6,  // $0.50 per MTok
		SupportsCacheBreakdown:     false,
	}

	// Claude 4 Sonnet
	s.fallbackPrices["claude-sonnet-4"] = &ModelPricing{
		InputPricePerToken:         3e-6,    // $3 per MTok
		OutputPricePerToken:        15e-6,   // $15 per MTok
		CacheCreationPricePerToken: 3.75e-6, // $3.75 per MTok
		CacheReadPricePerToken:     0.3e-6,  // $0.30 per MTok
		SupportsCacheBreakdown:     false,
	}

	// Claude 3.5 Sonnet
	s.fallbackPrices["claude-3-5-sonnet"] = &ModelPricing{
		InputPricePerToken:         3e-6,    // $3 per MTok
		OutputPricePerToken:        15e-6,   // $15 per MTok
		CacheCreationPricePerToken: 3.75e-6, // $3.75 per MTok
		CacheReadPricePerToken:     0.3e-6,  // $0.30 per MTok
		SupportsCacheBreakdown:     false,
	}

	// Claude 3.5 Haiku
	s.fallbackPrices["claude-3-5-haiku"] = &ModelPricing{
		InputPricePerToken:         1e-6,    // $1 per MTok
		OutputPricePerToken:        5e-6,    // $5 per MTok
		CacheCreationPricePerToken: 1.25e-6, // $1.25 per MTok
		CacheReadPricePerToken:     0.1e-6,  // $0.10 per MTok
		SupportsCacheBreakdown:     false,
	}

	// Claude 3 Opus
	s.fallbackPrices["claude-3-opus"] = &ModelPricing{
		InputPricePerToken:         15e-6,    // $15 per MTok
		OutputPricePerToken:        75e-6,    // $75 per MTok
		CacheCreationPricePerToken: 18.75e-6, // $18.75 per MTok
		CacheReadPricePerToken:     1.5e-6,   // $1.50 per MTok
		SupportsCacheBreakdown:     false,
	}

	// Claude 3 Haiku
	s.fallbackPrices["claude-3-haiku"] = &ModelPricing{
		InputPricePerToken:         0.25e-6, // $0.25 per MTok
		OutputPricePerToken:        1.25e-6, // $1.25 per MTok
		CacheCreationPricePerToken: 0.3e-6,  // $0.30 per MTok
		CacheReadPricePerToken:     0.03e-6, // $0.03 per MTok
		SupportsCacheBreakdown:     false,
	}

	// Claude 4.6 Opus (与4.5同价)
	s.fallbackPrices["claude-opus-4.6"] = s.fallbackPrices["claude-opus-4.5"]

	// Claude 4.7 Opus (暂与4.6同价，待官方定价更新)
	s.fallbackPrices["claude-opus-4.7"] = s.fallbackPrices["claude-opus-4.6"]

	// Gemini 3.1 Pro
	s.fallbackPrices["gemini-3.1-pro"] = &ModelPricing{
		InputPricePerToken:         2e-6,   // $2 per MTok
		OutputPricePerToken:        12e-6,  // $12 per MTok
		CacheCreationPricePerToken: 2e-6,   // $2 per MTok
		CacheReadPricePerToken:     0.2e-6, // $0.20 per MTok
		SupportsCacheBreakdown:     false,
	}

	// OpenAI GPT-5.4（业务指定价格）
	s.fallbackPrices["gpt-5.4"] = &ModelPricing{
		InputPricePerToken:             2.5e-6,  // $2.5 per MTok
		InputPricePerTokenPriority:     5e-6,    // $5 per MTok
		OutputPricePerToken:            15e-6,   // $15 per MTok
		OutputPricePerTokenPriority:    30e-6,   // $30 per MTok
		CacheCreationPricePerToken:     2.5e-6,  // $2.5 per MTok
		CacheReadPricePerToken:         0.25e-6, // $0.25 per MTok
		CacheReadPricePerTokenPriority: 0.5e-6,  // $0.5 per MTok
		SupportsCacheBreakdown:         false,
		LongContextInputThreshold:      openAIGPT54LongContextInputThreshold,
		LongContextInputMultiplier:     openAIGPT54LongContextInputMultiplier,
		LongContextOutputMultiplier:    openAIGPT54LongContextOutputMultiplier,
	}
	// GPT-5.5 暂无独立定价，回退到 GPT-5.4
	s.fallbackPrices["gpt-5.5"] = s.fallbackPrices["gpt-5.4"]
	s.fallbackPrices["gpt-5.5-pro"] = s.fallbackPrices["gpt-5.4"]

	// OpenAI GPT-5.6 官方价格（USD/token）。缓存写入为输入价的 1.25 倍。
	s.fallbackPrices["gpt-5.6-sol"] = &ModelPricing{InputPricePerToken: 5e-6, InputPricePerTokenPriority: 10e-6, OutputPricePerToken: 30e-6, OutputPricePerTokenPriority: 60e-6, CacheCreationPricePerToken: 6.25e-6, CacheCreationPricePerTokenPriority: 12.5e-6, CacheReadPricePerToken: 0.5e-6, CacheReadPricePerTokenPriority: 1e-6, LongContextInputThreshold: openAIGPT54LongContextInputThreshold, LongContextInputMultiplier: openAIGPT54LongContextInputMultiplier, LongContextOutputMultiplier: openAIGPT54LongContextOutputMultiplier}
	s.fallbackPrices["gpt-5.6-terra"] = &ModelPricing{InputPricePerToken: 2.5e-6, InputPricePerTokenPriority: 5e-6, OutputPricePerToken: 15e-6, OutputPricePerTokenPriority: 30e-6, CacheCreationPricePerToken: 3.125e-6, CacheCreationPricePerTokenPriority: 6.25e-6, CacheReadPricePerToken: 0.25e-6, CacheReadPricePerTokenPriority: 0.5e-6, LongContextInputThreshold: openAIGPT54LongContextInputThreshold, LongContextInputMultiplier: openAIGPT54LongContextInputMultiplier, LongContextOutputMultiplier: openAIGPT54LongContextOutputMultiplier}
	s.fallbackPrices["gpt-5.6-luna"] = &ModelPricing{InputPricePerToken: 1e-6, InputPricePerTokenPriority: 2e-6, OutputPricePerToken: 6e-6, OutputPricePerTokenPriority: 12e-6, CacheCreationPricePerToken: 1.25e-6, CacheCreationPricePerTokenPriority: 2.5e-6, CacheReadPricePerToken: 0.1e-6, CacheReadPricePerTokenPriority: 0.2e-6, LongContextInputThreshold: openAIGPT54LongContextInputThreshold, LongContextInputMultiplier: openAIGPT54LongContextInputMultiplier, LongContextOutputMultiplier: openAIGPT54LongContextOutputMultiplier}

	s.fallbackPrices["gpt-5.4-mini"] = &ModelPricing{
		InputPricePerToken:     7.5e-7,
		OutputPricePerToken:    4.5e-6,
		CacheReadPricePerToken: 7.5e-8,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["gpt-5.4-nano"] = &ModelPricing{
		InputPricePerToken:     2e-7,
		OutputPricePerToken:    1.25e-6,
		CacheReadPricePerToken: 2e-8,
		SupportsCacheBreakdown: false,
	}
	// OpenAI GPT-5.2（本地兜底）
	s.fallbackPrices["gpt-5.2"] = &ModelPricing{
		InputPricePerToken:             1.75e-6,
		InputPricePerTokenPriority:     3.5e-6,
		OutputPricePerToken:            14e-6,
		OutputPricePerTokenPriority:    28e-6,
		CacheCreationPricePerToken:     1.75e-6,
		CacheReadPricePerToken:         0.175e-6,
		CacheReadPricePerTokenPriority: 0.35e-6,
		SupportsCacheBreakdown:         false,
	}
	// Codex 族兜底统一按 GPT-5.3 Codex 价格计费
	s.fallbackPrices["gpt-5.3-codex"] = &ModelPricing{
		InputPricePerToken:             1.5e-6, // $1.5 per MTok
		InputPricePerTokenPriority:     3e-6,   // $3 per MTok
		OutputPricePerToken:            12e-6,  // $12 per MTok
		OutputPricePerTokenPriority:    24e-6,  // $24 per MTok
		CacheCreationPricePerToken:     1.5e-6, // $1.5 per MTok
		CacheReadPricePerToken:         0.15e-6,
		CacheReadPricePerTokenPriority: 0.3e-6,
		SupportsCacheBreakdown:         false,
	}

	// ---- DeepSeek V4 系列 ----
	// deepseek-chat / deepseek-reasoner 是官方 V4 Flash 兼容别名。
	s.fallbackPrices["deepseek-v4-pro"] = &ModelPricing{
		InputPricePerToken:     4.35e-7,
		OutputPricePerToken:    8.7e-7,
		CacheReadPricePerToken: 3.625e-9,
		SupportsCacheBreakdown: false,
	}
	s.fallbackPrices["deepseek-v4-flash"] = &ModelPricing{
		InputPricePerToken:     1.4e-7,
		OutputPricePerToken:    2.8e-7,
		CacheReadPricePerToken: 2.8e-9,
		SupportsCacheBreakdown: false,
	}

	// ---- 国产 LLM 兜底价格 ----
	// 仅白名单匹配公开可核验 SKU，避免未知国产模型 alias 被宽泛误计价。
	// GLM-5.2 与 GLM-5.1 在 z.ai 上同价。
	s.fallbackPrices["glm-5.2"] = &ModelPricing{InputPricePerToken: 1.4e-6, OutputPricePerToken: 4.4e-6, CacheReadPricePerToken: 0.26e-6, SupportsCacheBreakdown: false}
	s.fallbackPrices["glm-5.1"] = &ModelPricing{InputPricePerToken: 1.4e-6, OutputPricePerToken: 4.4e-6, CacheReadPricePerToken: 0.26e-6, SupportsCacheBreakdown: false}
	s.fallbackPrices["glm-5"] = &ModelPricing{InputPricePerToken: 1e-6, OutputPricePerToken: 3.2e-6, CacheReadPricePerToken: 0.2e-6, SupportsCacheBreakdown: false}
	s.fallbackPrices["glm-5-turbo"] = &ModelPricing{InputPricePerToken: 1.2e-6, OutputPricePerToken: 4e-6, CacheReadPricePerToken: 0.24e-6, SupportsCacheBreakdown: false}
	s.fallbackPrices["glm-4.7"] = &ModelPricing{InputPricePerToken: 0.6e-6, OutputPricePerToken: 2.2e-6, CacheReadPricePerToken: 0.11e-6, SupportsCacheBreakdown: false}
	s.fallbackPrices["glm-4.7-flashx"] = &ModelPricing{InputPricePerToken: 0.07e-6, OutputPricePerToken: 0.4e-6, CacheReadPricePerToken: 0.01e-6, SupportsCacheBreakdown: false}
	s.fallbackPrices["glm-4.6"] = &ModelPricing{InputPricePerToken: 0.6e-6, OutputPricePerToken: 2.2e-6, CacheReadPricePerToken: 0.11e-6, SupportsCacheBreakdown: false}
	s.fallbackPrices["glm-4.5"] = &ModelPricing{InputPricePerToken: 0.6e-6, OutputPricePerToken: 2.2e-6, CacheReadPricePerToken: 0.11e-6, SupportsCacheBreakdown: false}
	s.fallbackPrices["glm-4.5-x"] = &ModelPricing{InputPricePerToken: 2.2e-6, OutputPricePerToken: 8.9e-6, CacheReadPricePerToken: 0.45e-6, SupportsCacheBreakdown: false}
	s.fallbackPrices["glm-4.5-air"] = &ModelPricing{InputPricePerToken: 0.2e-6, OutputPricePerToken: 1.1e-6, CacheReadPricePerToken: 0.03e-6, SupportsCacheBreakdown: false}
	s.fallbackPrices["glm-4.5-airx"] = &ModelPricing{InputPricePerToken: 1.1e-6, OutputPricePerToken: 4.5e-6, CacheReadPricePerToken: 0.22e-6, SupportsCacheBreakdown: false}
	s.fallbackPrices["glm-4-32b-0414-128k"] = &ModelPricing{InputPricePerToken: 0.1e-6, OutputPricePerToken: 0.1e-6, SupportsCacheBreakdown: false}
	s.fallbackPrices["glm-4.5-flash"] = &ModelPricing{SupportsCacheBreakdown: false}
	s.fallbackPrices["glm-4.7-flash"] = &ModelPricing{SupportsCacheBreakdown: false}

	s.fallbackPrices["kimi-k2.6"] = &ModelPricing{InputPricePerToken: 0.95e-6, OutputPricePerToken: 4e-6, CacheReadPricePerToken: 0.15e-6, SupportsCacheBreakdown: false}
	s.fallbackPrices["kimi-for-coding"] = s.fallbackPrices["kimi-k2.6"]
	s.fallbackPrices["kimi-k2.5"] = &ModelPricing{InputPricePerToken: 0.60e-6, OutputPricePerToken: 3e-6, CacheReadPricePerToken: 0.098e-6, SupportsCacheBreakdown: false}
	s.fallbackPrices["kimi-k2-thinking"] = &ModelPricing{InputPricePerToken: 0.56e-6, OutputPricePerToken: 2.24e-6, CacheReadPricePerToken: 0.14e-6, SupportsCacheBreakdown: false}
	s.fallbackPrices["kimi-k2"] = &ModelPricing{InputPricePerToken: 0.56e-6, OutputPricePerToken: 2.24e-6, CacheReadPricePerToken: 0.14e-6, SupportsCacheBreakdown: false}

	s.fallbackPrices["minimax-m3"] = &ModelPricing{InputPricePerToken: 0.60e-6, OutputPricePerToken: 2.40e-6, CacheReadPricePerToken: 0.12e-6, SupportsCacheBreakdown: false}
	s.fallbackPrices["minimax-m2.7"] = &ModelPricing{InputPricePerToken: 0.30e-6, OutputPricePerToken: 1.20e-6, CacheReadPricePerToken: 0.06e-6, SupportsCacheBreakdown: false}
	s.fallbackPrices["minimax-m2.7-highspeed"] = &ModelPricing{InputPricePerToken: 0.60e-6, OutputPricePerToken: 2.40e-6, CacheReadPricePerToken: 0.06e-6, SupportsCacheBreakdown: false}
	s.fallbackPrices["minimax-m2.5"] = &ModelPricing{InputPricePerToken: 0.30e-6, OutputPricePerToken: 1.20e-6, CacheReadPricePerToken: 0.03e-6, SupportsCacheBreakdown: false}
	s.fallbackPrices["minimax-m2.1"] = &ModelPricing{InputPricePerToken: 0.30e-6, OutputPricePerToken: 1.20e-6, CacheReadPricePerToken: 0.03e-6, SupportsCacheBreakdown: false}
	s.fallbackPrices["minimax-m2"] = &ModelPricing{InputPricePerToken: 0.30e-6, OutputPricePerToken: 1.20e-6, CacheReadPricePerToken: 0.03e-6, SupportsCacheBreakdown: false}

	// doubao-embedding-vision 图文向量化存在文本/图片输入双档价格。
	s.fallbackPrices["doubao-embedding-vision"] = &ModelPricing{
		InputPricePerToken:      0.098e-6,
		ImageInputPricePerToken: 0.252e-6,
		OutputPricePerToken:     0,
		SupportsCacheBreakdown:  false,
	}
}

func fallbackPricingKey(model string) string {
	modelLower := strings.ToLower(model)

	// 按模型系列匹配
	if strings.Contains(modelLower, "opus") {
		if strings.Contains(modelLower, "4.7") || strings.Contains(modelLower, "4-7") {
			return "claude-opus-4.7"
		}
		if strings.Contains(modelLower, "4.6") || strings.Contains(modelLower, "4-6") {
			return "claude-opus-4.6"
		}
		if strings.Contains(modelLower, "4.5") || strings.Contains(modelLower, "4-5") {
			return "claude-opus-4.5"
		}
		return "claude-3-opus"
	}
	if strings.Contains(modelLower, "sonnet") {
		if strings.Contains(modelLower, "4") && !strings.Contains(modelLower, "3") {
			return "claude-sonnet-4"
		}
		return "claude-3-5-sonnet"
	}
	if strings.Contains(modelLower, "haiku") {
		if strings.Contains(modelLower, "3-5") || strings.Contains(modelLower, "3.5") {
			return "claude-3-5-haiku"
		}
		return "claude-3-haiku"
	}
	// Claude 未知型号统一回退到 Sonnet，避免计费中断。
	if strings.Contains(modelLower, "claude") {
		return "claude-sonnet-4"
	}
	if strings.Contains(modelLower, "gemini-3.1-pro") || strings.Contains(modelLower, "gemini-3-1-pro") {
		return "gemini-3.1-pro"
	}

	// DeepSeek V4 系列：仅匹配已知 V4 Pro/Flash 与官方兼容别名。
	if strings.Contains(modelLower, "deepseek-v4-flash") {
		return "deepseek-v4-flash"
	}
	if strings.Contains(modelLower, "deepseek-v4-pro") {
		return "deepseek-v4-pro"
	}
	if strings.Contains(modelLower, "deepseek-chat") || strings.Contains(modelLower, "deepseek-reasoner") {
		return "deepseek-v4-flash"
	}

	// 国产 LLM 兜底采用白名单语义：长 key 优先，未知 alias 不回退。
	// 带小数点的型号必须排在裸 "glm-5" 之前，否则会被 strings.Contains 抢走。
	if strings.Contains(modelLower, "glm-5.2") {
		return "glm-5.2"
	}
	if strings.Contains(modelLower, "glm-5.1") {
		return "glm-5.1"
	}
	if strings.Contains(modelLower, "glm-5-turbo") || strings.Contains(modelLower, "glm-5turbo") {
		return "glm-5-turbo"
	}
	if strings.Contains(modelLower, "glm-5") {
		return "glm-5"
	}
	if strings.Contains(modelLower, "glm-4.7-flashx") {
		return "glm-4.7-flashx"
	}
	if strings.Contains(modelLower, "glm-4.7-flash") {
		return "glm-4.7-flash"
	}
	if strings.Contains(modelLower, "glm-4.7") {
		return "glm-4.7"
	}
	if strings.Contains(modelLower, "glm-4.6") {
		return "glm-4.6"
	}
	if strings.Contains(modelLower, "glm-4.5-flash") {
		return "glm-4.5-flash"
	}
	if strings.Contains(modelLower, "glm-4.5-x") || strings.Contains(modelLower, "glm-4.5x") {
		return "glm-4.5-x"
	}
	if strings.Contains(modelLower, "glm-4.5-airx") || strings.Contains(modelLower, "glm-4.5airx") {
		return "glm-4.5-airx"
	}
	if strings.Contains(modelLower, "glm-4.5-air") || strings.Contains(modelLower, "glm-4.5air") {
		return "glm-4.5-air"
	}
	if strings.Contains(modelLower, "glm-4.5") {
		return "glm-4.5"
	}
	if strings.Contains(modelLower, "glm-4-32b") {
		return "glm-4-32b-0414-128k"
	}

	if strings.Contains(modelLower, "kimi-for-coding") {
		return "kimi-for-coding"
	}
	if strings.Contains(modelLower, "kimi-k2.6") || strings.Contains(modelLower, "kimi-k2-6") {
		return "kimi-k2.6"
	}
	if strings.Contains(modelLower, "kimi-k2.5") || strings.Contains(modelLower, "kimi-k2-5") {
		return "kimi-k2.5"
	}
	if strings.Contains(modelLower, "kimi-k2-thinking") {
		return "kimi-k2-thinking"
	}
	if strings.Contains(modelLower, "kimi-k2") || strings.Contains(modelLower, "kimi/k2") {
		return "kimi-k2"
	}

	if strings.Contains(modelLower, "minimax-m3") {
		return "minimax-m3"
	}
	if strings.Contains(modelLower, "minimax-m2.7-highspeed") || strings.Contains(modelLower, "minimax-m2-7-highspeed") {
		return "minimax-m2.7-highspeed"
	}
	if strings.Contains(modelLower, "minimax-m2.7") || strings.Contains(modelLower, "minimax-m2-7") {
		return "minimax-m2.7"
	}
	if strings.Contains(modelLower, "minimax-m2.5") || strings.Contains(modelLower, "minimax-m2-5") {
		return "minimax-m2.5"
	}
	if strings.Contains(modelLower, "minimax-m2.1") || strings.Contains(modelLower, "minimax-m2-1") {
		return "minimax-m2.1"
	}
	if strings.Contains(modelLower, "minimax-m2") || strings.Contains(modelLower, "minimax-m-2") {
		return "minimax-m2"
	}

	if strings.Contains(modelLower, "doubao-embedding-vision") {
		return "doubao-embedding-vision"
	}

	// OpenAI 仅匹配已知 GPT-5/Codex 族，避免未知 OpenAI 型号误计价。
	if normalized := normalizeKnownOpenAICodexModel(modelLower); normalized != "" {
		switch normalized {
		case "gpt-5.6-sol":
			return "gpt-5.6-sol"
		case "gpt-5.6-terra":
			return "gpt-5.6-terra"
		case "gpt-5.6-luna":
			return "gpt-5.6-luna"
		case "gpt-5.5-pro":
			return "gpt-5.5-pro"
		case "gpt-5.5":
			return "gpt-5.5"
		case "gpt-5.4-mini":
			return "gpt-5.4-mini"
		case "gpt-5.4-nano":
			return "gpt-5.4-nano"
		case "gpt-5.4":
			return "gpt-5.4"
		case "gpt-5.2":
			return "gpt-5.2"
		case "gpt-5.3-codex", "gpt-5.3-codex-spark":
			return "gpt-5.3-codex"
		}
	}

	return ""
}

// getFallbackPricing 根据模型系列获取回退价格
func (s *BillingService) getFallbackPricing(model string) *ModelPricing {
	key := fallbackPricingKey(model)
	if key == "" {
		return nil
	}
	return s.fallbackPrices[key]
}

// GetModelPricing 获取模型价格配置
func (s *BillingService) GetModelPricing(model string) (*ModelPricing, error) {
	// 标准化模型名称（转小写）
	model = strings.ToLower(model)

	// 1. 优先从动态价格服务获取
	if s.pricingService != nil {
		litellmPricing := s.pricingService.GetModelPricing(model)
		if litellmPricing != nil && !litellmPricing.tokenBillingUnsupported {
			// 启用 5m/1h 分类计费的条件：
			// 1. 存在 1h 价格
			// 2. 1h 价格 > 5m 价格（防止 LiteLLM 数据错误导致少收费）
			price5m := litellmPricing.CacheCreationInputTokenCost
			price1h := litellmPricing.CacheCreationInputTokenCostAbove1hr
			enableBreakdown := price1h > 0 && price1h > price5m
			return s.applyModelSpecificPricingPolicy(model, &ModelPricing{
				InputPricePerToken:             litellmPricing.InputCostPerToken,
				InputPricePerTokenPriority:     litellmPricing.InputCostPerTokenPriority,
				ImageInputPricePerToken:        litellmPricing.InputCostPerImageToken,
				OutputPricePerToken:            litellmPricing.OutputCostPerToken,
				OutputPricePerTokenPriority:    litellmPricing.OutputCostPerTokenPriority,
				CacheCreationPricePerToken:     litellmPricing.CacheCreationInputTokenCost,
				CacheReadPricePerToken:         litellmPricing.CacheReadInputTokenCost,
				CacheReadPricePerTokenPriority: litellmPricing.CacheReadInputTokenCostPriority,
				CacheCreation5mPrice:           price5m,
				CacheCreation1hPrice:           price1h,
				SupportsCacheBreakdown:         enableBreakdown,
				LongContextInputThreshold:      litellmPricing.LongContextInputTokenThreshold,
				LongContextInputMultiplier:     litellmPricing.LongContextInputCostMultiplier,
				LongContextOutputMultiplier:    litellmPricing.LongContextOutputCostMultiplier,
				ImageOutputPricePerToken:       litellmPricing.OutputCostPerImageToken,
			}), nil
		}
	}

	// 2. 使用硬编码回退价格
	fallbackKey := fallbackPricingKey(model)
	fallback := s.fallbackPrices[fallbackKey]
	if fallback != nil {
		if _, seen := s.fallbackWarnSeen.LoadOrStore(fallbackKey, struct{}{}); !seen {
			log.Printf("[Billing] Using fallback pricing for model: %s (fallback: %s)", model, fallbackKey)
		}
		// fallbackPrices is shared across requests; callers may apply channel
		// overrides to the returned value.
		pricing := *fallback
		return s.applyModelSpecificPricingPolicy(model, &pricing), nil
	}

	return nil, fmt.Errorf("%w for model: %s", ErrModelPricingUnavailable, model)
}

// GetModelPricingWithChannel 获取模型定价，渠道配置的价格覆盖默认值
// 渠道存在时，未配置的图片输出价格归零（不回退到 LiteLLM）
func (s *BillingService) GetModelPricingWithChannel(model string, channelPricing *ChannelModelPricing) (*ModelPricing, error) {
	pricing, err := s.GetModelPricing(model)
	if err != nil {
		return nil, err
	}
	if channelPricing == nil {
		return pricing, nil
	}
	if channelPricing.InputPrice != nil {
		pricing.InputPricePerToken = *channelPricing.InputPrice
		pricing.InputPricePerTokenPriority = *channelPricing.InputPrice
	}
	if channelPricing.OutputPrice != nil {
		pricing.OutputPricePerToken = *channelPricing.OutputPrice
		pricing.OutputPricePerTokenPriority = *channelPricing.OutputPrice
	}
	if channelPricing.CacheWritePrice != nil {
		pricing.CacheCreationPricePerToken = *channelPricing.CacheWritePrice
		pricing.CacheCreation5mPrice = *channelPricing.CacheWritePrice
		pricing.CacheCreation1hPrice = *channelPricing.CacheWritePrice
	}
	if channelPricing.CacheReadPrice != nil {
		pricing.CacheReadPricePerToken = *channelPricing.CacheReadPrice
		pricing.CacheReadPricePerTokenPriority = *channelPricing.CacheReadPrice
	}
	if channelPricing.ImageOutputPrice != nil {
		pricing.ImageOutputPricePerToken = *channelPricing.ImageOutputPrice
	} else {
		pricing.ImageOutputPricePerToken = 0
	}
	pricing.ImageOutputPriceExplicit = true
	return pricing, nil
}

// --- 统一计费入口 ---

// CostInput 统一计费输入
type CostInput struct {
	Ctx                             context.Context
	Model                           string
	GroupID                         *int64 // 用于渠道定价查找
	Tokens                          UsageTokens
	RequestCount                    int    // 按次计费时使用
	SizeTier                        string // 按次/图片模式的层级标签（"1K","2K","4K","HD" 等）
	RateMultiplier                  float64
	ServiceTier                     string                // "priority","flex","" 等
	Resolver                        *ModelPricingResolver // 定价解析器
	Resolved                        *ResolvedPricing      // 可选：预解析的定价结果（避免重复 Resolve 调用）
	OpenAILongContextBillingEnabled bool                  // GPT-5.6 超 272k 长上下文倍率，由 OpenAI 分组显式开启
}

// CalculateCostUnified 统一计费入口，支持三种计费模式。
// 使用 ModelPricingResolver 解析定价，然后根据 BillingMode 分发计算。
func (s *BillingService) CalculateCostUnified(input CostInput) (*CostBreakdown, error) {
	if err := validateUsageTokensForBilling(input.Tokens); err != nil {
		return nil, err
	}
	if input.Resolver == nil {
		// 无 Resolver 时仍需保留调用方传入的 GPT-5.6 分组计费策略。
		return s.calculateCostInternalWithPolicy(
			input.Model,
			input.Tokens,
			input.RateMultiplier,
			input.ServiceTier,
			nil,
			input.OpenAILongContextBillingEnabled,
		)
	}

	// 优先使用预解析结果，避免重复 Resolve 调用
	resolved := input.Resolved
	if resolved == nil {
		resolved = input.Resolver.Resolve(input.Ctx, PricingInput{
			Model:   input.Model,
			GroupID: input.GroupID,
		})
	}

	// 保存时强制 > 0；若仍有负数泄漏（缓存/迁移残留），按 0 处理避免按 1x 误扣。
	if input.RateMultiplier < 0 {
		input.RateMultiplier = 0
	}

	var breakdown *CostBreakdown
	var err error
	switch resolved.Mode {
	case BillingModePerRequest, BillingModeImage:
		if err := validateRequestCountForBilling(input.RequestCount); err != nil {
			return nil, err
		}
		breakdown, err = s.calculatePerRequestCost(resolved, input)
	default: // BillingModeToken
		breakdown, err = s.calculateTokenCost(resolved, input)
	}
	if err == nil && breakdown != nil {
		breakdown.BillingMode = string(resolved.Mode)
		if breakdown.BillingMode == "" {
			breakdown.BillingMode = string(BillingModeToken)
		}
		err = validateCostBreakdownForBilling(breakdown)
	}
	return breakdown, err
}

func validateRequestCountForBilling(requestCount int) error {
	if requestCount < 0 {
		return fmt.Errorf("invalid usage for billing: request_count is negative: %d", requestCount)
	}
	return nil
}

func validateUsageTokensForBilling(tokens UsageTokens) error {
	checks := [...]struct {
		name  string
		value int
	}{
		{"input_tokens", tokens.InputTokens}, {"image_input_tokens", tokens.ImageInputTokens},
		{"output_tokens", tokens.OutputTokens}, {"image_output_tokens", tokens.ImageOutputTokens},
		{"cache_read_tokens", tokens.CacheReadTokens}, {"cache_creation_tokens", tokens.CacheCreationTokens},
		{"cache_creation_5m_tokens", tokens.CacheCreation5mTokens}, {"cache_creation_1h_tokens", tokens.CacheCreation1hTokens},
	}
	for _, check := range checks {
		if check.value < 0 {
			return fmt.Errorf("invalid usage for billing: %s is negative: %d", check.name, check.value)
		}
	}
	return nil
}

func validateImageCountForBilling(imageCount int) error {
	if imageCount < 0 {
		return fmt.Errorf("invalid usage for billing: image_count is negative: %d", imageCount)
	}
	return nil
}

func checkedBillingTokenSum(values ...int) (int, error) {
	total := 0
	for _, value := range values {
		if value < 0 || total > int(^uint(0)>>1)-value {
			return 0, fmt.Errorf("invalid usage for billing: token total overflows int")
		}
		total += value
	}
	return total, nil
}

func validateCostBreakdownForBilling(cost *CostBreakdown) error {
	if cost == nil {
		return nil
	}
	values := [...]struct {
		name  string
		value float64
	}{
		{"input_cost", cost.InputCost}, {"output_cost", cost.OutputCost}, {"image_output_cost", cost.ImageOutputCost},
		{"cache_creation_cost", cost.CacheCreationCost}, {"cache_read_cost", cost.CacheReadCost},
		{"total_cost", cost.TotalCost}, {"actual_cost", cost.ActualCost},
	}
	for _, item := range values {
		if math.IsNaN(item.value) || math.IsInf(item.value, 0) || item.value < 0 {
			return fmt.Errorf("invalid billing cost: %s must be finite and nonnegative: %v", item.name, item.value)
		}
	}
	return nil
}

// calculateTokenCost 按 token 区间计费
func (s *BillingService) calculateTokenCost(resolved *ResolvedPricing, input CostInput) (*CostBreakdown, error) {
	totalContext, err := checkedBillingTokenSum(input.Tokens.InputTokens, input.Tokens.CacheCreationTokens, input.Tokens.CacheReadTokens)
	if err != nil {
		return nil, err
	}

	pricing := input.Resolver.GetIntervalPricing(resolved, totalContext)
	if pricing == nil {
		return nil, fmt.Errorf("no pricing available for model: %s: %w", input.Model, ErrModelPricingUnavailable)
	}

	pricing = s.applyModelSpecificPricingPolicy(input.Model, pricing)

	// 区间定价已包含上下文分层；GPT-5.6 的整次会话倍率还需由 OpenAI 分组显式开启。
	applyLongCtx := len(resolved.Intervals) == 0
	if isOpenAIGPT56Model(input.Model) {
		applyLongCtx = applyLongCtx && input.OpenAILongContextBillingEnabled
	}

	return s.computeTokenBreakdown(pricing, input.Tokens, input.RateMultiplier, input.ServiceTier, applyLongCtx)
}

// computeTokenBreakdown 是 token 计费的核心逻辑，由 calculateTokenCost 和 calculateCostInternal 共用。
// applyLongCtx 控制是否检查长上下文定价（区间定价已自含上下文分层，不需要额外应用）。
func (s *BillingService) computeTokenBreakdown(
	pricing *ModelPricing, tokens UsageTokens,
	rateMultiplier float64, serviceTier string,
	applyLongCtx bool,
) (*CostBreakdown, error) {
	// 保存时强制 > 0；若仍有负数泄漏，按 0 处理避免按 1x 误扣。
	if rateMultiplier < 0 {
		rateMultiplier = 0
	}

	inputPrice := pricing.InputPricePerToken
	outputPrice := pricing.OutputPricePerToken
	cacheReadPrice := pricing.CacheReadPricePerToken
	cacheCreationPrice := pricing.CacheCreationPricePerToken
	cacheCreationMultiplier := 1.0
	tierMultiplier := 1.0

	if usePriorityServiceTierPricing(serviceTier, pricing) {
		if pricing.InputPricePerTokenPriority > 0 {
			inputPrice = pricing.InputPricePerTokenPriority
		}
		if pricing.OutputPricePerTokenPriority > 0 {
			outputPrice = pricing.OutputPricePerTokenPriority
		}
		if pricing.CacheReadPricePerTokenPriority > 0 {
			cacheReadPrice = pricing.CacheReadPricePerTokenPriority
		}
		if pricing.CacheCreationPricePerTokenPriority > 0 {
			cacheCreationPrice = pricing.CacheCreationPricePerTokenPriority
		}
	} else {
		tierMultiplier = serviceTierCostMultiplier(serviceTier)
	}

	imageInputPrice := pricing.ImageInputPricePerToken
	applySessionLongContext := false
	if applyLongCtx {
		var err error
		applySessionLongContext, err = s.shouldApplySessionLongContextPricing(tokens, pricing)
		if err != nil {
			return nil, err
		}
	}
	if applySessionLongContext {
		inputPrice *= pricing.LongContextInputMultiplier
		if imageInputPrice > 0 {
			imageInputPrice *= pricing.LongContextInputMultiplier
		}
		outputPrice *= pricing.LongContextOutputMultiplier
		// 缓存读取本质上是输入侧的复用，应与 input 一同应用长上下文倍率；
		// 否则 cache hit 越多，少计的费用越多（见 #2293）。
		cacheReadPrice *= pricing.LongContextInputMultiplier
		// 缓存创建（cache_write）也是输入侧操作，三档价格（标准 / 5m / 1h）
		// 都通过 computeCacheCreationCost 直接读取 pricing.*，不会经过这里
		// 的倍率修改，因此显式向下传一个倍率，避免长上下文场景下被漏乘。
		cacheCreationMultiplier = pricing.LongContextInputMultiplier
	}

	bd := &CostBreakdown{}
	bd.InputCost = computeInputCostWithImageTokens(tokens, inputPrice, imageInputPrice)

	// 分离图片输出 token 与文本输出 token
	textOutputTokens := tokens.OutputTokens - tokens.ImageOutputTokens
	if textOutputTokens < 0 {
		textOutputTokens = 0
	}
	bd.OutputCost = float64(textOutputTokens) * outputPrice

	// 图片输出 token 费用（独立费率）
	if tokens.ImageOutputTokens > 0 {
		imgPrice := pricing.ImageOutputPricePerToken
		if imgPrice == 0 && !pricing.ImageOutputPriceExplicit {
			imgPrice = outputPrice
		}
		bd.ImageOutputCost = float64(tokens.ImageOutputTokens) * imgPrice
	}

	// 缓存创建费用
	bd.CacheCreationCost = s.computeCacheCreationCost(pricing, tokens, cacheCreationPrice, cacheCreationMultiplier)

	bd.CacheReadCost = float64(tokens.CacheReadTokens) * cacheReadPrice

	if tierMultiplier != 1.0 {
		bd.InputCost *= tierMultiplier
		bd.OutputCost *= tierMultiplier
		bd.ImageOutputCost *= tierMultiplier
		bd.CacheCreationCost *= tierMultiplier
		bd.CacheReadCost *= tierMultiplier
	}

	bd.TotalCost = bd.InputCost + bd.OutputCost + bd.ImageOutputCost +
		bd.CacheCreationCost + bd.CacheReadCost
	bd.ActualCost = bd.TotalCost * rateMultiplier

	return bd, nil
}

func computeInputCostWithImageTokens(tokens UsageTokens, inputPrice, imageInputPrice float64) float64 {
	if tokens.ImageInputTokens <= 0 {
		return float64(tokens.InputTokens) * inputPrice
	}
	imageInputTokens := tokens.ImageInputTokens
	textInputTokens := tokens.InputTokens - imageInputTokens
	if textInputTokens < 0 {
		textInputTokens = 0
		imageInputTokens = tokens.InputTokens
	}
	if imageInputPrice == 0 {
		imageInputPrice = inputPrice
	}
	return float64(textInputTokens)*inputPrice + float64(imageInputTokens)*imageInputPrice
}

// computeCacheCreationCost 计算缓存创建费用（支持 5m/1h 分类或标准计费）。
// multiplier 用于长上下文等场景下的整体价格缩放（普通调用传 1.0 即可）。
func (s *BillingService) computeCacheCreationCost(pricing *ModelPricing, tokens UsageTokens, price, multiplier float64) float64 {
	if pricing.SupportsCacheBreakdown && (pricing.CacheCreation5mPrice > 0 || pricing.CacheCreation1hPrice > 0) {
		if tokens.CacheCreation5mTokens == 0 && tokens.CacheCreation1hTokens == 0 && tokens.CacheCreationTokens > 0 {
			// API 未返回 ephemeral 明细，回退到全部按 5m 单价计费
			return float64(tokens.CacheCreationTokens) * pricing.CacheCreation5mPrice * multiplier
		}
		return float64(tokens.CacheCreation5mTokens)*pricing.CacheCreation5mPrice*multiplier +
			float64(tokens.CacheCreation1hTokens)*pricing.CacheCreation1hPrice*multiplier
	}
	return float64(tokens.CacheCreationTokens) * price * multiplier
}

// calculatePerRequestCost 按次/图片计费
func (s *BillingService) calculatePerRequestCost(resolved *ResolvedPricing, input CostInput) (*CostBreakdown, error) {
	count := input.RequestCount
	if count <= 0 {
		count = 1
	}

	var unitPrice float64

	if input.SizeTier != "" {
		unitPrice = input.Resolver.GetRequestTierPrice(resolved, input.SizeTier)
	}

	if unitPrice == 0 {
		totalContext, err := checkedBillingTokenSum(input.Tokens.InputTokens, input.Tokens.CacheCreationTokens, input.Tokens.CacheReadTokens)
		if err != nil {
			return nil, err
		}
		unitPrice = input.Resolver.GetRequestTierPriceByContext(resolved, totalContext)
	}

	// 回退到默认按次价格
	if unitPrice == 0 {
		unitPrice = resolved.DefaultPerRequestPrice
	}

	totalCost := unitPrice * float64(count)
	actualCost := totalCost * input.RateMultiplier

	return &CostBreakdown{
		TotalCost:  totalCost,
		ActualCost: actualCost,
	}, nil
}

// CalculateCost 计算使用费用
func (s *BillingService) CalculateCost(model string, tokens UsageTokens, rateMultiplier float64) (*CostBreakdown, error) {
	return s.calculateCostInternal(model, tokens, rateMultiplier, "", nil)
}

func (s *BillingService) CalculateCostWithServiceTier(model string, tokens UsageTokens, rateMultiplier float64, serviceTier string) (*CostBreakdown, error) {
	return s.calculateCostInternal(model, tokens, rateMultiplier, serviceTier, nil)
}

func (s *BillingService) calculateCostInternal(model string, tokens UsageTokens, rateMultiplier float64, serviceTier string, channelPricing *ChannelModelPricing) (*CostBreakdown, error) {
	return s.calculateCostInternalWithPolicy(model, tokens, rateMultiplier, serviceTier, channelPricing, true)
}

func (s *BillingService) calculateCostInternalWithPolicy(model string, tokens UsageTokens, rateMultiplier float64, serviceTier string, channelPricing *ChannelModelPricing, openAILongContextBillingEnabled bool) (*CostBreakdown, error) {
	if err := validateUsageTokensForBilling(tokens); err != nil {
		return nil, err
	}
	var pricing *ModelPricing
	var err error
	if channelPricing != nil {
		pricing, err = s.GetModelPricingWithChannel(model, channelPricing)
	} else {
		pricing, err = s.GetModelPricing(model)
	}
	if err != nil {
		return nil, err
	}

	// 旧路径没有区间定价；GPT-5.6 仍须遵循调用方传入的分组策略。
	applyLongContext := true
	if isOpenAIGPT56Model(model) {
		applyLongContext = openAILongContextBillingEnabled
	}
	breakdown, err := s.computeTokenBreakdown(pricing, tokens, rateMultiplier, serviceTier, applyLongContext)
	if err != nil {
		return nil, err
	}
	if err := validateCostBreakdownForBilling(breakdown); err != nil {
		return nil, err
	}
	return breakdown, nil
}

func (s *BillingService) applyModelSpecificPricingPolicy(model string, pricing *ModelPricing) *ModelPricing {
	if pricing == nil {
		return nil
	}
	normalized := normalizeKnownOpenAICodexModel(model)
	isGPT56 := isOpenAIGPT56Model(normalized)
	usesLegacy := normalized == "gpt-5.4" || normalized == "gpt-5.5" || normalized == "gpt-5.5-pro"
	if !isGPT56 && !usesLegacy {
		return pricing
	}
	needsLong := pricing.LongContextInputThreshold <= 0 || pricing.LongContextInputMultiplier <= 0 || pricing.LongContextOutputMultiplier <= 0
	needsCache := isGPT56 && !pricing.CacheCreationPriceExplicit && (pricing.CacheCreationPricePerToken <= 0 || (pricing.InputPricePerTokenPriority > 0 && pricing.CacheCreationPricePerTokenPriority <= 0))
	if !needsLong && !needsCache {
		return pricing
	}
	cloned := *pricing
	if needsCache {
		if cloned.CacheCreationPricePerToken <= 0 {
			cloned.CacheCreationPricePerToken = cloned.InputPricePerToken * 1.25
		}
		if cloned.CacheCreationPricePerTokenPriority <= 0 {
			cloned.CacheCreationPricePerTokenPriority = cloned.InputPricePerTokenPriority * 1.25
		}
	}
	if cloned.LongContextInputThreshold <= 0 {
		cloned.LongContextInputThreshold = openAIGPT54LongContextInputThreshold
	}
	if cloned.LongContextInputMultiplier <= 0 {
		cloned.LongContextInputMultiplier = openAIGPT54LongContextInputMultiplier
	}
	if cloned.LongContextOutputMultiplier <= 0 {
		cloned.LongContextOutputMultiplier = openAIGPT54LongContextOutputMultiplier
	}
	return &cloned
}

func (s *BillingService) shouldApplySessionLongContextPricing(tokens UsageTokens, pricing *ModelPricing) (bool, error) {
	if pricing == nil || pricing.LongContextInputThreshold <= 0 {
		return false, nil
	}
	if pricing.LongContextInputMultiplier <= 1 && pricing.LongContextOutputMultiplier <= 1 {
		return false, nil
	}
	total, err := checkedBillingTokenSum(tokens.InputTokens, tokens.CacheCreationTokens, tokens.CacheReadTokens)
	if err != nil {
		return false, err
	}
	return total > pricing.LongContextInputThreshold, nil
}

// CalculateCostWithConfig 使用配置中的默认倍率计算费用
func (s *BillingService) CalculateCostWithConfig(model string, tokens UsageTokens) (*CostBreakdown, error) {
	multiplier := s.cfg.Default.RateMultiplier
	if multiplier <= 0 {
		multiplier = 1.0
	}
	return s.CalculateCost(model, tokens, multiplier)
}

// CalculateCostWithLongContext 计算费用，支持长上下文双倍计费
// threshold: 阈值（如 200000），超过此值的部分按 extraMultiplier 倍计费
// extraMultiplier: 超出部分的倍率（如 2.0 表示双倍）
//
// 示例：缓存 210k + 输入 10k = 220k，阈值 200k，倍率 2.0
// 拆分为：范围内 (200k, 0) + 范围外 (10k, 10k)
// 范围内正常计费，范围外 × 2 计费
func (s *BillingService) CalculateCostWithLongContext(model string, tokens UsageTokens, rateMultiplier float64, threshold int, extraMultiplier float64) (*CostBreakdown, error) {
	if err := validateUsageTokensForBilling(tokens); err != nil {
		return nil, err
	}
	// 未启用长上下文计费，直接走正常计费
	if threshold <= 0 || extraMultiplier <= 1 {
		return s.CalculateCost(model, tokens, rateMultiplier)
	}

	// 计算总输入 token（缓存读取 + 新输入）
	total, err := checkedBillingTokenSum(tokens.CacheReadTokens, tokens.InputTokens)
	if err != nil {
		return nil, err
	}
	if total <= threshold {
		return s.CalculateCost(model, tokens, rateMultiplier)
	}

	// 拆分成范围内和范围外
	var inRangeCacheTokens, inRangeInputTokens int
	var outRangeCacheTokens, outRangeInputTokens int

	if tokens.CacheReadTokens >= threshold {
		// 缓存已超过阈值：范围内只有缓存，范围外是超出的缓存+全部输入
		inRangeCacheTokens = threshold
		inRangeInputTokens = 0
		outRangeCacheTokens = tokens.CacheReadTokens - threshold
		outRangeInputTokens = tokens.InputTokens
	} else {
		// 缓存未超过阈值：范围内是全部缓存+部分输入，范围外是剩余输入
		inRangeCacheTokens = tokens.CacheReadTokens
		inRangeInputTokens = threshold - tokens.CacheReadTokens
		outRangeCacheTokens = 0
		outRangeInputTokens = tokens.InputTokens - inRangeInputTokens
	}

	// 范围内部分：正常计费
	inRangeTokens := UsageTokens{
		InputTokens:           inRangeInputTokens,
		OutputTokens:          tokens.OutputTokens, // 输出只算一次
		CacheCreationTokens:   tokens.CacheCreationTokens,
		CacheReadTokens:       inRangeCacheTokens,
		CacheCreation5mTokens: tokens.CacheCreation5mTokens,
		CacheCreation1hTokens: tokens.CacheCreation1hTokens,
		ImageOutputTokens:     tokens.ImageOutputTokens,
	}
	inRangeCost, err := s.CalculateCost(model, inRangeTokens, rateMultiplier)
	if err != nil {
		return nil, err
	}

	// 范围外部分：× extraMultiplier 计费
	outRangeTokens := UsageTokens{
		InputTokens:     outRangeInputTokens,
		CacheReadTokens: outRangeCacheTokens,
	}
	outRangeCost, err := s.CalculateCost(model, outRangeTokens, rateMultiplier*extraMultiplier)
	if err != nil {
		return inRangeCost, fmt.Errorf("out-range cost: %w", err)
	}

	// 合并成本
	combined := &CostBreakdown{
		InputCost:         inRangeCost.InputCost + outRangeCost.InputCost,
		OutputCost:        inRangeCost.OutputCost,
		ImageOutputCost:   inRangeCost.ImageOutputCost,
		CacheCreationCost: inRangeCost.CacheCreationCost,
		CacheReadCost:     inRangeCost.CacheReadCost + outRangeCost.CacheReadCost,
		TotalCost:         inRangeCost.TotalCost + outRangeCost.TotalCost,
		ActualCost:        inRangeCost.ActualCost + outRangeCost.ActualCost,
	}
	if err := validateCostBreakdownForBilling(combined); err != nil {
		return nil, err
	}
	return combined, nil
}

// ListSupportedModels 列出所有支持的模型（现在总是返回true，因为有模糊匹配）
func (s *BillingService) ListSupportedModels() []string {
	models := make([]string, 0)
	// 返回回退价格支持的模型系列
	for model := range s.fallbackPrices {
		models = append(models, model)
	}
	return models
}

// IsModelSupported 检查模型是否支持（现在总是返回true，因为有模糊匹配回退）
func (s *BillingService) IsModelSupported(model string) bool {
	// 所有Claude模型都有回退价格支持
	modelLower := strings.ToLower(model)
	return strings.Contains(modelLower, "claude") ||
		strings.Contains(modelLower, "opus") ||
		strings.Contains(modelLower, "sonnet") ||
		strings.Contains(modelLower, "haiku")
}

// GetEstimatedCost 估算费用（用于前端展示）
func (s *BillingService) GetEstimatedCost(model string, estimatedInputTokens, estimatedOutputTokens int) (float64, error) {
	tokens := UsageTokens{
		InputTokens:  estimatedInputTokens,
		OutputTokens: estimatedOutputTokens,
	}

	breakdown, err := s.CalculateCostWithConfig(model, tokens)
	if err != nil {
		return 0, err
	}

	return breakdown.ActualCost, nil
}

// GetPricingServiceStatus 获取价格服务状态
func (s *BillingService) GetPricingServiceStatus() map[string]any {
	if s.pricingService != nil {
		return s.pricingService.GetStatus()
	}
	return map[string]any{
		"model_count":  len(s.fallbackPrices),
		"last_updated": "using fallback",
		"local_hash":   "N/A",
	}
}

// ForceUpdatePricing 强制更新价格数据
func (s *BillingService) ForceUpdatePricing() error {
	if s.pricingService != nil {
		return s.pricingService.ForceUpdate()
	}
	return fmt.Errorf("pricing service not initialized")
}

// ImagePriceConfig 图片计费配置
type ImagePriceConfig struct {
	Price1K *float64 // 1K 尺寸价格（nil 表示使用默认值）
	Price2K *float64 // 2K 尺寸价格（nil 表示使用默认值）
	Price4K *float64 // 4K 尺寸价格（nil 表示使用默认值）
}

// Codex alpha/search 网页搜索单次默认价：OpenAI 官方 web search 定价 $10/1000 次。
const defaultWebSearchPricePerCall = 0.01

// CalculateWebSearchCost 计算 Codex alpha/search 网页搜索按次费用。
// callCount: 搜索调用次数（每次请求为 1）
// groupPrice: 分组配置的单次价格（nil 表示使用默认价 0.01；0 表示免费）
// rateMultiplier: 分组费率倍数
func (s *BillingService) CalculateWebSearchCost(callCount int, groupPrice *float64, rateMultiplier float64) *CostBreakdown {
	if callCount <= 0 {
		return &CostBreakdown{}
	}
	unitPrice := defaultWebSearchPricePerCall
	if groupPrice != nil && *groupPrice >= 0 {
		unitPrice = *groupPrice
	}
	totalCost := unitPrice * float64(callCount)

	// 应用倍率（保存时强制 > 0；负数按 0 处理避免按 1x 误扣）
	if rateMultiplier < 0 {
		rateMultiplier = 0
	}
	return &CostBreakdown{
		TotalCost:   totalCost,
		ActualCost:  totalCost * rateMultiplier,
		BillingMode: string(BillingModePerRequest),
	}
}

// webSearchPricePerCallFromAPIKey 从 APIKey 已缓存的 Group 读取按次价格，
// 避免在热路径上产生 DB 查询。
func webSearchPricePerCallFromAPIKey(apiKey *APIKey) *float64 {
	if apiKey == nil || apiKey.Group == nil {
		return nil
	}
	return apiKey.Group.WebSearchPricePerCall
}

// CalculateImageCost 计算图片生成费用
// model: 请求的模型名称（用于获取 LiteLLM 默认价格）
// imageSize: 图片尺寸 "1K", "2K", "4K"
// imageCount: 生成的图片数量
// groupConfig: 分组配置的价格（可能为 nil，表示使用默认值）
// rateMultiplier: 费率倍数
func (s *BillingService) CalculateImageCost(model string, imageSize string, imageCount int, groupConfig *ImagePriceConfig, rateMultiplier float64) *CostBreakdown {
	if imageCount <= 0 {
		return &CostBreakdown{}
	}
	imageSize = NormalizeImageBillingTierOrDefault(imageSize)

	// 获取单价
	unitPrice := s.getImageUnitPrice(model, imageSize, groupConfig)

	// 计算总费用
	totalCost := unitPrice * float64(imageCount)

	// 应用倍率（保存时强制 > 0；负数按 0 处理避免按 1x 误扣）
	if rateMultiplier < 0 {
		rateMultiplier = 0
	}
	actualCost := totalCost * rateMultiplier

	return &CostBreakdown{
		TotalCost:   totalCost,
		ActualCost:  actualCost,
		BillingMode: string(BillingModeImage),
	}
}

// getImageUnitPrice 获取图片单价
func (s *BillingService) getImageUnitPrice(model string, imageSize string, groupConfig *ImagePriceConfig) float64 {
	// 优先使用分组配置的价格
	if groupConfig != nil {
		switch imageSize {
		case "1K":
			if groupConfig.Price1K != nil {
				return *groupConfig.Price1K
			}
		case "2K":
			if groupConfig.Price2K != nil {
				return *groupConfig.Price2K
			}
		case "4K":
			if groupConfig.Price4K != nil {
				return *groupConfig.Price4K
			}
		}
	}

	// 回退到 LiteLLM 默认价格
	return s.getDefaultImagePrice(model, imageSize)
}

// getDefaultImagePrice 获取 LiteLLM 默认图片价格
func (s *BillingService) getDefaultImagePrice(model string, imageSize string) float64 {
	basePrice := 0.0

	// 从 PricingService 获取 output_cost_per_image
	if s.pricingService != nil {
		pricing := s.pricingService.GetModelPricing(model)
		if pricing != nil && pricing.OutputCostPerImage > 0 {
			basePrice = pricing.OutputCostPerImage
		}
	}

	// 如果没有找到价格，使用硬编码默认值（$0.134，来自 gemini-3-pro-image-preview）
	if basePrice <= 0 {
		basePrice = 0.134
	}

	// 2K 尺寸 1.5 倍，4K 尺寸翻倍
	if imageSize == "2K" {
		return basePrice * 1.5
	}
	if imageSize == "4K" {
		return basePrice * 2
	}

	return basePrice
}
