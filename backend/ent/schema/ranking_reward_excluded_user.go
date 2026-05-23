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

type RankingRewardExcludedUser struct {
	ent.Schema
}

func (RankingRewardExcludedUser) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "ranking_reward_excluded_users"},
	}
}

func (RankingRewardExcludedUser) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("campaign_id"),
		field.Int64("user_id"),
		field.String("reason").
			SchemaType(map[string]string{dialect.Postgres: "text"}).
			Default(""),
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

func (RankingRewardExcludedUser) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("campaign", RankingRewardCampaign.Type).
			Ref("excluded_users").
			Field("campaign_id").
			Unique().
			Required(),
		edge.From("user", User.Type).
			Ref("ranking_reward_exclusions").
			Field("user_id").
			Unique().
			Required(),
	}
}

func (RankingRewardExcludedUser) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("campaign_id", "user_id").
			Unique(),
	}
}
