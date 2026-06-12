package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LunarHUE/MLS-Grid-Sync/config"
)

// withCleanupFlags is a helper that resets and restores the package-
// level cleanup flag vars between tests. Without it, a `--yes` flag
// set in one test bleeds into the next.
func withCleanupFlags(t *testing.T, yes, knowNotProd bool) {
	t.Helper()
	origYes, origKnow := cleanupYes, cleanupKnowNotProd
	cleanupYes, cleanupKnowNotProd = yes, knowNotProd
	t.Cleanup(func() { cleanupYes, cleanupKnowNotProd = origYes, origKnow })
}

// withAppConfig temporarily replaces appConfig for the duration of a
// test. Cleanup subcommand reads appConfig.Storage at run-time.
func withAppConfig(t *testing.T, cfg *config.Config) {
	t.Helper()
	orig := appConfig
	appConfig = cfg
	t.Cleanup(func() { appConfig = orig })
}

func TestRunWorkerStorageCleanup_LocalWithYes(t *testing.T) {
	root := t.TempDir()
	// Plant some content under the prefix so we can verify deletion.
	prefixDir := filepath.Join(root, "drain-test")
	if err := os.MkdirAll(prefixDir, 0o755); err != nil {
		t.Fatalf("mkdir prefix: %v", err)
	}
	if err := os.WriteFile(filepath.Join(prefixDir, "doomed.txt"), []byte("bye"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	// Plant a sibling outside the prefix to confirm it survives.
	if err := os.WriteFile(filepath.Join(root, "sibling.txt"), []byte("alive"), 0o644); err != nil {
		t.Fatalf("seed sibling: %v", err)
	}

	withAppConfig(t, &config.Config{
		Storage: config.StorageConfig{
			Backend:   "local",
			KeyPrefix: "drain-test",
			Local:     config.LocalStorageConfig{RootDir: root, CapBytes: 1 << 30},
		},
	})
	withCleanupFlags(t, true, false)

	var out bytes.Buffer
	if err := runWorkerStorageCleanup(context.Background(), nil, &out); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if !strings.Contains(out.String(), "Cleanup complete.") {
		t.Errorf("expected completion line, got: %s", out.String())
	}
	if _, err := os.Stat(prefixDir); !os.IsNotExist(err) {
		t.Errorf("prefix dir survived cleanup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "sibling.txt")); err != nil {
		t.Errorf("sibling unexpectedly removed: %v", err)
	}
}

func TestRunWorkerStorageCleanup_PromptDeclines(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "survivor.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	withAppConfig(t, &config.Config{
		Storage: config.StorageConfig{
			Backend: "local",
			Local:   config.LocalStorageConfig{RootDir: root, CapBytes: 1 << 30},
		},
	})
	withCleanupFlags(t, false, false)

	var out bytes.Buffer
	// Pipe in "n\n" — anything but "y" must abort.
	if err := runWorkerStorageCleanup(context.Background(), strings.NewReader("n\n"), &out); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if !strings.Contains(out.String(), "Aborted") {
		t.Errorf("expected Aborted, got: %s", out.String())
	}
	if _, err := os.Stat(filepath.Join(root, "survivor.txt")); err != nil {
		t.Errorf("file removed despite abort: %v", err)
	}
}

// TestRunWorkerStorageCleanup_NonLocalRequiresKnowNotProd asserts the
// prod-safety gate fires BEFORE any storer construction or network
// call, and that the error names the flag the user must pass.
func TestRunWorkerStorageCleanup_NonLocalRequiresKnowNotProd(t *testing.T) {
	withAppConfig(t, &config.Config{
		Storage: config.StorageConfig{Backend: "azure"},
	})
	withCleanupFlags(t, true /* yes */, false /* knowNotProd */)

	var out bytes.Buffer
	err := runWorkerStorageCleanup(context.Background(), nil, &out)
	if err == nil {
		t.Fatal("expected an error for non-local cleanup")
	}
	if !strings.Contains(err.Error(), "i-know-this-is-not-prod") {
		t.Errorf("error should name the required flag, got: %v", err)
	}
	// The gate fires PRE-construct, so no banner output should have
	// been emitted yet.
	if strings.Contains(out.String(), "About to delete") {
		t.Errorf("gate should fire before describeTarget output, got: %s", out.String())
	}
}

// TestRunWorkerStorageCleanup_NonLocalWithKnowNotProd shows the gate
// CAN be cleared with the flag set. The next stage (newStorer) will
// then error because we provided no auth, but the gate itself passed.
func TestRunWorkerStorageCleanup_NonLocalWithKnowNotProd(t *testing.T) {
	withAppConfig(t, &config.Config{
		Storage: config.StorageConfig{
			Backend: "azure",
			Azure:   config.AzureStorageConfig{Container: "media"},
		},
	})
	withCleanupFlags(t, true /* yes */, true /* knowNotProd */)

	var out bytes.Buffer
	err := runWorkerStorageCleanup(context.Background(), nil, &out)
	if err == nil {
		t.Fatal("expected newStorer error after gate cleared")
	}
	// Past the gate: error should NOT mention the flag.
	if strings.Contains(err.Error(), "i-know-this-is-not-prod") {
		t.Errorf("flag-required error after flag set: %v", err)
	}
}

func TestDescribeTarget_Local(t *testing.T) {
	got := describeTarget(config.StorageConfig{
		Backend: "local",
		Local:   config.LocalStorageConfig{RootDir: "/tmp/x"},
	}, "drain-test")
	if !strings.Contains(got, "/tmp/x") || !strings.Contains(got, "drain-test") {
		t.Errorf("missing path components: %q", got)
	}
}

func TestDescribeTarget_LocalEmptyPrefix(t *testing.T) {
	got := describeTarget(config.StorageConfig{
		Backend: "local",
		Local:   config.LocalStorageConfig{RootDir: "/tmp/x"},
	}, "")
	if !strings.Contains(got, "entire root") {
		t.Errorf("expected entire-root note: %q", got)
	}
}
