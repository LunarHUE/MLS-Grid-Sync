package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/lunarhue/libs-go/log"
	"github.com/spf13/cobra"

	pkgsync "github.com/LunarHUE/MLS-Grid-Sync/sync"
	"github.com/LunarHUE/MLS-Grid-Sync/sync/processor"
)

var syncCmd = &cobra.Command{
	Use:   "sync [resource]",
	Short: "Start the continuous delta sync daemon",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		resourceName := args[0]

		dbResource, err := pkgsync.MLSToDBResource(resourceName)
		if err != nil {
			return fmt.Errorf("invalid resource name: %w", err)
		}
		if processor.IsExpandOnly(dbResource) {
			return fmt.Errorf("%s is expand-only in MLS Grid v2 — it is delivered inside Property. Run `sync Property` to pick up its delta", resourceName)
		}

		if _, err := resolveOriginatingSystem(cmd, appConfig); err != nil {
			return err
		}

		svc, db, err := setupService(ctx)
		if err != nil {
			return err
		}
		defer db.Close()

		log.Infof("Starting delta sync daemon for %s (15m interval)...", resourceName)

		// Sweep stale running events left by previous host crashes. Their
		// high_water_mark is NULL by Phase 4 §7, so the next-cycle HWM read
		// picks up from the prior success — failed windows are re-fetched.
		if _, err := svc.SweepStaleRunningEvents(ctx, pkgsync.DefaultStaleRunningThreshold); err != nil {
			log.Errorf("startup sweep failed: %v", err)
		}

		srcSystem, err := db.SourceSystem.Query().First(ctx)
		if err != nil {
			log.Panicf("Critical: SourceSystem not found. Run 'import' or 'init' first to initialize. Error: %v", err)
		}

		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()

		for {
			log.Info("Running delta sync cycle...")
			// RunDelta owns the lifecycle (HWM read, create-running,
			// execute, stamp success/failed). The daemon never exits on a
			// per-cycle failure — it logs and waits for the next tick.
			if err := svc.RunDelta(ctx, srcSystem.ID, appConfig.MLS.V2URL, appConfig.MLS.OriginatingSystem, dbResource); err != nil {
				log.Errorf("Sync cycle failed: %v", err)
			}

			if !sleepUntilNextTick(ctx, ticker) {
				return ctx.Err()
			}
		}
	},
}

// sleepUntilNextTick returns true if the ticker fired, false if the
// context was canceled.
func sleepUntilNextTick(ctx context.Context, ticker *time.Ticker) bool {
	select {
	case <-ctx.Done():
		return false
	case <-ticker.C:
		return true
	}
}

func init() {
	addOriginatingSystemFlag(syncCmd)
	rootCmd.AddCommand(syncCmd)
}
