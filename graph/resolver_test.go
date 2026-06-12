package graph_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
)

// ---- tests ----

func TestQueryLookups_Empty(t *testing.T) {
	t.Parallel()
	srv, _ := testutil.NewTestServer(t)

	var data struct {
		Lookups struct {
			TotalCount int `json:"totalCount"`
		} `json:"lookups"`
	}
	testutil.GQL(t, srv, `{ lookups(first: 10) { totalCount } }`, nil, &data)
	assert.Equal(t, 0, data.Lookups.TotalCount)
}

func TestQueryLookups_WithData(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	ctx := context.Background()

	client.Lookup.Create().
		SetID("lookup-1").
		SetSourceModifiedAt(time.Now()).
		SetMlgCanView(true).
		SetLookupName("PropertyType").
		SetLookupValue("Residential").
		SaveX(ctx)

	client.Lookup.Create().
		SetID("lookup-2").
		SetSourceModifiedAt(time.Now()).
		SetMlgCanView(true).
		SetLookupName("PropertyType").
		SetLookupValue("Commercial").
		SaveX(ctx)

	var data struct {
		Lookups struct {
			TotalCount int `json:"totalCount"`
			Edges      []struct {
				Node struct {
					LookupName  string `json:"lookupName"`
					LookupValue string `json:"lookupValue"`
				} `json:"node"`
			} `json:"edges"`
		} `json:"lookups"`
	}
	testutil.GQL(t, srv, `{
		lookups(first: 10) {
			totalCount
			edges {
				node { lookupName lookupValue }
			}
		}
	}`, nil, &data)

	assert.Equal(t, 2, data.Lookups.TotalCount)
	require.Len(t, data.Lookups.Edges, 2)
}

func TestQueryLookups_Pagination(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	ctx := context.Background()

	for i := range 5 {
		client.Lookup.Create().
			SetID("lkp-" + string(rune('A'+i))).
			SetSourceModifiedAt(time.Now()).
			SetMlgCanView(true).
			SetLookupName("Feature").
			SetLookupValue("Value" + string(rune('A'+i))).
			SaveX(ctx)
	}

	// first page
	var page1 struct {
		Lookups struct {
			TotalCount int `json:"totalCount"`
			PageInfo   struct {
				HasNextPage bool   `json:"hasNextPage"`
				EndCursor   string `json:"endCursor"`
			} `json:"pageInfo"`
			Edges []struct {
				Node struct{ LookupValue string `json:"lookupValue"` } `json:"node"`
			} `json:"edges"`
		} `json:"lookups"`
	}
	testutil.GQL(t, srv, `{
		lookups(first: 3) {
			totalCount
			pageInfo { hasNextPage endCursor }
			edges { node { lookupValue } }
		}
	}`, nil, &page1)

	assert.Equal(t, 5, page1.Lookups.TotalCount)
	assert.Len(t, page1.Lookups.Edges, 3)
	assert.True(t, page1.Lookups.PageInfo.HasNextPage)

	// second page
	cursor := page1.Lookups.PageInfo.EndCursor
	var page2 struct {
		Lookups struct {
			Edges []struct {
				Node struct{ LookupValue string `json:"lookupValue"` } `json:"node"`
			} `json:"edges"`
			PageInfo struct {
				HasNextPage bool `json:"hasNextPage"`
			} `json:"pageInfo"`
		} `json:"lookups"`
	}
	testutil.GQL(t, srv, `query($after: Cursor) {
		lookups(first: 3, after: $after) {
			edges { node { lookupValue } }
			pageInfo { hasNextPage }
		}
	}`, map[string]any{"after": cursor}, &page2)

	assert.Len(t, page2.Lookups.Edges, 2)
	assert.False(t, page2.Lookups.PageInfo.HasNextPage)
}

func TestQueryNode_Lookup(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	ctx := context.Background()

	client.Lookup.Create().
		SetID("node-test-1").
		SetSourceModifiedAt(time.Now()).
		SetMlgCanView(true).
		SetLookupName("Amenity").
		SetLookupValue("Pool").
		SaveX(ctx)

	var data struct {
		Node struct {
			ID          string `json:"id"`
			LookupName  string `json:"lookupName"`
			LookupValue string `json:"lookupValue"`
		} `json:"node"`
	}
	testutil.GQL(t, srv, `query($id: ID!) {
		node(id: $id) {
			id
			... on Lookup {
				lookupName
				lookupValue
			}
		}
	}`, map[string]any{"id": "node-test-1"}, &data)

	assert.Equal(t, "node-test-1", data.Node.ID)
	assert.Equal(t, "Amenity", data.Node.LookupName)
	assert.Equal(t, "Pool", data.Node.LookupValue)
}

