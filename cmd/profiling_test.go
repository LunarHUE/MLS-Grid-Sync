package cmd

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lunarhue/website-highpointe/packages/mls-grid-sync/config"
)

// pickFreePort returns a port the OS just confirmed was free. Tests
// can't hardcode 6060 because the dev pprof endpoint might already own
// it locally.
func pickFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return port
}

// TestStartProfilingServer_DisabledIsSilent: with enabled=false the
// function must NOT start a listener and MUST NOT enable block
// sampling. The smoke test the runbook calls for.
func TestStartProfilingServer_DisabledIsSilent(t *testing.T) {
	port := pickFreePort(t)
	startProfilingServer(config.ProfilingConfig{Enabled: false, Port: port})

	// Give a no-op goroutine a beat just in case — if startProfilingServer
	// did try to start a listener despite enabled=false, this test must
	// still fail. (It doesn't, but the wait makes the assertion honest.)
	time.Sleep(50 * time.Millisecond)

	_, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
	assert.Error(t, err, "disabled profiling must not bind a listener")
}

// TestStartProfilingServer_EnabledServesPprofIndex: with enabled=true,
// /debug/pprof/ serves the standard pprof index. Catches the regression
// where someone deletes the blank net/http/pprof import (it registers
// the handlers as a side effect) and breaks the runbook silently.
func TestStartProfilingServer_EnabledServesPprofIndex(t *testing.T) {
	port := pickFreePort(t)
	startProfilingServer(config.ProfilingConfig{Enabled: true, Port: port})

	// Poll for the listener — the goroutine race with ListenAndServe is
	// real on slow CI.
	addr := fmt.Sprintf("http://127.0.0.1:%d/debug/pprof/", port)
	var body string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(addr)
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body = string(b)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.NotEmpty(t, body, "pprof index never responded at %s", addr)
	assert.True(t, strings.Contains(body, "Types of profiles available") ||
		strings.Contains(body, "/debug/pprof/heap"),
		"pprof index should advertise its profile endpoints; got: %q", body[:min(200, len(body))])
}
