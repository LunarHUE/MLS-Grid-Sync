package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/rawoutput"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/syncevent"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/internal/testutil"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/mls"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/storage"
)

// TestChunkSize_UnderPgParamCap is the cheap permanent guard: Postgres
// caps a prepared statement at 65,535 bind parameters (uint16 in the
// extended-protocol wire format). If someone adds a column to raw_output
// and bumps rawOutputInsertColumns past 16 without lowering
// maxInsertChunkRows, this test fails before production does. Pure unit,
// no DB.
func TestChunkSize_UnderPgParamCap(t *testing.T) {
	const pgParamCap = 65535
	product := maxInsertChunkRows * rawOutputInsertColumns
	require.Less(t, product, pgParamCap,
		"maxInsertChunkRows (%d) × rawOutputInsertColumns (%d) = %d must stay under Postgres' %d-parameter cap; bump down maxInsertChunkRows if a column was added",
		maxInsertChunkRows, rawOutputInsertColumns, product, pgParamCap)
}

// withChunkSize swaps maxInsertChunkRows for the duration of a test so
// the chunk loop can be exercised without constructing 4000+ rows.
func withChunkSize(t *testing.T, n int) {
	t.Helper()
	prev := maxInsertChunkRows
	maxInsertChunkRows = n
	t.Cleanup(func() { maxInsertChunkRows = prev })
}

func mkSyncEvent(t *testing.T, client *ent.Client, ctx context.Context, resource syncevent.Resource) uuid.UUID {
	t.Helper()
	src, err := client.SourceSystem.Create().
		SetID("mlsgrid-test").
		SetSourceSystemName("MLS Grid (test)").
		Save(ctx)
	require.NoError(t, err)
	ev, err := client.SyncEvent.Create().
		SetSourceSystemID(src.ID).
		SetResource(resource).
		SetRunType(syncevent.RunTypeSync).
		SetProcessorVersion("test").
		SetStartedAt(time.Now().UTC()).
		Save(ctx)
	require.NoError(t, err)
	return ev.ID
}

// TestSaveToRawOutput_MultiChunkLandsCompletely shrinks the chunk size to
// 2 and pushes 5 Lookup records through saveToRawOutput, forcing the
// chunk loop to run three times (2+2+1). All five rows must persist and
// the page's HWM must equal the max ModificationTimestamp across them.
// Pins: the chunk loop and the transaction commit cooperate to deliver
// the same end-state as a single oversized INSERT would have.
func TestSaveToRawOutput_MultiChunkLandsCompletely(t *testing.T) {
	withChunkSize(t, 2)

	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	syncEventID := mkSyncEvent(t, client, ctx, syncevent.ResourceLookup)

	svc := NewService(nil /* fetcher unused */, client, sqlDB, &storage.FakeStorer{}, nil)

	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	const n = 5
	records := make([]json.RawMessage, 0, n)
	for i := 0; i < n; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		b, err := json.Marshal(map[string]any{
			"LookupKey":             fmt.Sprintf("chunk-key-%d", i),
			"LookupName":            "Region",
			"LookupValue":           fmt.Sprintf("v-%d", i),
			"ModificationTimestamp": ts.Format(time.RFC3339),
		})
		require.NoError(t, err)
		records = append(records, b)
	}

	hwm, _, err := svc.saveToRawOutput(ctx, syncEventID, mls.ResourceLookup, records)
	require.NoError(t, err, "chunk loop must commit all chunks cleanly")

	wantHWM := base.Add(time.Duration(n-1) * time.Minute)
	assert.True(t, hwm.Equal(wantHWM),
		"HWM must accumulate across chunks; want %v got %v", wantHWM, hwm)

	count, err := client.RawOutput.Query().
		Where(rawoutput.ResourceEQ(rawoutput.ResourceLookup)).
		Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, n, count, "every row across every chunk must land")

	// Re-running the same page must dedup every row via the unique index
	// (ON CONFLICT DO NOTHING), regardless of chunking — chunking does not
	// perturb dedup semantics.
	hwm2, _, err := svc.saveToRawOutput(ctx, syncEventID, mls.ResourceLookup, records)
	require.NoError(t, err)
	assert.True(t, hwm2.IsZero(),
		"re-landing identical records must return zero HWM (every chunk returned no RETURNING rows)")

	countAfter, err := client.RawOutput.Query().
		Where(rawoutput.ResourceEQ(rawoutput.ResourceLookup)).
		Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, n, countAfter, "ON CONFLICT must drop the duplicate re-land")
}

// TestSaveToRawOutput_MidChunkFailureRollsBackPage is the load-bearing
// atomicity pin: when one chunk fails inside the page transaction, the
// WHOLE page rolls back — even chunks that succeeded before it. The
// pre-chunking single-statement INSERT got this for free; the chunked
// path must preserve it via the explicit BeginTx/Commit wrap.
//
// Trigger: call bulkInsertChunk twice manually inside one tx — chunk 1
// with a valid sync_event_id, chunk 2 with a random UUID that has no
// matching sync_event row (FK violation). After the second call errors,
// rolling back the tx must wipe out chunk 1's row as well.
//
// We drive bulkInsertChunk directly (not through saveToRawOutput) only
// because building a poisoned record via the public input API requires a
// constraint-violating payload that buildPreparedRows would never
// produce. The behavior asserted — that chunk 1's rows do not survive a
// chunk 2 failure — is exactly the property saveToRawOutput depends on.
func TestSaveToRawOutput_MidChunkFailureRollsBackPage(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	syncEventID := mkSyncEvent(t, client, ctx, syncevent.ResourceLookup)

	tx, err := sqlDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()
	ts := now.Add(-time.Hour).Truncate(time.Second)

	chunk1 := []preparedRow{{
		resource:   rawoutput.ResourceLookup,
		sourceKey:  "survivor-1",
		modifiedAt: ts,
		payload:    []byte(`{"LookupKey":"survivor-1"}`),
	}}
	_, err = bulkInsertChunk(ctx, tx, chunk1, syncEventID, now, rawoutput.ResourceLookup)
	require.NoError(t, err, "chunk 1 must insert cleanly inside the tx")

	// FK violation: a sync_event_id that doesn't exist anywhere.
	badSyncEventID := uuid.New()
	chunk2 := []preparedRow{{
		resource:   rawoutput.ResourceLookup,
		sourceKey:  "poison-1",
		modifiedAt: ts.Add(time.Minute),
		payload:    []byte(`{"LookupKey":"poison-1"}`),
	}}
	_, err = bulkInsertChunk(ctx, tx, chunk2, badSyncEventID, now, rawoutput.ResourceLookup)
	require.Error(t, err, "chunk 2 must fail on the FK violation")

	// saveToRawOutput's deferred Rollback fires on the wrapping error path;
	// simulate that here.
	require.NoError(t, tx.Rollback())

	// Chunk 1's row MUST NOT persist — the whole-page atomicity property.
	survivor, err := client.RawOutput.Query().
		Where(rawoutput.SourceKey("survivor-1")).
		Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, survivor,
		"chunk 1 must roll back when chunk 2 fails — the one-tx-per-page property the BeginTx wrap exists to preserve")
	poison, err := client.RawOutput.Query().
		Where(rawoutput.SourceKey("poison-1")).
		Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, poison, "the poison row must not be there either")
}
