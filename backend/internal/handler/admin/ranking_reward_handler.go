package admin

import (
	"context"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

type RankingRewardHandler struct {
	rankingRewardService *service.RankingRewardService
}

func NewRankingRewardHandler(rankingRewardService *service.RankingRewardService) *RankingRewardHandler {
	return &RankingRewardHandler{rankingRewardService: rankingRewardService}
}

type CreateRankingRewardCampaignRequest struct {
	Name               string         `json:"name" binding:"required"`
	Description        string         `json:"description"`
	Status             string         `json:"status" binding:"omitempty,oneof=draft active disabled ended"`
	LotteryCampaignID  int64          `json:"lottery_campaign_id" binding:"required,gt=0"`
	TopN               int            `json:"top_n" binding:"omitempty,min=1,max=1000"`
	ChanceCount        int            `json:"chance_count" binding:"omitempty,min=1,max=1000"`
	PublicDisplayLimit int            `json:"public_display_limit" binding:"omitempty,min=1,max=1000"`
	MinActualCost      float64        `json:"min_actual_cost" binding:"omitempty,min=0"`
	StartsAt           *time.Time     `json:"starts_at"`
	EndsAt             *time.Time     `json:"ends_at"`
	Timezone           string         `json:"timezone"`
	Metadata           map[string]any `json:"metadata,omitempty"`
}

type UpdateRankingRewardCampaignRequest struct {
	Name               *string        `json:"name"`
	Description        *string        `json:"description"`
	Status             *string        `json:"status" binding:"omitempty,oneof=draft active disabled ended"`
	LotteryCampaignID  *int64         `json:"lottery_campaign_id" binding:"omitempty,gt=0"`
	TopN               *int           `json:"top_n" binding:"omitempty,min=1,max=1000"`
	ChanceCount        *int           `json:"chance_count" binding:"omitempty,min=1,max=1000"`
	PublicDisplayLimit *int           `json:"public_display_limit" binding:"omitempty,min=1,max=1000"`
	MinActualCost      *float64       `json:"min_actual_cost" binding:"omitempty,min=0"`
	StartsAt           *time.Time     `json:"starts_at"`
	EndsAt             *time.Time     `json:"ends_at"`
	ClearEndsAt        bool           `json:"clear_ends_at"`
	Timezone           *string        `json:"timezone"`
	LastRunDate        *time.Time     `json:"last_run_date"`
	ClearLastRunDate   bool           `json:"clear_last_run_date"`
	Metadata           map[string]any `json:"metadata,omitempty"`
}

type CreateRankingRewardExclusionRequest struct {
	UserID int64  `json:"user_id" binding:"required,gt=0"`
	Reason string `json:"reason"`
}

type UpdateRankingRewardExclusionRequest struct {
	Reason *string `json:"reason"`
}

type RunRankingRewardCampaignRequest struct {
	RewardDate *time.Time `json:"reward_date"`
}

func (h *RankingRewardHandler) ListCampaigns(c *gin.Context) {
	page, pageSize := response.ParsePagination(c)
	status := c.Query("status")
	items, result, err := h.rankingRewardService.ListCampaigns(c.Request.Context(), pagination.PaginationParams{Page: page, PageSize: pageSize}, status)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Paginated(c, items, result.Total, page, pageSize)
}

func (h *RankingRewardHandler) GetCampaign(c *gin.Context) {
	campaignID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid campaign ID")
		return
	}
	item, err := h.rankingRewardService.GetCampaign(c.Request.Context(), campaignID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, item)
}

func (h *RankingRewardHandler) CreateCampaign(c *gin.Context) {
	var req CreateRankingRewardCampaignRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	startsAt := time.Now().UTC()
	if req.StartsAt != nil {
		startsAt = req.StartsAt.UTC()
	}
	if req.TopN <= 0 {
		req.TopN = 10
	}
	if req.ChanceCount <= 0 {
		req.ChanceCount = 1
	}
	if req.PublicDisplayLimit <= 0 {
		req.PublicDisplayLimit = req.TopN
	}
	executeAdminIdempotentJSON(c, "admin.ranking_reward.campaigns.create", req, service.DefaultWriteIdempotencyTTL(), func(ctx context.Context) (any, error) {
		return h.rankingRewardService.CreateCampaign(ctx, &service.CreateRankingRewardCampaignInput{
			Name:               req.Name,
			Description:        req.Description,
			Status:             req.Status,
			LotteryCampaignID:  req.LotteryCampaignID,
			TopN:               req.TopN,
			ChanceCount:        req.ChanceCount,
			PublicDisplayLimit: req.PublicDisplayLimit,
			MinActualCost:      req.MinActualCost,
			StartsAt:           startsAt,
			EndsAt:             req.EndsAt,
			Timezone:           req.Timezone,
			Metadata:           req.Metadata,
		})
	})
}

