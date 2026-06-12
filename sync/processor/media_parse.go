package processor

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// MediaResourceType discriminator values — must match the ent enum
// at ent/schema/media.go.
const (
	MediaResourceProperty = "property"
	MediaResourceMember   = "member"
	MediaResourceOffice   = "office"
)

// MediaFields mirrors typed columns on Media / MediaVersion. Media is
// polymorphic: resource_type discriminator + resource_record_key string
// reference the parent (no ent FK).
type MediaFields struct {
	MediaKey string

	SourceModifiedAt      time.Time
	OriginatingSystemName *string
	MlgCanView            bool
	MlgCanUse             []string

	// Polymorphic parent
	ResourceType      string // property | member | office (lowercased)
	ResourceRecordKey string

	MediaType                  *string
	MediaURL                   *string
	ImageHeight                *int64
	ImageWidth                 *int64
	ImageSizeDescription       *string
	LongDescription            *string
	Order                      *int16
	PreferredPhotoYn           *bool
	MediaModificationTimestamp *time.Time

	ExtendedFields map[string]any
}

// Audit (raw_output 'media' inventory, 2026-06-11, n=586,504):
//
//   - REQUIRED & natively present: MediaKey (100%), ResourceRecordKey (100%).
//   - REQUIRED & splitter-injected (decision 5 / cross-layer tripwire —
//     requirement is deliberate; if the splitter ever stops injecting,
//     parsing poisons loudly with the splitter in the stack rather than
//     landing rows with nil polymorphic discriminator):
//     ResourceName ("Property"). The audit found this absent (0%) from
//     the raw payload — splitExpandedChildren in sync/raw.go writes it.
//   - SourceModifiedAt: NOT parsed here. The splitter already owns
//     timestamp extraction (child's MediaModificationTimestamp, parent
//     fallback) and stamps raw_output.source_modified_at. The processor
//     sets fields.SourceModifiedAt = raw.SourceModifiedAt after parse;
//     this aligns the parsed value with the stale-skip comparison value
//     by construction.
//   - Optional & natively present: ImageSizeDescription, ImageWidth,
//     ImageHeight, MediaType, MediaURL, Order, MediaModificationTimestamp
//     (all 100%); LongDescription (1.49%, sparse but consumed).
//   - Optional, consumed-but-absent (dead-but-tolerated): MlgCanView,
//     OriginatingSystemName, MlgCanUse, PreferredPhotoYN. The
//     MlgCanView default-true plus the processor's tombstone branch
//     are documented dead-but-defensive (see media.go) — if MLS Grid
//     ever embeds per-child visibility, the logic wakes up.
func parseMedia(payload []byte) (*MediaFields, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("payload is not a JSON object: %w", err)
	}

	out := &MediaFields{MlgCanView: true}

	consume := func(key string) (json.RawMessage, bool) {
		v, ok := raw[key]
		if ok {
			delete(raw, key)
		}
		return v, ok
	}

	keyRaw, ok := consume("MediaKey")
	if !ok {
		return nil, fmt.Errorf("missing required field MediaKey")
	}
	mediaKey, err := parseString(keyRaw)
	if err != nil || mediaKey == nil || *mediaKey == "" {
		return nil, fmt.Errorf("MediaKey: empty or invalid")
	}
	out.MediaKey = *mediaKey

	// Polymorphic discriminator. RESO uses ResourceName ("Property" | "Member" |
	// "Office"); the ent enum is lowercase. Normalize here. The field is
	// splitter-injected (see audit comment above); the kept required-ness is
	// the cross-layer tripwire.
	resTypeRaw, ok := consume("ResourceName")
	if !ok {
		return nil, fmt.Errorf("missing required field ResourceName")
	}
	resType, err := parseString(resTypeRaw)
	if err != nil || resType == nil || *resType == "" {
		return nil, fmt.Errorf("ResourceName: empty or invalid")
	}
	switch strings.ToLower(*resType) {
	case MediaResourceProperty, MediaResourceMember, MediaResourceOffice:
		out.ResourceType = strings.ToLower(*resType)
	default:
		return nil, fmt.Errorf("ResourceName: unknown value %q (expected Property | Member | Office)", *resType)
	}

	recKeyRaw, ok := consume("ResourceRecordKey")
	if !ok {
		return nil, fmt.Errorf("missing required field ResourceRecordKey")
	}
	recKey, err := parseString(recKeyRaw)
	if err != nil || recKey == nil || *recKey == "" {
		return nil, fmt.Errorf("ResourceRecordKey: empty or invalid")
	}
	out.ResourceRecordKey = *recKey

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
	if v, ok := consume("MediaType"); ok {
		out.MediaType, err = parseString(v)
		if err != nil {
			return nil, fmt.Errorf("MediaType: %w", err)
		}
	}
	if v, ok := consume("MediaURL"); ok {
		out.MediaURL, err = parseString(v)
		if err != nil {
			return nil, fmt.Errorf("MediaURL: %w", err)
		}
	}
	if v, ok := consume("ImageHeight"); ok {
		out.ImageHeight, err = parseInt64(v)
		if err != nil {
			return nil, fmt.Errorf("ImageHeight: %w", err)
		}
	}
	if v, ok := consume("ImageWidth"); ok {
		out.ImageWidth, err = parseInt64(v)
		if err != nil {
			return nil, fmt.Errorf("ImageWidth: %w", err)
		}
	}
	if v, ok := consume("ImageSizeDescription"); ok {
		out.ImageSizeDescription, err = parseString(v)
		if err != nil {
			return nil, fmt.Errorf("ImageSizeDescription: %w", err)
		}
	}
	if v, ok := consume("LongDescription"); ok {
		out.LongDescription, err = parseString(v)
		if err != nil {
			return nil, fmt.Errorf("LongDescription: %w", err)
		}
	}
	if v, ok := consume("Order"); ok {
		out.Order, err = parseInt16(v)
		if err != nil {
			return nil, fmt.Errorf("Order: %w", err)
		}
	}
	if v, ok := consume("PreferredPhotoYN"); ok {
		out.PreferredPhotoYn, err = parseBool(v)
		if err != nil {
			return nil, fmt.Errorf("PreferredPhotoYN: %w", err)
		}
	}
	if v, ok := consume("MediaModificationTimestamp"); ok {
		out.MediaModificationTimestamp, err = parseTime(v)
		if err != nil {
			return nil, fmt.Errorf("MediaModificationTimestamp: %w", err)
		}
	}

	ext, err := bucketExtendedFields(raw)
	if err != nil {
		return nil, err
	}
	out.ExtendedFields = ext

	return out, nil
}
