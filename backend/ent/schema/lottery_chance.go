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

type LotteryChance struct {
	ent.Schema
}

func (LotteryChance) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "lottery_chances"},
	}
}

func (LotteryChance) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("campaign_id"),
		field.Int64("user_id"),
		field.String("source").
			MaxLen(64).
			Default(""),
		field.String("source_id").
			MaxLen(128).
			Default(""),
		field.String("status").
			MaxLen(20).
			Default("available"),
		field.Time("expires_at").
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("used_at").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
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

func (LotteryChance) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("campaign", LotteryCampaign.Type).
			Ref("chances").
			Field("campaign_id").
			Unique().
			Required(),
		edge.From("user", User.Type).
			Ref("lottery_chances").
			Field("user_id").
			Unique().
			Required(),
		edge.To("draw", LotteryDraw.Type).
			Unique(),
	}
}

func (LotteryChance) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("user_id", "campaign_id", "status", "expires_at").
			Annotations(entsql.IndexWhere("status = 'available'")),
		index.Fields("campaign_id", "status", "expires_at"),
		index.Fields("campaign_id", "user_id", "source", "source_id").
			Unique().
			Annotations(entsql.IndexWhere("source <> '' AND source_id <> ''")),
	}
}
