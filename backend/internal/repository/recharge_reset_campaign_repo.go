package repository

import (
	"context"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/rechargeresetcampaign"
	"github.com/Wei-Shaw/sub2api/ent/rechargeresetcampaignrule"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type rechargeResetCampaignRepository struct {
	client *dbent.Client
}

func NewRechargeResetCampaignRepository(client *dbent.Client) service.RechargeResetRepository {
	return &rechargeResetCampaignRepository{client: client}
}

func (r *rechargeResetCampaignRepository) CreateCampaign(ctx context.Context, input *service.CreateRechargeResetCampaignInput) (*service.RechargeResetCampaign, error) {
	client := clientFromContext(ctx, r.client)
	create := client.RechargeResetCampaign.Create().
		SetName(strings.TrimSpace(input.Name)).
		SetDescription(input.Description).
		SetStatus(input.Status).
		SetStartsAt(input.StartsAt).
		SetNillableEndsAt(input.EndsAt).
		SetResetDaily(input.ResetDaily).
		SetResetWeekly(input.ResetWeekly).
		SetResetMonthly(input.ResetMonthly)
	if input.Metadata != nil {
		create.SetMetadata(input.Metadata)
	}
	m, err := create.Save(ctx)
	if err != nil {
		return nil, err
	}
	return rechargeResetCampaignEntityToService(m), nil
}

func (r *rechargeResetCampaignRepository) UpdateCampaign(ctx context.Context, id int64, input *service.UpdateRechargeResetCampaignInput) (*service.RechargeResetCampaign, error) {
	client := clientFromContext(ctx, r.client)
	up := client.RechargeResetCampaign.UpdateOneID(id)
	if input.Name != nil {
		up.SetName(strings.TrimSpace(*input.Name))
	}
	if input.Description != nil {
		up.SetDescription(*input.Description)
	}
	if input.Status != nil {
		up.SetStatus(*input.Status)
	}
	if input.StartsAt != nil {
		up.SetStartsAt(*input.StartsAt)
	}
	if input.EndsAt.Set {
		if input.EndsAt.Value != nil {
			up.SetEndsAt(*input.EndsAt.Value)
		} else {
			up.ClearEndsAt()
		}
	}
	if input.ResetDaily != nil {
		up.SetResetDaily(*input.ResetDaily)
	}
	if input.ResetWeekly != nil {
		up.SetResetWeekly(*input.ResetWeekly)
	}
	if input.ResetMonthly != nil {
		up.SetResetMonthly(*input.ResetMonthly)
	}
	if input.Metadata != nil {
		up.SetMetadata(input.Metadata)
	}
	m, err := up.Save(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, service.ErrRechargeResetCampaignNotFound
		}
		return nil, err
	}
	return rechargeResetCampaignEntityToService(m), nil
}

func (r *rechargeResetCampaignRepository) GetCampaign(ctx context.Context, id int64) (*service.RechargeResetCampaign, error) {
	client := clientFromContext(ctx, r.client)
	m, err := client.RechargeResetCampaign.Query().
		Where(rechargeresetcampaign.IDEQ(id)).
		WithRules(func(q *dbent.RechargeResetCampaignRuleQuery) {
			q.Order(dbent.Asc(rechargeresetcampaignrule.FieldGroupID), dbent.Asc(rechargeresetcampaignrule.FieldID))
		}).
		Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, service.ErrRechargeResetCampaignNotFound
		}
		return nil, err
	}
	return rechargeResetCampaignEntityToService(m), nil
}

