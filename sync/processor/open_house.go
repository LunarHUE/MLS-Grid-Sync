package processor

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/google/uuid"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/openhouse"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/openhouseversion"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/property"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/version"
)

// OpenHouseProcessor handles the OpenHouse resource. Same lifecycle as
// PropertyProcessor with one addition: the parking semantics on the FK to
// Property — set parent_listing_key only if the parent exists at process time;
// the re-link AfterPass step on PropertyProcessor fills it in later when
// the parent shows up.
type OpenHouseProcessor struct{}

func NewOpenHouseProcessor() *OpenHouseProcessor { return &OpenHouseProcessor{} }

func (*OpenHouseProcessor) Resource() rawoutput.Resource { return rawoutput.ResourceOpenHouse }

func (p *OpenHouseProcessor) Process(ctx context.Context, tx *ent.Tx, raw *ent.RawOutput) (Outcome, error) {
	fields, err := parseOpenHouse(raw.Payload)
	if err != nil {
		return OutcomeUnknown, fmt.Errorf("parse: %w", err)
	}

	entityExists, currentMlgCanView, currentParentListingKey, err := p.lookupEntity(ctx, tx, fields.OpenHouseKey)
	if err != nil {
		return OutcomeUnknown, err
	}

	var currentVersion *ent.OpenHouseVersion
	if entityExists {
		currentVersion, err = p.lookupCurrentVersion(ctx, tx, fields.OpenHouseKey)
		if err != nil {
			return OutcomeUnknown, err
		}
	}

	now := time.Now().UTC()
	plan := decideOpenHouse(fields, entityExists, currentMlgCanView, currentVersion, raw)
	if plan.action == actSkip {
		return plan.outcome, nil
	}

	// Resolve the parking FK only when we're going to write: parent_listing_key
	// is set only if the parent Property exists now, else left nil for
	// PropertyProcessor.AfterPass to re-link later.
	parentFK, err := p.resolveParentFK(ctx, tx, fields.ListingKey)
	if err != nil {
		return OutcomeUnknown, err
	}
	if err := p.applyPlan(ctx, tx, fields, raw, currentVersion, plan, parentFK, currentParentListingKey, now); err != nil {
		return OutcomeUnknown, err
	}
	return plan.outcome, nil
}

// lookupEntity reads existence + mlg_can_view (tombstone-skip) + the parking FK
// (the promotion guard). The diff target is the version row, so the wide entity
// row is not scanned here.
func (p *OpenHouseProcessor) lookupEntity(ctx context.Context, tx *ent.Tx, key string) (exists, mlgCanView bool, parentListingKey *string, err error) {
	var rows []struct {
		MlgCanView       bool    `json:"mlg_can_view"`
		ParentListingKey *string `json:"parent_listing_key"`
	}
	if err := tx.OpenHouse.Query().
		Where(openhouse.IDEQ(key)).
		Select(openhouse.FieldMlgCanView, openhouse.FieldParentListingKey).
		Scan(ctx, &rows); err != nil {
		return false, false, nil, fmt.Errorf("lookup open_house: %w", err)
	}
	if len(rows) == 0 {
		return false, false, nil, nil
	}
	return true, rows[0].MlgCanView, rows[0].ParentListingKey, nil
}

func (p *OpenHouseProcessor) lookupCurrentVersion(ctx context.Context, tx *ent.Tx, key string) (*ent.OpenHouseVersion, error) {
	v, err := tx.OpenHouseVersion.Query().
		Where(openhouseversion.OpenHouseKey(key), openhouseversion.ValidToIsNil()).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("lookup current version: %w", err)
	}
	return v, nil
}

