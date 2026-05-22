package service

import (
	"context"
	"errors"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
)

const (
	RechargeResetCampaignStatusDraft    = "draft"
	RechargeResetCampaignStatusActive   = "active"
	RechargeResetCampaignStatusDisabled = "disabled"
	RechargeResetCampaignStatusEnded    = "ended"

	RechargeResetRuleStatusActive   = "active"
	RechargeResetRuleStatusDisabled = "disabled"

	RechargeResetAuditActionApplied = "RECHARGE_RESET_APPLIED"
	RechargeResetAuditActionSkipped = "RECHARGE_RESET_SKIPPED"
)

var (
	ErrRechargeResetCampaignNotFound = infraerrors.NotFound("RECHARGE_RESET_CAMPAIGN_NOT_FOUND", "recharge reset campaign not found")
	ErrRechargeResetRuleNotFound     = infraerrors.NotFound("RECHARGE_RESET_RULE_NOT_FOUND", "recharge reset campaign rule not found")
	ErrRechargeResetRecordExists     = infraerrors.Conflict("RECHARGE_RESET_RECORD_EXISTS", "recharge reset record already exists")
	ErrRechargeResetInvalidInput     = infraerrors.BadRequest("RECHARGE_RESET_INVALID_INPUT", "invalid recharge reset campaign input")
)

type RechargeResetCampaign struct {
	ID           int64                       `json:"id"`
	Name         string                      `json:"name"`
	Description  string                      `json:"description"`
	Status       string                      `json:"status"`
	StartsAt     time.Time                   `json:"starts_at"`
	EndsAt       *time.Time                  `json:"ends_at,omitempty"`
	ResetDaily   bool                        `json:"reset_daily"`
	ResetWeekly  bool                        `json:"reset_weekly"`
	ResetMonthly bool                        `json:"reset_monthly"`
	Metadata     map[string]any              `json:"metadata,omitempty"`
	CreatedAt    time.Time                   `json:"created_at"`
	UpdatedAt    time.Time                   `json:"updated_at"`
	Rules        []RechargeResetCampaignRule `json:"rules,omitempty"`
}

type RechargeResetCampaignRule struct {
	ID              int64          `json:"id"`
	CampaignID      int64          `json:"campaign_id"`
	GroupID         int64          `json:"group_id"`
	ThresholdAmount float64        `json:"threshold_amount"`
	Status          string         `json:"status"`
	Metadata        map[string]any `json:"metadata,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
}

type RechargeResetRecord struct {
	ID              int64          `json:"id"`
	CampaignID      int64          `json:"campaign_id"`
	RuleID          int64          `json:"rule_id"`
	OrderID         int64          `json:"order_id"`
	UserID          int64          `json:"user_id"`
	SubscriptionID  int64          `json:"subscription_id"`
	GroupID         int64          `json:"group_id"`
	RechargeAmount  float64        `json:"recharge_amount"`
	ThresholdAmount float64        `json:"threshold_amount"`
	ResetDaily      bool           `json:"reset_daily"`
	ResetWeekly     bool           `json:"reset_weekly"`
	ResetMonthly    bool           `json:"reset_monthly"`
	Metadata        map[string]any `json:"metadata,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
}

type CreateRechargeResetCampaignInput struct {
	Name         string
	Description  string
	Status       string
	StartsAt     time.Time
	EndsAt       *time.Time
	ResetDaily   bool
	ResetWeekly  bool
	ResetMonthly bool
	Metadata     map[string]any
}

type UpdateRechargeResetCampaignInput struct {
	Name         *string
	Description  *string
	Status       *string
	StartsAt     *time.Time
	EndsAt       NullableTimeUpdate
	ResetDaily   *bool
	ResetWeekly  *bool
	ResetMonthly *bool
	Metadata     map[string]any
}

type CreateRechargeResetRuleInput struct {
	CampaignID      int64
	GroupID         int64
	ThresholdAmount float64
	Status          string
	Metadata        map[string]any
}

