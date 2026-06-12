package processor

import (
	"context"
	"fmt"
	"reflect"
	"sort"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/member"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/memberversion"
	entmedia "github.com/LunarHUE/MLS-Grid-Sync/ent/media"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/mediaversion"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/office"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/officeversion"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/openhouse"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/openhouseversion"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/property"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/propertyroom"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/propertyroomversion"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/propertyunittype"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/propertyunittypeversion"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/propertyversion"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
)

// TypedDriftReport summarizes the result of one resource's entity-vs-
// current-version drift sweep. ent ID values are stored as strings so
// the report is rendering-agnostic (worker output prints them; an
// integration test diffs them).
type TypedDriftReport struct {
	Resource     rawoutput.Resource
	EntitiesSeen int
	Mismatches   []TypedDriftMismatch
}

type TypedDriftMismatch struct {
	EntityID    string
	Field       string
	EntityValue string
	VersionVal  string
}

// AllTypedDriftResources lists every resource ValidateTyped scans.
// Lookup is excluded — it has no version table.
var AllTypedDriftResources = []rawoutput.Resource{
	rawoutput.ResourceProperty,
	rawoutput.ResourceMember,
	rawoutput.ResourceOffice,
	rawoutput.ResourceOpenHouse,
	rawoutput.ResourceMedia,
	rawoutput.ResourcePropertyRooms,
	rawoutput.ResourcePropertyUnitTypes,
}

// ValidateTyped streams every visible (mlg_can_view=true) entity row
// for resource and compares it field-by-field to its current open
// version row. Tombstoned entities are skipped — the Phase 3 delete
// branch intentionally leaves last-known field values on the entity
// while the delete version is sparse, so drift on tombstones is
// expected and not a regression signal.
//
// The reflection sweep uses each entity type's per-resource field list
// (curated to exclude metadata: created_at, modified_at, valid_from/to,
// processor_version, changed_fields). Whenever this comparator returns
// non-empty Mismatches on a clean re-sync, drift exists and the version
// history is the source of truth — see [[feedback-dev-db-reset]] for
// the escape hatch.
func ValidateTyped(ctx context.Context, client *ent.Client, resource rawoutput.Resource) (*TypedDriftReport, error) {
	d, ok := typedDispatchFor(resource)
	if !ok {
		return nil, fmt.Errorf("validate-typed: no dispatch for resource %q", resource)
	}
	report := &TypedDriftReport{Resource: resource}
	if err := d.run(ctx, client, report); err != nil {
		return nil, err
	}
	return report, nil
}

// typedDispatch carries the per-resource adapter ValidateTyped needs.
// run is a closure rather than a struct of bare functions because the
// entity and version types differ per resource and Go generics on
// reflect.Value-only paths buy nothing — the closure keeps the typed
// loaders local without inflicting `any` on the public API.
type typedDispatch struct {
	run func(ctx context.Context, client *ent.Client, out *TypedDriftReport) error
}

// commonSkipFields is the field-name set excluded from every drift
// compare. Metadata, FK keys, audit columns, and the version-row-only
// columns all live here.
var commonSkipFields = map[string]bool{
	"ID":                  true, // primary key, equal by construction
	"CreatedAt":           true,
	"ModifiedAt":          true,
	"ValidFrom":           true,
	"ValidTo":             true,
	"ChangeType":          true,
	"ChangedFields":       true,
	"ProcessorVersion":    true,
	"SourceModifiedAt":    true, // identifies the version, not the data
	"OriginatingSystem":   true,
	"MlgCanView":          true, // tombstones already excluded at the query layer
	"MlgCanUse":           true,
	"ExtendedFields":      true, // jsonb compared structurally elsewhere if needed
	"ParentListingKey":    true, // parking column, not part of the data contract
	"ListingKey":          true, // natural-key fields the version mirrors
	"MemberKey":           true,
	"OfficeKey":           true,
	"OpenHouseKey":        true,
	"MediaKey":            true,
	"RoomKey":             true,
	"UnitTypeKey":         true,
	"ResourceType":        true, // Media polymorphic discriminator
	"ResourceRecordKey":   true,
	"OriginatingSystemName": true,
	// Version table FK back to the entity (varies by resource).
	"PropertyID":     true,
	"MemberID":       true,
	"OfficeID":       true,
	"OpenHouseID":    true,
	"MediaID":        true,
	"PropertyRoomID": true,
}

