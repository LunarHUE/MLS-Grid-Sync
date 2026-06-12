package processor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestParseProperty_DateFieldsFixture is the direct regression for the init
// halt at raw_output_id=019eb3d0-92b7-7347-acb0-eb47dc7a14dd, where a bare
// YYYY-MM-DD ListingContractDate was incorrectly routed through parseTime
// (strict RFC 3339) and poisoned the processor.
//
// testdata/property/date_fields.json starts as a synthetic minimal payload
// hitting the parseDate path. The intent is to overwrite it with the real
// captured production payload extracted via:
//
//	SELECT payload FROM raw_output
//	 WHERE raw_output_id = '019eb3d0-92b7-7347-acb0-eb47dc7a14dd';
//
// The assertion uses 2012-03-14 (from the original error message) — that
// is the load-bearing value for the regression; if the real captured
// payload carries a different ListingContractDate, update the constant.
func TestParseProperty_DateFieldsFixture(t *testing.T) {
	payload, err := os.ReadFile(filepath.Join("testdata", "property", "date_fields.json"))
	require.NoError(t, err, "fixture missing — extract per the comment above")

	got, err := parseProperty(payload)
	require.NoError(t, err, "fixture must parse — this is the regression for the init halt")

	require.NotNil(t, got.ListingContractDate, "ListingContractDate must populate from a bare YYYY-MM-DD")
	want := time.Date(2012, 3, 14, 0, 0, 0, 0, time.UTC)
	assert.True(t, got.ListingContractDate.Equal(want),
		"want %s, got %s — Date routed through parseTime would silently fail here", want, got.ListingContractDate)
	assert.Equal(t, time.UTC, got.ListingContractDate.Location(), "midnight-UTC encoding for date-only values")
}

