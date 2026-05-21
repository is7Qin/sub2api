package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/robfig/cron/v3"
)

const (
	RankingRewardCampaignStatusDraft    = "draft"
	RankingRewardCampaignStatusActive   = "active"
	RankingRewardCampaignStatusDisabled = "disabled"
	RankingRewardCampaignStatusEnded    = "ended"

	RankingRewardRunStatusRunning   = "running"
	RankingRewardRunStatusCompleted = "completed"
	RankingRewardRunStatusFailed    = "failed"

	RankingRewardDefaultTimezone = "Asia/Shanghai"
)

var (
	ErrRankingRewardCampaignNotFound  = infraerrors.NotFound("RANKING_REWARD_CAMPAIGN_NOT_FOUND", "ranking reward campaign not found")
	ErrRankingRewardExclusionNotFound = infraerrors.NotFound("RANKING_REWARD_EXCLUSION_NOT_FOUND", "ranking reward exclusion not found")
	ErrRankingRewardRunNotFound       = infraerrors.NotFound("RANKING_REWARD_RUN_NOT_FOUND", "ranking reward run not found")
	ErrRankingRewardInvalidPayload    = infraerrors.BadRequest("RANKING_REWARD_INVALID_PAYLOAD", "invalid ranking reward payload")
	ErrRankingRewardRunExists         = infraerrors.Conflict("RANKING_REWARD_RUN_EXISTS", "ranking reward run already exists")
	ErrRankingRewardCampaignInactive  = infraerrors.BadRequest("RANKING_REWARD_CAMPAIGN_INACTIVE", "ranking reward campaign is not active")
	ErrRankingRewardWindowUnavailable = infraerrors.BadRequest("RANKING_REWARD_WINDOW_UNAVAILABLE", "ranking reward window is unavailable")
)

