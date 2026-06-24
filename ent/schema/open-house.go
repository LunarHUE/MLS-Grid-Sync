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

// OpenHouseDataMixin: shared columns between OpenHouse and OpenHouseVersion.
type OpenHouseDataMixin struct{ mixin.Schema }

func (OpenHouseDataMixin) Fields() []ent.Field {
	return []ent.Field{
		field.String("listing_key").
			Comment("FK to property.listing_key"),
		field.String("listing_id").Optional().Nillable(),
		field.Time("open_house_date").Optional().Nillable(),
		field.Time("open_house_start_time").Optional().Nillable(),
		field.Time("open_house_end_time").Optional().Nillable(),
		field.String("open_house_status").Optional().Nillable(),
		field.String("open_house_type").Optional().Nillable(),
	}
}

type OpenHouse struct{ ent.Schema }

func (OpenHouse) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entgql.QueryField(),
		entgql.RelayConnection(),
		entsql.Annotation{Table: "open_house"},
	}
}

func (OpenHouse) Mixin() []ent.Mixin {
	return []ent.Mixin{
		AuditMixin{},
		MLSMetadataMixin{},
		OpenHouseDataMixin{},
		ExtendedFieldsMixin{},
		CurrentVersionMixin{},
	}
}

func (OpenHouse) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").
			StorageKey("open_house_key").
			Comment("RESO OpenHouseKey"),
		// current_version_id now lives on CurrentVersionMixin.
		field.String("parent_listing_key").
			Optional().Nillable().
			Comment("Nullable FK to property.listing_key. NULL means parent not yet processed (parked); re-link UPDATE fills it once Property arrives."),
	}
}

func (OpenHouse) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("listing_key"),
		index.Fields("parent_listing_key"),
		index.Fields("open_house_date"),
		index.Fields("open_house_status"),
		index.Fields("source_modified_at"),
	}
}

func (OpenHouse) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("property", Property.Type).
			Ref("open_houses").
			Field("parent_listing_key").
			Unique(),
	}
}
