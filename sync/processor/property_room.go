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
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/property"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/propertyroom"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/propertyroomversion"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/rawoutput"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/version"
)

// PropertyRoomProcessor — same parking semantics as OpenHouseProcessor.
type PropertyRoomProcessor struct{}

func NewPropertyRoomProcessor() *PropertyRoomProcessor { return &PropertyRoomProcessor{} }

func (*PropertyRoomProcessor) Resource() rawoutput.Resource {
	return rawoutput.ResourcePropertyRooms
}

func (p *PropertyRoomProcessor) Process(ctx context.Context, tx *ent.Tx, raw *ent.RawOutput) (Outcome, error) {
	payload, err := json.Marshal(raw.Payload)
	if err != nil {
		return OutcomeUnknown, fmt.Errorf("marshal payload: %w", err)
	}
	fields, err := parsePropertyRoom(payload)
	if err != nil {
		return OutcomeUnknown, fmt.Errorf("parse: %w", err)
	}
	// Timestamp seam (decision 1a): see media.go for the rationale.
	// Splitter owns timestamp extraction; processor sources from raw
	// to align with the stale-skip comparison value (below).
	fields.SourceModifiedAt = raw.SourceModifiedAt

	current, err := tx.PropertyRoom.Query().Where(propertyroom.IDEQ(fields.RoomKey)).Only(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return OutcomeUnknown, fmt.Errorf("lookup property_room: %w", err)
	}
	entityExists := err == nil

	var currentVersion *ent.PropertyRoomVersion
	if entityExists {
		currentVersion, err = tx.PropertyRoomVersion.Query().
			Where(
				propertyroomversion.RoomKey(fields.RoomKey),
				propertyroomversion.ValidToIsNil(),
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

	parentExists, err := tx.Property.Query().Where(property.IDEQ(fields.ListingKey)).Exist(ctx)
	if err != nil {
		return OutcomeUnknown, fmt.Errorf("check parent property: %w", err)
	}
	var parentFK *string
	if parentExists {
		k := fields.ListingKey
		parentFK = &k
	}

	// Dead-but-defensive (audit 2026-06-11): expanded property_rooms
	// payloads carry no per-child MlgCanView (0% of 148k rows).
	// Protection for hidden listings is the parent-visibility
	// resolver filter, not per-child tombstones. Cross-ref: sync/raw.go
	// splitExpandedChildren header doc.
	if !fields.MlgCanView {
		if current != nil && !current.MlgCanView {
			return OutcomeSkipTombstoned, nil
		}
		return OutcomeDelete, p.applyDelete(ctx, tx, fields, raw, current, currentVersion, parentFK, now)
	}

	if !entityExists {
		return OutcomeInsert, p.applyInsert(ctx, tx, fields, raw, parentFK, now)
	}

	diff := diffPropertyRoomFields(currentVersion, fields)
	if len(diff) == 0 {
		return OutcomeSkipNoDiff, nil
	}
	return OutcomeUpdate, p.applyUpdate(ctx, tx, fields, raw, current, currentVersion, parentFK, diff, now)
}

func (p *PropertyRoomProcessor) applyInsert(ctx context.Context, tx *ent.Tx, f *PropertyRoomFields, raw *ent.RawOutput, parentFK *string, now time.Time) error {
	ver, err := newPropertyRoomVersionCreate(tx, f, raw, propertyroomversion.ChangeTypeInsert, now, nil)
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

	c := tx.PropertyRoom.Create().
		SetID(f.RoomKey).
		SetCurrentVersionID(verID).
		SetNillableParentListingKey(parentFK)
	applyToPropertyRoomCreate(c, f)
	if _, err := c.Save(ctx); err != nil {
		return fmt.Errorf("create entity: %w", err)
	}
	return nil
}

func (p *PropertyRoomProcessor) applyUpdate(ctx context.Context, tx *ent.Tx, f *PropertyRoomFields, raw *ent.RawOutput, current *ent.PropertyRoom, currentVersion *ent.PropertyRoomVersion, parentFK *string, diff map[string]any, now time.Time) error {
	if currentVersion != nil {
		if _, err := tx.PropertyRoomVersion.UpdateOneID(currentVersion.ID).SetValidTo(now).Save(ctx); err != nil {
			return fmt.Errorf("close current version: %w", err)
		}
	}

	ver, err := newPropertyRoomVersionCreate(tx, f, raw, propertyroomversion.ChangeTypeUpdate, now, diff)
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

	u := tx.PropertyRoom.UpdateOneID(current.ID).SetCurrentVersionID(verID)
	if parentFK != nil && current.ParentListingKey == nil {
		u.SetParentListingKey(*parentFK)
	}
	applyToPropertyRoomUpdate(u, f)
	if _, err := u.Save(ctx); err != nil {
		return fmt.Errorf("update entity: %w", err)
	}
	return nil
}

func (p *PropertyRoomProcessor) applyDelete(ctx context.Context, tx *ent.Tx, f *PropertyRoomFields, raw *ent.RawOutput, current *ent.PropertyRoom, currentVersion *ent.PropertyRoomVersion, parentFK *string, now time.Time) error {
	if currentVersion != nil {
		if _, err := tx.PropertyRoomVersion.UpdateOneID(currentVersion.ID).SetValidTo(now).Save(ctx); err != nil {
			return fmt.Errorf("close current version: %w", err)
		}
	}

	ver, err := newPropertyRoomVersionCreate(tx, f, raw, propertyroomversion.ChangeTypeDelete, now, nil)
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
		c := tx.PropertyRoom.Create().
			SetID(f.RoomKey).
			SetCurrentVersionID(verID).
			SetNillableParentListingKey(parentFK)
		applyToPropertyRoomCreate(c, f)
		if _, err := c.Save(ctx); err != nil {
			return fmt.Errorf("create tombstoned entity: %w", err)
		}
		return nil
	}

	u := tx.PropertyRoom.UpdateOneID(current.ID).SetCurrentVersionID(verID).SetMlgCanView(false)
	if _, err := u.Save(ctx); err != nil {
		return fmt.Errorf("tombstone entity: %w", err)
	}
	return nil
}

func newPropertyRoomVersionCreate(tx *ent.Tx, f *PropertyRoomFields, raw *ent.RawOutput, ct propertyroomversion.ChangeType, now time.Time, diff map[string]any) (*ent.PropertyRoomVersionCreate, error) {
	if raw == nil {
		return nil, errors.New("raw_output required")
	}
	c := tx.PropertyRoomVersion.Create().
		SetRoomKey(f.RoomKey).
		SetSyncEventID(raw.SyncEventID).
		SetRawOutputID(raw.ID).
		SetValidFrom(now).
		SetChangeType(ct).
		SetProcessorVersion(version.Info())
	if diff != nil {
		c.SetChangedFields(diff)
	}
	applyToPropertyRoomVersionCreate(c, f)
	return c, nil
}

// --- diff ---

func diffPropertyRoomFields(currentVersion *ent.PropertyRoomVersion, next *PropertyRoomFields) map[string]any {
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
	if currentVersion.ListingKey != next.ListingKey {
		push("listing_key", currentVersion.ListingKey, next.ListingKey)
	}
	if !equalStringPtr(currentVersion.RoomType, next.RoomType) {
		push("room_type", currentVersion.RoomType, next.RoomType)
	}
	if !equalStringPtr(currentVersion.RoomLevel, next.RoomLevel) {
		push("room_level", currentVersion.RoomLevel, next.RoomLevel)
	}
	if !equalStringSlice([]string(currentVersion.RoomFeatures), []string(next.RoomFeatures)) {
		push("room_features", []string(currentVersion.RoomFeatures), []string(next.RoomFeatures))
	}
	if !reflect.DeepEqual(currentVersion.ExtendedFields, next.ExtendedFields) {
		push("extended_fields", currentVersion.ExtendedFields, next.ExtendedFields)
	}

	if len(diff) == 0 {
		return nil
	}
	return diff
}

// --- apply ---

func applyToPropertyRoomCreate(c *ent.PropertyRoomCreate, f *PropertyRoomFields) {
	c.SetSourceModifiedAt(f.SourceModifiedAt).
		SetMlgCanView(f.MlgCanView).
		SetNillableOriginatingSystemName(f.OriginatingSystemName).
		SetListingKey(f.ListingKey).
		SetNillableRoomType(f.RoomType).
		SetNillableRoomLevel(f.RoomLevel)
	if f.MlgCanUse != nil {
		c.SetMlgCanUse(f.MlgCanUse)
	}
	if f.RoomFeatures != nil {
		c.SetRoomFeatures(f.RoomFeatures)
	}
	if f.ExtendedFields != nil {
		c.SetExtendedFields(f.ExtendedFields)
	}
}

// applyToPropertyRoomUpdate uses clear-on-nil semantics — see apply.go.
func applyToPropertyRoomUpdate(c *ent.PropertyRoomUpdateOne, f *PropertyRoomFields) {
	c.SetSourceModifiedAt(f.SourceModifiedAt).
		SetMlgCanView(f.MlgCanView).
		SetListingKey(f.ListingKey)
	setOrClearStr(f.OriginatingSystemName, c.SetOriginatingSystemName, c.ClearOriginatingSystemName)
	setOrClearSlice(f.MlgCanUse, c.SetMlgCanUse, c.ClearMlgCanUse)
	setOrClearStr(f.RoomType, c.SetRoomType, c.ClearRoomType)
	setOrClearStr(f.RoomLevel, c.SetRoomLevel, c.ClearRoomLevel)
	setOrClearStringArray(f.RoomFeatures, c.SetRoomFeatures, c.ClearRoomFeatures)
	setOrClearMap(f.ExtendedFields, c.SetExtendedFields, c.ClearExtendedFields)
}

func applyToPropertyRoomVersionCreate(c *ent.PropertyRoomVersionCreate, f *PropertyRoomFields) {
	c.SetSourceModifiedAt(f.SourceModifiedAt).
		SetMlgCanView(f.MlgCanView).
		SetNillableOriginatingSystemName(f.OriginatingSystemName).
		SetListingKey(f.ListingKey).
		SetNillableRoomType(f.RoomType).
		SetNillableRoomLevel(f.RoomLevel)
	if f.MlgCanUse != nil {
		c.SetMlgCanUse(f.MlgCanUse)
	}
	if f.RoomFeatures != nil {
		c.SetRoomFeatures(f.RoomFeatures)
	}
	if f.ExtendedFields != nil {
		c.SetExtendedFields(f.ExtendedFields)
	}
}
