package version

import (
	"context"
	"fmt"
	"net/url"
	"runtime/debug"
	"sync"

	"github.com/LunarHUE/MLS-Grid-Sync/git"
	"github.com/lunarhue/libs-go/log"
)

// repoFallback is the last-resort module path used only when build info is
// unavailable (e.g. some `go test` runs). The runtime build-info path is
// primary, so a repo move is picked up automatically from go.mod with no edit
// here — this constant is purely defensive.
const repoFallback = "github.com/LunarHUE/MLS-Grid-Sync"

// RepoURL returns the project's source repository URL, derived at runtime from
// the module path in go.mod via build info — e.g.
// https://github.com/LunarHUE/MLS-Grid-Sync. No hardcoded URL to keep in sync.
func RepoURL() string {
	if bi, ok := readBuildInfoFn(); ok && bi != nil && bi.Main.Path != "" {
		return "https://" + bi.Main.Path
	}
	return "https://" + repoFallback
}

// NewIssueURL returns a pre-filled "open a new issue" URL for the repo. A
// non-empty title/body is URL-encoded into the query string so the operator
// lands on a populated GitHub issue form.
func NewIssueURL(title, body string) string {
	u := RepoURL() + "/issues/new"
	q := url.Values{}
	if title != "" {
		q.Set("title", title)
	}
	if body != "" {
		q.Set("body", body)
	}
	if e := q.Encode(); e != "" {
		u += "?" + e
	}
	return u
}

// Version is set at build time via -X ldflags.
var Version string

// Commit is the git hash, set at build time via -X ldflags.
var Commit string

// Info returns the formatted version stamp written to processor_version.
// Memoized — compute() runs at most once per process. Provenance metadata,
// best-effort, never load-bearing for correctness.
func Info() string {
	infoOnce.Do(func() { cachedInfo = compute() })
	return cachedInfo
}

var (
	infoOnce   sync.Once
	cachedInfo string
)

// Injected for tests so the memoization and fallback chain can be exercised
// without depending on a real git repo or the test binary's VCS stamp.
var (
	runGitFn        = realRunGit
	readBuildInfoFn = debug.ReadBuildInfo
)

// compute resolves the version stamp once. Precedence:
//  1. debug.ReadBuildInfo VCS metadata (zero-cost, set by `go build` >=1.18)
//  2. ldflags-set Commit (legacy path, kept for back-compat)
//  3. fork git once via runGitFn (the `go run` path)
//  4. "unknown" sentinel — never errors out; a missing .git/git binary
//     degrades the stamp rather than failing the pass.
func compute() string {
	version := Version
	if version == "" {
		version = "dev"
	}

	if sha, dirty, ok := buildInfoVCS(); ok {
		return format(version, sha, dirty)
	}

	if Commit != "" {
		return format(version, Commit, false)
	}

	if sha, dirty, err := runGitFn(); err == nil {
		return format(version, sha, dirty)
	}

	log.Infof("version: build-info and git both unavailable; processor_version stamps degraded to 'unknown'")
	return format(version, "unknown", false)
}

func format(version, sha string, dirty bool) string {
	s := fmt.Sprintf("mls_sync_db %s (%s)", version, sha)
	if dirty {
		s += " (dirty)"
	}
	return s
}

func buildInfoVCS() (sha string, dirty bool, ok bool) {
	bi, biOK := readBuildInfoFn()
	if !biOK || bi == nil {
		return "", false, false
	}
	var revision, modified string
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if revision == "" {
		return "", false, false
	}
	short := revision
	if len(short) > 7 {
		short = short[:7]
	}
	return short, modified == "true", true
}

func realRunGit() (sha string, dirty bool, err error) {
	r := git.OpenReader(".")
	ctx := context.Background()
	short, err := r.HeadShort(ctx)
	if err != nil {
		return "", false, err
	}
	d, err := r.IsDirty(ctx)
	if err != nil {
		// HeadShort succeeded; treat dirty-detection failure as clean.
		return short, false, nil
	}
	return short, d, nil
}
