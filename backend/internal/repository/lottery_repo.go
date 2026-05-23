package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/lotterycampaign"
	"github.com/Wei-Shaw/sub2api/ent/lotterychance"
	"github.com/Wei-Shaw/sub2api/ent/lotterydraw"
	"github.com/Wei-Shaw/sub2api/ent/lotteryprize"
	"github.com/Wei-Shaw/sub2api/ent/predicate"
	"github.com/Wei-Shaw/sub2api/ent/redeemcode"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"

	entsql "entgo.io/ent/dialect/sql"
)

type lotteryRepository struct {
	client *dbent.Client
	db     *sql.DB
}

func NewLotteryRepository(client *dbent.Client, sqlDB *sql.DB) service.LotteryRepository {
	return &lotteryRepository{client: client, db: sqlDB}
}

func (r *lotteryRepository) CreateCampaign(ctx context.Context, input *service.CreateLotteryCampaignInput) (*service.LotteryCampaign, error) {
	client := clientFromContext(ctx, r.client)
	create := client.LotteryCampaign.Create().
		SetName(input.Name).
		SetDescription(input.Description).
		SetStatus(input.Status).
		SetStartsAt(input.StartsAt).
		SetChanceExpiresInDays(input.ChanceExpiresInDays).
		SetNillableEndsAt(input.EndsAt)
	if input.Metadata != nil {
		create.SetMetadata(input.Metadata)
	}
	m, err := create.Save(ctx)
	if err != nil {
		return nil, err
	}
	return lotteryCampaignEntityToService(m), nil
}

func (r *lotteryRepository) UpdateCampaign(ctx context.Context, id int64, input *service.UpdateLotteryCampaignInput) (*service.LotteryCampaign, error) {
	client := clientFromContext(ctx, r.client)
	up := client.LotteryCampaign.UpdateOneID(id)
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
	if input.ChanceExpiresInDays != nil {
		up.SetChanceExpiresInDays(*input.ChanceExpiresInDays)
	}
	if input.Metadata != nil {
		up.SetMetadata(input.Metadata)
	}
	m, err := up.Save(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, service.ErrLotteryCampaignNotFound
		}
		return nil, err
	}
	return lotteryCampaignEntityToService(m), nil
}

func (r *lotteryRepository) GetCampaign(ctx context.Context, id int64) (*service.LotteryCampaign, error) {
	client := clientFromContext(ctx, r.client)
	m, err := client.LotteryCampaign.Query().
		Where(lotterycampaign.IDEQ(id)).
		WithPrizes(func(q *dbent.LotteryPrizeQuery) {
			q.Order(dbent.Asc(lotteryprize.FieldSortOrder), dbent.Asc(lotteryprize.FieldID))
		}).
		Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, service.ErrLotteryCampaignNotFound
		}
		return nil, err
	}
	return lotteryCampaignEntityToService(m), nil
}

func (r *lotteryRepository) ListCampaigns(ctx context.Context, params pagination.PaginationParams, status string, activeOnly bool) ([]service.LotteryCampaign, *pagination.PaginationResult, error) {
	client := clientFromContext(ctx, r.client)
	q := client.LotteryCampaign.Query()
	if status != "" {
		q = q.Where(lotterycampaign.StatusEQ(status))
	}
	if activeOnly {
		now := time.Now().UTC()
		q = q.Where(
			lotterycampaign.StatusEQ(service.LotteryCampaignStatusActive),
			lotterycampaign.StartsAtLTE(now),
			lotterycampaign.Or(lotterycampaign.EndsAtIsNil(), lotterycampaign.EndsAtGT(now)),
		)
	}
	total, err := q.Count(ctx)
	if err != nil {
		return nil, nil, err
	}
	items, err := q.
		WithPrizes(func(pq *dbent.LotteryPrizeQuery) {
			pq.Where(lotteryprize.StatusEQ(service.LotteryPrizeStatusActive)).
				Order(dbent.Asc(lotteryprize.FieldSortOrder), dbent.Asc(lotteryprize.FieldID))
		}).
		Offset(params.Offset()).
		Limit(params.Limit()).
		Order(dbent.Desc(lotterycampaign.FieldID)).
		All(ctx)
	if err != nil {
		return nil, nil, err
	}
	return lotteryCampaignEntitiesToService(items), paginationResultFromTotal(int64(total), params), nil
}

