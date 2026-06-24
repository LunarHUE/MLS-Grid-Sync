package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/LunarHUE/MLS-Grid-Sync/mls"
)

var systemsCmd = &cobra.Command{
	Use:   "systems",
	Short: "Probe MLS Grid for available originating systems (best-effort)",
	Long: `systems fires an unfiltered Lookup query through the rate-limited
mls.Client and reports the distinct OriginatingSystemName values it sees.

MLS Grid scopes tokens to specific originating systems and its API
generally expects OriginatingSystemName in the $filter — so whether an
unfiltered query succeeds, errors, or returns cross-system data is not
something to assume. This command probes and reports honestly: if the
upstream rejects the query, the error body prints verbatim and a
non-zero exit code surfaces so automation notices.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		if appConfig.MLS.Token == "" {
			return fmt.Errorf("fatal: MLS token is missing from configuration")
		}
		client := mls.NewClient(appConfig.MLS.Token, appConfig.MLS.APIRPS)
		names, err := mls.ProbeOriginatingSystems(ctx, client, appConfig.MLS.V2URL)
		if err != nil {
			fmt.Fprintf(cmd.OutOrStderr(), "discovery probe failed: %v\n", err)
			fmt.Fprintln(cmd.OutOrStderr(), "discovery is unsupported for this token; supply --originating-system explicitly")
			return err
		}
		if len(names) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "discovery returned 0 records (upstream allowed the query but produced no payloads)")
			return nil
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Originating systems visible to this token:")
		for _, n := range names {
			fmt.Fprintf(cmd.OutOrStdout(), "  - %s\n", n)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(systemsCmd)

	// Wire the resolver-time prompt-menu probe to use the real discovery
	// path. Called lazily so appConfig is loaded by the time it fires.
	discoverSystems = func() []string {
		if appConfig == nil || appConfig.MLS.Token == "" || appConfig.MLS.V2URL == "" {
			return nil
		}
		client := mls.NewClient(appConfig.MLS.Token, appConfig.MLS.APIRPS)
		names, err := mls.ProbeOriginatingSystems(context.Background(), client, appConfig.MLS.V2URL)
		if err != nil {
			// Discovery failure is non-fatal at prompt time — fall back to
			// free-text entry rather than crashing the resolver.
			return nil
		}
		return names
	}
}
