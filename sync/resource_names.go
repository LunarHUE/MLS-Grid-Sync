package sync

import (
	"fmt"

	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/mls"
)

// MLSToDBResource maps an MLS Grid API resource name (e.g. "OpenHouse") to
// the canonical rawoutput.Resource enum value (e.g. "open_house"). Returns
// an error for unknown names rather than a zero-valued enum so callers
// can't silently use the empty string as a resource identifier.
func MLSToDBResource(apiName string) (rawoutput.Resource, error) {
	switch apiName {
	case mls.ResourceProperty:
		return rawoutput.ResourceProperty, nil
	case mls.ResourceMedia:
		return rawoutput.ResourceMedia, nil
	case mls.ResourceMember:
		return rawoutput.ResourceMember, nil
	case mls.ResourceOffice:
		return rawoutput.ResourceOffice, nil
	case mls.ResourceOpenHouse:
		return rawoutput.ResourceOpenHouse, nil
	case mls.ResourcePropertyRooms:
		return rawoutput.ResourcePropertyRooms, nil
	case mls.ResourcePropertyUnitTypes:
		return rawoutput.ResourcePropertyUnitTypes, nil
	case mls.ResourceLookup:
		return rawoutput.ResourceLookup, nil
	default:
		return "", fmt.Errorf("unknown MLS API resource name: %s", apiName)
	}
}

// DBToMLSResource maps a rawoutput.Resource enum value to the exact MLS
// Grid API path segment the OData endpoint serves (e.g. "open_house" →
// "OpenHouse"). The returned strings are an external API contract pinned
// by TestResourceNames_MLSAPIContract.
func DBToMLSResource(r rawoutput.Resource) (string, error) {
	switch r {
	case rawoutput.ResourceProperty:
		return mls.ResourceProperty, nil
	case rawoutput.ResourceMedia:
		return mls.ResourceMedia, nil
	case rawoutput.ResourceMember:
		return mls.ResourceMember, nil
	case rawoutput.ResourceOffice:
		return mls.ResourceOffice, nil
	case rawoutput.ResourceOpenHouse:
		return mls.ResourceOpenHouse, nil
	case rawoutput.ResourcePropertyRooms:
		return mls.ResourcePropertyRooms, nil
	case rawoutput.ResourcePropertyUnitTypes:
		return mls.ResourcePropertyUnitTypes, nil
	case rawoutput.ResourceLookup:
		return mls.ResourceLookup, nil
	default:
		return "", fmt.Errorf("unknown DB resource: %q", r)
	}
}