func TestQueryMembers_Empty(t *testing.T) {
	t.Parallel()
	srv, _ := testutil.NewTestServer(t)

	var data struct {
		Members struct {
			TotalCount int `json:"totalCount"`
		} `json:"members"`
	}
	testutil.GQL(t, srv, `{ members(first: 10) { totalCount } }`, nil, &data)
	assert.Equal(t, 0, data.Members.TotalCount)
}

func TestQueryMembers_WithData(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	ctx := context.Background()

	client.Member.Create().
		SetID("member-1").
		SetSourceModifiedAt(time.Now()).
		SetMlgCanView(true).
		SetMemberFirstName("Alice").
		SetMemberLastName("Smith").
		SaveX(ctx)

	client.Member.Create().
		SetID("member-2").
		SetSourceModifiedAt(time.Now()).
		SetMlgCanView(true).
		SetMemberFirstName("Bob").
		SetMemberLastName("Jones").
		SaveX(ctx)

	var data struct {
		Members struct {
			TotalCount int `json:"totalCount"`
			Edges      []struct {
				Node struct {
					ID              string `json:"id"`
					MemberFirstName string `json:"memberFirstName"`
					MemberLastName  string `json:"memberLastName"`
				} `json:"node"`
			} `json:"edges"`
		} `json:"members"`
	}
	testutil.GQL(t, srv, `{
		members(first: 10) {
			totalCount
			edges {
				node { id memberFirstName memberLastName }
			}
		}
	}`, nil, &data)

	assert.Equal(t, 2, data.Members.TotalCount)
	require.Len(t, data.Members.Edges, 2)
}

func TestQueryMembers_Pagination(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	ctx := context.Background()

	for i := range 5 {
		client.Member.Create().
			SetID("mem-" + string(rune('A'+i))).
			SetSourceModifiedAt(time.Now()).
			SetMlgCanView(true).
			SaveX(ctx)
	}

	var page1 struct {
		Members struct {
			TotalCount int `json:"totalCount"`
			PageInfo   struct {
				HasNextPage bool   `json:"hasNextPage"`
				EndCursor   string `json:"endCursor"`
			} `json:"pageInfo"`
			Edges []struct {
				Node struct{ ID string `json:"id"` } `json:"node"`
			} `json:"edges"`
		} `json:"members"`
	}
	testutil.GQL(t, srv, `{
		members(first: 3) {
			totalCount
			pageInfo { hasNextPage endCursor }
			edges { node { id } }
		}
	}`, nil, &page1)

	assert.Equal(t, 5, page1.Members.TotalCount)
	assert.Len(t, page1.Members.Edges, 3)
	assert.True(t, page1.Members.PageInfo.HasNextPage)

	var page2 struct {
		Members struct {
			Edges    []struct{ Node struct{ ID string `json:"id"` } `json:"node"` } `json:"edges"`
			PageInfo struct{ HasNextPage bool `json:"hasNextPage"` } `json:"pageInfo"`
		} `json:"members"`
	}
	testutil.GQL(t, srv, `query($after: Cursor) {
		members(first: 3, after: $after) {
			edges { node { id } }
			pageInfo { hasNextPage }
		}
	}`, map[string]any{"after": page1.Members.PageInfo.EndCursor}, &page2)

	assert.Len(t, page2.Members.Edges, 2)
	assert.False(t, page2.Members.PageInfo.HasNextPage)
}

func TestQueryNode_Member(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	ctx := context.Background()

	client.Member.Create().
		SetID("member-node-1").
		SetSourceModifiedAt(time.Now()).
		SetMlgCanView(true).
		SetMemberFullName("Jane Doe").
		SaveX(ctx)

	var data struct {
		Node struct {
			ID             string `json:"id"`
			MemberFullName string `json:"memberFullName"`
		} `json:"node"`
	}
	testutil.GQL(t, srv, `query($id: ID!) {
		node(id: $id) {
			id
			... on Member {
				memberFullName
			}
		}
	}`, map[string]any{"id": "member-node-1"}, &data)

	assert.Equal(t, "member-node-1", data.Node.ID)
	assert.Equal(t, "Jane Doe", data.Node.MemberFullName)
}

