package sync

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lunarhue/libs-go/log"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/mls"
)

// rawOutputInsertColumns is the column count for raw_output's bulk INSERT:
// raw_output_id, sync_event_id, resource, source_key, change_type,
// source_modified_at, payload, created_at. Must match the placeholder
// count in bulkInsertChunk.
const rawOutputInsertColumns = 8

// maxInsertChunkRows caps how many rows go into one INSERT statement.
// Postgres caps a prepared statement at 65,535 bind parameters (uint16
// in the extended-protocol wire format), so 4000 × 8 = 32,000 leaves
// plenty of headroom for a future column addition without anyone
// thinking about it. The TestChunkSize_UnderPgParamCap test re-derives
// the math so the constant cannot silently go stale if columns are
// added. Declared as var (not const) only so tests can shrink it to
// exercise the chunk loop without constructing 4001+ rows.
var maxInsertChunkRows = 4000

// expandedChildSpec describes one expand-only child resource embedded in
// Property payloads. The MLS Grid v2 API returns 501 for /v2/Media and
// PropertyRooms / PropertyUnitTypes are not top-level RESO resources at
// all — they arrive only inside Property via $expand=Media,Rooms,UnitTypes.
// splitExpandedChildren turns each array element into its own raw_output
// row keyed by the child's own primary key + timestamp.
type expandedChildSpec struct {
	resource       rawoutput.Resource
	arrayKey       string // JSON key inside the Property payload
	keyField       string // child's primary key field
	timestampField string // child's preferred timestamp field
}

// expandedChildResources is the static schema for the splitter. Order is
// the dependency order processor passes also use (property → media →
// property_rooms → property_unit_types).
var expandedChildResources = []expandedChildSpec{
	{rawoutput.ResourceMedia, "Media", "MediaKey", "MediaModificationTimestamp"},
	{rawoutput.ResourcePropertyRooms, "Rooms", "RoomKey", "ModificationTimestamp"},
	{rawoutput.ResourcePropertyUnitTypes, "UnitTypes", "UnitTypeKey", "ModificationTimestamp"},
}

// preparedRow carries one raw_output row from the build phase into the
// bulk INSERT.
type preparedRow struct {
	resource   rawoutput.Resource
	sourceKey  string
	modifiedAt time.Time
	payload    []byte
}

// saveToRawOutput inserts records into raw_output, deduping at the DB layer
// via the §7 unique index on (resource, source_key, source_modified_at). It
// returns the max source_modified_at across PARENT rows it actually wrote —
// rows suppressed by ON CONFLICT DO NOTHING are not in RETURNING and child
// timestamps are excluded so the parent's delta cursor stays parent-derived
// (see [splitExpandedChildren] for why). The zero time.Time is returned
// when every parent record was skipped or duplicate; callers use that as
// the carry-forward signal (Phase 4 plan §8 zero-record success).
//
// When resourceName == Property, the embedded Media / Rooms / UnitTypes
// arrays are split out into child raw_output rows in the same bulk INSERT.
// The second return is the slice of media child payloads suitable for
// EnqueueAttachmentJobs — empty for every other resource.
func (s *Service) saveToRawOutput(ctx context.Context, syncEventID uuid.UUID, resourceName string, records []json.RawMessage) (time.Time, []json.RawMessage, error) {
	if len(records) == 0 {
		return time.Time{}, nil, nil
	}

	keyField, err := mls.ResourceKeyField(resourceName)
	if err != nil {
		return time.Time{}, nil, err
	}
	parentDB, err := MLSToDBResource(resourceName)
	if err != nil {
		return time.Time{}, nil, err
	}

	rows, mediaForEnqueue, err := buildPreparedRows(resourceName, parentDB, keyField, records)
	if err != nil {
		return time.Time{}, nil, err
	}

	// One transaction per page across however many chunks the row slice
	// produces. Page-level poison semantics are load-bearing: if any chunk
	// fails, the WHOLE page rolls back — the pre-chunking code path got
	// this for free from the single statement's atomicity. The chunk
	// constant exists because Postgres caps a prepared statement at 65,535
	// bind parameters; see [maxInsertChunkRows].
	tx, err := s.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, nil, fmt.Errorf("begin tx %s: %w", resourceName, err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once Commit succeeds

	now := time.Now().UTC()
	var maxModified time.Time
	for chunk := range slices.Chunk(rows, maxInsertChunkRows) {
		chunkMax, err := bulkInsertChunk(ctx, tx, chunk, syncEventID, now, parentDB)
		if err != nil {
			return time.Time{}, nil, fmt.Errorf("bulk insert %s (chunk of %d): %w", resourceName, len(chunk), err)
		}
		if chunkMax.After(maxModified) {
			maxModified = chunkMax
		}
	}

	if err := tx.Commit(); err != nil {
		return time.Time{}, nil, fmt.Errorf("commit tx %s: %w", resourceName, err)
	}

	return maxModified, mediaForEnqueue, nil
}

// bulkInsertChunk issues one INSERT ... ON CONFLICT DO NOTHING for up to
// [maxInsertChunkRows] rows inside the page's transaction and returns the
// max source_modified_at across PARENT rows actually written in this
// chunk (matching the resource the caller is paginating). The caller folds
// per-chunk maxes into the page-wide maxModified.
func bulkInsertChunk(ctx context.Context, tx *sql.Tx, chunk []preparedRow, syncEventID uuid.UUID, now time.Time, parentDB rawoutput.Resource) (time.Time, error) {
	args := make([]any, 0, len(chunk)*rawOutputInsertColumns)
	placeholders := make([]string, 0, len(chunk))
	for _, row := range chunk {
		i := len(args)
		placeholders = append(placeholders, fmt.Sprintf(
			"($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
			i+1, i+2, i+3, i+4, i+5, i+6, i+7, i+8,
		))
		args = append(args,
			uuid.Must(uuid.NewV7()),
			syncEventID,
			string(row.resource),
			row.sourceKey,
			string(rawoutput.ChangeTypeInsert),
			row.modifiedAt,
			row.payload,
			now,
		)
	}

	query := `INSERT INTO raw_output
  (raw_output_id, sync_event_id, resource, source_key, change_type, source_modified_at, payload, created_at)
VALUES ` + strings.Join(placeholders, ",") + `
ON CONFLICT (resource, source_key, source_modified_at) DO NOTHING
RETURNING resource, source_modified_at`

	qrows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return time.Time{}, err
	}
	defer qrows.Close()

	var maxModified time.Time
	for qrows.Next() {
		var r string
		var t time.Time
		if err := qrows.Scan(&r, &t); err != nil {
			return time.Time{}, fmt.Errorf("scan returning: %w", err)
		}
		if r == string(parentDB) && t.After(maxModified) {
			maxModified = t
		}
	}
	if err := qrows.Err(); err != nil {
		return time.Time{}, fmt.Errorf("rows err: %w", err)
	}
	return maxModified, nil
}

