package repository

import (
	"context"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/predicate"
	"github.com/Wei-Shaw/sub2api/ent/userquotagrant"
	"github.com/Wei-Shaw/sub2api/internal/service"

	entsql "entgo.io/ent/dialect/sql"
)

type userQuotaGrantRepository struct {
	client *dbent.Client
}

func NewUserQuotaGrantRepository(client *dbent.Client) service.UserQuotaGrantRepository {
	return &userQuotaGrantRepository{client: client}
}

func (r *userQuotaGrantRepository) Create(ctx context.Context, input *service.CreateUserQuotaGrantInput) (*service.UserQuotaGrant, error) {
	if input == nil {
		return nil, nil
	}
	client := clientFromContext(ctx, r.client)
	create := client.UserQuotaGrant.Create().
		SetUserID(input.UserID).
		SetAmountUsd(input.AmountUSD).
		SetUsedAmountUsd(0).
		SetStartsAt(input.StartsAt).
		SetExpiresAt(input.ExpiresAt).
		SetSource(input.Source).
		SetSourceID(input.SourceID).
		SetStatus(service.TimedQuotaGrantStatusActive)
	if input.Metadata != nil {
		create.SetMetadata(input.Metadata)
	}
	created, err := create.Save(ctx)
	if err != nil {
		return nil, err
	}
	return userQuotaGrantEntityToService(created), nil
}

func (r *userQuotaGrantRepository) ListActiveByUser(ctx context.Context, userID int64, now time.Time, limit int) ([]service.UserQuotaGrant, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	client := clientFromContext(ctx, r.client)
	models, err := client.UserQuotaGrant.Query().
		Where(
			userquotagrant.UserIDEQ(userID),
			userquotagrant.StatusEQ(service.TimedQuotaGrantStatusActive),
			userquotagrant.StartsAtLTE(now),
			userquotagrant.ExpiresAtGT(now),
			userQuotaGrantRemainingPredicate(),
		).
		Order(dbent.Asc(userquotagrant.FieldExpiresAt), dbent.Asc(userquotagrant.FieldID)).
		Limit(limit).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]service.UserQuotaGrant, 0, len(models))
	for _, m := range models {
		out = append(out, *userQuotaGrantEntityToService(m))
	}
	return out, nil
}

func (r *userQuotaGrantRepository) Revoke(ctx context.Context, id int64) error {
	client := clientFromContext(ctx, r.client)
	_, err := client.UserQuotaGrant.Update().
		Where(userquotagrant.IDEQ(id)).
		SetStatus(service.TimedQuotaGrantStatusRevoked).
		SetUpdatedAt(time.Now().UTC()).
		Save(ctx)
	return err
}

func userQuotaGrantRemainingPredicate() predicate.UserQuotaGrant {
	return predicate.UserQuotaGrant(func(s *entsql.Selector) {
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.Ident(s.C(userquotagrant.FieldUsedAmountUsd)).WriteString(" < ").Ident(s.C(userquotagrant.FieldAmountUsd))
		}))
	})
}

func userQuotaGrantEntityToService(m *dbent.UserQuotaGrant) *service.UserQuotaGrant {
	if m == nil {
		return nil
	}
	return &service.UserQuotaGrant{
		ID:            m.ID,
		UserID:        m.UserID,
		AmountUSD:     m.AmountUsd,
		UsedAmountUSD: m.UsedAmountUsd,
		StartsAt:      m.StartsAt,
		ExpiresAt:     m.ExpiresAt,
		Source:        m.Source,
		SourceID:      m.SourceID,
		Status:        m.Status,
		Metadata:      m.Metadata,
		CreatedAt:     m.CreatedAt,
		UpdatedAt:     m.UpdatedAt,
	}
}
