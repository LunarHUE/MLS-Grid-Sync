package processor

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/ent/property"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
)

// TestValidateTyped_CleanReturnsNoMismatches: a single Property processed
// through the normal pipeline shows zero drift between its entity row
// and its current open version row.
func TestValidateTyped_CleanReturnsNoMismatches(t *testing.T) {
	ctx, client, evID := setupPropertyTest(t)

	ts := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, runPropertyProcess(t, client, ctx, insertRaw(t, client, ctx, evID, map[string]any{
		"ListingKey":            "LK-CLEAN",
		"ModificationTimestamp": ts.Format(time.RFC3339),
		"ListPrice":             "750000.00",
		"City":                  "Austin",
	}, ts)))

	report, err := ValidateTyped(ctx, client, rawoutput.ResourceProperty)
	require.NoError(t, err)
	assert.Equal(t, 1, report.EntitiesSeen)
	assert.Empty(t, report.Mismatches, "fresh pipeline output must have zero drift")
}

// TestValidateTyped_DetectsManualDrift: pin a regression for the Phase 3
// SetNillableX(nil) bug class — if a future code path updates the
// entity without updating the version (or vice versa), validate-typed
// catches it.
func TestValidateTyped_DetectsManualDrift(t *testing.T) {
	ctx, client, evID := setupPropertyTest(t)

	ts := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, runPropertyProcess(t, client, ctx, insertRaw(t, client, ctx, evID, map[string]any{
		"ListingKey":            "LK-DRIFT",
		"ModificationTimestamp": ts.Format(time.RFC3339),
		"ListPrice":             "500000.00",
		"City":                  "Houston",
	}, ts)))

	// Manually mutate the entity's ListPrice without touching the version
	// row — simulates the Bug-1 drift class.
	newPrice := decimal.RequireFromString("999999.99")
	_, err := client.Property.Update().
		Where(property.IDEQ("LK-DRIFT")).
		SetListPrice(newPrice).
		Save(ctx)
	require.NoError(t, err)

	report, err := ValidateTyped(ctx, client, rawoutput.ResourceProperty)
	require.NoError(t, err)
	require.NotEmpty(t, report.Mismatches, "validate-typed must catch the entity-only mutation")

	// Find the ListPrice mismatch.
	var found bool
	for _, m := range report.Mismatches {
		if m.Field == "ListPrice" && m.EntityID == "LK-DRIFT" {
			found = true
			assert.Contains(t, m.EntityValue, "999999.99")
			assert.Contains(t, m.VersionVal, "500000")
		}
	}
	assert.True(t, found, "expected a ListPrice mismatch in the drift report")
}

// TestValidateTyped_TombstonedEntityExcluded: a Property tombstoned via
// MlgCanView=false has intentional entity/version divergence by Phase 3
// design (entity keeps last-known values, version is sparse). validate-
// typed must not flag it.
func TestValidateTyped_TombstonedEntityExcluded(t *testing.T) {
	ctx, client, evID := setupPropertyTest(t)

	ts1 := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, runPropertyProcess(t, client, ctx, insertRaw(t, client, ctx, evID, map[string]any{
		"ListingKey":            "LK-TOMB",
		"ModificationTimestamp": ts1.Format(time.RFC3339),
		"ListPrice":             "300000.00",
	}, ts1)))

	ts2 := ts1.Add(time.Hour)
	require.NoError(t, runPropertyProcess(t, client, ctx, insertRaw(t, client, ctx, evID, map[string]any{
		"ListingKey":            "LK-TOMB",
		"ModificationTimestamp": ts2.Format(time.RFC3339),
		"MlgCanView":            false,
	}, ts2)))

	report, err := ValidateTyped(ctx, client, rawoutput.ResourceProperty)
	require.NoError(t, err)
	assert.Equal(t, 0, report.EntitiesSeen, "tombstoned entity must be excluded from the scan")
	assert.Empty(t, report.Mismatches)
}

// keep context imported
var _ = context.Background
