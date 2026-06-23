package processor

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/attachmentjob"
	entmedia "github.com/LunarHUE/MLS-Grid-Sync/ent/media"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/property"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/propertyversion"
)

// maxBulkRows caps rows per bulk INSERT statement. Postgres' wire protocol
// allows at most 65535 bind parameters per statement; Property's entity and
// version rows are ~50 columns each, so 800 rows (~40k params) stays well under
// the limit. A larger commit-batch just issues several INSERTs in the one tx.
const maxBulkRows = 800

// entityState is the narrow current-entity read the decision needs: existence
// and the tombstone flag.
type entityState struct {
	exists     bool
	mlgCanView bool
}

// ProcessChunk is the bulk-projection path for Property (ChunkProcessor). It
// turns a commit-chunk of raw_output rows into a handful of batched SQL
// statements instead of ~3 round-trips per record:
//
//	one narrow entity read + one open-version read for the whole chunk,
//	decide every record in memory (shared decideProperty),
//	then bulk: close prior versions, insert new versions, upsert entities,
//	tombstone deletes, cascade attachment cancellations.
//
// It runs on the loop's chunk transaction and returns one Outcome per input row
// in order. Semantics are identical to the per-record Process — guarded by the
// bulk-vs-per-record equivalence test. Records whose listing_key appears more
// than once in the chunk are peeled off and applied in raw-sequence order via
// the per-record Process on this same tx (read-your-own-write chains them),
// because a single bulk insert can't carry two open versions for one key.
func (p *PropertyProcessor) ProcessChunk(ctx context.Context, tx *ent.Tx, raws []*ent.RawOutput) ([]Outcome, error) {
	outcomes := make([]Outcome, len(raws))
	now := time.Now().UTC()

	// 1. Parse every payload up front. A parse error returns immediately so the
	//    loop's per-record replay pinpoints the offending raw_output_id.
	fieldsByIdx := make([]*PropertyFields, len(raws))
	keyCount := make(map[string]int, len(raws))
	for i, raw := range raws {
		f, err := parseProperty(raw.Payload)
		if err != nil {
			return nil, fmt.Errorf("raw_output=%s parse: %w", raw.ID, err)
		}
		fieldsByIdx[i] = f
		keyCount[f.ListingKey]++
	}

	// 2. Partition: single-occurrence keys go the bulk path; any key appearing
	//    2+ times in the chunk has ALL its records applied per-record in raw
	//    order first (read-your-own-write within this tx chains their versions
	//    exactly as the per-record path does). Duplicate and unique key sets are
	//    disjoint, so running duplicates first is correctness-neutral.
	uniqueIdx := make([]int, 0, len(raws))
	for i := range raws {
		if keyCount[fieldsByIdx[i].ListingKey] > 1 {
			oc, err := p.Process(ctx, tx, raws[i])
			if err != nil {
				return nil, fmt.Errorf("raw_output=%s: %w", raws[i].ID, err)
			}
			outcomes[i] = oc
			continue
		}
		uniqueIdx = append(uniqueIdx, i)
	}

	// 3. Bulk-read current state for the unique keys (two queries total).
	keys := make([]string, len(uniqueIdx))
	for j, i := range uniqueIdx {
		keys[j] = fieldsByIdx[i].ListingKey
	}
	entities, err := p.bulkLookupEntities(ctx, tx, keys)
	if err != nil {
		return nil, err
	}
	openVersions, err := p.bulkLookupOpenVersions(ctx, tx, keys)
	if err != nil {
		return nil, err
	}

	// 4. Decide each record and accumulate the bulk write sets.
	var (
		closeIDs      []string
		verBuilders   []*ent.PropertyVersionCreate
		entityUpserts []*ent.PropertyCreate // insert + update (full upsert)
		tombstones    []*ent.PropertyCreate // delete (existing + first-sighting)
		cascadeKeys   []string
	)
	for _, i := range uniqueIdx {
		f := fieldsByIdx[i]
		raw := raws[i]
		st := entities[f.ListingKey] // zero value = {exists:false}
		cv := openVersions[f.ListingKey]

		plan := decideProperty(f, st.exists, st.mlgCanView, cv, raw)
		outcomes[i] = plan.outcome
		if plan.action == actSkip {
			continue
		}

		// Pre-generate the version id in Go (UUIDv7) so the entity upsert can
		// reference current_version_id without a round-trip.
		verID := uuid.Must(uuid.NewV7())
		vb, err := newPropertyVersionCreate(tx, f, raw, plan.changeType, now, plan.diff)
		if err != nil {
			return nil, fmt.Errorf("raw_output=%s build version: %w", raw.ID, err)
		}
		vb.SetID(verID.String())
		verBuilders = append(verBuilders, vb)
		if plan.closeVersionID != nil {
			closeIDs = append(closeIDs, *plan.closeVersionID)
		}

		switch plan.action {
		case actInsert, actUpdate:
			c := tx.Property.Create().SetID(f.ListingKey).SetCurrentVersionID(verID)
			applyToPropertyCreate(c, f)
			entityUpserts = append(entityUpserts, c)
		case actDeleteExisting, actDeleteFirstSighting:
			// Both delete sub-cases use one upsert builder carrying the full
			// (sparse) tombstone fields so the INSERT statement is valid for a
			// genuine first sighting. For an existing row the conflict path runs
			// the Update clause in bulkWrite, which touches only mlg_can_view +
			// current_version_id (+ modified_at) — preserving prior field values,
			// matching the per-record applyDelete.
			c := tx.Property.Create().SetID(f.ListingKey).SetCurrentVersionID(verID)
			applyToPropertyCreate(c, f) // sets mlg_can_view=false from f
			tombstones = append(tombstones, c)
			cascadeKeys = append(cascadeKeys, f.ListingKey)
		}
	}

	// 5. Execute the bulk writes in index-safe order.
	if err := p.bulkWrite(ctx, tx, now, closeIDs, verBuilders, entityUpserts, tombstones, cascadeKeys); err != nil {
		return nil, err
	}
	return outcomes, nil
}

