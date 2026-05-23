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

type LotteryHandler struct {
	lotteryService *service.LotteryService
}

func NewLotteryHandler(lotteryService *service.LotteryService) *LotteryHandler {
	return &LotteryHandler{lotteryService: lotteryService}
}

type CreateLotteryCampaignRequest struct {
	Name                string         `json:"name" binding:"required"`
	Description         string         `json:"description"`
	Status              string         `json:"status" binding:"omitempty,oneof=draft active disabled ended"`
	StartsAt            *time.Time     `json:"starts_at"`
	EndsAt              *time.Time     `json:"ends_at"`
	ChanceExpiresInDays int            `json:"chance_expires_in_days" binding:"omitempty,min=1,max=3650"`
	Metadata            map[string]any `json:"metadata,omitempty"`
}

type UpdateLotteryCampaignRequest struct {
	Name                *string        `json:"name"`
	Description         *string        `json:"description"`
	Status              *string        `json:"status" binding:"omitempty,oneof=draft active disabled ended"`
	StartsAt            *time.Time     `json:"starts_at"`
	EndsAt              *time.Time     `json:"ends_at"`
	ClearEndsAt         bool           `json:"clear_ends_at"`
	ChanceExpiresInDays *int           `json:"chance_expires_in_days" binding:"omitempty,min=1,max=3650"`
	Metadata            map[string]any `json:"metadata,omitempty"`
}

type CreateLotteryPrizeRequest struct {
	Name               string         `json:"name" binding:"required"`
	Description        string         `json:"description"`
	Status             string         `json:"status" binding:"omitempty,oneof=active disabled"`
	Weight             int            `json:"weight" binding:"min=0"`
	StockTotal         int            `json:"stock_total" binding:"min=0"`
	RedeemType         string         `json:"redeem_type" binding:"required,oneof=balance concurrency subscription invitation timed_quota random_timed_quota"`
	RedeemValue        float64        `json:"redeem_value"`
	RedeemGroupID      *int64         `json:"redeem_group_id"`
	RedeemValidityDays int            `json:"redeem_validity_days" binding:"omitempty,min=1,max=3650"`
	RedeemMetadata     map[string]any `json:"redeem_metadata,omitempty"`
	SortOrder          int            `json:"sort_order"`
	Metadata           map[string]any `json:"metadata,omitempty"`
}

type UpdateLotteryPrizeRequest struct {
	Name               *string        `json:"name"`
	Description        *string        `json:"description"`
	Status             *string        `json:"status" binding:"omitempty,oneof=active disabled"`
	Weight             *int           `json:"weight" binding:"omitempty,min=0"`
	StockTotal         *int           `json:"stock_total" binding:"omitempty,min=0"`
	RedeemType         *string        `json:"redeem_type" binding:"omitempty,oneof=balance concurrency subscription invitation timed_quota random_timed_quota"`
	RedeemValue        *float64       `json:"redeem_value"`
	RedeemGroupID      *int64         `json:"redeem_group_id"`
	ClearRedeemGroupID bool           `json:"clear_redeem_group_id"`
	RedeemValidityDays *int           `json:"redeem_validity_days" binding:"omitempty,min=1,max=3650"`
	RedeemMetadata     map[string]any `json:"redeem_metadata,omitempty"`
	SortOrder          *int           `json:"sort_order"`
	Metadata           map[string]any `json:"metadata,omitempty"`
}

