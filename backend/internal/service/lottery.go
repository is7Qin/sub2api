package service

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
)

const (
	LotteryCampaignStatusDraft    = "draft"
	LotteryCampaignStatusActive   = "active"
	LotteryCampaignStatusDisabled = "disabled"
	LotteryCampaignStatusEnded    = "ended"

	LotteryPrizeStatusActive   = "active"
	LotteryPrizeStatusDisabled = "disabled"

	LotteryChanceStatusAvailable = "available"
	LotteryChanceStatusUsed      = "used"
	LotteryChanceStatusExpired   = "expired"

	LotteryDrawStatusPending    = "pending"
	LotteryDrawStatusProcessing = "processing"
	LotteryDrawStatusAwarded    = "awarded"
	LotteryDrawStatusFailed     = "failed"

	LotteryDrawProcessingLease = 30 * time.Minute

	LotteryChanceSourceRanking = "ranking"
	LotteryRedeemCodePrefix    = "LOTTERY"
)

var (
	ErrLotteryCampaignNotFound    = infraerrors.NotFound("LOTTERY_CAMPAIGN_NOT_FOUND", "lottery campaign not found")
	ErrLotteryPrizeNotFound       = infraerrors.NotFound("LOTTERY_PRIZE_NOT_FOUND", "lottery prize not found")
	ErrLotteryChanceUnavailable   = infraerrors.BadRequest("LOTTERY_CHANCE_UNAVAILABLE", "no available lottery chance")
	ErrLotteryCampaignInactive    = infraerrors.BadRequest("LOTTERY_CAMPAIGN_INACTIVE", "lottery campaign is not active")
	ErrLotteryPrizeUnavailable    = infraerrors.BadRequest("LOTTERY_PRIZE_UNAVAILABLE", "no available lottery prize")
	ErrLotteryInvalidPayload      = infraerrors.BadRequest("LOTTERY_INVALID_PAYLOAD", "invalid lottery payload")
	ErrLotteryRedeemUnavailable   = infraerrors.ServiceUnavailable("LOTTERY_REDEEM_UNAVAILABLE", "redeem service not available")
	ErrLotteryChanceGrantConflict = infraerrors.Conflict("LOTTERY_CHANCE_GRANT_CONFLICT", "lottery chance source already granted")
)

type LotteryCampaign struct {
	ID                  int64          `json:"id"`
	Name                string         `json:"name"`
	Description         string         `json:"description"`
	Status              string         `json:"status"`
	StartsAt            time.Time      `json:"starts_at"`
	EndsAt              *time.Time     `json:"ends_at,omitempty"`
	ChanceExpiresInDays int            `json:"chance_expires_in_days"`
	Metadata            map[string]any `json:"metadata,omitempty"`
	CreatedAt           time.Time      `json:"created_at"`
	UpdatedAt           time.Time      `json:"updated_at"`
	Prizes              []LotteryPrize `json:"prizes,omitempty"`
}

type LotteryPrize struct {
	ID                 int64          `json:"id"`
	CampaignID         int64          `json:"campaign_id"`
	Name               string         `json:"name"`
	Description        string         `json:"description"`
	Status             string         `json:"status"`
	Weight             int            `json:"weight"`
	StockTotal         int            `json:"stock_total"`
	StockUsed          int            `json:"stock_used"`
	RedeemType         string         `json:"redeem_type"`
	RedeemValue        float64        `json:"redeem_value"`
	RedeemGroupID      *int64         `json:"redeem_group_id,omitempty"`
	RedeemValidityDays int            `json:"redeem_validity_days"`
	RedeemMetadata     map[string]any `json:"redeem_metadata,omitempty"`
	SortOrder          int            `json:"sort_order"`
	Metadata           map[string]any `json:"metadata,omitempty"`
	CreatedAt          time.Time      `json:"created_at"`
	UpdatedAt          time.Time      `json:"updated_at"`
}

