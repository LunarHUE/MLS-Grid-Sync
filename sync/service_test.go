package sync_test

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
	pkgsync "github.com/lunarhue/website-highpointe/packages/mls-grid-sync/sync"
)

// newSyncEvent inserts a SourceSystem + SyncEvent and returns the event's UUID,
// suitable for passing as syncEventID to SyncResource/InitialSync. raw_output has
// a required FK back to sync_event, so any test that actually inserts a raw row
// needs a real parent.
func newSyncEvent(t *testing.T, client *ent.Client, ctx context.Context, resource syncevent.Resource) uuid.UUID {
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
		SetStartedAt(time.Now()).
		Save(ctx)
	require.NoError(t, err)
	return ev.ID
}

// --- mock MLS client ---

type mockFetcher struct {
	pages []*mls.ODataResponse
	err   error
	idx   int
}

func (m *mockFetcher) FetchPage(_ context.Context, _ string) (*mls.ODataResponse, error) {
	if m.err != nil {
		return nil, m.err
	}
	if m.idx >= len(m.pages) {
		return &mls.ODataResponse{}, nil
	}
	p := m.pages[m.idx]
	m.idx++
	return p, nil
}

func rawRecords(t *testing.T, records ...map[string]any) []json.RawMessage {
	t.Helper()
	out := make([]json.RawMessage, len(records))
	for i, r := range records {
		b, err := json.Marshal(r)
		require.NoError(t, err)
		out[i] = b
	}
	return out
}

// --- tests ---

func TestSyncResource_SavesRawOutput(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	records := rawRecords(t,
		map[string]any{"LookupKey": "key-1", "LookupName": "PropertyType", "LookupValue": "Residential", "ModificationTimestamp": now.Format(time.RFC3339)},
		map[string]any{"LookupKey": "key-2", "LookupName": "PropertyType", "LookupValue": "Commercial", "ModificationTimestamp": now.Format(time.RFC3339)},
	)
	fetcher := &mockFetcher{pages: []*mls.ODataResponse{
		{Value: records, NextLink: ""},
	}}

	syncEventID := newSyncEvent(t, client, ctx, syncevent.ResourceLookup)
	svc := pkgsync.NewService(fetcher, client, sqlDB, &storage.FakeStorer{}, nil)
	_, err := svc.SyncResource(ctx, syncEventID, "https://api.mlsgrid.com/v2", "actris", mls.ResourceLookup, time.Now().Add(-time.Hour))
	require.NoError(t, err)

	saved, err := client.RawOutput.Query().All(ctx)
	require.NoError(t, err)
	assert.Len(t, saved, 2)

	assert.Equal(t, rawoutput.ResourceLookup, saved[0].Resource)
	assert.Equal(t, "key-1", saved[0].SourceKey)
}

func TestSyncResource_MultiPage(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()

	page1 := rawRecords(t,
		map[string]any{"LookupKey": "key-1", "LookupName": "A", "LookupValue": "1"},
		map[string]any{"LookupKey": "key-2", "LookupName": "A", "LookupValue": "2"},
	)
	page2 := rawRecords(t,
		map[string]any{"LookupKey": "key-3", "LookupName": "A", "LookupValue": "3"},
	)
	fetcher := &mockFetcher{pages: []*mls.ODataResponse{
		{Value: page1, NextLink: "https://api.mlsgrid.com/v2/Lookup?$skip=1000"},
		{Value: page2, NextLink: ""},
	}}

	svc := pkgsync.NewService(fetcher, client, sqlDB, &storage.FakeStorer{}, nil)
	_, err := svc.SyncResource(ctx, newSyncEvent(t, client, ctx, syncevent.ResourceLookup), "https://api.mlsgrid.com/v2", "actris", mls.ResourceLookup, time.Now().Add(-time.Hour))
	require.NoError(t, err)

	count, err := client.RawOutput.Query().Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, count)
}

func TestSyncResource_EmptyResponse(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()

	fetcher := &mockFetcher{pages: []*mls.ODataResponse{
		{Value: nil, NextLink: ""},
	}}

	svc := pkgsync.NewService(fetcher, client, sqlDB, &storage.FakeStorer{}, nil)
	_, err := svc.SyncResource(ctx, uuid.New(), "https://api.mlsgrid.com/v2", "actris", mls.ResourceLookup, time.Now().Add(-time.Hour))
	require.NoError(t, err)

	count, err := client.RawOutput.Query().Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

func TestSyncResource_FetchError(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()

	fetcher := &mockFetcher{err: fmt.Errorf("connection refused")}

	svc := pkgsync.NewService(fetcher, client, sqlDB, &storage.FakeStorer{}, nil)
	_, err := svc.SyncResource(ctx, uuid.New(), "https://api.mlsgrid.com/v2", "actris", mls.ResourceLookup, time.Now().Add(-time.Hour))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection refused")
}

func TestSyncResource_MissingPrimaryKey(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()

	records := rawRecords(t,
		map[string]any{"LookupName": "PropertyType"}, // missing LookupKey
	)
	fetcher := &mockFetcher{pages: []*mls.ODataResponse{
		{Value: records, NextLink: ""},
	}}

	svc := pkgsync.NewService(fetcher, client, sqlDB, &storage.FakeStorer{}, nil)
	_, err := svc.SyncResource(ctx, uuid.New(), "https://api.mlsgrid.com/v2", "actris", mls.ResourceLookup, time.Now().Add(-time.Hour))
	require.Error(t, err)
}

func TestInitialSync_SavesRawOutput(t *testing.T) {
	client, sqlDB := testutil.NewTestDBWithSQL(t)
	ctx := context.Background()

	records := rawRecords(t,
		map[string]any{"MemberKey": "agent-001", "MemberFullName": "Jane Agent"},
	)
	fetcher := &mockFetcher{pages: []*mls.ODataResponse{
		{Value: records, NextLink: ""},
	}}

	svc := pkgsync.NewService(fetcher, client, sqlDB, &storage.FakeStorer{}, nil)
	_, err := svc.InitialSync(ctx, newSyncEvent(t, client, ctx, syncevent.ResourceMember), "https://api.mlsgrid.com/v2", "actris", mls.ResourceMember)
	require.NoError(t, err)

	saved, err := client.RawOutput.Query().All(ctx)
	require.NoError(t, err)
	require.Len(t, saved, 1)
	assert.Equal(t, rawoutput.ResourceMember, saved[0].Resource)
	assert.Equal(t, "agent-001", saved[0].SourceKey)
}
