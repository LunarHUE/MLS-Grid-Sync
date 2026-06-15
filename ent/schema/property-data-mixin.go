package schema

import (
	"entgo.io/contrib/entgql"
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/mixin"
	"github.com/shopspring/decimal"
)

// PropertyDataMixin contains every typed column on Property. Used by both
// the current-state Property schema and the PropertyVersion history schema.
//
// Fields are grouped: identity, status/timestamps, pricing, characteristics,
// location, agents/offices, display flags, RESO arrays, free-text.
//
// All ACT_* Actris fields and rarely-queried RESO fields go in
// `extended_fields` (added via ExtendedFieldsMixin).
type PropertyDataMixin struct{ mixin.Schema }

func (PropertyDataMixin) Fields() []ent.Field {
	return []ent.Field{
		// --- Identity ---
		field.String("listing_id").Optional().Nillable(),
		field.String("parcel_number").Optional().Nillable(),

		// --- Status & timestamps ---
		field.String("mls_status").Optional().Nillable(),
		field.String("standard_status").Optional().Nillable(),
		field.String("major_change_type").Optional().Nillable(),
		field.Time("major_change_timestamp").Optional().Nillable(),
		field.Time("listing_contract_date").Optional().Nillable(),
		field.Time("on_market_timestamp").Optional().Nillable(),
		field.Time("original_entry_timestamp").Optional().Nillable(),
		field.Time("photos_change_timestamp").Optional().Nillable(),
		field.Time("availability_date").Optional().Nillable(),

		// --- Pricing & tax ---
		field.Other("list_price", decimal.Decimal{}).
			Optional().Nillable().
			SchemaType(map[string]string{dialect.Postgres: "numeric(13,2)"}).
			Annotations(entgql.OrderField("LIST_PRICE")),
		field.Other("original_list_price", decimal.Decimal{}).
			Optional().Nillable().
			SchemaType(map[string]string{dialect.Postgres: "numeric(13,2)"}),
		field.Other("previous_list_price", decimal.Decimal{}).
			Optional().Nillable().
			SchemaType(map[string]string{dialect.Postgres: "numeric(13,2)"}),
		field.Other("tax_annual_amount", decimal.Decimal{}).
			Optional().Nillable().
			SchemaType(map[string]string{dialect.Postgres: "numeric(13,2)"}),
		field.Int64("tax_assessed_value").Optional().Nillable(),
		field.Int16("tax_year").Optional().Nillable(),

		// --- Type & characteristics ---
		field.String("property_type").Optional().Nillable(),
		field.String("property_sub_type").Optional().Nillable(),
		field.Bool("new_construction_yn").Optional().Nillable(),
		field.Int16("bedrooms_total").Optional().Nillable(),
		field.Int16("bathrooms_total_integer").Optional().Nillable(),
		field.Int16("bathrooms_full").Optional().Nillable(),
		field.Int16("bathrooms_half").Optional().Nillable(),
		field.Int16("main_level_bedrooms").Optional().Nillable(),
		field.Other("living_area", decimal.Decimal{}).
			Optional().Nillable().
			SchemaType(map[string]string{dialect.Postgres: "numeric(13,2)"}),
		field.Other("building_area_total", decimal.Decimal{}).
			Optional().Nillable().
			SchemaType(map[string]string{dialect.Postgres: "numeric(13,2)"}),
		field.Other("lot_size_acres", decimal.Decimal{}).
			Optional().Nillable().
			SchemaType(map[string]string{dialect.Postgres: "numeric(15,4)"}),
		field.Other("lot_size_square_feet", decimal.Decimal{}).
			Optional().Nillable().
			SchemaType(map[string]string{dialect.Postgres: "numeric(13,2)"}),
		field.Int16("stories_total").Optional().Nillable(),
		field.Int16("year_built").Optional().Nillable(),
		field.Other("garage_spaces", decimal.Decimal{}).
			Optional().Nillable().
			SchemaType(map[string]string{dialect.Postgres: "numeric(13,2)"}),
		field.Other("covered_spaces", decimal.Decimal{}).
			Optional().Nillable().
			SchemaType(map[string]string{dialect.Postgres: "numeric(13,2)"}),
		field.Other("parking_total", decimal.Decimal{}).
			Optional().Nillable().
			SchemaType(map[string]string{dialect.Postgres: "numeric(13,2)"}),
		field.Int16("fireplaces_total").Optional().Nillable(),
		field.Bool("pool_private_yn").Optional().Nillable(),
		field.Bool("waterfront_yn").Optional().Nillable(),
		field.Bool("view_yn").Optional().Nillable(),
		field.Bool("horse_yn").Optional().Nillable(),

		// --- Address components ---
		field.String("street_number").Optional().Nillable(),
		field.Int32("street_number_numeric").Optional().Nillable(),
		field.String("street_name").Optional().Nillable(),
		field.String("street_suffix").Optional().Nillable(),
		field.String("street_dir_prefix").Optional().Nillable(),
		field.String("street_dir_suffix").Optional().Nillable(),
		field.String("unit_number").Optional().Nillable(),
		field.String("unparsed_address").Optional().Nillable(),
		field.String("city").Optional().Nillable().
			Annotations(entgql.OrderField("CITY")),
		field.String("state_or_province").Optional().Nillable(),
		field.String("postal_code").Optional().Nillable(),
		field.String("postal_code_plus4").Optional().Nillable(),
		field.String("country").Optional().Nillable(),
		field.String("county_or_parish").Optional().Nillable(),
		field.String("subdivision_name").Optional().Nillable(),
		field.String("mls_area_major").Optional().Nillable(),

		// --- Geo (lat/lon — geography column added via migration hook) ---
		field.Other("latitude", decimal.Decimal{}).
			Optional().Nillable().
			SchemaType(map[string]string{dialect.Postgres: "numeric(11,8)"}),
		field.Other("longitude", decimal.Decimal{}).
			Optional().Nillable().
			SchemaType(map[string]string{dialect.Postgres: "numeric(11,8)"}),

		// --- Schools ---
		field.String("elementary_school").Optional().Nillable(),
		field.String("middle_or_junior_school").Optional().Nillable(),
		field.String("high_school").Optional().Nillable(),
		field.String("high_school_district").Optional().Nillable(),

		// --- Agent FKs (4 roles, all point to Member) ---
		field.String("list_agent_key").Optional().Nillable(),
		field.String("list_agent_mls_id").Optional().Nillable(),
		field.String("co_list_agent_key").Optional().Nillable(),
		field.String("co_list_agent_mls_id").Optional().Nillable(),
		field.String("buyer_agent_key").Optional().Nillable(),
		field.String("buyer_agent_mls_id").Optional().Nillable(),
		field.String("co_buyer_agent_key").Optional().Nillable(),
		field.String("co_buyer_agent_mls_id").Optional().Nillable(),

		// --- Office FKs (4 roles, all point to Office) ---
		field.String("list_office_key").Optional().Nillable(),
		field.String("list_office_mls_id").Optional().Nillable(),
		field.String("co_list_office_key").Optional().Nillable(),
		field.String("co_list_office_mls_id").Optional().Nillable(),
		field.String("buyer_office_key").Optional().Nillable(),
		field.String("buyer_office_mls_id").Optional().Nillable(),
		field.String("co_buyer_office_key").Optional().Nillable(),
		field.String("co_buyer_office_mls_id").Optional().Nillable(),

		// --- Internet display flags ---
		field.Bool("internet_entire_listing_display_yn").Optional().Nillable(),
		field.Bool("internet_address_display_yn").Optional().Nillable(),
		field.Bool("internet_automated_valuation_display_yn").Optional().Nillable(),
		field.Bool("internet_consumer_comment_yn").Optional().Nillable(),

		// --- RESO Collection(Edm.String) arrays (text[]) ---
		textArray("appliances"),
		textArray("cooling"),
		textArray("heating"),
		textArray("flooring"),
		textArray("roof"),
		textArray("exterior_features"),
		textArray("interior_features"),
		textArray("parking_features"),
		textArray("pool_features"),
		textArray("view"),
		textArray("waterfront_features"),
		textArray("community_features"),
		textArray("accessibility_features"),
		textArray("utilities"),
		textArray("sewer"),
		textArray("water_source"),
		textArray("lot_features"),
		textArray("patio_and_porch_features"),
		textArray("security_features"),
		textArray("construction_materials"),
		textArray("foundation_details"),
		textArray("levels"),
		textArray("fireplace_features"),
		textArray("spa_features"),
		textArray("fencing"),
		textArray("horse_amenities"),
		textArray("window_features"),
		textArray("pets_allowed"),
		textArray("disclosures"),
		textArray("property_condition"),
		textArray("special_listing_conditions"),
		textArray("green_energy_efficient"),
		textArray("green_sustainability"),
		textArray("syndicate_to"),

		// --- Free text & misc ---
		field.Text("public_remarks").Optional().Nillable(),
		field.Text("syndication_remarks").Optional().Nillable(),
		field.Text("directions").Optional().Nillable(),
		field.String("furnished").Optional().Nillable(),
		field.String("direction_faces").Optional().Nillable(),
	}
}
