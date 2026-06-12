// Package git provides typed Reader and Writer abstractions over git CLI
// operations. Reader is used by both agent and controller code for read-only
// operations; Writer is used only by the controller to mutate repositories.
package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// LogFunc is called for each output line during a streamed command.
type LogFunc func(line string, isError bool)

// repo holds the working directory for git operations.
type repo struct {
	dir string
}

// run executes a git command in the repo directory and returns trimmed stdout.
func (r *repo) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if r.dir != "" {
		cmd.Dir = r.dir
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return strings.TrimSpace(string(out)),
			fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(stderr.String()), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// runSilent executes a git command in the repo directory, discarding stdout.
func (r *repo) runSilent(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	if r.dir != "" {
		cmd.Dir = r.dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return nil
}

// RunAndStream executes an arbitrary command, streaming stdout and stderr line
// by line to logFn. This is used for both git and non-git commands (e.g.
// nixos-rebuild).
func RunAndStream(ctx context.Context, dir string, logFn LogFunc, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	done := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			logFn("["+name+"] "+scanner.Text(), true)
		}
		close(done)
	}()

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		logFn("["+name+"] "+scanner.Text(), false)
	}
	<-done

	return cmd.Wait()
}
