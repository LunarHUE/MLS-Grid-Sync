package processor

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/lib/pq"
	"github.com/shopspring/decimal"
)

// PropertyFields mirrors every typed column on the Property/PropertyVersion
// schema. Pointer types are used for fields that the schema declares as
// Optional+Nillable so the upsert can distinguish "absent" from "zero".
// Value types are used for fields the schema requires.
//
// Unknown payload keys are collected into ExtendedFields (the catch-all JSONB
// column on the schema). Use propertyConsumedKeys() if you need the static set
// of typed keys for a coverage check.
type PropertyFields struct {
	// --- Entity identity (not a "data" column — drives the upsert key). ---
	ListingKey string

	// --- MLSMetadataMixin ---
	SourceModifiedAt      time.Time // required; ModificationTimestamp
	OriginatingSystemName *string
	MlgCanView            bool // schema default true; only flips false when the payload says so
	MlgCanUse             []string

	// --- Identity ---
	ListingID    *string
	ParcelNumber *string

	// --- Status & timestamps ---
	MlsStatus              *string
	StandardStatus         *string
	MajorChangeType        *string
	MajorChangeTimestamp   *time.Time
	ListingContractDate    *time.Time
	OnMarketTimestamp      *time.Time
	OriginalEntryTimestamp *time.Time
	PhotosChangeTimestamp  *time.Time
	AvailabilityDate       *time.Time

	// --- Pricing & tax ---
	ListPrice         *decimal.Decimal
	OriginalListPrice *decimal.Decimal
	PreviousListPrice *decimal.Decimal
	TaxAnnualAmount   *decimal.Decimal
	TaxAssessedValue  *int64
	TaxYear           *int16

	// --- Type & characteristics ---
	PropertyType          *string
	PropertySubType       *string
	NewConstructionYn     *bool
	BedroomsTotal         *int16
	BathroomsTotalInteger *int16
	BathroomsFull         *int16
	BathroomsHalf         *int16
	MainLevelBedrooms     *int16
	LivingArea            *decimal.Decimal
	BuildingAreaTotal     *decimal.Decimal
	LotSizeAcres          *decimal.Decimal
	LotSizeSquareFeet     *decimal.Decimal
	StoriesTotal          *int16
	YearBuilt             *int16
	GarageSpaces          *decimal.Decimal
	CoveredSpaces         *decimal.Decimal
	ParkingTotal          *decimal.Decimal
	FireplacesTotal       *int16
	PoolPrivateYn         *bool
	WaterfrontYn          *bool
	ViewYn                *bool
	HorseYn               *bool

	// --- Address ---
	StreetNumber        *string
	StreetNumberNumeric *int32
	StreetName          *string
	StreetSuffix        *string
	StreetDirPrefix     *string
	StreetDirSuffix     *string
	UnitNumber          *string
	UnparsedAddress     *string
	City                *string
	StateOrProvince     *string
	PostalCode          *string
	PostalCodePlus4     *string
	Country             *string
	CountyOrParish      *string
	SubdivisionName     *string
	MlsAreaMajor        *string

	// --- Geo ---
	Latitude  *decimal.Decimal
	Longitude *decimal.Decimal

	// --- Schools ---
	ElementarySchool     *string
	MiddleOrJuniorSchool *string
	HighSchool           *string
	HighSchoolDistrict   *string

	// --- Agent keys ---
	ListAgentKey      *string
	ListAgentMlsID    *string
	CoListAgentKey    *string
	CoListAgentMlsID  *string
	BuyerAgentKey     *string
	BuyerAgentMlsID   *string
	CoBuyerAgentKey   *string
	CoBuyerAgentMlsID *string

	// --- Office keys ---
	ListOfficeKey      *string
	ListOfficeMlsID    *string
	CoListOfficeKey    *string
	CoListOfficeMlsID  *string
	BuyerOfficeKey     *string
	BuyerOfficeMlsID   *string
	CoBuyerOfficeKey   *string
	CoBuyerOfficeMlsID *string

	// --- Internet display flags ---
	InternetEntireListingDisplayYn      *bool
	InternetAddressDisplayYn            *bool
	InternetAutomatedValuationDisplayYn *bool
	InternetConsumerCommentYn           *bool

	// --- RESO Collection arrays (text[]) ---
	Appliances               pq.StringArray
	Cooling                  pq.StringArray
	Heating                  pq.StringArray
	Flooring                 pq.StringArray
	Roof                     pq.StringArray
	ExteriorFeatures         pq.StringArray
	InteriorFeatures         pq.StringArray
	ParkingFeatures          pq.StringArray
	PoolFeatures             pq.StringArray
	View                     pq.StringArray
	WaterfrontFeatures       pq.StringArray
	CommunityFeatures        pq.StringArray
	AccessibilityFeatures    pq.StringArray
	Utilities                pq.StringArray
	Sewer                    pq.StringArray
	WaterSource              pq.StringArray
	LotFeatures              pq.StringArray
	PatioAndPorchFeatures    pq.StringArray
	SecurityFeatures         pq.StringArray
	ConstructionMaterials    pq.StringArray
	FoundationDetails        pq.StringArray
	Levels                   pq.StringArray
	FireplaceFeatures        pq.StringArray
	SpaFeatures              pq.StringArray
	Fencing                  pq.StringArray
	HorseAmenities           pq.StringArray
	WindowFeatures           pq.StringArray
	PetsAllowed              pq.StringArray
	Disclosures              pq.StringArray
	PropertyCondition        pq.StringArray
	SpecialListingConditions pq.StringArray
	GreenEnergyEfficient     pq.StringArray
	GreenSustainability      pq.StringArray
	SyndicateTo              pq.StringArray

	// --- Free text & misc ---
	PublicRemarks      *string
	SyndicationRemarks *string
	Directions         *string
	Furnished          *string
	DirectionFaces     *string

	// --- Catch-all for unmapped payload keys ---
	ExtendedFields map[string]any
}