func typedDispatchFor(resource rawoutput.Resource) (*typedDispatch, bool) {
	switch resource {
	case rawoutput.ResourceProperty:
		return &typedDispatch{run: scanProperty}, true
	case rawoutput.ResourceMember:
		return &typedDispatch{run: scanMember}, true
	case rawoutput.ResourceOffice:
		return &typedDispatch{run: scanOffice}, true
	case rawoutput.ResourceOpenHouse:
		return &typedDispatch{run: scanOpenHouse}, true
	case rawoutput.ResourceMedia:
		return &typedDispatch{run: scanMedia}, true
	case rawoutput.ResourcePropertyRooms:
		return &typedDispatch{run: scanPropertyRoom}, true
	case rawoutput.ResourcePropertyUnitTypes:
		return &typedDispatch{run: scanPropertyUnitType}, true
	}
	return nil, false
}

// Per-resource scanners. Each one mirrors the same shape:
// 1. fetch visible entities,
// 2. fetch current open versions and key them,
// 3. compareStructs via reflection on shared field names.

func scanProperty(ctx context.Context, client *ent.Client, out *TypedDriftReport) error {
	entities, err := client.Property.Query().Where(property.MlgCanView(true)).All(ctx)
	if err != nil {
		return fmt.Errorf("scan property entities: %w", err)
	}
	versions, err := client.PropertyVersion.Query().Where(propertyversion.ValidToIsNil()).All(ctx)
	if err != nil {
		return fmt.Errorf("scan property current versions: %w", err)
	}
	byKey := make(map[string]*ent.PropertyVersion, len(versions))
	for _, v := range versions {
		byKey[v.ListingKey] = v
	}
	for _, e := range entities {
		out.EntitiesSeen++
		v, ok := byKey[e.ID]
		if !ok {
			out.Mismatches = append(out.Mismatches, TypedDriftMismatch{
				EntityID: e.ID, Field: "<missing version>",
				EntityValue: "present", VersionVal: "absent",
			})
			continue
		}
		compareStructs(out, e.ID, reflect.ValueOf(*e), reflect.ValueOf(*v))
	}
	return nil
}

func scanMember(ctx context.Context, client *ent.Client, out *TypedDriftReport) error {
	entities, err := client.Member.Query().Where(member.MlgCanView(true)).All(ctx)
	if err != nil {
		return err
	}
	versions, err := client.MemberVersion.Query().Where(memberversion.ValidToIsNil()).All(ctx)
	if err != nil {
		return err
	}
	byKey := make(map[string]*ent.MemberVersion, len(versions))
	for _, v := range versions {
		byKey[v.MemberKey] = v
	}
	for _, e := range entities {
		out.EntitiesSeen++
		v, ok := byKey[e.ID]
		if !ok {
			out.Mismatches = append(out.Mismatches, TypedDriftMismatch{EntityID: e.ID, Field: "<missing version>", EntityValue: "present", VersionVal: "absent"})
			continue
		}
		compareStructs(out, e.ID, reflect.ValueOf(*e), reflect.ValueOf(*v))
	}
	return nil
}

func scanOffice(ctx context.Context, client *ent.Client, out *TypedDriftReport) error {
	entities, err := client.Office.Query().Where(office.MlgCanView(true)).All(ctx)
	if err != nil {
		return err
	}
	versions, err := client.OfficeVersion.Query().Where(officeversion.ValidToIsNil()).All(ctx)
	if err != nil {
		return err
	}
	byKey := make(map[string]*ent.OfficeVersion, len(versions))
	for _, v := range versions {
		byKey[v.OfficeKey] = v
	}
	for _, e := range entities {
		out.EntitiesSeen++
		v, ok := byKey[e.ID]
		if !ok {
			out.Mismatches = append(out.Mismatches, TypedDriftMismatch{EntityID: e.ID, Field: "<missing version>", EntityValue: "present", VersionVal: "absent"})
			continue
		}
		compareStructs(out, e.ID, reflect.ValueOf(*e), reflect.ValueOf(*v))
	}
	return nil
}

func scanOpenHouse(ctx context.Context, client *ent.Client, out *TypedDriftReport) error {
	entities, err := client.OpenHouse.Query().Where(openhouse.MlgCanView(true)).All(ctx)
	if err != nil {
		return err
	}
	versions, err := client.OpenHouseVersion.Query().Where(openhouseversion.ValidToIsNil()).All(ctx)
	if err != nil {
		return err
	}
	byKey := make(map[string]*ent.OpenHouseVersion, len(versions))
	for _, v := range versions {
		byKey[v.OpenHouseKey] = v
	}
	for _, e := range entities {
		out.EntitiesSeen++
		v, ok := byKey[e.ID]
		if !ok {
			out.Mismatches = append(out.Mismatches, TypedDriftMismatch{EntityID: e.ID, Field: "<missing version>", EntityValue: "present", VersionVal: "absent"})
			continue
		}
		compareStructs(out, e.ID, reflect.ValueOf(*e), reflect.ValueOf(*v))
	}
	return nil
}

