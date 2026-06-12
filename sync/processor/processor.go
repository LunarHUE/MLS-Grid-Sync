package processor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/lunarhue/libs-go/log"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/processorcursor"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/version"
)


// DefaultBatchSize is the number of raw_output rows fetched per round-trip.
// It does not affect transaction granularity — each row is processed in its
// own transaction regardless.
const DefaultBatchSize = 500

// Outcome is the per-record result a ResourceProcessor reports to the loop.
// Counters accumulate by Outcome into PassStats, which renders the periodic
// progress and pass-end log lines. Phase 6 metrics will export these as the
// processor's contribution.
//
// Insert/Update/Delete are the three writing outcomes; the three Skip*
// outcomes record decisions where the loop advanced the cursor without
// touching the entity. The invariant Processed == sum-of-six is asserted in
// processor_test.go and is what catches a future processor returning an
// unannotated path.
type Outcome int

const (
	OutcomeUnknown Outcome = iota
	OutcomeInsert
	OutcomeUpdate
	OutcomeDelete
	OutcomeSkipStale
	OutcomeSkipNoDiff
	OutcomeSkipTombstoned
)

// String renders the outcome for log lines. Stable strings; treat them as
// part of the operator interface.
func (o Outcome) String() string {
	switch o {
	case OutcomeInsert:
		return "insert"
	case OutcomeUpdate:
		return "update"
	case OutcomeDelete:
		return "delete"
	case OutcomeSkipStale:
		return "skip-stale"
	case OutcomeSkipNoDiff:
		return "skip-no-diff"
	case OutcomeSkipTombstoned:
		return "skip-tombstoned"
	}
	return "unknown"
}

// PassStats accumulates per-Outcome counts over a single RunPass and seeds
// the Phase 6 metrics export. Processed must equal the sum of the six
// outcome counters; a drift means a processor returned an unannotated path.
type PassStats struct {
	Resource       rawoutput.Resource
	StartedAt      time.Time
	Processed      int
	Inserts        int
	Updates        int
	Deletes        int
	SkipStale      int
	SkipNoDiff     int
	SkipTombstoned int
}

// record bumps Processed and the matching counter. Kept on the struct so
// the loop and the tests share one accounting code path.
func (s *PassStats) record(o Outcome) {
	s.Processed++
	switch o {
	case OutcomeInsert:
		s.Inserts++
	case OutcomeUpdate:
		s.Updates++
	case OutcomeDelete:
		s.Deletes++
	case OutcomeSkipStale:
		s.SkipStale++
	case OutcomeSkipNoDiff:
		s.SkipNoDiff++
	case OutcomeSkipTombstoned:
		s.SkipTombstoned++
	}
}

// summary renders the trailing counts of a progress or pass-end line. The
// "skip" bucket folds the three skip outcomes — they're individually useful
// for metrics but indistinguishable to a human watching log output.
func (s *PassStats) summary() string {
	skip := s.SkipStale + s.SkipNoDiff + s.SkipTombstoned
	return fmt.Sprintf("%d ins, %d upd, %d skip, %d del",
		s.Inserts, s.Updates, skip, s.Deletes)
}

// ResourceProcessor processes one raw_output row inside a caller-provided
// transaction. The generic loop in Processor owns the cursor, the lock, and
// the per-record transaction boundary; Process owns the semantic per-row work.
//
// Process MUST NOT advance the cursor itself, and MUST NOT commit/rollback
// the tx — that is the loop's responsibility. The returned Outcome reports
// which decision branch ran; the loop accumulates it into PassStats and the
// per-record DEBUG log line. Returning an error halts the pass
// (poison-record policy) so the offending raw_output_id can be inspected.
//
// Process runs inside an open per-record transaction. Nothing slow — no
// subprocess, no network, no unbounded computation — belongs in this path;
// it extends transaction hold time for every record. (version.Info() forked
// git per record until 2026-06; see docs/profiling.md for the case study.)
type ResourceProcessor interface {
	Resource() rawoutput.Resource
	Process(ctx context.Context, tx *ent.Tx, raw *ent.RawOutput) (Outcome, error)
}

