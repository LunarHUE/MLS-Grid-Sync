package processor

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	entmedia "github.com/LunarHUE/MLS-Grid-Sync/ent/media"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/mediaversion"
)

// ProcessChunk is the bulk-projection path for Media (ChunkProcessor). It
// mirrors Property's: one batched entity read + open-version read for the chunk,
// decide each record in memory (shared decideMedia), then bulk close prior
// versions, insert new versions, and upsert entities (UpdateNewValues for
// insert/update; an mlg_can_view/current_version_id/modified_at-only Update for
// the dead-but-defensive delete branch). Media has no parent FK to park and no
// attachment cascade, so there's no relink or cascade step. SourceModifiedAt is
// stamped from raw (the splitter owns it), exactly as the per-record path does.
//
// Records whose media_key appears 2+ times in the chunk are peeled to the
// per-record Process on the same tx (read-your-own-write chains them).
// Semantics are identical to per-record — guarded by the equivalence test.
func (p *MediaProcessor) ProcessChunk(ctx context.Context, tx *ent.Tx, raws []*ent.RawOutput) ([]Outcome, error) {
	outcomes := make([]Outcome, len(raws))
	now := time.Now().UTC()

	fieldsByIdx := make([]*MediaFields, len(raws))
	keyCount := make(map[string]int, len(raws))
	for i, raw := range raws {
		f, err := parseMedia(raw.Payload)
		if err != nil {
			return nil, fmt.Errorf("raw_output=%s parse: %w", raw.ID, err)
		}
		f.SourceModifiedAt = raw.SourceModifiedAt // timestamp seam, see Process
		fieldsByIdx[i] = f
		keyCount[f.MediaKey]++
	}

	uniqueIdx := make([]int, 0, len(raws))
	for i := range raws {
		if keyCount[fieldsByIdx[i].MediaKey] > 1 {
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
	for j, i := range uniqueIdx {
		keys[j] = fieldsByIdx[i].MediaKey
	}
	entities, err := p.bulkLookupEntities(ctx, tx, keys)
	if err != nil {
		return nil, err
	}
	openVersions, err := p.bulkLookupOpenVersions(ctx, tx, keys)
	if err != nil {
		return nil, err
	}

	var (
		closeIDs      []string
		verBuilders   []*ent.MediaVersionCreate
		entityUpserts []*ent.MediaCreate
		tombstones    []*ent.MediaCreate
	)
	for _, i := range uniqueIdx {
		f := fieldsByIdx[i]
		raw := raws[i]
		st := entities[f.MediaKey]
		cv := openVersions[f.MediaKey]

		plan := decideMedia(f, st.exists, st.mlgCanView, cv, raw)
		outcomes[i] = plan.outcome
		if plan.action == actSkip {
			continue
		}

		verID := uuid.Must(uuid.NewV7())
		vb, err := newMediaVersionCreate(tx, f, raw, plan.changeType, now, plan.diff)
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
			c := tx.Media.Create().SetID(f.MediaKey).SetCurrentVersionID(verID)
			applyToMediaCreate(c, f)
			entityUpserts = append(entityUpserts, c)
		case actDeleteExisting, actDeleteFirstSighting:
			c := tx.Media.Create().SetID(f.MediaKey).SetCurrentVersionID(verID)
			applyToMediaCreate(c, f) // sets mlg_can_view=false from f
			tombstones = append(tombstones, c)
		}
	}

	if err := p.bulkWrite(ctx, tx, now, closeIDs, verBuilders, entityUpserts, tombstones); err != nil {
		return nil, err
	}
	return outcomes, nil
}

func (p *MediaProcessor) bulkLookupEntities(ctx context.Context, tx *ent.Tx, keys []string) (map[string]entityState, error) {
	out := make(map[string]entityState, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	// media.id has StorageKey "media_key" — scan column is media_key.
	var rows []struct {
		ID         string `json:"media_key"`
		MlgCanView bool   `json:"mlg_can_view"`
	}
	if err := tx.Media.Query().
		Where(entmedia.IDIn(keys...)).
		Select(entmedia.FieldID, entmedia.FieldMlgCanView).
		Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("bulk lookup media: %w", err)
	}
	for _, r := range rows {
		out[r.ID] = entityState{exists: true, mlgCanView: r.MlgCanView}
	}
	return out, nil
}

func (p *MediaProcessor) bulkLookupOpenVersions(ctx context.Context, tx *ent.Tx, keys []string) (map[string]*ent.MediaVersion, error) {
	out := make(map[string]*ent.MediaVersion, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	vs, err := tx.MediaVersion.Query().
		Where(
			mediaversion.MediaKeyIn(keys...),
			mediaversion.ValidToIsNil(),
		).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("bulk lookup open versions: %w", err)
	}
	for _, v := range vs {
		out[v.MediaKey] = v
	}
	return out, nil
}

func (p *MediaProcessor) bulkWrite(
	ctx context.Context,
	tx *ent.Tx,
	now time.Time,
	closeIDs []string,
	verBuilders []*ent.MediaVersionCreate,
	entityUpserts []*ent.MediaCreate,
	tombstones []*ent.MediaCreate,
) error {
	if len(closeIDs) > 0 {
		if _, err := tx.MediaVersion.Update().
			Where(mediaversion.IDIn(closeIDs...)).
			SetValidTo(now).
			Save(ctx); err != nil {
			return fmt.Errorf("bulk close versions: %w", err)
		}
	}
	for sub := range slices.Chunk(verBuilders, maxBulkRows) {
		if _, err := tx.MediaVersion.CreateBulk(sub...).Save(ctx); err != nil {
			return fmt.Errorf("bulk insert versions: %w", err)
		}
	}
	// UpdateNewValues refreshes data + current_version_id + modified_at, ignoring
	// id and immutable created_at. attachment_id is never in the INSERT column
	// set (applyToMediaCreate doesn't set it), so the download pointer on an
	// existing row is preserved.
	for sub := range slices.Chunk(entityUpserts, maxBulkRows) {
		if err := tx.Media.CreateBulk(sub...).
			OnConflictColumns(entmedia.FieldID).
			Update(func(u *ent.MediaUpsert) {
				// Clear-on-nil parity; preserve PK, created_at, and the
				// attachment_id download pointer (never set by applyToMediaCreate).
				upsertSetExcluded(u.UpdateSet, entmedia.Columns, entmedia.FieldID, entmedia.FieldCreatedAt, entmedia.FieldAttachmentID)
			}).
			Exec(ctx); err != nil {
			return fmt.Errorf("bulk upsert entities: %w", err)
		}
	}
	for sub := range slices.Chunk(tombstones, maxBulkRows) {
		if err := tx.Media.CreateBulk(sub...).
			OnConflictColumns(entmedia.FieldID).
			Update(func(u *ent.MediaUpsert) {
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
