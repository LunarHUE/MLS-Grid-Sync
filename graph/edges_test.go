package graph_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/ent/media"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
)

// Edge-traversal tests: the polymorphic Property.media resolver and the
// entgql-generated edges (parent_listing_key children, Office self-ref).

func TestPropertyMedia_Polymorphic(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)

	seedProperty(t, client, "LK-med", true)
	// Visible photo for this listing — the only one that should appear.
	seedMedia(t, client, "med-visible", "LK-med", true)
	// Tombstoned photo for this listing — filtered out.
	seedMedia(t, client, "med-hidden", "LK-med", false)
	// Photo with the right key but the WRONG resource type — filtered out.
	client.Media.Create().
		SetID("med-wrong-type").
		SetSourceModifiedAt(time.Now()).
		SetMlgCanView(true).
		SetResourceType(media.ResourceTypeMember).
		SetResourceRecordKey("LK-med").
		SaveX(context.Background())

	var data struct {
		Node struct {
			Media []struct {
				ID string `json:"id"`
			} `json:"media"`
		} `json:"node"`
	}
	testutil.GQL(t, srv, `query($id: ID!) {
		node(id: $id) { ... on Property { media { id } } }
	}`, map[string]any{"id": "LK-med"}, &data)

	require.Len(t, data.Node.Media, 1)
	assert.Equal(t, "med-visible", data.Node.Media[0].ID)
}

// TestPropertyPrimaryPhoto covers the ranking contract of the primaryPhoto
// resolver: preferred_photo_yn=true beats any order value; without a flag
// the lowest order wins and NULL order sorts last; tombstoned rows never
// surface; no visible media resolves null.
func TestPropertyPrimaryPhoto(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)

	ptrI16 := func(v int16) *int16 { return &v }
	ptrBool := func(v bool) *bool { return &v }

	queryPrimary := func(listingKey string) *string {
		var data struct {
			Node struct {
				PrimaryPhoto *struct {
					ID string `json:"id"`
				} `json:"primaryPhoto"`
			} `json:"node"`
		}
		testutil.GQL(t, srv, `query($id: ID!) {
			node(id: $id) { ... on Property { primaryPhoto { id } } }
		}`, map[string]any{"id": listingKey}, &data)
		if data.Node.PrimaryPhoto == nil {
			return nil
		}
		return &data.Node.PrimaryPhoto.ID
	}

	// Flagged photo wins even though its order is higher.
	seedProperty(t, client, "LK-pp-flag", true)
	seedMediaPhoto(t, client, "pp-flag-first", "LK-pp-flag", true, ptrI16(0), nil)
	seedMediaPhoto(t, client, "pp-flag-pref", "LK-pp-flag", true, ptrI16(7), ptrBool(true))
	require.NotNil(t, queryPrimary("LK-pp-flag"))
	assert.Equal(t, "pp-flag-pref", *queryPrimary("LK-pp-flag"))

	// No flag anywhere: lowest order is the fallback.
	seedProperty(t, client, "LK-pp-fb", true)
	seedMediaPhoto(t, client, "pp-fb-late", "LK-pp-fb", true, ptrI16(3), nil)
	seedMediaPhoto(t, client, "pp-fb-first", "LK-pp-fb", true, ptrI16(1), ptrBool(false))
	require.NotNil(t, queryPrimary("LK-pp-fb"))
	assert.Equal(t, "pp-fb-first", *queryPrimary("LK-pp-fb"))

	// The flagged photo is tombstoned: fall back to a visible row rather
	// than surfacing it or returning null.
	seedProperty(t, client, "LK-pp-tomb", true)
	seedMediaPhoto(t, client, "pp-tomb-pref", "LK-pp-tomb", false, ptrI16(0), ptrBool(true))
	seedMediaPhoto(t, client, "pp-tomb-vis", "LK-pp-tomb", true, ptrI16(5), nil)
	require.NotNil(t, queryPrimary("LK-pp-tomb"))
	assert.Equal(t, "pp-tomb-vis", *queryPrimary("LK-pp-tomb"))

	// NULL order/preferred sort last, after any row with a concrete order.
	seedProperty(t, client, "LK-pp-null", true)
	seedMediaPhoto(t, client, "pp-null-bare", "LK-pp-null", true, nil, nil)
	seedMediaPhoto(t, client, "pp-null-ord", "LK-pp-null", true, ptrI16(2), nil)
	require.NotNil(t, queryPrimary("LK-pp-null"))
	assert.Equal(t, "pp-null-ord", *queryPrimary("LK-pp-null"))

	// No visible media at all: null, not an error.
	seedProperty(t, client, "LK-pp-none", true)
	assert.Nil(t, queryPrimary("LK-pp-none"))
}

