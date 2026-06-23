package processor

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/openhouse"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/openhouseversion"
)

// ProcessChunk is the bulk-projection path for OpenHouse (ChunkProcessor). Like
// Property's, plus the parking FK: parent existence is resolved once for the
// whole chunk (one query), and the entity upsert resolves parent_listing_key as
// COALESCE(existing, excluded) — the per-record promote-once/never-clear guard.
func (p *OpenHouseProcessor) ProcessChunk(ctx context.Context, tx *ent.Tx, raws []*ent.RawOutput) ([]Outcome, error) {
	outcomes := make([]Outcome, len(raws))
	now := time.Now().UTC()

	fieldsByIdx := make([]*OpenHouseFields, len(raws))
	keyCount := make(map[string]int, len(raws))
	for i, raw := range raws {
		f, err := parseOpenHouse(raw.Payload)
		if err != nil {
			return nil, fmt.Errorf("raw_output=%s parse: %w", raw.ID, err)
		}
		fieldsByIdx[i] = f
		keyCount[f.OpenHouseKey]++
	}

	uniqueIdx := make([]int, 0, len(raws))
	for i := range raws {
		if keyCount[fieldsByIdx[i].OpenHouseKey] > 1 {
			oc, err := p.Process(ctx, tx, raws[i])
			if err != nil {
				return nil, fmt.Errorf("raw_output=%s: %w", raws[i].ID, err)
			}
			outcomes[i] = oc
			continue
		}
		uniqueIdx = append(uniqueIdx, i)
	}

	keys := make([]string, len(uniqueIdx))
	parentKeys := make([]string, len(uniqueIdx))
	for j, i := range uniqueIdx {
		keys[j] = fieldsByIdx[i].OpenHouseKey
		parentKeys[j] = fieldsByIdx[i].ListingKey
	}
	entities, err := p.bulkLookupEntities(ctx, tx, keys)
	if err != nil {
		return nil, err
	}
	openVersions, err := p.bulkLookupOpenVersions(ctx, tx, keys)
	if err != nil {
		return nil, err
	}
	existingParents, err := bulkExistingParentKeys(ctx, tx, parentKeys)
	if err != nil {
		return nil, err
	}

	var (
		closeIDs      []string
		verBuilders   []*ent.OpenHouseVersionCreate
		entityUpserts []*ent.OpenHouseCreate
		tombstones    []*ent.OpenHouseCreate
	)
	for _, i := range uniqueIdx {
		f := fieldsByIdx[i]
		raw := raws[i]
		st := entities[f.OpenHouseKey]
		cv := openVersions[f.OpenHouseKey]

		plan := decideOpenHouse(f, st.exists, st.mlgCanView, cv, raw)
		outcomes[i] = plan.outcome
		if plan.action == actSkip {
			continue
		}

		parentFK := parentFKFor(f.ListingKey, existingParents)
		verID := uuid.Must(uuid.NewV7())
		vb, err := newOpenHouseVersionCreate(tx, f, raw, plan.changeType, now, plan.diff)
		if err != nil {
			return nil, fmt.Errorf("raw_output=%s build version: %w", raw.ID, err)
		}
		vb.SetID(verID.String())
		verBuilders = append(verBuilders, vb)
		if plan.closeVersionID != nil {
			closeIDs = append(closeIDs, *plan.closeVersionID)
		}

		c := tx.OpenHouse.Create().
			SetID(f.OpenHouseKey).
			SetCurrentVersionID(verID).
			SetNillableParentListingKey(parentFK)
		applyToOpenHouseCreate(c, f)
		switch plan.action {
		case actInsert, actUpdate:
			entityUpserts = append(entityUpserts, c)
		case actDeleteExisting, actDeleteFirstSighting:
			tombstones = append(tombstones, c)
		}
	}

	if err := p.bulkWrite(ctx, tx, now, closeIDs, verBuilders, entityUpserts, tombstones); err != nil {
		return nil, err
	}
	return outcomes, nil
}

