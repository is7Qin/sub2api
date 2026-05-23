package admin

import (
	"context"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

type RechargeResetHandler struct {
	rechargeResetService *service.RechargeResetCampaignService
}

func NewRechargeResetHandler(rechargeResetService *service.RechargeResetCampaignService) *RechargeResetHandler {
	return &RechargeResetHandler{rechargeResetService: rechargeResetService}
}

type CreateRechargeResetCampaignRequest struct {
	Name         string         `json:"name" binding:"required"`
	Description  string         `json:"description"`
	Status       string         `json:"status" binding:"omitempty,oneof=draft active disabled ended"`
	StartsAt     *time.Time     `json:"starts_at" binding:"required"`
	EndsAt       *time.Time     `json:"ends_at"`
	ResetDaily   *bool          `json:"reset_daily"`
	ResetWeekly  *bool          `json:"reset_weekly"`
	ResetMonthly *bool          `json:"reset_monthly"`
	Metadata     map[string]any `json:"metadata"`
}

type UpdateRechargeResetCampaignRequest struct {
	Name         *string               `json:"name"`
	Description  *string               `json:"description"`
	Status       *string               `json:"status" binding:"omitempty,oneof=draft active disabled ended"`
	StartsAt     *time.Time            `json:"starts_at"`
	EndsAt       dto.NullableTimeField `json:"ends_at,omitempty"`
	ResetDaily   *bool                 `json:"reset_daily"`
	ResetWeekly  *bool                 `json:"reset_weekly"`
	ResetMonthly *bool                 `json:"reset_monthly"`
	Metadata     map[string]any        `json:"metadata"`
}

type CreateRechargeResetRuleRequest struct {
	GroupID         int64          `json:"group_id" binding:"required,gt=0"`
	ThresholdAmount float64        `json:"threshold_amount" binding:"required,gt=0"`
	Status          string         `json:"status" binding:"omitempty,oneof=active disabled"`
	Metadata        map[string]any `json:"metadata"`
}

type UpdateRechargeResetRuleRequest struct {
	GroupID         *int64         `json:"group_id"`
	ThresholdAmount *float64       `json:"threshold_amount"`
	Status          *string        `json:"status" binding:"omitempty,oneof=active disabled"`
	Metadata        map[string]any `json:"metadata"`
}

func (h *RechargeResetHandler) ListCampaigns(c *gin.Context) {
	page, pageSize := response.ParsePagination(c)
	params := pagination.PaginationParams{Page: page, PageSize: pageSize}
	items, paginationResult, err := h.rechargeResetService.ListCampaigns(c.Request.Context(), params, c.Query("status"))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.PaginatedWithResult(c, items, toResponsePagination(paginationResult))
}

func (h *RechargeResetHandler) GetCampaign(c *gin.Context) {
	campaignID, ok := parseRechargeResetID(c, "id", "Invalid campaign ID")
	if !ok {
		return
	}
	campaign, err := h.rechargeResetService.GetCampaign(c.Request.Context(), campaignID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, campaign)
}

func (h *RechargeResetHandler) CreateCampaign(c *gin.Context) {
	var req CreateRechargeResetCampaignRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	input := &service.CreateRechargeResetCampaignInput{
		Name:         req.Name,
		Description:  req.Description,
		Status:       req.Status,
		StartsAt:     *req.StartsAt,
		EndsAt:       req.EndsAt,
		ResetDaily:   rechargeResetBoolDefault(req.ResetDaily, true),
		ResetWeekly:  rechargeResetBoolDefault(req.ResetWeekly, true),
		ResetMonthly: rechargeResetBoolDefault(req.ResetMonthly, true),
		Metadata:     req.Metadata,
	}
	executeAdminIdempotentJSON(c, "admin.recharge_reset.campaigns.create", req, service.DefaultWriteIdempotencyTTL(), func(ctx context.Context) (any, error) {
		return h.rechargeResetService.CreateCampaign(ctx, input)
	})
}

func (h *RechargeResetHandler) UpdateCampaign(c *gin.Context) {
	campaignID, ok := parseRechargeResetID(c, "id", "Invalid campaign ID")
	if !ok {
		return
	}
	var req UpdateRechargeResetCampaignRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	input := &service.UpdateRechargeResetCampaignInput{
		Name:         req.Name,
		Description:  req.Description,
		Status:       req.Status,
		StartsAt:     req.StartsAt,
		ResetDaily:   req.ResetDaily,
		ResetWeekly:  req.ResetWeekly,
		ResetMonthly: req.ResetMonthly,
		Metadata:     req.Metadata,
	}
	if req.EndsAt.Set {
		input.EndsAt = service.NullableTimeUpdate{Set: true, Value: req.EndsAt.Value}
	}
	payload := struct {
		CampaignID int64                              `json:"campaign_id"`
		Body       UpdateRechargeResetCampaignRequest `json:"body"`
	}{CampaignID: campaignID, Body: req}
	executeAdminIdempotentJSON(c, "admin.recharge_reset.campaigns.update", payload, service.DefaultWriteIdempotencyTTL(), func(ctx context.Context) (any, error) {
		return h.rechargeResetService.UpdateCampaign(ctx, campaignID, input)
	})
}

func (h *RechargeResetHandler) ListRules(c *gin.Context) {
	campaignID, ok := parseRechargeResetID(c, "id", "Invalid campaign ID")
	if !ok {
		return
	}
	rules, err := h.rechargeResetService.ListRules(c.Request.Context(), campaignID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, rules)
}

func (h *RechargeResetHandler) CreateRule(c *gin.Context) {
	campaignID, ok := parseRechargeResetID(c, "id", "Invalid campaign ID")
	if !ok {
		return
	}
	var req CreateRechargeResetRuleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	input := &service.CreateRechargeResetRuleInput{
		CampaignID:      campaignID,
		GroupID:         req.GroupID,
		ThresholdAmount: req.ThresholdAmount,
		Status:          req.Status,
		Metadata:        req.Metadata,
	}
	payload := struct {
		CampaignID int64                          `json:"campaign_id"`
		Body       CreateRechargeResetRuleRequest `json:"body"`
	}{CampaignID: campaignID, Body: req}
	executeAdminIdempotentJSON(c, "admin.recharge_reset.rules.create", payload, service.DefaultWriteIdempotencyTTL(), func(ctx context.Context) (any, error) {
		return h.rechargeResetService.CreateRule(ctx, input)
	})
}

func (h *RechargeResetHandler) UpdateRule(c *gin.Context) {
	ruleID, ok := parseRechargeResetID(c, "rule_id", "Invalid rule ID")
	if !ok {
		return
	}
	var req UpdateRechargeResetRuleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	input := &service.UpdateRechargeResetRuleInput{
		GroupID:         req.GroupID,
		ThresholdAmount: req.ThresholdAmount,
		Status:          req.Status,
		Metadata:        req.Metadata,
	}
	payload := struct {
		RuleID int64                          `json:"rule_id"`
		Body   UpdateRechargeResetRuleRequest `json:"body"`
	}{RuleID: ruleID, Body: req}
	executeAdminIdempotentJSON(c, "admin.recharge_reset.rules.update", payload, service.DefaultWriteIdempotencyTTL(), func(ctx context.Context) (any, error) {
		return h.rechargeResetService.UpdateRule(ctx, ruleID, input)
	})
}

func parseRechargeResetID(c *gin.Context, param string, message string) (int64, bool) {
	id, err := strconv.ParseInt(c.Param(param), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, message)
		return 0, false
	}
	return id, true
}

func rechargeResetBoolDefault(v *bool, fallback bool) bool {
	if v == nil {
		return fallback
	}
	return *v
}
