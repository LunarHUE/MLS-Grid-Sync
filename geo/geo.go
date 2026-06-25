// Package geo owns the PostGIS layer: the migration that adds the
// generated geography column to the property table, and the ent
// predicates the GraphQL geo-search resolvers use against it.
//
// The geom column is GENERATED ALWAYS from latitude/longitude, so the
// sync pipeline never writes it — any row with coordinates is
// automatically searchable, and rows without coordinates have a NULL
// geom and never match a geo predicate.
package geo

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	entsql "entgo.io/ent/dialect/sql"

	"github.com/LunarHUE/MLS-Grid-Sync/ent/predicate"
)

// geomColumn is the generated geography(Point,4326) column on property.
const geomColumn = "geom"

// migrations are idempotent and ordered. Radius search uses the
// geography index (true meters); bbox/polygon search casts to geometry
// (planar lat/lng — what a polygon drawn on a map means), so a second
// expression index covers that path.
var migrations = []string{
	`CREATE EXTENSION IF NOT EXISTS postgis`,
	`ALTER TABLE property ADD COLUMN IF NOT EXISTS geom geography(Point,4326)
		GENERATED ALWAYS AS (
			CASE WHEN latitude IS NULL OR longitude IS NULL THEN NULL
			     ELSE ST_SetSRID(ST_MakePoint(longitude::float8, latitude::float8), 4326)::geography
			END
		) STORED`,
	`CREATE INDEX IF NOT EXISTS property_geom_gist ON property USING GIST (geom)`,
	`CREATE INDEX IF NOT EXISTS property_geom_geometry_gist ON property USING GIST ((geom::geometry))`,
}

// Migrate enables PostGIS and adds the generated geom column + GIST
// indexes to the property table. Called after ent's Schema.Create by
// the commands that own migrations (sync/init/worker), never by serve.
// Requires a PostGIS-enabled Postgres image.
func Migrate(ctx context.Context, db *sql.DB) error {
	for _, stmt := range migrations {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("postgis migration %q: %w", strings.Fields(stmt)[0]+" …", err)
		}
	}
	return nil
}

// WithinRadius matches properties within radiusMeters of (lat, lng),
// measured on the WGS84 spheroid (ST_DWithin on geography).
func WithinRadius(lat, lng, radiusMeters float64) predicate.Property {
	return predicate.Property(func(s *entsql.Selector) {
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.WriteString("ST_DWithin(")
			b.WriteString(s.C(geomColumn))
			b.WriteString(", ST_SetSRID(ST_MakePoint(")
			b.Arg(lng).Comma().Arg(lat)
			b.WriteString("), 4326)::geography, ")
			b.Arg(radiusMeters)
			b.WriteString(")")
		}))
	})
}

// InBBox matches properties inside the lat/lng envelope. Planar
// semantics (the box a map viewport describes); boundary inclusive.
func InBBox(minLat, minLng, maxLat, maxLng float64) predicate.Property {
	return predicate.Property(func(s *entsql.Selector) {
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.WriteString(s.C(geomColumn))
			b.WriteString("::geometry && ST_MakeEnvelope(")
			b.Arg(minLng).Comma().Arg(minLat).Comma().Arg(maxLng).Comma().Arg(maxLat)
			b.WriteString(", 4326)")
		}))
	})
}

// InMultiPolygon matches properties covered by ANY of the polygons in a
// MULTIPOLYGON (boundary inclusive, planar lat/lng — matching shapes drawn
// on a map). ST_Covers over a multipolygon is true when the point lies in
// any constituent polygon, so several discontiguous search regions resolve
// in a single query (a single region is just a one-element MULTIPOLYGON).
// The WKT is passed as a single bind parameter, never interpolated.
func InMultiPolygon(wkt string) predicate.Property {
	return predicate.Property(func(s *entsql.Selector) {
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.WriteString("ST_Covers(ST_SetSRID(ST_GeomFromText(")
			b.Arg(wkt)
			b.WriteString("), 4326), ")
			b.WriteString(s.C(geomColumn))
			b.WriteString("::geometry)")
		}))
	})
}

// writeRingWKT writes a single WKT linear ring — "(lng lat, lng lat, …)" —
// from (lat, lng) pairs, closing it when the last vertex differs from the
// first. Callers validate that the ring is non-empty.
func writeRingWKT(b *strings.Builder, ring [][2]float64) {
	b.WriteByte('(')
	writeVertex := func(v [2]float64) {
		b.WriteString(strconv.FormatFloat(v[1], 'f', -1, 64)) // lng (x)
		b.WriteByte(' ')
		b.WriteString(strconv.FormatFloat(v[0], 'f', -1, 64)) // lat (y)
	}
	for i, v := range ring {
		if i > 0 {
			b.WriteString(", ")
		}
		writeVertex(v)
	}
	if ring[0] != ring[len(ring)-1] {
		b.WriteString(", ")
		writeVertex(ring[0])
	}
	b.WriteByte(')')
}

// MultiPolygonWKT builds a MULTIPOLYGON WKT string from several rings, one
// per discontiguous shape. Each ring is closed independently. Callers
// validate that there is at least one ring, each has enough vertices, and
// ranges are in bounds. The result is passed to InMultiPolygon as a single
// bind parameter, never interpolated.
func MultiPolygonWKT(rings [][][2]float64) string {
	var b strings.Builder
	b.WriteString("MULTIPOLYGON(")
	for i, ring := range rings {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('(') // outer paren: a polygon is a list of rings
		writeRingWKT(&b, ring)
		b.WriteByte(')')
	}
	b.WriteByte(')')
	return b.String()
}
