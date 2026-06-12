package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/rawoutput"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/syncevent"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/mls"
)

// TestResourceNames_RoundTrip is the load-bearing test for the four-domain
// naming contract:
//
//   - MLS API name (mls.Resource*)  ↔  rawoutput.Resource  (helper functions)
//   - rawoutput.Resource  ↔  syncevent.Resource           (direct cast, lockstep)
//
// A future change that drifts any value across any pair trips this test
// loudly. Catches: (a) a new resource added to AllValidatedResources but
// forgotten in a switch arm, (b) an ent regen that re-introduces title
// casing in syncevent, (c) a typo in the MLS API constants.
func TestResourceNames_RoundTrip(t *testing.T) {
	cases := []struct {
		db  rawoutput.Resource
		sev syncevent.Resource
		mls string
	}{
		{rawoutput.ResourceLookup, syncevent.ResourceLookup, mls.ResourceLookup},
		{rawoutput.ResourceOffice, syncevent.ResourceOffice, mls.ResourceOffice},
		{rawoutput.ResourceMember, syncevent.ResourceMember, mls.ResourceMember},
		{rawoutput.ResourceProperty, syncevent.ResourceProperty, mls.ResourceProperty},
		{rawoutput.ResourceOpenHouse, syncevent.ResourceOpenHouse, mls.ResourceOpenHouse},
		{rawoutput.ResourceMedia, syncevent.ResourceMedia, mls.ResourceMedia},
		{rawoutput.ResourcePropertyRooms, syncevent.ResourcePropertyRooms, mls.ResourcePropertyRooms},
		{rawoutput.ResourcePropertyUnitTypes, syncevent.ResourcePropertyUnitTypes, mls.ResourcePropertyUnitTypes},
	}
	for _, tc := range cases {
		t.Run(string(tc.db), func(t *testing.T) {
			got, err := MLSToDBResource(tc.mls)
			require.NoError(t, err)
			assert.Equal(t, tc.db, got, "MLSToDBResource(%q) drift", tc.mls)

			back, err := DBToMLSResource(tc.db)
			require.NoError(t, err)
			assert.Equal(t, tc.mls, back, "DBToMLSResource(%q) drift", tc.db)

			assert.NoError(t, syncevent.ResourceValidator(syncevent.Resource(tc.db)),
				"lockstep broken: %q valid as rawoutput but rejected as syncevent", tc.db)
			assert.NoError(t, rawoutput.ResourceValidator(rawoutput.Resource(tc.sev)),
				"lockstep broken: %q valid as syncevent but rejected as rawoutput", tc.sev)

			assert.Equal(t, string(tc.db), string(tc.sev),
				"syncevent and rawoutput enum values must be character-identical (identifier-level lockstep)")
		})
	}
}

// TestResourceNames_MLSAPIContract pins the exact path segments the MLS
// Grid OData endpoint serves. Plurals (PropertyRooms, PropertyUnitTypes)
// and the no-underscore casing (OpenHouse) are an external contract — a
// typo like ResourceOpenHouse = "Open_House" would crash init at the
// OpenHouse step with no test failure today.
//
// The only other thing that would catch a drift here is a live 400 from
// MLS Grid. This test is cheap insurance against that.
func TestResourceNames_MLSAPIContract(t *testing.T) {
	assert.Equal(t, "Property", mls.ResourceProperty)
	assert.Equal(t, "Media", mls.ResourceMedia)
	assert.Equal(t, "Member", mls.ResourceMember)
	assert.Equal(t, "Office", mls.ResourceOffice)
	assert.Equal(t, "OpenHouse", mls.ResourceOpenHouse)
	assert.Equal(t, "PropertyRooms", mls.ResourcePropertyRooms)
	assert.Equal(t, "PropertyUnitTypes", mls.ResourcePropertyUnitTypes)
	assert.Equal(t, "Lookup", mls.ResourceLookup)
}

// TestResourceNames_UnknownReturnsError catches the trap where a future
// contributor adds a resource to AllValidatedResources but forgets to add
// the switch arm to MLSToDBResource or DBToMLSResource. The empty-string
// zero value of rawoutput.Resource must not silently flow through.
func TestResourceNames_UnknownReturnsError(t *testing.T) {
	_, err := MLSToDBResource("Bogus")
	assert.Error(t, err)

	_, err = MLSToDBResource("")
	assert.Error(t, err)

	_, err = DBToMLSResource(rawoutput.Resource("garbage"))
	assert.Error(t, err)

	_, err = DBToMLSResource(rawoutput.Resource(""))
	assert.Error(t, err)
}
