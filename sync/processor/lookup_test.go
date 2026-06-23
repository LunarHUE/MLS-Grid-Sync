package processor

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/lookup"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
)

func runLookupProcess(t *testing.T, client *ent.Client, ctx context.Context, raw *ent.RawOutput) error {
	t.Helper()
	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	p := NewLookupProcessor()
	if _, err := p.Process(ctx, tx, raw); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func insertLookupRaw(t *testing.T, client *ent.Client, ctx context.Context, syncEventID uuid.UUID, payload map[string]any, modifiedAt time.Time) *ent.RawOutput {
	t.Helper()
	key, _ := payload["LookupKey"].(string)
	require.NotEmpty(t, key)
	return client.RawOutput.Create().
		SetSyncEventID(syncEventID).
		SetResource(rawoutput.ResourceLookup).
		SetSourceKey(key).
		SetChangeType(rawoutput.ChangeTypeInsert).
		SetSourceModifiedAt(modifiedAt).
		SetPayload(mustJSON(t, payload)).
		SaveX(ctx)
}

func setupLookupTest(t *testing.T) (context.Context, *ent.Client, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	client := testutil.NewTestDB(t)
	src := client.SourceSystem.Create().SetID("test-src").SetSourceSystemName("test").SaveX(ctx)
	ev := client.SyncEvent.Create().
		SetSourceSystemID(src.ID).
		SetResource("lookup").
		SetRunType("sync").
		SetProcessorVersion("test").
		SetStartedAt(time.Now()).
		SaveX(ctx)
	return ctx, client, ev.ID
}

func TestParseLookup_Requirements(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{"missing LookupKey", `{"ModificationTimestamp":"2026-01-15T12:00:00Z","LookupName":"X","LookupValue":"Y"}`, "LookupKey"},
		{"missing ModificationTimestamp", `{"LookupKey":"K","LookupName":"X","LookupValue":"Y"}`, "ModificationTimestamp"},
		{"missing LookupName", `{"LookupKey":"K","ModificationTimestamp":"2026-01-15T12:00:00Z","LookupValue":"Y"}`, "LookupName"},
		{"missing LookupValue", `{"LookupKey":"K","ModificationTimestamp":"2026-01-15T12:00:00Z","LookupName":"X"}`, "LookupValue"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseLookup([]byte(tc.payload))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestLookupProcess_UpsertNewValue(t *testing.T) {
	ctx, client, evID := setupLookupTest(t)

	ts1 := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, runLookupProcess(t, client, ctx, insertLookupRaw(t, client, ctx, evID, map[string]any{
		"LookupKey":             "K1",
		"ModificationTimestamp": ts1.Format(time.RFC3339),
		"LookupName":            "Appliances",
		"LookupValue":           "Dishwasher",
	}, ts1)))

	l, err := client.Lookup.Query().Where(lookup.IDEQ("K1")).Only(ctx)
	require.NoError(t, err)
	assert.Equal(t, "Appliances", l.LookupName)
	assert.Equal(t, "Dishwasher", l.LookupValue)

	// Same key, different value → one row, updated value, no version rows
	// anywhere (Lookup has no version table).
	ts2 := ts1.Add(time.Hour)
	require.NoError(t, runLookupProcess(t, client, ctx, insertLookupRaw(t, client, ctx, evID, map[string]any{
		"LookupKey":             "K1",
		"ModificationTimestamp": ts2.Format(time.RFC3339),
		"LookupName":            "Appliances",
		"LookupValue":           "Garbage Disposal",
	}, ts2)))

	count := client.Lookup.Query().Where(lookup.IDEQ("K1")).CountX(ctx)
	assert.Equal(t, 1, count, "still one row after upsert")
	l2, err := client.Lookup.Query().Where(lookup.IDEQ("K1")).Only(ctx)
	require.NoError(t, err)
	assert.Equal(t, "Garbage Disposal", l2.LookupValue)
}

func TestLookupProcess_MlgCanViewFalseDeletes(t *testing.T) {
	ctx, client, evID := setupLookupTest(t)

	ts1 := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, runLookupProcess(t, client, ctx, insertLookupRaw(t, client, ctx, evID, map[string]any{
		"LookupKey":             "K1",
		"ModificationTimestamp": ts1.Format(time.RFC3339),
		"LookupName":            "Appliances",
		"LookupValue":           "Old",
	}, ts1)))

	ts2 := ts1.Add(time.Hour)
	require.NoError(t, runLookupProcess(t, client, ctx, insertLookupRaw(t, client, ctx, evID, map[string]any{
		"LookupKey":             "K1",
		"ModificationTimestamp": ts2.Format(time.RFC3339),
		"LookupName":            "Appliances",
		"LookupValue":           "Old",
		"MlgCanView":            false,
	}, ts2)))

	count := client.Lookup.Query().Where(lookup.IDEQ("K1")).CountX(ctx)
	assert.Equal(t, 0, count, "Lookup tombstone deletes the row outright")
}
