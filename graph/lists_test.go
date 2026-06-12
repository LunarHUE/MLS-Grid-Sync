package graph_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/ent/propertyversion"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
)

// Data-bearing coverage for the root lists that previously had only
// empty-connection tests (or none at all).

// connData is the generic shape for `<list>(first:N){ totalCount }`.
type connData struct {
	TotalCount int `json:"totalCount"`
}

func TestQueryMediaSlice_WithData(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)

	seedMedia(t, client, "med-1", "LK-1", true)
	seedMedia(t, client, "med-2", "LK-1", true)

	var data struct {
		MediaSlice struct {
			TotalCount int `json:"totalCount"`
			Edges      []struct {
				Node struct {
					MediaURL     string `json:"mediaURL"`
					ResourceType string `json:"resourceType"`
				} `json:"node"`
			} `json:"edges"`
		} `json:"mediaSlice"`
	}
	testutil.GQL(t, srv, `{
		mediaSlice(first: 10) {
			totalCount
			edges { node { mediaURL resourceType } }
		}
	}`, nil, &data)

	assert.Equal(t, 2, data.MediaSlice.TotalCount)
	require.Len(t, data.MediaSlice.Edges, 2)
	assert.Contains(t, data.MediaSlice.Edges[0].Node.MediaURL, "cdn.example.com")
	assert.Equal(t, "property", data.MediaSlice.Edges[0].Node.ResourceType)
}

func TestQueryPropertyRooms_WithData(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)

	seedPropertyRoom(t, client, "room-1", "LK-1", true)
	seedPropertyRoom(t, client, "room-2", "LK-1", true)

	var data struct {
		PropertyRooms struct {
			TotalCount int `json:"totalCount"`
			Edges      []struct {
				Node struct {
					RoomType string `json:"roomType"`
				} `json:"node"`
			} `json:"edges"`
		} `json:"propertyRooms"`
	}
	testutil.GQL(t, srv, `{
		propertyRooms(first: 10) {
			totalCount
			edges { node { roomType } }
		}
	}`, nil, &data)

	assert.Equal(t, 2, data.PropertyRooms.TotalCount)
	require.Len(t, data.PropertyRooms.Edges, 2)
	assert.Equal(t, "Bedroom", data.PropertyRooms.Edges[0].Node.RoomType)
}

func TestQueryPropertyUnitTypes_WithData(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)

	seedPropertyUnitType(t, client, "unit-1", "LK-1", true)
	seedPropertyUnitType(t, client, "unit-2", "LK-1", true)

	var data struct {
		PropertyUnitTypes connData `json:"propertyUnitTypes"`
	}
	testutil.GQL(t, srv, `{ propertyUnitTypes(first: 10) { totalCount } }`, nil, &data)
	assert.Equal(t, 2, data.PropertyUnitTypes.TotalCount)
}

func TestQueryOffices_WithData(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)

	seedOffice(t, client, "off-1", true)
	seedOffice(t, client, "off-2", true)

	var data struct {
		Offices struct {
			TotalCount int `json:"totalCount"`
			Edges      []struct {
				Node struct {
					OfficeName string `json:"officeName"`
				} `json:"node"`
			} `json:"edges"`
		} `json:"offices"`
	}
	testutil.GQL(t, srv, `{
		offices(first: 10) {
			totalCount
			edges { node { officeName } }
		}
	}`, nil, &data)

	assert.Equal(t, 2, data.Offices.TotalCount)
	require.Len(t, data.Offices.Edges, 2)
	assert.Contains(t, data.Offices.Edges[0].Node.OfficeName, "Office ")
}

func TestQueryOpenHouses_WithData(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)

	seedOpenHouse(t, client, "oh-1", "LK-1", true)
	seedOpenHouse(t, client, "oh-2", "LK-1", true)

	var data struct {
		OpenHouses struct {
			TotalCount int `json:"totalCount"`
			Edges      []struct {
				Node struct {
					OpenHouseStatus string `json:"openHouseStatus"`
				} `json:"node"`
			} `json:"edges"`
		} `json:"openHouses"`
	}
	testutil.GQL(t, srv, `{
		openHouses(first: 10) {
			totalCount
			edges { node { openHouseStatus } }
		}
	}`, nil, &data)

	assert.Equal(t, 2, data.OpenHouses.TotalCount)
	require.Len(t, data.OpenHouses.Edges, 2)
	assert.Equal(t, "Active", data.OpenHouses.Edges[0].Node.OpenHouseStatus)
}