// AfterPasser is an optional interface a ResourceProcessor can implement to
// run a one-shot side-effect once the batch loop drains (still inside the
// advisory lock). Used by PropertyProcessor to re-link child entities
// (rooms, unit_types, open_houses) whose parent_listing_key was NULL because
// the parent wasn't yet processed at child-write time. Takes *sql.DB so the
// hook can run plain UPDATE statements (ent.Tx doesn't surface raw SQL).
type AfterPasser interface {
	AfterPass(ctx context.Context, db *sql.DB) error
}

// Processor multiplexes ResourceProcessors by resource and runs cursor-driven
// passes over the raw_output stream.
type Processor struct {
	client     *ent.Client
	db         *sql.DB
	processors map[rawoutput.Resource]ResourceProcessor
	batchSize  int
}

// New constructs a Processor with the given per-resource processors registered.
// Pass *sql.DB explicitly so the advisory-lock helper can pin a *sql.Conn (see
// lock.go for why this matters).
func New(client *ent.Client, db *sql.DB, ps ...ResourceProcessor) *Processor {
	m := make(map[rawoutput.Resource]ResourceProcessor, len(ps))
	for _, p := range ps {
		m[p.Resource()] = p
	}
	return &Processor{
		client:     client,
		db:         db,
		processors: m,
		batchSize:  DefaultBatchSize,
	}
}

// WithBatchSize sets a custom batch size (useful in tests). Returns p for chaining.
func (p *Processor) WithBatchSize(n int) *Processor {
	if n > 0 {
		p.batchSize = n
	}
	return p
}

// RunPass advances the cursor for resource by processing every raw_output row
// strictly after the cursor's last_raw_output_id. It acquires the per-resource
// advisory lock for the duration of the pass and returns once the batch
// query returns zero rows. Any per-record error halts and returns the error
// (the cursor stops where it stopped — re-running RunPass resumes there).
//
// If no ResourceProcessor is registered for the resource, RunPass returns
// ErrNoProcessor. Callers (sync.Service) treat this as a non-failure and log
// it at info level; raw_output rows are still safely persisted regardless.
//
// The accumulated PassStats are logged but not returned; tests reach the
// stats through runPassWithStats.
func (p *Processor) RunPass(ctx context.Context, resource rawoutput.Resource) error {
	_, err := p.runPassWithStats(ctx, resource)
	return err
}

// runPassWithStats is the bare loop with the PassStats accumulation exposed
// to the caller. RunPass wraps it for production code; the loop test
// asserts the stats-sum invariant directly against the returned value.
func (p *Processor) runPassWithStats(ctx context.Context, resource rawoutput.Resource) (PassStats, error) {
	proc, ok := p.processors[resource]
	if !ok {
		return PassStats{}, fmt.Errorf("%w: %s", ErrNoProcessor, resource)
	}

	release, err := acquireResourceLock(ctx, p.db, string(resource))
	if err != nil {
		return PassStats{}, fmt.Errorf("processor[%s]: %w", resource, err)
	}
	defer release()

	cursor, err := p.loadOrCreateCursor(ctx, resource)
	if err != nil {
		return PassStats{}, fmt.Errorf("processor[%s]: load cursor: %w", resource, err)
	}

	stats := PassStats{Resource: resource, StartedAt: time.Now()}
	if cursor.LastRawOutputID != nil {
		log.Infof("processor[%s]: starting pass from cursor %s", resource, *cursor.LastRawOutputID)
	} else {
		log.Infof("processor[%s]: starting pass from beginning", resource)
	}

	for {
		batch, err := p.fetchBatch(ctx, resource, cursor.LastRawOutputID)
		if err != nil {
			return stats, fmt.Errorf("processor[%s]: fetch batch: %w", resource, err)
		}
		if len(batch) == 0 {
			p.logPassComplete(&stats)
			return stats, p.runAfterPass(ctx, proc, resource)
		}

		for _, raw := range batch {
			outcome, err := p.processOne(ctx, proc, resource, raw)
			if err != nil {
				return stats, fmt.Errorf("processor[%s] raw_output=%s: %w", resource, raw.ID, err)
			}
			stats.record(outcome)
			log.Debugf("processor[%s]: %s %s", resource, raw.SourceKey, outcome)
			// Update the in-memory cursor so the next batch query advances.
			id := raw.ID
			cursor.LastRawOutputID = &id
		}

		// One progress line per drained batch — the cadence matches
		// DefaultBatchSize so an operator watching at INFO sees a heartbeat
		// every few seconds during a heavy pass.
		log.Infof("processor[%s]: %d processed (%s), cursor %s",
			resource, stats.Processed, stats.summary(), *cursor.LastRawOutputID)
	}
}

