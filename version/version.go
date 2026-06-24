package version

import (
	"context"
	"fmt"
	"net/url"
	"runtime"
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

// BuildDate is the build timestamp, set at build time via -X ldflags
// (the Dockerfile injects an ISO-8601 UTC value). When empty, Details()
// falls back to the vcs.time build setting, then to "unknown".
var BuildDate string

// Build is the structured build identity surfaced by the `version` command.
// It is provenance metadata for support reports — best-effort, never
// load-bearing for correctness. Commit holds the full SHA (callers shorten
// for display via ShortCommit); the JSON tags are the machine contract.
type Build struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Dirty     bool   `json:"dirty"`
	BuildDate string `json:"buildDate"`
	GoVersion string `json:"goVersion"`
	Module    string `json:"module"`
	RepoURL   string `json:"repoURL"`
}

// Details resolves the structured build identity. It shares the same
// explicit-ldflag-first provenance chain as Info()/compute() (see
// resolveCommit), so the `version` command and the processor_version stamp
// never disagree about which commit built the binary. Needs no DB, config,
// or network — safe to call from a bare `version` invocation.
func Details() Build {
	v := Version
	if v == "" {
		v = "dev"
	}
	sha, dirty := resolveCommit()
	return Build{
		Version:   v,
		Commit:    sha,
		Dirty:     dirty,
		BuildDate: resolveBuildDate(),
		GoVersion: goVersion(),
		Module:    moduleName(),
		RepoURL:   RepoURL(),
	}
}

// ShortCommit truncates a commit SHA to 7 chars for display. Length-safe:
// short or sentinel values ("", "dev", "unknown") pass through unchanged so
// human/--version output never panics.
func ShortCommit(c string) string {
	if len(c) <= 7 {
		return c
	}
	return c[:7]
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

// compute resolves the version stamp once, reusing resolveCommit so the stamp
// and the `version` command agree on provenance. The stamp keeps its short
// (7-char) SHA form via ShortCommit regardless of the source.
func compute() string {
	version := Version
	if version == "" {
		version = "dev"
	}

	sha, dirty := resolveCommit()
	if sha == "unknown" {
		log.Infof("version: build-info and git both unavailable; processor_version stamps degraded to 'unknown'")
	}
	return format(version, ShortCommit(sha), dirty)
}

// resolveCommit returns the commit SHA and dirty flag using an
// explicit-ldflag-first precedence:
//  1. ldflags-set Commit — the source of truth when CI injects it (the
//     released Docker image has no .git, so this is its only signal). Reports
//     clean: an injected commit carries no dirty bit.
//  2. debug.ReadBuildInfo VCS metadata (the `go build` path, in a work tree).
//  3. fork git once via runGitFn (the `go run` path).
//  4. "unknown" sentinel — never errors; a missing .git/git binary degrades
//     the value rather than failing.
//
// Details() returns this SHA verbatim (full length from the ldflag path);
// compute() shortens it for the stamp. The two never disagree on source.
func resolveCommit() (sha string, dirty bool) {
	if Commit != "" {
		return Commit, false
	}
	if s, d, ok := buildInfoVCS(); ok {
		return s, d
	}
	if s, d, err := runGitFn(); err == nil {
		return s, d
	}
	return "unknown", false
}

// resolveBuildDate prefers the ldflag-injected BuildDate (CI source of truth),
// falling back to the vcs.time build setting, then "unknown".
func resolveBuildDate() string {
	if BuildDate != "" {
		return BuildDate
	}
	if t := buildSetting("vcs.time"); t != "" {
		return t
	}
	return "unknown"
}

// buildSetting reads a single debug.BuildInfo setting (e.g. "vcs.time") via
// the injectable readBuildInfoFn so it stays unit-testable.
func buildSetting(key string) string {
	bi, ok := readBuildInfoFn()
	if !ok || bi == nil {
		return ""
	}
	for _, s := range bi.Settings {
		if s.Key == key {
			return s.Value
		}
	}
	return ""
}

// goVersion reports the Go toolchain that built the binary, from build info
// when available, else the running runtime version.
func goVersion() string {
	if bi, ok := readBuildInfoFn(); ok && bi != nil && bi.GoVersion != "" {
		return bi.GoVersion
	}
	return runtime.Version()
}

// moduleName reports the main module path from build info, falling back to the
// defensive repoFallback constant.
func moduleName() string {
	if bi, ok := readBuildInfoFn(); ok && bi != nil && bi.Main.Path != "" {
		return bi.Main.Path
	}
	return repoFallback
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
