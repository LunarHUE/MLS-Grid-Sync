package processor

import (
	"context"
	"fmt"

	entsql "entgo.io/ent/dialect/sql"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/property"
)

// Shared helpers for the Property-child bulk projectors (open_house,
// property_room, property_unit_type). These resources are Property-shaped
// versioned entities plus a parking FK to Property (parent_listing_key): set it
// only when the parent exists at process time, and once set never change or
// clear it (the PropertyProcessor.AfterPass re-link promotes parked rows later).

// childParentColumn is the parking FK column shared by all three child resources.
const childParentColumn = "parent_listing_key"

// bulkExistingParentKeys returns the subset of listingKeys for which a Property
// row exists — the parking-FK resolution (per-record does one EXISTS query per
// record) done once for the whole chunk.
func bulkExistingParentKeys(ctx context.Context, tx *ent.Tx, listingKeys []string) (map[string]struct{}, error) {
	out := make(map[string]struct{}, len(listingKeys))
	if len(listingKeys) == 0 {
		return out, nil
	}
	ids, err := tx.Property.Query().Where(property.IDIn(listingKeys...)).IDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("bulk parent existence: %w", err)
	}
	for _, id := range ids {
		out[id] = struct{}{}
	}
	return out, nil
}

// parentFKFor returns &listingKey when the parent exists in the set, else nil —
// the bulk equivalent of the per-record EXISTS check.
func parentFKFor(listingKey string, existingParents map[string]struct{}) *string {
	if _, ok := existingParents[listingKey]; ok {
		k := listingKey
		return &k
	}
	return nil
}

// upsertSetExcluded sets every schema column to its EXCLUDED (proposed-row)
// value on conflict, except the skip columns. Unlike ent's UpdateNewValues —
// which only touches columns present in *this chunk's* INSERT — it sets ALL
// columns, so a field that is NULL across the whole chunk resolves to EXCLUDED's
// NULL and is cleared, exactly matching the per-record clear-on-nil update
// (applyToXUpdate). skip holds the columns that must NOT be overwritten: the PK
// (id), the immutable created_at, and per-resource sticky/derived columns —
// media.attachment_id (download pointer) and the children's parent_listing_key
// (handled separately via COALESCE).
func upsertSetExcluded(u *entsql.UpdateSet, columns []string, skip ...string) {
	skipSet := make(map[string]bool, len(skip))
	for _, s := range skip {
		skipSet[s] = true
	}
	for _, col := range columns {
		if skipSet[col] {
			continue
		}
		u.SetExcluded(col)
	}
}

// setParentListingKeyCoalesce makes a child entity upsert resolve
// parent_listing_key = COALESCE(existing, excluded) on conflict — i.e. keep an
// already-linked FK, otherwise take the newly-resolved value (which is the
// listing_key when the parent now exists, or NULL when it still doesn't). This
// is exactly the per-record "promote only if it just became resolvable, never
// clear" guard, expressed in one statement. Call inside the upsert Update func.
func setParentListingKeyCoalesce(u *entsql.UpdateSet) {
	existing := u.Table().C(childParentColumn)
	excluded := entsql.Dialect(u.Dialect()).Table("excluded").C(childParentColumn)
	u.Set(childParentColumn, entsql.Expr("COALESCE("+existing+", "+excluded+")"))
}
