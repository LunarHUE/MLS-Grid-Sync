package processor

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/propertyroom"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/propertyroomversion"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/rawoutput"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/internal/testutil"
)

func runPropertyRoomProcess(t *testing.T, client *ent.Client, ctx context.Context, raw *ent.RawOutput) error {
	t.Helper()
	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	p := NewPropertyRoomProcessor()
	if _, err := p.Process(ctx, tx, raw); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func insertPropertyRoomRaw(t *testing.T, client *ent.Client, ctx context.Context, syncEventID uuid.UUID, payload map[string]any, modifiedAt time.Time) *ent.RawOutput {
	t.Helper()
	key, _ := payload["RoomKey"].(string)
	require.NotEmpty(t, key)
	return client.RawOutput.Create().
		SetSyncEventID(syncEventID).
		SetResource(rawoutput.ResourcePropertyRooms).
		SetSourceKey(key).
		SetChangeType(rawoutput.ChangeTypeInsert).
		SetSourceModifiedAt(modifiedAt).
		SetPayload(payload).
		SaveX(ctx)
}

func setupPropertyRoomTest(t *testing.T) (context.Context, *ent.Client, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	client := testutil.NewTestDB(t)
	src := client.SourceSystem.Create().SetID("test-src").SetSourceSystemName("test").SaveX(ctx)
	ev := client.SyncEvent.Create().
		SetSourceSystemID(src.ID).
		SetResource("property_rooms").
		SetRunType("sync").
		SetProcessorVersion("test").
		SetStartedAt(time.Now()).
		SaveX(ctx)
	return ctx, client, ev.ID
}

func TestPropertyRoomProcess_FreshInsertWithParent(t *testing.T) {
	ctx, client, evID := setupPropertyRoomTest(t)
	seedProperty(t, client, ctx, "LK-1")

	ts := time.Now().UTC().Truncate(time.Second)
	raw := insertPropertyRoomRaw(t, client, ctx, evID, map[string]any{
		"RoomKey":               "R-1",
		"ListingKey":            "LK-1",
		"RoomType":              "Bedroom",
	}, ts)
	require.NoError(t, runPropertyRoomProcess(t, client, ctx, raw))

	r, err := client.PropertyRoom.Query().Where(propertyroom.IDEQ("R-1")).Only(ctx)
	require.NoError(t, err)
	require.NotNil(t, r.ParentListingKey)
	assert.Equal(t, "LK-1", *r.ParentListingKey)
}

func TestPropertyRoomProcess_FreshInsertParked(t *testing.T) {
	ctx, client, evID := setupPropertyRoomTest(t)

	ts := time.Now().UTC().Truncate(time.Second)
	raw := insertPropertyRoomRaw(t, client, ctx, evID, map[string]any{
		"RoomKey":               "R-PARK",
		"ListingKey":            "LK-LATE",
	}, ts)
	require.NoError(t, runPropertyRoomProcess(t, client, ctx, raw))

	r, err := client.PropertyRoom.Query().Where(propertyroom.IDEQ("R-PARK")).Only(ctx)
	require.NoError(t, err)
	assert.Equal(t, "LK-LATE", r.ListingKey, "natural key lands")
	assert.Nil(t, r.ParentListingKey, "FK parked")
}

func TestPropertyRoomProcess_UpdateClearsField(t *testing.T) {
	ctx, client, evID := setupPropertyRoomTest(t)
	seedProperty(t, client, ctx, "LK-1")

	ts1 := time.Now().UTC().Truncate(time.Second)
	first := insertPropertyRoomRaw(t, client, ctx, evID, map[string]any{
		"RoomKey":               "R-1",
		"ListingKey":            "LK-1",
		"RoomType":              "Bedroom",
	}, ts1)
	require.NoError(t, runPropertyRoomProcess(t, client, ctx, first))

	ts2 := ts1.Add(time.Hour)
	second := insertPropertyRoomRaw(t, client, ctx, evID, map[string]any{
		"RoomKey":               "R-1",
		"ListingKey":            "LK-1",
	}, ts2)
	require.NoError(t, runPropertyRoomProcess(t, client, ctx, second))

	r, err := client.PropertyRoom.Query().Where(propertyroom.IDEQ("R-1")).Only(ctx)
	require.NoError(t, err)
	assert.Nil(t, r.RoomType)

	versions := client.PropertyRoomVersion.Query().
		Where(propertyroomversion.RoomKeyEQ("R-1")).
		Order(ent.Asc(propertyroomversion.FieldValidFrom)).
		AllX(ctx)
	require.Len(t, versions, 2)
	assert.Nil(t, versions[1].RoomType)
}

func TestPropertyRoomProcess_AlreadyTombstonedIsNoop(t *testing.T) {
	ctx, client, evID := setupPropertyRoomTest(t)
	seedProperty(t, client, ctx, "LK-1")

	ts1 := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, runPropertyRoomProcess(t, client, ctx, insertPropertyRoomRaw(t, client, ctx, evID, map[string]any{
		"RoomKey":               "R-1",
		"ListingKey":            "LK-1",
	}, ts1)))

	ts2 := ts1.Add(time.Hour)
	require.NoError(t, runPropertyRoomProcess(t, client, ctx, insertPropertyRoomRaw(t, client, ctx, evID, map[string]any{
		"RoomKey":               "R-1",
		"ListingKey":            "LK-1",
		"MlgCanView":            false,
	}, ts2)))

	ts3 := ts2.Add(time.Hour)
	require.NoError(t, runPropertyRoomProcess(t, client, ctx, insertPropertyRoomRaw(t, client, ctx, evID, map[string]any{
		"RoomKey":               "R-1",
		"ListingKey":            "LK-1",
		"MlgCanView":            false,
	}, ts3)))

	versions := client.PropertyRoomVersion.Query().Where(propertyroomversion.RoomKeyEQ("R-1")).AllX(ctx)
	require.Len(t, versions, 2)
}