func (r *lotteryRepository) CreatePrize(ctx context.Context, input *service.CreateLotteryPrizeInput) (*service.LotteryPrize, error) {
	client := clientFromContext(ctx, r.client)
	create := client.LotteryPrize.Create().
		SetCampaignID(input.CampaignID).
		SetName(input.Name).
		SetDescription(input.Description).
		SetStatus(input.Status).
		SetWeight(input.Weight).
		SetStockTotal(input.StockTotal).
		SetStockUsed(0).
		SetRedeemType(input.RedeemType).
		SetRedeemValue(input.RedeemValue).
		SetNillableRedeemGroupID(input.RedeemGroupID).
		SetRedeemValidityDays(input.RedeemValidityDays).
		SetSortOrder(input.SortOrder)
	if input.RedeemMetadata != nil {
		create.SetRedeemMetadata(input.RedeemMetadata)
	}
	if input.Metadata != nil {
		create.SetMetadata(input.Metadata)
	}
	m, err := create.Save(ctx)
	if err != nil {
		return nil, err
	}
	return lotteryPrizeEntityToService(m), nil
}

func (r *lotteryRepository) UpdatePrize(ctx context.Context, id int64, input *service.UpdateLotteryPrizeInput) (*service.LotteryPrize, error) {
	client := clientFromContext(ctx, r.client)
	up := client.LotteryPrize.UpdateOneID(id)
	if input.Name != nil {
		up.SetName(strings.TrimSpace(*input.Name))
	}
	if input.Description != nil {
		up.SetDescription(*input.Description)
	}
	if input.Status != nil {
		up.SetStatus(*input.Status)
	}
	if input.Weight != nil {
		up.SetWeight(*input.Weight)
	}
	if input.StockTotal != nil {
		up.SetStockTotal(*input.StockTotal)
	}
	if input.RedeemType != nil {
		up.SetRedeemType(*input.RedeemType)
	}
	if input.RedeemValue != nil {
		up.SetRedeemValue(*input.RedeemValue)
	}
	if input.RedeemGroupID.Set {
		if input.RedeemGroupID.Value != nil {
			up.SetRedeemGroupID(*input.RedeemGroupID.Value)
		} else {
			up.ClearRedeemGroupID()
		}
	}
	if input.RedeemValidityDays != nil {
		up.SetRedeemValidityDays(*input.RedeemValidityDays)
	}
	if input.RedeemMetadata != nil {
		up.SetRedeemMetadata(input.RedeemMetadata)
	}
	if input.SortOrder != nil {
		up.SetSortOrder(*input.SortOrder)
	}
	if input.Metadata != nil {
		up.SetMetadata(input.Metadata)
	}
	m, err := up.Save(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, service.ErrLotteryPrizeNotFound
		}
		return nil, err
	}
	return lotteryPrizeEntityToService(m), nil
}

func (r *lotteryRepository) GetPrize(ctx context.Context, id int64) (*service.LotteryPrize, error) {
	client := clientFromContext(ctx, r.client)
	m, err := client.LotteryPrize.Query().Where(lotteryprize.IDEQ(id)).Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, service.ErrLotteryPrizeNotFound
		}
		return nil, err
	}
	return lotteryPrizeEntityToService(m), nil
}

