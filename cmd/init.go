package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/applog"
	"github.com/LunarHUE/MLS-Grid-Sync/internal/progress"
	pkgsync "github.com/LunarHUE/MLS-Grid-Sync/sync"
	"github.com/LunarHUE/MLS-Grid-Sync/sync/processor"
)

const skipFlag = "skip"
const noPipelineFlag = "no-pipeline"

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Run the full initial corpus import across all fetchable resources in FK-dependency order",
	Long: `init runs InitialSync for every resource in processor.FetchableResources
(Lookup → Office → Member → Property → OpenHouse — the fetchable subset of
the FK-dependency order), one after the other.

Media, PropertyRooms, and PropertyUnitTypes are expand-only: MLS Grid v2
returns 501 for /v2/Media, and Rooms / UnitTypes are not top-level RESO
resources. They ride the Property fetch via $expand and are split out into
their own raw_output rows at sync time (see sync/raw.go).

On any failure init halts and returns a non-zero exit. Completed resources
are safe to re-run: their sync_event.hwm gates the next fetch's window to
a small delta, and the raw_output uniqueness index plus the processor's
stale-skip absorb any overlap. Re-run init with --skip to bypass already-
clean resources.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		// Open the progress session for the whole import so the pull/enqueue
		// bars render (TTY) or emit clean %/ETA lines (piped). End erases any
		// residual bar region.
		progress.Begin(progress.ParseMode(appConfig.Progress))
		defer progress.End()

		if _, err := resolveOriginatingSystem(cmd, appConfig); err != nil {
			return err
		}

		skipRaw, _ := cmd.Flags().GetString(skipFlag)
		skipSet, err := parseSkipList(skipRaw)
		if err != nil {
			return err
		}

		svc, db, err := setupService(ctx)
		if err != nil {
			return err
		}
		defer db.Close()

		// Pipeline init by default (config); --no-pipeline forces the
		// sequential fetch-then-process path.
		pipeline := appConfig.Processor.InitPipeline
		if noPipeline, _ := cmd.Flags().GetBool(noPipelineFlag); noPipeline {
			pipeline = false
		}
		svc.WithInitPipeline(pipeline)
		if pipeline {
			applog.Infof("init: pipelined fetch‖process enabled")
		}

		if _, err := svc.SweepStaleRunningEvents(ctx, pkgsync.DefaultStaleRunningThreshold); err != nil {
			applog.Errorf("startup sweep failed: %v", err)
		}

		srcSystemID, err := ensureSourceSystem(ctx, db)
		if err != nil {
			return err
		}

		applog.Infof("Starting full INIT import (originating_system=%s)...", appConfig.MLS.OriginatingSystem)
		return runInit(ctx, svc, srcSystemID, appConfig.MLS.V2URL, appConfig.MLS.OriginatingSystem, processor.FetchableResources, skipSet)
	},
}

// initialRunner is the minimal surface init needs from Service. Carved
// out so init_test can inject a fake that records call order without
// spinning up a database.
type initialRunner interface {
	RunInitial(ctx context.Context, srcSystemID, v2URL, originatingSystem string, resource rawoutput.Resource) error
}

func runInit(ctx context.Context, r initialRunner, srcSystemID, v2URL, originatingSystem string, order []rawoutput.Resource, skip map[rawoutput.Resource]bool) error {
	for _, resource := range order {
		if skip[resource] {
			applog.Infof("init: skipping %s (--skip)", resource)
			continue
		}
		apiName, err := pkgsync.DBToMLSResource(resource)
		if err != nil {
			return fmt.Errorf("init: %w", err)
		}
		applog.Infof("init: starting %s", apiName)
		if err := r.RunInitial(ctx, srcSystemID, v2URL, originatingSystem, resource); err != nil {
			return fmt.Errorf("%s — completed resources are safe to re-run; fix and run `mls-grid-sync init` again (or `--skip` to bypass): %w", formatInitHalt(apiName, resource, err), err)
		}
	}
	return nil
}

// formatInitHalt names the FAILING PASS, with a parenthetical that
// identifies which fetch step it rode on when the two differ. Example:
//
//	"init halted on media (riding Property step)"
//	"init halted on Property" (when the Property pass itself failed,
//	or when the failure was outside the processor — fetch/save).
//
// Operators need to see the failing pass FIRST: "Property" is a true
// statement when media halts but a misleading one for triage. The
// PassError typed wrapper from sync/resource.go carries the pass
// identity through; this formatter unwraps and chooses.
func formatInitHalt(stepAPIName string, stepResource rawoutput.Resource, err error) string {
	var pe *pkgsync.PassError
	if errors.As(err, &pe) && pe.Pass != stepResource {
		passAPI, derr := pkgsync.DBToMLSResource(pe.Pass)
		if derr != nil {
			passAPI = string(pe.Pass) // fallback to DB name
		}
		return fmt.Sprintf("init halted on %s (riding %s step)", passAPI, stepAPIName)
	}
	return fmt.Sprintf("init halted on %s", stepAPIName)
}

// parseSkipList splits "Office,Media" into a set of rawoutput.Resource
// values. Accepts both MLS API names (PascalCase) and DB enum names
// (snake_case) for flexibility — operators tend to copy from logs.
func parseSkipList(s string) (map[rawoutput.Resource]bool, error) {
	out := map[rawoutput.Resource]bool{}
	if s == "" {
		return out, nil
	}
	for _, name := range strings.Split(s, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		dbName, ok := normalizeResource(name)
		if !ok {
			return nil, fmt.Errorf("invalid --skip resource %q (try Property, Office, Member, …)", name)
		}
		out[dbName] = true
	}
	return out, nil
}

func init() {
	addOriginatingSystemFlag(initCmd)
	initCmd.Flags().String(skipFlag, "", "comma-separated resources to skip (e.g. Office,Media)")
	initCmd.Flags().Bool(noPipelineFlag, false, "disable fetch‖process overlap; fetch all pages, then process (sequential)")
	rootCmd.AddCommand(initCmd)
}
