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
	"github.com/LunarHUE/MLS-Grid-Sync/ent/propertyunittype"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/propertyunittypeversion"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
)

func runPropertyUnitTypeChunk(t *testing.T, client *ent.Client, ctx context.Context, raws []*ent.RawOutput, bulk bool) []Outcome {
	t.Helper()
	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	p := NewPropertyUnitTypeProcessor()
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
		t.Fatalf("property_unit_type chunk (bulk=%v): %v", bulk, err)
	}
	require.NoError(t, tx.Commit())
	return outcomes
}

func utPayload(key, lk, furnished string) map[string]any {
	m := map[string]any{"UnitTypeKey": key, "ListingKey": lk}
	if furnished != "" {
		m["UnitTypeFurnished"] = furnished
	}
	return m
}

func utDelete(key, lk string) map[string]any {
	return map[string]any{"UnitTypeKey": key, "ListingKey": lk, "MlgCanView": false}
}

func TestPropertyUnitTypeBulk_FreshInserts_ParentedAndParked(t *testing.T) {
	ctx, client, evID := setupPropertyUnitTypeTest(t)
	seedProperty(t, client, ctx, "LK-1")
	ts := time.Now().UTC().Truncate(time.Second)
	raws := []*ent.RawOutput{
		insertPropertyUnitTypeRaw(t, client, ctx, evID, utPayload("UT-1", "LK-1", "Furnished"), ts),
		insertPropertyUnitTypeRaw(t, client, ctx, evID, utPayload("UT-2", "LK-ABSENT", "Unfurnished"), ts),
	}
	outcomes := runPropertyUnitTypeChunk(t, client, ctx, raws, true)
	assert.Equal(t, []Outcome{OutcomeInsert, OutcomeInsert}, outcomes)
	u1 := client.PropertyUnitType.Query().Where(propertyunittype.IDEQ("UT-1")).OnlyX(ctx)
	require.NotNil(t, u1.ParentListingKey)
	assert.Equal(t, "LK-1", *u1.ParentListingKey)
	assert.Nil(t, client.PropertyUnitType.Query().Where(propertyunittype.IDEQ("UT-2")).OnlyX(ctx).ParentListingKey)
}

func TestPropertyUnitTypeBulk_UpdateClearsFieldSingletonChunk(t *testing.T) {
	ctx, client, evID := setupPropertyUnitTypeTest(t)
	seedProperty(t, client, ctx, "LK-1")
	ts1 := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, runPropertyUnitTypeProcess(t, client, ctx,
		insertPropertyUnitTypeRaw(t, client, ctx, evID, utPayload("UT-1", "LK-1", "Furnished"), ts1)))

	ts2 := ts1.Add(time.Hour)
	upd := insertPropertyUnitTypeRaw(t, client, ctx, evID, utPayload("UT-1", "LK-1", ""), ts2)
	outcomes := runPropertyUnitTypeChunk(t, client, ctx, []*ent.RawOutput{upd}, true)
	assert.Equal(t, []Outcome{OutcomeUpdate}, outcomes)
	assert.Nil(t, client.PropertyUnitType.Query().Where(propertyunittype.IDEQ("UT-1")).OnlyX(ctx).UnitTypeFurnished,
		"omitted field cleared by bulk update even as the sole chunk record")
}

func TestPropertyUnitTypeBulk_PromotesParkedFKOnUpdate(t *testing.T) {
	ctx, client, evID := setupPropertyUnitTypeTest(t)
	ts1 := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, runPropertyUnitTypeProcess(t, client, ctx,
		insertPropertyUnitTypeRaw(t, client, ctx, evID, utPayload("UT-1", "LK-LATE", "Furnished"), ts1)))
	assert.Nil(t, client.PropertyUnitType.Query().Where(propertyunittype.IDEQ("UT-1")).OnlyX(ctx).ParentListingKey)

	seedProperty(t, client, ctx, "LK-LATE")
	ts2 := ts1.Add(time.Hour)
	runPropertyUnitTypeChunk(t, client, ctx, []*ent.RawOutput{
		insertPropertyUnitTypeRaw(t, client, ctx, evID, utPayload("UT-1", "LK-LATE", "Unfurnished"), ts2),
	}, true)
	u := client.PropertyUnitType.Query().Where(propertyunittype.IDEQ("UT-1")).OnlyX(ctx)
	require.NotNil(t, u.ParentListingKey)
	assert.Equal(t, "LK-LATE", *u.ParentListingKey)
}