func TestQuerySourceSystems_WithData(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)

	seedSourceSystem(t, client, "sys-1")
	seedSourceSystem(t, client, "sys-2")

	var data struct {
		SourceSystems struct {
			TotalCount int `json:"totalCount"`
			Edges      []struct {
				Node struct {
					SourceSystemName string `json:"sourceSystemName"`
				} `json:"node"`
			} `json:"edges"`
		} `json:"sourceSystems"`
	}
	testutil.GQL(t, srv, `{
		sourceSystems(first: 10) {
			totalCount
			edges { node { sourceSystemName } }
		}
	}`, nil, &data)

	assert.Equal(t, 2, data.SourceSystems.TotalCount)
	require.Len(t, data.SourceSystems.Edges, 2)
}

// ---- *Versions audit lists ----

// versionConn is the generic shape for version lists asserting changeType.
type versionConn struct {
	TotalCount int `json:"totalCount"`
	Edges      []struct {
		Node struct {
			ChangeType string `json:"changeType"`
		} `json:"node"`
	} `json:"edges"`
}

func assertSingleInsertVersion(t *testing.T, conn versionConn) {
	t.Helper()
	assert.Equal(t, 1, conn.TotalCount)
	require.Len(t, conn.Edges, 1)
	assert.Equal(t, "insert", conn.Edges[0].Node.ChangeType)
}

func TestQueryPropertyVersions_WithData(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedPropertyVersion(t, client, "LK-1", propertyversion.ChangeTypeInsert, true)

	var data struct {
		PropertyVersions versionConn `json:"propertyVersions"`
	}
	testutil.GQL(t, srv, `{ propertyVersions(first: 10) { totalCount edges { node { changeType } } } }`, nil, &data)
	assertSingleInsertVersion(t, data.PropertyVersions)
}

func TestQueryMediaVersions_WithData(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedMediaVersion(t, client, "med-1")

	var data struct {
		MediaVersions versionConn `json:"mediaVersions"`
	}
	testutil.GQL(t, srv, `{ mediaVersions(first: 10) { totalCount edges { node { changeType } } } }`, nil, &data)
	assertSingleInsertVersion(t, data.MediaVersions)
}

func TestQueryMemberVersions_WithData(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedMemberVersion(t, client, "mem-1")

	var data struct {
		MemberVersions versionConn `json:"memberVersions"`
	}
	testutil.GQL(t, srv, `{ memberVersions(first: 10) { totalCount edges { node { changeType } } } }`, nil, &data)
	assertSingleInsertVersion(t, data.MemberVersions)
}

func TestQueryOfficeVersions_WithData(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedOfficeVersion(t, client, "off-1")

	var data struct {
		OfficeVersions versionConn `json:"officeVersions"`
	}
	testutil.GQL(t, srv, `{ officeVersions(first: 10) { totalCount edges { node { changeType } } } }`, nil, &data)
	assertSingleInsertVersion(t, data.OfficeVersions)
}

func TestQueryOpenHouseVersions_WithData(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedOpenHouseVersion(t, client, "oh-1")

	var data struct {
		OpenHouseVersions versionConn `json:"openHouseVersions"`
	}
	testutil.GQL(t, srv, `{ openHouseVersions(first: 10) { totalCount edges { node { changeType } } } }`, nil, &data)
	assertSingleInsertVersion(t, data.OpenHouseVersions)
}

func TestQueryPropertyRoomVersions_WithData(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedPropertyRoomVersion(t, client, "room-1")

	var data struct {
		PropertyRoomVersions versionConn `json:"propertyRoomVersions"`
	}
	testutil.GQL(t, srv, `{ propertyRoomVersions(first: 10) { totalCount edges { node { changeType } } } }`, nil, &data)
	assertSingleInsertVersion(t, data.PropertyRoomVersions)
}

func TestQueryPropertyUnitTypeVersions_WithData(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedPropertyUnitTypeVersion(t, client, "unit-1")

	var data struct {
		PropertyUnitTypeVersions versionConn `json:"propertyUnitTypeVersions"`
	}
	testutil.GQL(t, srv, `{ propertyUnitTypeVersions(first: 10) { totalCount edges { node { changeType } } } }`, nil, &data)
	assertSingleInsertVersion(t, data.PropertyUnitTypeVersions)
}
