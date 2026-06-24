package schema

import (
	"entgo.io/contrib/entgql"
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"entgo.io/ent/schema/mixin"
)

// MemberDataMixin: shared columns between Member and MemberVersion.
// NOTE: member_key is NOT here — it's the PK on the current entity (defined
// via StorageKey on the id field) and a plain column on the version table.
type MemberDataMixin struct{ mixin.Schema }

func (MemberDataMixin) Fields() []ent.Field {
	return []ent.Field{
		field.String("member_mls_id").Optional().Nillable().
			Comment("MLS-visible identifier (e.g. ABC123); unique alt-key"),
		// Names
		field.String("member_first_name").Optional().Nillable(),
		field.String("member_middle_name").Optional().Nillable(),
		field.String("member_last_name").Optional().Nillable(),
		field.String("member_full_name").Optional().Nillable(),
		field.String("member_name_prefix").Optional().Nillable(),
		field.String("member_name_suffix").Optional().Nillable(),
		field.String("member_nickname").Optional().Nillable(),
		// Status
		field.String("member_status").Optional().Nillable(),
		// Phones
		field.String("member_direct_phone").Optional().Nillable(),
		field.String("member_mobile_phone").Optional().Nillable(),
		field.String("member_home_phone").Optional().Nillable(),
		field.String("member_preferred_phone").Optional().Nillable(),
		field.String("member_preferred_phone_ext").Optional().Nillable(),
		field.String("member_office_phone_ext").Optional().Nillable(),
		field.String("member_fax").Optional().Nillable(),
		// Address
		field.String("member_address1").Optional().Nillable(),
		field.String("member_address2").Optional().Nillable(),
		field.String("member_city").Optional().Nillable(),
		field.String("member_state_or_province").Optional().Nillable(),
		field.String("member_postal_code").Optional().Nillable(),
		field.String("member_postal_code_plus4").Optional().Nillable(),
		field.String("member_country").Optional().Nillable(),
		field.String("member_county_or_parish").Optional().Nillable(),
		// Office FK
		field.String("office_key").Optional().Nillable(),
		field.String("office_mls_id").Optional().Nillable(),
	}
}

type Member struct{ ent.Schema }

func (Member) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entgql.QueryField(),
		entgql.RelayConnection(),
		entsql.Annotation{Table: "member"},
	}
}

func (Member) Mixin() []ent.Mixin {
	return []ent.Mixin{
		AuditMixin{},
		MLSMetadataMixin{},
		MemberDataMixin{},
		ExtendedFieldsMixin{},
		CurrentVersionMixin{},
	}
}

func (Member) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").
			StorageKey("member_key").
			Comment("PK = MemberKey (canonical identifier from MLS Grid)"),
		// current_version_id now lives on CurrentVersionMixin.
	}
}

func (Member) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("member_mls_id").Unique(),
		index.Fields("member_status"),
		index.Fields("office_key"),
		index.Fields("source_modified_at"),
		index.Fields("member_full_name"),
		index.Fields("member_last_name"),
	}
}

// Edges: Member.office and the four inverse Property edges are SOFT KEYS now —
// see the comment on Property.Edges(). office_key is a plain column linked via
// a manual GraphQL resolver, and no DB FK constrains it.
func (Member) Edges() []ent.Edge { return nil }
