package graph_test

import (
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/ent/propertyversion"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
)

// Visibility semantics: entity lists and node() expose only
// mlg_can_view=true rows; *Versions audit lists intentionally include
// tombstoned rows; SourceSystem has no visibility flag at all.

func TestVisibility_Lookups_HidesTombstoned(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedLookup(t, client, "lkp-vis", true)
	seedLookup(t, client, "lkp-tomb", false)
	assertListExactlyOne(t, srv, "lookups", "lkp-vis")
}

func TestVisibility_MediaSlice_HidesTombstoned(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedMedia(t, client, "med-vis", "LK-1", true)
	seedMedia(t, client, "med-tomb", "LK-1", false)
	assertListExactlyOne(t, srv, "mediaSlice", "med-vis")
}

func TestVisibility_Members_HidesTombstoned(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedMember(t, client, "mem-vis", true)
	seedMember(t, client, "mem-tomb", false)
	assertListExactlyOne(t, srv, "members", "mem-vis")
}

func TestVisibility_Offices_HidesTombstoned(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedOffice(t, client, "off-vis", true)
	seedOffice(t, client, "off-tomb", false)
	assertListExactlyOne(t, srv, "offices", "off-vis")
}

func TestVisibility_OpenHouses_HidesTombstoned(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedOpenHouse(t, client, "oh-vis", "LK-1", true)
	seedOpenHouse(t, client, "oh-tomb", "LK-1", false)
	assertListExactlyOne(t, srv, "openHouses", "oh-vis")
}

func TestVisibility_Properties_HidesTombstoned(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedProperty(t, client, "prop-vis", true)
	seedProperty(t, client, "prop-tomb", false)
	assertListExactlyOne(t, srv, "properties", "prop-vis")
}

func TestVisibility_PropertyRooms_HidesTombstoned(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedPropertyRoom(t, client, "room-vis", "LK-1", true)
	seedPropertyRoom(t, client, "room-tomb", "LK-1", false)
	assertListExactlyOne(t, srv, "propertyRooms", "room-vis")
}

func TestVisibility_PropertyUnitTypes_HidesTombstoned(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedPropertyUnitType(t, client, "unit-vis", "LK-1", true)
	seedPropertyUnitType(t, client, "unit-tomb", "LK-1", false)
	assertListExactlyOne(t, srv, "propertyUnitTypes", "unit-vis")
}

// TestVisibility_Versions_IncludeTombstoned: audit lists must NOT filter —
// a delete version with mlg_can_view=false IS the historical record.
func TestVisibility_Versions_IncludeTombstoned(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedPropertyVersion(t, client, "LK-del", propertyversion.ChangeTypeDelete, false)

	var data struct {
		PropertyVersions struct {
			TotalCount int `json:"totalCount"`
			Edges      []struct {
				Node struct {
					ChangeType string `json:"changeType"`
					MlgCanView bool   `json:"mlgCanView"`
				} `json:"node"`
			} `json:"edges"`
		} `json:"propertyVersions"`
	}
	testutil.GQL(t, srv, `{
		propertyVersions(first: 10) {
			totalCount
			edges { node { changeType mlgCanView } }
		}
	}`, nil, &data)

	require.Equal(t, 1, data.PropertyVersions.TotalCount)
	assert.Equal(t, "delete", data.PropertyVersions.Edges[0].Node.ChangeType)
	assert.False(t, data.PropertyVersions.Edges[0].Node.MlgCanView)
}

func TestVisibility_Node_HidesTombstonedProperty(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedProperty(t, client, "prop-hidden", false)

	var data struct {
		Node *struct {
			ID string `json:"id"`
		} `json:"node"`
	}
	testutil.GQL(t, srv, `query($id: ID!) { node(id: $id) { id } }`,
		map[string]any{"id": "prop-hidden"}, &data)

	assert.Nil(t, data.Node, "tombstoned entity must not be fetchable via node()")
}

func TestVisibility_Node_VersionAlwaysVisible(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	versionID := seedPropertyVersion(t, client, "LK-del", propertyversion.ChangeTypeDelete, false)

	var data struct {
		Node *struct {
			ID string `json:"id"`
		} `json:"node"`
	}
	testutil.GQL(t, srv, `query($id: ID!) { node(id: $id) { id } }`,
		map[string]any{"id": versionID}, &data)

	require.NotNil(t, data.Node, "audit version rows stay node-fetchable regardless of visibility")
	assert.Equal(t, versionID, data.Node.ID)
}

func TestVisibility_SourceSystem_NoFilter(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedSourceSystem(t, client, "sys-vis")

	var data struct {
		SourceSystems struct {
			TotalCount int `json:"totalCount"`
		} `json:"sourceSystems"`
	}
	testutil.GQL(t, srv, `{ sourceSystems(first: 10) { totalCount } }`, nil, &data)
	assert.Equal(t, 1, data.SourceSystems.TotalCount)
}

// assertListExactlyOne asserts `<listField>` returns exactly one edge
// whose node ID is visibleID — the tombstoned sibling seeded next to it
// must be absent.
func assertListExactlyOne(t *testing.T, srv *httptest.Server, listField, visibleID string) {
	t.Helper()

	query := fmt.Sprintf(`{ %s(first: 10) { totalCount edges { node { id } } } }`, listField)
	var raw map[string]struct {
		TotalCount int `json:"totalCount"`
		Edges      []struct {
			Node struct {
				ID string `json:"id"`
			} `json:"node"`
		} `json:"edges"`
	}
	testutil.GQL(t, srv, query, nil, &raw)

	conn := raw[listField]
	require.Equal(t, 1, conn.TotalCount, "tombstoned row leaked into %s", listField)
	require.Len(t, conn.Edges, 1)
	assert.Equal(t, visibleID, conn.Edges[0].Node.ID)
}
