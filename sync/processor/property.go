package processor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/google/uuid"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/attachmentjob"
	entmedia "github.com/LunarHUE/MLS-Grid-Sync/ent/media"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/property"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/propertyversion"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/applog"
	"github.com/LunarHUE/MLS-Grid-Sync/version"
)

// PropertyProcessor handles the Property resource: parse the raw_output
// payload, diff against the current version, and write a new version row +
// upsert the entity.
type PropertyProcessor struct{}

func NewPropertyProcessor() *PropertyProcessor { return &PropertyProcessor{} }

func (*PropertyProcessor) Resource() rawoutput.Resource { return rawoutput.ResourceProperty }

func (p *PropertyProcessor) Process(ctx context.Context, tx *ent.Tx, raw *ent.RawOutput) (Outcome, error) {
	// raw_output.payload is stored as raw JSON bytes (json.RawMessage); the
	// parser consumes them directly — no decode-to-map then re-marshal.
	fields, err := parseProperty(raw.Payload)
	if err != nil {
		return OutcomeUnknown, fmt.Errorf("parse: %w", err)
	}

	entityExists, currentMlgCanView, err := p.lookupEntity(ctx, tx, fields.ListingKey)
	if err != nil {
		return OutcomeUnknown, err
	}

	// The current open version (valid_to IS NULL) is the diff target — diffing
	// against it (not the raw payload) means repeated reads of the same MLS
	// state don't produce phantom diffs.
	var currentVersion *ent.PropertyVersion
	if entityExists {
		currentVersion, err = p.lookupCurrentVersion(ctx, tx, fields.ListingKey)
		if err != nil {
			return OutcomeUnknown, err
		}
	}

	now := time.Now().UTC()
	plan := decideProperty(fields, entityExists, currentMlgCanView, currentVersion, raw)
	if err := p.applyPlan(ctx, tx, fields, raw, currentVersion, plan, now); err != nil {
		return OutcomeUnknown, err
	}
	return plan.outcome, nil
}

// lookupEntity reads only the columns the control flow needs — existence +
// mlg_can_view (the tombstone-skip flag). The full entity row (34 text[] arrays
// + 2 JSONB blobs) is never scanned here; the diff target is the version row.
func (p *PropertyProcessor) lookupEntity(ctx context.Context, tx *ent.Tx, listingKey string) (exists, mlgCanView bool, err error) {
	var canView []bool
	if err := tx.Property.Query().
		Where(property.IDEQ(listingKey)).
		Select(property.FieldMlgCanView).
		Scan(ctx, &canView); err != nil {
		return false, false, fmt.Errorf("lookup property: %w", err)
	}
	return len(canView) > 0, len(canView) > 0 && canView[0], nil
}

// lookupCurrentVersion returns the open (valid_to IS NULL) version for a
// listing_key, or nil if none. The partial unique index guarantees at most one.
func (p *PropertyProcessor) lookupCurrentVersion(ctx context.Context, tx *ent.Tx, listingKey string) (*ent.PropertyVersion, error) {
	v, err := tx.PropertyVersion.Query().
		Where(
			propertyversion.ListingKey(listingKey),
			propertyversion.ValidToIsNil(),
		).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("lookup current version: %w", err)
	}
	return v, nil
}

// propertyAction is the write shape a decided record needs. It lets the
// per-record (applyPlan) and bulk (ProcessChunk) paths share one decision.
type propertyAction int

const (
	actSkip                propertyAction = iota // no write
	actInsert                                    // new version + new entity
	actUpdate                                    // close prior version, new version, upsert entity (full)
	actDeleteExisting                            // close prior version, delete version, tombstone existing entity
	actDeleteFirstSighting                       // delete version + insert tombstoned entity
)

// propertyPlan is the decision for one record: its outcome plus, for writing
// outcomes, the change_type / diff / prior-open-version-to-close.
type propertyPlan struct {
	action         propertyAction
	outcome        Outcome
	changeType     propertyversion.ChangeType
	diff           map[string]any // changed_fields, update only
	closeVersionID *string        // prior open version to close (update / delete-existing)
}