func TestParseProperty_RequiresListingKey(t *testing.T) {
	_, err := parseProperty([]byte(`{"ModificationTimestamp": "2026-01-15T12:00:00Z"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ListingKey")
}

func TestParseProperty_RequiresModificationTimestamp(t *testing.T) {
	_, err := parseProperty([]byte(`{"ListingKey": "abc"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ModificationTimestamp")
}

func TestParseProperty_RejectsBadJSON(t *testing.T) {
	_, err := parseProperty([]byte(`not json`))
	require.Error(t, err)
}

func TestParseProperty_RejectsMalformedTimestamp(t *testing.T) {
	_, err := parseProperty([]byte(`{
		"ListingKey": "abc",
		"ModificationTimestamp": "not-a-date"
	}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ModificationTimestamp")
}

func TestParseProperty_RejectsDecimalParseError(t *testing.T) {
	_, err := parseProperty([]byte(`{
		"ListingKey": "abc",
		"ModificationTimestamp": "2026-01-15T12:00:00Z",
		"ListPrice": "house"
	}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ListPrice")
}

func TestParseProperty_MinimalPayload(t *testing.T) {
	payload := []byte(`{
		"ListingKey": "list-1",
		"ModificationTimestamp": "2026-01-15T12:00:00Z"
	}`)
	got, err := parseProperty(payload)
	require.NoError(t, err)

	assert.Equal(t, "list-1", got.ListingKey)
	assert.True(t, got.SourceModifiedAt.Equal(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)))
	assert.True(t, got.MlgCanView, "MlgCanView defaults true when absent")
	assert.Nil(t, got.ExtendedFields)
}

func TestParseProperty_MlgCanViewFalse(t *testing.T) {
	payload := []byte(`{
		"ListingKey": "list-1",
		"ModificationTimestamp": "2026-01-15T12:00:00Z",
		"MlgCanView": false
	}`)
	got, err := parseProperty(payload)
	require.NoError(t, err)
	assert.False(t, got.MlgCanView)
}

func TestParseProperty_NullDecimal(t *testing.T) {
	payload := []byte(`{
		"ListingKey": "list-1",
		"ModificationTimestamp": "2026-01-15T12:00:00Z",
		"ListPrice": null
	}`)
	got, err := parseProperty(payload)
	require.NoError(t, err)
	assert.Nil(t, got.ListPrice, "null decimal → nil pointer")
}

func TestParseProperty_EmptyArray(t *testing.T) {
	payload := []byte(`{
		"ListingKey": "list-1",
		"ModificationTimestamp": "2026-01-15T12:00:00Z",
		"Appliances": []
	}`)
	got, err := parseProperty(payload)
	require.NoError(t, err)
	assert.Nil(t, got.Appliances, "empty array collapses to nil per parser policy")
}

func TestParseProperty_DecimalPreservesPrecision(t *testing.T) {
	// 8-decimal lat/lon shouldn't get rounded.
	payload := []byte(`{
		"ListingKey": "list-1",
		"ModificationTimestamp": "2026-01-15T12:00:00Z",
		"Latitude": 30.12345678,
		"Longitude": -97.87654321,
		"ListPrice": "1234567.89"
	}`)
	got, err := parseProperty(payload)
	require.NoError(t, err)

	require.NotNil(t, got.Latitude)
	require.NotNil(t, got.Longitude)
	require.NotNil(t, got.ListPrice)

	assert.Equal(t, "30.12345678", got.Latitude.String())
	assert.Equal(t, "-97.87654321", got.Longitude.String())
	assert.Equal(t, "1234567.89", got.ListPrice.String())
}

func TestParseProperty_ExtendedFieldsCatchAll(t *testing.T) {
	payload := []byte(`{
		"ListingKey": "list-1",
		"ModificationTimestamp": "2026-01-15T12:00:00Z",
		"ACT_SomeCustomField": "custom-value",
		"ACT_AnotherOne": 42
	}`)
	got, err := parseProperty(payload)
	require.NoError(t, err)
	require.NotNil(t, got.ExtendedFields)
	assert.Equal(t, "custom-value", got.ExtendedFields["ACT_SomeCustomField"])
	assert.InDelta(t, 42.0, got.ExtendedFields["ACT_AnotherOne"], 0)
}

func TestParseProperty_FullCoverage(t *testing.T) {
	// Every typed RESO field on Property gets a non-zero value. After parsing,
	// no nil pointer / no zero scalar / no empty slice should remain on
	// PropertyFields (with documented exceptions for entity-identity fields).
	// Catches "added a column to the schema, forgot to map it in the parser."
	full := buildFullPropertyPayload(t)
	raw, err := json.Marshal(full)
	require.NoError(t, err)

	got, err := parseProperty(raw)
	require.NoError(t, err)

	// Verify each field on PropertyFields is non-zero.
	v := reflect.ValueOf(got).Elem()
	tp := v.Type()
	// Fields that legitimately do not appear in the synthesized payload.
	skip := map[string]bool{
		"ExtendedFields": true, // by design — only populated with truly unmapped keys
	}
	for i := 0; i < tp.NumField(); i++ {
		name := tp.Field(i).Name
		if skip[name] {
			continue
		}
		f := v.Field(i)
		switch f.Kind() {
		case reflect.Ptr:
			assert.False(t, f.IsNil(),
				"PropertyFields.%s is nil — likely missing from parseProperty's mapping table", name)
		case reflect.Slice:
			assert.Greater(t, f.Len(), 0,
				"PropertyFields.%s is empty — likely missing from parseProperty's mapping table", name)
		case reflect.String:
			assert.NotEmpty(t, f.String(),
				"PropertyFields.%s is empty — required field missing", name)
		case reflect.Struct:
			// time.Time, decimal.Decimal value type, etc — should not be zero
			assert.False(t, f.IsZero(),
				"PropertyFields.%s is zero", name)
		case reflect.Bool:
			// MlgCanView (the only value-typed bool — every other YN bool
			// is *bool and caught by the Ptr case above). Asserting true
			// matches the payload but is weak because the parser's default
			// is also true. The dedicated MlgCanViewFalse test exercises
			// the actual mapping branch.
			assert.True(t, f.Bool(), "PropertyFields.%s false in full-coverage payload", name)
		}
	}
}

// buildFullPropertyPayload constructs a JSON payload that exercises every
// RESO key the parser knows about. Whenever a new column is added to
// PropertyDataMixin, both parseProperty's mapping table and this helper need
// to be updated — the FullCoverage test will fail loudly if either drifts.
func buildFullPropertyPayload(t *testing.T) map[string]any {
	t.Helper()
	// dt and ts are both load-bearing: dt feeds parseDate-routed fields,
	// ts feeds parseTime-routed fields. A future refactor that collapses
	// them into a single constant re-opens the routing-table drift class
	// the FullCoverage test currently guards. See plan §5.
	const (
		ts = "2026-01-15T12:00:00Z"
		dt = "2026-01-15"
	)
	arr := []string{"X"}
	return map[string]any{
		// Required
		"ListingKey":            "FULL-1",
		"ModificationTimestamp": ts,

		// MLSMetadataMixin
		"OriginatingSystemName": "actris",
		"MlgCanView":            true,
		"MlgCanUse":             []string{"IDX", "VOW"},

		// Identity
		"ListingId":    "MLS-99999",
		"ParcelNumber": "PARCEL-1",

		// Status & timestamps
		"MlsStatus":              "Active",
		"StandardStatus":         "Active",
		"MajorChangeType":        "Price Reduction",
		"MajorChangeTimestamp":   ts, // Timestamp (RFC 3339)
		"ListingContractDate":    dt, // Date (YYYY-MM-DD)
		"OnMarketTimestamp":      ts, // Timestamp (RFC 3339)
		"OriginalEntryTimestamp": ts, // Timestamp (RFC 3339)
		"PhotosChangeTimestamp":  ts, // Timestamp (RFC 3339)
		"AvailabilityDate":       dt, // Date (YYYY-MM-DD)

		// Pricing & tax
		"ListPrice":         "499000.00",
		"OriginalListPrice": "525000.00",
		"PreviousListPrice": "510000.00",
		"TaxAnnualAmount":   "12345.67",
		"TaxAssessedValue":  450000,
		"TaxYear":           2025,

		// Characteristics
		"PropertyType":          "Residential",
		"PropertySubType":       "SingleFamilyResidence",
		"NewConstructionYN":     false,
		"BedroomsTotal":         4,
		"BathroomsTotalInteger": 3,
		"BathroomsFull":         2,
		"BathroomsHalf":         1,
		"MainLevelBedrooms":     1,
		"LivingArea":            "2500.00",
		"BuildingAreaTotal":     "2800.00",
		"LotSizeAcres":          "0.2500",
		"LotSizeSquareFeet":     "10890.00",
		"StoriesTotal":          2,
		"YearBuilt":             2010,
		"GarageSpaces":          "2.00",
		"CoveredSpaces":         "2.00",
		"ParkingTotal":          "4.00",
		"FireplacesTotal":       1,
		"PoolPrivateYN":         true,
		"WaterfrontYN":          false,
		"ViewYN":                true,
		"HorseYN":               false,

		// Address
		"StreetNumber":        "123",
		"StreetNumberNumeric": 123,
		"StreetName":          "Main",
		"StreetSuffix":        "St",
		"StreetDirPrefix":     "N",
		"StreetDirSuffix":     "W",
		"UnitNumber":          "A",
		"UnparsedAddress":     "123 N Main St W Unit A, Austin TX 78701",
		"City":                "Austin",
		"StateOrProvince":     "TX",
		"PostalCode":          "78701",
		"PostalCodePlus4":     "1234",
		"Country":             "US",
		"CountyOrParish":      "Travis",
		"SubdivisionName":     "Downtown",
		"MLSAreaMajor":        "1A",

		// Geo
		"Latitude":  "30.26715",
		"Longitude": "-97.74306",

		// Schools
		"ElementarySchool":     "Lee",
		"MiddleOrJuniorSchool": "OHenry",
		"HighSchool":           "Austin",
		"HighSchoolDistrict":   "Austin ISD",

		// Agent keys
		"ListAgentKey":      "AK-1",
		"ListAgentMlsId":    "AMID-1",
		"CoListAgentKey":    "AK-2",
		"CoListAgentMlsId":  "AMID-2",
		"BuyerAgentKey":     "AK-3",
		"BuyerAgentMlsId":   "AMID-3",
		"CoBuyerAgentKey":   "AK-4",
		"CoBuyerAgentMlsId": "AMID-4",

		// Office keys
		"ListOfficeKey":      "OK-1",
		"ListOfficeMlsId":    "OMID-1",
		"CoListOfficeKey":    "OK-2",
		"CoListOfficeMlsId":  "OMID-2",
		"BuyerOfficeKey":     "OK-3",
		"BuyerOfficeMlsId":   "OMID-3",
		"CoBuyerOfficeKey":   "OK-4",
		"CoBuyerOfficeMlsId": "OMID-4",

		// Internet display flags
		"InternetEntireListingDisplayYN":      true,
		"InternetAddressDisplayYN":            true,
		"InternetAutomatedValuationDisplayYN": true,
		"InternetConsumerCommentYN":           true,

		// RESO arrays
		"Appliances":               arr,
		"Cooling":                  arr,
		"Heating":                  arr,
		"Flooring":                 arr,
		"Roof":                     arr,
		"ExteriorFeatures":         arr,
		"InteriorFeatures":         arr,
		"ParkingFeatures":          arr,
		"PoolFeatures":             arr,
		"View":                     arr,
		"WaterfrontFeatures":       arr,
		"CommunityFeatures":        arr,
		"AccessibilityFeatures":    arr,
		"Utilities":                arr,
		"Sewer":                    arr,
		"WaterSource":              arr,
		"LotFeatures":              arr,
		"PatioAndPorchFeatures":    arr,
		"SecurityFeatures":         arr,
		"ConstructionMaterials":    arr,
		"FoundationDetails":        arr,
		"Levels":                   arr,
		"FireplaceFeatures":        arr,
		"SpaFeatures":              arr,
		"Fencing":                  arr,
		"HorseAmenities":           arr,
		"WindowFeatures":           arr,
		"PetsAllowed":              arr,
		"Disclosures":              arr,
		"PropertyCondition":        arr,
		"SpecialListingConditions": arr,
		"GreenEnergyEfficient":     arr,
		"GreenSustainability":      arr,
		"SyndicateTo":              arr,

		// Free text
		"PublicRemarks":      "Beautiful home.",
		"SyndicationRemarks": "Syndicate me.",
		"Directions":         "Go north.",
		"Furnished":          "Unfurnished",
		"DirectionFaces":     "South",
	}
}

// Compile-time use of decimal so the import is not dropped if the
// precision-preserving test is ever removed.
var _ = decimal.Zero
