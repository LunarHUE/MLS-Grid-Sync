package graph_test

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
)

// seedAddress creates a Property with the address components the fuzzy
// search matches against. The combined-address index and the per-field
// predicates both read these columns.
func seedAddress(t *testing.T, client *ent.Client, id, streetNum, streetName, city, state, zip string, visible bool) {
	t.Helper()
	client.Property.Create().
		SetID(id).
		SetSourceModifiedAt(time.Now()).
		SetMlgCanView(visible).
		SetStreetNumber(streetNum).
		SetStreetName(streetName).
		SetCity(city).
		SetStateOrProvince(state).
		SetPostalCode(zip).
		SetUnparsedAddress(streetNum + " " + streetName + ", " + city + ", " + state + " " + zip).
		SaveX(context.Background())
}

// addressConn matches the shape both address queries return.
type addressConn struct {
	Edges []struct {
		Node struct {
			ID string `json:"id"`
		} `json:"node"`
	} `json:"edges"`
}

func (c addressConn) ids() []string {
	out := make([]string, 0, len(c.Edges))
	for _, e := range c.Edges {
		out = append(out, e.Node.ID)
	}
	sort.Strings(out)
	return out
}

// seedSearchFixture lays down a small, deterministic set of listings used
// across the address-search tests:
//
//	p1  123 Main St,  Austin TX 78704  (visible)
//	p2  456 Oak Ave,  Austin TX 78702  (visible)
//	p3  789 Maple Dr, Dallas TX 75201  (visible)
//	p4  123 Main St,  Austin TX 78704  (HIDDEN — mlg_can_view=false)
func seedSearchFixture(t *testing.T, client *ent.Client) {
	t.Helper()
	seedAddress(t, client, "p1", "123", "Main St", "Austin", "TX", "78704", true)
	seedAddress(t, client, "p2", "456", "Oak Ave", "Austin", "TX", "78702", true)
	seedAddress(t, client, "p3", "789", "Maple Dr", "Dallas", "TX", "75201", true)
	seedAddress(t, client, "p4", "123", "Main St", "Austin", "TX", "78704", false)
}

func TestPropertiesByAddress_FuzzyMatch(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedSearchFixture(t, client)

	var data struct {
		Conn addressConn `json:"propertiesByAddress"`
	}
	// A full street address discriminates to one listing. (A broad query
	// like "Austin" would, by design, match every Austin row on the shared
	// city token — combined-field trigram search filters, it does not rank.)
	testutil.GQL(t, srv, `query($q: String!) {
		propertiesByAddress(query: $q, first: 50) { edges { node { id } } }
	}`, map[string]any{"q": "123 Main St"}, &data)

	// Only the visible Main St listing — the Oak/Maple rows are dissimilar
	// and the hidden Main St duplicate (p4) is filtered by mlg_can_view.
	assert.Equal(t, []string{"p1"}, data.Conn.ids())
}

func TestPropertiesByAddress_ZipRoutesToExactMatch(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedSearchFixture(t, client)

	var data struct {
		Conn addressConn `json:"propertiesByAddress"`
	}
	testutil.GQL(t, srv, `query($q: String!) {
		propertiesByAddress(query: $q, first: 50) { edges { node { id } } }
	}`, map[string]any{"q": "78704"}, &data)

	// All-digit query → exact postal_code lookup; p4 shares the ZIP but is
	// hidden, so only p1 comes back.
	assert.Equal(t, []string{"p1"}, data.Conn.ids())
}

func TestPropertiesByAddress_EmptyQueryRejected(t *testing.T) {
	t.Parallel()
	srv, _ := testutil.NewTestServer(t)

	errs := testutil.GQLExpectError(t, srv, `{
		propertiesByAddress(query: "   ", first: 10) { edges { node { id } } }
	}`, nil)
	require.NotEmpty(t, errs)
}

func TestPropertiesByAddress_ThresholdOutOfRangeRejected(t *testing.T) {
	t.Parallel()
	srv, _ := testutil.NewTestServer(t)

	errs := testutil.GQLExpectError(t, srv, `{
		propertiesByAddress(query: "Main", threshold: 2.0, first: 10) { edges { node { id } } }
	}`, nil)
	require.NotEmpty(t, errs)
}

func TestPropertiesByAddressFields_CityTypoMatch(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedSearchFixture(t, client)

	var data struct {
		Conn addressConn `json:"propertiesByAddressFields"`
	}
	// "Austn" is a typo for Austin — trigram word_similarity still matches
	// both Austin listings; the Dallas one does not.
	testutil.GQL(t, srv, `query($c: String!) {
		propertiesByAddressFields(city: $c, first: 50) { edges { node { id } } }
	}`, map[string]any{"c": "Austn"}, &data)

	assert.Equal(t, []string{"p1", "p2"}, data.Conn.ids())
}

func TestPropertiesByAddressFields_ZipPrefix(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedSearchFixture(t, client)

	var data struct {
		Conn addressConn `json:"propertiesByAddressFields"`
	}
	// "787" is not a full 5-digit ZIP → prefix match: both 787xx Austin
	// listings, not the 75201 Dallas one.
	testutil.GQL(t, srv, `query($z: String!) {
		propertiesByAddressFields(zip: $z, first: 50) { edges { node { id } } }
	}`, map[string]any{"z": "787"}, &data)

	assert.Equal(t, []string{"p1", "p2"}, data.Conn.ids())
}

func TestPropertiesByAddressFields_StateCaseInsensitiveExact(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedSearchFixture(t, client)

	var data struct {
		Conn addressConn `json:"propertiesByAddressFields"`
	}
	// lower-case "tx" matches all three visible TX listings (p4 hidden).
	testutil.GQL(t, srv, `query($s: String!) {
		propertiesByAddressFields(state: $s, first: 50) { edges { node { id } } }
	}`, map[string]any{"s": "tx"}, &data)

	assert.Equal(t, []string{"p1", "p2", "p3"}, data.Conn.ids())
}

func TestPropertiesByAddressFields_AndCombined(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedSearchFixture(t, client)

	var data struct {
		Conn addressConn `json:"propertiesByAddressFields"`
	}
	// street AND city AND zip — all must hold, so only p1.
	testutil.GQL(t, srv, `query {
		propertiesByAddressFields(street: "Main", city: "Austin", zip: "78704", first: 50) {
			edges { node { id } }
		}
	}`, nil, &data)

	assert.Equal(t, []string{"p1"}, data.Conn.ids())
}

func TestPropertiesByAddressFields_NoFieldRejected(t *testing.T) {
	t.Parallel()
	srv, _ := testutil.NewTestServer(t)

	errs := testutil.GQLExpectError(t, srv, `{
		propertiesByAddressFields(first: 10) { edges { node { id } } }
	}`, nil)
	require.NotEmpty(t, errs)
}