type GrantLotteryChancesRequest struct {
	UserID    int64          `json:"user_id" binding:"required,gt=0"`
	Count     int            `json:"count" binding:"omitempty,min=1,max=1000"`
	Source    string         `json:"source"`
	SourceID  string         `json:"source_id"`
	ExpiresAt *time.Time     `json:"expires_at"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

func (h *LotteryHandler) ListCampaigns(c *gin.Context) {
	page, pageSize := response.ParsePagination(c)
	status := c.Query("status")
	items, result, err := h.lotteryService.ListCampaigns(c.Request.Context(), pagination.PaginationParams{Page: page, PageSize: pageSize}, status, false)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Paginated(c, items, result.Total, page, pageSize)
}

func (h *LotteryHandler) GetCampaign(c *gin.Context) {
	campaignID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid campaign ID")
		return
	}
	item, err := h.lotteryService.GetCampaign(c.Request.Context(), campaignID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, item)
}

func (h *LotteryHandler) CreateCampaign(c *gin.Context) {
	var req CreateLotteryCampaignRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	startsAt := time.Now().UTC()
	if req.StartsAt != nil {
		startsAt = req.StartsAt.UTC()
	}
	executeAdminIdempotentJSON(c, "admin.lottery.campaigns.create", req, service.DefaultWriteIdempotencyTTL(), func(ctx context.Context) (any, error) {
		return h.lotteryService.CreateCampaign(ctx, &service.CreateLotteryCampaignInput{
			Name:                req.Name,
			Description:         req.Description,
			Status:              req.Status,
			StartsAt:            startsAt,
			EndsAt:              req.EndsAt,
			ChanceExpiresInDays: req.ChanceExpiresInDays,
			Metadata:            req.Metadata,
		})
	})
}

func (h *LotteryHandler) UpdateCampaign(c *gin.Context) {
	campaignID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid campaign ID")
		return
	}
	var req UpdateLotteryCampaignRequest
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
	item, err := h.lotteryService.UpdateCampaign(c.Request.Context(), campaignID, &service.UpdateLotteryCampaignInput{
		Name:                req.Name,
		Description:         req.Description,
		Status:              req.Status,
		StartsAt:            req.StartsAt,
		EndsAt:              endsAt,
		ChanceExpiresInDays: req.ChanceExpiresInDays,
		Metadata:            req.Metadata,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, item)
}

func (h *LotteryHandler) ListPrizes(c *gin.Context) {
	campaignID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid campaign ID")
		return
	}
	items, err := h.lotteryService.ListPrizes(c.Request.Context(), campaignID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, items)
}

func (h *LotteryHandler) CreatePrize(c *gin.Context) {
	campaignID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid campaign ID")
		return
	}
	var req CreateLotteryPrizeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	executeAdminIdempotentJSON(c, "admin.lottery.prizes.create", req, service.DefaultWriteIdempotencyTTL(), func(ctx context.Context) (any, error) {
		return h.lotteryService.CreatePrize(ctx, &service.CreateLotteryPrizeInput{
			CampaignID:         campaignID,
			Name:               req.Name,
			Description:        req.Description,
			Status:             req.Status,
			Weight:             req.Weight,
			StockTotal:         req.StockTotal,
			RedeemType:         req.RedeemType,
			RedeemValue:        req.RedeemValue,
			RedeemGroupID:      req.RedeemGroupID,
			RedeemValidityDays: req.RedeemValidityDays,
			RedeemMetadata:     req.RedeemMetadata,
			SortOrder:          req.SortOrder,
			Metadata:           req.Metadata,
		})
	})
}

func (h *LotteryHandler) UpdatePrize(c *gin.Context) {
	prizeID, err := strconv.ParseInt(c.Param("prize_id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid prize ID")
		return
	}
	var req UpdateLotteryPrizeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	redeemGroupID := service.NullableInt64Update{}
	if req.ClearRedeemGroupID {
		redeemGroupID.Set = true
	} else if req.RedeemGroupID != nil {
		redeemGroupID.Set = true
		redeemGroupID.Value = req.RedeemGroupID
	}
	item, err := h.lotteryService.UpdatePrize(c.Request.Context(), prizeID, &service.UpdateLotteryPrizeInput{
		Name:               req.Name,
		Description:        req.Description,
		Status:             req.Status,
		Weight:             req.Weight,
		StockTotal:         req.StockTotal,
		RedeemType:         req.RedeemType,
		RedeemValue:        req.RedeemValue,
		RedeemGroupID:      redeemGroupID,
		RedeemValidityDays: req.RedeemValidityDays,
		RedeemMetadata:     req.RedeemMetadata,
		SortOrder:          req.SortOrder,
		Metadata:           req.Metadata,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, item)
}

func (h *LotteryHandler) GrantChances(c *gin.Context) {
	campaignID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid campaign ID")
		return
	}
	var req GrantLotteryChancesRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	executeAdminIdempotentJSON(c, "admin.lottery.chances.grant", req, service.DefaultWriteIdempotencyTTL(), func(ctx context.Context) (any, error) {
		return h.lotteryService.GrantChances(ctx, &service.GrantLotteryChanceInput{
			CampaignID: campaignID,
			UserID:     req.UserID,
			Count:      req.Count,
			Source:     req.Source,
			SourceID:   req.SourceID,
			ExpiresAt:  req.ExpiresAt,
			Metadata:   req.Metadata,
		})
	})
}
