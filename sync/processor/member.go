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
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/member"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/memberversion"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/rawoutput"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/version"
)

// MemberProcessor handles the Member resource: parse, diff, version+entity write.
// Same lifecycle as PropertyProcessor — see property.go for the canonical
// documentation.
type MemberProcessor struct{}

func NewMemberProcessor() *MemberProcessor { return &MemberProcessor{} }

func (*MemberProcessor) Resource() rawoutput.Resource { return rawoutput.ResourceMember }

func (p *MemberProcessor) Process(ctx context.Context, tx *ent.Tx, raw *ent.RawOutput) (Outcome, error) {
	payload, err := json.Marshal(raw.Payload)
	if err != nil {
		return OutcomeUnknown, fmt.Errorf("marshal payload: %w", err)
	}
	fields, err := parseMember(payload)
	if err != nil {
		return OutcomeUnknown, fmt.Errorf("parse: %w", err)
	}

	current, err := tx.Member.Query().
		Where(member.IDEQ(fields.MemberKey)).
		Only(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return OutcomeUnknown, fmt.Errorf("lookup member: %w", err)
	}
	entityExists := err == nil

	var currentVersion *ent.MemberVersion
	if entityExists {
		currentVersion, err = tx.MemberVersion.Query().
			Where(
				memberversion.MemberKey(fields.MemberKey),
				memberversion.ValidToIsNil(),
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
		// Already-tombstoned skip: a newer MlgCanView=false record for an
		// entity already in tombstoned state is a no-op. Without this, we'd
		// close the open delete version and write a second delete version
		// every time the upstream re-asserts the deletion.
		if current != nil && !current.MlgCanView {
			return OutcomeSkipTombstoned, nil
		}
		return OutcomeDelete, p.applyDelete(ctx, tx, fields, raw, current, currentVersion, now)
	}

	if !entityExists {
		return OutcomeInsert, p.applyInsert(ctx, tx, fields, raw, now)
	}

	diff := diffMemberFields(currentVersion, fields)
	if len(diff) == 0 {
		return OutcomeSkipNoDiff, nil
	}
	return OutcomeUpdate, p.applyUpdate(ctx, tx, fields, raw, current, currentVersion, diff, now)
}

func (p *MemberProcessor) applyInsert(ctx context.Context, tx *ent.Tx, f *MemberFields, raw *ent.RawOutput, now time.Time) error {
	ver, err := newMemberVersionCreate(tx, f, raw, memberversion.ChangeTypeInsert, now, nil)
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

	c := tx.Member.Create().
		SetID(f.MemberKey).
		SetCurrentVersionID(verID)
	applyToMemberCreate(c, f)
	if _, err := c.Save(ctx); err != nil {
		return fmt.Errorf("create entity: %w", err)
	}
	return nil
}

func (p *MemberProcessor) applyUpdate(ctx context.Context, tx *ent.Tx, f *MemberFields, raw *ent.RawOutput, current *ent.Member, currentVersion *ent.MemberVersion, diff map[string]any, now time.Time) error {
	if currentVersion != nil {
		if _, err := tx.MemberVersion.UpdateOneID(currentVersion.ID).
			SetValidTo(now).
			Save(ctx); err != nil {
			return fmt.Errorf("close current version: %w", err)
		}
	}

	ver, err := newMemberVersionCreate(tx, f, raw, memberversion.ChangeTypeUpdate, now, diff)
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

	u := tx.Member.UpdateOneID(current.ID).SetCurrentVersionID(verID)
	applyToMemberUpdate(u, f)
	if _, err := u.Save(ctx); err != nil {
		return fmt.Errorf("update entity: %w", err)
	}
	return nil
}

func (p *MemberProcessor) applyDelete(ctx context.Context, tx *ent.Tx, f *MemberFields, raw *ent.RawOutput, current *ent.Member, currentVersion *ent.MemberVersion, now time.Time) error {
	if currentVersion != nil {
		if _, err := tx.MemberVersion.UpdateOneID(currentVersion.ID).
			SetValidTo(now).
			Save(ctx); err != nil {
			return fmt.Errorf("close current version: %w", err)
		}
	}

	ver, err := newMemberVersionCreate(tx, f, raw, memberversion.ChangeTypeDelete, now, nil)
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
		c := tx.Member.Create().
			SetID(f.MemberKey).
			SetCurrentVersionID(verID)
		applyToMemberCreate(c, f)
		if _, err := c.Save(ctx); err != nil {
			return fmt.Errorf("create tombstoned entity: %w", err)
		}
		return nil
	}

	u := tx.Member.UpdateOneID(current.ID).
		SetCurrentVersionID(verID).
		SetMlgCanView(false)
	if _, err := u.Save(ctx); err != nil {
		return fmt.Errorf("tombstone entity: %w", err)
	}
	return nil
}

