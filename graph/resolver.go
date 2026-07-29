package graph

import (
	"context"

	"entgo.io/contrib/entgql"
	"entgo.io/ent/dialect/sql"
	"github.com/99designs/gqlgen/graphql"
	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/lookup"
	entmedia "github.com/LunarHUE/MLS-Grid-Sync/ent/media"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/member"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/office"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/openhouse"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/predicate"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/property"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/propertyroom"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/propertyunittype"
	"github.com/LunarHUE/MLS-Grid-Sync/graph/model"
)

type Resolver struct{ client *ent.Client }

func NewSchema(client *ent.Client) graphql.ExecutableSchema {
	return NewExecutableSchema(Config{
		Resolvers:  &Resolver{client: client},
		Complexity: complexityRoot(),
	})
}

// --- type aliases for sub-resolvers ---

type queryResolver struct{ *Resolver }
type mediaResolver struct{ *Resolver }
type mediaVersionResolver struct{ *Resolver }
type memberResolver struct{ *Resolver }
type memberVersionResolver struct{ *Resolver }
type officeResolver struct{ *Resolver }
type officeVersionResolver struct{ *Resolver }
type openHouseResolver struct{ *Resolver }
type openHouseVersionResolver struct{ *Resolver }
type propertyResolver struct{ *Resolver }
type propertyRoomResolver struct{ *Resolver }
type propertyRoomVersionResolver struct{ *Resolver }
type propertyUnitTypeResolver struct{ *Resolver }
type propertyUnitTypeVersionResolver struct{ *Resolver }
type propertyVersionResolver struct{ *Resolver }

func (r *Resolver) Query() QueryResolver                               { return &queryResolver{r} }
func (r *Resolver) Media() MediaResolver                               { return &mediaResolver{r} }
func (r *Resolver) MediaVersion() MediaVersionResolver                 { return &mediaVersionResolver{r} }
func (r *Resolver) Member() MemberResolver                             { return &memberResolver{r} }
func (r *Resolver) MemberVersion() MemberVersionResolver               { return &memberVersionResolver{r} }
func (r *Resolver) Office() OfficeResolver                             { return &officeResolver{r} }
func (r *Resolver) OfficeVersion() OfficeVersionResolver               { return &officeVersionResolver{r} }
func (r *Resolver) OpenHouse() OpenHouseResolver                       { return &openHouseResolver{r} }
func (r *Resolver) OpenHouseVersion() OpenHouseVersionResolver         { return &openHouseVersionResolver{r} }
func (r *Resolver) Property() PropertyResolver                         { return &propertyResolver{r} }
func (r *Resolver) PropertyRoom() PropertyRoomResolver                 { return &propertyRoomResolver{r} }
func (r *Resolver) PropertyRoomVersion() PropertyRoomVersionResolver   { return &propertyRoomVersionResolver{r} }
func (r *Resolver) PropertyUnitType() PropertyUnitTypeResolver         { return &propertyUnitTypeResolver{r} }
func (r *Resolver) PropertyUnitTypeVersion() PropertyUnitTypeVersionResolver {
	return &propertyUnitTypeVersionResolver{r}
}
func (r *Resolver) PropertyVersion() PropertyVersionResolver { return &propertyVersionResolver{r} }

// --- helpers ---

func uuidPtr(u *uuid.UUID) *string {
	if u == nil {
		return nil
	}
	s := u.String()
	return &s
}

func uuidStr(u uuid.UUID) string { return u.String() }

func stringArray(arr pq.StringArray) any {
	if arr == nil {
		return nil
	}
	return []string(arr)
}

// ============================================================
// QueryResolver
// ============================================================

// entityTables lists every GraphQL-exposed table. Because entity IDs are plain
// MLS Grid strings (no type prefix), we probe tables in order to resolve a
// bare ID to its concrete type.
var entityTables = []string{
	"lookup",
	"media", "media_version",
	"member", "member_version",
	"office", "office_version",
	"open_house", "open_house_version",
	"property", "property_room", "property_room_version",
	"property_unit_type", "property_unit_type_version",
	"property_version",
	"source_system",
}

func (r *queryResolver) Node(ctx context.Context, id string) (ent.Noder, error) {
	for _, table := range entityTables {
		noder, err := r.client.Noder(ctx, id, ent.WithFixedNodeType(table))
		if err == nil {
			visible, err := r.nodeVisible(ctx, noder)
			if err != nil {
				return nil, err
			}
			if !visible {
				return nil, nil
			}
			return noder, nil
		}
		if !ent.IsNotFound(err) {
			return nil, err
		}
	}
	return nil, nil
}

