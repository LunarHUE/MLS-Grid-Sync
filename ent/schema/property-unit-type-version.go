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

type PropertyUnitTypeVersion struct{ ent.Schema }

func (PropertyUnitTypeVersion) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entgql.QueryField(),
		entgql.RelayConnection(),
		entsql.Annotation{Table: "property_unit_type_version"},
	}
}

func (PropertyUnitTypeVersion) Mixin() []ent.Mixin {
	return []ent.Mixin{
		VersionMixin{},
		MLSMetadataMixin{},
		PropertyUnitTypeDataMixin{},
		ExtendedFieldsMixin{},
	}
}

func (PropertyUnitTypeVersion) Fields() []ent.Field {
	return []ent.Field{
		field.String("unit_type_key"),
		field.UUID("sync_event_id", uuid.UUID{}),
		field.UUID("raw_output_id", uuid.UUID{}).Optional().Nillable(),
	}
}

func (PropertyUnitTypeVersion) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("unit_type_key", "valid_from"),
		index.Fields("sync_event_id"),
		index.Fields("processor_version"),
		index.Fields("unit_type_key").
			Unique().
			Annotations(entsql.IndexWhere("valid_to IS NULL")),
	}
}
