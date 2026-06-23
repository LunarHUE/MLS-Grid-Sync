package graph

import (
	"context"
	"fmt"

	"entgo.io/contrib/entgql"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/property"
	"github.com/LunarHUE/MLS-Grid-Sync/geo"
	"github.com/LunarHUE/MLS-Grid-Sync/graph/model"
)

// Geo-search resolvers. All three apply the same visibility filter as
// the properties list and rely on the generated geom column (rows
// without coordinates have a NULL geom and never match).

func validatePoint(name string, p model.GeoPoint) error {
	if p.Latitude < -90 || p.Latitude > 90 {
		return fmt.Errorf("%s.latitude %v out of range [-90, 90]", name, p.Latitude)
	}
	if p.Longitude < -180 || p.Longitude > 180 {
		return fmt.Errorf("%s.longitude %v out of range [-180, 180]", name, p.Longitude)
	}
	return nil
}

func (r *queryResolver) PropertiesNear(ctx context.Context, center model.GeoPoint, radiusMeters float64, after *entgql.Cursor[string], first *int, before *entgql.Cursor[string], last *int, orderBy *ent.PropertyOrder, where *ent.PropertyWhereInput) (*ent.PropertyConnection, error) {
	first, last = clampPage(first, last)
	if err := validatePoint("center", center); err != nil {
		return nil, err
	}
	if radiusMeters <= 0 {
		return nil, fmt.Errorf("radiusMeters must be > 0, got %v", radiusMeters)
	}
	return r.client.Property.Query().
		Where(property.MlgCanView(true), geo.WithinRadius(center.Latitude, center.Longitude, radiusMeters)).
		Paginate(ctx, after, first, before, last,
			ent.WithPropertyOrder(orderBy),
			ent.WithPropertyFilter(where.Filter))
}

func (r *queryResolver) PropertiesInBBox(ctx context.Context, southWest model.GeoPoint, northEast model.GeoPoint, after *entgql.Cursor[string], first *int, before *entgql.Cursor[string], last *int, orderBy *ent.PropertyOrder, where *ent.PropertyWhereInput) (*ent.PropertyConnection, error) {
	first, last = clampPage(first, last)
	if err := validatePoint("southWest", southWest); err != nil {
		return nil, err
	}
	if err := validatePoint("northEast", northEast); err != nil {
		return nil, err
	}
	if southWest.Latitude >= northEast.Latitude {
		return nil, fmt.Errorf("southWest.latitude must be < northEast.latitude")
	}
	if southWest.Longitude >= northEast.Longitude {
		return nil, fmt.Errorf("southWest.longitude must be < northEast.longitude (antimeridian-crossing boxes are not supported)")
	}
	return r.client.Property.Query().
		Where(property.MlgCanView(true), geo.InBBox(southWest.Latitude, southWest.Longitude, northEast.Latitude, northEast.Longitude)).
		Paginate(ctx, after, first, before, last,
			ent.WithPropertyOrder(orderBy),
			ent.WithPropertyFilter(where.Filter))
}

// maxPolygonVertices caps propertiesInPolygon input. Map-drawing tools
// produce dozens of vertices; anything past this is either a client bug
// or an attempt to make the server chew on a degenerate geometry.
const maxPolygonVertices = 1024

func (r *queryResolver) PropertiesInPolygon(ctx context.Context, vertices []*model.GeoPoint, after *entgql.Cursor[string], first *int, before *entgql.Cursor[string], last *int, orderBy *ent.PropertyOrder, where *ent.PropertyWhereInput) (*ent.PropertyConnection, error) {
	first, last = clampPage(first, last)
	if len(vertices) < 3 {
		return nil, fmt.Errorf("polygon needs at least 3 vertices, got %d", len(vertices))
	}
	if len(vertices) > maxPolygonVertices {
		return nil, fmt.Errorf("polygon supports at most %d vertices, got %d", maxPolygonVertices, len(vertices))
	}
	latLngs := make([][2]float64, len(vertices))
	for i, v := range vertices {
		if err := validatePoint(fmt.Sprintf("vertices[%d]", i), *v); err != nil {
			return nil, err
		}
		latLngs[i] = [2]float64{v.Latitude, v.Longitude}
	}
	return r.client.Property.Query().
		Where(property.MlgCanView(true), geo.InPolygon(geo.PolygonWKT(latLngs))).
		Paginate(ctx, after, first, before, last,
			ent.WithPropertyOrder(orderBy),
			ent.WithPropertyFilter(where.Filter))
}

// multipolygon caps. Each polygon still needs >= 3 vertices (a ring); the
// total vertex count across all polygons bounds the work ST_Covers does,
// mirroring how maxPolygonVertices bounds the single-polygon path.
const (
	maxMultiPolygons             = 64
	maxMultiPolygonTotalVertices = 4096
)

// PropertiesInMultiPolygon matches properties covered by ANY of several
// polygons (a multipolygon), so consumers can search discontiguous regions
// — several separate neighborhoods, say — in a single query. Each polygon
// is validated and built into a ring exactly like propertiesInPolygon, then
// all rings are combined into one MULTIPOLYGON and tested with ST_Covers
// (true when a point lies in any constituent polygon). Same visibility
// filter as the rest of the geo searches.
func (r *queryResolver) PropertiesInMultiPolygon(ctx context.Context, polygons [][]*model.GeoPoint, after *entgql.Cursor[string], first *int, before *entgql.Cursor[string], last *int, orderBy *ent.PropertyOrder, where *ent.PropertyWhereInput) (*ent.PropertyConnection, error) {
	first, last = clampPage(first, last)
	if len(polygons) == 0 {
		return nil, fmt.Errorf("multipolygon needs at least 1 polygon, got 0")
	}
	if len(polygons) > maxMultiPolygons {
		return nil, fmt.Errorf("multipolygon supports at most %d polygons, got %d", maxMultiPolygons, len(polygons))
	}
	total := 0
	rings := make([][][2]float64, len(polygons))
	for pi, poly := range polygons {
		if len(poly) < 3 {
			return nil, fmt.Errorf("polygons[%d] needs at least 3 vertices, got %d", pi, len(poly))
		}
		total += len(poly)
		if total > maxMultiPolygonTotalVertices {
			return nil, fmt.Errorf("multipolygon supports at most %d total vertices across all polygons", maxMultiPolygonTotalVertices)
		}
		ring := make([][2]float64, len(poly))
		for i, v := range poly {
			if err := validatePoint(fmt.Sprintf("polygons[%d][%d]", pi, i), *v); err != nil {
				return nil, err
			}
			ring[i] = [2]float64{v.Latitude, v.Longitude}
		}
		rings[pi] = ring
	}
	return r.client.Property.Query().
		Where(property.MlgCanView(true), geo.InMultiPolygon(geo.MultiPolygonWKT(rings))).
		Paginate(ctx, after, first, before, last,
			ent.WithPropertyOrder(orderBy),
			ent.WithPropertyFilter(where.Filter))
}