// bulkLookupEntities reads {exists, mlg_can_view} for the keys in one query.
func (p *PropertyProcessor) bulkLookupEntities(ctx context.Context, tx *ent.Tx, keys []string) (map[string]entityState, error) {
	out := make(map[string]entityState, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	// property.id has StorageKey "listing_key" — the scanned column is
	// listing_key, so the struct tag must match the column, not the ent field.
	var rows []struct {
		ID         string `json:"listing_key"`
		MlgCanView bool   `json:"mlg_can_view"`
	}
	if err := tx.Property.Query().
		Where(property.IDIn(keys...)).
		Select(property.FieldID, property.FieldMlgCanView).
		Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("bulk lookup property: %w", err)
	}
	for _, r := range rows {
		out[r.ID] = entityState{exists: true, mlgCanView: r.MlgCanView}
	}
	return out, nil
}

// bulkLookupOpenVersions reads the open (valid_to IS NULL) version per key in
// one query. The partial unique index guarantees at most one per key.
func (p *PropertyProcessor) bulkLookupOpenVersions(ctx context.Context, tx *ent.Tx, keys []string) (map[string]*ent.PropertyVersion, error) {
	out := make(map[string]*ent.PropertyVersion, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	vs, err := tx.PropertyVersion.Query().
		Where(
			propertyversion.ListingKeyIn(keys...),
			propertyversion.ValidToIsNil(),
		).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("bulk lookup open versions: %w", err)
	}
	for _, v := range vs {
		out[v.ListingKey] = v
	}
	return out, nil
}