// decideProperty is the pure decision: given the parsed fields and the current
// entity/version state, return the outcome and the writes it implies. Shared by
// Process (per-record) and ProcessChunk (bulk) so the two paths cannot diverge.
// It performs no I/O.
func decideProperty(f *PropertyFields, entityExists, currentMlgCanView bool, currentVersion *ent.PropertyVersion, raw *ent.RawOutput) propertyPlan {
	// Stale: a raw_output no newer than what we already wrote is a replay.
	if currentVersion != nil && !raw.SourceModifiedAt.After(currentVersion.SourceModifiedAt) {
		return propertyPlan{action: actSkip, outcome: OutcomeSkipStale}
	}

	// MlgCanView == false → delete branch.
	if !f.MlgCanView {
		// Already tombstoned: re-arriving delete for an already-invisible entity
		// is a no-op (don't pollute the audit trail with repeated deletes).
		if entityExists && !currentMlgCanView {
			return propertyPlan{action: actSkip, outcome: OutcomeSkipTombstoned}
		}
		plan := propertyPlan{outcome: OutcomeDelete, changeType: propertyversion.ChangeTypeDelete}
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

	// First-time sighting (no entity) → insert.
	if !entityExists {
		return propertyPlan{action: actInsert, outcome: OutcomeInsert, changeType: propertyversion.ChangeTypeInsert}
	}

	// Existing entity: diff against the open version. Empty diff → skip.
	diff := diffPropertyFields(currentVersion, f)
	if len(diff) == 0 {
		return propertyPlan{action: actSkip, outcome: OutcomeSkipNoDiff}
	}
	plan := propertyPlan{action: actUpdate, outcome: OutcomeUpdate, changeType: propertyversion.ChangeTypeUpdate, diff: diff}
	if currentVersion != nil {
		id := currentVersion.ID
		plan.closeVersionID = &id
	}
	return plan
}

// applyPlan executes a decided plan via the per-record apply helpers. The bulk
// path (ProcessChunk) executes the same plans in batched form instead.
func (p *PropertyProcessor) applyPlan(ctx context.Context, tx *ent.Tx, f *PropertyFields, raw *ent.RawOutput, currentVersion *ent.PropertyVersion, plan propertyPlan, now time.Time) error {
	switch plan.action {
	case actSkip:
		return nil
	case actInsert:
		return p.applyInsert(ctx, tx, f, raw, now)
	case actUpdate:
		return p.applyUpdate(ctx, tx, f, raw, currentVersion, plan.diff, now)
	case actDeleteExisting:
		return p.applyDelete(ctx, tx, f, raw, currentVersion, true, now)
	case actDeleteFirstSighting:
		return p.applyDelete(ctx, tx, f, raw, currentVersion, false, now)
	}
	return nil
}

// cancelPendingAttachmentJobs flips status to 'canceled' (and clears
// claim state) for pending/retrying/in_progress attachment_jobs whose
// media row is a property-typed photo of listingKey. Runs inside the
// tombstone tx so the cascade lands atomically with the entity-level
// tombstone.
//
// Phase 4 §3 widens the StatusIn set to include in_progress. The Phase
// 4 §2 CAS guard on the worker's success write makes this safe: if a
// worker is mid-download when the cascade cancels its job, the worker's
// terminal UPDATE matches zero rows and yields. The attachment row (if
// already created) is sha256-content-addressed and harmless to keep.
func cancelPendingAttachmentJobs(ctx context.Context, tx *ent.Tx, listingKey string) error {
	mediaIDs, err := tx.Media.Query().
		Where(
			entmedia.ResourceTypeEQ(entmedia.ResourceTypeProperty),
			entmedia.ResourceRecordKeyEQ(listingKey),
		).
		IDs(ctx)
	if err != nil {
		return fmt.Errorf("collect media ids: %w", err)
	}
	if len(mediaIDs) == 0 {
		return nil
	}
	_, err = tx.AttachmentJob.Update().
		Where(
			attachmentjob.MediaKeyIn(mediaIDs...),
			attachmentjob.StatusIn(
				attachmentjob.StatusPending,
				attachmentjob.StatusRetrying,
				attachmentjob.StatusInProgress,
			),
		).
		SetStatus(attachmentjob.StatusCanceled).
		ClearClaimedAt().
		ClearClaimedBy().
		Save(ctx)
	if err != nil {
		return fmt.Errorf("update attachment_jobs: %w", err)
	}
	return nil
}

// AfterPass re-links child entities (property_room, property_unit_type,
// open_house) whose parent_listing_key was NULL because the parent Property
// hadn't been processed at the time the child was written. Each UPDATE
// promotes `parent_listing_key = listing_key` when a matching Property now
// exists. Idempotent.
//
// Runs in its own tx after the Property batch loop drains (inside the
// per-resource advisory lock), so multiple passes don't double-link and a
// re-link failure doesn't unwind already-committed per-record cursor
// advances.
func (p *PropertyProcessor) AfterPass(ctx context.Context, db *sql.DB) error {
	queries := []struct {
		name string
		sql  string
	}{
		{
			"property_room",
			`UPDATE property_room AS r
			    SET parent_listing_key = r.listing_key
			  WHERE r.parent_listing_key IS NULL
			    AND EXISTS (SELECT 1 FROM property p WHERE p.listing_key = r.listing_key)`,
		},
		{
			"property_unit_type",
			`UPDATE property_unit_type AS u
			    SET parent_listing_key = u.listing_key
			  WHERE u.parent_listing_key IS NULL
			    AND EXISTS (SELECT 1 FROM property p WHERE p.listing_key = u.listing_key)`,
		},
		{
			"open_house",
			`UPDATE open_house AS o
			    SET parent_listing_key = o.listing_key
			  WHERE o.parent_listing_key IS NULL
			    AND EXISTS (SELECT 1 FROM property p WHERE p.listing_key = o.listing_key)`,
		},
	}
	counts := make(map[string]int64, len(queries))
	for _, q := range queries {
		res, err := db.ExecContext(ctx, q.sql)
		if err != nil {
			return fmt.Errorf("re-link %s: %w", q.name, err)
		}
		n, _ := res.RowsAffected()
		counts[q.name] = n
	}
	// Zero rows is information — it confirms the re-link ran and nothing
	// was parked. Always log.
	applog.Infof("processor[property]: re-link — %d property_rooms, %d property_unit_types, %d open_houses",
		counts["property_room"], counts["property_unit_type"], counts["open_house"])
	return nil
}

// --- branches ---

func (p *PropertyProcessor) applyInsert(ctx context.Context, tx *ent.Tx, f *PropertyFields, raw *ent.RawOutput, now time.Time) error {
	ver, err := newPropertyVersionCreate(tx, f, raw, propertyversion.ChangeTypeInsert, now, nil)
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

	c := tx.Property.Create().
		SetID(f.ListingKey).
		SetCurrentVersionID(verID)
	applyToPropertyCreate(c, f)
	if _, err := c.Save(ctx); err != nil {
		return fmt.Errorf("create entity: %w", err)
	}
	return nil
}

func (p *PropertyProcessor) applyUpdate(ctx context.Context, tx *ent.Tx, f *PropertyFields, raw *ent.RawOutput, currentVersion *ent.PropertyVersion, diff map[string]any, now time.Time) error {
	// Close the prior open version.
	if currentVersion != nil {
		if _, err := tx.PropertyVersion.UpdateOneID(currentVersion.ID).
			SetValidTo(now).
			Save(ctx); err != nil {
			return fmt.Errorf("close current version: %w", err)
		}
	}

	// Insert new open version with the diff.
	ver, err := newPropertyVersionCreate(tx, f, raw, propertyversion.ChangeTypeUpdate, now, diff)
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

	// Update the entity in place (id == f.ListingKey, the key we looked up by).
	u := tx.Property.UpdateOneID(f.ListingKey).SetCurrentVersionID(verID)
	applyToPropertyUpdate(u, f)
	if _, err := u.Save(ctx); err != nil {
		return fmt.Errorf("update entity: %w", err)
	}
	return nil
}

// applyDelete deliberately preserves the entity's prior field values — only
// mlg_can_view and current_version_id change on tombstone. The new delete
// version row, by contrast, is built from the sparse MlgCanView=false
// payload (mostly NULL). So a tombstoned entity and its current (delete)
// version legitimately disagree on field values: entity preserves
// last-known state, version records the delete event.
//
// This is the one place the "entity mirrors current version" invariant
// that the Bug-1 regression tests enforce intentionally breaks. Any
// future drift-audit query (entity.col vs current_version.col) must
// exclude tombstoned entities (mlg_can_view = false) or it will flag
// every delete as drift.
func (p *PropertyProcessor) applyDelete(ctx context.Context, tx *ent.Tx, f *PropertyFields, raw *ent.RawOutput, currentVersion *ent.PropertyVersion, entityExists bool, now time.Time) error {
	if currentVersion != nil {
		if _, err := tx.PropertyVersion.UpdateOneID(currentVersion.ID).
			SetValidTo(now).
			Save(ctx); err != nil {
			return fmt.Errorf("close current version: %w", err)
		}
	}

	ver, err := newPropertyVersionCreate(tx, f, raw, propertyversion.ChangeTypeDelete, now, nil)
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
		// First sighting and already invisible — create a tombstoned entity so
		// the audit trail (the delete version row) has somewhere to point.
		c := tx.Property.Create().
			SetID(f.ListingKey).
			SetCurrentVersionID(verID)
		applyToPropertyCreate(c, f) // sets MlgCanView=false from f.MlgCanView
		if _, err := c.Save(ctx); err != nil {
			return fmt.Errorf("create tombstoned entity: %w", err)
		}
		// Cascade: cancel pending/retrying attachment_jobs even on
		// first-sighting tombstones — harmless no-op when no Media row
		// references this listing yet, but covers the case where a Media
		// delta landed first.
		if err := cancelPendingAttachmentJobs(ctx, tx, f.ListingKey); err != nil {
			return fmt.Errorf("cascade jobs: %w", err)
		}
		return nil
	}

	u := tx.Property.UpdateOneID(f.ListingKey).
		SetCurrentVersionID(verID).
		SetMlgCanView(false)
	if _, err := u.Save(ctx); err != nil {
		return fmt.Errorf("tombstone entity: %w", err)
	}
	if err := cancelPendingAttachmentJobs(ctx, tx, f.ListingKey); err != nil {
		return fmt.Errorf("cascade jobs: %w", err)
	}
	return nil
}

