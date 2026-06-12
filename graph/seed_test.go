package graph_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/media"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/mediaversion"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/memberversion"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/officeversion"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/openhouseversion"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/propertyroomversion"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/propertyunittypeversion"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/propertyversion"
)

// Seed helpers shared by the graph test files. Each seeds the minimum
// required fields plus whatever a test typically asserts on. visible
// maps to mlg_can_view.

func seedLookup(t *testing.T, client *ent.Client, id string, visible bool) {
	t.Helper()
	client.Lookup.Create().
		SetID(id).
		SetSourceModifiedAt(time.Now()).
		SetMlgCanView(visible).
		SetLookupName("Feature").
		SetLookupValue("Value-" + id).
		SaveX(context.Background())
}

func seedProperty(t *testing.T, client *ent.Client, id string, visible bool) {
	t.Helper()
	client.Property.Create().
		SetID(id).
		SetSourceModifiedAt(time.Now()).
		SetMlgCanView(visible).
		SaveX(context.Background())
}

func seedPropertyAt(t *testing.T, client *ent.Client, id string, lat, lng float64, visible bool) {
	t.Helper()
	client.Property.Create().
		SetID(id).
		SetSourceModifiedAt(time.Now()).
		SetMlgCanView(visible).
		SetLatitude(decimal.NewFromFloat(lat)).
		SetLongitude(decimal.NewFromFloat(lng)).
		SaveX(context.Background())
}

func seedMember(t *testing.T, client *ent.Client, id string, visible bool) {
	t.Helper()
	client.Member.Create().
		SetID(id).
		SetSourceModifiedAt(time.Now()).
		SetMlgCanView(visible).
		SaveX(context.Background())
}

func seedOffice(t *testing.T, client *ent.Client, id string, visible bool) {
	t.Helper()
	client.Office.Create().
		SetID(id).
		SetSourceModifiedAt(time.Now()).
		SetMlgCanView(visible).
		SetOfficeName("Office " + id).
		SaveX(context.Background())
}

func seedMedia(t *testing.T, client *ent.Client, id, recordKey string, visible bool) {
	t.Helper()
	client.Media.Create().
		SetID(id).
		SetSourceModifiedAt(time.Now()).
		SetMlgCanView(visible).
		SetResourceType(media.ResourceTypeProperty).
		SetResourceRecordKey(recordKey).
		SetMediaURL("https://cdn.example.com/" + id + ".jpg").
		SaveX(context.Background())
}

// The child tables carry a real FK on parent_listing_key → property, so
// the seed helpers leave it unset ("parked"). Tests exercising the
// Property edges use the *Linked variants after seeding the parent.

func seedOpenHouse(t *testing.T, client *ent.Client, id, listingKey string, visible bool) {
	t.Helper()
	client.OpenHouse.Create().
		SetID(id).
		SetSourceModifiedAt(time.Now()).
		SetMlgCanView(visible).
		SetListingKey(listingKey).
		SetOpenHouseStatus("Active").
		SaveX(context.Background())
}

func seedOpenHouseLinked(t *testing.T, client *ent.Client, id, listingKey string, visible bool) {
	t.Helper()
	client.OpenHouse.Create().
		SetID(id).
		SetSourceModifiedAt(time.Now()).
		SetMlgCanView(visible).
		SetListingKey(listingKey).
		SetParentListingKey(listingKey).
		SetOpenHouseStatus("Active").
		SaveX(context.Background())
}

func seedPropertyRoom(t *testing.T, client *ent.Client, id, listingKey string, visible bool) {
	t.Helper()
	client.PropertyRoom.Create().
		SetID(id).
		SetSourceModifiedAt(time.Now()).
		SetMlgCanView(visible).
		SetListingKey(listingKey).
		SetRoomType("Bedroom").
		SaveX(context.Background())
}

func seedPropertyRoomLinked(t *testing.T, client *ent.Client, id, listingKey string, visible bool) {
	t.Helper()
	client.PropertyRoom.Create().
		SetID(id).
		SetSourceModifiedAt(time.Now()).
		SetMlgCanView(visible).
		SetListingKey(listingKey).
		SetParentListingKey(listingKey).
		SetRoomType("Bedroom").
		SaveX(context.Background())
}

func seedPropertyUnitType(t *testing.T, client *ent.Client, id, listingKey string, visible bool) {
	t.Helper()
	client.PropertyUnitType.Create().
		SetID(id).
		SetSourceModifiedAt(time.Now()).
		SetMlgCanView(visible).
		SetListingKey(listingKey).
		SaveX(context.Background())
}

