package graph_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
)

// Geo-region tests over the composable `properties(geo: GeoFilter)` argument —
// the server-side predicate that replaced the standalone propertiesNear /
// propertiesInBBox / propertiesInMultiPolygon queries. Reference points around
// downtown Austin, TX: downtown (30.2672, -97.7431), ~1.1km north (+0.01°),
// ~111km north (+1.0°). 0.01° of latitude ≈ 1.11km.

const (
	dtLat = 30.2672
	dtLng = -97.7431
)

// geoConn is the wire shape the list queries return.
type geoConn struct {
	TotalCount int `json:"totalCount"`
	PageInfo   struct {
		HasNextPage bool `json:"hasNextPage"`
	} `json:"pageInfo"`
	Edges []struct {
		Node struct {
			ID string `json:"id"`
		} `json:"node"`
	} `json:"edges"`
}

func (c geoConn) ids() []string {
	out := make([]string, len(c.Edges))
	for i, e := range c.Edges {
		out[i] = e.Node.ID
	}
	return out
}

func pt(lat, lng float64) map[string]any {
	return map[string]any{"latitude": lat, "longitude": lng}
}

// square returns a 4-vertex (open) ring of half-side halfDeg around (lat,lng).
func square(lat, lng, halfDeg float64) []any {
	return []any{
		pt(lat-halfDeg, lng-halfDeg),
		pt(lat-halfDeg, lng+halfDeg),
		pt(lat+halfDeg, lng+halfDeg),
		pt(lat+halfDeg, lng-halfDeg),
	}
}

// GeoFilter variable builders — each returns a full `geo` argument value.

func withinRadiusGeo(lat, lng, radiusMeters float64) map[string]any {
	return map[string]any{"withinRadius": map[string]any{"center": pt(lat, lng), "radiusMeters": radiusMeters}}
}

func withinBoundsGeo(swLat, swLng, neLat, neLng float64) map[string]any {
	return map[string]any{"withinBounds": map[string]any{"southWest": pt(swLat, swLng), "northEast": pt(neLat, neLng)}}
}

func withinPolygonsGeo(polys ...[]any) map[string]any {
	rings := make([]any, len(polys))
	for i, p := range polys {
		rings[i] = p
	}
	return map[string]any{"withinPolygons": rings}
}

// downtownBoxGeo is a viewport around downtown that catches a property at
// (dtLat, dtLng) and one ~1.1km north (+0.01°) but not one ~111km north (+1.0°).
func downtownBoxGeo() map[string]any {
	return withinBoundsGeo(dtLat-0.01, dtLng-0.01, dtLat+0.02, dtLng+0.01)
}

// propertiesGeoQuery is the shared list query: `where` and `geo` are both
// optional and AND-combined server-side. `first` is parameterized so the
// pagination test can ask for a partial page.
const propertiesGeoQuery = `query($where: PropertyWhereInput, $geo: GeoFilter, $first: Int) {
	properties(where: $where, geo: $geo, first: $first) {
		totalCount
		pageInfo { hasNextPage }
		edges { node { id } }
	}
}`

func TestPropertiesGeo_Radius(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedPropertyAt(t, client, "geo-downtown", dtLat, dtLng, true)
	seedPropertyAt(t, client, "geo-1km", dtLat+0.01, dtLng, true)
	seedPropertyAt(t, client, "geo-111km", dtLat+1.0, dtLng, true)

	var data struct {
		Properties geoConn `json:"properties"`
	}

	// 5km: downtown + the 1.1km neighbor, not the 111km outlier.
	testutil.GQL(t, srv, propertiesGeoQuery, map[string]any{"geo": withinRadiusGeo(dtLat, dtLng, 5000.0), "first": 50}, &data)
	assert.Equal(t, 2, data.Properties.TotalCount)
	assert.ElementsMatch(t, []string{"geo-downtown", "geo-1km"}, data.Properties.ids())

	// 100m: downtown only.
	testutil.GQL(t, srv, propertiesGeoQuery, map[string]any{"geo": withinRadiusGeo(dtLat, dtLng, 100.0), "first": 50}, &data)
	assert.Equal(t, 1, data.Properties.TotalCount)
	assert.Equal(t, []string{"geo-downtown"}, data.Properties.ids())

	// 200km: all three.
	testutil.GQL(t, srv, propertiesGeoQuery, map[string]any{"geo": withinRadiusGeo(dtLat, dtLng, 200000.0), "first": 50}, &data)
	assert.Equal(t, 3, data.Properties.TotalCount)
}

