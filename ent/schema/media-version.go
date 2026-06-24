package schema

import (
	"entgo.io/contrib/entgql"
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type MediaVersion struct{ ent.Schema }

func (MediaVersion) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entgql.QueryField(),
		entgql.RelayConnection(),
		entsql.Annotation{Table: "media_version"},
	}
}

func (MediaVersion) Mixin() []ent.Mixin {
	return []ent.Mixin{
		VersionMixin{},
		MLSMetadataMixin{},
		MediaDataMixin{},
		ExtendedFieldsMixin{},
	}
}

func (MediaVersion) Fields() []ent.Field {
	return []ent.Field{
		field.String("media_key"),
		// sync_event_id and raw_output_id now live on VersionMixin.
	}
}

func (MediaVersion) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("media_key", "valid_from"),
		// sync_event_id and processor_version indexes now live on VersionMixin.
		index.Fields("media_key").
			Unique().
			Annotations(entsql.IndexWhere("valid_to IS NULL")),
	}
}
