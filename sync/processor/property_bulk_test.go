package processor

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/property"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/propertyversion"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
)

// runPropertyChunk processes raws as a single commit-chunk, via the bulk
// ProcessChunk path or the per-record Process loop, committing once. Returns the
// per-record outcomes. Both paths run on one tx, mirroring tryCommitChunk.
func runPropertyChunk(t *testing.T, client *ent.Client, ctx context.Context, raws []*ent.RawOutput, bulk bool) []Outcome {
	t.Helper()
	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	p := NewPropertyProcessor()

	var outcomes []Outcome
	if bulk {
		outcomes, err = p.ProcessChunk(ctx, tx, raws)
	} else {
		for _, raw := range raws {
			var oc Outcome
			oc, err = p.Process(ctx, tx, raw)
			if err != nil {
				break
			}
			outcomes = append(outcomes, oc)
		}
	}
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("process chunk (bulk=%v): %v", bulk, err)
	}
	require.NoError(t, tx.Commit())
	return outcomes
}

// --- focused bulk-path tests ---

func TestBulk_FreshInserts(t *testing.T) {
	ctx, client, evID := setupPropertyTest(t)
	ts := time.Now().UTC().Truncate(time.Second)

	raws := []*ent.RawOutput{
		insertRaw(t, client, ctx, evID, propPayload("LK-1", ts, "500000.00"), ts),
		insertRaw(t, client, ctx, evID, propPayload("LK-2", ts, "600000.00"), ts),
		insertRaw(t, client, ctx, evID, propPayload("LK-3", ts, "700000.00"), ts),
	}

	outcomes := runPropertyChunk(t, client, ctx, raws, true)
	assert.Equal(t, []Outcome{OutcomeInsert, OutcomeInsert, OutcomeInsert}, outcomes)

	for _, k := range []string{"LK-1", "LK-2", "LK-3"} {
		ent_, err := client.Property.Query().Where(property.IDEQ(k)).Only(ctx)
		require.NoError(t, err, "entity %s", k)
		require.NotNil(t, ent_.CurrentVersionID, "%s current_version_id", k)
		assert.True(t, ent_.MlgCanView)
		vs := client.PropertyVersion.Query().Where(propertyversion.ListingKey(k)).AllX(ctx)
		require.Len(t, vs, 1)
		assert.Equal(t, propertyversion.ChangeTypeInsert, vs[0].ChangeType)
		assert.Nil(t, vs[0].ValidTo, "insert version is open")
		assert.Equal(t, ent_.CurrentVersionID.String(), vs[0].ID, "entity points at its version")
	}
}

func TestBulk_UpdateClosesPriorVersion_PreservesCreatedAt(t *testing.T) {
	ctx, client, evID := setupPropertyTest(t)

	ts1 := time.Now().UTC().Truncate(time.Second)
	first := insertRaw(t, client, ctx, evID, propPayload("LK-1", ts1, "500000.00"), ts1)
	// Seed the insert via the per-record path so the bulk path is exercised only
	// for the update.
	require.NoError(t, runPropertyProcess(t, client, ctx, first))

	before := client.Property.Query().Where(property.IDEQ("LK-1")).OnlyX(ctx)
	createdAt := before.CreatedAt

	time.Sleep(5 * time.Millisecond)
	ts2 := ts1.Add(time.Hour)
	second := insertRaw(t, client, ctx, evID, propPayload("LK-1", ts2, "475000.00"), ts2)
	outcomes := runPropertyChunk(t, client, ctx, []*ent.RawOutput{second}, true)
	assert.Equal(t, []Outcome{OutcomeUpdate}, outcomes)

	vs := client.PropertyVersion.Query().
		Where(propertyversion.ListingKey("LK-1")).
		Order(ent.Asc(propertyversion.FieldValidFrom)).
		AllX(ctx)
	require.Len(t, vs, 2)
	require.NotNil(t, vs[0].ValidTo, "prior version closed")
	assert.Nil(t, vs[1].ValidTo, "new version open")
	assert.Equal(t, propertyversion.ChangeTypeUpdate, vs[1].ChangeType)
	require.Contains(t, vs[1].ChangedFields, "list_price")

	after := client.Property.Query().Where(property.IDEQ("LK-1")).OnlyX(ctx)
	assert.Equal(t, "475000", after.ListPrice.String())
	assert.Equal(t, after.CurrentVersionID.String(), vs[1].ID)
	assert.True(t, after.CreatedAt.Equal(createdAt), "created_at must be preserved across a bulk upsert: was %v now %v", createdAt, after.CreatedAt)
	assert.False(t, after.ModifiedAt.Before(before.ModifiedAt), "modified_at must advance")
}

