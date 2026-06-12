# MLS-Grid-Sync

Syncs [MLS Grid](https://www.mlsgrid.com/) data into a PostgreSQL database and
exposes it via a GraphQL API.

Module path: `github.com/LunarHUE/MLS-Grid-Sync`. The binary is a single
Cobra CLI (`mls-cli`).

## Commands

```bash
go build -o mls-cli .
```

| Command | Purpose |
|---------|---------|
| `serve` | Serve the GraphQL API over HTTP (playground at `/`, API at `/query`, health at `/healthz`) |
| `init` | Full initial corpus import across all resources in FK-dependency order |
| `import <Resource>` | Initial bulk import of one resource (e.g. `import Property`) |
| `sync` | Continuous delta sync daemon |
| `worker` | Background attachment uploader |
| `worker-storage-cleanup` | Delete attachment objects from the storage backend |
| `reprocess` | Replay `raw_output` through the processor |
| `systems` | Probe MLS Grid for available originating systems |
| `validate-raw` / `validate-typed` | Mapping-coverage / drift validation |

## Configuration

Defaults live in `config/default.config.yaml` (embedded in the binary).
Override with a gitignored `config.yaml` in the working directory, or with
environment variables prefixed `MLS_SYNC_` (nested keys joined with `_`):

| Variable | Meaning |
|----------|---------|
| `MLS_SYNC_DATABASE_DSN` | PostgreSQL DSN |
| `MLS_SYNC_MLS_TOKEN` | MLS Grid API token (required by sync/import/worker; not by `serve`) |
| `MLS_SYNC_MLS_ORIGINATING_SYSTEM` | Originating system name (e.g. `actris`) |
| `MLS_SYNC_SERVER_ADDR` | Listen address for `serve` (default `:8080`) |
| `MLS_SYNC_STORAGE_BACKEND` | `fake` \| `local` \| `azure` \| `s3` |
| `MLS_SYNC_LOG_LEVEL` | Log level (default `info`) |

Azure/S3 credentials follow the respective SDK default chains; see
`config/default.config.yaml` for the full key list.

## Docker

The published image runs the CLI with `serve` as the default command:

```bash
docker pull ghcr.io/lunarhue/mls-grid-sync:latest

# GraphQL API
docker run -p 8080:8080 \
  -e MLS_SYNC_DATABASE_DSN="host=... user=... dbname=mls_sync ..." \
  ghcr.io/lunarhue/mls-grid-sync:latest

# Any other subcommand
docker run ghcr.io/lunarhue/mls-grid-sync:latest sync
```

Build locally (needs a GitHub token with read access to the private
`github.com/lunarhue/libs-go` module, passed as a BuildKit secret):

```bash
DOCKER_BUILDKIT=1 docker build --secret id=gh_token,env=GH_TOKEN -t mls-grid-sync:dev .
```

## CI/CD

`.github/workflows/ci.yml`:

- **Pull requests** — `go build`, `go vet`, `go test ./...` (Testcontainers
  against real Postgres).
- **Push to `main`** — tests, then build & push
  `ghcr.io/lunarhue/mls-grid-sync` tagged `latest` + `sha-<short>`.
- **Tags `v*`** — additionally tagged `<version>` and `<major>.<minor>`.

Required repository secret: `GH_PRIVATE_REPO_TOKEN` — a PAT with read access
to `lunarhue/libs-go` (used by `go mod download` on the runner and inside the
Docker build via a BuildKit secret). GHCR pushes use the built-in
`GITHUB_TOKEN`.

## Local development

```bash
docker compose up -d        # Postgres 15 on :5432
go build ./... && go vet ./...
go run . serve              # GraphQL playground on http://localhost:8080/
```

A Nix flake (`flake.nix`) provides the dev shell; the devcontainer sets it up
automatically via direnv.

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