// --- version builder ---

// newPropertyVersionCreate constructs the PropertyVersion create builder with
// version metadata + all data fields applied. raw_output_id is set when the
// raw row is available (which it always is from Process — but the schema
// declares the field optional for manual fixes).
func newPropertyVersionCreate(tx *ent.Tx, f *PropertyFields, raw *ent.RawOutput, ct propertyversion.ChangeType, now time.Time, diff map[string]any) (*ent.PropertyVersionCreate, error) {
	if raw == nil {
		return nil, errors.New("raw_output required")
	}
	c := tx.PropertyVersion.Create().
		SetListingKey(f.ListingKey).
		SetSyncEventID(raw.SyncEventID).
		SetRawOutputID(raw.ID).
		SetValidFrom(now).
		SetChangeType(ct).
		SetProcessorVersion(version.Info())
	if diff != nil {
		c.SetChangedFields(diff)
	}
	applyToPropertyVersionCreate(c, f)
	return c, nil
}

// --- diff ---

// diffPropertyFields compares the current open version's data to the new
// parsed fields and returns a {field: {"old":..., "new":...}} map. Only data
// fields are diffed; entity-level fields (ListingKey, current_version_id) and
// version-level fields (valid_from, valid_to, change_type, ...) are excluded.
//
// Diffs typed values rather than raw JSON to avoid phantom diffs from JSONB
// key reordering or numeric formatting (1.0 vs 1.00).
func diffPropertyFields(currentVersion *ent.PropertyVersion, next *PropertyFields) map[string]any {
	if currentVersion == nil {
		return nil
	}
	diff := map[string]any{}

	// Small helper to push a diff entry.
	push := func(name string, oldV, newV any) {
		diff[name] = map[string]any{"old": oldV, "new": newV}
	}

	// MLSMetadataMixin.
	// source_modified_at is intentionally excluded — it always advances on a
	// new sync but doesn't reflect actual MLS content changes. The stale
	// check upstream already guarantees we only consider newer-or-equal raws.
	if !equalStringPtr(currentVersion.OriginatingSystemName, next.OriginatingSystemName) {
		push("originating_system_name", currentVersion.OriginatingSystemName, next.OriginatingSystemName)
	}
	if currentVersion.MlgCanView != next.MlgCanView {
		push("mlg_can_view", currentVersion.MlgCanView, next.MlgCanView)
	}
	if !equalStringSlice(currentVersion.MlgCanUse, next.MlgCanUse) {
		push("mlg_can_use", currentVersion.MlgCanUse, next.MlgCanUse)
	}

	// Use reflection over the remaining string/int/bool/decimal/time pointer
	// fields. Both currentVersion and next have parallel field names for the
	// shared columns — easier than another 150-line table.
	diffReflect(currentVersion, next, diff)

	// Diff text[] arrays.
	diffArrays(currentVersion, next, diff)

	// extended_fields — diff structurally.
	if !reflect.DeepEqual(currentVersion.ExtendedFields, next.ExtendedFields) {
		push("extended_fields", currentVersion.ExtendedFields, next.ExtendedFields)
	}

	if len(diff) == 0 {
		return nil
	}
	return diff
}

