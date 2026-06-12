package processor

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseDate_ValidDateMidnightUTC(t *testing.T) {
	got, err := parseDate(json.RawMessage(`"2012-03-14"`))
	require.NoError(t, err)
	require.NotNil(t, got)
	want := time.Date(2012, 3, 14, 0, 0, 0, 0, time.UTC)
	assert.True(t, got.Equal(want), "want %s, got %s", want, got)
	assert.Equal(t, time.UTC, got.Location(), "must be UTC, not local — drivers will translate via session tz")
}

func TestParseDate_JSONNullReturnsNil(t *testing.T) {
	got, err := parseDate(json.RawMessage(`null`))
	require.NoError(t, err)
	assert.Nil(t, got, "JSON null must collapse to nil — same shape as parseTime")
}

func TestParseDate_EmptyStringReturnsNil(t *testing.T) {
	got, err := parseDate(json.RawMessage(`""`))
	require.NoError(t, err)
	assert.Nil(t, got, "empty string must collapse to nil — same shape as parseTime")
}

// TestParseDate_RejectsRFC3339 is the classification tripwire. A Timestamp
// value arriving in a Date field means a routing-table bug — the parser
// MUST refuse rather than silently truncating to midnight, otherwise the
// bug propagates downstream as data loss.
func TestParseDate_RejectsRFC3339(t *testing.T) {
	_, err := parseDate(json.RawMessage(`"2012-03-14T00:00:00Z"`))
	require.Error(t, err, "RFC 3339 in a Date field must error — that's the classification tripwire")
	assert.Contains(t, err.Error(), "date parse")
}

func TestParseDate_RejectsBadFormats(t *testing.T) {
	bads := []string{
		`"03/14/2012"`,
		`"2012-3-4"`,
		`"2012-03-14 "`,
		`"14-03-2012"`,
		`"2012/03/14"`,
	}
	for _, in := range bads {
		t.Run(in, func(t *testing.T) {
			_, err := parseDate(json.RawMessage(in))
			require.Error(t, err, "strict layout — must reject %s", in)
		})
	}
}

func TestParseDate_RejectsNonStringJSON(t *testing.T) {
	bads := []string{`12345`, `true`, `{"x":1}`, `["2012-03-14"]`}
	for _, in := range bads {
		t.Run(in, func(t *testing.T) {
			_, err := parseDate(json.RawMessage(in))
			require.Error(t, err)
		})
	}
}
