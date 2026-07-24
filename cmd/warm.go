package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	"github.com/google/uuid"
	"github.com/lunarhue/libs-go/log"
	"github.com/spf13/cobra"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/attachmentjob"
	entmedia "github.com/LunarHUE/MLS-Grid-Sync/ent/media"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/property"
	"github.com/LunarHUE/MLS-Grid-Sync/ent/syncevent"
)

// Default statuses worth holding images for: what a buyer browsing the site
// could actually be shown. ActiveUnderContract is included because such a
// listing can fall through and return to market — dropping its photos would
// mean re-downloading them later and showing an image-less page in between.
var defaultWarmStatuses = []string{"Active", "ActiveUnderContract"}

var (
	warmStatuses []string
	warmLimit    int
	warmDryRun   bool
)

// warmChunk bounds each bulk insert and each IN (...) list. Postgres accepts
// far more, but keeping the batches modest keeps memory flat on the small
// burstable instances this tends to run against.
const warmChunk = 1000

var warmCmd = &cobra.Command{
	Use:   "warm",
	Short: "Queue attachment downloads for listings worth showing (default: Active + ActiveUnderContract)",
	Long: `Enqueue attachment jobs for the media of listings a buyer could actually be
shown, so the images are in storage before anyone asks for them.

Why this exists rather than just downloading everything: MLS Grid media links
are single-use and expire an hour after the response that minted them, so a
whole-corpus backlog cannot be drained — at the licensed ~1 rps a large feed
takes days, and every link is long dead by the time the worker reaches it.
Restricting to on-market listings turns an impossible backlog into a small
one that finishes in a couple of hours and then stays warm via delta sync.

This only queues work. Run 'worker' to actually download:

  mls-cli warm
  mls-cli worker --max-jobs 10000`,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		// Like serve: no MLS token and no migrations. warm only reads the
		// typed tables and writes job rows; sync/init own the schema.
		sqlDB, err := sql.Open("postgres", appConfig.Database.DSN)
		if err != nil {
			return fmt.Errorf("failed opening connection to postgres: %w", err)
		}
		drv := entsql.OpenDB(dialect.Postgres, sqlDB)
		db := ent.NewClient(ent.Driver(drv))
		defer db.Close()

		statuses := warmStatuses
		if len(statuses) == 0 {
			statuses = defaultWarmStatuses
		}

		listingKeys, err := db.Property.Query().
			Where(property.StandardStatusIn(statuses...)).
			IDs(ctx)
		if err != nil {
			return fmt.Errorf("query listings by status: %w", err)
		}
		log.Infof("warm: %d listing(s) matching %v", len(listingKeys), statuses)
		if len(listingKeys) == 0 {
			return nil
		}

		// Media rows with no attachment yet, for those listings. Chunked
		// because the key list can run to tens of thousands.
		var mediaKeys []string
		for chunk := range slices.Chunk(listingKeys, warmChunk) {
			keys, err := db.Media.Query().
				Where(
					entmedia.ResourceTypeEQ(entmedia.ResourceTypeProperty),
					entmedia.ResourceRecordKeyIn(chunk...),
					entmedia.AttachmentIDIsNil(),
				).
				IDs(ctx)
			if err != nil {
				return fmt.Errorf("query unattached media: %w", err)
			}
			mediaKeys = append(mediaKeys, keys...)
		}
		log.Infof("warm: %d media record(s) without a stored attachment", len(mediaKeys))
		if len(mediaKeys) == 0 {
			return nil
		}

		// Drop anything already queued or already done, so re-running warm is
		// idempotent rather than additive.
		pending, err := existingJobKeys(ctx, db, mediaKeys)
		if err != nil {
			return err
		}
		todo := make([]string, 0, len(mediaKeys))
		for _, k := range mediaKeys {
			if _, blocked := pending[k]; !blocked {
				todo = append(todo, k)
			}
		}
		if warmLimit > 0 && len(todo) > warmLimit {
			log.Infof("warm: limiting to %d of %d (--limit)", warmLimit, len(todo))
			todo = todo[:warmLimit]
		}

		log.Infof("warm: %d new job(s) to enqueue (%d already queued or downloaded)",
			len(todo), len(mediaKeys)-len(todo))
		if warmDryRun {
			log.Info("warm: --dry-run, nothing written")
			return nil
		}
		if len(todo) == 0 {
			return nil
		}

		eventID, err := warmSyncEvent(ctx, db)
		if err != nil {
			return err
		}

		var created int
		for chunk := range slices.Chunk(todo, warmChunk) {
			builders := make([]*ent.AttachmentJobCreate, 0, len(chunk))
			for _, k := range chunk {
				builders = append(builders, db.AttachmentJob.Create().
					SetMediaKey(k).
					SetSyncEventID(eventID))
			}
			if err := db.AttachmentJob.CreateBulk(builders...).Exec(ctx); err != nil {
				return fmt.Errorf("bulk enqueue: %w", err)
			}
			created += len(chunk)
			log.Infof("warm: enqueued %d/%d", created, len(todo))
		}

		log.Infof("warm: done — %d job(s) queued. Run 'worker' to download them.", created)
		return nil
	},
}

