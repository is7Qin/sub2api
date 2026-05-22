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

type LotteryDraw struct {
	ent.Schema
}

func (LotteryDraw) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "lottery_draws"},
	}
}

func (LotteryDraw) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("campaign_id"),
		field.Int64("user_id"),
		field.Int64("chance_id"),
		field.Int64("prize_id").
			Optional().
			Nillable(),
		field.Int64("redeem_code_id").
			Optional().
			Nillable(),
		field.String("redeem_code").
			MaxLen(64).
			Default(""),
		field.String("status").
			MaxLen(20).
			Default("awarded"),
		field.String("error_message").
			SchemaType(map[string]string{dialect.Postgres: "text"}).
			Default(""),
		field.JSON("metadata", map[string]any{}).
			Optional().
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}),
		field.Time("drawn_at").
			Default(time.Now).
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
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

func (LotteryDraw) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("campaign", LotteryCampaign.Type).
			Ref("draws").
			Field("campaign_id").
			Unique().
			Required(),
		edge.From("user", User.Type).
			Ref("lottery_draws").
			Field("user_id").
			Unique().
			Required(),
		edge.From("chance", LotteryChance.Type).
			Ref("draw").
			Field("chance_id").
			Unique().
			Required(),
		edge.From("prize", LotteryPrize.Type).
			Ref("draws").
			Field("prize_id").
			Unique(),
		edge.From("redeem_code_entity", RedeemCode.Type).
			Ref("lottery_draws").
			Field("redeem_code_id").
			Unique(),
	}
}

func (LotteryDraw) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("chance_id").
			Unique(),
		index.Fields("user_id", "campaign_id", "drawn_at"),
		index.Fields("campaign_id", "drawn_at"),
	}
}
