package processor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/property"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/propertyversion"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/testutil"
)

// processOne runs the PropertyProcessor in its own transaction — used by tests
// that don't want to go through the full RunPass loop.
func runPropertyProcess(t *testing.T, client *ent.Client, ctx context.Context, raw *ent.RawOutput) error {
	t.Helper()
	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	p := NewPropertyProcessor()
	if _, err := p.Process(ctx, tx, raw); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// insertRaw inserts a raw_output row for Property with the given payload and
// timestamp. Returns the row.
func insertRaw(t *testing.T, client *ent.Client, ctx context.Context, syncEventID uuid.UUID, payload map[string]any, modifiedAt time.Time) *ent.RawOutput {
	t.Helper()
	listingKey, _ := payload["ListingKey"].(string)
	require.NotEmpty(t, listingKey, "test payload must include ListingKey")
	return client.RawOutput.Create().
		SetSyncEventID(syncEventID).
		SetResource(rawoutput.ResourceProperty).
		SetSourceKey(listingKey).
		SetChangeType(rawoutput.ChangeTypeInsert).
		SetSourceModifiedAt(modifiedAt).
		SetPayload(payload).
		SaveX(ctx)
}

// setupPropertyTest spins up the DB and creates a SourceSystem + SyncEvent
// for raw_output rows to attach to.
func setupPropertyTest(t *testing.T) (context.Context, *ent.Client, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	client := testutil.NewTestDB(t)

	src := client.SourceSystem.Create().
		SetID("test-src").
		SetSourceSystemName("test").
		SaveX(ctx)

	ev := client.SyncEvent.Create().
		SetSourceSystemID(src.ID).
		SetResource("property").
		SetRunType("sync").
		SetProcessorVersion("test").
		SetStartedAt(time.Now()).
		SaveX(ctx)
	return ctx, client, ev.ID
}

// --- tests ---

func TestPropertyProcess_FreshInsert(t *testing.T) {
	ctx, client, evID := setupPropertyTest(t)

	ts := time.Now().UTC().Truncate(time.Second)
	payload := map[string]any{
		"ListingKey":            "LK-1",
		"ModificationTimestamp": ts.Format(time.RFC3339),
		"ListPrice":             "500000.00",
		"BedroomsTotal":         3,
		"Appliances":            []string{"Dishwasher", "Refrigerator"},
		"City":                  "Austin",
	}
	raw := insertRaw(t, client, ctx, evID, payload, ts)

	require.NoError(t, runPropertyProcess(t, client, ctx, raw))

	// Entity created.
	ent_, err := client.Property.Query().Where(property.IDEQ("LK-1")).Only(ctx)
	require.NoError(t, err)
	require.NotNil(t, ent_.ListPrice)
	assert.Equal(t, "500000", ent_.ListPrice.String())
	require.NotNil(t, ent_.BedroomsTotal)
	assert.Equal(t, int16(3), *ent_.BedroomsTotal)
	require.NotNil(t, ent_.City)
	assert.Equal(t, "Austin", *ent_.City)
	require.NotNil(t, ent_.CurrentVersionID, "current_version_id must be set")

	// Exactly one version, change_type=insert, valid_to=nil, changed_fields=nil.
	versions := client.PropertyVersion.Query().
		Where(propertyversion.ListingKey("LK-1")).
		AllX(ctx)
	require.Len(t, versions, 1)
	v := versions[0]
	assert.Equal(t, propertyversion.ChangeTypeInsert, v.ChangeType)
	assert.Nil(t, v.ValidTo)
	assert.Nil(t, v.ChangedFields, "fresh insert has no diff — changed_fields is nil")
}

func TestPropertyProcess_UpdateWithDiff(t *testing.T) {
	ctx, client, evID := setupPropertyTest(t)

	ts1 := time.Now().UTC().Truncate(time.Second)
	first := insertRaw(t, client, ctx, evID, map[string]any{
		"ListingKey":            "LK-1",
		"ModificationTimestamp": ts1.Format(time.RFC3339),
		"ListPrice":             "500000.00",
		"BedroomsTotal":         3,
	}, ts1)
	require.NoError(t, runPropertyProcess(t, client, ctx, first))

	ts2 := ts1.Add(time.Hour)
	second := insertRaw(t, client, ctx, evID, map[string]any{
		"ListingKey":            "LK-1",
		"ModificationTimestamp": ts2.Format(time.RFC3339),
		"ListPrice":             "475000.00", // changed
		"BedroomsTotal":         3,           // unchanged
	}, ts2)
	require.NoError(t, runPropertyProcess(t, client, ctx, second))

	// Two versions now; oldest is closed.
	versions, err := client.PropertyVersion.Query().
		Where(propertyversion.ListingKey("LK-1")).
		Order(ent.Asc(propertyversion.FieldValidFrom)).
		All(ctx)
	require.NoError(t, err)
	require.Len(t, versions, 2)

	old := versions[0]
	new_ := versions[1]
	require.NotNil(t, old.ValidTo, "old version must be closed")
	assert.Nil(t, new_.ValidTo, "new version is the open one")
	assert.Equal(t, propertyversion.ChangeTypeUpdate, new_.ChangeType)

	require.NotNil(t, new_.ChangedFields)
	require.Contains(t, new_.ChangedFields, "list_price", "diff should record the price change")
	priceDiff, ok := new_.ChangedFields["list_price"].(map[string]any)
	require.True(t, ok)
	assert.NotEqual(t, priceDiff["old"], priceDiff["new"])

	// Entity reflects the new price + new current_version_id.
	ent_, err := client.Property.Query().Where(property.IDEQ("LK-1")).Only(ctx)
	require.NoError(t, err)
	require.NotNil(t, ent_.ListPrice)
	assert.Equal(t, "475000", ent_.ListPrice.String())
	require.NotNil(t, ent_.CurrentVersionID)
}

func TestPropertyProcess_NoDiffIsSkipped(t *testing.T) {
	ctx, client, evID := setupPropertyTest(t)

	ts1 := time.Now().UTC().Truncate(time.Second)
	payload := map[string]any{
		"ListingKey":            "LK-1",
		"ModificationTimestamp": ts1.Format(time.RFC3339),
		"ListPrice":             "500000.00",
	}
	first := insertRaw(t, client, ctx, evID, payload, ts1)
	require.NoError(t, runPropertyProcess(t, client, ctx, first))

	// Same data, but with a later ModificationTimestamp so the stale check
	// doesn't trip. Should produce no diff → no new version.
	ts2 := ts1.Add(time.Hour)
	payload["ModificationTimestamp"] = ts2.Format(time.RFC3339)
	second := insertRaw(t, client, ctx, evID, payload, ts2)
	require.NoError(t, runPropertyProcess(t, client, ctx, second))

	count, err := client.PropertyVersion.Query().
		Where(propertyversion.ListingKey("LK-1")).
		Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "second sync with no diff should not create a version")
}