// diffReflect handles scalar pointer fields shared between PropertyVersion and
// PropertyFields. Iterates fields on PropertyFields and uses each name to
// look up the matching field on PropertyVersion. Skips anything not present
// on PropertyVersion (e.g. ListingKey, ExtendedFields handled above) or non-
// pointer kinds (arrays handled separately).
func diffReflect(currentVersion *ent.PropertyVersion, next *PropertyFields, out map[string]any) {
	vNext := reflect.ValueOf(next).Elem()
	tNext := vNext.Type()
	vCur := reflect.ValueOf(currentVersion).Elem()
	tCur := vCur.Type()

	skip := map[string]bool{
		"ListingKey":            true, // entity identity
		"SourceModifiedAt":      true, // handled above
		"OriginatingSystemName": true, // handled above
		"MlgCanView":            true, // handled above
		"MlgCanUse":             true, // handled above
		"ExtendedFields":        true, // handled above
	}

	for i := 0; i < tNext.NumField(); i++ {
		name := tNext.Field(i).Name
		if skip[name] {
			continue
		}
		fNext := vNext.Field(i)
		if fNext.Kind() != reflect.Ptr {
			continue // arrays handled in diffArrays
		}
		curField, ok := tCur.FieldByName(name)
		_ = curField
		if !ok {
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

func diffArrays(currentVersion *ent.PropertyVersion, next *PropertyFields, out map[string]any) {
	vNext := reflect.ValueOf(next).Elem()
	tNext := vNext.Type()
	vCur := reflect.ValueOf(currentVersion).Elem()
	tCur := vCur.Type()
	for i := 0; i < tNext.NumField(); i++ {
		name := tNext.Field(i).Name
		fNext := vNext.Field(i)
		if fNext.Kind() != reflect.Slice {
			continue
		}
		curField, ok := tCur.FieldByName(name)
		_ = curField
		if !ok {
			continue
		}
		fCur := vCur.FieldByName(name)
		if fCur.Kind() != reflect.Slice {
			continue
		}
		curSlice := toStringSlice(fCur)
		nextSlice := toStringSlice(fNext)
		if !equalStringSlice(curSlice, nextSlice) {
			out[snakeCase(name)] = map[string]any{"old": curSlice, "new": nextSlice}
		}
	}
}

// --- equality helpers ---

func equalStringPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalPtr(a, b reflect.Value) bool {
	if a.IsNil() || b.IsNil() {
		return a.IsNil() == b.IsNil()
	}
	return reflect.DeepEqual(a.Elem().Interface(), b.Elem().Interface())
}

func ptrToAny(v reflect.Value) any {
	if v.IsNil() {
		return nil
	}
	return v.Elem().Interface()
}

func toStringSlice(v reflect.Value) []string {
	if v.IsNil() {
		return nil
	}
	out := make([]string, v.Len())
	for i := 0; i < v.Len(); i++ {
		out[i] = v.Index(i).String()
	}
	return out
}

// snakeCase converts an UpperCamelCase Go field name into snake_case for the
// changed_fields JSONB keys. Mirrors the schema's column naming.
func snakeCase(s string) string {
	var out []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			if i > 0 {
				prev := s[i-1]
				if prev >= 'a' && prev <= 'z' {
					out = append(out, '_')
				} else if prev >= 'A' && prev <= 'Z' && i+1 < len(s) {
					next := s[i+1]
					if next >= 'a' && next <= 'z' {
						out = append(out, '_')
					}
				}
			}
			out = append(out, c+('a'-'A'))
		} else {
			out = append(out, c)
		}
	}
	return string(out)
}

// --- apply: PropertyFields → ent builders ---
//
// Three near-identical copies for *ent.PropertyCreate, *ent.PropertyUpdateOne,
// and *ent.PropertyVersionCreate. The duplication is mechanical and obvious —
// any drift between them shows up immediately in the integration round-trip
// test, which asserts that the entity row and the version row carry the same
// data after a write.
//
// Phase 2 policy: a nil pointer in PropertyFields means "no value supplied in
// the payload" — we use SetNillableX, which is a no-op when nil. Explicit
// "clear this field" semantics (RESO sends null to reset a value) are out of
// scope for Phase 2.

func applyToPropertyCreate(c *ent.PropertyCreate, f *PropertyFields) {
	c.SetSourceModifiedAt(f.SourceModifiedAt).
		SetMlgCanView(f.MlgCanView).
		SetNillableOriginatingSystemName(f.OriginatingSystemName)
	if f.MlgCanUse != nil {
		c.SetMlgCanUse(f.MlgCanUse)
	}
	c.SetNillableListingID(f.ListingID).
		SetNillableParcelNumber(f.ParcelNumber).
		SetNillableMlsStatus(f.MlsStatus).
		SetNillableStandardStatus(f.StandardStatus).
		SetNillableMajorChangeType(f.MajorChangeType).
		SetNillableMajorChangeTimestamp(f.MajorChangeTimestamp).
		SetNillableListingContractDate(f.ListingContractDate).
		SetNillableOnMarketTimestamp(f.OnMarketTimestamp).
		SetNillableOriginalEntryTimestamp(f.OriginalEntryTimestamp).
		SetNillablePhotosChangeTimestamp(f.PhotosChangeTimestamp).
		SetNillableAvailabilityDate(f.AvailabilityDate).
		SetNillableListPrice(f.ListPrice).
		SetNillableOriginalListPrice(f.OriginalListPrice).
		SetNillablePreviousListPrice(f.PreviousListPrice).
		SetNillableTaxAnnualAmount(f.TaxAnnualAmount).
		SetNillableTaxAssessedValue(f.TaxAssessedValue).
		SetNillableTaxYear(f.TaxYear).
		SetNillablePropertyType(f.PropertyType).
		SetNillablePropertySubType(f.PropertySubType).
		SetNillableNewConstructionYn(f.NewConstructionYn).
		SetNillableBedroomsTotal(f.BedroomsTotal).
		SetNillableBathroomsTotalInteger(f.BathroomsTotalInteger).
		SetNillableBathroomsFull(f.BathroomsFull).
		SetNillableBathroomsHalf(f.BathroomsHalf).
		SetNillableMainLevelBedrooms(f.MainLevelBedrooms).
		SetNillableLivingArea(f.LivingArea).
		SetNillableBuildingAreaTotal(f.BuildingAreaTotal).
		SetNillableLotSizeAcres(f.LotSizeAcres).
		SetNillableLotSizeSquareFeet(f.LotSizeSquareFeet).
		SetNillableStoriesTotal(f.StoriesTotal).
		SetNillableYearBuilt(f.YearBuilt).
		SetNillableGarageSpaces(f.GarageSpaces).
		SetNillableCoveredSpaces(f.CoveredSpaces).
		SetNillableParkingTotal(f.ParkingTotal).
		SetNillableFireplacesTotal(f.FireplacesTotal).
		SetNillablePoolPrivateYn(f.PoolPrivateYn).
		SetNillableWaterfrontYn(f.WaterfrontYn).
		SetNillableViewYn(f.ViewYn).
		SetNillableHorseYn(f.HorseYn).
		SetNillableStreetNumber(f.StreetNumber).
		SetNillableStreetNumberNumeric(f.StreetNumberNumeric).
		SetNillableStreetName(f.StreetName).
		SetNillableStreetSuffix(f.StreetSuffix).
		SetNillableStreetDirPrefix(f.StreetDirPrefix).
		SetNillableStreetDirSuffix(f.StreetDirSuffix).
		SetNillableUnitNumber(f.UnitNumber).
		SetNillableUnparsedAddress(f.UnparsedAddress).
		SetNillableCity(f.City).
		SetNillableStateOrProvince(f.StateOrProvince).
		SetNillablePostalCode(f.PostalCode).
		SetNillablePostalCodePlus4(f.PostalCodePlus4).
		SetNillableCountry(f.Country).
		SetNillableCountyOrParish(f.CountyOrParish).
		SetNillableSubdivisionName(f.SubdivisionName).
		SetNillableMlsAreaMajor(f.MlsAreaMajor).
		SetNillableLatitude(f.Latitude).
		SetNillableLongitude(f.Longitude).
		SetNillableElementarySchool(f.ElementarySchool).
		SetNillableMiddleOrJuniorSchool(f.MiddleOrJuniorSchool).
		SetNillableHighSchool(f.HighSchool).
		SetNillableHighSchoolDistrict(f.HighSchoolDistrict).
		SetNillableListAgentKey(f.ListAgentKey).
		SetNillableListAgentMlsID(f.ListAgentMlsID).
		SetNillableCoListAgentKey(f.CoListAgentKey).
		SetNillableCoListAgentMlsID(f.CoListAgentMlsID).
		SetNillableBuyerAgentKey(f.BuyerAgentKey).
		SetNillableBuyerAgentMlsID(f.BuyerAgentMlsID).
		SetNillableCoBuyerAgentKey(f.CoBuyerAgentKey).
		SetNillableCoBuyerAgentMlsID(f.CoBuyerAgentMlsID).
		SetNillableListOfficeKey(f.ListOfficeKey).
		SetNillableListOfficeMlsID(f.ListOfficeMlsID).
		SetNillableCoListOfficeKey(f.CoListOfficeKey).
		SetNillableCoListOfficeMlsID(f.CoListOfficeMlsID).
		SetNillableBuyerOfficeKey(f.BuyerOfficeKey).
		SetNillableBuyerOfficeMlsID(f.BuyerOfficeMlsID).
		SetNillableCoBuyerOfficeKey(f.CoBuyerOfficeKey).
		SetNillableCoBuyerOfficeMlsID(f.CoBuyerOfficeMlsID).
		SetNillableInternetEntireListingDisplayYn(f.InternetEntireListingDisplayYn).
		SetNillableInternetAddressDisplayYn(f.InternetAddressDisplayYn).
		SetNillableInternetAutomatedValuationDisplayYn(f.InternetAutomatedValuationDisplayYn).
		SetNillableInternetConsumerCommentYn(f.InternetConsumerCommentYn).
		SetNillablePublicRemarks(f.PublicRemarks).
		SetNillableSyndicationRemarks(f.SyndicationRemarks).
		SetNillableDirections(f.Directions).
		SetNillableFurnished(f.Furnished).
		SetNillableDirectionFaces(f.DirectionFaces)
	if f.Appliances != nil {
		c.SetAppliances(f.Appliances)
	}
	if f.Cooling != nil {
		c.SetCooling(f.Cooling)
	}
	if f.Heating != nil {
		c.SetHeating(f.Heating)
	}
	if f.Flooring != nil {
		c.SetFlooring(f.Flooring)
	}
	if f.Roof != nil {
		c.SetRoof(f.Roof)
	}
	if f.ExteriorFeatures != nil {
		c.SetExteriorFeatures(f.ExteriorFeatures)
	}
	if f.InteriorFeatures != nil {
		c.SetInteriorFeatures(f.InteriorFeatures)
	}
	if f.ParkingFeatures != nil {
		c.SetParkingFeatures(f.ParkingFeatures)
	}
	if f.PoolFeatures != nil {
		c.SetPoolFeatures(f.PoolFeatures)
	}
	if f.View != nil {
		c.SetView(f.View)
	}
	if f.WaterfrontFeatures != nil {
		c.SetWaterfrontFeatures(f.WaterfrontFeatures)
	}
	if f.CommunityFeatures != nil {
		c.SetCommunityFeatures(f.CommunityFeatures)
	}
	if f.AccessibilityFeatures != nil {
		c.SetAccessibilityFeatures(f.AccessibilityFeatures)
	}
	if f.Utilities != nil {
		c.SetUtilities(f.Utilities)
	}
	if f.Sewer != nil {
		c.SetSewer(f.Sewer)
	}
	if f.WaterSource != nil {
		c.SetWaterSource(f.WaterSource)
	}
	if f.LotFeatures != nil {
		c.SetLotFeatures(f.LotFeatures)
	}
	if f.PatioAndPorchFeatures != nil {
		c.SetPatioAndPorchFeatures(f.PatioAndPorchFeatures)
	}
	if f.SecurityFeatures != nil {
		c.SetSecurityFeatures(f.SecurityFeatures)
	}
	if f.ConstructionMaterials != nil {
		c.SetConstructionMaterials(f.ConstructionMaterials)
	}
	if f.FoundationDetails != nil {
		c.SetFoundationDetails(f.FoundationDetails)
	}
	if f.Levels != nil {
		c.SetLevels(f.Levels)
	}
	if f.FireplaceFeatures != nil {
		c.SetFireplaceFeatures(f.FireplaceFeatures)
	}
	if f.SpaFeatures != nil {
		c.SetSpaFeatures(f.SpaFeatures)
	}
	if f.Fencing != nil {
		c.SetFencing(f.Fencing)
	}
	if f.HorseAmenities != nil {
		c.SetHorseAmenities(f.HorseAmenities)
	}
	if f.WindowFeatures != nil {
		c.SetWindowFeatures(f.WindowFeatures)
	}
	if f.PetsAllowed != nil {
		c.SetPetsAllowed(f.PetsAllowed)
	}
	if f.Disclosures != nil {
		c.SetDisclosures(f.Disclosures)
	}
	if f.PropertyCondition != nil {
		c.SetPropertyCondition(f.PropertyCondition)
	}
	if f.SpecialListingConditions != nil {
		c.SetSpecialListingConditions(f.SpecialListingConditions)
	}
	if f.GreenEnergyEfficient != nil {
		c.SetGreenEnergyEfficient(f.GreenEnergyEfficient)
	}
	if f.GreenSustainability != nil {
		c.SetGreenSustainability(f.GreenSustainability)
	}
	if f.SyndicateTo != nil {
		c.SetSyndicateTo(f.SyndicateTo)
	}
	if f.ExtendedFields != nil {
		c.SetExtendedFields(f.ExtendedFields)
	}
}

// applyToPropertyUpdate uses clear-on-nil semantics — see apply.go for why.
// Phase 2 originally used SetNillableX(nil) which is a no-op in ent and left
// the entity row stale when a field cleared upstream, while the version row
// (built fresh from PropertyFields → NULL) correctly recorded the clear —
// causing entity/version drift. Audited and fixed during Phase 3 §4 review.
func applyToPropertyUpdate(c *ent.PropertyUpdateOne, f *PropertyFields) {
	c.SetSourceModifiedAt(f.SourceModifiedAt).SetMlgCanView(f.MlgCanView)
	setOrClearStr(f.OriginatingSystemName, c.SetOriginatingSystemName, c.ClearOriginatingSystemName)
	setOrClearSlice(f.MlgCanUse, c.SetMlgCanUse, c.ClearMlgCanUse)

	setOrClearStr(f.ListingID, c.SetListingID, c.ClearListingID)
	setOrClearStr(f.ParcelNumber, c.SetParcelNumber, c.ClearParcelNumber)
	setOrClearStr(f.MlsStatus, c.SetMlsStatus, c.ClearMlsStatus)
	setOrClearStr(f.StandardStatus, c.SetStandardStatus, c.ClearStandardStatus)
	setOrClearStr(f.MajorChangeType, c.SetMajorChangeType, c.ClearMajorChangeType)
	setOrClearTime(f.MajorChangeTimestamp, c.SetMajorChangeTimestamp, c.ClearMajorChangeTimestamp)
	setOrClearTime(f.ListingContractDate, c.SetListingContractDate, c.ClearListingContractDate)
	setOrClearTime(f.OnMarketTimestamp, c.SetOnMarketTimestamp, c.ClearOnMarketTimestamp)
	setOrClearTime(f.OriginalEntryTimestamp, c.SetOriginalEntryTimestamp, c.ClearOriginalEntryTimestamp)
	setOrClearTime(f.PhotosChangeTimestamp, c.SetPhotosChangeTimestamp, c.ClearPhotosChangeTimestamp)
	setOrClearTime(f.AvailabilityDate, c.SetAvailabilityDate, c.ClearAvailabilityDate)
	setOrClearDecimal(f.ListPrice, c.SetListPrice, c.ClearListPrice)
	setOrClearDecimal(f.OriginalListPrice, c.SetOriginalListPrice, c.ClearOriginalListPrice)
	setOrClearDecimal(f.PreviousListPrice, c.SetPreviousListPrice, c.ClearPreviousListPrice)
	setOrClearDecimal(f.TaxAnnualAmount, c.SetTaxAnnualAmount, c.ClearTaxAnnualAmount)
	setOrClearInt64(f.TaxAssessedValue, c.SetTaxAssessedValue, c.ClearTaxAssessedValue)
	setOrClearInt16(f.TaxYear, c.SetTaxYear, c.ClearTaxYear)
	setOrClearStr(f.PropertyType, c.SetPropertyType, c.ClearPropertyType)
	setOrClearStr(f.PropertySubType, c.SetPropertySubType, c.ClearPropertySubType)
	setOrClearBool(f.NewConstructionYn, c.SetNewConstructionYn, c.ClearNewConstructionYn)
	setOrClearInt16(f.BedroomsTotal, c.SetBedroomsTotal, c.ClearBedroomsTotal)
	setOrClearInt16(f.BathroomsTotalInteger, c.SetBathroomsTotalInteger, c.ClearBathroomsTotalInteger)
	setOrClearInt16(f.BathroomsFull, c.SetBathroomsFull, c.ClearBathroomsFull)
	setOrClearInt16(f.BathroomsHalf, c.SetBathroomsHalf, c.ClearBathroomsHalf)
	setOrClearInt16(f.MainLevelBedrooms, c.SetMainLevelBedrooms, c.ClearMainLevelBedrooms)
	setOrClearDecimal(f.LivingArea, c.SetLivingArea, c.ClearLivingArea)
	setOrClearDecimal(f.BuildingAreaTotal, c.SetBuildingAreaTotal, c.ClearBuildingAreaTotal)
	setOrClearDecimal(f.LotSizeAcres, c.SetLotSizeAcres, c.ClearLotSizeAcres)
	setOrClearDecimal(f.LotSizeSquareFeet, c.SetLotSizeSquareFeet, c.ClearLotSizeSquareFeet)
	setOrClearInt16(f.StoriesTotal, c.SetStoriesTotal, c.ClearStoriesTotal)
	setOrClearInt16(f.YearBuilt, c.SetYearBuilt, c.ClearYearBuilt)
	setOrClearDecimal(f.GarageSpaces, c.SetGarageSpaces, c.ClearGarageSpaces)
	setOrClearDecimal(f.CoveredSpaces, c.SetCoveredSpaces, c.ClearCoveredSpaces)
	setOrClearDecimal(f.ParkingTotal, c.SetParkingTotal, c.ClearParkingTotal)
	setOrClearInt16(f.FireplacesTotal, c.SetFireplacesTotal, c.ClearFireplacesTotal)
	setOrClearBool(f.PoolPrivateYn, c.SetPoolPrivateYn, c.ClearPoolPrivateYn)
	setOrClearBool(f.WaterfrontYn, c.SetWaterfrontYn, c.ClearWaterfrontYn)
	setOrClearBool(f.ViewYn, c.SetViewYn, c.ClearViewYn)
	setOrClearBool(f.HorseYn, c.SetHorseYn, c.ClearHorseYn)
	setOrClearStr(f.StreetNumber, c.SetStreetNumber, c.ClearStreetNumber)
	setOrClearInt32(f.StreetNumberNumeric, c.SetStreetNumberNumeric, c.ClearStreetNumberNumeric)
	setOrClearStr(f.StreetName, c.SetStreetName, c.ClearStreetName)
	setOrClearStr(f.StreetSuffix, c.SetStreetSuffix, c.ClearStreetSuffix)
	setOrClearStr(f.StreetDirPrefix, c.SetStreetDirPrefix, c.ClearStreetDirPrefix)
	setOrClearStr(f.StreetDirSuffix, c.SetStreetDirSuffix, c.ClearStreetDirSuffix)
	setOrClearStr(f.UnitNumber, c.SetUnitNumber, c.ClearUnitNumber)
	setOrClearStr(f.UnparsedAddress, c.SetUnparsedAddress, c.ClearUnparsedAddress)
	setOrClearStr(f.City, c.SetCity, c.ClearCity)
	setOrClearStr(f.StateOrProvince, c.SetStateOrProvince, c.ClearStateOrProvince)
	setOrClearStr(f.PostalCode, c.SetPostalCode, c.ClearPostalCode)
	setOrClearStr(f.PostalCodePlus4, c.SetPostalCodePlus4, c.ClearPostalCodePlus4)
	setOrClearStr(f.Country, c.SetCountry, c.ClearCountry)
	setOrClearStr(f.CountyOrParish, c.SetCountyOrParish, c.ClearCountyOrParish)
	setOrClearStr(f.SubdivisionName, c.SetSubdivisionName, c.ClearSubdivisionName)
	setOrClearStr(f.MlsAreaMajor, c.SetMlsAreaMajor, c.ClearMlsAreaMajor)
	setOrClearDecimal(f.Latitude, c.SetLatitude, c.ClearLatitude)
	setOrClearDecimal(f.Longitude, c.SetLongitude, c.ClearLongitude)
	setOrClearStr(f.ElementarySchool, c.SetElementarySchool, c.ClearElementarySchool)
	setOrClearStr(f.MiddleOrJuniorSchool, c.SetMiddleOrJuniorSchool, c.ClearMiddleOrJuniorSchool)
	setOrClearStr(f.HighSchool, c.SetHighSchool, c.ClearHighSchool)
	setOrClearStr(f.HighSchoolDistrict, c.SetHighSchoolDistrict, c.ClearHighSchoolDistrict)
	setOrClearStr(f.ListAgentKey, c.SetListAgentKey, c.ClearListAgentKey)
	setOrClearStr(f.ListAgentMlsID, c.SetListAgentMlsID, c.ClearListAgentMlsID)
	setOrClearStr(f.CoListAgentKey, c.SetCoListAgentKey, c.ClearCoListAgentKey)
	setOrClearStr(f.CoListAgentMlsID, c.SetCoListAgentMlsID, c.ClearCoListAgentMlsID)
	setOrClearStr(f.BuyerAgentKey, c.SetBuyerAgentKey, c.ClearBuyerAgentKey)
	setOrClearStr(f.BuyerAgentMlsID, c.SetBuyerAgentMlsID, c.ClearBuyerAgentMlsID)
	setOrClearStr(f.CoBuyerAgentKey, c.SetCoBuyerAgentKey, c.ClearCoBuyerAgentKey)
	setOrClearStr(f.CoBuyerAgentMlsID, c.SetCoBuyerAgentMlsID, c.ClearCoBuyerAgentMlsID)
	setOrClearStr(f.ListOfficeKey, c.SetListOfficeKey, c.ClearListOfficeKey)
	setOrClearStr(f.ListOfficeMlsID, c.SetListOfficeMlsID, c.ClearListOfficeMlsID)
	setOrClearStr(f.CoListOfficeKey, c.SetCoListOfficeKey, c.ClearCoListOfficeKey)
	setOrClearStr(f.CoListOfficeMlsID, c.SetCoListOfficeMlsID, c.ClearCoListOfficeMlsID)
	setOrClearStr(f.BuyerOfficeKey, c.SetBuyerOfficeKey, c.ClearBuyerOfficeKey)
	setOrClearStr(f.BuyerOfficeMlsID, c.SetBuyerOfficeMlsID, c.ClearBuyerOfficeMlsID)
	setOrClearStr(f.CoBuyerOfficeKey, c.SetCoBuyerOfficeKey, c.ClearCoBuyerOfficeKey)
	setOrClearStr(f.CoBuyerOfficeMlsID, c.SetCoBuyerOfficeMlsID, c.ClearCoBuyerOfficeMlsID)
	setOrClearBool(f.InternetEntireListingDisplayYn, c.SetInternetEntireListingDisplayYn, c.ClearInternetEntireListingDisplayYn)
	setOrClearBool(f.InternetAddressDisplayYn, c.SetInternetAddressDisplayYn, c.ClearInternetAddressDisplayYn)
	setOrClearBool(f.InternetAutomatedValuationDisplayYn, c.SetInternetAutomatedValuationDisplayYn, c.ClearInternetAutomatedValuationDisplayYn)
	setOrClearBool(f.InternetConsumerCommentYn, c.SetInternetConsumerCommentYn, c.ClearInternetConsumerCommentYn)
	setOrClearStr(f.PublicRemarks, c.SetPublicRemarks, c.ClearPublicRemarks)
	setOrClearStr(f.SyndicationRemarks, c.SetSyndicationRemarks, c.ClearSyndicationRemarks)
	setOrClearStr(f.Directions, c.SetDirections, c.ClearDirections)
	setOrClearStr(f.Furnished, c.SetFurnished, c.ClearFurnished)
	setOrClearStr(f.DirectionFaces, c.SetDirectionFaces, c.ClearDirectionFaces)

	setOrClearStringArray(f.Appliances, c.SetAppliances, c.ClearAppliances)
	setOrClearStringArray(f.Cooling, c.SetCooling, c.ClearCooling)
	setOrClearStringArray(f.Heating, c.SetHeating, c.ClearHeating)
	setOrClearStringArray(f.Flooring, c.SetFlooring, c.ClearFlooring)
	setOrClearStringArray(f.Roof, c.SetRoof, c.ClearRoof)
	setOrClearStringArray(f.ExteriorFeatures, c.SetExteriorFeatures, c.ClearExteriorFeatures)
	setOrClearStringArray(f.InteriorFeatures, c.SetInteriorFeatures, c.ClearInteriorFeatures)
	setOrClearStringArray(f.ParkingFeatures, c.SetParkingFeatures, c.ClearParkingFeatures)
	setOrClearStringArray(f.PoolFeatures, c.SetPoolFeatures, c.ClearPoolFeatures)
	setOrClearStringArray(f.View, c.SetView, c.ClearView)
	setOrClearStringArray(f.WaterfrontFeatures, c.SetWaterfrontFeatures, c.ClearWaterfrontFeatures)
	setOrClearStringArray(f.CommunityFeatures, c.SetCommunityFeatures, c.ClearCommunityFeatures)
	setOrClearStringArray(f.AccessibilityFeatures, c.SetAccessibilityFeatures, c.ClearAccessibilityFeatures)
	setOrClearStringArray(f.Utilities, c.SetUtilities, c.ClearUtilities)
	setOrClearStringArray(f.Sewer, c.SetSewer, c.ClearSewer)
	setOrClearStringArray(f.WaterSource, c.SetWaterSource, c.ClearWaterSource)
	setOrClearStringArray(f.LotFeatures, c.SetLotFeatures, c.ClearLotFeatures)
	setOrClearStringArray(f.PatioAndPorchFeatures, c.SetPatioAndPorchFeatures, c.ClearPatioAndPorchFeatures)
	setOrClearStringArray(f.SecurityFeatures, c.SetSecurityFeatures, c.ClearSecurityFeatures)
	setOrClearStringArray(f.ConstructionMaterials, c.SetConstructionMaterials, c.ClearConstructionMaterials)
	setOrClearStringArray(f.FoundationDetails, c.SetFoundationDetails, c.ClearFoundationDetails)
	setOrClearStringArray(f.Levels, c.SetLevels, c.ClearLevels)
	setOrClearStringArray(f.FireplaceFeatures, c.SetFireplaceFeatures, c.ClearFireplaceFeatures)
	setOrClearStringArray(f.SpaFeatures, c.SetSpaFeatures, c.ClearSpaFeatures)
	setOrClearStringArray(f.Fencing, c.SetFencing, c.ClearFencing)
	setOrClearStringArray(f.HorseAmenities, c.SetHorseAmenities, c.ClearHorseAmenities)
	setOrClearStringArray(f.WindowFeatures, c.SetWindowFeatures, c.ClearWindowFeatures)
	setOrClearStringArray(f.PetsAllowed, c.SetPetsAllowed, c.ClearPetsAllowed)
	setOrClearStringArray(f.Disclosures, c.SetDisclosures, c.ClearDisclosures)
	setOrClearStringArray(f.PropertyCondition, c.SetPropertyCondition, c.ClearPropertyCondition)
	setOrClearStringArray(f.SpecialListingConditions, c.SetSpecialListingConditions, c.ClearSpecialListingConditions)
	setOrClearStringArray(f.GreenEnergyEfficient, c.SetGreenEnergyEfficient, c.ClearGreenEnergyEfficient)
	setOrClearStringArray(f.GreenSustainability, c.SetGreenSustainability, c.ClearGreenSustainability)
	setOrClearStringArray(f.SyndicateTo, c.SetSyndicateTo, c.ClearSyndicateTo)
	setOrClearMap(f.ExtendedFields, c.SetExtendedFields, c.ClearExtendedFields)
}

func applyToPropertyVersionCreate(c *ent.PropertyVersionCreate, f *PropertyFields) {
	c.SetSourceModifiedAt(f.SourceModifiedAt).
		SetMlgCanView(f.MlgCanView).
		SetNillableOriginatingSystemName(f.OriginatingSystemName)
	if f.MlgCanUse != nil {
		c.SetMlgCanUse(f.MlgCanUse)
	}
	c.SetNillableListingID(f.ListingID).
		SetNillableParcelNumber(f.ParcelNumber).
		SetNillableMlsStatus(f.MlsStatus).
		SetNillableStandardStatus(f.StandardStatus).
		SetNillableMajorChangeType(f.MajorChangeType).
		SetNillableMajorChangeTimestamp(f.MajorChangeTimestamp).
		SetNillableListingContractDate(f.ListingContractDate).
		SetNillableOnMarketTimestamp(f.OnMarketTimestamp).
		SetNillableOriginalEntryTimestamp(f.OriginalEntryTimestamp).
		SetNillablePhotosChangeTimestamp(f.PhotosChangeTimestamp).
		SetNillableAvailabilityDate(f.AvailabilityDate).
		SetNillableListPrice(f.ListPrice).
		SetNillableOriginalListPrice(f.OriginalListPrice).
		SetNillablePreviousListPrice(f.PreviousListPrice).
		SetNillableTaxAnnualAmount(f.TaxAnnualAmount).
		SetNillableTaxAssessedValue(f.TaxAssessedValue).
		SetNillableTaxYear(f.TaxYear).
		SetNillablePropertyType(f.PropertyType).
		SetNillablePropertySubType(f.PropertySubType).
		SetNillableNewConstructionYn(f.NewConstructionYn).
		SetNillableBedroomsTotal(f.BedroomsTotal).
		SetNillableBathroomsTotalInteger(f.BathroomsTotalInteger).
		SetNillableBathroomsFull(f.BathroomsFull).
		SetNillableBathroomsHalf(f.BathroomsHalf).
		SetNillableMainLevelBedrooms(f.MainLevelBedrooms).
		SetNillableLivingArea(f.LivingArea).
		SetNillableBuildingAreaTotal(f.BuildingAreaTotal).
		SetNillableLotSizeAcres(f.LotSizeAcres).
		SetNillableLotSizeSquareFeet(f.LotSizeSquareFeet).
		SetNillableStoriesTotal(f.StoriesTotal).
		SetNillableYearBuilt(f.YearBuilt).
		SetNillableGarageSpaces(f.GarageSpaces).
		SetNillableCoveredSpaces(f.CoveredSpaces).
		SetNillableParkingTotal(f.ParkingTotal).
		SetNillableFireplacesTotal(f.FireplacesTotal).
		SetNillablePoolPrivateYn(f.PoolPrivateYn).
		SetNillableWaterfrontYn(f.WaterfrontYn).
		SetNillableViewYn(f.ViewYn).
		SetNillableHorseYn(f.HorseYn).
		SetNillableStreetNumber(f.StreetNumber).
		SetNillableStreetNumberNumeric(f.StreetNumberNumeric).
		SetNillableStreetName(f.StreetName).
		SetNillableStreetSuffix(f.StreetSuffix).
		SetNillableStreetDirPrefix(f.StreetDirPrefix).
		SetNillableStreetDirSuffix(f.StreetDirSuffix).
		SetNillableUnitNumber(f.UnitNumber).
		SetNillableUnparsedAddress(f.UnparsedAddress).
		SetNillableCity(f.City).
		SetNillableStateOrProvince(f.StateOrProvince).
		SetNillablePostalCode(f.PostalCode).
		SetNillablePostalCodePlus4(f.PostalCodePlus4).
		SetNillableCountry(f.Country).
		SetNillableCountyOrParish(f.CountyOrParish).
		SetNillableSubdivisionName(f.SubdivisionName).
		SetNillableMlsAreaMajor(f.MlsAreaMajor).
		SetNillableLatitude(f.Latitude).
		SetNillableLongitude(f.Longitude).
		SetNillableElementarySchool(f.ElementarySchool).
		SetNillableMiddleOrJuniorSchool(f.MiddleOrJuniorSchool).
		SetNillableHighSchool(f.HighSchool).
		SetNillableHighSchoolDistrict(f.HighSchoolDistrict).
		SetNillableListAgentKey(f.ListAgentKey).
		SetNillableListAgentMlsID(f.ListAgentMlsID).
		SetNillableCoListAgentKey(f.CoListAgentKey).
		SetNillableCoListAgentMlsID(f.CoListAgentMlsID).
		SetNillableBuyerAgentKey(f.BuyerAgentKey).
		SetNillableBuyerAgentMlsID(f.BuyerAgentMlsID).
		SetNillableCoBuyerAgentKey(f.CoBuyerAgentKey).
		SetNillableCoBuyerAgentMlsID(f.CoBuyerAgentMlsID).
		SetNillableListOfficeKey(f.ListOfficeKey).
		SetNillableListOfficeMlsID(f.ListOfficeMlsID).
		SetNillableCoListOfficeKey(f.CoListOfficeKey).
		SetNillableCoListOfficeMlsID(f.CoListOfficeMlsID).
		SetNillableBuyerOfficeKey(f.BuyerOfficeKey).
		SetNillableBuyerOfficeMlsID(f.BuyerOfficeMlsID).
		SetNillableCoBuyerOfficeKey(f.CoBuyerOfficeKey).
		SetNillableCoBuyerOfficeMlsID(f.CoBuyerOfficeMlsID).
		SetNillableInternetEntireListingDisplayYn(f.InternetEntireListingDisplayYn).
		SetNillableInternetAddressDisplayYn(f.InternetAddressDisplayYn).
		SetNillableInternetAutomatedValuationDisplayYn(f.InternetAutomatedValuationDisplayYn).
		SetNillableInternetConsumerCommentYn(f.InternetConsumerCommentYn).
		SetNillablePublicRemarks(f.PublicRemarks).
		SetNillableSyndicationRemarks(f.SyndicationRemarks).
		SetNillableDirections(f.Directions).
		SetNillableFurnished(f.Furnished).
		SetNillableDirectionFaces(f.DirectionFaces)
	if f.Appliances != nil {
		c.SetAppliances(f.Appliances)
	}
	if f.Cooling != nil {
		c.SetCooling(f.Cooling)
	}
	if f.Heating != nil {
		c.SetHeating(f.Heating)
	}
	if f.Flooring != nil {
		c.SetFlooring(f.Flooring)
	}
	if f.Roof != nil {
		c.SetRoof(f.Roof)
	}
	if f.ExteriorFeatures != nil {
		c.SetExteriorFeatures(f.ExteriorFeatures)
	}
	if f.InteriorFeatures != nil {
		c.SetInteriorFeatures(f.InteriorFeatures)
	}
	if f.ParkingFeatures != nil {
		c.SetParkingFeatures(f.ParkingFeatures)
	}
	if f.PoolFeatures != nil {
		c.SetPoolFeatures(f.PoolFeatures)
	}
	if f.View != nil {
		c.SetView(f.View)
	}
	if f.WaterfrontFeatures != nil {
		c.SetWaterfrontFeatures(f.WaterfrontFeatures)
	}
	if f.CommunityFeatures != nil {
		c.SetCommunityFeatures(f.CommunityFeatures)
	}
	if f.AccessibilityFeatures != nil {
		c.SetAccessibilityFeatures(f.AccessibilityFeatures)
	}
	if f.Utilities != nil {
		c.SetUtilities(f.Utilities)
	}
	if f.Sewer != nil {
		c.SetSewer(f.Sewer)
	}
	if f.WaterSource != nil {
		c.SetWaterSource(f.WaterSource)
	}
	if f.LotFeatures != nil {
		c.SetLotFeatures(f.LotFeatures)
	}
	if f.PatioAndPorchFeatures != nil {
		c.SetPatioAndPorchFeatures(f.PatioAndPorchFeatures)
	}
	if f.SecurityFeatures != nil {
		c.SetSecurityFeatures(f.SecurityFeatures)
	}
	if f.ConstructionMaterials != nil {
		c.SetConstructionMaterials(f.ConstructionMaterials)
	}
	if f.FoundationDetails != nil {
		c.SetFoundationDetails(f.FoundationDetails)
	}
	if f.Levels != nil {
		c.SetLevels(f.Levels)
	}
	if f.FireplaceFeatures != nil {
		c.SetFireplaceFeatures(f.FireplaceFeatures)
	}
	if f.SpaFeatures != nil {
		c.SetSpaFeatures(f.SpaFeatures)
	}
	if f.Fencing != nil {
		c.SetFencing(f.Fencing)
	}
	if f.HorseAmenities != nil {
		c.SetHorseAmenities(f.HorseAmenities)
	}
	if f.WindowFeatures != nil {
		c.SetWindowFeatures(f.WindowFeatures)
	}
	if f.PetsAllowed != nil {
		c.SetPetsAllowed(f.PetsAllowed)
	}
	if f.Disclosures != nil {
		c.SetDisclosures(f.Disclosures)
	}
	if f.PropertyCondition != nil {
		c.SetPropertyCondition(f.PropertyCondition)
	}
	if f.SpecialListingConditions != nil {
		c.SetSpecialListingConditions(f.SpecialListingConditions)
	}
	if f.GreenEnergyEfficient != nil {
		c.SetGreenEnergyEfficient(f.GreenEnergyEfficient)
	}
	if f.GreenSustainability != nil {
		c.SetGreenSustainability(f.GreenSustainability)
	}
	if f.SyndicateTo != nil {
		c.SetSyndicateTo(f.SyndicateTo)
	}
	if f.ExtendedFields != nil {
		c.SetExtendedFields(f.ExtendedFields)
	}
}
