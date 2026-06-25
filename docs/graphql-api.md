# GraphQL API Reference

A **read-only**, Relay-style GraphQL API over the synced MLS Grid data.
There are no mutations and no subscriptions — the sync/worker pipeline is
the only writer.

## Endpoints

Started with `mls-cli serve` (the Docker image's default command):

| Path | Method | Purpose |
|------|--------|---------|
| `/` | GET | GraphQL Playground UI |
| `/query` | POST, GET | The GraphQL API |
| `/healthz` | GET | Liveness probe — pings the database; `200 ok` or `503` |

Listen address defaults to `:8080`; override with `MLS_SYNC_SERVER_ADDR`
or `--addr`.

**Authentication: none.** The API is read-only, but it serves the entire
licensed dataset — deploy it behind your own gateway/auth layer; do not
expose it directly to the public internet.

## Making requests

The endpoint speaks standard GraphQL-over-HTTP. **POST** is the normal
transport:

```bash
curl -s -X POST localhost:8080/query \
  -H 'Content-Type: application/json' \
  -d '{
    "query": "query Homes($n: Int) { properties(first: $n) { totalCount } }",
    "variables": { "n": 5 },
    "operationName": "Homes"
  }'
```

The request body has three fields: `query` (required — the GraphQL
document), `variables` (optional JSON object bound to `$`-prefixed
variables), and `operationName` (required only when the document
contains more than one named operation).

**GET** also works, with the document in the query string — handy for
quick checks and cacheable reads:

```bash
curl -s --get localhost:8080/query \
  --data-urlencode 'query={ properties(first: 1) { totalCount } }'
```

Every response is a JSON envelope with up to two keys:

```json
{ "data": { ... }, "errors": [ { "message": "...", "path": [...], "locations": [...] } ] }
```

### HTTP status codes

The status code tells you *which stage* failed (all verified against the
running server and pinned by `graph/protocol_test.go`):

| Status | Meaning | `errors[].extensions.code` |
|--------|---------|----------------------------|
| `200` | Executed. **This includes resolver failures** — always check `errors`, not just the status. | — |
| `400` | Request body was not decodable JSON. | — |
| `422` | Body was fine but the GraphQL document wasn't: syntax errors or queries that don't match the schema (unknown fields, wrong arg types). | `GRAPHQL_PARSE_FAILED` / `GRAPHQL_VALIDATION_FAILED` |

A `200` with `errors` set and `data: null` (or a null field inside
`data`) is the GraphQL way of reporting runtime failures — e.g. geo
input validation:

```json
{"errors":[{"message":"center.latitude 99 out of range [-90, 90]","path":["propertiesNear"]}],"data":null}
```

### Execution model

Each operation executes inside a single read-only database transaction
(entgql's Transactioner), so every field in one request sees a
consistent snapshot. gqlgen also performs **field collection**: only the
columns your query selects are fetched from Postgres, so narrow queries
are genuinely cheaper — ask for what you need.

## Root queries

Every entity exposes a Relay connection list; entities with version
history also expose an audit list:

| Entities | Audit (versions) |
|---|---|
| `lookups`, `mediaSlice`¹, `members`, `offices`, `openHouses`, `properties`, `propertyRooms`, `propertyUnitTypes`, `sourceSystems` | `mediaVersions`, `memberVersions`, `officeVersions`, `openHouseVersions`, `propertyVersions`, `propertyRoomVersions`, `propertyUnitTypeVersions` |

Plus `node(id: ID!)` and `nodes(ids: [ID!]!)` for direct fetch, three
**geo-search** queries over properties: `propertiesNear`,
`propertiesInBBox`, and `propertiesInMultiPolygon`
(see [Geo search](#geo-search)), and two **address-search** queries:
`propertiesByAddress` and `propertiesByAddressFields` (see
[Address search](#address-search)).

¹ The Media list is named `mediaSlice` because `Property.media` already
exists as a field name.

Every list takes the same four pagination arguments (`first`/`after`,
`last`/`before`) and nothing else — see [Limitations](#limitations).

## Entity types

Field names follow the [RESO Data Dictionary](https://www.reso.org/data-dictionary/)
(camelCased), so RESO documentation doubles as field documentation.
Non-standard, MLS-specific fields land in `Property.extendedFields`
(a JSON object).

| Type | What it is | Fields to know |
|------|-----------|----------------|
| `Property` | A listing — the big one (~150 fields) | `listPrice`, `standardStatus`, address block (`unparsedAddress`, `city`, `postalCode`, …), `latitude`/`longitude`, soft keys (`listAgentKey`→`listAgent`, …), `media`, `rooms`, `unitTypes`, `openHouses`, `extendedFields`, `currentVersionID` |
| `Member` | An agent | `memberFirstName`/`memberLastName`, `memberMlsID`, `officeKey`→`office` |
| `Office` | A brokerage office | `officeName`, `mainOfficeKey`→`mainOffice`, `branches` |
| `Media` | A photo/attachment | `mediaURL`, `resourceType` (`property`/`member`/`office`), `resourceRecordKey` (which record it belongs to), `order` |
| `OpenHouse` | A scheduled open house | `listingKey`, `openHouseStartTime`/`EndTime`, `openHouseStatus`, `property` |
| `PropertyRoom` | A room detail row for a listing | `listingKey`, `roomType`, dimensions, `property` |
| `PropertyUnitType` | A unit-mix row (multi-family listings) | `listingKey`, `unitTypeBedsTotal`, `property` |
| `Lookup` | Enumeration metadata from the feed | `lookupName` (the enum), `lookupValue` (one allowed value) |
| `SourceSystem` | A feed/MLS the data came from | `sourceSystemName` |

Each of the first seven also has a `<Type>Version` audit twin — see
[Version history](#version-history-audit-trail).

All entity types implement `Node` (fetchable by `node(id:)`) and carry
the sync metadata trio: `sourceModifiedAt` (upstream timestamp),
`mlgCanView` (visibility flag), `modifiedAt`/`createdAt` (local sync
timestamps).

## Version history (audit trail)

Every change the sync pipeline applies writes an immutable row to the
matching `*Version` table — the `*Versions` root queries expose that
history. A version row carries:

| Field | Meaning |
|-------|---------|
| `changeType` | `insert`, `update`, or `delete` |
| `validFrom` | When this version became the current state |
| `changedFields` | JSON object of the fields that differ from the prior version |
| `sourceModifiedAt` | The upstream MLS timestamp that produced it |
| `processorVersion` | Which parser version wrote it (for replay/repro) |
| `syncEventID` | UUID of the ingestion event — correlate rows written by the same sync run |
| `listingKey` / `memberKey` / … | The entity it describes |

Version rows are identified by UUIDv7 (time-ordered), so a version
list paginated in ID order is approximately chronological.

Recipes:
- **Latest version of a listing**: `Property.currentVersionID` → `node(id:)`
  with `... on PropertyVersion`.
- **Deletion history**: tombstoned entities disappear from entity lists,
  but their `changeType: delete` version rows remain in `propertyVersions`
  (see [Visibility semantics](#visibility-semantics)).
- **History of one listing**: there is no server-side `listingKey` filter
  yet — paginate `propertyVersions` and filter client-side, or query the
  database directly for heavy audit work.

## Scalar wire formats

| GraphQL type | JSON wire format | Example |
|---|---|---|
| `Decimal` | **string** (precision-safe) | `"450000.5"` |
| `UUID` | string | `"019eba4a-…"` |
| `StringArray` | array of strings, or `null` when unset (never `[]`) | `["Dishwasher","Microwave"]` |
| `Map` | JSON object | `{"ACT_Foo":"bar"}` |
| `Time` | RFC3339 string | `"2026-06-12T05:00:00Z"` |
| `Int` (int16-backed fields, e.g. `taxYear`) | number | `2024` |

Parse `Decimal` fields with a decimal library, not `parseFloat` — they are
strings precisely so no precision is lost.

## IDs, `node`, and `nodes`

Entity IDs are **raw MLS Grid keys** (e.g. `"ACT123456"`) with no type
prefix. Version-row IDs are UUIDv7 strings. Because IDs carry no type
information, `node(id:)` probes the tables in a fixed order and returns
the first match:

```
lookup, media, media_version, member, member_version, office,
office_version, open_house, open_house_version, property,
property_room, property_room_version, property_unit_type,
property_unit_type_version, property_version, source_system
```

Consequences:
- An ID present in two tables resolves to the earlier table (e.g. a key
  existing as both Member and Property resolves to the **Member**). MLS
  Grid keys are namespaced upstream, so this is theoretical.
- A miss returns `null`, never an error.
- `nodes(ids:)` returns one slot per input ID, in order, with `null` for
  misses.

Use an inline fragment to select fields: `node(id: $id) { ... on Property { listPrice } }`.

## Visibility semantics

MLS Grid soft-deletes records by setting `MlgCanView=false` ("tombstoned").
The API handles this consistently:

- **Entity lists and `node()`/`nodes()` hide tombstoned rows.** A
  tombstoned Property is absent from `properties` and `node()` returns
  `null` for its ID.
- **`*Versions` audit lists include tombstoned rows by design.** A
  `changeType: delete` version row with `mlgCanView: false` *is* the
  historical record of the deletion — filtering it would blind audit
  consumers. Version rows are also always fetchable via `node()`.
- **`sourceSystems` has no visibility flag** (source systems aren't MLS
  records).
- **Known gap:** nested ent-edge traversals — `Property.rooms`,
  `Property.unitTypes`, `Property.openHouses`, `Office.mainOffice`,
  `Office.branches`, and the child→`property` edges — do **not** yet
  filter visibility. A tombstoned room still appears under its parent
  property's `rooms`. (Pinned by `TestPropertyChildEdges_TombstonedStillVisible`;
  closing the gap means flipping that test deliberately.)

## Soft keys

Agent/office references are **soft keys** — string columns with no DB
foreign key, because MLS feeds routinely reference records outside the
feed (retired agents, out-of-subscription offices).

- `Property`: `listAgent`, `coListAgent`, `buyerAgent`, `coBuyerAgent`,
  `listOffice`, `coListOffice`, `buyerOffice`, `coBuyerOffice` (and the
  matching `*Key` / `*MlsID` string fields).
- `Member`: `office` (via `officeKey`).
- `Property.media` is polymorphic: Media rows with
  `resourceType=property` and `resourceRecordKey=<listing key>`.

Resolution rules: the `*Key` string is **always served**. The resolved
entity is `null` when the target is absent from the feed ("orphan") **or**
tombstoned. So `listAgentKey: "ABC" , listAgent: null` is a normal,
expected state.

## Geo search

Three property queries filter by location, backed by PostGIS (a
`geography(Point,4326)` column generated from each property's
`latitude`/`longitude`, GIST-indexed). All take a `GeoPoint` input
(`{ latitude: Float!, longitude: Float! }`, WGS84), return a standard
`PropertyConnection`, apply the usual visibility filter, and skip
properties without coordinates. Results are ID-ordered, not
distance-ordered.

**`propertiesNear(center, radiusMeters)`** — everything within a true
spheroid distance of a point:

```graphql
{
  propertiesNear(center: { latitude: 30.2672, longitude: -97.7431 },
                 radiusMeters: 5000, first: 25) {
    totalCount
    edges { node { id unparsedAddress listPrice latitude longitude } }
  }
}
```

**`propertiesInBBox(bounds)`** — a map viewport. `bounds` is a `Bounds`
input (`{ southWest, northEast }`); `southWest` must be south and west of
`northEast`; boxes crossing the antimeridian aren't supported.

```graphql
{
  propertiesInBBox(bounds: { southWest: { latitude: 30.25, longitude: -97.76 },
                             northEast: { latitude: 30.29, longitude: -97.72 } },
                   first: 25) {
    totalCount
    edges { node { id latitude longitude } }
  }
}
```

**`propertiesInMultiPolygon(polygons)`** — one or more shapes drawn on a
map, including several discontiguous ones in a single query (e.g. a handful
of separate neighborhoods). `polygons` is a list of rings (`[[GeoPoint!]!]!`);
each ring has ≥ 3 vertices, closes automatically (repeating the first vertex
also works), uses planar lat/lng edges, and is boundary inclusive. A property
matches if it falls inside **any** one of the polygons — under the hood it's a
single `ST_Covers` against a PostGIS `MULTIPOLYGON`, so there's no extra
round-trip per shape. At most 64 polygons and 4096 total vertices across all
of them. For a single shape, pass a one-element list.

```graphql
{
  propertiesInMultiPolygon(polygons: [
    [ { latitude: 30.257, longitude: -97.750 },
      { latitude: 30.257, longitude: -97.736 },
      { latitude: 30.279, longitude: -97.743 } ],
    [ { latitude: 30.500, longitude: -97.700 },
      { latitude: 30.500, longitude: -97.680 },
      { latitude: 30.520, longitude: -97.690 } ]
  ], first: 25) {
    totalCount
    edges { node { id unparsedAddress } }
  }
}
```

Validation errors (radius ≤ 0, coordinates out of range, inverted bbox,
fewer than 3 or more than 1024 polygon vertices, an empty `polygons` list,
a ring with fewer than 3 vertices, or more than 64 polygons / 4096 total
multipolygon vertices) come back as GraphQL errors.

**Infrastructure note:** these queries require a PostGIS-enabled
Postgres (the compose file and tests use `imresamu/postgis:15-3.5-alpine`).
The extension, generated `geom` column, and GIST indexes are applied by
the migration-owning commands (`sync`, `init`, `worker`, …) — run one of
them once before `serve` against a fresh or pre-PostGIS database.

## Address search

Two property queries match on the address text, backed by PostgreSQL
trigram similarity (the `pg_trgm` extension, GIN-indexed). Both return a
standard `PropertyConnection`, apply the usual visibility filter, and are
**ID-ordered, not relevance-ordered** — trigram search _filters_ rows, it
does not rank them, so cursor pagination stays stable. To narrow results,
raise the `threshold`; there is no "best match first."

### `propertiesByAddress(query, threshold)` — single search box

`query` is matched with trigram `word_similarity` against a **combined
address** — street number, street name, city, state, and postal code
concatenated — so typos and partial input still match. `threshold` is the
`word_similarity` cutoff in `[0, 1]` (default **0.3**); higher is stricter.

```graphql
{
  propertiesByAddress(query: "123 Mian St Austn", first: 25) {
    totalCount
    edges { node { id unparsedAddress city postalCode } }
  }
}
```

Because it matches the *combined* field, a broad query matches on any
strongly-similar token: `query: "Austin"` returns every Austin listing (the
city token alone clears the threshold). Send a fuller string, or use the
structured query below, to discriminate.

**ZIP fast-path:** an all-digit query (a ZIP or ZIP+4 like `"78704"` or
`"78704-1234"`) skips trigram entirely and does an exact / prefix
`postalCode` lookup — bare digits share no useful trigrams with an address.
A 5-digit value matches exactly; anything shorter is treated as a prefix
(`"787"` → `787xx`).

### `propertiesByAddressFields(street, city, state, zip, threshold)` — structured

Every field is optional and the supplied ones are **AND-combined** (an
advanced-search form). At least one of `street`/`city`/`state`/`zip` must be
provided.

| Field | Match | Notes |
|---|---|---|
| `street` | fuzzy (`word_similarity` on `streetName`) | governed by `threshold` |
| `city` | fuzzy (`word_similarity` on `city`) | governed by `threshold` |
| `state` | exact, case-insensitive | `"tx"` = `"TX"` |
| `zip` | exact (5 digits) or prefix | `"787"` → `787xx` |

```graphql
{
  propertiesByAddressFields(city: "Austn", zip: "787", first: 25) {
    totalCount
    edges { node { id unparsedAddress city postalCode } }
  }
}
```

Validation errors (an empty `query`, a `threshold` outside `[0, 1]`, or no
field set on `propertiesByAddressFields`) come back as GraphQL errors.

**Infrastructure note:** like geo search, these require the `pg_trgm`
extension and its GIN indexes, applied by the same migration-owning
commands (`sync`, `init`, `worker`, …). Run one once before `serve` against
a fresh database.

## Pagination

Every list is a Relay *connection* with the same anatomy:

```text
properties(first: 25, after: "<cursor>")   ← page size + position
└── PropertyConnection
    ├── totalCount        Int      total matching rows (after visibility/geo filters)
    ├── pageInfo
    │   ├── hasNextPage     Boolean   more rows after this page?
    │   ├── hasPreviousPage Boolean   more rows before it?
    │   ├── startCursor     Cursor    cursor of the first edge
    │   └── endCursor       Cursor    cursor of the last edge
    └── edges []
        ├── cursor         Cursor    position of this row (use as after/before)
        └── node           Property  the actual record
```

```graphql
{
  properties(first: 25) {
    totalCount
    pageInfo { hasNextPage endCursor }
    edges { cursor node { id listPrice } }
  }
}
```

Rules:
- Forward: `first` + `after`; backward: `last` + `before`.
- Cursors are opaque strings — never construct or parse them; they come
  from `edges[].cursor` / `pageInfo`.
- `totalCount` is the filtered total (visible rows only), constant
  across pages of the same query.
- `first: 0` is legal — returns no edges but still reports `totalCount`
  (cheap count query).
- Supplying both `first` and `last`, or a malformed cursor, returns a
  GraphQL error.
- Rows are ID-ordered; new rows arriving mid-pagination won't shift
  pages you've already read (cursors are positional on ID, not offsets).

**Walking all pages** — loop until `hasNextPage` is false:

```bash
CURSOR=null
while :; do
  RESP=$(curl -s -X POST localhost:8080/query -H 'Content-Type: application/json' \
    -d "{\"query\":\"query(\$c: Cursor){ properties(first: 500, after: \$c){ pageInfo{ hasNextPage endCursor } edges{ node{ id } } } }\",\"variables\":{\"c\":$CURSOR}}")
  echo "$RESP" | jq -r '.data.properties.edges[].node.id'
  [ "$(echo "$RESP" | jq '.data.properties.pageInfo.hasNextPage')" = "true" ] || break
  CURSOR=$(echo "$RESP" | jq '.data.properties.pageInfo.endCursor')
done
```

Fetch a page, then follow `pageInfo.endCursor`:

```bash
curl -s -X POST localhost:8080/query -H 'Content-Type: application/json' \
  -d '{"query":"{ properties(first: 2) { totalCount pageInfo { hasNextPage endCursor } edges { node { id listPrice } } } }"}'

curl -s -X POST localhost:8080/query -H 'Content-Type: application/json' \
  -d '{"query":"query($c: Cursor) { properties(first: 2, after: $c) { edges { node { id } } } }","variables":{"c":"<endCursor from above>"}}'
```

## Limitations

- **No general filtering** — the geo-search and address-search queries are
  the only purpose-built server-side filters; the plain entity lists take no
  filter beyond pagination. Filter client-side or query the database
  directly.
- **No ranking** — results come back in primary-key (ID) order; geo results
  are not distance-sorted and address results are not relevance-sorted.
  Narrow address matches with a higher `threshold` instead.
- **No mutations or subscriptions.**
- `nodes(ids:)` resolves each ID independently (N probes); prefer the
  list queries for bulk reads.

## Examples

Fetch one listing with its agent and photos:

```bash
curl -s -X POST localhost:8080/query -H 'Content-Type: application/json' -d '{
  "query": "query($id: ID!) { node(id: $id) { ... on Property { id listPrice listAgentKey listAgent { memberFirstName memberLastName } media { mediaURL } } } }",
  "variables": {"id": "ACT123456"}
}'
```

Audit history for a listing key:

```graphql
{
  propertyVersions(first: 50) {
    edges { node { listingKey changeType validFrom mlgCanView } }
  }
}
```

## Exploring the schema

The schema is fully introspectable:

- **Playground** — open `/` in a browser for an interactive editor with
  autocomplete, inline docs (the soft-key, media, and geo fields carry
  descriptions), and a schema browser.
- **Introspection queries** — standard `__schema`/`__type` queries work:

  ```bash
  curl -s -X POST localhost:8080/query -H 'Content-Type: application/json' \
    -d '{"query":"{ __type(name: \"Property\") { fields { name type { name kind } description } } }"}'
  ```

- **Codegen** — point any GraphQL client generator (`graphql-codegen`,
  Apollo, gql.tada, …) at the endpoint to get typed clients; everything
  they need is served via introspection.

## Performance notes

- **Select only the fields you need.** gqlgen field collection turns the
  selection set into the SQL column list — `{ id listPrice }` reads two
  columns, not 150.
- **Each request runs in one read-only transaction** — multiple root
  fields in one query see a consistent snapshot, at the cost of holding
  the transaction for the whole request. Prefer several small queries
  over one giant multi-root query.
- **`node(id:)` probes up to 16 tables** to type a bare MLS key (one
  indexed primary-key lookup each, plus a visibility check). Fine for
  point lookups; for bulk reads use the list queries instead of many
  `node` calls — `nodes(ids:)` is just a loop over `node`.
- **Geo queries are GIST-indexed** (geography index for radius, geometry
  expression index for bbox/polygon) and stay fast at full-table scale.
- **`totalCount` runs a COUNT per request.** If you don't need it, omit
  it and the count query is skipped.
