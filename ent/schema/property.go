package schema

import (
	"entgo.io/contrib/entgql"
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type Property struct{ ent.Schema }

func (Property) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entgql.QueryField(),
		entgql.RelayConnection(),
		entsql.Annotation{Table: "property"},
	}
}

func (Property) Mixin() []ent.Mixin {
	return []ent.Mixin{
		AuditMixin{},
		MLSMetadataMixin{},
		PropertyDataMixin{},
		ExtendedFieldsMixin{},
		CurrentVersionMixin{},
	}
}

func (Property) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").
			StorageKey("listing_key").
			Comment("Natural PK: the RESO ListingKey"),
		// current_version_id now lives on CurrentVersionMixin.
	}
}

func (Property) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("listing_id").Unique(),
		index.Fields("mls_status"),
		index.Fields("standard_status"),
		index.Fields("property_type"),
		index.Fields("property_sub_type"),
		index.Fields("list_price"),
		index.Fields("bedrooms_total"),
		index.Fields("bathrooms_total_integer"),
		index.Fields("living_area"),
		index.Fields("year_built"),
		index.Fields("city"),
		index.Fields("state_or_province"),
		index.Fields("postal_code"),
		index.Fields("county_or_parish"),
		index.Fields("subdivision_name"),
		index.Fields("mls_area_major"),
		index.Fields("major_change_timestamp"),
		index.Fields("source_modified_at"),
		index.Fields("list_agent_key"),
		index.Fields("co_list_agent_key"),
		index.Fields("buyer_agent_key"),
		index.Fields("co_buyer_agent_key"),
		index.Fields("list_office_key"),
		index.Fields("co_list_office_key"),
		index.Fields("buyer_office_key"),
		index.Fields("co_buyer_office_key"),
		// NOTE: GIN indexes on text[] columns (appliances, cooling, etc.) and
		// the GIST index on the location column must be added via migration
		// hook — Ent doesn't support these natively. See README.
	}
}

func (Property) Edges() []ent.Edge {
	// Agent and office references (list/co-list/buyer/co-buyer × member/office)
	// are SOFT KEYS: the *_key columns on PropertyDataMixin link to Member/Office
	// without an ent edge or DB FK. Real MLS feeds routinely reference agents
	// and offices that are absent from the feed (retired, transferred, out of
	// subscription) — a hard FK would halt Property ingest on those rows and a
	// park-and-relink pattern would never fire because the entity never arrives.
	// Forward-direction GraphQL fields are reintroduced via manual resolvers in
	// graph/extensions.graphql with mlg_can_view=true filtering.
	return []ent.Edge{
		// Child collections (transient orphans use the parking pattern).
		edge.To("rooms", PropertyRoom.Type),
		edge.To("unit_types", PropertyUnitType.Type),
		edge.To("open_houses", OpenHouse.Type),
	}
}
