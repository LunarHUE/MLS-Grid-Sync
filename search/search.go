// Package search owns the pg_trgm layer: the migration that adds the
// trigram (and supporting btree) indexes to the property table, and the
// ent predicates the GraphQL address-search resolvers use against them.
//
// Fuzzy matching is done with PostgreSQL pg_trgm `word_similarity`: a row
// matches when the search query is similar enough to a column (or to a
// combined address expression) — `word_similarity(query, target) >=
// threshold` gives the resolver explicit control over how loose a match
// is, and the gin_trgm_ops indexes accelerate the underlying trigram scan
// via the `%`/`<%` operator family. Correctness-first: the predicates use
// the explicit threshold comparison rather than relying on the session
// similarity GUC.
//
// The DB migration lives in this same package (migrate.go). The combined
// expression in FuzzyAddress MUST stay textually in sync with the GIN
// expression index created there, or Postgres won't use the index — see
// the comment on COMBINED_EXPR below.
package search

import (
	"strings"

	entsql "entgo.io/ent/dialect/sql"

	"github.com/LunarHUE/MLS-Grid-Sync/ent/predicate"
)

// Address component columns on property. Referenced via s.C(...) so the
// generated SQL is table-qualified, matching the geo predicates.
const (
	streetNameColumn = "street_name"
	cityColumn       = "city"
	stateColumn      = "state_or_province"
	postalCodeColumn = "postal_code"
)

// combinedAddressExpr is the SQL expression FuzzyAddress matches against.
//
// COUPLING: this MUST stay textually equivalent to the gin_trgm_ops
// expression index `property_address_combined_trgm` in migrate.go.
// Postgres only uses an expression index when the query expression matches
// the indexed one, so any change here (column set, order, or the `|| ' '
// ||` separators) must be mirrored there.
const combinedAddressExpr = `(
	coalesce(street_number, '') || ' ' ||
	coalesce(street_name, '') || ' ' ||
	coalesce(city, '') || ' ' ||
	coalesce(state_or_province, '') || ' ' ||
	coalesce(postal_code, '')
)`

// FuzzyAddress matches properties whose combined address is trigram-similar
// to query (Option B, single combined free-text search). It emits
//
//	word_similarity($query, <combinedAddressExpr>) >= $threshold
//
// word_similarity(a, b) >= t gives explicit threshold control (vs. the `%`
// operator, which keys off the session similarity GUC); the gin_trgm_ops
// expression index accelerates the trigram scan via the `%`/`<%` operator
// family. Correctness-first.
//
// The Option B resolver routes ZIP-shaped queries to PostalCode instead
// (see IsZipQuery) — trigram is useless on bare digits.
func FuzzyAddress(query string, threshold float64) predicate.Property {
	return predicate.Property(func(s *entsql.Selector) {
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.WriteString("word_similarity(")
			b.Arg(query)
			b.WriteString(", ")
			b.WriteString(combinedAddressExpr)
			b.WriteString(") >= ")
			b.Arg(threshold)
		}))
	})
}

// FuzzyStreet matches properties whose street_name is trigram-similar to
// query (Option C, per-field):
//
//	word_similarity($query, street_name) >= $threshold
func FuzzyStreet(query string, threshold float64) predicate.Property {
	return fuzzyColumn(streetNameColumn, query, threshold)
}

// FuzzyCity matches properties whose city is trigram-similar to query
// (Option C, per-field):
//
//	word_similarity($query, city) >= $threshold
func FuzzyCity(query string, threshold float64) predicate.Property {
	return fuzzyColumn(cityColumn, query, threshold)
}

// fuzzyColumn builds `word_similarity($query, <column>) >= $threshold` for
// a single trigram-indexed column. Shared by FuzzyStreet and FuzzyCity.
func fuzzyColumn(column, query string, threshold float64) predicate.Property {
	return predicate.Property(func(s *entsql.Selector) {
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.WriteString("word_similarity(")
			b.Arg(query)
			b.WriteString(", ")
			b.WriteString(s.C(column))
			b.WriteString(") >= ")
			b.Arg(threshold)
		}))
	})
}

// StateEquals matches properties whose state_or_province equals state,
// case-insensitively (Option C exact field): `upper(state_or_province) =
// upper($state)`. Raw SQL keeps it consistent with the geo predicates and
// the per-field btree index path.
func StateEquals(state string) predicate.Property {
	return predicate.Property(func(s *entsql.Selector) {
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.WriteString("upper(")
			b.WriteString(s.C(stateColumn))
			b.WriteString(") = upper(")
			b.Arg(state)
			b.WriteString(")")
		}))
	})
}

// PostalCode matches on postal_code (Option C exact/prefix field).
//
// If zip is exactly 5 digits it is treated as a full ZIP and matched
// exactly (`postal_code = $zip`). Otherwise — a partial ZIP, a ZIP+4, or
// any other length — it is treated as a PREFIX and matched with `postal_code
// LIKE $zip || '%'`. The LIKE pattern is built by concatenating in SQL so
// the user value still flows through a single bind parameter and is never
// interpolated into the statement text.
func PostalCode(zip string) predicate.Property {
	exact := isFiveDigits(zip)
	return predicate.Property(func(s *entsql.Selector) {
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.WriteString(s.C(postalCodeColumn))
			if exact {
				b.WriteString(" = ")
				b.Arg(zip)
				return
			}
			b.WriteString(" LIKE ")
			b.Arg(zip)
			b.WriteString(" || '%'")
		}))
	})
}

// IsZipQuery reports whether query is ZIP-shaped: after trimming, it is all
// digits, or a 5+4 form like "12345-6789". These are exactly the cases
// where trigram matching is useless (bare digits share no meaningful
// trigrams with addresses) and an exact/prefix postal_code match is the
// correct lookup.
//
// The Option B resolver calls IsZipQuery and routes to PostalCode(query)
// instead of FuzzyAddress(query) when it returns true. Pure and DB-free so
// it is trivially unit-testable.
func IsZipQuery(query string) bool {
	q := strings.TrimSpace(query)
	if q == "" {
		return false
	}
	if plus4, ok := splitZipPlus4(q); ok {
		return plus4
	}
	return isAllDigits(q)
}

// splitZipPlus4 reports whether q is a "12345-6789" ZIP+4. The bool result
// is the match; ok distinguishes "contains a dash" from "no dash at all" so
// IsZipQuery can fall through to the plain all-digits check.
func splitZipPlus4(q string) (match, ok bool) {
	dash := strings.IndexByte(q, '-')
	if dash < 0 {
		return false, false
	}
	five, four := q[:dash], q[dash+1:]
	return isFiveDigits(five) && len(four) == 4 && isAllDigits(four), true
}

// isFiveDigits reports whether s is exactly five ASCII digits.
func isFiveDigits(s string) bool {
	return len(s) == 5 && isAllDigits(s)
}

// isAllDigits reports whether s is non-empty and all ASCII digits.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
