package processor

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/propertyunittype"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/propertyunittypeversion"
)

// ProcessChunk is the bulk-projection path for PropertyUnitType (ChunkProcessor),
// identical in shape to PropertyRoom.
func (p *PropertyUnitTypeProcessor) ProcessChunk(ctx context.Context, tx *ent.Tx, raws []*ent.RawOutput) ([]Outcome, error) {
	outcomes := make([]Outcome, len(raws))
	now := time.Now().UTC()

	fieldsByIdx := make([]*PropertyUnitTypeFields, len(raws))
	keyCount := make(map[string]int, len(raws))
	for i, raw := range raws {
		f, err := parsePropertyUnitType(raw.Payload)
		if err != nil {
			return nil, fmt.Errorf("raw_output=%s parse: %w", raw.ID, err)
		}
		f.SourceModifiedAt = raw.SourceModifiedAt
		fieldsByIdx[i] = f
		keyCount[f.UnitTypeKey]++
	}

	uniqueIdx := make([]int, 0, len(raws))
	for i := range raws {
		if keyCount[fieldsByIdx[i].UnitTypeKey] > 1 {
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
		keys[j] = fieldsByIdx[i].UnitTypeKey
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
		verBuilders   []*ent.PropertyUnitTypeVersionCreate
		entityUpserts []*ent.PropertyUnitTypeCreate
		tombstones    []*ent.PropertyUnitTypeCreate
	)
	for _, i := range uniqueIdx {
		f := fieldsByIdx[i]
		raw := raws[i]
		st := entities[f.UnitTypeKey]
		cv := openVersions[f.UnitTypeKey]

		plan := decidePropertyUnitType(f, st.exists, st.mlgCanView, cv, raw)
		outcomes[i] = plan.outcome
		if plan.action == actSkip {
			continue
		}

		parentFK := parentFKFor(f.ListingKey, existingParents)
		verID := uuid.Must(uuid.NewV7())
		vb, err := newPropertyUnitTypeVersionCreate(tx, f, raw, plan.changeType, now, plan.diff)
		if err != nil {
			return nil, fmt.Errorf("raw_output=%s build version: %w", raw.ID, err)
		}
		vb.SetID(verID.String())
		verBuilders = append(verBuilders, vb)
		if plan.closeVersionID != nil {
			closeIDs = append(closeIDs, *plan.closeVersionID)
		}

		c := tx.PropertyUnitType.Create().
			SetID(f.UnitTypeKey).
			SetCurrentVersionID(verID).
			SetNillableParentListingKey(parentFK)
		applyToPropertyUnitTypeCreate(c, f)
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

func (p *PropertyUnitTypeProcessor) bulkLookupEntities(ctx context.Context, tx *ent.Tx, keys []string) (map[string]entityState, error) {
	out := make(map[string]entityState, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	var rows []struct {
		ID         string `json:"unit_type_key"`
		MlgCanView bool   `json:"mlg_can_view"`
	}
	if err := tx.PropertyUnitType.Query().
		Where(propertyunittype.IDIn(keys...)).
		Select(propertyunittype.FieldID, propertyunittype.FieldMlgCanView).
		Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("bulk lookup property_unit_type: %w", err)
	}
	for _, r := range rows {
		out[r.ID] = entityState{exists: true, mlgCanView: r.MlgCanView}
	}
	return out, nil
}

func (p *PropertyUnitTypeProcessor) bulkLookupOpenVersions(ctx context.Context, tx *ent.Tx, keys []string) (map[string]*ent.PropertyUnitTypeVersion, error) {
	out := make(map[string]*ent.PropertyUnitTypeVersion, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	vs, err := tx.PropertyUnitTypeVersion.Query().
		Where(propertyunittypeversion.UnitTypeKeyIn(keys...), propertyunittypeversion.ValidToIsNil()).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("bulk lookup open versions: %w", err)
	}
	for _, v := range vs {
		out[v.UnitTypeKey] = v
	}
	return out, nil
}

func (p *PropertyUnitTypeProcessor) bulkWrite(
	ctx context.Context,
	tx *ent.Tx,
	now time.Time,
	closeIDs []string,
	verBuilders []*ent.PropertyUnitTypeVersionCreate,
	entityUpserts []*ent.PropertyUnitTypeCreate,
	tombstones []*ent.PropertyUnitTypeCreate,
) error {
	if len(closeIDs) > 0 {
		if _, err := tx.PropertyUnitTypeVersion.Update().
			Where(propertyunittypeversion.IDIn(closeIDs...)).
			SetValidTo(now).
			Save(ctx); err != nil {
			return fmt.Errorf("bulk close versions: %w", err)
		}
	}
	for sub := range slices.Chunk(verBuilders, maxBulkRows) {
		if _, err := tx.PropertyUnitTypeVersion.CreateBulk(sub...).Save(ctx); err != nil {
			return fmt.Errorf("bulk insert versions: %w", err)
		}
	}
	for sub := range slices.Chunk(entityUpserts, maxBulkRows) {
		if err := tx.PropertyUnitType.CreateBulk(sub...).
			OnConflictColumns(propertyunittype.FieldID).
			Update(func(u *ent.PropertyUnitTypeUpsert) {
				upsertSetExcluded(u.UpdateSet, propertyunittype.Columns, propertyunittype.FieldID, propertyunittype.FieldCreatedAt, childParentColumn)
				setParentListingKeyCoalesce(u.UpdateSet)
			}).
			Exec(ctx); err != nil {
			return fmt.Errorf("bulk upsert entities: %w", err)
		}
	}
	for sub := range slices.Chunk(tombstones, maxBulkRows) {
		if err := tx.PropertyUnitType.CreateBulk(sub...).
			OnConflictColumns(propertyunittype.FieldID).
			Update(func(u *ent.PropertyUnitTypeUpsert) {
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
