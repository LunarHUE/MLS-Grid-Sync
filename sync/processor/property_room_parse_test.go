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

func TestParsePropertyRoom_RequiresRoomKey(t *testing.T) {
	_, err := parsePropertyRoom([]byte(`{"ListingKey": "LK-1", "ModificationTimestamp": "2026-01-15T12:00:00Z"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "RoomKey")
}

func TestParsePropertyRoom_RequiresListingKey(t *testing.T) {
	_, err := parsePropertyRoom([]byte(`{"RoomKey": "R-1", "ModificationTimestamp": "2026-01-15T12:00:00Z"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ListingKey")
}

func TestParsePropertyRoom_MinimalPayload(t *testing.T) {
	got, err := parsePropertyRoom([]byte(`{
		"RoomKey": "R-1",
		"ListingKey": "LK-1",
		"ModificationTimestamp": "2026-01-15T12:00:00Z"
	}`))
	require.NoError(t, err)
	assert.Equal(t, "R-1", got.RoomKey)
	assert.Equal(t, "LK-1", got.ListingKey)
	assert.True(t, got.MlgCanView)
}

// TestParsePropertyRoom_ExpandedShapeFixture is the post-reset golden
// test. The fixture starts as a synthetic minimal expanded-shape
// payload; the intent is to overwrite it with the real captured payload
// after the 2026-06-11 init re-baseline:
//
//	SELECT payload FROM raw_output
//	 WHERE resource = 'property_rooms'
//	 ORDER BY (SELECT count(*) FROM jsonb_object_keys(payload)) DESC
//	 LIMIT 1;
//
// Assertions pin the contract the parser must honor for the expanded
// shape: parses, SourceModifiedAt is processor-owned (zero from parser),
// RoomKey natively present, ListingKey splitter-injected.
func TestParsePropertyRoom_ExpandedShapeFixture(t *testing.T) {
	payload, err := os.ReadFile(filepath.Join("testdata", "property_rooms", "expanded_shape.json"))
	require.NoError(t, err, "fixture missing — extract per the comment above")

	got, err := parsePropertyRoom(payload)
	require.NoError(t, err, "expanded-shape property_rooms must parse")

	// Timestamp seam — parser must leave SourceModifiedAt zero.
	assert.True(t, got.SourceModifiedAt.IsZero(),
		"parser must leave SourceModifiedAt zero; processor sets it from raw row")

	// Always-present-natively.
	assert.NotEmpty(t, got.RoomKey)

	// Splitter-injected (cross-layer tripwire). Required for the typed
	// PropertyRoom entity's parent linkage; if absent here, the splitter
	// regressed.
	assert.NotEmpty(t, got.ListingKey,
		"ListingKey must be splitter-injected; absence means the splitter regressed")

	// MlgCanView defaults true per decision 1c (expanded children carry
	// no per-child visibility).
	assert.True(t, got.MlgCanView, "MlgCanView defaults true in the expanded shape")
}

func TestParsePropertyRoom_FullCoverage(t *testing.T) {
	full := buildFullPropertyRoomPayload(t)
	raw, err := json.Marshal(full)
	require.NoError(t, err)

	got, err := parsePropertyRoom(raw)
	require.NoError(t, err)

	v := reflect.ValueOf(got).Elem()
	tp := v.Type()
	// SourceModifiedAt is processor-owned (sourced from raw_output, not
	// the payload — see property_room.go's timestamp seam).
	skip := map[string]bool{"ExtendedFields": true, "SourceModifiedAt": true}
	for i := 0; i < tp.NumField(); i++ {
		name := tp.Field(i).Name
		if skip[name] {
			continue
		}
		f := v.Field(i)
		switch f.Kind() {
		case reflect.Ptr:
			assert.False(t, f.IsNil(), "PropertyRoomFields.%s nil — missing mapping", name)
		case reflect.Slice:
			assert.Greater(t, f.Len(), 0, "PropertyRoomFields.%s empty", name)
		case reflect.String:
			assert.NotEmpty(t, f.String(), "PropertyRoomFields.%s empty", name)
		case reflect.Struct:
			assert.False(t, f.IsZero(), "PropertyRoomFields.%s zero", name)
		case reflect.Bool:
			assert.True(t, f.Bool(), "PropertyRoomFields.%s false in full-coverage payload", name)
		}
	}
}

// buildFullPropertyRoomPayload models the EXPANDED-shape property_rooms
// payload — the only shape the parser meets, per audit (raw_output
// 'property_rooms' inventory, 2026-06-11, n=148,360). Splitter-injected
// fields: ListingKey. Tolerated-when-present (0% in corpus, kept for
// parser code-path coverage): MlgCanView, MlgCanUse,
// OriginatingSystemName, RoomFeatures. Deliberately ABSENT:
// ModificationTimestamp (decision 1a / timestamp seam).
func buildFullPropertyRoomPayload(t *testing.T) map[string]any {
	t.Helper()
	return map[string]any{
		"RoomKey":               "R-1",
		"ListingKey":            "LK-1",
		"OriginatingSystemName": "actris",
		"MlgCanView":            true,
		"MlgCanUse":             []string{"IDX"},
		"RoomType":              "Bedroom",
		"RoomLevel":             "Main",
		"RoomFeatures":          []string{"Walk-in Closet"},
	}
}