func TestQueryNode_NotFound(t *testing.T) {
	t.Parallel()
	srv, _ := testutil.NewTestServer(t)

	var data struct {
		Node *struct {
			ID string `json:"id"`
		} `json:"node"`
	}
	testutil.GQL(t, srv, `query($id: ID!) {
		node(id: $id) { id }
	}`, map[string]any{"id": "does-not-exist"}, &data)

	assert.Nil(t, data.Node)
}

func TestQueryProperties_Empty(t *testing.T) {
	t.Parallel()
	srv, _ := testutil.NewTestServer(t)

	var data struct {
		Properties struct {
			TotalCount int `json:"totalCount"`
		} `json:"properties"`
	}
	testutil.GQL(t, srv, `{ properties(first: 10) { totalCount } }`, nil, &data)
	assert.Equal(t, 0, data.Properties.TotalCount)
}

func TestQueryProperties_WithData(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	ctx := context.Background()

	client.Property.Create().
		SetID("listing-1").
		SetSourceModifiedAt(time.Now()).
		SetMlgCanView(true).
		SetStreetNumber("123").
		SetStreetName("Main St").
		SetCity("Highpoint").
		SetStateOrProvince("NC").
		SaveX(ctx)

	var data struct {
		Properties struct {
			TotalCount int `json:"totalCount"`
			Edges      []struct {
				Node struct {
					ID           string `json:"id"`
					StreetNumber string `json:"streetNumber"`
					StreetName   string `json:"streetName"`
					City         string `json:"city"`
				} `json:"node"`
			} `json:"edges"`
		} `json:"properties"`
	}
	testutil.GQL(t, srv, `{
		properties(first: 10) {
			totalCount
			edges {
				node { id streetNumber streetName city }
			}
		}
	}`, nil, &data)

	assert.Equal(t, 1, data.Properties.TotalCount)
	require.Len(t, data.Properties.Edges, 1)
	assert.Equal(t, "listing-1", data.Properties.Edges[0].Node.ID)
	assert.Equal(t, "123", data.Properties.Edges[0].Node.StreetNumber)
	assert.Equal(t, "Highpoint", data.Properties.Edges[0].Node.City)
}

func TestQueryNode_Property(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	ctx := context.Background()

	client.Property.Create().
		SetID("prop-node-1").
		SetSourceModifiedAt(time.Now()).
		SetMlgCanView(true).
		SetPostalCode("27260").
		SaveX(ctx)

	var data struct {
		Node struct {
			ID         string `json:"id"`
			PostalCode string `json:"postalCode"`
		} `json:"node"`
	}
	testutil.GQL(t, srv, `query($id: ID!) {
		node(id: $id) {
			id
			... on Property {
				postalCode
			}
		}
	}`, map[string]any{"id": "prop-node-1"}, &data)

	assert.Equal(t, "prop-node-1", data.Node.ID)
	assert.Equal(t, "27260", data.Node.PostalCode)
}

func TestQueryOffices_Empty(t *testing.T) {
	t.Parallel()
	srv, _ := testutil.NewTestServer(t)

	var data struct {
		Offices struct {
			TotalCount int `json:"totalCount"`
		} `json:"offices"`
	}
	testutil.GQL(t, srv, `{ offices(first: 10) { totalCount } }`, nil, &data)
	assert.Equal(t, 0, data.Offices.TotalCount)
}

func TestQueryOpenHouses_Empty(t *testing.T) {
	t.Parallel()
	srv, _ := testutil.NewTestServer(t)

	var data struct {
		OpenHouses struct {
			TotalCount int `json:"totalCount"`
		} `json:"openHouses"`
	}
	testutil.GQL(t, srv, `{ openHouses(first: 10) { totalCount } }`, nil, &data)
	assert.Equal(t, 0, data.OpenHouses.TotalCount)
}

func TestQuerySourceSystems_Empty(t *testing.T) {
	t.Parallel()
	srv, _ := testutil.NewTestServer(t)

	var data struct {
		SourceSystems struct {
			TotalCount int `json:"totalCount"`
		} `json:"sourceSystems"`
	}
	testutil.GQL(t, srv, `{ sourceSystems(first: 10) { totalCount } }`, nil, &data)
	assert.Equal(t, 0, data.SourceSystems.TotalCount)
}

