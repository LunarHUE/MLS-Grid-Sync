package graph_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/ent/propertyversion"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
)

// node(id:) coverage for the types resolver_test.go doesn't probe, plus
// nodes(ids:) and the probe-order collision pin.

func TestQueryNode_Media(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedMedia(t, client, "med-node-1", "LK-1", true)

	var data struct {
		Node struct {
			ID       string `json:"id"`
			MediaURL string `json:"mediaURL"`
		} `json:"node"`
	}
	testutil.GQL(t, srv, `query($id: ID!) {
		node(id: $id) { id ... on Media { mediaURL } }
	}`, map[string]any{"id": "med-node-1"}, &data)

	assert.Equal(t, "med-node-1", data.Node.ID)
	assert.Contains(t, data.Node.MediaURL, "med-node-1")
}

func TestQueryNode_Office(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedOffice(t, client, "off-node-1", true)

	var data struct {
		Node struct {
			ID         string `json:"id"`
			OfficeName string `json:"officeName"`
		} `json:"node"`
	}
	testutil.GQL(t, srv, `query($id: ID!) {
		node(id: $id) { id ... on Office { officeName } }
	}`, map[string]any{"id": "off-node-1"}, &data)

	assert.Equal(t, "Office off-node-1", data.Node.OfficeName)
}

func TestQueryNode_OpenHouse(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedOpenHouse(t, client, "oh-node-1", "LK-1", true)

	var data struct {
		Node struct {
			ID              string `json:"id"`
			OpenHouseStatus string `json:"openHouseStatus"`
		} `json:"node"`
	}
	testutil.GQL(t, srv, `query($id: ID!) {
		node(id: $id) { id ... on OpenHouse { openHouseStatus } }
	}`, map[string]any{"id": "oh-node-1"}, &data)

	assert.Equal(t, "Active", data.Node.OpenHouseStatus)
}

func TestQueryNode_PropertyRoom(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedPropertyRoom(t, client, "room-node-1", "LK-1", true)

	var data struct {
		Node struct {
			ID       string `json:"id"`
			RoomType string `json:"roomType"`
		} `json:"node"`
	}
	testutil.GQL(t, srv, `query($id: ID!) {
		node(id: $id) { id ... on PropertyRoom { roomType } }
	}`, map[string]any{"id": "room-node-1"}, &data)

	assert.Equal(t, "Bedroom", data.Node.RoomType)
}

func TestQueryNode_PropertyUnitType(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedPropertyUnitType(t, client, "unit-node-1", "LK-1", true)

	var data struct {
		Node struct {
			ID         string `json:"id"`
			ListingKey string `json:"listingKey"`
		} `json:"node"`
	}
	testutil.GQL(t, srv, `query($id: ID!) {
		node(id: $id) { id ... on PropertyUnitType { listingKey } }
	}`, map[string]any{"id": "unit-node-1"}, &data)

	assert.Equal(t, "LK-1", data.Node.ListingKey)
}

func TestQueryNode_SourceSystem(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedSourceSystem(t, client, "sys-node-1")

	var data struct {
		Node struct {
			ID               string `json:"id"`
			SourceSystemName string `json:"sourceSystemName"`
		} `json:"node"`
	}
	testutil.GQL(t, srv, `query($id: ID!) {
		node(id: $id) { id ... on SourceSystem { sourceSystemName } }
	}`, map[string]any{"id": "sys-node-1"}, &data)

	assert.Equal(t, "System sys-node-1", data.Node.SourceSystemName)
}

func TestQueryNode_PropertyVersion(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	versionID := seedPropertyVersion(t, client, "LK-1", propertyversion.ChangeTypeInsert, true)

	var data struct {
		Node struct {
			ID         string `json:"id"`
			ChangeType string `json:"changeType"`
			ListingKey string `json:"listingKey"`
		} `json:"node"`
	}
	testutil.GQL(t, srv, `query($id: ID!) {
		node(id: $id) { id ... on PropertyVersion { changeType listingKey } }
	}`, map[string]any{"id": versionID}, &data)

	assert.Equal(t, versionID, data.Node.ID)
	assert.Equal(t, "insert", data.Node.ChangeType)
	assert.Equal(t, "LK-1", data.Node.ListingKey)
}

