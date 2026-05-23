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

type RechargeResetCampaign struct {
	ent.Schema
}

func (RechargeResetCampaign) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "recharge_reset_campaigns"},
	}
}

func (RechargeResetCampaign) Fields() []ent.Field {
	return []ent.Field{
		field.String("name").MaxLen(100).NotEmpty(),
		field.String("description").SchemaType(map[string]string{dialect.Postgres: "text"}).Default(""),
		field.String("status").MaxLen(20).Default("draft"),
		field.Time("starts_at").SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("ends_at").Optional().Nillable().SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Bool("reset_daily").Default(true),
		field.Bool("reset_weekly").Default(true),
		field.Bool("reset_monthly").Default(true),
		field.JSON("metadata", map[string]any{}).Optional().SchemaType(map[string]string{dialect.Postgres: "jsonb"}),
		field.Time("created_at").Immutable().Default(time.Now).SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now).SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
	}
}

func (RechargeResetCampaign) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("rules", RechargeResetCampaignRule.Type),
		edge.To("records", RechargeResetRecord.Type),
	}
}

func (RechargeResetCampaign) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("status", "starts_at"),
	}
}
