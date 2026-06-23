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
	"github.com/LunarHUE/MLS-Grid-Sync/ent/openhouse"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/openhouseversion"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
)

func runOpenHouseChunk(t *testing.T, client *ent.Client, ctx context.Context, raws []*ent.RawOutput, bulk bool) []Outcome {
	t.Helper()
	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	p := NewOpenHouseProcessor()
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
		t.Fatalf("open_house chunk (bulk=%v): %v", bulk, err)
	}
	require.NoError(t, tx.Commit())
	return outcomes
}

func ohPayload(key, listingKey string, ts time.Time, status string) map[string]any {
	return map[string]any{
		"OpenHouseKey":          key,
		"ListingKey":            listingKey,
		"ModificationTimestamp": ts.Format(time.RFC3339),
		"OpenHouseStatus":       status,
	}
}

func ohDelete(key, listingKey string, ts time.Time) map[string]any {
	return map[string]any{
		"OpenHouseKey":          key,
		"ListingKey":            listingKey,
		"ModificationTimestamp": ts.Format(time.RFC3339),
		"MlgCanView":            false,
	}
}

func TestOpenHouseBulk_FreshInserts_ParentedAndParked(t *testing.T) {
	ctx, client, evID := setupOpenHouseTest(t)
	seedProperty(t, client, ctx, "LK-1") // parent present
	ts := time.Now().UTC().Truncate(time.Second)

	raws := []*ent.RawOutput{
		insertOpenHouseRaw(t, client, ctx, evID, ohPayload("OH-1", "LK-1", ts, "Active"), ts),
		insertOpenHouseRaw(t, client, ctx, evID, ohPayload("OH-2", "LK-ABSENT", ts, "Active"), ts),
	}
	outcomes := runOpenHouseChunk(t, client, ctx, raws, true)
	assert.Equal(t, []Outcome{OutcomeInsert, OutcomeInsert}, outcomes)

	oh1 := client.OpenHouse.Query().Where(openhouse.IDEQ("OH-1")).OnlyX(ctx)
	require.NotNil(t, oh1.ParentListingKey, "parent present → linked at insert")
	assert.Equal(t, "LK-1", *oh1.ParentListingKey)

	oh2 := client.OpenHouse.Query().Where(openhouse.IDEQ("OH-2")).OnlyX(ctx)
	assert.Nil(t, oh2.ParentListingKey, "parent absent → parked")
}

// TestOpenHouseBulk_PromotesParkedFKOnUpdate is the key parking guard: a parked
// row (no parent at insert) gets parent_listing_key promoted when the parent
// later exists and the row is bulk-updated — and an already-linked FK is never
// changed/cleared (COALESCE semantics).
func TestOpenHouseBulk_PromotesParkedFKOnUpdate(t *testing.T) {
	ctx, client, evID := setupOpenHouseTest(t)
	ts1 := time.Now().UTC().Truncate(time.Second)

	// Insert parked (parent LK-LATE absent).
	require.NoError(t, runOpenHouseProcess(t, client, ctx,
		insertOpenHouseRaw(t, client, ctx, evID, ohPayload("OH-1", "LK-LATE", ts1, "Active"), ts1)))
	assert.Nil(t, client.OpenHouse.Query().Where(openhouse.IDEQ("OH-1")).OnlyX(ctx).ParentListingKey)

	// Parent arrives; bulk-update the open house (status change) → promote.
	seedProperty(t, client, ctx, "LK-LATE")
	ts2 := ts1.Add(time.Hour)
	upd := insertOpenHouseRaw(t, client, ctx, evID, ohPayload("OH-1", "LK-LATE", ts2, "Ended"), ts2)
	outcomes := runOpenHouseChunk(t, client, ctx, []*ent.RawOutput{upd}, true)
	assert.Equal(t, []Outcome{OutcomeUpdate}, outcomes)

	oh := client.OpenHouse.Query().Where(openhouse.IDEQ("OH-1")).OnlyX(ctx)
	require.NotNil(t, oh.ParentListingKey, "parked FK promoted on update once parent exists")
	assert.Equal(t, "LK-LATE", *oh.ParentListingKey)

	// A further bulk update keeps it linked (never clears).
	ts3 := ts1.Add(2 * time.Hour)
	upd2 := insertOpenHouseRaw(t, client, ctx, evID, ohPayload("OH-1", "LK-LATE", ts3, "Active"), ts3)
	runOpenHouseChunk(t, client, ctx, []*ent.RawOutput{upd2}, true)
	oh = client.OpenHouse.Query().Where(openhouse.IDEQ("OH-1")).OnlyX(ctx)
	require.NotNil(t, oh.ParentListingKey, "FK stays linked across updates")
	assert.Equal(t, "LK-LATE", *oh.ParentListingKey)
}

