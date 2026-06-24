package processor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/processorcursor"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/applog"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/progress"
	"github.com/LunarHUE/MLS-Grid-Sync/version"
)

// DefaultBatchSize is the number of raw_output rows fetched per round-trip.
// It bounds how many rows a single fetch returns; the commit granularity is
// governed separately by DefaultCommitBatchSize.
const DefaultBatchSize = 500

// DefaultCommitBatchSize is the number of records committed per transaction in
// the typed pass. Batching amortizes the per-record COMMIT/fsync and cursor
// write that dominate the I/O-bound pass (docs/profiling.md, R2). A batch error
// triggers a one-record-per-tx fallback so the exact poison record is still
// pinpointed. Overridable via config / --commit-batch-size; 1 means
// one-record-per-tx (the historical behavior).
const DefaultCommitBatchSize = 100

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

// ChunkProcessor is an optional interface a ResourceProcessor can implement to
// project a whole commit-chunk in BULK — one set of batched SQL statements
// instead of the per-record Process round-trips — to cut the latency-bound cost
// of the projection (docs/profiling.md, R4). It runs inside the same chunk
// transaction the loop owns and MUST NOT advance the cursor or commit/rollback.
//
// It returns exactly one Outcome per input raw, in order (driving PassStats and
// the Processed == sum-of-six invariant). On error the loop rolls back and
// replays the chunk one-record-per-tx via Process, pinpointing the poison
// record — identical to the per-record halt semantics, so a ChunkProcessor must
// also implement Process. Used only when bulk projection is enabled
// (Processor.WithBulk); otherwise the loop always uses Process.
type ChunkProcessor interface {
	ResourceProcessor
	ProcessChunk(ctx context.Context, tx *ent.Tx, raws []*ent.RawOutput) ([]Outcome, error)
}

// Processor multiplexes ResourceProcessors by resource and runs cursor-driven
// passes over the raw_output stream.
type Processor struct {
	client          *ent.Client
	db              *sql.DB
	processors      map[rawoutput.Resource]ResourceProcessor
	batchSize       int
	commitBatchSize int
	bulk            bool
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
		client:          client,
		db:              db,
		processors:      m,
		batchSize:       DefaultBatchSize,
		commitBatchSize: DefaultCommitBatchSize,
		bulk:            true,
	}
}

// WithBatchSize sets a custom batch size (useful in tests). Returns p for chaining.
func (p *Processor) WithBatchSize(n int) *Processor {
	if n > 0 {
		p.batchSize = n
	}
	return p
}

// WithCommitBatchSize sets how many records commit per transaction. n <= 0 is
// ignored (keeps the current value); n == 1 restores one-record-per-tx.
// Returns p for chaining.
func (p *Processor) WithCommitBatchSize(n int) *Processor {
	if n > 0 {
		p.commitBatchSize = n
	}
	return p
}

// WithBulk toggles the bulk-projection path: when enabled (default) a chunk is
// projected via ProcessChunk for processors that implement ChunkProcessor;
// disabled forces the per-record Process path for every resource. Returns p for
// chaining. The flag is the operator kill-switch (processor.bulk) for the
// batched writes.
func (p *Processor) WithBulk(b bool) *Processor {
	p.bulk = b
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
	_, err := p.runPassWithStats(ctx, resource, true)
	return err
}

// RunPassNoFinalize advances the cursor exactly like RunPass but SKIPS the
// optional AfterPass finalize hook (the Property child-relink). It exists for
// the pipelined init path (sync.fetchProcessAndEnqueuePipelined), where the
// processor consumer drains repeatedly as pages stream in: running the
// full-table relink after every page would be quadratic. The caller runs one
// final RunPass once pagination completes to perform the relink exactly once.
func (p *Processor) RunPassNoFinalize(ctx context.Context, resource rawoutput.Resource) error {
	_, err := p.runPassWithStats(ctx, resource, false)
	return err
}