func TestBulk_SkipsWriteNothing(t *testing.T) {
	ctx, client, evID := setupPropertyTest(t)

	ts1 := time.Now().UTC().Truncate(time.Second)
	// Seed two distinct keys via per-record.
	require.NoError(t, runPropertyProcess(t, client, ctx,
		insertRaw(t, client, ctx, evID, propPayload("LK-1", ts1, "500000.00"), ts1)))
	require.NoError(t, runPropertyProcess(t, client, ctx,
		insertRaw(t, client, ctx, evID, propPayload("LK-2", ts1, "600000.00"), ts1)))

	// Bulk chunk, both keys unique: LK-1 no-diff (newer ts, same data),
	// LK-2 stale (older ts). Both must skip and write nothing.
	ts2 := ts1.Add(time.Hour)
	noDiff := insertRaw(t, client, ctx, evID, propPayload("LK-1", ts2, "500000.00"), ts2)
	ts0 := ts1.Add(-time.Hour)
	stale := insertRaw(t, client, ctx, evID, propPayload("LK-2", ts0, "111111.00"), ts0)

	outcomes := runPropertyChunk(t, client, ctx, []*ent.RawOutput{noDiff, stale}, true)
	assert.Equal(t, []Outcome{OutcomeSkipNoDiff, OutcomeSkipStale}, outcomes)

	assert.Equal(t, 1, client.PropertyVersion.Query().Where(propertyversion.ListingKey("LK-1")).CountX(ctx), "no new version for LK-1")
	assert.Equal(t, 1, client.PropertyVersion.Query().Where(propertyversion.ListingKey("LK-2")).CountX(ctx), "no new version for LK-2")
	assert.Equal(t, "600000", client.Property.Query().Where(property.IDEQ("LK-2")).OnlyX(ctx).ListPrice.String(), "stale must not overwrite")
}

func TestBulk_DeleteExistingAndFirstSighting(t *testing.T) {
	ctx, client, evID := setupPropertyTest(t)

	ts1 := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, runPropertyProcess(t, client, ctx,
		insertRaw(t, client, ctx, evID, propPayload("LK-1", ts1, "500000.00"), ts1)))

	ts2 := ts1.Add(time.Hour)
	delExisting := insertRaw(t, client, ctx, evID, propDelete("LK-1", ts2), ts2)
	delFirst := insertRaw(t, client, ctx, evID, propDelete("LK-NEW", ts2), ts2)

	outcomes := runPropertyChunk(t, client, ctx, []*ent.RawOutput{delExisting, delFirst}, true)
	assert.Equal(t, []Outcome{OutcomeDelete, OutcomeDelete}, outcomes)

	// Existing: tombstoned, prior list_price preserved, prior version closed.
	e1 := client.Property.Query().Where(property.IDEQ("LK-1")).OnlyX(ctx)
	assert.False(t, e1.MlgCanView, "LK-1 tombstoned")
	require.NotNil(t, e1.ListPrice)
	assert.Equal(t, "500000", e1.ListPrice.String(), "delete preserves prior field values")
	v1 := client.PropertyVersion.Query().Where(propertyversion.ListingKey("LK-1"), propertyversion.ValidToIsNil()).OnlyX(ctx)
	assert.Equal(t, propertyversion.ChangeTypeDelete, v1.ChangeType)
	assert.Equal(t, 2, client.PropertyVersion.Query().Where(propertyversion.ListingKey("LK-1")).CountX(ctx))

	// First sighting: tombstoned entity created.
	eNew := client.Property.Query().Where(property.IDEQ("LK-NEW")).OnlyX(ctx)
	assert.False(t, eNew.MlgCanView)
	vNew := client.PropertyVersion.Query().Where(propertyversion.ListingKey("LK-NEW")).AllX(ctx)
	require.Len(t, vNew, 1)
	assert.Equal(t, propertyversion.ChangeTypeDelete, vNew[0].ChangeType)
}