func TestPropertyProcess_OutOfOrderIsSkipped(t *testing.T) {
	ctx, client, evID := setupPropertyTest(t)

	ts1 := time.Now().UTC().Truncate(time.Second)
	first := insertRaw(t, client, ctx, evID, map[string]any{
		"ListingKey":            "LK-1",
		"ModificationTimestamp": ts1.Format(time.RFC3339),
		"ListPrice":             "500000.00",
	}, ts1)
	require.NoError(t, runPropertyProcess(t, client, ctx, first))

	// Replay an older raw row — even though the payload differs, we shouldn't
	// overwrite a newer version with stale data.
	ts0 := ts1.Add(-time.Hour)
	stale := insertRaw(t, client, ctx, evID, map[string]any{
		"ListingKey":            "LK-1",
		"ModificationTimestamp": ts0.Format(time.RFC3339),
		"ListPrice":             "999999.00",
	}, ts0)
	require.NoError(t, runPropertyProcess(t, client, ctx, stale))

	count := client.PropertyVersion.Query().
		Where(propertyversion.ListingKey("LK-1")).
		CountX(ctx)
	assert.Equal(t, 1, count, "stale replay should not create a new version")

	ent_, err := client.Property.Query().Where(property.IDEQ("LK-1")).Only(ctx)
	require.NoError(t, err)
	require.NotNil(t, ent_.ListPrice)
	assert.Equal(t, "500000", ent_.ListPrice.String(), "entity still reflects the newer (winning) version")
}

