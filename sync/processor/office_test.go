package processor

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/office"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/officeversion"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
)

func runOfficeProcess(t *testing.T, client *ent.Client, ctx context.Context, raw *ent.RawOutput) error {
	t.Helper()
	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	p := NewOfficeProcessor()
	if _, err := p.Process(ctx, tx, raw); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func insertOfficeRaw(t *testing.T, client *ent.Client, ctx context.Context, syncEventID uuid.UUID, payload map[string]any, modifiedAt time.Time) *ent.RawOutput {
	t.Helper()
	key, _ := payload["OfficeKey"].(string)
	require.NotEmpty(t, key)
	return client.RawOutput.Create().
		SetSyncEventID(syncEventID).
		SetResource(rawoutput.ResourceOffice).
		SetSourceKey(key).
		SetChangeType(rawoutput.ChangeTypeInsert).
		SetSourceModifiedAt(modifiedAt).
		SetPayload(payload).
		SaveX(ctx)
}

func setupOfficeTest(t *testing.T) (context.Context, *ent.Client, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	client := testutil.NewTestDB(t)
	src := client.SourceSystem.Create().SetID("test-src").SetSourceSystemName("test").SaveX(ctx)
	ev := client.SyncEvent.Create().
		SetSourceSystemID(src.ID).
		SetResource("office").
		SetRunType("sync").
		SetProcessorVersion("test").
		SetStartedAt(time.Now()).
		SaveX(ctx)
	return ctx, client, ev.ID
}

func TestOfficeProcess_FreshInsert(t *testing.T) {
	ctx, client, evID := setupOfficeTest(t)

	ts := time.Now().UTC().Truncate(time.Second)
	raw := insertOfficeRaw(t, client, ctx, evID, map[string]any{
		"OfficeKey":             "OFF-1",
		"ModificationTimestamp": ts.Format(time.RFC3339),
		"OfficeName":            "Highpoint",
	}, ts)
	require.NoError(t, runOfficeProcess(t, client, ctx, raw))

	o, err := client.Office.Query().Where(office.IDEQ("OFF-1")).Only(ctx)
	require.NoError(t, err)
	require.NotNil(t, o.OfficeName)
	assert.Equal(t, "Highpoint", *o.OfficeName)
	require.NotNil(t, o.CurrentVersionID)

	versions := client.OfficeVersion.Query().Where(officeversion.OfficeKeyEQ("OFF-1")).AllX(ctx)
	require.Len(t, versions, 1)
	assert.Equal(t, officeversion.ChangeTypeInsert, versions[0].ChangeType)
	assert.Nil(t, versions[0].ChangedFields)
}

func TestOfficeProcess_UpdateWithDiff(t *testing.T) {
	ctx, client, evID := setupOfficeTest(t)

	ts1 := time.Now().UTC().Truncate(time.Second)
	first := insertOfficeRaw(t, client, ctx, evID, map[string]any{
		"OfficeKey":             "OFF-1",
		"ModificationTimestamp": ts1.Format(time.RFC3339),
		"OfficeName":            "Highpoint",
	}, ts1)
	require.NoError(t, runOfficeProcess(t, client, ctx, first))

	ts2 := ts1.Add(time.Hour)
	second := insertOfficeRaw(t, client, ctx, evID, map[string]any{
		"OfficeKey":             "OFF-1",
		"ModificationTimestamp": ts2.Format(time.RFC3339),
		"OfficeName":            "Highpoint Realty",
	}, ts2)
	require.NoError(t, runOfficeProcess(t, client, ctx, second))

	versions := client.OfficeVersion.Query().
		Where(officeversion.OfficeKeyEQ("OFF-1")).
		Order(ent.Asc(officeversion.FieldValidFrom)).
		AllX(ctx)
	require.Len(t, versions, 2)
	diff, ok := versions[1].ChangedFields["office_name"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "Highpoint", diff["old"])
	assert.Equal(t, "Highpoint Realty", diff["new"])
}

func TestOfficeProcess_UpdateClearsField(t *testing.T) {
	// Bug-1 regression — clear-on-nil semantics.
	ctx, client, evID := setupOfficeTest(t)

	ts1 := time.Now().UTC().Truncate(time.Second)
	first := insertOfficeRaw(t, client, ctx, evID, map[string]any{
		"OfficeKey":             "OFF-1",
		"ModificationTimestamp": ts1.Format(time.RFC3339),
		"OfficeName":            "Highpoint",
		"OfficePhone":           "5125550000",
	}, ts1)
	require.NoError(t, runOfficeProcess(t, client, ctx, first))

	ts2 := ts1.Add(time.Hour)
	second := insertOfficeRaw(t, client, ctx, evID, map[string]any{
		"OfficeKey":             "OFF-1",
		"ModificationTimestamp": ts2.Format(time.RFC3339),
		"OfficeName":            "Highpoint",
	}, ts2)
	require.NoError(t, runOfficeProcess(t, client, ctx, second))

	o, err := client.Office.Query().Where(office.IDEQ("OFF-1")).Only(ctx)
	require.NoError(t, err)
	assert.Nil(t, o.OfficePhone, "entity row clears OfficePhone")

	versions := client.OfficeVersion.Query().
		Where(officeversion.OfficeKeyEQ("OFF-1")).
		Order(ent.Asc(officeversion.FieldValidFrom)).
		AllX(ctx)
	require.Len(t, versions, 2)
	assert.Nil(t, versions[1].OfficePhone, "new version row also NULL — no drift")
	diff, ok := versions[1].ChangedFields["office_phone"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "5125550000", diff["old"])
	assert.Nil(t, diff["new"])
}

func TestOfficeProcess_AlreadyTombstonedIsNoop(t *testing.T) {
	ctx, client, evID := setupOfficeTest(t)

	ts1 := time.Now().UTC().Truncate(time.Second)
	first := insertOfficeRaw(t, client, ctx, evID, map[string]any{
		"OfficeKey":             "OFF-1",
		"ModificationTimestamp": ts1.Format(time.RFC3339),
	}, ts1)
	require.NoError(t, runOfficeProcess(t, client, ctx, first))

	ts2 := ts1.Add(time.Hour)
	tomb := insertOfficeRaw(t, client, ctx, evID, map[string]any{
		"OfficeKey":             "OFF-1",
		"ModificationTimestamp": ts2.Format(time.RFC3339),
		"MlgCanView":            false,
	}, ts2)
	require.NoError(t, runOfficeProcess(t, client, ctx, tomb))

	ts3 := ts2.Add(time.Hour)
	tomb2 := insertOfficeRaw(t, client, ctx, evID, map[string]any{
		"OfficeKey":             "OFF-1",
		"ModificationTimestamp": ts3.Format(time.RFC3339),
		"MlgCanView":            false,
	}, ts3)
	require.NoError(t, runOfficeProcess(t, client, ctx, tomb2))

	versions := client.OfficeVersion.Query().Where(officeversion.OfficeKeyEQ("OFF-1")).AllX(ctx)
	require.Len(t, versions, 2)
}
