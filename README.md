# mls-grid-sync

Syncs MLS Grid data into a local PostgreSQL database and exposes it via a
GraphQL API.

## Testing

### Prerequisites

Tests spin up real PostgreSQL 15 containers via [Testcontainers](https://testcontainers.com/).
Docker must be running.

### Run all tests

```bash
go test ./...
```

### Run only GraphQL resolver tests

```bash
go test ./graph/... -v
```

### Run only sync service tests

```bash
go test ./sync/... -v
```

### Test infrastructure

**`internal/testutil/db.go`** — `NewTestDB(t)` starts a throwaway
`postgres:15-alpine` container, runs `ent` schema migrations, and returns an
`*ent.Client`. The container is terminated automatically via `t.Cleanup`.

**`internal/testutil/server.go`** — `NewTestServer(t)` wraps `NewTestDB` and
starts an `httptest.Server` with the full GraphQL handler attached. Returns
`(*httptest.Server, *ent.Client)`. Both are cleaned up automatically.

```go
srv, client := testutil.NewTestServer(t)
```

**`testutil.GQL`** posts a GraphQL query and unmarshals `data` into `out`. The
test fails immediately on HTTP errors, non-200 status, or GraphQL errors.

```go
var result struct {
    Lookups struct {
        TotalCount int `json:"totalCount"`
    } `json:"lookups"`
}
testutil.GQL(t, srv, `{ lookups(first: 10) { totalCount } }`, nil, &result)
```

Pass variables as a `map[string]any`:

```go
testutil.GQL(t, srv, `query($id: ID!) { node(id: $id) { id } }`,
    map[string]any{"id": "abc"}, &result)
```

### GraphQL resolver tests (`graph/resolver_test.go`)

Each test gets its own isolated database container.

| Test | What it covers |
|------|---------------|
| `TestQueryLookups_Empty` | Empty connection returns `totalCount: 0` |
| `TestQueryLookups_WithData` | Records inserted via ent show up in the API |
| `TestQueryLookups_Pagination` | `first`/`after` cursor pagination works |
| `TestQueryNode_Lookup` | Global `node(id:)` resolves a `Lookup` by ID |
| `TestQueryMembers_Empty` | Empty members connection |
| `TestQueryMembers_WithData` | Member records visible via GraphQL |
| `TestQueryMembers_Pagination` | Cursor pagination for members |
| `TestQueryNode_Member` | Global `node(id:)` resolves a `Member` |
| `TestQueryNode_NotFound` | Unknown ID returns `null` (not an error) |
| `TestQueryProperties_Empty` | Empty properties connection |
| `TestQueryProperties_WithData` | Property records visible via GraphQL |
| `TestQueryNode_Property` | Global `node(id:)` resolves a `Property` |
| `TestQueryOffices_Empty` | Empty offices connection |
| `TestQueryOpenHouses_Empty` | Empty open houses connection |
| `TestQuerySourceSystems_Empty` | Empty source systems connection |
| `TestIntrospection` | Schema contains expected type names |

### Node resolution

Entity IDs are plain strings sourced from MLS Grid (e.g. `"ABC123"`). They
carry no type prefix, so the `node(id:)` resolver probes each entity table in
order until it finds a match. This is correct for a read-heavy API where relay
re-fetching is infrequent; a production optimisation would be to encode the
type in the ID.

### Adding a new test

1. Call `testutil.NewTestServer(t)` to get `(srv, client)`.
2. Seed data directly with `client.<Entity>.Create()...SaveX(ctx)`.
3. Call `testutil.GQL` with the query and assert on the result.

No mocking, no fixtures files — every test runs against real SQL.
