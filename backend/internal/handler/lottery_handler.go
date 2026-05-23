package handler

import (
	"context"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

type LotteryHandler struct {
	lotteryService *service.LotteryService
	settingService *service.SettingService
}

func NewLotteryHandler(lotteryService *service.LotteryService, settingService *service.SettingService) *LotteryHandler {
	return &LotteryHandler{lotteryService: lotteryService, settingService: settingService}
}

func (h *LotteryHandler) ListCampaigns(c *gin.Context) {
	if !h.ensureLotteryEnabled(c) {
		return
	}
	page, pageSize := response.ParsePagination(c)
	items, result, err := h.lotteryService.ListCampaigns(c.Request.Context(), pagination.PaginationParams{Page: page, PageSize: pageSize}, "", true)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Paginated(c, items, result.Total, page, pageSize)
}

func (h *LotteryHandler) GetCampaign(c *gin.Context) {
	if !h.ensureLotteryEnabled(c) {
		return
	}
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

func (h *LotteryHandler) ListChances(c *gin.Context) {
	if !h.ensureLotteryEnabled(c) {
		return
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}
	campaignID, err := parseOptionalInt64Query(c, "campaign_id")
	if err != nil {
		response.BadRequest(c, "Invalid campaign ID")
		return
	}
	page, pageSize := response.ParsePagination(c)
	items, result, err := h.lotteryService.ListUserChances(c.Request.Context(), subject.UserID, campaignID, pagination.PaginationParams{Page: page, PageSize: pageSize})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Paginated(c, items, result.Total, page, pageSize)
}

func (h *LotteryHandler) ListDraws(c *gin.Context) {
	if !h.ensureLotteryEnabled(c) {
		return
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}
	campaignID, err := parseOptionalInt64Query(c, "campaign_id")
	if err != nil {
		response.BadRequest(c, "Invalid campaign ID")
		return
	}
	page, pageSize := response.ParsePagination(c)
	items, result, err := h.lotteryService.ListUserDraws(c.Request.Context(), subject.UserID, campaignID, pagination.PaginationParams{Page: page, PageSize: pageSize})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Paginated(c, items, result.Total, page, pageSize)
}

func (h *LotteryHandler) Draw(c *gin.Context) {
	if !h.ensureLotteryEnabled(c) {
		return
	}
	subject, ok := middleware2.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}
	var req struct {
		CampaignID int64 `json:"campaign_id" binding:"required,gt=0"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	executeUserIdempotentJSON(c, "user.lottery.draw", req, service.DefaultWriteIdempotencyTTL(), func(ctx context.Context) (any, error) {
		return h.lotteryService.Draw(ctx, &service.LotteryDrawInput{CampaignID: req.CampaignID, UserID: subject.UserID})
	})
}

func (h *LotteryHandler) ensureLotteryEnabled(c *gin.Context) bool {
	settings, err := h.settingService.GetPublicSettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return false
	}
	if !settings.LotteryEnabled {
		response.Forbidden(c, "Lottery is disabled")
		return false
	}
	return true
}

func parseOptionalInt64Query(c *gin.Context, key string) (int64, error) {
	raw := c.Query(key)
	if raw == "" {
		return 0, nil
	}
	return strconv.ParseInt(raw, 10, 64)
}
