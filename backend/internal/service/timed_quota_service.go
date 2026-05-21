package service

import (
	"context"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

type TimedQuotaService struct {
	repo UserQuotaGrantRepository
}

func NewTimedQuotaService(repo UserQuotaGrantRepository) *TimedQuotaService {
	return &TimedQuotaService{repo: repo}
}

func (s *TimedQuotaService) Grant(ctx context.Context, input *CreateUserQuotaGrantInput) (*UserQuotaGrant, error) {
	if s == nil || s.repo == nil {
		return nil, infraerrors.ServiceUnavailable("TIMED_QUOTA_REPO_UNAVAILABLE", "timed quota repository not available")
	}
	if input == nil {
		return nil, infraerrors.BadRequest("TIMED_QUOTA_INVALID", "timed quota grant input is required")
	}
	if input.UserID <= 0 {
		return nil, infraerrors.BadRequest("TIMED_QUOTA_INVALID_USER", "invalid user")
	}
	if input.AmountUSD <= 0 {
		return nil, infraerrors.BadRequest("TIMED_QUOTA_INVALID_AMOUNT", "amount must be positive")
	}
	now := time.Now().UTC()
	if input.StartsAt.IsZero() {
		input.StartsAt = now
	} else {
		input.StartsAt = input.StartsAt.UTC()
	}
	if input.ExpiresAt.IsZero() || !input.ExpiresAt.After(input.StartsAt) {
		return nil, infraerrors.BadRequest("TIMED_QUOTA_INVALID_EXPIRY", "expires_at must be after starts_at")
	}
	input.ExpiresAt = input.ExpiresAt.UTC()
	if input.Source == "" {
		input.Source = TimedQuotaGrantSourceRedeemCode
	}
	return s.repo.Create(ctx, input)
}

func (s *TimedQuotaService) ListActiveByUser(ctx context.Context, userID int64, limit int) ([]UserQuotaGrant, error) {
	if s == nil || s.repo == nil {
		return []UserQuotaGrant{}, nil
	}
	if userID <= 0 {
		return []UserQuotaGrant{}, nil
	}
	return s.repo.ListActiveByUser(ctx, userID, time.Now().UTC(), limit)
}
