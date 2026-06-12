package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/LunarHUE/MLS-Grid-Sync/config"
)

// withTestHooks resets the package-level injection points and restores
// them on test end. Pattern keeps the package vars private while
// letting tests substitute TTY behavior + IO streams.
func withTestHooks(t *testing.T, tty bool, stdin string, discovered []string) (*bytes.Buffer, func()) {
	t.Helper()
	out := &bytes.Buffer{}

	prevTTY := isTerminal
	prevIn := stdinForPrompt
	prevOut := stdoutForPrompt
	prevDiscover := discoverSystems

	isTerminal = func() bool { return tty }
	stdinForPrompt = strings.NewReader(stdin)
	stdoutForPrompt = out
	discoverSystems = func() []string { return discovered }

	return out, func() {
		isTerminal = prevTTY
		stdinForPrompt = prevIn
		stdoutForPrompt = prevOut
		discoverSystems = prevDiscover
	}
}

// newCmdWithFlag returns a cobra.Command with --originating-system
// wired up, optionally pre-set as if from CLI args.
func newCmdWithFlag(t *testing.T, flagValue string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	addOriginatingSystemFlag(cmd)
	if flagValue != "" {
		require.NoError(t, cmd.Flags().Set(originatingSystemFlag, flagValue))
	}
	return cmd
}

func TestResolveOriginatingSystem_FlagWinsOverConfig(t *testing.T) {
	_, cleanup := withTestHooks(t, true, "", nil)
	defer cleanup()

	cmd := newCmdWithFlag(t, "from-flag")
	cfg := &config.Config{}
	cfg.MLS.OriginatingSystem = "from-config"

	got, err := resolveOriginatingSystem(cmd, cfg)
	require.NoError(t, err)
	assert.Equal(t, "from-flag", got)
	assert.Equal(t, "from-flag", cfg.MLS.OriginatingSystem, "resolved value must be written back to appConfig")
}

func TestResolveOriginatingSystem_ConfigUsedWhenNoFlag(t *testing.T) {
	_, cleanup := withTestHooks(t, true, "", nil)
	defer cleanup()

	cmd := newCmdWithFlag(t, "")
	cfg := &config.Config{}
	cfg.MLS.OriginatingSystem = "from-config"

	got, err := resolveOriginatingSystem(cmd, cfg)
	require.NoError(t, err)
	assert.Equal(t, "from-config", got)
}

func TestResolveOriginatingSystem_NoTTYNoConfigErrors(t *testing.T) {
	_, cleanup := withTestHooks(t, false, "", nil)
	defer cleanup()

	cmd := newCmdWithFlag(t, "")
	cfg := &config.Config{}

	_, err := resolveOriginatingSystem(cmd, cfg)
	require.Error(t, err)
	msg := err.Error()
	// Hard error must name every resolution option so an operator can
	// fix it without reading code.
	assert.Contains(t, msg, "--originating-system")
	assert.Contains(t, msg, "MLS_SYNC_MLS_ORIGINATING_SYSTEM")
	assert.Contains(t, msg, "config.yaml")
	assert.Contains(t, msg, "interactively")
}

func TestResolveOriginatingSystem_TTYFreeTextPrompt(t *testing.T) {
	out, cleanup := withTestHooks(t, true, "actris\n", nil)
	defer cleanup()

	cmd := newCmdWithFlag(t, "")
	cfg := &config.Config{}

	got, err := resolveOriginatingSystem(cmd, cfg)
	require.NoError(t, err)
	assert.Equal(t, "actris", got)
	assert.Equal(t, "actris", cfg.MLS.OriginatingSystem)
	assert.Contains(t, out.String(), "Originating system name:", "free-text prompt must surface")
}

func TestResolveOriginatingSystem_TTYDiscoveredMenu(t *testing.T) {
	out, cleanup := withTestHooks(t, true, "2\n", []string{"actris", "flinthills"})
	defer cleanup()

	cmd := newCmdWithFlag(t, "")
	cfg := &config.Config{}

	got, err := resolveOriginatingSystem(cmd, cfg)
	require.NoError(t, err)
	assert.Equal(t, "flinthills", got, "menu choice 2 of [actris, flinthills] must select flinthills")
	assert.Contains(t, out.String(), "Available originating systems")
}

func TestResolveOriginatingSystem_TTYDiscoveredMenuCustom(t *testing.T) {
	_, cleanup := withTestHooks(t, true, "c\nfreshwater\n", []string{"actris"})
	defer cleanup()

	cmd := newCmdWithFlag(t, "")
	cfg := &config.Config{}

	got, err := resolveOriginatingSystem(cmd, cfg)
	require.NoError(t, err)
	assert.Equal(t, "freshwater", got, "'c' should escape the menu into free-text entry")
}

func TestResolveOriginatingSystem_TTYEmptyFreeTextErrors(t *testing.T) {
	_, cleanup := withTestHooks(t, true, "\n", nil)
	defer cleanup()

	cmd := newCmdWithFlag(t, "")
	cfg := &config.Config{}

	_, err := resolveOriginatingSystem(cmd, cfg)
	require.Error(t, err, "empty answer at the free-text prompt is a hard error")
}
