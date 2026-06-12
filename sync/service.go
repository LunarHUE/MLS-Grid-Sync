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
type Processor interface {
	RunPass(ctx context.Context, resource rawoutput.Resource) error
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
}

// NewService constructs a Service. processor may be nil when no post-sync
// pass is desired (the service then behaves exactly as it did before Phase 2).
// sqlDB is the same physical handle ent uses, exposed so saveToRawOutput can
// run a raw bulk INSERT ... ON CONFLICT DO NOTHING (ent's CreateBulk doesn't
// surface OnConflict) — see Phase 4 plan §7.
func NewService(mlsClient mls.PageFetcher, dbClient *ent.Client, sqlDB *sql.DB, store storage.Storer, processor Processor) *Service {
	return &Service{
		mlsClient: mlsClient,
		dbClient:  dbClient,
		sqlDB:     sqlDB,
		storer:    store,
		processor: processor,
	}
}

// SweepStaleRunningEvents marks any sync_event in state `running` and
// started before now-threshold as `failed`. Returns the number of rows
// updated. Per §7, swept events leave high_water_mark NULL, so the next
// cycle's success-gated HWM query re-fetches the failed window.
//
// CLI entry points (cmd/sync, cmd/import, cmd/reprocess) call this on
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