type RankingRewardCampaign struct {
	ID                int64          `json:"id"`
	Name              string         `json:"name"`
	Description       string         `json:"description"`
	Status            string         `json:"status"`
	LotteryCampaignID int64          `json:"lottery_campaign_id"`
	TopN              int            `json:"top_n"`
	ChanceCount       int            `json:"chance_count"`
	MinActualCost     float64        `json:"min_actual_cost"`
	StartsAt          time.Time      `json:"starts_at"`
	EndsAt            *time.Time     `json:"ends_at,omitempty"`
	Timezone          string         `json:"timezone"`
	LastRunDate       *time.Time     `json:"last_run_date,omitempty"`
	Metadata          map[string]any `json:"metadata,omitempty"`
	CreatedAt         time.Time      `json:"created_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
}

type RankingRewardExcludedUser struct {
	ID         int64     `json:"id"`
	CampaignID int64     `json:"campaign_id"`
	UserID     int64     `json:"user_id"`
	Reason     string    `json:"reason"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type RankingRewardRun struct {
	ID              int64          `json:"id"`
	CampaignID      int64          `json:"campaign_id"`
	RewardDate      time.Time      `json:"reward_date"`
	WindowStart     time.Time      `json:"window_start"`
	WindowEnd       time.Time      `json:"window_end"`
	Status          string         `json:"status"`
	AwardedCount    int            `json:"awarded_count"`
	TotalActualCost float64        `json:"total_actual_cost"`
	ErrorMessage    string         `json:"error_message,omitempty"`
	Metadata        map[string]any `json:"metadata,omitempty"`
	StartedAt       time.Time      `json:"started_at"`
	FinishedAt      *time.Time     `json:"finished_at,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
}

type RankingRewardAward struct {
	ID                int64          `json:"id"`
	RunID             int64          `json:"run_id"`
	CampaignID        int64          `json:"campaign_id"`
	LotteryCampaignID int64          `json:"lottery_campaign_id"`
	UserID            int64          `json:"user_id"`
	Rank              int            `json:"rank"`
	ActualCost        float64        `json:"actual_cost"`
	Requests          int64          `json:"requests"`
	Tokens            int64          `json:"tokens"`
	ChanceCount       int            `json:"chance_count"`
	LotteryChanceIDs  []int64        `json:"lottery_chance_ids"`
	Metadata          map[string]any `json:"metadata,omitempty"`
	CreatedAt         time.Time      `json:"created_at"`
}

type RankingRewardCandidate struct {
	UserID     int64   `json:"user_id"`
	Email      string  `json:"email"`
	Rank       int     `json:"rank"`
	ActualCost float64 `json:"actual_cost"`
	Requests   int64   `json:"requests"`
	Tokens     int64   `json:"tokens"`
}

type RankingRewardRunResult struct {
	Run    *RankingRewardRun        `json:"run"`
	Awards []RankingRewardAward     `json:"awards"`
	Ranks  []RankingRewardCandidate `json:"ranks,omitempty"`
}

type CreateRankingRewardCampaignInput struct {
	Name              string
	Description       string
	Status            string
	LotteryCampaignID int64
	TopN              int
	ChanceCount       int
	MinActualCost     float64
	StartsAt          time.Time
	EndsAt            *time.Time
	Timezone          string
	Metadata          map[string]any
}

type UpdateRankingRewardCampaignInput struct {
	Name              *string
	Description       *string
	Status            *string
	LotteryCampaignID *int64
	TopN              *int
	ChanceCount       *int
	MinActualCost     *float64
	StartsAt          *time.Time
	EndsAt            NullableTimeUpdate
	Timezone          *string
	LastRunDate       NullableTimeUpdate
	Metadata          map[string]any
}

type CreateRankingRewardExclusionInput struct {
	CampaignID int64
	UserID     int64
	Reason     string
}

type UpdateRankingRewardExclusionInput struct {
	Reason *string
}

type RankingRewardRunInput struct {
	CampaignID int64
	RewardDate *time.Time
}

type RankingRewardRepository interface {
	CreateCampaign(ctx context.Context, input *CreateRankingRewardCampaignInput) (*RankingRewardCampaign, error)
	UpdateCampaign(ctx context.Context, id int64, input *UpdateRankingRewardCampaignInput) (*RankingRewardCampaign, error)
	GetCampaign(ctx context.Context, id int64) (*RankingRewardCampaign, error)
	ListCampaigns(ctx context.Context, params pagination.PaginationParams, status string) ([]RankingRewardCampaign, *pagination.PaginationResult, error)
	ListActiveCampaigns(ctx context.Context, now time.Time) ([]RankingRewardCampaign, error)
	CreateExclusion(ctx context.Context, input *CreateRankingRewardExclusionInput) (*RankingRewardExcludedUser, error)
	UpdateExclusion(ctx context.Context, id int64, input *UpdateRankingRewardExclusionInput) (*RankingRewardExcludedUser, error)
	DeleteExclusion(ctx context.Context, id int64) error
	ListExclusions(ctx context.Context, campaignID int64, params pagination.PaginationParams) ([]RankingRewardExcludedUser, *pagination.PaginationResult, error)
	ListRuns(ctx context.Context, campaignID int64, params pagination.PaginationParams) ([]RankingRewardRun, *pagination.PaginationResult, error)
	ListAwards(ctx context.Context, runID int64, params pagination.PaginationParams) ([]RankingRewardAward, *pagination.PaginationResult, error)
	CreateRun(ctx context.Context, campaign *RankingRewardCampaign, rewardDate, windowStart, windowEnd time.Time) (*RankingRewardRun, error)
	CompleteRun(ctx context.Context, runID int64, awardedCount int, totalActualCost float64, metadata map[string]any) (*RankingRewardRun, error)
	FailRun(ctx context.Context, runID int64, errMessage string) error
	FindRankCandidates(ctx context.Context, campaignID int64, windowStart, windowEnd time.Time, limit int, minActualCost float64) ([]RankingRewardCandidate, error)
	CreateAward(ctx context.Context, award *RankingRewardAward) (*RankingRewardAward, error)
}

type RankingRewardService struct {
	repo           RankingRewardRepository
	lotteryService *LotteryService
	cfg            *config.Config
	cron           *cron.Cron
	startOnce      sync.Once
	stopOnce       sync.Once
}

func NewRankingRewardService(repo RankingRewardRepository, lotteryService *LotteryService, cfg *config.Config) *RankingRewardService {
	return &RankingRewardService{repo: repo, lotteryService: lotteryService, cfg: cfg}
}

func (s *RankingRewardService) Start() {
	if s == nil {
		return
	}
	s.startOnce.Do(func() {
		loc := time.Local
		if s.cfg != nil && strings.TrimSpace(s.cfg.Timezone) != "" {
			if parsed, err := time.LoadLocation(strings.TrimSpace(s.cfg.Timezone)); err == nil && parsed != nil {
				loc = parsed
			}
		}
		c := cron.New(cron.WithLocation(loc))
		if _, err := c.AddFunc("*/5 * * * *", s.runDueCampaigns); err != nil {
			logger.LegacyPrintf("service.ranking_reward", "[RankingReward] not started (invalid schedule): %v", err)
			return
		}
		s.cron = c
		s.cron.Start()
		logger.LegacyPrintf("service.ranking_reward", "[RankingReward] started (schedule=*/5 * * * *)")
	})
}

func (s *RankingRewardService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		if s.cron != nil {
			ctx := s.cron.Stop()
			select {
			case <-ctx.Done():
			case <-time.After(3 * time.Second):
				logger.LegacyPrintf("service.ranking_reward", "[RankingReward] cron stop timed out")
			}
		}
	})
}

func (s *RankingRewardService) CreateCampaign(ctx context.Context, input *CreateRankingRewardCampaignInput) (*RankingRewardCampaign, error) {
	if err := validateCreateRankingRewardCampaign(input); err != nil {
		return nil, err
	}
	return s.repo.CreateCampaign(ctx, input)
}

func (s *RankingRewardService) UpdateCampaign(ctx context.Context, id int64, input *UpdateRankingRewardCampaignInput) (*RankingRewardCampaign, error) {
	if id <= 0 || input == nil {
		return nil, ErrRankingRewardInvalidPayload
	}
	if err := validateUpdateRankingRewardCampaign(input); err != nil {
		return nil, err
	}
	return s.repo.UpdateCampaign(ctx, id, input)
}

func (s *RankingRewardService) GetCampaign(ctx context.Context, id int64) (*RankingRewardCampaign, error) {
	if id <= 0 {
		return nil, ErrRankingRewardCampaignNotFound
	}
	return s.repo.GetCampaign(ctx, id)
}

func (s *RankingRewardService) ListCampaigns(ctx context.Context, params pagination.PaginationParams, status string) ([]RankingRewardCampaign, *pagination.PaginationResult, error) {
	return s.repo.ListCampaigns(ctx, params, strings.TrimSpace(status))
}

func (s *RankingRewardService) CreateExclusion(ctx context.Context, input *CreateRankingRewardExclusionInput) (*RankingRewardExcludedUser, error) {
	if input == nil || input.CampaignID <= 0 || input.UserID <= 0 {
		return nil, ErrRankingRewardInvalidPayload
	}
	return s.repo.CreateExclusion(ctx, input)
}

func (s *RankingRewardService) UpdateExclusion(ctx context.Context, id int64, input *UpdateRankingRewardExclusionInput) (*RankingRewardExcludedUser, error) {
	if id <= 0 || input == nil {
		return nil, ErrRankingRewardInvalidPayload
	}
	return s.repo.UpdateExclusion(ctx, id, input)
}

func (s *RankingRewardService) DeleteExclusion(ctx context.Context, id int64) error {
	if id <= 0 {
		return ErrRankingRewardExclusionNotFound
	}
	return s.repo.DeleteExclusion(ctx, id)
}

func (s *RankingRewardService) ListExclusions(ctx context.Context, campaignID int64, params pagination.PaginationParams) ([]RankingRewardExcludedUser, *pagination.PaginationResult, error) {
	if campaignID <= 0 {
		return nil, nil, ErrRankingRewardCampaignNotFound
	}
	return s.repo.ListExclusions(ctx, campaignID, params)
}

func (s *RankingRewardService) ListRuns(ctx context.Context, campaignID int64, params pagination.PaginationParams) ([]RankingRewardRun, *pagination.PaginationResult, error) {
	if campaignID <= 0 {
		return nil, nil, ErrRankingRewardCampaignNotFound
	}
	return s.repo.ListRuns(ctx, campaignID, params)
}

func (s *RankingRewardService) ListAwards(ctx context.Context, runID int64, params pagination.PaginationParams) ([]RankingRewardAward, *pagination.PaginationResult, error) {
	if runID <= 0 {
		return nil, nil, ErrRankingRewardRunNotFound
	}
	return s.repo.ListAwards(ctx, runID, params)
}

func (s *RankingRewardService) RunCampaign(ctx context.Context, input *RankingRewardRunInput) (*RankingRewardRunResult, error) {
	if s == nil || s.repo == nil || s.lotteryService == nil || input == nil || input.CampaignID <= 0 {
		return nil, ErrRankingRewardInvalidPayload
	}
	campaign, err := s.repo.GetCampaign(ctx, input.CampaignID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if !campaign.IsActiveAt(now) {
		return nil, ErrRankingRewardCampaignInactive
	}
	rewardDate, windowStart, windowEnd, err := resolveRankingRewardWindow(campaign, now, input.RewardDate)
	if err != nil {
		return nil, err
	}
	windowStart, windowEnd, ok := campaign.ClampWindow(windowStart, windowEnd)
	if !ok {
		return nil, ErrRankingRewardWindowUnavailable
	}
	return s.runCampaignWindow(ctx, campaign, rewardDate, windowStart, windowEnd)
}

func (s *RankingRewardService) runDueCampaigns() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	now := time.Now().UTC()
	campaigns, err := s.repo.ListActiveCampaigns(ctx, now)
	if err != nil {
		logger.LegacyPrintf("service.ranking_reward", "[RankingReward] list active campaigns error: %v", err)
		return
	}
	for i := range campaigns {
		campaign := campaigns[i]
		rewardDate, windowStart, windowEnd, err := resolveRankingRewardWindow(&campaign, now, nil)
		if err != nil {
			logger.LegacyPrintf("service.ranking_reward", "[RankingReward] resolve campaign %d window error: %v", campaign.ID, err)
			continue
		}
		if campaign.LastRunDate != nil && sameRewardDate(*campaign.LastRunDate, rewardDate) {
			continue
		}
		windowStart, windowEnd, ok := campaign.ClampWindow(windowStart, windowEnd)
		if !ok {
			continue
		}
		if !windowEnd.Before(now) && !windowEnd.Equal(now) {
			continue
		}
		if _, err := s.runCampaignWindow(ctx, &campaign, rewardDate, windowStart, windowEnd); err != nil {
			if infraerrors.IsConflict(err) {
				continue
			}
			logger.LegacyPrintf("service.ranking_reward", "[RankingReward] run campaign %d error: %v", campaign.ID, err)
		}
	}
}

func (s *RankingRewardService) runCampaignWindow(ctx context.Context, campaign *RankingRewardCampaign, rewardDate, windowStart, windowEnd time.Time) (*RankingRewardRunResult, error) {
	run, err := s.repo.CreateRun(ctx, campaign, rewardDate, windowStart, windowEnd)
	if err != nil {
		return nil, err
	}
	candidates, err := s.repo.FindRankCandidates(ctx, campaign.ID, windowStart, windowEnd, campaign.TopN, campaign.MinActualCost)
	if err != nil {
		_ = s.repo.FailRun(ctx, run.ID, err.Error())
		return nil, err
	}
	if len(candidates) == 0 {
		completed, err := s.repo.CompleteRun(ctx, run.ID, 0, 0, map[string]any{"reason": "no_eligible_users"})
		if err != nil {
			return nil, err
		}
		return &RankingRewardRunResult{Run: completed, Awards: []RankingRewardAward{}, Ranks: candidates}, nil
	}

	awards := make([]RankingRewardAward, 0, len(candidates))
	totalActualCost := 0.0
	for _, candidate := range candidates {
		chances, err := s.lotteryService.GrantChances(ctx, &GrantLotteryChanceInput{
			CampaignID: campaign.LotteryCampaignID,
			UserID:     candidate.UserID,
			Count:      campaign.ChanceCount,
			Source:     LotteryChanceSourceRanking,
			SourceID:   fmt.Sprintf("ranking:%d:%s:%d", campaign.ID, rewardDate.Format("2006-01-02"), candidate.UserID),
			Metadata: map[string]any{
				"ranking_reward_run_id":      run.ID,
				"ranking_reward_campaign_id": campaign.ID,
				"reward_date":                rewardDate.Format("2006-01-02"),
				"rank":                       candidate.Rank,
			},
		})
		if err != nil {
			_ = s.repo.FailRun(ctx, run.ID, err.Error())
			return nil, err
		}
		chanceIDs := make([]int64, 0, len(chances))
		for _, chance := range chances {
			chanceIDs = append(chanceIDs, chance.ID)
		}
		award, err := s.repo.CreateAward(ctx, &RankingRewardAward{
			RunID:             run.ID,
			CampaignID:        campaign.ID,
			LotteryCampaignID: campaign.LotteryCampaignID,
			UserID:            candidate.UserID,
			Rank:              candidate.Rank,
			ActualCost:        candidate.ActualCost,
			Requests:          candidate.Requests,
			Tokens:            candidate.Tokens,
			ChanceCount:       campaign.ChanceCount,
			LotteryChanceIDs:  chanceIDs,
			Metadata: map[string]any{
				"email": candidate.Email,
			},
		})
		if err != nil {
			_ = s.repo.FailRun(ctx, run.ID, err.Error())
			return nil, err
		}
		awards = append(awards, *award)
		totalActualCost += candidate.ActualCost
	}
	completed, err := s.repo.CompleteRun(ctx, run.ID, len(awards), totalActualCost, map[string]any{"reward_date": rewardDate.Format("2006-01-02")})
	if err != nil {
		return nil, err
	}
	return &RankingRewardRunResult{Run: completed, Awards: awards, Ranks: candidates}, nil
}

func (c *RankingRewardCampaign) IsActiveAt(at time.Time) bool {
	if c == nil || c.Status != RankingRewardCampaignStatusActive {
		return false
	}
	at = at.UTC()
	if at.Before(c.StartsAt.UTC()) {
		return false
	}
	return c.EndsAt == nil || at.Before(c.EndsAt.UTC())
}

func (c *RankingRewardCampaign) ClampWindow(windowStart, windowEnd time.Time) (time.Time, time.Time, bool) {
	if c == nil {
		return time.Time{}, time.Time{}, false
	}
	start := windowStart.UTC()
	end := windowEnd.UTC()
	if c.StartsAt.UTC().After(start) {
		start = c.StartsAt.UTC()
	}
	if c.EndsAt != nil && c.EndsAt.UTC().Before(end) {
		end = c.EndsAt.UTC()
	}
	return start, end, end.After(start)
}

func validateCreateRankingRewardCampaign(input *CreateRankingRewardCampaignInput) error {
	if input == nil {
		return ErrRankingRewardInvalidPayload
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || input.LotteryCampaignID <= 0 || input.TopN <= 0 || input.ChanceCount <= 0 || input.MinActualCost < 0 {
		return ErrRankingRewardInvalidPayload
	}
	if input.Status == "" {
		input.Status = RankingRewardCampaignStatusDraft
	}
	if !isValidRankingRewardCampaignStatus(input.Status) {
		return ErrRankingRewardInvalidPayload
	}
	if input.StartsAt.IsZero() {
		input.StartsAt = time.Now().UTC()
	} else {
		input.StartsAt = input.StartsAt.UTC()
	}
	if input.EndsAt != nil {
		v := input.EndsAt.UTC()
		input.EndsAt = &v
		if !v.After(input.StartsAt) {
			return ErrRankingRewardInvalidPayload
		}
	}
	if strings.TrimSpace(input.Timezone) == "" {
		input.Timezone = RankingRewardDefaultTimezone
	}
	if _, err := time.LoadLocation(strings.TrimSpace(input.Timezone)); err != nil {
		return infraerrors.BadRequest("RANKING_REWARD_TIMEZONE_INVALID", "invalid ranking reward timezone")
	}
	input.Timezone = strings.TrimSpace(input.Timezone)
	return nil
}

func validateUpdateRankingRewardCampaign(input *UpdateRankingRewardCampaignInput) error {
	if input.Status != nil && !isValidRankingRewardCampaignStatus(*input.Status) {
		return ErrRankingRewardInvalidPayload
	}
	if input.LotteryCampaignID != nil && *input.LotteryCampaignID <= 0 {
		return ErrRankingRewardInvalidPayload
	}
	if input.TopN != nil && *input.TopN <= 0 {
		return ErrRankingRewardInvalidPayload
	}
	if input.ChanceCount != nil && *input.ChanceCount <= 0 {
		return ErrRankingRewardInvalidPayload
	}
	if input.MinActualCost != nil && *input.MinActualCost < 0 {
		return ErrRankingRewardInvalidPayload
	}
	if input.Timezone != nil {
		tz := strings.TrimSpace(*input.Timezone)
		if tz == "" {
			return ErrRankingRewardInvalidPayload
		}
		if _, err := time.LoadLocation(tz); err != nil {
			return infraerrors.BadRequest("RANKING_REWARD_TIMEZONE_INVALID", "invalid ranking reward timezone")
		}
		*input.Timezone = tz
	}
	return nil
}

func isValidRankingRewardCampaignStatus(status string) bool {
	switch status {
	case RankingRewardCampaignStatusDraft, RankingRewardCampaignStatusActive, RankingRewardCampaignStatusDisabled, RankingRewardCampaignStatusEnded:
		return true
	default:
		return false
	}
}

func resolveRankingRewardWindow(campaign *RankingRewardCampaign, now time.Time, rewardDate *time.Time) (time.Time, time.Time, time.Time, error) {
	if campaign == nil {
		return time.Time{}, time.Time{}, time.Time{}, ErrRankingRewardInvalidPayload
	}
	tz := strings.TrimSpace(campaign.Timezone)
	if tz == "" {
		tz = RankingRewardDefaultTimezone
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.Time{}, time.Time{}, time.Time{}, infraerrors.BadRequest("RANKING_REWARD_TIMEZONE_INVALID", "invalid ranking reward timezone")
	}
	localNow := now.In(loc)
	var localDate time.Time
	if rewardDate != nil {
		localDate = rewardDate.In(loc)
	} else {
		localDate = localNow.AddDate(0, 0, -1)
	}
	dayStart := time.Date(localDate.Year(), localDate.Month(), localDate.Day(), 0, 0, 0, 0, loc)
	dayEnd := dayStart.AddDate(0, 0, 1)
	return dayStart, dayStart.UTC(), dayEnd.UTC(), nil
}

func sameRewardDate(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}
