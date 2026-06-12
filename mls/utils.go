package mls

import "fmt"

func ResourceKeyField(resourceName string) (string, error) {
	switch resourceName {
	case ResourceProperty:
		return "ListingKey", nil
	case ResourceMember:
		return "MemberKey", nil
	case ResourceOffice:
		return "OfficeKey", nil
	case ResourceOpenHouse:
		return "OpenHouseKey", nil
	case ResourceMedia:
		return "MediaKey", nil
	case ResourcePropertyRooms:
		return "RoomKey", nil
	case ResourcePropertyUnitTypes:
		return "UnitTypeKey", nil
	case ResourceLookup:
		return "LookupKey", nil
	default:
		return "", fmt.Errorf("unknown resource type: %s", resourceName)
	}
}
