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

type RechargeResetRecord struct {
	ent.Schema
}

func (RechargeResetRecord) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "recharge_reset_records"},
	}
}

func (RechargeResetRecord) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("campaign_id"),
		field.Int64("rule_id"),
		field.Int64("order_id"),
		field.Int64("user_id"),
		field.Int64("subscription_id"),
		field.Int64("group_id"),
		field.Float("recharge_amount").SchemaType(map[string]string{dialect.Postgres: "decimal(20,2)"}),
		field.Float("threshold_amount").SchemaType(map[string]string{dialect.Postgres: "decimal(20,2)"}),
		field.Bool("reset_daily").Default(false),
		field.Bool("reset_weekly").Default(false),
		field.Bool("reset_monthly").Default(false),
		field.JSON("metadata", map[string]any{}).Optional().SchemaType(map[string]string{dialect.Postgres: "jsonb"}),
		field.Time("created_at").Immutable().Default(time.Now).SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
	}
}

func (RechargeResetRecord) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("campaign", RechargeResetCampaign.Type).Ref("records").Field("campaign_id").Unique().Required(),
		edge.From("rule", RechargeResetCampaignRule.Type).Ref("records").Field("rule_id").Unique().Required(),
		edge.From("order", PaymentOrder.Type).Ref("recharge_reset_records").Field("order_id").Unique().Required(),
		edge.From("user", User.Type).Ref("recharge_reset_records").Field("user_id").Unique().Required(),
		edge.From("subscription", UserSubscription.Type).Ref("recharge_reset_records").Field("subscription_id").Unique().Required(),
		edge.From("group", Group.Type).Ref("recharge_reset_records").Field("group_id").Unique().Required(),
	}
}

func (RechargeResetRecord) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("order_id", "rule_id").Unique(),
		index.Fields("order_id", "subscription_id").Unique(),
		index.Fields("user_id", "created_at"),
	}
}
