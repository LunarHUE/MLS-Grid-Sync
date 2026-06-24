package schema

import (
	"entgo.io/contrib/entgql"
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
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
		// sync_event_id and raw_output_id now live on VersionMixin.
	}
}

func (MemberVersion) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("member_key", "valid_from"),
		// sync_event_id and processor_version indexes now live on VersionMixin.
		index.Fields("member_key").
			Unique().
			Annotations(entsql.IndexWhere("valid_to IS NULL")),
	}
}