func (r *rechargeResetCampaignRepository) ListCampaigns(ctx context.Context, params pagination.PaginationParams, status string) ([]service.RechargeResetCampaign, *pagination.PaginationResult, error) {
	client := clientFromContext(ctx, r.client)
	q := client.RechargeResetCampaign.Query()
	if status != "" {
		q = q.Where(rechargeresetcampaign.StatusEQ(status))
	}
	total, err := q.Count(ctx)
	if err != nil {
		return nil, nil, err
	}
	items, err := q.WithRules(func(rq *dbent.RechargeResetCampaignRuleQuery) {
		rq.Order(dbent.Asc(rechargeresetcampaignrule.FieldGroupID), dbent.Asc(rechargeresetcampaignrule.FieldID))
	}).Offset(params.Offset()).Limit(params.Limit()).Order(dbent.Desc(rechargeresetcampaign.FieldID)).All(ctx)
	if err != nil {
		return nil, nil, err
	}
	return rechargeResetCampaignEntitiesToService(items), paginationResultFromTotal(int64(total), params), nil
}

func (r *rechargeResetCampaignRepository) ListActiveCampaigns(ctx context.Context, at time.Time) ([]service.RechargeResetCampaign, error) {
	client := clientFromContext(ctx, r.client)
	items, err := client.RechargeResetCampaign.Query().
		Where(
			rechargeresetcampaign.StatusEQ(service.RechargeResetCampaignStatusActive),
			rechargeresetcampaign.StartsAtLTE(at),
			rechargeresetcampaign.Or(rechargeresetcampaign.EndsAtIsNil(), rechargeresetcampaign.EndsAtGT(at)),
		).
		WithRules(func(q *dbent.RechargeResetCampaignRuleQuery) {
			q.Where(rechargeresetcampaignrule.StatusEQ(service.RechargeResetRuleStatusActive)).
				Order(dbent.Asc(rechargeresetcampaignrule.FieldGroupID), dbent.Asc(rechargeresetcampaignrule.FieldID))
		}).
		Order(dbent.Asc(rechargeresetcampaign.FieldID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return rechargeResetCampaignEntitiesToService(items), nil
}

func (r *rechargeResetCampaignRepository) CreateRule(ctx context.Context, input *service.CreateRechargeResetRuleInput) (*service.RechargeResetCampaignRule, error) {
	client := clientFromContext(ctx, r.client)
	create := client.RechargeResetCampaignRule.Create().
		SetCampaignID(input.CampaignID).
		SetGroupID(input.GroupID).
		SetThresholdAmount(input.ThresholdAmount).
		SetStatus(input.Status)
	if input.Metadata != nil {
		create.SetMetadata(input.Metadata)
	}
	m, err := create.Save(ctx)
	if err != nil {
		return nil, err
	}
	return rechargeResetRuleEntityToService(m), nil
}

func (r *rechargeResetCampaignRepository) UpdateRule(ctx context.Context, id int64, input *service.UpdateRechargeResetRuleInput) (*service.RechargeResetCampaignRule, error) {
	client := clientFromContext(ctx, r.client)
	up := client.RechargeResetCampaignRule.UpdateOneID(id)
	if input.GroupID != nil {
		up.SetGroupID(*input.GroupID)
	}
	if input.ThresholdAmount != nil {
		up.SetThresholdAmount(*input.ThresholdAmount)
	}
	if input.Status != nil {
		up.SetStatus(*input.Status)
	}
	if input.Metadata != nil {
		up.SetMetadata(input.Metadata)
	}
	m, err := up.Save(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, service.ErrRechargeResetRuleNotFound
		}
		return nil, err
	}
	return rechargeResetRuleEntityToService(m), nil
}

func (r *rechargeResetCampaignRepository) ListRules(ctx context.Context, campaignID int64) ([]service.RechargeResetCampaignRule, error) {
	client := clientFromContext(ctx, r.client)
	items, err := client.RechargeResetCampaignRule.Query().
		Where(rechargeresetcampaignrule.CampaignIDEQ(campaignID)).
		Order(dbent.Asc(rechargeresetcampaignrule.FieldGroupID), dbent.Asc(rechargeresetcampaignrule.FieldID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return rechargeResetRuleEntitiesToService(items), nil
}

func (r *rechargeResetCampaignRepository) CreateRecord(ctx context.Context, record *service.RechargeResetRecord) (*service.RechargeResetRecord, error) {
	client := clientFromContext(ctx, r.client)
	create := client.RechargeResetRecord.Create().
		SetCampaignID(record.CampaignID).
		SetRuleID(record.RuleID).
		SetRedeemCodeID(record.RedeemCodeID).
		SetUserID(record.UserID).
		SetSubscriptionID(record.SubscriptionID).
		SetGroupID(record.GroupID).
		SetRechargeAmount(record.RechargeAmount).
		SetThresholdAmount(record.ThresholdAmount).
		SetResetDaily(record.ResetDaily).
		SetResetWeekly(record.ResetWeekly).
		SetResetMonthly(record.ResetMonthly)
	if record.Metadata != nil {
		create.SetMetadata(record.Metadata)
	}
	m, err := create.Save(ctx)
	if err != nil {
		if isUniqueConstraintViolation(err) {
			return nil, service.ErrRechargeResetRecordExists
		}
		return nil, err
	}
	return rechargeResetRecordEntityToService(m), nil
}

func rechargeResetCampaignEntityToService(m *dbent.RechargeResetCampaign) *service.RechargeResetCampaign {
	if m == nil {
		return nil
	}
	out := &service.RechargeResetCampaign{
		ID:           m.ID,
		Name:         m.Name,
		Description:  m.Description,
		Status:       m.Status,
		StartsAt:     m.StartsAt,
		EndsAt:       m.EndsAt,
		ResetDaily:   m.ResetDaily,
		ResetWeekly:  m.ResetWeekly,
		ResetMonthly: m.ResetMonthly,
		Metadata:     m.Metadata,
		CreatedAt:    m.CreatedAt,
		UpdatedAt:    m.UpdatedAt,
	}
	if len(m.Edges.Rules) > 0 {
		out.Rules = rechargeResetRuleEntitiesToService(m.Edges.Rules)
	}
	return out
}

func rechargeResetCampaignEntitiesToService(models []*dbent.RechargeResetCampaign) []service.RechargeResetCampaign {
	out := make([]service.RechargeResetCampaign, 0, len(models))
	for _, m := range models {
		if s := rechargeResetCampaignEntityToService(m); s != nil {
			out = append(out, *s)
		}
	}
	return out
}

func rechargeResetRuleEntityToService(m *dbent.RechargeResetCampaignRule) *service.RechargeResetCampaignRule {
	if m == nil {
		return nil
	}
	return &service.RechargeResetCampaignRule{
		ID:              m.ID,
		CampaignID:      m.CampaignID,
		GroupID:         m.GroupID,
		ThresholdAmount: m.ThresholdAmount,
		Status:          m.Status,
		Metadata:        m.Metadata,
		CreatedAt:       m.CreatedAt,
		UpdatedAt:       m.UpdatedAt,
	}
}

func rechargeResetRuleEntitiesToService(models []*dbent.RechargeResetCampaignRule) []service.RechargeResetCampaignRule {
	out := make([]service.RechargeResetCampaignRule, 0, len(models))
	for _, m := range models {
		if s := rechargeResetRuleEntityToService(m); s != nil {
			out = append(out, *s)
		}
	}
	return out
}

func rechargeResetRecordEntityToService(m *dbent.RechargeResetRecord) *service.RechargeResetRecord {
	if m == nil {
		return nil
	}
	return &service.RechargeResetRecord{
		ID:              m.ID,
		CampaignID:      m.CampaignID,
		RuleID:          m.RuleID,
		RedeemCodeID:    m.RedeemCodeID,
		UserID:          m.UserID,
		SubscriptionID:  m.SubscriptionID,
		GroupID:         m.GroupID,
		RechargeAmount:  m.RechargeAmount,
		ThresholdAmount: m.ThresholdAmount,
		ResetDaily:      m.ResetDaily,
		ResetWeekly:     m.ResetWeekly,
		ResetMonthly:    m.ResetMonthly,
		Metadata:        m.Metadata,
		CreatedAt:       m.CreatedAt,
	}
}
