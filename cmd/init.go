package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/lunarhue/libs-go/log"
	"github.com/spf13/cobra"

	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/rawoutput"
	pkgsync "github.com/lunarhue/website-highpointe/packages/mls-grid-sync/sync"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/sync/processor"
)

const skipFlag = "skip"

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

		if _, err := svc.SweepStaleRunningEvents(ctx, pkgsync.DefaultStaleRunningThreshold); err != nil {
			log.Errorf("startup sweep failed: %v", err)
		}

		srcSystemID, err := ensureSourceSystem(ctx, db)
		if err != nil {
			return err
		}

		log.Infof("Starting full INIT import (originating_system=%s)...", appConfig.MLS.OriginatingSystem)
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
			log.Infof("init: skipping %s (--skip)", resource)
			continue
		}
		apiName, err := pkgsync.DBToMLSResource(resource)
		if err != nil {
			return fmt.Errorf("init: %w", err)
		}
		log.Infof("init: starting %s", apiName)
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
		dbName, err := normalizeSkipName(name)
		if err != nil {
			return nil, err
		}
		out[dbName] = true
	}
	return out, nil
}

func normalizeSkipName(name string) (rawoutput.Resource, error) {
	// Try DB enum directly first.
	r := rawoutput.Resource(name)
	if rawoutput.ResourceValidator(r) == nil {
		return r, nil
	}
	// Fall back to MLS API name (operators copy-paste from logs).
	if db, err := pkgsync.MLSToDBResource(name); err == nil {
		return db, nil
	}
	return "", fmt.Errorf("invalid --skip resource %q (try Property, Office, Member, …)", name)
}

func init() {
	addOriginatingSystemFlag(initCmd)
	initCmd.Flags().String(skipFlag, "", "comma-separated resources to skip (e.g. Office,Media)")
	rootCmd.AddCommand(initCmd)
}
