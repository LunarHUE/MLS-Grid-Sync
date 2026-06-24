package graph

import (
	"entgo.io/contrib/entgql"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
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

	// Each connection field now carries entity-specific orderBy/where args, so
	// the closures can no longer share a single signature. orderBy/where don't
	// affect the row budget (the page size already bounds it), so they're
	// ignored via `_`.
	root.Query.Lookups = func(childComplexity int, _ *entgql.Cursor[string], first *int, _ *entgql.Cursor[string], last *int, _ *ent.LookupOrder, _ *ent.LookupWhereInput) int {
		return connectionComplexity(childComplexity, first, last)
	}
	root.Query.MediaSlice = func(childComplexity int, _ *entgql.Cursor[string], first *int, _ *entgql.Cursor[string], last *int, _ *ent.MediaOrder, _ *ent.MediaWhereInput) int {
		return connectionComplexity(childComplexity, first, last)
	}
	root.Query.MediaVersions = func(childComplexity int, _ *entgql.Cursor[string], first *int, _ *entgql.Cursor[string], last *int, _ *ent.MediaVersionOrder, _ *ent.MediaVersionWhereInput) int {
		return connectionComplexity(childComplexity, first, last)
	}
	root.Query.Members = func(childComplexity int, _ *entgql.Cursor[string], first *int, _ *entgql.Cursor[string], last *int, _ *ent.MemberOrder, _ *ent.MemberWhereInput) int {
		return connectionComplexity(childComplexity, first, last)
	}
	root.Query.MemberVersions = func(childComplexity int, _ *entgql.Cursor[string], first *int, _ *entgql.Cursor[string], last *int, _ *ent.MemberVersionOrder, _ *ent.MemberVersionWhereInput) int {
		return connectionComplexity(childComplexity, first, last)
	}
	root.Query.Offices = func(childComplexity int, _ *entgql.Cursor[string], first *int, _ *entgql.Cursor[string], last *int, _ *ent.OfficeOrder, _ *ent.OfficeWhereInput) int {
		return connectionComplexity(childComplexity, first, last)
	}
	root.Query.OfficeVersions = func(childComplexity int, _ *entgql.Cursor[string], first *int, _ *entgql.Cursor[string], last *int, _ *ent.OfficeVersionOrder, _ *ent.OfficeVersionWhereInput) int {
		return connectionComplexity(childComplexity, first, last)
	}
	root.Query.OpenHouses = func(childComplexity int, _ *entgql.Cursor[string], first *int, _ *entgql.Cursor[string], last *int, _ *ent.OpenHouseOrder, _ *ent.OpenHouseWhereInput) int {
		return connectionComplexity(childComplexity, first, last)
	}
	root.Query.OpenHouseVersions = func(childComplexity int, _ *entgql.Cursor[string], first *int, _ *entgql.Cursor[string], last *int, _ *ent.OpenHouseVersionOrder, _ *ent.OpenHouseVersionWhereInput) int {
		return connectionComplexity(childComplexity, first, last)
	}
	root.Query.Properties = func(childComplexity int, _ *entgql.Cursor[string], first *int, _ *entgql.Cursor[string], last *int, _ *ent.PropertyOrder, _ *ent.PropertyWhereInput) int {
		return connectionComplexity(childComplexity, first, last)
	}
	root.Query.PropertyRooms = func(childComplexity int, _ *entgql.Cursor[string], first *int, _ *entgql.Cursor[string], last *int, _ *ent.PropertyRoomOrder, _ *ent.PropertyRoomWhereInput) int {
		return connectionComplexity(childComplexity, first, last)
	}
	root.Query.PropertyRoomVersions = func(childComplexity int, _ *entgql.Cursor[string], first *int, _ *entgql.Cursor[string], last *int, _ *ent.PropertyRoomVersionOrder, _ *ent.PropertyRoomVersionWhereInput) int {
		return connectionComplexity(childComplexity, first, last)
	}
	root.Query.PropertyUnitTypes = func(childComplexity int, _ *entgql.Cursor[string], first *int, _ *entgql.Cursor[string], last *int, _ *ent.PropertyUnitTypeOrder, _ *ent.PropertyUnitTypeWhereInput) int {
		return connectionComplexity(childComplexity, first, last)
	}
	root.Query.PropertyUnitTypeVersions = func(childComplexity int, _ *entgql.Cursor[string], first *int, _ *entgql.Cursor[string], last *int, _ *ent.PropertyUnitTypeVersionOrder, _ *ent.PropertyUnitTypeVersionWhereInput) int {
		return connectionComplexity(childComplexity, first, last)
	}
	root.Query.PropertyVersions = func(childComplexity int, _ *entgql.Cursor[string], first *int, _ *entgql.Cursor[string], last *int, _ *ent.PropertyVersionOrder, _ *ent.PropertyVersionWhereInput) int {
		return connectionComplexity(childComplexity, first, last)
	}
	root.Query.SourceSystems = func(childComplexity int, _ *entgql.Cursor[string], first *int, _ *entgql.Cursor[string], last *int, _ *ent.SourceSystemWhereInput) int {
		return connectionComplexity(childComplexity, first, last)
	}

	root.Query.PropertiesNear = func(childComplexity int, _ model.GeoPoint, _ float64, _ *entgql.Cursor[string], first *int, _ *entgql.Cursor[string], last *int, _ *ent.PropertyOrder, _ *ent.PropertyWhereInput) int {
		return connectionComplexity(childComplexity, first, last)
	}
	root.Query.PropertiesInBBox = func(childComplexity int, _ model.GeoPoint, _ model.GeoPoint, _ *entgql.Cursor[string], first *int, _ *entgql.Cursor[string], last *int, _ *ent.PropertyOrder, _ *ent.PropertyWhereInput) int {
		return connectionComplexity(childComplexity, first, last)
	}
	root.Query.PropertiesInPolygon = func(childComplexity int, _ []*model.GeoPoint, _ *entgql.Cursor[string], first *int, _ *entgql.Cursor[string], last *int, _ *ent.PropertyOrder, _ *ent.PropertyWhereInput) int {
		return connectionComplexity(childComplexity, first, last)
	}
	root.Query.PropertiesInMultiPolygon = func(childComplexity int, _ [][]*model.GeoPoint, _ *entgql.Cursor[string], first *int, _ *entgql.Cursor[string], last *int, _ *ent.PropertyOrder, _ *ent.PropertyWhereInput) int {
		return connectionComplexity(childComplexity, first, last)
	}
	root.Query.PropertiesByAddress = func(childComplexity int, _ string, _ *float64, _ *entgql.Cursor[string], first *int, _ *entgql.Cursor[string], last *int, _ *ent.PropertyOrder, _ *ent.PropertyWhereInput) int {
		return connectionComplexity(childComplexity, first, last)
	}
	root.Query.PropertiesByAddressFields = func(childComplexity int, _ *string, _ *string, _ *string, _ *string, _ *float64, _ *entgql.Cursor[string], first *int, _ *entgql.Cursor[string], last *int, _ *ent.PropertyOrder, _ *ent.PropertyWhereInput) int {
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
