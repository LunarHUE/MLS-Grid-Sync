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

type MemberVersion struct{ ent.Schema }

func (MemberVersion) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entgql.QueryField(),
		entgql.RelayConnection(),
		entsql.Annotation{Table: "member_version"},
	}
}

func (MemberVersion) Mixin() []ent.Mixin {
	return []ent.Mixin{
		VersionMixin{},
		MLSMetadataMixin{},
		MemberDataMixin{},
		ExtendedFieldsMixin{},
	}
}

func (MemberVersion) Fields() []ent.Field {
	return []ent.Field{
		field.String("member_key"),
		field.UUID("sync_event_id", uuid.UUID{}),
		field.UUID("raw_output_id", uuid.UUID{}).Optional().Nillable(),
	}
}

func (MemberVersion) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("member_key", "valid_from"),
		index.Fields("sync_event_id"),
		index.Fields("processor_version"),
		index.Fields("member_key").
			Unique().
			Annotations(entsql.IndexWhere("valid_to IS NULL")),
	}
}
