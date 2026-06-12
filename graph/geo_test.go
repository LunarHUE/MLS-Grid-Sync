package graph_test

import (
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

const bboxQuery = `query($sw: GeoPoint!, $ne: GeoPoint!) {
	propertiesInBBox(southWest: $sw, northEast: $ne, first: 50) {
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
		"sw": map[string]any{"latitude": dtLat - 0.01, "longitude": dtLng - 0.01},
		"ne": map[string]any{"latitude": dtLat + 0.02, "longitude": dtLng + 0.01},
	}, &data)

	assert.Equal(t, 2, data.PropertiesInBBox.TotalCount)
	assert.ElementsMatch(t, []string{"geo-downtown", "geo-1km"}, data.PropertiesInBBox.ids())
}

func TestPropertiesInBBox_InvalidArgs(t *testing.T) {
	t.Parallel()
	srv, _ := testutil.NewTestServer(t)

	// southWest north of northEast.
	errs := testutil.GQLExpectError(t, srv, bboxQuery, map[string]any{
		"sw": map[string]any{"latitude": dtLat + 1, "longitude": dtLng},
		"ne": map[string]any{"latitude": dtLat, "longitude": dtLng + 1},
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
	seedPropertyAt(t, client, "geo-1km", dtLat+0.01, dtLng, true)     // inside the pentagon
	seedPropertyAt(t, client, "geo-far", dtLat+1.0, dtLng, true)      // far outside
	seedPropertyAt(t, client, "geo-east", dtLat, dtLng+0.05, true)    // outside, east

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
