package graph_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
)

// Phase 3 — GraphQL filtering (`where`) + ordering (`orderBy`) via entgql.
// These lock in the LIST_PRICE order field and the generated where-input
// predicates on the properties connection.

func seedPricedProperty(t *testing.T, client *ent.Client, id, city, price string) {
	t.Helper()
	client.Property.Create().
		SetID(id).
		SetSourceModifiedAt(time.Now()).
		SetMlgCanView(true).
		SetCity(city).
		SetListPrice(decimal.RequireFromString(price)).
		SaveX(context.Background())
}

// priceConn unpacks `properties(...) { edges { node { listPrice } } }`.
type priceConn struct {
	Properties struct {
		TotalCount int `json:"totalCount"`
		Edges      []struct {
			Node struct {
				ListPrice string `json:"listPrice"`
			} `json:"node"`
		} `json:"edges"`
	} `json:"properties"`
}

func TestProperties_OrderByListPrice(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)

	seedPricedProperty(t, client, "p-mid", "Austin", "300000")
	seedPricedProperty(t, client, "p-low", "Austin", "150000")
	seedPricedProperty(t, client, "p-high", "Austin", "750000")

	// Ascending.
	var asc priceConn
	testutil.GQL(t, srv, `{
		properties(first: 10, orderBy: {field: LIST_PRICE, direction: ASC}) {
			totalCount
			edges { node { listPrice } }
		}
	}`, nil, &asc)
	require.Len(t, asc.Properties.Edges, 3)
	assert.Equal(t, 3, asc.Properties.TotalCount)
	assertPriceOrder(t, asc, []string{"150000", "300000", "750000"})

	// Descending.
	var desc priceConn
	testutil.GQL(t, srv, `{
		properties(first: 10, orderBy: {field: LIST_PRICE, direction: DESC}) {
			edges { node { listPrice } }
		}
	}`, nil, &desc)
	require.Len(t, desc.Properties.Edges, 3)
	assertPriceOrder(t, desc, []string{"750000", "300000", "150000"})
}

func TestProperties_WhereListPriceRange(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)

	seedPricedProperty(t, client, "p-mid", "Austin", "300000")
	seedPricedProperty(t, client, "p-low", "Austin", "150000")
	seedPricedProperty(t, client, "p-high", "Austin", "750000")

	// listPriceGTE filters out the 150k listing.
	var data priceConn
	testutil.GQL(t, srv, `{
		properties(
			first: 10,
			where: {listPriceGTE: "250000"},
			orderBy: {field: LIST_PRICE, direction: ASC}
		) {
			totalCount
			edges { node { listPrice } }
		}
	}`, nil, &data)
	assert.Equal(t, 2, data.Properties.TotalCount)
	assertPriceOrder(t, data, []string{"300000", "750000"})
}

func assertPriceOrder(t *testing.T, conn priceConn, want []string) {
	t.Helper()
	require.Len(t, conn.Properties.Edges, len(want))
	for i, w := range want {
		got := conn.Properties.Edges[i].Node.ListPrice
		assert.Truef(t,
			decimal.RequireFromString(got).Equal(decimal.RequireFromString(w)),
			"edge[%d] listPrice = %q, want %q", i, got, w)
	}
}