// buildPreparedRows turns the page's records into rows ready for the bulk
// INSERT. For Property, splitExpandedChildren peels the embedded Media /
// Rooms / UnitTypes arrays out of each parent and produces child rows for
// the appropriate resources; the parent payload is re-marshalled AFTER the
// strip so the stored bytes omit the three arrays.
func buildPreparedRows(resourceName string, parentDB rawoutput.Resource, keyField string, records []json.RawMessage) ([]preparedRow, []json.RawMessage, error) {
	rows := make([]preparedRow, 0, len(records))
	var mediaForEnqueue []json.RawMessage
	now := time.Now().UTC()

	for _, rawBytes := range records {
		var metadata map[string]any
		if err := json.Unmarshal(rawBytes, &metadata); err != nil {
			return nil, nil, fmt.Errorf("unmarshal record: %w", err)
		}

		sourceKey, ok := metadata[keyField].(string)
		if !ok || sourceKey == "" {
			return nil, nil, fmt.Errorf("record missing primary key field: %s", keyField)
		}

		parentModifiedAt := parseRecordTimestamp(metadata, now)

		if resourceName == mls.ResourceProperty {
			childRows, mediaJSON, err := splitExpandedChildren(metadata, sourceKey, parentModifiedAt)
			if err != nil {
				return nil, nil, err
			}
			rows = append(rows, childRows...)
			mediaForEnqueue = append(mediaForEnqueue, mediaJSON...)
		}

		payloadBytes, err := json.Marshal(metadata)
		if err != nil {
			return nil, nil, fmt.Errorf("marshal payload: %w", err)
		}
		rows = append(rows, preparedRow{
			resource:   parentDB,
			sourceKey:  sourceKey,
			modifiedAt: parentModifiedAt,
			payload:    payloadBytes,
		})
	}
	return rows, mediaForEnqueue, nil
}

