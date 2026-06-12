package sync

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/mls"
)

// rfc3339 is the timestamp format raw payloads carry. Tests use it for
// child / parent timestamps so the parser sees realistic input.
const rfc3339 = time.RFC3339

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

// TestSplitExpandedChildren_FullPayload pins the happy path: 3 media, 2
// rooms, 1 unit type → 6 child rows with correct (resource, sourceKey,
// modifiedAt) + the parent payload no longer carries the three arrays.
func TestSplitExpandedChildren_FullPayload(t *testing.T) {
	parentTS := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	mediaTS := parentTS.Add(-time.Hour)
	roomsTS := parentTS.Add(-2 * time.Hour)
	unitTS := parentTS.Add(-3 * time.Hour)

	property := map[string]any{
		"ListingKey":            "P-001",
		"ModificationTimestamp": parentTS.Format(rfc3339),
		"ListPrice":             425000,
		"City":                  "Austin",
		"Media": []any{
			map[string]any{"MediaKey": "M-1", "MediaModificationTimestamp": mediaTS.Format(rfc3339), "MediaURL": "https://example/1.jpg", "MlgCanView": true},
			map[string]any{"MediaKey": "M-2", "MediaModificationTimestamp": mediaTS.Format(rfc3339), "MediaURL": "https://example/2.jpg", "MlgCanView": true},
			map[string]any{"MediaKey": "M-3", "MediaModificationTimestamp": mediaTS.Format(rfc3339), "MediaURL": "https://example/3.jpg", "MlgCanView": true},
		},
		"Rooms": []any{
			map[string]any{"RoomKey": "R-1", "ModificationTimestamp": roomsTS.Format(rfc3339), "RoomType": "Kitchen"},
			map[string]any{"RoomKey": "R-2", "ModificationTimestamp": roomsTS.Format(rfc3339), "RoomType": "Bath"},
		},
		"UnitTypes": []any{
			map[string]any{"UnitTypeKey": "U-1", "ModificationTimestamp": unitTS.Format(rfc3339), "UnitTypeType": "Apartment"},
		},
	}

	rows, mediaForEnqueue, err := splitExpandedChildren(property, "P-001", parentTS)
	require.NoError(t, err)

	require.Len(t, rows, 6, "want 3 media + 2 rooms + 1 unit type = 6 child rows")
	require.Len(t, mediaForEnqueue, 3, "media-for-enqueue must include all 3 media")

	// Resource distribution
	counts := map[rawoutput.Resource]int{}
	keys := map[rawoutput.Resource][]string{}
	for _, r := range rows {
		counts[r.resource]++
		keys[r.resource] = append(keys[r.resource], r.sourceKey)
	}
	assert.Equal(t, 3, counts[rawoutput.ResourceMedia])
	assert.Equal(t, 2, counts[rawoutput.ResourcePropertyRooms])
	assert.Equal(t, 1, counts[rawoutput.ResourcePropertyUnitTypes])
	assert.ElementsMatch(t, []string{"M-1", "M-2", "M-3"}, keys[rawoutput.ResourceMedia])
	assert.ElementsMatch(t, []string{"R-1", "R-2"}, keys[rawoutput.ResourcePropertyRooms])
	assert.ElementsMatch(t, []string{"U-1"}, keys[rawoutput.ResourcePropertyUnitTypes])

	// Each child carries its OWN timestamp, not the parent's.
	for _, r := range rows {
		switch r.resource {
		case rawoutput.ResourceMedia:
			assert.True(t, r.modifiedAt.Equal(mediaTS), "media %s should carry MediaModificationTimestamp", r.sourceKey)
		case rawoutput.ResourcePropertyRooms:
			assert.True(t, r.modifiedAt.Equal(roomsTS), "room %s should carry ModificationTimestamp", r.sourceKey)
		case rawoutput.ResourcePropertyUnitTypes:
			assert.True(t, r.modifiedAt.Equal(unitTS), "unit %s should carry ModificationTimestamp", r.sourceKey)
		}
	}

	// Parent-context injection (decision 5 / cross-layer tripwire): each
	// child payload must carry the field the parsers require — the
	// splitter's injection is the upstream half of the contract; the
	// parsers' kept requirement is the downstream half. Asserting the
	// injection here is the upstream-side guard against the regression
	// that would otherwise land rows silently with nil parent linkage.
	for _, r := range rows {
		var roundtrip map[string]any
		require.NoError(t, json.Unmarshal(r.payload, &roundtrip), "child payload must unmarshal")
		switch r.resource {
		case rawoutput.ResourceMedia:
			assert.Equal(t, "Property", roundtrip["ResourceName"],
				"media %s must carry splitter-injected ResourceName=Property", r.sourceKey)
		case rawoutput.ResourcePropertyRooms, rawoutput.ResourcePropertyUnitTypes:
			assert.Equal(t, "P-001", roundtrip["ListingKey"],
				"child %s under Property P-001 must carry splitter-injected ListingKey", r.sourceKey)
		}
	}

	// Parent map is mutated in place: Media / Rooms / UnitTypes are stripped.
	_, hasMedia := property["Media"]
	_, hasRooms := property["Rooms"]
	_, hasUnits := property["UnitTypes"]
	assert.False(t, hasMedia, "parent payload must have Media stripped")
	assert.False(t, hasRooms, "parent payload must have Rooms stripped")
	assert.False(t, hasUnits, "parent payload must have UnitTypes stripped")

	// Other parent keys are intact (the strip is targeted, not destructive).
	assert.Equal(t, "P-001", property["ListingKey"])
	assert.Equal(t, parentTS.Format(rfc3339), property["ModificationTimestamp"])
	assert.Equal(t, 425000, property["ListPrice"])
	assert.Equal(t, "Austin", property["City"])
}

