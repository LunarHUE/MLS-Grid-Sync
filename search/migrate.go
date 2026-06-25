package search

import (
	"context"
	"database/sql"
	"errors"
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
// ExtensionName is the PostgreSQL extension the fuzzy address-search
// resolvers depend on. Without it, every word_similarity(...) predicate
// fails at query time with `function word_similarity(...) does not exist`.
// health (/readyz) and doctor verify it via CheckExtension.
const ExtensionName = "pg_trgm"

// IndexNames lists the indexes Migrate creates, in creation order. They are a
// performance concern, not a correctness one — the resolvers return correct
// results without them (via a sequential scan), just slowly. doctor reports a
// missing index as a warning; readiness does not gate on them. Kept in sync
// with the CREATE INDEX statements below.
var IndexNames = []string{
	"property_address_combined_trgm",
	"property_street_name_trgm",
	"property_city_trgm",
	"property_unparsed_address_trgm",
	"property_postal_code_btree",
	"property_state_btree",
}

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
// Schema.Create by the commands that own migrations (sync/init/worker/migrate),
// never by serve.
func Migrate(ctx context.Context, db *sql.DB) error {
	for _, stmt := range migrations {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("trigram migration %q: %w", strings.Fields(stmt)[0]+" …", err)
		}
	}
	return nil
}

// ErrExtensionMissing reports that pg_trgm is not installed on the target
// database — the root cause of `word_similarity(...) does not exist` at query
// time. serve does not run migrations, so a database whose data predates the
// fuzzy-search feature can reach this state; running `mls-cli migrate` (or any
// sync/init/worker startup) installs it idempotently.
var ErrExtensionMissing = errors.New("pg_trgm extension is not installed")

// CheckExtension returns nil when pg_trgm is installed, ErrExtensionMissing
// when it is absent, and a wrapped error if the catalog query itself fails.
// This is the correctness gate the fuzzy resolvers depend on; both /readyz and
// doctor call it. Read-only.
func CheckExtension(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return errors.New("no database handle")
	}
	var present bool
	err := db.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = $1)", ExtensionName).
		Scan(&present)
	if err != nil {
		return fmt.Errorf("querying for the %s extension: %w", ExtensionName, err)
	}
	if !present {
		return ErrExtensionMissing
	}
	return nil
}

// MissingIndexes returns the subset of IndexNames not present on the database,
// in IndexNames order. An empty slice means all expected fuzzy-search indexes
// exist. Indexes are advisory (performance, not correctness), so callers treat
// a non-empty result as a warning rather than a hard failure. Read-only.
func MissingIndexes(ctx context.Context, db *sql.DB) ([]string, error) {
	if db == nil {
		return nil, errors.New("no database handle")
	}
	var missing []string
	for _, name := range IndexNames {
		var present bool
		err := db.QueryRowContext(ctx,
			"SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1)", name).
			Scan(&present)
		if err != nil {
			return nil, fmt.Errorf("querying for index %q: %w", name, err)
		}
		if !present {
			missing = append(missing, name)
		}
	}
	return missing, nil
}