// nodeVisible reports whether a noder is consumer-visible, matching the
// mlg_can_view filter the list resolvers and soft-key resolvers apply.
// It re-queries by ID rather than reading the struct field: the Noder
// fetch uses gqlgen field collection, so MlgCanView is only populated
// when the client happened to request it. Version types and SourceSystem
// are always visible: versions are audit history (a tombstoned delete
// version IS the record), and SourceSystem carries no visibility flag.
func (r *queryResolver) nodeVisible(ctx context.Context, n ent.Noder) (bool, error) {
	switch v := n.(type) {
	case *ent.Lookup:
		return r.client.Lookup.Query().Where(lookup.ID(v.ID), lookup.MlgCanView(true)).Exist(ctx)
	case *ent.Media:
		return r.client.Media.Query().Where(entmedia.ID(v.ID), entmedia.MlgCanView(true)).Exist(ctx)
	case *ent.Member:
		return r.client.Member.Query().Where(member.ID(v.ID), member.MlgCanView(true)).Exist(ctx)
	case *ent.Office:
		return r.client.Office.Query().Where(office.ID(v.ID), office.MlgCanView(true)).Exist(ctx)
	case *ent.OpenHouse:
		return r.client.OpenHouse.Query().Where(openhouse.ID(v.ID), openhouse.MlgCanView(true)).Exist(ctx)
	case *ent.Property:
		return r.client.Property.Query().Where(property.ID(v.ID), property.MlgCanView(true)).Exist(ctx)
	case *ent.PropertyRoom:
		return r.client.PropertyRoom.Query().Where(propertyroom.ID(v.ID), propertyroom.MlgCanView(true)).Exist(ctx)
	case *ent.PropertyUnitType:
		return r.client.PropertyUnitType.Query().Where(propertyunittype.ID(v.ID), propertyunittype.MlgCanView(true)).Exist(ctx)
	default:
		return true, nil
	}
}

func (r *queryResolver) Nodes(ctx context.Context, ids []string) ([]ent.Noder, error) {
	noders := make([]ent.Noder, len(ids))
	for i, id := range ids {
		noder, err := r.Node(ctx, id)
		if err != nil {
			return nil, err
		}
		noders[i] = noder
	}
	return noders, nil
}

func (r *queryResolver) Lookups(ctx context.Context, after *entgql.Cursor[string], first *int, before *entgql.Cursor[string], last *int, orderBy *ent.LookupOrder, where *ent.LookupWhereInput) (*ent.LookupConnection, error) {
	first, last = clampPage(first, last)
	return r.client.Lookup.Query().
		Where(lookup.MlgCanView(true)).
		Paginate(ctx, after, first, before, last,
			ent.WithLookupOrder(orderBy),
			ent.WithLookupFilter(where.Filter))
}

func (r *queryResolver) MediaSlice(ctx context.Context, after *entgql.Cursor[string], first *int, before *entgql.Cursor[string], last *int, orderBy *ent.MediaOrder, where *ent.MediaWhereInput) (*ent.MediaConnection, error) {
	first, last = clampPage(first, last)
	return r.client.Media.Query().
		Where(entmedia.MlgCanView(true)).
		Paginate(ctx, after, first, before, last,
			ent.WithMediaOrder(orderBy),
			ent.WithMediaFilter(where.Filter))
}

func (r *queryResolver) MediaVersions(ctx context.Context, after *entgql.Cursor[string], first *int, before *entgql.Cursor[string], last *int, orderBy *ent.MediaVersionOrder, where *ent.MediaVersionWhereInput) (*ent.MediaVersionConnection, error) {
	first, last = clampPage(first, last)
	return r.client.MediaVersion.Query().
		Paginate(ctx, after, first, before, last,
			ent.WithMediaVersionOrder(orderBy),
			ent.WithMediaVersionFilter(where.Filter))
}

func (r *queryResolver) Members(ctx context.Context, after *entgql.Cursor[string], first *int, before *entgql.Cursor[string], last *int, orderBy *ent.MemberOrder, where *ent.MemberWhereInput) (*ent.MemberConnection, error) {
	first, last = clampPage(first, last)
	return r.client.Member.Query().
		Where(member.MlgCanView(true)).
		Paginate(ctx, after, first, before, last,
			ent.WithMemberOrder(orderBy),
			ent.WithMemberFilter(where.Filter))
}

func (r *queryResolver) MemberVersions(ctx context.Context, after *entgql.Cursor[string], first *int, before *entgql.Cursor[string], last *int, orderBy *ent.MemberVersionOrder, where *ent.MemberVersionWhereInput) (*ent.MemberVersionConnection, error) {
	first, last = clampPage(first, last)
	return r.client.MemberVersion.Query().
		Paginate(ctx, after, first, before, last,
			ent.WithMemberVersionOrder(orderBy),
			ent.WithMemberVersionFilter(where.Filter))
}

