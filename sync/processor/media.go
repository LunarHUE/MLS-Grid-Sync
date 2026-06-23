package processor

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/google/uuid"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	entmedia "github.com/LunarHUE/MLS-Grid-Sync/ent/media"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/mediaversion"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/version"
)

// MediaProcessor handles the Media resource. Media is polymorphic
// (resource_type discriminator + resource_record_key string) so there is no
// FK to park or re-link — the parent reference is just data on the row.
type MediaProcessor struct{}

func NewMediaProcessor() *MediaProcessor { return &MediaProcessor{} }

func (*MediaProcessor) Resource() rawoutput.Resource { return rawoutput.ResourceMedia }

func (p *MediaProcessor) Process(ctx context.Context, tx *ent.Tx, raw *ent.RawOutput) (Outcome, error) {
	fields, err := parseMedia(raw.Payload)
	if err != nil {
		return OutcomeUnknown, fmt.Errorf("parse: %w", err)
	}
	// Timestamp seam (decision 1a): the parser intentionally does NOT
	// extract SourceModifiedAt from the payload — the splitter (sync/raw.go)
	// owns timestamp extraction and stamps raw_output.source_modified_at.
	// Sourcing it from raw here aligns the parsed value with the stale-skip
	// comparison value by construction.
	fields.SourceModifiedAt = raw.SourceModifiedAt

	entityExists, currentMlgCanView, err := p.lookupEntity(ctx, tx, fields.MediaKey)
	if err != nil {
		return OutcomeUnknown, err
	}

	var currentVersion *ent.MediaVersion
	if entityExists {
		currentVersion, err = p.lookupCurrentVersion(ctx, tx, fields.MediaKey)
		if err != nil {
			return OutcomeUnknown, err
		}
	}

	now := time.Now().UTC()
	plan := decideMedia(fields, entityExists, currentMlgCanView, currentVersion, raw)
	if err := p.applyPlan(ctx, tx, fields, raw, plan, now); err != nil {
		return OutcomeUnknown, err
	}
	return plan.outcome, nil
}

// lookupEntity reads only existence + mlg_can_view (the tombstone-skip flag);
// the per-record path previously read the full row only to reach current.ID,
// which equals the known media_key.
func (p *MediaProcessor) lookupEntity(ctx context.Context, tx *ent.Tx, mediaKey string) (exists, mlgCanView bool, err error) {
	var canView []bool
	if err := tx.Media.Query().
		Where(entmedia.IDEQ(mediaKey)).
		Select(entmedia.FieldMlgCanView).
		Scan(ctx, &canView); err != nil {
		return false, false, fmt.Errorf("lookup media: %w", err)
	}
	return len(canView) > 0, len(canView) > 0 && canView[0], nil
}

// lookupCurrentVersion returns the open (valid_to IS NULL) version for a
// media_key, or nil if none.
func (p *MediaProcessor) lookupCurrentVersion(ctx context.Context, tx *ent.Tx, mediaKey string) (*ent.MediaVersion, error) {
	v, err := tx.MediaVersion.Query().
		Where(
			mediaversion.MediaKey(mediaKey),
			mediaversion.ValidToIsNil(),
		).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("lookup current version: %w", err)
	}
	return v, nil
}

// mediaPlan mirrors propertyPlan for the Media resource (shared action enum).
type mediaPlan struct {
	action         propertyAction
	outcome        Outcome
	changeType     mediaversion.ChangeType
	diff           map[string]any
	closeVersionID *string
}

