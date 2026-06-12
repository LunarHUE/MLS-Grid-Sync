package schema

import (
	"time"

	"entgo.io/contrib/entgql"
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/mixin"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// uuidV7 generates a UUIDv7. Used as the default for every UUID PK in this
// package so primary keys are sortable by creation time — raw_output's
// processor cursor depends on this property.
func uuidV7() uuid.UUID {
	return uuid.Must(uuid.NewV7())
}

// AuditMixin: every row tracks when WE created/modified it (separate from
// the source's modification timestamp).
type AuditMixin struct{ mixin.Schema }

func (AuditMixin) Fields() []ent.Field {
	return []ent.Field{
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("modified_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// MLSMetadataMixin: fields every MLS Grid resource carries.
//   - source_modified_at: the upstream ModificationTimestamp (canonical ordering)
//   - originating_system_name: the MLS that produced the record
//   - mlg_can_view: MLS Grid's delete flag (when false, the record is gone upstream)
//   - mlg_can_use: allowed use-case groups
type MLSMetadataMixin struct{ mixin.Schema }

func (MLSMetadataMixin) Fields() []ent.Field {
	return []ent.Field{
		field.Time("source_modified_at").
			Comment("Upstream ModificationTimestamp — canonical ordering").
			Annotations(entgql.OrderField("SOURCE_MODIFIED_AT")),
		field.String("originating_system_name").
			Optional().
			Nillable(),
		field.Bool("mlg_can_view").
			Default(true).
			Comment("MLS Grid delete flag — false means record is removed upstream"),
		field.Strings("mlg_can_use").
			Optional().
			Comment("Allowed use-case groups"),
	}
}

// ExtendedFieldsMixin: JSONB blob for ACT_* and uncommon RESO fields.
type ExtendedFieldsMixin struct{ mixin.Schema }

func (ExtendedFieldsMixin) Fields() []ent.Field {
	return []ent.Field{
		field.JSON("extended_fields", map[string]any{}).
			Optional().
			SchemaType(map[string]string{
				dialect.Postgres: "jsonb",
			}),
	}
}

// VersionMixin: append-only history rows have these fields in addition
// to mirroring the current entity's columns.
//
//	valid_from / valid_to: temporal range (valid_to == nil means current)
//	change_type:           Insert | Update | Delete
//	changed_fields:        JSONB dictionary of which keys differed from prior version
//	processor_version:     parser version that produced this row (for replay debugging)
type VersionMixin struct{ mixin.Schema }

func (VersionMixin) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").
			DefaultFunc(func() string {
				return uuidV7().String()
			}).
			Immutable(),
		field.Time("valid_from"),
		field.Time("valid_to").
			Optional().
			Nillable().
			Comment("Nil means this is the current version"),
		field.Enum("change_type").
			Values("insert", "update", "delete"),
		field.JSON("changed_fields", map[string]any{}).
			Optional().
			SchemaType(map[string]string{
				dialect.Postgres: "jsonb",
			}).
			Comment("Keys that differ from the prior version"),
		field.String("processor_version"),
	}
}

// textArray builds a native Postgres text[] column backed by pq.StringArray.
// Use for RESO Collection(Edm.String) fields you want to filter with GIN
// containment (e.g. appliances @> ARRAY['Dishwasher']). GIN index itself is
// added via migration hook — see README.
func textArray(name string) ent.Field {
	return field.Other(name, pq.StringArray{}).
		SchemaType(map[string]string{
			dialect.Postgres: "text[]",
		}).
		Optional().
		Annotations(entgql.Skip(entgql.SkipWhereInput))
}
