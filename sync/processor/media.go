package processor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/google/uuid"

	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent"
	entmedia "github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/media"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/mediaversion"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/rawoutput"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/version"
)

// MediaProcessor handles the Media resource. Media is polymorphic
// (resource_type discriminator + resource_record_key string) so there is no
// FK to park or re-link — the parent reference is just data on the row.
type MediaProcessor struct{}

func NewMediaProcessor() *MediaProcessor { return &MediaProcessor{} }

func (*MediaProcessor) Resource() rawoutput.Resource { return rawoutput.ResourceMedia }

func (p *MediaProcessor) Process(ctx context.Context, tx *ent.Tx, raw *ent.RawOutput) (Outcome, error) {
	payload, err := json.Marshal(raw.Payload)
	if err != nil {
		return OutcomeUnknown, fmt.Errorf("marshal payload: %w", err)
	}
	fields, err := parseMedia(payload)
	if err != nil {
		return OutcomeUnknown, fmt.Errorf("parse: %w", err)
	}
	// Timestamp seam (decision 1a): the parser intentionally does NOT
	// extract SourceModifiedAt from the payload — the splitter
	// (sync/raw.go) owns timestamp extraction (child's
	// MediaModificationTimestamp, parent fallback) and stamps
	// raw_output.source_modified_at. Sourcing the field from raw here
	// aligns the parsed value with the stale-skip comparison value
	// (below) by construction — one source of truth.
	fields.SourceModifiedAt = raw.SourceModifiedAt

	current, err := tx.Media.Query().Where(entmedia.IDEQ(fields.MediaKey)).Only(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return OutcomeUnknown, fmt.Errorf("lookup media: %w", err)
	}
	entityExists := err == nil

	var currentVersion *ent.MediaVersion
	if entityExists {
		currentVersion, err = tx.MediaVersion.Query().
			Where(
				mediaversion.MediaKey(fields.MediaKey),
				mediaversion.ValidToIsNil(),
			).
			Only(ctx)
		if err != nil && !ent.IsNotFound(err) {
			return OutcomeUnknown, fmt.Errorf("lookup current version: %w", err)
		}
		if ent.IsNotFound(err) {
			currentVersion = nil
		}
	}

	now := time.Now().UTC()

	if currentVersion != nil && !raw.SourceModifiedAt.After(currentVersion.SourceModifiedAt) {
		return OutcomeSkipStale, nil
	}

	// Dead-but-defensive (audit 2026-06-11): expanded-media payloads
	// carry no per-child MlgCanView field (0% of 586k rows), so this
	// branch is unreachable from the real feed today. Protection for
	// hidden listings is the Phase 3 parent-visibility resolver
	// filter, NOT per-child tombstones. Kept defensive so the logic
	// wakes up rather than needing rediscovery if MLS Grid ever
	// starts embedding per-child visibility. Cross-ref: sync/raw.go
	// splitExpandedChildren header doc, hidden-listing watch item.
	if !fields.MlgCanView {
		if current != nil && !current.MlgCanView {
			return OutcomeSkipTombstoned, nil
		}
		return OutcomeDelete, p.applyDelete(ctx, tx, fields, raw, current, currentVersion, now)
	}

	if !entityExists {
		return OutcomeInsert, p.applyInsert(ctx, tx, fields, raw, now)
	}

	diff := diffMediaFields(currentVersion, fields)
	if len(diff) == 0 {
		return OutcomeSkipNoDiff, nil
	}
	return OutcomeUpdate, p.applyUpdate(ctx, tx, fields, raw, current, currentVersion, diff, now)
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

func (p *MediaProcessor) applyUpdate(ctx context.Context, tx *ent.Tx, f *MediaFields, raw *ent.RawOutput, current *ent.Media, currentVersion *ent.MediaVersion, diff map[string]any, now time.Time) error {
	if currentVersion != nil {
		if _, err := tx.MediaVersion.UpdateOneID(currentVersion.ID).SetValidTo(now).Save(ctx); err != nil {
			return fmt.Errorf("close current version: %w", err)
		}
	}

	ver, err := newMediaVersionCreate(tx, f, raw, mediaversion.ChangeTypeUpdate, now, diff)
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

	u := tx.Media.UpdateOneID(current.ID).SetCurrentVersionID(verID)
	applyToMediaUpdate(u, f)
	if _, err := u.Save(ctx); err != nil {
		return fmt.Errorf("update entity: %w", err)
	}
	return nil
}

func (p *MediaProcessor) applyDelete(ctx context.Context, tx *ent.Tx, f *MediaFields, raw *ent.RawOutput, current *ent.Media, currentVersion *ent.MediaVersion, now time.Time) error {
	if currentVersion != nil {
		if _, err := tx.MediaVersion.UpdateOneID(currentVersion.ID).SetValidTo(now).Save(ctx); err != nil {
			return fmt.Errorf("close current version: %w", err)
		}
	}

	ver, err := newMediaVersionCreate(tx, f, raw, mediaversion.ChangeTypeDelete, now, nil)
	if err != nil {
		return fmt.Errorf("build delete version: %w", err)
	}
	verRow, err := ver.Save(ctx)
	if err != nil {
		return fmt.Errorf("save delete version: %w", err)
	}
	verID, err := uuid.Parse(verRow.ID)
	if err != nil {
		return fmt.Errorf("version id is not a uuid: %w", err)
	}

	if current == nil {
		c := tx.Media.Create().SetID(f.MediaKey).SetCurrentVersionID(verID)
		applyToMediaCreate(c, f)
		if _, err := c.Save(ctx); err != nil {
			return fmt.Errorf("create tombstoned entity: %w", err)
		}
		return nil
	}

	u := tx.Media.UpdateOneID(current.ID).SetCurrentVersionID(verID).SetMlgCanView(false)
	if _, err := u.Save(ctx); err != nil {
		return fmt.Errorf("tombstone entity: %w", err)
	}
	return nil
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
