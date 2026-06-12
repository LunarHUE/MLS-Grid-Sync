package processor

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseOpenHouse_RequiresOpenHouseKey(t *testing.T) {
	_, err := parseOpenHouse([]byte(`{"ModificationTimestamp": "2026-01-15T12:00:00Z", "ListingKey": "abc"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "OpenHouseKey")
}

func TestParseOpenHouse_RequiresListingKey(t *testing.T) {
	_, err := parseOpenHouse([]byte(`{"OpenHouseKey": "oh-1", "ModificationTimestamp": "2026-01-15T12:00:00Z"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ListingKey")
}

func TestParseOpenHouse_MinimalPayload(t *testing.T) {
	got, err := parseOpenHouse([]byte(`{
		"OpenHouseKey": "oh-1",
		"ListingKey": "LK-1",
		"ModificationTimestamp": "2026-01-15T12:00:00Z"
	}`))
	require.NoError(t, err)
	assert.Equal(t, "oh-1", got.OpenHouseKey)
	assert.Equal(t, "LK-1", got.ListingKey)
	assert.True(t, got.MlgCanView)
}

func TestParseOpenHouse_FullCoverage(t *testing.T) {
	full := buildFullOpenHousePayload(t)
	raw, err := json.Marshal(full)
	require.NoError(t, err)

	got, err := parseOpenHouse(raw)
	require.NoError(t, err)

	v := reflect.ValueOf(got).Elem()
	tp := v.Type()
	skip := map[string]bool{"ExtendedFields": true}
	for i := 0; i < tp.NumField(); i++ {
		name := tp.Field(i).Name
		if skip[name] {
			continue
		}
		f := v.Field(i)
		switch f.Kind() {
		case reflect.Ptr:
			assert.False(t, f.IsNil(),
				"OpenHouseFields.%s is nil — missing from parseOpenHouse mapping", name)
		case reflect.Slice:
			assert.Greater(t, f.Len(), 0, "OpenHouseFields.%s empty", name)
		case reflect.String:
			assert.NotEmpty(t, f.String(), "OpenHouseFields.%s empty", name)
		case reflect.Struct:
			assert.False(t, f.IsZero(), "OpenHouseFields.%s zero", name)
		case reflect.Bool:
			assert.True(t, f.Bool(), "OpenHouseFields.%s false in full-coverage payload", name)
		}
	}
}

func buildFullOpenHousePayload(t *testing.T) map[string]any {
	t.Helper()
	// dt and ts are both load-bearing: dt feeds parseDate-routed fields,
	// ts feeds parseTime-routed fields. A future refactor that collapses
	// them into a single constant re-opens the routing-table drift class
	// the FullCoverage test currently guards. See plan §5.
	const (
		ts = "2026-01-15T12:00:00Z"
		dt = "2026-01-15"
	)
	return map[string]any{
		"OpenHouseKey":          "OH-1",
		"ListingKey":            "LK-1",
		"ListingId":             "MLS-99999",
		"ModificationTimestamp": ts,
		"OriginatingSystemName": "actris",
		"MlgCanView":            true,
		"MlgCanUse":             []string{"IDX"},
		"OpenHouseDate":         dt, // Date (YYYY-MM-DD)
		"OpenHouseStartTime":    ts, // Timestamp (RFC 3339)
		"OpenHouseEndTime":      ts, // Timestamp (RFC 3339)
		"OpenHouseStatus":       "Scheduled",
		"OpenHouseType":         "Public",
	}
}

func TestParseOpenHouse_RejectsMalformedTimestamp(t *testing.T) {
	_, err := parseOpenHouse([]byte(`{
		"OpenHouseKey": "OH-1",
		"ListingKey": "LK-1",
		"ModificationTimestamp": "2026-01-15T12:00:00Z",
		"OpenHouseDate": "not-a-date"
	}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "OpenHouseDate")
}

func TestParseOpenHouse_ExtendedFieldsCatchAll(t *testing.T) {
	got, err := parseOpenHouse([]byte(`{
		"OpenHouseKey": "OH-1",
		"ListingKey": "LK-1",
		"ModificationTimestamp": "2026-01-15T12:00:00Z",
		"ACT_HostsRefreshments": true
	}`))
	require.NoError(t, err)
	require.NotNil(t, got.ExtendedFields)
	assert.Equal(t, true, got.ExtendedFields["ACT_HostsRefreshments"])
}