func TestQueryNodes_Mixed(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedProperty(t, client, "prop-n1", true)
	seedMember(t, client, "mem-n1", true)
	seedLookup(t, client, "lkp-n1", true)

	var data struct {
		Nodes []*struct {
			ID       string `json:"id"`
			TypeName string `json:"__typename"`
		} `json:"nodes"`
	}
	testutil.GQL(t, srv, `query($ids: [ID!]!) {
		nodes(ids: $ids) { id __typename }
	}`, map[string]any{"ids": []string{"prop-n1", "mem-n1", "lkp-n1"}}, &data)

	require.Len(t, data.Nodes, 3)
	// Order mirrors the input ids.
	assert.Equal(t, "Property", data.Nodes[0].TypeName)
	assert.Equal(t, "Member", data.Nodes[1].TypeName)
	assert.Equal(t, "Lookup", data.Nodes[2].TypeName)
}

func TestQueryNodes_WithMiss(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedProperty(t, client, "prop-n2", true)

	var data struct {
		Nodes []*struct {
			ID string `json:"id"`
		} `json:"nodes"`
	}
	testutil.GQL(t, srv, `query($ids: [ID!]!) {
		nodes(ids: $ids) { id }
	}`, map[string]any{"ids": []string{"prop-n2", "does-not-exist"}}, &data)

	require.Len(t, data.Nodes, 2)
	require.NotNil(t, data.Nodes[0])
	assert.Equal(t, "prop-n2", data.Nodes[0].ID)
	assert.Nil(t, data.Nodes[1], "missing id must resolve to null, not an error")
}

// TestIntrospection_SoftKeyDescriptions verifies the docstrings added in
// extensions.graphql are actually served via introspection (they only
// exist at runtime if gqlgen was regenerated after editing the SDL).
func TestIntrospection_SoftKeyDescriptions(t *testing.T) {
	t.Parallel()
	srv, _ := testutil.NewTestServer(t)

	var data struct {
		Type struct {
			Fields []struct {
				Name        string  `json:"name"`
				Description *string `json:"description"`
			} `json:"fields"`
		} `json:"__type"`
	}
	testutil.GQL(t, srv, `{
		__type(name: "Property") {
			fields { name description }
		}
	}`, nil, &data)

	descs := map[string]string{}
	for _, f := range data.Type.Fields {
		if f.Description != nil {
			descs[f.Name] = *f.Description
		}
	}

	for _, field := range []string{"listAgent", "coListAgent", "buyerAgent", "coBuyerAgent",
		"listOffice", "coListOffice", "buyerOffice", "coBuyerOffice"} {
		require.Contains(t, descs, field, "field %s has no description", field)
		assert.Contains(t, descs[field], "soft key", "description of %s", field)
	}
	require.Contains(t, descs, "media")
	assert.Contains(t, descs["media"], "mlg_can_view")
}

// TestQueryNode_ProbeCollision pins the documented probe-order
// precedence: entityTables probes member before property, so a bare ID
// existing in both tables resolves to the Member. MLS Grid keys are
// namespaced upstream so collisions shouldn't happen in practice — this
// test exists so a probe-order change is a deliberate, visible decision.
func TestQueryNode_ProbeCollision(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedMember(t, client, "DUP-1", true)
	seedProperty(t, client, "DUP-1", true)

	var data struct {
		Node struct {
			TypeName string `json:"__typename"`
		} `json:"node"`
	}
	testutil.GQL(t, srv, `query($id: ID!) {
		node(id: $id) { __typename }
	}`, map[string]any{"id": "DUP-1"}, &data)

	assert.Equal(t, "Member", data.Node.TypeName)
}
