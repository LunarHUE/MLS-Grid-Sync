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

// Tests for the composable `geo: GeoFilter` argument on the list queries.
// The three sub-fields reproduce the standalone geo root queries
// (withinBounds↔propertiesInBBox, withinPolygons↔propertiesInMultiPolygon,
// withinRadius↔propertiesNear), and — the point of the whole change — they
// AND-compose with `where` and with address search in a single server-side
// query so the list and the map pins can never diverge. Reuses geoConn, pt,
// square, dtLat/dtLng, and seedPropertyAt from geo_test.go (same package).

// downtownBox is a viewport around downtown that catches a property at
// (dtLat, dtLng) and one ~1.1km north (+0.01°) but not one ~111km north (+1.0°).
func downtownBox() map[string]any {
	return map[string]any{
		"southWest": pt(dtLat-0.01, dtLng-0.01),
		"northEast": pt(dtLat+0.02, dtLng+0.01),
	}
}

const propertiesGeoQuery = `query($where: PropertyWhereInput, $geo: GeoFilter) {
	properties(where: $where, geo: $geo, first: 50) {
		totalCount
		edges { node { id } }
	}
}`

// TestPropertiesGeoArg_AllRegions checks each GeoFilter sub-field narrows the
// plain list to the same set its retired root field did.
func TestPropertiesGeoArg_AllRegions(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedPropertyAt(t, client, "geo-downtown", dtLat, dtLng, true)
	seedPropertyAt(t, client, "geo-1km", dtLat+0.01, dtLng, true)
	seedPropertyAt(t, client, "geo-111km", dtLat+1.0, dtLng, true)

	var data struct {
		Properties geoConn `json:"properties"`
	}

	// withinBounds ↔ propertiesInBBox: downtown + 1km, not the 111km outlier.
	testutil.GQL(t, srv, propertiesGeoQuery, map[string]any{
		"geo": map[string]any{"withinBounds": downtownBox()},
	}, &data)
	assert.Equal(t, 2, data.Properties.TotalCount)
	assert.ElementsMatch(t, []string{"geo-downtown", "geo-1km"}, data.Properties.ids())

	// withinRadius ↔ propertiesNear: 100m catches downtown only.
	testutil.GQL(t, srv, propertiesGeoQuery, map[string]any{
		"geo": map[string]any{"withinRadius": map[string]any{"center": pt(dtLat, dtLng), "radiusMeters": 100.0}},
	}, &data)
	assert.Equal(t, 1, data.Properties.TotalCount)
	assert.Equal(t, []string{"geo-downtown"}, data.Properties.ids())

	// withinPolygons ↔ propertiesInMultiPolygon: two discontiguous boxes pick
	// up downtown and the 111km outlier but not the 1km neighbor between them.
	testutil.GQL(t, srv, propertiesGeoQuery, map[string]any{
		"geo": map[string]any{"withinPolygons": []any{
			square(dtLat, dtLng, 0.005),
			square(dtLat+1.0, dtLng, 0.005),
		}},
	}, &data)
	assert.Equal(t, 2, data.Properties.TotalCount)
	assert.ElementsMatch(t, []string{"geo-downtown", "geo-111km"}, data.Properties.ids())
}

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
		"geo":   map[string]any{"withinBounds": downtownBox()},
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
		"geo": map[string]any{"withinBounds": downtownBox()},
	}, &data)
	assert.Equal(t, 1, data.PropertiesByAddress.TotalCount)
	assert.Equal(t, []string{"addr-in"}, data.PropertiesByAddress.ids())
}

// TestPropertiesGeoArg_InvalidArgs covers the GeoFilter-specific validation:
// it must hold exactly one sub-field, and the sub-fields keep the per-region
// validation the root queries had.
func TestPropertiesGeoArg_InvalidArgs(t *testing.T) {
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
			"withinBounds": downtownBox(),
			"withinRadius": map[string]any{"center": pt(dtLat, dtLng), "radiusMeters": 1000.0},
		},
	})
	require.NotEmpty(t, errs)

	// Non-positive radius (per-region validation still applies).
	errs = testutil.GQLExpectError(t, srv, propertiesGeoQuery, map[string]any{
		"geo": map[string]any{"withinRadius": map[string]any{"center": pt(dtLat, dtLng), "radiusMeters": 0.0}},
	})
	require.NotEmpty(t, errs)
}