func (r *queryResolver) Offices(ctx context.Context, after *entgql.Cursor[string], first *int, before *entgql.Cursor[string], last *int, orderBy *ent.OfficeOrder, where *ent.OfficeWhereInput) (*ent.OfficeConnection, error) {
	first, last = clampPage(first, last)
	return r.client.Office.Query().
		Where(office.MlgCanView(true)).
		Paginate(ctx, after, first, before, last,
			ent.WithOfficeOrder(orderBy),
			ent.WithOfficeFilter(where.Filter))
}

func (r *queryResolver) OfficeVersions(ctx context.Context, after *entgql.Cursor[string], first *int, before *entgql.Cursor[string], last *int, orderBy *ent.OfficeVersionOrder, where *ent.OfficeVersionWhereInput) (*ent.OfficeVersionConnection, error) {
	first, last = clampPage(first, last)
	return r.client.OfficeVersion.Query().
		Paginate(ctx, after, first, before, last,
			ent.WithOfficeVersionOrder(orderBy),
			ent.WithOfficeVersionFilter(where.Filter))
}

func (r *queryResolver) OpenHouses(ctx context.Context, after *entgql.Cursor[string], first *int, before *entgql.Cursor[string], last *int, orderBy *ent.OpenHouseOrder, where *ent.OpenHouseWhereInput) (*ent.OpenHouseConnection, error) {
	first, last = clampPage(first, last)
	return r.client.OpenHouse.Query().
		Where(openhouse.MlgCanView(true)).
		Paginate(ctx, after, first, before, last,
			ent.WithOpenHouseOrder(orderBy),
			ent.WithOpenHouseFilter(where.Filter))
}

func (r *queryResolver) OpenHouseVersions(ctx context.Context, after *entgql.Cursor[string], first *int, before *entgql.Cursor[string], last *int, orderBy *ent.OpenHouseVersionOrder, where *ent.OpenHouseVersionWhereInput) (*ent.OpenHouseVersionConnection, error) {
	first, last = clampPage(first, last)
	return r.client.OpenHouseVersion.Query().
		Paginate(ctx, after, first, before, last,
			ent.WithOpenHouseVersionOrder(orderBy),
			ent.WithOpenHouseVersionFilter(where.Filter))
}

func (r *queryResolver) Properties(ctx context.Context, after *entgql.Cursor[string], first *int, before *entgql.Cursor[string], last *int, orderBy *ent.PropertyOrder, where *ent.PropertyWhereInput, geo *model.GeoFilter) (*ent.PropertyConnection, error) {
	first, last = clampPage(first, last)
	preds := []predicate.Property{property.MlgCanView(true)}
	gp, err := geoPredicate(geo)
	if err != nil {
		return nil, err
	}
	if gp != nil {
		preds = append(preds, gp)
	}
	return r.client.Property.Query().
		Where(preds...).
		Paginate(ctx, after, first, before, last,
			ent.WithPropertyOrder(orderBy),
			ent.WithPropertyFilter(where.Filter))
}

func (r *queryResolver) PropertyRooms(ctx context.Context, after *entgql.Cursor[string], first *int, before *entgql.Cursor[string], last *int, orderBy *ent.PropertyRoomOrder, where *ent.PropertyRoomWhereInput) (*ent.PropertyRoomConnection, error) {
	first, last = clampPage(first, last)
	return r.client.PropertyRoom.Query().
		Where(propertyroom.MlgCanView(true)).
		Paginate(ctx, after, first, before, last,
			ent.WithPropertyRoomOrder(orderBy),
			ent.WithPropertyRoomFilter(where.Filter))
}

func (r *queryResolver) PropertyRoomVersions(ctx context.Context, after *entgql.Cursor[string], first *int, before *entgql.Cursor[string], last *int, orderBy *ent.PropertyRoomVersionOrder, where *ent.PropertyRoomVersionWhereInput) (*ent.PropertyRoomVersionConnection, error) {
	first, last = clampPage(first, last)
	return r.client.PropertyRoomVersion.Query().
		Paginate(ctx, after, first, before, last,
			ent.WithPropertyRoomVersionOrder(orderBy),
			ent.WithPropertyRoomVersionFilter(where.Filter))
}

func (r *queryResolver) PropertyUnitTypes(ctx context.Context, after *entgql.Cursor[string], first *int, before *entgql.Cursor[string], last *int, orderBy *ent.PropertyUnitTypeOrder, where *ent.PropertyUnitTypeWhereInput) (*ent.PropertyUnitTypeConnection, error) {
	first, last = clampPage(first, last)
	return r.client.PropertyUnitType.Query().
		Where(propertyunittype.MlgCanView(true)).
		Paginate(ctx, after, first, before, last,
			ent.WithPropertyUnitTypeOrder(orderBy),
			ent.WithPropertyUnitTypeFilter(where.Filter))
}

