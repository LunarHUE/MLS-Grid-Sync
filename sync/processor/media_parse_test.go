package processor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseMedia_RequiresMediaKey(t *testing.T) {
	_, err := parseMedia([]byte(`{"ModificationTimestamp": "2026-01-15T12:00:00Z", "ResourceName": "Property", "ResourceRecordKey": "LK-1"}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MediaKey")
}

func TestParseMedia_RequiresResourceName(t *testing.T) {
	_, err := parseMedia([]byte(`{
		"MediaKey": "M-1",
		"ModificationTimestamp": "2026-01-15T12:00:00Z",
		"ResourceRecordKey": "LK-1"
	}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ResourceName")
}

func TestParseMedia_RequiresResourceRecordKey(t *testing.T) {
	_, err := parseMedia([]byte(`{
		"MediaKey": "M-1",
		"ModificationTimestamp": "2026-01-15T12:00:00Z",
		"ResourceName": "Property"
	}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ResourceRecordKey")
}

func TestParseMedia_RejectsUnknownResourceName(t *testing.T) {
	_, err := parseMedia([]byte(`{
		"MediaKey": "M-1",
		"ModificationTimestamp": "2026-01-15T12:00:00Z",
		"ResourceName": "Listing",
		"ResourceRecordKey": "LK-1"
	}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ResourceName")
}

func TestParseMedia_NormalizesResourceNameCase(t *testing.T) {
	// Wire may send "Property" — ent enum is "property".
	got, err := parseMedia([]byte(`{
		"MediaKey": "M-1",
		"ModificationTimestamp": "2026-01-15T12:00:00Z",
		"ResourceName": "Property",
		"ResourceRecordKey": "LK-1"
	}`))
	require.NoError(t, err)
	assert.Equal(t, "property", got.ResourceType)
	assert.Equal(t, "LK-1", got.ResourceRecordKey)
}

// TestParseMedia_ExpandedShapeFixture is the post-reset golden test for
// the formerly-poisonous row (raw_output_id=019eb7e9-a189-74a6-9ec8-
// b8ce122c4889, source_key=FHR231528). The fixture starts as a synthetic
// minimal expanded-shape payload; the intent is to overwrite it with the
// real captured payload after the 2026-06-11 init re-baseline:
//
//	SELECT payload FROM raw_output
//	 WHERE resource = 'media' AND source_key = 'FHR231528';
//
// Assertions are structural — they hold for both the synthetic skeleton
// and the real row because they pin the contract the parser must honor
// regardless of which exact optionals MLS Grid sent.
func TestParseMedia_ExpandedShapeFixture(t *testing.T) {
	payload, err := os.ReadFile(filepath.Join("testdata", "media", "expanded_shape.json"))
	require.NoError(t, err, "fixture missing — extract per the comment above")

	got, err := parseMedia(payload)
	require.NoError(t, err, "expanded-shape media must parse — this is the regression for the init halt")

	// Timestamp seam: parser does NOT set SourceModifiedAt — processor
	// sources it from raw_output.source_modified_at after parse.
	assert.True(t, got.SourceModifiedAt.IsZero(),
		"parser must leave SourceModifiedAt zero; processor sets it from raw row")

	// Always-present-natively (per audit).
	assert.NotEmpty(t, got.MediaKey)
	assert.NotEmpty(t, got.ResourceRecordKey)

	// Splitter-injected (decision 5 / cross-layer tripwire). Normalized to
	// the ent enum's lowercase form.
	assert.Equal(t, "property", got.ResourceType,
		"splitter-injected ResourceName must flow through to ResourceType")

	// Pre-registered audit expectation: MediaURL is the attachment
	// pipeline's only download path; if the fixture or parser ever drops
	// it, this fails loudly.
	require.NotNil(t, got.MediaURL)
	assert.NotEmpty(t, *got.MediaURL)
}

func TestParseMedia_FullCoverage(t *testing.T) {
	full := buildFullMediaPayload(t)
	raw, err := json.Marshal(full)
	require.NoError(t, err)

	got, err := parseMedia(raw)
	require.NoError(t, err)

	v := reflect.ValueOf(got).Elem()
	tp := v.Type()
	// SourceModifiedAt is processor-owned (sourced from raw_output, not
	// the payload — see media.go's timestamp seam). Skipping it here
	// keeps the parser's contract clean: the parser sets every typed
	// field whose JSON key it consumes; SourceModifiedAt is not one of
	// them anymore.
	skip := map[string]bool{"ExtendedFields": true, "SourceModifiedAt": true}
	for i := 0; i < tp.NumField(); i++ {
		name := tp.Field(i).Name
		if skip[name] {
			continue
		}
		f := v.Field(i)
		switch f.Kind() {
		case reflect.Ptr:
			assert.False(t, f.IsNil(), "MediaFields.%s nil — missing mapping", name)
		case reflect.Slice:
			assert.Greater(t, f.Len(), 0, "MediaFields.%s empty", name)
		case reflect.String:
			assert.NotEmpty(t, f.String(), "MediaFields.%s empty", name)
		case reflect.Struct:
			assert.False(t, f.IsZero(), "MediaFields.%s zero", name)
		case reflect.Bool:
			assert.True(t, f.Bool(), "MediaFields.%s false in full-coverage payload", name)
		}
	}
}

// buildFullMediaPayload models the EXPANDED-shape media payload — the
// only shape the parsers actually meet, per the audit (raw_output 'media'
// inventory, 2026-06-11, n=586,504). It exercises every parser code
// path:
//
//   - Always-present-natively: MediaKey, ResourceRecordKey, MediaURL,
//     MediaType, ImageWidth, ImageHeight, ImageSizeDescription, Order,
//     MediaModificationTimestamp, LongDescription.
//   - Splitter-injected (decision 5 / cross-layer tripwire): ResourceName.
//   - Tolerated-when-present (0% in real corpus today, kept as code-path
//     coverage so a future where MLS Grid adds them back stays
//     regression-tested): MlgCanView, MlgCanUse, OriginatingSystemName,
//     PreferredPhotoYN.
//
// Deliberately ABSENT: ModificationTimestamp — moved out of the parser's
// contract per decision 1a / timestamp seam. Including it here would
// enshrine the standalone fantasy the parsers were fixed to forget.
func buildFullMediaPayload(t *testing.T) map[string]any {
	t.Helper()
	const ts = "2026-01-15T12:00:00Z"
	return map[string]any{
		"MediaKey":                   "M-1",
		"ResourceName":               "Property",
		"ResourceRecordKey":          "LK-1",
		"OriginatingSystemName":      "actris",
		"MlgCanView":                 true,
		"MlgCanUse":                  []string{"IDX"},
		"MediaType":                  "image/jpeg",
		"MediaURL":                   "https://cdn.example/photo.jpg",
		"ImageHeight":                1080,
		"ImageWidth":                 1920,
		"ImageSizeDescription":       "Large",
		"LongDescription":            "Front exterior",
		"Order":                      1,
		"PreferredPhotoYN":           true,
		"MediaModificationTimestamp": ts,
	}
}
