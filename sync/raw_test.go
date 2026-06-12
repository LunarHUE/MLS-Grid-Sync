package sync_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/syncevent"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
	"github.com/LunarHUE/MLS-Grid-Sync/mls"
	"github.com/LunarHUE/MLS-Grid-Sync/storage"
	pkgsync "github.com/LunarHUE/MLS-Grid-Sync/sync"
)

// TestSaveToRawOutput_BoundaryReplayDedupes pins Phase 4 §7's two-layer
// idempotency. Two runs share a boundary record (same source_key, same
// source_modified_at). The first run lands it; the second run is told to
// land it again (simulating the ge boundary re-fetch). The unique index
// drops the duplicate on the floor via ON CONFLICT DO NOTHING. The second
// run's HWM return reflects "no new rows" (zero time.Time) — the carry-
// forward signal that §8 uses for zero-record / all-duplicate runs.
func TestSaveToRawOutput_BoundaryReplayDedupes(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()

	syncEventID := newSyncEvent(t, client, ctx, syncevent.ResourceLookup)

	ts := time.Now().UTC().Truncate(time.Second)
	records := rawRecords(t,
		map[string]any{
			"LookupKey":             "boundary-1",
			"LookupName":            "X",
			"LookupValue":           "Y",
			"ModificationTimestamp": ts.Format(time.RFC3339),
		},
	)
	// First landing: write the boundary record.
	fetcher1 := &mockFetcher{pages: []*mls.ODataResponse{{Value: records}}}
	svc1 := pkgsync.NewService(fetcher1, client, sqlDB, &storage.FakeStorer{}, nil)
	firstHWM, err := svc1.SyncResource(ctx, syncEventID, "https://api.mlsgrid.com/v2", "actris", mls.ResourceLookup, ts.Add(-time.Hour))
	require.NoError(t, err)
	assert.True(t, firstHWM.Equal(ts), "first run lands the record and returns its timestamp")

	// Second landing: same record, simulating the ge boundary re-fetch.
	fetcher2 := &mockFetcher{pages: []*mls.ODataResponse{{Value: records}}}
	svc2 := pkgsync.NewService(fetcher2, client, sqlDB, &storage.FakeStorer{}, nil)
	hwm, err := svc2.SyncResource(ctx, syncEventID, "https://api.mlsgrid.com/v2", "actris", mls.ResourceLookup, ts.Add(-time.Hour))
	require.NoError(t, err)

	count, err := client.RawOutput.Query().
		Where(rawoutput.SourceKey("boundary-1")).
		Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "ON CONFLICT must drop the duplicate boundary record")

	assert.True(t, hwm.IsZero(),
		"the second run wrote nothing — HWM must be zero so the caller carries the cursor forward")
}

// TestSaveToRawOutput_ReturnsMaxWritten pins the HWM aggregation: the
// per-call HWM is the max source_modified_at of rows that *actually*
// landed (not just rows in the input batch). Two records arrive; the
// caller gets the later timestamp.
func TestSaveToRawOutput_ReturnsMaxWritten(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	syncEventID := newSyncEvent(t, client, ctx, syncevent.ResourceLookup)

	tEarlier := time.Now().UTC().Truncate(time.Second).Add(-2 * time.Hour)
	tLater := tEarlier.Add(time.Hour)

	records := rawRecords(t,
		map[string]any{
			"LookupKey":             "k-early",
			"LookupName":            "X",
			"LookupValue":           "1",
			"ModificationTimestamp": tEarlier.Format(time.RFC3339),
		},
		map[string]any{
			"LookupKey":             "k-late",
			"LookupName":            "X",
			"LookupValue":           "2",
			"ModificationTimestamp": tLater.Format(time.RFC3339),
		},
	)
	fetcher := &mockFetcher{pages: []*mls.ODataResponse{{Value: records}}}
	svc := pkgsync.NewService(fetcher, client, sqlDB, &storage.FakeStorer{}, nil)

	hwm, err := svc.SyncResource(ctx, syncEventID, "https://api.mlsgrid.com/v2", "actris", mls.ResourceLookup, tEarlier.Add(-time.Hour))
	require.NoError(t, err)

	assert.True(t, hwm.Equal(tLater), "expected %v, got %v", tLater, hwm)
}

// helper to keep mockFetcher idle for cases that don't fetch pages.
var _ = uuid.UUID{}
