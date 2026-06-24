package processor

import (
	"context"
	"fmt"
	"reflect"
	"sort"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
)

// ValidateReport summarizes the results of streaming every raw_output row for
// a resource through the parser without writing anything. Used by the
// validate-raw CLI to check that the field mapping matches real MLS Grid
// data, not just the spec.
type ValidateReport struct {
	Resource        rawoutput.Resource
	TotalRows       int
	ParseErrors     []ParseError
	UnconsumedKeys  map[string]int // key → count of payloads containing it
	AlwaysNilFields []string       // typed Fields struct fields that were nil/empty across every row
}

type ParseError struct {
	RawOutputID string
	Err         string
}

// validateDispatch is the per-resource adapter the generic sweep needs:
//   - parse a payload into the resource's typed Fields struct,
//   - access the parsed value as a reflect.Value (used for non-nil tracking),
//   - access the ExtendedFields map (the unconsumed-keys source).
type validateDispatch struct {
	parse           func(payload []byte) (any, error)
	extendedFields  func(parsed any) map[string]any
	fieldsValue     func(parsed any) reflect.Value
	zeroFieldsType  reflect.Type
	commonSkipNames map[string]bool
}

func dispatchFor(resource rawoutput.Resource) (*validateDispatch, error) {
	switch resource {
	case rawoutput.ResourceProperty:
		return &validateDispatch{
			parse: func(b []byte) (any, error) { return parseProperty(b) },
			extendedFields: func(v any) map[string]any {
				return v.(*PropertyFields).ExtendedFields
			},
			fieldsValue:    func(v any) reflect.Value { return reflect.ValueOf(v).Elem() },
			zeroFieldsType: reflect.TypeOf(PropertyFields{}),
			commonSkipNames: map[string]bool{
				"ListingKey":       true,
				"SourceModifiedAt": true,
				"MlgCanView":       true,
				"MlgCanUse":        true,
				"ExtendedFields":   true,
			},
		}, nil
	case rawoutput.ResourceMember:
		return &validateDispatch{
			parse:          func(b []byte) (any, error) { return parseMember(b) },
			extendedFields: func(v any) map[string]any { return v.(*MemberFields).ExtendedFields },
			fieldsValue:    func(v any) reflect.Value { return reflect.ValueOf(v).Elem() },
			zeroFieldsType: reflect.TypeOf(MemberFields{}),
			commonSkipNames: map[string]bool{
				"MemberKey": true, "SourceModifiedAt": true,
				"MlgCanView": true, "MlgCanUse": true, "ExtendedFields": true,
			},
		}, nil
	case rawoutput.ResourceOffice:
		return &validateDispatch{
			parse:          func(b []byte) (any, error) { return parseOffice(b) },
			extendedFields: func(v any) map[string]any { return v.(*OfficeFields).ExtendedFields },
			fieldsValue:    func(v any) reflect.Value { return reflect.ValueOf(v).Elem() },
			zeroFieldsType: reflect.TypeOf(OfficeFields{}),
			commonSkipNames: map[string]bool{
				"OfficeKey": true, "SourceModifiedAt": true,
				"MlgCanView": true, "MlgCanUse": true, "ExtendedFields": true,
			},
		}, nil
	case rawoutput.ResourceOpenHouse:
		return &validateDispatch{
			parse:          func(b []byte) (any, error) { return parseOpenHouse(b) },
			extendedFields: func(v any) map[string]any { return v.(*OpenHouseFields).ExtendedFields },
			fieldsValue:    func(v any) reflect.Value { return reflect.ValueOf(v).Elem() },
			zeroFieldsType: reflect.TypeOf(OpenHouseFields{}),
			commonSkipNames: map[string]bool{
				"OpenHouseKey": true, "ListingKey": true, "SourceModifiedAt": true,
				"MlgCanView": true, "MlgCanUse": true, "ExtendedFields": true,
			},
		}, nil
	case rawoutput.ResourceMedia:
		return &validateDispatch{
			parse:          func(b []byte) (any, error) { return parseMedia(b) },
			extendedFields: func(v any) map[string]any { return v.(*MediaFields).ExtendedFields },
			fieldsValue:    func(v any) reflect.Value { return reflect.ValueOf(v).Elem() },
			zeroFieldsType: reflect.TypeOf(MediaFields{}),
			commonSkipNames: map[string]bool{
				"MediaKey": true, "SourceModifiedAt": true,
				"MlgCanView": true, "MlgCanUse": true,
				"ResourceType": true, "ResourceRecordKey": true,
				"ExtendedFields": true,
			},
		}, nil
	case rawoutput.ResourcePropertyRooms:
		return &validateDispatch{
			parse:          func(b []byte) (any, error) { return parsePropertyRoom(b) },
			extendedFields: func(v any) map[string]any { return v.(*PropertyRoomFields).ExtendedFields },
			fieldsValue:    func(v any) reflect.Value { return reflect.ValueOf(v).Elem() },
			zeroFieldsType: reflect.TypeOf(PropertyRoomFields{}),
			commonSkipNames: map[string]bool{
				"RoomKey": true, "ListingKey": true, "SourceModifiedAt": true,
				"MlgCanView": true, "MlgCanUse": true, "ExtendedFields": true,
			},
		}, nil
	case rawoutput.ResourcePropertyUnitTypes:
		return &validateDispatch{
			parse:          func(b []byte) (any, error) { return parsePropertyUnitType(b) },
			extendedFields: func(v any) map[string]any { return v.(*PropertyUnitTypeFields).ExtendedFields },
			fieldsValue:    func(v any) reflect.Value { return reflect.ValueOf(v).Elem() },
			zeroFieldsType: reflect.TypeOf(PropertyUnitTypeFields{}),
			commonSkipNames: map[string]bool{
				"UnitTypeKey": true, "ListingKey": true, "SourceModifiedAt": true,
				"MlgCanView": true, "MlgCanUse": true, "ExtendedFields": true,
			},
		}, nil
	case rawoutput.ResourceLookup:
		return &validateDispatch{
			parse:          func(b []byte) (any, error) { return parseLookup(b) },
			extendedFields: func(v any) map[string]any { return nil }, // Lookup has no ExtendedFields column
			fieldsValue:    func(v any) reflect.Value { return reflect.ValueOf(v).Elem() },
			zeroFieldsType: reflect.TypeOf(LookupFields{}),
			commonSkipNames: map[string]bool{
				"LookupKey": true, "SourceModifiedAt": true,
				"MlgCanView": true, "MlgCanUse": true, "ExtendedFields": true,
				// Required-not-optional fields:
				"LookupName": true, "LookupValue": true,
			},
		}, nil
	default:
		return nil, fmt.Errorf("validate-raw: no dispatch for resource %q", resource)
	}
}