// parseProperty turns a raw_output.payload JSON blob into a typed
// PropertyFields. Unknown keys flow into ExtendedFields rather than failing,
// so we don't reject legitimate RESO/Actris fields we haven't mapped yet.
//
// Fails (poison-record policy) when:
//   - the payload is not a JSON object,
//   - ListingKey is missing or empty,
//   - ModificationTimestamp is missing or not RFC 3339,
//   - any typed field's value is structurally wrong for its target Go type
//     (e.g. "house" in a numeric column).
func parseProperty(payload []byte) (*PropertyFields, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("payload is not a JSON object: %w", err)
	}

	out := &PropertyFields{
		MlgCanView:     true, // schema default
		ExtendedFields: nil,
	}

	consume := func(key string) (json.RawMessage, bool) {
		v, ok := raw[key]
		if ok {
			delete(raw, key)
		}
		return v, ok
	}

	// --- Required keys ---
	keyRaw, ok := consume("ListingKey")
	if !ok {
		return nil, fmt.Errorf("missing required field ListingKey")
	}
	listingKey, err := parseString(keyRaw)
	if err != nil || listingKey == nil || *listingKey == "" {
		return nil, fmt.Errorf("ListingKey: empty or invalid")
	}
	out.ListingKey = *listingKey

	tsRaw, ok := consume("ModificationTimestamp")
	if !ok {
		return nil, fmt.Errorf("missing required field ModificationTimestamp")
	}
	ts, err := parseTime(tsRaw)
	if err != nil || ts == nil {
		return nil, fmt.Errorf("ModificationTimestamp: %w", err)
	}
	out.SourceModifiedAt = *ts

	// --- MLSMetadataMixin ---
	if v, ok := consume("OriginatingSystemName"); ok {
		out.OriginatingSystemName, err = parseString(v)
		if err != nil {
			return nil, fmt.Errorf("OriginatingSystemName: %w", err)
		}
	}
	if v, ok := consume("MlgCanView"); ok {
		b, err := parseBool(v)
		if err != nil {
			return nil, fmt.Errorf("MlgCanView: %w", err)
		}
		if b != nil {
			out.MlgCanView = *b
		}
	}
	if v, ok := consume("MlgCanUse"); ok {
		arr, err := parseStringArray(v)
		if err != nil {
			return nil, fmt.Errorf("MlgCanUse: %w", err)
		}
		out.MlgCanUse = []string(arr)
	}

	// Bulk-parse the rest via a name → assigner table. Each row consumes its
	// key from raw if present and assigns into the corresponding field. After
	// this loop, whatever remains in raw is unmapped and gets sunk into
	// ExtendedFields.
	type strPtr = **string
	type intPtr16 = **int16
	type intPtr32 = **int32
	type intPtr64 = **int64
	type decPtr = **decimal.Decimal
	type boolPtr = **bool
	type timePtr = **time.Time
	type arrPtr = *pq.StringArray

	stringFields := map[string]strPtr{
		"ListingId":          &out.ListingID,
		"ParcelNumber":       &out.ParcelNumber,
		"MlsStatus":          &out.MlsStatus,
		"StandardStatus":     &out.StandardStatus,
		"MajorChangeType":    &out.MajorChangeType,
		"PropertyType":       &out.PropertyType,
		"PropertySubType":    &out.PropertySubType,
		"StreetNumber":       &out.StreetNumber,
		"StreetName":         &out.StreetName,
		"StreetSuffix":       &out.StreetSuffix,
		"StreetDirPrefix":    &out.StreetDirPrefix,
		"StreetDirSuffix":    &out.StreetDirSuffix,
		"UnitNumber":         &out.UnitNumber,
		"UnparsedAddress":    &out.UnparsedAddress,
		"City":               &out.City,
		"StateOrProvince":    &out.StateOrProvince,
		"PostalCode":         &out.PostalCode,
		"PostalCodePlus4":    &out.PostalCodePlus4,
		"Country":            &out.Country,
		"CountyOrParish":     &out.CountyOrParish,
		"SubdivisionName":    &out.SubdivisionName,
		"MLSAreaMajor":       &out.MlsAreaMajor,
		"ElementarySchool":   &out.ElementarySchool,
		"MiddleOrJuniorSchool": &out.MiddleOrJuniorSchool,
		"HighSchool":         &out.HighSchool,
		"HighSchoolDistrict": &out.HighSchoolDistrict,
		"ListAgentKey":       &out.ListAgentKey,
		"ListAgentMlsId":     &out.ListAgentMlsID,
		"CoListAgentKey":     &out.CoListAgentKey,
		"CoListAgentMlsId":   &out.CoListAgentMlsID,
		"BuyerAgentKey":      &out.BuyerAgentKey,
		"BuyerAgentMlsId":    &out.BuyerAgentMlsID,
		"CoBuyerAgentKey":    &out.CoBuyerAgentKey,
		"CoBuyerAgentMlsId":  &out.CoBuyerAgentMlsID,
		"ListOfficeKey":      &out.ListOfficeKey,
		"ListOfficeMlsId":    &out.ListOfficeMlsID,
		"CoListOfficeKey":    &out.CoListOfficeKey,
		"CoListOfficeMlsId":  &out.CoListOfficeMlsID,
		"BuyerOfficeKey":     &out.BuyerOfficeKey,
		"BuyerOfficeMlsId":   &out.BuyerOfficeMlsID,
		"CoBuyerOfficeKey":   &out.CoBuyerOfficeKey,
		"CoBuyerOfficeMlsId": &out.CoBuyerOfficeMlsID,
		"PublicRemarks":      &out.PublicRemarks,
		"SyndicationRemarks": &out.SyndicationRemarks,
		"Directions":         &out.Directions,
		"Furnished":          &out.Furnished,
		"DirectionFaces":     &out.DirectionFaces,
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

	// RESO temporal classification: Timestamp (full RFC 3339) vs Date (bare
	// YYYY-MM-DD). Routing differs but both land in *time.Time slots.
	//
	// Empirical audit (per plan §2): run
	//   SELECT key, count(*) FROM raw_output, jsonb_each_text(payload)
	//   WHERE resource='property' AND value ~ '^\d{4}-\d{2}-\d{2}$'
	//   GROUP BY key;
	// to re-confirm before promoting any ExtendedFields key into dateFields.
	// Misrouting is caught by parseDate/parseTime's symmetric rejection plus
	// the buildFullPropertyPayload sentinel split (const dt vs const ts).
	timeFields := map[string]timePtr{
		"MajorChangeTimestamp":   &out.MajorChangeTimestamp,
		"OnMarketTimestamp":      &out.OnMarketTimestamp,
		"OriginalEntryTimestamp": &out.OriginalEntryTimestamp,
		"PhotosChangeTimestamp":  &out.PhotosChangeTimestamp,
	}
	for k, dst := range timeFields {
		if v, ok := consume(k); ok {
			parsed, err := parseTime(v)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			*dst = parsed
		}
	}

	dateFields := map[string]timePtr{
		"ListingContractDate": &out.ListingContractDate,
		"AvailabilityDate":    &out.AvailabilityDate,
	}
	for k, dst := range dateFields {
		if v, ok := consume(k); ok {
			parsed, err := parseDate(v)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			*dst = parsed
		}
	}

	decFields := map[string]decPtr{
		"ListPrice":         &out.ListPrice,
		"OriginalListPrice": &out.OriginalListPrice,
		"PreviousListPrice": &out.PreviousListPrice,
		"TaxAnnualAmount":   &out.TaxAnnualAmount,
		"LivingArea":        &out.LivingArea,
		"BuildingAreaTotal": &out.BuildingAreaTotal,
		"LotSizeAcres":      &out.LotSizeAcres,
		"LotSizeSquareFeet": &out.LotSizeSquareFeet,
		"GarageSpaces":      &out.GarageSpaces,
		"CoveredSpaces":     &out.CoveredSpaces,
		"ParkingTotal":      &out.ParkingTotal,
		"Latitude":          &out.Latitude,
		"Longitude":         &out.Longitude,
	}
	for k, dst := range decFields {
		if v, ok := consume(k); ok {
			parsed, err := parseDecimal(v)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			*dst = parsed
		}
	}

	int16Fields := map[string]intPtr16{
		"TaxYear":               &out.TaxYear,
		"BedroomsTotal":         &out.BedroomsTotal,
		"BathroomsTotalInteger": &out.BathroomsTotalInteger,
		"BathroomsFull":         &out.BathroomsFull,
		"BathroomsHalf":         &out.BathroomsHalf,
		"MainLevelBedrooms":     &out.MainLevelBedrooms,
		"StoriesTotal":          &out.StoriesTotal,
		"YearBuilt":             &out.YearBuilt,
		"FireplacesTotal":       &out.FireplacesTotal,
	}
	for k, dst := range int16Fields {
		if v, ok := consume(k); ok {
			parsed, err := parseInt16(v)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			*dst = parsed
		}
	}

	int32Fields := map[string]intPtr32{
		"StreetNumberNumeric": &out.StreetNumberNumeric,
	}
	for k, dst := range int32Fields {
		if v, ok := consume(k); ok {
			parsed, err := parseInt32(v)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			*dst = parsed
		}
	}

	int64Fields := map[string]intPtr64{
		"TaxAssessedValue": &out.TaxAssessedValue,
	}
	for k, dst := range int64Fields {
		if v, ok := consume(k); ok {
			parsed, err := parseInt64(v)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			*dst = parsed
		}
	}

	boolFields := map[string]boolPtr{
		"NewConstructionYN":                    &out.NewConstructionYn,
		"PoolPrivateYN":                        &out.PoolPrivateYn,
		"WaterfrontYN":                         &out.WaterfrontYn,
		"ViewYN":                               &out.ViewYn,
		"HorseYN":                              &out.HorseYn,
		"InternetEntireListingDisplayYN":       &out.InternetEntireListingDisplayYn,
		"InternetAddressDisplayYN":             &out.InternetAddressDisplayYn,
		"InternetAutomatedValuationDisplayYN":  &out.InternetAutomatedValuationDisplayYn,
		"InternetConsumerCommentYN":            &out.InternetConsumerCommentYn,
	}
	for k, dst := range boolFields {
		if v, ok := consume(k); ok {
			parsed, err := parseBool(v)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			*dst = parsed
		}
	}

	arrFields := map[string]arrPtr{
		"Appliances":               &out.Appliances,
		"Cooling":                  &out.Cooling,
		"Heating":                  &out.Heating,
		"Flooring":                 &out.Flooring,
		"Roof":                     &out.Roof,
		"ExteriorFeatures":         &out.ExteriorFeatures,
		"InteriorFeatures":         &out.InteriorFeatures,
		"ParkingFeatures":          &out.ParkingFeatures,
		"PoolFeatures":             &out.PoolFeatures,
		"View":                     &out.View,
		"WaterfrontFeatures":       &out.WaterfrontFeatures,
		"CommunityFeatures":        &out.CommunityFeatures,
		"AccessibilityFeatures":    &out.AccessibilityFeatures,
		"Utilities":                &out.Utilities,
		"Sewer":                    &out.Sewer,
		"WaterSource":              &out.WaterSource,
		"LotFeatures":              &out.LotFeatures,
		"PatioAndPorchFeatures":    &out.PatioAndPorchFeatures,
		"SecurityFeatures":         &out.SecurityFeatures,
		"ConstructionMaterials":    &out.ConstructionMaterials,
		"FoundationDetails":        &out.FoundationDetails,
		"Levels":                   &out.Levels,
		"FireplaceFeatures":        &out.FireplaceFeatures,
		"SpaFeatures":              &out.SpaFeatures,
		"Fencing":                  &out.Fencing,
		"HorseAmenities":           &out.HorseAmenities,
		"WindowFeatures":           &out.WindowFeatures,
		"PetsAllowed":              &out.PetsAllowed,
		"Disclosures":              &out.Disclosures,
		"PropertyCondition":        &out.PropertyCondition,
		"SpecialListingConditions": &out.SpecialListingConditions,
		"GreenEnergyEfficient":     &out.GreenEnergyEfficient,
		"GreenSustainability":      &out.GreenSustainability,
		"SyndicateTo":              &out.SyndicateTo,
	}
	for k, dst := range arrFields {
		if v, ok := consume(k); ok {
			parsed, err := parseStringArray(v)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			*dst = parsed
		}
	}

	// Whatever is left is unmapped — bucket into ExtendedFields.
	ext, err := bucketExtendedFields(raw)
	if err != nil {
		return nil, err
	}
	out.ExtendedFields = ext

	return out, nil
}
