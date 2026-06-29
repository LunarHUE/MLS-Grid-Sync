package graph

import (
	"context"
	"fmt"
	"strings"

	"entgo.io/contrib/entgql"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/predicate"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/property"
	"github.com/LunarHUE/MLS-Grid-Sync/graph/model"
	"github.com/LunarHUE/MLS-Grid-Sync/search"
)

// Fuzzy address-search resolvers. Both apply the same mlg_can_view=true
// visibility filter as the rest of the Property queries and delegate the
// SQL to the search package's pg_trgm predicates. Like the geo searches,
// results are ID-ordered (the connection's default), not relevance-ordered:
// a relevance ORDER BY would fight keyset pagination, so consumers that want
// "best match first" should narrow with a higher threshold instead.

// defaultAddressThreshold is the word_similarity cutoff used when a query
// omits `threshold`. 0.3 is loose enough to forgive typos and partial input
// but tight enough to keep unrelated rows out; callers tune per request.
const defaultAddressThreshold = 0.3

// resolveThreshold clamps a caller-supplied threshold to [0, 1] and applies
// the default when omitted. word_similarity returns a value in [0, 1], so a
// threshold outside that range is either always-true or always-false — we
// reject it rather than silently returning everything/nothing.
func resolveThreshold(threshold *float64) (float64, error) {
	if threshold == nil {
		return defaultAddressThreshold, nil
	}
	if *threshold < 0 || *threshold > 1 {
		return 0, fmt.Errorf("threshold %v out of range [0, 1]", *threshold)
	}
	return *threshold, nil
}

// PropertiesByAddress is the single-box fuzzy search (Option B). An all-digit
// query is a ZIP and routes to an exact/prefix postal_code lookup (trigram is
// useless on bare digits); everything else is matched with trigram
// word_similarity against the combined address.
func (r *queryResolver) PropertiesByAddress(ctx context.Context, query string, threshold *float64, after *entgql.Cursor[string], first *int, before *entgql.Cursor[string], last *int, orderBy *ent.PropertyOrder, where *ent.PropertyWhereInput, geo *model.GeoFilter) (*ent.PropertyConnection, error) {
	first, last = clampPage(first, last)
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, fmt.Errorf("query must not be empty")
	}
	thr, err := resolveThreshold(threshold)
	if err != nil {
		return nil, err
	}

	var addrPred predicate.Property
	if search.IsZipQuery(q) {
		addrPred = search.PostalCode(q)
	} else {
		addrPred = search.FuzzyAddress(q, thr)
	}
	preds := []predicate.Property{property.MlgCanView(true), addrPred}
	gp, err := geoPredicate(geo)
	if err != nil {
		return nil, err
	}
	if gp != nil {
		preds = append(preds, gp)
	}
	return r.client.Property.Query().
		Where(preds...).
		Paginate(ctx, after, first, before, last,
			ent.WithPropertyOrder(orderBy),
			ent.WithPropertyFilter(where.Filter))
}

// PropertiesByAddressFields is the structured search (Option C). Provided
// fields are AND-combined: street/city are fuzzy (word_similarity), state is
// exact case-insensitive, zip is exact (5 digits) or prefix. At least one
// field must be set, otherwise the query degenerates to "every visible row".
func (r *queryResolver) PropertiesByAddressFields(ctx context.Context, street *string, city *string, state *string, zip *string, threshold *float64, after *entgql.Cursor[string], first *int, before *entgql.Cursor[string], last *int, orderBy *ent.PropertyOrder, where *ent.PropertyWhereInput, geo *model.GeoFilter) (*ent.PropertyConnection, error) {
	first, last = clampPage(first, last)
	thr, err := resolveThreshold(threshold)
	if err != nil {
		return nil, err
	}

	preds := []predicate.Property{property.MlgCanView(true)}
	if v := trimmed(street); v != "" {
		preds = append(preds, search.FuzzyStreet(v, thr))
	}
	if v := trimmed(city); v != "" {
		preds = append(preds, search.FuzzyCity(v, thr))
	}
	if v := trimmed(state); v != "" {
		preds = append(preds, search.StateEquals(v))
	}
	if v := trimmed(zip); v != "" {
		preds = append(preds, search.PostalCode(v))
	}
	if len(preds) == 1 { // only the visibility predicate
		return nil, fmt.Errorf("at least one of street, city, state, or zip must be provided")
	}
	gp, err := geoPredicate(geo)
	if err != nil {
		return nil, err
	}
	if gp != nil {
		preds = append(preds, gp)
	}
	return r.client.Property.Query().
		Where(preds...).
		Paginate(ctx, after, first, before, last,
			ent.WithPropertyOrder(orderBy),
			ent.WithPropertyFilter(where.Filter))
}

// trimmed dereferences an optional string argument and trims it; nil and
// whitespace-only both collapse to "" (an unset field).
func trimmed(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}
