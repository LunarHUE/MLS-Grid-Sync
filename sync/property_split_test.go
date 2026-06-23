package sync_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/ent/attachmentjob"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/syncevent"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
	"github.com/LunarHUE/MLS-Grid-Sync/mls"
	"github.com/LunarHUE/MLS-Grid-Sync/storage"
	pkgsync "github.com/LunarHUE/MLS-Grid-Sync/sync"
)

// recordingProcessor lets a test assert the exact sequence of RunPass
// calls the splitter+runProcessor chain produces. Distinct from
// fakeProcessor (run_test.go) which doesn't record calls.
type recordingProcessor struct {
	calls []rawoutput.Resource
	err   error
}

func (r *recordingProcessor) RunPass(_ context.Context, resource rawoutput.Resource) error {
	r.calls = append(r.calls, resource)
	return r.err
}

func (r *recordingProcessor) RunPassNoFinalize(_ context.Context, resource rawoutput.Resource) error {
	r.calls = append(r.calls, resource)
	return r.err
}

func propertyRecord(t *testing.T, listingKey string, listingTS time.Time, media []map[string]any, rooms []map[string]any, unitTypes []map[string]any) json.RawMessage {
	t.Helper()
	rec := map[string]any{
		"ListingKey":            listingKey,
		"ModificationTimestamp": listingTS.Format(time.RFC3339),
		"City":                  "Austin",
		"ListPrice":             425000,
	}
	if len(media) > 0 {
		arr := make([]any, len(media))
		for i, m := range media {
			arr[i] = m
		}
		rec["Media"] = arr
	}
	if len(rooms) > 0 {
		arr := make([]any, len(rooms))
		for i, r := range rooms {
			arr[i] = r
		}
		rec["Rooms"] = arr
	}
	if len(unitTypes) > 0 {
		arr := make([]any, len(unitTypes))
		for i, u := range unitTypes {
			arr[i] = u
		}
		rec["UnitTypes"] = arr
	}
	b, err := json.Marshal(rec)
	require.NoError(t, err)
	return b
}

// TestPropertyFetch_SplitsChildrenAndStripsParent rounds a Property page
// through paginate → saveToRawOutput and asserts: parent + each child
// resource lands as its own raw_output row, with the parent's payload no
// longer carrying the three arrays.
func TestPropertyFetch_SplitsChildrenAndStripsParent(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	syncEventID := newSyncEvent(t, client, ctx, syncevent.ResourceProperty)

	parentTS := time.Now().UTC().Truncate(time.Second)
	mediaTS := parentTS.Add(-time.Hour)

	// Seed the Media rows the EnqueueAttachmentJobs FK requires.
	// Production wires a real processor whose Media pass writes these from
	// the split raw_output rows; here we shortcut so the test can run with
	// a nil processor and still satisfy the FK.
	for _, mk := range []string{"M-100a", "M-100b", "M-100c"} {
		seedMedia(t, client, ctx, mk, mediaTS)
	}

	records := []json.RawMessage{
		propertyRecord(t, "P-100", parentTS,
			[]map[string]any{
				{"MediaKey": "M-100a", "MediaModificationTimestamp": mediaTS.Format(time.RFC3339), "MediaURL": "https://example/100a.jpg", "MlgCanView": true},
				{"MediaKey": "M-100b", "MediaModificationTimestamp": mediaTS.Format(time.RFC3339), "MediaURL": "https://example/100b.jpg", "MlgCanView": true},
				{"MediaKey": "M-100c", "MediaModificationTimestamp": mediaTS.Format(time.RFC3339), "MediaURL": "https://example/100c.jpg", "MlgCanView": true},
			},
			[]map[string]any{
				{"RoomKey": "R-100a", "ModificationTimestamp": parentTS.Format(time.RFC3339), "RoomType": "Kitchen"},
				{"RoomKey": "R-100b", "ModificationTimestamp": parentTS.Format(time.RFC3339), "RoomType": "Bath"},
			},
			[]map[string]any{
				{"UnitTypeKey": "U-100", "ModificationTimestamp": parentTS.Format(time.RFC3339), "UnitTypeType": "Apartment"},
			},
		),
	}

	fetcher := &mockFetcher{pages: []*mls.ODataResponse{{Value: records}}}
	svc := pkgsync.NewService(fetcher, client, sqlDB, &storage.FakeStorer{}, nil)

	hwm, err := svc.SyncResource(ctx, syncEventID, "https://api.example/v2", "actris", mls.ResourceProperty, parentTS.Add(-time.Hour))
	require.NoError(t, err)
	assert.True(t, hwm.Equal(parentTS), "HWM must be the parent's timestamp; got %v want %v", hwm, parentTS)

	// One parent row.
	parents, err := client.RawOutput.Query().
		Where(rawoutput.ResourceEQ(rawoutput.ResourceProperty)).
		All(ctx)
	require.NoError(t, err)
	require.Len(t, parents, 1)
	var parentPayload map[string]any
	require.NoError(t, json.Unmarshal(parents[0].Payload, &parentPayload))
	_, hasMedia := parentPayload["Media"]
	_, hasRooms := parentPayload["Rooms"]
	_, hasUnits := parentPayload["UnitTypes"]
	assert.False(t, hasMedia, "stored Property payload must have Media stripped")
	assert.False(t, hasRooms, "stored Property payload must have Rooms stripped")
	assert.False(t, hasUnits, "stored Property payload must have UnitTypes stripped")
	assert.Equal(t, "Austin", parentPayload["City"], "non-stripped Property fields must survive verbatim")

	// 3 media child rows.
	mediaRows, err := client.RawOutput.Query().
		Where(rawoutput.ResourceEQ(rawoutput.ResourceMedia)).
		All(ctx)
	require.NoError(t, err)
	require.Len(t, mediaRows, 3)

	// 2 room child rows.
	roomRows, err := client.RawOutput.Query().
		Where(rawoutput.ResourceEQ(rawoutput.ResourcePropertyRooms)).
		All(ctx)
	require.NoError(t, err)
	require.Len(t, roomRows, 2)

	// 1 unit-type child row.
	unitRows, err := client.RawOutput.Query().
		Where(rawoutput.ResourceEQ(rawoutput.ResourcePropertyUnitTypes)).
		All(ctx)
	require.NoError(t, err)
	require.Len(t, unitRows, 1)
}

