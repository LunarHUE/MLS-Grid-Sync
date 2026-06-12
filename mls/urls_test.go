package mls

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func decodeFilter(t *testing.T, u string) string {
	t.Helper()
	parsed, err := url.Parse(u)
	require.NoError(t, err)
	return parsed.Query().Get("$filter")
}

// TestDeltaURL_UsesGENotGT pins Phase 4 §7's boundary-inclusive fix.
// gt skips the boundary record across two consecutive runs sharing the
// same ModificationTimestamp; ge re-fetches it and lets the DB unique
// index dedup. Reverting to gt would re-open the silent-data-loss class.
func TestDeltaURL_UsesGENotGT(t *testing.T) {
	since := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	u := DeltaURL("https://api.example/v2", "actris", ResourceProperty, since)

	filter := decodeFilter(t, u)
	assert.Contains(t, filter, "ModificationTimestamp ge ", "must use ge (boundary-inclusive)")
	assert.NotContains(t, filter, "ModificationTimestamp gt ", "must NOT use gt — that's the bug §7 closes")
}

// TestURL_OrderByAscending pins Phase 4 §7's defense-in-depth ordering
// AND the encoding fix that lets the request survive nginx in front of
// the MLS Grid API. Without $orderby, OData pagination order is
// undefined; with $orderby but a raw space, nginx 400s before OData ever
// sees the request. Both must hold.
func TestURL_OrderByAscending(t *testing.T) {
	since := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name string
		got  string
	}{
		{"initial property", InitialURL("https://api.example/v2", "actris", ResourceProperty)},
		{"initial media", InitialURL("https://api.example/v2", "actris", ResourceMedia)},
		{"delta property", DeltaURL("https://api.example/v2", "actris", ResourceProperty, since)},
		{"delta member", DeltaURL("https://api.example/v2", "actris", ResourceMember, since)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := url.Parse(tc.got)
			require.NoError(t, err, "rendered URL must parse cleanly")
			assert.Equal(t, "ModificationTimestamp asc", parsed.Query().Get("$orderby"),
				"$orderby missing or wrong after decode")
			// Raw spaces in a URL are the exact bug nginx 400s on.
			assert.False(t, strings.Contains(tc.got, " "),
				"rendered URL must not contain raw spaces: %s", tc.got)
		})
	}
}

// TestDiscoveryURL pins the probe shape: no OriginatingSystemName
// filter, no $orderby (avoids the URL-encoding pitfall), and points at
// the Lookup resource (cheapest probe).
func TestDiscoveryURL(t *testing.T) {
	got := DiscoveryURL("https://api.example/v2")
	parsed, err := url.Parse(got)
	require.NoError(t, err)
	assert.Equal(t, "/v2/Lookup", parsed.Path)
	assert.NotContains(t, got, "OriginatingSystemName",
		"discovery probe must NOT carry the OriginatingSystemName filter — that's the entire point")
	assert.NotContains(t, got, "$orderby",
		"discovery probe must NOT include $orderby — keeps the encoding surface minimal")
	assert.Contains(t, parsed.Query().Get("$top"), "100",
		"$top should be large enough to surface multiple OriginatingSystemName values in one request")
}

func TestInitialURL_PropertyExpand(t *testing.T) {
	u := InitialURL("https://api.example/v2", "actris", ResourceProperty)
	assert.Contains(t, u, "$expand=Media,Rooms,UnitTypes")

	u2 := InitialURL("https://api.example/v2", "actris", ResourceMember)
	assert.NotContains(t, u2, "$expand=", "$expand is Property-only")
}
