package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

var ErrUsageBillingRequestIDRequired = errors.New("usage billing request_id is required")
var ErrUsageBillingRequestConflict = errors.New("usage billing request fingerprint conflict")

// UsageBillingCommand describes one billable request that must be applied at most once.
type UsageBillingCommand struct {
	RequestID          string
	APIKeyID           int64
	RequestFingerprint string
	RequestPayloadHash string

	UserID              int64
	AccountID           int64
	SubscriptionID      *int64
	AccountType         string
	Model               string
	ServiceTier         string
	ReasoningEffort     string
	BillingType         int8
	InputTokens         int
	OutputTokens        int
	CacheCreationTokens int
	CacheReadTokens     int
	ImageCount          int
	MediaType           string

	BalanceCost         float64
	SubscriptionCost    float64
	APIKeyQuotaCost     float64
	APIKeyRateLimitCost float64
	AccountQuotaCost    float64

	// UsageLog is persisted in the same transaction as billing effects.
	UsageLog *UsageLog
}

func (c *UsageBillingCommand) Normalize() {
	if c == nil {
		return
	}
	c.RequestID = strings.TrimSpace(c.RequestID)
	if strings.TrimSpace(c.RequestFingerprint) == "" {
		c.RequestFingerprint = buildUsageBillingFingerprint(c)
	}
}

func (c *UsageBillingCommand) Validate() error {
	if c == nil || c.AccountID <= 0 {
		return ErrUsageLogAccountRequired
	}
	if c.UsageLog == nil {
		return nil
	}
	if err := c.UsageLog.ValidateForCreate(); err != nil {
		return err
	}
	if *c.UsageLog.AccountID != c.AccountID {
		return ErrUsageLogAccountRequired
	}
	return nil
}

func buildUsageBillingFingerprint(c *UsageBillingCommand) string {
	if c == nil {
		return ""
	}
	raw := fmt.Sprintf(
		"%d|%d|%d|%s|%s|%s|%s|%d|%d|%d|%d|%d|%d|%s|%d|%0.10f|%0.10f|%0.10f|%0.10f|%0.10f",
		c.UserID,
		c.AccountID,
		c.APIKeyID,
		strings.TrimSpace(c.AccountType),
		strings.TrimSpace(c.Model),
		strings.TrimSpace(c.ServiceTier),
		strings.TrimSpace(c.ReasoningEffort),
		c.BillingType,
		c.InputTokens,
		c.OutputTokens,
		c.CacheCreationTokens,
		c.CacheReadTokens,
		c.ImageCount,
		strings.TrimSpace(c.MediaType),
		valueOrZero(c.SubscriptionID),
		c.BalanceCost,
		c.SubscriptionCost,
		c.APIKeyQuotaCost,
		c.APIKeyRateLimitCost,
		c.AccountQuotaCost,
	)
	if payloadHash := strings.TrimSpace(c.RequestPayloadHash); payloadHash != "" {
		raw += "|" + payloadHash
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func HashUsageRequestPayload(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func valueOrZero(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

// AccountQuotaState holds the post-increment quota state returned by the DB transaction.
// All values are post-update (i.e., already include the increment).
type AccountQuotaState struct {
	TotalUsed   float64 `json:"total_used"`
	TotalLimit  float64 `json:"total_limit"`
	DailyUsed   float64 `json:"daily_used"`
	DailyLimit  float64 `json:"daily_limit"`
	WeeklyUsed  float64 `json:"weekly_used"`
	WeeklyLimit float64 `json:"weekly_limit"`
}

type UsageBillingOutboxBinding struct {
	OutboxID int64
	WorkerID string
}

type UsageBillingApplyResult struct {
	Applied              bool               `json:"applied"`
	UsageLogPersisted    bool               `json:"usage_log_persisted"`
	APIKeyQuotaExhausted bool               `json:"api_key_quota_exhausted"`
	BalanceOverdrafted   bool               `json:"balance_overdrafted"`
	NewBalance           *float64           `json:"new_balance,omitempty"` // post-deduction balance (nil = no balance deduction)
	QuotaState           *AccountQuotaState `json:"quota_state,omitempty"` // post-increment quota state (nil = no quota increment)
}

type UsageBillingRepository interface {
	Apply(ctx context.Context, cmd *UsageBillingCommand) (*UsageBillingApplyResult, error)
}

// UsageBillingFinalizationRepository atomically commits a newly applied billing
// command with the durable outbox marker needed to replay post-effects.
type UsageBillingFinalizationRepository interface {
	UsageBillingRepository
	ApplyAndStageOutboxFinalization(ctx context.Context, cmd *UsageBillingCommand, binding UsageBillingOutboxBinding) (*UsageBillingApplyResult, error)
}

// UsageBillingBatchItem is one outbox record handed to a batch apply transaction.
type UsageBillingBatchItem struct {
	Command UsageBillingCommand
	Binding UsageBillingOutboxBinding
}

// UsageBillingBatchOutcome reports one item's outcome inside a batch apply
// transaction. Err is non-nil when the item failed and was rolled back to its
// own savepoint; the remaining items still committed. A non-nil Err means
// Result is nil (nothing of this item persisted).
type UsageBillingBatchOutcome struct {
	Result *UsageBillingApplyResult
	Err    error
}

// UsageBillingBatchFinalizationRepository applies a whole worker round in ONE
// transaction. Per-record savepoints isolate failures: one failed record never
// drags the rest of the round. Same-user records inside one batch run serially
// within the transaction, preserving the per-user shard serialization contract
// of the worker.
type UsageBillingBatchFinalizationRepository interface {
	ApplyBatchAndStageOutboxFinalizations(ctx context.Context, items []UsageBillingBatchItem) ([]UsageBillingBatchOutcome, error)
}
