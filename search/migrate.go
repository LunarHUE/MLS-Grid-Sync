package search

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// migrations are idempotent and ordered. They back the two fuzzy
// address-search resolvers:
//
//   - The combined expression index serves Option B (propertiesByAddress),
//     a single free-text trigram match over the whole address. Its
//     expression text MUST stay byte-for-byte in sync with the predicate
//     built in search.go — Postgres only uses an expression index when the
//     query expression matches the indexed one exactly.
//   - The per-field trigram indexes (street_name, city, unparsed_address)
//     plus the btree indexes (postal_code, state_or_province) serve
//     Option C (propertiesByAddressFields), which matches structured
//     fields independently: trigram for fuzzy text, btree for exact/prefix
//     state + ZIP lookups.
//
// pg_trgm must be enabled before any gin_trgm_ops index can be created.
var migrations = []string{
	`CREATE EXTENSION IF NOT EXISTS pg_trgm`,
	`CREATE INDEX IF NOT EXISTS property_address_combined_trgm ON property USING gin ((
		coalesce(street_number, '') || ' ' ||
		coalesce(street_name, '') || ' ' ||
		coalesce(city, '') || ' ' ||
		coalesce(state_or_province, '') || ' ' ||
		coalesce(postal_code, '')
	) gin_trgm_ops)`,
	`CREATE INDEX IF NOT EXISTS property_street_name_trgm ON property USING gin (street_name gin_trgm_ops)`,
	`CREATE INDEX IF NOT EXISTS property_city_trgm ON property USING gin (city gin_trgm_ops)`,
	`CREATE INDEX IF NOT EXISTS property_unparsed_address_trgm ON property USING gin (unparsed_address gin_trgm_ops)`,
	`CREATE INDEX IF NOT EXISTS property_postal_code_btree ON property (postal_code)`,
	`CREATE INDEX IF NOT EXISTS property_state_btree ON property (state_or_province)`,
}

// Migrate enables pg_trgm and adds the trigram + btree indexes that the
// fuzzy address-search resolvers query against. Called after ent's
// Schema.Create by the commands that own migrations (sync/init/worker),
// never by serve.
func Migrate(ctx context.Context, db *sql.DB) error {
	for _, stmt := range migrations {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("trigram migration %q: %w", strings.Fields(stmt)[0]+" …", err)
		}
	}
	return nil
}
