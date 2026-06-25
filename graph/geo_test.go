package graph_test

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
)

// Geo-search tests. Reference points around downtown Austin, TX:
// downtown (30.2672, -97.7431), ~1.1km north (30.2772), ~111km north
// (31.2672). 0.01° of latitude ≈ 1.11km.

const (
	dtLat = 30.2672
	dtLng = -97.7431
)

// geoConn is the wire shape shared by the three geo queries.
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

const nearQuery = `query($center: GeoPoint!, $radius: Float!) {
	propertiesNear(center: $center, radiusMeters: $radius, first: 50) {
		totalCount
		pageInfo { hasNextPage }
		edges { node { id } }
	}
}`

func TestPropertiesNear(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedPropertyAt(t, client, "geo-downtown", dtLat, dtLng, true)
	seedPropertyAt(t, client, "geo-1km", dtLat+0.01, dtLng, true)
	seedPropertyAt(t, client, "geo-111km", dtLat+1.0, dtLng, true)

	center := map[string]any{"latitude": dtLat, "longitude": dtLng}

	var data struct {
		PropertiesNear geoConn `json:"propertiesNear"`
	}

	// 5km: downtown + the 1.1km neighbor, not the 111km outlier.
	testutil.GQL(t, srv, nearQuery, map[string]any{"center": center, "radius": 5000.0}, &data)
	assert.Equal(t, 2, data.PropertiesNear.TotalCount)
	assert.ElementsMatch(t, []string{"geo-downtown", "geo-1km"}, data.PropertiesNear.ids())

	// 100m: downtown only.
	testutil.GQL(t, srv, nearQuery, map[string]any{"center": center, "radius": 100.0}, &data)
	assert.Equal(t, 1, data.PropertiesNear.TotalCount)
	assert.Equal(t, []string{"geo-downtown"}, data.PropertiesNear.ids())

	// 200km: all three.
	testutil.GQL(t, srv, nearQuery, map[string]any{"center": center, "radius": 200000.0}, &data)
	assert.Equal(t, 3, data.PropertiesNear.TotalCount)
}

func TestPropertiesNear_ExcludesTombstonedAndCoordless(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedPropertyAt(t, client, "geo-vis", dtLat, dtLng, true)
	seedPropertyAt(t, client, "geo-tomb", dtLat, dtLng, false)
	seedProperty(t, client, "geo-nocoords", true) // no lat/lng → NULL geom

	var data struct {
		PropertiesNear geoConn `json:"propertiesNear"`
	}
	testutil.GQL(t, srv, nearQuery, map[string]any{
		"center": map[string]any{"latitude": dtLat, "longitude": dtLng},
		"radius": 1000.0,
	}, &data)

	assert.Equal(t, 1, data.PropertiesNear.TotalCount)
	assert.Equal(t, []string{"geo-vis"}, data.PropertiesNear.ids())
}

func TestPropertiesNear_InvalidArgs(t *testing.T) {
	t.Parallel()
	srv, _ := testutil.NewTestServer(t)

	errs := testutil.GQLExpectError(t, srv, nearQuery, map[string]any{
		"center": map[string]any{"latitude": dtLat, "longitude": dtLng},
		"radius": 0.0,
	})
	require.NotEmpty(t, errs)

	errs = testutil.GQLExpectError(t, srv, nearQuery, map[string]any{
		"center": map[string]any{"latitude": 91.0, "longitude": dtLng},
		"radius": 1000.0,
	})
	require.NotEmpty(t, errs)
}

const bboxQuery = `query($bounds: Bounds!) {
	propertiesInBBox(bounds: $bounds, first: 50) {
		totalCount
		edges { node { id } }
	}
}`

func TestPropertiesInBBox(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedPropertyAt(t, client, "geo-downtown", dtLat, dtLng, true)
	seedPropertyAt(t, client, "geo-1km", dtLat+0.01, dtLng, true)
	seedPropertyAt(t, client, "geo-111km", dtLat+1.0, dtLng, true)

	var data struct {
		PropertiesInBBox geoConn `json:"propertiesInBBox"`
	}
	// Viewport around downtown: catches downtown + 1km, not 111km.
	testutil.GQL(t, srv, bboxQuery, map[string]any{
		"bounds": map[string]any{
			"southWest": map[string]any{"latitude": dtLat - 0.01, "longitude": dtLng - 0.01},
			"northEast": map[string]any{"latitude": dtLat + 0.02, "longitude": dtLng + 0.01},
		},
	}, &data)

	assert.Equal(t, 2, data.PropertiesInBBox.TotalCount)
	assert.ElementsMatch(t, []string{"geo-downtown", "geo-1km"}, data.PropertiesInBBox.ids())
}

func TestPropertiesInBBox_InvalidArgs(t *testing.T) {
	t.Parallel()
	srv, _ := testutil.NewTestServer(t)

	// southWest north of northEast.
	errs := testutil.GQLExpectError(t, srv, bboxQuery, map[string]any{
		"bounds": map[string]any{
			"southWest": map[string]any{"latitude": dtLat + 1, "longitude": dtLng},
			"northEast": map[string]any{"latitude": dtLat, "longitude": dtLng + 1},
		},
	})
	require.NotEmpty(t, errs)
}

