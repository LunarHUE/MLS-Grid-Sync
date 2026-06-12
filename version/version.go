package version

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"

	"github.com/lunarhue/libs-go/log"
	"github.com/LunarHUE/MLS-Grid-Sync/git"
)

// Version is set at build time via -X ldflags.
var Version string

// Commit is the git hash, set at build time via -X ldflags.
var Commit string

// Short returns just the semver string, e.g. "0.0.8".
func Short() string {
	if Version == "" {
		return "dev"
	}
	return Version
}

// Tag returns the release tag for this build, e.g. "v0.0.8-abc1234".
// Returns ("", false) if version or commit is unknown (dev build).
func Tag() (string, bool) {
	if Version == "" || Commit == "" || Commit == "unknown" {
		return "", false
	}
	return fmt.Sprintf("v%s-%s", Version, Commit), true
}

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
