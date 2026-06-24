package version

import (
	"errors"
	"net/url"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func stubBuildInfoMain(t *testing.T, path string) {
	t.Helper()
	prev := readBuildInfoFn
	readBuildInfoFn = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Main: debug.Module{Path: path}}, true
	}
	t.Cleanup(func() { readBuildInfoFn = prev })
}

// TestRepoURL_FromBuildInfo asserts the repo URL is derived from the module
// path in build info (go.mod) rather than a hardcoded constant.
func TestRepoURL_FromBuildInfo(t *testing.T) {
	stubBuildInfoMain(t, "github.com/LunarHUE/MLS-Grid-Sync")
	if got, want := RepoURL(), "https://github.com/LunarHUE/MLS-Grid-Sync"; got != want {
		t.Fatalf("RepoURL() = %q, want %q", got, want)
	}
}

// TestRepoURL_Fallback asserts the defensive fallback when build info is
// unavailable (the path that keeps the diagnostic link usable under `go test`).
func TestRepoURL_Fallback(t *testing.T) {
	stubBuildInfoAbsent(t)
	if got := RepoURL(); !strings.HasPrefix(got, "https://") || !strings.Contains(got, repoFallback) {
		t.Fatalf("RepoURL() = %q, want fallback containing %q", got, repoFallback)
	}
}

// TestNewIssueURL_EncodesTitleAndBody asserts title/body are URL-encoded into
// the query string so the operator lands on a pre-filled issue form.
func TestNewIssueURL_EncodesTitleAndBody(t *testing.T) {
	stubBuildInfoMain(t, "github.com/LunarHUE/MLS-Grid-Sync")
	got := NewIssueURL("New field: A&B", "see logs")

	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("NewIssueURL produced unparseable URL %q: %v", got, err)
	}
	if want := "/LunarHUE/MLS-Grid-Sync/issues/new"; u.Path != want {
		t.Fatalf("path = %q, want %q", u.Path, want)
	}
	if got, want := u.Query().Get("title"), "New field: A&B"; got != want {
		t.Fatalf("title = %q, want %q", got, want)
	}
	if got, want := u.Query().Get("body"), "see logs"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

// TestNewIssueURL_NoQueryWhenEmpty asserts the bare issue URL when no title or
// body is supplied (no trailing "?").
func TestNewIssueURL_NoQueryWhenEmpty(t *testing.T) {
	stubBuildInfoMain(t, "github.com/LunarHUE/MLS-Grid-Sync")
	if got, want := NewIssueURL("", ""), "https://github.com/LunarHUE/MLS-Grid-Sync/issues/new"; got != want {
		t.Fatalf("NewIssueURL(\"\",\"\") = %q, want %q", got, want)
	}
}

// resetInfoCache clears the memoization state. Same-package tests reach
// into the package-private once/cached pair directly; intentionally not
// exported.
func resetInfoCache() {
	infoOnce = sync.Once{}
	cachedInfo = ""
}

func stubBuildInfoAbsent(t *testing.T) {
	t.Helper()
	prev := readBuildInfoFn
	readBuildInfoFn = func() (*debug.BuildInfo, bool) { return nil, false }
	t.Cleanup(func() { readBuildInfoFn = prev })
}

func stubBuildInfo(t *testing.T, revision, modified string) {
	t.Helper()
	prev := readBuildInfoFn
	readBuildInfoFn = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{
			Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: revision},
				{Key: "vcs.modified", Value: modified},
			},
		}, true
	}
	t.Cleanup(func() { readBuildInfoFn = prev })
}

func stubRunGit(t *testing.T, fn func() (string, bool, error)) {
	t.Helper()
	prev := runGitFn
	runGitFn = fn
	t.Cleanup(func() { runGitFn = prev })
}

// TestInfo_ComputesOnce is the bug-class regression test. The original
// version.Info() forked git on every call; this test catches a future
// regression to that behavior. 100 calls must trigger exactly one
// runGit invocation.
func TestInfo_ComputesOnce(t *testing.T) {
	resetInfoCache()
	t.Cleanup(resetInfoCache)
	stubBuildInfoAbsent(t)

	var calls int64
	stubRunGit(t, func() (string, bool, error) {
		atomic.AddInt64(&calls, 1)
		return "abc1234", false, nil
	})

	for range 100 {
		_ = Info()
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("expected 1 git-exec call from 100 Info() calls, got %d", got)
	}
}