const polygonQuery = `query($vertices: [GeoPoint!]!) {
	propertiesInPolygon(vertices: $vertices, first: 50) {
		totalCount
		edges { node { id } }
	}
}`

func pt(lat, lng float64) map[string]any {
	return map[string]any{"latitude": lat, "longitude": lng}
}

func TestPropertiesInPolygon_Triangle(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedPropertyAt(t, client, "geo-inside", dtLat, dtLng, true)
	seedPropertyAt(t, client, "geo-west", dtLat, dtLng-0.02, true) // outside, to the west

	var data struct {
		PropertiesInPolygon geoConn `json:"propertiesInPolygon"`
	}
	// Open ring (3 vertices) — server closes it.
	testutil.GQL(t, srv, polygonQuery, map[string]any{
		"vertices": []any{
			pt(dtLat-0.007, dtLng-0.007),
			pt(dtLat-0.007, dtLng+0.007),
			pt(dtLat+0.013, dtLng),
		},
	}, &data)

	assert.Equal(t, 1, data.PropertiesInPolygon.TotalCount)
	assert.Equal(t, []string{"geo-inside"}, data.PropertiesInPolygon.ids())
}

// "x number of vertices": a 5-vertex polygon (roughly a pentagon around
// downtown) with an explicit closing vertex — both shapes consumers
// produce from map-drawing tools must work.
func TestPropertiesInPolygon_PentagonClosedRing(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedPropertyAt(t, client, "geo-inside", dtLat, dtLng, true)
	seedPropertyAt(t, client, "geo-1km", dtLat+0.01, dtLng, true)  // inside the pentagon
	seedPropertyAt(t, client, "geo-far", dtLat+1.0, dtLng, true)   // far outside
	seedPropertyAt(t, client, "geo-east", dtLat, dtLng+0.05, true) // outside, east

	pentagon := []any{
		pt(dtLat-0.010, dtLng-0.010),
		pt(dtLat-0.010, dtLng+0.010),
		pt(dtLat+0.012, dtLng+0.014),
		pt(dtLat+0.020, dtLng),
		pt(dtLat+0.012, dtLng-0.014),
		pt(dtLat-0.010, dtLng-0.010), // explicit closing vertex
	}

	var data struct {
		PropertiesInPolygon geoConn `json:"propertiesInPolygon"`
	}
	testutil.GQL(t, srv, polygonQuery, map[string]any{"vertices": pentagon}, &data)

	assert.Equal(t, 2, data.PropertiesInPolygon.TotalCount)
	assert.ElementsMatch(t, []string{"geo-inside", "geo-1km"}, data.PropertiesInPolygon.ids())
}

func TestPropertiesInPolygon_TooFewVertices(t *testing.T) {
	t.Parallel()
	srv, _ := testutil.NewTestServer(t)

	errs := testutil.GQLExpectError(t, srv, polygonQuery, map[string]any{
		"vertices": []any{pt(dtLat, dtLng), pt(dtLat+0.01, dtLng)},
	})
	require.NotEmpty(t, errs)
}

func TestPropertiesInPolygon_VertexCap(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedPropertyAt(t, client, "geo-cap-inside", dtLat, dtLng, true)

	// circleVertices approximates a circle of the given radius (in
	// degrees) around downtown with n vertices.
	circleVertices := func(n int, radiusDeg float64) []any {
		out := make([]any, n)
		for i := range n {
			angle := 2 * math.Pi * float64(i) / float64(n)
			out[i] = pt(dtLat+radiusDeg*math.Sin(angle), dtLng+radiusDeg*math.Cos(angle))
		}
		return out
	}

	// Exactly at the cap: accepted, and the point inside matches.
	var data struct {
		PropertiesInPolygon geoConn `json:"propertiesInPolygon"`
	}
	testutil.GQL(t, srv, polygonQuery, map[string]any{
		"vertices": circleVertices(1024, 0.01),
	}, &data)
	assert.Equal(t, 1, data.PropertiesInPolygon.TotalCount)

	// One past the cap: rejected.
	errs := testutil.GQLExpectError(t, srv, polygonQuery, map[string]any{
		"vertices": circleVertices(1025, 0.01),
	})
	require.NotEmpty(t, errs)
}

const multiPolygonQuery = `query($polygons: [[GeoPoint!]!]!) {
	propertiesInMultiPolygon(polygons: $polygons, first: 50) {
		totalCount
		edges { node { id } }
	}
}`

// square returns a 4-vertex (open) ring of half-side halfDeg around (lat,lng).
func square(lat, lng, halfDeg float64) []any {
	return []any{
		pt(lat-halfDeg, lng-halfDeg),
		pt(lat-halfDeg, lng+halfDeg),
		pt(lat+halfDeg, lng+halfDeg),
		pt(lat+halfDeg, lng-halfDeg),
	}
}

