package cmd

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/mls"
	pkgsync "github.com/LunarHUE/MLS-Grid-Sync/sync"
	"github.com/LunarHUE/MLS-Grid-Sync/sync/processor"
)

// fakeInitialRunner records the resource of every RunInitial call and
// can fail on a chosen resource. Used to exercise the orchestration loop
// without a database.
type fakeInitialRunner struct {
	calls       []rawoutput.Resource
	failOn      rawoutput.Resource
	failWithErr error
}

func (f *fakeInitialRunner) RunInitial(_ context.Context, _, _, _ string, resource rawoutput.Resource) error {
	f.calls = append(f.calls, resource)
	if resource == f.failOn {
		if f.failWithErr != nil {
			return f.failWithErr
		}
		return errors.New("fake halt")
	}
	return nil
}

func TestRunInit_OrderMatchesFetchableResources(t *testing.T) {
	r := &fakeInitialRunner{}
	err := runInit(context.Background(), r, "src", "v2", "actris", processor.FetchableResources, nil)
	require.NoError(t, err)

	assert.Equal(t, processor.FetchableResources, r.calls,
		"init must invoke resources in FetchableResources order (FK-dependency safe; expand-only resources ride Property)")
}

func TestRunInit_SkipDropsNamedResourcesWithoutReordering(t *testing.T) {
	r := &fakeInitialRunner{}
	skip := map[rawoutput.Resource]bool{
		rawoutput.ResourceOffice:    true,
		rawoutput.ResourceOpenHouse: true,
	}
	err := runInit(context.Background(), r, "src", "v2", "actris", processor.FetchableResources, skip)
	require.NoError(t, err)

	want := []rawoutput.Resource{
		rawoutput.ResourceLookup,
		// Office skipped
		rawoutput.ResourceMember,
		rawoutput.ResourceProperty,
		// OpenHouse skipped
	}
	assert.Equal(t, want, r.calls)
}

func TestRunInit_HaltsOnFirstError(t *testing.T) {
	r := &fakeInitialRunner{failOn: rawoutput.ResourceProperty}
	err := runInit(context.Background(), r, "src", "v2", "actris", processor.FetchableResources, nil)
	require.Error(t, err)
	// Halt error formats the resource in operator-facing MLS form.
	assert.Contains(t, err.Error(), mls.ResourceProperty, "halt error should name the failed resource (MLS form)")

	// Earlier resources ran; Property is the last entry.
	wantSoFar := []rawoutput.Resource{
		rawoutput.ResourceLookup,
		rawoutput.ResourceOffice,
		rawoutput.ResourceMember,
		rawoutput.ResourceProperty,
	}
	assert.Equal(t, wantSoFar, r.calls, "halt MUST stop the chain — no later resource may be called after the failure")
}

func TestRunInit_HaltErrorMentionsRetryGuidance(t *testing.T) {
	r := &fakeInitialRunner{failOn: rawoutput.ResourceMember, failWithErr: errors.New("FK violation")}
	err := runInit(context.Background(), r, "src", "v2", "actris", processor.FetchableResources, nil)
	require.Error(t, err)

	msg := err.Error()
	// Operator-facing message should hint at safe re-run, --skip, and
	// surface the underlying error.
	assert.Contains(t, msg, "safe to re-run")
	assert.Contains(t, msg, "--skip")
	assert.Contains(t, msg, "FK violation", "underlying error must be preserved")
}

// TestRunInit_HaltNamesFailingPassWithRidingStep covers the case the
// halt formatter was reworked for: Property's step runs passes for
// media / property_rooms / property_unit_types after the property pass;
// when one of those fails, operators need to see the failing PASS up
// front, with the step as parenthetical context. Without this, the
// halt message says "init halted on Property" while the stack trace
// points at the media processor — accurate but actively misleading
// at the worst possible moment.
func TestRunInit_HaltNamesFailingPassWithRidingStep(t *testing.T) {
	r := &fakeInitialRunner{
		failOn: rawoutput.ResourceProperty,
		failWithErr: &pkgsync.PassError{
			Pass: rawoutput.ResourceMedia,
			Err:  errors.New("parse: missing required field MediaKey"),
		},
	}
	err := runInit(context.Background(), r, "src", "v2", "actris", processor.FetchableResources, nil)
	require.Error(t, err)

	msg := err.Error()
	// Failing pass named first, in MLS form.
	assert.Contains(t, msg, "init halted on "+mls.ResourceMedia,
		"halt message must lead with the FAILING PASS, not the step")
	// Step appears as parenthetical context.
	assert.Contains(t, msg, "(riding "+mls.ResourceProperty+" step)",
		"halt message must name the riding step for context")
	// Underlying error preserved.
	assert.Contains(t, msg, "MediaKey")
}

// TestRunInit_HaltOnStepFailureNamesStep covers the contrapositive:
// when the failure is NOT a PassError (e.g. fetch or save failed for
// the step's own resource), the halt formatter names the step without
// a riding parenthetical.
func TestRunInit_HaltOnStepFailureNamesStep(t *testing.T) {
	r := &fakeInitialRunner{
		failOn:      rawoutput.ResourceProperty,
		failWithErr: errors.New("page 5 fetch: connection reset"),
	}
	err := runInit(context.Background(), r, "src", "v2", "actris", processor.FetchableResources, nil)
	require.Error(t, err)

	msg := err.Error()
	assert.Contains(t, msg, "init halted on "+mls.ResourceProperty)
	assert.NotContains(t, msg, "riding",
		"non-pass failures must NOT print a riding-step parenthetical")
}

func TestParseSkipList(t *testing.T) {
	cases := []struct {
		in      string
		want    map[rawoutput.Resource]bool
		wantErr bool
	}{
		{"", map[rawoutput.Resource]bool{}, false},
		{"Office", map[rawoutput.Resource]bool{rawoutput.ResourceOffice: true}, false},
		{"Office,Media", map[rawoutput.Resource]bool{rawoutput.ResourceOffice: true, rawoutput.ResourceMedia: true}, false},
		{"office,media", map[rawoutput.Resource]bool{rawoutput.ResourceOffice: true, rawoutput.ResourceMedia: true}, false},
		{" Office , Media ", map[rawoutput.Resource]bool{rawoutput.ResourceOffice: true, rawoutput.ResourceMedia: true}, false},
		{"Office,Bogus", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseSkipList(tc.in)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