func TestPropertiesGeo_ExcludesTombstonedAndCoordless(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedPropertyAt(t, client, "geo-vis", dtLat, dtLng, true)
	seedPropertyAt(t, client, "geo-tomb", dtLat, dtLng, false)
	seedProperty(t, client, "geo-nocoords", true) // no lat/lng → NULL geom

	var data struct {
		Properties geoConn `json:"properties"`
	}
	testutil.GQL(t, srv, propertiesGeoQuery, map[string]any{"geo": withinRadiusGeo(dtLat, dtLng, 1000.0), "first": 50}, &data)

	assert.Equal(t, 1, data.Properties.TotalCount)
	assert.Equal(t, []string{"geo-vis"}, data.Properties.ids())
}

func TestPropertiesGeo_Bounds(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedPropertyAt(t, client, "geo-downtown", dtLat, dtLng, true)
	seedPropertyAt(t, client, "geo-1km", dtLat+0.01, dtLng, true)
	seedPropertyAt(t, client, "geo-111km", dtLat+1.0, dtLng, true)

	var data struct {
		Properties geoConn `json:"properties"`
	}
	// Viewport around downtown: catches downtown + 1km, not 111km.
	testutil.GQL(t, srv, propertiesGeoQuery, map[string]any{"geo": downtownBoxGeo(), "first": 50}, &data)

	assert.Equal(t, 2, data.Properties.TotalCount)
	assert.ElementsMatch(t, []string{"geo-downtown", "geo-1km"}, data.Properties.ids())
}

// TestPropertiesGeo_MultiPolygon_Discontiguous is the load-bearing check: two
// separate boxes — one downtown, one ~111km north — must both contribute
// matches in a single query, while a property between them does not. This is
// what a single polygon cannot express.
func TestPropertiesGeo_MultiPolygon_Discontiguous(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedPropertyAt(t, client, "geo-downtown", dtLat, dtLng, true)
	seedPropertyAt(t, client, "geo-north", dtLat+1.0, dtLng, true)   // inside the north box
	seedPropertyAt(t, client, "geo-between", dtLat+0.5, dtLng, true) // in the gap, matches neither

	var data struct {
		Properties geoConn `json:"properties"`
	}
	testutil.GQL(t, srv, propertiesGeoQuery, map[string]any{
		"geo":   withinPolygonsGeo(square(dtLat, dtLng, 0.01), square(dtLat+1.0, dtLng, 0.01)),
		"first": 50,
	}, &data)

	assert.Equal(t, 2, data.Properties.TotalCount)
	assert.ElementsMatch(t, []string{"geo-downtown", "geo-north"}, data.Properties.ids())
}

// TestPropertiesGeo_MultiPolygon_SinglePolygon pins the single-shape case: a
// one-element polygons list searches one region, with the same open-ring
// auto-close behavior as a discontiguous search (square is an open ring).
func TestPropertiesGeo_MultiPolygon_SinglePolygon(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedPropertyAt(t, client, "geo-inside", dtLat, dtLng, true)
	seedPropertyAt(t, client, "geo-west", dtLat, dtLng-0.02, true) // outside

	var data struct {
		Properties geoConn `json:"properties"`
	}
	testutil.GQL(t, srv, propertiesGeoQuery, map[string]any{"geo": withinPolygonsGeo(square(dtLat, dtLng, 0.01)), "first": 50}, &data)

	assert.Equal(t, 1, data.Properties.TotalCount)
	assert.Equal(t, []string{"geo-inside"}, data.Properties.ids())
}

