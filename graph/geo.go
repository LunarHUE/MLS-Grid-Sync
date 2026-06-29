package graph

import (
	"fmt"

	"github.com/LunarHUE/MLS-Grid-Sync/ent/predicate"
	"github.com/LunarHUE/MLS-Grid-Sync/geo"
	"github.com/LunarHUE/MLS-Grid-Sync/graph/model"
)

// Geo filtering for the property list queries. A GeoFilter argument becomes a
// single composable predicate (geoPredicate) that AND-combines with `where`
// and any address search, so the list and the map pins evaluate the same set.
// Matching relies on the generated geom column (rows without coordinates have
// a NULL geom and never match).

func validatePoint(name string, p model.GeoPoint) error {
	if p.Latitude < -90 || p.Latitude > 90 {
		return fmt.Errorf("%s.latitude %v out of range [-90, 90]", name, p.Latitude)
	}
	if p.Longitude < -180 || p.Longitude > 180 {
		return fmt.Errorf("%s.longitude %v out of range [-180, 180]", name, p.Longitude)
	}
	return nil
}

// multipolygon caps. Each polygon still needs >= 3 vertices (a ring); the
// total vertex count across all polygons bounds the work ST_Covers does.
const (
	maxMultiPolygons             = 64
	maxMultiPolygonTotalVertices = 4096
)

// geoPredicate turns a GeoFilter argument into a single composable
// predicate.Property that AND-combines with `where` and any address search in
// the list resolvers. Exactly one sub-field may be set; each maps to a PostGIS
// region test: withinRadius → ST_DWithin (true meters on the WGS84 spheroid),
// withinBounds → bounding-box containment, withinPolygons → ST_Covers over a
// multipolygon (point inside any constituent polygon).
//
// A nil filter (the arg was omitted) yields a nil predicate and no error, so
// callers can append unconditionally:
//
//	if gp, err := geoPredicate(geo); err != nil { return nil, err } else if gp != nil { preds = append(preds, gp) }
func geoPredicate(g *model.GeoFilter) (predicate.Property, error) {
	if g == nil {
		return nil, nil
	}

	set := 0
	if g.WithinPolygons != nil {
		set++
	}
	if g.WithinBounds != nil {
		set++
	}
	if g.WithinRadius != nil {
		set++
	}
	switch {
	case set == 0:
		return nil, fmt.Errorf("geo requires exactly one of withinPolygons, withinBounds, or withinRadius")
	case set > 1:
		return nil, fmt.Errorf("geo accepts only one of withinPolygons, withinBounds, or withinRadius")
	}

	switch {
	case g.WithinRadius != nil:
		rf := g.WithinRadius
		center := *rf.Center
		if err := validatePoint("withinRadius.center", center); err != nil {
			return nil, err
		}
		if rf.RadiusMeters <= 0 {
			return nil, fmt.Errorf("withinRadius.radiusMeters must be > 0, got %v", rf.RadiusMeters)
		}
		return geo.WithinRadius(center.Latitude, center.Longitude, rf.RadiusMeters), nil

	case g.WithinBounds != nil:
		southWest, northEast := *g.WithinBounds.SouthWest, *g.WithinBounds.NorthEast
		if err := validatePoint("withinBounds.southWest", southWest); err != nil {
			return nil, err
		}
		if err := validatePoint("withinBounds.northEast", northEast); err != nil {
			return nil, err
		}
		if southWest.Latitude >= northEast.Latitude {
			return nil, fmt.Errorf("withinBounds.southWest.latitude must be < northEast.latitude")
		}
		if southWest.Longitude >= northEast.Longitude {
			return nil, fmt.Errorf("withinBounds.southWest.longitude must be < northEast.longitude (antimeridian-crossing boxes are not supported)")
		}
		return geo.InBBox(southWest.Latitude, southWest.Longitude, northEast.Latitude, northEast.Longitude), nil

	default: // g.WithinPolygons != nil
		polygons := g.WithinPolygons
		if len(polygons) == 0 {
			return nil, fmt.Errorf("withinPolygons needs at least 1 polygon, got 0")
		}
		if len(polygons) > maxMultiPolygons {
			return nil, fmt.Errorf("withinPolygons supports at most %d polygons, got %d", maxMultiPolygons, len(polygons))
		}
		total := 0
		rings := make([][][2]float64, len(polygons))
		for pi, poly := range polygons {
			if len(poly) < 3 {
				return nil, fmt.Errorf("withinPolygons[%d] needs at least 3 vertices, got %d", pi, len(poly))
			}
			total += len(poly)
			if total > maxMultiPolygonTotalVertices {
				return nil, fmt.Errorf("withinPolygons supports at most %d total vertices across all polygons", maxMultiPolygonTotalVertices)
			}
			ring := make([][2]float64, len(poly))
			for i, v := range poly {
				if err := validatePoint(fmt.Sprintf("withinPolygons[%d][%d]", pi, i), *v); err != nil {
					return nil, err
				}
				ring[i] = [2]float64{v.Latitude, v.Longitude}
			}
			rings[pi] = ring
		}
		return geo.InMultiPolygon(geo.MultiPolygonWKT(rings)), nil
	}
}
