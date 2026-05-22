package repository

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/lotterychance"
	"github.com/Wei-Shaw/sub2api/ent/predicate"
	"github.com/Wei-Shaw/sub2api/ent/rankingrewardaward"
	"github.com/Wei-Shaw/sub2api/ent/rankingrewardcampaign"
	"github.com/Wei-Shaw/sub2api/ent/rankingrewardexcludeduser"
	"github.com/Wei-Shaw/sub2api/ent/rankingrewardrun"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"

	entsql "entgo.io/ent/dialect/sql"
)

type rankingRewardRepository struct {
	client *dbent.Client
	db     *sql.DB
}

func NewRankingRewardRepository(client *dbent.Client, sqlDB *sql.DB) service.RankingRewardRepository {
	return &rankingRewardRepository{client: client, db: sqlDB}
}

func (r *rankingRewardRepository) CreateCampaign(ctx context.Context, input *service.CreateRankingRewardCampaignInput) (*service.RankingRewardCampaign, error) {
	client := clientFromContext(ctx, r.client)
	create := client.RankingRewardCampaign.Create().
		SetName(input.Name).
		SetDescription(input.Description).
		SetStatus(input.Status).
		SetLotteryCampaignID(input.LotteryCampaignID).
		SetTopN(input.TopN).
		SetChanceCount(input.ChanceCount).
		SetMinActualCost(input.MinActualCost).
		SetStartsAt(input.StartsAt).
		SetTimezone(input.Timezone).
		SetNillableEndsAt(input.EndsAt)
	if input.Metadata != nil {
		create.SetMetadata(input.Metadata)
	}
	m, err := create.Save(ctx)
	if err != nil {
		return nil, err
	}
	return rankingRewardCampaignEntityToService(m), nil
}

func (r *rankingRewardRepository) UpdateCampaign(ctx context.Context, id int64, input *service.UpdateRankingRewardCampaignInput) (*service.RankingRewardCampaign, error) {
	client := clientFromContext(ctx, r.client)
	up := client.RankingRewardCampaign.UpdateOneID(id)
	if input.Name != nil {
		up.SetName(strings.TrimSpace(*input.Name))
	}
	if input.Description != nil {
		up.SetDescription(*input.Description)
	}
	if input.Status != nil {
		up.SetStatus(*input.Status)
	}
	if input.LotteryCampaignID != nil {
		up.SetLotteryCampaignID(*input.LotteryCampaignID)
	}
	if input.TopN != nil {
		up.SetTopN(*input.TopN)
	}
	if input.ChanceCount != nil {
		up.SetChanceCount(*input.ChanceCount)
	}
	if input.MinActualCost != nil {
		up.SetMinActualCost(*input.MinActualCost)
	}
	if input.StartsAt != nil {
		up.SetStartsAt(input.StartsAt.UTC())
	}
	if input.EndsAt.Set {
		if input.EndsAt.Value != nil {
			up.SetEndsAt(input.EndsAt.Value.UTC())
		} else {
			up.ClearEndsAt()
		}
	}
	if input.Timezone != nil {
		up.SetTimezone(strings.TrimSpace(*input.Timezone))
	}
	if input.LastRunDate.Set {
		if input.LastRunDate.Value != nil {
			up.SetLastRunDate(*input.LastRunDate.Value)
		} else {
			up.ClearLastRunDate()
		}
	}
	if input.Metadata != nil {
		up.SetMetadata(input.Metadata)
	}
	m, err := up.Save(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, service.ErrRankingRewardCampaignNotFound
		}
		return nil, err
	}
	return rankingRewardCampaignEntityToService(m), nil
}

func (r *rankingRewardRepository) GetCampaign(ctx context.Context, id int64) (*service.RankingRewardCampaign, error) {
	client := clientFromContext(ctx, r.client)
	m, err := client.RankingRewardCampaign.Query().Where(rankingrewardcampaign.IDEQ(id)).Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, service.ErrRankingRewardCampaignNotFound
		}
		return nil, err
	}
	return rankingRewardCampaignEntityToService(m), nil
}

