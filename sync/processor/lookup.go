package processor

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/lookup"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/rawoutput"
)

// LookupProcessor handles the Lookup resource — MLS Grid's enumeration
// registry. Lookup is reference data, not entity history:
//   - upsert-by-lookup_key on every sync; new values overwrite.
//   - no version table, no diff, no tombstone semantics.
//   - MlgCanView=false simply deletes the row (the enumeration value is
//     no longer published).
//
// Concurrent writes to the same lookup_key are prevented by the per-resource
// advisory lock in the generic loop, so the query-then-create/update pattern
// here is race-free without OnConflict (which isn't enabled in ent codegen).
type LookupProcessor struct{}

func NewLookupProcessor() *LookupProcessor { return &LookupProcessor{} }

func (*LookupProcessor) Resource() rawoutput.Resource { return rawoutput.ResourceLookup }

// LookupFields is parsed inline — only four typed fields plus MLSMetadataMixin.
type LookupFields struct {
	LookupKey             string
	SourceModifiedAt      time.Time
	OriginatingSystemName *string
	MlgCanView            bool
	MlgCanUse             []string

	LookupName          string
	LookupValue         string
	StandardLookupValue *string

	ExtendedFields map[string]any
}

func parseLookup(payload []byte) (*LookupFields, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("payload is not a JSON object: %w", err)
	}
	out := &LookupFields{MlgCanView: true}

	consume := func(key string) (json.RawMessage, bool) {
		v, ok := raw[key]
		if ok {
			delete(raw, key)
		}
		return v, ok
	}

	// Required: LookupKey, ModificationTimestamp, LookupName, LookupValue.
	keyRaw, ok := consume("LookupKey")
	if !ok {
		return nil, fmt.Errorf("missing required field LookupKey")
	}
	lookupKey, err := parseString(keyRaw)
	if err != nil || lookupKey == nil || *lookupKey == "" {
		return nil, fmt.Errorf("LookupKey: empty or invalid")
	}
	out.LookupKey = *lookupKey

	tsRaw, ok := consume("ModificationTimestamp")
	if !ok {
		return nil, fmt.Errorf("missing required field ModificationTimestamp")
	}
	ts, err := parseTime(tsRaw)
	if err != nil || ts == nil {
		return nil, fmt.Errorf("ModificationTimestamp: %w", err)
	}
	out.SourceModifiedAt = *ts

	nameRaw, ok := consume("LookupName")
	if !ok {
		return nil, fmt.Errorf("missing required field LookupName")
	}
	name, err := parseString(nameRaw)
	if err != nil || name == nil || *name == "" {
		return nil, fmt.Errorf("LookupName: empty or invalid")
	}
	out.LookupName = *name

	valRaw, ok := consume("LookupValue")
	if !ok {
		return nil, fmt.Errorf("missing required field LookupValue")
	}
	val, err := parseString(valRaw)
	if err != nil || val == nil || *val == "" {
		return nil, fmt.Errorf("LookupValue: empty or invalid")
	}
	out.LookupValue = *val

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
	if v, ok := consume("StandardLookupValue"); ok {
		out.StandardLookupValue, err = parseString(v)
		if err != nil {
			return nil, fmt.Errorf("StandardLookupValue: %w", err)
		}
	}

	// Lookup has no ExtendedFields column on the schema — anything unmapped
	// at this layer is silently dropped. The validate-raw sweep surfaces
	// it for triage rather than failing in the hot path.
	out.ExtendedFields = nil
	_ = raw

	return out, nil
}

func (p *LookupProcessor) Process(ctx context.Context, tx *ent.Tx, raw *ent.RawOutput) (Outcome, error) {
	payload, err := json.Marshal(raw.Payload)
	if err != nil {
		return OutcomeUnknown, fmt.Errorf("marshal payload: %w", err)
	}
	fields, err := parseLookup(payload)
	if err != nil {
		return OutcomeUnknown, fmt.Errorf("parse: %w", err)
	}

	current, err := tx.Lookup.Query().Where(lookup.IDEQ(fields.LookupKey)).Only(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return OutcomeUnknown, fmt.Errorf("lookup lookup: %w", err)
	}
	entityExists := err == nil

	// MlgCanView=false → delete the row. Lookup has no version history,
	// so there's no tombstone to write — just remove. When the row is
	// already absent the no-op surfaces as skip-tombstoned for stats
	// uniformity with the other resources.
	if !fields.MlgCanView {
		if !entityExists {
			return OutcomeSkipTombstoned, nil
		}
		if _, err := tx.Lookup.Delete().Where(lookup.IDEQ(fields.LookupKey)).Exec(ctx); err != nil {
			return OutcomeUnknown, fmt.Errorf("delete lookup: %w", err)
		}
		return OutcomeDelete, nil
	}

	if !entityExists {
		c := tx.Lookup.Create().
			SetID(fields.LookupKey).
			SetSourceModifiedAt(fields.SourceModifiedAt).
			SetMlgCanView(fields.MlgCanView).
			SetLookupName(fields.LookupName).
			SetLookupValue(fields.LookupValue).
			SetNillableOriginatingSystemName(fields.OriginatingSystemName).
			SetNillableStandardLookupValue(fields.StandardLookupValue)
		if fields.MlgCanUse != nil {
			c.SetMlgCanUse(fields.MlgCanUse)
		}
		if _, err := c.Save(ctx); err != nil {
			return OutcomeUnknown, fmt.Errorf("create lookup: %w", err)
		}
		return OutcomeInsert, nil
	}

	// Update path — apply clear-on-nil for the optional fields.
	u := tx.Lookup.UpdateOneID(current.ID).
		SetSourceModifiedAt(fields.SourceModifiedAt).
		SetMlgCanView(fields.MlgCanView).
		SetLookupName(fields.LookupName).
		SetLookupValue(fields.LookupValue)
	setOrClearStr(fields.OriginatingSystemName, u.SetOriginatingSystemName, u.ClearOriginatingSystemName)
	setOrClearSlice(fields.MlgCanUse, u.SetMlgCanUse, u.ClearMlgCanUse)
	setOrClearStr(fields.StandardLookupValue, u.SetStandardLookupValue, u.ClearStandardLookupValue)
	if _, err := u.Save(ctx); err != nil {
		return OutcomeUnknown, fmt.Errorf("update lookup: %w", err)
	}
	return OutcomeUpdate, nil
}
