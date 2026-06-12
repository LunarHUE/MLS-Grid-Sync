package graph_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
)

// A deeply cyclic rooms→property→rooms selection on a full page blows the
// complexity budget and is rejected before execution.
func TestComplexity_CyclicQueryRejected(t *testing.T) {
	t.Parallel()
	srv, _ := testutil.NewTestServer(t)

	const q = `{"query":"{ properties(first: 500) { edges { node { rooms { property { rooms { property { rooms { id } } } } } } } } }"}`
	status, env := rawPost(t, srv.URL, q)
	require.NotEmpty(t, env.Errors, "expected a complexity error (status %d)", status)
	assert.Equal(t, "COMPLEXITY_LIMIT_EXCEEDED", env.Errors[0].Extensions.Code)
}

// Introspection is exempt from the complexity budget: a full
// IntrospectionQuery must still succeed.
func TestComplexity_IntrospectionAllowed(t *testing.T) {
	t.Parallel()
	srv, _ := testutil.NewTestServer(t)

	body, err := json.Marshal(map[string]any{"query": introspectionQuery})
	require.NoError(t, err)

	status, env := rawPost(t, srv.URL, string(body))
	assert.Empty(t, env.Errors, "introspection rejected (status %d): %v", status, env.Errors)
	assert.NotNil(t, env.Data)
	assert.NotEqual(t, "null", string(env.Data))
}

// A full page selecting ~10 scalar fields plus pageInfo stays within
// budget — the limit must not block legitimate large reads.
func TestComplexity_FullPageQueryAllowed(t *testing.T) {
	t.Parallel()
	srv, _ := testutil.NewTestServer(t)

	const q = `{"query":"{ properties(first: 500) { totalCount pageInfo { hasNextPage endCursor } edges { node { id city stateOrProvince postalCode listPrice bedroomsTotal bathroomsFull yearBuilt latitude longitude } } } }"}`
	status, env := rawPost(t, srv.URL, q)
	assert.Empty(t, env.Errors, "full-page query rejected (status %d): %v", status, env.Errors)
}

// A huge first passed via a variable is clamped before scoring, so the
// query succeeds — proving complexity uses the clamped page size.
func TestComplexity_HugeFirstVariableClamped(t *testing.T) {
	t.Parallel()
	srv, client := testutil.NewTestServer(t)
	seedLookup(t, client, "lkp-clamp", true)

	var data struct {
		Lookups struct {
			TotalCount int `json:"totalCount"`
		} `json:"lookups"`
	}
	testutil.GQL(t, srv, `query($n: Int){ lookups(first: $n){ totalCount edges { node { id } } } }`,
		map[string]any{"n": 2000000000}, &data)

	assert.Equal(t, 1, data.Lookups.TotalCount)
}

// The canonical GraphQL IntrospectionQuery (the long form with the
// FullType / InputValue / TypeRef fragments).
const introspectionQuery = `
query IntrospectionQuery {
  __schema {
    queryType { name }
    mutationType { name }
    subscriptionType { name }
    types {
      ...FullType
    }
    directives {
      name
      description
      locations
      args {
        ...InputValue
      }
    }
  }
}

fragment FullType on __Type {
  kind
  name
  description
  fields(includeDeprecated: true) {
    name
    description
    args {
      ...InputValue
    }
    type {
      ...TypeRef
    }
    isDeprecated
    deprecationReason
  }
  inputFields {
    ...InputValue
  }
  interfaces {
    ...TypeRef
  }
  enumValues(includeDeprecated: true) {
    name
    description
    isDeprecated
    deprecationReason
  }
  possibleTypes {
    ...TypeRef
  }
}

fragment InputValue on __InputValue {
  name
  description
  type { ...TypeRef }
  defaultValue
}

fragment TypeRef on __Type {
  kind
  name
  ofType {
    kind
    name
    ofType {
      kind
      name
      ofType {
        kind
        name
        ofType {
          kind
          name
          ofType {
            kind
            name
            ofType {
              kind
              name
              ofType {
                kind
                name
              }
            }
          }
        }
      }
    }
  }
}
`
