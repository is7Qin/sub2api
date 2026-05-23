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

type RankingRewardRun struct {
	ent.Schema
}

func (RankingRewardRun) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "ranking_reward_runs"},
	}
}

func (RankingRewardRun) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("campaign_id"),
		field.Time("reward_date").
			SchemaType(map[string]string{dialect.Postgres: "date"}),
		field.Time("window_start").
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("window_end").
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.String("status").
			MaxLen(20).
			Default("running"),
		field.Int("awarded_count").
			Default(0),
		field.Float("total_actual_cost").
			Default(0).
			SchemaType(map[string]string{dialect.Postgres: "decimal(20,10)"}),
		field.String("error_message").
			SchemaType(map[string]string{dialect.Postgres: "text"}).
			Default(""),
		field.JSON("metadata", map[string]any{}).
			Optional().
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}),
		field.Time("started_at").
			Default(time.Now).
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("finished_at").
			Optional().
			Nillable().
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

func (RankingRewardRun) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("campaign", RankingRewardCampaign.Type).
			Ref("runs").
			Field("campaign_id").
			Unique().
			Required(),
		edge.To("awards", RankingRewardAward.Type),
	}
}

func (RankingRewardRun) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("campaign_id", "reward_date").
			Unique(),
	}
}
