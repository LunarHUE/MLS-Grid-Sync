package schema

import (
	"time"

	"entgo.io/contrib/entgql"
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

type RawOutput struct{ ent.Schema }

func (RawOutput) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entgql.Skip(entgql.SkipAll),
		entsql.Annotation{Table: "raw_output"},
	}
}

func (RawOutput) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuidV7).
			StorageKey("raw_output_id"),
		field.UUID("sync_event_id", uuid.UUID{}),
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
		field.String("source_key").
			Comment("RESO key from payload: ListingKey, MediaKey, MemberMlsId, etc."),
		field.Enum("change_type").
			Values("insert", "update", "delete"),
		field.Time("source_modified_at").
			Comment("Upstream ModificationTimestamp from the payload"),
		field.JSON("payload", map[string]any{}).
			SchemaType(map[string]string{
				dialect.Postgres: "jsonb",
			}),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

func (RawOutput) Indexes() []ent.Index {
	return []ent.Index{
		// Unique: enforces ON CONFLICT DO NOTHING dedup at the DB layer so
		// boundary records re-fetched under the §7 ge filter land idempotently.
		index.Fields("resource", "source_key", "source_modified_at").Unique(),
		index.Fields("sync_event_id"),
	}
}

func (RawOutput) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("sync_event", SyncEvent.Type).
			Ref("raw_outputs").
			Field("sync_event_id").
			Unique().
			Required(),
		edge.To("attachment_jobs", AttachmentJob.Type),
	}
}
