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

type RankingRewardAward struct {
	ent.Schema
}

func (RankingRewardAward) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "ranking_reward_awards"},
	}
}

func (RankingRewardAward) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("run_id"),
		field.Int64("campaign_id"),
		field.Int64("lottery_campaign_id"),
		field.Int64("user_id"),
		field.Int("rank"),
		field.Float("actual_cost").
			Default(0).
			SchemaType(map[string]string{dialect.Postgres: "decimal(20,10)"}),
		field.Int64("requests").
			Default(0),
		field.Int64("tokens").
			Default(0),
		field.Int("chance_count").
			Default(1),
		field.JSON("lottery_chance_ids", []int64{}).
			Optional().
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}),
		field.JSON("metadata", map[string]any{}).
			Optional().
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}),
		field.Time("created_at").
			Immutable().
			Default(time.Now).
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
	}
}

func (RankingRewardAward) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("run", RankingRewardRun.Type).
			Ref("awards").
			Field("run_id").
			Unique().
			Required(),
		edge.From("campaign", RankingRewardCampaign.Type).
			Ref("awards").
			Field("campaign_id").
			Unique().
			Required(),
		edge.From("lottery_campaign", LotteryCampaign.Type).
			Ref("ranking_reward_awards").
			Field("lottery_campaign_id").
			Unique().
			Required(),
		edge.From("user", User.Type).
			Ref("ranking_reward_awards").
			Field("user_id").
			Unique().
			Required(),
	}
}

func (RankingRewardAward) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("run_id", "user_id").
			Unique(),
		index.Fields("campaign_id", "user_id", "created_at"),
	}
}