// AllValidatedResources lists every resource the validate-raw CLI's "all"
// mode sweeps AND is the canonical FK-dependency order for processor passes
// and reprocess. Office must precede Member because member.office_key →
// office; Property precedes its children (OpenHouse/Media/Rooms/UnitTypes)
// for the same reason.
var AllValidatedResources = []rawoutput.Resource{
	rawoutput.ResourceLookup,
	rawoutput.ResourceOffice,
	rawoutput.ResourceMember,
	rawoutput.ResourceProperty,
	rawoutput.ResourceOpenHouse,
	rawoutput.ResourceMedia,
	rawoutput.ResourcePropertyRooms,
	rawoutput.ResourcePropertyUnitTypes,
}

// FetchableResources lists the resources `init` and the sync daemon fetch
// standalone from the MLS Grid v2 OData API. The three resources missing
// from this list — Media, PropertyRooms, PropertyUnitTypes — are expand-only:
// MLS Grid returns 501 for /v2/Media, and Rooms / UnitTypes are not
// top-level RESO resources at all. They are delivered inside Property
// payloads via $expand=Media,Rooms,UnitTypes and split out at sync time
// (see sync/raw.go splitExpandedChildren).
//
// Order is a prefix-ordered subset of AllValidatedResources. The fetch-
// list pin test asserts both the membership and the absence of the
// expand-only resources — regression here is someone "helpfully" re-adding
// Media to the fetch loop and recreating the 501.
var FetchableResources = []rawoutput.Resource{
	rawoutput.ResourceLookup,
	rawoutput.ResourceOffice,
	rawoutput.ResourceMember,
	rawoutput.ResourceProperty,
	rawoutput.ResourceOpenHouse,
}

// ChildResources returns the expand-only child resources whose raw_output
// rows ride a fetch of r. Property carries Media, PropertyRooms, and
// PropertyUnitTypes (in dependency order — the parent must already be
// typed when child processors run); every other resource has no children.
//
// runProcessor uses this to iterate self + children: a Property fetch
// triggers four processor passes (property → media → property_rooms →
// property_unit_types), one per resource whose raw_output rows the fetch
// just landed.
func ChildResources(r rawoutput.Resource) []rawoutput.Resource {
	if r == rawoutput.ResourceProperty {
		return []rawoutput.Resource{
			rawoutput.ResourceMedia,
			rawoutput.ResourcePropertyRooms,
			rawoutput.ResourcePropertyUnitTypes,
		}
	}
	return nil
}