func TestPropertyChildEdges(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)

	seedProperty(t, client, "LK-kids", true)
	seedPropertyRoomLinked(t, client, "room-kid", "LK-kids", true)
	seedPropertyUnitTypeLinked(t, client, "unit-kid", "LK-kids", true)
	seedOpenHouseLinked(t, client, "oh-kid", "LK-kids", true)

	var data struct {
		Node struct {
			Rooms      []struct{ ID string }
			UnitTypes  []struct{ ID string }
			OpenHouses []struct{ ID string }
		} `json:"node"`
	}
	testutil.GQL(t, srv, `query($id: ID!) {
		node(id: $id) {
			... on Property {
				rooms { id }
				unitTypes { id }
				openHouses { id }
			}
		}
	}`, map[string]any{"id": "LK-kids"}, &data)

	require.Len(t, data.Node.Rooms, 1)
	assert.Equal(t, "room-kid", data.Node.Rooms[0].ID)
	require.Len(t, data.Node.UnitTypes, 1)
	assert.Equal(t, "unit-kid", data.Node.UnitTypes[0].ID)
	require.Len(t, data.Node.OpenHouses, 1)
	assert.Equal(t, "oh-kid", data.Node.OpenHouses[0].ID)
}

// TestPropertyChildEdges_TombstonedStillVisible pins a KNOWN GAP: the
// entgql-generated child edges (rooms/unitTypes/openHouses) traverse
// parent_listing_key WITHOUT a mlg_can_view filter, unlike the lists and
// soft-key resolvers. Documented in docs/graphql-api.md. If this test
// starts failing because the edge filters, that gap was closed — update
// the docs and flip this test deliberately.
func TestPropertyChildEdges_TombstonedStillVisible(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)

	seedProperty(t, client, "LK-gap", true)
	seedPropertyRoomLinked(t, client, "room-gap-tomb", "LK-gap", false)

	var data struct {
		Node struct {
			Rooms []struct{ ID string }
		} `json:"node"`
	}
	testutil.GQL(t, srv, `query($id: ID!) {
		node(id: $id) { ... on Property { rooms { id } } }
	}`, map[string]any{"id": "LK-gap"}, &data)

	require.Len(t, data.Node.Rooms, 1, "ent edges currently do NOT filter visibility (documented gap)")
	assert.Equal(t, "room-gap-tomb", data.Node.Rooms[0].ID)
}

func TestOpenHouseProperty_Reverse(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)

	seedProperty(t, client, "LK-rev", true)
	seedOpenHouseLinked(t, client, "oh-rev", "LK-rev", true)

	var data struct {
		Node struct {
			Property *struct {
				ID string `json:"id"`
			} `json:"property"`
		} `json:"node"`
	}
	testutil.GQL(t, srv, `query($id: ID!) {
		node(id: $id) { ... on OpenHouse { property { id } } }
	}`, map[string]any{"id": "oh-rev"}, &data)

	require.NotNil(t, data.Node.Property)
	assert.Equal(t, "LK-rev", data.Node.Property.ID)
}

func TestOfficeSelfReference(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)

	seedOffice(t, client, "O-main", true)
	client.Office.Create().
		SetID("O-branch").
		SetSourceModifiedAt(time.Now()).
		SetMlgCanView(true).
		SetOfficeName("Branch").
		SetMainOfficeKey("O-main").
		SaveX(context.Background())

	var data struct {
		Node struct {
			MainOffice *struct {
				ID string `json:"id"`
			} `json:"mainOffice"`
		} `json:"node"`
	}
	testutil.GQL(t, srv, `query($id: ID!) {
		node(id: $id) { ... on Office { mainOffice { id } } }
	}`, map[string]any{"id": "O-branch"}, &data)

	require.NotNil(t, data.Node.MainOffice)
	assert.Equal(t, "O-main", data.Node.MainOffice.ID)

	var back struct {
		Node struct {
			Branches []struct {
				ID string `json:"id"`
			} `json:"branches"`
		} `json:"node"`
	}
	testutil.GQL(t, srv, `query($id: ID!) {
		node(id: $id) { ... on Office { branches { id } } }
	}`, map[string]any{"id": "O-main"}, &back)

	require.Len(t, back.Node.Branches, 1)
	assert.Equal(t, "O-branch", back.Node.Branches[0].ID)
}
