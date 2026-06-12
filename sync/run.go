package sync

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/lunarhue/libs-go/log"

	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/rawoutput"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/syncevent"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/version"
)

// RunInitial wraps a single resource's initial-import lifecycle: create
// the sync_event in `running`, call InitialSync, and stamp the outcome.
// On success the high_water_mark is the max source_modified_at observed
// (or the run-start time for a zero-record import, so the next delta
// starts from the import boundary, not a month earlier).
//
// Returns the InitialSync error so callers can decide whether to halt
// (init / import propagate; the sync daemon never calls this path).
//
// Lifecycle is owned here so cmd/import.go, cmd/init.go, and any future
// caller never replicate the create-running → stamp-outcome dance. A
// third call site that quietly skipped the stamp would silently break
// the success-gated HWM cursor (Phase 4 §7).
func (s *Service) RunInitial(ctx context.Context, srcSystemID, v2URL, originatingSystem string, resource rawoutput.Resource) error {
	apiName, err := DBToMLSResource(resource)
	if err != nil {
		return err
	}
	runStart := time.Now()

	ev, err := s.dbClient.SyncEvent.Create().
		SetRunType(syncevent.RunTypeBackfill).
		SetSourceSystemID(srcSystemID).
		SetResource(syncevent.Resource(resource)).
		SetStatus(syncevent.StatusRunning).
		SetProcessorVersion(version.Info()).
		SetStartedAt(runStart).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("create sync_event: %w", err)
	}

	hwm, syncErr := s.InitialSync(ctx, ev.ID, v2URL, originatingSystem, apiName)
	s.stampOutcome(ctx, ev.ID, hwm, syncErr, runStart.UTC())
	return syncErr
}

// RunDelta wraps one delta-sync cycle: read HWM from the most recent
// successful event (falling back to "one month ago" on first-ever run),
// create the sync_event in `running`, call SyncResource, stamp outcome.
// Zero-record success carries the cursor forward so the next cycle
// reads from the same boundary instead of resetting to the fallback.
//
// Returns the SyncResource error; the daemon explicitly log+continues
// per-cycle errors so the loop survives transient failures.
func (s *Service) RunDelta(ctx context.Context, srcSystemID, v2URL, originatingSystem string, resource rawoutput.Resource) error {
	apiName, err := DBToMLSResource(resource)
	if err != nil {
		return err
	}

	cursor, err := s.LastSuccessfulHWM(ctx, syncevent.Resource(resource))
	if err != nil && !ent.IsNotFound(err) {
		return fmt.Errorf("read HWM: %w", err)
	}
	if cursor.IsZero() {
		cursor = time.Now().UTC().AddDate(0, -1, 0)
		log.Infof("no prior successful sync for %s — starting from %s", apiName, cursor.Format(time.RFC3339))
	}

	ev, err := s.dbClient.SyncEvent.Create().
		SetRunType(syncevent.RunTypeSync).
		SetSourceSystemID(srcSystemID).
		SetResource(syncevent.Resource(resource)).
		SetStatus(syncevent.StatusRunning).
		SetProcessorVersion(version.Info()).
		SetStartedAt(time.Now()).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("create sync_event: %w", err)
	}

	hwm, syncErr := s.SyncResource(ctx, ev.ID, v2URL, originatingSystem, apiName, cursor)
	s.stampOutcome(ctx, ev.ID, hwm, syncErr, cursor)
	return syncErr
}

// stampOutcome writes the terminal sync_event row: success+hwm on a
// clean run, failed+error_summary on a sync error. zeroRecordFallback
// is the value to stamp when the run succeeded with no new records
// (initial → run-start time; delta → cursor that was queried with).
// Both paths leave hwm NOT NULL on success so the §7 read query
// (which excludes NULL hwm) sees this run.
//
// Stamp-write failures are logged but not returned: the caller cares
// about syncErr, not the stamp write. A stamp failure leaves the event
// `running` and the next-startup stale-sweep reaps it (Phase 4 §8).
func (s *Service) stampOutcome(ctx context.Context, eventID uuid.UUID, hwm time.Time, syncErr error, zeroRecordFallback time.Time) {
	update := s.dbClient.SyncEvent.UpdateOneID(eventID).SetEndedAt(time.Now())
	if syncErr != nil {
		update.
			SetStatus(syncevent.StatusFailed).
			SetErrorSummary(syncErr.Error())
		if _, err := update.Save(ctx); err != nil {
			log.Errorf("stamp sync_event %s as failed: %v", eventID, err)
		}
		return
	}
	stamp := hwm
	if stamp.IsZero() {
		stamp = zeroRecordFallback
	}
	update.
		SetStatus(syncevent.StatusSuccess).
		SetHighWaterMark(stamp)
	if _, err := update.Save(ctx); err != nil {
		log.Errorf("stamp sync_event %s as success: %v", eventID, err)
	}
}