type LotteryChance struct {
	ID         int64          `json:"id"`
	CampaignID int64          `json:"campaign_id"`
	UserID     int64          `json:"user_id"`
	Source     string         `json:"source"`
	SourceID   string         `json:"source_id"`
	Status     string         `json:"status"`
	ExpiresAt  time.Time      `json:"expires_at"`
	UsedAt     *time.Time     `json:"used_at,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
}

type LotteryDraw struct {
	ID           int64          `json:"id"`
	CampaignID   int64          `json:"campaign_id"`
	UserID       int64          `json:"user_id"`
	ChanceID     int64          `json:"chance_id"`
	PrizeID      *int64         `json:"prize_id,omitempty"`
	RedeemCodeID *int64         `json:"redeem_code_id,omitempty"`
	RedeemCode   string         `json:"redeem_code"`
	Status       string         `json:"status"`
	ErrorMessage string         `json:"error_message,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
	DrawnAt      time.Time      `json:"drawn_at"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
	Prize        *LotteryPrize  `json:"prize,omitempty"`
}

type CreateLotteryCampaignInput struct {
	Name                string
	Description         string
	Status              string
	StartsAt            time.Time
	EndsAt              *time.Time
	ChanceExpiresInDays int
	Metadata            map[string]any
}

type UpdateLotteryCampaignInput struct {
	Name                *string
	Description         *string
	Status              *string
	StartsAt            *time.Time
	EndsAt              NullableTimeUpdate
	ChanceExpiresInDays *int
	Metadata            map[string]any
}

type CreateLotteryPrizeInput struct {
	CampaignID         int64
	Name               string
	Description        string
	Status             string
	Weight             int
	StockTotal         int
	RedeemType         string
	RedeemValue        float64
	RedeemGroupID      *int64
	RedeemValidityDays int
	RedeemMetadata     map[string]any
	SortOrder          int
	Metadata           map[string]any
}

type UpdateLotteryPrizeInput struct {
	Name               *string
	Description        *string
	Status             *string
	Weight             *int
	StockTotal         *int
	RedeemType         *string
	RedeemValue        *float64
	RedeemGroupID      NullableInt64Update
	RedeemValidityDays *int
	RedeemMetadata     map[string]any
	SortOrder          *int
	Metadata           map[string]any
}

type GrantLotteryChanceInput struct {
	CampaignID int64
	UserID     int64
	Count      int
	Source     string
	SourceID   string
	ExpiresAt  *time.Time
	Metadata   map[string]any
}

type LotteryDrawInput struct {
	CampaignID int64
	UserID     int64
}

type LotteryDrawResult struct {
	Draw       *LotteryDraw  `json:"draw"`
	Prize      *LotteryPrize `json:"prize"`
	RedeemCode *RedeemCode   `json:"redeem_code,omitempty"`
}

type LotteryRepository interface {
	CreateCampaign(ctx context.Context, input *CreateLotteryCampaignInput) (*LotteryCampaign, error)
	UpdateCampaign(ctx context.Context, id int64, input *UpdateLotteryCampaignInput) (*LotteryCampaign, error)
	GetCampaign(ctx context.Context, id int64) (*LotteryCampaign, error)
	ListCampaigns(ctx context.Context, params pagination.PaginationParams, status string, activeOnly bool) ([]LotteryCampaign, *pagination.PaginationResult, error)
	CreatePrize(ctx context.Context, input *CreateLotteryPrizeInput) (*LotteryPrize, error)
	UpdatePrize(ctx context.Context, id int64, input *UpdateLotteryPrizeInput) (*LotteryPrize, error)
	GetPrize(ctx context.Context, id int64) (*LotteryPrize, error)
	ListPrizes(ctx context.Context, campaignID int64) ([]LotteryPrize, error)
	GrantChances(ctx context.Context, input *GrantLotteryChanceInput, expiresAt time.Time) ([]LotteryChance, error)
	ListChancesBySource(ctx context.Context, input *GrantLotteryChanceInput) ([]LotteryChance, error)
	ListUserChances(ctx context.Context, userID int64, campaignID int64, params pagination.PaginationParams) ([]LotteryChance, *pagination.PaginationResult, error)
	ListUserDraws(ctx context.Context, userID int64, campaignID int64, params pagination.PaginationParams) ([]LotteryDraw, *pagination.PaginationResult, error)
	GetPendingUserDraw(ctx context.Context, campaignID int64, userID int64) (*LotteryDraw, *LotteryPrize, *RedeemCode, error)
	ClaimDrawRecovery(ctx context.Context, drawID int64, lease time.Duration) error
	PrepareDraw(ctx context.Context, input *LotteryDrawInput, selector func([]LotteryPrize) (*LotteryPrize, error)) (*LotteryDraw, *LotteryPrize, error)
	CompleteDraw(ctx context.Context, drawID int64, redeemCodeID int64, redeemCode string) (*LotteryDraw, error)
	FailDraw(ctx context.Context, drawID int64, errMessage string) error
}

type LotteryService struct {
	repo          LotteryRepository
	redeemService *RedeemService
}

func NewLotteryService(repo LotteryRepository, redeemService *RedeemService) *LotteryService {
	return &LotteryService{repo: repo, redeemService: redeemService}
}

func (s *LotteryService) CreateCampaign(ctx context.Context, input *CreateLotteryCampaignInput) (*LotteryCampaign, error) {
	if err := validateCreateLotteryCampaign(input); err != nil {
		return nil, err
	}
	return s.repo.CreateCampaign(ctx, input)
}

func (s *LotteryService) UpdateCampaign(ctx context.Context, id int64, input *UpdateLotteryCampaignInput) (*LotteryCampaign, error) {
	if id <= 0 || input == nil {
		return nil, ErrLotteryInvalidPayload
	}
	return s.repo.UpdateCampaign(ctx, id, input)
}

func (s *LotteryService) GetCampaign(ctx context.Context, id int64) (*LotteryCampaign, error) {
	if id <= 0 {
		return nil, ErrLotteryCampaignNotFound
	}
	return s.repo.GetCampaign(ctx, id)
}

func (s *LotteryService) ListCampaigns(ctx context.Context, params pagination.PaginationParams, status string, activeOnly bool) ([]LotteryCampaign, *pagination.PaginationResult, error) {
	return s.repo.ListCampaigns(ctx, params, strings.TrimSpace(status), activeOnly)
}

func (s *LotteryService) CreatePrize(ctx context.Context, input *CreateLotteryPrizeInput) (*LotteryPrize, error) {
	if err := validateCreateLotteryPrize(input); err != nil {
		return nil, err
	}
	return s.repo.CreatePrize(ctx, input)
}

func (s *LotteryService) UpdatePrize(ctx context.Context, id int64, input *UpdateLotteryPrizeInput) (*LotteryPrize, error) {
	if id <= 0 || input == nil {
		return nil, ErrLotteryInvalidPayload
	}
	current, err := s.repo.GetPrize(ctx, id)
	if err != nil {
		return nil, err
	}
	merged := CreateLotteryPrizeInput{
		CampaignID:         current.CampaignID,
		Name:               current.Name,
		Description:        current.Description,
		Status:             current.Status,
		Weight:             current.Weight,
		StockTotal:         current.StockTotal,
		RedeemType:         current.RedeemType,
		RedeemValue:        current.RedeemValue,
		RedeemGroupID:      current.RedeemGroupID,
		RedeemValidityDays: current.RedeemValidityDays,
		RedeemMetadata:     current.RedeemMetadata,
		SortOrder:          current.SortOrder,
		Metadata:           current.Metadata,
	}
	if input.Name != nil {
		merged.Name = *input.Name
	}
	if input.Description != nil {
		merged.Description = *input.Description
	}
	if input.Status != nil {
		merged.Status = *input.Status
	}
	if input.Weight != nil {
		merged.Weight = *input.Weight
	}
	if input.StockTotal != nil {
		merged.StockTotal = *input.StockTotal
	}
	if input.RedeemType != nil {
		merged.RedeemType = *input.RedeemType
	}
	if input.RedeemValue != nil {
		merged.RedeemValue = *input.RedeemValue
	}
	if input.RedeemGroupID.Set {
		merged.RedeemGroupID = input.RedeemGroupID.Value
	}
	if input.RedeemValidityDays != nil {
		merged.RedeemValidityDays = *input.RedeemValidityDays
	}
	if input.RedeemMetadata != nil {
		merged.RedeemMetadata = input.RedeemMetadata
	}
	if input.SortOrder != nil {
		merged.SortOrder = *input.SortOrder
	}
	if input.Metadata != nil {
		merged.Metadata = input.Metadata
	}
	if err := validateCreateLotteryPrize(&merged); err != nil {
		return nil, err
	}
	return s.repo.UpdatePrize(ctx, id, input)
}

func (s *LotteryService) ListPrizes(ctx context.Context, campaignID int64) ([]LotteryPrize, error) {
	if campaignID <= 0 {
		return nil, ErrLotteryCampaignNotFound
	}
	return s.repo.ListPrizes(ctx, campaignID)
}

func (s *LotteryService) GrantChances(ctx context.Context, input *GrantLotteryChanceInput) ([]LotteryChance, error) {
	if input == nil || input.CampaignID <= 0 || input.UserID <= 0 {
		return nil, ErrLotteryInvalidPayload
	}
	if input.Count <= 0 {
		input.Count = 1
	}
	if input.Count > 1000 {
		return nil, infraerrors.BadRequest("LOTTERY_CHANCE_COUNT_INVALID", "lottery chance count cannot exceed 1000")
	}
	campaign, err := s.repo.GetCampaign(ctx, input.CampaignID)
	if err != nil {
		return nil, err
	}
	if !campaign.IsActiveAt(time.Now().UTC()) {
		return nil, ErrLotteryCampaignInactive
	}
	expiresAt := time.Now().UTC().AddDate(0, 0, campaign.ChanceExpiresInDays)
	if input.ExpiresAt != nil {
		expiresAt = input.ExpiresAt.UTC()
	}
	if !expiresAt.After(time.Now().UTC()) {
		return nil, infraerrors.BadRequest("LOTTERY_CHANCE_EXPIRY_INVALID", "lottery chance expiry must be in the future")
	}
	return s.repo.GrantChances(ctx, input, expiresAt)
}

func (s *LotteryService) ListChancesBySource(ctx context.Context, input *GrantLotteryChanceInput) ([]LotteryChance, error) {
	if input == nil || input.CampaignID <= 0 || input.UserID <= 0 || strings.TrimSpace(input.Source) == "" || strings.TrimSpace(input.SourceID) == "" {
		return nil, ErrLotteryInvalidPayload
	}
	if input.Count <= 0 {
		input.Count = 1
	}
	return s.repo.ListChancesBySource(ctx, input)
}

func (s *LotteryService) ListUserChances(ctx context.Context, userID int64, campaignID int64, params pagination.PaginationParams) ([]LotteryChance, *pagination.PaginationResult, error) {
	if userID <= 0 {
		return nil, nil, ErrLotteryInvalidPayload
	}
	return s.repo.ListUserChances(ctx, userID, campaignID, params)
}

func (s *LotteryService) ListUserDraws(ctx context.Context, userID int64, campaignID int64, params pagination.PaginationParams) ([]LotteryDraw, *pagination.PaginationResult, error) {
	if userID <= 0 {
		return nil, nil, ErrLotteryInvalidPayload
	}
	return s.repo.ListUserDraws(ctx, userID, campaignID, params)
}

func (s *LotteryService) Draw(ctx context.Context, input *LotteryDrawInput) (*LotteryDrawResult, error) {
	if s == nil || s.repo == nil {
		return nil, ErrLotteryInvalidPayload
	}
	if s.redeemService == nil {
		return nil, ErrLotteryRedeemUnavailable
	}
	if input == nil || input.CampaignID <= 0 || input.UserID <= 0 {
		return nil, ErrLotteryInvalidPayload
	}
	if draw, prize, redeemCode, err := s.repo.GetPendingUserDraw(ctx, input.CampaignID, input.UserID); err != nil {
		return nil, err
	} else if draw != nil {
		return s.recoverPendingDraw(ctx, input.UserID, draw, prize, redeemCode)
	}
	draw, prize, err := s.repo.PrepareDraw(ctx, input, selectLotteryPrize)
	if err != nil {
		return nil, err
	}
	redeemCode, err := s.awardPrize(ctx, input.UserID, prize, draw.ID)
	if err != nil {
		_ = s.repo.FailDraw(ctx, draw.ID, err.Error())
		return nil, err
	}
	completedDraw, err := s.repo.CompleteDraw(ctx, draw.ID, redeemCode.ID, redeemCode.Code)
	if err != nil {
		return nil, err
	}
	return &LotteryDrawResult{Draw: completedDraw, Prize: prize, RedeemCode: redeemCode}, nil
}

func (s *LotteryService) recoverPendingDraw(ctx context.Context, userID int64, draw *LotteryDraw, prize *LotteryPrize, redeemCode *RedeemCode) (*LotteryDrawResult, error) {
	if draw == nil {
		return nil, ErrLotteryChanceUnavailable
	}
	if err := s.repo.ClaimDrawRecovery(ctx, draw.ID, LotteryDrawProcessingLease); err != nil {
		return nil, err
	}
	if redeemCode == nil {
		return s.completePendingDrawWithAward(ctx, userID, draw, prize)
	}
	if redeemCode.IsUsed() {
		if redeemCode.UsedBy == nil || *redeemCode.UsedBy != userID {
			_ = s.repo.FailDraw(ctx, draw.ID, "lottery redeem code used by another user")
			return nil, ErrLotteryChanceUnavailable
		}
		completedDraw, err := s.repo.CompleteDraw(ctx, draw.ID, redeemCode.ID, redeemCode.Code)
		if err != nil {
			return nil, err
		}
		return &LotteryDrawResult{Draw: completedDraw, Prize: prize, RedeemCode: redeemCode}, nil
	}
	if !redeemCode.CanUse() {
		_ = s.repo.FailDraw(ctx, draw.ID, "lottery redeem code is not usable")
		return nil, ErrLotteryChanceUnavailable
	}
	redeemed, err := s.redeemService.redeemLoadedCode(ctx, userID, redeemCode)
	if err != nil {
		_ = s.repo.FailDraw(ctx, draw.ID, err.Error())
		return nil, fmt.Errorf("redeem lottery prize: %w", err)
	}
	completedDraw, err := s.repo.CompleteDraw(ctx, draw.ID, redeemed.ID, redeemed.Code)
	if err != nil {
		return nil, err
	}
	return &LotteryDrawResult{Draw: completedDraw, Prize: prize, RedeemCode: redeemed}, nil
}

func (s *LotteryService) completePendingDrawWithAward(ctx context.Context, userID int64, draw *LotteryDraw, prize *LotteryPrize) (*LotteryDrawResult, error) {
	redeemCode, err := s.awardPrize(ctx, userID, prize, draw.ID)
	if err != nil {
		_ = s.repo.FailDraw(ctx, draw.ID, err.Error())
		return nil, err
	}
	completedDraw, err := s.repo.CompleteDraw(ctx, draw.ID, redeemCode.ID, redeemCode.Code)
	if err != nil {
		return nil, err
	}
	return &LotteryDrawResult{Draw: completedDraw, Prize: prize, RedeemCode: redeemCode}, nil
}

func (s *LotteryService) awardPrize(ctx context.Context, userID int64, prize *LotteryPrize, drawID int64) (*RedeemCode, error) {
	if prize == nil {
		return nil, ErrLotteryPrizeUnavailable
	}
	code, err := GenerateRedeemCode()
	if err != nil {
		return nil, fmt.Errorf("generate lottery redeem code: %w", err)
	}
	notes := fmt.Sprintf("lottery draw %d", drawID)
	metadata := map[string]any{
		"lottery_draw_id":     drawID,
		"lottery_prize_id":    prize.ID,
		"lottery_campaign_id": prize.CampaignID,
	}
	for k, v := range prize.RedeemMetadata {
		metadata[k] = v
	}
	redeemCode := &RedeemCode{
		Code:         code,
		Type:         prize.RedeemType,
		Value:        prize.RedeemValue,
		Status:       StatusUnused,
		Notes:        notes,
		GroupID:      prize.RedeemGroupID,
		ValidityDays: prize.RedeemValidityDays,
		Metadata:     metadata,
	}
	if err := s.redeemService.CreateCode(ctx, redeemCode); err != nil {
		return nil, fmt.Errorf("create lottery redeem code: %w", err)
	}
	redeemed, err := s.redeemService.redeemLoadedCode(ctx, userID, redeemCode)
	if err != nil {
		return nil, fmt.Errorf("redeem lottery prize: %w", err)
	}
	return redeemed, nil
}

func (c *LotteryCampaign) IsActiveAt(now time.Time) bool {
	if c == nil || c.Status != LotteryCampaignStatusActive {
		return false
	}
	if c.StartsAt.After(now) {
		return false
	}
	return c.EndsAt == nil || c.EndsAt.After(now)
}

func validateCreateLotteryCampaign(input *CreateLotteryCampaignInput) error {
	if input == nil {
		return ErrLotteryInvalidPayload
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" {
		return infraerrors.BadRequest("LOTTERY_CAMPAIGN_NAME_REQUIRED", "lottery campaign name is required")
	}
	if input.Status == "" {
		input.Status = LotteryCampaignStatusDraft
	}
	if input.ChanceExpiresInDays <= 0 {
		input.ChanceExpiresInDays = 1
	}
	if input.StartsAt.IsZero() {
		input.StartsAt = time.Now().UTC()
	}
	if input.EndsAt != nil && !input.EndsAt.After(input.StartsAt) {
		return infraerrors.BadRequest("LOTTERY_CAMPAIGN_TIME_INVALID", "lottery campaign ends_at must be after starts_at")
	}
	return validateLotteryCampaignStatus(input.Status)
}

func validateCreateLotteryPrize(input *CreateLotteryPrizeInput) error {
	if input == nil || input.CampaignID <= 0 {
		return ErrLotteryInvalidPayload
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" {
		return infraerrors.BadRequest("LOTTERY_PRIZE_NAME_REQUIRED", "lottery prize name is required")
	}
	if input.Status == "" {
		input.Status = LotteryPrizeStatusActive
	}
	if input.Weight < 0 || input.StockTotal < 0 {
		return ErrLotteryInvalidPayload
	}
	if input.RedeemValidityDays <= 0 {
		input.RedeemValidityDays = 30
	}
	code := &RedeemCode{
		Type:         input.RedeemType,
		Value:        input.RedeemValue,
		GroupID:      input.RedeemGroupID,
		ValidityDays: input.RedeemValidityDays,
		Metadata:     input.RedeemMetadata,
	}
	if err := validateRedeemCodePayload(code); err != nil {
		return err
	}
	return validateLotteryPrizeStatus(input.Status)
}

func validateLotteryCampaignStatus(status string) error {
	switch status {
	case LotteryCampaignStatusDraft, LotteryCampaignStatusActive, LotteryCampaignStatusDisabled, LotteryCampaignStatusEnded:
		return nil
	default:
		return infraerrors.BadRequest("LOTTERY_CAMPAIGN_STATUS_INVALID", "lottery campaign status is invalid")
	}
}

func validateLotteryPrizeStatus(status string) error {
	switch status {
	case LotteryPrizeStatusActive, LotteryPrizeStatusDisabled:
		return nil
	default:
		return infraerrors.BadRequest("LOTTERY_PRIZE_STATUS_INVALID", "lottery prize status is invalid")
	}
}

func selectLotteryPrize(prizes []LotteryPrize) (*LotteryPrize, error) {
	weighted := make([]LotteryPrize, 0, len(prizes))
	total := 0
	for _, prize := range prizes {
		if prize.Status != LotteryPrizeStatusActive || prize.Weight <= 0 {
			continue
		}
		if prize.StockTotal > 0 && prize.StockUsed >= prize.StockTotal {
			continue
		}
		weighted = append(weighted, prize)
		total += prize.Weight
	}
	if total <= 0 {
		return nil, ErrLotteryPrizeUnavailable
	}
	pick, err := cryptoRandInt(total)
	if err != nil {
		return nil, err
	}
	cursor := pick + 1
	for i := range weighted {
		cursor -= weighted[i].Weight
		if cursor <= 0 {
			return &weighted[i], nil
		}
	}
	return nil, ErrLotteryPrizeUnavailable
}

func cryptoRandInt(max int) (int, error) {
	if max <= 0 {
		return 0, errors.New("max must be positive")
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	return int(binary.BigEndian.Uint64(b[:]) % uint64(max)), nil
}
