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
		field.UUID("sync_event_id", uuid.UUID{}).
			Annotations(entgql.Skip(entgql.SkipWhereInput)),
		field.UUID("raw_output_id", uuid.UUID{}).Optional().Nillable().
			Annotations(entgql.Skip(entgql.SkipWhereInput)),
	}
}

func (OfficeVersion) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("office_key", "valid_from"),
		index.Fields("sync_event_id"),
		index.Fields("processor_version"),
		index.Fields("office_key").
			Unique().
			Annotations(entsql.IndexWhere("valid_to IS NULL")),
	}
}