func seedPropertyUnitTypeLinked(t *testing.T, client *ent.Client, id, listingKey string, visible bool) {
	t.Helper()
	client.PropertyUnitType.Create().
		SetID(id).
		SetSourceModifiedAt(time.Now()).
		SetMlgCanView(visible).
		SetListingKey(listingKey).
		SetParentListingKey(listingKey).
		SaveX(context.Background())
}

func seedSourceSystem(t *testing.T, client *ent.Client, id string) {
	t.Helper()
	// SourceSystem has no MLS metadata mixin: no source_modified_at,
	// no mlg_can_view.
	client.SourceSystem.Create().
		SetID(id).
		SetSourceSystemName("System " + id).
		SaveX(context.Background())
}

func seedPropertyVersion(t *testing.T, client *ent.Client, listingKey string, change propertyversion.ChangeType, visible bool) string {
	t.Helper()
	v := client.PropertyVersion.Create().
		SetValidFrom(time.Now()).
		SetChangeType(change).
		SetProcessorVersion("v1").
		SetSourceModifiedAt(time.Now()).
		SetMlgCanView(visible).
		SetListingKey(listingKey).
		SetSyncEventID(uuid.New()).
		SaveX(context.Background())
	return v.ID
}

func seedMediaVersion(t *testing.T, client *ent.Client, mediaKey string) {
	t.Helper()
	client.MediaVersion.Create().
		SetValidFrom(time.Now()).
		SetChangeType(mediaversion.ChangeTypeInsert).
		SetProcessorVersion("v1").
		SetSourceModifiedAt(time.Now()).
		SetMlgCanView(true).
		SetResourceType(mediaversion.ResourceTypeProperty).
		SetResourceRecordKey("LK-" + mediaKey).
		SetMediaKey(mediaKey).
		SetSyncEventID(uuid.New()).
		SaveX(context.Background())
}

func seedMemberVersion(t *testing.T, client *ent.Client, memberKey string) {
	t.Helper()
	client.MemberVersion.Create().
		SetValidFrom(time.Now()).
		SetChangeType(memberversion.ChangeTypeInsert).
		SetProcessorVersion("v1").
		SetSourceModifiedAt(time.Now()).
		SetMlgCanView(true).
		SetMemberKey(memberKey).
		SetSyncEventID(uuid.New()).
		SaveX(context.Background())
}

func seedOfficeVersion(t *testing.T, client *ent.Client, officeKey string) {
	t.Helper()
	client.OfficeVersion.Create().
		SetValidFrom(time.Now()).
		SetChangeType(officeversion.ChangeTypeInsert).
		SetProcessorVersion("v1").
		SetSourceModifiedAt(time.Now()).
		SetMlgCanView(true).
		SetOfficeKey(officeKey).
		SetSyncEventID(uuid.New()).
		SaveX(context.Background())
}

func seedOpenHouseVersion(t *testing.T, client *ent.Client, openHouseKey string) {
	t.Helper()
	client.OpenHouseVersion.Create().
		SetValidFrom(time.Now()).
		SetChangeType(openhouseversion.ChangeTypeInsert).
		SetProcessorVersion("v1").
		SetSourceModifiedAt(time.Now()).
		SetMlgCanView(true).
		SetListingKey("LK-" + openHouseKey).
		SetOpenHouseKey(openHouseKey).
		SetSyncEventID(uuid.New()).
		SaveX(context.Background())
}

func seedPropertyRoomVersion(t *testing.T, client *ent.Client, roomKey string) {
	t.Helper()
	client.PropertyRoomVersion.Create().
		SetValidFrom(time.Now()).
		SetChangeType(propertyroomversion.ChangeTypeInsert).
		SetProcessorVersion("v1").
		SetSourceModifiedAt(time.Now()).
		SetMlgCanView(true).
		SetListingKey("LK-" + roomKey).
		SetRoomKey(roomKey).
		SetSyncEventID(uuid.New()).
		SaveX(context.Background())
}

func seedPropertyUnitTypeVersion(t *testing.T, client *ent.Client, unitTypeKey string) {
	t.Helper()
	client.PropertyUnitTypeVersion.Create().
		SetValidFrom(time.Now()).
		SetChangeType(propertyunittypeversion.ChangeTypeInsert).
		SetProcessorVersion("v1").
		SetSourceModifiedAt(time.Now()).
		SetMlgCanView(true).
		SetListingKey("LK-" + unitTypeKey).
		SetUnitTypeKey(unitTypeKey).
		SetSyncEventID(uuid.New()).
		SaveX(context.Background())
}