// splitExpandedChildren extracts the Media, Rooms, and UnitTypes arrays out
// of a Property payload, returning child preparedRows for each element and
// a parallel slice of media child JSON payloads for EnqueueAttachmentJobs.
//
// MUTATIONS APPLIED TO STORED PAYLOADS (documented exceptions):
//
//  1. Parent payload: the three array keys (Media, Rooms, UnitTypes) are
//     DELETED so the stored parent omits them. The removed content is
//     preserved verbatim as child rows, so total information is conserved
//     and the corpus is replayable from raw_output alone, just across more
//     rows. Future-you auditing a raw parent payload against an API response
//     will see three keys missing; the difference is by design.
//
//  2. Child payloads: parent-context fields are INJECTED before marshal:
//     - rooms / unit_types get `ListingKey = parentKey`
//     - media gets `ResourceName = "Property"`
//     This extends, not breaks, the verbatim-invariant — child rows were
//     never byte-for-byte API responses (standalone /v2/Media returns 501,
//     rooms / unit_types have no top-level endpoint at all). They are
//     extracted fragments; parent-context completion makes each fragment
//     parseable as a standalone record without losing information. MLS
//     Grid already does this natively for media's ResourceRecordKey
//     (audit: 100%); the splitter does it uniformly for the rest. The
//     downstream parsers KEEP the injected fields as REQUIRED — that's
//     the cross-layer tripwire: if this injection ever regresses, the
//     first record poisons loudly with the splitter named in the stack.
//
// Hidden-listing watch item (RESOLVED 2026-06-11): the audit of 586k
// media / 148k rooms / 1.5k unit-type rows showed MlgCanView absent in
// every expanded-child shape. Child visibility IS parent visibility;
// protection is the Phase 3 parent-visibility resolver filter, NOT
// per-child tombstones. Each child processor's `if !fields.MlgCanView`
// branch is therefore dead-but-defensive (cross-ref the comments
// there): if MLS Grid ever starts embedding per-child visibility, the
// logic wakes up rather than needing rediscovery.
//
// Policy:
//   - A child missing its primary key poisons the page (same policy as parent).
//   - A child missing its own timestamp falls back to the parent's, and the
//     fallback is logged (recorded, not silent).
//   - An array slot that isn't an object is a hard error — the page poisons.
//   - An absent or empty array is fine; the strip is still applied (no-op
//     when the key was already absent).
func splitExpandedChildren(parent map[string]any, parentKey string, parentModifiedAt time.Time) ([]preparedRow, []json.RawMessage, error) {
	var rows []preparedRow
	var mediaForEnqueue []json.RawMessage

	for _, spec := range expandedChildResources {
		arr, present := parent[spec.arrayKey].([]any)
		delete(parent, spec.arrayKey)
		if !present || len(arr) == 0 {
			continue
		}
		for i, elem := range arr {
			obj, ok := elem.(map[string]any)
			if !ok {
				return nil, nil, fmt.Errorf("Property %s: %s[%d] is not an object", parentKey, spec.arrayKey, i)
			}
			childKey, ok := obj[spec.keyField].(string)
			if !ok || childKey == "" {
				return nil, nil, fmt.Errorf("Property %s: %s[%d] missing primary key field %s", parentKey, spec.arrayKey, i, spec.keyField)
			}
			modifiedAt := parseChildTimestamp(obj, spec.timestampField)
			if modifiedAt.IsZero() {
				modifiedAt = parentModifiedAt
				log.Debugf("split: %s %s under Property %s has no %s; using parent ModificationTimestamp",
					spec.arrayKey, childKey, parentKey, spec.timestampField)
			}
			// Parent-context injection (see mutation #2 in the header
			// doc). Downstream parsers REQUIRE these — the requirement
			// is the cross-layer tripwire on this stanza.
			switch spec.resource {
			case rawoutput.ResourceMedia:
				obj["ResourceName"] = "Property"
			case rawoutput.ResourcePropertyRooms, rawoutput.ResourcePropertyUnitTypes:
				obj["ListingKey"] = parentKey
			}
			payloadBytes, err := json.Marshal(obj)
			if err != nil {
				return nil, nil, fmt.Errorf("marshal %s child %s: %w", spec.arrayKey, childKey, err)
			}
			rows = append(rows, preparedRow{
				resource:   spec.resource,
				sourceKey:  childKey,
				modifiedAt: modifiedAt,
				payload:    payloadBytes,
			})
			if spec.resource == rawoutput.ResourceMedia {
				mediaForEnqueue = append(mediaForEnqueue, json.RawMessage(payloadBytes))
			}
		}
	}
	return rows, mediaForEnqueue, nil
}

// parseRecordTimestamp pulls ModificationTimestamp from a record, falling
// back to the provided default when the field is missing or unparseable.
// Matches the pre-split saveToRawOutput behavior for parent records.
func parseRecordTimestamp(record map[string]any, fallback time.Time) time.Time {
	if ts, ok := record["ModificationTimestamp"].(string); ok {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			return t
		}
	}
	return fallback
}

// parseChildTimestamp pulls a named timestamp field from a child element.
// Returns zero on absence/parse-failure; the caller falls back to the
// parent's timestamp (and logs the fallback).
func parseChildTimestamp(child map[string]any, field string) time.Time {
	if ts, ok := child[field].(string); ok {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			return t
		}
	}
	return time.Time{}
}