// TestPropertyFetch_ReLandDedupsChildren simulates the delta boundary
// re-fetch case: the same Property record (and the same embedded child
// timestamps) landing twice. ON CONFLICT (resource, source_key,
// source_modified_at) DO NOTHING absorbs every duplicate — parent and
// all 3 children — and the second saveToRawOutput returns zero HWM.
func TestPropertyFetch_ReLandDedupsChildren(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	syncEventID := newSyncEvent(t, client, ctx, syncevent.ResourceProperty)

	parentTS := time.Now().UTC().Truncate(time.Second)
	mediaTS := parentTS
	seedMedia(t, client, ctx, "M-DEDUP", mediaTS)

	records := []json.RawMessage{
		propertyRecord(t, "P-DEDUP", parentTS,
			[]map[string]any{
				{"MediaKey": "M-DEDUP", "MediaModificationTimestamp": mediaTS.Format(time.RFC3339), "MediaURL": "https://example/d.jpg", "MlgCanView": true},
			},
			nil,
			nil,
		),
	}

	// First landing: writes parent + 1 media.
	fetcher1 := &mockFetcher{pages: []*mls.ODataResponse{{Value: records}}}
	svc1 := pkgsync.NewService(fetcher1, client, sqlDB, &storage.FakeStorer{}, nil)
	hwm1, err := svc1.SyncResource(ctx, syncEventID, "https://api.example/v2", "actris", mls.ResourceProperty, parentTS.Add(-time.Hour))
	require.NoError(t, err)
	assert.True(t, hwm1.Equal(parentTS), "first landing returns parent's timestamp")

	// Second landing: simulates the ge boundary re-fetch.
	fetcher2 := &mockFetcher{pages: []*mls.ODataResponse{{Value: records}}}
	svc2 := pkgsync.NewService(fetcher2, client, sqlDB, &storage.FakeStorer{}, nil)
	hwm2, err := svc2.SyncResource(ctx, syncEventID, "https://api.example/v2", "actris", mls.ResourceProperty, parentTS.Add(-time.Hour))
	require.NoError(t, err)
	assert.True(t, hwm2.IsZero(),
		"re-land of identical Property+children must produce zero parent HWM (carry-forward signal)")

	// Counts: exactly one of each.
	parents, _ := client.RawOutput.Query().Where(rawoutput.ResourceEQ(rawoutput.ResourceProperty)).Count(ctx)
	media, _ := client.RawOutput.Query().Where(rawoutput.ResourceEQ(rawoutput.ResourceMedia)).Count(ctx)
	assert.Equal(t, 1, parents, "ON CONFLICT absorbs duplicate parent")
	assert.Equal(t, 1, media, "ON CONFLICT absorbs duplicate media child")
}

