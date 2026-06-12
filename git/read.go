package git

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/lunarhue/libs-go/log"
)

// Reader provides read-only git operations on an existing repository.
type Reader struct {
	repo repo
}

// OpenReader wraps an existing directory as a Reader.
func OpenReader(dir string) *Reader {
	return &Reader{repo: repo{dir: dir}}
}

// CloneOption configures a clone operation.
type CloneOption func(*cloneOpts)

type cloneOpts struct {
	env []string
}

// WithEnv adds environment variables to the clone command.
func WithEnv(env ...string) CloneOption {
	return func(o *cloneOpts) {
		o.env = append(o.env, env...)
	}
}

// CloneReader clones a repository and returns a Reader for the working tree.
func CloneReader(ctx context.Context, url, dir string, opts ...CloneOption) (*Reader, error) {
	var o cloneOpts
	for _, fn := range opts {
		fn(&o)
	}
	r := &Reader{repo: repo{dir: dir}}
	if err := r.clone(ctx, url, o.env); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Reader) clone(ctx context.Context, url string, env []string) error {
	cmd := execGit(ctx, "clone", url, r.repo.dir)
	cmd.Env = append(cmd.Environ(), env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git clone: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// Dir returns the repository working directory.
func (r *Reader) Dir() string { return r.repo.dir }

// RevParse runs git rev-parse on the given ref.
func (r *Reader) RevParse(ctx context.Context, ref string) (string, error) {
	return r.repo.run(ctx, "rev-parse", ref)
}

// HeadShort returns the abbreviated HEAD commit hash.
func (r *Reader) HeadShort(ctx context.Context) (string, error) {
	return r.repo.run(ctx, "rev-parse", "--short", "HEAD")
}

// HeadCommit returns the full HEAD hash and the one-line subject.
func (r *Reader) HeadCommit(ctx context.Context) (sha, subject string, err error) {
	sha, err = r.repo.run(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", "", err
	}
	subject, err = r.repo.run(ctx, "log", "-1", "--format=%s")
	if err != nil {
		// Non-fatal: return hash without subject.
		return sha, "", nil
	}
	return sha, subject, nil
}

// IsDirty returns true if the working tree has uncommitted changes.
func (r *Reader) IsDirty(ctx context.Context) (bool, error) {
	out, err := r.repo.run(ctx, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return len(out) > 0, nil
}

// RemoteURL returns the URL of the named remote.
func (r *Reader) RemoteURL(ctx context.Context, name string) (string, error) {
	return r.repo.run(ctx, "remote", "get-url", name)
}

// ConfigGet reads a local git config value. Returns the value, whether it was
// found, and any unexpected error.
func (r *Reader) ConfigGet(ctx context.Context, key string) (string, bool, error) {
	out, err := r.repo.run(ctx, "config", "--local", "--get", key)
	if err != nil {
		// git config exits 1 when key is not found — not a real error.
		if strings.Contains(err.Error(), "exit status 1") {
			return "", false, nil
		}
		return "", false, err
	}
	return out, true, nil
}

// FileExistsAtRef checks whether a path exists at the given ref using
// cat-file -e.
func (r *Reader) FileExistsAtRef(ctx context.Context, ref, path string) (bool, error) {
	err := r.repo.runSilent(ctx, "cat-file", "-e", ref+":"+path)
	if err != nil {
		// cat-file -e exits 128 if the object doesn't exist.
		return false, nil
	}
	return true, nil
}

// LsTreeAtRef returns the file listing at the given ref.
func (r *Reader) LsTreeAtRef(ctx context.Context, ref string) (string, error) {
	return r.repo.run(ctx, "ls-tree", "-r", "--name-only", ref)
}

// Fetch fetches a ref from a remote.
func (r *Reader) Fetch(ctx context.Context, remote, ref string) error {
	return r.repo.runSilent(ctx, "fetch", remote, ref)
}

// ResetHard resets the working tree to the given ref.
func (r *Reader) ResetHard(ctx context.Context, ref string) error {
	return r.repo.runSilent(ctx, "reset", "--hard", ref)
}

// ForceCleanCheckout performs a full clobber sequence to bring the working tree
// in line with origin/<ref>:
//  1. Abort any in-progress merge/rebase/cherry-pick
//  2. Clear stash
//  3. Fetch origin <ref>
//  4. Checkout <ref> (reattach HEAD if detached)
//  5. Reset --hard origin/<ref>
//  6. Clean -fdx (remove untracked + gitignored files)
func (r *Reader) ForceCleanCheckout(ctx context.Context, ref string) error {
	// Abort any in-progress operations (best-effort).
	_ = r.repo.runSilent(ctx, "merge", "--abort")
	_ = r.repo.runSilent(ctx, "rebase", "--abort")
	_ = r.repo.runSilent(ctx, "cherry-pick", "--abort")

	// Clear stash.
	_ = r.repo.runSilent(ctx, "stash", "clear")

	// Fetch.
	if err := r.repo.runSilent(ctx, "fetch", "origin", ref); err != nil {
		return fmt.Errorf("fetch origin %s: %w", ref, err)
	}

	// Checkout ref (reattach HEAD).
	_ = r.repo.runSilent(ctx, "checkout", ref)

	// Reset to remote.
	if err := r.repo.runSilent(ctx, "reset", "--hard", "origin/"+ref); err != nil {
		return fmt.Errorf("reset --hard origin/%s: %w", ref, err)
	}

	// Clean untracked and gitignored files.
	if err := r.repo.runSilent(ctx, "clean", "-fdx"); err != nil {
		return fmt.Errorf("clean -fdx: %w", err)
	}

	return nil
}

// PollOption configures PollForFile behavior.
type PollOption func(*pollOpts)

type pollOpts struct {
	initialBackoff time.Duration
	maxBackoff     time.Duration
}

// WithPollBackoff sets the initial and maximum backoff durations.
func WithPollBackoff(initial, max time.Duration) PollOption {
	return func(o *pollOpts) {
		o.initialBackoff = initial
		o.maxBackoff = max
	}
}

// PollForFile polls until ref:<path> exists in the repository. On each
// iteration it fetches and resets to pick up new commits. On the first
// failure it dumps recent commits and the tree for diagnostics.
func (r *Reader) PollForFile(ctx context.Context, ref, path string, opts ...PollOption) error {
	o := pollOpts{
		initialBackoff: 2 * time.Second,
		maxBackoff:     30 * time.Second,
	}
	for _, fn := range opts {
		fn(&o)
	}

	backoff := o.initialBackoff
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return ErrPollTimeout
		}

		exists, _ := r.FileExistsAtRef(ctx, ref, path)
		if exists {
			return nil
		}

		// Debug dump on first failure.
		if attempt == 0 {
			if out, err := r.repo.run(ctx, "log", "--oneline", "-5", ref); err == nil {
				log.Warnf("git: poll: recent commits at %s:\n%s", ref, out)
			}
			if out, err := r.LsTreeAtRef(ctx, ref); err == nil {
				log.Warnf("git: poll: tree at %s:\n%s", ref, out)
			} else {
				log.Warnf("git: poll: ls-tree %s failed: %v", ref, err)
			}
		}

		log.Infof("git: waiting for %s:%s (attempt %d)", ref, path, attempt+1)
		select {
		case <-ctx.Done():
			return ErrPollTimeout
		case <-time.After(backoff):
		}
		if backoff < o.maxBackoff {
			backoff *= 2
		}

		// Fetch + reset to pick up new commits.
		// Extract remote and branch from ref like "origin/main".
		parts := strings.SplitN(ref, "/", 2)
		if len(parts) == 2 {
			_ = r.repo.runSilent(ctx, "fetch", parts[0], parts[1])
			_ = r.repo.runSilent(ctx, "reset", "--hard", ref)
		}
	}
}

// execGit creates an exec.Cmd for git with the given args (no repo dir set).
func execGit(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "git", args...)
}