// TestPropertiesGeo_MultiPolygon_ExcludesTombstoned confirms the same
// mlg_can_view filter the other regions apply.
func TestPropertiesGeo_MultiPolygon_ExcludesTombstoned(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedPropertyAt(t, client, "geo-vis", dtLat, dtLng, true)
	seedPropertyAt(t, client, "geo-tomb", dtLat, dtLng, false)

	var data struct {
		Properties geoConn `json:"properties"`
	}
	testutil.GQL(t, srv, propertiesGeoQuery, map[string]any{"geo": withinPolygonsGeo(square(dtLat, dtLng, 0.01)), "first": 50}, &data)

	assert.Equal(t, 1, data.Properties.TotalCount)
	assert.Equal(t, []string{"geo-vis"}, data.Properties.ids())
}

func TestPropertiesGeo_Pagination(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedPropertyAt(t, client, "geo-a", dtLat, dtLng, true)
	seedPropertyAt(t, client, "geo-b", dtLat+0.001, dtLng, true)
	seedPropertyAt(t, client, "geo-c", dtLat+0.002, dtLng, true)

	var data struct {
		Properties geoConn `json:"properties"`
	}
	testutil.GQL(t, srv, propertiesGeoQuery, map[string]any{"geo": withinRadiusGeo(dtLat, dtLng, 5000.0), "first": 2}, &data)

	assert.Equal(t, 3, data.Properties.TotalCount)
	assert.Len(t, data.Properties.Edges, 2)
	assert.True(t, data.Properties.PageInfo.HasNextPage)
}

func TestPropertiesGeo_InvalidArgs_Radius(t *testing.T) {
	t.Parallel()
	srv, _ := testutil.NewTestServer(t)

	// Non-positive radius.
	errs := testutil.GQLExpectError(t, srv, propertiesGeoQuery, map[string]any{"geo": withinRadiusGeo(dtLat, dtLng, 0.0)})
	require.NotEmpty(t, errs)

	// Out-of-range center latitude.
	errs = testutil.GQLExpectError(t, srv, propertiesGeoQuery, map[string]any{"geo": withinRadiusGeo(91.0, dtLng, 1000.0)})
	require.NotEmpty(t, errs)
}

func TestPropertiesGeo_InvalidArgs_Bounds(t *testing.T) {
	t.Parallel()
	srv, _ := testutil.NewTestServer(t)

	// southWest north of northEast.
	errs := testutil.GQLExpectError(t, srv, propertiesGeoQuery, map[string]any{
		"geo": withinBoundsGeo(dtLat+1, dtLng, dtLat, dtLng+1),
	})
	require.NotEmpty(t, errs)
}

func TestPropertiesGeo_InvalidArgs_Polygons(t *testing.T) {
	t.Parallel()
	srv, _ := testutil.NewTestServer(t)

	// Empty polygon list.
	errs := testutil.GQLExpectError(t, srv, propertiesGeoQuery, map[string]any{"geo": withinPolygonsGeo()})
	require.NotEmpty(t, errs)

	// A polygon with fewer than 3 vertices.
	errs = testutil.GQLExpectError(t, srv, propertiesGeoQuery, map[string]any{
		"geo": withinPolygonsGeo([]any{pt(dtLat, dtLng), pt(dtLat+0.01, dtLng)}),
	})
	require.NotEmpty(t, errs)

	// Out-of-range latitude inside an otherwise valid ring.
	errs = testutil.GQLExpectError(t, srv, propertiesGeoQuery, map[string]any{
		"geo": withinPolygonsGeo([]any{pt(91.0, dtLng), pt(dtLat, dtLng+0.01), pt(dtLat+0.01, dtLng)}),
	})
	require.NotEmpty(t, errs)
}
