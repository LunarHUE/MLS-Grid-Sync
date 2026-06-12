package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/LunarHUE/MLS-Grid-Sync/config"
	"github.com/LunarHUE/MLS-Grid-Sync/storage"
)

var (
	cleanupYes         bool
	cleanupKnowNotProd bool
)

var workerStorageCleanupCmd = &cobra.Command{
	Use:   "worker-storage-cleanup",
	Short: "Delete attachment objects from the configured storage backend",
	Long: `Delete attachment objects from the configured storage backend.

Removes <key_prefix> from the configured backend (or the entire root
if key_prefix is empty). Intended for cleaning up after the Phase 2
bounded worker drain — do NOT run against a production backend.

The local backend can be cleaned with just --yes. Non-local backends
additionally require --i-know-this-is-not-prod as a deliberate guard
against fat-fingered production cleanup.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runWorkerStorageCleanup(cmd.Context(), os.Stdin, os.Stdout)
	},
}

func runWorkerStorageCleanup(ctx context.Context, in io.Reader, out io.Writer) error {
	// Fire the prod-safety gate BEFORE constructing the storer. This
	// (a) makes the gate testable without spinning up a real backend,
	// and (b) refuses cleanup before any network call that might cost
	// money or hit production credentials.
	if appConfig.Storage.Backend != "local" && appConfig.Storage.Backend != "" && !cleanupKnowNotProd {
		return fmt.Errorf("non-local backend %q requires --i-know-this-is-not-prod", appConfig.Storage.Backend)
	}

	storer, err := newStorer(appConfig.Storage)
	if err != nil {
		return fmt.Errorf("construct storer: %w", err)
	}
	cleaner, ok := storer.(storage.Cleaner)
	if !ok {
		return fmt.Errorf("backend %q does not implement Cleaner", appConfig.Storage.Backend)
	}

	prefix := appConfig.Storage.KeyPrefix
	target := describeTarget(appConfig.Storage, prefix)

	fmt.Fprintf(out, "About to delete: %s\n", target)
	fmt.Fprintf(out, "Backend:         %s\n", appConfig.Storage.Backend)
	fmt.Fprintf(out, "Prefix:          %q (empty = entire root)\n", prefix)

	if !cleanupYes {
		fmt.Fprint(out, "\nProceed? Type 'y' to confirm: ")
		reader := bufio.NewReader(in)
		line, _ := reader.ReadString('\n')
		if strings.TrimSpace(strings.ToLower(line)) != "y" {
			fmt.Fprintln(out, "Aborted.")
			return nil
		}
	}

	if err := cleaner.CleanupPrefix(ctx, prefix); err != nil {
		return fmt.Errorf("cleanup: %w", err)
	}
	fmt.Fprintln(out, "Cleanup complete.")
	return nil
}

// describeTarget renders the most-honest description of what will be
// deleted. For local: the absolute path the user can verify with `ls`.
// For other backends: the container/bucket and prefix.
func describeTarget(cfg config.StorageConfig, prefix string) string {
	switch cfg.Backend {
	case "local":
		if prefix == "" {
			return cfg.Local.RootDir + "/  (entire root)"
		}
		return cfg.Local.RootDir + "/" + prefix
	case "azure":
		return fmt.Sprintf("azure://%s/%s", cfg.Azure.Container, prefix)
	case "s3":
		return fmt.Sprintf("s3://%s/%s", cfg.S3.Bucket, prefix)
	default:
		return fmt.Sprintf("<%s>/%s", cfg.Backend, prefix)
	}
}

func init() {
	workerStorageCleanupCmd.Flags().BoolVar(&cleanupYes, "yes", false,
		"Skip the interactive confirmation prompt.")
	workerStorageCleanupCmd.Flags().BoolVar(&cleanupKnowNotProd, "i-know-this-is-not-prod", false,
		"Required when backend is not 'local' — explicit acknowledgement that the cleanup target is not production.")
	rootCmd.AddCommand(workerStorageCleanupCmd)
}
