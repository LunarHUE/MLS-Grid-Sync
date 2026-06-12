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
		field.UUID("sync_event_id", uuid.UUID{}).
			Annotations(entgql.Skip(entgql.SkipWhereInput)),
		field.UUID("raw_output_id", uuid.UUID{}).Optional().Nillable().
			Annotations(entgql.Skip(entgql.SkipWhereInput)),
	}
}

func (MediaVersion) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("media_key", "valid_from"),
		index.Fields("sync_event_id"),
		index.Fields("processor_version"),
		index.Fields("media_key").
			Unique().
			Annotations(entsql.IndexWhere("valid_to IS NULL")),
	}
}
