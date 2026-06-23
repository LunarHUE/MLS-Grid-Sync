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
	entmedia "github.com/LunarHUE/MLS-Grid-Sync/ent/media"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/mediaversion"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
)

func runMediaChunk(t *testing.T, client *ent.Client, ctx context.Context, raws []*ent.RawOutput, bulk bool) []Outcome {
	t.Helper()
	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	p := NewMediaProcessor()

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
		t.Fatalf("media chunk (bulk=%v): %v", bulk, err)
	}
	require.NoError(t, tx.Commit())
	return outcomes
}

func TestMediaBulk_FreshInserts(t *testing.T) {
	ctx, client, evID := setupMediaTest(t)
	ts := time.Now().UTC().Truncate(time.Second)

	raws := []*ent.RawOutput{
		insertMediaRaw(t, client, ctx, evID, mediaPayload("M-1", "LK-1", "https://cdn/1.jpg"), ts),
		insertMediaRaw(t, client, ctx, evID, mediaPayload("M-2", "LK-1", "https://cdn/2.jpg"), ts),
		insertMediaRaw(t, client, ctx, evID, mediaPayload("M-3", "LK-2", "https://cdn/3.jpg"), ts),
	}
	outcomes := runMediaChunk(t, client, ctx, raws, true)
	assert.Equal(t, []Outcome{OutcomeInsert, OutcomeInsert, OutcomeInsert}, outcomes)

	for _, k := range []string{"M-1", "M-2", "M-3"} {
		m, err := client.Media.Query().Where(entmedia.IDEQ(k)).Only(ctx)
		require.NoError(t, err, "media %s", k)
		require.NotNil(t, m.CurrentVersionID)
		vs := client.MediaVersion.Query().Where(mediaversion.MediaKey(k)).AllX(ctx)
		require.Len(t, vs, 1)
		assert.Equal(t, mediaversion.ChangeTypeInsert, vs[0].ChangeType)
		assert.Nil(t, vs[0].ValidTo)
		assert.Equal(t, m.CurrentVersionID.String(), vs[0].ID)
	}
}

// TestMediaBulk_UpdatePreservesCreatedAtAndAttachmentID is the media-specific
// guard: a bulk upsert must NOT clobber created_at OR attachment_id (the
// download pointer). applyToMediaCreate never sets attachment_id, so it stays
// out of the INSERT column set and UpdateNewValues leaves it untouched.
func TestMediaBulk_UpdatePreservesCreatedAtAndAttachmentID(t *testing.T) {
	ctx, client, evID := setupMediaTest(t)
	ts1 := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, runMediaProcess(t, client, ctx,
		insertMediaRaw(t, client, ctx, evID, mediaPayload("M-1", "LK-1", "https://cdn/old.jpg"), ts1)))

	// Simulate the binary having been downloaded: create an attachment and
	// point the media row at it (attachment_id has a real FK).
	att := client.Attachment.Create().
		SetSourceURL("https://cdn/old.jpg").
		SetSourceHash("deadbeef").
		SetHostURL("https://store/old.jpg").
		SaveX(ctx)
	attID := att.ID
	client.Media.UpdateOneID("M-1").SetAttachmentID(attID).SaveX(ctx)
	before := client.Media.Query().Where(entmedia.IDEQ("M-1")).OnlyX(ctx)

	time.Sleep(5 * time.Millisecond)
	ts2 := ts1.Add(time.Hour)
	upd := insertMediaRaw(t, client, ctx, evID, mediaPayload("M-1", "LK-1", "https://cdn/new.jpg"), ts2)
	outcomes := runMediaChunk(t, client, ctx, []*ent.RawOutput{upd}, true)
	assert.Equal(t, []Outcome{OutcomeUpdate}, outcomes)

	after := client.Media.Query().Where(entmedia.IDEQ("M-1")).OnlyX(ctx)
	require.NotNil(t, after.MediaURL)
	assert.Equal(t, "https://cdn/new.jpg", *after.MediaURL, "url updated")
	assert.True(t, after.CreatedAt.Equal(before.CreatedAt), "created_at preserved")
	require.NotNil(t, after.AttachmentID, "attachment_id must survive the bulk upsert")
	assert.Equal(t, attID, *after.AttachmentID, "attachment_id (download pointer) preserved")

	vs := client.MediaVersion.Query().Where(mediaversion.MediaKey("M-1")).
		Order(ent.Asc(mediaversion.FieldValidFrom)).AllX(ctx)
	require.Len(t, vs, 2)
	require.NotNil(t, vs[0].ValidTo, "prior version closed")
	assert.Nil(t, vs[1].ValidTo)
	assert.Equal(t, mediaversion.ChangeTypeUpdate, vs[1].ChangeType)
}