// logPassComplete emits the end-of-pass INFO line. Splits into a quiet form
// when no records were processed (the common delta-cycle case) and the full
// stats+rate form otherwise.
func (p *Processor) logPassComplete(stats *PassStats) {
	if stats.Processed == 0 {
		log.Infof("processor[%s]: pass complete — nothing to process", stats.Resource)
		return
	}
	elapsed := time.Since(stats.StartedAt)
	rate := float64(stats.Processed) / elapsed.Seconds()
	log.Infof("processor[%s]: pass complete — %d records in %s (%.1f/s): %s",
		stats.Resource, stats.Processed, elapsed.Round(time.Second), rate, stats.summary())
}

// runAfterPass invokes the optional AfterPass hook if the processor implements
// it. Errors are returned to the caller (logged by sync.Service) but the
// already-committed per-record cursor advances are not undone.
func (p *Processor) runAfterPass(ctx context.Context, proc ResourceProcessor, resource rawoutput.Resource) error {
	hook, ok := proc.(AfterPasser)
	if !ok {
		return nil
	}
	if err := hook.AfterPass(ctx, p.db); err != nil {
		return fmt.Errorf("processor[%s]: after-pass: %w", resource, err)
	}
	return nil
}

// loadOrCreateCursor returns the processor_cursor row for resource, creating
// one with NULL last_raw_output_id on first call.
func (p *Processor) loadOrCreateCursor(ctx context.Context, resource rawoutput.Resource) (*ent.ProcessorCursor, error) {
	row, err := p.client.ProcessorCursor.Query().
		Where(processorcursor.ResourceEQ(processorcursor.Resource(resource))).
		Only(ctx)
	if err == nil {
		return row, nil
	}
	if !ent.IsNotFound(err) {
		return nil, err
	}
	return p.client.ProcessorCursor.Create().
		SetResource(processorcursor.Resource(resource)).
		SetProcessorVersion(version.Info()).
		Save(ctx)
}

// fetchBatch returns up to batchSize raw_output rows for resource, ordered by
// id (UUIDv7 — time-orderable), strictly greater than after (or all rows if
// after is nil).
func (p *Processor) fetchBatch(ctx context.Context, resource rawoutput.Resource, after *uuid.UUID) ([]*ent.RawOutput, error) {
	q := p.client.RawOutput.Query().
		Where(rawoutput.ResourceEQ(resource)).
		Order(ent.Asc(rawoutput.FieldID)).
		Limit(p.batchSize)
	if after != nil {
		q = q.Where(rawoutput.IDGT(*after))
	}
	return q.All(ctx)
}

// processOne runs one raw_output through proc.Process inside a transaction
// that also advances the cursor. Both writes commit atomically; rollback on
// any error. Returns the processor's reported Outcome so the loop can
// accumulate it into PassStats.
func (p *Processor) processOne(ctx context.Context, proc ResourceProcessor, resource rawoutput.Resource, raw *ent.RawOutput) (Outcome, error) {
	tx, err := p.client.Tx(ctx)
	if err != nil {
		return OutcomeUnknown, fmt.Errorf("begin tx: %w", err)
	}

	outcome, err := proc.Process(ctx, tx, raw)
	if err != nil {
		return OutcomeUnknown, rollback(tx, fmt.Errorf("process: %w", err))
	}

	if _, err := tx.ProcessorCursor.Update().
		Where(processorcursor.ResourceEQ(processorcursor.Resource(resource))).
		SetLastRawOutputID(raw.ID).
		SetProcessorVersion(version.Info()).
		Save(ctx); err != nil {
		return OutcomeUnknown, rollback(tx, fmt.Errorf("advance cursor: %w", err))
	}

	if err := tx.Commit(); err != nil {
		return OutcomeUnknown, fmt.Errorf("commit: %w", err)
	}
	return outcome, nil
}

// rollback combines tx rollback with the original error, preserving the
// underlying cause for the caller.
func rollback(tx *ent.Tx, err error) error {
	if rerr := tx.Rollback(); rerr != nil {
		return errors.Join(err, fmt.Errorf("rollback: %w", rerr))
	}
	return err
}