// decideMedia is the pure decision shared by Process (per-record) and
// ProcessChunk (bulk). The delete branch is dead-but-defensive (expanded media
// carry no per-child MlgCanView today) but preserved for equivalence.
func decideMedia(f *MediaFields, entityExists, currentMlgCanView bool, currentVersion *ent.MediaVersion, raw *ent.RawOutput) mediaPlan {
	if currentVersion != nil && !raw.SourceModifiedAt.After(currentVersion.SourceModifiedAt) {
		return mediaPlan{action: actSkip, outcome: OutcomeSkipStale}
	}

	if !f.MlgCanView {
		if entityExists && !currentMlgCanView {
			return mediaPlan{action: actSkip, outcome: OutcomeSkipTombstoned}
		}
		plan := mediaPlan{outcome: OutcomeDelete, changeType: mediaversion.ChangeTypeDelete}
		if currentVersion != nil {
			id := currentVersion.ID
			plan.closeVersionID = &id
		}
		if entityExists {
			plan.action = actDeleteExisting
		} else {
			plan.action = actDeleteFirstSighting
		}
		return plan
	}

	if !entityExists {
		return mediaPlan{action: actInsert, outcome: OutcomeInsert, changeType: mediaversion.ChangeTypeInsert}
	}

	diff := diffMediaFields(currentVersion, f)
	if len(diff) == 0 {
		return mediaPlan{action: actSkip, outcome: OutcomeSkipNoDiff}
	}
	plan := mediaPlan{action: actUpdate, outcome: OutcomeUpdate, changeType: mediaversion.ChangeTypeUpdate, diff: diff}
	if currentVersion != nil {
		id := currentVersion.ID
		plan.closeVersionID = &id
	}
	return plan
}

func (p *MediaProcessor) applyPlan(ctx context.Context, tx *ent.Tx, f *MediaFields, raw *ent.RawOutput, plan mediaPlan, now time.Time) error {
	switch plan.action {
	case actSkip:
		return nil
	case actInsert:
		return p.applyInsert(ctx, tx, f, raw, now)
	case actUpdate:
		return p.applyUpdate(ctx, tx, f, raw, plan.closeVersionID, plan.diff, now)
	case actDeleteExisting:
		return p.applyDelete(ctx, tx, f, raw, plan.closeVersionID, true, now)
	case actDeleteFirstSighting:
		return p.applyDelete(ctx, tx, f, raw, plan.closeVersionID, false, now)
	}
	return nil
}

func (p *MediaProcessor) applyInsert(ctx context.Context, tx *ent.Tx, f *MediaFields, raw *ent.RawOutput, now time.Time) error {
	ver, err := newMediaVersionCreate(tx, f, raw, mediaversion.ChangeTypeInsert, now, nil)
	if err != nil {
		return fmt.Errorf("build version: %w", err)
	}
	verRow, err := ver.Save(ctx)
	if err != nil {
		return fmt.Errorf("save version: %w", err)
	}
	verID, err := uuid.Parse(verRow.ID)
	if err != nil {
		return fmt.Errorf("version id is not a uuid: %w", err)
	}

	c := tx.Media.Create().SetID(f.MediaKey).SetCurrentVersionID(verID)
	applyToMediaCreate(c, f)
	if _, err := c.Save(ctx); err != nil {
		return fmt.Errorf("create entity: %w", err)
	}
	return nil
}

func (p *MediaProcessor) applyUpdate(ctx context.Context, tx *ent.Tx, f *MediaFields, raw *ent.RawOutput, closeVersionID *string, diff map[string]any, now time.Time) error {
	verID, err := p.closeAndInsertVersion(ctx, tx, f, raw, mediaversion.ChangeTypeUpdate, closeVersionID, diff, now)
	if err != nil {
		return err
	}
	u := tx.Media.UpdateOneID(f.MediaKey).SetCurrentVersionID(verID)
	applyToMediaUpdate(u, f)
	if _, err := u.Save(ctx); err != nil {
		return fmt.Errorf("update entity: %w", err)
	}
	return nil
}

func (p *MediaProcessor) applyDelete(ctx context.Context, tx *ent.Tx, f *MediaFields, raw *ent.RawOutput, closeVersionID *string, entityExists bool, now time.Time) error {
	verID, err := p.closeAndInsertVersion(ctx, tx, f, raw, mediaversion.ChangeTypeDelete, closeVersionID, nil, now)
	if err != nil {
		return err
	}
	if !entityExists {
		c := tx.Media.Create().SetID(f.MediaKey).SetCurrentVersionID(verID)
		applyToMediaCreate(c, f)
		if _, err := c.Save(ctx); err != nil {
			return fmt.Errorf("create tombstoned entity: %w", err)
		}
		return nil
	}
	u := tx.Media.UpdateOneID(f.MediaKey).SetCurrentVersionID(verID).SetMlgCanView(false)
	if _, err := u.Save(ctx); err != nil {
		return fmt.Errorf("tombstone entity: %w", err)
	}
	return nil
}

