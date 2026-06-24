package sync

import (
	"context"
	"database/sql"
	"time"

	"github.com/lunarhue/libs-go/log"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/syncevent"
	"github.com/LunarHUE/MLS-Grid-Sync/mls"
	"github.com/LunarHUE/MLS-Grid-Sync/storage"
)

// DefaultStaleRunningThreshold is the wall-clock age past which a
// running sync_event is presumed dead. Anything older was almost
// certainly killed by a host crash or deploy — its high_water_mark was
// never persisted (§7 makes hwm writes conditional on success), so
// re-fetching the failed run's window is the recovery path.
const DefaultStaleRunningThreshold = 2 * time.Hour

// Processor is implemented by sync/processor.Processor — the post-sync
// raw → typed pass. Declared as an interface here to avoid an import cycle
// and to make Service testable with a fake.
//
// RunPassNoFinalize drains a resource exactly like RunPass but skips the
// AfterPass finalize hook. The pipelined init consumer
// (fetchProcessAndEnqueuePipelined) calls it on each page-stream wake, then
// runs one final RunPass to relink once. See sync/processor for details.
type Processor interface {
	RunPass(ctx context.Context, resource rawoutput.Resource) error
	RunPassNoFinalize(ctx context.Context, resource rawoutput.Resource) error
}

// Service coordinates fetching from MLS, persisting raw output, queuing
// attachments, and invoking the typed-record processor after each successful
// sync.
type Service struct {
	mlsClient mls.PageFetcher
	dbClient  *ent.Client
	sqlDB     *sql.DB
	storer    storage.Storer
	processor Processor

	// pipelineInit, when true, makes InitialSync overlap page-fetching with the
	// typed processor (producer/consumer) instead of fetch-then-process. Delta
	// syncs are unaffected. Toggled by cmd/init's --no-pipeline / config.
	pipelineInit bool

	// fetchConcurrency > 1 makes InitialSync fetch pages in parallel via $skip
	// offset paging, overlapping MLS Grid's per-page server latency. <=1 keeps
	// the sequential nextLink path (pipelined or not). Set from mls.fetch_concurrency.
	// Init-only; delta fetches stay sequential (a key may recur across delta
	// pages and must project in raw order, which concurrent out-of-order saves
	// would break). See InitialSync.
	fetchConcurrency int
}

// NewService constructs a Service. processor may be nil when no post-sync
// pass is desired (the service then behaves exactly as it did before Phase 2).
// sqlDB is the same physical handle ent uses, exposed so saveToRawOutput can
// run a raw bulk INSERT ... ON CONFLICT DO NOTHING RETURNING (the raw path
// predates enabling ent's upsert codegen and keeps its hand-rolled SQL) — see
// Phase 4 plan §7.
func NewService(mlsClient mls.PageFetcher, dbClient *ent.Client, sqlDB *sql.DB, store storage.Storer, processor Processor) *Service {
	return &Service{
		mlsClient: mlsClient,
		dbClient:  dbClient,
		sqlDB:     sqlDB,
		storer:    store,
		processor: processor,
	}
}

// WithInitPipeline enables (or disables) the pipelined init path where
// page-fetching overlaps with the typed processor. Returns s for chaining.
// Default (zero value) is false; cmd/init sets it from config/--no-pipeline.
func (s *Service) WithInitPipeline(b bool) *Service {
	s.pipelineInit = b
	return s
}

// WithFetchConcurrency sets how many pages InitialSync fetches in parallel via
// $skip offset paging. <=1 disables it (sequential nextLink paging). Returns s
// for chaining. Default (zero value) is sequential; cmd/root sets it from
// mls.fetch_concurrency.
func (s *Service) WithFetchConcurrency(n int) *Service {
	s.fetchConcurrency = n
	return s
}

// SweepStaleRunningEvents marks any sync_event in state `running` and
// started before now-threshold as `failed`. Returns the number of rows
// updated. Per §7, swept events leave high_water_mark NULL, so the next
// cycle's success-gated HWM query re-fetches the failed window.
//
// CLI entry points (cmd/sync, cmd/import, cmd/init) call this on
// startup. It's safe to call repeatedly and concurrently — fresh events
// (started_at >= now-threshold) are untouched.
func (s *Service) SweepStaleRunningEvents(ctx context.Context, threshold time.Duration) (int, error) {
	cutoff := time.Now().Add(-threshold)
	updated, err := s.dbClient.SyncEvent.Update().
		Where(
			syncevent.StatusEQ(syncevent.StatusRunning),
			syncevent.StartedAtLT(cutoff),
		).
		SetStatus(syncevent.StatusFailed).
		SetEndedAt(time.Now()).
		SetErrorSummary("stale: marked failed by startup sweep").
		Save(ctx)
	if err != nil {
		return 0, err
	}
	if updated > 0 {
		log.Infof("sweep: marked %d stale running sync_event(s) as failed", updated)
	}
	return updated, nil
}

// LastSuccessfulHWM returns the high_water_mark of the most recent
// successful sync_event for (resource), or zero time.Time + ent.NotFoundError
// if no successful run exists yet. Callers fall back to the "one month ago"
// default in that case.
//
// Per §7, this query MUST exclude failed and stale-swept events (status
// filter does it) and MUST require non-NULL hwm — a successful run that
// stamped NULL is malformed and would poison the cursor under Postgres'
// default NULLS-FIRST ordering on DESC.
func (s *Service) LastSuccessfulHWM(ctx context.Context, resource syncevent.Resource) (time.Time, error) {
	ev, err := s.dbClient.SyncEvent.Query().
		Where(
			syncevent.ResourceEQ(resource),
			syncevent.StatusEQ(syncevent.StatusSuccess),
			syncevent.HighWaterMarkNotNil(),
		).
		Order(ent.Desc(syncevent.FieldHighWaterMark)).
		First(ctx)
	if err != nil {
		return time.Time{}, err
	}
	if ev.HighWaterMark == nil {
		return time.Time{}, nil
	}
	return *ev.HighWaterMark, nil
}
