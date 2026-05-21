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

type RechargeResetCampaignRule struct {
	ent.Schema
}

func (RechargeResetCampaignRule) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "recharge_reset_campaign_rules"},
	}
}

func (RechargeResetCampaignRule) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("campaign_id"),
		field.Int64("group_id"),
		field.Float("threshold_amount").SchemaType(map[string]string{dialect.Postgres: "decimal(20,2)"}),
		field.String("status").MaxLen(20).Default("active"),
		field.JSON("metadata", map[string]any{}).Optional().SchemaType(map[string]string{dialect.Postgres: "jsonb"}),
		field.Time("created_at").Immutable().Default(time.Now).SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now).SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
	}
}

func (RechargeResetCampaignRule) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("campaign", RechargeResetCampaign.Type).Ref("rules").Field("campaign_id").Unique().Required(),
		edge.From("group", Group.Type).Ref("recharge_reset_rules").Field("group_id").Unique().Required(),
		edge.To("records", RechargeResetRecord.Type),
	}
}

func (RechargeResetCampaignRule) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("campaign_id", "group_id").Unique(),
		index.Fields("group_id", "status"),
	}
}