// TestInfo_ComputesOnceConcurrent guards thread-safety of the memoization.
// Cheap to write because sync.Once handles it; the test exists so a future
// refactor that drops the Once shows up immediately.
func TestInfo_ComputesOnceConcurrent(t *testing.T) {
	resetInfoCache()
	t.Cleanup(resetInfoCache)
	stubBuildInfoAbsent(t)

	var calls int64
	stubRunGit(t, func() (string, bool, error) {
		atomic.AddInt64(&calls, 1)
		return "abc1234", false, nil
	})

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			_ = Info()
		})
	}
	wg.Wait()
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("expected 1 git-exec call from 50 concurrent Info() callers, got %d", got)
	}
}

// TestInfo_FallbackChain asserts the terminal sentinel: when neither
// build-info nor git is available, Info() returns a string containing
// "unknown" and does not error or panic.
func TestInfo_FallbackChain(t *testing.T) {
	resetInfoCache()
	t.Cleanup(resetInfoCache)
	stubBuildInfoAbsent(t)
	stubRunGit(t, func() (string, bool, error) {
		return "", false, errors.New("git not available")
	})

	got := Info()
	if !strings.Contains(got, "unknown") {
		t.Fatalf("expected fallback to contain 'unknown', got %q", got)
	}
}

// TestInfo_BuildInfoShortCircuits proves the build-info precedence step
// avoids the git fork entirely when VCS metadata is present.
func TestInfo_BuildInfoShortCircuits(t *testing.T) {
	resetInfoCache()
	t.Cleanup(resetInfoCache)
	stubBuildInfo(t, "abcdef1234567890", "false")

	var calls int64
	stubRunGit(t, func() (string, bool, error) {
		atomic.AddInt64(&calls, 1)
		return "should-not-be-called", false, nil
	})

	got := Info()
	if !strings.Contains(got, "abcdef1") {
		t.Fatalf("expected build-info sha prefix abcdef1 in %q", got)
	}
	if strings.Contains(got, "dirty") {
		t.Fatalf("expected no dirty flag when vcs.modified=false, got %q", got)
	}
	if c := atomic.LoadInt64(&calls); c != 0 {
		t.Fatalf("expected 0 git-exec calls when build-info has vcs.revision, got %d", c)
	}
}

// TestInfo_BuildInfoDirtyFlag confirms the vcs.modified=true case
// surfaces the dirty suffix.
func TestInfo_BuildInfoDirtyFlag(t *testing.T) {
	resetInfoCache()
	t.Cleanup(resetInfoCache)
	stubBuildInfo(t, "abcdef1234567890", "true")

	got := Info()
	if !strings.Contains(got, "(dirty)") {
		t.Fatalf("expected (dirty) suffix when vcs.modified=true, got %q", got)
	}
}

// setLdflags overrides the -X-injected build vars (Version/Commit/BuildDate)
// and restores them via t.Cleanup so a test that exercises the ldflag path
// can't leak into the next test's provenance resolution.
func setLdflags(t *testing.T, version, commit, buildDate string) {
	t.Helper()
	pv, pc, pb := Version, Commit, BuildDate
	Version, Commit, BuildDate = version, commit, buildDate
	t.Cleanup(func() { Version, Commit, BuildDate = pv, pc, pb })
}

// stubBuildInfoFull installs a build-info stub carrying VCS revision/modified/
// time plus GoVersion and the main module path — the inputs Details() reads.
func stubBuildInfoFull(t *testing.T, revision, modified, vcsTime, goVer, modPath string) {
	t.Helper()
	prev := readBuildInfoFn
	readBuildInfoFn = func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{
			GoVersion: goVer,
			Main:      debug.Module{Path: modPath},
			Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: revision},
				{Key: "vcs.modified", Value: modified},
				{Key: "vcs.time", Value: vcsTime},
			},
		}, true
	}
	t.Cleanup(func() { readBuildInfoFn = prev })
}

// TestDetails_FromBuildInfo asserts each Build field is resolved from build
// info when no ldflags are set: short SHA + dirty from VCS, date from vcs.time,
// Go version and module from build info, and empty Version → "dev".
func TestDetails_FromBuildInfo(t *testing.T) {
	setLdflags(t, "", "", "")
	stubBuildInfoFull(t, "abcdef1234567890", "true", "2026-06-24T18:00:00Z",
		"go1.26", "github.com/LunarHUE/MLS-Grid-Sync")

	d := Details()
	if d.Version != "dev" {
		t.Fatalf("Version = %q, want dev", d.Version)
	}
	if d.Commit != "abcdef1" { // buildInfoVCS truncates to 7
		t.Fatalf("Commit = %q, want abcdef1", d.Commit)
	}
	if !d.Dirty {
		t.Fatalf("Dirty = false, want true (vcs.modified=true)")
	}
	if d.BuildDate != "2026-06-24T18:00:00Z" {
		t.Fatalf("BuildDate = %q, want vcs.time value", d.BuildDate)
	}
	if d.GoVersion != "go1.26" {
		t.Fatalf("GoVersion = %q, want go1.26", d.GoVersion)
	}
	if d.Module != "github.com/LunarHUE/MLS-Grid-Sync" {
		t.Fatalf("Module = %q, want module path", d.Module)
	}
	if d.RepoURL != "https://github.com/LunarHUE/MLS-Grid-Sync" {
		t.Fatalf("RepoURL = %q", d.RepoURL)
	}
}