// IsExpandOnly reports whether r is delivered to raw_output only as an
// expanded child of another resource's fetch. `import` and `sync` use this
// to reject expand-only resources with a friendly error rather than letting
// them silently 501 against the MLS Grid v2 API.
func IsExpandOnly(r rawoutput.Resource) bool {
	switch r {
	case rawoutput.ResourceMedia, rawoutput.ResourcePropertyRooms, rawoutput.ResourcePropertyUnitTypes:
		return true
	}
	return false
}

// ValidateRaw streams every raw_output row for resource through the parser
// and returns a ValidateReport. Returns an error if the resource has no
// registered parser (caller can decide to skip).
func ValidateRaw(ctx context.Context, client *ent.Client, resource rawoutput.Resource) (*ValidateReport, error) {
	d, err := dispatchFor(resource)
	if err != nil {
		return nil, err
	}
	return validateOne(ctx, client, resource, d)
}

func validateOne(ctx context.Context, client *ent.Client, resource rawoutput.Resource, d *validateDispatch) (*ValidateReport, error) {
	report := &ValidateReport{
		Resource:       resource,
		UnconsumedKeys: map[string]int{},
	}

	seenNonNil := map[string]bool{}

	const batchSize = 500
	q := client.RawOutput.Query().
		Where(rawoutput.ResourceEQ(resource)).
		Order(ent.Asc(rawoutput.FieldID)).
		Limit(batchSize)

	offset := 0
	for {
		batch, err := q.Clone().Offset(offset).All(ctx)
		if err != nil {
			return nil, fmt.Errorf("fetch batch at offset %d: %w", offset, err)
		}
		if len(batch) == 0 {
			break
		}
		for _, raw := range batch {
			report.TotalRows++
			parsed, err := d.parse(raw.Payload)
			if err != nil {
				report.ParseErrors = append(report.ParseErrors, ParseError{
					RawOutputID: raw.ID.String(),
					Err:         err.Error(),
				})
				continue
			}
			for k := range d.extendedFields(parsed) {
				report.UnconsumedKeys[k]++
			}
			recordNonNilFieldsGeneric(d.fieldsValue(parsed), d.commonSkipNames, seenNonNil)
		}
		if len(batch) < batchSize {
			break
		}
		offset += batchSize
	}

	report.AlwaysNilFields = nilFieldNamesGeneric(d.zeroFieldsType, d.commonSkipNames, seenNonNil)
	return report, nil
}

// recordNonNilFieldsGeneric walks a parsed Fields struct (reflect.Value of
// the dereferenced struct) and marks every nillable field that holds a
// value. Pointers and slices count; scalars don't (they always have a value).
func recordNonNilFieldsGeneric(v reflect.Value, skip map[string]bool, seen map[string]bool) {
	tp := v.Type()
	for i := 0; i < tp.NumField(); i++ {
		name := tp.Field(i).Name
		if skip[name] {
			continue
		}
		fv := v.Field(i)
		switch fv.Kind() {
		case reflect.Ptr:
			if !fv.IsNil() {
				seen[name] = true
			}
		case reflect.Slice:
			if fv.Len() > 0 {
				seen[name] = true
			}
		}
	}
}

// nilFieldNamesGeneric returns the alphabetized list of Fields-struct field
// names that did NOT appear in seenNonNil — i.e. were nil/empty across every
// row. Filters out scalars and skip-listed fields.
func nilFieldNamesGeneric(zeroType reflect.Type, skip map[string]bool, seenNonNil map[string]bool) []string {
	var out []string
	for i := 0; i < zeroType.NumField(); i++ {
		name := zeroType.Field(i).Name
		if skip[name] {
			continue
		}
		k := zeroType.Field(i).Type.Kind()
		if k != reflect.Ptr && k != reflect.Slice {
			continue
		}
		if !seenNonNil[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// SortedUnconsumedKeys returns the unconsumed-key counts sorted by frequency
// (desc) — handy for the CLI to print the most-common leftovers first.
func (r *ValidateReport) SortedUnconsumedKeys() []struct {
	Key   string
	Count int
} {
	type kc struct {
		Key   string
		Count int
	}
	all := make([]kc, 0, len(r.UnconsumedKeys))
	for k, c := range r.UnconsumedKeys {
		all = append(all, kc{k, c})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Count != all[j].Count {
			return all[i].Count > all[j].Count
		}
		return all[i].Key < all[j].Key
	})
	out := make([]struct {
		Key   string
		Count int
	}, len(all))
	for i, x := range all {
		out[i] = struct {
			Key   string
			Count int
		}{x.Key, x.Count}
	}
	return out
}