func (r *lotteryRepository) ListPrizes(ctx context.Context, campaignID int64) ([]service.LotteryPrize, error) {
	client := clientFromContext(ctx, r.client)
	items, err := client.LotteryPrize.Query().
		Where(lotteryprize.CampaignIDEQ(campaignID)).
		Order(dbent.Asc(lotteryprize.FieldSortOrder), dbent.Asc(lotteryprize.FieldID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return lotteryPrizeEntitiesToService(items), nil
}

func (r *lotteryRepository) GrantChances(ctx context.Context, input *service.GrantLotteryChanceInput, expiresAt time.Time) ([]service.LotteryChance, error) {
	if input.Count <= 0 {
		return nil, nil
	}
	client := clientFromContext(ctx, r.client)
	builders := make([]*dbent.LotteryChanceCreate, 0, input.Count)
	for i := 0; i < input.Count; i++ {
		sourceID := input.SourceID
		if input.Count > 1 && sourceID != "" {
			sourceID = sourceID + ":" + strconv.Itoa(i+1)
		}
		b := client.LotteryChance.Create().
			SetCampaignID(input.CampaignID).
			SetUserID(input.UserID).
			SetSource(strings.TrimSpace(input.Source)).
			SetSourceID(strings.TrimSpace(sourceID)).
			SetStatus(service.LotteryChanceStatusAvailable).
			SetExpiresAt(expiresAt)
		if input.Metadata != nil {
			b.SetMetadata(input.Metadata)
		}
		builders = append(builders, b)
	}
	created, err := client.LotteryChance.CreateBulk(builders...).Save(ctx)
	if err != nil {
		if isUniqueConstraintViolation(err) {
			return nil, service.ErrLotteryChanceGrantConflict
		}
		return nil, err
	}
	return lotteryChanceEntitiesToService(created), nil
}

func (r *lotteryRepository) ListChancesBySource(ctx context.Context, input *service.GrantLotteryChanceInput) ([]service.LotteryChance, error) {
	if input.Count <= 0 {
		return nil, nil
	}
	client := clientFromContext(ctx, r.client)
	sourceIDs := make([]string, 0, input.Count)
	for i := 0; i < input.Count; i++ {
		sourceID := input.SourceID
		if input.Count > 1 && sourceID != "" {
			sourceID = sourceID + ":" + strconv.Itoa(i+1)
		}
		sourceIDs = append(sourceIDs, strings.TrimSpace(sourceID))
	}
	items, err := client.LotteryChance.Query().
		Where(
			lotterychance.CampaignIDEQ(input.CampaignID),
			lotterychance.UserIDEQ(input.UserID),
			lotterychance.SourceEQ(strings.TrimSpace(input.Source)),
			lotterychance.SourceIDIn(sourceIDs...),
		).
		Order(dbent.Asc(lotterychance.FieldID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return lotteryChanceEntitiesToService(items), nil
}

func (r *lotteryRepository) ListUserChances(ctx context.Context, userID int64, campaignID int64, params pagination.PaginationParams) ([]service.LotteryChance, *pagination.PaginationResult, error) {
	client := clientFromContext(ctx, r.client)
	q := client.LotteryChance.Query().Where(lotterychance.UserIDEQ(userID))
	if campaignID > 0 {
		q = q.Where(lotterychance.CampaignIDEQ(campaignID))
	}
	total, err := q.Count(ctx)
	if err != nil {
		return nil, nil, err
	}
	items, err := q.Offset(params.Offset()).Limit(params.Limit()).Order(dbent.Desc(lotterychance.FieldID)).All(ctx)
	if err != nil {
		return nil, nil, err
	}
	return lotteryChanceEntitiesToService(items), paginationResultFromTotal(int64(total), params), nil
}

func (r *lotteryRepository) ListUserDraws(ctx context.Context, userID int64, campaignID int64, params pagination.PaginationParams) ([]service.LotteryDraw, *pagination.PaginationResult, error) {
	client := clientFromContext(ctx, r.client)
	q := client.LotteryDraw.Query().Where(lotterydraw.UserIDEQ(userID)).WithPrize()
	if campaignID > 0 {
		q = q.Where(lotterydraw.CampaignIDEQ(campaignID))
	}
	total, err := q.Count(ctx)
	if err != nil {
		return nil, nil, err
	}
	items, err := q.Offset(params.Offset()).Limit(params.Limit()).Order(dbent.Desc(lotterydraw.FieldDrawnAt), dbent.Desc(lotterydraw.FieldID)).All(ctx)
	if err != nil {
		return nil, nil, err
	}
	return lotteryDrawEntitiesToService(items), paginationResultFromTotal(int64(total), params), nil
}

func (r *lotteryRepository) GetPendingUserDraw(ctx context.Context, campaignID int64, userID int64) (*service.LotteryDraw, *service.LotteryPrize, *service.RedeemCode, error) {
	client := clientFromContext(ctx, r.client)
	leaseCutoff := time.Now().UTC().Add(-service.LotteryDrawProcessingLease)
	draw, err := client.LotteryDraw.Query().
		Where(
			lotterydraw.CampaignIDEQ(campaignID),
			lotterydraw.UserIDEQ(userID),
			lotterydraw.Or(
				lotterydraw.StatusEQ(service.LotteryDrawStatusPending),
				lotterydraw.And(
					lotterydraw.StatusEQ(service.LotteryDrawStatusProcessing),
					lotterydraw.UpdatedAtLT(leaseCutoff),
				),
			),
		).
		WithPrize().
		Order(dbent.Asc(lotterydraw.FieldID)).
		First(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, nil, nil, nil
		}
		return nil, nil, nil, err
	}
	serviceDraw := lotteryDrawEntityToService(draw)
	var prize *service.LotteryPrize
	if draw.Edges.Prize != nil {
		prize = lotteryPrizeEntityToService(draw.Edges.Prize)
	}
	redeem, err := client.RedeemCode.Query().
		Where(redeemCodeLotteryDrawIDPredicate(draw.ID)).
		Order(dbent.Asc(redeemcode.FieldID)).
		First(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return serviceDraw, prize, nil, nil
		}
		return nil, nil, nil, err
	}
	return serviceDraw, prize, redeemCodeEntityToService(redeem), nil
}

func (r *lotteryRepository) ClaimDrawRecovery(ctx context.Context, drawID int64, lease time.Duration) error {
	now := time.Now().UTC()
	leaseCutoff := now.Add(-lease)
	client := clientFromContext(ctx, r.client)
	affected, err := client.LotteryDraw.Update().
		Where(
			lotterydraw.IDEQ(drawID),
			lotterydraw.Or(
				lotterydraw.StatusEQ(service.LotteryDrawStatusPending),
				lotterydraw.And(
					lotterydraw.StatusEQ(service.LotteryDrawStatusProcessing),
					lotterydraw.UpdatedAtLT(leaseCutoff),
				),
			),
		).
		SetStatus(service.LotteryDrawStatusProcessing).
		SetUpdatedAt(now).
		Save(ctx)
	if err != nil {
		return err
	}
	if affected == 0 {
		return service.ErrLotteryChanceUnavailable
	}
	return nil
}

func (r *lotteryRepository) PrepareDraw(ctx context.Context, input *service.LotteryDrawInput, selector func([]service.LotteryPrize) (*service.LotteryPrize, error)) (_ *service.LotteryDraw, _ *service.LotteryPrize, err error) {
	if r.db == nil {
		return nil, nil, errors.New("lottery repository db is nil")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()

	campaign, err := lockLotteryCampaign(ctx, tx, input.CampaignID)
	if err != nil {
		return nil, nil, err
	}
	if !campaign.IsActiveAt(time.Now().UTC()) {
		return nil, nil, service.ErrLotteryCampaignInactive
	}
	chance, err := lockAvailableLotteryChance(ctx, tx, input.CampaignID, input.UserID)
	if err != nil {
		return nil, nil, err
	}
	prizes, err := lockLotteryPrizes(ctx, tx, input.CampaignID)
	if err != nil {
		return nil, nil, err
	}
	selected, err := selector(prizes)
	if err != nil {
		return nil, nil, err
	}
	if selected == nil {
		return nil, nil, service.ErrLotteryPrizeUnavailable
	}
	if err := consumeLotteryPrizeStock(ctx, tx, selected.ID); err != nil {
		return nil, nil, err
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `
		UPDATE lottery_chances
		SET status = $1, used_at = $2, updated_at = $2
		WHERE id = $3
	`, service.LotteryChanceStatusUsed, now, chance.ID); err != nil {
		return nil, nil, err
	}
	var draw service.LotteryDraw
	var prizeID int64
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO lottery_draws (campaign_id, user_id, chance_id, prize_id, status, drawn_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6, $6)
		RETURNING id, campaign_id, user_id, chance_id, prize_id, redeem_code, status, error_message, drawn_at, created_at, updated_at
	`, input.CampaignID, input.UserID, chance.ID, selected.ID, service.LotteryDrawStatusPending, now).Scan(
		&draw.ID, &draw.CampaignID, &draw.UserID, &draw.ChanceID, &prizeID, &draw.RedeemCode, &draw.Status, &draw.ErrorMessage, &draw.DrawnAt, &draw.CreatedAt, &draw.UpdatedAt,
	); err != nil {
		return nil, nil, err
	}
	draw.PrizeID = &prizeID
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	tx = nil
	return &draw, selected, nil
}

func (r *lotteryRepository) CompleteDraw(ctx context.Context, drawID int64, redeemCodeID int64, redeemCode string) (*service.LotteryDraw, error) {
	now := time.Now().UTC()
	client := clientFromContext(ctx, r.client)
	affected, err := client.LotteryDraw.Update().
		Where(
			lotterydraw.IDEQ(drawID),
			lotterydraw.Or(
				lotterydraw.StatusEQ(service.LotteryDrawStatusPending),
				lotterydraw.StatusEQ(service.LotteryDrawStatusProcessing),
			),
		).
		SetStatus(service.LotteryDrawStatusAwarded).
		SetRedeemCodeID(redeemCodeID).
		SetRedeemCode(redeemCode).
		SetUpdatedAt(now).
		Save(ctx)
	if err != nil {
		return nil, err
	}
	if affected == 0 {
		return nil, service.ErrLotteryChanceUnavailable
	}
	m, err := client.LotteryDraw.Get(ctx, drawID)
	if err != nil {
		return nil, err
	}
	return lotteryDrawEntityToService(m), nil
}

func (r *lotteryRepository) FailDraw(ctx context.Context, drawID int64, errMessage string) error {
	if r.db == nil {
		return errors.New("lottery repository db is nil")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()

	now := time.Now().UTC()
	var chanceID int64
	var prizeID sql.NullInt64
	var status string
	if err := tx.QueryRowContext(ctx, `
		SELECT chance_id, prize_id, status
		FROM lottery_draws
		WHERE id = $1
		FOR UPDATE
	`, drawID).Scan(&chanceID, &prizeID, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return service.ErrLotteryChanceUnavailable
		}
		return err
	}
	if status != service.LotteryDrawStatusPending && status != service.LotteryDrawStatusProcessing {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE lottery_draws
		SET status = $1, error_message = $2, updated_at = $3
		WHERE id = $4
	`, service.LotteryDrawStatusFailed, errMessage, now, drawID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE lottery_chances
		SET status = $1, used_at = NULL, updated_at = $2
		WHERE id = $3 AND status = $4
	`, service.LotteryChanceStatusAvailable, now, chanceID, service.LotteryChanceStatusUsed); err != nil {
		return err
	}
	if prizeID.Valid {
		if _, err := tx.ExecContext(ctx, `
			UPDATE lottery_prizes
			SET stock_used = GREATEST(stock_used - 1, 0), updated_at = $1
			WHERE id = $2 AND stock_used > 0
		`, now, prizeID.Int64); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	tx = nil
	return nil
}

func lockLotteryCampaign(ctx context.Context, tx *sql.Tx, campaignID int64) (*service.LotteryCampaign, error) {
	var c service.LotteryCampaign
	err := tx.QueryRowContext(ctx, `
		SELECT id, name, description, status, starts_at, ends_at, chance_expires_in_days, metadata, created_at, updated_at
		FROM lottery_campaigns
		WHERE id = $1
		FOR UPDATE
	`, campaignID).Scan(&c.ID, &c.Name, &c.Description, &c.Status, &c.StartsAt, &c.EndsAt, &c.ChanceExpiresInDays, scanJSONMap(&c.Metadata), &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrLotteryCampaignNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func lockAvailableLotteryChance(ctx context.Context, tx *sql.Tx, campaignID, userID int64) (*service.LotteryChance, error) {
	var c service.LotteryChance
	err := tx.QueryRowContext(ctx, `
		SELECT id, campaign_id, user_id, source, source_id, status, expires_at, used_at, metadata, created_at, updated_at
		FROM lottery_chances
		WHERE campaign_id = $1
			AND user_id = $2
			AND status = $3
			AND expires_at > NOW()
		ORDER BY expires_at ASC, id ASC
		LIMIT 1
		FOR UPDATE SKIP LOCKED
	`, campaignID, userID, service.LotteryChanceStatusAvailable).Scan(&c.ID, &c.CampaignID, &c.UserID, &c.Source, &c.SourceID, &c.Status, &c.ExpiresAt, &c.UsedAt, scanJSONMap(&c.Metadata), &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrLotteryChanceUnavailable
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func lockLotteryPrizes(ctx context.Context, tx *sql.Tx, campaignID int64) ([]service.LotteryPrize, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, campaign_id, name, description, status, weight, stock_total, stock_used, redeem_type,
			redeem_value::double precision, redeem_group_id, redeem_validity_days, redeem_metadata,
			sort_order, metadata, created_at, updated_at
		FROM lottery_prizes
		WHERE campaign_id = $1
			AND status = $2
			AND weight > 0
			AND (stock_total = 0 OR stock_used < stock_total)
		ORDER BY sort_order ASC, id ASC
		FOR UPDATE
	`, campaignID, service.LotteryPrizeStatusActive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]service.LotteryPrize, 0)
	for rows.Next() {
		var p service.LotteryPrize
		if err := rows.Scan(
			&p.ID, &p.CampaignID, &p.Name, &p.Description, &p.Status, &p.Weight, &p.StockTotal, &p.StockUsed,
			&p.RedeemType, &p.RedeemValue, &p.RedeemGroupID, &p.RedeemValidityDays, scanJSONMap(&p.RedeemMetadata),
			&p.SortOrder, scanJSONMap(&p.Metadata), &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, service.ErrLotteryPrizeUnavailable
	}
	return out, nil
}

func consumeLotteryPrizeStock(ctx context.Context, tx *sql.Tx, prizeID int64) error {
	res, err := tx.ExecContext(ctx, `
		UPDATE lottery_prizes
		SET stock_used = stock_used + 1, updated_at = NOW()
		WHERE id = $1
			AND (stock_total = 0 OR stock_used < stock_total)
	`, prizeID)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return service.ErrLotteryPrizeUnavailable
	}
	return nil
}

func redeemCodeLotteryDrawIDPredicate(drawID int64) predicate.RedeemCode {
	return predicate.RedeemCode(func(s *entsql.Selector) {
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.Ident(s.C(redeemcode.FieldMetadata)).WriteString(" ->> 'lottery_draw_id' = ").Arg(strconv.FormatInt(drawID, 10))
		}))
	})
}