func TestBulk_TombstoneSkip(t *testing.T) {
	ctx, client, evID := setupPropertyTest(t)

	ts1 := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, runPropertyProcess(t, client, ctx,
		insertRaw(t, client, ctx, evID, propPayload("LK-1", ts1, "500000.00"), ts1)))
	ts2 := ts1.Add(time.Hour)
	require.NoError(t, runPropertyProcess(t, client, ctx,
		insertRaw(t, client, ctx, evID, propDelete("LK-1", ts2), ts2)))

	// A second delete on an already-tombstoned entity → skip-tombstoned, no write.
	ts3 := ts1.Add(2 * time.Hour)
	again := insertRaw(t, client, ctx, evID, propDelete("LK-1", ts3), ts3)
	outcomes := runPropertyChunk(t, client, ctx, []*ent.RawOutput{again}, true)
	assert.Equal(t, []Outcome{OutcomeSkipTombstoned}, outcomes)
	assert.Equal(t, 2, client.PropertyVersion.Query().Where(propertyversion.ListingKey("LK-1")).CountX(ctx), "no new version on repeat delete")
}

// TestBulk_DuplicateKeysInChunk: same listing_key twice in one chunk. Both
// records must apply in raw order (insert then update), chaining versions, and
// the result must equal the per-record path.
func TestBulk_DuplicateKeysInChunk(t *testing.T) {
	ts := time.Now().UTC().Truncate(time.Second)
	spec := []rawSpec{
		{key: "LK-1", ts: ts, price: "500000.00"},                // insert
		{key: "LK-2", ts: ts, price: "600000.00"},                // insert (unique)
		{key: "LK-1", ts: ts.Add(time.Hour), price: "450000.00"}, // update (duplicate key)
	}

	bulk := snapshotAfterChunk(t, spec, true)
	perRecord := snapshotAfterChunk(t, spec, false)
	assert.Equal(t, perRecord.outcomes, bulk.outcomes, "outcomes must match per-record")
	assertSnapshotsEqual(t, perRecord, bulk)

	// LK-1 specifically: two versions, the second open with the lower price.
	require.Len(t, bulk.versions["LK-1"], 2)
	assert.Equal(t, "insert", bulk.versions["LK-1"][0].changeType)
	assert.False(t, bulk.versions["LK-1"][0].open)
	assert.Equal(t, "update", bulk.versions["LK-1"][1].changeType)
	assert.True(t, bulk.versions["LK-1"][1].open)
}