// runPassWithStats is the bare loop with the PassStats accumulation exposed
// to the caller. RunPass wraps it for production code; the loop test
// asserts the stats-sum invariant directly against the returned value.
//
// finalize gates the AfterPass hook at drain: RunPass passes true (full
// semantics); the streaming consumer passes false to drain without relinking.
func (p *Processor) runPassWithStats(ctx context.Context, resource rawoutput.Resource, finalize bool) (PassStats, error) {
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
	// Streaming drains (finalize=false) run once per fetched page, concurrently
	// with the producer's fetch logging — stay silent to avoid racing the
	// shared libs-go log buffer. The finalize pass logs normally.
	if finalize {
		if cursor.LastRawOutputID != nil {
			applog.Infof("processor[%s]: starting pass from cursor %s", resource, *cursor.LastRawOutputID)
		} else {
			applog.Infof("processor[%s]: starting pass from beginning", resource)
		}
	}

	// Process lane: count the rows this pass will type so the bar shows how much
	// is left. Best-effort — a count error just leaves the lane untouched (it's
	// display only, never a reason to fail the pass). In the default
	// fetch-then-process path this count is exact and the bar fills 0→100% for
	// the resource; in the pipelined path each wake's pass shows its own slice.
	lane := progress.Process()
	laneActive := false
	if pending, cerr := p.countPending(ctx, resource, cursor.LastRawOutputID); cerr == nil && pending > 0 {
		lane.Start(string(resource), pending)
		laneActive = true
		defer lane.Done()
	}
	batchBaseline := 0

	for {
		batch, err := p.fetchBatch(ctx, resource, cursor.LastRawOutputID)
		if err != nil {
			return stats, fmt.Errorf("processor[%s]: fetch batch: %w", resource, err)
		}
		if len(batch) == 0 {
			if finalize {
				p.logPassComplete(&stats)
				return stats, p.runAfterPass(ctx, proc, resource)
			}
			// Streaming drain (finalize=false): nothing left to drain this wake.
			// Per-batch heartbeats below already reported progress, so return
			// quietly and wait for the next page signal.
			return stats, nil
		}

		// Commit the fetched batch in sub-chunks of commitBatchSize, each in
		// one transaction. processChunk advances the in-memory cursor + stats
		// only after its tx commits.
		for start := 0; start < len(batch); start += p.commitBatchSize {
			end := start + p.commitBatchSize
			if end > len(batch) {
				end = len(batch)
			}
			if err := p.processChunk(ctx, proc, resource, batch[start:end], cursor, &stats, finalize); err != nil {
				return stats, err
			}
		}

		// Advance the Process bar by what this batch typed. The bar is the
		// operator-facing heartbeat now; the per-batch line is DEBUG.
		if laneActive {
			lane.Add(stats.Processed - batchBaseline)
			batchBaseline = stats.Processed
		}
		applog.Debugf("processor[%s]: %d processed (%s), cursor %s",
			resource, stats.Processed, stats.summary(), *cursor.LastRawOutputID)
	}
}

// countPending returns how many raw_output rows this resource still has to
// process strictly after the cursor — the Process lane's denominator. It
// mirrors fetchBatch's filter so it counts exactly the rows the pass will read.
func (p *Processor) countPending(ctx context.Context, resource rawoutput.Resource, after *uuid.UUID) (int, error) {
	q := p.client.RawOutput.Query().Where(rawoutput.ResourceEQ(resource))
	if after != nil {
		q = q.Where(rawoutput.IDGT(*after))
	}
	return q.Count(ctx)
}

