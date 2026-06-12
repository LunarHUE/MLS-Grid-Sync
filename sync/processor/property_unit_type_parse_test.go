package processor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePropertyUnitType_RequiresUnitTypeKey(t *testing.T) {
	_, err := parsePropertyUnitType([]byte(`{"ListingKey": "LK-1", "ModificationTimestamp": "2026-01-15T12:00:00Z"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "UnitTypeKey")
}

func TestParsePropertyUnitType_RequiresListingKey(t *testing.T) {
	_, err := parsePropertyUnitType([]byte(`{"UnitTypeKey": "UT-1", "ModificationTimestamp": "2026-01-15T12:00:00Z"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ListingKey")
}

func TestParsePropertyUnitType_MinimalPayload(t *testing.T) {
	got, err := parsePropertyUnitType([]byte(`{
		"UnitTypeKey": "UT-1",
		"ListingKey": "LK-1",
		"ModificationTimestamp": "2026-01-15T12:00:00Z"
	}`))
	require.NoError(t, err)
	assert.Equal(t, "UT-1", got.UnitTypeKey)
	assert.Equal(t, "LK-1", got.ListingKey)
	assert.True(t, got.MlgCanView)
}

// TestParsePropertyUnitType_ExpandedShapeFixture is the post-reset
// golden test. The fixture starts as a synthetic minimal expanded-shape
// payload; the intent is to overwrite it with the real captured payload
// after the 2026-06-11 init re-baseline:
//
//	SELECT payload FROM raw_output
//	 WHERE resource = 'property_unit_types'
//	 ORDER BY (SELECT count(*) FROM jsonb_object_keys(payload)) DESC
//	 LIMIT 1;
//
// Assertions pin the contract the parser must honor for the expanded
// shape: parses, SourceModifiedAt is processor-owned (zero from parser),
// UnitTypeKey natively present, ListingKey splitter-injected.
func TestParsePropertyUnitType_ExpandedShapeFixture(t *testing.T) {
	payload, err := os.ReadFile(filepath.Join("testdata", "property_unit_types", "expanded_shape.json"))
	require.NoError(t, err, "fixture missing — extract per the comment above")

	got, err := parsePropertyUnitType(payload)
	require.NoError(t, err, "expanded-shape property_unit_types must parse")

	// Timestamp seam — parser must leave SourceModifiedAt zero.
	assert.True(t, got.SourceModifiedAt.IsZero(),
		"parser must leave SourceModifiedAt zero; processor sets it from raw row")

	// Always-present-natively.
	assert.NotEmpty(t, got.UnitTypeKey)

	// Splitter-injected (cross-layer tripwire).
	assert.NotEmpty(t, got.ListingKey,
		"ListingKey must be splitter-injected; absence means the splitter regressed")

	// MlgCanView defaults true per decision 1c.
	assert.True(t, got.MlgCanView, "MlgCanView defaults true in the expanded shape")
}

func TestParsePropertyUnitType_FullCoverage(t *testing.T) {
	full := buildFullPropertyUnitTypePayload(t)
	raw, err := json.Marshal(full)
	require.NoError(t, err)

	got, err := parsePropertyUnitType(raw)
	require.NoError(t, err)

	v := reflect.ValueOf(got).Elem()
	tp := v.Type()
	// SourceModifiedAt is processor-owned (sourced from raw_output, not
	// the payload — see property_unit_type.go's timestamp seam).
	skip := map[string]bool{"ExtendedFields": true, "SourceModifiedAt": true}
	for i := 0; i < tp.NumField(); i++ {
		name := tp.Field(i).Name
		if skip[name] {
			continue
		}
		f := v.Field(i)
		switch f.Kind() {
		case reflect.Ptr:
			assert.False(t, f.IsNil(), "PropertyUnitTypeFields.%s nil — missing mapping", name)
		case reflect.Slice:
			assert.Greater(t, f.Len(), 0, "PropertyUnitTypeFields.%s empty", name)
		case reflect.String:
			assert.NotEmpty(t, f.String(), "PropertyUnitTypeFields.%s empty", name)
		case reflect.Struct:
			assert.False(t, f.IsZero(), "PropertyUnitTypeFields.%s zero", name)
		case reflect.Bool:
			assert.True(t, f.Bool(), "PropertyUnitTypeFields.%s false in full-coverage payload", name)
		}
	}
}

// buildFullPropertyUnitTypePayload models the EXPANDED-shape
// property_unit_types payload (audit: raw_output 'property_unit_types'
// inventory, 2026-06-11, n=1,544). Splitter-injected: ListingKey.
// Tolerated-when-present: MlgCanView, MlgCanUse, OriginatingSystemName,
// UnitTypeFurnished. Deliberately ABSENT: ModificationTimestamp
// (decision 1a / timestamp seam).
func buildFullPropertyUnitTypePayload(t *testing.T) map[string]any {
	t.Helper()
	return map[string]any{
		"UnitTypeKey":           "UT-1",
		"ListingKey":            "LK-1",
		"OriginatingSystemName": "actris",
		"MlgCanView":            true,
		"MlgCanUse":             []string{"IDX"},
		"UnitTypeBedsTotal":     2,
		"UnitTypeFurnished":     "Unfurnished",
	}
}