// existingJobKeys returns the media keys that already have a job in a state
// meaning "spoken for". canceled and permanently_failed are deliberately
// absent: re-warming those is the point of running warm again.
func existingJobKeys(ctx context.Context, db *ent.Client, mediaKeys []string) (map[string]struct{}, error) {
	blocking := []attachmentjob.Status{
		attachmentjob.StatusPending,
		attachmentjob.StatusRetrying,
		attachmentjob.StatusInProgress,
		attachmentjob.StatusSucceeded,
	}
	out := make(map[string]struct{})
	for chunk := range slices.Chunk(mediaKeys, warmChunk) {
		jobs, err := db.AttachmentJob.Query().
			Where(
				attachmentjob.MediaKeyIn(chunk...),
				attachmentjob.StatusIn(blocking...),
			).
			Select(attachmentjob.FieldMediaKey).
			All(ctx)
		if err != nil {
			return nil, fmt.Errorf("query existing jobs: %w", err)
		}
		for _, j := range jobs {
			out[j.MediaKey] = struct{}{}
		}
	}
	return out, nil
}

// warmSyncEvent reuses the running backfill event if one exists (serve's
// prefetch path creates the same shape), else opens one. AttachmentJob
// requires a sync_event_id and a warm run has no originating sync.
func warmSyncEvent(ctx context.Context, db *ent.Client) (uuid.UUID, error) {
	existing, err := db.SyncEvent.Query().
		Where(
			syncevent.ResourceEQ(syncevent.ResourceMedia),
			syncevent.RunTypeEQ(syncevent.RunTypeBackfill),
			syncevent.StatusEQ(syncevent.StatusRunning),
		).
		First(ctx)
	if err == nil {
		return existing.ID, nil
	}
	if !ent.IsNotFound(err) {
		return uuid.Nil, fmt.Errorf("query backfill event: %w", err)
	}

	src, err := db.SourceSystem.Query().First(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("no source system — run 'init' first: %w", err)
	}
	created, err := db.SyncEvent.Create().
		SetSourceSystemID(src.ID).
		SetResource(syncevent.ResourceMedia).
		SetRunType(syncevent.RunTypeBackfill).
		SetStatus(syncevent.StatusRunning).
		SetProcessorVersion("warm").
		SetStartedAt(time.Now()).
		Save(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("create backfill event: %w", err)
	}
	return created.ID, nil
}

func init() {
	warmCmd.Flags().StringSliceVar(&warmStatuses, "status", nil,
		"StandardStatus values to warm (default: Active,ActiveUnderContract)")
	warmCmd.Flags().IntVar(&warmLimit, "limit", 0,
		"Cap the number of jobs enqueued this run. 0 means no cap.")
	warmCmd.Flags().BoolVar(&warmDryRun, "dry-run", false,
		"Report what would be enqueued without writing anything.")
	rootCmd.AddCommand(warmCmd)
}
