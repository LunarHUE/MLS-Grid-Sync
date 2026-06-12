package sync_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/syncevent"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
	"github.com/LunarHUE/MLS-Grid-Sync/storage"
	pkgsync "github.com/LunarHUE/MLS-Grid-Sync/sync"
)

// seedEvent inserts a single sync_event for the given resource with the
// caller-supplied fields. All other required fields get sensible defaults.
func seedEvent(t *testing.T, client *ent.Client, ctx context.Context, resource syncevent.Resource, status syncevent.Status, startedAt time.Time, hwm *time.Time) *ent.SyncEvent {
	t.Helper()
	src, _ := client.SourceSystem.Create().
		SetID("mlsgrid-test").
		SetSourceSystemName("MLS Grid (test)").
		Save(ctx)
	if src == nil {
		src, _ = client.SourceSystem.Query().Only(ctx)
	}
	creator := client.SyncEvent.Create().
		SetSourceSystemID(src.ID).
		SetResource(resource).
		SetRunType(syncevent.RunTypeSync).
		SetStatus(status).
		SetProcessorVersion("test").
		SetStartedAt(startedAt)
	if hwm != nil {
		creator.SetHighWaterMark(*hwm)
	}
	ev, err := creator.Save(ctx)
	require.NoError(t, err)
	return ev
}

// TestLastSuccessfulHWM_IgnoresFailed pins Phase 4 §7's silent-data-loss
// regression: even when a more recent failed (or stale-swept) run exists,
// the cursor must point at the last *successful* run's high_water_mark.
// Pre-Phase-4 the cursor was derived from raw_output.max(source_modified_at),
// which conflated fetched-and-landed with completed and silently advanced
// the cursor past records a partial run never saw.
func TestLastSuccessfulHWM_IgnoresFailed(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()

	tLow := time.Now().UTC().Truncate(time.Second).Add(-3 * time.Hour)
	tHigh := tLow.Add(2 * time.Hour)

	// Seed: an earlier successful run with hwm=tLow, then a later run that
	// failed (no hwm written).
	seedEvent(t, client, ctx, syncevent.ResourceLookup, syncevent.StatusSuccess, tLow, &tLow)
	seedEvent(t, client, ctx, syncevent.ResourceLookup, syncevent.StatusFailed, tHigh, nil)

	svc := pkgsync.NewService(nil, client, sqlDB, &storage.FakeStorer{}, nil)

	got, err := svc.LastSuccessfulHWM(ctx, syncevent.ResourceLookup)
	require.NoError(t, err)
	assert.True(t, got.Equal(tLow),
		"must return the success HWM (%v), not the failed run's started_at (%v); got %v", tLow, tHigh, got)
}

// TestLastSuccessfulHWM_EmptyHistory: no successful event ever → caller
// must get ent.NotFoundError so it can fall back to the "one month ago"
// boot cursor in cmd/sync.go.
func TestLastSuccessfulHWM_EmptyHistory(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	svc := pkgsync.NewService(nil, client, sqlDB, &storage.FakeStorer{}, nil)

	_, err := svc.LastSuccessfulHWM(ctx, syncevent.ResourceLookup)
	require.Error(t, err)
	assert.True(t, ent.IsNotFound(err), "expected NotFound, got %T: %v", err, err)
}

// TestLastSuccessfulHWM_ExcludesNullHWM is the §8 zero-record defense:
// even if some path writes status=success with hwm=NULL (it shouldn't —
// the CLI carries-forward — but defense in depth), the read query
// excludes it so the prior real HWM stays in effect.
func TestLastSuccessfulHWM_ExcludesNullHWM(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()

	tReal := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	tNull := tReal.Add(30 * time.Minute) // newer started_at

	seedEvent(t, client, ctx, syncevent.ResourceLookup, syncevent.StatusSuccess, tReal, &tReal)
	seedEvent(t, client, ctx, syncevent.ResourceLookup, syncevent.StatusSuccess, tNull, nil) // status=success but hwm=NULL

	svc := pkgsync.NewService(nil, client, sqlDB, &storage.FakeStorer{}, nil)

	got, err := svc.LastSuccessfulHWM(ctx, syncevent.ResourceLookup)
	require.NoError(t, err)
	assert.True(t, got.Equal(tReal),
		"NULL-hwm success row must be excluded; expected %v got %v", tReal, got)
}

// TestSweepStaleRunningEvents marks running events older than the
// threshold as failed without writing high_water_mark, leaves fresh
// ones alone, and is idempotent.
func TestSweepStaleRunningEvents(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()

	stale := seedEvent(t, client, ctx, syncevent.ResourceLookup, syncevent.StatusRunning, time.Now().Add(-3*time.Hour), nil)
	fresh := seedEvent(t, client, ctx, syncevent.ResourceLookup, syncevent.StatusRunning, time.Now().Add(-30*time.Minute), nil)

	svc := pkgsync.NewService(nil, client, sqlDB, &storage.FakeStorer{}, nil)

	n, err := svc.SweepStaleRunningEvents(ctx, 2*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "exactly the stale event should be swept")

	staleAfter := client.SyncEvent.GetX(ctx, stale.ID)
	freshAfter := client.SyncEvent.GetX(ctx, fresh.ID)
	assert.Equal(t, syncevent.StatusFailed, staleAfter.Status)
	require.NotNil(t, staleAfter.EndedAt)
	assert.Nil(t, staleAfter.HighWaterMark, "swept runs MUST NOT advance the cursor — §7")
	require.NotNil(t, staleAfter.ErrorSummary)
	assert.Contains(t, *staleAfter.ErrorSummary, "stale")

	assert.Equal(t, syncevent.StatusRunning, freshAfter.Status, "fresh running event untouched")
	assert.Nil(t, freshAfter.EndedAt)

	// Idempotent: a second sweep does nothing more.
	n2, err := svc.SweepStaleRunningEvents(ctx, 2*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 0, n2)
}
