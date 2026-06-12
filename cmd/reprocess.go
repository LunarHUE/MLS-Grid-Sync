package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/lunarhue/libs-go/log"
	"github.com/spf13/cobra"

	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/processorcursor"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/rawoutput"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/syncevent"
	pkgsync "github.com/lunarhue/website-highpointe/packages/mls-grid-sync/sync"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/version"
)

var (
	reprocessFromID string
	reprocessAll    bool
)

var reprocessCmd = &cobra.Command{
	Use:   "reprocess <resource>",
	Short: "Replay raw_output through the processor (after a parser fix, schema change, etc.)",
	Long: `reprocess resets the processor cursor for one resource and re-runs the
typed pass. Use --all to replay every raw_output row from the beginning,
or --from <raw_output_id> to resume from a specific row.

The processor's stale check makes replay safe: records whose
source_modified_at is <= the current entity row's source_modified_at are
skipped UNLESS the processor logic now writes different values for them —
which is exactly what a reprocess is for. Expected output: update versions
appear ONLY for fields whose values changed under the new parser.

Audit: a sync_event row is created with run_type=reprocess. The
processor_cursor.processor_version is stamped with the current build.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		resourceArg := args[0]

		// Accept either the syncevent enum form ("Property") or the
		// raw_output form ("property"). Normalize to raw_output.Resource
		// since that's what the processor expects.
		dbResource, err := normalizeToDBResource(resourceArg)
		if err != nil {
			return err
		}

		if !reprocessAll && reprocessFromID == "" {
			return fmt.Errorf("must specify --all or --from <raw_output_id>")
		}
		if reprocessAll && reprocessFromID != "" {
			return fmt.Errorf("--all and --from are mutually exclusive")
		}

		var fromID *uuid.UUID
		if reprocessFromID != "" {
			id, err := uuid.Parse(reprocessFromID)
			if err != nil {
				return fmt.Errorf("invalid --from UUID: %w", err)
			}
			fromID = &id
		}

		comps, err := setupComponents(ctx)
		if err != nil {
			return err
		}
		defer comps.db.Close()

		srcSystem, err := comps.db.SourceSystem.Query().First(ctx)
		if err != nil {
			return fmt.Errorf("source system lookup: %w (run 'import' first)", err)
		}

		// One tx: reset cursor + insert audit row. Either both land or neither.
		auditID, err := resetCursorAndAudit(ctx, comps.db, dbResource, fromID, srcSystem.ID)
		if err != nil {
			return err
		}
		log.Infof("reprocess: cursor reset for %s (from=%v, all=%v), audit sync_event %s",
			dbResource, fromID, reprocessAll, auditID)

		// Run the processor in normal-pass mode — same code path the
		// post-sync trigger uses, just with the cursor reset.
		runErr := comps.proc.RunPass(ctx, dbResource)
		update := comps.db.SyncEvent.UpdateOneID(auditID).SetEndedAt(time.Now())
		if runErr != nil {
			update.SetStatus(syncevent.StatusFailed).SetErrorSummary(runErr.Error())
		} else {
			update.SetStatus(syncevent.StatusSuccess)
		}
		if _, err := update.Save(ctx); err != nil {
			log.Errorf("failed to stamp reprocess sync_event outcome: %v", err)
		}
		if runErr != nil {
			return runErr
		}
		log.Info("reprocess complete.")
		return nil
	},
}

// normalizeToDBResource accepts either the rawoutput enum form
// ("property", "open_house") or the MLS API name ("Property", "OpenHouse")
// — operators tend to copy from logs. Returns the canonical rawoutput
// form.
func normalizeToDBResource(arg string) (rawoutput.Resource, error) {
	// Try DB enum directly first.
	candidate := rawoutput.Resource(arg)
	if err := rawoutput.ResourceValidator(candidate); err == nil {
		return candidate, nil
	}
	// Fall back to MLS API name.
	if db, err := pkgsync.MLSToDBResource(arg); err == nil {
		return db, nil
	}
	return "", fmt.Errorf("invalid resource %q (try Property, OpenHouse, property, open_house, …)", arg)
}

// resetCursorAndAudit updates processor_cursor.last_raw_output_id to the
// requested position (or NULL for --all), bumps processor_version, and
// inserts the run_type=reprocess audit sync_event — all in one tx.
func resetCursorAndAudit(ctx context.Context, db *ent.Client, dbResource rawoutput.Resource, fromID *uuid.UUID, srcID string) (uuid.UUID, error) {
	tx, err := db.Tx(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("begin tx: %w", err)
	}
	rollback := func(in error) (uuid.UUID, error) {
		_ = tx.Rollback()
		return uuid.Nil, in
	}

	// Ensure the cursor row exists; create with NULL if not.
	cur, err := tx.ProcessorCursor.Query().
		Where(processorcursor.ResourceEQ(processorcursor.Resource(dbResource))).
		Only(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return rollback(fmt.Errorf("cursor lookup: %w", err))
	}
	if cur == nil {
		cur, err = tx.ProcessorCursor.Create().
			SetResource(processorcursor.Resource(dbResource)).
			SetProcessorVersion(version.Info()).
			Save(ctx)
		if err != nil {
			return rollback(fmt.Errorf("create cursor: %w", err))
		}
	}

	upd := tx.ProcessorCursor.UpdateOneID(cur.ID).SetProcessorVersion(version.Info())
	if fromID != nil {
		upd.SetLastRawOutputID(*fromID)
	} else {
		upd.ClearLastRawOutputID()
	}
	if _, err := upd.Save(ctx); err != nil {
		return rollback(fmt.Errorf("cursor reset: %w", err))
	}

	audit, err := tx.SyncEvent.Create().
		SetRunType(syncevent.RunTypeReprocess).
		SetSourceSystemID(srcID).
		SetResource(syncevent.Resource(dbResource)).
		SetStatus(syncevent.StatusRunning).
		SetProcessorVersion(version.Info()).
		SetStartedAt(time.Now()).
		Save(ctx)
	if err != nil {
		return rollback(fmt.Errorf("audit insert: %w", err))
	}

	if err := tx.Commit(); err != nil {
		return uuid.Nil, fmt.Errorf("commit: %w", err)
	}
	return audit.ID, nil
}

func init() {
	reprocessCmd.Flags().StringVar(&reprocessFromID, "from", "", "raw_output_id (uuid) to resume from")
	reprocessCmd.Flags().BoolVar(&reprocessAll, "all", false, "replay every raw_output row from the beginning")
	rootCmd.AddCommand(reprocessCmd)
}
