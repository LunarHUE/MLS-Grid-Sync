package sync_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/lookup"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
	"github.com/LunarHUE/MLS-Grid-Sync/mls"
	"github.com/LunarHUE/MLS-Grid-Sync/storage"
	pkgsync "github.com/LunarHUE/MLS-Grid-Sync/sync"
	"github.com/LunarHUE/MLS-Grid-Sync/sync/processor"
)

// skipFetcher simulates MLS Grid's $skip offset paging over a fixed, ordered
// corpus: it parses $skip from the request URL and returns up to pageSize
// records starting there (mirroring server-side $orderby + $top + $skip). It is
// concurrency-safe so the concurrent fetcher can hammer it from many goroutines
// under -race. callCount tallies total requests (including speculative empties
// past the end) for over-fetch assertions.
type skipFetcher struct {
	records  []json.RawMessage
	pageSize int

	mu        sync.Mutex
	callCount int
}

func (f *skipFetcher) FetchPage(_ context.Context, pageURL string) (*mls.ODataResponse, error) {
	f.mu.Lock()
	f.callCount++
	f.mu.Unlock()

	skip := 0
	if u, err := url.Parse(pageURL); err == nil {
		if v := u.Query().Get("$skip"); v != "" {
			skip, _ = strconv.Atoi(v)
		}
	}

	start := skip
	if start > len(f.records) {
		start = len(f.records)
	}
	end := start + f.pageSize
	if end > len(f.records) {
		end = len(f.records)
	}
	page := append([]json.RawMessage(nil), f.records[start:end]...)
	resp := &mls.ODataResponse{Value: page}
	// Also wire NextLink (carrying the next $skip) so the SEQUENTIAL nextLink
	// path — used when fetch_concurrency<=1 — can walk the same corpus. The
	// concurrent path ignores NextLink and builds its own $skip URLs.
	if end < len(f.records) {
		resp.NextLink = fmt.Sprintf("https://api.example/v2/Lookup?$skip=%d", end)
	}
	return resp, nil
}

func (f *skipFetcher) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.callCount
}

// makeLookupCorpus builds n Lookup records with strictly ascending keys and
// ModificationTimestamps, so a skip-paged walk over the ordered corpus is
// deterministic. Returns the records and the max timestamp (expected hwm).
func makeLookupCorpus(t *testing.T, n int) ([]json.RawMessage, time.Time) {
	t.Helper()
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Duration(n) * time.Second)
	recs := make([]json.RawMessage, 0, n)
	var maxAt time.Time
	for i := range n {
		at := base.Add(time.Duration(i) * time.Second)
		if at.After(maxAt) {
			maxAt = at
		}
		recs = append(recs, rawRecords(t, map[string]any{
			"LookupKey":             fmt.Sprintf("k%06d", i),
			"LookupName":            "PropertyType",
			"LookupValue":           fmt.Sprintf("v%06d", i),
			"ModificationTimestamp": at.Format(time.RFC3339),
		})...)
	}
	return recs, maxAt
}

// runConcurrentInitLookups runs a Lookup init through the concurrent skip-paged
// fetcher with the real Lookup processor on a fresh DB, returning the typed
// lookup keys in id order plus the stamped hwm. workers>1 exercises the
// concurrent path; workers<=1 falls back to sequential nextLink paging.
func runConcurrentInitLookups(t *testing.T, recs []json.RawMessage, workers int) ([]string, *time.Time) {
	t.Helper()
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	srcID := seedSourceSystem(t, client, ctx)

	fetcher := &skipFetcher{records: recs, pageSize: mls.PageSize}
	proc := processor.New(client, sqlDB, processor.NewLookupProcessor())
	svc := pkgsync.NewService(fetcher, client, sqlDB, &storage.FakeStorer{}, proc).
		WithFetchConcurrency(workers)

	require.NoError(t, svc.RunInitial(ctx, srcID, "https://api.example/v2", "actris", rawoutput.ResourceLookup))

	rows, err := client.Lookup.Query().Order(ent.Asc(lookup.FieldID)).All(ctx)
	require.NoError(t, err)
	keys := make([]string, len(rows))
	for i, r := range rows {
		keys[i] = r.ID
	}
	return keys, onlyEvent(t, client, ctx).HighWaterMark
}