func TestOpenHouseBulk_DeleteExistingAndFirstSighting(t *testing.T) {
	ctx, client, evID := setupOpenHouseTest(t)
	seedProperty(t, client, ctx, "LK-1")
	ts1 := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, runOpenHouseProcess(t, client, ctx,
		insertOpenHouseRaw(t, client, ctx, evID, ohPayload("OH-1", "LK-1", ts1, "Active"), ts1)))

	ts2 := ts1.Add(time.Hour)
	raws := []*ent.RawOutput{
		insertOpenHouseRaw(t, client, ctx, evID, ohDelete("OH-1", "LK-1", ts2), ts2),
		insertOpenHouseRaw(t, client, ctx, evID, ohDelete("OH-NEW", "LK-1", ts2), ts2),
	}
	outcomes := runOpenHouseChunk(t, client, ctx, raws, true)
	assert.Equal(t, []Outcome{OutcomeDelete, OutcomeDelete}, outcomes)

	e1 := client.OpenHouse.Query().Where(openhouse.IDEQ("OH-1")).OnlyX(ctx)
	assert.False(t, e1.MlgCanView)
	require.NotNil(t, e1.ParentListingKey, "delete preserves the linked FK")
	assert.Equal(t, 2, client.OpenHouseVersion.Query().Where(openhouseversion.OpenHouseKey("OH-1")).CountX(ctx))
	eNew := client.OpenHouse.Query().Where(openhouse.IDEQ("OH-NEW")).OnlyX(ctx)
	assert.False(t, eNew.MlgCanView)
}

func TestOpenHouseBulk_DuplicateKeysInChunk(t *testing.T) {
	bulk := ohSnapshotAfterChunk(t, func() []ohSpec {
		ts := time.Now().UTC().Truncate(time.Second)
		return []ohSpec{
			{key: "OH-1", lk: "LK-1", status: "Active", ts: ts},
			{key: "OH-2", lk: "LK-1", status: "Active", ts: ts},
			{key: "OH-1", lk: "LK-1", status: "Ended", ts: ts.Add(time.Hour)},
		}
	}, true)
	perRecord := ohSnapshotAfterChunk(t, func() []ohSpec {
		ts := time.Now().UTC().Truncate(time.Second)
		return []ohSpec{
			{key: "OH-1", lk: "LK-1", status: "Active", ts: ts},
			{key: "OH-2", lk: "LK-1", status: "Active", ts: ts},
			{key: "OH-1", lk: "LK-1", status: "Ended", ts: ts.Add(time.Hour)},
		}
	}, false)
	assert.Equal(t, perRecord.outcomes, bulk.outcomes)
	assertOHSnapshotsEqual(t, perRecord, bulk)
	require.Len(t, bulk.versions["OH-1"], 2)
	assert.True(t, bulk.versions["OH-1"][1].open)
}

// TestOpenHouseBulk_MatchesPerRecord_AB covers insert/update/delete/skip plus
// parking + promotion, asserting bulk == per-record end state.
func TestOpenHouseBulk_MatchesPerRecord_AB(t *testing.T) {
	run := func(bulk bool) ohSnap {
		ctx, client, evID := setupOpenHouseTest(t)
		seedProperty(t, client, ctx, "LK-1") // present for INS/UPD/DEL
		ts := time.Now().UTC().Truncate(time.Second)
		later := ts.Add(time.Hour)
		older := ts.Add(-time.Hour)

		spec1 := []ohSpec{
			{key: "INS", lk: "LK-1", status: "Active", ts: ts},
			{key: "UPD", lk: "LK-1", status: "Active", ts: ts},
			{key: "NODIFF", lk: "LK-1", status: "Active", ts: ts},
			{key: "DEL", lk: "LK-1", status: "Active", ts: ts},
			{key: "STALE", lk: "LK-1", status: "Active", ts: ts},
			{key: "PARK", lk: "LK-LATE", status: "Active", ts: ts}, // parent absent → parked
		}
		_ = runOpenHouseChunk(t, client, ctx, seedOHRaws(t, client, ctx, evID, spec1), bulk)

		seedProperty(t, client, ctx, "LK-LATE") // now PARK can promote
		spec2 := []ohSpec{
			{key: "UPD", lk: "LK-1", status: "Ended", ts: later},
			{key: "NODIFF", lk: "LK-1", status: "Active", ts: later},
			{key: "DEL", lk: "LK-1", ts: later, del: true},
			{key: "STALE", lk: "LK-1", status: "X", ts: older},
			{key: "PARK", lk: "LK-LATE", status: "Ended", ts: later}, // update → promote FK
			{key: "NEWDEL", lk: "LK-1", ts: later, del: true},
		}
		oc := runOpenHouseChunk(t, client, ctx, seedOHRaws(t, client, ctx, evID, spec2), bulk)
		snap := takeOHSnapshot(t, client, ctx)
		snap.outcomes = oc
		return snap
	}
	bulk := run(true)
	perRecord := run(false)
	assert.Equal(t, perRecord.outcomes, bulk.outcomes)
	assertOHSnapshotsEqual(t, perRecord, bulk)
}

