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

type RankingRewardCampaign struct {
	ent.Schema
}

func (RankingRewardCampaign) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "ranking_reward_campaigns"},
	}
}

func (RankingRewardCampaign) Fields() []ent.Field {
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
		field.Int64("lottery_campaign_id"),
		field.Int("top_n").
			Default(10),
		field.Int("chance_count").
			Default(1),
		field.Float("min_actual_cost").
			Default(0).
			SchemaType(map[string]string{dialect.Postgres: "decimal(20,10)"}),
		field.Time("starts_at").
			Default(time.Now).
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("ends_at").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.String("timezone").
			MaxLen(64).
			Default("Asia/Shanghai"),
		field.Time("last_run_date").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "date"}),
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

func (RankingRewardCampaign) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("lottery_campaign", LotteryCampaign.Type).
			Ref("ranking_reward_campaigns").
			Field("lottery_campaign_id").
			Unique().
			Required(),
		edge.To("excluded_users", RankingRewardExcludedUser.Type),
		edge.To("runs", RankingRewardRun.Type),
		edge.To("awards", RankingRewardAward.Type),
	}
}

func (RankingRewardCampaign) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("status", "starts_at", "ends_at"),
	}
}
