package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/LunarHUE/MLS-Grid-Sync/ent/rawoutput"
	"github.com/LunarHUE/MLS-Grid-Sync/sync/processor"
)

var validateRawCmd = &cobra.Command{
	Use:   "validate-raw [resource|all]",
	Short: "Stream every raw_output row through the parser and report mapping coverage",
	Long: `validate-raw inspects every raw_output row for the named resource (or every
registered resource if "all" is passed) without writing anything. It reports:

  - parse errors (with raw_output_id),
  - unconsumed payload keys (anything that landed in extended_fields),
    aggregated across the dataset,
  - typed Fields struct fields that never received a non-nil value across any
    row (likely a misnamed key in the parser).`,
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
			resources = processor.AllValidatedResources
		} else {
			res := rawoutput.Resource(args[0])
			if err := rawoutput.ResourceValidator(res); err != nil {
				return fmt.Errorf("invalid resource: %w", err)
			}
			resources = []rawoutput.Resource{res}
		}

		for i, res := range resources {
			if i > 0 {
				fmt.Println()
			}
			report, err := processor.ValidateRaw(ctx, db, res)
			if err != nil {
				fmt.Printf("Resource: %s\n  ERROR: %v\n", res, err)
				continue
			}
			printReport(report)
		}
		return nil
	},
}

func printReport(report *processor.ValidateReport) {
	fmt.Printf("Resource: %s\n", report.Resource)
	fmt.Printf("Rows scanned: %d\n", report.TotalRows)
	fmt.Printf("Parse errors: %d\n", len(report.ParseErrors))
	for _, e := range report.ParseErrors {
		fmt.Printf("  %s  %s\n", e.RawOutputID, e.Err)
	}

	fmt.Printf("\nUnconsumed payload keys: %d distinct\n", len(report.UnconsumedKeys))
	for _, kc := range report.SortedUnconsumedKeys() {
		fmt.Printf("  %6d  %s\n", kc.Count, kc.Key)
	}

	fmt.Printf("\nAlways-nil typed fields (potential mapping miss): %d\n", len(report.AlwaysNilFields))
	for _, name := range report.AlwaysNilFields {
		fmt.Printf("  %s\n", name)
	}
}

func init() {
	rootCmd.AddCommand(validateRawCmd)
}