// closeAndInsertVersion closes the prior open version (by id, when present) and
// inserts the new version row, returning its parsed UUID for the entity link.
func (p *MediaProcessor) closeAndInsertVersion(ctx context.Context, tx *ent.Tx, f *MediaFields, raw *ent.RawOutput, ct mediaversion.ChangeType, closeVersionID *string, diff map[string]any, now time.Time) (uuid.UUID, error) {
	if closeVersionID != nil {
		if _, err := tx.MediaVersion.UpdateOneID(*closeVersionID).SetValidTo(now).Save(ctx); err != nil {
			return uuid.Nil, fmt.Errorf("close current version: %w", err)
		}
	}
	ver, err := newMediaVersionCreate(tx, f, raw, ct, now, diff)
	if err != nil {
		return uuid.Nil, fmt.Errorf("build version: %w", err)
	}
	verRow, err := ver.Save(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("save version: %w", err)
	}
	verID, err := uuid.Parse(verRow.ID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("version id is not a uuid: %w", err)
	}
	return verID, nil
}

func newMediaVersionCreate(tx *ent.Tx, f *MediaFields, raw *ent.RawOutput, ct mediaversion.ChangeType, now time.Time, diff map[string]any) (*ent.MediaVersionCreate, error) {
	if raw == nil {
		return nil, errors.New("raw_output required")
	}
	c := tx.MediaVersion.Create().
		SetMediaKey(f.MediaKey).
		SetSyncEventID(raw.SyncEventID).
		SetRawOutputID(raw.ID).
		SetValidFrom(now).
		SetChangeType(ct).
		SetProcessorVersion(version.Info())
	if diff != nil {
		c.SetChangedFields(diff)
	}
	applyToMediaVersionCreate(c, f)
	return c, nil
}

// --- diff ---

func diffMediaFields(currentVersion *ent.MediaVersion, next *MediaFields) map[string]any {
	if currentVersion == nil {
		return nil
	}
	diff := map[string]any{}
	push := func(name string, oldV, newV any) {
		diff[name] = map[string]any{"old": oldV, "new": newV}
	}

	if !equalStringPtr(currentVersion.OriginatingSystemName, next.OriginatingSystemName) {
		push("originating_system_name", currentVersion.OriginatingSystemName, next.OriginatingSystemName)
	}
	if currentVersion.MlgCanView != next.MlgCanView {
		push("mlg_can_view", currentVersion.MlgCanView, next.MlgCanView)
	}
	if !equalStringSlice(currentVersion.MlgCanUse, next.MlgCanUse) {
		push("mlg_can_use", currentVersion.MlgCanUse, next.MlgCanUse)
	}
	if string(currentVersion.ResourceType) != next.ResourceType {
		push("resource_type", string(currentVersion.ResourceType), next.ResourceType)
	}
	if currentVersion.ResourceRecordKey != next.ResourceRecordKey {
		push("resource_record_key", currentVersion.ResourceRecordKey, next.ResourceRecordKey)
	}

	diffMediaReflect(currentVersion, next, diff)

	if !reflect.DeepEqual(currentVersion.ExtendedFields, next.ExtendedFields) {
		push("extended_fields", currentVersion.ExtendedFields, next.ExtendedFields)
	}

	if len(diff) == 0 {
		return nil
	}
	return diff
}

func diffMediaReflect(currentVersion *ent.MediaVersion, next *MediaFields, out map[string]any) {
	vNext := reflect.ValueOf(next).Elem()
	tNext := vNext.Type()
	vCur := reflect.ValueOf(currentVersion).Elem()
	tCur := vCur.Type()

	skip := map[string]bool{
		"MediaKey":              true,
		"SourceModifiedAt":      true,
		"OriginatingSystemName": true,
		"MlgCanView":            true,
		"MlgCanUse":             true,
		"ResourceType":          true,
		"ResourceRecordKey":     true,
		"ExtendedFields":        true,
	}

	for i := 0; i < tNext.NumField(); i++ {
		name := tNext.Field(i).Name
		if skip[name] {
			continue
		}
		fNext := vNext.Field(i)
		if fNext.Kind() != reflect.Ptr {
			continue
		}
		if _, ok := tCur.FieldByName(name); !ok {
			continue
		}
		fCur := vCur.FieldByName(name)
		if fCur.Kind() != reflect.Ptr {
			continue
		}
		if !equalPtr(fCur, fNext) {
			out[snakeCase(name)] = map[string]any{
				"old": ptrToAny(fCur),
				"new": ptrToAny(fNext),
			}
		}
	}
}

