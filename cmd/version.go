package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/LunarHUE/MLS-Grid-Sync/version"
)

var versionJSON bool

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print build/version information",
	Long: `version reports the build identity of this binary — semantic version,
git commit, build date, Go toolchain, module path, and repo URL — so a
support report or field-drift issue can name the exact build in use.

It needs no configuration, database, or MLS token, and works on a bare
install. Use --json for machine-readable output (carries the full commit SHA).`,
	SilenceUsage:  true,
	SilenceErrors: true,
	// Bypass the root PersistentPreRunE (config load + optional pprof).
	// `version` must stay usable with no config file, DB, or MLS token; a
	// future stricter root setup must not be able to break it. In default
	// Cobra only the closest hook in the chain runs, so this fully replaces
	// the root's for this command. (If cobra.EnableTraverseRunHooks is ever
	// turned on, this must become a guarded root hook instead.)
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error { return nil },
	RunE: func(cmd *cobra.Command, args []string) error {
		d := version.Details()
		out := cmd.OutOrStdout()

		if versionJSON {
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			return enc.Encode(d)
		}

		commit := version.ShortCommit(d.Commit)
		if d.Dirty {
			commit += " dirty"
		}
		fmt.Fprintf(out, "Version:    %s\n", d.Version)
		fmt.Fprintf(out, "Commit:     %s\n", commit)
		fmt.Fprintf(out, "BuildDate:  %s\n", d.BuildDate)
		fmt.Fprintf(out, "GoVersion:  %s\n", d.GoVersion)
		fmt.Fprintf(out, "Module:     %s\n", d.Module)
		fmt.Fprintf(out, "RepoURL:    %s\n", d.RepoURL)
		return nil
	},
}

func init() {
	versionCmd.Flags().BoolVar(&versionJSON, "json", false, "emit machine-readable JSON")
	rootCmd.AddCommand(versionCmd)

	// Wire `mls-cli --version`. Cobra's version template only exposes
	// .Name/.Version, so bake the full one-line string into rootCmd.Version
	// rather than relying on template functions.
	d := version.Details()
	rootCmd.Version = fmt.Sprintf("%s (%s)", d.Version, version.ShortCommit(d.Commit))
	rootCmd.SetVersionTemplate("{{.Name}} {{.Version}}\n")
}
