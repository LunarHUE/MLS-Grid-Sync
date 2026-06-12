package processor

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/openhouse"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/propertyroom"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/propertyunittype"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/rawoutput"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/syncevent"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/internal/testutil"
)

// TestParking_ChildrenParkAndAfterPassReLinks verifies §5 end-to-end:
//
// PropertyRoom + PropertyUnitType + OpenHouse rows whose parent listing
// hasn't been processed land with listing_key set and parent_listing_key
// NULL. The PropertyProcessor's AfterPass runs three idempotent UPDATEs
// that promote NULL → listing_key once a matching Property exists.
func TestParking_ChildrenParkAndAfterPassReLinks(t *testing.T) {
	ctx := context.Background()
	client, db := testutil.NewTestDBWithSQL(t)
	src := client.SourceSystem.Create().SetID("test-src").SetSourceSystemName("test").SaveX(ctx)

	mkEv := func(resource syncevent.Resource) uuid.UUID {
		return client.SyncEvent.Create().
			SetSourceSystemID(src.ID).
			SetResource(resource).
			SetRunType("sync").
			SetProcessorVersion("test").
			SetStartedAt(time.Now()).
			SaveX(ctx).ID
	}
	roomEvID := mkEv(syncevent.ResourcePropertyRooms)
	unitTypeEvID := mkEv(syncevent.ResourcePropertyUnitTypes)
	openHouseEvID := mkEv(syncevent.ResourceOpenHouse)

	ts := time.Now().UTC().Truncate(time.Second)

	// Three parked children, all pointing at the not-yet-synced LK-LATE.
	require.NoError(t, runPropertyRoomProcess(t, client, ctx, insertPropertyRoomRaw(t, client, ctx, roomEvID, map[string]any{
		"RoomKey":               "R-PARK",
		"ListingKey":            "LK-LATE",
		"ModificationTimestamp": ts.Format(time.RFC3339),
	}, ts)))
	require.NoError(t, runPropertyUnitTypeProcess(t, client, ctx, insertPropertyUnitTypeRaw(t, client, ctx, unitTypeEvID, map[string]any{
		"UnitTypeKey":           "UT-PARK",
		"ListingKey":            "LK-LATE",
		"ModificationTimestamp": ts.Format(time.RFC3339),
	}, ts)))
	require.NoError(t, runOpenHouseProcess(t, client, ctx, insertOpenHouseRaw(t, client, ctx, openHouseEvID, map[string]any{
		"OpenHouseKey":          "OH-PARK",
		"ListingKey":            "LK-LATE",
		"ModificationTimestamp": ts.Format(time.RFC3339),
	}, ts)))

	// All three are parked: parent_listing_key is NULL.
	assert.Nil(t, client.PropertyRoom.Query().Where(propertyroom.IDEQ("R-PARK")).OnlyX(ctx).ParentListingKey)
	assert.Nil(t, client.PropertyUnitType.Query().Where(propertyunittype.IDEQ("UT-PARK")).OnlyX(ctx).ParentListingKey)
	assert.Nil(t, client.OpenHouse.Query().Where(openhouse.IDEQ("OH-PARK")).OnlyX(ctx).ParentListingKey)

	// Parent arrives. Run AfterPass — the three re-link UPDATEs fire.
	seedProperty(t, client, ctx, "LK-LATE")
	require.NoError(t, (&PropertyProcessor{}).AfterPass(ctx, db))

	// All three FKs filled.
	require.NotNil(t, client.PropertyRoom.Query().Where(propertyroom.IDEQ("R-PARK")).OnlyX(ctx).ParentListingKey)
	assert.Equal(t, "LK-LATE", *client.PropertyRoom.Query().Where(propertyroom.IDEQ("R-PARK")).OnlyX(ctx).ParentListingKey)
	require.NotNil(t, client.PropertyUnitType.Query().Where(propertyunittype.IDEQ("UT-PARK")).OnlyX(ctx).ParentListingKey)
	require.NotNil(t, client.OpenHouse.Query().Where(openhouse.IDEQ("OH-PARK")).OnlyX(ctx).ParentListingKey)

	// AfterPass is idempotent — re-running doesn't fail.
	require.NoError(t, (&PropertyProcessor{}).AfterPass(ctx, db))
}

// TestParking_OutsideSubscriptionStaysUnlinked: a child referencing a
// listing we never sync should remain parked forever — that's correct
// behavior. AfterPass must not error on it; the EXISTS-qualified UPDATE
// is the verification-§4 invariant.
func TestParking_OutsideSubscriptionStaysUnlinked(t *testing.T) {
	ctx := context.Background()
	client, db := testutil.NewTestDBWithSQL(t)
	src := client.SourceSystem.Create().SetID("test-src").SetSourceSystemName("test").SaveX(ctx)
	roomEvID := client.SyncEvent.Create().
		SetSourceSystemID(src.ID).
		SetResource(syncevent.ResourcePropertyRooms).
		SetRunType("sync").
		SetProcessorVersion("test").
		SetStartedAt(time.Now()).
		SaveX(ctx).ID

	ts := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, runPropertyRoomProcess(t, client, ctx, insertPropertyRoomRaw(t, client, ctx, roomEvID, map[string]any{
		"RoomKey":               "R-ORPHAN",
		"ListingKey":            "LK-OUTSIDE",
		"ModificationTimestamp": ts.Format(time.RFC3339),
	}, ts)))

	require.NoError(t, (&PropertyProcessor{}).AfterPass(ctx, db))

	r := client.PropertyRoom.Query().Where(propertyroom.IDEQ("R-ORPHAN")).OnlyX(ctx)
	assert.Nil(t, r.ParentListingKey, "remains parked — parent never arrived")
	assert.Equal(t, "LK-OUTSIDE", r.ListingKey, "natural key preserved")
}

var _ = rawoutput.ResourceProperty