// bulkWrite issues the batched statements for a decided chunk, ordered so the
// `one open version per listing_key` partial-unique index never sees two open
// rows: close prior versions BEFORE inserting the new open versions.
func (p *PropertyProcessor) bulkWrite(
	ctx context.Context,
	tx *ent.Tx,
	now time.Time,
	closeIDs []string,
	verBuilders []*ent.PropertyVersionCreate,
	entityUpserts []*ent.PropertyCreate,
	tombstones []*ent.PropertyCreate,
	cascadeKeys []string,
) error {
	// 1. Close prior open versions (one UPDATE).
	if len(closeIDs) > 0 {
		if _, err := tx.PropertyVersion.Update().
			Where(propertyversion.IDIn(closeIDs...)).
			SetValidTo(now).
			Save(ctx); err != nil {
			return fmt.Errorf("bulk close versions: %w", err)
		}
	}

	// 2. Insert the new versions (each carries a pre-assigned id).
	for sub := range slices.Chunk(verBuilders, maxBulkRows) {
		if _, err := tx.PropertyVersion.CreateBulk(sub...).Save(ctx); err != nil {
			return fmt.Errorf("bulk insert versions: %w", err)
		}
	}

	// 3. Upsert entities (insert + update). UpdateNewValues refreshes all data
	//    columns + current_version_id + modified_at and (per codegen) ignores id
	//    and the immutable created_at, matching the per-record insert/update.
	for sub := range slices.Chunk(entityUpserts, maxBulkRows) {
		if err := tx.Property.CreateBulk(sub...).
			OnConflictColumns(property.FieldID).
			Update(func(u *ent.PropertyUpsert) {
				// Set every column to EXCLUDED (clear-on-nil parity), preserving
				// the PK and immutable created_at.
				upsertSetExcluded(u.UpdateSet, property.Columns, property.FieldID, property.FieldCreatedAt)
			}).
			Exec(ctx); err != nil {
			return fmt.Errorf("bulk upsert entities: %w", err)
		}
	}

	// 4. Tombstones (delete). The Update clause flips only mlg_can_view +
	//    current_version_id (+ modified_at) on an existing row — preserving its
	//    prior field values, matching applyDelete — while a first-sighting key
	//    inserts the sparse tombstoned row.
	for sub := range slices.Chunk(tombstones, maxBulkRows) {
		if err := tx.Property.CreateBulk(sub...).
			OnConflictColumns(property.FieldID).
			Update(func(u *ent.PropertyUpsert) {
				u.UpdateMlgCanView()
				u.UpdateCurrentVersionID()
				u.UpdateModifiedAt()
			}).
			Exec(ctx); err != nil {
			return fmt.Errorf("bulk tombstone entities: %w", err)
		}
	}

	// 5. Cascade: cancel pending attachment jobs for all tombstoned listings in
	//    two statements (bulk form of cancelPendingAttachmentJobs).
	if err := p.bulkCancelAttachmentJobs(ctx, tx, cascadeKeys); err != nil {
		return err
	}
	return nil
}

// bulkCancelAttachmentJobs is the chunk-wide form of cancelPendingAttachmentJobs:
// one media-id select + one attachment_job update over all tombstoned keys.
func (p *PropertyProcessor) bulkCancelAttachmentJobs(ctx context.Context, tx *ent.Tx, listingKeys []string) error {
	if len(listingKeys) == 0 {
		return nil
	}
	mediaIDs, err := tx.Media.Query().
		Where(
			entmedia.ResourceTypeEQ(entmedia.ResourceTypeProperty),
			entmedia.ResourceRecordKeyIn(listingKeys...),
		).
		IDs(ctx)
	if err != nil {
		return fmt.Errorf("collect media ids: %w", err)
	}
	if len(mediaIDs) == 0 {
		return nil
	}
	if _, err := tx.AttachmentJob.Update().
		Where(
			attachmentjob.MediaKeyIn(mediaIDs...),
			attachmentjob.StatusIn(
				attachmentjob.StatusPending,
				attachmentjob.StatusRetrying,
				attachmentjob.StatusInProgress,
			),
		).
		SetStatus(attachmentjob.StatusCanceled).
		ClearClaimedAt().
		ClearClaimedBy().
		Save(ctx); err != nil {
		return fmt.Errorf("update attachment_jobs: %w", err)
	}
	return nil
}