func (r *queryResolver) PropertyUnitTypeVersions(ctx context.Context, after *entgql.Cursor[string], first *int, before *entgql.Cursor[string], last *int, orderBy *ent.PropertyUnitTypeVersionOrder, where *ent.PropertyUnitTypeVersionWhereInput) (*ent.PropertyUnitTypeVersionConnection, error) {
	first, last = clampPage(first, last)
	return r.client.PropertyUnitTypeVersion.Query().
		Paginate(ctx, after, first, before, last,
			ent.WithPropertyUnitTypeVersionOrder(orderBy),
			ent.WithPropertyUnitTypeVersionFilter(where.Filter))
}

func (r *queryResolver) PropertyVersions(ctx context.Context, after *entgql.Cursor[string], first *int, before *entgql.Cursor[string], last *int, orderBy *ent.PropertyVersionOrder, where *ent.PropertyVersionWhereInput) (*ent.PropertyVersionConnection, error) {
	first, last = clampPage(first, last)
	return r.client.PropertyVersion.Query().
		Paginate(ctx, after, first, before, last,
			ent.WithPropertyVersionOrder(orderBy),
			ent.WithPropertyVersionFilter(where.Filter))
}

func (r *queryResolver) SourceSystems(ctx context.Context, after *entgql.Cursor[string], first *int, before *entgql.Cursor[string], last *int, where *ent.SourceSystemWhereInput) (*ent.SourceSystemConnection, error) {
	first, last = clampPage(first, last)
	return r.client.SourceSystem.Query().
		Paginate(ctx, after, first, before, last,
			ent.WithSourceSystemFilter(where.Filter))
}

// ============================================================
// MediaResolver — type conversions for fields gqlgen can't autobind
// ============================================================

func (r *mediaResolver) AttachmentID(ctx context.Context, obj *ent.Media) (*string, error) {
	return uuidPtr(obj.AttachmentID), nil
}

func (r *mediaResolver) CurrentVersionID(ctx context.Context, obj *ent.Media) (*string, error) {
	return uuidPtr(obj.CurrentVersionID), nil
}

// ============================================================
// MediaVersionResolver
// ============================================================

func (r *mediaVersionResolver) SyncEventID(ctx context.Context, obj *ent.MediaVersion) (string, error) {
	return uuidStr(obj.SyncEventID), nil
}

func (r *mediaVersionResolver) RawOutputID(ctx context.Context, obj *ent.MediaVersion) (*string, error) {
	return uuidPtr(obj.RawOutputID), nil
}

// ============================================================
// MemberResolver
// ============================================================

func (r *memberResolver) CurrentVersionID(ctx context.Context, obj *ent.Member) (*string, error) {
	return uuidPtr(obj.CurrentVersionID), nil
}

// ============================================================
// MemberVersionResolver
// ============================================================

func (r *memberVersionResolver) SyncEventID(ctx context.Context, obj *ent.MemberVersion) (string, error) {
	return uuidStr(obj.SyncEventID), nil
}

func (r *memberVersionResolver) RawOutputID(ctx context.Context, obj *ent.MemberVersion) (*string, error) {
	return uuidPtr(obj.RawOutputID), nil
}

// ============================================================
// OfficeResolver
// ============================================================

func (r *officeResolver) CurrentVersionID(ctx context.Context, obj *ent.Office) (*string, error) {
	return uuidPtr(obj.CurrentVersionID), nil
}

// ============================================================
// OfficeVersionResolver
// ============================================================

func (r *officeVersionResolver) SyncEventID(ctx context.Context, obj *ent.OfficeVersion) (string, error) {
	return uuidStr(obj.SyncEventID), nil
}

func (r *officeVersionResolver) RawOutputID(ctx context.Context, obj *ent.OfficeVersion) (*string, error) {
	return uuidPtr(obj.RawOutputID), nil
}

// ============================================================
// OpenHouseResolver
// ============================================================

func (r *openHouseResolver) CurrentVersionID(ctx context.Context, obj *ent.OpenHouse) (*string, error) {
	return uuidPtr(obj.CurrentVersionID), nil
}

// ============================================================
// OpenHouseVersionResolver
// ============================================================

func (r *openHouseVersionResolver) SyncEventID(ctx context.Context, obj *ent.OpenHouseVersion) (string, error) {
	return uuidStr(obj.SyncEventID), nil
}

func (r *openHouseVersionResolver) RawOutputID(ctx context.Context, obj *ent.OpenHouseVersion) (*string, error) {
	return uuidPtr(obj.RawOutputID), nil
}

// ============================================================
// PropertyResolver — StringArray + UUID conversions
//
// Decimal and int16 fields are autobound by gqlgen (graph/scalar.Decimal and
// the Int/Int16 model binding in gqlgen.yml), so they no longer need manual
// conversion resolvers. StringArray (pq.StringArray → []string) and UUID
// (uuid.UUID → string) still do.
// ============================================================