func scanMedia(ctx context.Context, client *ent.Client, out *TypedDriftReport) error {
	entities, err := client.Media.Query().Where(entmedia.MlgCanView(true)).All(ctx)
	if err != nil {
		return err
	}
	versions, err := client.MediaVersion.Query().Where(mediaversion.ValidToIsNil()).All(ctx)
	if err != nil {
		return err
	}
	byKey := make(map[string]*ent.MediaVersion, len(versions))
	for _, v := range versions {
		byKey[v.MediaKey] = v
	}
	for _, e := range entities {
		out.EntitiesSeen++
		v, ok := byKey[e.ID]
		if !ok {
			out.Mismatches = append(out.Mismatches, TypedDriftMismatch{EntityID: e.ID, Field: "<missing version>", EntityValue: "present", VersionVal: "absent"})
			continue
		}
		compareStructs(out, e.ID, reflect.ValueOf(*e), reflect.ValueOf(*v))
	}
	return nil
}

func scanPropertyRoom(ctx context.Context, client *ent.Client, out *TypedDriftReport) error {
	entities, err := client.PropertyRoom.Query().Where(propertyroom.MlgCanView(true)).All(ctx)
	if err != nil {
		return err
	}
	versions, err := client.PropertyRoomVersion.Query().Where(propertyroomversion.ValidToIsNil()).All(ctx)
	if err != nil {
		return err
	}
	byKey := make(map[string]*ent.PropertyRoomVersion, len(versions))
	for _, v := range versions {
		byKey[v.RoomKey] = v
	}
	for _, e := range entities {
		out.EntitiesSeen++
		v, ok := byKey[e.ID]
		if !ok {
			out.Mismatches = append(out.Mismatches, TypedDriftMismatch{EntityID: e.ID, Field: "<missing version>", EntityValue: "present", VersionVal: "absent"})
			continue
		}
		compareStructs(out, e.ID, reflect.ValueOf(*e), reflect.ValueOf(*v))
	}
	return nil
}

func scanPropertyUnitType(ctx context.Context, client *ent.Client, out *TypedDriftReport) error {
	entities, err := client.PropertyUnitType.Query().Where(propertyunittype.MlgCanView(true)).All(ctx)
	if err != nil {
		return err
	}
	versions, err := client.PropertyUnitTypeVersion.Query().Where(propertyunittypeversion.ValidToIsNil()).All(ctx)
	if err != nil {
		return err
	}
	byKey := make(map[string]*ent.PropertyUnitTypeVersion, len(versions))
	for _, v := range versions {
		byKey[v.UnitTypeKey] = v
	}
	for _, e := range entities {
		out.EntitiesSeen++
		v, ok := byKey[e.ID]
		if !ok {
			out.Mismatches = append(out.Mismatches, TypedDriftMismatch{EntityID: e.ID, Field: "<missing version>", EntityValue: "present", VersionVal: "absent"})
			continue
		}
		compareStructs(out, e.ID, reflect.ValueOf(*e), reflect.ValueOf(*v))
	}
	return nil
}

// compareStructs walks the entity struct's fields and, for each name
// also present on the version struct, applies reflect.DeepEqual. Skip
// names listed in commonSkipFields are excluded.
//
// Both arguments must be reflect.Value of struct kind. ent's entity
// rows are loaded as *T but we dereference at the call site so this
// helper only sees structs.
func compareStructs(out *TypedDriftReport, entityID string, entity, version reflect.Value) {
	et := entity.Type()
	vt := version.Type()
	for i := 0; i < et.NumField(); i++ {
		f := et.Field(i)
		// Skip unexported fields (ent embeds selectValues, config, etc.).
		// reflect.Value.Interface() panics on them.
		if f.PkgPath != "" {
			continue
		}
		name := f.Name
		if commonSkipFields[name] {
			continue
		}
		// Look up the same-named field on the version struct; if absent
		// or unexported there, skip.
		vField, ok := vt.FieldByName(name)
		if !ok || vField.PkgPath != "" {
			continue
		}
		vVal := version.FieldByName(name).Interface()
		eVal := entity.Field(i).Interface()
		if !reflect.DeepEqual(eVal, vVal) {
			out.Mismatches = append(out.Mismatches, TypedDriftMismatch{
				EntityID:    entityID,
				Field:       name,
				EntityValue: fmt.Sprintf("%v", eVal),
				VersionVal:  fmt.Sprintf("%v", vVal),
			})
		}
	}
}

// SortedMismatches returns the report's mismatches grouped by entity
// id, alphabetized — handy for stable CLI output.
func (r *TypedDriftReport) SortedMismatches() []TypedDriftMismatch {
	out := make([]TypedDriftMismatch, len(r.Mismatches))
	copy(out, r.Mismatches)
	sort.Slice(out, func(i, j int) bool {
		if out[i].EntityID != out[j].EntityID {
			return out[i].EntityID < out[j].EntityID
		}
		return out[i].Field < out[j].Field
	})
	return out
}
