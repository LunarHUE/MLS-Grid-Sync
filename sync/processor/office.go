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
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/office"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/officeversion"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/rawoutput"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/version"
)

// OfficeProcessor handles the Office resource. See property.go for the
// canonical lifecycle documentation; Office mirrors that pattern.
type OfficeProcessor struct{}

func NewOfficeProcessor() *OfficeProcessor { return &OfficeProcessor{} }

func (*OfficeProcessor) Resource() rawoutput.Resource { return rawoutput.ResourceOffice }

func (p *OfficeProcessor) Process(ctx context.Context, tx *ent.Tx, raw *ent.RawOutput) (Outcome, error) {
	payload, err := json.Marshal(raw.Payload)
	if err != nil {
		return OutcomeUnknown, fmt.Errorf("marshal payload: %w", err)
	}
	fields, err := parseOffice(payload)
	if err != nil {
		return OutcomeUnknown, fmt.Errorf("parse: %w", err)
	}

	current, err := tx.Office.Query().Where(office.IDEQ(fields.OfficeKey)).Only(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return OutcomeUnknown, fmt.Errorf("lookup office: %w", err)
	}
	entityExists := err == nil

	var currentVersion *ent.OfficeVersion
	if entityExists {
		currentVersion, err = tx.OfficeVersion.Query().
			Where(
				officeversion.OfficeKey(fields.OfficeKey),
				officeversion.ValidToIsNil(),
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

	if !fields.MlgCanView {
		if current != nil && !current.MlgCanView {
			return OutcomeSkipTombstoned, nil
		}
		return OutcomeDelete, p.applyDelete(ctx, tx, fields, raw, current, currentVersion, now)
	}

	if !entityExists {
		return OutcomeInsert, p.applyInsert(ctx, tx, fields, raw, now)
	}

	diff := diffOfficeFields(currentVersion, fields)
	if len(diff) == 0 {
		return OutcomeSkipNoDiff, nil
	}
	return OutcomeUpdate, p.applyUpdate(ctx, tx, fields, raw, current, currentVersion, diff, now)
}

func (p *OfficeProcessor) applyInsert(ctx context.Context, tx *ent.Tx, f *OfficeFields, raw *ent.RawOutput, now time.Time) error {
	ver, err := newOfficeVersionCreate(tx, f, raw, officeversion.ChangeTypeInsert, now, nil)
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

	c := tx.Office.Create().SetID(f.OfficeKey).SetCurrentVersionID(verID)
	applyToOfficeCreate(c, f)
	if _, err := c.Save(ctx); err != nil {
		return fmt.Errorf("create entity: %w", err)
	}
	return nil
}

func (p *OfficeProcessor) applyUpdate(ctx context.Context, tx *ent.Tx, f *OfficeFields, raw *ent.RawOutput, current *ent.Office, currentVersion *ent.OfficeVersion, diff map[string]any, now time.Time) error {
	if currentVersion != nil {
		if _, err := tx.OfficeVersion.UpdateOneID(currentVersion.ID).SetValidTo(now).Save(ctx); err != nil {
			return fmt.Errorf("close current version: %w", err)
		}
	}

	ver, err := newOfficeVersionCreate(tx, f, raw, officeversion.ChangeTypeUpdate, now, diff)
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

	u := tx.Office.UpdateOneID(current.ID).SetCurrentVersionID(verID)
	applyToOfficeUpdate(u, f)
	if _, err := u.Save(ctx); err != nil {
		return fmt.Errorf("update entity: %w", err)
	}
	return nil
}

func (p *OfficeProcessor) applyDelete(ctx context.Context, tx *ent.Tx, f *OfficeFields, raw *ent.RawOutput, current *ent.Office, currentVersion *ent.OfficeVersion, now time.Time) error {
	if currentVersion != nil {
		if _, err := tx.OfficeVersion.UpdateOneID(currentVersion.ID).SetValidTo(now).Save(ctx); err != nil {
			return fmt.Errorf("close current version: %w", err)
		}
	}

	ver, err := newOfficeVersionCreate(tx, f, raw, officeversion.ChangeTypeDelete, now, nil)
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
		c := tx.Office.Create().SetID(f.OfficeKey).SetCurrentVersionID(verID)
		applyToOfficeCreate(c, f)
		if _, err := c.Save(ctx); err != nil {
			return fmt.Errorf("create tombstoned entity: %w", err)
		}
		return nil
	}

	u := tx.Office.UpdateOneID(current.ID).SetCurrentVersionID(verID).SetMlgCanView(false)
	if _, err := u.Save(ctx); err != nil {
		return fmt.Errorf("tombstone entity: %w", err)
	}
	return nil
}

func newOfficeVersionCreate(tx *ent.Tx, f *OfficeFields, raw *ent.RawOutput, ct officeversion.ChangeType, now time.Time, diff map[string]any) (*ent.OfficeVersionCreate, error) {
	if raw == nil {
		return nil, errors.New("raw_output required")
	}
	c := tx.OfficeVersion.Create().
		SetOfficeKey(f.OfficeKey).
		SetSyncEventID(raw.SyncEventID).
		SetRawOutputID(raw.ID).
		SetValidFrom(now).
		SetChangeType(ct).
		SetProcessorVersion(version.Info())
	if diff != nil {
		c.SetChangedFields(diff)
	}
	applyToOfficeVersionCreate(c, f)
	return c, nil
}

// --- diff ---

func diffOfficeFields(currentVersion *ent.OfficeVersion, next *OfficeFields) map[string]any {
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

	diffOfficeReflect(currentVersion, next, diff)

	if !reflect.DeepEqual(currentVersion.ExtendedFields, next.ExtendedFields) {
		push("extended_fields", currentVersion.ExtendedFields, next.ExtendedFields)
	}

	if len(diff) == 0 {
		return nil
	}
	return diff
}

func diffOfficeReflect(currentVersion *ent.OfficeVersion, next *OfficeFields, out map[string]any) {
	vNext := reflect.ValueOf(next).Elem()
	tNext := vNext.Type()
	vCur := reflect.ValueOf(currentVersion).Elem()
	tCur := vCur.Type()

	skip := map[string]bool{
		"OfficeKey":             true,
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

// --- apply ---

func applyToOfficeCreate(c *ent.OfficeCreate, f *OfficeFields) {
	c.SetSourceModifiedAt(f.SourceModifiedAt).
		SetMlgCanView(f.MlgCanView).
		SetNillableOriginatingSystemName(f.OriginatingSystemName)
	if f.MlgCanUse != nil {
		c.SetMlgCanUse(f.MlgCanUse)
	}
	c.SetNillableOfficeMlsID(f.OfficeMlsID).
		SetNillableOfficeName(f.OfficeName).
		SetNillableOfficeStatus(f.OfficeStatus).
		SetNillableOfficeType(f.OfficeType).
		SetNillableOfficePhone(f.OfficePhone).
		SetNillableOfficePhoneExt(f.OfficePhoneExt).
		SetNillableOfficeFax(f.OfficeFax).
		SetNillableOfficeAddress1(f.OfficeAddress1).
		SetNillableOfficeAddress2(f.OfficeAddress2).
		SetNillableOfficeCity(f.OfficeCity).
		SetNillableOfficeStateOrProvince(f.OfficeStateOrProvince).
		SetNillableOfficePostalCode(f.OfficePostalCode).
		SetNillableOfficePostalCodePlus4(f.OfficePostalCodePlus4).
		SetNillableOfficeCountyOrParish(f.OfficeCountyOrParish).
		SetNillableOfficeCorporateLicense(f.OfficeCorporateLicense).
		SetNillableOfficeNationalAssociationID(f.OfficeNationalAssociationID).
		SetNillableMainOfficeKey(f.MainOfficeKey).
		SetNillableMainOfficeMlsID(f.MainOfficeMlsID).
		SetNillableOfficeBrokerKey(f.OfficeBrokerKey).
		SetNillableOfficeBrokerMlsID(f.OfficeBrokerMlsID).
		SetNillableOfficeManagerKey(f.OfficeManagerKey).
		SetNillableIdxOfficeParticipationYn(f.IdxOfficeParticipationYn).
		SetNillablePhotosChangeTimestamp(f.PhotosChangeTimestamp)
	if f.ExtendedFields != nil {
		c.SetExtendedFields(f.ExtendedFields)
	}
}

// applyToOfficeUpdate uses clear-on-nil semantics — see apply.go for why.
func applyToOfficeUpdate(c *ent.OfficeUpdateOne, f *OfficeFields) {
	c.SetSourceModifiedAt(f.SourceModifiedAt).SetMlgCanView(f.MlgCanView)
	setOrClearStr(f.OriginatingSystemName, c.SetOriginatingSystemName, c.ClearOriginatingSystemName)
	setOrClearSlice(f.MlgCanUse, c.SetMlgCanUse, c.ClearMlgCanUse)
	setOrClearStr(f.OfficeMlsID, c.SetOfficeMlsID, c.ClearOfficeMlsID)
	setOrClearStr(f.OfficeName, c.SetOfficeName, c.ClearOfficeName)
	setOrClearStr(f.OfficeStatus, c.SetOfficeStatus, c.ClearOfficeStatus)
	setOrClearStr(f.OfficeType, c.SetOfficeType, c.ClearOfficeType)
	setOrClearStr(f.OfficePhone, c.SetOfficePhone, c.ClearOfficePhone)
	setOrClearStr(f.OfficePhoneExt, c.SetOfficePhoneExt, c.ClearOfficePhoneExt)
	setOrClearStr(f.OfficeFax, c.SetOfficeFax, c.ClearOfficeFax)
	setOrClearStr(f.OfficeAddress1, c.SetOfficeAddress1, c.ClearOfficeAddress1)
	setOrClearStr(f.OfficeAddress2, c.SetOfficeAddress2, c.ClearOfficeAddress2)
	setOrClearStr(f.OfficeCity, c.SetOfficeCity, c.ClearOfficeCity)
	setOrClearStr(f.OfficeStateOrProvince, c.SetOfficeStateOrProvince, c.ClearOfficeStateOrProvince)
	setOrClearStr(f.OfficePostalCode, c.SetOfficePostalCode, c.ClearOfficePostalCode)
	setOrClearStr(f.OfficePostalCodePlus4, c.SetOfficePostalCodePlus4, c.ClearOfficePostalCodePlus4)
	setOrClearStr(f.OfficeCountyOrParish, c.SetOfficeCountyOrParish, c.ClearOfficeCountyOrParish)
	setOrClearStr(f.OfficeCorporateLicense, c.SetOfficeCorporateLicense, c.ClearOfficeCorporateLicense)
	setOrClearStr(f.OfficeNationalAssociationID, c.SetOfficeNationalAssociationID, c.ClearOfficeNationalAssociationID)
	setOrClearStr(f.MainOfficeKey, c.SetMainOfficeKey, c.ClearMainOfficeKey)
	setOrClearStr(f.MainOfficeMlsID, c.SetMainOfficeMlsID, c.ClearMainOfficeMlsID)
	setOrClearStr(f.OfficeBrokerKey, c.SetOfficeBrokerKey, c.ClearOfficeBrokerKey)
	setOrClearStr(f.OfficeBrokerMlsID, c.SetOfficeBrokerMlsID, c.ClearOfficeBrokerMlsID)
	setOrClearStr(f.OfficeManagerKey, c.SetOfficeManagerKey, c.ClearOfficeManagerKey)
	setOrClearBool(f.IdxOfficeParticipationYn, c.SetIdxOfficeParticipationYn, c.ClearIdxOfficeParticipationYn)
	setOrClearTime(f.PhotosChangeTimestamp, c.SetPhotosChangeTimestamp, c.ClearPhotosChangeTimestamp)
	setOrClearMap(f.ExtendedFields, c.SetExtendedFields, c.ClearExtendedFields)
}

func applyToOfficeVersionCreate(c *ent.OfficeVersionCreate, f *OfficeFields) {
	c.SetSourceModifiedAt(f.SourceModifiedAt).
		SetMlgCanView(f.MlgCanView).
		SetNillableOriginatingSystemName(f.OriginatingSystemName)
	if f.MlgCanUse != nil {
		c.SetMlgCanUse(f.MlgCanUse)
	}
	c.SetNillableOfficeMlsID(f.OfficeMlsID).
		SetNillableOfficeName(f.OfficeName).
		SetNillableOfficeStatus(f.OfficeStatus).
		SetNillableOfficeType(f.OfficeType).
		SetNillableOfficePhone(f.OfficePhone).
		SetNillableOfficePhoneExt(f.OfficePhoneExt).
		SetNillableOfficeFax(f.OfficeFax).
		SetNillableOfficeAddress1(f.OfficeAddress1).
		SetNillableOfficeAddress2(f.OfficeAddress2).
		SetNillableOfficeCity(f.OfficeCity).
		SetNillableOfficeStateOrProvince(f.OfficeStateOrProvince).
		SetNillableOfficePostalCode(f.OfficePostalCode).
		SetNillableOfficePostalCodePlus4(f.OfficePostalCodePlus4).
		SetNillableOfficeCountyOrParish(f.OfficeCountyOrParish).
		SetNillableOfficeCorporateLicense(f.OfficeCorporateLicense).
		SetNillableOfficeNationalAssociationID(f.OfficeNationalAssociationID).
		SetNillableMainOfficeKey(f.MainOfficeKey).
		SetNillableMainOfficeMlsID(f.MainOfficeMlsID).
		SetNillableOfficeBrokerKey(f.OfficeBrokerKey).
		SetNillableOfficeBrokerMlsID(f.OfficeBrokerMlsID).
		SetNillableOfficeManagerKey(f.OfficeManagerKey).
		SetNillableIdxOfficeParticipationYn(f.IdxOfficeParticipationYn).
		SetNillablePhotosChangeTimestamp(f.PhotosChangeTimestamp)
	if f.ExtendedFields != nil {
		c.SetExtendedFields(f.ExtendedFields)
	}
}