func (p *OpenHouseProcessor) resolveParentFK(ctx context.Context, tx *ent.Tx, listingKey string) (*string, error) {
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

// openHousePlan mirrors propertyPlan for OpenHouse (shared action enum).
type openHousePlan struct {
	action         propertyAction
	outcome        Outcome
	changeType     openhouseversion.ChangeType
	diff           map[string]any
	closeVersionID *string
}

// decideOpenHouse is the pure decision shared by Process and ProcessChunk.
func decideOpenHouse(f *OpenHouseFields, entityExists, currentMlgCanView bool, currentVersion *ent.OpenHouseVersion, raw *ent.RawOutput) openHousePlan {
	if currentVersion != nil && !raw.SourceModifiedAt.After(currentVersion.SourceModifiedAt) {
		return openHousePlan{action: actSkip, outcome: OutcomeSkipStale}
	}
	if !f.MlgCanView {
		if entityExists && !currentMlgCanView {
			return openHousePlan{action: actSkip, outcome: OutcomeSkipTombstoned}
		}
		plan := openHousePlan{outcome: OutcomeDelete, changeType: openhouseversion.ChangeTypeDelete}
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
		return openHousePlan{action: actInsert, outcome: OutcomeInsert, changeType: openhouseversion.ChangeTypeInsert}
	}
	diff := diffOpenHouseFields(currentVersion, f)
	if len(diff) == 0 {
		return openHousePlan{action: actSkip, outcome: OutcomeSkipNoDiff}
	}
	plan := openHousePlan{action: actUpdate, outcome: OutcomeUpdate, changeType: openhouseversion.ChangeTypeUpdate, diff: diff}
	if currentVersion != nil {
		id := currentVersion.ID
		plan.closeVersionID = &id
	}
	return plan
}

func (p *OpenHouseProcessor) applyPlan(ctx context.Context, tx *ent.Tx, f *OpenHouseFields, raw *ent.RawOutput, currentVersion *ent.OpenHouseVersion, plan openHousePlan, parentFK, currentParentListingKey *string, now time.Time) error {
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

func (p *OpenHouseProcessor) applyInsert(ctx context.Context, tx *ent.Tx, f *OpenHouseFields, raw *ent.RawOutput, parentFK *string, now time.Time) error {
	ver, err := newOpenHouseVersionCreate(tx, f, raw, openhouseversion.ChangeTypeInsert, now, nil)
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

	c := tx.OpenHouse.Create().
		SetID(f.OpenHouseKey).
		SetCurrentVersionID(verID).
		SetNillableParentListingKey(parentFK)
	applyToOpenHouseCreate(c, f)
	if _, err := c.Save(ctx); err != nil {
		return fmt.Errorf("create entity: %w", err)
	}
	return nil
}

func (p *OpenHouseProcessor) applyUpdate(ctx context.Context, tx *ent.Tx, f *OpenHouseFields, raw *ent.RawOutput, currentVersion *ent.OpenHouseVersion, parentFK, currentParentListingKey *string, diff map[string]any, now time.Time) error {
	if currentVersion != nil {
		if _, err := tx.OpenHouseVersion.UpdateOneID(currentVersion.ID).
			SetValidTo(now).
			Save(ctx); err != nil {
			return fmt.Errorf("close current version: %w", err)
		}
	}

	ver, err := newOpenHouseVersionCreate(tx, f, raw, openhouseversion.ChangeTypeUpdate, now, diff)
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

	u := tx.OpenHouse.UpdateOneID(f.OpenHouseKey).
		SetCurrentVersionID(verID)
	// Promote parent_listing_key if it just became resolvable, but never
	// clear an already-linked FK on a routine update.
	if parentFK != nil && currentParentListingKey == nil {
		u.SetParentListingKey(*parentFK)
	}
	applyToOpenHouseUpdate(u, f)
	if _, err := u.Save(ctx); err != nil {
		return fmt.Errorf("update entity: %w", err)
	}
	return nil
}

func (p *OpenHouseProcessor) applyDelete(ctx context.Context, tx *ent.Tx, f *OpenHouseFields, raw *ent.RawOutput, currentVersion *ent.OpenHouseVersion, parentFK *string, entityExists bool, now time.Time) error {
	if currentVersion != nil {
		if _, err := tx.OpenHouseVersion.UpdateOneID(currentVersion.ID).
			SetValidTo(now).
			Save(ctx); err != nil {
			return fmt.Errorf("close current version: %w", err)
		}
	}

	ver, err := newOpenHouseVersionCreate(tx, f, raw, openhouseversion.ChangeTypeDelete, now, nil)
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
		c := tx.OpenHouse.Create().
			SetID(f.OpenHouseKey).
			SetCurrentVersionID(verID).
			SetNillableParentListingKey(parentFK)
		applyToOpenHouseCreate(c, f)
		if _, err := c.Save(ctx); err != nil {
			return fmt.Errorf("create tombstoned entity: %w", err)
		}
		return nil
	}

	u := tx.OpenHouse.UpdateOneID(f.OpenHouseKey).
		SetCurrentVersionID(verID).
		SetMlgCanView(false)
	if _, err := u.Save(ctx); err != nil {
		return fmt.Errorf("tombstone entity: %w", err)
	}
	return nil
}

func newOpenHouseVersionCreate(tx *ent.Tx, f *OpenHouseFields, raw *ent.RawOutput, ct openhouseversion.ChangeType, now time.Time, diff map[string]any) (*ent.OpenHouseVersionCreate, error) {
	if raw == nil {
		return nil, errors.New("raw_output required")
	}
	c := tx.OpenHouseVersion.Create().
		SetOpenHouseKey(f.OpenHouseKey).
		SetSyncEventID(raw.SyncEventID).
		SetRawOutputID(raw.ID).
		SetValidFrom(now).
		SetChangeType(ct).
		SetProcessorVersion(version.Info())
	if diff != nil {
		c.SetChangedFields(diff)
	}
	applyToOpenHouseVersionCreate(c, f)
	return c, nil
}

// --- diff ---

func diffOpenHouseFields(currentVersion *ent.OpenHouseVersion, next *OpenHouseFields) map[string]any {
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

	diffOpenHouseReflect(currentVersion, next, diff)

	if !reflect.DeepEqual(currentVersion.ExtendedFields, next.ExtendedFields) {
		push("extended_fields", currentVersion.ExtendedFields, next.ExtendedFields)
	}

	if len(diff) == 0 {
		return nil
	}
	return diff
}

func diffOpenHouseReflect(currentVersion *ent.OpenHouseVersion, next *OpenHouseFields, out map[string]any) {
	vNext := reflect.ValueOf(next).Elem()
	tNext := vNext.Type()
	vCur := reflect.ValueOf(currentVersion).Elem()
	tCur := vCur.Type()

	skip := map[string]bool{
		"OpenHouseKey":          true,
		"ListingKey":            true, // handled above
		"SourceModifiedAt":      true,
		"OriginatingSystemName": true,
		"MlgCanView":            true,
		"MlgCanUse":             true,
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

// --- apply: OpenHouseFields → ent builders ---

func applyToOpenHouseCreate(c *ent.OpenHouseCreate, f *OpenHouseFields) {
	c.SetSourceModifiedAt(f.SourceModifiedAt).
		SetMlgCanView(f.MlgCanView).
		SetNillableOriginatingSystemName(f.OriginatingSystemName).
		SetListingKey(f.ListingKey).
		SetNillableListingID(f.ListingID).
		SetNillableOpenHouseDate(f.OpenHouseDate).
		SetNillableOpenHouseStartTime(f.OpenHouseStartTime).
		SetNillableOpenHouseEndTime(f.OpenHouseEndTime).
		SetNillableOpenHouseStatus(f.OpenHouseStatus).
		SetNillableOpenHouseType(f.OpenHouseType)
	if f.MlgCanUse != nil {
		c.SetMlgCanUse(f.MlgCanUse)
	}
	if f.ExtendedFields != nil {
		c.SetExtendedFields(f.ExtendedFields)
	}
}

// applyToOpenHouseUpdate uses clear-on-nil semantics — see apply.go for why.
func applyToOpenHouseUpdate(c *ent.OpenHouseUpdateOne, f *OpenHouseFields) {
	c.SetSourceModifiedAt(f.SourceModifiedAt).
		SetMlgCanView(f.MlgCanView).
		SetListingKey(f.ListingKey)
	setOrClearStr(f.OriginatingSystemName, c.SetOriginatingSystemName, c.ClearOriginatingSystemName)
	setOrClearSlice(f.MlgCanUse, c.SetMlgCanUse, c.ClearMlgCanUse)
	setOrClearStr(f.ListingID, c.SetListingID, c.ClearListingID)
	setOrClearTime(f.OpenHouseDate, c.SetOpenHouseDate, c.ClearOpenHouseDate)
	setOrClearTime(f.OpenHouseStartTime, c.SetOpenHouseStartTime, c.ClearOpenHouseStartTime)
	setOrClearTime(f.OpenHouseEndTime, c.SetOpenHouseEndTime, c.ClearOpenHouseEndTime)
	setOrClearStr(f.OpenHouseStatus, c.SetOpenHouseStatus, c.ClearOpenHouseStatus)
	setOrClearStr(f.OpenHouseType, c.SetOpenHouseType, c.ClearOpenHouseType)
	setOrClearMap(f.ExtendedFields, c.SetExtendedFields, c.ClearExtendedFields)
}

func applyToOpenHouseVersionCreate(c *ent.OpenHouseVersionCreate, f *OpenHouseFields) {
	c.SetSourceModifiedAt(f.SourceModifiedAt).
		SetMlgCanView(f.MlgCanView).
		SetNillableOriginatingSystemName(f.OriginatingSystemName).
		SetListingKey(f.ListingKey).
		SetNillableListingID(f.ListingID).
		SetNillableOpenHouseDate(f.OpenHouseDate).
		SetNillableOpenHouseStartTime(f.OpenHouseStartTime).
		SetNillableOpenHouseEndTime(f.OpenHouseEndTime).
		SetNillableOpenHouseStatus(f.OpenHouseStatus).
		SetNillableOpenHouseType(f.OpenHouseType)
	if f.MlgCanUse != nil {
		c.SetMlgCanUse(f.MlgCanUse)
	}
	if f.ExtendedFields != nil {
		c.SetExtendedFields(f.ExtendedFields)
	}
}
