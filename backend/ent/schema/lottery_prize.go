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

type LotteryPrize struct {
	ent.Schema
}

func (LotteryPrize) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "lottery_prizes"},
	}
}

func (LotteryPrize) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("campaign_id"),
		field.String("name").
			MaxLen(100).
			NotEmpty(),
		field.String("description").
			SchemaType(map[string]string{dialect.Postgres: "text"}).
			Default(""),
		field.String("status").
			MaxLen(20).
			Default("active"),
		field.Int("weight").
			Default(0),
		field.Int("stock_total").
			Default(0),
		field.Int("stock_used").
			Default(0),
		field.String("redeem_type").
			MaxLen(32),
		field.Float("redeem_value").
			SchemaType(map[string]string{dialect.Postgres: "decimal(20,8)"}).
			Default(0),
		field.Int64("redeem_group_id").
			Optional().
			Nillable(),
		field.Int("redeem_validity_days").
			Default(30),
		field.JSON("redeem_metadata", map[string]any{}).
			Optional().
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}),
		field.Int("sort_order").
			Default(0),
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

func (LotteryPrize) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("campaign", LotteryCampaign.Type).
			Ref("prizes").
			Field("campaign_id").
			Unique().
			Required(),
		edge.To("draws", LotteryDraw.Type),
	}
}

func (LotteryPrize) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("campaign_id", "status", "sort_order"),
	}
}
