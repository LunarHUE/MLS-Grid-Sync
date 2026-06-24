package processor

import (
	"encoding/json"
	"strings"
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

// TestRequiredModTimestamp covers the three failure modes the helper exists to
// distinguish. The null/empty case is the regression guard: parseTime returns
// (nil, nil) there, and the historical `fmt.Errorf("...: %w", err)` wrapped a
// nil error and rendered the useless "ModificationTimestamp: %!w(<nil>)".
func TestRequiredModTimestamp(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		_, err := requiredModTimestamp(nil, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "missing required field")
	})

	t.Run("present but null", func(t *testing.T) {
		_, err := requiredModTimestamp(json.RawMessage(`null`), true)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "null or empty")
		assert.NotContains(t, err.Error(), "%!w", "must not wrap a nil error")
	})

	t.Run("present but empty string", func(t *testing.T) {
		_, err := requiredModTimestamp(json.RawMessage(`""`), true)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "null or empty")
		assert.NotContains(t, err.Error(), "%!w")
	})

	t.Run("malformed", func(t *testing.T) {
		_, err := requiredModTimestamp(json.RawMessage(`"not-a-time"`), true)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ModificationTimestamp")
		assert.NotContains(t, err.Error(), "%!w")
	})

	t.Run("valid", func(t *testing.T) {
		got, err := requiredModTimestamp(json.RawMessage(`"2012-03-14T08:30:00Z"`), true)
		require.NoError(t, err)
		assert.True(t, got.Equal(time.Date(2012, 3, 14, 8, 30, 0, 0, time.UTC)))
	})
}

// TestParseProperty_NullModificationTimestamp is the end-to-end regression: a
// present-but-null ModificationTimestamp must produce a clear poison message,
// never the garbage "%!w(<nil>)" the nil-wrap produced before the fix.
func TestParseProperty_NullModificationTimestamp(t *testing.T) {
	_, err := parseProperty([]byte(`{"ListingKey":"x","ModificationTimestamp":null}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "null or empty")
	assert.False(t, strings.Contains(err.Error(), "%!w"),
		"present-but-null timestamp must not render a wrapped-nil error: %q", err.Error())
}
