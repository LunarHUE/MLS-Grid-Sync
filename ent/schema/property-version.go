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

type PropertyVersion struct{ ent.Schema }

func (PropertyVersion) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entgql.QueryField(),
		entgql.RelayConnection(),
		entsql.Annotation{Table: "property_version"},
	}
}

func (PropertyVersion) Mixin() []ent.Mixin {
	return []ent.Mixin{
		VersionMixin{},        // id, valid_from, valid_to, change_type, changed_fields, processor_version
		MLSMetadataMixin{},    // source_modified_at, originating_system_name, mlg_can_view, mlg_can_use
		PropertyDataMixin{},   // all typed property columns
		ExtendedFieldsMixin{}, // jsonb extended_fields
	}
}

func (PropertyVersion) Fields() []ent.Field {
	return []ent.Field{
		// We carry listing_key directly (no FK constraint — current row may be gone).
		field.String("listing_key"),
		// sync_event_id and raw_output_id are plain UUIDs — no Ent edges back to
		// SyncEvent/RawOutput because doing so requires inverse edges on those
		// schemas for all 7 version tables, which is noisy. Integrity is
		// maintained at write time (versions written in same tx as sync_event).
		// Add FK constraints via Atlas migration hooks if desired.
		field.UUID("sync_event_id", uuid.UUID{}),
		field.UUID("raw_output_id", uuid.UUID{}).
			Optional().Nillable().
			Comment("Nullable — manual fixes may not derive from a raw_output row"),
	}
}

func (PropertyVersion) Indexes() []ent.Index {
	return []ent.Index{
		// As-of queries: latest version of listing X at time T
		index.Fields("listing_key", "valid_from"),
		index.Fields("sync_event_id"),
		index.Fields("processor_version"),
		// At most one current row per listing — DB-level invariant
		index.Fields("listing_key").
			Unique().
			Annotations(entsql.IndexWhere("valid_to IS NULL")),
	}
}
