package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type LotteryCampaign struct {
	ent.Schema
}

func (LotteryCampaign) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "lottery_campaigns"},
	}
}

func (LotteryCampaign) Fields() []ent.Field {
	return []ent.Field{
		field.String("name").
			MaxLen(100).
			NotEmpty(),
		field.String("description").
			SchemaType(map[string]string{dialect.Postgres: "text"}).
			Default(""),
		field.String("status").
			MaxLen(20).
			Default("draft"),
		field.Time("starts_at").
			Default(time.Now).
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("ends_at").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Int("chance_expires_in_days").
			Default(1),
		field.JSON("metadata", map[string]any{}).
			Optional().
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}),
		field.Time("created_at").
			Immutable().
			Default(time.Now).
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now).
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
	}
}

func (LotteryCampaign) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("prizes", LotteryPrize.Type),
		edge.To("chances", LotteryChance.Type),
		edge.To("draws", LotteryDraw.Type),
		edge.To("ranking_reward_campaigns", RankingRewardCampaign.Type),
		edge.To("ranking_reward_awards", RankingRewardAward.Type),
	}
}

func (LotteryCampaign) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("status", "starts_at", "ends_at"),
	}
}