func TestPropertyUnitTypeBulk_MatchesPerRecord_AB(t *testing.T) {
	run := func(bulk bool) utSnap {
		ctx, client, evID := setupPropertyUnitTypeTest(t)
		seedProperty(t, client, ctx, "LK-1")
		ts := time.Now().UTC().Truncate(time.Second)
		later := ts.Add(time.Hour)
		older := ts.Add(-time.Hour)
		spec1 := []utSpec{
			{key: "INS", lk: "LK-1", f: "Furnished", ts: ts},
			{key: "UPD", lk: "LK-1", f: "Furnished", ts: ts},
			{key: "CLR", lk: "LK-1", f: "Furnished", ts: ts},
			{key: "NODIFF", lk: "LK-1", f: "Partial", ts: ts},
			{key: "DEL", lk: "LK-1", f: "Furnished", ts: ts},
			{key: "STALE", lk: "LK-1", f: "Furnished", ts: ts},
			{key: "PARK", lk: "LK-LATE", f: "Furnished", ts: ts},
		}
		_ = runPropertyUnitTypeChunk(t, client, ctx, seedUTRaws(t, client, ctx, evID, spec1), bulk)
		seedProperty(t, client, ctx, "LK-LATE")
		spec2 := []utSpec{
			{key: "UPD", lk: "LK-1", f: "Unfurnished", ts: later},
			{key: "CLR", lk: "LK-1", f: "", ts: later},
			{key: "NODIFF", lk: "LK-1", f: "Partial", ts: later},
			{key: "DEL", lk: "LK-1", ts: later, del: true},
			{key: "STALE", lk: "LK-1", f: "X", ts: older},
			{key: "PARK", lk: "LK-LATE", f: "Unfurnished", ts: later},
		}
		oc := runPropertyUnitTypeChunk(t, client, ctx, seedUTRaws(t, client, ctx, evID, spec2), bulk)
		snap := takeUTSnapshot(t, client, ctx)
		snap.outcomes = oc
		return snap
	}
	bulk := run(true)
	perRecord := run(false)
	assert.Equal(t, perRecord.outcomes, bulk.outcomes)
	assert.Equal(t, perRecord.entities, bulk.entities)
	assert.Equal(t, perRecord.versions, bulk.versions)
}

func TestRunPass_PropertyUnitTypeBulkPoisonFallback(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	src := client.SourceSystem.Create().SetID("test-src").SetSourceSystemName("test").SaveX(ctx)
	evID := client.SyncEvent.Create().SetSourceSystemID(src.ID).SetResource("property_unit_types").
		SetRunType("sync").SetProcessorVersion("test").SetStartedAt(time.Now()).SaveX(ctx).ID
	seedProperty(t, client, ctx, "LK-1")
	ts := time.Now().UTC().Truncate(time.Second)
	insertPropertyUnitTypeRaw(t, client, ctx, evID, utPayload("UT-1", "LK-1", "A"), ts)
	poison := client.RawOutput.Create().
		SetSyncEventID(evID).SetResource(rawoutput.ResourcePropertyUnitTypes).SetSourceKey("UT-BAD").
		SetChangeType(rawoutput.ChangeTypeInsert).SetSourceModifiedAt(ts).
		SetPayload([]byte(`{"ListingKey":"LK-1"}`)).
		SaveX(ctx)
	insertPropertyUnitTypeRaw(t, client, ctx, evID, utPayload("UT-3", "LK-1", "C"), ts)

	p := New(client, sqlDB, NewPropertyUnitTypeProcessor())
	err := p.RunPass(ctx, rawoutput.ResourcePropertyUnitTypes)
	require.Error(t, err)
	assert.Contains(t, err.Error(), poison.ID.String())
	assert.True(t, client.PropertyUnitType.Query().Where(propertyunittype.IDEQ("UT-1")).ExistX(ctx))
	assert.False(t, client.PropertyUnitType.Query().Where(propertyunittype.IDEQ("UT-3")).ExistX(ctx))
}

type utEntitySnap struct {
	mlgCanView bool
	parentLK   string
	furnished  string
}

type utSnap struct {
	entities map[string]utEntitySnap
	versions map[string][]verSnap
	outcomes []Outcome
}

func takeUTSnapshot(t *testing.T, client *ent.Client, ctx context.Context) utSnap {
	t.Helper()
	snap := utSnap{entities: map[string]utEntitySnap{}, versions: map[string][]verSnap{}}
	for _, e := range client.PropertyUnitType.Query().AllX(ctx) {
		plk, f := "", ""
		if e.ParentListingKey != nil {
			plk = *e.ParentListingKey
		}
		if e.UnitTypeFurnished != nil {
			f = *e.UnitTypeFurnished
		}
		snap.entities[e.ID] = utEntitySnap{mlgCanView: e.MlgCanView, parentLK: plk, furnished: f}
	}
	for _, v := range client.PropertyUnitTypeVersion.Query().Order(ent.Asc(propertyunittypeversion.FieldValidFrom)).AllX(ctx) {
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
		snap.versions[v.UnitTypeKey] = append(snap.versions[v.UnitTypeKey], verSnap{
			changeType:  string(v.ChangeType),
			open:        v.ValidTo == nil,
			changedKeys: joined,
		})
	}
	return snap
}

type utSpec struct {
	key string
	lk  string
	f   string
	ts  time.Time
	del bool
}

func seedUTRaws(t *testing.T, client *ent.Client, ctx context.Context, evID uuid.UUID, spec []utSpec) []*ent.RawOutput {
	t.Helper()
	out := make([]*ent.RawOutput, len(spec))
	for i, s := range spec {
		var payload map[string]any
		if s.del {
			payload = utDelete(s.key, s.lk)
		} else {
			payload = utPayload(s.key, s.lk, s.f)
		}
		time.Sleep(time.Millisecond)
		out[i] = insertPropertyUnitTypeRaw(t, client, ctx, evID, payload, s.ts)
	}
	return out
}
