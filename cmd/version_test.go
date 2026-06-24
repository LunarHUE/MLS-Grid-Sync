package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"runtime"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/version"
)

// setVersionLdflags overrides the version package's -X-injected globals and
// restores them via t.Cleanup so a test can exercise the stamped path without
// leaking into other tests.
func setVersionLdflags(t *testing.T, ver, commit, buildDate string) {
	t.Helper()
	pv, pc, pb := version.Version, version.Commit, version.BuildDate
	version.Version, version.Commit, version.BuildDate = ver, commit, buildDate
	t.Cleanup(func() {
		version.Version, version.Commit, version.BuildDate = pv, pc, pb
	})
}

// runVersion dispatches `version [args]` through rootCmd (cobra delegates a
// child's Execute() to the root anyway, so driving the root is the reliable
// path), capturing stdout and resetting shared command/flag state afterward.
func runVersion(t *testing.T, args ...string) string {
	t.Helper()
	buf := &bytes.Buffer{}
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs(append([]string{"version"}, args...))
	t.Cleanup(func() {
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		versionJSON = false
	})
	require.NoError(t, rootCmd.Execute())
	return buf.String()
}

// TestVersionCmd_JSON asserts --json emits valid JSON decoding into
// version.Build, with the FULL commit SHA preserved (machine output is not
// lossily truncated).
func TestVersionCmd_JSON(t *testing.T) {
	const fullSHA = "0123456789abcdef0123456789abcdef01234567"
	setVersionLdflags(t, "v9.9.9", fullSHA, "2026-06-24T00:00:00Z")

	var b version.Build
	require.NoError(t, json.Unmarshal([]byte(runVersion(t, "--json")), &b))

	assert.Equal(t, "v9.9.9", b.Version)
	assert.Equal(t, fullSHA, b.Commit, "JSON must carry the full SHA, not the short form")
	assert.Equal(t, "2026-06-24T00:00:00Z", b.BuildDate)
	assert.NotEmpty(t, b.GoVersion)
	assert.NotEmpty(t, b.Module)
}

// TestVersionCmd_Human asserts the human output shortens the commit to 7 chars
// (never the full SHA) and reports the Go toolchain.
func TestVersionCmd_Human(t *testing.T) {
	const fullSHA = "0123456789abcdef0123456789abcdef01234567"
	setVersionLdflags(t, "v9.9.9", fullSHA, "")

	out := runVersion(t)
	assert.Contains(t, out, "Version:    v9.9.9")
	assert.Contains(t, out, "Commit:     0123456")
	assert.NotContains(t, out, fullSHA, "human output must not print the full SHA")
	assert.Contains(t, out, runtime.Version())
	assert.Contains(t, out, "Module:")
}

// TestVersionCmd_DefaultsSafe asserts that with no ldflags the command still
// produces safe, non-empty output (Version defaults to "dev"); never empty or
// a crash.
func TestVersionCmd_DefaultsSafe(t *testing.T) {
	setVersionLdflags(t, "", "", "")

	var b version.Build
	require.NoError(t, json.Unmarshal([]byte(runVersion(t, "--json")), &b))
	assert.Equal(t, "dev", b.Version)
	assert.NotEmpty(t, b.GoVersion)
	assert.NotEmpty(t, b.Module)
	assert.NotEmpty(t, b.Commit) // "unknown" at worst, never empty
}

// TestVersionCmd_BypassesRootPreRun proves the config-free guarantee: even if a
// future root PersistentPreRunE would fail (e.g. a stricter config/DB demand),
// `mls-cli version` still succeeds because it overrides the hook. Dispatched
// through rootCmd to exercise the real chain.
func TestVersionCmd_BypassesRootPreRun(t *testing.T) {
	prev := rootCmd.PersistentPreRunE
	rootCmd.PersistentPreRunE = func(*cobra.Command, []string) error {
		return fmt.Errorf("root pre-run must not run for version")
	}
	t.Cleanup(func() { rootCmd.PersistentPreRunE = prev })

	buf := &bytes.Buffer{}
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs([]string{"version", "--json"})
	t.Cleanup(func() {
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		versionJSON = false
	})

	require.NoError(t, rootCmd.Execute(), "version must bypass the root PersistentPreRunE")
	assert.Contains(t, buf.String(), `"version"`)
}