func (r *rankingRewardRepository) ListCampaigns(ctx context.Context, params pagination.PaginationParams, status string) ([]service.RankingRewardCampaign, *pagination.PaginationResult, error) {
	client := clientFromContext(ctx, r.client)
	q := client.RankingRewardCampaign.Query()
	if status != "" {
		q = q.Where(rankingrewardcampaign.StatusEQ(status))
	}
	total, err := q.Count(ctx)
	if err != nil {
		return nil, nil, err
	}
	items, err := q.Offset(params.Offset()).Limit(params.Limit()).Order(dbent.Desc(rankingrewardcampaign.FieldID)).All(ctx)
	if err != nil {
		return nil, nil, err
	}
	return rankingRewardCampaignEntitiesToService(items), paginationResultFromTotal(int64(total), params), nil
}

func (r *rankingRewardRepository) ListActiveCampaigns(ctx context.Context, now time.Time) ([]service.RankingRewardCampaign, error) {
	client := clientFromContext(ctx, r.client)
	items, err := client.RankingRewardCampaign.Query().
		Where(
			rankingrewardcampaign.StatusEQ(service.RankingRewardCampaignStatusActive),
			rankingrewardcampaign.StartsAtLTE(now),
			rankingrewardcampaign.Or(rankingrewardcampaign.EndsAtIsNil(), rankingrewardcampaign.EndsAtGT(now)),
		).
		Order(dbent.Asc(rankingrewardcampaign.FieldID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return rankingRewardCampaignEntitiesToService(items), nil
}

func (r *rankingRewardRepository) CreateExclusion(ctx context.Context, input *service.CreateRankingRewardExclusionInput) (*service.RankingRewardExcludedUser, error) {
	client := clientFromContext(ctx, r.client)
	m, err := client.RankingRewardExcludedUser.Create().
		SetCampaignID(input.CampaignID).
		SetUserID(input.UserID).
		SetReason(strings.TrimSpace(input.Reason)).
		Save(ctx)
	if err != nil {
		return nil, err
	}
	return rankingRewardExclusionEntityToService(m), nil
}

func (r *rankingRewardRepository) UpdateExclusion(ctx context.Context, id int64, input *service.UpdateRankingRewardExclusionInput) (*service.RankingRewardExcludedUser, error) {
	client := clientFromContext(ctx, r.client)
	up := client.RankingRewardExcludedUser.UpdateOneID(id)
	if input.Reason != nil {
		up.SetReason(strings.TrimSpace(*input.Reason))
	}
	m, err := up.Save(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, service.ErrRankingRewardExclusionNotFound
		}
		return nil, err
	}
	return rankingRewardExclusionEntityToService(m), nil
}

func (r *rankingRewardRepository) DeleteExclusion(ctx context.Context, id int64) error {
	client := clientFromContext(ctx, r.client)
	err := client.RankingRewardExcludedUser.DeleteOneID(id).Exec(ctx)
	if err != nil && dbent.IsNotFound(err) {
		return service.ErrRankingRewardExclusionNotFound
	}
	return err
}

func (r *rankingRewardRepository) ListExclusions(ctx context.Context, campaignID int64, params pagination.PaginationParams) ([]service.RankingRewardExcludedUser, *pagination.PaginationResult, error) {
	client := clientFromContext(ctx, r.client)
	q := client.RankingRewardExcludedUser.Query().Where(rankingrewardexcludeduser.CampaignIDEQ(campaignID))
	total, err := q.Count(ctx)
	if err != nil {
		return nil, nil, err
	}
	items, err := q.Offset(params.Offset()).Limit(params.Limit()).Order(dbent.Desc(rankingrewardexcludeduser.FieldID)).All(ctx)
	if err != nil {
		return nil, nil, err
	}
	return rankingRewardExclusionEntitiesToService(items), paginationResultFromTotal(int64(total), params), nil
}

func (r *rankingRewardRepository) ListRuns(ctx context.Context, campaignID int64, params pagination.PaginationParams) ([]service.RankingRewardRun, *pagination.PaginationResult, error) {
	client := clientFromContext(ctx, r.client)
	q := client.RankingRewardRun.Query().Where(rankingrewardrun.CampaignIDEQ(campaignID))
	total, err := q.Count(ctx)
	if err != nil {
		return nil, nil, err
	}
	items, err := q.Offset(params.Offset()).Limit(params.Limit()).Order(dbent.Desc(rankingrewardrun.FieldRewardDate), dbent.Desc(rankingrewardrun.FieldID)).All(ctx)
	if err != nil {
		return nil, nil, err
	}
	return rankingRewardRunEntitiesToService(items), paginationResultFromTotal(int64(total), params), nil
}

func (r *rankingRewardRepository) ListAwards(ctx context.Context, runID int64, params pagination.PaginationParams) ([]service.RankingRewardAward, *pagination.PaginationResult, error) {
	client := clientFromContext(ctx, r.client)
	q := client.RankingRewardAward.Query().Where(rankingrewardaward.RunIDEQ(runID))
	total, err := q.Count(ctx)
	if err != nil {
		return nil, nil, err
	}
	items, err := q.Offset(params.Offset()).Limit(params.Limit()).Order(dbent.Asc(rankingrewardaward.FieldRank), dbent.Asc(rankingrewardaward.FieldID)).All(ctx)
	if err != nil {
		return nil, nil, err
	}
	return rankingRewardAwardEntitiesToService(items), paginationResultFromTotal(int64(total), params), nil
}

func (r *rankingRewardRepository) CountRunLotteryChances(ctx context.Context, runID int64) (int, error) {
	client := clientFromContext(ctx, r.client)
	return client.LotteryChance.Query().
		Where(
			lotterychance.SourceEQ(service.LotteryChanceSourceRanking),
			lotterychance.MetadataNotNil(),
			lotteryChanceRankingRunIDPredicate(runID),
		).
		Count(ctx)
}

func (r *rankingRewardRepository) CreateRun(ctx context.Context, campaign *service.RankingRewardCampaign, rewardDate, windowStart, windowEnd time.Time) (*service.RankingRewardRun, error) {
	client := clientFromContext(ctx, r.client)
	m, err := client.RankingRewardRun.Create().
		SetCampaignID(campaign.ID).
		SetRewardDate(rewardDate).
		SetWindowStart(windowStart).
		SetWindowEnd(windowEnd).
		SetStatus(service.RankingRewardRunStatusRunning).
		SetAwardedCount(0).
		SetTotalActualCost(0).
		SetMetadata(map[string]any{"reward_date": rewardDate.Format("2006-01-02")}).
		Save(ctx)
	if err != nil {
		if isUniqueConstraintViolation(err) {
			return nil, service.ErrRankingRewardRunExists
		}
		return nil, err
	}
	return rankingRewardRunEntityToService(m), nil
}

func (r *rankingRewardRepository) GetRunByCampaignDate(ctx context.Context, campaignID int64, rewardDate time.Time) (*service.RankingRewardRun, error) {
	client := clientFromContext(ctx, r.client)
	m, err := client.RankingRewardRun.Query().
		Where(rankingrewardrun.CampaignIDEQ(campaignID), rankingrewardrun.RewardDateEQ(rewardDate)).
		Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, service.ErrRankingRewardRunNotFound
		}
		return nil, err
	}
	return rankingRewardRunEntityToService(m), nil
}

