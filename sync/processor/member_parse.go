package processor

import (
	"encoding/json"
	"fmt"
	"time"
)

// MemberFields mirrors every typed column on the Member/MemberVersion schema.
// Pointer types for Optional+Nillable schema fields so the upsert can
// distinguish "absent" from "zero". Unknown payload keys flow into
// ExtendedFields rather than failing.
type MemberFields struct {
	// --- Entity identity ---
	MemberKey string

	// --- MLSMetadataMixin ---
	SourceModifiedAt      time.Time
	OriginatingSystemName *string
	MlgCanView            bool
	MlgCanUse             []string

	// --- Identity ---
	MemberMlsID *string

	// --- Names ---
	MemberFirstName  *string
	MemberMiddleName *string
	MemberLastName   *string
	MemberFullName   *string
	MemberNamePrefix *string
	MemberNameSuffix *string
	MemberNickname   *string

	// --- Status ---
	MemberStatus *string

	// --- Phones ---
	MemberDirectPhone       *string
	MemberMobilePhone       *string
	MemberHomePhone         *string
	MemberPreferredPhone    *string
	MemberPreferredPhoneExt *string
	MemberOfficePhoneExt    *string
	MemberFax               *string

	// --- Address ---
	MemberAddress1        *string
	MemberAddress2        *string
	MemberCity            *string
	MemberStateOrProvince *string
	MemberPostalCode      *string
	MemberPostalCodePlus4 *string
	MemberCountry         *string
	MemberCountyOrParish  *string

	// --- Office FK ---
	OfficeKey   *string
	OfficeMlsID *string

	// --- Catch-all for unmapped payload keys ---
	ExtendedFields map[string]any
}

// parseMember turns a raw_output.payload JSON blob into a typed MemberFields.
// Poisons on missing MemberKey or missing/malformed ModificationTimestamp.
func parseMember(payload []byte) (*MemberFields, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("payload is not a JSON object: %w", err)
	}

	out := &MemberFields{MlgCanView: true}

	consume := func(key string) (json.RawMessage, bool) {
		v, ok := raw[key]
		if ok {
			delete(raw, key)
		}
		return v, ok
	}

	// --- Required keys ---
	keyRaw, ok := consume("MemberKey")
	if !ok {
		return nil, fmt.Errorf("missing required field MemberKey")
	}
	memberKey, err := parseString(keyRaw)
	if err != nil || memberKey == nil || *memberKey == "" {
		return nil, fmt.Errorf("MemberKey: empty or invalid")
	}
	out.MemberKey = *memberKey

	tsRaw, ok := consume("ModificationTimestamp")
	modAt, err := requiredModTimestamp(tsRaw, ok)
	if err != nil {
		return nil, err
	}
	out.SourceModifiedAt = modAt

	// --- MLSMetadataMixin ---
	if err := parseMLSMetadata(consume, &out.OriginatingSystemName, &out.MlgCanView, &out.MlgCanUse); err != nil {
		return nil, err
	}

	type strPtr = **string
	stringFields := map[string]strPtr{
		"MemberMlsId":             &out.MemberMlsID,
		"MemberFirstName":         &out.MemberFirstName,
		"MemberMiddleName":        &out.MemberMiddleName,
		"MemberLastName":          &out.MemberLastName,
		"MemberFullName":          &out.MemberFullName,
		"MemberNamePrefix":        &out.MemberNamePrefix,
		"MemberNameSuffix":        &out.MemberNameSuffix,
		"MemberNickname":          &out.MemberNickname,
		"MemberStatus":            &out.MemberStatus,
		"MemberDirectPhone":       &out.MemberDirectPhone,
		"MemberMobilePhone":       &out.MemberMobilePhone,
		"MemberHomePhone":         &out.MemberHomePhone,
		"MemberPreferredPhone":    &out.MemberPreferredPhone,
		"MemberPreferredPhoneExt": &out.MemberPreferredPhoneExt,
		"MemberOfficePhoneExt":    &out.MemberOfficePhoneExt,
		"MemberFax":               &out.MemberFax,
		"MemberAddress1":          &out.MemberAddress1,
		"MemberAddress2":          &out.MemberAddress2,
		"MemberCity":              &out.MemberCity,
		"MemberStateOrProvince":   &out.MemberStateOrProvince,
		"MemberPostalCode":        &out.MemberPostalCode,
		"MemberPostalCodePlus4":   &out.MemberPostalCodePlus4,
		"MemberCountry":           &out.MemberCountry,
		"MemberCountyOrParish":    &out.MemberCountyOrParish,
		"OfficeKey":               &out.OfficeKey,
		"OfficeMlsId":             &out.OfficeMlsID,
	}
	for k, dst := range stringFields {
		if v, ok := consume(k); ok {
			parsed, err := parseString(v)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			*dst = parsed
		}
	}

	ext, err := bucketExtendedFields(raw)
	if err != nil {
		return nil, err
	}
	out.ExtendedFields = ext

	return out, nil
}
