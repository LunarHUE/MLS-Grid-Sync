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

## Root queries

Every entity exposes a Relay connection list; entities with version
history also expose an audit list:

| Entities | Audit (versions) |
|---|---|
| `lookups`, `mediaSlice`¹, `members`, `offices`, `openHouses`, `properties`, `propertyRooms`, `propertyUnitTypes`, `sourceSystems` | `mediaVersions`, `memberVersions`, `officeVersions`, `openHouseVersions`, `propertyVersions`, `propertyRoomVersions`, `propertyUnitTypeVersions` |

Plus `node(id: ID!)` and `nodes(ids: [ID!]!)` for direct fetch.

¹ The Media list is named `mediaSlice` because `Property.media` already
exists as a field name.

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

## Pagination

Standard Relay cursors on every connection:

```graphql
{
  properties(first: 25) {
    totalCount
    pageInfo { hasNextPage endCursor }
    edges { cursor node { id listPrice } }
  }
}
```

- Forward: `first` + `after`; backward: `last` + `before`.
- Cursors are opaque strings — never construct or parse them.
- `totalCount` is the filtered total (visible rows only).
- Supplying both `first` and `last`, or a malformed cursor, returns a
  GraphQL error.

Fetch a page, then follow `pageInfo.endCursor`:

```bash
curl -s -X POST localhost:8080/query -H 'Content-Type: application/json' \
  -d '{"query":"{ properties(first: 2) { totalCount pageInfo { hasNextPage endCursor } edges { node { id listPrice } } } }"}'

curl -s -X POST localhost:8080/query -H 'Content-Type: application/json' \
  -d '{"query":"query($c: Cursor) { properties(first: 2, after: $c) { edges { node { id } } } }","variables":{"c":"<endCursor from above>"}}'
```

## Limitations

- **No filtering** — there are no `where` arguments. Pagination is the
  only navigation; filter client-side or query the database directly.
- **No ordering** — results come back in primary-key (ID) order; there is
  no `orderBy`.
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

The schema itself is fully introspectable — open the Playground at `/`
for autocomplete and per-field documentation (the soft-key and media
fields carry inline descriptions).
