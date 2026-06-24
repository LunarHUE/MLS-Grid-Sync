package processor

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"sync"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/applog"
	"github.com/LunarHUE/MLS-Grid-Sync/version"
)

// Field-drift diagnostics.
//
// When the MLS Grid feed gains a new field, the parsers don't know about it and
// it lands in extended_fields (bucketExtendedFields, parse.go) silently. This
// file samples a small fraction of processed records during a pass and warns —
// once per process per field — when a key shows up in extended_fields that we
// don't already expect. The operator gets the field's value, a pre-filled
// GitHub issue link, and a nudge to upgrade in case a newer release already
// maps it.
//
// The check is deliberately cheap and off the transaction path: it re-parses
// only the ~0.4% of payloads it samples (reusing the validate.go dispatch), so
// the other 99.6% of records pay just one rand.Float64() compare.

// sampleRate is the probability a processed record is inspected for field
// drift — ≈ 4 samples per 1000 records. A var (not const) so tests can force
// 1.0; new feed-wide fields appear on most records, so a low rate still catches
// them quickly across a multi-thousand-record pass.
var sampleRate = 0.004

// sampled reports whether this record should be inspected. math/rand/v2's
// top-level funcs are goroutine-safe and lock-free, which matters because
// distinct resources can run passes concurrently.
func sampled() bool { return rand.Float64() < sampleRate }

// knownExtendedFields is the per-resource allowlist of keys we already expect
// to land in extended_fields. The drift check warns only on keys NOT listed
// here, so it flags genuinely new spec fields rather than the fields we've
// already decided to leave unmapped. A resource absent from the map treats
// every leftover key as novel.
//
// Regenerate after a deliberate parser change with `go run . validate-raw all`
// and copy each resource's reported "Unconsumed payload keys" into
// knownExtendedKeys below. Last seeded 2026-06-24 against the Actris/FHR feed.
var knownExtendedFields = buildKnownExtended(knownExtendedKeys)

func buildKnownExtended(m map[rawoutput.Resource][]string) map[rawoutput.Resource]map[string]bool {
	out := make(map[rawoutput.Resource]map[string]bool, len(m))
	for r, keys := range m {
		set := make(map[string]bool, len(keys))
		for _, k := range keys {
			set[k] = true
		}
		out[r] = set
	}
	return out
}