// ----------------------------------------------------------------------
// Soft-key resolver tests — agent/office references on Property and
// Member.office. Each resolver applies mlg_can_view=true and returns null
// when the keyed entity is missing, hidden, or the key is nil.
// ----------------------------------------------------------------------

// softKeyResolverCase is the table for all eight Property forward resolvers.
// Each row tests three states against ONE field at a time, holding the other
// seven role-keys nil so we don't cross-contaminate visibility checks.
type softKeyResolverCase struct {
	field     string  // GraphQL selection on Property, e.g. "listAgent"
	keyField  string  // GraphQL key field, e.g. "listAgentKey"
	keySetter func(c *ent.PropertyCreate, key string)
	target    string  // "Member" or "Office"
}

func TestPropertySoftKeyResolvers(t *testing.T) {
	t.Parallel()
	cases := []softKeyResolverCase{
		{"listAgent", "listAgentKey", func(c *ent.PropertyCreate, k string) { c.SetListAgentKey(k) }, "Member"},
		{"coListAgent", "coListAgentKey", func(c *ent.PropertyCreate, k string) { c.SetCoListAgentKey(k) }, "Member"},
		{"buyerAgent", "buyerAgentKey", func(c *ent.PropertyCreate, k string) { c.SetBuyerAgentKey(k) }, "Member"},
		{"coBuyerAgent", "coBuyerAgentKey", func(c *ent.PropertyCreate, k string) { c.SetCoBuyerAgentKey(k) }, "Member"},
		{"listOffice", "listOfficeKey", func(c *ent.PropertyCreate, k string) { c.SetListOfficeKey(k) }, "Office"},
		{"coListOffice", "coListOfficeKey", func(c *ent.PropertyCreate, k string) { c.SetCoListOfficeKey(k) }, "Office"},
		{"buyerOffice", "buyerOfficeKey", func(c *ent.PropertyCreate, k string) { c.SetBuyerOfficeKey(k) }, "Office"},
		{"coBuyerOffice", "coBuyerOfficeKey", func(c *ent.PropertyCreate, k string) { c.SetCoBuyerOfficeKey(k) }, "Office"},
	}

	for _, tc := range cases {
		t.Run(tc.field+"/orphan", func(t *testing.T) {
			srv, client := testutil.NewTestServer(t)
			ctx := context.Background()

			pc := client.Property.Create().
				SetID("LK-" + tc.field + "-orphan").
				SetSourceModifiedAt(time.Now()).
				SetMlgCanView(true)
			tc.keySetter(pc, "ABSENT-KEY")
			pc.SaveX(ctx)

			data := queryPropertyResolverField(t, srv, "LK-"+tc.field+"-orphan", tc.field, tc.keyField)
			assert.Equal(t, "ABSENT-KEY", data.KeyValue,
				"%s string is still served when the entity resolves null", tc.keyField)
			assert.Nil(t, data.Resolved, "%s must be null for an orphan key", tc.field)
		})

		t.Run(tc.field+"/tombstoned", func(t *testing.T) {
			srv, client := testutil.NewTestServer(t)
			ctx := context.Background()

			// Seed a target row that is present-but-hidden.
			switch tc.target {
			case "Member":
				client.Member.Create().
					SetID("HIDDEN-1").
					SetSourceModifiedAt(time.Now()).
					SetMlgCanView(false).
					SaveX(ctx)
			case "Office":
				client.Office.Create().
					SetID("HIDDEN-1").
					SetSourceModifiedAt(time.Now()).
					SetMlgCanView(false).
					SaveX(ctx)
			}

			pc := client.Property.Create().
				SetID("LK-" + tc.field + "-hidden").
				SetSourceModifiedAt(time.Now()).
				SetMlgCanView(true)
			tc.keySetter(pc, "HIDDEN-1")
			pc.SaveX(ctx)

			data := queryPropertyResolverField(t, srv, "LK-"+tc.field+"-hidden", tc.field, tc.keyField)
			assert.Equal(t, "HIDDEN-1", data.KeyValue)
			assert.Nil(t, data.Resolved,
				"%s must be null for tombstoned target — visibility filter takes precedence over FK presence", tc.field)
		})

		t.Run(tc.field+"/visible", func(t *testing.T) {
			srv, client := testutil.NewTestServer(t)
			ctx := context.Background()

			switch tc.target {
			case "Member":
				client.Member.Create().
					SetID("VISIBLE-1").
					SetSourceModifiedAt(time.Now()).
					SetMlgCanView(true).
					SaveX(ctx)
			case "Office":
				client.Office.Create().
					SetID("VISIBLE-1").
					SetSourceModifiedAt(time.Now()).
					SetMlgCanView(true).
					SaveX(ctx)
			}

			pc := client.Property.Create().
				SetID("LK-" + tc.field + "-visible").
				SetSourceModifiedAt(time.Now()).
				SetMlgCanView(true)
			tc.keySetter(pc, "VISIBLE-1")
			pc.SaveX(ctx)

			data := queryPropertyResolverField(t, srv, "LK-"+tc.field+"-visible", tc.field, tc.keyField)
			assert.Equal(t, "VISIBLE-1", data.KeyValue)
			require.NotNil(t, data.Resolved, "%s must resolve when target is visible", tc.field)
			assert.Equal(t, "VISIBLE-1", *data.Resolved)
		})
	}
}

