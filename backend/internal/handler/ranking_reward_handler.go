package handler

import (
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

type RankingRewardHandler struct {
	rankingRewardService *service.RankingRewardService
	settingService       *service.SettingService
}

func NewRankingRewardHandler(rankingRewardService *service.RankingRewardService, settingService *service.SettingService) *RankingRewardHandler {
	return &RankingRewardHandler{rankingRewardService: rankingRewardService, settingService: settingService}
}

func (h *RankingRewardHandler) ListLeaderboards(c *gin.Context) {
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		response.Unauthorized(c, "User not authenticated")
		return
	}
	if !h.ensureRankingRewardEnabled(c) {
		return
	}
	runLimit := 10
	if raw := c.Query("run_limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			response.BadRequest(c, "Invalid run_limit")
			return
		}
		runLimit = parsed
	} else if raw := c.Query("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			response.BadRequest(c, "Invalid limit")
			return
		}
		runLimit = parsed
	}
	items, err := h.rankingRewardService.ListPublicLeaderboards(c.Request.Context(), subject.UserID, runLimit)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, items)
}

func (h *RankingRewardHandler) ensureRankingRewardEnabled(c *gin.Context) bool {
	settings, err := h.settingService.GetPublicSettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return false
	}
	if !settings.RankingRewardEnabled {
		response.Forbidden(c, "Ranking reward is disabled")
		return false
	}
	return true
}
