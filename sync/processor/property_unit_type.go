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
	"github.com/LunarHUE/MLS-Grid-Sync/ent/propertyunittype"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/propertyunittypeversion"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/version"
)

// PropertyUnitTypeProcessor — same parking semantics as OpenHouseProcessor.
type PropertyUnitTypeProcessor struct{}

func NewPropertyUnitTypeProcessor() *PropertyUnitTypeProcessor { return &PropertyUnitTypeProcessor{} }

func (*PropertyUnitTypeProcessor) Resource() rawoutput.Resource {
	return rawoutput.ResourcePropertyUnitTypes
}

func (p *PropertyUnitTypeProcessor) Process(ctx context.Context, tx *ent.Tx, raw *ent.RawOutput) (Outcome, error) {
	fields, err := parsePropertyUnitType(raw.Payload)
	if err != nil {
		return OutcomeUnknown, fmt.Errorf("parse: %w", err)
	}
	// Timestamp seam (decision 1a): see media.go for the rationale.
	// Splitter owns timestamp extraction; processor sources from raw
	// to align with the stale-skip comparison value (below).
	fields.SourceModifiedAt = raw.SourceModifiedAt

	entityExists, currentMlgCanView, currentParentListingKey, err := p.lookupEntity(ctx, tx, fields.UnitTypeKey)
	if err != nil {
		return OutcomeUnknown, err
	}

	var currentVersion *ent.PropertyUnitTypeVersion
	if entityExists {
		currentVersion, err = p.lookupCurrentVersion(ctx, tx, fields.UnitTypeKey)
		if err != nil {
			return OutcomeUnknown, err
		}
	}

	now := time.Now().UTC()
	plan := decidePropertyUnitType(fields, entityExists, currentMlgCanView, currentVersion, raw)
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

func (p *PropertyUnitTypeProcessor) lookupEntity(ctx context.Context, tx *ent.Tx, key string) (exists, mlgCanView bool, parentListingKey *string, err error) {
	var rows []struct {
		MlgCanView       bool    `json:"mlg_can_view"`
		ParentListingKey *string `json:"parent_listing_key"`
	}
	if err := tx.PropertyUnitType.Query().
		Where(propertyunittype.IDEQ(key)).
		Select(propertyunittype.FieldMlgCanView, propertyunittype.FieldParentListingKey).
		Scan(ctx, &rows); err != nil {
		return false, false, nil, fmt.Errorf("lookup property_unit_type: %w", err)
	}
	if len(rows) == 0 {
		return false, false, nil, nil
	}
	return true, rows[0].MlgCanView, rows[0].ParentListingKey, nil
}

func (p *PropertyUnitTypeProcessor) lookupCurrentVersion(ctx context.Context, tx *ent.Tx, key string) (*ent.PropertyUnitTypeVersion, error) {
	v, err := tx.PropertyUnitTypeVersion.Query().
		Where(propertyunittypeversion.UnitTypeKey(key), propertyunittypeversion.ValidToIsNil()).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("lookup current version: %w", err)
	}
	return v, nil
}