func (h *RankingRewardHandler) UpdateCampaign(c *gin.Context) {
	campaignID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid campaign ID")
		return
	}
	var req UpdateRankingRewardCampaignRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	endsAt := service.NullableTimeUpdate{}
	if req.ClearEndsAt {
		endsAt.Set = true
	} else if req.EndsAt != nil {
		v := req.EndsAt.UTC()
		endsAt.Set = true
		endsAt.Value = &v
	}
	lastRunDate := service.NullableTimeUpdate{}
	if req.ClearLastRunDate {
		lastRunDate.Set = true
	} else if req.LastRunDate != nil {
		lastRunDate.Set = true
		lastRunDate.Value = req.LastRunDate
	}
	item, err := h.rankingRewardService.UpdateCampaign(c.Request.Context(), campaignID, &service.UpdateRankingRewardCampaignInput{
		Name:               req.Name,
		Description:        req.Description,
		Status:             req.Status,
		LotteryCampaignID:  req.LotteryCampaignID,
		TopN:               req.TopN,
		ChanceCount:        req.ChanceCount,
		PublicDisplayLimit: req.PublicDisplayLimit,
		MinActualCost:      req.MinActualCost,
		StartsAt:           req.StartsAt,
		EndsAt:             endsAt,
		Timezone:           req.Timezone,
		LastRunDate:        lastRunDate,
		Metadata:           req.Metadata,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, item)
}

func (h *RankingRewardHandler) ListExclusions(c *gin.Context) {
	campaignID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid campaign ID")
		return
	}
	page, pageSize := response.ParsePagination(c)
	items, result, err := h.rankingRewardService.ListExclusions(c.Request.Context(), campaignID, pagination.PaginationParams{Page: page, PageSize: pageSize})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Paginated(c, items, result.Total, page, pageSize)
}

func (h *RankingRewardHandler) CreateExclusion(c *gin.Context) {
	campaignID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid campaign ID")
		return
	}
	var req CreateRankingRewardExclusionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	executeAdminIdempotentJSON(c, "admin.ranking_reward.exclusions.create", req, service.DefaultWriteIdempotencyTTL(), func(ctx context.Context) (any, error) {
		return h.rankingRewardService.CreateExclusion(ctx, &service.CreateRankingRewardExclusionInput{
			CampaignID: campaignID,
			UserID:     req.UserID,
			Reason:     req.Reason,
		})
	})
}

func (h *RankingRewardHandler) UpdateExclusion(c *gin.Context) {
	exclusionID, err := strconv.ParseInt(c.Param("exclusion_id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid exclusion ID")
		return
	}
	var req UpdateRankingRewardExclusionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	item, err := h.rankingRewardService.UpdateExclusion(c.Request.Context(), exclusionID, &service.UpdateRankingRewardExclusionInput{Reason: req.Reason})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, item)
}

func (h *RankingRewardHandler) DeleteExclusion(c *gin.Context) {
	exclusionID, err := strconv.ParseInt(c.Param("exclusion_id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid exclusion ID")
		return
	}
	if err := h.rankingRewardService.DeleteExclusion(c.Request.Context(), exclusionID); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"deleted": true})
}

func (h *RankingRewardHandler) ListRuns(c *gin.Context) {
	campaignID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid campaign ID")
		return
	}
	page, pageSize := response.ParsePagination(c)
	items, result, err := h.rankingRewardService.ListRuns(c.Request.Context(), campaignID, pagination.PaginationParams{Page: page, PageSize: pageSize})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Paginated(c, items, result.Total, page, pageSize)
}

func (h *RankingRewardHandler) ListAwards(c *gin.Context) {
	runID, err := strconv.ParseInt(c.Param("run_id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid run ID")
		return
	}
	page, pageSize := response.ParsePagination(c)
	items, result, err := h.rankingRewardService.ListAwards(c.Request.Context(), runID, pagination.PaginationParams{Page: page, PageSize: pageSize})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Paginated(c, items, result.Total, page, pageSize)
}

func (h *RankingRewardHandler) RunCampaign(c *gin.Context) {
	campaignID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid campaign ID")
		return
	}
	var req RunRankingRewardCampaignRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	idempotencyPayload := struct {
		CampaignID int64                           `json:"campaign_id"`
		Request    RunRankingRewardCampaignRequest `json:"request"`
	}{CampaignID: campaignID, Request: req}
	executeAdminIdempotentJSON(c, "admin.ranking_reward.campaigns.run", idempotencyPayload, service.DefaultWriteIdempotencyTTL(), func(ctx context.Context) (any, error) {
		return h.rankingRewardService.RunCampaign(ctx, &service.RankingRewardRunInput{
			CampaignID: campaignID,
			RewardDate: req.RewardDate,
		})
	})
}
