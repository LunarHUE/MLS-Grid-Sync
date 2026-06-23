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
	"github.com/LunarHUE/MLS-Grid-Sync/ent/propertyroom"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/propertyroomversion"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
)

func runPropertyRoomChunk(t *testing.T, client *ent.Client, ctx context.Context, raws []*ent.RawOutput, bulk bool) []Outcome {
	t.Helper()
	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	p := NewPropertyRoomProcessor()
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
		t.Fatalf("property_room chunk (bulk=%v): %v", bulk, err)
	}
	require.NoError(t, tx.Commit())
	return outcomes
}

func roomPayload(key, lk, roomType string) map[string]any {
	m := map[string]any{"RoomKey": key, "ListingKey": lk}
	if roomType != "" {
		m["RoomType"] = roomType
	}
	return m
}

func roomDelete(key, lk string) map[string]any {
	return map[string]any{"RoomKey": key, "ListingKey": lk, "MlgCanView": false}
}

func TestPropertyRoomBulk_FreshInserts_ParentedAndParked(t *testing.T) {
	ctx, client, evID := setupPropertyRoomTest(t)
	seedProperty(t, client, ctx, "LK-1")
	ts := time.Now().UTC().Truncate(time.Second)

	raws := []*ent.RawOutput{
		insertPropertyRoomRaw(t, client, ctx, evID, roomPayload("R-1", "LK-1", "Bedroom"), ts),
		insertPropertyRoomRaw(t, client, ctx, evID, roomPayload("R-2", "LK-ABSENT", "Kitchen"), ts),
	}
	outcomes := runPropertyRoomChunk(t, client, ctx, raws, true)
	assert.Equal(t, []Outcome{OutcomeInsert, OutcomeInsert}, outcomes)

	r1 := client.PropertyRoom.Query().Where(propertyroom.IDEQ("R-1")).OnlyX(ctx)
	require.NotNil(t, r1.ParentListingKey)
	assert.Equal(t, "LK-1", *r1.ParentListingKey)
	assert.Nil(t, client.PropertyRoom.Query().Where(propertyroom.IDEQ("R-2")).OnlyX(ctx).ParentListingKey, "parked")
}

// TestPropertyRoomBulk_UpdateClearsFieldSingletonChunk is the clear-on-nil
// regression: a bulk update that omits RoomType must CLEAR it even when it's the
// only record in the chunk (so the column is absent from the INSERT). The old
// UpdateNewValues path would have wrongly preserved it.
func TestPropertyRoomBulk_UpdateClearsFieldSingletonChunk(t *testing.T) {
	ctx, client, evID := setupPropertyRoomTest(t)
	seedProperty(t, client, ctx, "LK-1")
	ts1 := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, runPropertyRoomProcess(t, client, ctx,
		insertPropertyRoomRaw(t, client, ctx, evID, roomPayload("R-1", "LK-1", "Bedroom"), ts1)))
	require.NotNil(t, client.PropertyRoom.Query().Where(propertyroom.IDEQ("R-1")).OnlyX(ctx).RoomType)

	ts2 := ts1.Add(time.Hour)
	upd := insertPropertyRoomRaw(t, client, ctx, evID, roomPayload("R-1", "LK-1", ""), ts2) // RoomType omitted
	outcomes := runPropertyRoomChunk(t, client, ctx, []*ent.RawOutput{upd}, true)
	assert.Equal(t, []Outcome{OutcomeUpdate}, outcomes)
	assert.Nil(t, client.PropertyRoom.Query().Where(propertyroom.IDEQ("R-1")).OnlyX(ctx).RoomType,
		"bulk update must clear an omitted field even as the sole chunk record")
}

func TestPropertyRoomBulk_PromotesParkedFKOnUpdate(t *testing.T) {
	ctx, client, evID := setupPropertyRoomTest(t)
	ts1 := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, runPropertyRoomProcess(t, client, ctx,
		insertPropertyRoomRaw(t, client, ctx, evID, roomPayload("R-1", "LK-LATE", "Bedroom"), ts1)))
	assert.Nil(t, client.PropertyRoom.Query().Where(propertyroom.IDEQ("R-1")).OnlyX(ctx).ParentListingKey)

	seedProperty(t, client, ctx, "LK-LATE")
	ts2 := ts1.Add(time.Hour)
	upd := insertPropertyRoomRaw(t, client, ctx, evID, roomPayload("R-1", "LK-LATE", "Office"), ts2)
	runPropertyRoomChunk(t, client, ctx, []*ent.RawOutput{upd}, true)
	r := client.PropertyRoom.Query().Where(propertyroom.IDEQ("R-1")).OnlyX(ctx)
	require.NotNil(t, r.ParentListingKey, "parked FK promoted on update once parent exists")
	assert.Equal(t, "LK-LATE", *r.ParentListingKey)
}

