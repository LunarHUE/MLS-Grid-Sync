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
	"github.com/google/uuid"
)

// PropertyUnitTypeDataMixin: shared columns for current and version.
type PropertyUnitTypeDataMixin struct{ mixin.Schema }

func (PropertyUnitTypeDataMixin) Fields() []ent.Field {
	return []ent.Field{
		field.String("listing_key").
			Comment("FK to property.listing_key"),
		field.Int16("unit_type_beds_total").Optional().Nillable(),
		field.String("unit_type_furnished").Optional().Nillable(),
	}
}

type PropertyUnitType struct{ ent.Schema }

func (PropertyUnitType) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entgql.QueryField(),
		entgql.RelayConnection(),
		entsql.Annotation{Table: "property_unit_type"},
	}
}

func (PropertyUnitType) Mixin() []ent.Mixin {
	return []ent.Mixin{
		AuditMixin{},
		MLSMetadataMixin{},
		PropertyUnitTypeDataMixin{},
		ExtendedFieldsMixin{},
	}
}

func (PropertyUnitType) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").
			StorageKey("unit_type_key").
			Comment("RESO UnitTypeKey"),
		field.UUID("current_version_id", uuid.UUID{}).
			Optional().Nillable(),
		field.String("parent_listing_key").
			Optional().Nillable().
			Comment("Nullable FK to property.listing_key. NULL means parent not yet processed (parked); re-link UPDATE fills it once Property arrives."),
	}
}

func (PropertyUnitType) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("listing_key"),
		index.Fields("parent_listing_key"),
		index.Fields("source_modified_at"),
	}
}

func (PropertyUnitType) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("property", Property.Type).
			Ref("unit_types").
			Field("parent_listing_key").
			Unique(),
	}
}