func (r *propertyResolver) Appliances(ctx context.Context, obj *ent.Property) (any, error) {
	return stringArray(obj.Appliances), nil
}
func (r *propertyResolver) Cooling(ctx context.Context, obj *ent.Property) (any, error) {
	return stringArray(obj.Cooling), nil
}
func (r *propertyResolver) Heating(ctx context.Context, obj *ent.Property) (any, error) {
	return stringArray(obj.Heating), nil
}
func (r *propertyResolver) Flooring(ctx context.Context, obj *ent.Property) (any, error) {
	return stringArray(obj.Flooring), nil
}
func (r *propertyResolver) Roof(ctx context.Context, obj *ent.Property) (any, error) {
	return stringArray(obj.Roof), nil
}
func (r *propertyResolver) ExteriorFeatures(ctx context.Context, obj *ent.Property) (any, error) {
	return stringArray(obj.ExteriorFeatures), nil
}
func (r *propertyResolver) InteriorFeatures(ctx context.Context, obj *ent.Property) (any, error) {
	return stringArray(obj.InteriorFeatures), nil
}
func (r *propertyResolver) ParkingFeatures(ctx context.Context, obj *ent.Property) (any, error) {
	return stringArray(obj.ParkingFeatures), nil
}
func (r *propertyResolver) PoolFeatures(ctx context.Context, obj *ent.Property) (any, error) {
	return stringArray(obj.PoolFeatures), nil
}
func (r *propertyResolver) View(ctx context.Context, obj *ent.Property) (any, error) {
	return stringArray(obj.View), nil
}
func (r *propertyResolver) WaterfrontFeatures(ctx context.Context, obj *ent.Property) (any, error) {
	return stringArray(obj.WaterfrontFeatures), nil
}
func (r *propertyResolver) CommunityFeatures(ctx context.Context, obj *ent.Property) (any, error) {
	return stringArray(obj.CommunityFeatures), nil
}
func (r *propertyResolver) AccessibilityFeatures(ctx context.Context, obj *ent.Property) (any, error) {
	return stringArray(obj.AccessibilityFeatures), nil
}
func (r *propertyResolver) Utilities(ctx context.Context, obj *ent.Property) (any, error) {
	return stringArray(obj.Utilities), nil
}
func (r *propertyResolver) Sewer(ctx context.Context, obj *ent.Property) (any, error) {
	return stringArray(obj.Sewer), nil
}
func (r *propertyResolver) WaterSource(ctx context.Context, obj *ent.Property) (any, error) {
	return stringArray(obj.WaterSource), nil
}
func (r *propertyResolver) LotFeatures(ctx context.Context, obj *ent.Property) (any, error) {
	return stringArray(obj.LotFeatures), nil
}
func (r *propertyResolver) PatioAndPorchFeatures(ctx context.Context, obj *ent.Property) (any, error) {
	return stringArray(obj.PatioAndPorchFeatures), nil
}
func (r *propertyResolver) SecurityFeatures(ctx context.Context, obj *ent.Property) (any, error) {
	return stringArray(obj.SecurityFeatures), nil
}
func (r *propertyResolver) ConstructionMaterials(ctx context.Context, obj *ent.Property) (any, error) {
	return stringArray(obj.ConstructionMaterials), nil
}
func (r *propertyResolver) FoundationDetails(ctx context.Context, obj *ent.Property) (any, error) {
	return stringArray(obj.FoundationDetails), nil
}
func (r *propertyResolver) Levels(ctx context.Context, obj *ent.Property) (any, error) {
	return stringArray(obj.Levels), nil
}
func (r *propertyResolver) FireplaceFeatures(ctx context.Context, obj *ent.Property) (any, error) {
	return stringArray(obj.FireplaceFeatures), nil
}
func (r *propertyResolver) SpaFeatures(ctx context.Context, obj *ent.Property) (any, error) {
	return stringArray(obj.SpaFeatures), nil
}
func (r *propertyResolver) Fencing(ctx context.Context, obj *ent.Property) (any, error) {
	return stringArray(obj.Fencing), nil
}
func (r *propertyResolver) HorseAmenities(ctx context.Context, obj *ent.Property) (any, error) {
	return stringArray(obj.HorseAmenities), nil
}
func (r *propertyResolver) WindowFeatures(ctx context.Context, obj *ent.Property) (any, error) {
	return stringArray(obj.WindowFeatures), nil
}
func (r *propertyResolver) PetsAllowed(ctx context.Context, obj *ent.Property) (any, error) {
	return stringArray(obj.PetsAllowed), nil
}
func (r *propertyResolver) Disclosures(ctx context.Context, obj *ent.Property) (any, error) {
	return stringArray(obj.Disclosures), nil
}
func (r *propertyResolver) PropertyCondition(ctx context.Context, obj *ent.Property) (any, error) {
	return stringArray(obj.PropertyCondition), nil
}
func (r *propertyResolver) SpecialListingConditions(ctx context.Context, obj *ent.Property) (any, error) {
	return stringArray(obj.SpecialListingConditions), nil
}
func (r *propertyResolver) GreenEnergyEfficient(ctx context.Context, obj *ent.Property) (any, error) {
	return stringArray(obj.GreenEnergyEfficient), nil
}
func (r *propertyResolver) GreenSustainability(ctx context.Context, obj *ent.Property) (any, error) {
	return stringArray(obj.GreenSustainability), nil
}
func (r *propertyResolver) SyndicateTo(ctx context.Context, obj *ent.Property) (any, error) {
	return stringArray(obj.SyndicateTo), nil
}
func (r *propertyResolver) CurrentVersionID(ctx context.Context, obj *ent.Property) (*string, error) {
	return uuidPtr(obj.CurrentVersionID), nil
}

