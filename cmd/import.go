package cmd

import (
	"context"
	"fmt"

	"github.com/LunarHUE/MLS-Grid-Sync/ent"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/applog"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/progress"
	pkgsync "github.com/LunarHUE/MLS-Grid-Sync/sync"
	"github.com/LunarHUE/MLS-Grid-Sync/sync/processor"
	"github.com/spf13/cobra"
)

var importCmd = &cobra.Command{
	Use:           "import [resource]",
	Short:         "Run an initial bulk import (e.g. 'import Property')",
	Args:          cobra.ExactArgs(1),
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		resourceName := args[0]

		// Live pull/enqueue bars (TTY) or clean %/ETA lines (piped) for the import.
		progress.Begin(progress.ParseMode(appConfig.Progress))
		defer progress.End()

		dbResource, err := pkgsync.MLSToDBResource(resourceName)
		if err != nil {
			return fmt.Errorf("invalid resource name: %w", err)
		}
		if processor.IsExpandOnly(dbResource) {
			return fmt.Errorf("%s is expand-only in MLS Grid v2 — it is fetched inside Property. Run `import Property` to refresh it; `reprocess %s` to re-run typed processing", resourceName, dbResource)
		}

		if _, err := resolveOriginatingSystem(cmd, appConfig); err != nil {
			return err
		}

		svc, db, err := setupService(ctx)
		if err != nil {
			return err
		}
		defer db.Close()

		// Sweep stale running events from prior runs — Phase 4 §8.
		if _, err := svc.SweepStaleRunningEvents(ctx, pkgsync.DefaultStaleRunningThreshold); err != nil {
			applog.Errorf("startup sweep failed: %v", err)
		}

		srcSystemID, err := ensureSourceSystem(ctx, db)
		if err != nil {
			return err
		}

		applog.Infof("Starting INITIAL import for %s...", resourceName)
		return svc.RunInitial(ctx, srcSystemID, appConfig.MLS.V2URL, appConfig.MLS.OriginatingSystem, dbResource)
	},
}

// ensureSourceSystem returns the source_system ID, creating the row if
// it doesn't exist yet. import + init use this; the sync daemon expects
// it to exist (run import or init first) and surfaces a clear error if not.
func ensureSourceSystem(ctx context.Context, db *ent.Client) (string, error) {
	src, err := db.SourceSystem.Query().First(ctx)
	if ent.IsNotFound(err) {
		applog.Infof("Source system 'mlsgrid' not found. Creating it...")
		src, err = db.SourceSystem.Create().
			SetID("mlsgrid").
			SetSourceSystemName("MLSGrid").
			Save(ctx)
	}
	if err != nil {
		return "", fmt.Errorf("failed to get/create source system: %w", err)
	}
	return src.ID, nil
}

func init() {
	addOriginatingSystemFlag(importCmd)
	rootCmd.AddCommand(importCmd)
}
