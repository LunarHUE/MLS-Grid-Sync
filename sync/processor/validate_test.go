package processor

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
)

// TestAllValidatedResources_FKOrder pins the canonical FK-dependency
// order. Processor passes and `reprocess` iterate this slice in order;
// reordering Member before Office would re-introduce the FK halt that
// the 2026-06-10 initial-sync attempt hit. Don't alphabetize.
func TestAllValidatedResources_FKOrder(t *testing.T) {
	want := []rawoutput.Resource{
		rawoutput.ResourceLookup,
		rawoutput.ResourceOffice,
		rawoutput.ResourceMember,
		rawoutput.ResourceProperty,
		rawoutput.ResourceOpenHouse,
		rawoutput.ResourceMedia,
		rawoutput.ResourcePropertyRooms,
		rawoutput.ResourcePropertyUnitTypes,
	}
	assert.Equal(t, want, AllValidatedResources, "AllValidatedResources order is load-bearing")
}

// TestFetchableResources_ContentsAndOrder is the fetch-list pin: `init`
// and the sync daemon fetch only these five resources. Adding Media,
// PropertyRooms, or PropertyUnitTypes to the list would recreate the
// MLS Grid 501 "Use expand by adding &$expand=Media" — they are
// expand-only and must arrive via Property's $expand, not as standalone
// fetches.
func TestFetchableResources_ContentsAndOrder(t *testing.T) {
	want := []rawoutput.Resource{
		rawoutput.ResourceLookup,
		rawoutput.ResourceOffice,
		rawoutput.ResourceMember,
		rawoutput.ResourceProperty,
		rawoutput.ResourceOpenHouse,
	}
	assert.Equal(t, want, FetchableResources, "FetchableResources contents/order are load-bearing for init")

	// Explicitly assert the expand-only trio is ABSENT — the regression
	// here is someone "helpfully" re-adding Media to the fetch loop.
	for _, r := range []rawoutput.Resource{
		rawoutput.ResourceMedia,
		rawoutput.ResourcePropertyRooms,
		rawoutput.ResourcePropertyUnitTypes,
	} {
		assert.NotContains(t, FetchableResources, r,
			"%s is expand-only and must NOT appear in FetchableResources", r)
	}
}

// TestFetchableResources_PrefixOfAllValidated asserts FetchableResources
// preserves the FK-dependency order of AllValidatedResources for the
// resources it includes. The fetch list isn't literally a prefix
// (OpenHouse comes after Property in AllValidatedResources but the
// expand-only trio sits between Property and OpenHouse there), but the
// relative order of fetchable resources must match.
func TestFetchableResources_PrefixOfAllValidated(t *testing.T) {
	pos := map[rawoutput.Resource]int{}
	for i, r := range AllValidatedResources {
		pos[r] = i
	}
	for i := 1; i < len(FetchableResources); i++ {
		prev := FetchableResources[i-1]
		curr := FetchableResources[i]
		assert.Less(t, pos[prev], pos[curr],
			"FetchableResources must follow AllValidatedResources order: %s before %s", prev, curr)
	}
}

// TestChildResources pins the Property → [media, property_rooms,
// property_unit_types] mapping in dependency order so child processor
// passes find their parent already typed.
func TestChildResources(t *testing.T) {
	got := ChildResources(rawoutput.ResourceProperty)
	want := []rawoutput.Resource{
		rawoutput.ResourceMedia,
		rawoutput.ResourcePropertyRooms,
		rawoutput.ResourcePropertyUnitTypes,
	}
	assert.Equal(t, want, got, "Property must trigger exactly these three child passes, in this order")

	// Every other resource has no children — the splitter only fires
	// for Property. If a new expand-only relationship is ever added
	// (e.g. Office.Members), this test must be extended consciously.
	for _, r := range []rawoutput.Resource{
		rawoutput.ResourceLookup,
		rawoutput.ResourceOffice,
		rawoutput.ResourceMember,
		rawoutput.ResourceOpenHouse,
		rawoutput.ResourceMedia,
		rawoutput.ResourcePropertyRooms,
		rawoutput.ResourcePropertyUnitTypes,
	} {
		assert.Empty(t, ChildResources(r), "%s must have no child resources", r)
	}
}

// TestIsExpandOnly guards the CLI gate that returns a friendly error
// from `import Media` / `sync Media`.
func TestIsExpandOnly(t *testing.T) {
	for _, r := range []rawoutput.Resource{
		rawoutput.ResourceMedia,
		rawoutput.ResourcePropertyRooms,
		rawoutput.ResourcePropertyUnitTypes,
	} {
		assert.True(t, IsExpandOnly(r), "%s is expand-only", r)
	}
	for _, r := range FetchableResources {
		assert.False(t, IsExpandOnly(r), "%s is fetchable, not expand-only", r)
	}
}
