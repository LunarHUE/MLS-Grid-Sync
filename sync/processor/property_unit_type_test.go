package processor

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/propertyunittype"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/propertyunittypeversion"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
)

func runPropertyUnitTypeProcess(t *testing.T, client *ent.Client, ctx context.Context, raw *ent.RawOutput) error {
	t.Helper()
	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	p := NewPropertyUnitTypeProcessor()
	if _, err := p.Process(ctx, tx, raw); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func insertPropertyUnitTypeRaw(t *testing.T, client *ent.Client, ctx context.Context, syncEventID uuid.UUID, payload map[string]any, modifiedAt time.Time) *ent.RawOutput {
	t.Helper()
	key, _ := payload["UnitTypeKey"].(string)
	require.NotEmpty(t, key)
	return client.RawOutput.Create().
		SetSyncEventID(syncEventID).
		SetResource(rawoutput.ResourcePropertyUnitTypes).
		SetSourceKey(key).
		SetChangeType(rawoutput.ChangeTypeInsert).
		SetSourceModifiedAt(modifiedAt).
		SetPayload(payload).
		SaveX(ctx)
}

func setupPropertyUnitTypeTest(t *testing.T) (context.Context, *ent.Client, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	client := testutil.NewTestDB(t)
	src := client.SourceSystem.Create().SetID("test-src").SetSourceSystemName("test").SaveX(ctx)
	ev := client.SyncEvent.Create().
		SetSourceSystemID(src.ID).
		SetResource("property_unit_types").
		SetRunType("sync").
		SetProcessorVersion("test").
		SetStartedAt(time.Now()).
		SaveX(ctx)
	return ctx, client, ev.ID
}

func TestPropertyUnitTypeProcess_FreshInsertWithParent(t *testing.T) {
	ctx, client, evID := setupPropertyUnitTypeTest(t)
	seedProperty(t, client, ctx, "LK-1")

	ts := time.Now().UTC().Truncate(time.Second)
	raw := insertPropertyUnitTypeRaw(t, client, ctx, evID, map[string]any{
		"UnitTypeKey":           "UT-1",
		"ListingKey":            "LK-1",
		"UnitTypeBedsTotal":     2,
	}, ts)
	require.NoError(t, runPropertyUnitTypeProcess(t, client, ctx, raw))

	u, err := client.PropertyUnitType.Query().Where(propertyunittype.IDEQ("UT-1")).Only(ctx)
	require.NoError(t, err)
	require.NotNil(t, u.ParentListingKey)
	assert.Equal(t, "LK-1", *u.ParentListingKey)
	require.NotNil(t, u.UnitTypeBedsTotal)
	assert.Equal(t, int16(2), *u.UnitTypeBedsTotal)
}

func TestPropertyUnitTypeProcess_FreshInsertParked(t *testing.T) {
	ctx, client, evID := setupPropertyUnitTypeTest(t)

	ts := time.Now().UTC().Truncate(time.Second)
	raw := insertPropertyUnitTypeRaw(t, client, ctx, evID, map[string]any{
		"UnitTypeKey":           "UT-PARK",
		"ListingKey":            "LK-LATE",
	}, ts)
	require.NoError(t, runPropertyUnitTypeProcess(t, client, ctx, raw))

	u, err := client.PropertyUnitType.Query().Where(propertyunittype.IDEQ("UT-PARK")).Only(ctx)
	require.NoError(t, err)
	assert.Equal(t, "LK-LATE", u.ListingKey)
	assert.Nil(t, u.ParentListingKey)
}

func TestPropertyUnitTypeProcess_UpdateClearsField(t *testing.T) {
	ctx, client, evID := setupPropertyUnitTypeTest(t)
	seedProperty(t, client, ctx, "LK-1")

	ts1 := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, runPropertyUnitTypeProcess(t, client, ctx, insertPropertyUnitTypeRaw(t, client, ctx, evID, map[string]any{
		"UnitTypeKey":           "UT-1",
		"ListingKey":            "LK-1",
		"UnitTypeFurnished":     "Furnished",
	}, ts1)))

	ts2 := ts1.Add(time.Hour)
	require.NoError(t, runPropertyUnitTypeProcess(t, client, ctx, insertPropertyUnitTypeRaw(t, client, ctx, evID, map[string]any{
		"UnitTypeKey":           "UT-1",
		"ListingKey":            "LK-1",
	}, ts2)))

	u, err := client.PropertyUnitType.Query().Where(propertyunittype.IDEQ("UT-1")).Only(ctx)
	require.NoError(t, err)
	assert.Nil(t, u.UnitTypeFurnished)

	versions := client.PropertyUnitTypeVersion.Query().
		Where(propertyunittypeversion.UnitTypeKeyEQ("UT-1")).
		Order(ent.Asc(propertyunittypeversion.FieldValidFrom)).
		AllX(ctx)
	require.Len(t, versions, 2)
	assert.Nil(t, versions[1].UnitTypeFurnished)
}

func TestPropertyUnitTypeProcess_AlreadyTombstonedIsNoop(t *testing.T) {
	ctx, client, evID := setupPropertyUnitTypeTest(t)
	seedProperty(t, client, ctx, "LK-1")

	ts1 := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, runPropertyUnitTypeProcess(t, client, ctx, insertPropertyUnitTypeRaw(t, client, ctx, evID, map[string]any{
		"UnitTypeKey":           "UT-1",
		"ListingKey":            "LK-1",
	}, ts1)))

	ts2 := ts1.Add(time.Hour)
	require.NoError(t, runPropertyUnitTypeProcess(t, client, ctx, insertPropertyUnitTypeRaw(t, client, ctx, evID, map[string]any{
		"UnitTypeKey":           "UT-1",
		"ListingKey":            "LK-1",
		"MlgCanView":            false,
	}, ts2)))

	ts3 := ts2.Add(time.Hour)
	require.NoError(t, runPropertyUnitTypeProcess(t, client, ctx, insertPropertyUnitTypeRaw(t, client, ctx, evID, map[string]any{
		"UnitTypeKey":           "UT-1",
		"ListingKey":            "LK-1",
		"MlgCanView":            false,
	}, ts3)))

	versions := client.PropertyUnitTypeVersion.Query().Where(propertyunittypeversion.UnitTypeKeyEQ("UT-1")).AllX(ctx)
	require.Len(t, versions, 2)
}
