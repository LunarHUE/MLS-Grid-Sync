// Package git provides a minimal read-only Reader over the git CLI. It exposes
// only what version.Info() needs to stamp processor_version on a `go run`
// build: the abbreviated HEAD hash and whether the working tree is dirty.
package git

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Reader provides read-only git operations on an existing repository.
type Reader struct {
	dir string
}

// OpenReader wraps an existing directory as a Reader. An empty dir uses the
// current working directory.
func OpenReader(dir string) *Reader {
	return &Reader{dir: dir}
}

// HeadShort returns the abbreviated HEAD commit hash.
func (r *Reader) HeadShort(ctx context.Context) (string, error) {
	return r.run(ctx, "rev-parse", "--short", "HEAD")
}

// IsDirty returns true if the working tree has uncommitted changes.
func (r *Reader) IsDirty(ctx context.Context) (bool, error) {
	out, err := r.run(ctx, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return len(out) > 0, nil
}

// run executes a git command in the repo directory and returns trimmed stdout.
func (r *Reader) run(ctx context.Context, args ...string) (string, error) {
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
