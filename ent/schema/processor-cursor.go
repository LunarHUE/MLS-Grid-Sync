package schema

import (
	"entgo.io/contrib/entgql"
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// ProcessorCursor tracks how far the raw → typed processor has advanced
// per resource. Exactly one row per resource (enforced by the unique index).
//
// Single-writer invariant: only one processor goroutine may advance a given
// resource's cursor at a time. The sync service enforces this with a
// per-resource Postgres advisory lock (added in Phase 2).
type ProcessorCursor struct{ ent.Schema }

func (ProcessorCursor) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entgql.Skip(entgql.SkipAll),
		entsql.Annotation{Table: "processor_cursor"},
	}
}

func (ProcessorCursor) Mixin() []ent.Mixin {
	return []ent.Mixin{AuditMixin{}}
}

func (ProcessorCursor) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuidV7).
			StorageKey("processor_cursor_id"),
		field.Enum("resource").
			Values(
				"property",
				"media",
				"member",
				"office",
				"open_house",
				"property_rooms",
				"property_unit_types",
				"lookup",
			).
			Immutable(),
		field.UUID("last_raw_output_id", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("Cursor — last raw_output.id processed. NULL means nothing processed yet."),
		field.String("processor_version").
			Comment("Code version that produced the most recent advance"),
	}
}

func (ProcessorCursor) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("resource").Unique(),
	}
}
