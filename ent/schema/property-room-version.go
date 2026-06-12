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
		field.UUID("sync_event_id", uuid.UUID{}),
		field.UUID("raw_output_id", uuid.UUID{}).Optional().Nillable(),
	}
}

func (PropertyRoomVersion) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("room_key", "valid_from"),
		index.Fields("sync_event_id"),
		index.Fields("processor_version"),
		index.Fields("room_key").
			Unique().
			Annotations(entsql.IndexWhere("valid_to IS NULL")),
	}
}
