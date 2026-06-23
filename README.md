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
| `MLS_SYNC_LOG_LEVEL` | Log level (default `info`); `debug` restores the per-page/per-chunk fetch lines |
| `MLS_SYNC_PROGRESS` | Import progress display: `auto` (default), `never`, or `always` |

Azure/S3 credentials follow the respective SDK default chains; see
`config/default.config.yaml` for the full key list.

### Import progress

`init` and `import` render progress through `internal/progress` (built on
[`mpb`](https://github.com/vbauerster/mpb)) as **two persistent bars, one per
worker goroutine**: **Fetch** (the producer / network) and **Process** (the
consumer / DB typing). Both bars are always on screen — a worker with no current
work stays visible, full, labeled **`idle`** — so you can see which of the two is
the bottleneck. They run concurrently in the pipelined path and one-at-a-time in
the default concurrent-fetch-then-process path.

```
# interactive terminal:
Fetch    Property  ▕████████████████████▏ 100%  589k/589k  idle
Process  property  ▕██████████░░░░░░░░░░▏  52%  306k/589k  15.5k/s  ETA 18s
```

- **Fetch** learns its denominator from a one-shot OData `$count`
  (`@odata.count`) on the first page; **Process** counts the pending
  `raw_output` rows for the resource — so both percentages/ETAs are real, not a
  running tally. (Media enqueue borrows the Process lane, relabeled.)
- **Piped / redirected / CI** — no bars (mpb would refresh-spam); throttled plain
  log lines instead, e.g. `Process property: 306,000/589,081 (52%) — 15.5k/s,
  ETA 18s`. Ordinary log lines print cleanly above the bars.
- Force a mode with `MLS_SYNC_PROGRESS=never|always` (default `auto`).
- The high-volume per-page (`Fetching … page N`, `fetch timing:`), per-batch
  (`processor[x]: N processed`), and per-chunk (`enqueue: X/Y processed`) lines
  are now `debug` level — set `MLS_SYNC_LOG_LEVEL=debug` to see them.

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
docker compose up -d        # Postgres 15 + PostGIS on :5432
go build ./... && go vet ./...
go run . serve              # GraphQL playground on http://localhost:8080/
```

A Nix flake (`flake.nix`) provides the dev shell; the devcontainer sets it up
automatically via direnv.

## Testing

### Prerequisites

Tests spin up a real PostgreSQL 15 + PostGIS container
(`imresamu/postgis:15-3.5-alpine`, multi-arch) via
[Testcontainers](https://testcontainers.com/). Docker must be running.

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
| `geo_test.go` | PostGIS geo search: radius (true meters), bbox, N-vertex polygons (open + closed rings), discontiguous multipolygons, validation errors, visibility/no-coords exclusion |
| `protocol_test.go` | HTTP wire contract: status codes (200/400/422), error envelope shapes, GET transport, operationName, transactioner panic guard |
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
