package processor

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseOffice_RequiresOfficeKey(t *testing.T) {
	_, err := parseOffice([]byte(`{"ModificationTimestamp": "2026-01-15T12:00:00Z"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "OfficeKey")
}

func TestParseOffice_RequiresModificationTimestamp(t *testing.T) {
	_, err := parseOffice([]byte(`{"OfficeKey": "off-1"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ModificationTimestamp")
}

func TestParseOffice_MinimalPayload(t *testing.T) {
	got, err := parseOffice([]byte(`{
		"OfficeKey": "OFF-1",
		"ModificationTimestamp": "2026-01-15T12:00:00Z"
	}`))
	require.NoError(t, err)
	assert.Equal(t, "OFF-1", got.OfficeKey)
	assert.True(t, got.SourceModifiedAt.Equal(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)))
	assert.True(t, got.MlgCanView)
}

func TestParseOffice_MlgCanViewFalse(t *testing.T) {
	got, err := parseOffice([]byte(`{
		"OfficeKey": "OFF-1",
		"ModificationTimestamp": "2026-01-15T12:00:00Z",
		"MlgCanView": false
	}`))
	require.NoError(t, err)
	assert.False(t, got.MlgCanView)
}

func TestParseOffice_ExtendedFieldsCatchAll(t *testing.T) {
	got, err := parseOffice([]byte(`{
		"OfficeKey": "OFF-1",
		"ModificationTimestamp": "2026-01-15T12:00:00Z",
		"ACT_OfficeRegion": "Central"
	}`))
	require.NoError(t, err)
	require.NotNil(t, got.ExtendedFields)
	assert.Equal(t, "Central", got.ExtendedFields["ACT_OfficeRegion"])
}

func TestParseOffice_FullCoverage(t *testing.T) {
	full := buildFullOfficePayload(t)
	raw, err := json.Marshal(full)
	require.NoError(t, err)

	got, err := parseOffice(raw)
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
				"OfficeFields.%s is nil — missing from parseOffice's mapping", name)
		case reflect.Slice:
			assert.Greater(t, f.Len(), 0, "OfficeFields.%s empty", name)
		case reflect.String:
			assert.NotEmpty(t, f.String(), "OfficeFields.%s empty", name)
		case reflect.Struct:
			assert.False(t, f.IsZero(), "OfficeFields.%s zero", name)
		case reflect.Bool:
			assert.True(t, f.Bool(), "OfficeFields.%s false in full-coverage payload", name)
		}
	}
}

func buildFullOfficePayload(t *testing.T) map[string]any {
	t.Helper()
	const ts = "2026-01-15T12:00:00Z"
	return map[string]any{
		"OfficeKey":                   "OFF-1",
		"ModificationTimestamp":       ts,
		"OriginatingSystemName":       "actris",
		"MlgCanView":                  true,
		"MlgCanUse":                   []string{"IDX"},
		"OfficeMlsId":                 "OFC100",
		"OfficeName":                  "Highpoint Realty",
		"OfficeStatus":                "Active",
		"OfficeType":                  "Brokerage",
		"OfficePhone":                 "5125550000",
		"OfficePhoneExt":              "100",
		"OfficeFax":                   "5125559999",
		"OfficeAddress1":              "1 Main",
		"OfficeAddress2":              "Suite 1",
		"OfficeCity":                  "Austin",
		"OfficeStateOrProvince":       "TX",
		"OfficePostalCode":            "78701",
		"OfficePostalCodePlus4":       "1234",
		"OfficeCountyOrParish":        "Travis",
		"OfficeCorporateLicense":      "LIC-1",
		"OfficeNationalAssociationId": "NAR-1",
		"MainOfficeKey":               "OFF-MAIN",
		"MainOfficeMlsId":             "OFC-MAIN",
		"OfficeBrokerKey":             "MEM-BRK",
		"OfficeBrokerMlsId":           "BRK-1",
		"OfficeManagerKey":            "MEM-MGR",
		"IdxOfficeParticipationYN":    true,
		"PhotosChangeTimestamp":       ts,
	}
}