// ============================================================
// Property.media + 8 agent/office soft-key resolvers, Member.office
// ============================================================
//
// Property.media is a polymorphic query (resource_type+resource_record_key)
// with no ent edge. The eight agent/office resolvers and Member.office wrap
// soft keys: the *_key columns have no DB FK because real MLS feeds reference
// agents and offices that are absent from the feed (retired, transferred,
// out of subscription). A non-null key with a null resolved entity is a
// VALID state — the consumer still sees the *Key string field. Every
// resolver applies mlg_can_view=true so tombstoned-but-present rows resolve
// null too.

func (r *propertyResolver) Media(ctx context.Context, obj *ent.Property) ([]*ent.Media, error) {
	return r.client.Media.Query().
		Where(
			entmedia.ResourceTypeEQ(entmedia.ResourceTypeProperty),
			entmedia.ResourceRecordKeyEQ(obj.ID),
			entmedia.MlgCanView(true),
		).
		All(ctx)
}

// PrimaryPhoto ranks preferred_photo_yn true > false > NULL, then ascending
// `order` — so one query returns the flagged photo when present and falls
// back to the lowest-order visible row otherwise.
func (r *propertyResolver) PrimaryPhoto(ctx context.Context, obj *ent.Property) (*ent.Media, error) {
	m, err := r.client.Media.Query().
		Where(
			entmedia.ResourceTypeEQ(entmedia.ResourceTypeProperty),
			entmedia.ResourceRecordKeyEQ(obj.ID),
			entmedia.MlgCanView(true),
		).
		Order(
			entmedia.ByPreferredPhotoYn(sql.OrderDesc(), sql.OrderNullsLast()),
			entmedia.ByOrder(sql.OrderNullsLast()),
		).
		First(ctx)
	if ent.IsNotFound(err) {
		return nil, nil
	}
	return m, err
}

func (r *propertyResolver) resolveMember(ctx context.Context, key *string) (*ent.Member, error) {
	if key == nil {
		return nil, nil
	}
	m, err := r.client.Member.Query().
		Where(member.IDEQ(*key), member.MlgCanView(true)).
		Only(ctx)
	if ent.IsNotFound(err) {
		return nil, nil
	}
	return m, err
}

func (r *propertyResolver) resolveOffice(ctx context.Context, key *string) (*ent.Office, error) {
	if key == nil {
		return nil, nil
	}
	o, err := r.client.Office.Query().
		Where(office.IDEQ(*key), office.MlgCanView(true)).
		Only(ctx)
	if ent.IsNotFound(err) {
		return nil, nil
	}
	return o, err
}

func (r *propertyResolver) ListAgent(ctx context.Context, obj *ent.Property) (*ent.Member, error) {
	return r.resolveMember(ctx, obj.ListAgentKey)
}
func (r *propertyResolver) CoListAgent(ctx context.Context, obj *ent.Property) (*ent.Member, error) {
	return r.resolveMember(ctx, obj.CoListAgentKey)
}
func (r *propertyResolver) BuyerAgent(ctx context.Context, obj *ent.Property) (*ent.Member, error) {
	return r.resolveMember(ctx, obj.BuyerAgentKey)
}
func (r *propertyResolver) CoBuyerAgent(ctx context.Context, obj *ent.Property) (*ent.Member, error) {
	return r.resolveMember(ctx, obj.CoBuyerAgentKey)
}
func (r *propertyResolver) ListOffice(ctx context.Context, obj *ent.Property) (*ent.Office, error) {
	return r.resolveOffice(ctx, obj.ListOfficeKey)
}
func (r *propertyResolver) CoListOffice(ctx context.Context, obj *ent.Property) (*ent.Office, error) {
	return r.resolveOffice(ctx, obj.CoListOfficeKey)
}
func (r *propertyResolver) BuyerOffice(ctx context.Context, obj *ent.Property) (*ent.Office, error) {
	return r.resolveOffice(ctx, obj.BuyerOfficeKey)
}
func (r *propertyResolver) CoBuyerOffice(ctx context.Context, obj *ent.Property) (*ent.Office, error) {
	return r.resolveOffice(ctx, obj.CoBuyerOfficeKey)
}

