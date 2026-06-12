package graph_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
)

// Pagination edge cases on the lookups connection (simplest seed shape).
// Forward first/after is already covered by resolver_test.go.

type lookupPage struct {
	Lookups struct {
		TotalCount int `json:"totalCount"`
		PageInfo   struct {
			HasNextPage     bool    `json:"hasNextPage"`
			HasPreviousPage bool    `json:"hasPreviousPage"`
			StartCursor     *string `json:"startCursor"`
			EndCursor       *string `json:"endCursor"`
		} `json:"pageInfo"`
		Edges []struct {
			Node struct {
				ID string `json:"id"`
			} `json:"node"`
		} `json:"edges"`
	} `json:"lookups"`
}

func seedFiveLookups(t *testing.T, srvSeed func(id string)) {
	t.Helper()
	for i := 0; i < 5; i++ {
		srvSeed(fmt.Sprintf("lkp-%c", 'A'+i))
	}
}

func TestPagination_BackwardLastBefore(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedFiveLookups(t, func(id string) { seedLookup(t, client, id, true) })

	// Tail page: last 2 of 5.
	var tail lookupPage
	testutil.GQL(t, srv, `{
		lookups(last: 2) {
			totalCount
			pageInfo { hasNextPage hasPreviousPage startCursor endCursor }
			edges { node { id } }
		}
	}`, nil, &tail)

	assert.Equal(t, 5, tail.Lookups.TotalCount)
	require.Len(t, tail.Lookups.Edges, 2)
	assert.Equal(t, "lkp-D", tail.Lookups.Edges[0].Node.ID)
	assert.Equal(t, "lkp-E", tail.Lookups.Edges[1].Node.ID)
	assert.True(t, tail.Lookups.PageInfo.HasPreviousPage)

	// Walk backward: last 2 before the tail page's startCursor → C and B's slot.
	require.NotNil(t, tail.Lookups.PageInfo.StartCursor)
	var prev lookupPage
	testutil.GQL(t, srv, `query($before: Cursor) {
		lookups(last: 2, before: $before) {
			pageInfo { hasPreviousPage }
			edges { node { id } }
		}
	}`, map[string]any{"before": *tail.Lookups.PageInfo.StartCursor}, &prev)

	require.Len(t, prev.Lookups.Edges, 2)
	assert.Equal(t, "lkp-B", prev.Lookups.Edges[0].Node.ID)
	assert.Equal(t, "lkp-C", prev.Lookups.Edges[1].Node.ID)
	assert.True(t, prev.Lookups.PageInfo.HasPreviousPage)
}

func TestPagination_FirstZero(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedFiveLookups(t, func(id string) { seedLookup(t, client, id, true) })

	var data lookupPage
	testutil.GQL(t, srv, `{
		lookups(first: 0) {
			totalCount
			pageInfo { hasNextPage }
			edges { node { id } }
		}
	}`, nil, &data)

	assert.Equal(t, 5, data.Lookups.TotalCount)
	assert.Empty(t, data.Lookups.Edges)
	assert.True(t, data.Lookups.PageInfo.HasNextPage)
}

func TestPagination_Overfetch(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedFiveLookups(t, func(id string) { seedLookup(t, client, id, true) })

	var data lookupPage
	testutil.GQL(t, srv, `{
		lookups(first: 100) {
			totalCount
			pageInfo { hasNextPage }
			edges { node { id } }
		}
	}`, nil, &data)

	assert.Equal(t, 5, data.Lookups.TotalCount)
	assert.Len(t, data.Lookups.Edges, 5)
	assert.False(t, data.Lookups.PageInfo.HasNextPage)
}

func TestPagination_InvalidCursor(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedLookup(t, client, "lkp-1", true)

	errs := testutil.GQLExpectError(t, srv, `query($after: Cursor) {
		lookups(first: 2, after: $after) { totalCount }
	}`, map[string]any{"after": "not-a-real-cursor"})

	require.NotEmpty(t, errs)
}

// TestPagination_BothFirstLast pins entgql's observed behavior when both
// first and last are supplied (the relay spec discourages it). entgql
// rejects the combination with a validation error.
func TestPagination_BothFirstLast(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedFiveLookups(t, func(id string) { seedLookup(t, client, id, true) })

	errs := testutil.GQLExpectError(t, srv, `{
		lookups(first: 2, last: 2) { totalCount }
	}`, nil)

	require.NotEmpty(t, errs)
}
