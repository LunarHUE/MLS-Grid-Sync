package schema

import (
	"entgo.io/contrib/entgql"
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type PropertyRoomVersion struct{ ent.Schema }

func (PropertyRoomVersion) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entgql.QueryField(),
		entgql.RelayConnection(),
		entsql.Annotation{Table: "property_room_version"},
	}
}

func (PropertyRoomVersion) Mixin() []ent.Mixin {
	return []ent.Mixin{
		VersionMixin{},
		MLSMetadataMixin{},
		PropertyRoomDataMixin{},
		ExtendedFieldsMixin{},
	}
}

func (PropertyRoomVersion) Fields() []ent.Field {
	return []ent.Field{
		field.String("room_key"),
		// sync_event_id and raw_output_id now live on VersionMixin.
	}
}

func (PropertyRoomVersion) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("room_key", "valid_from"),
		// sync_event_id and processor_version indexes now live on VersionMixin.
		index.Fields("room_key").
			Unique().
			Annotations(entsql.IndexWhere("valid_to IS NULL")),
	}
}