func newMemberVersionCreate(tx *ent.Tx, f *MemberFields, raw *ent.RawOutput, ct memberversion.ChangeType, now time.Time, diff map[string]any) (*ent.MemberVersionCreate, error) {
	if raw == nil {
		return nil, errors.New("raw_output required")
	}
	c := tx.MemberVersion.Create().
		SetMemberKey(f.MemberKey).
		SetSyncEventID(raw.SyncEventID).
		SetRawOutputID(raw.ID).
		SetValidFrom(now).
		SetChangeType(ct).
		SetProcessorVersion(version.Info())
	if diff != nil {
		c.SetChangedFields(diff)
	}
	applyToMemberVersionCreate(c, f)
	return c, nil
}

// --- diff ---

func diffMemberFields(currentVersion *ent.MemberVersion, next *MemberFields) map[string]any {
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

	// Reflect over the remaining pointer fields. Same shape as Property's
	// diffReflect — see property.go for the rationale.
	diffMemberReflect(currentVersion, next, diff)

	if !reflect.DeepEqual(currentVersion.ExtendedFields, next.ExtendedFields) {
		push("extended_fields", currentVersion.ExtendedFields, next.ExtendedFields)
	}

	if len(diff) == 0 {
		return nil
	}
	return diff
}

func diffMemberReflect(currentVersion *ent.MemberVersion, next *MemberFields, out map[string]any) {
	vNext := reflect.ValueOf(next).Elem()
	tNext := vNext.Type()
	vCur := reflect.ValueOf(currentVersion).Elem()
	tCur := vCur.Type()

	skip := map[string]bool{
		"MemberKey":             true,
		"SourceModifiedAt":      true,
		"OriginatingSystemName": true,
		"MlgCanView":             true,
		"MlgCanUse":              true,
		"ExtendedFields":         true,
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

// --- apply: MemberFields → ent builders ---

func applyToMemberCreate(c *ent.MemberCreate, f *MemberFields) {
	c.SetSourceModifiedAt(f.SourceModifiedAt).
		SetMlgCanView(f.MlgCanView).
		SetNillableOriginatingSystemName(f.OriginatingSystemName)
	if f.MlgCanUse != nil {
		c.SetMlgCanUse(f.MlgCanUse)
	}
	c.SetNillableMemberMlsID(f.MemberMlsID).
		SetNillableMemberFirstName(f.MemberFirstName).
		SetNillableMemberMiddleName(f.MemberMiddleName).
		SetNillableMemberLastName(f.MemberLastName).
		SetNillableMemberFullName(f.MemberFullName).
		SetNillableMemberNamePrefix(f.MemberNamePrefix).
		SetNillableMemberNameSuffix(f.MemberNameSuffix).
		SetNillableMemberNickname(f.MemberNickname).
		SetNillableMemberStatus(f.MemberStatus).
		SetNillableMemberDirectPhone(f.MemberDirectPhone).
		SetNillableMemberMobilePhone(f.MemberMobilePhone).
		SetNillableMemberHomePhone(f.MemberHomePhone).
		SetNillableMemberPreferredPhone(f.MemberPreferredPhone).
		SetNillableMemberPreferredPhoneExt(f.MemberPreferredPhoneExt).
		SetNillableMemberOfficePhoneExt(f.MemberOfficePhoneExt).
		SetNillableMemberFax(f.MemberFax).
		SetNillableMemberAddress1(f.MemberAddress1).
		SetNillableMemberAddress2(f.MemberAddress2).
		SetNillableMemberCity(f.MemberCity).
		SetNillableMemberStateOrProvince(f.MemberStateOrProvince).
		SetNillableMemberPostalCode(f.MemberPostalCode).
		SetNillableMemberPostalCodePlus4(f.MemberPostalCodePlus4).
		SetNillableMemberCountry(f.MemberCountry).
		SetNillableMemberCountyOrParish(f.MemberCountyOrParish).
		SetNillableOfficeKey(f.OfficeKey).
		SetNillableOfficeMlsID(f.OfficeMlsID)
	if f.ExtendedFields != nil {
		c.SetExtendedFields(f.ExtendedFields)
	}
}

// applyToMemberUpdate uses clear-on-nil semantics: a nil pointer in
// MemberFields means the field was absent from the payload, which MLS Grid
// uses to signal "cleared upstream." SetNillableX(nil) is a no-op in ent —
// using it would leave stale values on the entity row while the version row
// (built fresh from PropertyFields → NULL) records the clear, producing
// entity/version drift. Same for the optional slice/map fields.
func applyToMemberUpdate(c *ent.MemberUpdateOne, f *MemberFields) {
	c.SetSourceModifiedAt(f.SourceModifiedAt).SetMlgCanView(f.MlgCanView)
	setOrClearStr(f.OriginatingSystemName, c.SetOriginatingSystemName, c.ClearOriginatingSystemName)
	setOrClearSlice(f.MlgCanUse, c.SetMlgCanUse, c.ClearMlgCanUse)
	setOrClearStr(f.MemberMlsID, c.SetMemberMlsID, c.ClearMemberMlsID)
	setOrClearStr(f.MemberFirstName, c.SetMemberFirstName, c.ClearMemberFirstName)
	setOrClearStr(f.MemberMiddleName, c.SetMemberMiddleName, c.ClearMemberMiddleName)
	setOrClearStr(f.MemberLastName, c.SetMemberLastName, c.ClearMemberLastName)
	setOrClearStr(f.MemberFullName, c.SetMemberFullName, c.ClearMemberFullName)
	setOrClearStr(f.MemberNamePrefix, c.SetMemberNamePrefix, c.ClearMemberNamePrefix)
	setOrClearStr(f.MemberNameSuffix, c.SetMemberNameSuffix, c.ClearMemberNameSuffix)
	setOrClearStr(f.MemberNickname, c.SetMemberNickname, c.ClearMemberNickname)
	setOrClearStr(f.MemberStatus, c.SetMemberStatus, c.ClearMemberStatus)
	setOrClearStr(f.MemberDirectPhone, c.SetMemberDirectPhone, c.ClearMemberDirectPhone)
	setOrClearStr(f.MemberMobilePhone, c.SetMemberMobilePhone, c.ClearMemberMobilePhone)
	setOrClearStr(f.MemberHomePhone, c.SetMemberHomePhone, c.ClearMemberHomePhone)
	setOrClearStr(f.MemberPreferredPhone, c.SetMemberPreferredPhone, c.ClearMemberPreferredPhone)
	setOrClearStr(f.MemberPreferredPhoneExt, c.SetMemberPreferredPhoneExt, c.ClearMemberPreferredPhoneExt)
	setOrClearStr(f.MemberOfficePhoneExt, c.SetMemberOfficePhoneExt, c.ClearMemberOfficePhoneExt)
	setOrClearStr(f.MemberFax, c.SetMemberFax, c.ClearMemberFax)
	setOrClearStr(f.MemberAddress1, c.SetMemberAddress1, c.ClearMemberAddress1)
	setOrClearStr(f.MemberAddress2, c.SetMemberAddress2, c.ClearMemberAddress2)
	setOrClearStr(f.MemberCity, c.SetMemberCity, c.ClearMemberCity)
	setOrClearStr(f.MemberStateOrProvince, c.SetMemberStateOrProvince, c.ClearMemberStateOrProvince)
	setOrClearStr(f.MemberPostalCode, c.SetMemberPostalCode, c.ClearMemberPostalCode)
	setOrClearStr(f.MemberPostalCodePlus4, c.SetMemberPostalCodePlus4, c.ClearMemberPostalCodePlus4)
	setOrClearStr(f.MemberCountry, c.SetMemberCountry, c.ClearMemberCountry)
	setOrClearStr(f.MemberCountyOrParish, c.SetMemberCountyOrParish, c.ClearMemberCountyOrParish)
	setOrClearStr(f.OfficeKey, c.SetOfficeKey, c.ClearOfficeKey)
	setOrClearStr(f.OfficeMlsID, c.SetOfficeMlsID, c.ClearOfficeMlsID)
	setOrClearMap(f.ExtendedFields, c.SetExtendedFields, c.ClearExtendedFields)
}

func applyToMemberVersionCreate(c *ent.MemberVersionCreate, f *MemberFields) {
	c.SetSourceModifiedAt(f.SourceModifiedAt).
		SetMlgCanView(f.MlgCanView).
		SetNillableOriginatingSystemName(f.OriginatingSystemName)
	if f.MlgCanUse != nil {
		c.SetMlgCanUse(f.MlgCanUse)
	}
	c.SetNillableMemberMlsID(f.MemberMlsID).
		SetNillableMemberFirstName(f.MemberFirstName).
		SetNillableMemberMiddleName(f.MemberMiddleName).
		SetNillableMemberLastName(f.MemberLastName).
		SetNillableMemberFullName(f.MemberFullName).
		SetNillableMemberNamePrefix(f.MemberNamePrefix).
		SetNillableMemberNameSuffix(f.MemberNameSuffix).
		SetNillableMemberNickname(f.MemberNickname).
		SetNillableMemberStatus(f.MemberStatus).
		SetNillableMemberDirectPhone(f.MemberDirectPhone).
		SetNillableMemberMobilePhone(f.MemberMobilePhone).
		SetNillableMemberHomePhone(f.MemberHomePhone).
		SetNillableMemberPreferredPhone(f.MemberPreferredPhone).
		SetNillableMemberPreferredPhoneExt(f.MemberPreferredPhoneExt).
		SetNillableMemberOfficePhoneExt(f.MemberOfficePhoneExt).
		SetNillableMemberFax(f.MemberFax).
		SetNillableMemberAddress1(f.MemberAddress1).
		SetNillableMemberAddress2(f.MemberAddress2).
		SetNillableMemberCity(f.MemberCity).
		SetNillableMemberStateOrProvince(f.MemberStateOrProvince).
		SetNillableMemberPostalCode(f.MemberPostalCode).
		SetNillableMemberPostalCodePlus4(f.MemberPostalCodePlus4).
		SetNillableMemberCountry(f.MemberCountry).
		SetNillableMemberCountyOrParish(f.MemberCountyOrParish).
		SetNillableOfficeKey(f.OfficeKey).
		SetNillableOfficeMlsID(f.OfficeMlsID)
	if f.ExtendedFields != nil {
		c.SetExtendedFields(f.ExtendedFields)
	}
}
