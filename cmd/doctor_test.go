package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/config"
	"github.com/LunarHUE/MLS-Grid-Sync/doctor"
	"github.com/LunarHUE/MLS-Grid-Sync/storage"
)

func okStorer(context.Context, config.StorageConfig) (storage.Storer, error) {
	return &storage.FakeStorer{}, nil
}

// healthyDeps is a config-only Deps (no DB/MLS) used to drive runDoctor in
// isolation. DB checks will fail, which is fine for render/exit-code tests.
func healthyDeps() doctor.Deps {
	return doctor.Deps{
		Config: &config.Config{
			MLS:     config.MLSConfig{Token: "t", OriginatingSystem: "actris", V2URL: "https://x/v2", APIRPS: 1, MediaDownloadRPS: 1},
			Storage: config.StorageConfig{Backend: "fake"},
			Server:  config.ServerConfig{APIKey: "k", CORSAllowedOrigins: "https://app"},
		},
		BuildStorer: okStorer,
	}
}

func TestRunDoctor_HumanRender(t *testing.T) {
	buf := &bytes.Buffer{}
	err := runDoctor(context.Background(), healthyDeps(), doctor.Options{SkipMLS: true, SkipStorage: true}, false, buf)

	// DB checks fail (no handle), so a non-zero exit is expected.
	require.ErrorIs(t, err, errDoctorChecksFailed)
	out := buf.String()
	assert.Contains(t, out, "PASS")
	assert.Contains(t, out, "config")
	assert.Contains(t, out, "FAIL")    // postgres has no handle
	assert.Contains(t, out, "SKIPPED") // skip-mls / skip-storage
}

func TestRunDoctor_JSON(t *testing.T) {
	buf := &bytes.Buffer{}
	err := runDoctor(context.Background(), healthyDeps(), doctor.Options{SkipMLS: true, SkipStorage: true}, true, buf)
	require.ErrorIs(t, err, errDoctorChecksFailed)

	var rep doctor.Report
	require.NoError(t, json.Unmarshal(buf.Bytes(), &rep))
	assert.False(t, rep.OK)
	assert.NotEmpty(t, rep.Checks)
	assert.Equal(t, len(rep.Checks), rep.Summary.Pass+rep.Summary.Warn+rep.Summary.Fail+rep.Summary.Skipped)
}

// TestDoctorCmd_BypassesRootPreRun proves the config-free guarantee: even if a
// future root PersistentPreRunE would fail, `doctor` still runs because it
// overrides the hook. Dispatched through rootCmd to exercise the real chain.
func TestDoctorCmd_BypassesRootPreRun(t *testing.T) {
	prev := rootCmd.PersistentPreRunE
	rootCmd.PersistentPreRunE = func(*cobra.Command, []string) error {
		return errors.New("root pre-run must not run for doctor")
	}
	t.Cleanup(func() { rootCmd.PersistentPreRunE = prev })

	buf := &bytes.Buffer{}
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs([]string{"doctor", "--json", "--skip-mls", "--skip-storage"})
	t.Cleanup(func() {
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		doctorJSON, doctorSkipMLS, doctorSkipStorage, doctorStrict = false, false, false, false
	})

	// doctor runs and reports; the command returns the sentinel (DB checks
	// fail in this env) but it MUST have produced output rather than aborting
	// in the root hook.
	_ = rootCmd.Execute()
	assert.Contains(t, buf.String(), `"checks"`, "doctor must run despite a failing root PersistentPreRunE")
}
