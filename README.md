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
| `serve` | Serve the GraphQL API over HTTP (playground at `/`, API at `/query`, health at `/healthz`) — see [docs/graphql-api.md](docs/graphql-api.md) |
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

Build locally:

```bash
docker build -t mls-grid-sync:dev .
```

## CI/CD

`.github/workflows/ci.yml`:

- **Pull requests** — `go build`, `go vet`, `go test ./...` inside the
  flake's `ci` dev shell (Testcontainers against real Postgres).
- **Push to `main`** — tests, then build & push
  `ghcr.io/lunarhue/mls-grid-sync` tagged `latest` + `sha-<short>`.
- **Tags `v*`** — additionally tagged `<version>` and `<major>.<minor>`.

No secrets to configure: GHCR pushes use the workflow's built-in
`GITHUB_TOKEN` (`packages: write` permission is set in the workflow).

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

**`internal/testutil/db.go`** — one shared `postgres:15-alpine` container
per test process (started on first use, reaped by Testcontainers' ryuk at
process exit). The ent schema is migrated once into a `mls_template`
database; each `NewTestDB(t)` call clones it via
`CREATE DATABASE … TEMPLATE` (~100ms), so every test gets an isolated,
fully-migrated database and `t.Parallel()` is safe. `NewTestDBWithSQL(t)`
additionally returns the raw `*sql.DB` for code that needs it (e.g. the
processor's advisory-lock helper).

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

### GraphQL test suite (`graph/*_test.go`)

Each test gets its own isolated database (cloned from the migrated
template). The suite is split by concern:

| File | What it covers |
|------|---------------|
| `resolver_test.go` | Lookup/Member/Property lists, forward pagination, `node()` basics, the full soft-key matrix (orphan/tombstoned/visible × 8 Property refs + `Member.office`), introspection |
| `lists_test.go` | Data-bearing coverage for every remaining root list, incl. all 7 `*Versions` audit lists |
| `node_test.go` | `node()` for every remaining type, `nodes(ids:)` ordering + null-on-miss, probe-order collision precedence, introspected field descriptions |
| `visibility_test.go` | Entity lists + `node()` hide `mlg_can_view=false` rows; version lists include them by design; SourceSystem has no flag |
| `pagination_test.go` | Backward (`last`/`before`) paging, `first: 0`, overfetch, invalid cursor and `first`+`last` errors |
| `wire_format_test.go` | JSON wire shapes of Decimal/UUID/StringArray/Map/Time/int16 fields, null handling, Property↔PropertyVersion parity |
| `edges_test.go` | Polymorphic `Property.media`, parent/child ent edges, Office self-reference, and the pinned edge-visibility gap |
| `seed_test.go` | Shared seed helpers (required-field cheat sheet per entity/version type) |

### Node resolution

Entity IDs are plain strings sourced from MLS Grid (e.g. `"ABC123"`). They
carry no type prefix, so the `node(id:)` resolver probes each entity table in
order until it finds a match. This is correct for a read-heavy API where relay
re-fetching is infrequent; a production optimisation would be to encode the
type in the ID. Visibility semantics (tombstoned rows hidden from lists and
`node()`, audit lists unfiltered) are documented in
[docs/graphql-api.md](docs/graphql-api.md).

### Adding a new test

1. Call `testutil.NewTestServer(t)` to get `(srv, client)`.
2. Seed data directly with `client.<Entity>.Create()...SaveX(ctx)`.
3. Call `testutil.GQL` with the query and assert on the result.

No mocking, no fixtures files — every test runs against real SQL.
