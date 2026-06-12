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

type OpenHouseVersion struct{ ent.Schema }

func (OpenHouseVersion) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entgql.QueryField(),
		entgql.RelayConnection(),
		entsql.Annotation{Table: "open_house_version"},
	}
}

func (OpenHouseVersion) Mixin() []ent.Mixin {
	return []ent.Mixin{
		VersionMixin{},
		MLSMetadataMixin{},
		OpenHouseDataMixin{},
		ExtendedFieldsMixin{},
	}
}

func (OpenHouseVersion) Fields() []ent.Field {
	return []ent.Field{
		field.String("open_house_key"),
		field.UUID("sync_event_id", uuid.UUID{}),
		field.UUID("raw_output_id", uuid.UUID{}).Optional().Nillable(),
	}
}

func (OpenHouseVersion) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("open_house_key", "valid_from"),
		index.Fields("sync_event_id"),
		index.Fields("processor_version"),
		index.Fields("open_house_key").
			Unique().
			Annotations(entsql.IndexWhere("valid_to IS NULL")),
	}
}
