package sync_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/lookup"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/syncevent"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
	"github.com/LunarHUE/MLS-Grid-Sync/mls"
	"github.com/LunarHUE/MLS-Grid-Sync/storage"
	pkgsync "github.com/LunarHUE/MLS-Grid-Sync/sync"
	"github.com/LunarHUE/MLS-Grid-Sync/sync/processor"
)

// threePagesOfLookups returns a mock fetcher serving 3 pages of Lookup records
// (2 + 2 + 1) with NextLinks wired so paginate walks all three, plus the max
// ModificationTimestamp so callers can assert the stamped hwm.
func threePagesOfLookups(t *testing.T) (*mockFetcher, time.Time) {
	t.Helper()
	modAt := time.Now().UTC().Truncate(time.Second)
	mk := func(k string) map[string]any {
		return map[string]any{
			"LookupKey":             k,
			"LookupName":            "PropertyType",
			"LookupValue":           k,
			"ModificationTimestamp": modAt.Format(time.RFC3339),
		}
	}
	f := &mockFetcher{pages: []*mls.ODataResponse{
		{Value: rawRecords(t, mk("k1"), mk("k2")), NextLink: "next1"},
		{Value: rawRecords(t, mk("k3"), mk("k4")), NextLink: "next2"},
		{Value: rawRecords(t, mk("k5")), NextLink: ""},
	}}
	return f, modAt
}

// runInitCollectKeys runs a full Lookup init (with the real Lookup processor)
// under the given pipeline mode on a fresh DB and returns the typed lookup keys
// in id order. Used to assert the pipelined and sequential paths converge to
// the same end state.
func runInitCollectKeys(t *testing.T, pipeline bool) []string {
	t.Helper()
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	srcID := seedSourceSystem(t, client, ctx)

	fetcher, _ := threePagesOfLookups(t)
	proc := processor.New(client, sqlDB, processor.NewLookupProcessor())
	svc := pkgsync.NewService(fetcher, client, sqlDB, &storage.FakeStorer{}, proc).
		WithInitPipeline(pipeline)

	require.NoError(t, svc.RunInitial(ctx, srcID, "https://api.example/v2", "actris", rawoutput.ResourceLookup))

	rows, err := client.Lookup.Query().Order(ent.Asc(lookup.FieldID)).All(ctx)
	require.NoError(t, err)
	keys := make([]string, len(rows))
	for i, r := range rows {
		keys[i] = r.ID
	}
	return keys
}

// TestRunInitial_Pipelined_ProcessesAllPages is the core correctness check:
// the pipelined consumer drains every fetched record into typed rows and the
// run stamps success + the right hwm.
func TestRunInitial_Pipelined_ProcessesAllPages(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	srcID := seedSourceSystem(t, client, ctx)

	fetcher, modAt := threePagesOfLookups(t)
	proc := processor.New(client, sqlDB, processor.NewLookupProcessor())
	svc := pkgsync.NewService(fetcher, client, sqlDB, &storage.FakeStorer{}, proc).
		WithInitPipeline(true)

	require.NoError(t, svc.RunInitial(ctx, srcID, "https://api.example/v2", "actris", rawoutput.ResourceLookup))

	cnt, err := client.Lookup.Query().Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 5, cnt, "pipelined init must process every fetched record into typed rows")

	ev := onlyEvent(t, client, ctx)
	assert.Equal(t, syncevent.StatusSuccess, ev.Status)
	require.NotNil(t, ev.HighWaterMark, "success run must stamp hwm")
	assert.True(t, ev.HighWaterMark.Equal(modAt), "hwm must be the max source_modified_at: want %v got %v", modAt, *ev.HighWaterMark)
}

// TestRunInitial_Pipelined_MatchesSequential pins behavior-equivalence: the
// pipelined path and the --no-pipeline path land identical typed rows.
func TestRunInitial_Pipelined_MatchesSequential(t *testing.T) {
	pipelined := runInitCollectKeys(t, true)
	sequential := runInitCollectKeys(t, false)
	assert.Equal(t, []string{"k1", "k2", "k3", "k4", "k5"}, sequential, "sequential baseline must type all keys in order")
	assert.Equal(t, sequential, pipelined, "pipelined init must produce the same end state as sequential")
}