func TestPropertyProcess_MlgCanViewFalse_TombstonesExistingEntity(t *testing.T) {
	ctx, client, evID := setupPropertyTest(t)

	ts1 := time.Now().UTC().Truncate(time.Second)
	first := insertRaw(t, client, ctx, evID, map[string]any{
		"ListingKey":            "LK-1",
		"ModificationTimestamp": ts1.Format(time.RFC3339),
		"ListPrice":             "500000.00",
	}, ts1)
	require.NoError(t, runPropertyProcess(t, client, ctx, first))

	ts2 := ts1.Add(time.Hour)
	delete_ := insertRaw(t, client, ctx, evID, map[string]any{
		"ListingKey":            "LK-1",
		"ModificationTimestamp": ts2.Format(time.RFC3339),
		"MlgCanView":            false,
	}, ts2)
	require.NoError(t, runPropertyProcess(t, client, ctx, delete_))

	// Two versions: original (closed) + delete (open).
	versions, err := client.PropertyVersion.Query().
		Where(propertyversion.ListingKey("LK-1")).
		Order(ent.Asc(propertyversion.FieldValidFrom)).
		All(ctx)
	require.NoError(t, err)
	require.Len(t, versions, 2)
	assert.NotNil(t, versions[0].ValidTo)
	assert.Nil(t, versions[1].ValidTo)
	assert.Equal(t, propertyversion.ChangeTypeDelete, versions[1].ChangeType)
	assert.False(t, versions[1].MlgCanView)

	// Entity still exists, tombstoned.
	ent_, err := client.Property.Query().Where(property.IDEQ("LK-1")).Only(ctx)
	require.NoError(t, err)
	assert.False(t, ent_.MlgCanView)
}

func TestPropertyProcess_MlgCanViewFalse_FirstSighting_CreatesTombstone(t *testing.T) {
	ctx, client, evID := setupPropertyTest(t)

	ts := time.Now().UTC().Truncate(time.Second)
	raw := insertRaw(t, client, ctx, evID, map[string]any{
		"ListingKey":            "LK-INVISIBLE",
		"ModificationTimestamp": ts.Format(time.RFC3339),
		"MlgCanView":            false,
	}, ts)
	require.NoError(t, runPropertyProcess(t, client, ctx, raw))

	// Entity row created, tombstoned.
	ent_, err := client.Property.Query().Where(property.IDEQ("LK-INVISIBLE")).Only(ctx)
	require.NoError(t, err)
	assert.False(t, ent_.MlgCanView)
	require.NotNil(t, ent_.CurrentVersionID)

	// One version, change_type=delete, valid_to=nil.
	versions := client.PropertyVersion.Query().
		Where(propertyversion.ListingKey("LK-INVISIBLE")).
		AllX(ctx)
	require.Len(t, versions, 1)
	assert.Equal(t, propertyversion.ChangeTypeDelete, versions[0].ChangeType)
	assert.Nil(t, versions[0].ValidTo)
}

func TestPropertyProcess_UpdateClearsField(t *testing.T) {
	// Phase 2 defect regression: ent's SetNillableX(nil) is a no-op. When a
	// payload omits a previously-set field, applyToPropertyUpdate must
	// explicitly ClearX(), otherwise the entity row keeps the stale value
	// while the version row records NULL — entity/version drift.
	ctx, client, evID := setupPropertyTest(t)

	ts1 := time.Now().UTC().Truncate(time.Second)
	first := insertRaw(t, client, ctx, evID, map[string]any{
		"ListingKey":            "LK-1",
		"ModificationTimestamp": ts1.Format(time.RFC3339),
		"ListPrice":             "500000.00",
		"BedroomsTotal":         3,
		"City":                  "Austin",
	}, ts1)
	require.NoError(t, runPropertyProcess(t, client, ctx, first))

	// Second payload OMITS City and BedroomsTotal entirely.
	ts2 := ts1.Add(time.Hour)
	second := insertRaw(t, client, ctx, evID, map[string]any{
		"ListingKey":            "LK-1",
		"ModificationTimestamp": ts2.Format(time.RFC3339),
		"ListPrice":             "500000.00",
	}, ts2)
	require.NoError(t, runPropertyProcess(t, client, ctx, second))

	// Entity columns cleared.
	ent_, err := client.Property.Query().Where(property.IDEQ("LK-1")).Only(ctx)
	require.NoError(t, err)
	assert.Nil(t, ent_.City, "entity row clears City when payload omits it")
	assert.Nil(t, ent_.BedroomsTotal, "entity row clears BedroomsTotal when payload omits it")

	// New version row also reflects NULL — no entity/version drift.
	versions := client.PropertyVersion.Query().
		Where(propertyversion.ListingKey("LK-1")).
		Order(ent.Asc(propertyversion.FieldValidFrom)).
		AllX(ctx)
	require.Len(t, versions, 2)
	assert.Nil(t, versions[1].City)
	assert.Nil(t, versions[1].BedroomsTotal)

	// Diff records both clears.
	require.NotNil(t, versions[1].ChangedFields)
	cityDiff, ok := versions[1].ChangedFields["city"].(map[string]any)
	require.True(t, ok, "city clear recorded in diff")
	assert.Equal(t, "Austin", cityDiff["old"])
	assert.Nil(t, cityDiff["new"])
}

