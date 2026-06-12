package processor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/processorcursor"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
)

// --- fake ResourceProcessor ---

// fakeProcessor reports a configurable Outcome (or a synthetic error) per
// raw_output. outcomes is keyed by raw.ID; missing entries default to
// OutcomeInsert so existing tests keep their no-config behavior.
type fakeProcessor struct {
	mu        sync.Mutex
	calls     []uuid.UUID
	failOn    uuid.UUID
	failError error
	outcomes  map[uuid.UUID]Outcome
}

func (f *fakeProcessor) Resource() rawoutput.Resource { return rawoutput.ResourceLookup }

func (f *fakeProcessor) Process(_ context.Context, _ *ent.Tx, raw *ent.RawOutput) (Outcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOn != uuid.Nil && raw.ID == f.failOn {
		return OutcomeUnknown, f.failError
	}
	f.calls = append(f.calls, raw.ID)
	if o, ok := f.outcomes[raw.ID]; ok {
		return o, nil
	}
	return OutcomeInsert, nil
}

func (f *fakeProcessor) seen() []uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]uuid.UUID, len(f.calls))
	copy(out, f.calls)
	return out
}

// --- helpers ---

// seedRawOutputs inserts n raw_output rows for ResourceLookup (which is
// schema-shaped and cheap to seed). Returns the inserted ids in insertion
// order. Each row gets a unique source_key so the (resource, source_key,
// source_modified_at) index doesn't constrain us. Bound to a real
// sync_event so the FK is satisfied.
func seedRawOutputs(t *testing.T, client *ent.Client, ctx context.Context, n int) []uuid.UUID {
	t.Helper()

	src, err := client.SourceSystem.Create().
		SetID("test-src").
		SetSourceSystemName("test").
		Save(ctx)
	require.NoError(t, err)

	ev, err := client.SyncEvent.Create().
		SetSourceSystemID(src.ID).
		SetResource("lookup").
		SetRunType("sync").
		SetProcessorVersion("test").
		SetStartedAt(time.Now()).
		Save(ctx)
	require.NoError(t, err)

	ids := make([]uuid.UUID, n)
	for i := 0; i < n; i++ {
		// Yield between inserts so UUIDv7 timestamps advance monotonically.
		time.Sleep(time.Millisecond)
		row, err := client.RawOutput.Create().
			SetSyncEventID(ev.ID).
			SetResource(rawoutput.ResourceLookup).
			SetSourceKey(uuid.NewString()).
			SetChangeType(rawoutput.ChangeTypeInsert).
			SetSourceModifiedAt(time.Now().UTC()).
			SetPayload(map[string]any{"i": i}).
			Save(ctx)
		require.NoError(t, err)
		ids[i] = row.ID
	}
	return ids
}

// --- tests ---

func TestRunPass_NoProcessorRegistered_ReturnsErrNoProcessor(t *testing.T) {
	client, db := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	seedRawOutputs(t, client, ctx, 3)

	p := New(client, db) // no processors
	err := p.RunPass(ctx, rawoutput.ResourceLookup)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoProcessor)

	// No cursor created either — nothing to do.
	got, err := client.ProcessorCursor.Query().Count(ctx)
	require.NoError(t, err)
	assert.Zero(t, got)
}

func TestRunPass_FirstCall_CreatesCursorAndDrainsAll(t *testing.T) {
	client, db := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	ids := seedRawOutputs(t, client, ctx, 5)

	fake := &fakeProcessor{}
	p := New(client, db, fake).WithBatchSize(2)
	require.NoError(t, p.RunPass(ctx, rawoutput.ResourceLookup))

	// Every raw row visited, in insertion order.
	assert.Equal(t, ids, fake.seen())

	// Cursor advanced to the last id.
	cur, err := client.ProcessorCursor.Query().
		Where(processorcursor.ResourceEQ(processorcursor.ResourceLookup)).
		Only(ctx)
	require.NoError(t, err)
	require.NotNil(t, cur.LastRawOutputID)
	assert.Equal(t, ids[len(ids)-1], *cur.LastRawOutputID)
}

func TestRunPass_EmptyStream_TerminatesCleanly(t *testing.T) {
	client, db := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()

	fake := &fakeProcessor{}
	p := New(client, db, fake)
	require.NoError(t, p.RunPass(ctx, rawoutput.ResourceLookup))
	assert.Empty(t, fake.seen())

	// Cursor row was created even though nothing was processed.
	cur, err := client.ProcessorCursor.Query().
		Where(processorcursor.ResourceEQ(processorcursor.ResourceLookup)).
		Only(ctx)
	require.NoError(t, err)
	assert.Nil(t, cur.LastRawOutputID)
}

