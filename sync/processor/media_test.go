package processor

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	entmedia "github.com/LunarHUE/MLS-Grid-Sync/ent/media"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/mediaversion"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
)

func runMediaProcess(t *testing.T, client *ent.Client, ctx context.Context, raw *ent.RawOutput) error {
	t.Helper()
	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	p := NewMediaProcessor()
	if _, err := p.Process(ctx, tx, raw); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func insertMediaRaw(t *testing.T, client *ent.Client, ctx context.Context, syncEventID uuid.UUID, payload map[string]any, modifiedAt time.Time) *ent.RawOutput {
	t.Helper()
	key, _ := payload["MediaKey"].(string)
	require.NotEmpty(t, key)
	return client.RawOutput.Create().
		SetSyncEventID(syncEventID).
		SetResource(rawoutput.ResourceMedia).
		SetSourceKey(key).
		SetChangeType(rawoutput.ChangeTypeInsert).
		SetSourceModifiedAt(modifiedAt).
		SetPayload(payload).
		SaveX(ctx)
}

func setupMediaTest(t *testing.T) (context.Context, *ent.Client, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	client := testutil.NewTestDB(t)
	src := client.SourceSystem.Create().SetID("test-src").SetSourceSystemName("test").SaveX(ctx)
	ev := client.SyncEvent.Create().
		SetSourceSystemID(src.ID).
		SetResource("media").
		SetRunType("sync").
		SetProcessorVersion("test").
		SetStartedAt(time.Now()).
		SaveX(ctx)
	return ctx, client, ev.ID
}

func TestMediaProcess_FreshInsert(t *testing.T) {
	// Media has no FK to Property — polymorphic resource_record_key. No
	// parent seeding required.
	ctx, client, evID := setupMediaTest(t)

	ts := time.Now().UTC().Truncate(time.Second)
	raw := insertMediaRaw(t, client, ctx, evID, map[string]any{
		"MediaKey":              "M-1",
		"ResourceName":          "Property",
		"ResourceRecordKey":     "LK-1",
		"MediaURL":              "https://cdn.example/photo.jpg",
		"Order":                 1,
	}, ts)
	require.NoError(t, runMediaProcess(t, client, ctx, raw))

	m, err := client.Media.Query().Where(entmedia.IDEQ("M-1")).Only(ctx)
	require.NoError(t, err)
	assert.Equal(t, entmedia.ResourceTypeProperty, m.ResourceType)
	assert.Equal(t, "LK-1", m.ResourceRecordKey)
	require.NotNil(t, m.MediaURL)
	assert.Equal(t, "https://cdn.example/photo.jpg", *m.MediaURL)
}

func TestMediaProcess_UpdateWithDiff(t *testing.T) {
	ctx, client, evID := setupMediaTest(t)

	ts1 := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, runMediaProcess(t, client, ctx, insertMediaRaw(t, client, ctx, evID, map[string]any{
		"MediaKey":              "M-1",
		"ResourceName":          "Property",
		"ResourceRecordKey":     "LK-1",
		"MediaURL":              "https://cdn.example/v1.jpg",
	}, ts1)))

	ts2 := ts1.Add(time.Hour)
	require.NoError(t, runMediaProcess(t, client, ctx, insertMediaRaw(t, client, ctx, evID, map[string]any{
		"MediaKey":              "M-1",
		"ResourceName":          "Property",
		"ResourceRecordKey":     "LK-1",
		"MediaURL":              "https://cdn.example/v2.jpg",
	}, ts2)))

	versions := client.MediaVersion.Query().
		Where(mediaversion.MediaKeyEQ("M-1")).
		Order(ent.Asc(mediaversion.FieldValidFrom)).
		AllX(ctx)
	require.Len(t, versions, 2)
	diff, ok := versions[1].ChangedFields["media_url"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "https://cdn.example/v1.jpg", diff["old"])
	assert.Equal(t, "https://cdn.example/v2.jpg", diff["new"])
}

func TestMediaProcess_UpdateClearsField(t *testing.T) {
	ctx, client, evID := setupMediaTest(t)

	ts1 := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, runMediaProcess(t, client, ctx, insertMediaRaw(t, client, ctx, evID, map[string]any{
		"MediaKey":              "M-1",
		"ResourceName":          "Property",
		"ResourceRecordKey":     "LK-1",
		"MediaURL":              "https://cdn.example/v1.jpg",
	}, ts1)))

	ts2 := ts1.Add(time.Hour)
	require.NoError(t, runMediaProcess(t, client, ctx, insertMediaRaw(t, client, ctx, evID, map[string]any{
		"MediaKey":              "M-1",
		"ResourceName":          "Property",
		"ResourceRecordKey":     "LK-1",
	}, ts2)))

	m, err := client.Media.Query().Where(entmedia.IDEQ("M-1")).Only(ctx)
	require.NoError(t, err)
	assert.Nil(t, m.MediaURL, "entity clears MediaURL when omitted")

	versions := client.MediaVersion.Query().
		Where(mediaversion.MediaKeyEQ("M-1")).
		Order(ent.Asc(mediaversion.FieldValidFrom)).
		AllX(ctx)
	require.Len(t, versions, 2)
	assert.Nil(t, versions[1].MediaURL)
}

func TestMediaProcess_AlreadyTombstonedIsNoop(t *testing.T) {
	ctx, client, evID := setupMediaTest(t)

	ts1 := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, runMediaProcess(t, client, ctx, insertMediaRaw(t, client, ctx, evID, map[string]any{
		"MediaKey":              "M-1",
		"ResourceName":          "Property",
		"ResourceRecordKey":     "LK-1",
	}, ts1)))

	ts2 := ts1.Add(time.Hour)
	require.NoError(t, runMediaProcess(t, client, ctx, insertMediaRaw(t, client, ctx, evID, map[string]any{
		"MediaKey":              "M-1",
		"ResourceName":          "Property",
		"ResourceRecordKey":     "LK-1",
		"MlgCanView":            false,
	}, ts2)))

	ts3 := ts2.Add(time.Hour)
	require.NoError(t, runMediaProcess(t, client, ctx, insertMediaRaw(t, client, ctx, evID, map[string]any{
		"MediaKey":              "M-1",
		"ResourceName":          "Property",
		"ResourceRecordKey":     "LK-1",
		"MlgCanView":            false,
	}, ts3)))

	versions := client.MediaVersion.Query().Where(mediaversion.MediaKeyEQ("M-1")).AllX(ctx)
	require.Len(t, versions, 2)
}
