package processor

import (
	"encoding/json"
	"fmt"
	"time"
)

// OfficeFields mirrors the typed columns on Office / OfficeVersion.
type OfficeFields struct {
	// --- Entity identity ---
	OfficeKey string

	// --- MLSMetadataMixin ---
	SourceModifiedAt      time.Time
	OriginatingSystemName *string
	MlgCanView            bool
	MlgCanUse             []string

	// --- Identity ---
	OfficeMlsID *string

	// --- Names / status ---
	OfficeName   *string
	OfficeStatus *string
	OfficeType   *string

	// --- Contact ---
	OfficePhone    *string
	OfficePhoneExt *string
	OfficeFax      *string

	// --- Address ---
	OfficeAddress1        *string
	OfficeAddress2        *string
	OfficeCity            *string
	OfficeStateOrProvince *string
	OfficePostalCode      *string
	OfficePostalCodePlus4 *string
	OfficeCountyOrParish  *string

	// --- Identifiers ---
	OfficeCorporateLicense      *string
	OfficeNationalAssociationID *string

	// --- Org structure ---
	MainOfficeKey     *string
	MainOfficeMlsID   *string
	OfficeBrokerKey   *string
	OfficeBrokerMlsID *string
	OfficeManagerKey  *string

	// --- Flags / timestamps ---
	IdxOfficeParticipationYn *bool
	PhotosChangeTimestamp    *time.Time

	ExtendedFields map[string]any
}

// parseOffice turns a raw_output.payload JSON blob into OfficeFields.
// Poisons on missing OfficeKey or ModificationTimestamp.
func parseOffice(payload []byte) (*OfficeFields, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("payload is not a JSON object: %w", err)
	}

	out := &OfficeFields{MlgCanView: true}

	consume := func(key string) (json.RawMessage, bool) {
		v, ok := raw[key]
		if ok {
			delete(raw, key)
		}
		return v, ok
	}

	keyRaw, ok := consume("OfficeKey")
	if !ok {
		return nil, fmt.Errorf("missing required field OfficeKey")
	}
	officeKey, err := parseString(keyRaw)
	if err != nil || officeKey == nil || *officeKey == "" {
		return nil, fmt.Errorf("OfficeKey: empty or invalid")
	}
	out.OfficeKey = *officeKey

	tsRaw, ok := consume("ModificationTimestamp")
	modAt, err := requiredModTimestamp(tsRaw, ok)
	if err != nil {
		return nil, err
	}
	out.SourceModifiedAt = modAt

	if err := parseMLSMetadata(consume, &out.OriginatingSystemName, &out.MlgCanView, &out.MlgCanUse); err != nil {
		return nil, err
	}

	type strPtr = **string
	stringFields := map[string]strPtr{
		"OfficeMlsId":                 &out.OfficeMlsID,
		"OfficeName":                  &out.OfficeName,
		"OfficeStatus":                &out.OfficeStatus,
		"OfficeType":                  &out.OfficeType,
		"OfficePhone":                 &out.OfficePhone,
		"OfficePhoneExt":              &out.OfficePhoneExt,
		"OfficeFax":                   &out.OfficeFax,
		"OfficeAddress1":              &out.OfficeAddress1,
		"OfficeAddress2":              &out.OfficeAddress2,
		"OfficeCity":                  &out.OfficeCity,
		"OfficeStateOrProvince":       &out.OfficeStateOrProvince,
		"OfficePostalCode":            &out.OfficePostalCode,
		"OfficePostalCodePlus4":       &out.OfficePostalCodePlus4,
		"OfficeCountyOrParish":        &out.OfficeCountyOrParish,
		"OfficeCorporateLicense":      &out.OfficeCorporateLicense,
		"OfficeNationalAssociationId": &out.OfficeNationalAssociationID,
		"MainOfficeKey":               &out.MainOfficeKey,
		"MainOfficeMlsId":             &out.MainOfficeMlsID,
		"OfficeBrokerKey":             &out.OfficeBrokerKey,
		"OfficeBrokerMlsId":           &out.OfficeBrokerMlsID,
		"OfficeManagerKey":            &out.OfficeManagerKey,
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

	if v, ok := consume("IdxOfficeParticipationYN"); ok {
		out.IdxOfficeParticipationYn, err = parseBool(v)
		if err != nil {
			return nil, fmt.Errorf("IdxOfficeParticipationYN: %w", err)
		}
	}
	if v, ok := consume("PhotosChangeTimestamp"); ok {
		out.PhotosChangeTimestamp, err = parseTime(v)
		if err != nil {
			return nil, fmt.Errorf("PhotosChangeTimestamp: %w", err)
		}
	}

	ext, err := bucketExtendedFields(raw)
	if err != nil {
		return nil, err
	}
	out.ExtendedFields = ext

	return out, nil
}