func (r *memberResolver) Office(ctx context.Context, obj *ent.Member) (*ent.Office, error) {
	if obj.OfficeKey == nil {
		return nil, nil
	}
	o, err := r.client.Office.Query().
		Where(office.IDEQ(*obj.OfficeKey), office.MlgCanView(true)).
		Only(ctx)
	if ent.IsNotFound(err) {
		return nil, nil
	}
	return o, err
}

// ============================================================
// PropertyRoomResolver
// ============================================================

func (r *propertyRoomResolver) RoomFeatures(ctx context.Context, obj *ent.PropertyRoom) (any, error) {
	return stringArray(obj.RoomFeatures), nil
}

func (r *propertyRoomResolver) CurrentVersionID(ctx context.Context, obj *ent.PropertyRoom) (*string, error) {
	return uuidPtr(obj.CurrentVersionID), nil
}

// ============================================================
// PropertyRoomVersionResolver
// ============================================================

func (r *propertyRoomVersionResolver) RoomFeatures(ctx context.Context, obj *ent.PropertyRoomVersion) (any, error) {
	return stringArray(obj.RoomFeatures), nil
}

func (r *propertyRoomVersionResolver) SyncEventID(ctx context.Context, obj *ent.PropertyRoomVersion) (string, error) {
	return uuidStr(obj.SyncEventID), nil
}

func (r *propertyRoomVersionResolver) RawOutputID(ctx context.Context, obj *ent.PropertyRoomVersion) (*string, error) {
	return uuidPtr(obj.RawOutputID), nil
}

// ============================================================
// PropertyUnitTypeResolver
// ============================================================

func (r *propertyUnitTypeResolver) CurrentVersionID(ctx context.Context, obj *ent.PropertyUnitType) (*string, error) {
	return uuidPtr(obj.CurrentVersionID), nil
}

// ============================================================
// PropertyUnitTypeVersionResolver
// ============================================================

func (r *propertyUnitTypeVersionResolver) SyncEventID(ctx context.Context, obj *ent.PropertyUnitTypeVersion) (string, error) {
	return uuidStr(obj.SyncEventID), nil
}

func (r *propertyUnitTypeVersionResolver) RawOutputID(ctx context.Context, obj *ent.PropertyUnitTypeVersion) (*string, error) {
	return uuidPtr(obj.RawOutputID), nil
}

// ============================================================
// PropertyVersionResolver — StringArray + UUID conversions
// (Decimal/int16 fields are autobound, same as PropertyResolver above.)
// ============================================================

