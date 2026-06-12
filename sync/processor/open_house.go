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
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/openhouse"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/openhouseversion"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/property"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/rawoutput"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/version"
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
	payload, err := json.Marshal(raw.Payload)
	if err != nil {
		return OutcomeUnknown, fmt.Errorf("marshal payload: %w", err)
	}
	fields, err := parseOpenHouse(payload)
	if err != nil {
		return OutcomeUnknown, fmt.Errorf("parse: %w", err)
	}

	current, err := tx.OpenHouse.Query().
		Where(openhouse.IDEQ(fields.OpenHouseKey)).
		Only(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return OutcomeUnknown, fmt.Errorf("lookup open_house: %w", err)
	}
	entityExists := err == nil

	var currentVersion *ent.OpenHouseVersion
	if entityExists {
		currentVersion, err = tx.OpenHouseVersion.Query().
			Where(
				openhouseversion.OpenHouseKey(fields.OpenHouseKey),
				openhouseversion.ValidToIsNil(),
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

	// Resolve the parking FK: set parent_listing_key only if the parent
	// Property exists at this moment. Otherwise leave nil and rely on the
	// re-link step in PropertyProcessor.AfterPass.
	parentExists, err := tx.Property.Query().Where(property.IDEQ(fields.ListingKey)).Exist(ctx)
	if err != nil {
		return OutcomeUnknown, fmt.Errorf("check parent property: %w", err)
	}
	var parentFK *string
	if parentExists {
		k := fields.ListingKey
		parentFK = &k
	}

	if !fields.MlgCanView {
		// Already-tombstoned skip — see property.go for rationale.
		if current != nil && !current.MlgCanView {
			return OutcomeSkipTombstoned, nil
		}
		return OutcomeDelete, p.applyDelete(ctx, tx, fields, raw, current, currentVersion, parentFK, now)
	}

	if !entityExists {
		return OutcomeInsert, p.applyInsert(ctx, tx, fields, raw, parentFK, now)
	}

	diff := diffOpenHouseFields(currentVersion, fields)
	if len(diff) == 0 {
		return OutcomeSkipNoDiff, nil
	}
	return OutcomeUpdate, p.applyUpdate(ctx, tx, fields, raw, current, currentVersion, parentFK, diff, now)
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

func (p *OpenHouseProcessor) applyUpdate(ctx context.Context, tx *ent.Tx, f *OpenHouseFields, raw *ent.RawOutput, current *ent.OpenHouse, currentVersion *ent.OpenHouseVersion, parentFK *string, diff map[string]any, now time.Time) error {
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

	u := tx.OpenHouse.UpdateOneID(current.ID).
		SetCurrentVersionID(verID)
	// Promote parent_listing_key if it just became resolvable, but never
	// clear an already-linked FK on a routine update.
	if parentFK != nil && current.ParentListingKey == nil {
		u.SetParentListingKey(*parentFK)
	}
	applyToOpenHouseUpdate(u, f)
	if _, err := u.Save(ctx); err != nil {
		return fmt.Errorf("update entity: %w", err)
	}
	return nil
}

func (p *OpenHouseProcessor) applyDelete(ctx context.Context, tx *ent.Tx, f *OpenHouseFields, raw *ent.RawOutput, current *ent.OpenHouse, currentVersion *ent.OpenHouseVersion, parentFK *string, now time.Time) error {
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

	if current == nil {
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

	u := tx.OpenHouse.UpdateOneID(current.ID).
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
