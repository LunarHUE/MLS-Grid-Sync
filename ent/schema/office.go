package schema

import (
	"entgo.io/contrib/entgql"
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"entgo.io/ent/schema/mixin"
	"github.com/google/uuid"
)

// OfficeDataMixin: shared columns between Office and OfficeVersion.
// NOTE: office_key is NOT here — it's the PK on Office (via StorageKey)
// and a plain column on OfficeVersion.
type OfficeDataMixin struct{ mixin.Schema }

func (OfficeDataMixin) Fields() []ent.Field {
	return []ent.Field{
		field.String("office_mls_id").Optional().Nillable().
			Comment("MLS-visible identifier; unique alt-key"),
		field.String("office_name").Optional().Nillable(),
		field.String("office_status").Optional().Nillable(),
		field.String("office_type").Optional().Nillable(),
		// Contact
		field.String("office_phone").Optional().Nillable(),
		field.String("office_phone_ext").Optional().Nillable(),
		field.String("office_fax").Optional().Nillable(),
		// Address
		field.String("office_address1").Optional().Nillable(),
		field.String("office_address2").Optional().Nillable(),
		field.String("office_city").Optional().Nillable(),
		field.String("office_state_or_province").Optional().Nillable(),
		field.String("office_postal_code").Optional().Nillable(),
		field.String("office_postal_code_plus4").Optional().Nillable(),
		field.String("office_county_or_parish").Optional().Nillable(),
		// Identifiers
		field.String("office_corporate_license").Optional().Nillable(),
		field.String("office_national_association_id").Optional().Nillable(),
		// Org structure
		field.String("main_office_key").Optional().Nillable(),
		field.String("main_office_mls_id").Optional().Nillable(),
		field.String("office_broker_key").Optional().Nillable(),
		field.String("office_broker_mls_id").Optional().Nillable(),
		field.String("office_manager_key").Optional().Nillable(),
		// Flags / timestamps
		field.Bool("idx_office_participation_yn").Optional().Nillable(),
		field.Time("photos_change_timestamp").Optional().Nillable(),
	}
}

type Office struct{ ent.Schema }

func (Office) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entgql.QueryField(),
		entgql.RelayConnection(),
		entsql.Annotation{Table: "office"},
	}
}

func (Office) Mixin() []ent.Mixin {
	return []ent.Mixin{
		AuditMixin{},
		MLSMetadataMixin{},
		OfficeDataMixin{},
		ExtendedFieldsMixin{},
	}
}

func (Office) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").
			StorageKey("office_key").
			Comment("PK = OfficeKey (canonical identifier from MLS Grid)"),
		field.UUID("current_version_id", uuid.UUID{}).
			Optional().Nillable().
			Annotations(entgql.Skip(entgql.SkipWhereInput)),
	}
}

func (Office) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("office_mls_id").Unique(),
		index.Fields("office_name"),
		index.Fields("office_status"),
		index.Fields("main_office_key"),
		index.Fields("source_modified_at"),
	}
}

func (Office) Edges() []ent.Edge {
	// Office.members and the four inverse Property edges are SOFT KEYS now —
	// see the comment on Property.Edges(). Office self-reference (main_office /
	// branches) is unrelated to the agent/office orphan class and remains.
	return []ent.Edge{
		edge.To("branches", Office.Type).
			From("main_office").
			Field("main_office_key").
			Unique(),
	}
}