func TestRunPass_OpenHouseBulkPoisonFallback(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	src := client.SourceSystem.Create().SetID("test-src").SetSourceSystemName("test").SaveX(ctx)
	evID := client.SyncEvent.Create().SetSourceSystemID(src.ID).SetResource("open_house").
		SetRunType("sync").SetProcessorVersion("test").SetStartedAt(time.Now()).SaveX(ctx).ID
	seedProperty(t, client, ctx, "LK-1")

	ts := time.Now().UTC().Truncate(time.Second)
	insertOpenHouseRaw(t, client, ctx, evID, ohPayload("OH-1", "LK-1", ts, "Active"), ts)
	insertOpenHouseRaw(t, client, ctx, evID, ohPayload("OH-2", "LK-1", ts, "Active"), ts)
	poison := client.RawOutput.Create().
		SetSyncEventID(evID).SetResource(rawoutput.ResourceOpenHouse).SetSourceKey("OH-BAD").
		SetChangeType(rawoutput.ChangeTypeInsert).SetSourceModifiedAt(ts).
		SetPayload([]byte(`{"ListingKey":"LK-1"}`)). // valid JSON, missing OpenHouseKey
		SaveX(ctx)
	insertOpenHouseRaw(t, client, ctx, evID, ohPayload("OH-3", "LK-1", ts, "Active"), ts)

	p := New(client, sqlDB, NewOpenHouseProcessor())
	err := p.RunPass(ctx, rawoutput.ResourceOpenHouse)
	require.Error(t, err)
	assert.Contains(t, err.Error(), poison.ID.String())
	assert.True(t, client.OpenHouse.Query().Where(openhouse.IDEQ("OH-1")).ExistX(ctx))
	assert.True(t, client.OpenHouse.Query().Where(openhouse.IDEQ("OH-2")).ExistX(ctx))
	assert.False(t, client.OpenHouse.Query().Where(openhouse.IDEQ("OH-3")).ExistX(ctx))
}

// --- snapshot helpers (capture parent_listing_key) ---

type ohEntitySnap struct {
	mlgCanView bool
	parentLK   string // "" == nil
}

type ohSnap struct {
	entities map[string]ohEntitySnap
	versions map[string][]verSnap
	outcomes []Outcome
}

func takeOHSnapshot(t *testing.T, client *ent.Client, ctx context.Context) ohSnap {
	t.Helper()
	snap := ohSnap{entities: map[string]ohEntitySnap{}, versions: map[string][]verSnap{}}
	for _, e := range client.OpenHouse.Query().AllX(ctx) {
		plk := ""
		if e.ParentListingKey != nil {
			plk = *e.ParentListingKey
		}
		snap.entities[e.ID] = ohEntitySnap{mlgCanView: e.MlgCanView, parentLK: plk}
	}
	for _, v := range client.OpenHouseVersion.Query().Order(ent.Asc(openhouseversion.FieldValidFrom)).AllX(ctx) {
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
		snap.versions[v.OpenHouseKey] = append(snap.versions[v.OpenHouseKey], verSnap{
			changeType:  string(v.ChangeType),
			open:        v.ValidTo == nil,
			changedKeys: joined,
		})
	}
	return snap
}

func assertOHSnapshotsEqual(t *testing.T, want, got ohSnap) {
	t.Helper()
	assert.Equal(t, want.entities, got.entities, "open_house entities (mlg_can_view + parent_listing_key) must match")
	assert.Equal(t, want.versions, got.versions, "open_house version chains must match")
}

func ohSnapshotAfterChunk(t *testing.T, mkSpec func() []ohSpec, bulk bool) ohSnap {
	t.Helper()
	ctx, client, evID := setupOpenHouseTest(t)
	seedProperty(t, client, ctx, "LK-1")
	oc := runOpenHouseChunk(t, client, ctx, seedOHRaws(t, client, ctx, evID, mkSpec()), bulk)
	snap := takeOHSnapshot(t, client, ctx)
	snap.outcomes = oc
	return snap
}

type ohSpec struct {
	key    string
	lk     string
	status string
	ts     time.Time
	del    bool
}

func seedOHRaws(t *testing.T, client *ent.Client, ctx context.Context, evID uuid.UUID, spec []ohSpec) []*ent.RawOutput {
	t.Helper()
	out := make([]*ent.RawOutput, len(spec))
	for i, s := range spec {
		var payload map[string]any
		if s.del {
			payload = ohDelete(s.key, s.lk, s.ts)
		} else {
			payload = ohPayload(s.key, s.lk, s.ts, s.status)
		}
		time.Sleep(time.Millisecond)
		out[i] = insertOpenHouseRaw(t, client, ctx, evID, payload, s.ts)
	}
	return out
}