// --- apply ---

func applyToMediaCreate(c *ent.MediaCreate, f *MediaFields) {
	c.SetSourceModifiedAt(f.SourceModifiedAt).
		SetMlgCanView(f.MlgCanView).
		SetNillableOriginatingSystemName(f.OriginatingSystemName).
		SetResourceType(entmedia.ResourceType(f.ResourceType)).
		SetResourceRecordKey(f.ResourceRecordKey).
		SetNillableMediaType(f.MediaType).
		SetNillableMediaURL(f.MediaURL).
		SetNillableImageHeight(f.ImageHeight).
		SetNillableImageWidth(f.ImageWidth).
		SetNillableImageSizeDescription(f.ImageSizeDescription).
		SetNillableLongDescription(f.LongDescription).
		SetNillableOrder(f.Order).
		SetNillablePreferredPhotoYn(f.PreferredPhotoYn).
		SetNillableMediaModificationTimestamp(f.MediaModificationTimestamp)
	if f.MlgCanUse != nil {
		c.SetMlgCanUse(f.MlgCanUse)
	}
	if f.ExtendedFields != nil {
		c.SetExtendedFields(f.ExtendedFields)
	}
}

// applyToMediaUpdate uses clear-on-nil semantics — see apply.go.
func applyToMediaUpdate(c *ent.MediaUpdateOne, f *MediaFields) {
	c.SetSourceModifiedAt(f.SourceModifiedAt).
		SetMlgCanView(f.MlgCanView).
		SetResourceType(entmedia.ResourceType(f.ResourceType)).
		SetResourceRecordKey(f.ResourceRecordKey)
	setOrClearStr(f.OriginatingSystemName, c.SetOriginatingSystemName, c.ClearOriginatingSystemName)
	setOrClearSlice(f.MlgCanUse, c.SetMlgCanUse, c.ClearMlgCanUse)
	setOrClearStr(f.MediaType, c.SetMediaType, c.ClearMediaType)
	setOrClearStr(f.MediaURL, c.SetMediaURL, c.ClearMediaURL)
	setOrClearInt64(f.ImageHeight, c.SetImageHeight, c.ClearImageHeight)
	setOrClearInt64(f.ImageWidth, c.SetImageWidth, c.ClearImageWidth)
	setOrClearStr(f.ImageSizeDescription, c.SetImageSizeDescription, c.ClearImageSizeDescription)
	setOrClearStr(f.LongDescription, c.SetLongDescription, c.ClearLongDescription)
	setOrClearInt16(f.Order, c.SetOrder, c.ClearOrder)
	setOrClearBool(f.PreferredPhotoYn, c.SetPreferredPhotoYn, c.ClearPreferredPhotoYn)
	setOrClearTime(f.MediaModificationTimestamp, c.SetMediaModificationTimestamp, c.ClearMediaModificationTimestamp)
	setOrClearMap(f.ExtendedFields, c.SetExtendedFields, c.ClearExtendedFields)
}

func applyToMediaVersionCreate(c *ent.MediaVersionCreate, f *MediaFields) {
	c.SetSourceModifiedAt(f.SourceModifiedAt).
		SetMlgCanView(f.MlgCanView).
		SetNillableOriginatingSystemName(f.OriginatingSystemName).
		SetResourceType(mediaversion.ResourceType(f.ResourceType)).
		SetResourceRecordKey(f.ResourceRecordKey).
		SetNillableMediaType(f.MediaType).
		SetNillableMediaURL(f.MediaURL).
		SetNillableImageHeight(f.ImageHeight).
		SetNillableImageWidth(f.ImageWidth).
		SetNillableImageSizeDescription(f.ImageSizeDescription).
		SetNillableLongDescription(f.LongDescription).
		SetNillableOrder(f.Order).
		SetNillablePreferredPhotoYn(f.PreferredPhotoYn).
		SetNillableMediaModificationTimestamp(f.MediaModificationTimestamp)
	if f.MlgCanUse != nil {
		c.SetMlgCanUse(f.MlgCanUse)
	}
	if f.ExtendedFields != nil {
		c.SetExtendedFields(f.ExtendedFields)
	}
}