// logPassComplete emits the end-of-pass INFO line. Splits into a quiet form
// when no records were processed (the common delta-cycle case) and the full
// stats+rate form otherwise.
func (p *Processor) logPassComplete(stats *PassStats) {
	if stats.Processed == 0 {
		applog.Infof("processor[%s]: pass complete — nothing to process", stats.Resource)
		return
	}
	elapsed := time.Since(stats.StartedAt)
	rate := float64(stats.Processed) / elapsed.Seconds()
	applog.Infof("processor[%s]: pass complete — %d records in %s (%.1f/s): %s",
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

// processChunk runs up to commitBatchSize records in ONE transaction, writing
// the cursor once (the chunk's last record) so the cursor advance commits
// atomically with the entity writes — the replayability invariant relaxed
// durability relies on (docs/profiling.md). Stats and the in-memory cursor
// advance only after the tx commits.
//
// If a record's Process errors mid-chunk, the whole tx rolls back and the chunk
// is replayed one-record-per-tx via processOne, so the cursor stops exactly
// before the poison record and the returned error names its raw_output id —
// identical to the unbatched halt semantics. Rolled-back records are never
// counted, so PassStats stays honest.
func (p *Processor) processChunk(ctx context.Context, proc ResourceProcessor, resource rawoutput.Resource, chunk []*ent.RawOutput, cursor *ent.ProcessorCursor, stats *PassStats, finalize bool) error {
	// A single-record chunk (commit_batch_size == 1, or a lone trailing record)
	// is exactly the historical per-record path — go straight to processOne so
	// a poison record is attempted once, not once-then-replayed.
	if len(chunk) == 1 {
		outcome, err := p.processOne(ctx, proc, resource, chunk[0])
		if err != nil {
			return fmt.Errorf("processor[%s] raw_output=%s: %w", resource, chunk[0].ID, err)
		}
		p.recordProcessed(resource, chunk[0], outcome, cursor, stats, finalize)
		return nil
	}

	outcomes, committed, err := p.tryCommitChunk(ctx, proc, resource, chunk)
	if err != nil {
		return err
	}
	if committed {
		for i, raw := range chunk {
			p.recordProcessed(resource, raw, outcomes[i], cursor, stats, finalize)
		}
		return nil
	}

	// Fallback: a per-record Process error somewhere in the chunk. Replay the
	// chunk one tx at a time to pinpoint (and halt at) the poison record.
	for _, raw := range chunk {
		outcome, err := p.processOne(ctx, proc, resource, raw)
		if err != nil {
			return fmt.Errorf("processor[%s] raw_output=%s: %w", resource, raw.ID, err)
		}
		p.recordProcessed(resource, raw, outcome, cursor, stats, finalize)
	}
	return nil
}

// recordProcessed folds one committed record into stats and advances the
// in-memory cursor so the next fetchBatch pages past it. The per-record DEBUG
// line is suppressed during a streaming drain (finalize=false): it routes
// through applog (so it is race-safe against the concurrent producer), but at
// one line per record it would still flood the output mid-stream — the
// per-batch heartbeat already reports streaming progress.
func (p *Processor) recordProcessed(resource rawoutput.Resource, raw *ent.RawOutput, outcome Outcome, cursor *ent.ProcessorCursor, stats *PassStats, finalize bool) {
	stats.record(outcome)
	if finalize {
		applog.Debugf("processor[%s]: %s %s", resource, raw.SourceKey, outcome)
	}
	id := raw.ID
	cursor.LastRawOutputID = &id

	// Sampled field-drift diagnostic: cheap (one rand compare) for the vast
	// majority of records; re-parses only the sampled fraction. Runs here,
	// after the record has committed, so it is fully off the transaction path.
	checkFieldDrift(resource, raw)
}

// tryCommitChunk runs the whole chunk in a single transaction. Returns
// committed=true with per-record outcomes on success; committed=false (err==nil)
// when a record's Process errored and the caller should replay one-record-per-tx;
// err!=nil for an infrastructure failure (begin / cursor write / commit /
// rollback) that must halt the pass.
func (p *Processor) tryCommitChunk(ctx context.Context, proc ResourceProcessor, resource rawoutput.Resource, chunk []*ent.RawOutput) ([]Outcome, bool, error) {
	tx, err := p.client.Tx(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("begin tx: %w", err)
	}

	outcomes, perr := p.projectChunk(ctx, proc, tx, chunk)
	if perr != nil {
		// Roll back and signal the caller to replay record-by-record, which
		// reproduces and names the poison record. perr itself is discarded —
		// the deterministic replay surfaces it with the offending id. This holds
		// for a bulk ProcessChunk failure too: the per-record replay re-runs the
		// chunk through Process and halts at (and names) the offending record.
		if rerr := tx.Rollback(); rerr != nil {
			return nil, false, errors.Join(fmt.Errorf("process: %w", perr), fmt.Errorf("rollback: %w", rerr))
		}
		return nil, false, nil
	}

	// Advance the cursor ONCE, to the chunk's last record, inside this tx.
	last := chunk[len(chunk)-1]
	if _, err := tx.ProcessorCursor.Update().
		Where(processorcursor.ResourceEQ(processorcursor.Resource(resource))).
		SetLastRawOutputID(last.ID).
		SetProcessorVersion(version.Info()).
		Save(ctx); err != nil {
		return nil, false, rollback(tx, fmt.Errorf("advance cursor: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit: %w", err)
	}
	return outcomes, true, nil
}

// projectChunk produces the per-record outcomes for the chunk on the open tx,
// choosing the bulk path when enabled and supported, else the per-record loop.
// It never commits/rolls back — the caller owns the tx. A returned error means
// "this chunk failed; replay per-record" (poison pinpointing).
func (p *Processor) projectChunk(ctx context.Context, proc ResourceProcessor, tx *ent.Tx, chunk []*ent.RawOutput) ([]Outcome, error) {
	if p.bulk {
		if bp, ok := proc.(ChunkProcessor); ok {
			return bp.ProcessChunk(ctx, tx, chunk)
		}
	}
	outcomes := make([]Outcome, 0, len(chunk))
	for _, raw := range chunk {
		outcome, perr := proc.Process(ctx, tx, raw)
		if perr != nil {
			return nil, fmt.Errorf("process: %w", perr)
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes, nil
}

// rollback combines tx rollback with the original error, preserving the
// underlying cause for the caller.
func rollback(tx *ent.Tx, err error) error {
	if rerr := tx.Rollback(); rerr != nil {
		return errors.Join(err, fmt.Errorf("rollback: %w", rerr))
	}
	return err
}