func (r *propertyVersionResolver) Appliances(ctx context.Context, obj *ent.PropertyVersion) (any, error) {
	return stringArray(obj.Appliances), nil
}
func (r *propertyVersionResolver) Cooling(ctx context.Context, obj *ent.PropertyVersion) (any, error) {
	return stringArray(obj.Cooling), nil
}
func (r *propertyVersionResolver) Heating(ctx context.Context, obj *ent.PropertyVersion) (any, error) {
	return stringArray(obj.Heating), nil
}
func (r *propertyVersionResolver) Flooring(ctx context.Context, obj *ent.PropertyVersion) (any, error) {
	return stringArray(obj.Flooring), nil
}
func (r *propertyVersionResolver) Roof(ctx context.Context, obj *ent.PropertyVersion) (any, error) {
	return stringArray(obj.Roof), nil
}
func (r *propertyVersionResolver) ExteriorFeatures(ctx context.Context, obj *ent.PropertyVersion) (any, error) {
	return stringArray(obj.ExteriorFeatures), nil
}
func (r *propertyVersionResolver) InteriorFeatures(ctx context.Context, obj *ent.PropertyVersion) (any, error) {
	return stringArray(obj.InteriorFeatures), nil
}
func (r *propertyVersionResolver) ParkingFeatures(ctx context.Context, obj *ent.PropertyVersion) (any, error) {
	return stringArray(obj.ParkingFeatures), nil
}
func (r *propertyVersionResolver) PoolFeatures(ctx context.Context, obj *ent.PropertyVersion) (any, error) {
	return stringArray(obj.PoolFeatures), nil
}
func (r *propertyVersionResolver) View(ctx context.Context, obj *ent.PropertyVersion) (any, error) {
	return stringArray(obj.View), nil
}
func (r *propertyVersionResolver) WaterfrontFeatures(ctx context.Context, obj *ent.PropertyVersion) (any, error) {
	return stringArray(obj.WaterfrontFeatures), nil
}
func (r *propertyVersionResolver) CommunityFeatures(ctx context.Context, obj *ent.PropertyVersion) (any, error) {
	return stringArray(obj.CommunityFeatures), nil
}
func (r *propertyVersionResolver) AccessibilityFeatures(ctx context.Context, obj *ent.PropertyVersion) (any, error) {
	return stringArray(obj.AccessibilityFeatures), nil
}
func (r *propertyVersionResolver) Utilities(ctx context.Context, obj *ent.PropertyVersion) (any, error) {
	return stringArray(obj.Utilities), nil
}
func (r *propertyVersionResolver) Sewer(ctx context.Context, obj *ent.PropertyVersion) (any, error) {
	return stringArray(obj.Sewer), nil
}
func (r *propertyVersionResolver) WaterSource(ctx context.Context, obj *ent.PropertyVersion) (any, error) {
	return stringArray(obj.WaterSource), nil
}
func (r *propertyVersionResolver) LotFeatures(ctx context.Context, obj *ent.PropertyVersion) (any, error) {
	return stringArray(obj.LotFeatures), nil
}
func (r *propertyVersionResolver) PatioAndPorchFeatures(ctx context.Context, obj *ent.PropertyVersion) (any, error) {
	return stringArray(obj.PatioAndPorchFeatures), nil
}
func (r *propertyVersionResolver) SecurityFeatures(ctx context.Context, obj *ent.PropertyVersion) (any, error) {
	return stringArray(obj.SecurityFeatures), nil
}
func (r *propertyVersionResolver) ConstructionMaterials(ctx context.Context, obj *ent.PropertyVersion) (any, error) {
	return stringArray(obj.ConstructionMaterials), nil
}
func (r *propertyVersionResolver) FoundationDetails(ctx context.Context, obj *ent.PropertyVersion) (any, error) {
	return stringArray(obj.FoundationDetails), nil
}
func (r *propertyVersionResolver) Levels(ctx context.Context, obj *ent.PropertyVersion) (any, error) {
	return stringArray(obj.Levels), nil
}
func (r *propertyVersionResolver) FireplaceFeatures(ctx context.Context, obj *ent.PropertyVersion) (any, error) {
	return stringArray(obj.FireplaceFeatures), nil
}
func (r *propertyVersionResolver) SpaFeatures(ctx context.Context, obj *ent.PropertyVersion) (any, error) {
	return stringArray(obj.SpaFeatures), nil
}
func (r *propertyVersionResolver) Fencing(ctx context.Context, obj *ent.PropertyVersion) (any, error) {
	return stringArray(obj.Fencing), nil
}
func (r *propertyVersionResolver) HorseAmenities(ctx context.Context, obj *ent.PropertyVersion) (any, error) {
	return stringArray(obj.HorseAmenities), nil
}
func (r *propertyVersionResolver) WindowFeatures(ctx context.Context, obj *ent.PropertyVersion) (any, error) {
	return stringArray(obj.WindowFeatures), nil
}
func (r *propertyVersionResolver) PetsAllowed(ctx context.Context, obj *ent.PropertyVersion) (any, error) {
	return stringArray(obj.PetsAllowed), nil
}
func (r *propertyVersionResolver) Disclosures(ctx context.Context, obj *ent.PropertyVersion) (any, error) {
	return stringArray(obj.Disclosures), nil
}
func (r *propertyVersionResolver) PropertyCondition(ctx context.Context, obj *ent.PropertyVersion) (any, error) {
	return stringArray(obj.PropertyCondition), nil
}
func (r *propertyVersionResolver) SpecialListingConditions(ctx context.Context, obj *ent.PropertyVersion) (any, error) {
	return stringArray(obj.SpecialListingConditions), nil
}
func (r *propertyVersionResolver) GreenEnergyEfficient(ctx context.Context, obj *ent.PropertyVersion) (any, error) {
	return stringArray(obj.GreenEnergyEfficient), nil
}
func (r *propertyVersionResolver) GreenSustainability(ctx context.Context, obj *ent.PropertyVersion) (any, error) {
	return stringArray(obj.GreenSustainability), nil
}
func (r *propertyVersionResolver) SyndicateTo(ctx context.Context, obj *ent.PropertyVersion) (any, error) {
	return stringArray(obj.SyndicateTo), nil
}
func (r *propertyVersionResolver) SyncEventID(ctx context.Context, obj *ent.PropertyVersion) (string, error) {
	return uuidStr(obj.SyncEventID), nil
}
func (r *propertyVersionResolver) RawOutputID(ctx context.Context, obj *ent.PropertyVersion) (*string, error) {
	return uuidPtr(obj.RawOutputID), nil
}
