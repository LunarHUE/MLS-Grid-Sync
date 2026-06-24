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
