package service

import (
	"context"
	"errors"
	"strings"
	"time"
)

var (
	ErrBillingOutboxAttemptIDRequired       = errors.New("billing outbox attempt_id is required")
	ErrBillingOutboxAPIKeyRequired          = errors.New("billing outbox api_key_id is required")
	ErrBillingOutboxFingerprintConflict     = errors.New("billing outbox request fingerprint conflict")
	ErrBillingOutboxClaimLost               = errors.New("billing outbox claim is no longer owned")
	ErrBillingOutboxFinalizationUnsupported = errors.New("billing outbox durable finalization is unsupported")
)

const BillingOutboxLastErrorLimit = 2048

// BillingOutboxPostEffects contains the immutable, non-secret inputs needed
// to restore enforcement caches and notifications after an applied command.
type BillingOutboxPostEffects struct {
	UserID                         int64              `json:"user_id"`
	UserUsername                   string             `json:"user_username,omitempty"`
	UserEmail                      string             `json:"user_email,omitempty"`
	UserBalance                    float64            `json:"user_balance"`
	UserTotalRecharged             float64            `json:"user_total_recharged"`
	BalanceNotifyEnabled           bool               `json:"balance_notify_enabled"`
	BalanceNotifyThreshold         *float64           `json:"balance_notify_threshold,omitempty"`
	BalanceNotifyThresholdType     string             `json:"balance_notify_threshold_type,omitempty"`
	BalanceNotifyExtraEmails       []NotifyEmailEntry `json:"balance_notify_extra_emails,omitempty"`
	APIKeyGroupID                  *int64             `json:"api_key_group_id,omitempty"`
	AccountID                      int64              `json:"account_id"`
	AccountName                    string             `json:"account_name,omitempty"`
	AccountPlatform                string             `json:"account_platform,omitempty"`
	AccountType                    string             `json:"account_type"`
	QuotaNotifyDailyEnabled        bool               `json:"quota_notify_daily_enabled"`
	QuotaNotifyDailyThreshold      float64            `json:"quota_notify_daily_threshold"`
	QuotaNotifyDailyThresholdType  string             `json:"quota_notify_daily_threshold_type,omitempty"`
	QuotaNotifyWeeklyEnabled       bool               `json:"quota_notify_weekly_enabled"`
	QuotaNotifyWeeklyThreshold     float64            `json:"quota_notify_weekly_threshold"`
	QuotaNotifyWeeklyThresholdType string             `json:"quota_notify_weekly_threshold_type,omitempty"`
	QuotaNotifyTotalEnabled        bool               `json:"quota_notify_total_enabled"`
	QuotaNotifyTotalThreshold      float64            `json:"quota_notify_total_threshold"`
	QuotaNotifyTotalThresholdType  string             `json:"quota_notify_total_threshold_type,omitempty"`
	ActualCost                     float64            `json:"actual_cost"`
	TotalCost                      float64            `json:"total_cost"`
	IsSubscriptionBill             bool               `json:"is_subscription_bill"`
	AccountRateMultiplier          float64            `json:"account_rate_multiplier"`
	Platform                       string             `json:"platform,omitempty"`
	PreserveAccountHealth          bool               `json:"preserve_account_health"`
}

// BillingOutboxCommand is the immutable command envelope persisted before a
// usage billing attempt is handed to asynchronous replay.
type BillingOutboxCommand struct {
	AttemptID          string                    `json:"attempt_id"`
	RequestID          string                    `json:"request_id"`
	APIKeyID           int64                     `json:"api_key_id"`
	RequestFingerprint string                    `json:"request_fingerprint"`
	Billing            UsageBillingCommand       `json:"billing"`
	PostEffects        *BillingOutboxPostEffects `json:"post_effects,omitempty"`
}

func (c *BillingOutboxCommand) Normalize() {
	if c == nil {
		return
	}
	c.AttemptID = strings.TrimSpace(c.AttemptID)
	c.RequestID = strings.TrimSpace(c.RequestID)
	c.RequestFingerprint = strings.TrimSpace(c.RequestFingerprint)
	c.Billing.Normalize()
	if c.RequestID == "" {
		c.RequestID = c.Billing.RequestID
	}
	if c.APIKeyID == 0 {
		c.APIKeyID = c.Billing.APIKeyID
	}
	if c.RequestFingerprint == "" {
		c.RequestFingerprint = c.Billing.RequestFingerprint
	}
	c.Billing.RequestID = c.RequestID
	c.Billing.APIKeyID = c.APIKeyID
	c.Billing.RequestFingerprint = c.RequestFingerprint
}

func (c *BillingOutboxCommand) Validate() error {
	if c == nil || strings.TrimSpace(c.AttemptID) == "" {
		return ErrBillingOutboxAttemptIDRequired
	}
	if c.APIKeyID <= 0 {
		return ErrBillingOutboxAPIKeyRequired
	}
	c.Normalize()
	if c.RequestFingerprint == "" {
		return ErrUsageBillingRequestIDRequired
	}
	return nil
}

type BillingOutboxRecord struct {
	ID          int64
	Command     BillingOutboxCommand
	ApplyResult *UsageBillingApplyResult
	Status      string
	Attempts    int
	AvailableAt time.Time
	LeaseUntil  *time.Time
	LeasedBy    string
	LastError   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type BillingOutboxStats struct {
	Pending         int64
	Processing      int64
	Terminal        int64
	MaxAttempts     int
	OldestCreatedAt *time.Time
	LastError       string
}

type BillingOutboxRepository interface {
	Enqueue(ctx context.Context, command *BillingOutboxCommand) (*BillingOutboxRecord, error)
	// Claim 认领到期可执行的 pending 命令；租约过期的 processing 记录由
	// ClaimExpiredLeased 独立认领。两条语句必须分开：OR 双分支条件无法匹配
	// 部分索引 idx_billing_attempt_outbox_claim (status, available_at, id)，
	// 会迫使 planner 回退 pkey 全表扫描。
	Claim(ctx context.Context, workerID string, limit int, lease time.Duration) ([]BillingOutboxRecord, error)
	ClaimExpiredLeased(ctx context.Context, workerID string, limit int, lease time.Duration) ([]BillingOutboxRecord, error)
	Retry(ctx context.Context, id int64, workerID string, availableAt time.Time, lastError string, terminal bool) error
	Ack(ctx context.Context, id int64, workerID string) error
	Stats(ctx context.Context) (BillingOutboxStats, error)
	// CleanupTerminal 批量删除超过保留期的终态行（succeeded/terminal）。
	// 保留期内的行仍用于对账与排障；删除不影响计费幂等（由 usage_billing_dedup 兜底）。
	CleanupTerminal(ctx context.Context, cutoff time.Time, limit int) (int64, error)
}

type BillingOutboxFinalizationRepository interface {
	BillingOutboxRepository
	ClaimFinalization(ctx context.Context, workerID string, limit int, lease time.Duration) ([]BillingOutboxRecord, error)
	ClaimFinalizationExpiredLeased(ctx context.Context, workerID string, limit int, lease time.Duration) ([]BillingOutboxRecord, error)
	RenewFinalizationLease(ctx context.Context, id int64, workerID string, lease time.Duration) error
	RetryFinalization(ctx context.Context, id int64, workerID string, availableAt time.Time, lastError string, terminal bool) error
	AckFinalization(ctx context.Context, id int64, workerID string) error
}
