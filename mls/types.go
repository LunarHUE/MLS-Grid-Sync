package mls

import (
	"context"
	"encoding/json"
)

// PageFetcher is the subset of Client used by sync.Service, allowing easy mocking in tests.
type PageFetcher interface {
	FetchPage(ctx context.Context, pageURL string) (*ODataResponse, error)
}

// ODataResponse is the wrapper for every MLS Grid API response.
//
// Count is the total number of records matching the query ($filter), present
// only when the request asked for it with $count=true (the initial/delta page
// URLs do). It's a pointer so a missing/omitted count is distinguishable from a
// real zero — the pull progress bar uses it as its denominator and falls back
// to a count-only display when it's nil.
type ODataResponse struct {
	NextLink string            `json:"@odata.nextLink"`
	Count    *int64            `json:"@odata.count"`
	Value    []json.RawMessage `json:"value"`
}

const (
	ResourceProperty          = "Property"
	ResourceMedia             = "Media"
	ResourceMember            = "Member"
	ResourceOffice            = "Office"
	ResourceOpenHouse         = "OpenHouse"
	ResourcePropertyRooms     = "PropertyRooms"
	ResourcePropertyUnitTypes = "PropertyUnitTypes"
	ResourceLookup            = "Lookup"
)
