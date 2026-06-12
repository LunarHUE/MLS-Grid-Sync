package processor

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/openhouse"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/openhouseversion"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
)

func runOpenHouseProcess(t *testing.T, client *ent.Client, ctx context.Context, raw *ent.RawOutput) error {
	t.Helper()
	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	p := NewOpenHouseProcessor()
	if _, err := p.Process(ctx, tx, raw); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func insertOpenHouseRaw(t *testing.T, client *ent.Client, ctx context.Context, syncEventID uuid.UUID, payload map[string]any, modifiedAt time.Time) *ent.RawOutput {
	t.Helper()
	key, _ := payload["OpenHouseKey"].(string)
	require.NotEmpty(t, key, "test payload must include OpenHouseKey")
	return client.RawOutput.Create().
		SetSyncEventID(syncEventID).
		SetResource(rawoutput.ResourceOpenHouse).
		SetSourceKey(key).
		SetChangeType(rawoutput.ChangeTypeInsert).
		SetSourceModifiedAt(modifiedAt).
		SetPayload(payload).
		SaveX(ctx)
}

func setupOpenHouseTest(t *testing.T) (context.Context, *ent.Client, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	client := testutil.NewTestDB(t)

	src := client.SourceSystem.Create().
		SetID("test-src").
		SetSourceSystemName("test").
		SaveX(ctx)
	ev := client.SyncEvent.Create().
		SetSourceSystemID(src.ID).
		SetResource("open_house").
		SetRunType("sync").
		SetProcessorVersion("test").
		SetStartedAt(time.Now()).
		SaveX(ctx)
	return ctx, client, ev.ID
}

func seedProperty(t *testing.T, client *ent.Client, ctx context.Context, listingKey string) {
	t.Helper()
	client.Property.Create().
		SetID(listingKey).
		SetSourceModifiedAt(time.Now().UTC()).
		SaveX(ctx)
}

func TestOpenHouseProcess_FreshInsertWithParent(t *testing.T) {
	ctx, client, evID := setupOpenHouseTest(t)
	seedProperty(t, client, ctx, "LK-1")

	ts := time.Now().UTC().Truncate(time.Second)
	raw := insertOpenHouseRaw(t, client, ctx, evID, map[string]any{
		"OpenHouseKey":          "OH-1",
		"ListingKey":            "LK-1",
		"ModificationTimestamp": ts.Format(time.RFC3339),
		"OpenHouseStatus":       "Scheduled",
	}, ts)
	require.NoError(t, runOpenHouseProcess(t, client, ctx, raw))

	oh, err := client.OpenHouse.Query().Where(openhouse.IDEQ("OH-1")).Only(ctx)
	require.NoError(t, err)
	assert.Equal(t, "LK-1", oh.ListingKey)
	require.NotNil(t, oh.ParentListingKey, "parent linked at insert because Property exists")
	assert.Equal(t, "LK-1", *oh.ParentListingKey)
}

func TestOpenHouseProcess_FreshInsertParked_ThenAfterPassReLinks(t *testing.T) {
	ctx := context.Background()
	client, db := testutil.NewTestDBWithSQL(t)
	src := client.SourceSystem.Create().SetID("test-src").SetSourceSystemName("test").SaveX(ctx)
	ev := client.SyncEvent.Create().
		SetSourceSystemID(src.ID).
		SetResource("open_house").
		SetRunType("sync").
		SetProcessorVersion("test").
		SetStartedAt(time.Now()).
		SaveX(ctx)

	// Deliberately do NOT seed Property — the OpenHouse should park.
	ts := time.Now().UTC().Truncate(time.Second)
	raw := insertOpenHouseRaw(t, client, ctx, ev.ID, map[string]any{
		"OpenHouseKey":          "OH-PARK",
		"ListingKey":            "LK-LATE",
		"ModificationTimestamp": ts.Format(time.RFC3339),
	}, ts)
	require.NoError(t, runOpenHouseProcess(t, client, ctx, raw))

	oh, err := client.OpenHouse.Query().Where(openhouse.IDEQ("OH-PARK")).Only(ctx)
	require.NoError(t, err)
	assert.Equal(t, "LK-LATE", oh.ListingKey, "natural key always lands")
	assert.Nil(t, oh.ParentListingKey, "FK parked — parent not yet present")

	// Now seed the parent Property and run PropertyProcessor.AfterPass on
	// the same DB. Confirm the re-link UPDATE fills the parked FK.
	seedProperty(t, client, ctx, "LK-LATE")
	require.NoError(t, (&PropertyProcessor{}).AfterPass(ctx, db))

	oh2, err := client.OpenHouse.Query().Where(openhouse.IDEQ("OH-PARK")).Only(ctx)
	require.NoError(t, err)
	require.NotNil(t, oh2.ParentListingKey, "AfterPass re-linked the parked FK")
	assert.Equal(t, "LK-LATE", *oh2.ParentListingKey)
}

func TestOpenHouseProcess_UpdateWithDiff(t *testing.T) {
	ctx, client, evID := setupOpenHouseTest(t)
	seedProperty(t, client, ctx, "LK-1")

	ts1 := time.Now().UTC().Truncate(time.Second)
	first := insertOpenHouseRaw(t, client, ctx, evID, map[string]any{
		"OpenHouseKey":          "OH-1",
		"ListingKey":            "LK-1",
		"ModificationTimestamp": ts1.Format(time.RFC3339),
		"OpenHouseStatus":       "Scheduled",
	}, ts1)
	require.NoError(t, runOpenHouseProcess(t, client, ctx, first))

	ts2 := ts1.Add(time.Hour)
	second := insertOpenHouseRaw(t, client, ctx, evID, map[string]any{
		"OpenHouseKey":          "OH-1",
		"ListingKey":            "LK-1",
		"ModificationTimestamp": ts2.Format(time.RFC3339),
		"OpenHouseStatus":       "Canceled",
	}, ts2)
	require.NoError(t, runOpenHouseProcess(t, client, ctx, second))

	versions := client.OpenHouseVersion.Query().
		Where(openhouseversion.OpenHouseKeyEQ("OH-1")).
		Order(ent.Asc(openhouseversion.FieldValidFrom)).
		AllX(ctx)
	require.Len(t, versions, 2)
	assert.NotNil(t, versions[0].ValidTo)
	assert.Nil(t, versions[1].ValidTo)
	require.NotNil(t, versions[1].ChangedFields)
	diff, ok := versions[1].ChangedFields["open_house_status"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "Scheduled", diff["old"])
	assert.Equal(t, "Canceled", diff["new"])
}

func TestOpenHouseProcess_UpdateClearsField(t *testing.T) {
	// Bug-1 regression — see TestMemberProcess_UpdateClearsField.
	ctx, client, evID := setupOpenHouseTest(t)
	seedProperty(t, client, ctx, "LK-1")

	ts1 := time.Now().UTC().Truncate(time.Second)
	first := insertOpenHouseRaw(t, client, ctx, evID, map[string]any{
		"OpenHouseKey":          "OH-1",
		"ListingKey":            "LK-1",
		"ModificationTimestamp": ts1.Format(time.RFC3339),
		"OpenHouseStatus":       "Scheduled",
	}, ts1)
	require.NoError(t, runOpenHouseProcess(t, client, ctx, first))

	// Second payload omits OpenHouseStatus.
	ts2 := ts1.Add(time.Hour)
	second := insertOpenHouseRaw(t, client, ctx, evID, map[string]any{
		"OpenHouseKey":          "OH-1",
		"ListingKey":            "LK-1",
		"ModificationTimestamp": ts2.Format(time.RFC3339),
	}, ts2)
	require.NoError(t, runOpenHouseProcess(t, client, ctx, second))

	oh, err := client.OpenHouse.Query().Where(openhouse.IDEQ("OH-1")).Only(ctx)
	require.NoError(t, err)
	assert.Nil(t, oh.OpenHouseStatus, "entity clears OpenHouseStatus")

	versions := client.OpenHouseVersion.Query().
		Where(openhouseversion.OpenHouseKeyEQ("OH-1")).
		Order(ent.Asc(openhouseversion.FieldValidFrom)).
		AllX(ctx)
	require.Len(t, versions, 2)
	assert.Nil(t, versions[1].OpenHouseStatus, "new version row also NULL — no drift")
	diff, ok := versions[1].ChangedFields["open_house_status"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "Scheduled", diff["old"])
	assert.Nil(t, diff["new"])
}

func TestOpenHouseProcess_AlreadyTombstonedIsNoop(t *testing.T) {
	// Bug-2 regression — see TestMemberProcess_AlreadyTombstonedIsNoop.
	ctx, client, evID := setupOpenHouseTest(t)
	seedProperty(t, client, ctx, "LK-1")

	ts1 := time.Now().UTC().Truncate(time.Second)
	first := insertOpenHouseRaw(t, client, ctx, evID, map[string]any{
		"OpenHouseKey":          "OH-1",
		"ListingKey":            "LK-1",
		"ModificationTimestamp": ts1.Format(time.RFC3339),
	}, ts1)
	require.NoError(t, runOpenHouseProcess(t, client, ctx, first))

	ts2 := ts1.Add(time.Hour)
	tomb := insertOpenHouseRaw(t, client, ctx, evID, map[string]any{
		"OpenHouseKey":          "OH-1",
		"ListingKey":            "LK-1",
		"ModificationTimestamp": ts2.Format(time.RFC3339),
		"MlgCanView":            false,
	}, ts2)
	require.NoError(t, runOpenHouseProcess(t, client, ctx, tomb))

	ts3 := ts2.Add(time.Hour)
	tomb2 := insertOpenHouseRaw(t, client, ctx, evID, map[string]any{
		"OpenHouseKey":          "OH-1",
		"ListingKey":            "LK-1",
		"ModificationTimestamp": ts3.Format(time.RFC3339),
		"MlgCanView":            false,
	}, ts3)
	require.NoError(t, runOpenHouseProcess(t, client, ctx, tomb2))

	versions := client.OpenHouseVersion.Query().
		Where(openhouseversion.OpenHouseKeyEQ("OH-1")).
		AllX(ctx)
	require.Len(t, versions, 2, "second tombstone is no-op")
}

func TestOpenHouseProcess_MlgCanViewFalseTombstone(t *testing.T) {
	ctx, client, evID := setupOpenHouseTest(t)
	seedProperty(t, client, ctx, "LK-1")

	ts1 := time.Now().UTC().Truncate(time.Second)
	first := insertOpenHouseRaw(t, client, ctx, evID, map[string]any{
		"OpenHouseKey":          "OH-1",
		"ListingKey":            "LK-1",
		"ModificationTimestamp": ts1.Format(time.RFC3339),
	}, ts1)
	require.NoError(t, runOpenHouseProcess(t, client, ctx, first))

	ts2 := ts1.Add(time.Hour)
	tomb := insertOpenHouseRaw(t, client, ctx, evID, map[string]any{
		"OpenHouseKey":          "OH-1",
		"ListingKey":            "LK-1",
		"ModificationTimestamp": ts2.Format(time.RFC3339),
		"MlgCanView":            false,
	}, ts2)
	require.NoError(t, runOpenHouseProcess(t, client, ctx, tomb))

	oh, err := client.OpenHouse.Query().Where(openhouse.IDEQ("OH-1")).Only(ctx)
	require.NoError(t, err)
	assert.False(t, oh.MlgCanView)

	versions := client.OpenHouseVersion.Query().
		Where(openhouseversion.OpenHouseKeyEQ("OH-1")).
		Order(ent.Asc(openhouseversion.FieldValidFrom)).
		AllX(ctx)
	require.Len(t, versions, 2)
	assert.Equal(t, openhouseversion.ChangeTypeDelete, versions[1].ChangeType)
}
