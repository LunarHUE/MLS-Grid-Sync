package geo

import "testing"

// These guard the WKT string builders, which feed PostGIS as a single bind
// parameter. The ring-closing rule and the (lng lat) axis order are the
// load-bearing details — a regression here silently returns the wrong rows.

func TestPolygonWKT_ClosesOpenRing(t *testing.T) {
	// (lat, lng) input; WKT emits "lng lat" and appends the first vertex.
	got := PolygonWKT([][2]float64{{30, -97}, {30, -96}, {31, -96}})
	want := "POLYGON((-97 30, -96 30, -96 31, -97 30))"
	if got != want {
		t.Fatalf("PolygonWKT open ring:\n got %q\nwant %q", got, want)
	}
}

func TestPolygonWKT_AlreadyClosedRingNotDuplicated(t *testing.T) {
	got := PolygonWKT([][2]float64{{30, -97}, {30, -96}, {31, -96}, {30, -97}})
	want := "POLYGON((-97 30, -96 30, -96 31, -97 30))"
	if got != want {
		t.Fatalf("PolygonWKT closed ring:\n got %q\nwant %q", got, want)
	}
}

func TestMultiPolygonWKT_TwoRings(t *testing.T) {
	got := MultiPolygonWKT([][][2]float64{
		{{30, -97}, {30, -96}, {31, -96}},            // open → closed
		{{40, -80}, {40, -79}, {41, -79}, {40, -80}}, // already closed
	})
	want := "MULTIPOLYGON(((-97 30, -96 30, -96 31, -97 30)), ((-80 40, -79 40, -79 41, -80 40)))"
	if got != want {
		t.Fatalf("MultiPolygonWKT:\n got %q\nwant %q", got, want)
	}
}

func TestMultiPolygonWKT_SingleRingMatchesPolygon(t *testing.T) {
	ring := [][2]float64{{30, -97}, {30, -96}, {31, -96}}
	mp := MultiPolygonWKT([][][2]float64{ring})
	// A single-ring multipolygon is the polygon WKT with the leading
	// "POLYGON" swapped for "MULTIPOLYGON(" and a closing paren added:
	// POLYGON(<body>) → MULTIPOLYGON((<body>)).
	want := "MULTIPOLYGON(" + PolygonWKT(ring)[len("POLYGON"):] + ")"
	if mp != want {
		t.Fatalf("MultiPolygonWKT single ring:\n got %q\nwant %q", mp, want)
	}
}
