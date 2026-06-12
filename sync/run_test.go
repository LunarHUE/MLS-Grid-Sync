package sync_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/syncevent"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
	"github.com/LunarHUE/MLS-Grid-Sync/mls"
	"github.com/LunarHUE/MLS-Grid-Sync/storage"
	pkgsync "github.com/LunarHUE/MLS-Grid-Sync/sync"
)

// fakeProcessor lets tests opt the processor pass into success or a
// canned failure without spinning up the real registry.
type fakeProcessor struct {
	err error
}

func (f *fakeProcessor) RunPass(_ context.Context, _ rawoutput.Resource) error {
	return f.err
}

// seedSourceSystem ensures the source_system row the lifecycle wrappers
// FK to exists. Returns its ID.
func seedSourceSystem(t *testing.T, client *ent.Client, ctx context.Context) string {
	t.Helper()
	src, err := client.SourceSystem.Create().
		SetID("mlsgrid-test").
		SetSourceSystemName("MLS Grid (test)").
		Save(ctx)
	if err == nil {
		return src.ID
	}
	// Tolerate "already exists" so a test can call this twice without ordering it.
	existing, qerr := client.SourceSystem.Query().Only(ctx)
	require.NoError(t, qerr, "seed: %v then query: %v", err, qerr)
	return existing.ID
}

// onlyEvent returns the single sync_event row, failing the test if
// there isn't exactly one. Most lifecycle tests want to assert against
// the row just created.
func onlyEvent(t *testing.T, client *ent.Client, ctx context.Context) *ent.SyncEvent {
	t.Helper()
	rows, err := client.SyncEvent.Query().All(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1, "expected exactly one sync_event")
	return rows[0]
}

// --- RunInitial ---

func TestRunInitial_HappyPath(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	srcID := seedSourceSystem(t, client, ctx)

	modAt := time.Now().UTC().Truncate(time.Second)
	fetcher := &mockFetcher{pages: []*mls.ODataResponse{
		{Value: rawRecords(t,
			map[string]any{"LookupKey": "k1", "LookupName": "A", "LookupValue": "1", "ModificationTimestamp": modAt.Format(time.RFC3339)},
		), NextLink: ""},
	}}
	svc := pkgsync.NewService(fetcher, client, sqlDB, &storage.FakeStorer{}, &fakeProcessor{})

	err := svc.RunInitial(ctx, srcID, "https://api.example/v2", "actris", rawoutput.ResourceLookup)
	require.NoError(t, err)

	ev := onlyEvent(t, client, ctx)
	assert.Equal(t, syncevent.RunTypeBackfill, ev.RunType)
	assert.Equal(t, syncevent.StatusSuccess, ev.Status)
	require.NotNil(t, ev.EndedAt, "ended_at must be set")
	require.NotNil(t, ev.HighWaterMark, "success run must stamp hwm")
	assert.True(t, ev.HighWaterMark.Equal(modAt), "hwm must be the max source_modified_at: want %v got %v", modAt, *ev.HighWaterMark)
	assert.Nil(t, ev.ErrorSummary)
}

func TestRunInitial_ZeroRecordStampsRunStart(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	srcID := seedSourceSystem(t, client, ctx)

	fetcher := &mockFetcher{pages: []*mls.ODataResponse{{Value: nil, NextLink: ""}}}
	svc := pkgsync.NewService(fetcher, client, sqlDB, &storage.FakeStorer{}, &fakeProcessor{})

	// Postgres timestamptz is microsecond-precision; Go time.Now() carries
	// nanoseconds. Truncate the bracketing reads to match, otherwise a stamp
	// captured in the same microsecond as `before` can still appear "before"
	// it by the lost ns digits.
	before := time.Now().UTC().Truncate(time.Microsecond)
	err := svc.RunInitial(ctx, srcID, "https://api.example/v2", "actris", rawoutput.ResourceLookup)
	require.NoError(t, err)
	after := time.Now().UTC().Truncate(time.Microsecond)

	ev := onlyEvent(t, client, ctx)
	assert.Equal(t, syncevent.StatusSuccess, ev.Status)
	require.NotNil(t, ev.HighWaterMark, "zero-record success must still stamp hwm — Phase 4 §8")
	stamp := *ev.HighWaterMark
	assert.True(t, !stamp.Before(before) && !stamp.After(after),
		"zero-record initial must stamp run-start (between %v and %v); got %v", before, after, stamp)
}

func TestRunInitial_FetchFailureStampsFailed(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	srcID := seedSourceSystem(t, client, ctx)

	fetcher := &mockFetcher{err: errors.New("connection refused")}
	svc := pkgsync.NewService(fetcher, client, sqlDB, &storage.FakeStorer{}, &fakeProcessor{})

	err := svc.RunInitial(ctx, srcID, "https://api.example/v2", "actris", rawoutput.ResourceLookup)
	require.Error(t, err)

	ev := onlyEvent(t, client, ctx)
	assert.Equal(t, syncevent.StatusFailed, ev.Status)
	assert.Nil(t, ev.HighWaterMark, "failed run must NOT stamp hwm — Phase 4 §7 silent-data-loss guard")
	require.NotNil(t, ev.EndedAt)
	require.NotNil(t, ev.ErrorSummary)
	assert.Contains(t, *ev.ErrorSummary, "connection refused")
}

