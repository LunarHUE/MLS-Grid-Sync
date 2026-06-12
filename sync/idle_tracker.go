package sync

// IdleTracker tracks the busy→empty edge so the worker's polling loop emits
// the "no pending jobs, idling" log exactly once per transition instead of
// every poll tick. A 2-second poll loop spamming the line every tick is
// how log files become landfill.
//
// Starts in the busy state so the first poll that returns no work yields
// a transition (treated as busy-on-startup → empty), which is the right
// behavior on cold start when nothing is enqueued yet.
type IdleTracker struct {
	busy bool
}

// NewIdleTracker returns a tracker primed to emit on the first empty poll.
func NewIdleTracker() *IdleTracker {
	return &IdleTracker{busy: true}
}

// Record processes a poll result. Returns true exactly on a busy→empty
// edge — the caller should emit the idle log only in that case.
// Returns false on any worked poll (resetting the busy flag) and on any
// subsequent empty poll (no transition).
func (t *IdleTracker) Record(worked bool) bool {
	if worked {
		t.busy = true
		return false
	}
	if t.busy {
		t.busy = false
		return true
	}
	return false
}