func (r *rankingRewardRepository) RestartRun(ctx context.Context, runID int64, windowStart, windowEnd time.Time) (*service.RankingRewardRun, error) {
	client := clientFromContext(ctx, r.client)
	now := time.Now().UTC()
	affected, err := client.RankingRewardRun.Update().
		Where(rankingrewardrun.IDEQ(runID), rankingrewardrun.StatusEQ(service.RankingRewardRunStatusFailed)).
		SetStatus(service.RankingRewardRunStatusRunning).
		SetWindowStart(windowStart).
		SetWindowEnd(windowEnd).
		SetAwardedCount(0).
		SetTotalActualCost(0).
		SetErrorMessage("").
		SetStartedAt(now).
		ClearFinishedAt().
		Save(ctx)
	if err != nil {
		return nil, err
	}
	if affected == 0 {
		return nil, service.ErrRankingRewardRunExists
	}
	m, err := client.RankingRewardRun.Get(ctx, runID)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, service.ErrRankingRewardRunNotFound
		}
		return nil, err
	}
	return rankingRewardRunEntityToService(m), nil
}

func (r *rankingRewardRepository) CompleteRun(ctx context.Context, runID int64, awardedCount int, totalActualCost float64, metadata map[string]any) (*service.RankingRewardRun, error) {
	now := time.Now().UTC()
	client := clientFromContext(ctx, r.client)
	affected, err := client.RankingRewardRun.Update().
		Where(rankingrewardrun.IDEQ(runID), rankingrewardrun.StatusEQ(service.RankingRewardRunStatusRunning)).
		SetStatus(service.RankingRewardRunStatusCompleted).
		SetAwardedCount(awardedCount).
		SetTotalActualCost(totalActualCost).
		SetMetadata(metadata).
		SetFinishedAt(now).
		Save(ctx)
	if err != nil {
		return nil, err
	}
	if affected == 0 {
		return nil, service.ErrRankingRewardRunExists
	}
	m, err := client.RankingRewardRun.Get(ctx, runID)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, service.ErrRankingRewardRunNotFound
		}
		return nil, err
	}
	_, err = client.RankingRewardCampaign.UpdateOneID(m.CampaignID).
		SetLastRunDate(m.RewardDate).
		Save(ctx)
	if err != nil {
		return nil, err
	}
	return rankingRewardRunEntityToService(m), nil
}