type UpdateRechargeResetRuleInput struct {
	GroupID         *int64
	ThresholdAmount *float64
	Status          *string
	Metadata        map[string]any
}

type ApplyRechargeResetInput struct {
	OrderID        int64
	UserID         int64
	RechargeAmount float64
	OccurredAt     time.Time
}

type RechargeResetRepository interface {
	CreateCampaign(ctx context.Context, input *CreateRechargeResetCampaignInput) (*RechargeResetCampaign, error)
	UpdateCampaign(ctx context.Context, id int64, input *UpdateRechargeResetCampaignInput) (*RechargeResetCampaign, error)
	GetCampaign(ctx context.Context, id int64) (*RechargeResetCampaign, error)
	ListCampaigns(ctx context.Context, params pagination.PaginationParams, status string) ([]RechargeResetCampaign, *pagination.PaginationResult, error)
	ListActiveCampaigns(ctx context.Context, at time.Time) ([]RechargeResetCampaign, error)
	CreateRule(ctx context.Context, input *CreateRechargeResetRuleInput) (*RechargeResetCampaignRule, error)
	UpdateRule(ctx context.Context, id int64, input *UpdateRechargeResetRuleInput) (*RechargeResetCampaignRule, error)
	ListRules(ctx context.Context, campaignID int64) ([]RechargeResetCampaignRule, error)
	CreateRecord(ctx context.Context, record *RechargeResetRecord) (*RechargeResetRecord, error)
}

type RechargeResetCampaignService struct {
	repo            RechargeResetRepository
	subscriptionSvc *SubscriptionService
}

func NewRechargeResetCampaignService(repo RechargeResetRepository, subscriptionSvc *SubscriptionService) *RechargeResetCampaignService {
	return &RechargeResetCampaignService{repo: repo, subscriptionSvc: subscriptionSvc}
}

func (s *RechargeResetCampaignService) CreateCampaign(ctx context.Context, input *CreateRechargeResetCampaignInput) (*RechargeResetCampaign, error) {
	if input == nil || input.Name == "" || !hasAnyRechargeResetWindow(input.ResetDaily, input.ResetWeekly, input.ResetMonthly) {
		return nil, ErrRechargeResetInvalidInput
	}
	if input.Status == "" {
		input.Status = RechargeResetCampaignStatusDraft
	}
	return s.repo.CreateCampaign(ctx, input)
}

func (s *RechargeResetCampaignService) UpdateCampaign(ctx context.Context, id int64, input *UpdateRechargeResetCampaignInput) (*RechargeResetCampaign, error) {
	if input == nil {
		return nil, ErrRechargeResetInvalidInput
	}
	return s.repo.UpdateCampaign(ctx, id, input)
}

func (s *RechargeResetCampaignService) GetCampaign(ctx context.Context, id int64) (*RechargeResetCampaign, error) {
	return s.repo.GetCampaign(ctx, id)
}

func (s *RechargeResetCampaignService) ListCampaigns(ctx context.Context, params pagination.PaginationParams, status string) ([]RechargeResetCampaign, *pagination.PaginationResult, error) {
	return s.repo.ListCampaigns(ctx, params, status)
}

func (s *RechargeResetCampaignService) CreateRule(ctx context.Context, input *CreateRechargeResetRuleInput) (*RechargeResetCampaignRule, error) {
	if input == nil || input.CampaignID <= 0 || input.GroupID <= 0 || input.ThresholdAmount <= 0 {
		return nil, ErrRechargeResetInvalidInput
	}
	if input.Status == "" {
		input.Status = RechargeResetRuleStatusActive
	}
	return s.repo.CreateRule(ctx, input)
}

func (s *RechargeResetCampaignService) UpdateRule(ctx context.Context, id int64, input *UpdateRechargeResetRuleInput) (*RechargeResetCampaignRule, error) {
	if input == nil {
		return nil, ErrRechargeResetInvalidInput
	}
	return s.repo.UpdateRule(ctx, id, input)
}