func TestPropertyProcess_AlreadyTombstonedIsNoop(t *testing.T) {
	// Regression: a second MlgCanView=false record on an already-tombstoned
	// entity must skip. Without the skip we'd close the open delete version
	// and write a duplicate delete version every time the upstream re-asserts.
	ctx, client, evID := setupPropertyTest(t)

	ts1 := time.Now().UTC().Truncate(time.Second)
	first := insertRaw(t, client, ctx, evID, map[string]any{
		"ListingKey":            "LK-1",
		"ModificationTimestamp": ts1.Format(time.RFC3339),
		"ListPrice":             "500000.00",
	}, ts1)
	require.NoError(t, runPropertyProcess(t, client, ctx, first))

	ts2 := ts1.Add(time.Hour)
	tomb := insertRaw(t, client, ctx, evID, map[string]any{
		"ListingKey":            "LK-1",
		"ModificationTimestamp": ts2.Format(time.RFC3339),
		"MlgCanView":            false,
	}, ts2)
	require.NoError(t, runPropertyProcess(t, client, ctx, tomb))

	ts3 := ts2.Add(time.Hour)
	tomb2 := insertRaw(t, client, ctx, evID, map[string]any{
		"ListingKey":            "LK-1",
		"ModificationTimestamp": ts3.Format(time.RFC3339),
		"MlgCanView":            false,
	}, ts3)
	require.NoError(t, runPropertyProcess(t, client, ctx, tomb2))

	count := client.PropertyVersion.Query().
		Where(propertyversion.ListingKey("LK-1")).
		CountX(ctx)
	assert.Equal(t, 2, count, "second tombstone is no-op — only insert + delete versions exist")
}

func TestPropertyProcess_PartialUniqueIndex_RejectsSecondOpenVersion(t *testing.T) {
	// Sanity check the Phase 1 partial unique index: at most one
	// (listing_key) row with valid_to IS NULL.
	ctx, client, evID := setupPropertyTest(t)

	ts := time.Now().UTC().Truncate(time.Second)
	raw := insertRaw(t, client, ctx, evID, map[string]any{
		"ListingKey":            "LK-DUP",
		"ModificationTimestamp": ts.Format(time.RFC3339),
		"ListPrice":             "500000.00",
	}, ts)
	require.NoError(t, runPropertyProcess(t, client, ctx, raw))

	// Try to insert a second open version row directly.
	_, err := client.PropertyVersion.Create().
		SetListingKey("LK-DUP").
		SetSyncEventID(evID).
		SetSourceModifiedAt(ts.Add(time.Hour)).
		SetValidFrom(time.Now()).
		SetChangeType(propertyversion.ChangeTypeUpdate).
		SetProcessorVersion("test").
		Save(ctx)
	require.Error(t, err, "second open version must violate the partial unique index")
}

// --- helper: serialize payload to JSON for raw_output (sanity check) ---

func TestPayloadRoundTrip(t *testing.T) {
	// Sanity check that raw_output.payload round-trips through ent unchanged.
	// Failure here would mean the processor sees a different shape than what
	// was inserted, which would break parseProperty.
	ctx, client, evID := setupPropertyTest(t)

	payload := map[string]any{
		"ListingKey":            "LK-RT",
		"ModificationTimestamp": "2026-01-15T12:00:00Z",
		"ListPrice":             "500000.00",
		"Appliances":            []any{"A", "B"},
	}
	raw := insertRaw(t, client, ctx, evID, payload, time.Now())

	got, err := json.Marshal(raw.Payload)
	require.NoError(t, err)
	assert.Contains(t, string(got), `"ListingKey":"LK-RT"`)
}

