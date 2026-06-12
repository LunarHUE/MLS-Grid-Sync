package processor

import (
	"encoding/json"
	"fmt"
	"time"
)

// PropertyUnitTypeFields mirrors typed columns on PropertyUnitType /
// PropertyUnitTypeVersion. Same parking semantics as OpenHouse / PropertyRoom.
type PropertyUnitTypeFields struct {
	UnitTypeKey string

	SourceModifiedAt      time.Time
	OriginatingSystemName *string
	MlgCanView            bool
	MlgCanUse             []string

	ListingKey         string
	UnitTypeBedsTotal  *int16
	UnitTypeFurnished  *string

	ExtendedFields map[string]any
}

// Audit (raw_output 'property_unit_types' inventory, 2026-06-11, n=1,544):
//
//   - REQUIRED & natively present: UnitTypeKey (100%).
//   - REQUIRED & splitter-injected (decision 5 / cross-layer tripwire —
//     requirement is deliberate; if the splitter ever stops injecting,
//     parsing poisons loudly rather than landing rows with nil
//     parent_listing_key): ListingKey. The audit found this absent (0%)
//     from the raw payload — splitExpandedChildren in sync/raw.go writes it.
//   - SourceModifiedAt: NOT parsed here. The splitter owns timestamp
//     extraction; the processor sets fields.SourceModifiedAt =
//     raw.SourceModifiedAt after parse to align with the stale-skip
//     comparison value.
//   - Optional sparse: UnitTypeBedsTotal (87.37%).
//   - Optional, consumed-but-absent (dead-but-tolerated):
//     OriginatingSystemName, MlgCanView, MlgCanUse, UnitTypeFurnished. The
//     MlgCanView default-true + processor tombstone branch are
//     dead-but-defensive (see property_unit_type.go).
//   - Unconsumed RESO fields (→ ExtendedFields; typed-column promotion
//     candidates, deferred to Phase 6+): UnitTypeType (100% — feels like
//     a typed-column oversight rather than deliberate deferral, flagged
//     explicitly), UnitTypeBathsTotal (86.66%), UnitTypeActualRent
//     (67.16%), UnitTypeDescription (47.41%).
//   - Vendor extensions (→ ExtendedFields, correct): FHR_Vacant (76.49%),
//     FHR_LeaseExpires (40.67%), FHR_SecurityDeposit (36.40%).
func parsePropertyUnitType(payload []byte) (*PropertyUnitTypeFields, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("payload is not a JSON object: %w", err)
	}

	out := &PropertyUnitTypeFields{MlgCanView: true}

	consume := func(key string) (json.RawMessage, bool) {
		v, ok := raw[key]
		if ok {
			delete(raw, key)
		}
		return v, ok
	}

	keyRaw, ok := consume("UnitTypeKey")
	if !ok {
		return nil, fmt.Errorf("missing required field UnitTypeKey")
	}
	unitTypeKey, err := parseString(keyRaw)
	if err != nil || unitTypeKey == nil || *unitTypeKey == "" {
		return nil, fmt.Errorf("UnitTypeKey: empty or invalid")
	}
	out.UnitTypeKey = *unitTypeKey

	// ListingKey is splitter-injected (see audit comment above); the kept
	// required-ness is the cross-layer tripwire.
	listingRaw, ok := consume("ListingKey")
	if !ok {
		return nil, fmt.Errorf("missing required field ListingKey")
	}
	listingKey, err := parseString(listingRaw)
	if err != nil || listingKey == nil || *listingKey == "" {
		return nil, fmt.Errorf("ListingKey: empty or invalid")
	}
	out.ListingKey = *listingKey

	if v, ok := consume("OriginatingSystemName"); ok {
		out.OriginatingSystemName, err = parseString(v)
		if err != nil {
			return nil, fmt.Errorf("OriginatingSystemName: %w", err)
		}
	}
	if v, ok := consume("MlgCanView"); ok {
		b, err := parseBool(v)
		if err != nil {
			return nil, fmt.Errorf("MlgCanView: %w", err)
		}
		if b != nil {
			out.MlgCanView = *b
		}
	}
	if v, ok := consume("MlgCanUse"); ok {
		arr, err := parseStringArray(v)
		if err != nil {
			return nil, fmt.Errorf("MlgCanUse: %w", err)
		}
		out.MlgCanUse = []string(arr)
	}
	if v, ok := consume("UnitTypeBedsTotal"); ok {
		out.UnitTypeBedsTotal, err = parseInt16(v)
		if err != nil {
			return nil, fmt.Errorf("UnitTypeBedsTotal: %w", err)
		}
	}
	if v, ok := consume("UnitTypeFurnished"); ok {
		out.UnitTypeFurnished, err = parseString(v)
		if err != nil {
			return nil, fmt.Errorf("UnitTypeFurnished: %w", err)
		}
	}

	ext, err := bucketExtendedFields(raw)
	if err != nil {
		return nil, err
	}
	out.ExtendedFields = ext

	return out, nil
}