// resolverResult holds the parsed { keyField, resolvedField { id } } pair.
type resolverResult struct {
	KeyValue string
	Resolved *string // resolved entity's id; nil if the resolver returned null
}

func queryPropertyResolverField(t *testing.T, srv *httptest.Server, listingKey, field, keyField string) resolverResult {
	t.Helper()
	q := `query($id: ID!) {
		node(id: $id) {
			... on Property {
				` + keyField + `
				` + field + ` { id }
			}
		}
	}`
	var raw struct {
		Node map[string]any `json:"node"`
	}
	testutil.GQL(t, srv, q, map[string]any{"id": listingKey}, &raw)

	out := resolverResult{}
	if v, ok := raw.Node[keyField].(string); ok {
		out.KeyValue = v
	}
	if obj, ok := raw.Node[field].(map[string]any); ok && obj != nil {
		if id, ok := obj["id"].(string); ok {
			out.Resolved = &id
		}
	}
	return out
}

// TestMemberSoftKeyOffice covers Member.office with the same three states.
func TestMemberSoftKeyOffice(t *testing.T) {
	t.Parallel()
	type state struct {
		name           string
		officeMlgCanView *bool // nil = no Office row seeded; true/false = seeded with this visibility
		wantResolved   bool
	}
	cases := []state{
		{"orphan", nil, false},
		{"tombstoned", boolPtr(false), false},
		{"visible", boolPtr(true), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, client := testutil.NewTestServer(t)
			ctx := context.Background()

			officeKey := "OFFICE-" + tc.name
			if tc.officeMlgCanView != nil {
				client.Office.Create().
					SetID(officeKey).
					SetSourceModifiedAt(time.Now()).
					SetMlgCanView(*tc.officeMlgCanView).
					SaveX(ctx)
			}

			client.Member.Create().
				SetID("MEM-" + tc.name).
				SetSourceModifiedAt(time.Now()).
				SetMlgCanView(true).
				SetOfficeKey(officeKey).
				SaveX(ctx)

			var data struct {
				Node struct {
					OfficeKey string `json:"officeKey"`
					Office    *struct {
						ID string `json:"id"`
					} `json:"office"`
				} `json:"node"`
			}
			testutil.GQL(t, srv, `query($id: ID!) {
				node(id: $id) {
					... on Member {
						officeKey
						office { id }
					}
				}
			}`, map[string]any{"id": "MEM-" + tc.name}, &data)

			assert.Equal(t, officeKey, data.Node.OfficeKey, "officeKey string is always served")
			if tc.wantResolved {
				require.NotNil(t, data.Node.Office)
				assert.Equal(t, officeKey, data.Node.Office.ID)
			} else {
				assert.Nil(t, data.Node.Office, "office must be null for %s state", tc.name)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }

func TestIntrospection(t *testing.T) {
	t.Parallel()
	srv, _ := testutil.NewTestServer(t)

	var data struct {
		Schema struct {
			Types []struct {
				Name string `json:"name"`
			} `json:"types"`
		} `json:"__schema"`
	}
	testutil.GQL(t, srv, `{ __schema { types { name } } }`, nil, &data)
	assert.NotEmpty(t, data.Schema.Types)

	var typeNames []string
	for _, t := range data.Schema.Types {
		typeNames = append(typeNames, t.Name)
	}
	assert.Contains(t, typeNames, "Property")
	assert.Contains(t, typeNames, "Lookup")
	assert.Contains(t, typeNames, "Member")
}