func TestRunPass_PoisonRecord_Halts_ResumesAfterFix(t *testing.T) {
	client, db := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	ids := seedRawOutputs(t, client, ctx, 4)

	// Fail on the 3rd row.
	fake := &fakeProcessor{failOn: ids[2], failError: errors.New("synthetic poison")}
	p := New(client, db, fake)

	err := p.RunPass(ctx, rawoutput.ResourceLookup)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "synthetic poison")

	// Only the first two should have been "seen" — and the cursor stopped at id[1].
	assert.Equal(t, ids[:2], fake.seen())
	cur, err := client.ProcessorCursor.Query().
		Where(processorcursor.ResourceEQ(processorcursor.ResourceLookup)).
		Only(ctx)
	require.NoError(t, err)
	require.NotNil(t, cur.LastRawOutputID)
	assert.Equal(t, ids[1], *cur.LastRawOutputID)

	// "Fix" the parser and re-run. Cursor resumes from id[2].
	fake.failOn = uuid.Nil
	require.NoError(t, p.RunPass(ctx, rawoutput.ResourceLookup))
	assert.Equal(t, ids, fake.seen(), "all rows visited eventually, in order, no duplicates")

	cur2, err := client.ProcessorCursor.Query().
		Where(processorcursor.ResourceEQ(processorcursor.ResourceLookup)).
		Only(ctx)
	require.NoError(t, err)
	require.NotNil(t, cur2.LastRawOutputID)
	assert.Equal(t, ids[len(ids)-1], *cur2.LastRawOutputID)
}

func TestRunPass_SecondCallWithNoNewRows_IsNoop(t *testing.T) {
	client, db := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	seedRawOutputs(t, client, ctx, 3)

	fake := &fakeProcessor{}
	p := New(client, db, fake)
	require.NoError(t, p.RunPass(ctx, rawoutput.ResourceLookup))
	first := len(fake.seen())

	// Second run: nothing new. seen() count stays the same.
	require.NoError(t, p.RunPass(ctx, rawoutput.ResourceLookup))
	assert.Equal(t, first, len(fake.seen()))
}

// TestRunPass_StatsSumInvariant is the load-bearing test for the
// Outcome-annotation contract: Processed must equal Inserts + Updates +
// Deletes + SkipStale + SkipNoDiff + SkipTombstoned. A future processor
// that returns an unannotated path (or an outcome that doesn't map onto
// the six counters) trips this invariant.
//
// Runs through runPassWithStats so the assertion sees the stats the
// production loop builds — not a parallel rebuild that could drift.
func TestRunPass_StatsSumInvariant(t *testing.T) {
	client, db := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	ids := seedRawOutputs(t, client, ctx, 12)

	// Assign each raw_output one of the six outcomes — mixed coverage of
	// every counter, plus a duplicate Insert at position 0 to confirm
	// the loop doesn't deduplicate.
	outcomes := map[uuid.UUID]Outcome{
		ids[0]:  OutcomeInsert,
		ids[1]:  OutcomeInsert,
		ids[2]:  OutcomeUpdate,
		ids[3]:  OutcomeUpdate,
		ids[4]:  OutcomeUpdate,
		ids[5]:  OutcomeDelete,
		ids[6]:  OutcomeSkipStale,
		ids[7]:  OutcomeSkipStale,
		ids[8]:  OutcomeSkipNoDiff,
		ids[9]:  OutcomeSkipNoDiff,
		ids[10]: OutcomeSkipNoDiff,
		ids[11]: OutcomeSkipTombstoned,
	}
	want := PassStats{
		Inserts:        2,
		Updates:        3,
		Deletes:        1,
		SkipStale:      2,
		SkipNoDiff:     3,
		SkipTombstoned: 1,
	}

	fake := &fakeProcessor{outcomes: outcomes}
	p := New(client, db, fake).WithBatchSize(4) // multi-batch to exercise progress logging
	stats, err := p.runPassWithStats(ctx, rawoutput.ResourceLookup)
	require.NoError(t, err)

	assert.Equal(t, len(ids), stats.Processed, "Processed must equal records visited")
	assert.Equal(t, want.Inserts, stats.Inserts)
	assert.Equal(t, want.Updates, stats.Updates)
	assert.Equal(t, want.Deletes, stats.Deletes)
	assert.Equal(t, want.SkipStale, stats.SkipStale)
	assert.Equal(t, want.SkipNoDiff, stats.SkipNoDiff)
	assert.Equal(t, want.SkipTombstoned, stats.SkipTombstoned)

	sum := stats.Inserts + stats.Updates + stats.Deletes +
		stats.SkipStale + stats.SkipNoDiff + stats.SkipTombstoned
	assert.Equal(t, stats.Processed, sum,
		"sum-of-six invariant: a drift here means a processor returned an unannotated path")
}

// TestOutcomeString documents the operator-facing names. These appear in
// the per-record DEBUG line, so they're part of the log contract.
func TestOutcomeString(t *testing.T) {
	cases := []struct {
		o    Outcome
		want string
	}{
		{OutcomeInsert, "insert"},
		{OutcomeUpdate, "update"},
		{OutcomeDelete, "delete"},
		{OutcomeSkipStale, "skip-stale"},
		{OutcomeSkipNoDiff, "skip-no-diff"},
		{OutcomeSkipTombstoned, "skip-tombstoned"},
		{OutcomeUnknown, "unknown"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, c.o.String())
	}
}