func TestRunInitial_ProcessorFailurePropagates(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	srcID := seedSourceSystem(t, client, ctx)

	fetcher := &mockFetcher{pages: []*mls.ODataResponse{
		{Value: rawRecords(t, map[string]any{"LookupKey": "k1", "LookupName": "A", "LookupValue": "1"}), NextLink: ""},
	}}
	svc := pkgsync.NewService(fetcher, client, sqlDB, &storage.FakeStorer{}, &fakeProcessor{err: errors.New("processor halt")})

	err := svc.RunInitial(ctx, srcID, "https://api.example/v2", "actris", rawoutput.ResourceLookup)
	require.Error(t, err, "processor failure MUST propagate — regression for the silent-swallow bug")
	assert.Contains(t, err.Error(), "processor halt")

	ev := onlyEvent(t, client, ctx)
	assert.Equal(t, syncevent.StatusFailed, ev.Status, "processor halt should stamp failed")
	assert.Nil(t, ev.HighWaterMark)
}

// --- RunDelta ---

func TestRunDelta_HappyPathReadsHWM(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	srcID := seedSourceSystem(t, client, ctx)

	// Seed a prior successful event so RunDelta reads its hwm, not the fallback.
	priorHWM := time.Now().UTC().Truncate(time.Second).Add(-2 * time.Hour)
	_, err := client.SyncEvent.Create().
		SetSourceSystemID(srcID).
		SetResource(syncevent.ResourceLookup).
		SetRunType(syncevent.RunTypeSync).
		SetStatus(syncevent.StatusSuccess).
		SetProcessorVersion("test").
		SetStartedAt(priorHWM.Add(-time.Hour)).
		SetHighWaterMark(priorHWM).
		Save(ctx)
	require.NoError(t, err)

	newHWM := priorHWM.Add(time.Hour)
	fetcher := &mockFetcher{pages: []*mls.ODataResponse{
		{Value: rawRecords(t,
			map[string]any{"LookupKey": "k2", "LookupName": "A", "LookupValue": "2", "ModificationTimestamp": newHWM.Format(time.RFC3339)},
		), NextLink: ""},
	}}
	svc := pkgsync.NewService(fetcher, client, sqlDB, &storage.FakeStorer{}, &fakeProcessor{})

	require.NoError(t, svc.RunDelta(ctx, srcID, "https://api.example/v2", "actris", rawoutput.ResourceLookup))

	// Find the new event (run_type=sync, distinct from the seeded one which
	// we also marked sync but has hwm=priorHWM).
	events, err := client.SyncEvent.Query().Order(ent.Asc(syncevent.FieldStartedAt)).All(ctx)
	require.NoError(t, err)
	require.Len(t, events, 2)
	latest := events[1]
	assert.Equal(t, syncevent.RunTypeSync, latest.RunType)
	assert.Equal(t, syncevent.StatusSuccess, latest.Status)
	require.NotNil(t, latest.HighWaterMark)
	assert.True(t, latest.HighWaterMark.Equal(newHWM), "want %v got %v", newHWM, *latest.HighWaterMark)
}

func TestRunDelta_ZeroRecordCarriesCursorForward(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	srcID := seedSourceSystem(t, client, ctx)

	// Prior success with a known hwm. RunDelta should read it as the cursor,
	// find zero records, and stamp the new success with hwm = that cursor
	// (NOT NULL, NOT the month-ago fallback).
	cursorWas := time.Now().UTC().Truncate(time.Second).Add(-90 * time.Minute)
	_, err := client.SyncEvent.Create().
		SetSourceSystemID(srcID).
		SetResource(syncevent.ResourceLookup).
		SetRunType(syncevent.RunTypeSync).
		SetStatus(syncevent.StatusSuccess).
		SetProcessorVersion("test").
		SetStartedAt(cursorWas.Add(-time.Hour)).
		SetHighWaterMark(cursorWas).
		Save(ctx)
	require.NoError(t, err)

	fetcher := &mockFetcher{pages: []*mls.ODataResponse{{Value: nil, NextLink: ""}}}
	svc := pkgsync.NewService(fetcher, client, sqlDB, &storage.FakeStorer{}, &fakeProcessor{})

	require.NoError(t, svc.RunDelta(ctx, srcID, "https://api.example/v2", "actris", rawoutput.ResourceLookup))

	events, err := client.SyncEvent.Query().Order(ent.Asc(syncevent.FieldStartedAt)).All(ctx)
	require.NoError(t, err)
	require.Len(t, events, 2)
	latest := events[1]
	assert.Equal(t, syncevent.StatusSuccess, latest.Status)
	require.NotNil(t, latest.HighWaterMark, "zero-record success must carry the cursor forward, not stamp NULL — Phase 4 §8")
	assert.True(t, latest.HighWaterMark.Equal(cursorWas),
		"zero-record delta must carry the queried cursor forward; want %v got %v", cursorWas, *latest.HighWaterMark)
}

func TestRunDelta_FetchFailureLeavesNullHWM(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	srcID := seedSourceSystem(t, client, ctx)

	fetcher := &mockFetcher{err: fmt.Errorf("upstream 500")}
	svc := pkgsync.NewService(fetcher, client, sqlDB, &storage.FakeStorer{}, &fakeProcessor{})

	err := svc.RunDelta(ctx, srcID, "https://api.example/v2", "actris", rawoutput.ResourceLookup)
	require.Error(t, err)

	ev := onlyEvent(t, client, ctx)
	assert.Equal(t, syncevent.StatusFailed, ev.Status)
	assert.Nil(t, ev.HighWaterMark, "failed delta MUST NOT advance the cursor — Phase 4 §7")
	require.NotNil(t, ev.ErrorSummary)
}