// knownExtendedKeys holds the baseline as plain string slices, in the
// frequency order `validate-raw` prints, so re-seeding is a straight copy.
// lookup and media are omitted: they currently have zero unconsumed keys.
var knownExtendedKeys = map[rawoutput.Resource][]string{
	rawoutput.ResourceOffice: {
		"@odata.id", "FranchiseAffiliation", "IDXOfficeParticipationYN",
		"OriginalEntryTimestamp", "OfficeEmail",
	},
	rawoutput.ResourceMember: {
		"@odata.id", "FHR_MainOfficeMlsId", "FHR_OfficeType", "FHR_Status",
		"MemberType", "OfficeName", "OriginalEntryTimestamp", "MemberEmail",
		"MemberNationalAssociationId", "MemberOfficePhone", "FHR_MemberDirectPhoneExt",
	},
	rawoutput.ResourceOpenHouse: {
		"@odata.id", "FHR_OH_StartTime", "OpenHouseRemarks",
	},
	rawoutput.ResourcePropertyRooms: {
		"RoomDimensions", "RoomDescription",
	},
	rawoutput.ResourcePropertyUnitTypes: {
		"UnitTypeType", "UnitTypeBathsTotal", "FHR_Vacant", "UnitTypeActualRent",
		"UnitTypeDescription", "FHR_LeaseExpires", "FHR_SecurityDeposit",
	},
	rawoutput.ResourceProperty: {
		"@odata.id", "DocumentsChangeTimestamp", "DocumentsCount", "FHR_AgentOwned",
		"FHR_ForSaleRent", "FHR_ListMainOfficeMlsId", "FHR_VOWInclude", "ListAgentFirstName",
		"ListAgentLastName", "ListOfficeName", "PhotosCount", "FHR_SearchPrice",
		"CumulativeDaysOnMarket", "DaysOnMarket", "FHR_DaysOnMls", "FHR_ExclusiveAgency",
		"ListAgentEmail", "FHR_SearchNumber", "FHR_ListingAgencyType", "ListingTerms",
		"TaxLegalDescription", "StatusChangeTimestamp", "FHR_DistressedProperty", "FHR_SpecialTaxes",
		"FHR_GeneralTaxes", "FHR_TitleCompany", "PriceChangeTimestamp", "FHR_HotSheetDate",
		"ListOfficeEmail", "ListAgentPreferredPhone", "ListOfficePhone", "AssociationYN",
		"BuyerAgentFirstName", "BuyerAgentLastName", "BuyerOfficeName", "Concessions",
		"FHR_AgentHitCount", "FHR_ClientHitCount", "FHR_PricePerSqFt", "Basement",
		"FHR_NumberConformingBedrooms", "FHR_BasementLightExposure", "FHR_ConstructionType", "BasementYN",
		"FHR_ApproxAcres", "FHR_ApproxSquareFootage", "BuyerAgentEmail", "BuyerOfficeEmail",
		"BuyerOfficePhone", "BuyerAgentPreferredPhone", "FHR_Age", "AboveGradeFinishedArea",
		"FHR_BasementLevelBathrooms", "FHR_BasementLevelBedrooms", "FHR_Level1Bathrooms", "FHR_Level1Bedrooms",
		"FHR_Level2Bathrooms", "FHR_Level2Bedrooms", "FHR_Level3Bathrooms", "FHR_Level3Bedrooms",
		"FHR_GarageType", "FHR_WaterSewerType", "FHR_AgentLicense", "FHR_BrokerLicenseNumber",
		"RoadSurfaceType", "FHR_FuelType", "ArchitecturalStyle", "Possession",
		"RoadFrontageType", "FHR_PersonalPropertyIncluded", "LockBoxLocation", "FHR_MainLevelSqFt",
		"FHR_BelowGradeArea", "Contingency", "DocumentsAvailable", "FHR_PersonalPropertyIncludedDetail",
		"FHR_UtilitiesToProperty", "OtherStructures", "FHR_SubAgentYN", "Disclaimer",
		"FHR_WaterTreatmentSystem", "ListOfficeURL", "BuyerOfficeURL", "BelowGradeFinishedArea",
		"AssociationAmenities", "VirtualTourURLUnbranded", "ListAgentOfficePhone", "FHR_Ponds",
		"FHR_PersonalPropertyExcluded", "AssociationFee", "BuyerAgentOfficePhone", "AssociationFeeFrequency",
		"AssociationFeeIncludes", "Zoning", "HomeWarrantyYN", "FHR_OptionalRentalProperty",
		"ListAgentURL", "CoListOfficeName", "CoListAgentFirstName", "CoListAgentLastName",
		"CoListOfficePhone", "CoListOfficeEmail", "CoListAgentEmail", "FHR_LotDepth",
		"CoListAgentPreferredPhone", "FHR_LandData", "FHR_PossessionDate", "BuyerAgentURL",
		"CurrentUse", "FrontageLength", "FHR_LotSizeWidth", "ListAgentMiddleName",
		"FHR_HomeWarrantyVendor", "BuyerAgentMiddleName", "FHR_FloodZoneInsurance", "FHR_Miscellaneous",
		"FHR_SiteFeatures", "NumberOfUnitsTotal", "FHR_HomeWarrantyDeductible", "FHR_HomeWarrantyPrice",
		"LeaseExpiration", "OtherEquipment", "CoBuyerOfficeName", "CoBuyerAgentFirstName",
		"CoBuyerAgentLastName", "FHR_EstimatedCompletionDate", "CoBuyerOfficeEmail", "CoListOfficeURL",
		"CoBuyerOfficePhone", "CoBuyerAgentEmail", "CoBuyerAgentPreferredPhone", "CoListAgentOfficePhone",
		"FHR_RentalRate", "FHR_CurrentAnnualRent", "PossibleUse", "BuildingFeatures",
		"FHR_EquipmentIncluded", "FHR_LeaseTypeTerms", "FHR_PropertyAssociationYN", "FHR_SaleIncludes",
		"FHR_SpaceForLeaseYN", "FHR_MonthToMonth", "ListAgentPreferredPhoneExt", "BuyerAgentPreferredPhoneExt",
		"FHR_SecurityDeposit", "TenantPays", "FHR_OffStreetParkingSpaces", "FHR_Location",
		"FHR_CRPYN", "FHR_CurrentVacancyRate", "GrossIncome", "FHR_PetsAllowedYN",
		"Stories", "FHR_PropertyManagementCompany", "CoListAgentMiddleName", "FHR_NumberOfRestrooms",
		"FHR_GrossBuildingArea", "CoBuyerAgentOfficePhone", "FHR_PreviousYearVacancyRate", "FHR_ApproxPastureAcres",
		"FHR_RentVerifiableYN", "CoBuyerOfficeURL", "FHR_ApproxTillableAcres", "FHR_LotSizeLength",
		"FHR_ADACompliant", "FHR_TotalSecurityDepositAmount", "FHR_LeaseIncludes", "ZoningDescription",
		"FHR_NumberOfOffices", "FHR_CodeInspectionYN", "CoBuyerAgentMiddleName", "FHR_MethodOfVerification",
		"FHR_ApproxWoodedAcres", "FHR_NumberOfOverheadDoors", "BusinessType", "OpenParkingSpaces",
		"FHR_ApproxAcresLeased", "FHR_PersonalPropertyTaxes", "OperatingExpense", "FHR_ApproxNonTillableAcres",
		"FHR_PropertyManagementFee", "CoListAgentURL", "FHR_HistoricalRestrictions", "FHR_RentAsIsYN",
		"WaterSewerExpense", "FHR_ApproxClearedAcres", "FHR_NumberLoadingDocks", "FHR_PetDeposit",
		"TrashExpense", "CoListAgentPreferredPhoneExt", "FHR_OverheadDoorHeight", "FHR_RestrictiveCovenants",
		"FHR_Insurance", "NetOperatingIncome", "FHR_ApproxWetlandAcres", "ElectricExpense",
		"FHR_CeilingHeight", "FuelExpense", "FHR_CodeCompliance", "FHR_PropertyAssociationDues",
		"CoBuyerAgentURL", "FHR_EscrowAccount", "FHR_OtherMonthlyIncome", "CapRate",
		"OtherExpense", "GardenerExpense", "MaintenanceExpense", "FHR_NetAverageAnnualIncome",
		"FHR_AverageAnnualExpense", "FHR_RentalPerFootGross", "FHR_SpecialAssessment1Desc", "FHR_AmenityFee",
		"FHR_RentalPerFootNet", "FHR_SpecialAssessment1Amount", "FHR_SpecialAssessment2Amount", "FHR_SpecialAssessment2Desc",
		"FHR_SpecialAssessment3Amount", "FHR_SpecialAssessment3Desc", "FHR_SpecialAssessment4Amount", "FHR_SpecialAssessment4Desc",
	},
}

