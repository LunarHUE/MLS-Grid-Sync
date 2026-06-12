package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestIdleTracker_LogsOncePerTransition is the contract test for the
// cmd/worker.go polling loop: across N empty polls the "idling" line is
// emitted exactly once, then again only when the worker goes back to busy
// and drains again.
//
// The plan calls for "exactly two transition logs" in the canonical case:
// initial busy → empty (log 1), several empties (no log), back to work
// (no log), drain again (log 2).
func TestIdleTracker_LogsOncePerTransition(t *testing.T) {
	tracker := NewIdleTracker()

	// Sequence: empty, empty, empty, busy, busy, empty, empty
	// The "1" entries mark expected transition emissions.
	steps := []struct {
		worked bool
		emits  bool
	}{
		{worked: false, emits: true},  // initial busy → empty
		{worked: false, emits: false}, // still idle
		{worked: false, emits: false}, // still idle
		{worked: true, emits: false},  // back to work — no log
		{worked: true, emits: false},  // still busy
		{worked: false, emits: true},  // busy → empty again
		{worked: false, emits: false}, // still idle
	}

	var emits int
	for i, s := range steps {
		got := tracker.Record(s.worked)
		assert.Equal(t, s.emits, got, "step %d (worked=%v)", i, s.worked)
		if got {
			emits++
		}
	}

	assert.Equal(t, 2, emits, "exactly two transition emissions across the sequence")
}

// TestIdleTracker_AllEmptyEmitsOnce exercises the edge the plan calls out
// explicitly: N consecutive empty polls produce exactly one idle log.
func TestIdleTracker_AllEmptyEmitsOnce(t *testing.T) {
	tracker := NewIdleTracker()

	emits := 0
	for i := 0; i < 20; i++ {
		if tracker.Record(false) {
			emits++
		}
	}
	assert.Equal(t, 1, emits, "20 empty polls → 1 idle line")
}

// TestIdleTracker_AllBusyNeverEmits — a continuously-busy worker never
// emits the idle line at all.
func TestIdleTracker_AllBusyNeverEmits(t *testing.T) {
	tracker := NewIdleTracker()

	for i := 0; i < 20; i++ {
		assert.False(t, tracker.Record(true), "busy poll never triggers idle log")
	}
}
