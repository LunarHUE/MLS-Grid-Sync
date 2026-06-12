package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/ent/rawoutput"
	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/sync/processor"
)

var validateTypedCmd = &cobra.Command{
	Use:   "validate-typed [resource|all]",
	Short: "Compare each entity row to its current open version row for drift",
	Long: `validate-typed walks every visible (mlg_can_view=true) entity row for the
named resource (or all 7 versioned resources if "all" is passed) and compares
its data fields to the matching current open version row (valid_to IS NULL).

Tombstoned entities are excluded by design — the Phase 3 delete branch leaves
last-known field values on the entity row while the delete version is sparse.

Any reported mismatch is drift between the entity and its history — a
regression signal for the §2 SetNillableX(nil) bug class or for any future
code path that updates one but not the other.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		_, db, err := setupService(ctx)
		if err != nil {
			return err
		}
		defer db.Close()

		var resources []rawoutput.Resource
		if args[0] == "all" {
			resources = processor.AllTypedDriftResources
		} else {
			res := rawoutput.Resource(args[0])
			if err := rawoutput.ResourceValidator(res); err != nil {
				return fmt.Errorf("invalid resource: %w", err)
			}
			resources = []rawoutput.Resource{res}
		}

		anyDrift := false
		for i, res := range resources {
			if i > 0 {
				fmt.Println()
			}
			report, err := processor.ValidateTyped(ctx, db, res)
			if err != nil {
				fmt.Printf("Resource: %s\n  ERROR: %v\n", res, err)
				continue
			}
			printTypedReport(report)
			if len(report.Mismatches) > 0 {
				anyDrift = true
			}
		}
		if anyDrift {
			return fmt.Errorf("drift detected — see report above")
		}
		return nil
	},
}

func printTypedReport(report *processor.TypedDriftReport) {
	fmt.Printf("Resource: %s\n", report.Resource)
	fmt.Printf("Entities scanned: %d\n", report.EntitiesSeen)
	fmt.Printf("Mismatches: %d\n", len(report.Mismatches))
	for _, m := range report.SortedMismatches() {
		fmt.Printf("  %s  field=%s\n    entity:  %s\n    version: %s\n", m.EntityID, m.Field, m.EntityValue, m.VersionVal)
	}
}

func init() {
	rootCmd.AddCommand(validateTypedCmd)
}
