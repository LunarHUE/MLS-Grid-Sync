package schema

import (
	"entgo.io/contrib/entgql"
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type OfficeVersion struct{ ent.Schema }

func (OfficeVersion) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entgql.QueryField(),
		entgql.RelayConnection(),
		entsql.Annotation{Table: "office_version"},
	}
}

func (OfficeVersion) Mixin() []ent.Mixin {
	return []ent.Mixin{
		VersionMixin{},
		MLSMetadataMixin{},
		OfficeDataMixin{},
		ExtendedFieldsMixin{},
	}
}

func (OfficeVersion) Fields() []ent.Field {
	return []ent.Field{
		field.String("office_key"),
		// sync_event_id and raw_output_id now live on VersionMixin.
	}
}

func (OfficeVersion) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("office_key", "valid_from"),
		// sync_event_id and processor_version indexes now live on VersionMixin.
		index.Fields("office_key").
			Unique().
			Annotations(entsql.IndexWhere("valid_to IS NULL")),
	}
}