// TestBulk_MatchesPerRecord_AB is the load-bearing equivalence gate: a mixed
// chunk (insert, update, delete, no-diff skip, stale skip, tombstone) produces
// identical structure and outcomes under bulk and per-record.
func TestBulk_MatchesPerRecord_AB(t *testing.T) {
	ts := time.Now().UTC().Truncate(time.Second)
	later := ts.Add(time.Hour)
	older := ts.Add(-time.Hour)
	spec := []rawSpec{
		{key: "INS", ts: ts, price: "100000.00"},     // insert
		{key: "UPD", ts: ts, price: "200000.00"},     // (seeded) insert
		{key: "UPD", ts: later, price: "250000.00"},  // update — but UPD appears twice → dup path
		{key: "NODIFF", ts: ts, price: "300000.00"},  // insert
		{key: "NODIFF2", ts: ts, price: "300000.00"}, // insert; we add a no-diff below
		{key: "DEL", ts: ts, price: "400000.00"},     // insert
		{key: "STALE", ts: ts, price: "500000.00"},   // insert
	}
	// Second pass spec (all unique keys) exercising update/delete/skip in bulk.
	spec2 := []rawSpec{
		{key: "NODIFF", ts: later, price: "300000.00"}, // no-diff skip
		{key: "DEL", ts: later, del: true},             // delete existing
		{key: "STALE", ts: older, price: "999999.00"},  // stale skip
		{key: "NEWDEL", ts: later, del: true},          // delete first-sighting
		{key: "UPD2", ts: ts, price: "700000.00"},      // insert
	}

	run := func(bulk bool) propSnapshot {
		ctx, client, evID := setupPropertyTest(t)
		raws1 := seedRaws(t, client, ctx, evID, spec)
		_ = runPropertyChunk(t, client, ctx, raws1, bulk)
		raws2 := seedRaws(t, client, ctx, evID, spec2)
		oc := runPropertyChunk(t, client, ctx, raws2, bulk)
		snap := takeSnapshot(t, client, ctx)
		snap.outcomes = oc
		return snap
	}

	bulk := run(true)
	perRecord := run(false)
	assert.Equal(t, perRecord.outcomes, bulk.outcomes, "second-pass outcomes must match")
	assertSnapshotsEqual(t, perRecord, bulk)
}

// TestRunPass_BulkFalse_ForcesPerRecord drives the full loop with WithBulk(false)
// and confirms Property still projects correctly through Process.
func TestRunPass_BulkFalse_ForcesPerRecord(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	evID := seedPropertySyncEvent(t, client, ctx)

	ts := time.Now().UTC().Truncate(time.Second)
	for _, k := range []string{"LK-1", "LK-2", "LK-3"} {
		insertRaw(t, client, ctx, evID, propPayload(k, ts, "500000.00"), ts)
	}

	p := New(client, sqlDB, NewPropertyProcessor()).WithBulk(false)
	require.NoError(t, p.RunPass(ctx, rawoutput.ResourceProperty))
	assert.Equal(t, 3, client.Property.Query().CountX(ctx))
	assert.Equal(t, 3, client.PropertyVersion.Query().CountX(ctx))
}

// TestRunPass_BulkPoisonFallback: a poison record mid-chunk makes the bulk path
// fail; the loop replays per-record, halting at the exact raw_output and
// committing the records before it.
func TestRunPass_BulkPoisonFallback(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	evID := seedPropertySyncEvent(t, client, ctx)

	ts := time.Now().UTC().Truncate(time.Second)
	good1 := insertRaw(t, client, ctx, evID, propPayload("LK-1", ts, "500000.00"), ts)
	good2 := insertRaw(t, client, ctx, evID, propPayload("LK-2", ts, "600000.00"), ts)
	// Poison: payload missing ModificationTimestamp → parseProperty fails.
	poison := client.RawOutput.Create().
		SetSyncEventID(evID).
		SetResource(rawoutput.ResourceProperty).
		SetSourceKey("LK-BAD").
		SetChangeType(rawoutput.ChangeTypeInsert).
		SetSourceModifiedAt(ts).
		SetPayload([]byte(`{"ListingKey":"LK-BAD"}`)).
		SaveX(ctx)
	insertRaw(t, client, ctx, evID, propPayload("LK-3", ts, "700000.00"), ts) // after poison

	p := New(client, sqlDB, NewPropertyProcessor()) // bulk default on
	err := p.RunPass(ctx, rawoutput.ResourceProperty)
	require.Error(t, err)
	assert.Contains(t, err.Error(), poison.ID.String(), "error names the poison raw_output")

	// Records before the poison committed; the one after did not.
	assert.True(t, client.Property.Query().Where(property.IDEQ("LK-1")).ExistX(ctx))
	assert.True(t, client.Property.Query().Where(property.IDEQ("LK-2")).ExistX(ctx))
	assert.False(t, client.Property.Query().Where(property.IDEQ("LK-3")).ExistX(ctx), "record after poison not processed")
	_, _ = good1, good2
}

