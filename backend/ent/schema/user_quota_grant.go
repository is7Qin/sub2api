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

type UserQuotaGrant struct {
	ent.Schema
}

func (UserQuotaGrant) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "user_quota_grants"},
	}
}

func (UserQuotaGrant) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("user_id"),
		field.Float("amount_usd").
			SchemaType(map[string]string{dialect.Postgres: "decimal(20,8)"}),
		field.Float("used_amount_usd").
			SchemaType(map[string]string{dialect.Postgres: "decimal(20,8)"}).
			Default(0),
		field.Time("starts_at").
			Default(time.Now).
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("expires_at").
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.String("source").
			MaxLen(64).
			Default(""),
		field.String("source_id").
			MaxLen(128).
			Default(""),
		field.String("status").
			MaxLen(20).
			Default("active"),
		field.JSON("metadata", map[string]any{}).
			Optional(),
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

func (UserQuotaGrant) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("user", User.Type).
			Ref("quota_grants").
			Field("user_id").
			Unique().
			Required(),
	}
}

func (UserQuotaGrant) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("user_id", "status", "expires_at"),
		index.Fields("source", "source_id"),
		index.Fields("expires_at"),
	}
}