// blockingFetcher releases page N+1 only after the consumer has drained page N
// (via the drained channel). In the SEQUENTIAL path the consumer never runs
// until pagination finishes, so the second FetchPage would block forever —
// completion therefore proves fetch and process actually overlap.
type blockingFetcher struct {
	pages   []*mls.ODataResponse
	drained chan struct{}
	idx     int
}

func (f *blockingFetcher) FetchPage(ctx context.Context, _ string) (*mls.ODataResponse, error) {
	if f.idx > 0 {
		select {
		case <-f.drained:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.idx >= len(f.pages) {
		return &mls.ODataResponse{}, nil
	}
	p := f.pages[f.idx]
	f.idx++
	return p, nil
}

// signalingProcessor pings drained on each streaming-drain wake so the
// blockingFetcher can release the next page. The buffered channel decouples
// timing so no signal is dropped and no extra send deadlocks.
type signalingProcessor struct {
	drained chan struct{}
}

func (p *signalingProcessor) RunPass(_ context.Context, _ rawoutput.Resource) error { return nil }

func (p *signalingProcessor) RunPassNoFinalize(_ context.Context, _ rawoutput.Resource) error {
	p.drained <- struct{}{}
	return nil
}

// TestRunInitial_Pipelined_FetchAndProcessOverlap proves the producer/consumer
// interleave: the fetcher gates each page on the consumer draining the prior
// one, a handshake that can only complete if processing runs concurrently with
// fetching. A watchdog catches a regression to sequential behavior (deadlock).
func TestRunInitial_Pipelined_FetchAndProcessOverlap(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	srcID := seedSourceSystem(t, client, ctx)

	_, modAt := threePagesOfLookups(t)
	mk := func(k string) map[string]any {
		return map[string]any{"LookupKey": k, "LookupName": "PropertyType", "LookupValue": k, "ModificationTimestamp": modAt.Format(time.RFC3339)}
	}
	fetcher := &blockingFetcher{
		drained: make(chan struct{}, 8),
		pages: []*mls.ODataResponse{
			{Value: rawRecords(t, mk("k1")), NextLink: "next1"},
			{Value: rawRecords(t, mk("k2")), NextLink: "next2"},
			{Value: rawRecords(t, mk("k3")), NextLink: ""},
		},
	}
	proc := &signalingProcessor{drained: fetcher.drained}
	svc := pkgsync.NewService(fetcher, client, sqlDB, &storage.FakeStorer{}, proc).
		WithInitPipeline(true)

	done := make(chan error, 1)
	go func() {
		done <- svc.RunInitial(ctx, srcID, "https://api.example/v2", "actris", rawoutput.ResourceLookup)
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("pipelined init did not complete — fetch and process are not overlapping (regressed to sequential)")
	}
}

// TestRunInitial_Pipelined_ConsumerErrorPropagates: a processor error in the
// consumer must cancel the producer, surface as the RunInitial error, and
// stamp the sync_event failed with NULL hwm (no silent data loss).
func TestRunInitial_Pipelined_ConsumerErrorPropagates(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	srcID := seedSourceSystem(t, client, ctx)

	fetcher, _ := threePagesOfLookups(t)
	svc := pkgsync.NewService(fetcher, client, sqlDB, &storage.FakeStorer{},
		&fakeProcessor{err: errors.New("processor halt")}).
		WithInitPipeline(true)

	err := svc.RunInitial(ctx, srcID, "https://api.example/v2", "actris", rawoutput.ResourceLookup)
	require.Error(t, err, "consumer processor failure must propagate")
	assert.Contains(t, err.Error(), "processor halt")

	ev := onlyEvent(t, client, ctx)
	assert.Equal(t, syncevent.StatusFailed, ev.Status, "consumer halt must stamp failed")
	assert.Nil(t, ev.HighWaterMark, "failed run must NOT stamp hwm")
}