func (p *OpenHouseProcessor) bulkLookupEntities(ctx context.Context, tx *ent.Tx, keys []string) (map[string]entityState, error) {
	out := make(map[string]entityState, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	// open_house.id has StorageKey "open_house_key".
	var rows []struct {
		ID         string `json:"open_house_key"`
		MlgCanView bool   `json:"mlg_can_view"`
	}
	if err := tx.OpenHouse.Query().
		Where(openhouse.IDIn(keys...)).
		Select(openhouse.FieldID, openhouse.FieldMlgCanView).
		Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("bulk lookup open_house: %w", err)
	}
	for _, r := range rows {
		out[r.ID] = entityState{exists: true, mlgCanView: r.MlgCanView}
	}
	return out, nil
}

func (p *OpenHouseProcessor) bulkLookupOpenVersions(ctx context.Context, tx *ent.Tx, keys []string) (map[string]*ent.OpenHouseVersion, error) {
	out := make(map[string]*ent.OpenHouseVersion, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	vs, err := tx.OpenHouseVersion.Query().
		Where(openhouseversion.OpenHouseKeyIn(keys...), openhouseversion.ValidToIsNil()).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("bulk lookup open versions: %w", err)
	}
	for _, v := range vs {
		out[v.OpenHouseKey] = v
	}
	return out, nil
}

func (p *OpenHouseProcessor) bulkWrite(
	ctx context.Context,
	tx *ent.Tx,
	now time.Time,
	closeIDs []string,
	verBuilders []*ent.OpenHouseVersionCreate,
	entityUpserts []*ent.OpenHouseCreate,
	tombstones []*ent.OpenHouseCreate,
) error {
	if len(closeIDs) > 0 {
		if _, err := tx.OpenHouseVersion.Update().
			Where(openhouseversion.IDIn(closeIDs...)).
			SetValidTo(now).
			Save(ctx); err != nil {
			return fmt.Errorf("bulk close versions: %w", err)
		}
	}
	for sub := range slices.Chunk(verBuilders, maxBulkRows) {
		if _, err := tx.OpenHouseVersion.CreateBulk(sub...).Save(ctx); err != nil {
			return fmt.Errorf("bulk insert versions: %w", err)
		}
	}
	// Insert+update: UpdateNewValues refreshes data + current_version_id +
	// modified_at (ignoring id/created_at); the trailing Update overrides
	// parent_listing_key to COALESCE(existing, excluded) — promote-once.
	for sub := range slices.Chunk(entityUpserts, maxBulkRows) {
		if err := tx.OpenHouse.CreateBulk(sub...).
			OnConflictColumns(openhouse.FieldID).
			Update(func(u *ent.OpenHouseUpsert) {
				// Clear-on-nil parity for data columns; parent_listing_key via
				// COALESCE (promote-once); preserve PK + created_at.
				upsertSetExcluded(u.UpdateSet, openhouse.Columns, openhouse.FieldID, openhouse.FieldCreatedAt, childParentColumn)
				setParentListingKeyCoalesce(u.UpdateSet)
			}).
			Exec(ctx); err != nil {
			return fmt.Errorf("bulk upsert entities: %w", err)
		}
	}
	// Tombstones: flip only mlg_can_view + current_version_id (+ modified_at) on
	// an existing row (parent_listing_key untouched → preserved); first sighting
	// inserts the sparse tombstone with parent_listing_key from the builder.
	for sub := range slices.Chunk(tombstones, maxBulkRows) {
		if err := tx.OpenHouse.CreateBulk(sub...).
			OnConflictColumns(openhouse.FieldID).
			Update(func(u *ent.OpenHouseUpsert) {
				u.UpdateMlgCanView()
				u.UpdateCurrentVersionID()
				u.UpdateModifiedAt()
			}).
			Exec(ctx); err != nil {
			return fmt.Errorf("bulk tombstone entities: %w", err)
		}
	}
	return nil
}