func TestMediaBulk_SkipsWriteNothing(t *testing.T) {
	ctx, client, evID := setupMediaTest(t)
	ts1 := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, runMediaProcess(t, client, ctx,
		insertMediaRaw(t, client, ctx, evID, mediaPayload("M-1", "LK-1", "https://cdn/1.jpg"), ts1)))
	require.NoError(t, runMediaProcess(t, client, ctx,
		insertMediaRaw(t, client, ctx, evID, mediaPayload("M-2", "LK-1", "https://cdn/2.jpg"), ts1)))

	ts2 := ts1.Add(time.Hour)
	noDiff := insertMediaRaw(t, client, ctx, evID, mediaPayload("M-1", "LK-1", "https://cdn/1.jpg"), ts2)
	ts0 := ts1.Add(-time.Hour)
	stale := insertMediaRaw(t, client, ctx, evID, mediaPayload("M-2", "LK-1", "https://cdn/x.jpg"), ts0)

	outcomes := runMediaChunk(t, client, ctx, []*ent.RawOutput{noDiff, stale}, true)
	assert.Equal(t, []Outcome{OutcomeSkipNoDiff, OutcomeSkipStale}, outcomes)
	assert.Equal(t, 1, client.MediaVersion.Query().Where(mediaversion.MediaKey("M-1")).CountX(ctx))
	assert.Equal(t, 1, client.MediaVersion.Query().Where(mediaversion.MediaKey("M-2")).CountX(ctx))
	assert.Equal(t, "https://cdn/2.jpg", *client.Media.Query().Where(entmedia.IDEQ("M-2")).OnlyX(ctx).MediaURL, "stale must not overwrite")
}

func TestMediaBulk_DuplicateKeysInChunk(t *testing.T) {
	ts := time.Now().UTC().Truncate(time.Second)
	spec := []mediaSpec{
		{key: "M-1", rec: "LK-1", url: "https://cdn/a.jpg", ts: ts},
		{key: "M-2", rec: "LK-1", url: "https://cdn/b.jpg", ts: ts},
		{key: "M-1", rec: "LK-1", url: "https://cdn/a2.jpg", ts: ts.Add(time.Hour)},
	}
	bulk := mediaSnapshotAfterChunk(t, spec, true)
	perRecord := mediaSnapshotAfterChunk(t, spec, false)
	assert.Equal(t, perRecord.outcomes, bulk.outcomes)
	assertMediaSnapshotsEqual(t, perRecord, bulk)

	require.Len(t, bulk.versions["M-1"], 2)
	assert.Equal(t, "insert", bulk.versions["M-1"][0].changeType)
	assert.False(t, bulk.versions["M-1"][0].open)
	assert.Equal(t, "update", bulk.versions["M-1"][1].changeType)
	assert.True(t, bulk.versions["M-1"][1].open)
}

func TestMediaBulk_MatchesPerRecord_AB(t *testing.T) {
	ts := time.Now().UTC().Truncate(time.Second)
	later := ts.Add(time.Hour)
	older := ts.Add(-time.Hour)
	spec1 := []mediaSpec{
		{key: "INS", rec: "LK-1", url: "https://cdn/ins.jpg", ts: ts},
		{key: "UPD", rec: "LK-1", url: "https://cdn/u1.jpg", ts: ts},
		{key: "NODIFF", rec: "LK-1", url: "https://cdn/nd.jpg", ts: ts},
		{key: "DEL", rec: "LK-1", url: "https://cdn/d.jpg", ts: ts},
		{key: "STALE", rec: "LK-1", url: "https://cdn/s.jpg", ts: ts},
	}
	spec2 := []mediaSpec{
		{key: "UPD", rec: "LK-1", url: "https://cdn/u2.jpg", ts: later},    // update
		{key: "NODIFF", rec: "LK-1", url: "https://cdn/nd.jpg", ts: later}, // no-diff
		{key: "DEL", rec: "LK-1", ts: later, del: true},                    // delete existing
		{key: "STALE", rec: "LK-1", url: "https://cdn/sx.jpg", ts: older},  // stale
		{key: "NEWDEL", rec: "LK-1", ts: later, del: true},                 // delete first-sighting
	}

	run := func(bulk bool) mediaSnap {
		ctx, client, evID := setupMediaTest(t)
		_ = runMediaChunk(t, client, ctx, seedMediaRaws(t, client, ctx, evID, spec1), bulk)
		oc := runMediaChunk(t, client, ctx, seedMediaRaws(t, client, ctx, evID, spec2), bulk)
		snap := takeMediaSnapshot(t, client, ctx)
		snap.outcomes = oc
		return snap
	}
	bulk := run(true)
	perRecord := run(false)
	assert.Equal(t, perRecord.outcomes, bulk.outcomes)
	assertMediaSnapshotsEqual(t, perRecord, bulk)
}

