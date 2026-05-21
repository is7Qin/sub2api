package service

import (
	"context"
	"time"
)

const (
	TimedQuotaGrantStatusActive    = "active"
	TimedQuotaGrantStatusExhausted = "exhausted"
	TimedQuotaGrantStatusExpired   = "expired"
	TimedQuotaGrantStatusRevoked   = "revoked"

	TimedQuotaGrantSourceRedeemCode = "redeem_code"
)

type UserQuotaGrant struct {
	ID            int64          `json:"id"`
	UserID        int64          `json:"user_id"`
	AmountUSD     float64        `json:"amount_usd"`
	UsedAmountUSD float64        `json:"used_amount_usd"`
	StartsAt      time.Time      `json:"starts_at"`
	ExpiresAt     time.Time      `json:"expires_at"`
	Source        string         `json:"source"`
	SourceID      string         `json:"source_id"`
	Status        string         `json:"status"`
	Metadata      map[string]any `json:"metadata,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

type CreateUserQuotaGrantInput struct {
	UserID        int64
	AmountUSD     float64
	StartsAt      time.Time
	ExpiresAt     time.Time
	Source        string
	SourceID      string
	Metadata      map[string]any
}

type UserQuotaGrantRepository interface {
	Create(ctx context.Context, input *CreateUserQuotaGrantInput) (*UserQuotaGrant, error)
	ListActiveByUser(ctx context.Context, userID int64, now time.Time, limit int) ([]UserQuotaGrant, error)
	Revoke(ctx context.Context, id int64) error
}