func (r *rankingRewardRepository) FailRun(ctx context.Context, runID int64, errMessage string) error {
	now := time.Now().UTC()
	client := clientFromContext(ctx, r.client)
	_, err := client.RankingRewardRun.Update().
		Where(rankingrewardrun.IDEQ(runID), rankingrewardrun.StatusEQ(service.RankingRewardRunStatusRunning)).
		SetStatus(service.RankingRewardRunStatusFailed).
		SetErrorMessage(errMessage).
		SetFinishedAt(now).
		Save(ctx)
	return err
}

func (r *rankingRewardRepository) FindRankCandidates(ctx context.Context, campaignID int64, windowStart, windowEnd time.Time, limit int, minActualCost float64) (results []service.RankingRewardCandidate, err error) {
	if limit <= 0 {
		limit = 10
	}
	query := `
		WITH user_spend AS (
			SELECT
				u.user_id,
				COALESCE(us.email, '') AS email,
				COALESCE(SUM(u.actual_cost), 0) AS actual_cost,
				COUNT(*) AS requests,
				COALESCE(SUM(u.input_tokens + u.output_tokens + u.cache_creation_tokens + u.cache_read_tokens), 0) AS tokens
			FROM usage_logs u
			LEFT JOIN users us ON u.user_id = us.id
			WHERE u.created_at >= $1 AND u.created_at < $2
			GROUP BY u.user_id, us.email
		)
		SELECT user_id, email, actual_cost, requests, tokens
		FROM user_spend s
		WHERE s.actual_cost >= $4
		  AND NOT EXISTS (
			SELECT 1 FROM ranking_reward_excluded_users e
			WHERE e.campaign_id = $3 AND e.user_id = s.user_id
		  )
		ORDER BY actual_cost DESC, tokens DESC, user_id ASC
		LIMIT $5
	`
	rows, err := r.db.QueryContext(ctx, query, windowStart, windowEnd, campaignID, minActualCost, limit)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil && err == nil {
			err = closeErr
			results = nil
		}
	}()
	results = make([]service.RankingRewardCandidate, 0)
	rank := 1
	for rows.Next() {
		var item service.RankingRewardCandidate
		item.Rank = rank
		if err = rows.Scan(&item.UserID, &item.Email, &item.ActualCost, &item.Requests, &item.Tokens); err != nil {
			return nil, err
		}
		results = append(results, item)
		rank++
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

func (r *rankingRewardRepository) CreateAward(ctx context.Context, award *service.RankingRewardAward) (*service.RankingRewardAward, error) {
	client := clientFromContext(ctx, r.client)
	m, err := client.RankingRewardAward.Create().
		SetRunID(award.RunID).
		SetCampaignID(award.CampaignID).
		SetLotteryCampaignID(award.LotteryCampaignID).
		SetUserID(award.UserID).
		SetRank(award.Rank).
		SetActualCost(award.ActualCost).
		SetRequests(award.Requests).
		SetTokens(award.Tokens).
		SetChanceCount(award.ChanceCount).
		SetLotteryChanceIds(award.LotteryChanceIDs).
		SetMetadata(award.Metadata).
		Save(ctx)
	if err != nil {
		if isUniqueConstraintViolation(err) {
			return nil, service.ErrRankingRewardAwardExists
		}
		return nil, err
	}
	return rankingRewardAwardEntityToService(m), nil
}

func (r *rankingRewardRepository) GetAwardByRunUser(ctx context.Context, runID int64, userID int64) (*service.RankingRewardAward, error) {
	client := clientFromContext(ctx, r.client)
	m, err := client.RankingRewardAward.Query().
		Where(rankingrewardaward.RunIDEQ(runID), rankingrewardaward.UserIDEQ(userID)).
		Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, service.ErrRankingRewardAwardNotFound
		}
		return nil, err
	}
	return rankingRewardAwardEntityToService(m), nil
}

