package processor

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/google/uuid"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/property"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/propertyroom"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/propertyroomversion"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/version"
)

// PropertyRoomProcessor — same parking semantics as OpenHouseProcessor.
type PropertyRoomProcessor struct{}

func NewPropertyRoomProcessor() *PropertyRoomProcessor { return &PropertyRoomProcessor{} }

func (*PropertyRoomProcessor) Resource() rawoutput.Resource {
	return rawoutput.ResourcePropertyRooms
}

func (p *PropertyRoomProcessor) Process(ctx context.Context, tx *ent.Tx, raw *ent.RawOutput) (Outcome, error) {
	fields, err := parsePropertyRoom(raw.Payload)
	if err != nil {
		return OutcomeUnknown, fmt.Errorf("parse: %w", err)
	}
	// Timestamp seam (decision 1a): see media.go — the splitter owns timestamp
	// extraction; the processor sources it from raw.
	fields.SourceModifiedAt = raw.SourceModifiedAt

	entityExists, currentMlgCanView, currentParentListingKey, err := p.lookupEntity(ctx, tx, fields.RoomKey)
	if err != nil {
		return OutcomeUnknown, err
	}

	var currentVersion *ent.PropertyRoomVersion
	if entityExists {
		currentVersion, err = p.lookupCurrentVersion(ctx, tx, fields.RoomKey)
		if err != nil {
			return OutcomeUnknown, err
		}
	}

	now := time.Now().UTC()
	plan := decidePropertyRoom(fields, entityExists, currentMlgCanView, currentVersion, raw)
	if plan.action == actSkip {
		return plan.outcome, nil
	}
	parentFK, err := p.resolveParentFK(ctx, tx, fields.ListingKey)
	if err != nil {
		return OutcomeUnknown, err
	}
	if err := p.applyPlan(ctx, tx, fields, raw, currentVersion, plan, parentFK, currentParentListingKey, now); err != nil {
		return OutcomeUnknown, err
	}
	return plan.outcome, nil
}

func (p *PropertyRoomProcessor) lookupEntity(ctx context.Context, tx *ent.Tx, key string) (exists, mlgCanView bool, parentListingKey *string, err error) {
	var rows []struct {
		MlgCanView       bool    `json:"mlg_can_view"`
		ParentListingKey *string `json:"parent_listing_key"`
	}
	if err := tx.PropertyRoom.Query().
		Where(propertyroom.IDEQ(key)).
		Select(propertyroom.FieldMlgCanView, propertyroom.FieldParentListingKey).
		Scan(ctx, &rows); err != nil {
		return false, false, nil, fmt.Errorf("lookup property_room: %w", err)
	}
	if len(rows) == 0 {
		return false, false, nil, nil
	}
	return true, rows[0].MlgCanView, rows[0].ParentListingKey, nil
}

func (p *PropertyRoomProcessor) lookupCurrentVersion(ctx context.Context, tx *ent.Tx, key string) (*ent.PropertyRoomVersion, error) {
	v, err := tx.PropertyRoomVersion.Query().
		Where(propertyroomversion.RoomKey(key), propertyroomversion.ValidToIsNil()).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("lookup current version: %w", err)
	}
	return v, nil
}

func (p *PropertyRoomProcessor) resolveParentFK(ctx context.Context, tx *ent.Tx, listingKey string) (*string, error) {
	exists, err := tx.Property.Query().Where(property.IDEQ(listingKey)).Exist(ctx)
	if err != nil {
		return nil, fmt.Errorf("check parent property: %w", err)
	}
	if exists {
		k := listingKey
		return &k, nil
	}
	return nil, nil
}

type propertyRoomPlan struct {
	action         propertyAction
	outcome        Outcome
	changeType     propertyroomversion.ChangeType
	diff           map[string]any
	closeVersionID *string
}

// decidePropertyRoom is the pure decision shared by Process and ProcessChunk.
// The delete branch is dead-but-defensive (expanded rooms carry no MlgCanView).
func decidePropertyRoom(f *PropertyRoomFields, entityExists, currentMlgCanView bool, currentVersion *ent.PropertyRoomVersion, raw *ent.RawOutput) propertyRoomPlan {
	if currentVersion != nil && !raw.SourceModifiedAt.After(currentVersion.SourceModifiedAt) {
		return propertyRoomPlan{action: actSkip, outcome: OutcomeSkipStale}
	}
	if !f.MlgCanView {
		if entityExists && !currentMlgCanView {
			return propertyRoomPlan{action: actSkip, outcome: OutcomeSkipTombstoned}
		}
		plan := propertyRoomPlan{outcome: OutcomeDelete, changeType: propertyroomversion.ChangeTypeDelete}
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
		return propertyRoomPlan{action: actInsert, outcome: OutcomeInsert, changeType: propertyroomversion.ChangeTypeInsert}
	}
	diff := diffPropertyRoomFields(currentVersion, f)
	if len(diff) == 0 {
		return propertyRoomPlan{action: actSkip, outcome: OutcomeSkipNoDiff}
	}
	plan := propertyRoomPlan{action: actUpdate, outcome: OutcomeUpdate, changeType: propertyroomversion.ChangeTypeUpdate, diff: diff}
	if currentVersion != nil {
		id := currentVersion.ID
		plan.closeVersionID = &id
	}
	return plan
}

func (p *PropertyRoomProcessor) applyPlan(ctx context.Context, tx *ent.Tx, f *PropertyRoomFields, raw *ent.RawOutput, currentVersion *ent.PropertyRoomVersion, plan propertyRoomPlan, parentFK, currentParentListingKey *string, now time.Time) error {
	switch plan.action {
	case actSkip:
		return nil
	case actInsert:
		return p.applyInsert(ctx, tx, f, raw, parentFK, now)
	case actUpdate:
		return p.applyUpdate(ctx, tx, f, raw, currentVersion, parentFK, currentParentListingKey, plan.diff, now)
	case actDeleteExisting:
		return p.applyDelete(ctx, tx, f, raw, currentVersion, parentFK, true, now)
	case actDeleteFirstSighting:
		return p.applyDelete(ctx, tx, f, raw, currentVersion, parentFK, false, now)
	}
	return nil
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

func (p *PropertyRoomProcessor) applyUpdate(ctx context.Context, tx *ent.Tx, f *PropertyRoomFields, raw *ent.RawOutput, currentVersion *ent.PropertyRoomVersion, parentFK, currentParentListingKey *string, diff map[string]any, now time.Time) error {
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

	u := tx.PropertyRoom.UpdateOneID(f.RoomKey).SetCurrentVersionID(verID)
	if parentFK != nil && currentParentListingKey == nil {
		u.SetParentListingKey(*parentFK)
	}
	applyToPropertyRoomUpdate(u, f)
	if _, err := u.Save(ctx); err != nil {
		return fmt.Errorf("update entity: %w", err)
	}
	return nil
}

func (p *PropertyRoomProcessor) applyDelete(ctx context.Context, tx *ent.Tx, f *PropertyRoomFields, raw *ent.RawOutput, currentVersion *ent.PropertyRoomVersion, parentFK *string, entityExists bool, now time.Time) error {
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

	if !entityExists {
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

	u := tx.PropertyRoom.UpdateOneID(f.RoomKey).SetCurrentVersionID(verID).SetMlgCanView(false)
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
