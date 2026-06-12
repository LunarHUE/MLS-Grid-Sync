package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/config"
)

// originatingSystemFlag is the canonical flag name; added to init,
// import, and sync. Resolution precedence is per-command but the flag
// definition itself is shared so tests and docs reference one source.
const originatingSystemFlag = "originating-system"

// errOriginatingSystemUnresolved is returned when no resolution path
// produced a value AND the runtime is non-interactive (so the prompt
// can't fire). The message names every option so the operator can fix
// it without grep.
var errOriginatingSystemUnresolved = errors.New(
	"originating system not configured — set one of:\n" +
		"  --originating-system <name>\n" +
		"  MLS_SYNC_MLS_ORIGINATING_SYSTEM env var\n" +
		"  mls.originating_system in config.yaml\n" +
		"or run interactively to be prompted")

// Hooks for tests: stub TTY detection and the prompt itself so the
// precedence/no-TTY branches can be exercised without a real terminal.
var (
	isTerminal       = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
	stdinForPrompt   io.Reader = os.Stdin
	stdoutForPrompt  io.Writer = os.Stdout
	discoverSystems            = defaultDiscoverSystems // overridden in tests
)

// defaultDiscoverSystems is wired up by cmd/systems.go at init time so
// the resolver can offer discovered names as menu choices without a
// hard package cycle. If discovery is not available or fails, it
// returns an empty slice — the prompt falls back to free-text.
var defaultDiscoverSystems = func() []string { return nil }

// resolveOriginatingSystem returns the originating system to use,
// applying precedence: --originating-system flag > env+config (merged
// by the config loader) > interactive prompt > hard error.
//
// On success it ALSO writes the resolved value into
// appConfig.MLS.OriginatingSystem so the existing call shape down to
// Service.RunInitial / Service.RunDelta doesn't need to change.
func resolveOriginatingSystem(cmd *cobra.Command, appConfig *config.Config) (string, error) {
	// 1. Flag.
	if f := cmd.Flag(originatingSystemFlag); f != nil && f.Changed {
		v := f.Value.String()
		if v != "" {
			appConfig.MLS.OriginatingSystem = v
			return v, nil
		}
	}

	// 2. Env + config (env merges into config via libs-go/config's
	//    MLS_SYNC prefix at load time — config/config.go:43).
	if v := strings.TrimSpace(appConfig.MLS.OriginatingSystem); v != "" {
		return v, nil
	}

	// 3. Interactive prompt iff stdin is a TTY.
	if !isTerminal() {
		return "", errOriginatingSystemUnresolved
	}
	v, err := promptOriginatingSystem(discoverSystems())
	if err != nil {
		return "", err
	}
	appConfig.MLS.OriginatingSystem = v
	return v, nil
}

// promptOriginatingSystem asks for an originating system name. If
// `discovered` is non-empty it offers a menu (1-based selection or a
// custom free-text entry); otherwise it's free-text only.
func promptOriginatingSystem(discovered []string) (string, error) {
	w := stdoutForPrompt
	r := bufio.NewReader(stdinForPrompt)

	if len(discovered) > 0 {
		fmt.Fprintln(w, "Available originating systems for this token:")
		for i, name := range discovered {
			fmt.Fprintf(w, "  %d) %s\n", i+1, name)
		}
		fmt.Fprintln(w, "  c) custom (enter your own)")
		fmt.Fprint(w, "Choose [1-", len(discovered), " or c]: ")
		line, err := r.ReadString('\n')
		if err != nil {
			return "", fmt.Errorf("read prompt: %w", err)
		}
		choice := strings.TrimSpace(line)
		if choice != "c" && choice != "C" {
			n, err := strconv.Atoi(choice)
			if err != nil || n < 1 || n > len(discovered) {
				return "", fmt.Errorf("invalid selection %q", choice)
			}
			return discovered[n-1], nil
		}
	}

	fmt.Fprint(w, "Originating system name: ")
	line, err := r.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read prompt: %w", err)
	}
	v := strings.TrimSpace(line)
	if v == "" {
		return "", fmt.Errorf("originating system cannot be empty")
	}
	return v, nil
}

// addOriginatingSystemFlag wires --originating-system onto a cobra
// command. Each subcommand calls this in its own init() — the flag is
// per-command (not persistent) so help text only lists it where it
// applies.
func addOriginatingSystemFlag(cmd *cobra.Command) {
	cmd.Flags().String(originatingSystemFlag, "", "MLS OriginatingSystemName (overrides config + env)")
}
