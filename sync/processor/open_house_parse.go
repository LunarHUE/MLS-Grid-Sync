package processor

import (
	"encoding/json"
	"fmt"
	"time"
)

// OpenHouseFields mirrors the typed columns on OpenHouse / OpenHouseVersion.
// ListingKey is the natural FK to Property — always populated from the wire.
// The nullable parent_listing_key (the actual FK column) is set on the entity
// at write time iff the parent Property is present at that moment.
type OpenHouseFields struct {
	// --- Entity identity ---
	OpenHouseKey string

	// --- MLSMetadataMixin ---
	SourceModifiedAt      time.Time
	OriginatingSystemName *string
	MlgCanView            bool
	MlgCanUse             []string

	// --- OpenHouseDataMixin ---
	ListingKey         string
	ListingID          *string
	OpenHouseDate      *time.Time
	OpenHouseStartTime *time.Time
	OpenHouseEndTime   *time.Time
	OpenHouseStatus    *string
	OpenHouseType      *string

	ExtendedFields map[string]any
}

// parseOpenHouse turns a raw_output.payload JSON blob into OpenHouseFields.
// Poisons on missing OpenHouseKey, missing/malformed ModificationTimestamp,
// or missing ListingKey (the data mixin requires it).
func parseOpenHouse(payload []byte) (*OpenHouseFields, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("payload is not a JSON object: %w", err)
	}

	out := &OpenHouseFields{MlgCanView: true}

	consume := func(key string) (json.RawMessage, bool) {
		v, ok := raw[key]
		if ok {
			delete(raw, key)
		}
		return v, ok
	}

	// --- Required keys ---
	keyRaw, ok := consume("OpenHouseKey")
	if !ok {
		return nil, fmt.Errorf("missing required field OpenHouseKey")
	}
	openHouseKey, err := parseString(keyRaw)
	if err != nil || openHouseKey == nil || *openHouseKey == "" {
		return nil, fmt.Errorf("OpenHouseKey: empty or invalid")
	}
	out.OpenHouseKey = *openHouseKey

	tsRaw, ok := consume("ModificationTimestamp")
	if !ok {
		return nil, fmt.Errorf("missing required field ModificationTimestamp")
	}
	ts, err := parseTime(tsRaw)
	if err != nil || ts == nil {
		return nil, fmt.Errorf("ModificationTimestamp: %w", err)
	}
	out.SourceModifiedAt = *ts

	listingRaw, ok := consume("ListingKey")
	if !ok {
		return nil, fmt.Errorf("missing required field ListingKey")
	}
	listingKey, err := parseString(listingRaw)
	if err != nil || listingKey == nil || *listingKey == "" {
		return nil, fmt.Errorf("ListingKey: empty or invalid")
	}
	out.ListingKey = *listingKey

	// --- MLSMetadataMixin ---
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

	if v, ok := consume("ListingId"); ok {
		out.ListingID, err = parseString(v)
		if err != nil {
			return nil, fmt.Errorf("ListingId: %w", err)
		}
	}
	if v, ok := consume("OpenHouseStatus"); ok {
		out.OpenHouseStatus, err = parseString(v)
		if err != nil {
			return nil, fmt.Errorf("OpenHouseStatus: %w", err)
		}
	}
	if v, ok := consume("OpenHouseType"); ok {
		out.OpenHouseType, err = parseString(v)
		if err != nil {
			return nil, fmt.Errorf("OpenHouseType: %w", err)
		}
	}

	// RESO temporal classification: OpenHouseDate is Date (YYYY-MM-DD);
	// OpenHouseStartTime / OpenHouseEndTime are Timestamp (RFC 3339).
	// Misrouting is caught by parseDate/parseTime's symmetric rejection
	// plus the buildFullOpenHousePayload sentinel split (const dt vs ts).
	for k, dst := range map[string]**time.Time{
		"OpenHouseStartTime": &out.OpenHouseStartTime,
		"OpenHouseEndTime":   &out.OpenHouseEndTime,
	} {
		if v, ok := consume(k); ok {
			parsed, err := parseTime(v)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			*dst = parsed
		}
	}
	if v, ok := consume("OpenHouseDate"); ok {
		parsed, err := parseDate(v)
		if err != nil {
			return nil, fmt.Errorf("OpenHouseDate: %w", err)
		}
		out.OpenHouseDate = parsed
	}

	ext, err := bucketExtendedFields(raw)
	if err != nil {
		return nil, err
	}
	out.ExtendedFields = ext

	return out, nil
}
