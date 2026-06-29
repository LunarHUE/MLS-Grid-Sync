# Platform Roadmap — Feature Build-Out

Status legend: ⬜ pending · 🔶 in progress · ✅ done

| # | Phase | Status |
|---|-------|--------|
| 1 | API protection: CORS, optional API key, page-size cap, complexity limit | ✅ |
| 2 | Multi-arch Docker image (amd64 + arm64) | ✅ |
| 3 | GraphQL filtering (`where`) + ordering (`orderBy`) via entgql | ✅ |
| 4 | Edge-visibility fix via ent interceptors | ⬜ |
| 5 | `propertiesNearest` with `distanceMeters` (nearest-first) | ⬜ |
| 6 | Observability: `/metrics`, request logging, `syncStatus` query | ⬜ |
| 7 | `prune` command (raw_output retention) | ⬜ |
| 8 | Documentation refresh + full audit | ⬜ |

## Phase 1 — API protection
- CORS middleware on `serve` (config `server.cors_allowed_origins` /
  `MLS_SYNC_SERVER_CORS_ALLOWED_ORIGINS`, comma-separated, default `*`).
- Optional API key on `/query`: `server.api_key` / `MLS_SYNC_SERVER_API_KEY`;
  empty = disabled. Accepts `X-API-Key` or `Authorization: Bearer`.
  `/healthz` and the playground page stay open.
- Page-size guard: `first`/`last` clamped to 500; unpaginated list queries
  get an implicit `first: 500`.
- gqlgen complexity limit to stop deeply-nested cyclic queries
  (`property { rooms { property { … } } }`).

## Phase 2 — Multi-arch image
- Dockerfile: `--platform=$BUILDPLATFORM` builder + `GOOS=$TARGETOS
  GOARCH=$TARGETARCH` cross-compile (native-speed, no QEMU for the Go build).
- CI: `docker/setup-qemu-action` + `platforms: linux/amd64,linux/arm64`.

## Phase 3 — Filtering & ordering
- `entgql.WithWhereInputs(true)` in `ent/entc.go` → `<Type>WhereInput` on
  every connection (also gives `propertyVersions(where: {listingKey: …})`
  for per-listing audit history).
- `entgql.OrderField` annotations: `SOURCE_MODIFIED_AT` (metadata mixin →
  all entities/versions), `LIST_PRICE` + `CITY` (property data mixin).
- Regenerate ent + gqlgen; resolvers gain `where`/`orderBy` args; the geo
  queries accept `where: PropertyWhereInput` too.

## Phase 4 — Edge visibility
- `mlg_can_view=true` query interceptors for the 8 entity types, attached
  to the client view used by the GraphQL handler — fixes
  `Property.rooms/unitTypes/openHouses`, `Office.mainOffice/branches`,
  child→`property` traversals without N+1 resolvers.
- Version types and SourceSystem stay uninterceptored (audit semantics).
- Flip the pinned `TestPropertyChildEdges_TombstonedStillVisible`.

## Phase 5 — Nearest-first search
- `propertiesNearest(center, limit ≤ 100, where?) → [PropertyDistanceResult!]!`
  (`{ property, distanceMeters }`), ordered by `ST_Distance` in SQL.
- Non-paginated by design (distance order doesn't compose with relay
  cursors); the paginated radius filter remains
  `properties(geo: { withinRadius })`.

## Phase 6 — Observability
- `GET /metrics` (Prometheus): GraphQL request count/duration/errors.
- Request-logging middleware on `serve`.
- `syncStatus: [SyncStatus!]!` GraphQL query — latest sync event per
  resource (status, run type, timestamps, processor version).

## Phase 7 — Retention
- `mls-cli prune --raw-output-older-than <dur> [--dry-run]` — batched
  deletes of old `raw_output` rows. Version tables are intentionally NOT
  pruned (they are the audit trail).

## Phase 8 — Docs + audit
- `docs/graphql-api.md`: auth, CORS, limits, filtering, ordering,
  `propertiesNearest`, `syncStatus`.
- README: new commands, env vars, metrics, multi-arch note.
- Full test suite, live smoke against dev data, code-review audit of the
  entire diff.

## Explicitly deferred (future work)
- GraphQL subscriptions / webhook change notifications
- Versioned migrations (Atlas) instead of auto-migrate
- Kubernetes manifests / Helm chart, release automation
- Multi-MLS (multiple originating systems) in one deployment
- API-side distance ordering with cursor pagination
