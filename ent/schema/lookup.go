package schema

import (
	"entgo.io/contrib/entgql"
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// Lookup is MLS Grid's enumeration registry. No version table — just upsert
// on each sync. Synced as its own resource.
type Lookup struct{ ent.Schema }

func (Lookup) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entgql.QueryField(),
		entgql.RelayConnection(),
		entsql.Annotation{Table: "lookup"},
	}
}

func (Lookup) Mixin() []ent.Mixin {
	return []ent.Mixin{
		AuditMixin{},
		MLSMetadataMixin{},
	}
}

func (Lookup) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").
			StorageKey("lookup_key"),
		field.String("lookup_name").
			Comment("The name of the lookup category (e.g., Appliances, Cooling)"),
		field.Text("lookup_value").
			Comment("Human-friendly display name as it appears in payloads"),
		field.Text("standard_lookup_value").
			Optional().
			Nillable().
			Comment("Data Dictionary LookupDisplayName"),
	}
}

func (Lookup) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("lookup_name"),
		index.Fields("lookup_name", "lookup_value"),
	}
}
