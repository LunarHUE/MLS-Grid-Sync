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

	current, err := tx.PropertyUnitType.Query().Where(propertyunittype.IDEQ(fields.UnitTypeKey)).Only(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return OutcomeUnknown, fmt.Errorf("lookup property_unit_type: %w", err)
	}
	entityExists := err == nil

	var currentVersion *ent.PropertyUnitTypeVersion
	if entityExists {
		currentVersion, err = tx.PropertyUnitTypeVersion.Query().
			Where(
				propertyunittypeversion.UnitTypeKey(fields.UnitTypeKey),
				propertyunittypeversion.ValidToIsNil(),
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

	// Dead-but-defensive (audit 2026-06-11): expanded
	// property_unit_types payloads carry no per-child MlgCanView
	// (0% of 1.5k rows). Protection for hidden listings is the
	// parent-visibility resolver filter, not per-child tombstones.
	// Cross-ref: sync/raw.go splitExpandedChildren header doc.
	if !fields.MlgCanView {
		if current != nil && !current.MlgCanView {
			return OutcomeSkipTombstoned, nil
		}
		return OutcomeDelete, p.applyDelete(ctx, tx, fields, raw, current, currentVersion, parentFK, now)
	}

	if !entityExists {
		return OutcomeInsert, p.applyInsert(ctx, tx, fields, raw, parentFK, now)
	}

	diff := diffPropertyUnitTypeFields(currentVersion, fields)
	if len(diff) == 0 {
		return OutcomeSkipNoDiff, nil
	}
	return OutcomeUpdate, p.applyUpdate(ctx, tx, fields, raw, current, currentVersion, parentFK, diff, now)
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

func (p *PropertyUnitTypeProcessor) applyUpdate(ctx context.Context, tx *ent.Tx, f *PropertyUnitTypeFields, raw *ent.RawOutput, current *ent.PropertyUnitType, currentVersion *ent.PropertyUnitTypeVersion, parentFK *string, diff map[string]any, now time.Time) error {
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

	u := tx.PropertyUnitType.UpdateOneID(current.ID).SetCurrentVersionID(verID)
	if parentFK != nil && current.ParentListingKey == nil {
		u.SetParentListingKey(*parentFK)
	}
	applyToPropertyUnitTypeUpdate(u, f)
	if _, err := u.Save(ctx); err != nil {
		return fmt.Errorf("update entity: %w", err)
	}
	return nil
}

func (p *PropertyUnitTypeProcessor) applyDelete(ctx context.Context, tx *ent.Tx, f *PropertyUnitTypeFields, raw *ent.RawOutput, current *ent.PropertyUnitType, currentVersion *ent.PropertyUnitTypeVersion, parentFK *string, now time.Time) error {
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

	if current == nil {
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

	u := tx.PropertyUnitType.UpdateOneID(current.ID).SetCurrentVersionID(verID).SetMlgCanView(false)
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