// --- snapshot / equivalence helpers ---

type verSnap struct {
	changeType  string
	open        bool
	changedKeys string // sorted, comma-joined changed_fields keys
}

type propSnapshot struct {
	entities map[string]bool // listing_key -> mlg_can_view
	versions map[string][]verSnap
	outcomes []Outcome
}

func takeSnapshot(t *testing.T, client *ent.Client, ctx context.Context) propSnapshot {
	t.Helper()
	snap := propSnapshot{entities: map[string]bool{}, versions: map[string][]verSnap{}}
	for _, e := range client.Property.Query().AllX(ctx) {
		snap.entities[e.ID] = e.MlgCanView
	}
	vs := client.PropertyVersion.Query().Order(ent.Asc(propertyversion.FieldValidFrom)).AllX(ctx)
	for _, v := range vs {
		keys := make([]string, 0, len(v.ChangedFields))
		for k := range v.ChangedFields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		joined := ""
		for i, k := range keys {
			if i > 0 {
				joined += ","
			}
			joined += k
		}
		snap.versions[v.ListingKey] = append(snap.versions[v.ListingKey], verSnap{
			changeType:  string(v.ChangeType),
			open:        v.ValidTo == nil,
			changedKeys: joined,
		})
	}
	return snap
}

func assertSnapshotsEqual(t *testing.T, want, got propSnapshot) {
	t.Helper()
	assert.Equal(t, want.entities, got.entities, "entity set / mlg_can_view must match")
	assert.Equal(t, want.versions, got.versions, "version chains must match (change_type, open, changed_fields keys)")
}

func snapshotAfterChunk(t *testing.T, spec []rawSpec, bulk bool) propSnapshot {
	t.Helper()
	ctx, client, evID := setupPropertyTest(t)
	raws := seedRaws(t, client, ctx, evID, spec)
	oc := runPropertyChunk(t, client, ctx, raws, bulk)
	snap := takeSnapshot(t, client, ctx)
	snap.outcomes = oc
	return snap
}

// --- payload / seeding helpers ---

type rawSpec struct {
	key   string
	ts    time.Time
	price string
	del   bool
}

func propPayload(key string, ts time.Time, price string) map[string]any {
	return map[string]any{
		"ListingKey":            key,
		"ModificationTimestamp": ts.Format(time.RFC3339),
		"ListPrice":             price,
		"City":                  "Austin",
	}
}

func propDelete(key string, ts time.Time) map[string]any {
	return map[string]any{
		"ListingKey":            key,
		"ModificationTimestamp": ts.Format(time.RFC3339),
		"MlgCanView":            false,
	}
}

func seedRaws(t *testing.T, client *ent.Client, ctx context.Context, evID uuid.UUID, spec []rawSpec) []*ent.RawOutput {
	t.Helper()
	out := make([]*ent.RawOutput, len(spec))
	for i, s := range spec {
		var payload map[string]any
		if s.del {
			payload = propDelete(s.key, s.ts)
		} else {
			payload = propPayload(s.key, s.ts, s.price)
		}
		// Tiny stagger so UUIDv7 raw_output ids stay in spec order.
		time.Sleep(time.Millisecond)
		out[i] = insertRaw(t, client, ctx, evID, payload, s.ts)
	}
	return out
}

func seedPropertySyncEvent(t *testing.T, client *ent.Client, ctx context.Context) uuid.UUID {
	t.Helper()
	src := client.SourceSystem.Create().SetID("test-src").SetSourceSystemName("test").SaveX(ctx)
	return client.SyncEvent.Create().
		SetSourceSystemID(src.ID).
		SetResource("property").
		SetRunType("sync").
		SetProcessorVersion("test").
		SetStartedAt(time.Now()).
		SaveX(ctx).ID
}