// TestPropertiesInMultiPolygon_DiscontiguousRegions is the load-bearing
// check: two separate boxes — one downtown, one ~111km north — must both
// contribute matches in a single query, while a property between them does
// not. This is what a single polygon cannot express.
func TestPropertiesInMultiPolygon_DiscontiguousRegions(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedPropertyAt(t, client, "geo-downtown", dtLat, dtLng, true)
	seedPropertyAt(t, client, "geo-north", dtLat+1.0, dtLng, true)   // inside the north box
	seedPropertyAt(t, client, "geo-between", dtLat+0.5, dtLng, true) // in the gap, matches neither

	var data struct {
		PropertiesInMultiPolygon geoConn `json:"propertiesInMultiPolygon"`
	}
	testutil.GQL(t, srv, multiPolygonQuery, map[string]any{
		"polygons": []any{
			square(dtLat, dtLng, 0.01),     // downtown
			square(dtLat+1.0, dtLng, 0.01), // ~111km north
		},
	}, &data)

	assert.Equal(t, 2, data.PropertiesInMultiPolygon.TotalCount)
	assert.ElementsMatch(t, []string{"geo-downtown", "geo-north"}, data.PropertiesInMultiPolygon.ids())
}

// TestPropertiesInMultiPolygon_SinglePolygonMatchesPolygonQuery pins that a
// one-element multipolygon behaves exactly like propertiesInPolygon — the
// multipolygon path is a strict superset.
func TestPropertiesInMultiPolygon_SinglePolygon(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedPropertyAt(t, client, "geo-inside", dtLat, dtLng, true)
	seedPropertyAt(t, client, "geo-west", dtLat, dtLng-0.02, true) // outside

	var data struct {
		PropertiesInMultiPolygon geoConn `json:"propertiesInMultiPolygon"`
	}
	testutil.GQL(t, srv, multiPolygonQuery, map[string]any{
		"polygons": []any{square(dtLat, dtLng, 0.01)},
	}, &data)

	assert.Equal(t, 1, data.PropertiesInMultiPolygon.TotalCount)
	assert.Equal(t, []string{"geo-inside"}, data.PropertiesInMultiPolygon.ids())
}

// TestPropertiesInMultiPolygon_ExcludesTombstoned confirms the same
// mlg_can_view filter the other geo searches apply.
func TestPropertiesInMultiPolygon_ExcludesTombstoned(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedPropertyAt(t, client, "geo-vis", dtLat, dtLng, true)
	seedPropertyAt(t, client, "geo-tomb", dtLat, dtLng, false)

	var data struct {
		PropertiesInMultiPolygon geoConn `json:"propertiesInMultiPolygon"`
	}
	testutil.GQL(t, srv, multiPolygonQuery, map[string]any{
		"polygons": []any{square(dtLat, dtLng, 0.01)},
	}, &data)

	assert.Equal(t, 1, data.PropertiesInMultiPolygon.TotalCount)
	assert.Equal(t, []string{"geo-vis"}, data.PropertiesInMultiPolygon.ids())
}

func TestPropertiesInMultiPolygon_InvalidArgs(t *testing.T) {
	t.Parallel()
	srv, _ := testutil.NewTestServer(t)

	// Empty polygon list.
	errs := testutil.GQLExpectError(t, srv, multiPolygonQuery, map[string]any{
		"polygons": []any{},
	})
	require.NotEmpty(t, errs)

	// A polygon with fewer than 3 vertices.
	errs = testutil.GQLExpectError(t, srv, multiPolygonQuery, map[string]any{
		"polygons": []any{[]any{pt(dtLat, dtLng), pt(dtLat+0.01, dtLng)}},
	})
	require.NotEmpty(t, errs)

	// Out-of-range latitude inside an otherwise valid ring.
	errs = testutil.GQLExpectError(t, srv, multiPolygonQuery, map[string]any{
		"polygons": []any{[]any{pt(91.0, dtLng), pt(dtLat, dtLng+0.01), pt(dtLat+0.01, dtLng)}},
	})
	require.NotEmpty(t, errs)
}

func TestPropertiesGeo_Pagination(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedPropertyAt(t, client, "geo-a", dtLat, dtLng, true)
	seedPropertyAt(t, client, "geo-b", dtLat+0.001, dtLng, true)
	seedPropertyAt(t, client, "geo-c", dtLat+0.002, dtLng, true)

	var data struct {
		PropertiesNear geoConn `json:"propertiesNear"`
	}
	testutil.GQL(t, srv, `query($center: GeoPoint!, $radius: Float!) {
		propertiesNear(center: $center, radiusMeters: $radius, first: 2) {
			totalCount
			pageInfo { hasNextPage }
			edges { node { id } }
		}
	}`, map[string]any{
		"center": map[string]any{"latitude": dtLat, "longitude": dtLng},
		"radius": 5000.0,
	}, &data)

	assert.Equal(t, 3, data.PropertiesNear.TotalCount)
	assert.Len(t, data.PropertiesNear.Edges, 2)
	assert.True(t, data.PropertiesNear.PageInfo.HasNextPage)
}