func TestPropertyRoomBulk_MatchesPerRecord_AB(t *testing.T) {
	run := func(bulk bool) roomSnap {
		ctx, client, evID := setupPropertyRoomTest(t)
		seedProperty(t, client, ctx, "LK-1")
		ts := time.Now().UTC().Truncate(time.Second)
		later := ts.Add(time.Hour)
		older := ts.Add(-time.Hour)

		spec1 := []roomSpec{
			{key: "INS", lk: "LK-1", rt: "Bedroom", ts: ts},
			{key: "UPD", lk: "LK-1", rt: "Bedroom", ts: ts},
			{key: "CLR", lk: "LK-1", rt: "Kitchen", ts: ts},
			{key: "NODIFF", lk: "LK-1", rt: "Den", ts: ts},
			{key: "DEL", lk: "LK-1", rt: "Bath", ts: ts},
			{key: "STALE", lk: "LK-1", rt: "Loft", ts: ts},
			{key: "PARK", lk: "LK-LATE", rt: "Attic", ts: ts},
		}
		_ = runPropertyRoomChunk(t, client, ctx, seedRoomRaws(t, client, ctx, evID, spec1), bulk)

		seedProperty(t, client, ctx, "LK-LATE")
		spec2 := []roomSpec{
			{key: "UPD", lk: "LK-1", rt: "Office", ts: later},
			{key: "CLR", lk: "LK-1", rt: "", ts: later}, // clear RoomType
			{key: "NODIFF", lk: "LK-1", rt: "Den", ts: later},
			{key: "DEL", lk: "LK-1", ts: later, del: true},
			{key: "STALE", lk: "LK-1", rt: "X", ts: older},
			{key: "PARK", lk: "LK-LATE", rt: "Cellar", ts: later},
		}
		oc := runPropertyRoomChunk(t, client, ctx, seedRoomRaws(t, client, ctx, evID, spec2), bulk)
		snap := takeRoomSnapshot(t, client, ctx)
		snap.outcomes = oc
		return snap
	}
	bulk := run(true)
	perRecord := run(false)
	assert.Equal(t, perRecord.outcomes, bulk.outcomes)
	assert.Equal(t, perRecord.entities, bulk.entities, "room entities (mlg_can_view + parent + room_type) must match")
	assert.Equal(t, perRecord.versions, bulk.versions, "room version chains must match")
}

func TestRunPass_PropertyRoomBulkPoisonFallback(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	src := client.SourceSystem.Create().SetID("test-src").SetSourceSystemName("test").SaveX(ctx)
	evID := client.SyncEvent.Create().SetSourceSystemID(src.ID).SetResource("property_rooms").
		SetRunType("sync").SetProcessorVersion("test").SetStartedAt(time.Now()).SaveX(ctx).ID
	seedProperty(t, client, ctx, "LK-1")

	ts := time.Now().UTC().Truncate(time.Second)
	insertPropertyRoomRaw(t, client, ctx, evID, roomPayload("R-1", "LK-1", "A"), ts)
	insertPropertyRoomRaw(t, client, ctx, evID, roomPayload("R-2", "LK-1", "B"), ts)
	poison := client.RawOutput.Create().
		SetSyncEventID(evID).SetResource(rawoutput.ResourcePropertyRooms).SetSourceKey("R-BAD").
		SetChangeType(rawoutput.ChangeTypeInsert).SetSourceModifiedAt(ts).
		SetPayload([]byte(`{"ListingKey":"LK-1"}`)). // missing RoomKey
		SaveX(ctx)
	insertPropertyRoomRaw(t, client, ctx, evID, roomPayload("R-3", "LK-1", "C"), ts)

	p := New(client, sqlDB, NewPropertyRoomProcessor())
	err := p.RunPass(ctx, rawoutput.ResourcePropertyRooms)
	require.Error(t, err)
	assert.Contains(t, err.Error(), poison.ID.String())
	assert.True(t, client.PropertyRoom.Query().Where(propertyroom.IDEQ("R-1")).ExistX(ctx))
	assert.False(t, client.PropertyRoom.Query().Where(propertyroom.IDEQ("R-3")).ExistX(ctx))
}

// --- snapshot helpers ---

type roomEntitySnap struct {
	mlgCanView bool
	parentLK   string
	roomType   string
}

type roomSnap struct {
	entities map[string]roomEntitySnap
	versions map[string][]verSnap
	outcomes []Outcome
}

func takeRoomSnapshot(t *testing.T, client *ent.Client, ctx context.Context) roomSnap {
	t.Helper()
	snap := roomSnap{entities: map[string]roomEntitySnap{}, versions: map[string][]verSnap{}}
	for _, e := range client.PropertyRoom.Query().AllX(ctx) {
		plk, rt := "", ""
		if e.ParentListingKey != nil {
			plk = *e.ParentListingKey
		}
		if e.RoomType != nil {
			rt = *e.RoomType
		}
		snap.entities[e.ID] = roomEntitySnap{mlgCanView: e.MlgCanView, parentLK: plk, roomType: rt}
	}
	for _, v := range client.PropertyRoomVersion.Query().Order(ent.Asc(propertyroomversion.FieldValidFrom)).AllX(ctx) {
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
		snap.versions[v.RoomKey] = append(snap.versions[v.RoomKey], verSnap{
			changeType:  string(v.ChangeType),
			open:        v.ValidTo == nil,
			changedKeys: joined,
		})
	}
	return snap
}

type roomSpec struct {
	key string
	lk  string
	rt  string
	ts  time.Time
	del bool
}

func seedRoomRaws(t *testing.T, client *ent.Client, ctx context.Context, evID uuid.UUID, spec []roomSpec) []*ent.RawOutput {
	t.Helper()
	out := make([]*ent.RawOutput, len(spec))
	for i, s := range spec {
		var payload map[string]any
		if s.del {
			payload = roomDelete(s.key, s.lk)
		} else {
			payload = roomPayload(s.key, s.lk, s.rt)
		}
		time.Sleep(time.Millisecond)
		out[i] = insertPropertyRoomRaw(t, client, ctx, evID, payload, s.ts)
	}
	return out
}