func (s *RechargeResetCampaignService) ListRules(ctx context.Context, campaignID int64) ([]RechargeResetCampaignRule, error) {
	return s.repo.ListRules(ctx, campaignID)
}

func (s *RechargeResetCampaignService) ApplyForRecharge(ctx context.Context, input *ApplyRechargeResetInput) ([]RechargeResetRecord, error) {
	if s == nil || s.repo == nil || s.subscriptionSvc == nil || input == nil || input.OrderID <= 0 || input.UserID <= 0 || input.RechargeAmount <= 0 {
		return nil, nil
	}
	if dbent.TxFromContext(ctx) == nil && s.subscriptionSvc.entClient != nil {
		tx, err := s.subscriptionSvc.entClient.Tx(ctx)
		if err != nil {
			return nil, err
		}
		defer func() { _ = tx.Rollback() }()
		records, err := s.ApplyForRecharge(dbent.NewTxContext(ctx, tx), input)
		if err != nil {
			return records, err
		}
		if err := tx.Commit(); err != nil {
			return records, err
		}
		s.invalidateRechargeResetRecordCaches(ctx, records)
		return records, nil
	}
	at := input.OccurredAt
	if at.IsZero() {
		at = time.Now().UTC()
	}
	at = at.UTC()
	campaigns, err := s.repo.ListActiveCampaigns(ctx, at)
	if err != nil {
		return nil, err
	}
	if len(campaigns) == 0 {
		return nil, nil
	}
	subs, err := s.subscriptionSvc.ListActiveUserSubscriptions(ctx, input.UserID)
	if err != nil {
		return nil, err
	}
	subByGroup := make(map[int64]UserSubscription, len(subs))
	for _, sub := range subs {
		subByGroup[sub.GroupID] = sub
	}
	records := make([]RechargeResetRecord, 0)
	for _, campaign := range campaigns {
		for _, rule := range campaign.Rules {
			if rule.Status != RechargeResetRuleStatusActive || input.RechargeAmount < rule.ThresholdAmount {
				continue
			}
			sub, ok := subByGroup[rule.GroupID]
			if !ok {
				continue
			}
			resetDaily, resetWeekly, resetMonthly := campaign.ResetDaily, campaign.ResetWeekly, campaign.ResetMonthly
			if !hasAnyRechargeResetWindow(resetDaily, resetWeekly, resetMonthly) {
				continue
			}
			record, err := s.repo.CreateRecord(ctx, &RechargeResetRecord{
				CampaignID:      campaign.ID,
				RuleID:          rule.ID,
				OrderID:         input.OrderID,
				UserID:          input.UserID,
				SubscriptionID:  sub.ID,
				GroupID:         sub.GroupID,
				RechargeAmount:  input.RechargeAmount,
				ThresholdAmount: rule.ThresholdAmount,
				ResetDaily:      resetDaily,
				ResetWeekly:     resetWeekly,
				ResetMonthly:    resetMonthly,
				Metadata: map[string]any{
					"occurred_at": at.Format(time.RFC3339Nano),
				},
			})
			if err != nil {
				if errors.Is(err, ErrRechargeResetRecordExists) {
					continue
				}
				return records, err
			}
			if err := s.subscriptionSvc.AdminResetQuotaBySubscriptionAt(ctx, &sub, at, resetDaily, resetWeekly, resetMonthly); err != nil {
				return records, err
			}
			if record != nil {
				records = append(records, *record)
			}
		}
	}
	return records, nil
}

func (s *RechargeResetCampaignService) invalidateRechargeResetRecordCaches(ctx context.Context, records []RechargeResetRecord) {
	seen := make(map[[2]int64]struct{}, len(records))
	for _, record := range records {
		key := [2]int64{record.UserID, record.GroupID}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		s.subscriptionSvc.InvalidateSubscriptionCaches(ctx, record.UserID, record.GroupID)
	}
}

func hasAnyRechargeResetWindow(daily, weekly, monthly bool) bool {
	return daily || weekly || monthly
}
