package schema

import (
	"entgo.io/contrib/entgql"
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

type SourceSystem struct{ ent.Schema }

func (SourceSystem) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entgql.QueryField(),
		entgql.RelayConnection(),
		entsql.Annotation{Table: "source_system"},
	}
}

func (SourceSystem) Mixin() []ent.Mixin {
	return []ent.Mixin{AuditMixin{}}
}

func (SourceSystem) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").
			StorageKey("source_system_id").
			Comment("Stable slug like 'mlsgrid'"),
		field.String("source_system_name"),
	}
}

func (SourceSystem) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("sync_events", SyncEvent.Type),
	}
}
