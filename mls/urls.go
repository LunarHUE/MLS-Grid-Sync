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

// InitialURL builds the full-import URL for a resource (no time filter).
// Property gets $expand=Media,Rooms,UnitTypes. All resources get $top=1000.
func InitialURL(v2url, originatingSystem, resource string) string {
	filter := fmt.Sprintf("OriginatingSystemName eq '%s'", originatingSystem)
	u := fmt.Sprintf("%s/%s?$filter=%s&$top=1000%s", v2url, resource, url.QueryEscape(filter), orderByAsc)
	if resource == ResourceProperty {
		u += "&$expand=Media,Rooms,UnitTypes"
	}
	return u
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

// DeltaURL builds the delta sync URL filtered to records modified at or
// after since. ge (not gt) so the boundary record re-fetches and dedups
// at the DB unique-index layer — see Phase 4 plan §7.
func DeltaURL(v2url, originatingSystem, resource string, since time.Time) string {
	filter := fmt.Sprintf(
		"OriginatingSystemName eq '%s' and ModificationTimestamp ge %s",
		originatingSystem,
		since.UTC().Format("2006-01-02T15:04:05.000Z"),
	)
	u := fmt.Sprintf("%s/%s?$filter=%s&$top=1000%s", v2url, resource, url.QueryEscape(filter), orderByAsc)
	if resource == ResourceProperty {
		u += "&$expand=Media,Rooms,UnitTypes"
	}
	return u
}
