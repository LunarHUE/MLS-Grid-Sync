package schema

import (
	"entgo.io/contrib/entgql"
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"entgo.io/ent/schema/mixin"
)

// PropertyRoomDataMixin: shared columns between PropertyRoom and PropertyRoomVersion.
type PropertyRoomDataMixin struct{ mixin.Schema }

func (PropertyRoomDataMixin) Fields() []ent.Field {
	return []ent.Field{
		field.String("listing_key").
			Comment("FK to property.listing_key"),
		field.String("room_type").Optional().Nillable(),
		field.String("room_level").Optional().Nillable(),
		textArray("room_features"),
	}
}

type PropertyRoom struct{ ent.Schema }

func (PropertyRoom) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entgql.QueryField(),
		entgql.RelayConnection(),
		entsql.Annotation{Table: "property_room"},
	}
}

func (PropertyRoom) Mixin() []ent.Mixin {
	return []ent.Mixin{
		AuditMixin{},
		MLSMetadataMixin{},
		PropertyRoomDataMixin{},
		ExtendedFieldsMixin{},
		CurrentVersionMixin{},
	}
}

func (PropertyRoom) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").
			StorageKey("room_key").
			Comment("RESO RoomKey"),
		// current_version_id now lives on CurrentVersionMixin.
		field.String("parent_listing_key").
			Optional().Nillable().
			Comment("Nullable FK to property.listing_key. NULL means parent not yet processed (parked); re-link UPDATE fills it once Property arrives."),
	}
}

func (PropertyRoom) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("listing_key"),
		index.Fields("parent_listing_key"),
		index.Fields("source_modified_at"),
	}
}

func (PropertyRoom) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("property", Property.Type).
			Ref("rooms").
			Field("parent_listing_key").
			Unique(),
	}
}