// warnedDrift dedupes warnings so each novel field alerts at most once per
// process. Guarded by driftMu because concurrent resource passes may write it.
var (
	driftMu     sync.Mutex
	warnedDrift = map[string]bool{} // key: "<resource>.<field>"
)

// checkFieldDrift inspects one processed record for unmapped fields, gated by
// the sampler. It is fully defensive: it never returns an error and never
// panics into the pass — a parse failure here is ignored because the real
// Process path already surfaces parse errors with poison-record semantics.
func checkFieldDrift(resource rawoutput.Resource, raw *ent.RawOutput) {
	if !sampled() {
		return
	}
	d, err := dispatchFor(resource)
	if err != nil {
		return
	}
	parsed, err := d.parse(raw.Payload)
	if err != nil {
		return
	}
	known := knownExtendedFields[resource]
	for k, v := range d.extendedFields(parsed) {
		if known[k] {
			continue
		}
		warnNovelField(resource, k, v, raw.ID.String())
	}
}

// warnNovelField emits the one-time warning for a newly seen unmapped field.
func warnNovelField(resource rawoutput.Resource, field string, value any, rawID string) {
	dedupeKey := string(resource) + "." + field

	driftMu.Lock()
	if warnedDrift[dedupeKey] {
		driftMu.Unlock()
		return
	}
	warnedDrift[dedupeKey] = true
	driftMu.Unlock()

	valStr := renderDriftValue(value)
	ver := version.Info()
	title := fmt.Sprintf("New unmapped field: %s.%s", resource, field)
	body := fmt.Sprintf(
		"The sync processor saw a field in the %s feed that is not mapped to a "+
			"typed column and fell through to extended_fields.\n\n"+
			"- Field: %s\n- Example value: %s\n- Sample raw_output id: %s\n- Version: %s\n\n"+
			"Paste the surrounding processor logs below:\n\n```\n\n```\n",
		resource, field, valStr, rawID, ver)

	applog.Warnf(
		"field-drift: new unmapped field %s.%s (not in typed schema) — value: %s, "+
			"sample raw_output=%s, version: %s. If a newer release is available, "+
			"upgrade — it may already map this field. File an issue (attach the logs "+
			"above): %s",
		resource, field, valStr, rawID, ver, version.NewIssueURL(title, body))
}

// renderDriftValue renders an extended-field value as compact JSON for the log
// line, truncating very long values so a fat array/object can't flood output.
func renderDriftValue(value any) string {
	b, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	const max = 200
	if len(b) > max {
		return string(b[:max]) + "…(truncated)"
	}
	return string(b)
}
