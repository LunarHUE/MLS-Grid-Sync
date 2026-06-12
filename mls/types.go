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
type ODataResponse struct {
	NextLink string            `json:"@odata.nextLink"`
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
