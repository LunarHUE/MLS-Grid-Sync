package processor

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/lib/pq"
)

// PropertyRoomFields mirrors the typed columns on PropertyRoom /
// PropertyRoomVersion. Same parking pattern as OpenHouse — the natural
// ListingKey lands always; the FK column parent_listing_key is filled at
// write time only if Property exists, and re-linked by PropertyProcessor.
// AfterPass otherwise.
type PropertyRoomFields struct {
	RoomKey string

	SourceModifiedAt      time.Time
	OriginatingSystemName *string
	MlgCanView            bool
	MlgCanUse             []string

	ListingKey   string
	RoomType     *string
	RoomLevel    *string
	RoomFeatures pq.StringArray

	ExtendedFields map[string]any
}

// Audit (raw_output 'property_rooms' inventory, 2026-06-11, n=148,360):
//
//   - REQUIRED & natively present: RoomKey (100%), RoomType (100%).
//   - REQUIRED & splitter-injected (decision 5 / cross-layer tripwire —
//     requirement is deliberate; if the splitter ever stops injecting,
//     parsing poisons loudly rather than landing rows with nil
//     parent_listing_key): ListingKey. The audit found this absent (0%)
//     from the raw payload — splitExpandedChildren in sync/raw.go writes it.
//   - SourceModifiedAt: NOT parsed here. The splitter owns timestamp
//     extraction (child's ModificationTimestamp, parent fallback); the
//     processor sets fields.SourceModifiedAt = raw.SourceModifiedAt after
//     parse to align with the stale-skip comparison value.
//   - Optional & natively present: RoomLevel (98.46%).
//   - Optional, consumed-but-absent (dead-but-tolerated):
//     OriginatingSystemName, MlgCanView, MlgCanUse, RoomFeatures. The
//     MlgCanView default-true + processor tombstone branch are
//     dead-but-defensive (see property_room.go).
//   - Unconsumed (→ ExtendedFields; typed-column promotion candidates,
//     deferred to Phase 6+): RoomDimensions (50.82%), RoomDescription
//     (30.89%).
func parsePropertyRoom(payload []byte) (*PropertyRoomFields, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("payload is not a JSON object: %w", err)
	}

	out := &PropertyRoomFields{MlgCanView: true}

	consume := func(key string) (json.RawMessage, bool) {
		v, ok := raw[key]
		if ok {
			delete(raw, key)
		}
		return v, ok
	}

	keyRaw, ok := consume("RoomKey")
	if !ok {
		return nil, fmt.Errorf("missing required field RoomKey")
	}
	roomKey, err := parseString(keyRaw)
	if err != nil || roomKey == nil || *roomKey == "" {
		return nil, fmt.Errorf("RoomKey: empty or invalid")
	}
	out.RoomKey = *roomKey

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

	if v, ok := consume("RoomType"); ok {
		out.RoomType, err = parseString(v)
		if err != nil {
			return nil, fmt.Errorf("RoomType: %w", err)
		}
	}
	if v, ok := consume("RoomLevel"); ok {
		out.RoomLevel, err = parseString(v)
		if err != nil {
			return nil, fmt.Errorf("RoomLevel: %w", err)
		}
	}
	if v, ok := consume("RoomFeatures"); ok {
		out.RoomFeatures, err = parseStringArray(v)
		if err != nil {
			return nil, fmt.Errorf("RoomFeatures: %w", err)
		}
	}

	ext, err := bucketExtendedFields(raw)
	if err != nil {
		return nil, err
	}
	out.ExtendedFields = ext

	return out, nil
}
