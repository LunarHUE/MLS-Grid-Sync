package mls

import (
	"fmt"
	"net/url"
	"time"
)

// orderByAsc is appended to every fetch URL so pagination is strictly
// ascending by ModificationTimestamp. The space between field and
// direction is percent-encoded — without it, nginx in front of the MLS
// Grid API rejects the request with a 400 before it ever reaches OData.
const orderByAsc = "&$orderby=ModificationTimestamp%20asc"

// countTrue asks OData to include the total matching-record count (@odata.count)
// in the response. Appended to the initial/delta page URLs so the pull progress
// bar learns its denominator up front (and on every page — the server computes
// it from $filter, independent of paging), with no extra round trip. $skip pages
// inherit it from the base URL.
const countTrue = "&$count=true"

// PageSize is the OData $top applied to every paginated fetch. It doubles as
// the stride for $skip offset paging (SkipURL) and the end-of-data signal for
// the concurrent fetcher: a page returning fewer than PageSize records is the
// last one. MLS Grid caps $top at 1000.
const PageSize = 1000

// InitialURL builds the full-import URL for a resource (no time filter).
// Property gets $expand=Media,Rooms,UnitTypes. All resources get $top=PageSize.
func InitialURL(v2url, originatingSystem, resource string) string {
	filter := fmt.Sprintf("OriginatingSystemName eq '%s'", originatingSystem)
	u := fmt.Sprintf("%s/%s?$filter=%s&$top=%d%s%s", v2url, resource, url.QueryEscape(filter), PageSize, orderByAsc, countTrue)
	if resource == ResourceProperty {
		u += "&$expand=Media,Rooms,UnitTypes"
	}
	return u
}

// SkipURL appends an OData $skip to a base page URL produced by InitialURL or
// DeltaURL, for offset-based concurrent pagination. The base already carries
// $orderby=ModificationTimestamp asc, so $skip is deterministic — a stable
// sort yields disjoint, non-overlapping pages across concurrent requests
// (verified against the live feed). skip<=0 returns the base URL unchanged.
func SkipURL(baseURL string, skip int) string {
	if skip <= 0 {
		return baseURL
	}
	return fmt.Sprintf("%s&$skip=%d", baseURL, skip)
}

// DiscoveryURL builds a single-page Lookup query with NO OriginatingSystemName
// filter — used by the `systems` subcommand to probe which originating systems
// the configured token can see. $top=100 keeps the probe to one request
// while giving enough payload to extract distinct OriginatingSystemName
// values. Omits $orderby (no need for ordering on a best-effort probe;
// also dodges the URL-encoding surface). The probe still flows through
// mls.Client so it inherits the rate limiter, auth bearer token, and retry
// path — see plan §5.
func DiscoveryURL(v2url string) string {
	return fmt.Sprintf("%s/%s?$top=100", v2url, ResourceLookup)
}

// MediaRefreshURL builds a single-listing Property query whose only purpose is
// to mint FRESH media links.
//
// MLS Grid media URLs are not stable references. Each carries a token and an
// `expires` stamp exactly one hour out, and each is SINGLE USE — re-requesting
// a URL that already served bytes returns 429 "Request limit reached", while a
// freshly minted URL for the very same image succeeds. So the media_url
// captured at sync time is unusable for any download that does not happen
// almost immediately — which is every download, once a backlog exists.
//
// Refreshing per LISTING rather than per image is deliberate: every Media
// entry inside one Property response shares that response's token, so a single
// request re-arms all of a listing's photos for the next hour. Per-image
// refresh would multiply OData calls by ~20 against the same ~1 rps budget.
//
// $top=1 because ListingKey is unique; the $expand is the part that matters.
func MediaRefreshURL(v2url, originatingSystem, listingKey string) string {
	filter := fmt.Sprintf(
		"OriginatingSystemName eq '%s' and ListingKey eq '%s'",
		originatingSystem,
		listingKey,
	)
	return fmt.Sprintf("%s/%s?$filter=%s&$top=1&$expand=Media",
		v2url, ResourceProperty, url.QueryEscape(filter))
}

// DeltaURL builds the delta sync URL filtered to records modified at or
// after since. ge (not gt) so the boundary record re-fetches and dedups
// at the DB unique-index layer — see Phase 4 plan §7.
func DeltaURL(v2url, originatingSystem, resource string, since time.Time) string {
	filter := fmt.Sprintf(
		"OriginatingSystemName eq '%s' and ModificationTimestamp ge %s",
		originatingSystem,
		since.UTC().Format("2006-01-02T15:04:05.000Z"),
	)
	u := fmt.Sprintf("%s/%s?$filter=%s&$top=%d%s%s", v2url, resource, url.QueryEscape(filter), PageSize, orderByAsc, countTrue)
	if resource == ResourceProperty {
		u += "&$expand=Media,Rooms,UnitTypes"
	}
	return u
}