// TestSplitExpandedChildren_AbsentArrays — common in real feeds (listings
// with no rooms or unit types). No children produced, no error.
func TestSplitExpandedChildren_AbsentArrays(t *testing.T) {
	property := map[string]any{
		"ListingKey":            "P-002",
		"ModificationTimestamp": "2026-06-01T12:00:00Z",
		"City":                  "Austin",
	}
	parentTS, _ := time.Parse(rfc3339, "2026-06-01T12:00:00Z")

	rows, mediaForEnqueue, err := splitExpandedChildren(property, "P-002", parentTS)
	require.NoError(t, err)
	assert.Empty(t, rows)
	assert.Empty(t, mediaForEnqueue)

	// Parent payload untouched.
	assert.Equal(t, "P-002", property["ListingKey"])
	assert.Equal(t, "Austin", property["City"])
}

// TestSplitExpandedChildren_ChildMissingKey poisons the page — same
// policy the sync layer already applies to parents missing their key.
func TestSplitExpandedChildren_ChildMissingKey(t *testing.T) {
	property := map[string]any{
		"ListingKey": "P-003",
		"Media": []any{
			map[string]any{"MediaKey": "M-OK", "MediaURL": "https://example/ok.jpg"},
			map[string]any{"MediaURL": "https://example/bad.jpg"}, // missing MediaKey
		},
	}
	parentTS := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	_, _, err := splitExpandedChildren(property, "P-003", parentTS)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MediaKey")
	assert.Contains(t, err.Error(), "P-003", "error must name the parent listing for ops triage")
}

// TestSplitExpandedChildren_ChildMissingTimestampFallsBack: when a child
// has no timestamp of its own, the parent's ModificationTimestamp is
// stamped on the child's row. Recorded (the helper logs at Debug); the
// row is still produced — the alternative (drop the child) would lose
// data.
func TestSplitExpandedChildren_ChildMissingTimestampFallsBack(t *testing.T) {
	parentTS := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	property := map[string]any{
		"ListingKey": "P-004",
		"Media": []any{
			map[string]any{"MediaKey": "M-NO-TS", "MediaURL": "https://example/x.jpg"},
		},
	}

	rows, _, err := splitExpandedChildren(property, "P-004", parentTS)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.True(t, rows[0].modifiedAt.Equal(parentTS),
		"child missing MediaModificationTimestamp must fall back to parent's; got %v want %v", rows[0].modifiedAt, parentTS)
}

// TestBuildPreparedRows_PropertyEmitsParentPlusChildren round-trips a
// Property record through buildPreparedRows: one parent row + the
// expected number of children, with the parent payload no longer
// carrying the three arrays.
func TestBuildPreparedRows_PropertyEmitsParentPlusChildren(t *testing.T) {
	parentTS := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	property := map[string]any{
		"ListingKey":            "P-005",
		"ModificationTimestamp": parentTS.Format(rfc3339),
		"City":                  "Austin",
		"Media": []any{
			map[string]any{"MediaKey": "M-5a", "MediaModificationTimestamp": parentTS.Format(rfc3339), "MediaURL": "https://example/a.jpg", "MlgCanView": true},
			map[string]any{"MediaKey": "M-5b", "MediaModificationTimestamp": parentTS.Format(rfc3339), "MediaURL": "https://example/b.jpg", "MlgCanView": false},
		},
	}
	records := []json.RawMessage{mustMarshal(t, property)}

	rows, mediaForEnqueue, err := buildPreparedRows(mls.ResourceProperty, rawoutput.ResourceProperty, "ListingKey", records)
	require.NoError(t, err)
	require.Len(t, rows, 3, "1 parent + 2 media children")

	// Find the parent row.
	var parentRow *preparedRow
	for i := range rows {
		if rows[i].resource == rawoutput.ResourceProperty {
			parentRow = &rows[i]
		}
	}
	require.NotNil(t, parentRow)

	// The stored parent payload must NOT contain the Media key any more.
	var roundtrip map[string]any
	require.NoError(t, json.Unmarshal(parentRow.payload, &roundtrip))
	_, hasMedia := roundtrip["Media"]
	assert.False(t, hasMedia, "stored parent payload must have Media stripped post-split")
	assert.Equal(t, "Austin", roundtrip["City"], "non-stripped parent fields must survive verbatim")

	// All 2 media payloads flow to enqueue — EnqueueAttachmentJobs handles
	// the MlgCanView=false skip downstream (see attachment.go).
	assert.Len(t, mediaForEnqueue, 2, "splitter forwards all media payloads; enqueue applies MlgCanView filter")
}

// TestBuildPreparedRows_NonPropertyUnchanged: for any non-Property
// resource, the splitter never fires. One record → one row, no children,
// no media-for-enqueue.
func TestBuildPreparedRows_NonPropertyUnchanged(t *testing.T) {
	member := map[string]any{
		"MemberKey":             "MEM-1",
		"MemberFirstName":       "Sam",
		"ModificationTimestamp": "2026-06-01T12:00:00Z",
	}
	records := []json.RawMessage{mustMarshal(t, member)}

	rows, mediaForEnqueue, err := buildPreparedRows(mls.ResourceMember, rawoutput.ResourceMember, "MemberKey", records)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, rawoutput.ResourceMember, rows[0].resource)
	assert.Equal(t, "MEM-1", rows[0].sourceKey)
	assert.Empty(t, mediaForEnqueue)
}
