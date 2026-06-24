package processor

import (
	"context"
	"fmt"
	"reflect"
	"sort"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	entmedia "github.com/LunarHUE/MLS-Grid-Sync/ent/media"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/mediaversion"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/member"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/memberversion"
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
	"ID":                    true, // primary key, equal by construction
	"CreatedAt":             true,
	"ModifiedAt":            true,
	"ValidFrom":             true,
	"ValidTo":               true,
	"ChangeType":            true,
	"ChangedFields":         true,
	"ProcessorVersion":      true,
	"SourceModifiedAt":      true, // identifies the version, not the data
	"OriginatingSystem":     true,
	"MlgCanView":            true, // tombstones already excluded at the query layer
	"MlgCanUse":             true,
	"ExtendedFields":        true, // jsonb compared structurally elsewhere if needed
	"ParentListingKey":      true, // parking column, not part of the data contract
	"ListingKey":            true, // natural-key fields the version mirrors
	"MemberKey":             true,
	"OfficeKey":             true,
	"OpenHouseKey":          true,
	"MediaKey":              true,
	"RoomKey":               true,
	"UnitTypeKey":           true,
	"ResourceType":          true, // Media polymorphic discriminator
	"ResourceRecordKey":     true,
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
		return &typedDispatch{run: func(ctx context.Context, client *ent.Client, out *TypedDriftReport) error {
			return scanVersioned(out,
				func() ([]*ent.Property, error) {
					return client.Property.Query().Where(property.MlgCanView(true)).All(ctx)
				},
				func() ([]*ent.PropertyVersion, error) {
					return client.PropertyVersion.Query().Where(propertyversion.ValidToIsNil()).All(ctx)
				},
				func(e *ent.Property) string { return e.ID },
				func(v *ent.PropertyVersion) string { return v.ListingKey },
			)
		}}, true
	case rawoutput.ResourceMember:
		return &typedDispatch{run: func(ctx context.Context, client *ent.Client, out *TypedDriftReport) error {
			return scanVersioned(out,
				func() ([]*ent.Member, error) { return client.Member.Query().Where(member.MlgCanView(true)).All(ctx) },
				func() ([]*ent.MemberVersion, error) {
					return client.MemberVersion.Query().Where(memberversion.ValidToIsNil()).All(ctx)
				},
				func(e *ent.Member) string { return e.ID },
				func(v *ent.MemberVersion) string { return v.MemberKey },
			)
		}}, true
	case rawoutput.ResourceOffice:
		return &typedDispatch{run: func(ctx context.Context, client *ent.Client, out *TypedDriftReport) error {
			return scanVersioned(out,
				func() ([]*ent.Office, error) { return client.Office.Query().Where(office.MlgCanView(true)).All(ctx) },
				func() ([]*ent.OfficeVersion, error) {
					return client.OfficeVersion.Query().Where(officeversion.ValidToIsNil()).All(ctx)
				},
				func(e *ent.Office) string { return e.ID },
				func(v *ent.OfficeVersion) string { return v.OfficeKey },
			)
		}}, true
	case rawoutput.ResourceOpenHouse:
		return &typedDispatch{run: func(ctx context.Context, client *ent.Client, out *TypedDriftReport) error {
			return scanVersioned(out,
				func() ([]*ent.OpenHouse, error) {
					return client.OpenHouse.Query().Where(openhouse.MlgCanView(true)).All(ctx)
				},
				func() ([]*ent.OpenHouseVersion, error) {
					return client.OpenHouseVersion.Query().Where(openhouseversion.ValidToIsNil()).All(ctx)
				},
				func(e *ent.OpenHouse) string { return e.ID },
				func(v *ent.OpenHouseVersion) string { return v.OpenHouseKey },
			)
		}}, true
	case rawoutput.ResourceMedia:
		return &typedDispatch{run: func(ctx context.Context, client *ent.Client, out *TypedDriftReport) error {
			return scanVersioned(out,
				func() ([]*ent.Media, error) { return client.Media.Query().Where(entmedia.MlgCanView(true)).All(ctx) },
				func() ([]*ent.MediaVersion, error) {
					return client.MediaVersion.Query().Where(mediaversion.ValidToIsNil()).All(ctx)
				},
				func(e *ent.Media) string { return e.ID },
				func(v *ent.MediaVersion) string { return v.MediaKey },
			)
		}}, true
	case rawoutput.ResourcePropertyRooms:
		return &typedDispatch{run: func(ctx context.Context, client *ent.Client, out *TypedDriftReport) error {
			return scanVersioned(out,
				func() ([]*ent.PropertyRoom, error) {
					return client.PropertyRoom.Query().Where(propertyroom.MlgCanView(true)).All(ctx)
				},
				func() ([]*ent.PropertyRoomVersion, error) {
					return client.PropertyRoomVersion.Query().Where(propertyroomversion.ValidToIsNil()).All(ctx)
				},
				func(e *ent.PropertyRoom) string { return e.ID },
				func(v *ent.PropertyRoomVersion) string { return v.RoomKey },
			)
		}}, true
	case rawoutput.ResourcePropertyUnitTypes:
		return &typedDispatch{run: func(ctx context.Context, client *ent.Client, out *TypedDriftReport) error {
			return scanVersioned(out,
				func() ([]*ent.PropertyUnitType, error) {
					return client.PropertyUnitType.Query().Where(propertyunittype.MlgCanView(true)).All(ctx)
				},
				func() ([]*ent.PropertyUnitTypeVersion, error) {
					return client.PropertyUnitTypeVersion.Query().Where(propertyunittypeversion.ValidToIsNil()).All(ctx)
				},
				func(e *ent.PropertyUnitType) string { return e.ID },
				func(v *ent.PropertyUnitTypeVersion) string { return v.UnitTypeKey },
			)
		}}, true
	}
	return nil, false
}

// scanVersioned is the shared drift sweep every resource runs:
//  1. fetch visible (mlg_can_view=true) entities,
//  2. fetch current open versions and key them by their natural key,
//  3. compareStructs via reflection on shared field names, recording a
//     synthetic mismatch for any entity with no open version.
//
// The per-resource adapters (loaders + key extractors) are the only thing that
// varies; folding the loop here also gives every resource the same query-error
// wrapping (previously only Property wrapped its errors).
func scanVersioned[E any, V any](
	out *TypedDriftReport,
	loadEntities func() ([]*E, error),
	loadVersions func() ([]*V, error),
	entityID func(*E) string,
	versionKey func(*V) string,
) error {
	entities, err := loadEntities()
	if err != nil {
		return fmt.Errorf("scan %s entities: %w", out.Resource, err)
	}
	versions, err := loadVersions()
	if err != nil {
		return fmt.Errorf("scan %s current versions: %w", out.Resource, err)
	}
	byKey := make(map[string]*V, len(versions))
	for _, v := range versions {
		byKey[versionKey(v)] = v
	}
	for _, e := range entities {
		out.EntitiesSeen++
		id := entityID(e)
		v, ok := byKey[id]
		if !ok {
			out.Mismatches = append(out.Mismatches, TypedDriftMismatch{
				EntityID: id, Field: "<missing version>",
				EntityValue: "present", VersionVal: "absent",
			})
			continue
		}
		compareStructs(out, id, reflect.ValueOf(*e), reflect.ValueOf(*v))
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
