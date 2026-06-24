package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/LunarHUE/MLS-Grid-Sync/config"
	"github.com/LunarHUE/MLS-Grid-Sync/doctor"
	"github.com/LunarHUE/MLS-Grid-Sync/mls"
)

var (
	doctorJSON        bool
	doctorSkipMLS     bool
	doctorSkipStorage bool
	doctorStrict      bool
	doctorTimeout     time.Duration
)

// errDoctorChecksFailed is a sentinel returned when at least one check did not
// pass (or, under --strict, warned). The human/JSON report is already on
// stdout; this just drives the non-zero process exit via Execute().
var errDoctorChecksFailed = errors.New("doctor: one or more checks did not pass")

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Validate deployment configuration and subsystems before sync",
	Long: `doctor runs independent health checks against a deployment's configuration
and subsystems — config load, MLS Grid API reachability/token/originating
system, PostgreSQL + PostGIS + required tables, storage backend, and the
GraphQL server's auth/CORS/rate-limit knobs — and reports pass/warn/fail/
skipped per check with a remediation hint.

It is read-only for PostgreSQL. The storage check may perform the same
idempotent bucket/container creation that normal startup performs (disclosed
in its result message); use --skip-storage to avoid any object-storage calls.

Exit code is non-zero if any check fails; --strict also fails on warnings.
Use --json for machine-readable output.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	// Bypass the root PersistentPreRunE (which calls config.Load and would
	// abort the whole command on a bad config). doctor loads config itself so
	// a load failure becomes a reported `fail`, not a crash. Same mechanism as
	// `version`; if cobra.EnableTraverseRunHooks is ever enabled, guard the
	// root hook for this command instead.
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error { return nil },
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		cfg, cfgErr := config.Load()

		deps := doctor.Deps{
			Config:      cfg,
			ConfigErr:   cfgErr,
			Now:         time.Now,
			BuildStorer: newStorer,
		}
		if cfgErr == nil {
			if !doctorSkipMLS && cfg.MLS.Token != "" {
				deps.Fetcher = mls.NewClient(cfg.MLS.Token, cfg.MLS.APIRPS)
			}
			// sql.Open is lazy and almost never errors here; if it does (bad
			// DSN), leave DB nil so the postgres check fails cleanly rather
			// than aborting the whole run.
			if db, err := sql.Open("postgres", cfg.Database.DSN); err == nil {
				defer db.Close()
				deps.DB = db
			}
		}

		opts := doctor.Options{
			SkipMLS:     doctorSkipMLS,
			SkipStorage: doctorSkipStorage,
			Strict:      doctorStrict,
			Timeout:     doctorTimeout,
		}
		return runDoctor(ctx, deps, opts, doctorJSON, cmd.OutOrStdout())
	},
}

// runDoctor executes the checks, renders them (human or JSON) to out, and
// returns errDoctorChecksFailed when the exit code should be non-zero. Carved
// out so cmd/doctor_test can drive it with injected fakes.
func runDoctor(ctx context.Context, deps doctor.Deps, opts doctor.Options, jsonOut bool, out io.Writer) error {
	results := doctor.Run(ctx, deps, opts)

	if jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(doctor.NewReport(results, opts.Strict)); err != nil {
			return err
		}
	} else {
		renderDoctorHuman(out, results)
	}

	if doctor.ExitCode(results, opts.Strict) != 0 {
		return errDoctorChecksFailed
	}
	return nil
}

// renderDoctorHuman prints an aligned STATUS/name/message table, with a
// remediation line under any warn/fail.
func renderDoctorHuman(out io.Writer, results []doctor.CheckResult) {
	nameW := 0
	for _, c := range results {
		if len(c.Name) > nameW {
			nameW = len(c.Name)
		}
	}
	for _, c := range results {
		fmt.Fprintf(out, "%-7s %-*s %s\n", strings.ToUpper(string(c.Status)), nameW, c.Name, c.Message)
		if c.Remediation != "" && (c.Status == doctor.StatusFail || c.Status == doctor.StatusWarn) {
			fmt.Fprintf(out, "%-7s %-*s   ↳ %s\n", "", nameW, "", c.Remediation)
		}
	}
}

func init() {
	doctorCmd.Flags().BoolVar(&doctorJSON, "json", false, "emit machine-readable JSON")
	doctorCmd.Flags().BoolVar(&doctorSkipMLS, "skip-mls", false, "skip all MLS Grid API checks (token/url/originating-system/probe)")
	doctorCmd.Flags().BoolVar(&doctorSkipStorage, "skip-storage", false, "skip the storage backend check (guarantees no object-storage calls)")
	doctorCmd.Flags().BoolVar(&doctorStrict, "strict", false, "treat warnings as failures (non-zero exit)")
	doctorCmd.Flags().DurationVar(&doctorTimeout, "timeout", doctor.DefaultTimeout, "per-check timeout for MLS/DB/storage checks")
	rootCmd.AddCommand(doctorCmd)
}