// TestRunInitial_PropertyTriggersChildPasses is the trigger-order pin:
// a Property fetch runs the property pass, then the three child passes,
// in dependency order. RunInitial for any other resource still runs
// just that resource's pass.
func TestRunInitial_PropertyTriggersChildPasses(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	src, err := client.SourceSystem.Create().SetID("mlsgrid-test").SetSourceSystemName("MLS Grid (test)").Save(ctx)
	require.NoError(t, err)

	parentTS := time.Now().UTC().Truncate(time.Second)
	seedMedia(t, client, ctx, "M-TRIG", parentTS)
	records := []json.RawMessage{
		propertyRecord(t, "P-TRIG", parentTS,
			[]map[string]any{{"MediaKey": "M-TRIG", "MediaModificationTimestamp": parentTS.Format(time.RFC3339), "MediaURL": "https://example/t.jpg", "MlgCanView": true}},
			nil,
			nil,
		),
	}
	fetcher := &mockFetcher{pages: []*mls.ODataResponse{{Value: records}}}
	rec := &recordingProcessor{}
	svc := pkgsync.NewService(fetcher, client, sqlDB, &storage.FakeStorer{}, rec)

	require.NoError(t, svc.RunInitial(ctx, src.ID, "https://api.example/v2", "actris", rawoutput.ResourceProperty))

	want := []rawoutput.Resource{
		rawoutput.ResourceProperty,
		rawoutput.ResourceMedia,
		rawoutput.ResourcePropertyRooms,
		rawoutput.ResourcePropertyUnitTypes,
	}
	assert.Equal(t, want, rec.calls,
		"a Property fetch must trigger four processor passes in dependency order")
}

// TestRunInitial_NonPropertyTriggersOnlySelfPass guards the negative:
// fetching Member (or any non-Property resource) still triggers exactly
// one pass — the splitter only fires for Property.
func TestRunInitial_NonPropertyTriggersOnlySelfPass(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	src, err := client.SourceSystem.Create().SetID("mlsgrid-test").SetSourceSystemName("MLS Grid (test)").Save(ctx)
	require.NoError(t, err)

	ts := time.Now().UTC().Truncate(time.Second)
	records := rawRecords(t,
		map[string]any{"MemberKey": "MEM-T", "ModificationTimestamp": ts.Format(time.RFC3339), "MemberFirstName": "Sam"},
	)
	fetcher := &mockFetcher{pages: []*mls.ODataResponse{{Value: records}}}
	rec := &recordingProcessor{}
	svc := pkgsync.NewService(fetcher, client, sqlDB, &storage.FakeStorer{}, rec)

	require.NoError(t, svc.RunInitial(ctx, src.ID, "https://api.example/v2", "actris", rawoutput.ResourceMember))

	assert.Equal(t, []rawoutput.Resource{rawoutput.ResourceMember}, rec.calls,
		"non-Property fetches trigger only the resource's own pass")
}

// TestPropertyFetch_EnqueueRehome confirms the EnqueueAttachmentJobs
// re-home: media children with MlgCanView=true produce attachment_jobs;
// MlgCanView=false media are skipped. The enqueue trigger is now the
// splitter's media output, not a separate /v2/Media fetch.
func TestPropertyFetch_EnqueueRehome(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	syncEventID := newSyncEvent(t, client, ctx, syncevent.ResourceProperty)

	parentTS := time.Now().UTC().Truncate(time.Second)
	// EnqueueAttachmentJobs creates a job only for M-VIS; M-HID is filtered
	// by the MlgCanView guard. Only M-VIS needs the FK parent.
	seedMedia(t, client, ctx, "M-VIS", parentTS)
	records := []json.RawMessage{
		propertyRecord(t, "P-ENQ", parentTS,
			[]map[string]any{
				{"MediaKey": "M-VIS", "MediaModificationTimestamp": parentTS.Format(time.RFC3339), "MediaURL": "https://example/v.jpg", "MlgCanView": true},
				{"MediaKey": "M-HID", "MediaModificationTimestamp": parentTS.Format(time.RFC3339), "MediaURL": "https://example/h.jpg", "MlgCanView": false},
			},
			nil,
			nil,
		),
	}

	fetcher := &mockFetcher{pages: []*mls.ODataResponse{{Value: records}}}
	svc := pkgsync.NewService(fetcher, client, sqlDB, &storage.FakeStorer{}, nil)
	_, err := svc.SyncResource(ctx, syncEventID, "https://api.example/v2", "actris", mls.ResourceProperty, parentTS.Add(-time.Hour))
	require.NoError(t, err)

	// Only M-VIS should produce an attachment_job; M-HID is filtered by
	// EnqueueAttachmentJobs' MlgCanView guard.
	jobs, err := client.AttachmentJob.Query().All(ctx)
	require.NoError(t, err)
	require.Len(t, jobs, 1, "exactly one visible media should yield an attachment_job")
	assert.Equal(t, "M-VIS", jobs[0].MediaKey)
	assert.Equal(t, attachmentjob.StatusPending, jobs[0].Status)
}
