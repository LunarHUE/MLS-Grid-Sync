package graph

import (
	"entgo.io/contrib/entgql"

	"github.com/LunarHUE/MLS-Grid-Sync/graph/model"
)

const (
	edgeListFanout = 20
	// MaxComplexity is the per-operation budget enforced by
	// extension.FixedComplexityLimit in NewHandler. A full page
	// (first: 500) selecting every Property scalar costs ~65k; each
	// cyclic rooms→property→rooms level multiplies by edgeListFanout,
	// so three levels ≈ 4M exceeds the budget.
	MaxComplexity = 1_000_000
)

// effectivePageSize mirrors clampPage: it reports the row count a
// connection resolver will actually request, so complexity is scored
// against the clamped size rather than the (possibly huge) raw argument.
func effectivePageSize(first, last *int) int {
	switch {
	case first != nil:
		n := *first
		if n < 0 {
			n = 0
		}
		if n > MaxPageSize {
			n = MaxPageSize
		}
		return n
	case last != nil:
		n := *last
		if n < 0 {
			n = 0
		}
		if n > MaxPageSize {
			n = MaxPageSize
		}
		return n
	default:
		return MaxPageSize
	}
}

// connectionComplexity scores a relay connection: one unit for the
// connection plus the per-row child cost times the effective page size.
func connectionComplexity(childComplexity int, first, last *int) int {
	return 1 + effectivePageSize(first, last)*childComplexity
}

// listComplexity scores an unpaginated edge list (Property.rooms etc.)
// using a fixed fan-out estimate.
func listComplexity(childComplexity int) int {
	return 1 + edgeListFanout*childComplexity
}

// complexityRoot wires per-field complexity estimators. Only the fields
// that can fan out (relay connections, geo searches, multi-id nodes, and
// the unbounded edge lists) carry custom costs; everything else uses
// gqlgen's default of 1.
func complexityRoot() ComplexityRoot {
	var root ComplexityRoot

	conn := func(childComplexity int, _ *entgql.Cursor[string], first *int, _ *entgql.Cursor[string], last *int) int {
		return connectionComplexity(childComplexity, first, last)
	}

	root.Query.Lookups = conn
	root.Query.MediaSlice = conn
	root.Query.MediaVersions = conn
	root.Query.Members = conn
	root.Query.MemberVersions = conn
	root.Query.Offices = conn
	root.Query.OfficeVersions = conn
	root.Query.OpenHouses = conn
	root.Query.OpenHouseVersions = conn
	root.Query.Properties = conn
	root.Query.PropertyRooms = conn
	root.Query.PropertyRoomVersions = conn
	root.Query.PropertyUnitTypes = conn
	root.Query.PropertyUnitTypeVersions = conn
	root.Query.PropertyVersions = conn
	root.Query.SourceSystems = conn

	root.Query.PropertiesNear = func(childComplexity int, _ model.GeoPoint, _ float64, _ *entgql.Cursor[string], first *int, _ *entgql.Cursor[string], last *int) int {
		return connectionComplexity(childComplexity, first, last)
	}
	root.Query.PropertiesInBBox = func(childComplexity int, _ model.GeoPoint, _ model.GeoPoint, _ *entgql.Cursor[string], first *int, _ *entgql.Cursor[string], last *int) int {
		return connectionComplexity(childComplexity, first, last)
	}
	root.Query.PropertiesInPolygon = func(childComplexity int, _ []*model.GeoPoint, _ *entgql.Cursor[string], first *int, _ *entgql.Cursor[string], last *int) int {
		return connectionComplexity(childComplexity, first, last)
	}

	root.Query.Nodes = func(childComplexity int, ids []string) int {
		return 1 + len(ids)*childComplexity
	}

	root.Property.Rooms = listComplexity
	root.Property.UnitTypes = listComplexity
	root.Property.OpenHouses = listComplexity
	root.Property.Media = listComplexity
	root.Office.Branches = listComplexity

	return root
}