func (p *PropertyUnitTypeProcessor) resolveParentFK(ctx context.Context, tx *ent.Tx, listingKey string) (*string, error) {
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

type propertyUnitTypePlan struct {
	action         propertyAction
	outcome        Outcome
	changeType     propertyunittypeversion.ChangeType
	diff           map[string]any
	closeVersionID *string
}

// decidePropertyUnitType is the pure decision shared by Process and ProcessChunk.
func decidePropertyUnitType(f *PropertyUnitTypeFields, entityExists, currentMlgCanView bool, currentVersion *ent.PropertyUnitTypeVersion, raw *ent.RawOutput) propertyUnitTypePlan {
	if currentVersion != nil && !raw.SourceModifiedAt.After(currentVersion.SourceModifiedAt) {
		return propertyUnitTypePlan{action: actSkip, outcome: OutcomeSkipStale}
	}
	if !f.MlgCanView {
		if entityExists && !currentMlgCanView {
			return propertyUnitTypePlan{action: actSkip, outcome: OutcomeSkipTombstoned}
		}
		plan := propertyUnitTypePlan{outcome: OutcomeDelete, changeType: propertyunittypeversion.ChangeTypeDelete}
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
		return propertyUnitTypePlan{action: actInsert, outcome: OutcomeInsert, changeType: propertyunittypeversion.ChangeTypeInsert}
	}
	diff := diffPropertyUnitTypeFields(currentVersion, f)
	if len(diff) == 0 {
		return propertyUnitTypePlan{action: actSkip, outcome: OutcomeSkipNoDiff}
	}
	plan := propertyUnitTypePlan{action: actUpdate, outcome: OutcomeUpdate, changeType: propertyunittypeversion.ChangeTypeUpdate, diff: diff}
	if currentVersion != nil {
		id := currentVersion.ID
		plan.closeVersionID = &id
	}
	return plan
}

func (p *PropertyUnitTypeProcessor) applyPlan(ctx context.Context, tx *ent.Tx, f *PropertyUnitTypeFields, raw *ent.RawOutput, currentVersion *ent.PropertyUnitTypeVersion, plan propertyUnitTypePlan, parentFK, currentParentListingKey *string, now time.Time) error {
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

func (p *PropertyUnitTypeProcessor) applyInsert(ctx context.Context, tx *ent.Tx, f *PropertyUnitTypeFields, raw *ent.RawOutput, parentFK *string, now time.Time) error {
	ver, err := newPropertyUnitTypeVersionCreate(tx, f, raw, propertyunittypeversion.ChangeTypeInsert, now, nil)
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

	c := tx.PropertyUnitType.Create().
		SetID(f.UnitTypeKey).
		SetCurrentVersionID(verID).
		SetNillableParentListingKey(parentFK)
	applyToPropertyUnitTypeCreate(c, f)
	if _, err := c.Save(ctx); err != nil {
		return fmt.Errorf("create entity: %w", err)
	}
	return nil
}

func (p *PropertyUnitTypeProcessor) applyUpdate(ctx context.Context, tx *ent.Tx, f *PropertyUnitTypeFields, raw *ent.RawOutput, currentVersion *ent.PropertyUnitTypeVersion, parentFK, currentParentListingKey *string, diff map[string]any, now time.Time) error {
	if currentVersion != nil {
		if _, err := tx.PropertyUnitTypeVersion.UpdateOneID(currentVersion.ID).SetValidTo(now).Save(ctx); err != nil {
			return fmt.Errorf("close current version: %w", err)
		}
	}

	ver, err := newPropertyUnitTypeVersionCreate(tx, f, raw, propertyunittypeversion.ChangeTypeUpdate, now, diff)
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

	u := tx.PropertyUnitType.UpdateOneID(f.UnitTypeKey).SetCurrentVersionID(verID)
	if parentFK != nil && currentParentListingKey == nil {
		u.SetParentListingKey(*parentFK)
	}
	applyToPropertyUnitTypeUpdate(u, f)
	if _, err := u.Save(ctx); err != nil {
		return fmt.Errorf("update entity: %w", err)
	}
	return nil
}

func (p *PropertyUnitTypeProcessor) applyDelete(ctx context.Context, tx *ent.Tx, f *PropertyUnitTypeFields, raw *ent.RawOutput, currentVersion *ent.PropertyUnitTypeVersion, parentFK *string, entityExists bool, now time.Time) error {
	if currentVersion != nil {
		if _, err := tx.PropertyUnitTypeVersion.UpdateOneID(currentVersion.ID).SetValidTo(now).Save(ctx); err != nil {
			return fmt.Errorf("close current version: %w", err)
		}
	}

	ver, err := newPropertyUnitTypeVersionCreate(tx, f, raw, propertyunittypeversion.ChangeTypeDelete, now, nil)
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
		c := tx.PropertyUnitType.Create().
			SetID(f.UnitTypeKey).
			SetCurrentVersionID(verID).
			SetNillableParentListingKey(parentFK)
		applyToPropertyUnitTypeCreate(c, f)
		if _, err := c.Save(ctx); err != nil {
			return fmt.Errorf("create tombstoned entity: %w", err)
		}
		return nil
	}

	u := tx.PropertyUnitType.UpdateOneID(f.UnitTypeKey).SetCurrentVersionID(verID).SetMlgCanView(false)
	if _, err := u.Save(ctx); err != nil {
		return fmt.Errorf("tombstone entity: %w", err)
	}
	return nil
}

func newPropertyUnitTypeVersionCreate(tx *ent.Tx, f *PropertyUnitTypeFields, raw *ent.RawOutput, ct propertyunittypeversion.ChangeType, now time.Time, diff map[string]any) (*ent.PropertyUnitTypeVersionCreate, error) {
	if raw == nil {
		return nil, errors.New("raw_output required")
	}
	c := tx.PropertyUnitTypeVersion.Create().
		SetUnitTypeKey(f.UnitTypeKey).
		SetSyncEventID(raw.SyncEventID).
		SetRawOutputID(raw.ID).
		SetValidFrom(now).
		SetChangeType(ct).
		SetProcessorVersion(version.Info())
	if diff != nil {
		c.SetChangedFields(diff)
	}
	applyToPropertyUnitTypeVersionCreate(c, f)
	return c, nil
}

// --- diff ---

func diffPropertyUnitTypeFields(currentVersion *ent.PropertyUnitTypeVersion, next *PropertyUnitTypeFields) map[string]any {
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
	if !equalInt16Ptr(currentVersion.UnitTypeBedsTotal, next.UnitTypeBedsTotal) {
		push("unit_type_beds_total", currentVersion.UnitTypeBedsTotal, next.UnitTypeBedsTotal)
	}
	if !equalStringPtr(currentVersion.UnitTypeFurnished, next.UnitTypeFurnished) {
		push("unit_type_furnished", currentVersion.UnitTypeFurnished, next.UnitTypeFurnished)
	}
	if !reflect.DeepEqual(currentVersion.ExtendedFields, next.ExtendedFields) {
		push("extended_fields", currentVersion.ExtendedFields, next.ExtendedFields)
	}

	if len(diff) == 0 {
		return nil
	}
	return diff
}

func equalInt16Ptr(a, b *int16) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// --- apply ---

func applyToPropertyUnitTypeCreate(c *ent.PropertyUnitTypeCreate, f *PropertyUnitTypeFields) {
	c.SetSourceModifiedAt(f.SourceModifiedAt).
		SetMlgCanView(f.MlgCanView).
		SetNillableOriginatingSystemName(f.OriginatingSystemName).
		SetListingKey(f.ListingKey).
		SetNillableUnitTypeBedsTotal(f.UnitTypeBedsTotal).
		SetNillableUnitTypeFurnished(f.UnitTypeFurnished)
	if f.MlgCanUse != nil {
		c.SetMlgCanUse(f.MlgCanUse)
	}
	if f.ExtendedFields != nil {
		c.SetExtendedFields(f.ExtendedFields)
	}
}

// applyToPropertyUnitTypeUpdate uses clear-on-nil semantics — see apply.go.
func applyToPropertyUnitTypeUpdate(c *ent.PropertyUnitTypeUpdateOne, f *PropertyUnitTypeFields) {
	c.SetSourceModifiedAt(f.SourceModifiedAt).
		SetMlgCanView(f.MlgCanView).
		SetListingKey(f.ListingKey)
	setOrClearStr(f.OriginatingSystemName, c.SetOriginatingSystemName, c.ClearOriginatingSystemName)
	setOrClearSlice(f.MlgCanUse, c.SetMlgCanUse, c.ClearMlgCanUse)
	setOrClearInt16(f.UnitTypeBedsTotal, c.SetUnitTypeBedsTotal, c.ClearUnitTypeBedsTotal)
	setOrClearStr(f.UnitTypeFurnished, c.SetUnitTypeFurnished, c.ClearUnitTypeFurnished)
	setOrClearMap(f.ExtendedFields, c.SetExtendedFields, c.ClearExtendedFields)
}

func applyToPropertyUnitTypeVersionCreate(c *ent.PropertyUnitTypeVersionCreate, f *PropertyUnitTypeFields) {
	c.SetSourceModifiedAt(f.SourceModifiedAt).
		SetMlgCanView(f.MlgCanView).
		SetNillableOriginatingSystemName(f.OriginatingSystemName).
		SetListingKey(f.ListingKey).
		SetNillableUnitTypeBedsTotal(f.UnitTypeBedsTotal).
		SetNillableUnitTypeFurnished(f.UnitTypeFurnished)
	if f.MlgCanUse != nil {
		c.SetMlgCanUse(f.MlgCanUse)
	}
	if f.ExtendedFields != nil {
		c.SetExtendedFields(f.ExtendedFields)
	}
}