func lotteryCampaignEntityToService(m *dbent.LotteryCampaign) *service.LotteryCampaign {
	if m == nil {
		return nil
	}
	out := &service.LotteryCampaign{
		ID:                  m.ID,
		Name:                m.Name,
		Description:         m.Description,
		Status:              m.Status,
		StartsAt:            m.StartsAt,
		EndsAt:              m.EndsAt,
		ChanceExpiresInDays: m.ChanceExpiresInDays,
		Metadata:            m.Metadata,
		CreatedAt:           m.CreatedAt,
		UpdatedAt:           m.UpdatedAt,
	}
	if len(m.Edges.Prizes) > 0 {
		out.Prizes = lotteryPrizeEntitiesToService(m.Edges.Prizes)
	}
	return out
}

func lotteryCampaignEntitiesToService(models []*dbent.LotteryCampaign) []service.LotteryCampaign {
	out := make([]service.LotteryCampaign, 0, len(models))
	for _, m := range models {
		if s := lotteryCampaignEntityToService(m); s != nil {
			out = append(out, *s)
		}
	}
	return out
}

func lotteryPrizeEntityToService(m *dbent.LotteryPrize) *service.LotteryPrize {
	if m == nil {
		return nil
	}
	return &service.LotteryPrize{
		ID:                 m.ID,
		CampaignID:         m.CampaignID,
		Name:               m.Name,
		Description:        m.Description,
		Status:             m.Status,
		Weight:             m.Weight,
		StockTotal:         m.StockTotal,
		StockUsed:          m.StockUsed,
		RedeemType:         m.RedeemType,
		RedeemValue:        m.RedeemValue,
		RedeemGroupID:      m.RedeemGroupID,
		RedeemValidityDays: m.RedeemValidityDays,
		RedeemMetadata:     m.RedeemMetadata,
		SortOrder:          m.SortOrder,
		Metadata:           m.Metadata,
		CreatedAt:          m.CreatedAt,
		UpdatedAt:          m.UpdatedAt,
	}
}