func lotteryChanceRankingRunIDPredicate(runID int64) predicate.LotteryChance {
	return predicate.LotteryChance(func(s *entsql.Selector) {
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.Ident(s.C(lotterychance.FieldMetadata)).WriteString(" ->> 'ranking_reward_run_id' = ").Arg(strconv.FormatInt(runID, 10))
		}))
	})
}

func rankingRewardCampaignEntityToService(m *dbent.RankingRewardCampaign) *service.RankingRewardCampaign {
	if m == nil {
		return nil
	}
	return &service.RankingRewardCampaign{
		ID:                m.ID,
		Name:              m.Name,
		Description:       m.Description,
		Status:            m.Status,
		LotteryCampaignID: m.LotteryCampaignID,
		TopN:              m.TopN,
		ChanceCount:       m.ChanceCount,
		MinActualCost:     m.MinActualCost,
		StartsAt:          m.StartsAt,
		EndsAt:            m.EndsAt,
		Timezone:          m.Timezone,
		LastRunDate:       m.LastRunDate,
		Metadata:          m.Metadata,
		CreatedAt:         m.CreatedAt,
		UpdatedAt:         m.UpdatedAt,
	}
}

func rankingRewardCampaignEntitiesToService(items []*dbent.RankingRewardCampaign) []service.RankingRewardCampaign {
	result := make([]service.RankingRewardCampaign, 0, len(items))
	for _, item := range items {
		if v := rankingRewardCampaignEntityToService(item); v != nil {
			result = append(result, *v)
		}
	}
	return result
}

func rankingRewardExclusionEntityToService(m *dbent.RankingRewardExcludedUser) *service.RankingRewardExcludedUser {
	if m == nil {
		return nil
	}
	return &service.RankingRewardExcludedUser{
		ID:         m.ID,
		CampaignID: m.CampaignID,
		UserID:     m.UserID,
		Reason:     m.Reason,
		CreatedAt:  m.CreatedAt,
		UpdatedAt:  m.UpdatedAt,
	}
}

func rankingRewardExclusionEntitiesToService(items []*dbent.RankingRewardExcludedUser) []service.RankingRewardExcludedUser {
	result := make([]service.RankingRewardExcludedUser, 0, len(items))
	for _, item := range items {
		if v := rankingRewardExclusionEntityToService(item); v != nil {
			result = append(result, *v)
		}
	}
	return result
}

func rankingRewardRunEntityToService(m *dbent.RankingRewardRun) *service.RankingRewardRun {
	if m == nil {
		return nil
	}
	return &service.RankingRewardRun{
		ID:              m.ID,
		CampaignID:      m.CampaignID,
		RewardDate:      m.RewardDate,
		WindowStart:     m.WindowStart,
		WindowEnd:       m.WindowEnd,
		Status:          m.Status,
		AwardedCount:    m.AwardedCount,
		TotalActualCost: m.TotalActualCost,
		ErrorMessage:    m.ErrorMessage,
		Metadata:        m.Metadata,
		StartedAt:       m.StartedAt,
		FinishedAt:      m.FinishedAt,
		CreatedAt:       m.CreatedAt,
		UpdatedAt:       m.UpdatedAt,
	}
}

func rankingRewardRunEntitiesToService(items []*dbent.RankingRewardRun) []service.RankingRewardRun {
	result := make([]service.RankingRewardRun, 0, len(items))
	for _, item := range items {
		if v := rankingRewardRunEntityToService(item); v != nil {
			result = append(result, *v)
		}
	}
	return result
}

func rankingRewardAwardEntityToService(m *dbent.RankingRewardAward) *service.RankingRewardAward {
	if m == nil {
		return nil
	}
	return &service.RankingRewardAward{
		ID:                m.ID,
		RunID:             m.RunID,
		CampaignID:        m.CampaignID,
		LotteryCampaignID: m.LotteryCampaignID,
		UserID:            m.UserID,
		Rank:              m.Rank,
		ActualCost:        m.ActualCost,
		Requests:          m.Requests,
		Tokens:            m.Tokens,
		ChanceCount:       m.ChanceCount,
		LotteryChanceIDs:  m.LotteryChanceIds,
		Metadata:          m.Metadata,
		CreatedAt:         m.CreatedAt,
	}
}

func rankingRewardAwardEntitiesToService(items []*dbent.RankingRewardAward) []service.RankingRewardAward {
	result := make([]service.RankingRewardAward, 0, len(items))
	for _, item := range items {
		if v := rankingRewardAwardEntityToService(item); v != nil {
			result = append(result, *v)
		}
	}
	return result
}