// expectedKeys is the id-ordered key set makeLookupCorpus produces.
func expectedKeys(n int) []string {
	keys := make([]string, n)
	for i := range n {
		keys[i] = fmt.Sprintf("k%06d", i)
	}
	return keys
}

// TestConcurrentFetch_MatchesSequential is the load-bearing equivalence check:
// a corpus spanning a full page + a partial tail, fetched concurrently
// (out-of-order saves), lands the exact same typed rows as the sequential path.
func TestConcurrentFetch_MatchesSequential(t *testing.T) {
	recs, maxAt := makeLookupCorpus(t, mls.PageSize+7) // 2 pages: full + 7

	concurrent, hwm := runConcurrentInitLookups(t, recs, 4)
	require.NotNil(t, hwm, "success run must stamp hwm")
	assert.True(t, hwm.Equal(maxAt), "hwm must be the max source_modified_at: want %v got %v", maxAt, *hwm)
	assert.Equal(t, expectedKeys(mls.PageSize+7), concurrent,
		"concurrent skip-paged init must type every key exactly once, in order")
}

// TestConcurrentFetch_ExactMultipleBoundary covers the tricky end-detection
// case: when the corpus is an exact multiple of PageSize, no page is partial —
// the end is only discovered by a page returning zero records. All records must
// still land with none duplicated.
func TestConcurrentFetch_ExactMultipleBoundary(t *testing.T) {
	recs, _ := makeLookupCorpus(t, mls.PageSize) // exactly one full page

	concurrent, _ := runConcurrentInitLookups(t, recs, 4)
	assert.Equal(t, expectedKeys(mls.PageSize), concurrent,
		"exact-multiple corpus must be fully fetched; end detected via the trailing empty page")
}

// TestConcurrentFetch_SinglePartialPage covers the smallest case: a corpus
// smaller than one page returns short on the first fetch, so the boundary is
// hit immediately and the over-fetch is at most workers-1 empty requests.
func TestConcurrentFetch_SinglePartialPage(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()
	srcID := seedSourceSystem(t, client, ctx)

	recs, _ := makeLookupCorpus(t, 5)
	fetcher := &skipFetcher{records: recs, pageSize: mls.PageSize}
	proc := processor.New(client, sqlDB, processor.NewLookupProcessor())
	svc := pkgsync.NewService(fetcher, client, sqlDB, &storage.FakeStorer{}, proc).
		WithFetchConcurrency(4)

	require.NoError(t, svc.RunInitial(ctx, srcID, "https://api.example/v2", "actris", rawoutput.ResourceLookup))

	cnt, err := client.Lookup.Query().Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 5, cnt)
	// page 0 (data) + at most workers-1 speculative empties.
	assert.LessOrEqual(t, fetcher.calls(), 4, "over-fetch past the boundary must stay bounded by the worker count")
}

// TestConcurrentFetch_DisabledMatchesEnabled pins that toggling the optimization
// off (fetch_concurrency=1) yields the identical end state — the kill-switch is
// behavior-preserving.
func TestConcurrentFetch_DisabledMatchesEnabled(t *testing.T) {
	recs, _ := makeLookupCorpus(t, mls.PageSize+3)

	enabled, _ := runConcurrentInitLookups(t, recs, 4)
	disabled, _ := runConcurrentInitLookups(t, recs, 1)
	assert.Equal(t, expectedKeys(mls.PageSize+3), disabled, "disabled path must type all keys in order")
	assert.Equal(t, disabled, enabled, "enabling concurrency must not change the end state")
}