// TestPropertyProcess_DateFieldsRoundTrip is the storage-layer guard for the
// parseDate fix: the parse-side tests prove the bytes-to-struct path, this
// one proves the struct-through-ent-through-lib/pq-and-back path preserves
// midnight UTC. Date columns are timestamptz (Ent field.Time default), and
// session-timezone handling in the driver is exactly where 2012-03-14
// silently becomes 2012-03-13T23:00 or 2012-03-14T01:00. Cross-checks
// ModificationTimestamp in the same row to prove the symmetric Timestamp
// path also still round-trips intact.
func TestPropertyProcess_DateFieldsRoundTrip(t *testing.T) {
	ctx, client, evID := setupPropertyTest(t)

	payloadBytes, err := os.ReadFile(filepath.Join("testdata", "property", "date_fields.json"))
	require.NoError(t, err, "fixture missing — extract per TestParseProperty_DateFieldsFixture comment")

	var payload map[string]any
	require.NoError(t, json.Unmarshal(payloadBytes, &payload))

	listingKey, _ := payload["ListingKey"].(string)
	modStr, _ := payload["ModificationTimestamp"].(string)
	modAt, err := time.Parse(time.RFC3339, modStr)
	require.NoError(t, err)

	raw := insertRaw(t, client, ctx, evID, payload, modAt)
	require.NoError(t, runPropertyProcess(t, client, ctx, raw))

	ent_, err := client.Property.Query().Where(property.IDEQ(listingKey)).Only(ctx)
	require.NoError(t, err)

	// Date field: bit-exact midnight UTC. Any session-timezone drift through
	// lib/pq shows up here as an off-by-one day or off-by-N hours.
	require.NotNil(t, ent_.ListingContractDate, "ListingContractDate must populate from a YYYY-MM-DD payload")
	wantDate := time.Date(2012, 3, 14, 0, 0, 0, 0, time.UTC)
	gotDate := ent_.ListingContractDate.UTC()
	assert.True(t, gotDate.Equal(wantDate),
		"date drifted through driver: want %s, got %s", wantDate, gotDate)

	// Symmetric Timestamp path: hh:mm:ss must survive intact in the same row.
	gotMod := ent_.SourceModifiedAt.UTC()
	assert.True(t, gotMod.Equal(modAt),
		"timestamp drifted through driver: want %s, got %s", modAt, gotMod)
}

// Orphan agent/office references — Properties whose ListAgentKey (etc.)
// names a Member or Office absent from this MLS feed must INSERT, not halt.
// Before the FK constraint drop these payloads tripped pq 23503; the
// soft-key model is what makes the row legitimate. See plan §1.

func TestPropertyProcess_OrphanListAgentInserts(t *testing.T) {
	ctx, client, evID := setupPropertyTest(t)

	ts := time.Now().UTC().Truncate(time.Second)
	payload := map[string]any{
		"ListingKey":            "LK-ORPHAN-AGENT-1",
		"ModificationTimestamp": ts.Format(time.RFC3339),
		"ListAgentKey":          "NO-SUCH-MEMBER-1", // intentionally orphan
		"ListPrice":             "100000.00",
	}
	raw := insertRaw(t, client, ctx, evID, payload, ts)

	require.NoError(t, runPropertyProcess(t, client, ctx, raw),
		"orphan ListAgentKey must not block Property insert (this is the regression for raw_output 019eb3d0-92bf-7556-...)")

	ent_, err := client.Property.Query().Where(property.IDEQ("LK-ORPHAN-AGENT-1")).Only(ctx)
	require.NoError(t, err)
	require.NotNil(t, ent_.ListAgentKey)
	assert.Equal(t, "NO-SUCH-MEMBER-1", *ent_.ListAgentKey,
		"orphan key persisted verbatim — the resolver, not the constraint, decides visibility")

	// No Member row was created as a side effect.
	memberCount, err := client.Member.Query().Count(ctx)
	require.NoError(t, err)
	assert.Zero(t, memberCount, "no phantom Member row was upserted")
}

func TestPropertyProcess_OrphanCoBuyerOfficeInserts(t *testing.T) {
	ctx, client, evID := setupPropertyTest(t)

	ts := time.Now().UTC().Truncate(time.Second)
	payload := map[string]any{
		"ListingKey":            "LK-ORPHAN-OFFICE-1",
		"ModificationTimestamp": ts.Format(time.RFC3339),
		"CoBuyerOfficeKey":      "NO-SUCH-OFFICE-1",
		"ListPrice":             "200000.00",
	}
	raw := insertRaw(t, client, ctx, evID, payload, ts)

	require.NoError(t, runPropertyProcess(t, client, ctx, raw),
		"orphan CoBuyerOfficeKey must not block insert — coverage that the FK drop is uniform across all 8 forward keys, not just the one that fired")

	ent_, err := client.Property.Query().Where(property.IDEQ("LK-ORPHAN-OFFICE-1")).Only(ctx)
	require.NoError(t, err)
	require.NotNil(t, ent_.CoBuyerOfficeKey)
	assert.Equal(t, "NO-SUCH-OFFICE-1", *ent_.CoBuyerOfficeKey)
}