func TestRunPass_MediaBulkPoisonFallback(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	evID := seedMediaSyncEvent(t, client, ctx)

	ts := time.Now().UTC().Truncate(time.Second)
	insertMediaRaw(t, client, ctx, evID, mediaPayload("M-1", "LK-1", "https://cdn/1.jpg"), ts)
	insertMediaRaw(t, client, ctx, evID, mediaPayload("M-2", "LK-1", "https://cdn/2.jpg"), ts)
	// Valid JSON the column accepts, but parseMedia rejects (missing MediaKey).
	poison := client.RawOutput.Create().
		SetSyncEventID(evID).
		SetResource(rawoutput.ResourceMedia).
		SetSourceKey("M-BAD").
		SetChangeType(rawoutput.ChangeTypeInsert).
		SetSourceModifiedAt(ts).
		SetPayload([]byte(`{"ResourceName":"Property"}`)).
		SaveX(ctx)
	insertMediaRaw(t, client, ctx, evID, mediaPayload("M-3", "LK-1", "https://cdn/3.jpg"), ts)

	p := New(client, sqlDB, NewMediaProcessor()) // bulk on
	err := p.RunPass(ctx, rawoutput.ResourceMedia)
	require.Error(t, err)
	assert.Contains(t, err.Error(), poison.ID.String())
	assert.True(t, client.Media.Query().Where(entmedia.IDEQ("M-1")).ExistX(ctx))
	assert.True(t, client.Media.Query().Where(entmedia.IDEQ("M-2")).ExistX(ctx))
	assert.False(t, client.Media.Query().Where(entmedia.IDEQ("M-3")).ExistX(ctx))
}

// --- media snapshot / seeding helpers ---

type mediaSnap struct {
	entities map[string]bool // media_key -> mlg_can_view
	versions map[string][]verSnap
	outcomes []Outcome
}

func takeMediaSnapshot(t *testing.T, client *ent.Client, ctx context.Context) mediaSnap {
	t.Helper()
	snap := mediaSnap{entities: map[string]bool{}, versions: map[string][]verSnap{}}
	for _, e := range client.Media.Query().AllX(ctx) {
		snap.entities[e.ID] = e.MlgCanView
	}
	for _, v := range client.MediaVersion.Query().Order(ent.Asc(mediaversion.FieldValidFrom)).AllX(ctx) {
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
		snap.versions[v.MediaKey] = append(snap.versions[v.MediaKey], verSnap{
			changeType:  string(v.ChangeType),
			open:        v.ValidTo == nil,
			changedKeys: joined,
		})
	}
	return snap
}

func assertMediaSnapshotsEqual(t *testing.T, want, got mediaSnap) {
	t.Helper()
	assert.Equal(t, want.entities, got.entities, "media entity set / mlg_can_view must match")
	assert.Equal(t, want.versions, got.versions, "media version chains must match")
}

func mediaSnapshotAfterChunk(t *testing.T, spec []mediaSpec, bulk bool) mediaSnap {
	t.Helper()
	ctx, client, evID := setupMediaTest(t)
	oc := runMediaChunk(t, client, ctx, seedMediaRaws(t, client, ctx, evID, spec), bulk)
	snap := takeMediaSnapshot(t, client, ctx)
	snap.outcomes = oc
	return snap
}

type mediaSpec struct {
	key string
	rec string
	url string
	ts  time.Time
	del bool
}

func mediaPayload(key, recordKey, url string) map[string]any {
	return map[string]any{
		"MediaKey":          key,
		"ResourceName":      "Property",
		"ResourceRecordKey": recordKey,
		"MediaURL":          url,
		"Order":             1,
	}
}

func mediaDelete(key, recordKey string) map[string]any {
	return map[string]any{
		"MediaKey":          key,
		"ResourceName":      "Property",
		"ResourceRecordKey": recordKey,
		"MlgCanView":        false,
	}
}

func seedMediaRaws(t *testing.T, client *ent.Client, ctx context.Context, evID uuid.UUID, spec []mediaSpec) []*ent.RawOutput {
	t.Helper()
	out := make([]*ent.RawOutput, len(spec))
	for i, s := range spec {
		var payload map[string]any
		if s.del {
			payload = mediaDelete(s.key, s.rec)
		} else {
			payload = mediaPayload(s.key, s.rec, s.url)
		}
		time.Sleep(time.Millisecond)
		out[i] = insertMediaRaw(t, client, ctx, evID, payload, s.ts)
	}
	return out
}

func seedMediaSyncEvent(t *testing.T, client *ent.Client, ctx context.Context) uuid.UUID {
	t.Helper()
	src := client.SourceSystem.Create().SetID("test-src").SetSourceSystemName("test").SaveX(ctx)
	return client.SyncEvent.Create().
		SetSourceSystemID(src.ID).
		SetResource("media").
		SetRunType("sync").
		SetProcessorVersion("test").
		SetStartedAt(time.Now()).
		SaveX(ctx).ID
}