// TestDetails_LdflagCommitWins asserts an explicit -X Commit is the source of
// truth over VCS build info, retains the FULL SHA (no truncation in the
// struct), and reports clean (no dirty bit from an injected commit).
func TestDetails_LdflagCommitWins(t *testing.T) {
	const fullSHA = "0123456789abcdef0123456789abcdef01234567"
	setLdflags(t, "v1.2.3", fullSHA, "2026-06-24T00:00:00Z")
	// VCS info present but with a DIFFERENT, dirty revision; the ldflag must win.
	stubBuildInfoFull(t, "ffffffffffffffff", "true", "1999-01-01T00:00:00Z",
		"go1.26", "github.com/LunarHUE/MLS-Grid-Sync")

	d := Details()
	if d.Version != "v1.2.3" {
		t.Fatalf("Version = %q, want v1.2.3", d.Version)
	}
	if d.Commit != fullSHA {
		t.Fatalf("Commit = %q, want full ldflag SHA %q", d.Commit, fullSHA)
	}
	if d.Dirty {
		t.Fatalf("Dirty = true, want false for an ldflag-provided commit")
	}
	if d.BuildDate != "2026-06-24T00:00:00Z" {
		t.Fatalf("BuildDate = %q, want ldflag value (not vcs.time)", d.BuildDate)
	}
}

// TestInfo_LdflagCommitWins guards the in-tandem precedence change: Info()'s
// stamp now uses the ldflag commit (shortened) over a stubbed vcs.revision.
func TestInfo_LdflagCommitWins(t *testing.T) {
	resetInfoCache()
	t.Cleanup(resetInfoCache)
	setLdflags(t, "", "0123456789abcdef", "")
	stubBuildInfo(t, "ffffffffffffffff", "false")

	got := Info()
	if !strings.Contains(got, "0123456") {
		t.Fatalf("Info() = %q, want short ldflag commit 0123456", got)
	}
	if strings.Contains(got, "fffffff") {
		t.Fatalf("Info() = %q leaked the vcs.revision; ldflag should win", got)
	}
}

// TestDetails_FallbackUnknown asserts the terminal sentinels when neither
// ldflags, build-info VCS, nor git are available — no panic, safe defaults.
func TestDetails_FallbackUnknown(t *testing.T) {
	setLdflags(t, "", "", "")
	stubBuildInfoAbsent(t)
	stubRunGit(t, func() (string, bool, error) { return "", false, errors.New("no git") })

	d := Details()
	if d.Commit != "unknown" {
		t.Fatalf("Commit = %q, want unknown", d.Commit)
	}
	if d.BuildDate != "unknown" {
		t.Fatalf("BuildDate = %q, want unknown", d.BuildDate)
	}
	if d.GoVersion == "" {
		t.Fatalf("GoVersion empty; want runtime.Version() fallback")
	}
	if !strings.Contains(d.Module, repoFallback) {
		t.Fatalf("Module = %q, want fallback %q", d.Module, repoFallback)
	}
}

// TestShortCommit asserts the display helper is length-safe.
func TestShortCommit(t *testing.T) {
	cases := map[string]string{
		"":        "",
		"dev":     "dev",
		"unknown": "unknown",
		"abc":     "abc",
		"abcdefg": "abcdefg", // exactly 7
		"0123456789abcdef0123456789abcdef01234567": "0123456",
	}
	for in, want := range cases {
		if got := ShortCommit(in); got != want {
			t.Fatalf("ShortCommit(%q) = %q, want %q", in, got, want)
		}
	}
}

// BenchmarkInfo is executable documentation. A future regression to
// per-call forking shows up as a 6-orders-of-magnitude cliff in this
// benchmark even if no one runs the counting tests.
func BenchmarkInfo(b *testing.B) {
	resetInfoCache()
	b.Cleanup(resetInfoCache)
	prevBI := readBuildInfoFn
	readBuildInfoFn = func() (*debug.BuildInfo, bool) { return nil, false }
	b.Cleanup(func() { readBuildInfoFn = prevBI })
	prevRG := runGitFn
	runGitFn = func() (string, bool, error) { return "deadbee", false, nil }
	b.Cleanup(func() { runGitFn = prevRG })

	_ = Info() // prime the cache
	b.ResetTimer()
	for b.Loop() {
		_ = Info()
	}
}
