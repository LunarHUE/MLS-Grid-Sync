package geo

import "testing"

// These guard the WKT string builders, which feed PostGIS as a single bind
// parameter. The ring-closing rule and the (lng lat) axis order are the
// load-bearing details — a regression here silently returns the wrong rows.

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

func TestMultiPolygonWKT_SingleRing(t *testing.T) {
	// A single search region is just a one-element multipolygon. (lat, lng)
	// input; WKT emits "lng lat", auto-closes the open ring, and wraps it in
	// the MULTIPOLYGON(( … )) nesting.
	got := MultiPolygonWKT([][][2]float64{{{30, -97}, {30, -96}, {31, -96}}})
	want := "MULTIPOLYGON(((-97 30, -96 30, -96 31, -97 30)))"
	if got != want {
		t.Fatalf("MultiPolygonWKT single ring:\n got %q\nwant %q", got, want)
	}
}
