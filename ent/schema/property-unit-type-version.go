package schema

import (
	"entgo.io/contrib/entgql"
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
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
		// sync_event_id and raw_output_id now live on VersionMixin.
	}
}

func (PropertyUnitTypeVersion) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("unit_type_key", "valid_from"),
		// sync_event_id and processor_version indexes now live on VersionMixin.
		index.Fields("unit_type_key").
			Unique().
			Annotations(entsql.IndexWhere("valid_to IS NULL")),
	}
}
