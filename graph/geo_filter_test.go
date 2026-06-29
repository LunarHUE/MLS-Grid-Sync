package graph_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
)

// Composition tests for the `geo: GeoFilter` argument — the point of the whole
// change is that a region AND-combines with `where` (facets) and with address
// search in a single server-side query, so the list and the map pins can never
// diverge. Region semantics themselves live in geo_test.go; the shared helpers
// (geoConn, pt, propertiesGeoQuery, downtownBoxGeo, withinBoundsGeo) come from
// there too (same package).

// TestPropertiesGeoArg_AndsWithWhere is the composition guarantee for the list:
// a facet predicate (here an id filter) AND a region run in one query.
func TestPropertiesGeoArg_AndsWithWhere(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedPropertyAt(t, client, "geo-keep", dtLat, dtLng, true)
	seedPropertyAt(t, client, "geo-drop", dtLat+0.005, dtLng, true) // also inside the box

	var data struct {
		Properties geoConn `json:"properties"`
	}
	// Both are inside the box, but `where` keeps only one → intersection.
	testutil.GQL(t, srv, propertiesGeoQuery, map[string]any{
		"where": map[string]any{"id": "geo-keep"},
		"geo":   downtownBoxGeo(),
		"first": 50,
	}, &data)
	assert.Equal(t, 1, data.Properties.TotalCount)
	assert.Equal(t, []string{"geo-keep"}, data.Properties.ids())
}

// seedAddressAt seeds a Property with both a postal code (for the deterministic
// ZIP search path) and coordinates, so address search and a geo region can be
// exercised together.
func seedAddressAt(t *testing.T, client *ent.Client, id, zip string, lat, lng float64, visible bool) {
	t.Helper()
	client.Property.Create().
		SetID(id).
		SetSourceModifiedAt(time.Now()).
		SetMlgCanView(visible).
		SetPostalCode(zip).
		SetLatitude(decimal.NewFromFloat(lat)).
		SetLongitude(decimal.NewFromFloat(lng)).
		SaveX(context.Background())
}

// TestPropertiesByAddress_AndsWithGeo is the original bug: an address search
// with an active layer must return address ∩ region, not the whole address
// set. Both rows share the ZIP, but only the one inside the box comes back.
func TestPropertiesByAddress_AndsWithGeo(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedAddressAt(t, client, "addr-in", "78704", dtLat, dtLng, true)      // inside the box
	seedAddressAt(t, client, "addr-out", "78704", dtLat+1.0, dtLng, true) // ~111km north, outside

	query := `query($q: String!, $geo: GeoFilter) {
		propertiesByAddress(query: $q, geo: $geo, first: 50) {
			totalCount
			edges { node { id } }
		}
	}`

	var data struct {
		PropertiesByAddress geoConn `json:"propertiesByAddress"`
	}

	// Address-only (no geo): both ZIP matches come back — establishes the set
	// the region must narrow.
	testutil.GQL(t, srv, query, map[string]any{"q": "78704"}, &data)
	assert.Equal(t, 2, data.PropertiesByAddress.TotalCount)

	// Address AND region: only the row inside the box survives.
	testutil.GQL(t, srv, query, map[string]any{
		"q":   "78704",
		"geo": downtownBoxGeo(),
	}, &data)
	assert.Equal(t, 1, data.PropertiesByAddress.TotalCount)
	assert.Equal(t, []string{"addr-in"}, data.PropertiesByAddress.ids())
}

// TestPropertiesGeoArg_ExactlyOneSubfield covers the GeoFilter-structural rule
// that the per-region validation (in geo_test.go) does not: exactly one of the
// three sub-fields must be set.
func TestPropertiesGeoArg_ExactlyOneSubfield(t *testing.T) {
	t.Parallel()
	srv, _ := testutil.NewTestServer(t)

	// No sub-field set.
	errs := testutil.GQLExpectError(t, srv, propertiesGeoQuery, map[string]any{
		"geo": map[string]any{},
	})
	require.NotEmpty(t, errs)

	// More than one sub-field set.
	errs = testutil.GQLExpectError(t, srv, propertiesGeoQuery, map[string]any{
		"geo": map[string]any{
			"withinBounds": map[string]any{"southWest": pt(dtLat-0.01, dtLng-0.01), "northEast": pt(dtLat+0.02, dtLng+0.01)},
			"withinRadius": map[string]any{"center": pt(dtLat, dtLng), "radiusMeters": 1000.0},
		},
	})
	require.NotEmpty(t, errs)
}