func lotteryPrizeEntitiesToService(models []*dbent.LotteryPrize) []service.LotteryPrize {
	out := make([]service.LotteryPrize, 0, len(models))
	for _, m := range models {
		if s := lotteryPrizeEntityToService(m); s != nil {
			out = append(out, *s)
		}
	}
	return out
}

func lotteryChanceEntityToService(m *dbent.LotteryChance) *service.LotteryChance {
	if m == nil {
		return nil
	}
	return &service.LotteryChance{
		ID:         m.ID,
		CampaignID: m.CampaignID,
		UserID:     m.UserID,
		Source:     m.Source,
		SourceID:   m.SourceID,
		Status:     m.Status,
		ExpiresAt:  m.ExpiresAt,
		UsedAt:     m.UsedAt,
		Metadata:   m.Metadata,
		CreatedAt:  m.CreatedAt,
		UpdatedAt:  m.UpdatedAt,
	}
}

func lotteryChanceEntitiesToService(models []*dbent.LotteryChance) []service.LotteryChance {
	out := make([]service.LotteryChance, 0, len(models))
	for _, m := range models {
		if s := lotteryChanceEntityToService(m); s != nil {
			out = append(out, *s)
		}
	}
	return out
}

func lotteryDrawEntityToService(m *dbent.LotteryDraw) *service.LotteryDraw {
	if m == nil {
		return nil
	}
	out := &service.LotteryDraw{
		ID:           m.ID,
		CampaignID:   m.CampaignID,
		UserID:       m.UserID,
		ChanceID:     m.ChanceID,
		PrizeID:      m.PrizeID,
		RedeemCodeID: m.RedeemCodeID,
		RedeemCode:   m.RedeemCode,
		Status:       m.Status,
		ErrorMessage: m.ErrorMessage,
		Metadata:     m.Metadata,
		DrawnAt:      m.DrawnAt,
		CreatedAt:    m.CreatedAt,
		UpdatedAt:    m.UpdatedAt,
	}
	if m.Edges.Prize != nil {
		out.Prize = lotteryPrizeEntityToService(m.Edges.Prize)
	}
	return out
}

func lotteryDrawEntitiesToService(models []*dbent.LotteryDraw) []service.LotteryDraw {
	out := make([]service.LotteryDraw, 0, len(models))
	for _, m := range models {
		if s := lotteryDrawEntityToService(m); s != nil {
			out = append(out, *s)
		}
	}
	return out
}

func scanJSONMap(dest *map[string]any) any {
	return &jsonMapScanner{dest: dest}
}

type jsonMapScanner struct {
	dest *map[string]any
}

func (s *jsonMapScanner) Scan(value any) error {
	if s.dest == nil {
		return nil
	}
	if value == nil {
		*s.dest = map[string]any{}
		return nil
	}
	switch v := value.(type) {
	case []byte:
		if len(v) == 0 {
			*s.dest = map[string]any{}
			return nil
		}
		return json.Unmarshal(v, s.dest)
	case string:
		if v == "" {
			*s.dest = map[string]any{}
			return nil
		}
		return json.Unmarshal([]byte(v), s.dest)
	case map[string]any:
		*s.dest = v
		return nil
	default:
		*s.dest = map[string]any{}
		return nil
	}
}
