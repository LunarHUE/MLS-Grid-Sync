package processor

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseMember_RequiresMemberKey(t *testing.T) {
	_, err := parseMember([]byte(`{"ModificationTimestamp": "2026-01-15T12:00:00Z"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MemberKey")
}

func TestParseMember_RequiresModificationTimestamp(t *testing.T) {
	_, err := parseMember([]byte(`{"MemberKey": "abc"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ModificationTimestamp")
}

func TestParseMember_RejectsMalformedTimestamp(t *testing.T) {
	_, err := parseMember([]byte(`{
		"MemberKey": "abc",
		"ModificationTimestamp": "not-a-date"
	}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ModificationTimestamp")
}

func TestParseMember_MinimalPayload(t *testing.T) {
	got, err := parseMember([]byte(`{
		"MemberKey": "mem-1",
		"ModificationTimestamp": "2026-01-15T12:00:00Z"
	}`))
	require.NoError(t, err)
	assert.Equal(t, "mem-1", got.MemberKey)
	assert.True(t, got.SourceModifiedAt.Equal(time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)))
	assert.True(t, got.MlgCanView, "MlgCanView defaults true when absent")
	assert.Nil(t, got.ExtendedFields)
}

func TestParseMember_MlgCanViewFalse(t *testing.T) {
	got, err := parseMember([]byte(`{
		"MemberKey": "mem-1",
		"ModificationTimestamp": "2026-01-15T12:00:00Z",
		"MlgCanView": false
	}`))
	require.NoError(t, err)
	assert.False(t, got.MlgCanView)
}

func TestParseMember_ExtendedFieldsCatchAll(t *testing.T) {
	got, err := parseMember([]byte(`{
		"MemberKey": "mem-1",
		"ModificationTimestamp": "2026-01-15T12:00:00Z",
		"ACT_CustomLanguage": "es"
	}`))
	require.NoError(t, err)
	require.NotNil(t, got.ExtendedFields)
	assert.Equal(t, "es", got.ExtendedFields["ACT_CustomLanguage"])
}

func TestParseMember_FullCoverage(t *testing.T) {
	full := buildFullMemberPayload(t)
	raw, err := json.Marshal(full)
	require.NoError(t, err)

	got, err := parseMember(raw)
	require.NoError(t, err)

	v := reflect.ValueOf(got).Elem()
	tp := v.Type()
	skip := map[string]bool{
		"ExtendedFields": true,
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
				"MemberFields.%s is nil — likely missing from parseMember's mapping table", name)
		case reflect.Slice:
			assert.Greater(t, f.Len(), 0,
				"MemberFields.%s is empty — likely missing from parseMember's mapping table", name)
		case reflect.String:
			assert.NotEmpty(t, f.String(), "MemberFields.%s is empty", name)
		case reflect.Struct:
			assert.False(t, f.IsZero(), "MemberFields.%s is zero", name)
		case reflect.Bool:
			// Weak: payload sets MlgCanView=true which matches the parser's
			// default. Asserting true here only checks the field is reachable
			// — the parser-consumes-the-key assertion lives in the dedicated
			// TestParseMember_MlgCanViewFalse test.
			assert.True(t, f.Bool(), "MemberFields.%s false in full-coverage payload", name)
		}
	}
}

func buildFullMemberPayload(t *testing.T) map[string]any {
	t.Helper()
	const ts = "2026-01-15T12:00:00Z"
	return map[string]any{
		"MemberKey":               "MEM-1",
		"ModificationTimestamp":   ts,
		"OriginatingSystemName":   "actris",
		"MlgCanView":              true,
		"MlgCanUse":               []string{"IDX"},
		"MemberMlsId":             "AGT123",
		"MemberFirstName":         "Jane",
		"MemberMiddleName":        "Q",
		"MemberLastName":          "Smith",
		"MemberFullName":          "Jane Q Smith",
		"MemberNamePrefix":        "Ms",
		"MemberNameSuffix":        "Jr",
		"MemberNickname":          "Janey",
		"MemberStatus":            "Active",
		"MemberDirectPhone":       "5125551111",
		"MemberMobilePhone":       "5125552222",
		"MemberHomePhone":         "5125553333",
		"MemberPreferredPhone":    "5125554444",
		"MemberPreferredPhoneExt": "101",
		"MemberOfficePhoneExt":    "202",
		"MemberFax":               "5125559999",
		"MemberAddress1":          "123 Main",
		"MemberAddress2":          "Suite 200",
		"MemberCity":              "Austin",
		"MemberStateOrProvince":   "TX",
		"MemberPostalCode":        "78701",
		"MemberPostalCodePlus4":   "1234",
		"MemberCountry":           "US",
		"MemberCountyOrParish":    "Travis",
		"OfficeKey":               "OFF-1",
		"OfficeMlsId":             "OFC100",
	}
}
