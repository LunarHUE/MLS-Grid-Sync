package schema

import (
	"entgo.io/contrib/entgql"
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

type SyncEvent struct{ ent.Schema }

func (SyncEvent) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entgql.Skip(entgql.SkipAll),
		entsql.Annotation{Table: "sync_event"},
	}
}

func (SyncEvent) Mixin() []ent.Mixin {
	return []ent.Mixin{AuditMixin{}}
}

func (SyncEvent) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuidV7).
			StorageKey("sync_event_id"),
		field.String("source_system_id"),
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
			),
		field.Enum("run_type").
			Values("sync", "reprocess", "backfill"),
		field.Enum("status").
			Values("pending", "running", "success", "failed", "partial_success").
			Default("pending"),
		field.String("processor_version"),
		field.UUID("reprocess_of", uuid.UUID{}).
			Optional().
			Nillable().
			Comment("If this is a reprocess run, points at the original sync"),
		field.Time("started_at"),
		field.Time("ended_at").
			Optional().
			Nillable(),
		field.Int("record_count").
			Default(0),
		field.Time("high_water_mark").
			Optional().
			Nillable().
			Comment("Max source_modified_at observed this run — used as gt cursor for next sync"),
		field.Text("error_summary").
			Optional().
			Nillable(),
		field.Strings("logs").
			Optional(),
	}
}

func (SyncEvent) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("source_system_id", "resource", "status"),
		index.Fields("resource", "status", "started_at"),
	}
}

func (SyncEvent) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("source_system", SourceSystem.Type).
			Ref("sync_events").
			Field("source_system_id").
			Unique().
			Required(),
		edge.To("raw_outputs", RawOutput.Type),
		edge.To("attachment_jobs", AttachmentJob.Type),
		// Self-reference: a sync may have many reprocesses; each reprocess
		// has one original sync. FK column is `reprocess_of` on the child.
		edge.To("reprocesses", SyncEvent.Type).
			From("original_sync").
			Field("reprocess_of").
			Unique(),
	}
}
